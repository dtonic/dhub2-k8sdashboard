package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	urlpkg "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/auth"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/config"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(new(strings.Builder), nil))
}

/* ── Resource Explorer 배선 (ADR 0018 · ADR 0019 Phase 1) ─────────────────

   순수 빌더 두 개만 확인합니다. 클러스터도 네트워크도 없이 "설정이 실제로
   전달되는가"를 보는 것이 목적이고, 그 이상의 주입 지점은 만들지 않았습니다.

   전용 클라이언트가 상세와 다른 인스턴스라는 것과 disabled면 capability가
   false라는 것은 U2 core 테스트가 이미 증명하므로 여기서 중복하지 않습니다.
   이 테스트들은 env를 읽지 않으므로 t.Parallel 제약과 무관하지만, 같은 파일의
   관례를 따라 병렬로 돌리지 않습니다. */

func dryRunWiringConfig(dryRunEnabled bool) config.Config {
	cfg := config.Config{
		ClusterID: "prod-seoul",
		QPS:       20,
		Burst:     30,
		Resync:    10 * time.Minute,
	}
	// 기본값과 구분되는 값만 씁니다 — 그래야 "전달되었다"와 "우연히 같다"가 갈립니다.
	cfg.ResourceExplorer = config.ResourceExplorerConfig{
		Enabled:          true,
		Refresh:          11 * time.Minute,
		AllowCRDs:        true,
		DetailRate:       2,
		DetailBurst:      5,
		DetailConcurrent: 2,
		DetailTimeout:    5 * time.Second,
		MaxObjectBytes:   2 << 20,

		SearchEnabled:     true,
		SearchIncremental: true,
		SearchMaxBytes:    48 << 20,

		DryRunEnabled:          dryRunEnabled,
		DryRunTimeout:          9 * time.Second,
		DryRunRate:             2,
		DryRunBurst:            7,
		DryRunConcurrent:       3,
		DryRunMaxManifestBytes: 128 << 10,
		DryRunMaxObjectBytes:   512 << 10,
	}
	return cfg
}

// TestResourceClientOptionsCarryDryRunSettingsOnlyWhenEnabled — 검토 클라이언트는
// 켰을 때만 만들어지고, 상한은 상세와 별개로 전달되어야 합니다.
func TestResourceClientOptionsCarryDryRunSettingsOnlyWhenEnabled(t *testing.T) {
	off := resourceClientOptions(dryRunWiringConfig(false))
	if off.DryRunEnabled {
		t.Error("꺼져 있는데 검토 클라이언트를 만들라고 지시했습니다")
	}

	on := resourceClientOptions(dryRunWiringConfig(true))
	if !on.DryRunEnabled {
		t.Fatal("켰는데 검토 클라이언트 생성 지시가 없습니다")
	}
	if on.DryRunQPS != 2 {
		t.Errorf("DryRunQPS=%v want 2", on.DryRunQPS)
	}
	if on.DryRunBurst != 7 {
		t.Errorf("DryRunBurst=%d want 7", on.DryRunBurst)
	}
	if on.DryRunTimeout != 9*time.Second {
		t.Errorf("DryRunTimeout=%v want 9s", on.DryRunTimeout)
	}
	if on.MaxDryRunObjectBytes != int64(512<<10) {
		t.Errorf("MaxDryRunObjectBytes=%d want %d", on.MaxDryRunObjectBytes, 512<<10)
	}
	// 상세 상한은 검토와 **섞이지 않습니다.**
	if on.DetailQPS != 2 || on.DetailBurst != 5 || on.DetailTimeout != 5*time.Second {
		t.Errorf("상세 상한이 바뀌었습니다: %+v", on)
	}
	if on.MaxObjectBytes != int64(2<<20) {
		t.Errorf("상세 본문 상한이 검토 값으로 덮였습니다: %d", on.MaxObjectBytes)
	}
	if on.UserAgent != "k8s-dashboard-resources" {
		t.Errorf("UserAgent=%q", on.UserAgent)
	}
}

// TestResourceServiceConfigCarriesFinalAllowlistAndNilDeny — main은 config가 이미
// deny까지 적용해 돌려준 **최종 목록**을 그대로 넘기고 deny는 넘기지 않습니다.
// 두 곳에서 빼면 어느 쪽이 적용됐는지 알 수 없고 한쪽만 고치면 우회가 생깁니다.
func TestResourceServiceConfigCarriesFinalAllowlistAndNilDeny(t *testing.T) {
	allowlist := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "configmaps"},
		{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
	}
	dryRunAllow := []schema.GroupVersionResource{{Group: "", Version: "v1", Resource: "configmaps"}}

	got := resourceServiceConfig(dryRunWiringConfig(true), allowlist, dryRunAllow, discardLogger())

	if !got.DryRunEnabled {
		t.Fatal("DryRunEnabled가 전달되지 않았습니다")
	}
	if got.DryRunDeny != nil {
		t.Errorf("DryRunDeny는 nil이어야 합니다: %v", got.DryRunDeny)
	}
	if len(got.DryRunAllowlist) != 1 || got.DryRunAllowlist[0] != dryRunAllow[0] {
		t.Errorf("최종 대상 목록이 전달되지 않았습니다: %v", got.DryRunAllowlist)
	}
	if got.DryRunTimeout != 9*time.Second {
		t.Errorf("DryRunTimeout=%v want 9s", got.DryRunTimeout)
	}
	if got.DryRunRate != 2 {
		t.Errorf("DryRunRate=%v want 2", got.DryRunRate)
	}
	if got.DryRunBurst != 7 {
		t.Errorf("DryRunBurst=%d want 7", got.DryRunBurst)
	}
	if got.DryRunConcurrent != 3 {
		t.Errorf("DryRunConcurrent=%d want 3", got.DryRunConcurrent)
	}
	if got.MaxManifestBytes != 128<<10 {
		t.Errorf("MaxManifestBytes=%d want %d", got.MaxManifestBytes, 128<<10)
	}

	// 기존 Resource Explorer·검색 배선은 그대로여야 합니다.
	if len(got.Allowlist) != 2 || !got.AllowCRDs || got.RefreshInterval != 11*time.Minute {
		t.Errorf("Explorer 배선이 바뀌었습니다: %+v", got)
	}
	if got.DetailRate != 2 || got.DetailBurst != 5 || got.DetailConcurrent != 2 ||
		got.DetailTimeout != 5*time.Second || got.MaxObjectBytes != 2<<20 {
		t.Errorf("상세 배선이 바뀌었습니다: %+v", got)
	}
	if !got.SearchEnabled || !got.SearchIncremental || got.MaxSearchIndexBytes != int64(48<<20) {
		t.Errorf("검색 배선이 바뀌었습니다: %+v", got)
	}
	if got.ClusterID != "prod-seoul" || got.Resync != 10*time.Minute || got.Logger == nil {
		t.Errorf("공통 배선이 바뀌었습니다: %+v", got)
	}
	// 최종 목록은 core가 nil deny로 재검증해도 그대로여야 합니다(고정점).
	again, err := resourcecatalog.NormalizeDryRunAllowlist(got.DryRunAllowlist, got.Allowlist, got.DryRunDeny)
	if err != nil {
		t.Fatalf("nil deny 재검증이 실패했습니다: %v", err)
	}
	if len(again) != len(got.DryRunAllowlist) {
		t.Fatalf("재검증이 목록을 바꿨습니다: %v → %v", got.DryRunAllowlist, again)
	}
}

// TestResourceServiceConfigLeavesDryRunOffByDefault — 꺼져 있으면 대상 목록은
// nil이고 스위치도 false입니다.
func TestResourceServiceConfigLeavesDryRunOffByDefault(t *testing.T) {
	allowlist := []schema.GroupVersionResource{{Group: "", Version: "v1", Resource: "configmaps"}}
	got := resourceServiceConfig(dryRunWiringConfig(false), allowlist, nil, discardLogger())
	if got.DryRunEnabled {
		t.Error("꺼져 있는데 서비스에 켜라고 전달했습니다")
	}
	if got.DryRunAllowlist != nil {
		t.Errorf("꺼져 있는데 대상 목록이 전달되었습니다: %v", got.DryRunAllowlist)
	}
	if got.DryRunDeny != nil {
		t.Errorf("DryRunDeny는 nil이어야 합니다: %v", got.DryRunDeny)
	}
	// 기존 Explorer 배선은 검토 여부와 무관하게 같습니다.
	if len(got.Allowlist) != 1 || !got.SearchEnabled {
		t.Errorf("Explorer 배선이 검토 스위치에 딸려 바뀌었습니다: %+v", got)
	}
}

func defaultCatalog(t *testing.T) querycatalog.Catalog {
	t.Helper()
	qc, err := querycatalog.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	return qc
}

type cancelCountingAlerts struct{ calls atomic.Int32 }

func (a *cancelCountingAlerts) List(ctx context.Context, _ datasource.AlertQuery) (datasource.AlertResult, error) {
	a.calls.Add(1)
	<-ctx.Done()
	return datasource.AlertResult{}, ctx.Err()
}

func TestDirectAlertPollerJoinsBeforeReturnAndStopsCalls(t *testing.T) {
	source := &cancelCountingAlerts{}
	poller := stream.NewAlertPoller(stream.AlertPollerConfig{ClusterID: "a", Interval: time.Second, MaxAlerts: 10}, source, nil, discardLogger())
	stop := startDirectAlertPoller(context.Background(), poller, discardLogger())
	deadline := time.Now().Add(time.Second)
	for source.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	stop()
	if source.calls.Load() != 1 || time.Since(started) > time.Second {
		t.Fatalf("direct poller calls=%d stop=%s", source.calls.Load(), time.Since(started))
	}
	time.Sleep(20 * time.Millisecond)
	if source.calls.Load() != 1 {
		t.Fatalf("direct poller called source after stop: %d", source.calls.Load())
	}
}

// TestRunStopsOnInvalidConfigBeforeKubernetes — 잘못된 필수 설정은 Kubernetes
// 클라이언트·informer를 만들기 전에 설정 오류로 멈춰야 합니다. (#5)
// kubeconfig 없는 환경에서도 이 테스트가 통과한다는 것 자체가 Validate가
// 클러스터 접속 시도보다 먼저 실행된다는 증거입니다.
func TestRunStopsOnInvalidConfigBeforeKubernetes(t *testing.T) {
	t.Setenv("AUTH_MODE", "what-is-this")
	err := run(discardLogger())
	if err == nil {
		t.Fatal("잘못된 AUTH_MODE로 run이 통과했습니다")
	}
	if !strings.Contains(err.Error(), "설정 오류") || !strings.Contains(err.Error(), "AUTH_MODE") {
		t.Fatalf("설정 검증이 아닌 다른 단계에서 실패했습니다: %v", err)
	}

	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "not-an-absolute-url")
	if err := run(discardLogger()); err == nil || !strings.Contains(err.Error(), "OIDC_ISSUER") {
		t.Fatalf("잘못된 issuer가 기동 전에 잡히지 않았습니다: %v", err)
	}

	t.Setenv("AUTH_MODE", "none")
	t.Setenv("ADDR", "not-an-address")
	if err := run(discardLogger()); err == nil || !strings.Contains(err.Error(), "ADDR") {
		t.Fatalf("잘못된 listen 주소가 Kubernetes 설정 전에 잡히지 않았습니다: %v", err)
	}

	t.Setenv("ADDR", ":8080")
	t.Setenv("AUTH_MODE", "mock")
	t.Setenv("AUTH_MOCK_ADDR", "0.0.0.0:8091")
	if err := run(discardLogger()); err == nil || !strings.Contains(err.Error(), "AUTH_MOCK_ADDR") {
		t.Fatalf("안전하지 않은 mock 주소가 Kubernetes 설정 전에 잡히지 않았습니다: %v", err)
	}

	t.Setenv("AUTH_MODE", "none")
	t.Setenv("AUTH_MOCK_ADDR", "")
	t.Setenv("DASHBOARD_BUILDER_ENABLED", "true")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DASHBOARD_CURSOR_KEY", "")
	if err := run(discardLogger()); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("builder without database did not fail before Kubernetes setup: %v", err)
	}
}

func TestRunRejectsEnabledAlertmanagerBeforeKubernetesOrListener(t *testing.T) {
	t.Setenv("AUTH_MODE", "none")
	t.Setenv("ADDR", "127.0.0.1:0")
	t.Setenv("ALERTMANAGER_ENABLED", "true")
	t.Setenv("ALERTMANAGER_URL", "https://alerts.example/prefix")
	t.Setenv("ALERTMANAGER_PUBLIC_URL", "https://alerts.example/ui")
	t.Setenv("ALERTMANAGER_TOKEN_FILE", "/definitely-missing/alertmanager-token")
	t.Setenv("ALERTMANAGER_CA_FILE", "/definitely-missing/alertmanager-ca")
	t.Setenv("ALERTMANAGER_SERVER_NAME", "alerts.example")
	err := run(discardLogger())
	if err == nil || !strings.Contains(err.Error(), "Alertmanager token file") {
		t.Fatalf("Alertmanager preflight did not fail first: %v", err)
	}
}

func TestRunRoutesValidatedCentralModeBeforeAnyKubernetesClient(t *testing.T) {
	dir := t.TempDir()
	ca, caKey := testCA(t)
	caPath := writeTestPEM(t, dir, "preflight-ca.pem", "CERTIFICATE", ca.Raw)
	apiCert, apiKey := testLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-api/preflight")
	certPath := writeTestCert(t, dir, "preflight-api", apiCert, apiKey)
	t.Setenv("CLUSTER_STATE_MODE", "central")
	t.Setenv("CLUSTER_STATE_REGISTRY_ENDPOINT", "registry.example.test:9443")
	t.Setenv("CLUSTER_STATE_REGISTRY_SERVER_NAME", "registry.example.test")
	t.Setenv("CLUSTER_STATE_CLUSTERS", "cluster-a")
	t.Setenv("CLUSTER_STATE_TRUST_DOMAIN", "example.test")
	t.Setenv("CLUSTER_STATE_TLS_CERT_FILE", certPath)
	t.Setenv("CLUSTER_STATE_TLS_KEY_FILE", filepath.Join(dir, "preflight-api.key"))
	t.Setenv("CLUSTER_STATE_TLS_CA_FILE", caPath)
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "https://issuer.example.test")
	t.Setenv("OIDC_AUDIENCE", "dashboard")
	t.Setenv("USE_DEMO_DATA", "false")
	t.Setenv("QUERY_CATALOG_DIR", filepath.Join(t.TempDir(), "missing-query-catalog"))
	err := run(discardLogger())
	if err == nil || !strings.Contains(err.Error(), "query-catalog") {
		t.Fatalf("central mode did not fail at the query catalog boundary: %v", err)
	}
}

func TestPanelQueryRefsAreSortedAndUnique(t *testing.T) {
	refs := panelQueryRefs(defaultCatalog(t))
	if len(refs) == 0 {
		t.Fatal("default catalog has no panel query refs")
	}
	for i, ref := range refs {
		if i > 0 && refs[i-1] >= ref {
			t.Fatalf("refs are not strictly sorted and unique: %v", refs)
		}
	}
}

func TestOpenDashboardStoreDisabledAndFailFast(t *testing.T) {
	store, err := openDashboardStore(context.Background(), config.DashboardBuilderConfig{})
	if err != nil || store != nil {
		t.Fatalf("disabled store=%v err=%v", store, err)
	}
	_, err = openDashboardStore(context.Background(), config.DashboardBuilderConfig{
		Enabled: true, DatabaseURL: "postgres://localhost/dashboard", CursorKey: "short", MaxConns: 1, ConnectTimeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "dashboard metadata store") {
		t.Fatalf("enabled invalid store error=%v", err)
	}
}

func TestOpenDashboardStoreWithActualPostgres(t *testing.T) {
	url := os.Getenv("DASHBOARD_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("DASHBOARD_POSTGRES_TEST_URL is not set")
	}
	parsedURL, err := urlpkg.Parse(url)
	if err != nil || parsedURL.Path != "/dashboard_ci" {
		t.Fatal("integration test requires dedicated dashboard_ci database")
	}
	store, err := openDashboardStore(context.Background(), config.DashboardBuilderConfig{
		Enabled: true, DatabaseURL: url, CursorKey: "cmd-wiring-cursor-key-00000000000000", MaxConns: 1, ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.(interface{ Close() error }).Close()
	if err := store.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestOpenDashboardStoreSQLite는 외부 인프라 없이 SQLite 백엔드 배선을 검증합니다. (ADR 0016)
func TestOpenDashboardStoreSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drafts.db")
	store, err := openDashboardStore(context.Background(), config.DashboardBuilderConfig{
		Enabled: true, DBPath: path, CursorKey: "cmd-wiring-sqlite-cursor-000000000000", MaxConns: 1, ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.(interface{ Close() error }).Close()
	if err := store.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestSourcesSelectionMatrix — 데이터소스 선택 우선순위(실주소 > 데모 >
// Unavailable)의 전체 조합입니다. 어댑터 자체는 각 패키지가 검증하고,
// 여기는 **배선**이 맞는지만 봅니다.
func TestSourcesSelectionMatrix(t *testing.T) {
	qc := defaultCatalog(t)

	// 데모 모드 — 네 어댑터 전부 데모입니다.
	cfg := config.Config{UseDemoData: true}
	m, l, a, topo := sources(discardLogger(), cfg, nil, qc)
	if m == nil || l == nil || a == nil || topo == nil {
		t.Fatal("데모 어댑터가 비었습니다")
	}
	if _, unavailable := m.(datasource.Unavailable); unavailable {
		t.Fatal("데모 모드의 메트릭이 Unavailable입니다")
	}

	// 데모 끔 · 실주소 없음 — 전부 Unavailable로 degraded를 그리게 합니다.
	cfg = config.Config{UseDemoData: false}
	m, l, a, topo = sources(discardLogger(), cfg, nil, qc)
	for name, v := range map[string]any{"metrics": m, "logs": l, "alerts": a, "topology": topo} {
		if _, ok := v.(datasource.Unavailable); !ok {
			t.Fatalf("%s가 Unavailable이 아닙니다: %T", name, v)
		}
	}

	// 실주소가 있으면 데모 여부와 무관하게 실제 어댑터가 이깁니다.
	cfg = config.Config{UseDemoData: true}
	cfg.Greptime.URL = "http://greptime.example:4000"
	cfg.Quickwit.URL = "http://quickwit.example:7280"
	m, l, a, _ = sources(discardLogger(), cfg, nil, qc)
	if _, ok := m.(datasource.Unavailable); ok {
		t.Fatal("실주소 메트릭이 Unavailable로 접혔습니다")
	}
	if strings.Contains(fmt.Sprintf("%T", m), "demo") || strings.Contains(fmt.Sprintf("%T", l), "demo") {
		t.Fatalf("실주소인데 데모 어댑터가 선택되었습니다: %T %T", m, l)
	}
	// 이 helper 호출은 준비된 Alertmanager source를 주입하지 않으므로 알림·토폴로지는 데모가 남습니다.
	if _, ok := a.(datasource.Unavailable); ok {
		t.Fatal("데모 모드의 알림이 Unavailable입니다")
	}

	// 주소가 잘못된 형식이면 그 데이터소스만 Unavailable입니다.
	cfg = config.Config{UseDemoData: false}
	cfg.Greptime.URL = "not-a-url"
	m, _, _, _ = sources(discardLogger(), cfg, nil, qc)
	if _, ok := m.(datasource.Unavailable); !ok {
		t.Fatal("잘못된 주소가 Unavailable로 접히지 않았습니다")
	}
}

// TestBuildResolverModes — AUTH_MODE 배선입니다. none은 정적, mock은 로컬
// IdP와 함께 실제 OIDC 검증 경로, 알 수 없는 값·잘못된 oidc 설정은 기동 실패입니다.
func TestBuildResolverModes(t *testing.T) {
	ctx := context.Background()

	cfg := config.Config{}
	cfg.Auth.Mode = "none"
	r, err := buildResolver(ctx, discardLogger(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(scope.Static); !ok {
		t.Fatalf("none은 정적 Scope여야 합니다: %T", r)
	}

	cfg.Auth.Mode = "mock"
	cfg.Auth.MockAddr = "127.0.0.1:0"
	cfg.ClusterID = "seoul"
	mockResolver, err := buildResolver(ctx, discardLogger(), cfg)
	if err != nil {
		t.Fatalf("mock 모드 기동 실패: %v", err)
	}
	managed, ok := mockResolver.(*managedMockResolver)
	if !ok {
		t.Fatalf("mock resolver does not own its IdP: %T", mockResolver)
	}
	issuer := managed.idp.Issuer
	if err := managed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := managed.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	client := &http.Client{Timeout: 200 * time.Millisecond}
	if response, err := client.Get(issuer + "/.well-known/openid-configuration"); err == nil {
		response.Body.Close()
		t.Fatal("mock IdP remained reachable after resolver close")
	}

	cfg.Auth.Mode = "oidc"
	cfg.Auth.Issuer = "" // issuer 없는 oidc는 설정 오류입니다.
	if _, err := buildResolver(ctx, discardLogger(), cfg); err == nil {
		t.Fatal("issuer 없는 oidc가 통과했습니다")
	}

	cfg.Auth.Mode = "what-is-this"
	if _, err := buildResolver(ctx, discardLogger(), cfg); err == nil {
		t.Fatal("알 수 없는 AUTH_MODE가 통과했습니다")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cfg.Auth.Mode = "mock"
	cfg.Auth.MockAddr = listener.Addr().String()
	if _, err := buildResolver(ctx, discardLogger(), cfg); err == nil {
		t.Fatal("mock resolver accepted an occupied listen address")
	}
}

func TestBuildResolverSessionCloseDirectAndCentral(t *testing.T) {
	idp, err := auth.StartMockIDP("127.0.0.1:0", "resolver-close", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idp.Close() })
	for _, mode := range []string{"direct", "central"} {
		t.Run(mode, func(t *testing.T) {
			redisServer := miniredis.RunT(t)
			cfg := config.Load()
			cfg.Auth = config.AuthConfig{Mode: "oidc", Issuer: idp.Issuer, Audience: "resolver-close", RolesClaim: "roles", Leeway: time.Minute, JWKSMinRefresh: time.Second}
			cfg.ClusterState.Mode = mode
			cfg.ClusterState.Clusters = []string{"a"}
			enableCentralBrowserSession(&cfg, redisServer.Addr())
			resolver, err := buildResolver(context.Background(), discardLogger(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			closer, ok := resolver.(interface{ Close() error })
			if !ok {
				t.Fatalf("resolver is not closeable: %T", resolver)
			}
			if redisServer.CurrentConnectionCount() == 0 {
				t.Fatal("session Redis connection was not opened")
			}
			if err := closer.Close(); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, time.Second, func() bool { return redisServer.CurrentConnectionCount() == 0 })
		})
	}
}

// TestRefreshUsageAppliesSnapshotAndSurvivesFailure — 사용량 갱신 루프가
// 스냅숏을 store에 반영하고, 데이터소스 오류에도 죽지 않는지 확인합니다.
func TestRefreshUsageAppliesSnapshotAndSurvivesFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	// 정상 경로 — 데모 어댑터의 사용량이 반영됩니다.
	runCtx, runCancel := context.WithCancel(ctx)
	go refreshUsage(runCtx, discardLogger(), store, demo.New(store), testcluster.ClusterID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		pods, _ := store.PodsForWorkload("payments", "Deployment", "payments-api", testcluster.UIDDeploymentPaymentsAPI)
		if len(pods) > 0 && pods[0].Usage.CPUMilli > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("사용량 스냅숏이 반영되지 않았습니다")
		}
		time.Sleep(10 * time.Millisecond)
	}
	runCancel()

	// 실패 경로 — 오류를 경고로만 남기고 패닉 없이 종료합니다.
	failCtx, failCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer failCancel()
	refreshUsage(failCtx, discardLogger(), store, datasource.Unavailable{}, testcluster.ClusterID)
}

// TestQuickwitFieldsMapping — 알려진 키만 FieldMap에 반영되고,
// 오타 키는 무시됩니다.
func TestQuickwitFieldsMapping(t *testing.T) {
	f := quickwitFields(map[string]string{
		"message":   "body.message",
		"level":     "severity_text",
		"timestamp": "ts",
		"event_id":  "log_uuid",
		"whoops":    "ignored",
	})
	if f.Message != "body.message" || f.Level != "severity_text" || f.Timestamp != "ts" || f.EventID != "log_uuid" {
		t.Fatalf("매핑: %+v", f)
	}
	if f.Namespace != "" {
		t.Fatal("지정하지 않은 필드가 채워졌습니다 (기본값은 어댑터가 채웁니다)")
	}
}
