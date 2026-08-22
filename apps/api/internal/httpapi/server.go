// Package httpapi는 화면 단위 엔드포인트를 제공합니다.
//
// 규칙은 세 가지입니다.
//   - **화면 하나 = 요청 하나.** 위젯마다 엔드포인트를 만들지 않습니다. (ADR 0002)
//   - **Scope는 서버가 강제합니다.** 요청의 cluster/namespace는 힌트일 뿐입니다. (README §10)
//   - **부분 장애는 섹션 값으로 표현합니다.** 한 데이터소스가 죽어도 화면은 삽니다.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/auth"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterid"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/dashboard"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/observability"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/queryprotect"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/timerange"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/topologylayout"
	"k8s.io/client-go/kubernetes"
)

// Deps는 서버가 쓰는 바깥 세계 전부입니다. 테스트에서는 전부 대체할 수 있습니다.
type Deps struct {
	Store             clusterstate.Provider
	ProviderRegistry  clusterstate.ProviderRegistry
	ScopeNamespaces   clusterstate.NamespaceCatalog
	Metrics           datasource.Metrics
	Logs              datasource.Logs
	Alerts            datasource.Alerts
	Topology          datasource.Topology
	Resolver          scope.Resolver
	Cache             *cache.TTL
	Guard             *queryprotect.Guard
	ProtectionMetrics *queryprotect.Metrics
	Observability     *observability.Metrics
	PlannedQueryRefs  []string
	CacheTTL          cache.TTLPolicy
	Logger            *slog.Logger
	// Stream은 상태 변경 SSE 허브입니다. nil이면 SSE 경로는 503을 반환합니다.
	Stream *stream.Hub
	// StreamMetrics는 /metrics로 나가는 스트림 계측입니다. Stream을 직접 만들어
	// 넘길 때는 그 허브의 Observer와 같은 인스턴스여야 값이 한 곳에 모입니다.
	StreamMetrics *stream.Metrics
	// StreamOptions는 SSE 전송 동작(heartbeat·write 유휴 상한·재연결 힌트)입니다.
	StreamOptions      StreamOptions
	DashboardStore     dashboard.Store
	DashboardQueryRefs []string
	// TopologyLayout은 공유 토폴로지 배치 저장소입니다. nil이면 배치 저장이 503입니다. (#28)
	TopologyLayout *topologylayout.Store
	// KubeClient는 관리(Deployment/Secret write) 전용 clientset입니다. direct 모드에서만
	// 설정되며 nil이면 관리 엔드포인트가 503입니다. 조회 경로는 쓰지 않습니다. (ADR 0014, #32)
	KubeClient kubernetes.Interface
	// Resources는 direct 모드의 Resource Explorer 서비스입니다. central 모드는 nil이며
	// 관련 엔드포인트가 권한 판정 뒤 안정적으로 503을 돌려줍니다. (ADR 0018)
	Resources *resourcecatalog.Service
	// AllowedOrigin이 있으면 CORS 헤더를 붙입니다. 개발 중 Vite 오리진용입니다.
	AllowedOrigin string
	// Now는 테스트에서 시간을 고정합니다.
	Now func() time.Time
	// Version은 GET /version 응답입니다. 값은 cmd/api가 ldflags로 채웁니다. (#5)
	Version contract.VersionInfo
	// NewRequestID는 테스트에서 요청 ID 생성을 고정합니다. 기본은 crypto/rand입니다.
	NewRequestID func() string
}

type Server struct {
	deps      Deps
	mux       *http.ServeMux
	authGuard *authGuard
}

var errQueryBudget = errors.New("server query budget exceeded")

func NewServer(d Deps) *Server {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Cache == nil {
		d.Cache = cache.NewTTL(5 * time.Second)
	}
	if d.ProtectionMetrics == nil {
		d.ProtectionMetrics = queryprotect.NewMetrics()
	}
	if d.Observability == nil {
		d.Observability = observability.New()
	}
	if d.Guard == nil {
		d.Guard = queryprotect.New(queryprotect.DefaultConfig(), d.ProtectionMetrics)
	}
	if d.CacheTTL.State <= 0 {
		d.CacheTTL = cache.TTLPolicy{State: 5 * time.Second, Short: 30 * time.Second, Historical: 10 * time.Minute, HistoricalSafety: 5 * time.Minute}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.NewRequestID == nil {
		d.NewRequestID = newRequestID
	}
	if d.StreamMetrics == nil {
		d.StreamMetrics = stream.NewMetrics()
	}
	d.StreamOptions.setDefaults()
	if d.ProviderRegistry == nil {
		d.ProviderRegistry = localProviderRegistry{provider: d.Store}
	}
	s := &Server{deps: d, mux: http.NewServeMux(), authGuard: newAuthGuard(d.Now)}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := s.mux
	if routes, ok := s.deps.Resolver.(interface{ RegisterAuthRoutes(*http.ServeMux) }); ok {
		routes.RegisterAuthRoutes(m)
	}
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// readyz는 informer 캐시가 채워진 뒤에만 통과합니다.
	// 동기화 전에 트래픽을 받으면 "Pod 0개"가 정상처럼 보입니다.
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.deps.ProviderRegistry.Ready() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "syncing"})
			return
		}
		if s.deps.DashboardStore != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if s.deps.DashboardStore.Ready(ctx) != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "dashboard-store-unavailable"})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	m.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.deps.Version)
	})
	m.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = s.deps.ProtectionMetrics.WritePrometheus(r.Context(), w)
		if s.deps.StreamMetrics != nil {
			_ = s.deps.StreamMetrics.WritePrometheus(w)
		}
		// 검색 인덱스 보유·정점 바이트는 운영이 예산을 확인하는 유일한 창입니다. (ADR 0023)
		_ = s.deps.Resources.WriteSearchMetrics(w)
		s.deps.Observability.SetInformerSynced(s.deps.ProviderRegistry.Ready())
		_ = s.deps.Observability.WritePrometheus(w)
	})

	m.HandleFunc("GET /api/v1/scope", s.handleScope)
	m.HandleFunc("GET /api/v1/dashboard-capabilities", s.handleDashboardCapabilities)
	m.HandleFunc("GET /api/v1/dashboard-drafts", s.handleDashboardList)
	m.HandleFunc("POST /api/v1/dashboard-drafts", s.handleDashboardCreate)
	m.HandleFunc("POST /api/v1/dashboard-drafts/import", s.handleDashboardImport)
	m.HandleFunc("GET /api/v1/dashboard-drafts/{id}", s.handleDashboardGet)
	m.HandleFunc("PUT /api/v1/dashboard-drafts/{id}", s.handleDashboardUpdate)
	m.HandleFunc("DELETE /api/v1/dashboard-drafts/{id}", s.handleDashboardDelete)
	m.HandleFunc("POST /api/v1/dashboard-drafts/{id}/submit", s.handleDashboardSubmit)
	m.HandleFunc("POST /api/v1/dashboard-drafts/{id}/approve", s.handleDashboardApprove)
	m.HandleFunc("POST /api/v1/dashboard-drafts/{id}/clone", s.handleDashboardClone)
	m.HandleFunc("GET /api/v1/dashboard-drafts/{id}/export", s.handleDashboardExport)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/overview", s.withProvider("overview", s.handleOverview))
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/nodes", s.withProvider("nodes", s.handleNodes))
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/namespaces", s.withProvider("namespace-list", s.handleNamespaceList))
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/namespaces/{namespace}", s.withProvider("namespace", s.handleNamespaceDetail))
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/workloads/{kind}/{name}", s.withProvider("workload", s.handleWorkloadDetail))
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/pods/{name}", s.withProvider("pod", s.handlePodDetail))
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/topology", s.withProvider("topology", s.handleTopology))
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/logs", s.withProvider("logs", s.handleLogs))
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/topology/edges/{edgeId}/series", s.handleEdgeSeries)
	m.HandleFunc("PUT /api/v1/clusters/{clusterId}/topology/layout", s.handleTopologyLayoutPut)
	// 관리(ADR 0014, #32) — 조회 경로와 분리, platform.admin 전용.
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/deployments", s.handleDeploymentList)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/deployments/{namespace}/{name}", s.handleDeploymentDetail)
	m.HandleFunc("PUT /api/v1/clusters/{clusterId}/deployments/{namespace}/{name}", s.handleDeploymentUpdate)
	m.HandleFunc("POST /api/v1/clusters/{clusterId}/deployments/{namespace}/{name}/restart", s.handleDeploymentRestart)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/secrets", s.handleSecretList)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/secrets/{namespace}/{name}", s.handleSecretDetail)
	m.HandleFunc("PUT /api/v1/clusters/{clusterId}/secrets/{namespace}/{name}", s.handleSecretUpdate)
	m.HandleFunc("POST /api/v1/clusters/{clusterId}/secrets/{namespace}/{name}/restart", s.handleSecretRestart)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/alerts", s.handleAlerts)
	// Resource Explorer(ADR 0018) — 조회 전용. 카탈로그·목록은 로컬 snapshot,
	// 상세 하나만 격리된 live GET입니다.
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/resources", s.handleResourceCatalog)
	// 전역 검색·최근 항목(ADR 0023)은 세그먼트 수가 달라 목록 라우트와 겹치지 않습니다 —
	// 목록은 {group}/{version}/{resource} 세 개를 요구하고 이 둘은 한 개입니다.
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/resources/search", s.handleResourceSearch)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/resources/recent", s.handleResourceRecent)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}", s.handleResourceList)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}/object", s.handleResourceDetail)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/events/stream", s.handleEventStream)
}

// operationalPath는 인증 없이 접근 가능한 운영 경로입니다. probe·버전 확인은
// Credential 없이 가능해야 하고, /api/v1/*의 인증 경계는 그대로 유지합니다. (#5)
var operationalPath = map[string]bool{"/healthz": true, "/readyz": true, "/version": true, "/metrics": true}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := s.deps.Now()
	rec := &statusRecorder{ResponseWriter: w}
	w = rec
	route := routeName(s.mux, r)
	isStreamRequest := route == "stream"
	observeHTTP := route != "metrics"
	if observeHTTP {
		s.deps.Observability.HTTPStarted()
		defer func() {
			s.deps.Observability.HTTPFinished(route, rec.status, rec.bytes, s.deps.Now().Sub(started), isStreamRequest, r.Context().Err() != nil)
		}()
	}
	// 요청 ID는 가장 먼저 확정합니다 — 401·404·패닉을 포함한 모든 응답이 달고 나갑니다.
	reqID := r.Header.Get(requestIDHeader)
	if !safeRequestID(reqID) {
		reqID = s.deps.NewRequestID()
	}
	w.Header().Set(requestIDHeader, reqID)
	r = r.WithContext(withRequestID(r.Context(), reqID))

	if s.deps.AllowedOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", s.deps.AllowedOrigin)
		w.Header().Set("Vary", "Origin")
	}
	// 대시보드 응답은 항상 지금의 상태입니다. 중간 캐시가 끼면 낡은 값이 정상처럼 보입니다.
	w.Header().Set("Cache-Control", "no-store")
	sc := scope.Scope{}
	var trace *observability.Trace

	defer func() {
		if recovered := recover(); recovered != nil {
			s.deps.Logger.Error("패닉", "route", route, "requestId", reqID)
			if !rec.wroteHeader {
				writeError(w, r, http.StatusInternalServerError, "internal", "요청을 처리하지 못했습니다.")
			}
			s.audit(r, sc, rec.status, started)
		}
	}()

	isAuthPath := strings.HasPrefix(r.URL.Path, "/api/v1/auth/")
	if isAuthPath {
		release, ok := s.authGuard.acquire(r)
		if !ok {
			w.Header().Set("Retry-After", "1")
			writeError(w, r, http.StatusTooManyRequests, "auth_rate_limited", "인증 요청 한도를 초과했습니다.")
			return
		}
		defer release()
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
	}
	if !operationalPath[r.URL.Path] && !isAuthPath {
		var err error
		sc, err = s.deps.Resolver.Resolve(r)
		if err != nil {
			// 인증 실패(401)와 권한 부족(403)은 다른 상태입니다. 여기는 401만 나갑니다 —
			// 유효한 토큰의 권한 부족은 빈 Scope로 통과해 아래에서 403이 됩니다. (#10)
			status, code := http.StatusUnauthorized, "unauthorized"
			if errors.Is(err, auth.ErrSessionUnavailable) {
				status, code = http.StatusServiceUnavailable, "session_unavailable"
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="k8s-dashboard"`)
			writeError(w, r, status, code, "인증이 필요합니다.")
			s.audit(r, scope.Scope{}, status, started)
			return
		}
	}

	// 등록되지 않은 경로·메서드는 ServeMux의 text/plain 대신 JSON 에러 계약으로 답합니다.
	// Handler()는 매칭만 확인하고 핸들러를 실행하지 않으므로 성공 응답은 버퍼링되지 않습니다.
	if _, currentPattern := s.mux.Handler(r); r.Method == http.MethodHead || currentPattern == "" {
		allowed := registeredMethods(s.mux, r)
		if len(allowed) > 0 {
			// ServeMux는 HEAD를 GET처럼 처리하지만 API 계약은 메서드를 명시적으로 등록합니다.
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "허용되지 않은 메서드입니다.")
			s.audit(r, sc, http.StatusMethodNotAllowed, started)
			return
		}
	}
	_, pattern := s.mux.Handler(r)
	if pattern == "" {
		writeError(w, r, http.StatusNotFound, "not_found", "등록되지 않은 경로입니다.")
		s.audit(r, sc, http.StatusNotFound, started)
		return
	}
	r.Pattern = pattern
	// SSE 스트림은 질의가 아니라 연결입니다 — query guard의 12s budget·rate·slow
	// 계측을 타면 스트림이 강제 종료됩니다. 스트림 전용 상한이 자원을 지킵니다. (#12)
	isStream := pattern == streamRoute
	if !operationalPath[r.URL.Path] && !isAuthPath && !isStream {
		user := sc.Subject
		if user == "" {
			user = "auth-none"
		}
		clusterPartition := authorizedGuardPartition(sc, clusterIDFromPath(r.URL.Path))
		release, reason, retry := s.deps.Guard.Acquire(clusterPartition+"\x00"+user, clusterPartition+"\x00"+pattern)
		if reason != "" {
			seconds := int((retry + time.Second - 1) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			s.deps.ProtectionMetrics.Reject(reason, pattern)
			writeError(w, r, http.StatusTooManyRequests, "query_rejected", "요청 보호 한도를 초과했습니다.")
			s.audit(r, sc, http.StatusTooManyRequests, started)
			return
		}
		defer release()
		ctx, cancel := context.WithTimeoutCause(r.Context(), s.deps.Guard.Timeout(), errQueryBudget)
		defer cancel()
		r = r.WithContext(ctx)
	}

	if route == "overview" || route == "namespace" || route == "workload" || route == "pod" {
		var traceCtx context.Context
		traceCtx, trace = observability.WithTrace(r.Context())
		trace.SetRequestID(reqID)
		r = r.WithContext(traceCtx)
	}
	s.mux.ServeHTTP(rec, r.WithContext(scope.With(r.Context(), sc)))
	if trace != nil && rec.status < 400 {
		for _, ref := range s.deps.PlannedQueryRefs {
			observability.RecordQueryRef(r.Context(), ref)
		}
	}
	if !operationalPath[r.URL.Path] && !isAuthPath && !isStream && s.deps.Now().Sub(started) >= s.deps.Guard.SlowThreshold() {
		s.deps.ProtectionMetrics.Slow(pattern)
		args := []any{"route", route, "status", rec.status, "durationMs", s.deps.Now().Sub(started).Milliseconds(), "bytes", rec.bytes, "requestId", reqID, "scope", scopeText(sc)}
		if trace != nil {
			refs, overflow := trace.Summary()
			args = append(args, "queryRefs", refs, "queryRefsOverflow", overflow)
		}
		s.deps.Logger.Warn("slow_api", args...)
	}
	s.audit(r, sc, rec.status, started)
}

// authorizedGuardPartition prevents untrusted path IDs from creating rate
// limiter identities or resetting a caller's rate budget. Only a canonical,
// accessible cluster from the resolved server-side scope gets its own bucket.
func authorizedGuardPartition(sc scope.Scope, requested string) string {
	if clusterid.Valid(requested) {
		if cluster, ok := sc.Cluster(requested); ok && cluster.Accessible() {
			return requested
		}
	}
	return "denied"
}
func clusterIDFromPath(path string) string {
	const marker = "/api/v1/clusters/"
	if !strings.HasPrefix(path, marker) {
		return "platform"
	}
	rest := strings.TrimPrefix(path, marker)
	id, _, _ := strings.Cut(rest, "/")
	if id == "" {
		return "invalid"
	}
	return id
}

func registeredMethods(mux *http.ServeMux, r *http.Request) []string {
	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete}
	allowed := make([]string, 0, len(methods))
	for _, method := range methods {
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := mux.Handler(probe); pattern != "" {
			allowed = append(allowed, method)
		}
	}
	return allowed
}

// statusRecorder는 감사 로그에 결과 상태를 남기기 위해 상태 코드를 붙잡습니다.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

// Unwrap은 http.ResponseController가 원본 writer의 Flush·SetWriteDeadline을
// 찾을 수 있게 합니다. SSE 핸들러(#12)가 이 경로에 의존합니다.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func routeName(mux *http.ServeMux, r *http.Request) string {
	_, pattern := mux.Handler(r)
	switch pattern {
	case "GET /healthz":
		return "healthz"
	case "GET /readyz":
		return "readyz"
	case "GET /version":
		return "version"
	case "GET /metrics":
		return "metrics"
	case "GET /api/v1/scope":
		return "scope"
	case "GET /api/v1/dashboard-capabilities":
		return "dashboard_capabilities"
	case "GET /api/v1/dashboard-drafts":
		return "dashboard_list"
	case "POST /api/v1/dashboard-drafts":
		return "dashboard_create"
	case "GET /api/v1/dashboard-drafts/{id}":
		return "dashboard_get"
	case "PUT /api/v1/dashboard-drafts/{id}":
		return "dashboard_update"
	case "DELETE /api/v1/dashboard-drafts/{id}":
		return "dashboard_delete"
	case "POST /api/v1/dashboard-drafts/{id}/submit":
		return "dashboard_submit"
	case "POST /api/v1/dashboard-drafts/{id}/approve":
		return "dashboard_approve"
	case "POST /api/v1/dashboard-drafts/{id}/clone":
		return "dashboard_clone"
	case "GET /api/v1/dashboard-drafts/{id}/export":
		return "dashboard_export"
	case "GET /api/v1/clusters/{clusterId}/overview":
		return "overview"
	case "GET /api/v1/clusters/{clusterId}/namespaces":
		return "namespaces"
	case "GET /api/v1/clusters/{clusterId}/namespaces/{namespace}":
		return "namespace"
	case "GET /api/v1/clusters/{clusterId}/workloads/{kind}/{name}":
		return "workload"
	case "GET /api/v1/clusters/{clusterId}/pods/{name}":
		return "pod"
	case "GET /api/v1/clusters/{clusterId}/logs":
		return "logs"
	case "GET /api/v1/clusters/{clusterId}/topology":
		return "topology"
	case "GET /api/v1/clusters/{clusterId}/topology/edges/{edgeId}/series":
		return "edge_series"
	case "PUT /api/v1/clusters/{clusterId}/topology/layout":
		return "topology_layout_put"
	case "GET /api/v1/clusters/{clusterId}/deployments":
		return "deployment_list"
	case "GET /api/v1/clusters/{clusterId}/deployments/{namespace}/{name}":
		return "deployment_detail"
	case "PUT /api/v1/clusters/{clusterId}/deployments/{namespace}/{name}":
		return "deployment_update"
	case "POST /api/v1/clusters/{clusterId}/deployments/{namespace}/{name}/restart":
		return "deployment_restart"
	case "GET /api/v1/clusters/{clusterId}/secrets":
		return "secret_list"
	case "GET /api/v1/clusters/{clusterId}/secrets/{namespace}/{name}":
		return "secret_detail"
	case "PUT /api/v1/clusters/{clusterId}/secrets/{namespace}/{name}":
		return "secret_update"
	case "POST /api/v1/clusters/{clusterId}/secrets/{namespace}/{name}/restart":
		return "secret_restart"
	case "GET /api/v1/clusters/{clusterId}/alerts":
		return "alerts"
	case "GET /api/v1/clusters/{clusterId}/resources":
		return "resource_catalog"
	case "GET /api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}":
		return "resource_list"
	case "GET /api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}/object":
		return "resource_detail"
	case "GET /api/v1/clusters/{clusterId}/resources/search":
		return "resource_search"
	case "GET /api/v1/clusters/{clusterId}/resources/recent":
		return "resource_recent"
	case streamRoute:
		return "stream"
	default:
		return "unmatched"
	}
}

/* ── 공통 처리 ──────────────────────────────────────────────────────────── */

// errForbidden은 화면 전체가 권한 안내가 되어야 하는 경우입니다.
type errForbidden struct{ msg string }

func (e errForbidden) Error() string { return e.msg }

// errBadRequest는 요청 자체가 성립하지 않는 경우입니다.
type errBadRequest struct{ code, msg string }

func (e errBadRequest) Error() string { return e.msg }

type cachedPanic struct{ value any }

func (e cachedPanic) Error() string { return fmt.Sprint(e.value) }

// serve는 캐시·singleflight·에러 매핑·직렬화를 한 곳에 모읍니다.
//
// 같은 화면을 여러 사람이 동시에 열면 upstream 호출은 **1회**입니다.
func serve[T any](s *Server, w http.ResponseWriter, r *http.Request, fn func(context.Context) (T, error)) {
	key := cacheKey(r)
	ttl := cacheTTL(s, r)
	v, err := s.deps.Cache.Bytes(r.Context(), key, ttl, func(ctx context.Context) (raw []byte, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = cachedPanic{recovered}
			}
		}()
		value, err := fn(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	})
	if err != nil {
		var cp cachedPanic
		if errors.As(err, &cp) {
			panic(cp.value)
		}
		if errors.Is(context.Cause(r.Context()), errQueryBudget) || (r.Context().Err() == nil && errors.Is(err, context.DeadlineExceeded)) {
			s.deps.ProtectionMetrics.Reject("query_timeout", r.Pattern)
			writeError(w, r, http.StatusGatewayTimeout, "query_timeout", "서버 질의 시간 한도를 초과했습니다.")
			return
		}
		if r.Context().Err() != nil {
			return
		}
		var fb errForbidden
		var br errBadRequest
		switch {
		case errors.As(err, &fb):
			writeError(w, r, http.StatusForbidden, "forbidden", fb.msg)
		case errors.As(err, &br):
			writeError(w, r, http.StatusBadRequest, br.code, br.msg)
		case errors.Is(err, cache.ErrValueTooLarge):
			s.deps.ProtectionMetrics.Reject("result_too_large", r.Pattern)
			writeError(w, r, http.StatusBadGateway, "result_too_large", "응답 크기 한도를 초과했습니다.")
		default:
			s.deps.Logger.Error("요청 처리 실패", "route", routeName(s.mux, r), "requestId", requestIDFrom(r.Context()))
			writeError(w, r, http.StatusInternalServerError, "internal", "요청을 처리하지 못했습니다.")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(v)
}

// cacheKey는 경로 + 정렬된 쿼리 + Scope입니다.
// **Scope를 키에 넣지 않으면 권한이 다른 사용자끼리 캐시를 공유하게 됩니다.**
func cacheKey(r *http.Request) string {
	q := r.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == "range" || k == "from" || k == "to" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	params := make([]cache.Param, 0, len(keys)+1)
	params = append(params, cache.Param{Name: "path", Values: []string{r.URL.Path}})
	for _, k := range keys {
		params = append(params, cache.Param{Name: k, Values: append([]string(nil), q[k]...)})
	}
	sc := scope.From(r.Context())
	identScope := cache.ScopeIdentity{TopologyEditor: sc.CanEditTopology}
	for _, c := range sc.Clusters {
		identScope.Clusters = append(identScope.Clusters, cache.ClusterIdentity{ID: c.ID, All: c.All, Namespaces: append([]string(nil), c.Namespaces...)})
	}
	identity := cache.Identity{Dashboard: r.Pattern, QueryRef: r.Pattern, Scope: identScope, Range: q.Get("range"), Params: params}
	if win, err := timerange.Parse(q.Get("range"), q.Get("from"), q.Get("to"), time.Now()); err == nil {
		identity.Range = string(win.Key)
		identity.StepSeconds = int64(win.Step / time.Second)
		if win.Key == contract.RangeCustom {
			identity.Range = string(win.Key)
			identity.From = win.From.UTC().Format(time.RFC3339Nano)
			identity.To = win.To.UTC().Format(time.RFC3339Nano)
		}
	}
	return identity.Key()
}

func cacheTTL(s *Server, r *http.Request) time.Duration {
	class := cache.State
	if r.Pattern == "GET /api/v1/clusters/{clusterId}/topology/edges/{edgeId}/series" {
		class = cache.Short
	}
	q := r.URL.Query()
	if class == cache.Short && (q.Get("from") != "" || q.Get("to") != "") {
		if win, err := timerange.Parse(q.Get("range"), q.Get("from"), q.Get("to"), s.deps.Now()); err == nil {
			return s.deps.CacheTTL.For(cache.Historical, win.To, s.deps.Now())
		}
	}
	return s.deps.CacheTTL.For(class, time.Time{}, s.deps.Now())
}

// authorize는 클러스터 접근 권한을 확인합니다.
// 권한이 없으면 **부분 데이터도 만들지 않고** 403으로 끝냅니다.
func (s *Server) authorize(ctx context.Context, clusterID string) (scope.Cluster, error) {
	c, ok := scope.From(ctx).Cluster(clusterID)
	if !ok || !c.Accessible() {
		return scope.Cluster{}, errForbidden{"이 클러스터에 대한 접근 권한이 없습니다."}
	}
	return c, nil
}

// namespaceFilter는 요청이 원한 namespace를 Scope와 교차시킵니다.
// 요청 값이 Scope 밖이면 거절합니다. 조용히 무시하면 사용자는 "전체"를 본 줄 압니다.
func namespaceFilter(c scope.Cluster, requested string) (clusterstate.NamespaceFilter, error) {
	if requested == "" || requested == "all" {
		return clusterstate.NamespaceFilter{All: c.All, List: c.Namespaces}, nil
	}
	if !c.AllowsNamespace(requested) {
		return clusterstate.NamespaceFilter{}, errForbidden{
			fmt.Sprintf("Namespace %q에 대한 접근 권한이 없습니다.", requested),
		}
	}
	return clusterstate.NamespaceFilter{List: []string{requested}}, nil
}

// kubeSection은 informer 캐시에서 읽은 값을 섹션으로 감쌉니다.
// 아직 동기화 중이면 0이 아니라 **degraded**를 내려보냅니다 —
// "Pod 0개"는 정상처럼 보이지만 사실이 아닙니다.
func kubeSection[T any](provider clusterstate.Provider, v T, err error) contract.Section[T] {
	if err != nil {
		return contract.Degraded[T](contract.SourceKubernetes, "클러스터 상태를 읽지 못했습니다", nil)
	}
	if provider == nil || !provider.HasSynced() {
		if freshness, ok := provider.(interface{ ObservedAt() time.Time }); ok && !freshness.ObservedAt().IsZero() {
			out := contract.Degraded(contract.SourceKubernetes, "cache is stale", &v)
			out.ObservedAt = freshness.ObservedAt().UTC().Format(time.RFC3339)
			return out
		}
		return contract.Degraded(contract.SourceKubernetes, "캐시 동기화 중 · 값이 최신이 아닐 수 있습니다", &v)
	}
	out := contract.OK(v)
	if freshness, ok := provider.(interface{ ObservedAt() time.Time }); ok && !freshness.ObservedAt().IsZero() {
		out.ObservedAt = freshness.ObservedAt().UTC().Format(time.RFC3339)
	}
	return out
}

// dsSection은 외부 데이터소스 결과를 섹션으로 감쌉니다.
// 에러 원문은 내려보내지 않습니다 — 내부 주소나 질의가 그대로 노출됩니다. (README §10)
func dsSection[T any](v T, err error, src contract.Source, what string) contract.Section[T] {
	if err != nil {
		return contract.Degraded[T](src, what+" 응답 없음", nil)
	}
	return contract.OK(v)
}
