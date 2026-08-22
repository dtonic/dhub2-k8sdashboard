package httpapi_test

// Resource Explorer 엔드포인트의 경계 검증입니다. (ADR 0018)
//
//   - 권한 없는 요청은 배포 형태와 무관하게 403이고, 권한이 있는 요청만 503을 봅니다.
//   - 요청의 ns/경로로 Scope를 넓힐 수 없습니다.
//   - cursor는 opaque이며 중복·누락이 없습니다.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

var (
	svcGVR          = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	secGVR          = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	storageGVR      = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	resourceFixture = time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC)
)

func resourceDiscovery() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "services", Namespaced: true, Kind: "Service", Verbs: []string{"get", "list", "watch"}},
			{Name: "secrets", Namespaced: true, Kind: "Secret", Verbs: []string{"get", "list", "watch"}},
		}},
		{GroupVersion: "storage.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "storageclasses", Namespaced: false, Kind: "StorageClass", Verbs: []string{"get", "list", "watch"}},
		}},
	}
}

func partialMeta(apiVersion, kind, namespace, name, uid string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: apiVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID(uid),
			CreationTimestamp: metav1.NewTime(resourceFixture),
		},
	}
}

func httpSecretObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name": "db", "namespace": "payments", "uid": "uid-secret-db", "resourceVersion": "77",
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
			"annotations":   map[string]any{"example.com/password": "s3cr3t-annotation", "example.com/team": "payments"},
		},
		"type": "Opaque",
		"data": map[string]any{"PASSWORD": "czNjcjN0"},
	}}
}

// newResourceService는 fake 클러스터 위의 Resource Explorer 서비스를 만듭니다.
// 전역 검색(ADR 0023)은 켜져 있습니다 — 기존 목록·상세 테스트도 검색 인덱스가 함께
// 만들어지는 재구성 경로를 그대로 지나게 하려는 것입니다.
func newResourceService(t *testing.T, metaObjects []runtime.Object) *resourcecatalog.Service {
	t.Helper()
	return newResourceServiceTuned(t, metaObjects, true)
}

// newResourceServiceTuned는 검색 롤백 스위치까지 지정합니다. (ADR 0023)
func newResourceServiceTuned(t *testing.T, metaObjects []runtime.Object, searchEnabled bool) *resourcecatalog.Service {
	t.Helper()
	return newResourceServiceBudget(t, metaObjects, searchEnabled, resourcecatalog.DefaultMaxSearchIndexBytes)
}

// newResourceServiceBudget은 검색 인덱스 예산까지 지정합니다.
// 예산이 모자라 리소스가 검색에서 빠지는 경계를 HTTP 계층에서 확인할 때 씁니다.
func newResourceServiceBudget(t *testing.T, metaObjects []runtime.Object, searchEnabled bool, searchMaxBytes int64) *resourcecatalog.Service {
	t.Helper()
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	meta := metadatafake.NewSimpleMetadataClient(scheme, metaObjects...)
	disc := &discoveryfake.FakeDiscovery{Fake: &k8stesting.Fake{Resources: resourceDiscovery()}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			svcGVR:     "ServiceList",
			secGVR:     "SecretList",
			storageGVR: "StorageClassList",
		}, httpSecretObject())

	service, err := resourcecatalog.New(
		resourcecatalog.Clients{Metadata: meta, Discovery: disc, Dynamic: dyn},
		resourcecatalog.Config{
			ClusterID:       testcluster.ClusterID,
			Allowlist:       []schema.GroupVersionResource{svcGVR, secGVR, storageGVR},
			RefreshInterval: time.Hour,
			Resync:          time.Hour,
			IndexInterval:   20 * time.Millisecond,
			SyncTimeout:     5 * time.Second,
			DetailRate:      100, DetailBurst: 50, DetailConcurrent: 4,
			SearchEnabled:       searchEnabled,
			MaxSearchIndexBytes: searchMaxBytes,
			Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	// 상세는 metadata 인덱스에 있는 행만 열 수 있으므로 Secret 인덱스도 준비되어야 합니다.
	waitForReady(t, service, svcGVR)
	waitForReady(t, service, secGVR)
	waitForReady(t, service, storageGVR)
	return service
}

func waitForReady(t *testing.T, service *resourcecatalog.Service, gvr schema.GroupVersionResource) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		desc, err := service.Describe(gvr)
		if err != nil {
			t.Fatal(err)
		}
		if desc.State == resourcecatalog.StateReady {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%v가 ready가 되지 않았습니다", gvr)
}

func exploreObjects() []runtime.Object {
	objs := []runtime.Object{
		partialMeta("v1", "Secret", "payments", "db", "uid-secret-db"),
		partialMeta("storage.k8s.io/v1", "StorageClass", "", "fast", "uid-sc-fast"),
	}
	for i := 0; i < 7; i++ {
		objs = append(objs, partialMeta("v1", "Service", "payments", "api-"+string(rune('a'+i)), "uid-payments-"+string(rune('a'+i))))
		objs = append(objs, partialMeta("v1", "Service", "media", "cdn-"+string(rune('a'+i)), "uid-media-"+string(rune('a'+i))))
	}
	return objs
}

// resourceFixtureFor는 Scope와 서비스 유무를 조합한 서버를 만듭니다.
func resourceFixtureFor(t *testing.T, sc scope.Scope, withService bool) fixture {
	t.Helper()
	var service *resourcecatalog.Service
	if withService {
		service = newResourceService(t, exploreObjects())
	}
	return resourceFixtureWithService(t, sc, service)
}

// resourceFixtureWithSearchOff는 검색 롤백 스위치가 내려간 배포입니다. (ADR 0023)
// Explorer는 살아 있고 검색·최근 항목 경로만 없습니다.
func resourceFixtureWithSearchOff(t *testing.T, sc scope.Scope) fixture {
	t.Helper()
	return resourceFixtureWithService(t, sc, newResourceServiceTuned(t, exploreObjects(), false))
}

// resourceFixtureWithTinySearchBudget은 어떤 리소스도 색인 예산에 들어가지 않는 배포입니다.
// 검색이 "동기화 중"이 아니라 "예산 초과"로 분류하는지 확인할 때 씁니다. (P1-D)
func resourceFixtureWithTinySearchBudget(t *testing.T, sc scope.Scope) fixture {
	t.Helper()
	// GVR 몫은 전체의 1/2이므로 512는 256바이트입니다. 스냅숏 하나의 고정 비용만으로도
	// 그 몫을 넘기므로 **모든** GVR이 색인되지 않습니다. 객체 수에 기대지 않는 값이어야
	// 픽스처가 우연히 ready를 만들어 내는 일이 없습니다.
	// 설정 계층의 하한(16MiB)은 운영 값이고, 여기서는 경계 자체를 확인하려고 그 아래를 직접 넣습니다.
	return resourceFixtureWithService(t, sc, newResourceServiceBudget(t, exploreObjects(), true, 512))
}

func resourceFixtureWithService(t *testing.T, sc scope.Scope, service *resourcecatalog.Service) fixture {
	t.Helper()
	return newFixture(t, func(d *httpapi.Deps) {
		d.Resolver = scope.Static{S: sc}
		d.Resources = service
		d.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	})
}

func explorerScope(all bool, namespaces ...string) scope.Scope {
	return scope.Scope{
		Subject:             "admin",
		CanExploreResources: true,
		Clusters: []scope.Cluster{{
			ID: testcluster.ClusterID, Name: "Seoul Production", All: all, Namespaces: namespaces,
		}},
	}
}

func resourcePath(suffix string) string {
	return "/api/v1/clusters/" + testcluster.ClusterID + "/resources" + suffix
}

func explorerGet(t *testing.T, f fixture, url string, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s 응답 파싱 실패: %v (%s)", url, err, rec.Body.String())
		}
	}
	return rec
}

func explorerErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var apiErr contract.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("에러 본문 파싱 실패: %v (%s)", err, rec.Body.String())
	}
	if apiErr.RequestID == "" {
		t.Fatal("에러 본문에 requestId가 없습니다")
	}
	return apiErr.Code
}

/* ── 권한 ─────────────────────────────────────────────────────────────────── */

// TestResourceEndpointsRequireExploreCapability — capability가 없으면 전부 403입니다.
func TestResourceEndpointsRequireExploreCapability(t *testing.T) {
	viewer := explorerScope(true)
	viewer.CanExploreResources = false
	f := resourceFixtureFor(t, viewer, true)
	for _, url := range []string{
		resourcePath(""),
		resourcePath("/core/v1/services"),
		resourcePath("/core/v1/secrets/object?namespace=payments&name=db&uid=uid-secret-db"),
	} {
		rec := explorerGet(t, f, url, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s viewer = %d, want 403", url, rec.Code)
		}
		if code := explorerErrorCode(t, rec); code != "forbidden" {
			t.Fatalf("%s code=%q", url, code)
		}
	}
}

// TestResourceEndpointsDenyForeignClusterBeforeAvailability — 권한 판정이 먼저입니다.
// 다른 클러스터 ID로는 이 배포에 기능이 있는지조차 알 수 없어야 합니다.
func TestResourceEndpointsDenyForeignClusterBeforeAvailability(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), false)
	rec := explorerGet(t, f, "/api/v1/clusters/other-cluster/resources", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("다른 클러스터 = %d, want 403", rec.Code)
	}
	if code := explorerErrorCode(t, rec); code != "cluster_access_denied" {
		t.Fatalf("code=%q", code)
	}
}

// TestResourceEndpointsAreStable503WhenServiceIsAbsent — central 배포의 계약입니다.
func TestResourceEndpointsAreStable503WhenServiceIsAbsent(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), false)
	for _, url := range []string{
		resourcePath(""),
		resourcePath("/core/v1/services"),
		resourcePath("/core/v1/secrets/object?namespace=payments&name=db&uid=uid-secret-db"),
	} {
		rec := explorerGet(t, f, url, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s central = %d, want 503 (%s)", url, rec.Code, rec.Body.String())
		}
		if code := explorerErrorCode(t, rec); code != "resources_unavailable" {
			t.Fatalf("%s code=%q", url, code)
		}
	}
}

// TestScopeCapabilityRequiresBothPermissionAndAvailability — 화면 진입점 노출 조건입니다.
func TestScopeCapabilityRequiresBothPermissionAndAvailability(t *testing.T) {
	read := func(f fixture) contract.ScopeResponse {
		var out contract.ScopeResponse
		rec := explorerGet(t, f, "/api/v1/scope", &out)
		if rec.Code != http.StatusOK {
			t.Fatalf("scope = %d", rec.Code)
		}
		return out
	}
	if got := read(resourceFixtureFor(t, explorerScope(true), true)); !got.CanExploreResources {
		t.Fatal("admin + 서비스 있음인데 canExploreResources가 false입니다")
	}
	if got := read(resourceFixtureFor(t, explorerScope(true), false)); got.CanExploreResources {
		t.Fatal("서비스가 없는데 canExploreResources가 true입니다")
	}
	viewer := explorerScope(true)
	viewer.CanExploreResources = false
	if got := read(resourceFixtureFor(t, viewer, true)); got.CanExploreResources {
		t.Fatal("권한이 없는데 canExploreResources가 true입니다")
	}
}

/* ── Scope 강제 ───────────────────────────────────────────────────────────── */

// TestResourceListCannotEscapeScope — URL을 고쳐도 Scope 밖 행은 한 줄도 나오지 않습니다.
func TestResourceListCannotEscapeScope(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(false, "payments"), true)

	rec := explorerGet(t, f, resourcePath("/core/v1/services?ns=media"), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Scope 밖 namespace 요청 = %d, want 403", rec.Code)
	}
	if code := explorerErrorCode(t, rec); code != "namespace_access_denied" {
		t.Fatalf("code=%q", code)
	}

	var page contract.ResourceListResponse
	if rec := explorerGet(t, f, resourcePath("/core/v1/services?limit=200"), &page); rec.Code != http.StatusOK {
		t.Fatalf("목록 = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(page.Items) == 0 {
		t.Fatal("Scope 안 행이 하나도 없습니다")
	}
	for _, item := range page.Items {
		if item.Namespace != "payments" {
			t.Fatalf("Scope 밖 행이 나왔습니다: %+v", item)
		}
	}
}

// TestClusterScopedResourceNeedsClusterWideScope — namespace 사용자에게 0건이 아니라
// "권한 없음"을 돌려줍니다. 둘을 같은 빈 화면으로 만들지 않습니다.
func TestClusterScopedResourceNeedsClusterWideScope(t *testing.T) {
	narrow := resourceFixtureFor(t, explorerScope(false, "payments"), true)
	rec := explorerGet(t, narrow, resourcePath("/storage.k8s.io/v1/storageclasses"), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("namespace 사용자 cluster 범위 조회 = %d, want 403", rec.Code)
	}
	if code := explorerErrorCode(t, rec); code != "cluster_scope_required" {
		t.Fatalf("code=%q", code)
	}

	wide := resourceFixtureFor(t, explorerScope(true), true)
	var page contract.ResourceListResponse
	if rec := explorerGet(t, wide, resourcePath("/storage.k8s.io/v1/storageclasses"), &page); rec.Code != http.StatusOK {
		t.Fatalf("cluster 전체 권한 조회 = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(page.Items) != 1 || page.Items[0].Name != "fast" || page.Namespaced {
		t.Fatalf("cluster 범위 응답이 이상합니다: %+v", page)
	}
	if rec := explorerGet(t, wide, resourcePath("/storage.k8s.io/v1/storageclasses?ns=payments"), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("cluster 범위 + ns = %d, want 400", rec.Code)
	}
}

// TestResourceDetailCannotEscapeScope — 상세도 같은 Scope를 탑니다.
func TestResourceDetailCannotEscapeScope(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(false, "payments"), true)
	rec := explorerGet(t, f, resourcePath("/core/v1/secrets/object?namespace=media&name=db&uid=uid-secret-db"), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Scope 밖 상세 = %d, want 403", rec.Code)
	}
	if code := explorerErrorCode(t, rec); code != "namespace_access_denied" {
		t.Fatalf("code=%q", code)
	}
}

/* ── 카탈로그·목록·페이지 ────────────────────────────────────────────────── */

func TestResourceCatalogReportsAllowlistAndState(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	var out contract.ResourceCatalogResponse
	if rec := explorerGet(t, f, resourcePath(""), &out); rec.Code != http.StatusOK {
		t.Fatalf("카탈로그 = %d", rec.Code)
	}
	if out.ClusterID != testcluster.ClusterID || len(out.Items) != 3 {
		t.Fatalf("카탈로그 응답이 이상합니다: %+v", out)
	}
	byResource := map[string]contract.ResourceDescriptor{}
	for _, item := range out.Items {
		byResource[item.Group+"/"+item.Resource] = item
	}
	services, ok := byResource["core/services"]
	if !ok {
		t.Fatalf("core group 표기가 %v에 없습니다", byResource)
	}
	if services.Kind != "Service" || !services.Namespaced || services.State != contract.ResourceStateReady {
		t.Fatalf("services descriptor: %+v", services)
	}
	if classes := byResource["storage.k8s.io/storageclasses"]; classes.Namespaced {
		t.Fatalf("StorageClass가 namespaced로 나왔습니다: %+v", classes)
	}
}

// TestResourceListPagingHasNoDuplicatesOrGaps — cursor를 따라간 결과가 정확히 전체입니다.
func TestResourceListPagingHasNoDuplicatesOrGaps(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	seen := map[string]bool{}
	url := resourcePath("/core/v1/services?limit=3")
	for i := 0; i < 20; i++ {
		var page contract.ResourceListResponse
		rec := explorerGet(t, f, url, &page)
		if rec.Code != http.StatusOK {
			t.Fatalf("페이지 %d = %d (%s)", i, rec.Code, rec.Body.String())
		}
		if len(page.Items) > 3 {
			t.Fatalf("limit을 넘겼습니다: %d행", len(page.Items))
		}
		for _, item := range page.Items {
			key := item.Namespace + "/" + item.Name
			if seen[key] {
				t.Fatalf("중복 행: %s", key)
			}
			seen[key] = true
		}
		if page.NextCursor == "" {
			if len(seen) != 14 {
				t.Fatalf("전체 %d행 (want 14)", len(seen))
			}
			if page.Total != 14 {
				t.Fatalf("total=%d (want 14)", page.Total)
			}
			return
		}
		url = resourcePath("/core/v1/services?limit=3&cursor=" + page.NextCursor)
	}
	t.Fatal("cursor 순회가 끝나지 않았습니다")
}

func TestResourceListRejectsMalformedInput(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	cases := map[string]string{
		"limit 초과":      resourcePath("/core/v1/services?limit=201"),
		"limit 문자":      resourcePath("/core/v1/services?limit=abc"),
		"깨진 cursor":     resourcePath("/core/v1/services?cursor=%21%21%21"),
		"다른 질의의 cursor": resourcePath("/core/v1/services?limit=3&cursor=" + "MXwwMDAwMDAwMDAwMDAwMDAwfHBheW1lbnRzfGFwaS1h"),
		"깨진 selector":   resourcePath("/core/v1/services?labelSelector=" + strings.Repeat("a", 600)),
		"모르는 정렬 키":      resourcePath("/core/v1/services?sort=created"),
		"모르는 정렬 방향":     resourcePath("/core/v1/services?order=sideways"),
	}
	for name, url := range cases {
		rec := explorerGet(t, f, url, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
		if code := explorerErrorCode(t, rec); code != "invalid_filter" && code != "invalid_cursor" {
			t.Fatalf("%s code=%q", name, code)
		}
	}
}

func TestResourceListRejectsUnknownAndUnallowlistedResources(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	// allowlist 밖(등록되지 않음)
	rec := explorerGet(t, f, resourcePath("/core/v1/configmaps"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("allowlist 밖 = %d, want 404", rec.Code)
	}
	if code := explorerErrorCode(t, rec); code != "resource_not_allowlisted" {
		t.Fatalf("code=%q", code)
	}
	// 형식 자체가 틀린 경로
	if rec := explorerGet(t, f, resourcePath("/core/V1/Services"), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("잘못된 경로 = %d, want 400", rec.Code)
	}
}

/* ── 상세 ─────────────────────────────────────────────────────────────────── */

func TestResourceDetailRedactsSecretsAndVerifiesUID(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	var detail contract.ResourceDetailResponse
	rec := explorerGet(t, f, resourcePath("/core/v1/secrets/object?namespace=payments&name=db&uid=uid-secret-db"), &detail)
	if rec.Code != http.StatusOK {
		t.Fatalf("상세 = %d (%s)", rec.Code, rec.Body.String())
	}
	for _, forbidden := range []string{"czNjcjN0", "PASSWORD", "s3cr3t-annotation", "managedFields"} {
		if strings.Contains(detail.YAML, forbidden) {
			t.Fatalf("응답에 %q가 남아 있습니다", forbidden)
		}
	}
	if detail.UID != "uid-secret-db" || detail.ResourceVersion != "77" || detail.Kind != "Secret" {
		t.Fatalf("상세 응답이 이상합니다: %+v", detail)
	}
	if len(detail.Redacted) == 0 {
		t.Fatal("무엇이 가려졌는지 알려주지 않습니다")
	}

	rec = explorerGet(t, f, resourcePath("/core/v1/secrets/object?namespace=payments&name=db&uid=uid-old"), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("UID 교체 = %d, want 409", rec.Code)
	}
	if code := explorerErrorCode(t, rec); code != "uid_mismatch" {
		t.Fatalf("code=%q", code)
	}
}

// TestResourceDetailOnlyOpensListedRows — 목록에 없는 이름은 404이고, 그 요청은
// Kubernetes로 나가지 않습니다. 사용자가 지어낸 신원으로 클러스터를 훑을 수 없습니다.
func TestResourceDetailOnlyOpensListedRows(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	rec := explorerGet(t, f, resourcePath("/core/v1/secrets/object?namespace=payments&name=not-listed&uid=uid-secret-db"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("목록에 없는 이름 = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if code := explorerErrorCode(t, rec); code != "not_found" {
		t.Fatalf("code=%q", code)
	}
	// namespace는 Scope 안이지만 그 안에 그 행이 없는 경우도 같습니다.
	rec = explorerGet(t, f, resourcePath("/core/v1/secrets/object?namespace=media&name=db&uid=uid-secret-db"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("다른 namespace의 같은 이름 = %d, want 404", rec.Code)
	}
}

// TestResourceDetailBoundedToAllowlistAndCache — 상세는 allowlist와 로컬 캐시 두 경계를
// 모두 통과한 신원에서만 열립니다.
func TestResourceDetailBoundedToAllowlistAndCache(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	// storageclasses는 allowlist에 있고 ready이지만 이 이름의 행은 캐시에 없습니다.
	rec := explorerGet(t, f, resourcePath("/storage.k8s.io/v1/storageclasses/object?name=missing&uid=uid-sc-fast"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("캐시에 없는 cluster 범위 항목 = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	// allowlist 밖은 상세에서도 404 resource_not_allowlisted입니다.
	rec = explorerGet(t, f, resourcePath("/core/v1/configmaps/object?namespace=payments&name=db&uid=x"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("allowlist 밖 상세 = %d, want 404", rec.Code)
	}
	if code := explorerErrorCode(t, rec); code != "resource_not_allowlisted" {
		t.Fatalf("code=%q", code)
	}
}

func TestResourceDetailRequiresIdentity(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	for name, url := range map[string]string{
		"uid 없음":       resourcePath("/core/v1/secrets/object?namespace=payments&name=db"),
		"이름 없음":        resourcePath("/core/v1/secrets/object?namespace=payments&uid=uid-secret-db"),
		"namespace 없음": resourcePath("/core/v1/secrets/object?name=db&uid=uid-secret-db"),
	} {
		if rec := explorerGet(t, f, url, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", name, rec.Code)
		}
	}
}
