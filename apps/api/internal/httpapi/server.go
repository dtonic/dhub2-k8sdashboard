// Package httpapi는 화면 단위 엔드포인트를 제공합니다.
//
// 규칙은 세 가지입니다.
//   - **화면 하나 = 요청 하나.** 위젯마다 엔드포인트를 만들지 않습니다. (ADR 0002)
//   - **Scope는 서버가 강제합니다.** 요청의 cluster/namespace는 힌트일 뿐입니다. (README §10)
//   - **부분 장애는 섹션 값으로 표현합니다.** 한 데이터소스가 죽어도 화면은 삽니다.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

// Deps는 서버가 쓰는 바깥 세계 전부입니다. 테스트에서는 전부 대체할 수 있습니다.
type Deps struct {
	Store    *clusterstate.Store
	Metrics  datasource.Metrics
	Logs     datasource.Logs
	Alerts   datasource.Alerts
	Topology datasource.Topology
	Resolver scope.Resolver
	Cache    *cache.TTL
	Logger   *slog.Logger
	// AllowedOrigin이 있으면 CORS 헤더를 붙입니다. 개발 중 Vite 오리진용입니다.
	AllowedOrigin string
	// Now는 테스트에서 시간을 고정합니다.
	Now func() time.Time
}

type Server struct {
	deps Deps
	mux  *http.ServeMux
}

func NewServer(d Deps) *Server {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Cache == nil {
		d.Cache = cache.NewTTL(5 * time.Second)
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	s := &Server{deps: d, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// readyz는 informer 캐시가 채워진 뒤에만 통과합니다.
	// 동기화 전에 트래픽을 받으면 "Pod 0개"가 정상처럼 보입니다.
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.deps.Store.HasSynced() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "syncing"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	m.HandleFunc("GET /api/v1/scope", s.handleScope)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/overview", s.handleOverview)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/namespaces", s.handleNamespaceList)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/namespaces/{namespace}", s.handleNamespaceDetail)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/workloads/{kind}/{name}", s.handleWorkloadDetail)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/pods/{name}", s.handlePodDetail)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/logs", s.handleLogs)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/topology", s.handleTopology)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/topology/edges/{edgeId}/series", s.handleEdgeSeries)
	m.HandleFunc("GET /api/v1/clusters/{clusterId}/alerts", s.handleAlerts)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.deps.AllowedOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", s.deps.AllowedOrigin)
		w.Header().Set("Vary", "Origin")
	}
	// 대시보드 응답은 항상 지금의 상태입니다. 중간 캐시가 끼면 낡은 값이 정상처럼 보입니다.
	w.Header().Set("Cache-Control", "no-store")

	defer func() {
		if rec := recover(); rec != nil {
			s.deps.Logger.Error("패닉", "path", r.URL.Path, "recover", fmt.Sprint(rec))
			writeError(w, http.StatusInternalServerError, "internal", "요청을 처리하지 못했습니다.")
		}
	}()

	started := s.deps.Now()
	sc, err := s.deps.Resolver.Resolve(r)
	if err != nil {
		// 인증 실패(401)와 권한 부족(403)은 다른 상태입니다. 여기는 401만 나갑니다 —
		// 유효한 토큰의 권한 부족은 빈 Scope로 통과해 아래에서 403이 됩니다. (#10)
		w.Header().Set("WWW-Authenticate", `Bearer realm="k8s-dashboard"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "인증이 필요합니다.")
		s.audit(r, scope.Scope{}, http.StatusUnauthorized, started)
		return
	}

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(rec, r.WithContext(scope.With(r.Context(), sc)))
	s.audit(r, sc, rec.status, started)
}

// statusRecorder는 감사 로그에 결과 상태를 남기기 위해 상태 코드를 붙잡습니다.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

/* ── 공통 처리 ──────────────────────────────────────────────────────────── */

// errForbidden은 화면 전체가 권한 안내가 되어야 하는 경우입니다.
type errForbidden struct{ msg string }

func (e errForbidden) Error() string { return e.msg }

// errBadRequest는 요청 자체가 성립하지 않는 경우입니다.
type errBadRequest struct{ code, msg string }

func (e errBadRequest) Error() string { return e.msg }

// serve는 캐시·singleflight·에러 매핑·직렬화를 한 곳에 모읍니다.
//
// 같은 화면을 여러 사람이 동시에 열면 upstream 호출은 **1회**입니다.
func serve[T any](s *Server, w http.ResponseWriter, r *http.Request, fn func(context.Context) (T, error)) {
	key := cacheKey(r)
	v, err := cache.Typed(r.Context(), s.deps.Cache, key, fn)
	if err != nil {
		var fb errForbidden
		var br errBadRequest
		switch {
		case errors.As(err, &fb):
			writeError(w, http.StatusForbidden, "forbidden", fb.msg)
		case errors.As(err, &br):
			writeError(w, http.StatusBadRequest, br.code, br.msg)
		default:
			s.deps.Logger.Error("요청 처리 실패", "path", r.URL.Path, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "요청을 처리하지 못했습니다.")
		}
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// cacheKey는 경로 + 정렬된 쿼리 + Scope입니다.
// **Scope를 키에 넣지 않으면 권한이 다른 사용자끼리 캐시를 공유하게 됩니다.**
func cacheKey(r *http.Request) string {
	q := r.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == "scenario" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(r.URL.Path)
	for _, k := range keys {
		b.WriteString("&")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(strings.Join(q[k], ","))
	}
	b.WriteString("|scope=")
	for _, c := range scope.From(r.Context()).Clusters {
		b.WriteString(c.ID)
		if c.All {
			b.WriteString(":*")
			continue
		}
		b.WriteString(":" + strings.Join(c.Namespaces, "+"))
	}
	return b.String()
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
func kubeSection[T any](s *Server, v T, err error) contract.Section[T] {
	if err != nil {
		return contract.Degraded[T](contract.SourceKubernetes, "클러스터 상태를 읽지 못했습니다", nil)
	}
	if !s.deps.Store.HasSynced() {
		return contract.Degraded(contract.SourceKubernetes, "캐시 동기화 중 · 값이 최신이 아닐 수 있습니다", &v)
	}
	return contract.OK(v)
}

// dsSection은 외부 데이터소스 결과를 섹션으로 감쌉니다.
// 에러 원문은 내려보내지 않습니다 — 내부 주소나 질의가 그대로 노출됩니다. (README §10)
func dsSection[T any](v T, err error, src contract.Source, what string) contract.Section[T] {
	if err != nil {
		return contract.Degraded[T](src, what+" 응답 없음", nil)
	}
	return contract.OK(v)
}
