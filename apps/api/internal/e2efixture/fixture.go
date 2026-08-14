//go:build e2efixture

package e2efixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/auth"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

// 중단 선택지 — 데이터소스 부분 장애를 픽스처에서 **독립적으로** 켭니다.
// 프로덕션 서버에는 이런 스위치가 없습니다. 테스트 전용 표면입니다.
const (
	OutageGreptime = "greptime"
	OutageQuickwit = "quickwit"
	OutageAlerts   = "alerts"
)

// Config는 픽스처 기동 설정입니다. 잘못된 값은 리스너를 열기 전에 실패합니다.
type Config struct {
	// Addr은 loopback listen 주소입니다. 비우면 127.0.0.1:0(임의 포트)입니다.
	Addr string
	// DistDir은 기본 mock-off 프로덕션 번들 디렉터리입니다.
	DistDir string
	// Scenarios는 켤 시나리오 ID 목록입니다. 비우면 corpus 전체(네 가지)입니다.
	Scenarios []string
	// Outages는 강제로 내릴 데이터소스입니다. (greptime|quickwit|alerts)
	Outages []string
	// Audience는 mock OIDC 토큰의 aud입니다. 비우면 기본값입니다.
	Audience string
}

// Fixture는 떠 있는 픽스처 서버입니다. Close로 전부 정리합니다.
type Fixture struct {
	// URL은 단일 오리진(UI + API)입니다. 예: http://127.0.0.1:4273
	URL string
	// IssuerURL은 mock IdP입니다. 토큰 발급은 같은 오리진의 POST /e2e/token을 쓰면 됩니다.
	IssuerURL string
	Scenarios []Scenario

	httpSrv   *http.Server
	ln        net.Listener
	idp       *auth.MockIDP
	cancel    context.CancelFunc
	serveErr  chan error
	closeOnce sync.Once
}

// Start는 픽스처를 띄웁니다. informer 동기화·IdP·카탈로그 로드가 전부 끝난 뒤에만
// 리스너를 열므로, 리스너가 열린 순간이 곧 준비 완료(readiness)입니다.
func Start(ctx context.Context, cfg Config, logger *slog.Logger) (*Fixture, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// 1. 설정 검증 — 전부 fail-fast입니다. 잘못 뜬 픽스처는 틀린 화면을 정상처럼 보여줍니다.
	scenarios, err := scenariosFor(cfg.Scenarios)
	if err != nil {
		return nil, err
	}
	outages, err := parseOutages(cfg.Outages)
	if err != nil {
		return nil, err
	}
	if cfg.DistDir == "" {
		return nil, errors.New("DistDir이 비어 있습니다 — 기본 mock-off 프로덕션 번들 경로가 필요합니다")
	}
	distDir, err := filepath.EvalSymlinks(cfg.DistDir)
	if err != nil {
		return nil, fmt.Errorf("invalid dist directory: %w", err)
	}
	distDir, err = filepath.Abs(distDir)
	if err != nil {
		return nil, fmt.Errorf("invalid dist directory: %w", err)
	}
	indexPath := filepath.Join(distDir, "index.html")
	if info, err := os.Lstat(indexPath); err != nil {
		return nil, fmt.Errorf("프로덕션 번들이 없습니다 (%s): 먼저 `make build-web-production`을 실행하세요: %w", indexPath, err)
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("프로덕션 index는 일반 파일이어야 합니다: %s", indexPath)
	}
	addr := cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if err := requireLoopback(addr); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	f := &Fixture{cancel: cancel, Scenarios: scenarios, serveErr: make(chan error, 1)}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()

	// 2. 가짜 informer 캐시 — testcluster 사실 + 시나리오 객체. 동기화까지 기다립니다.
	store, _, err := testcluster.Build(ctx, testcluster.ScenarioObjects()...)
	if err != nil {
		return nil, err
	}

	// 3. 데이터소스 — demo 호환 시나리오 소스. 중단 선택은 datasource.Unavailable로
	//    바꿔치기만 하고, 화면 강등 경로는 프로덕션과 완전히 같습니다.
	src := NewSource(store, scenarios)
	var metrics datasource.Metrics = src
	var logs datasource.Logs = src
	var alerts datasource.Alerts = src
	var topo datasource.Topology = src
	if outages[OutageGreptime] {
		metrics = datasource.Unavailable{Reason: "fixture: GreptimeDB 중단 시나리오"}
	}
	if outages[OutageQuickwit] {
		logs = datasource.Unavailable{Reason: "fixture: Quickwit 중단 시나리오"}
	}
	if outages[OutageAlerts] {
		alerts = datasource.Unavailable{Reason: "fixture: Alertmanager 중단 시나리오"}
	}
	// 사용량 스냅숏은 한 번만 채웁니다 — 픽스처는 폴링 고루틴을 두지 않습니다.
	if usage, err := metrics.Usage(ctx, testcluster.ClusterID); err == nil {
		store.SetUsage(func(uid string) (contract.ContainerUsage, bool) {
			v, ok := usage[uid]
			return v, ok
		})
	}

	// 4. 실제 mock OIDC — 검증 경로는 운영과 같고 발급자만 loopback입니다.
	audience := cfg.Audience
	if audience == "" {
		audience = auth.DefaultMockAudience
	}
	idp, err := auth.StartMockIDP("", audience, nil)
	if err != nil {
		return nil, err
	}
	f.idp = idp
	f.IssuerURL = idp.Issuer
	resolver, err := auth.NewResolver(ctx, auth.Config{
		IssuerURL:   idp.Issuer,
		Audience:    audience,
		ClusterID:   testcluster.ClusterID,
		ClusterName: store.ClusterName(),
	}, logger)
	if err != nil {
		return nil, err
	}

	// 5. 실제 httpapi — 시간은 픽스처 기준 시각으로 고정해 조회 구간이 결정적입니다.
	queries, err := querycatalog.LoadPath("")
	if err != nil {
		return nil, err
	}
	_ = queries // 카탈로그 로드 성공 자체가 기동 검증입니다. 핸들러 경로는 데모 소스를 씁니다.
	api := httpapi.NewServer(httpapi.Deps{
		Store:    store,
		Metrics:  metrics,
		Logs:     logs,
		Alerts:   alerts,
		Topology: topo,
		Resolver: resolver,
		Logger:   logger,
		Now:      func() time.Time { return testcluster.Now },
		Version:  contract.VersionInfo{Version: "e2e-fixture", Commit: "none", BuildDate: testcluster.Now.UTC().Format(time.RFC3339)},
	})

	// 6. 단일 오리진 — API·정적 번들·토큰 발급을 한 리스너로 묶습니다.
	handler := &originHandler{api: api, distDir: distDir, indexPath: indexPath, idp: idp}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	f.ln = ln
	f.URL = "http://" + ln.Addr().String()
	f.httpSrv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		err := f.httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			f.serveErr <- err
		}
		close(f.serveErr)
	}()

	// 토큰·발급자 상세는 남기지 않습니다 — URL과 선택 상태만 남깁니다.
	logger.Info("e2e fixture ready",
		"url", f.URL,
		"scenarios", scenarioIDs(scenarios),
		"outages", cfg.Outages)
	ok = true
	return f, nil
}

// Close는 리스너·IdP·informer를 내립니다. 몇 번 불려도 안전합니다.
func (f *Fixture) Close() {
	f.closeOnce.Do(func() {
		if f.httpSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = f.httpSrv.Shutdown(ctx)
			cancel()
		}
		if f.idp != nil {
			_ = f.idp.Close()
		}
		if f.cancel != nil {
			f.cancel()
		}
	})
}

// Errors reports an unexpected HTTP serving failure. Normal shutdown closes it
// without a value so callers can fail fast instead of silently hanging.
func (f *Fixture) Errors() <-chan error { return f.serveErr }

/* ── HTTP 배선 ─────────────────────────────────────────────────────────── */

// operationalPath는 httpapi가 인증 없이 여는 운영 경로와 같은 목록입니다.
var operationalPath = map[string]bool{"/healthz": true, "/readyz": true, "/version": true, "/metrics": true}

type originHandler struct {
	api       http.Handler
	distDir   string
	indexPath string
	idp       *auth.MockIDP
}

func (h *originHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.URL.Path == "/e2e/token" && r.Method == http.MethodPost) {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/") || operationalPath[p]:
		h.api.ServeHTTP(w, r)
	case p == "/e2e/token":
		h.handleToken(w, r)
	default:
		h.serveStatic(w, r)
	}
}

// handleToken은 테스트 컨텍스트용 Bearer 토큰을 발급합니다.
// 신원·역할은 **POST JSON 본문**으로만 받습니다 — 쿼리스트링은 접근 로그·history에
// 남기 쉽기 때문입니다. 토큰은 응답 본문에만 실리고, 어떤 로그에도 남지 않습니다.
func (h *originHandler) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST만 허용합니다", http.StatusMethodNotAllowed)
		return
	}
	if len(r.URL.RawQuery) != 0 {
		http.Error(w, "쿼리 파라미터 대신 JSON 본문을 쓰세요", http.StatusBadRequest)
		return
	}
	var req struct {
		Sub   string   `json:"sub"`
		Roles []string `json:"roles"`
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, `본문 형식: {"sub":"...","roles":["..."]}`, http.StatusBadRequest)
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		http.Error(w, "exactly one JSON object is required", http.StatusBadRequest)
		return
	}
	if !allowedFixtureIdentity(req.Sub, req.Roles) {
		http.Error(w, "fixture identity is not allowed", http.StatusForbidden)
		return
	}
	token, err := h.idp.Token(req.Sub, req.Roles, time.Hour)
	if err != nil {
		http.Error(w, "토큰 발급 실패", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token, "token_type": "Bearer"})
}

// serveStatic은 dist 파일을 서빙하고, 없는 경로는 SPA 진입점으로 되돌립니다.
//
// dist 밖으로는 어떤 경로도 나가지 않습니다: URL을 절대 경로로 정규화하고,
// dist 기준 상대 경로가 밖을 가리키면 거르며, symlink는 따라가지 않습니다.
func (h *originHandler) serveStatic(w http.ResponseWriter, r *http.Request) {
	for _, segment := range strings.Split(r.URL.Path, "/") {
		if segment == ".." || strings.ContainsRune(segment, 0) || strings.ContainsRune(segment, '\\') {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
	}
	name := path.Clean("/" + r.URL.Path)
	fp := filepath.Join(h.distDir, filepath.FromSlash(strings.TrimPrefix(name, "/")))
	real, err := filepath.EvalSymlinks(fp)
	if err != nil {
		h.serveIndex(w, r)
		return
	}
	rel, err := filepath.Rel(h.distDir, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if info, err := os.Stat(real); err == nil && info.Mode().IsRegular() {
		http.ServeFile(w, r, real)
		return
	}
	h.serveIndex(w, r)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("extra JSON value")
		}
		return err
	}
	return nil
}

var fixtureIdentities = map[string][]string{
	"admin":           {"platform.admin"},
	"oncall-admin":    {"platform.admin"},
	"operator":        {"platform.admin"},
	"media-viewer":    {"namespace.viewer:media"},
	"payments-viewer": {"namespace.viewer:payments"},
	"no-roles":        {},
}

func allowedFixtureIdentity(sub string, roles []string) bool {
	want, ok := fixtureIdentities[sub]
	if !ok || len(want) != len(roles) {
		return false
	}
	for i := range want {
		if roles[i] != want[i] {
			return false
		}
	}
	return true
}

func (h *originHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, h.indexPath)
}

/* ── 검증 도우미 ───────────────────────────────────────────────────────── */

func parseOutages(list []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, o := range list {
		switch o {
		case OutageGreptime, OutageQuickwit, OutageAlerts:
			out[o] = true
		case "":
		default:
			return nil, fmt.Errorf("알 수 없는 outage %q (사용 가능: greptime|quickwit|alerts)", o)
		}
	}
	return out, nil
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("addr가 host:port 형식이 아닙니다: %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("픽스처는 loopback에만 바인드합니다: %q", addr)
	}
	return nil
}

func scenarioIDs(list []Scenario) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out
}
