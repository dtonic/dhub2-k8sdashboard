package queryprotect_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/queryprotect"
)

func TestUserAndDashboardLimitsAndRelease(t *testing.T) {
	now := time.Unix(1, 0)
	cfg := queryprotect.DefaultConfig()
	cfg.Now = func() time.Time { return now }
	cfg.UserConcurrent = 1
	cfg.DashboardConcurrent = 1
	cfg.UserRate = 100
	cfg.DashboardRate = 100
	g := queryprotect.New(cfg, nil)
	release, reason, _ := g.Acquire("u", "a")
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason, _ := g.Acquire("u", "b"); reason != "user_concurrency" {
		t.Fatalf("across dashboards reason=%s", reason)
	}
	if _, reason, _ := g.Acquire("v", "a"); reason != "dashboard_concurrency" {
		t.Fatalf("across users reason=%s", reason)
	}
	release()
	release()
	if r, reason, _ := g.Acquire("u", "a"); reason != "" {
		t.Fatal(reason)
	} else {
		r()
	}
}

func TestRateAndMetricsHaveBoundedLabels(t *testing.T) {
	cfg := queryprotect.DefaultConfig()
	cfg.UserRate = 1
	cfg.UserBurst = 1
	g := queryprotect.New(cfg, nil)
	r, _, _ := g.Acquire("secret-user", "overview")
	r()
	if _, reason, _ := g.Acquire("secret-user", "overview"); reason != "user_rate" {
		t.Fatalf("reason=%s", reason)
	}
	m := queryprotect.NewMetrics()
	m.Reject("user_rate", "overview")
	m.Slow("overview")
	var b bytes.Buffer
	if err := m.WritePrometheus(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "secret-user") || !strings.Contains(b.String(), "dashboard_query_rejected_total") {
		t.Fatalf("metrics=%s", b.String())
	}
}

func TestActiveIdentityCapacityRejectsWithoutGrowing(t *testing.T) {
	cfg := queryprotect.DefaultConfig()
	cfg.MaxIdentities = 1
	guard := queryprotect.New(cfg, nil)
	if users, dashboards := guard.IdentityCounts(); users != 0 || dashboards != 0 { t.Fatal(users, dashboards) }
	release, reason, _ := guard.Acquire("active-user", "active-dashboard")
	if reason != "" {
		t.Fatal(reason)
	}
	defer release()
	if users, dashboards := guard.IdentityCounts(); users != 1 || dashboards != 1 { t.Fatal(users, dashboards) }
	if _, reason, _ := guard.Acquire("second-user", "second-dashboard"); reason != "identity_capacity" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestMetricCacheCountersAndDefaultDurations(t *testing.T) {
	g := queryprotect.New(queryprotect.Config{}, nil)
	if g.Timeout() <= 0 || g.SlowThreshold() <= 0 { t.Fatal(g.Timeout(), g.SlowThreshold()) }
	m := queryprotect.NewMetrics()
	m.ObserveCache(cache.ResultL1)
	m.ObserveCacheError("decode")
	m.ObserveCacheLoad()
	var b bytes.Buffer
	if err := m.WritePrometheus(t.Context(), &b); err != nil { t.Fatal(err) }
	for _, want := range []string{"dashboard_cache_requests_total", "dashboard_cache_errors_total", "dashboard_cache_loads_total"} {
		if !strings.Contains(b.String(), want) { t.Fatalf("missing %s in %s", want, b.String()) }
	}
}

func BenchmarkGuardAllow(b *testing.B) {
	cfg := queryprotect.DefaultConfig()
	cfg.UserRate = 1e9
	cfg.DashboardRate = 1e9
	cfg.UserBurst = 1 << 30
	cfg.DashboardBurst = 1 << 30
	g := queryprotect.New(cfg, nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r, _, _ := g.Acquire("u", "overview")
		r()
	}
}
