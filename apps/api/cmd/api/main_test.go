package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	urlpkg "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/config"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(new(strings.Builder), nil))
}

func defaultCatalog(t *testing.T) querycatalog.Catalog {
	t.Helper()
	qc, err := querycatalog.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	return qc
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

func TestRunRoutesValidatedCentralModeBeforeAnyKubernetesClient(t *testing.T) {
	t.Setenv("CLUSTER_STATE_MODE", "central")
	t.Setenv("CLUSTER_STATE_REGISTRY_ENDPOINT", "registry.example.test:9443")
	t.Setenv("CLUSTER_STATE_REGISTRY_SERVER_NAME", "registry.example.test")
	t.Setenv("CLUSTER_STATE_CLUSTERS", "cluster-a")
	t.Setenv("CLUSTER_STATE_TRUST_DOMAIN", "example.test")
	t.Setenv("CLUSTER_STATE_TLS_CERT_FILE", "/nonexistent/api.crt")
	t.Setenv("CLUSTER_STATE_TLS_KEY_FILE", "/nonexistent/api.key")
	t.Setenv("CLUSTER_STATE_TLS_CA_FILE", "/nonexistent/ca.crt")
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "https://issuer.example.test")
	t.Setenv("OIDC_AUDIENCE", "dashboard")
	t.Setenv("USE_DEMO_DATA", "false")
	t.Setenv("QUERY_CATALOG_DIR", "/nonexistent/query-catalog")
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
	defer store.Close()
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
	// 알림·토폴로지는 실클라이언트가 없으므로(#17 잔여) 데모가 남습니다.
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
	if _, err := buildResolver(ctx, discardLogger(), cfg); err != nil {
		t.Fatalf("mock 모드 기동 실패: %v", err)
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
