// Package queryprotect implements bounded request guards and dependency-free metrics.
package queryprotect

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
)

type Config struct {
	UserRate, DashboardRate              float64
	UserBurst, DashboardBurst            int
	UserConcurrent, DashboardConcurrent  int
	QueryTimeout, SlowThreshold, IdleTTL time.Duration
	MaxIdentities                        int
	Now                                  func() time.Time
}

func DefaultConfig() Config {
	return Config{UserRate: 200, DashboardRate: 1000, UserBurst: 400, DashboardBurst: 2000, UserConcurrent: 8, DashboardConcurrent: 32, QueryTimeout: 12 * time.Second, SlowThreshold: 2 * time.Second, IdleTTL: 10 * time.Minute, MaxIdentities: 4096, Now: time.Now}
}

type state struct {
	tokens        float64
	last, touched time.Time
	concurrent    int
}
type Guard struct {
	mu                sync.Mutex
	cfg               Config
	users, dashboards map[string]*state
	ops               uint64
	metrics           *Metrics
}

func New(cfg Config, metrics *Metrics) *Guard {
	d := DefaultConfig()
	if cfg.UserRate <= 0 {
		cfg.UserRate = d.UserRate
	}
	if cfg.DashboardRate <= 0 {
		cfg.DashboardRate = d.DashboardRate
	}
	if cfg.UserBurst <= 0 {
		cfg.UserBurst = d.UserBurst
	}
	if cfg.DashboardBurst <= 0 {
		cfg.DashboardBurst = d.DashboardBurst
	}
	if cfg.UserConcurrent <= 0 {
		cfg.UserConcurrent = d.UserConcurrent
	}
	if cfg.DashboardConcurrent <= 0 {
		cfg.DashboardConcurrent = d.DashboardConcurrent
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = d.QueryTimeout
	}
	if cfg.SlowThreshold <= 0 {
		cfg.SlowThreshold = d.SlowThreshold
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = d.IdleTTL
	}
	if cfg.MaxIdentities <= 0 {
		cfg.MaxIdentities = d.MaxIdentities
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Guard{cfg: cfg, users: map[string]*state{}, dashboards: map[string]*state{}, metrics: metrics}
}

func (g *Guard) Timeout() time.Duration       { return g.cfg.QueryTimeout }
func (g *Guard) SlowThreshold() time.Duration { return g.cfg.SlowThreshold }

// Acquire atomically applies per-user and per-dashboard rate and concurrency limits.
func (g *Guard) Acquire(user, dashboard string) (release func(), reason string, retry time.Duration) {
	now := g.cfg.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ops++
	if g.ops%64 == 0 {
		g.evict(now)
	}
	u, ok := g.get(g.users, user, now, float64(g.cfg.UserBurst))
	if !ok {
		return nil, "identity_capacity", time.Second
	}
	d, ok := g.get(g.dashboards, dashboard, now, float64(g.cfg.DashboardBurst))
	if !ok {
		return nil, "identity_capacity", time.Second
	}
	if ok, wait := take(u, now, g.cfg.UserRate, g.cfg.UserBurst); !ok {
		return nil, "user_rate", wait
	}
	if ok, wait := take(d, now, g.cfg.DashboardRate, g.cfg.DashboardBurst); !ok {
		u.tokens++
		return nil, "dashboard_rate", wait
	}
	if u.concurrent >= g.cfg.UserConcurrent {
		u.tokens++
		d.tokens++
		return nil, "user_concurrency", time.Second
	}
	if d.concurrent >= g.cfg.DashboardConcurrent {
		u.tokens++
		d.tokens++
		return nil, "dashboard_concurrency", time.Second
	}
	u.concurrent++
	d.concurrent++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			u.concurrent--
			d.concurrent--
			u.touched = g.cfg.Now()
			d.touched = g.cfg.Now()
			g.mu.Unlock()
		})
	}, "", 0
}

func (g *Guard) get(m map[string]*state, key string, now time.Time, burst float64) (*state, bool) {
	if s := m[key]; s != nil {
		return s, true
	}
	if len(m) >= g.cfg.MaxIdentities {
		g.evictOldest(m)
		if len(m) >= g.cfg.MaxIdentities {
			return nil, false
		}
	}
	s := &state{tokens: burst, last: now, touched: now}
	m[key] = s
	return s, true
}
func take(s *state, now time.Time, rate float64, burst int) (bool, time.Duration) {
	s.tokens += now.Sub(s.last).Seconds() * rate
	if s.tokens > float64(burst) {
		s.tokens = float64(burst)
	}
	s.last = now
	s.touched = now
	if s.tokens >= 1 {
		s.tokens--
		return true, 0
	}
	return false, time.Duration((1 - s.tokens) / rate * float64(time.Second))
}
func (g *Guard) evict(now time.Time) {
	for _, m := range []map[string]*state{g.users, g.dashboards} {
		for k, s := range m {
			if s.concurrent == 0 && now.Sub(s.touched) >= g.cfg.IdleTTL {
				delete(m, k)
			}
		}
	}
}
func (g *Guard) evictOldest(m map[string]*state) {
	var key string
	var oldest time.Time
	for k, s := range m {
		if s.concurrent > 0 {
			continue
		}
		if key == "" || s.touched.Before(oldest) {
			key, oldest = k, s.touched
		}
	}
	if key != "" {
		delete(m, key)
	}
}

type Metrics struct {
	counters sync.Map
}

func NewMetrics() *Metrics { return &Metrics{} }
func (m *Metrics) add(name string) {
	v, _ := m.counters.LoadOrStore(name, &atomic.Uint64{})
	v.(*atomic.Uint64).Add(1)
}
func (m *Metrics) ObserveCache(r cache.Result) {
	m.add(fmt.Sprintf("dashboard_cache_requests_total{result=%q}", r))
}
func (m *Metrics) ObserveCacheError(reason string) {
	m.add(fmt.Sprintf("dashboard_cache_errors_total{reason=%q}", reason))
}
func (m *Metrics) ObserveCacheLoad() { m.add("dashboard_cache_loads_total") }
func (m *Metrics) Reject(reason, dashboard string) {
	m.add(fmt.Sprintf("dashboard_query_rejected_total{dashboard=%q,reason=%q}", dashboard, reason))
}
func (m *Metrics) Slow(dashboard string) {
	m.add(fmt.Sprintf("dashboard_query_slow_total{dashboard=%q}", dashboard))
}
func (m *Metrics) WritePrometheus(ctx context.Context, w io.Writer) error {
	_ = ctx
	keys := []string{}
	m.counters.Range(func(k, v any) bool { keys = append(keys, k.(string)); return true })
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := m.counters.Load(k)
		if _, err := fmt.Fprintf(w, "%s %d\n", k, v.(*atomic.Uint64).Load()); err != nil {
			return err
		}
	}
	return nil
}
