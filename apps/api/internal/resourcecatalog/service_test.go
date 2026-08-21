package resourcecatalog_test

// Resource Explorer 서비스의 경계 검증입니다.
//
//   - 카탈로그·목록 요청은 Kubernetes를 호출하지 않는다.
//   - metadata 미지원(406)은 명시적이며 **full-object watch로 물러나지 않는다.**
//   - 상세는 격리된 client에서만, Secret 값은 0바이트로 나간다.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
)

const testClusterID = "prod-seoul"

var (
	serviceGVR      = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	secretGVR       = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	storageClassGVR = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	widgetGVR       = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
)

func defaultDiscovery() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "services", Namespaced: true, Kind: "Service", Verbs: []string{"get", "list", "watch"}},
			{Name: "secrets", Namespaced: true, Kind: "Secret", Verbs: []string{"get", "list", "watch"}},
			// subresource는 카탈로그 대상이 아닙니다.
			{Name: "services/status", Namespaced: true, Kind: "Service", Verbs: []string{"get"}},
		}},
		{GroupVersion: "storage.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "storageclasses", Namespaced: false, Kind: "StorageClass", Verbs: []string{"get", "list", "watch"}},
		}},
	}
}

func metaObject(apiVersion, kind, namespace, name, uid string, labels map[string]string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: apiVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID(uid), Labels: labels,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC)),
		},
	}
}

func listKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		serviceGVR:      "ServiceList",
		secretGVR:       "SecretList",
		storageClassGVR: "StorageClassList",
		widgetGVR:       "WidgetList",
	}
}

type options struct {
	allowlist   []schema.GroupVersionResource
	discovery   []*metav1.APIResourceList
	metaObjects []runtime.Object
	dynObjects  []runtime.Object
	metaSetup   func(*metadatafake.FakeMetadataClient)
	dynSetup    func(*dynamicfake.FakeDynamicClient)
	tune        func(*resourcecatalog.Config)
}

type harness struct {
	svc  *resourcecatalog.Service
	meta *metadatafake.FakeMetadataClient
	disc *discoveryfake.FakeDiscovery
	dyn  *dynamicfake.FakeDynamicClient
}

// actions는 세 클라이언트가 지금까지 받은 호출 수의 합입니다.
func (h *harness) actions() int {
	return len(h.meta.Actions()) + len(h.disc.Actions()) + len(h.dyn.Actions())
}

func start(t *testing.T, o options) *harness {
	t.Helper()
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	meta := metadatafake.NewSimpleMetadataClient(scheme, o.metaObjects...)
	if o.metaSetup != nil {
		o.metaSetup(meta)
	}
	if o.discovery == nil {
		o.discovery = defaultDiscovery()
	}
	disc := &discoveryfake.FakeDiscovery{Fake: &k8stesting.Fake{Resources: o.discovery}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds(), o.dynObjects...)
	if o.dynSetup != nil {
		o.dynSetup(dyn)
	}
	if len(o.allowlist) == 0 {
		o.allowlist = []schema.GroupVersionResource{serviceGVR, secretGVR}
	}
	cfg := resourcecatalog.Config{
		ClusterID:       testClusterID,
		Allowlist:       o.allowlist,
		AllowCRDs:       true,
		RefreshInterval: time.Hour,
		Resync:          time.Hour,
		IndexInterval:   20 * time.Millisecond,
		SyncTimeout:     5 * time.Second,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if o.tune != nil {
		o.tune(&cfg)
	}
	svc, err := resourcecatalog.New(resourcecatalog.Clients{Metadata: meta, Discovery: disc, Dynamic: dyn}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return &harness{svc: svc, meta: meta, disc: disc, dyn: dyn}
}

func waitForState(t *testing.T, svc *resourcecatalog.Service, gvr schema.GroupVersionResource, want resourcecatalog.State) resourcecatalog.Descriptor {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last resourcecatalog.Descriptor
	for time.Now().Before(deadline) {
		desc, err := svc.Describe(gvr)
		if err != nil {
			t.Fatalf("describe %v: %v", gvr, err)
		}
		last = desc
		if desc.State == want {
			return desc
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%v 상태가 %q가 되지 않았습니다: %q (%s)", gvr, want, last.State, last.Reason)
	return last
}

/* ── 카탈로그와 수명 ─────────────────────────────────────────────────────── */

func TestCatalogContainsOnlyAllowlistedResources(t *testing.T) {
	h := start(t, options{metaObjects: []runtime.Object{
		metaObject("v1", "Service", "payments", "api", "uid-svc-api", nil),
	}})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	snapshot := h.svc.Catalog()
	if len(snapshot.Descriptors) != 2 {
		t.Fatalf("카탈로그 항목 %d개 (want 2 = allowlist 크기)", len(snapshot.Descriptors))
	}
	if snapshot.RefreshedAt.IsZero() {
		t.Fatal("discovery snapshot 시각이 비어 있습니다")
	}
	for _, d := range snapshot.Descriptors {
		if d.Resource == "services" {
			if d.Kind != "Service" || !d.Namespaced {
				t.Fatalf("discovery 사실이 반영되지 않았습니다: %+v", d)
			}
		}
		if strings.Contains(d.Resource, "/") {
			t.Fatalf("subresource가 카탈로그에 들어왔습니다: %s", d.Resource)
		}
	}
}

// TestCatalogAndListNeverCallKubernetes — ADR 0004의 1번 규칙입니다.
func TestCatalogAndListNeverCallKubernetes(t *testing.T) {
	h := start(t, options{metaObjects: []runtime.Object{
		metaObject("v1", "Service", "payments", "api", "uid-svc-api", nil),
		metaObject("v1", "Service", "media", "cdn", "uid-svc-cdn", nil),
	}})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)
	time.Sleep(100 * time.Millisecond) // 기동 중 호출이 모두 기록되도록 잠깐 둡니다.

	before := h.actions()
	for i := 0; i < 25; i++ {
		h.svc.Catalog()
		if _, _, err := h.svc.List(resourcecatalog.ListRequest{
			Group: serviceGVR.Group, Version: serviceGVR.Version, Resource: serviceGVR.Resource,
			Namespaces: resourcecatalog.NamespaceFilter{All: true},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.actions() - before; got != 0 {
		t.Fatalf("조회 중 Kubernetes 호출 %d회 발생 (want 0)", got)
	}
}

func TestListReadsScopedRowsFromTheLocalIndex(t *testing.T) {
	h := start(t, options{metaObjects: []runtime.Object{
		metaObject("v1", "Service", "payments", "api", "uid-svc-api", map[string]string{"tier": "web"}),
		metaObject("v1", "Service", "payments", "worker", "uid-svc-worker", map[string]string{"tier": "batch"}),
		metaObject("v1", "Service", "media", "cdn", "uid-svc-cdn", nil),
	}})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	page, desc, err := h.svc.List(resourcecatalog.ListRequest{
		Group: serviceGVR.Group, Version: serviceGVR.Version, Resource: serviceGVR.Resource,
		Namespaces: resourcecatalog.NamespaceFilter{List: []string{"payments"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if desc.Kind != "Service" {
		t.Fatalf("kind=%q", desc.Kind)
	}
	if len(page.Items) != 2 {
		t.Fatalf("payments 행 %d개 (want 2): %+v", len(page.Items), page.Items)
	}
	for _, item := range page.Items {
		if item.Namespace != "payments" {
			t.Fatalf("Scope 밖 행이 나왔습니다: %+v", item)
		}
		if item.UID == "" {
			t.Fatalf("UID가 비어 있습니다: %+v", item)
		}
	}
}

// TestUnsupportedMetadataNeverFallsBackToFullObjects — 406은 명시적 상태이며,
// full-object list/watch로 조용히 물러나지 않습니다. (ADR 0018 결정 4)
func TestUnsupportedMetadataNeverFallsBackToFullObjects(t *testing.T) {
	h := start(t, options{
		metaSetup: func(meta *metadatafake.FakeMetadataClient) {
			meta.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewGenericServerResponse(
					http.StatusNotAcceptable, "get", serviceGVR.GroupResource(), "", "not acceptable", 0, false)
			})
		},
		tune: func(c *resourcecatalog.Config) { c.SyncTimeout = 2 * time.Second },
	})
	desc := waitForState(t, h.svc, serviceGVR, resourcecatalog.StateUnsupported)
	if desc.Reason == "" {
		t.Fatal("unsupported 사유가 비어 있습니다 — 사용자가 이유를 알 수 없습니다")
	}
	if _, _, err := h.svc.List(resourcecatalog.ListRequest{
		Group: serviceGVR.Group, Version: serviceGVR.Version, Resource: serviceGVR.Resource,
		Namespaces: resourcecatalog.NamespaceFilter{All: true},
	}); !errors.Is(err, resourcecatalog.ErrUnsupported) {
		t.Fatalf("unsupported 목록이 %v를 돌려줬습니다 (want ErrUnsupported)", err)
	}
	for _, action := range h.dyn.Actions() {
		if verb := action.GetVerb(); verb == "list" || verb == "watch" {
			t.Fatalf("full-object %s fallback이 발생했습니다: %v", verb, action.GetResource())
		}
	}
	// 되지 않는 watch를 계속 재시도하지 않습니다.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		before := len(h.meta.Actions())
		time.Sleep(300 * time.Millisecond)
		if len(h.meta.Actions()) == before {
			return
		}
	}
	t.Fatal("406 이후에도 metadata LIST 재시도가 멈추지 않았습니다")
}

func TestForbiddenListIsExplicit(t *testing.T) {
	h := start(t, options{
		metaSetup: func(meta *metadatafake.FakeMetadataClient) {
			meta.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(serviceGVR.GroupResource(), "", errors.New("denied"))
			})
		},
		tune: func(c *resourcecatalog.Config) { c.SyncTimeout = 2 * time.Second },
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateForbidden)
	if _, _, err := h.svc.List(resourcecatalog.ListRequest{
		Group: serviceGVR.Group, Version: serviceGVR.Version, Resource: serviceGVR.Resource,
		Namespaces: resourcecatalog.NamespaceFilter{All: true},
	}); !errors.Is(err, resourcecatalog.ErrForbidden) {
		t.Fatalf("forbidden 목록이 %v를 돌려줬습니다", err)
	}
}

func TestMissingAndNotAllowlistedAreDifferentAnswers(t *testing.T) {
	h := start(t, options{allowlist: []schema.GroupVersionResource{serviceGVR, widgetGVR}})
	waitForState(t, h.svc, widgetGVR, resourcecatalog.StateMissing)

	if _, _, err := h.svc.List(resourcecatalog.ListRequest{
		Group: widgetGVR.Group, Version: widgetGVR.Version, Resource: widgetGVR.Resource,
		Namespaces: resourcecatalog.NamespaceFilter{All: true},
	}); !errors.Is(err, resourcecatalog.ErrNotServed) {
		t.Fatalf("미설치 CRD가 %v를 돌려줬습니다 (want ErrNotServed)", err)
	}
	if _, _, err := h.svc.List(resourcecatalog.ListRequest{
		Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
		Namespaces: resourcecatalog.NamespaceFilter{All: true},
	}); !errors.Is(err, resourcecatalog.ErrNotAllowlisted) {
		t.Fatalf("allowlist 밖 리소스가 %v를 돌려줬습니다 (want ErrNotAllowlisted)", err)
	}
}

func TestClusterScopedResourceIndexesWithoutNamespaces(t *testing.T) {
	h := start(t, options{
		allowlist:   []schema.GroupVersionResource{storageClassGVR},
		metaObjects: []runtime.Object{metaObject("storage.k8s.io/v1", "StorageClass", "", "fast", "uid-sc-fast", nil)},
	})
	desc := waitForState(t, h.svc, storageClassGVR, resourcecatalog.StateReady)
	if desc.Namespaced {
		t.Fatal("StorageClass가 namespaced로 잡혔습니다")
	}
	page, _, err := h.svc.List(resourcecatalog.ListRequest{
		Group: storageClassGVR.Group, Version: storageClassGVR.Version, Resource: storageClassGVR.Resource,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "fast" {
		t.Fatalf("cluster 범위 목록 %+v", page.Items)
	}
}

func TestInvalidFiltersAreRejectedNotTruncated(t *testing.T) {
	h := start(t, options{metaObjects: []runtime.Object{
		metaObject("v1", "Service", "payments", "api", "uid-svc-api", nil),
	}})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	base := resourcecatalog.ListRequest{
		Group: serviceGVR.Group, Version: serviceGVR.Version, Resource: serviceGVR.Resource,
		Namespaces: resourcecatalog.NamespaceFilter{All: true},
	}
	cases := map[string]resourcecatalog.ListRequest{
		"limit 초과":    {Limit: resourcecatalog.MaxPageSize + 1},
		"긴 name":      {NamePrefix: strings.Repeat("n", resourcecatalog.MaxNameFilterLen+1)},
		"이상한 name 문자": {NamePrefix: "a b"},
		"긴 selector":  {LabelSelector: "a=" + strings.Repeat("b", resourcecatalog.MaxSelectorLen)},
		"깨진 selector": {LabelSelector: "a==="},
		"selector 과다": {LabelSelector: "a=1,b=2,c=3,d=4,e=5,f=6,g=7,h=8,i=9"},
	}
	for name, override := range cases {
		req := base
		if override.Limit != 0 {
			req.Limit = override.Limit
		}
		req.NamePrefix = override.NamePrefix
		req.LabelSelector = override.LabelSelector
		if _, _, err := h.svc.List(req); !errors.Is(err, resourcecatalog.ErrInvalidFilter) {
			t.Fatalf("%s가 %v를 돌려줬습니다 (want ErrInvalidFilter)", name, err)
		}
	}
	req := base
	req.Cursor = "!!!not-base64!!!"
	if _, _, err := h.svc.List(req); !errors.Is(err, resourcecatalog.ErrInvalidCursor) {
		t.Fatalf("깨진 cursor가 %v를 돌려줬습니다 (want ErrInvalidCursor)", err)
	}
}

func TestCloseStopsEverythingAndIsIdempotent(t *testing.T) {
	h := start(t, options{metaObjects: []runtime.Object{
		metaObject("v1", "Service", "payments", "api", "uid-svc-api", nil),
	}})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	h.svc.Close()
	h.svc.Close() // 두 번 불러도 안전해야 합니다.
	if h.svc.Available() {
		t.Fatal("Close 후에도 Available이 true입니다")
	}
	if _, _, err := h.svc.List(resourcecatalog.ListRequest{
		Group: serviceGVR.Group, Version: serviceGVR.Version, Resource: serviceGVR.Resource,
		Namespaces: resourcecatalog.NamespaceFilter{All: true},
	}); !errors.Is(err, resourcecatalog.ErrUnavailable) {
		t.Fatalf("Close 후 목록이 %v를 돌려줬습니다 (want ErrUnavailable)", err)
	}
	if _, err := h.svc.Get(context.Background(), resourcecatalog.DetailRequest{
		Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
		Namespace: "payments", Name: "db", ExpectedUID: "uid-secret-db",
	}); !errors.Is(err, resourcecatalog.ErrUnavailable) {
		t.Fatalf("Close 후 상세가 %v를 돌려줬습니다", err)
	}
}

// TestNilServiceIsSafelyUnavailable — central 배포는 서비스를 만들지 않습니다.
func TestNilServiceIsSafelyUnavailable(t *testing.T) {
	var svc *resourcecatalog.Service
	if svc.Available() {
		t.Fatal("nil 서비스가 available입니다")
	}
	if snapshot := svc.Catalog(); len(snapshot.Descriptors) != 0 {
		t.Fatalf("nil 서비스가 카탈로그를 냈습니다: %+v", snapshot)
	}
	if _, _, err := svc.List(resourcecatalog.ListRequest{}); !errors.Is(err, resourcecatalog.ErrUnavailable) {
		t.Fatalf("nil 서비스 목록이 %v를 돌려줬습니다", err)
	}
	svc.Close()
}

/* ── 상세 ─────────────────────────────────────────────────────────────────── */

func secretObject(namespace, name, uid string, extra map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name": name, "namespace": namespace, "uid": uid, "resourceVersion": "4242",
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"PASSWORD":"czNjcjN0"}}`,
				"example.com/api-token":                            "t0ps3cr3t-annotation",
				"example.com/owner":                                "payments-team",
			},
			"managedFields": []any{map[string]any{"manager": "kubectl", "operation": "Apply"}},
		},
		"type": "Opaque",
		"data": map[string]any{"PASSWORD": "czNjcjN0"},
	}
	for k, v := range extra {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}

func detailHarness(t *testing.T, o options) *harness {
	t.Helper()
	if len(o.allowlist) == 0 {
		o.allowlist = []schema.GroupVersionResource{serviceGVR, secretGVR}
	}
	if o.dynObjects == nil {
		o.dynObjects = []runtime.Object{secretObject("payments", "db", "uid-secret-db", nil)}
	}
	// 상세는 목록에서 본 행만 열 수 있으므로 metadata 인덱스에도 같은 신원이 있어야 합니다.
	if o.metaObjects == nil {
		o.metaObjects = []runtime.Object{metaObject("v1", "Secret", "payments", "db", "uid-secret-db", nil)}
	}
	h := start(t, o)
	waitForState(t, h.svc, secretGVR, resourcecatalog.StateReady)
	return h
}

// liveGets는 상세 전용 dynamic client가 실제로 API 서버를 부른 횟수입니다.
func (h *harness) liveGets() int {
	count := 0
	for _, action := range h.dyn.Actions() {
		if action.GetVerb() == "get" {
			count++
		}
	}
	return count
}

// TestSecretValuesNeverLeaveTheServer — Secret 값은 0바이트입니다. (ADR 0018 결정 6)
func TestSecretValuesNeverLeaveTheServer(t *testing.T) {
	h := detailHarness(t, options{})
	detail, err := h.svc.Get(context.Background(), resourcecatalog.DetailRequest{
		Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
		Namespace: "payments", Name: "db", ExpectedUID: "uid-secret-db",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"czNjcjN0", "PASSWORD", "t0ps3cr3t-annotation", "managedFields", "last-applied-configuration"} {
		if strings.Contains(detail.YAML, forbidden) {
			t.Fatalf("정제된 매니페스트에 %q가 남아 있습니다", forbidden)
		}
	}
	if !strings.Contains(detail.YAML, "payments-team") {
		t.Fatal("민감하지 않은 annotation까지 사라졌습니다")
	}
	if detail.UID != "uid-secret-db" || detail.ResourceVersion != "4242" {
		t.Fatalf("uid/resourceVersion이 비어 있습니다: %+v", detail)
	}
	want := map[string]bool{"data": false, "metadata.managedFields": false}
	for _, path := range detail.Redacted {
		if _, ok := want[path]; ok {
			want[path] = true
		}
	}
	for path, seen := range want {
		if !seen {
			t.Fatalf("제거 목록에 %q가 없습니다: %v", path, detail.Redacted)
		}
	}
}

// TestDetailRejectsUIDReplacement — 같은 이름의 다른 객체를 대신 보여주지 않습니다.
// 캐시 행의 UID와 다르면 API 서버로 나가는 요청 자체가 없습니다.
func TestDetailRejectsUIDReplacement(t *testing.T) {
	h := detailHarness(t, options{})
	if _, err := h.svc.Get(context.Background(), resourcecatalog.DetailRequest{
		Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
		Namespace: "payments", Name: "db", ExpectedUID: "uid-secret-db-OLD",
	}); !errors.Is(err, resourcecatalog.ErrUIDMismatch) {
		t.Fatalf("교체된 객체가 %v를 돌려줬습니다 (want ErrUIDMismatch)", err)
	}
	if got := h.liveGets(); got != 0 {
		t.Fatalf("UID가 다른데 live GET이 %d회 나갔습니다", got)
	}
}

// TestDetailOnlyOpensRowsPresentInTheMetadataCache — 상세는 "목록에서 본 행"만 엽니다.
// 사용자가 지어낸 이름·namespace·UID로는 읽기 전용 캐시 경계를 넘을 수 없습니다.
func TestDetailOnlyOpensRowsPresentInTheMetadataCache(t *testing.T) {
	h := detailHarness(t, options{})
	base := resourcecatalog.DetailRequest{
		Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
		Namespace: "payments", Name: "db", ExpectedUID: "uid-secret-db",
	}
	notInCache := map[string]func(*resourcecatalog.DetailRequest){
		"캐시에 없는 이름":        func(r *resourcecatalog.DetailRequest) { r.Name = "other" },
		"캐시에 없는 namespace": func(r *resourcecatalog.DetailRequest) { r.Namespace = "media" },
	}
	for name, mutate := range notInCache {
		req := base
		mutate(&req)
		if _, err := h.svc.Get(context.Background(), req); !errors.Is(err, resourcecatalog.ErrObjectNotFound) {
			t.Fatalf("%s가 %v를 돌려줬습니다 (want ErrObjectNotFound)", name, err)
		}
	}
	if got := h.liveGets(); got != 0 {
		t.Fatalf("캐시에 없는 대상에 live GET이 %d회 나갔습니다", got)
	}
	// 정확히 일치하는 신원만 API 서버로 나갑니다.
	if _, err := h.svc.Get(context.Background(), base); err != nil {
		t.Fatalf("캐시에 있는 행 조회가 실패했습니다: %v", err)
	}
	if got := h.liveGets(); got != 1 {
		t.Fatalf("정확한 신원 조회의 live GET 횟수=%d (want 1)", got)
	}
}

// TestDetailRequiresReadyCatalogEntry — ready가 아닌 항목은 상세도 열리지 않습니다.
// unsupported/forbidden/missing이 live GET으로 우회되면 metadata 경계가 무의미해집니다.
func TestDetailRequiresReadyCatalogEntry(t *testing.T) {
	cases := map[string]struct {
		setup func(*metadatafake.FakeMetadataClient)
		want  error
		state resourcecatalog.State
	}{
		"unsupported(406)": {
			setup: func(meta *metadatafake.FakeMetadataClient) {
				meta.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewGenericServerResponse(
						http.StatusNotAcceptable, "get", secretGVR.GroupResource(), "", "not acceptable", 0, false)
				})
			},
			want: resourcecatalog.ErrUnsupported, state: resourcecatalog.StateUnsupported,
		},
		"forbidden": {
			setup: func(meta *metadatafake.FakeMetadataClient) {
				meta.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(secretGVR.GroupResource(), "", errors.New("denied"))
				})
			},
			want: resourcecatalog.ErrForbidden, state: resourcecatalog.StateForbidden,
		},
	}
	for name, tc := range cases {
		h := start(t, options{
			allowlist:   []schema.GroupVersionResource{secretGVR},
			metaObjects: []runtime.Object{metaObject("v1", "Secret", "payments", "db", "uid-secret-db", nil)},
			dynObjects:  []runtime.Object{secretObject("payments", "db", "uid-secret-db", nil)},
			metaSetup:   tc.setup,
			tune:        func(c *resourcecatalog.Config) { c.SyncTimeout = 2 * time.Second },
		})
		waitForState(t, h.svc, secretGVR, tc.state)
		if _, err := h.svc.Get(context.Background(), resourcecatalog.DetailRequest{
			Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
			Namespace: "payments", Name: "db", ExpectedUID: "uid-secret-db",
		}); !errors.Is(err, tc.want) {
			t.Fatalf("%s 상세가 %v를 돌려줬습니다 (want %v)", name, err, tc.want)
		}
		if got := h.liveGets(); got != 0 {
			t.Fatalf("%s 상태에서 live GET이 %d회 나갔습니다", name, got)
		}
	}
}

// TestDetailIsUnavailableForResourcesNotServed — discovery에 없는 GVR은 상세도 없습니다.
func TestDetailIsUnavailableForResourcesNotServed(t *testing.T) {
	h := start(t, options{
		allowlist:  []schema.GroupVersionResource{widgetGVR},
		dynObjects: []runtime.Object{},
	})
	waitForState(t, h.svc, widgetGVR, resourcecatalog.StateMissing)
	if _, err := h.svc.Get(context.Background(), resourcecatalog.DetailRequest{
		Group: widgetGVR.Group, Version: widgetGVR.Version, Resource: widgetGVR.Resource,
		Namespace: "payments", Name: "db", ExpectedUID: "uid-widget",
	}); !errors.Is(err, resourcecatalog.ErrNotServed) {
		t.Fatalf("미설치 CRD 상세가 %v를 돌려줬습니다 (want ErrNotServed)", err)
	}
	if got := h.liveGets(); got != 0 {
		t.Fatalf("미설치 CRD에 live GET이 %d회 나갔습니다", got)
	}
}

func TestDetailRejectsUnknownAndMalformedTargets(t *testing.T) {
	h := detailHarness(t, options{})
	base := resourcecatalog.DetailRequest{
		Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
		Namespace: "payments", Name: "db", ExpectedUID: "uid-secret-db",
	}
	cases := map[string]func(*resourcecatalog.DetailRequest){
		"uid 없음":        func(r *resourcecatalog.DetailRequest) { r.ExpectedUID = "" },
		"이름 없음":         func(r *resourcecatalog.DetailRequest) { r.Name = "" },
		"이상한 이름":        func(r *resourcecatalog.DetailRequest) { r.Name = "../../etc/passwd" },
		"namespace 없음":  func(r *resourcecatalog.DetailRequest) { r.Namespace = "" },
		"이상한 namespace": func(r *resourcecatalog.DetailRequest) { r.Namespace = "a/b" },
	}
	for name, mutate := range cases {
		req := base
		mutate(&req)
		if _, err := h.svc.Get(context.Background(), req); !errors.Is(err, resourcecatalog.ErrInvalidFilter) {
			t.Fatalf("%s가 %v를 돌려줬습니다 (want ErrInvalidFilter)", name, err)
		}
	}
	req := base
	req.Resource = "configmaps"
	if _, err := h.svc.Get(context.Background(), req); !errors.Is(err, resourcecatalog.ErrNotAllowlisted) {
		t.Fatalf("allowlist 밖 상세가 %v를 돌려줬습니다", err)
	}
}

func TestDetailStopsOnCanceledContextBeforeSpendingBudget(t *testing.T) {
	h := detailHarness(t, options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.svc.Get(ctx, resourcecatalog.DetailRequest{
		Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
		Namespace: "payments", Name: "db", ExpectedUID: "uid-secret-db",
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("취소된 요청이 %v를 돌려줬습니다 (want context.Canceled)", err)
	}
	if got := len(h.dyn.Actions()); got != 0 {
		t.Fatalf("취소된 요청이 Kubernetes를 %d회 호출했습니다", got)
	}
}

func TestDetailRateLimitIsIsolatedFromQueryPath(t *testing.T) {
	h := detailHarness(t, options{tune: func(c *resourcecatalog.Config) {
		c.DetailRate, c.DetailBurst, c.DetailConcurrent = 0.0001, 1, 4
	}})
	req := resourcecatalog.DetailRequest{
		Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
		Namespace: "payments", Name: "db", ExpectedUID: "uid-secret-db",
	}
	if _, err := h.svc.Get(context.Background(), req); err != nil {
		t.Fatalf("첫 상세 조회가 실패했습니다: %v", err)
	}
	if _, err := h.svc.Get(context.Background(), req); !errors.Is(err, resourcecatalog.ErrRateLimited) {
		t.Fatalf("rate 상한을 넘긴 요청이 %v를 돌려줬습니다 (want ErrRateLimited)", err)
	}
}

func TestDetailConcurrencyLimitRejectsInsteadOfQueueing(t *testing.T) {
	block := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(block) }) }
	t.Cleanup(release)

	h := detailHarness(t, options{
		dynSetup: func(dyn *dynamicfake.FakeDynamicClient) {
			dyn.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				<-block
				return false, nil, nil // 기본 reactor가 실제 객체를 돌려줍니다.
			})
		},
		tune: func(c *resourcecatalog.Config) {
			c.DetailRate, c.DetailBurst, c.DetailConcurrent = 100, 10, 1
		},
	})
	req := resourcecatalog.DetailRequest{
		Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
		Namespace: "payments", Name: "db", ExpectedUID: "uid-secret-db",
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := h.svc.Get(context.Background(), req)
		done <- err
	}()
	<-started
	time.Sleep(50 * time.Millisecond) // 첫 요청이 슬롯을 잡을 시간을 줍니다.

	if _, err := h.svc.Get(context.Background(), req); !errors.Is(err, resourcecatalog.ErrRateLimited) {
		t.Fatalf("동시성 상한을 넘긴 요청이 %v를 돌려줬습니다 (want ErrRateLimited)", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("첫 요청이 실패했습니다: %v", err)
	}
}

func TestDetailRejectsOversizedObject(t *testing.T) {
	big := strings.Repeat("x", 40_000)
	h := detailHarness(t, options{
		dynObjects: []runtime.Object{secretObject("payments", "db", "uid-secret-db", map[string]any{
			"stringDataNote": big,
		})},
		tune: func(c *resourcecatalog.Config) { c.MaxObjectBytes = 8192 },
	})
	if _, err := h.svc.Get(context.Background(), resourcecatalog.DetailRequest{
		Group: secretGVR.Group, Version: secretGVR.Version, Resource: secretGVR.Resource,
		Namespace: "payments", Name: "db", ExpectedUID: "uid-secret-db",
	}); !errors.Is(err, resourcecatalog.ErrTooLarge) {
		t.Fatalf("한도를 넘긴 객체가 %v를 돌려줬습니다 (want ErrTooLarge)", err)
	}
}

// TestDetailClientEnforcesTimeoutAndBodyLimit — 상세 전용 클라이언트가 실제 HTTP에서
// timeout과 본문 상한을 거는지 확인합니다. 조회 경로와 공유하지 않는 상한입니다.
func TestDetailClientEnforcesTimeoutAndBodyLimit(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"}}`))
	}))
	t.Cleanup(slow.Close)
	clients, err := resourcecatalog.NewClients(&rest.Config{Host: slow.URL}, resourcecatalog.ClientOptions{
		DetailTimeout: 60 * time.Millisecond, MaxObjectBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients.Dynamic.Resource(secretGVR).Namespace("payments").
		Get(context.Background(), "db", metav1.GetOptions{}); err == nil {
		t.Fatal("timeout이 걸리지 않았습니다")
	}

	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Secret","spec":"` + strings.Repeat("x", 200_000) + `"}`))
	}))
	t.Cleanup(huge.Close)
	limited, err := resourcecatalog.NewClients(&rest.Config{Host: huge.URL}, resourcecatalog.ClientOptions{
		DetailTimeout: 5 * time.Second, MaxObjectBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Dynamic.Resource(secretGVR).Namespace("payments").
		Get(context.Background(), "db", metav1.GetOptions{}); err == nil {
		t.Fatal("본문 상한이 걸리지 않았습니다")
	}
}

// TestMetadataClientNeverOffersFullObjectFallback — 실제 metadata 클라이언트가 보낸
// LIST 요청을 서버에서 받아 확인합니다. Accept에 남아야 하는 것은 PartialObjectMetadata
// 두 종류뿐이고, metadata 협상을 모르는 API가 전체 객체로 내려올 수 있는 평범한
// application/json 대안은 없어야 합니다. (ADR 0018 결정 4)
func TestMetadataClientNeverOffersFullObjectFallback(t *testing.T) {
	accepts := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case accepts <- r.Header.Get("Accept"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{},"items":[]}`))
	}))
	t.Cleanup(server.Close)

	clients, err := resourcecatalog.NewClients(&rest.Config{Host: server.URL}, resourcecatalog.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients.Metadata.Resource(secretGVR).Namespace("payments").
		List(context.Background(), metav1.ListOptions{}); err != nil {
		t.Logf("목록 디코딩 결과는 이 테스트의 관심사가 아닙니다: %v", err)
	}

	var accept string
	select {
	case accept = <-accepts:
	default:
		t.Fatal("서버가 metadata LIST 요청을 받지 못했습니다")
	}

	parts := strings.Split(accept, ",")
	if len(parts) != 2 {
		t.Fatalf("Accept 미디어 타입이 2개가 아닙니다: %q", accept)
	}
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "application/json" || trimmed == "application/vnd.kubernetes.protobuf" {
			t.Fatalf("full-object로 떨어질 수 있는 대안이 남았습니다: %q", accept)
		}
		if !strings.Contains(trimmed, "as=PartialObjectMetadata") {
			t.Fatalf("metadata 미디어 타입이 아닙니다: %q", trimmed)
		}
	}
	if !strings.Contains(accept, "application/vnd.kubernetes.protobuf;as=PartialObjectMetadata") {
		t.Fatalf("PartialObjectMetadata protobuf 협상이 사라졌습니다: %q", accept)
	}
	if !strings.Contains(accept, "application/json;as=PartialObjectMetadata") {
		t.Fatalf("PartialObjectMetadata JSON 협상이 사라졌습니다: %q", accept)
	}
}

// TestDiscoveryClientAcceptIsUntouched — 잘라내기는 metadata 클라이언트에만 걸립니다.
func TestDiscoveryClientAcceptIsUntouched(t *testing.T) {
	accepts := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case accepts <- r.Header.Get("Accept"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`))
	}))
	t.Cleanup(server.Close)

	clients, err := resourcecatalog.NewClients(&rest.Config{Host: server.URL}, resourcecatalog.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients.Discovery.ServerGroups(); err != nil {
		t.Logf("discovery 응답 디코딩은 이 테스트의 관심사가 아닙니다: %v", err)
	}

	var accept string
	select {
	case accept = <-accepts:
	default:
		t.Fatal("서버가 discovery 요청을 받지 못했습니다")
	}
	if strings.Contains(accept, "as=PartialObjectMetadata") {
		t.Fatalf("discovery가 metadata 협상을 쓰고 있습니다: %q", accept)
	}
	if accept != "" && !strings.Contains(accept, "application/json") {
		t.Fatalf("discovery Accept가 변형되었습니다: %q", accept)
	}
}

/* ── allowlist ────────────────────────────────────────────────────────────── */

// TestGVRSegmentValidationFollowsKubernetesRules — 세그먼트 검증은 직접 만든 정규식이
// 아니라 apimachinery 규칙(group=DNS1123 subdomain, version·resource=DNS1035 label)을
// 씁니다. 클러스터가 실제로 제공할 수 있는 CRD는 통과해야 하고, 경로에 위험한 문자가
// 들어간 입력은 여전히 막혀야 합니다.
func TestGVRSegmentValidationFollowsKubernetesRules(t *testing.T) {
	valid := []string{
		"core/v1/services",
		"v1/pods",
		"apps/v1/deployments",
		"flowcontrol.apiserver.k8s.io/v1/flowschemas",
		"example.com/v1alpha1/widgets",
		// CRD 복수형은 하이픈을 포함할 수 있습니다 — 여기가 좁으면 정상 CRD가 막힙니다.
		"example.com/v1/my-widgets",
		"x-k8s.io/v2/foo-bars",
		// 버전 이름도 DNS1035 label이면 됩니다(vN·alpha·beta로 제한되지 않습니다).
		"example.com/stable-v1/widgets",
		"example.com/v1beta1/backup-schedules",
		"core/v1/" + strings.Repeat("s", 63),
	}
	for _, raw := range valid {
		if _, err := resourcecatalog.ParseGVR(raw); err != nil {
			t.Errorf("정상 GVR이 거절됐습니다 %q: %v", raw, err)
		}
	}

	invalid := map[string]string{
		"대문자 resource":     "example.com/v1/My-Widgets",
		"대문자 version":      "example.com/V1/widgets",
		"대문자 group":        "Example.com/v1/widgets",
		"resource 선행 하이픈":  "example.com/v1/-widgets",
		"resource 후행 하이픈":  "example.com/v1/widgets-",
		"resource 숫자 시작":   "example.com/v1/1widgets",
		"resource 점":       "example.com/v1/wid.gets",
		"resource 슬래시":     "example.com/v1/widgets/status",
		"resource 공백":      "example.com/v1/wid gets",
		"resource 길이 초과":   "core/v1/" + strings.Repeat("s", 64),
		"group 빈 label":    "example..com/v1/widgets",
		"group 경로 탈출":      "../../v1/widgets",
		"version 비어 있음":    "example.com//widgets",
		"resource 비어 있음":   "example.com/v1/",
		"세그먼트 부족":          "widgets",
		"세그먼트 과다":          "a.io/v1/widgets/extra",
		"빈 문자열":            "",
		"resource 퍼센트 인코딩": "example.com/v1/%2e%2e",
	}
	for name, raw := range invalid {
		if _, err := resourcecatalog.ParseGVR(raw); err == nil {
			t.Errorf("%s가 통과했습니다: %q", name, raw)
		}
	}

	// 직접 호출 경계 — core group만 빈 문자열이 허용됩니다.
	if err := resourcecatalog.ValidateGVRSegments("", "v1", "pods"); err != nil {
		t.Errorf("core group이 거절됐습니다: %v", err)
	}
	for _, tc := range []struct{ group, version, resource string }{
		{"", "", "pods"},
		{"", "v1", ""},
		{"bad_group", "v1", "pods"},
		{"", "v1", "pods/log"},
	} {
		if err := resourcecatalog.ValidateGVRSegments(tc.group, tc.version, tc.resource); err == nil {
			t.Errorf("잘못된 세그먼트가 통과했습니다: %+v", tc)
		}
	}
}

func TestAllowlistParsingAndCRDOptIn(t *testing.T) {
	if _, err := resourcecatalog.ParseGVR("core/v1/services"); err != nil {
		t.Fatalf("core alias 해석 실패: %v", err)
	}
	if gvr, err := resourcecatalog.ParseGVR("v1/services"); err != nil || gvr.Group != "" {
		t.Fatalf("group 생략 해석 실패: %+v %v", gvr, err)
	}
	for _, bad := range []string{"", "services", "core/V1/services", "core/v1/Services", "core/v1/pods/log", "a/b/c/d", "core/v1/" + strings.Repeat("s", 64)} {
		if _, err := resourcecatalog.ParseGVR(bad); err == nil {
			t.Fatalf("잘못된 GVR이 통과했습니다: %q", bad)
		}
	}
	if _, err := resourcecatalog.NormalizeAllowlist([]schema.GroupVersionResource{widgetGVR}, false); err == nil {
		t.Fatal("CRD가 opt-in 없이 통과했습니다")
	}
	if _, err := resourcecatalog.NormalizeAllowlist([]schema.GroupVersionResource{widgetGVR}, true); err != nil {
		t.Fatalf("opt-in한 CRD가 거절됐습니다: %v", err)
	}
	if _, err := resourcecatalog.NormalizeAllowlist(nil, true); err == nil {
		t.Fatal("빈 allowlist가 통과했습니다")
	}
	tooMany := make([]schema.GroupVersionResource, 0, resourcecatalog.MaxAllowlistEntries+1)
	for i := 0; i <= resourcecatalog.MaxAllowlistEntries; i++ {
		tooMany = append(tooMany, schema.GroupVersionResource{Version: "v1", Resource: fmt.Sprintf("resource%03d", i)})
	}
	if _, err := resourcecatalog.NormalizeAllowlist(tooMany, true); err == nil {
		t.Fatalf("allowlist가 %d개를 넘었는데 통과했습니다", resourcecatalog.MaxAllowlistEntries)
	}
	// 기본 목록은 CRD를 포함하지 않고 그대로 통과해야 합니다.
	if _, err := resourcecatalog.NormalizeAllowlist(resourcecatalog.DefaultAllowlist(), false); err != nil {
		t.Fatalf("기본 allowlist가 CRD opt-in 없이 실패했습니다: %v", err)
	}
}
