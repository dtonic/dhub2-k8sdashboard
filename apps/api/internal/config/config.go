// Package config는 환경변수 하나로 서버 동작을 정합니다.
//
// 기본값은 **클러스터에 가장 안전한 쪽**으로 둡니다. 잘못 설정했을 때
// 대시보드가 느려지는 것은 괜찮지만, 대시보드가 클러스터를 흔드는 것은 안 됩니다.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/auth"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterid"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/registry"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/alertmanager"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	ClusterID    string
	ClusterName  string
	ClusterState ClusterStateConfig

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
	QueryCatalogDir  string
	DashboardBuilder DashboardBuilderConfig

	// ResourceExplorer는 API discovery 기반 Resource Explorer(ADR 0018) 설정입니다.
	// 기본은 비활성이며, 켜지 않으면 관련 엔드포인트는 503으로 남습니다.
	ResourceExplorer ResourceExplorerConfig

	// Greptime은 메트릭 데이터소스(GreptimeDB) 설정입니다. URL이 비어 있으면 미사용입니다.
	Greptime GreptimeConfig
	// Quickwit은 로그 데이터소스 설정입니다. URL이 비어 있으면 미사용입니다.
	Quickwit     QuickwitConfig
	Alertmanager alertmanager.Config

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
	Mode       string
	Issuer     string
	Audience   string
	RolesClaim string
	// RoleMap은 IdP 역할 이름 → 내부 역할 이름 변환표입니다.
	// 예: dhub2-auth의 "dhub2-admin" 그룹을 platform.admin으로 인정합니다.
	RoleMap map[string]string
	// UserinfoRoles는 access token에 역할 클레임이 없는 provider(Dhub2.0 등)를 위해
	// issuer userinfo에서 역할을 보충 조회하는 opt-in입니다.
	UserinfoRoles  bool
	Leeway         time.Duration
	JWKSMinRefresh time.Duration
	// MockAddr은 mock IdP의 바인드 주소입니다. loopback을 벗어나지 마세요.
	MockAddr              string
	SessionEnabled        bool
	SessionEnabledInvalid bool
	PublicOrigin          string
	RedirectURI           string
	ClientID              string
	ClientSecret          string
	SessionKey            string
	SessionIdleTTL        time.Duration
	SessionAbsoluteTTL    time.Duration
	LoginTransactionTTL   time.Duration
	RefreshSkew           time.Duration
	SessionMaxSessions    int
}

type ClusterStateConfig struct {
	Mode                                                          string
	RegistryEndpoint                                              string
	RegistryServerName                                            string
	Clusters                                                      []string
	TrustDomain, CertFile, KeyFile, CAFile                        string
	MaxClusters, MaxResources, MaxChunkResources, MaxMessageBytes int
	StaleTTL, HeartbeatTimeout                                    time.Duration
}

type DashboardBuilderConfig struct {
	Enabled        bool
	DatabaseURL    string
	DBPath         string
	CursorKey      string
	MaxConns       int
	ConnectTimeout time.Duration
	RequireTLS     bool
}

// ResourceExplorerConfig는 Resource Explorer(ADR 0018)가 클러스터에 주는 부하의 상한입니다.
//
// 잘못 설정하면 기동을 막습니다 — allowlist 오타 하나가 조용히 "리소스 없음"으로
// 보이는 것보다 낫습니다. CRD는 언제나 명시적 opt-in입니다.
type ResourceExplorerConfig struct {
	Enabled bool
	// EnabledInvalid는 RESOURCE_EXPLORER_ENABLED가 boolean이 아닐 때입니다.
	EnabledInvalid bool
	// Resources는 "group/version/resource" 목록입니다. 비우면 보수적 기본 목록입니다.
	Resources []string
	AllowCRDs bool
	// Refresh는 discovery snapshot 갱신 주기입니다(1m..24h).
	Refresh time.Duration
	// DetailRate/DetailBurst/DetailConcurrent는 상세 live GET 전용 상한입니다.
	DetailRate       float64
	DetailBurst      int
	DetailConcurrent int
	DetailTimeout    time.Duration
	// MaxObjectBytes는 상세 응답 본문 상한입니다.
	MaxObjectBytes int

	// SearchEnabled는 전역 검색(ADR 0023)의 opt-out 스위치입니다. 기본은 켜짐이며,
	// Resource Explorer 자체가 꺼져 있으면 이 값과 무관하게 검색도 없습니다.
	// 끄면 검색·최근 항목 경로만 사라지고 카탈로그·목록·상세는 그대로입니다.
	SearchEnabled bool
	// SearchInvalid는 RESOURCE_EXPLORER_SEARCH_ENABLED가 boolean이 아닐 때입니다.
	SearchInvalid bool
	// SearchMaxBytes는 **모든 GVR이 동시에 보유하는** 검색 인덱스 바이트 합의 상한입니다.
	SearchMaxBytes int

	// SearchIncremental은 watch 이벤트로 검색 인덱스를 증분 갱신할지입니다(기본 켜짐).
	// 끄면 오늘까지의 경로 그대로 dirty → 전체 검색 재구성으로 돌아갑니다.
	// 검색 자체를 끄는 상위 스위치는 SearchEnabled입니다.
	SearchIncremental bool
	// SearchIncrementalInvalid는 RESOURCE_EXPLORER_SEARCH_INCREMENTAL이 boolean이 아닐 때입니다.
	SearchIncrementalInvalid bool

	/* ── 변경 검토 dry-run (ADR 0019 Phase 1) ─────────────────────────────
	   전부 RESOURCE_EXPLORER_DRY_RUN_* 접두사입니다. 기본은 꺼짐이고 대상 목록도
	   비어 있습니다 — 두 번 opt-in해야 실제로 열립니다. */

	// DryRunEnabled는 기능 스위치입니다. Resource Explorer가 켜져 있어야만 켤 수 있습니다.
	DryRunEnabled bool
	// DryRunEnabledInvalid는 RESOURCE_EXPLORER_DRY_RUN_ENABLED가 boolean이 아닐 때입니다.
	DryRunEnabledInvalid bool
	// DryRunResources는 검토를 허용할 "group/version/resource" 목록입니다.
	// core group은 "core/v1/resource"로 적습니다.
	DryRunResources []string
	// DryRunDenyResources는 위 목록에서 다시 빼는 GVR입니다.
	// **DryRunResources의 부분집합**이어야 합니다 — 목록 밖 항목은 오타로 봅니다.
	DryRunDenyResources []string
	// DryRunTimeout은 검토 한 건(live GET + dry-run patch)의 상한입니다.
	DryRunTimeout time.Duration
	// DryRunRate/DryRunBurst/DryRunConcurrent는 검토 전용 예산입니다.
	DryRunRate       float64
	DryRunBurst      int
	DryRunConcurrent int
	// DryRunMaxManifestBytes는 파싱 전에 적용하는 매니페스트 바이트 상한입니다.
	DryRunMaxManifestBytes int
	// DryRunMaxObjectBytes는 검토 전용 클라이언트의 응답 본문 상한입니다.
	// 상세 조회(MaxObjectBytes)와 별개이며 더 좁습니다.
	DryRunMaxObjectBytes int
}

// UsesSQLite는 SQLite 파일 백엔드(ADR 0016)를 쓰는지 알려줍니다.
// DBPath가 있으면 SQLite, 없고 DatabaseURL이 있으면 PostgreSQL(ADR 0009)입니다.
func (c DashboardBuilderConfig) UsesSQLite() bool { return c.DBPath != "" }

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
	sessionEnabled, sessionEnabledInvalid := strictEnvBool("AUTH_SESSION_ENABLED", false)
	alertmanagerEnabled, alertmanagerEnabledInvalid := strictEnvBool("ALERTMANAGER_ENABLED", false)
	resourcesEnabled, resourcesEnabledInvalid := strictEnvBool("RESOURCE_EXPLORER_ENABLED", false)
	// 전역 검색은 Resource Explorer 안에서만 기본 켜짐입니다. Explorer가 꺼져 있으면
	// 이 값과 무관하게 검색 경로도 없습니다. (ADR 0023 롤백 스위치)
	searchEnabled, searchInvalid := strictEnvBool("RESOURCE_EXPLORER_SEARCH_ENABLED", true)
	// 증분 갱신은 검색 안에서 기본 켜짐입니다. 끄면 dirty → 전체 재구성으로 돌아갑니다.
	searchIncremental, searchIncrementalInvalid := strictEnvBool("RESOURCE_EXPLORER_SEARCH_INCREMENTAL", true)
	// 변경 검토는 Resource Explorer 안에서도 **따로** 켜야 합니다. (ADR 0019 Phase 1)
	dryRunEnabled, dryRunEnabledInvalid := strictEnvBool("RESOURCE_EXPLORER_DRY_RUN_ENABLED", false)
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
		ClusterState:             ClusterStateConfig{Mode: env("CLUSTER_STATE_MODE", "direct"), RegistryEndpoint: env("CLUSTER_STATE_REGISTRY_ENDPOINT", ""), RegistryServerName: env("CLUSTER_STATE_REGISTRY_SERVER_NAME", ""), Clusters: splitCSV(env("CLUSTER_STATE_CLUSTERS", "")), TrustDomain: env("CLUSTER_STATE_TRUST_DOMAIN", ""), CertFile: env("CLUSTER_STATE_TLS_CERT_FILE", ""), KeyFile: env("CLUSTER_STATE_TLS_KEY_FILE", ""), CAFile: env("CLUSTER_STATE_TLS_CA_FILE", ""), MaxClusters: strictEnvInt("CLUSTER_STATE_MAX_CLUSTERS", registry.MaxConfiguredClusters), MaxResources: strictEnvInt("CLUSTER_STATE_MAX_RESOURCES", registry.MaxProjectedResources), MaxChunkResources: strictEnvInt("CLUSTER_STATE_MAX_CHUNK_RESOURCES", registry.MaxSnapshotChunkResources), MaxMessageBytes: strictEnvInt("CLUSTER_STATE_MAX_MESSAGE_BYTES", registry.MaxProtocolMessageBytes), StaleTTL: strictEnvDuration("CLUSTER_STATE_STALE_TTL", 5*time.Minute), HeartbeatTimeout: strictEnvDuration("CLUSTER_STATE_HEARTBEAT_TIMEOUT", 45*time.Second)},
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
		DashboardBuilder: DashboardBuilderConfig{
			Enabled: envBool("DASHBOARD_BUILDER_ENABLED", false), DatabaseURL: env("DATABASE_URL", ""),
			DBPath:    env("DASHBOARD_DB_PATH", ""),
			CursorKey: env("DASHBOARD_CURSOR_KEY", ""), MaxConns: envInt("DASHBOARD_DB_MAX_CONNS", 8),
			ConnectTimeout: envDuration("DASHBOARD_DB_CONNECT_TIMEOUT", 5*time.Second),
			RequireTLS:     envBool("DASHBOARD_DB_REQUIRE_TLS", false),
		},
		ResourceExplorer: ResourceExplorerConfig{
			Enabled: resourcesEnabled, EnabledInvalid: resourcesEnabledInvalid,
			Resources:        splitCSV(env("RESOURCE_EXPLORER_RESOURCES", "")),
			AllowCRDs:        envBool("RESOURCE_EXPLORER_ALLOW_CRDS", false),
			Refresh:          strictEnvDuration("RESOURCE_EXPLORER_REFRESH", 10*time.Minute),
			DetailRate:       envFloat("RESOURCE_EXPLORER_DETAIL_RATE", 2),
			DetailBurst:      strictEnvInt("RESOURCE_EXPLORER_DETAIL_BURST", 5),
			DetailConcurrent: strictEnvInt("RESOURCE_EXPLORER_DETAIL_CONCURRENT", 2),
			DetailTimeout:    strictEnvDuration("RESOURCE_EXPLORER_DETAIL_TIMEOUT", 5*time.Second),
			MaxObjectBytes:   strictEnvInt("RESOURCE_EXPLORER_MAX_OBJECT_BYTES", 1<<20),
			SearchEnabled:    searchEnabled,
			SearchInvalid:    searchInvalid,
			SearchMaxBytes:   strictEnvInt("RESOURCE_EXPLORER_SEARCH_MAX_BYTES", resourcecatalog.DefaultMaxSearchIndexBytes),

			SearchIncremental:        searchIncremental,
			SearchIncrementalInvalid: searchIncrementalInvalid,

			DryRunEnabled:          dryRunEnabled,
			DryRunEnabledInvalid:   dryRunEnabledInvalid,
			DryRunResources:        splitCSV(env("RESOURCE_EXPLORER_DRY_RUN_RESOURCES", "")),
			DryRunDenyResources:    splitCSV(env("RESOURCE_EXPLORER_DRY_RUN_DENY_RESOURCES", "")),
			DryRunTimeout:          strictEnvDuration("RESOURCE_EXPLORER_DRY_RUN_TIMEOUT", 8*time.Second),
			DryRunRate:             strictEnvFloat("RESOURCE_EXPLORER_DRY_RUN_RATE", 1),
			DryRunBurst:            strictEnvInt("RESOURCE_EXPLORER_DRY_RUN_BURST", 3),
			DryRunConcurrent:       strictEnvInt("RESOURCE_EXPLORER_DRY_RUN_CONCURRENT", 1),
			DryRunMaxManifestBytes: strictEnvInt("RESOURCE_EXPLORER_DRY_RUN_MAX_MANIFEST_BYTES", contract.DefaultDryRunManifestBytes),
			DryRunMaxObjectBytes:   strictEnvInt("RESOURCE_EXPLORER_DRY_RUN_MAX_OBJECT_BYTES", resourcecatalog.DefaultMaxObjectBytes),
		},
		Auth: AuthConfig{
			Mode:                  env("AUTH_MODE", "none"),
			Issuer:                env("OIDC_ISSUER", ""),
			Audience:              env("OIDC_AUDIENCE", ""),
			RolesClaim:            env("OIDC_ROLES_CLAIM", "roles"),
			RoleMap:               envPairs("OIDC_ROLE_MAP"),
			UserinfoRoles:         envBool("OIDC_USERINFO_ROLES", false),
			Leeway:                envDuration("OIDC_LEEWAY", time.Minute),
			JWKSMinRefresh:        envDuration("OIDC_JWKS_MIN_REFRESH", 5*time.Minute),
			MockAddr:              env("AUTH_MOCK_ADDR", "127.0.0.1:8091"),
			SessionEnabled:        sessionEnabled,
			SessionEnabledInvalid: sessionEnabledInvalid,
			PublicOrigin:          env("AUTH_PUBLIC_ORIGIN", ""),
			RedirectURI:           env("OIDC_REDIRECT_URI", ""),
			ClientID:              env("OIDC_CLIENT_ID", ""),
			ClientSecret:          env("OIDC_CLIENT_SECRET", ""),
			SessionKey:            env("AUTH_SESSION_KEY", ""),
			SessionIdleTTL:        strictEnvDuration("AUTH_SESSION_IDLE_TTL", 30*time.Minute),
			SessionAbsoluteTTL:    strictEnvDuration("AUTH_SESSION_ABSOLUTE_TTL", 8*time.Hour),
			LoginTransactionTTL:   strictEnvDuration("AUTH_LOGIN_TTL", 5*time.Minute),
			RefreshSkew:           strictEnvDuration("AUTH_REFRESH_SKEW", 2*time.Minute),
			SessionMaxSessions:    strictEnvInt("AUTH_SESSION_MAX", auth.DefaultMaxSessions),
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
		Alertmanager: alertmanager.Config{
			Enabled: alertmanagerEnabled, EnabledInvalid: alertmanagerEnabledInvalid,
			BaseURL: env("ALERTMANAGER_URL", ""), PublicURL: env("ALERTMANAGER_PUBLIC_URL", ""),
			TokenFile: env("ALERTMANAGER_TOKEN_FILE", ""), CAFile: env("ALERTMANAGER_CA_FILE", ""),
			ClientCertFile: env("ALERTMANAGER_CLIENT_CERT_FILE", ""), ClientKeyFile: env("ALERTMANAGER_CLIENT_KEY_FILE", ""),
			ServerName: env("ALERTMANAGER_SERVER_NAME", ""), ClusterLabel: env("ALERTMANAGER_CLUSTER_LABEL", "k8s_cluster_name"), NamespaceLabel: env("ALERTMANAGER_NAMESPACE_LABEL", "namespace"),
			Timeout: strictEnvDuration("ALERTMANAGER_TIMEOUT", 5*time.Second), MaxBodyBytes: int64(strictEnvInt("ALERTMANAGER_MAX_BODY_BYTES", 4<<20)),
			MaxAlerts: strictEnvInt("ALERTMANAGER_MAX_ALERTS", 2000), MaxConcurrent: strictEnvInt("ALERTMANAGER_MAX_CONCURRENT", 4),
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
	if err := alertmanager.Validate(c.Alertmanager); err != nil {
		errs = append(errs, err)
	}
	if c.Auth.SessionEnabledInvalid {
		errs = append(errs, errors.New("AUTH_SESSION_ENABLED must be a boolean"))
	}
	if c.Auth.SessionEnabled && c.Auth.Mode != "oidc" {
		errs = append(errs, errors.New("AUTH_SESSION_ENABLED에는 AUTH_MODE=oidc가 필요합니다"))
	}
	if c.ClusterState.Mode != "direct" && c.ClusterState.Mode != "central" {
		errs = append(errs, errors.New("CLUSTER_STATE_MODE must be direct or central"))
	}
	if c.ClusterState.Mode == "central" {
		host, port, e := net.SplitHostPort(c.ClusterState.RegistryEndpoint)
		pn, pe := strconv.Atoi(port)
		if e != nil || pe != nil || host == "" || !clusterid.ValidHost(host) || pn < 1 || pn > 65535 {
			errs = append(errs, errors.New("CLUSTER_STATE_REGISTRY_ENDPOINT must be host:port"))
		}
		if c.ClusterState.RegistryServerName == "" || strings.ContainsAny(c.ClusterState.RegistryServerName, "/: *\t\r\n") || !clusterid.ValidHost(c.ClusterState.RegistryServerName) {
			errs = append(errs, errors.New("CLUSTER_STATE_REGISTRY_SERVER_NAME is required"))
		}
		if len(c.ClusterState.Clusters) == 0 || len(c.ClusterState.Clusters) > c.ClusterState.MaxClusters {
			errs = append(errs, errors.New("CLUSTER_STATE_CLUSTERS count is invalid"))
		}
		seen := map[string]bool{}
		for _, id := range c.ClusterState.Clusters {
			if seen[id] || !clusterid.Valid(id) {
				errs = append(errs, errors.New("CLUSTER_STATE_CLUSTERS contains duplicate or invalid ID"))
			}
			seen[id] = true
		}
		if c.Auth.Mode != "oidc" {
			errs = append(errs, errors.New("central mode requires OIDC authentication"))
		}
		if c.UseDemoData {
			errs = append(errs, errors.New("central mode forbids demo data sources"))
		}
		if c.Quickwit.URL != "" && c.Quickwit.Fields["cluster"] != "resource_attributes.k8s.cluster.name" {
			errs = append(errs, errors.New("central Quickwit requires cluster=resource_attributes.k8s.cluster.name"))
		}
		if c.ClusterState.TrustDomain == "" || strings.ContainsAny(c.ClusterState.TrustDomain, "/: ") {
			errs = append(errs, errors.New("central mode requires a valid trust domain"))
		}
		for _, v := range []string{c.ClusterState.CertFile, c.ClusterState.KeyFile, c.ClusterState.CAFile} {
			if v == "" || !filepath.IsAbs(v) {
				errs = append(errs, errors.New("central mode requires absolute existing TLS file paths"))
				break
			}
		}
	}
	if c.ClusterState.Mode == "central" && (c.ClusterState.MaxClusters < 1 || c.ClusterState.MaxClusters > registry.MaxConfiguredClusters || c.ClusterState.MaxResources < 1 || c.ClusterState.MaxResources > registry.MaxProjectedResources || c.ClusterState.MaxChunkResources < 1 || c.ClusterState.MaxChunkResources > registry.MaxSnapshotChunkResources || c.ClusterState.MaxChunkResources > c.ClusterState.MaxResources || c.ClusterState.MaxMessageBytes < registry.MinProtocolMessageBytes || c.ClusterState.MaxMessageBytes > registry.MaxProtocolMessageBytes || c.ClusterState.StaleTTL <= 0 || c.ClusterState.StaleTTL > registry.MaxStaleTTL || c.ClusterState.HeartbeatTimeout <= 0 || c.ClusterState.HeartbeatTimeout > registry.MaxHeartbeatTimeout || c.ClusterState.HeartbeatTimeout > c.ClusterState.StaleTTL) {
		errs = append(errs, errors.New("invalid cluster-state limits"))
	}
	if c.DashboardBuilder.Enabled {
		// SQLite(ADR 0016)와 PostgreSQL(ADR 0009)은 배타적입니다. 둘 다 주면 의도가 모호하므로 실패시킵니다.
		if c.DashboardBuilder.DBPath != "" && c.DashboardBuilder.DatabaseURL != "" {
			errs = append(errs, errors.New("set only one of DASHBOARD_DB_PATH (SQLite) or DATABASE_URL (PostgreSQL)"))
		}
		if c.DashboardBuilder.DBPath == "" && c.DashboardBuilder.DatabaseURL == "" {
			errs = append(errs, errors.New("DASHBOARD_DB_PATH (SQLite) or DATABASE_URL (PostgreSQL) is required when dashboard builder is enabled"))
		}
		if len(c.DashboardBuilder.CursorKey) < 32 {
			errs = append(errs, errors.New("DASHBOARD_CURSOR_KEY must contain at least 32 bytes"))
		}
		if c.DashboardBuilder.MaxConns < 1 || c.DashboardBuilder.MaxConns > 32 {
			errs = append(errs, errors.New("DASHBOARD_DB_MAX_CONNS must be between 1 and 32"))
		}
		if c.DashboardBuilder.ConnectTimeout <= 0 || c.DashboardBuilder.ConnectTimeout > 30*time.Second {
			errs = append(errs, errors.New("DASHBOARD_DB_CONNECT_TIMEOUT must be between 0 and 30s"))
		}
	}
	if c.ResourceExplorer.EnabledInvalid {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_ENABLED must be a boolean"))
	}
	if c.ResourceExplorer.SearchInvalid {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_SEARCH_ENABLED must be a boolean"))
	}
	if c.ResourceExplorer.SearchIncrementalInvalid {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_SEARCH_INCREMENTAL must be a boolean"))
	}
	// 기능 스위치의 파싱 실패는 **언제나** 기동을 막습니다. 조용히 꺼진 것으로
	// 접으면 운영자는 켰다고 믿는데 실제로는 없는 상태가 됩니다.
	if c.ResourceExplorer.DryRunEnabledInvalid {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_DRY_RUN_ENABLED must be a boolean"))
	}
	// 이 검사는 Enabled 블록 **밖**이어야 합니다. 안에 두면 Explorer가 꺼진 채
	// 검토만 켠 설정이 검사 자체를 건너뛰고 통과합니다.
	// central 조합도 이 경로로 막힙니다 — Explorer가 켜져 있으면
	// validateResourceExplorer가, 꺼져 있으면 아래가 잡습니다.
	if c.ResourceExplorer.DryRunEnabled && !c.ResourceExplorer.Enabled {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_DRY_RUN_ENABLED requires RESOURCE_EXPLORER_ENABLED"))
	}
	if c.ResourceExplorer.Enabled {
		errs = append(errs, c.validateResourceExplorer()...)
	}
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
		if err != nil || !validOIDCIssuer(u) {
			errs = append(errs, fmt.Errorf(
				"AUTH_MODE=oidc에는 절대 HTTPS OIDC_ISSUER가 필요합니다: %q", c.Auth.Issuer))
		}
		if c.Auth.Audience == "" {
			errs = append(errs, errors.New("AUTH_MODE=oidc에는 OIDC_AUDIENCE가 필요합니다"))
		}
		if c.Auth.SessionEnabled {
			origin, originErr := url.Parse(c.Auth.PublicOrigin)
			if originErr != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
				errs = append(errs, errors.New("AUTH_SESSION_ENABLED에는 경로 없는 HTTPS AUTH_PUBLIC_ORIGIN이 필요합니다"))
			}
			if c.Auth.RedirectURI != strings.TrimSuffix(c.Auth.PublicOrigin, "/")+"/api/v1/auth/callback" {
				errs = append(errs, errors.New("OIDC_REDIRECT_URI는 AUTH_PUBLIC_ORIGIN callback과 정확히 같아야 합니다"))
			}
			key, keyErr := base64.RawURLEncoding.DecodeString(c.Auth.SessionKey)
			if c.Auth.ClientID == "" || c.RedisAddr == "" || keyErr != nil || len(key) != 32 {
				errs = append(errs, errors.New("브라우저 세션에는 OIDC_CLIENT_ID, 공유 REDIS_ADDR, 32-byte base64url AUTH_SESSION_KEY가 필요합니다"))
			}
			if c.Auth.SessionIdleTTL <= 0 || c.Auth.SessionIdleTTL > auth.MaxSessionIdleTTL || c.Auth.SessionAbsoluteTTL <= 0 || c.Auth.SessionAbsoluteTTL > auth.MaxSessionAbsoluteTTL || c.Auth.SessionIdleTTL > c.Auth.SessionAbsoluteTTL || c.Auth.LoginTransactionTTL <= 0 || c.Auth.LoginTransactionTTL > auth.MaxLoginTransactionTTL || c.Auth.RefreshSkew <= 0 || c.Auth.RefreshSkew > auth.MaxRefreshSkew {
				errs = append(errs, errors.New("브라우저 세션 TTL 설정이 유효하지 않습니다"))
			}
			if c.Auth.SessionMaxSessions < 1 || c.Auth.SessionMaxSessions > auth.MaxSessions {
				errs = append(errs, errors.New("AUTH_SESSION_MAX must be between 1 and 100000"))
			}
		}
	default:
		errs = append(errs, fmt.Errorf("알 수 없는 AUTH_MODE %q (none|oidc|mock)", c.Auth.Mode))
	}
	return errors.Join(errs...)
}

// ResourceAllowlist는 Resource Explorer가 informer를 붙일 GVR 목록입니다. (ADR 0018)
//
// 비우면 보수적 기본 목록을 씁니다. CRD group은 RESOURCE_EXPLORER_ALLOW_CRDS 없이는
// 통과하지 못합니다 — 오타 하나가 조용히 "리소스 없음"이 되는 편보다 기동 실패가 낫습니다.
func (c Config) ResourceAllowlist() ([]schema.GroupVersionResource, error) {
	raw := c.ResourceExplorer.Resources
	if len(raw) == 0 {
		return resourcecatalog.NormalizeAllowlist(resourcecatalog.DefaultAllowlist(), c.ResourceExplorer.AllowCRDs)
	}
	out := make([]schema.GroupVersionResource, 0, len(raw))
	for _, entry := range raw {
		gvr, err := resourcecatalog.ParseGVR(entry)
		if err != nil {
			return nil, fmt.Errorf("RESOURCE_EXPLORER_RESOURCES: %w", err)
		}
		out = append(out, gvr)
	}
	list, err := resourcecatalog.NormalizeAllowlist(out, c.ResourceExplorer.AllowCRDs)
	if err != nil {
		return nil, fmt.Errorf("RESOURCE_EXPLORER_RESOURCES: %w", err)
	}
	return list, nil
}

// ResourceDryRunAllowlist는 변경 검토 대상의 **최종** 목록입니다. (ADR 0019 Phase 1)
//
// deny는 여기서 **정확히 한 번** 적용됩니다. 호출자는 반환값을 그대로 쓰고 deny를
// 다시 넘기지 않습니다 — 두 곳에서 빼면 어느 쪽이 실제로 적용됐는지 알 수 없고,
// 한쪽만 고치면 조용히 우회가 생깁니다.
//
// hard-deny(core secrets·serviceaccounts·nodes·namespaces, RBAC group 전체,
// apiextensions CRD)는 core의 NormalizeDryRunAllowlist가 판정합니다. 이 함수는
// 더 넓은 group·kind 규칙을 새로 만들지 않습니다.
func (c Config) ResourceDryRunAllowlist() ([]schema.GroupVersionResource, error) {
	explorer, err := c.ResourceAllowlist()
	if err != nil {
		return nil, err
	}
	allow := make([]schema.GroupVersionResource, 0, len(c.ResourceExplorer.DryRunResources))
	for _, entry := range c.ResourceExplorer.DryRunResources {
		gvr, parseErr := resourcecatalog.ParseGVR(entry)
		if parseErr != nil {
			return nil, fmt.Errorf("RESOURCE_EXPLORER_DRY_RUN_RESOURCES: %w", parseErr)
		}
		allow = append(allow, gvr)
	}
	// deny는 **opt-in 목록의 부분집합**이어야 합니다. Explorer 전체 allowlist가
	// 기준이 아닙니다 — 검토 대상이 아닌 것을 빼겠다고 적는 것은 오타입니다.
	inAllow := make(map[schema.GroupVersionResource]bool, len(allow))
	for _, gvr := range allow {
		inAllow[gvr] = true
	}
	deny := make([]schema.GroupVersionResource, 0, len(c.ResourceExplorer.DryRunDenyResources))
	for _, entry := range c.ResourceExplorer.DryRunDenyResources {
		gvr, parseErr := resourcecatalog.ParseGVR(entry)
		if parseErr != nil {
			return nil, fmt.Errorf("RESOURCE_EXPLORER_DRY_RUN_DENY_RESOURCES: %w", parseErr)
		}
		if !inAllow[gvr] {
			return nil, fmt.Errorf("RESOURCE_EXPLORER_DRY_RUN_DENY_RESOURCES: %s는 RESOURCE_EXPLORER_DRY_RUN_RESOURCES에 없습니다",
				resourcecatalog.FormatGVR(gvr))
		}
		deny = append(deny, gvr)
	}
	final, err := resourcecatalog.NormalizeDryRunAllowlist(allow, explorer, deny)
	if err != nil {
		return nil, fmt.Errorf("RESOURCE_EXPLORER_DRY_RUN_RESOURCES: %w", err)
	}
	return final, nil
}

// validateResourceDryRun은 활성화된 변경 검토 설정의 상한을 검사합니다.
// 조용한 clamp는 없습니다 — 상한을 벗어나면 기동이 실패합니다.
func (c Config) validateResourceDryRun() []error {
	var errs []error
	d := c.ResourceExplorer
	if d.DryRunTimeout <= 0 || d.DryRunTimeout > 30*time.Second {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_DRY_RUN_TIMEOUT must be between 0 and 30s"))
	}
	// NaN·±Inf는 <=0 과 >10 비교를 **둘 다** 통과합니다. 그대로 두면 검토 token
	// bucket이 fail-open 됩니다. 파싱 실패도 NaN으로 들어오므로 같이 걸립니다.
	if math.IsNaN(d.DryRunRate) || math.IsInf(d.DryRunRate, 0) || d.DryRunRate <= 0 || d.DryRunRate > 10 {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_DRY_RUN_RATE must be a finite value between 0 and 10"))
	}
	if d.DryRunBurst < 1 || d.DryRunBurst > 20 {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_DRY_RUN_BURST must be between 1 and 20"))
	}
	if d.DryRunConcurrent < 1 || d.DryRunConcurrent > 4 {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_DRY_RUN_CONCURRENT must be between 1 and 4"))
	}
	if d.DryRunMaxManifestBytes < 4096 || d.DryRunMaxManifestBytes > contract.MaxDryRunManifestBytes {
		errs = append(errs, fmt.Errorf("RESOURCE_EXPLORER_DRY_RUN_MAX_MANIFEST_BYTES must be between 4096 and %d",
			contract.MaxDryRunManifestBytes))
	}
	if d.DryRunMaxObjectBytes < 4096 || d.DryRunMaxObjectBytes > resourcecatalog.DefaultMaxObjectBytes {
		errs = append(errs, fmt.Errorf("RESOURCE_EXPLORER_DRY_RUN_MAX_OBJECT_BYTES must be between 4096 and %d",
			resourcecatalog.DefaultMaxObjectBytes))
	}
	final, err := c.ResourceDryRunAllowlist()
	if err != nil {
		errs = append(errs, err)
		return errs
	}
	// 켜 놓고 대상이 없으면 기능이 있는 줄 알고 쓰다가 매번 403을 봅니다.
	// deny로 전부 빠진 경우도 여기서 걸립니다.
	if len(final) < 1 || len(final) > resourcecatalog.MaxDryRunAllowlistEntries {
		errs = append(errs, fmt.Errorf("RESOURCE_EXPLORER_DRY_RUN_RESOURCES must resolve to between 1 and %d resources",
			resourcecatalog.MaxDryRunAllowlistEntries))
	}
	return errs
}

// validateResourceExplorer는 활성화된 Resource Explorer 설정의 상한을 검사합니다.
func (c Config) validateResourceExplorer() []error {
	var errs []error
	if _, err := c.ResourceAllowlist(); err != nil {
		errs = append(errs, err)
	}
	if c.ResourceExplorer.Refresh < resourcecatalog.MinRefreshInterval || c.ResourceExplorer.Refresh > resourcecatalog.MaxRefreshInterval {
		errs = append(errs, fmt.Errorf("RESOURCE_EXPLORER_REFRESH must be between %v and %v", resourcecatalog.MinRefreshInterval, resourcecatalog.MaxRefreshInterval))
	}
	// NaN은 <=0 과 >100 비교를 **둘 다** 통과합니다. 그대로 두면 token bucket의
	// tokens가 NaN이 되어 `tokens < 1` 검사가 영원히 false — 상세 조회 rate limit이
	// fail-open 됩니다. 그래서 명시적으로 거절합니다.
	if math.IsNaN(c.ResourceExplorer.DetailRate) || c.ResourceExplorer.DetailRate <= 0 || c.ResourceExplorer.DetailRate > 100 {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_DETAIL_RATE must be a finite value between 0 and 100"))
	}
	if c.ResourceExplorer.DetailBurst < 1 || c.ResourceExplorer.DetailBurst > 100 {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_DETAIL_BURST must be between 1 and 100"))
	}
	// 검색 인덱스 상한은 **서비스 전체** 보유량입니다. 너무 작으면 큰 리소스가 통째로
	// 검색에서 빠지고, 너무 크면 프로세스 메모리를 삼킵니다. 둘 다 기동 시점에 막습니다.
	if int64(c.ResourceExplorer.SearchMaxBytes) < resourcecatalog.MinMaxSearchIndexBytes ||
		int64(c.ResourceExplorer.SearchMaxBytes) > resourcecatalog.MaxMaxSearchIndexBytes {
		errs = append(errs, fmt.Errorf("RESOURCE_EXPLORER_SEARCH_MAX_BYTES must be between %d and %d",
			resourcecatalog.MinMaxSearchIndexBytes, resourcecatalog.MaxMaxSearchIndexBytes))
	}
	if c.ResourceExplorer.DetailConcurrent < 1 || c.ResourceExplorer.DetailConcurrent > 16 {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_DETAIL_CONCURRENT must be between 1 and 16"))
	}
	if c.ResourceExplorer.DetailTimeout <= 0 || c.ResourceExplorer.DetailTimeout > 30*time.Second {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_DETAIL_TIMEOUT must be between 0 and 30s"))
	}
	if c.ResourceExplorer.MaxObjectBytes < 4096 || c.ResourceExplorer.MaxObjectBytes > 16<<20 {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_MAX_OBJECT_BYTES must be between 4096 and 16777216"))
	}
	// central 모드는 로컬 informer도 kubeconfig도 없습니다. 켜졌다면 설정 오류입니다.
	if c.ClusterState.Mode != "direct" {
		errs = append(errs, errors.New("RESOURCE_EXPLORER_ENABLED requires CLUSTER_STATE_MODE=direct"))
	}
	// 검토는 Explorer 안에서만 존재하므로 여기서 이어 검사합니다. Explorer가 꺼져
	// 있으면 이 함수 자체가 불리지 않고, 그 조합은 Validate가 따로 막습니다.
	if c.ResourceExplorer.DryRunEnabled {
		errs = append(errs, c.validateResourceDryRun()...)
	}
	return errs
}

func validOIDCIssuer(u *url.URL) bool {
	if u == nil || !u.IsAbs() || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return false
	}
	if u.Path == "" || u.Path == "/" {
		return true
	}
	return pathpkg.Clean(u.Path) == strings.TrimSuffix(u.Path, "/")
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
	// AUTH_MODE=none은 개발·데모 전용이므로 토폴로지 편집·워크로드 관리·대시보드 편집/발행과
	// Resource Explorer를 모두 허용합니다. (#28, #32, ADR 0016, ADR 0018)
	// draft 소유·감사에는 Subject가 필요하므로 고정값을 둡니다.
	return scope.Scope{
		Subject:             "local",
		CanEditTopology:     true,
		CanManageWorkloads:  true,
		CanExploreResources: true,
		CanEditDashboard:    true,
		CanPublishDashboard: true,
		Clusters: []scope.Cluster{{
			ID:         c.ClusterID,
			Name:       name,
			Namespaces: c.Namespaces,
			All:        c.AllNS,
		}},
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitCSV(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		x = strings.TrimSpace(x)
		if x != "" {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
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

func strictEnvBool(k string, def bool) (bool, bool) {
	v := os.Getenv(k)
	if v == "" {
		return def, false
	}
	b, err := strconv.ParseBool(v)
	return b, err != nil
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

func strictEnvInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
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

// strictEnvFloat는 파싱 실패를 조용한 기본값으로 덮지 않습니다.
//
// 실패하면 NaN을 돌려줍니다. 검증이 "유한한 값"을 요구하므로, 오타(`abc`)와 리터럴
// `NaN`·`Inf`가 **같은 거절 경로**로 수렴합니다. rate가 조용히 기본값으로 흐르면
// 운영자는 자기가 적은 값이 적용된 줄 알고, token bucket은 다른 예산으로 돕니다.
func strictEnvFloat(k string, def float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return math.NaN()
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

func strictEnvDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return -1
	}
	return d
}
