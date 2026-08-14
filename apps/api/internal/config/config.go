// Package config는 환경변수 하나로 서버 동작을 정합니다.
//
// 기본값은 **클러스터에 가장 안전한 쪽**으로 둡니다. 잘못 설정했을 때
// 대시보드가 느려지는 것은 괜찮지만, 대시보드가 클러스터를 흔드는 것은 안 됩니다.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
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
	CacheTTL time.Duration

	// UseDemoData는 GreptimeDB/Quickwit/Alertmanager 없이 결정적 값을 씁니다.
	// 실주소(GREPTIME_URL·QUICKWIT_URL)가 설정된 데이터소스는 이 값과 무관하게
	// 실제 어댑터를 씁니다 — 주소를 적은 것이 의도이기 때문입니다.
	UseDemoData bool

	// Greptime은 메트릭 데이터소스(GreptimeDB) 설정입니다. URL이 비어 있으면 미사용입니다.
	Greptime GreptimeConfig
	// Quickwit은 로그 데이터소스 설정입니다. URL이 비어 있으면 미사용입니다.
	Quickwit QuickwitConfig

	// AllowedOrigin은 개발 중 Vite 오리진을 허용할 때 씁니다. 비어 있으면 CORS 헤더를 붙이지 않습니다.
	AllowedOrigin string

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
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
		Addr:               env("ADDR", ":8080"),
		Kubeconfig:         env("KUBECONFIG", ""),
		DisableProtobuf:    envBool("K8S_DISABLE_PROTOBUF", false),
		QPS:                float32(envFloat("K8S_QPS", 20)),
		Burst:              envInt("K8S_BURST", 30),
		Resync:             envDuration("K8S_RESYNC", 10*time.Minute),
		EventFieldSelector: env("K8S_EVENT_FIELD_SELECTOR", "type=Warning"),
		ClusterID:          env("CLUSTER_ID", "default"),
		ClusterName:        env("CLUSTER_NAME", ""),
		Namespaces:         nsList,
		AllNS:              all,
		CacheTTL:           envDuration("CACHE_TTL", 5*time.Second),
		UseDemoData:        envBool("USE_DEMO_DATA", true),
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
		AllowedOrigin:      env("ALLOWED_ORIGIN", ""),
		ReadTimeout:        envDuration("READ_TIMEOUT", 15*time.Second),
		WriteTimeout:       envDuration("WRITE_TIMEOUT", 30*time.Second),
	}
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
