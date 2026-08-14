// Package config는 환경변수 하나로 서버 동작을 정합니다.
//
// 기본값은 **클러스터에 가장 안전한 쪽**으로 둡니다. 잘못 설정했을 때
// 대시보드가 느려지는 것은 괜찮지만, 대시보드가 클러스터를 흔드는 것은 안 됩니다.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
)

type Config struct {
	Addr string

	Kubeconfig      string
	DisableProtobuf bool
	QPS             float32
	Burst           int
	Resync          time.Duration
	// EventFieldSelector 기본값 `type=Warning`. Event는 대부분의 클러스터에서 가장 수가 많습니다.
	EventFieldSelector string

	ClusterID   string
	ClusterName string

	// Namespaces는 이 프로세스가 노출할 범위입니다. "*"면 전체입니다.
	// 실제 인증 연동 전까지의 정적 Scope입니다.
	Namespaces []string
	AllNS      bool

	// CacheTTL은 화면 응답 재사용 시간입니다. 짧게 두되 0은 아닙니다 —
	// 0이면 자동 갱신을 켠 사용자 수만큼 데이터소스 팬아웃이 그대로 늘어납니다.
	CacheTTL                 time.Duration
	CacheShortTTL            time.Duration
	CacheHistoricalTTL       time.Duration
	CacheHistoricalSafety    time.Duration
	CacheMaxEntries          int
	CacheMaxValueBytes       int
	CacheMaxLocalBytes       int
	RedisAddr                string
	RedisOpTimeout           time.Duration
	RedisCooldown            time.Duration
	QueryTimeout             time.Duration
	QuerySlowThreshold       time.Duration
	QueryUserRate            float64
	QueryDashboardRate       float64
	QueryUserBurst           int
	QueryDashboardBurst      int
	QueryUserConcurrent      int
	QueryDashboardConcurrent int

	// Stream은 상태 변경 SSE(#12)의 유계 상한입니다.
	StreamMaxConnections int
	StreamMaxPerSubject  int
	StreamReplayEvents   int
	StreamSubBuffer      int
	StreamHeartbeat      time.Duration
	StreamWriteIdle      time.Duration
	// AlertPoll은 알림 스냅숏 diff 폴러(#12)의 주기·상한입니다.
	AlertPollInterval   time.Duration
	AlertPollMaxBackoff time.Duration
	AlertSnapshotMax    int

	// UseDemoData는 GreptimeDB/Quickwit/Alertmanager 없이 결정적 값을 씁니다.
	// 실주소(GREPTIME_URL·QUICKWIT_URL)가 설정된 데이터소스는 이 값과 무관하게
	// 실제 어댑터를 씁니다 — 주소를 적은 것이 의도이기 때문입니다.
	UseDemoData bool

	// Auth는 인증 방식입니다. 기본은 none(정적 Scope)입니다. (#10)
	Auth AuthConfig

	// QueryCatalogDir이 비어 있으면 바이너리에 임베드된 기본 카탈로그를 씁니다.
	// 지정하면 그 디렉터리의 *.yaml이 기본 카탈로그를 **대체**합니다. (#9)
	QueryCatalogDir string

	// Greptime은 메트릭 데이터소스(GreptimeDB) 설정입니다. URL이 비어 있으면 미사용입니다.
	Greptime GreptimeConfig
	// Quickwit은 로그 데이터소스 설정입니다. URL이 비어 있으면 미사용입니다.
	Quickwit QuickwitConfig

	// AllowedOrigin은 개발 중 Vite 오리진을 허용할 때 씁니다. 비어 있으면 CORS 헤더를 붙이지 않습니다.
	AllowedOrigin string

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// AuthConfig는 인증 설정입니다. (#10)
//
// Mode:
//   - "none": SCOPE_NAMESPACES 기반 정적 Scope. 인증 없는 개발·데모용입니다.
//   - "oidc": 표준 OIDC provider의 Bearer JWT를 검증합니다. 운영 기본입니다.
//   - "mock": 로컬 mock IdP를 함께 띄웁니다. 검증 경로는 oidc와 같고
//     발급자만 로컬입니다. **운영 금지** — 누구나 토큰을 만들 수 있습니다.
type AuthConfig struct {
	Mode           string
	Issuer         string
	Audience       string
	RolesClaim     string
	Leeway         time.Duration
	JWKSMinRefresh time.Duration
	// MockAddr은 mock IdP의 바인드 주소입니다. loopback을 벗어나지 마세요.
	MockAddr string
}

// GreptimeConfig는 GreptimeDB 접속 설정입니다. Credential은 서버만 압니다 —
// 브라우저로 나가는 응답 어디에도 실리지 않습니다. (README §10)
type GreptimeConfig struct {
	URL           string
	DB            string
	Username      string
	Password      string
	Timeout       time.Duration
	MaxDataPoints int
}

// QuickwitConfig는 Quickwit 접속 설정입니다.
type QuickwitConfig struct {
	URL         string
	Index       string
	Username    string
	Password    string
	Timeout     time.Duration
	MaxPageSize int
	MaxLines    int
	// Fields는 인덱스 필드 이름 재정의입니다. "message=body.message,level=severity" 형식입니다.
	Fields map[string]string
}

func Load() Config {
	nsList, all := scope.ParseNamespaces(env("SCOPE_NAMESPACES", "*"))
	return Config{
		Addr:                     env("ADDR", ":8080"),
		Kubeconfig:               env("KUBECONFIG", ""),
		DisableProtobuf:          envBool("K8S_DISABLE_PROTOBUF", false),
		QPS:                      float32(envFloat("K8S_QPS", 20)),
		Burst:                    envInt("K8S_BURST", 30),
		Resync:                   envDuration("K8S_RESYNC", 10*time.Minute),
		EventFieldSelector:       env("K8S_EVENT_FIELD_SELECTOR", "type=Warning"),
		ClusterID:                env("CLUSTER_ID", "default"),
		ClusterName:              env("CLUSTER_NAME", ""),
		Namespaces:               nsList,
		AllNS:                    all,
		CacheTTL:                 envDuration("CACHE_TTL", 5*time.Second),
		CacheShortTTL:            envDuration("CACHE_SHORT_TTL", 30*time.Second),
		CacheHistoricalTTL:       envDuration("CACHE_HISTORICAL_TTL", 10*time.Minute),
		CacheHistoricalSafety:    envDuration("CACHE_HISTORICAL_SAFETY", 5*time.Minute),
		CacheMaxEntries:          envInt("CACHE_MAX_ENTRIES", 1024),
		CacheMaxValueBytes:       envInt("CACHE_MAX_VALUE_BYTES", 4<<20),
		CacheMaxLocalBytes:       envInt("CACHE_MAX_LOCAL_BYTES", 64<<20),
		RedisAddr:                env("REDIS_ADDR", ""),
		RedisOpTimeout:           envDuration("REDIS_OP_TIMEOUT", 75*time.Millisecond),
		RedisCooldown:            envDuration("REDIS_COOLDOWN", 2*time.Second),
		QueryTimeout:             envDuration("QUERY_TIMEOUT", 12*time.Second),
		QuerySlowThreshold:       envDuration("QUERY_SLOW_THRESHOLD", 2*time.Second),
		QueryUserRate:            envFloat("QUERY_USER_RATE", 20),
		QueryDashboardRate:       envFloat("QUERY_DASHBOARD_RATE", 100),
		QueryUserBurst:           envInt("QUERY_USER_BURST", 40),
		QueryDashboardBurst:      envInt("QUERY_DASHBOARD_BURST", 200),
		QueryUserConcurrent:      envInt("QUERY_USER_CONCURRENT", 8),
		QueryDashboardConcurrent: envInt("QUERY_DASHBOARD_CONCURRENT", 32),
		StreamMaxConnections:     envInt("STREAM_MAX_CONNECTIONS", 256),
		StreamMaxPerSubject:      envInt("STREAM_MAX_PER_SUBJECT", 8),
		StreamReplayEvents:       envInt("STREAM_REPLAY_EVENTS", 1024),
		StreamSubBuffer:          envInt("STREAM_SUB_BUFFER", 64),
		StreamHeartbeat:          envDuration("STREAM_HEARTBEAT", 15*time.Second),
		StreamWriteIdle:          envDuration("STREAM_WRITE_IDLE", 60*time.Second),
		AlertPollInterval:        envDuration("ALERT_POLL_INTERVAL", 30*time.Second),
		AlertPollMaxBackoff:      envDuration("ALERT_POLL_MAX_BACKOFF", 5*time.Minute),
		AlertSnapshotMax:         envInt("ALERT_SNAPSHOT_MAX", 2000),
		UseDemoData:              envBool("USE_DEMO_DATA", true),
		QueryCatalogDir:          env("QUERY_CATALOG_DIR", ""),
		Auth: AuthConfig{
			Mode:           env("AUTH_MODE", "none"),
			Issuer:         env("OIDC_ISSUER", ""),
			Audience:       env("OIDC_AUDIENCE", ""),
			RolesClaim:     env("OIDC_ROLES_CLAIM", "roles"),
			Leeway:         envDuration("OIDC_LEEWAY", time.Minute),
			JWKSMinRefresh: envDuration("OIDC_JWKS_MIN_REFRESH", 5*time.Minute),
			MockAddr:       env("AUTH_MOCK_ADDR", "127.0.0.1:8091"),
		},
		Greptime: GreptimeConfig{
			URL:           env("GREPTIME_URL", ""),
			DB:            env("GREPTIME_DB", "public"),
			Username:      env("GREPTIME_USERNAME", ""),
			Password:      env("GREPTIME_PASSWORD", ""),
			Timeout:       envDuration("GREPTIME_TIMEOUT", 10*time.Second),
			MaxDataPoints: envInt("GREPTIME_MAX_POINTS", 1000),
		},
		Quickwit: QuickwitConfig{
			URL:         env("QUICKWIT_URL", ""),
			Index:       env("QUICKWIT_INDEX", "k8s-logs"),
			Username:    env("QUICKWIT_USERNAME", ""),
			Password:    env("QUICKWIT_PASSWORD", ""),
			Timeout:     envDuration("QUICKWIT_TIMEOUT", 10*time.Second),
			MaxPageSize: envInt("QUICKWIT_MAX_PAGE", 500),
			MaxLines:    envInt("QUICKWIT_MAX_LINES", 5000),
			Fields:      envPairs("QUICKWIT_FIELDS"),
		},
		AllowedOrigin: env("ALLOWED_ORIGIN", ""),
		ReadTimeout:   envDuration("READ_TIMEOUT", 15*time.Second),
		WriteTimeout:  envDuration("WRITE_TIMEOUT", 30*time.Second),
	}
}

// Validate는 서버를 띄우면 안 되는 필수 설정 오류를 기동 전에 모아 돌려줍니다. (#5)
//
// 여기서 잡는 것은 **유효하지 않은 서버**를 만드는 값뿐입니다. 데이터소스 주소
// 오류는 문서된 대로 해당 섹션만 degraded로 내려가므로 여기서 막지 않고,
// 형식이 틀린 선택적 튜닝 env는 Load()가 기본값으로 대체합니다.
func (c Config) Validate() error {
	var errs []error
	if err := validateListenAddr(c.Addr); err != nil {
		errs = append(errs, fmt.Errorf("ADDR가 유효한 listen 주소가 아닙니다: %q: %w", c.Addr, err))
	}
	if c.ClusterID == "" {
		errs = append(errs, errors.New("CLUSTER_ID가 비어 있습니다"))
	}
	if c.QPS <= 0 {
		errs = append(errs, fmt.Errorf("K8S_QPS는 0보다 커야 합니다: %v", c.QPS))
	}
	if c.Burst <= 0 {
		errs = append(errs, fmt.Errorf("K8S_BURST는 0보다 커야 합니다: %d", c.Burst))
	}
	if c.Resync <= 0 {
		errs = append(errs, fmt.Errorf("K8S_RESYNC는 0보다 커야 합니다: %v", c.Resync))
	}
	if c.CacheTTL <= 0 {
		// 0이면 자동 갱신 사용자 수만큼 데이터소스 팬아웃이 늘어납니다.
		errs = append(errs, fmt.Errorf("CACHE_TTL은 0보다 커야 합니다: %v", c.CacheTTL))
	}
	for name, value := range map[string]time.Duration{"CACHE_SHORT_TTL": c.CacheShortTTL, "CACHE_HISTORICAL_TTL": c.CacheHistoricalTTL, "CACHE_HISTORICAL_SAFETY": c.CacheHistoricalSafety, "REDIS_OP_TIMEOUT": c.RedisOpTimeout, "REDIS_COOLDOWN": c.RedisCooldown, "QUERY_TIMEOUT": c.QueryTimeout, "QUERY_SLOW_THRESHOLD": c.QuerySlowThreshold} {
		if value <= 0 {
			errs = append(errs, fmt.Errorf("%s은 0보다 커야 합니다: %v", name, value))
		}
	}
	for name, value := range map[string]int{"CACHE_MAX_ENTRIES": c.CacheMaxEntries, "CACHE_MAX_VALUE_BYTES": c.CacheMaxValueBytes, "CACHE_MAX_LOCAL_BYTES": c.CacheMaxLocalBytes, "QUERY_USER_BURST": c.QueryUserBurst, "QUERY_DASHBOARD_BURST": c.QueryDashboardBurst, "QUERY_USER_CONCURRENT": c.QueryUserConcurrent, "QUERY_DASHBOARD_CONCURRENT": c.QueryDashboardConcurrent} {
		if value <= 0 {
			errs = append(errs, fmt.Errorf("%s은 0보다 커야 합니다: %d", name, value))
		}
	}
	if c.QueryUserRate <= 0 || c.QueryDashboardRate <= 0 {
		errs = append(errs, errors.New("query rate limits must be positive"))
	}
	if c.CacheShortTTL < c.CacheTTL || c.CacheHistoricalTTL < c.CacheShortTTL {
		errs = append(errs, errors.New("cache TTLs must satisfy historical >= short >= state"))
	}
	if c.RedisOpTimeout >= c.QueryTimeout {
		errs = append(errs, errors.New("REDIS_OP_TIMEOUT must be less than QUERY_TIMEOUT"))
	}
	if c.QuerySlowThreshold > c.QueryTimeout {
		errs = append(errs, errors.New("QUERY_SLOW_THRESHOLD must not exceed QUERY_TIMEOUT"))
	}
	if c.WriteTimeout <= c.QueryTimeout {
		errs = append(errs, errors.New("WRITE_TIMEOUT must be greater than QUERY_TIMEOUT"))
	}
	if c.CacheMaxLocalBytes < c.CacheMaxValueBytes {
		errs = append(errs, errors.New("CACHE_MAX_LOCAL_BYTES must be at least CACHE_MAX_VALUE_BYTES"))
	}
	for name, value := range map[string]int{"STREAM_MAX_CONNECTIONS": c.StreamMaxConnections, "STREAM_MAX_PER_SUBJECT": c.StreamMaxPerSubject, "STREAM_REPLAY_EVENTS": c.StreamReplayEvents, "STREAM_SUB_BUFFER": c.StreamSubBuffer, "ALERT_SNAPSHOT_MAX": c.AlertSnapshotMax} {
		if value <= 0 {
			errs = append(errs, fmt.Errorf("%s은 0보다 커야 합니다: %d", name, value))
		}
	}
	for name, value := range map[string]time.Duration{"STREAM_HEARTBEAT": c.StreamHeartbeat, "STREAM_WRITE_IDLE": c.StreamWriteIdle, "ALERT_POLL_INTERVAL": c.AlertPollInterval, "ALERT_POLL_MAX_BACKOFF": c.AlertPollMaxBackoff} {
		if value <= 0 {
			errs = append(errs, fmt.Errorf("%s은 0보다 커야 합니다: %v", name, value))
		}
	}
	// heartbeat가 idle 한도보다 짧아야 살아 있는 연결이 write deadline에 걸리지 않습니다.
	if c.StreamHeartbeat >= c.StreamWriteIdle {
		errs = append(errs, errors.New("STREAM_HEARTBEAT must be less than STREAM_WRITE_IDLE"))
	}
	if c.StreamMaxPerSubject > c.StreamMaxConnections {
		errs = append(errs, errors.New("STREAM_MAX_PER_SUBJECT must not exceed STREAM_MAX_CONNECTIONS"))
	}
	if c.StreamReplayEvents > stream.MaxRingSize {
		errs = append(errs, fmt.Errorf("STREAM_REPLAY_EVENTS must not exceed %d", stream.MaxRingSize))
	}
	if c.StreamMaxConnections > stream.MaxConnections {
		errs = append(errs, fmt.Errorf("STREAM_MAX_CONNECTIONS must not exceed %d", stream.MaxConnections))
	}
	if c.StreamSubBuffer > stream.MaxSubscriberBuffer {
		errs = append(errs, fmt.Errorf("STREAM_SUB_BUFFER must not exceed %d", stream.MaxSubscriberBuffer))
	}
	if int64(c.StreamMaxConnections)*int64(c.StreamSubBuffer) > stream.MaxSubscriberSlots {
		errs = append(errs, fmt.Errorf("STREAM_MAX_CONNECTIONS*STREAM_SUB_BUFFER must not exceed %d slots", stream.MaxSubscriberSlots))
	}
	if c.AlertPollMaxBackoff < c.AlertPollInterval {
		errs = append(errs, errors.New("ALERT_POLL_MAX_BACKOFF must be at least ALERT_POLL_INTERVAL"))
	}
	if c.RedisAddr != "" {
		host, port, err := net.SplitHostPort(c.RedisAddr)
		n, convErr := strconv.Atoi(port)
		if err != nil || host == "" || convErr != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("REDIS_ADDR must be a valid host:port: %q", c.RedisAddr))
		}
	}
	if c.ReadTimeout <= 0 {
		errs = append(errs, fmt.Errorf("READ_TIMEOUT은 0보다 커야 합니다: %v", c.ReadTimeout))
	}
	if c.WriteTimeout <= 0 {
		errs = append(errs, fmt.Errorf("WRITE_TIMEOUT은 0보다 커야 합니다: %v", c.WriteTimeout))
	}

	switch c.Auth.Mode {
	case "", "none":
	case "mock":
		host, _, err := net.SplitHostPort(c.Auth.MockAddr)
		if err != nil {
			errs = append(errs, fmt.Errorf("AUTH_MOCK_ADDR가 유효한 host:port가 아닙니다: %q", c.Auth.MockAddr))
		} else if ip := net.ParseIP(host); !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			errs = append(errs, fmt.Errorf("AUTH_MOCK_ADDR는 loopback literal 또는 localhost여야 합니다: %q", c.Auth.MockAddr))
		} else if err := validateListenAddr(c.Auth.MockAddr); err != nil {
			errs = append(errs, fmt.Errorf("AUTH_MOCK_ADDR가 유효한 host:port가 아닙니다: %q: %w", c.Auth.MockAddr, err))
		}
	case "oidc":
		u, err := url.Parse(c.Auth.Issuer)
		if err != nil || !u.IsAbs() || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, fmt.Errorf(
				"AUTH_MODE=oidc에는 절대 HTTPS OIDC_ISSUER가 필요합니다: %q", c.Auth.Issuer))
		}
		if c.Auth.Audience == "" {
			errs = append(errs, errors.New("AUTH_MODE=oidc에는 OIDC_AUDIENCE가 필요합니다"))
		}
	default:
		errs = append(errs, fmt.Errorf("알 수 없는 AUTH_MODE %q (none|oidc|mock)", c.Auth.Mode))
	}
	return errors.Join(errs...)
}

func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("port must be an integer between 0 and 65535")
	}
	return nil
}

// Scope는 설정에서 만든 정적 Scope입니다.
func (c Config) Scope() scope.Scope {
	name := c.ClusterName
	if name == "" {
		name = c.ClusterID
	}
	return scope.Scope{Clusters: []scope.Cluster{{
		ID:         c.ClusterID,
		Name:       name,
		Namespaces: c.Namespaces,
		All:        c.AllNS,
	}}}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(k string, def float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// envPairs는 "a=b,c=d" 형식을 맵으로 해석합니다. 잘못된 항목은 무시합니다.
func envPairs(k string) map[string]string {
	v := os.Getenv(k)
	if v == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		key, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || key == "" || val == "" {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return out
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
