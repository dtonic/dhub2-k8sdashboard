package httpapi_test

// 변경 검토 엔드포인트의 경계 검증입니다. (ADR 0019 Phase 1)
//
// 이 계층이 책임지는 것은 네 가지입니다.
//   - 권한·Scope·본문이 틀린 요청은 **클러스터로 나가지 않습니다.**
//   - 본문 상한은 디코드 **전에** 걸리고, JSON 값은 정확히 하나여야 합니다.
//   - 응답의 신원은 서비스가 돌려준 값이고, 필수 배열은 null이 아닙니다.
//   - 매니페스트 원문과 Kubernetes Status 원문은 응답에도 감사 로그에도 없습니다.
//
// dry-run 클라이언트는 U2와 같은 방식으로 dynamic.Interface를 직접 구현합니다.
// fake 라이브러리가 server-side apply를 어떻게 다루는지에 결과가 좌우되면 안 되고,
// 허용하지 않는 동사는 호출되는 순간 드러나야 하기 때문입니다.

import (
	"bytes"
	"context"
	"encoding/json"
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
	"k8s.io/apimachinery/pkg/watch"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

const (
	httpDryRunUID      = "uid-payments-a"
	httpDryRunRV       = "77"
	httpDryRunName     = "api-a"
	httpDryRunNS       = "payments"
	httpRawManifestTag = "RAW_MANIFEST_MARKER"
	httpRawUpstreamTag = "RAW_UPSTREAM_MARKER"
)

/* ── 기록형 dry-run 클라이언트 ───────────────────────────────────────────── */

type httpDryRunClient struct {
	getObj   *unstructured.Unstructured
	patchObj *unstructured.Unstructured
	patchErr error

	mu    sync.Mutex
	verbs []string
}

func (c *httpDryRunClient) note(verb string) {
	c.mu.Lock()
	c.verbs = append(c.verbs, verb)
	c.mu.Unlock()
}

func (c *httpDryRunClient) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.verbs...)
}

func (c *httpDryRunClient) Resource(schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &httpDryRunResource{parent: c}
}

type httpDryRunResource struct {
	parent    *httpDryRunClient
	namespace string
}

func (r *httpDryRunResource) Namespace(ns string) dynamic.ResourceInterface {
	return &httpDryRunResource{parent: r.parent, namespace: ns}
}

func (r *httpDryRunResource) Get(ctx context.Context, _ string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	r.parent.note("get")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.parent.getObj, nil
}

func (r *httpDryRunResource) Patch(ctx context.Context, _ string, _ types.PatchType, _ []byte, _ metav1.PatchOptions, _ ...string) (*unstructured.Unstructured, error) {
	r.parent.note("patch")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.parent.patchErr != nil {
		return nil, r.parent.patchErr
	}
	return r.parent.patchObj, nil
}

// 아래는 전부 호출되면 안 되는 동사입니다. 기록되므로 테스트가 즉시 드러냅니다.
func (r *httpDryRunResource) Create(context.Context, *unstructured.Unstructured, metav1.CreateOptions, ...string) (*unstructured.Unstructured, error) {
	r.parent.note("create")
	return nil, apierrors.NewBadRequest("create는 허용되지 않습니다")
}
func (r *httpDryRunResource) Update(context.Context, *unstructured.Unstructured, metav1.UpdateOptions, ...string) (*unstructured.Unstructured, error) {
	r.parent.note("update")
	return nil, apierrors.NewBadRequest("update는 허용되지 않습니다")
}
func (r *httpDryRunResource) UpdateStatus(context.Context, *unstructured.Unstructured, metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	r.parent.note("updateStatus")
	return nil, apierrors.NewBadRequest("updateStatus는 허용되지 않습니다")
}
func (r *httpDryRunResource) Delete(context.Context, string, metav1.DeleteOptions, ...string) error {
	r.parent.note("delete")
	return apierrors.NewBadRequest("delete는 허용되지 않습니다")
}
func (r *httpDryRunResource) DeleteCollection(context.Context, metav1.DeleteOptions, metav1.ListOptions) error {
	r.parent.note("deleteCollection")
	return apierrors.NewBadRequest("deleteCollection은 허용되지 않습니다")
}
func (r *httpDryRunResource) List(context.Context, metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	r.parent.note("list")
	return nil, apierrors.NewBadRequest("list는 허용되지 않습니다")
}
func (r *httpDryRunResource) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	r.parent.note("watch")
	return nil, apierrors.NewBadRequest("watch는 허용되지 않습니다")
}
func (r *httpDryRunResource) Apply(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions, ...string) (*unstructured.Unstructured, error) {
	r.parent.note("apply")
	return nil, apierrors.NewBadRequest("apply는 허용되지 않습니다")
}
func (r *httpDryRunResource) ApplyStatus(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	r.parent.note("applyStatus")
	return nil, apierrors.NewBadRequest("applyStatus는 허용되지 않습니다")
}

/* ── 픽스처 ─────────────────────────────────────────────────────────────── */

// httpDryRunDiscovery는 patch verb까지 제공하는 discovery입니다.
// resources_test.go의 목록은 조회 전용이라 검토 경로가 열리지 않습니다.
func httpDryRunDiscovery() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "services", Namespaced: true, Kind: "Service", Verbs: []string{"get", "list", "watch", "patch"}},
			{Name: "secrets", Namespaced: true, Kind: "Secret", Verbs: []string{"get", "list", "watch", "patch"}},
		}},
		{GroupVersion: "storage.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "storageclasses", Namespaced: false, Kind: "StorageClass", Verbs: []string{"get", "list", "watch", "patch"}},
		}},
	}
}

func httpDryRunLiveService() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name": httpDryRunName, "namespace": httpDryRunNS,
			"uid": httpDryRunUID, "resourceVersion": httpDryRunRV,
		},
		"spec": map[string]any{"type": "NodePort"},
	}}
}

func httpDryRunPatchedService() *unstructured.Unstructured {
	obj := httpDryRunLiveService()
	obj.Object["spec"] = map[string]any{"type": "ClusterIP"}
	return obj
}

// httpDryRunLog는 informer 배경 고루틴과 요청 고루틴이 **같은 버퍼**에 쓰기 때문에
// 필요합니다. bytes.Buffer는 goroutine-safe가 아니라 -race에서 그대로 터집니다.
type httpDryRunLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *httpDryRunLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *httpDryRunLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type httpDryRunFixture struct {
	fixture
	client *httpDryRunClient
	logs   *httpDryRunLog
}

// newHTTPDryRunFixture는 검토가 열린 배포를 세웁니다.
// enabled=false면 서비스는 살아 있고 검토만 없습니다(부분 롤백 경계).
func newHTTPDryRunFixture(t *testing.T, sc scope.Scope, enabled bool, client *httpDryRunClient) httpDryRunFixture {
	t.Helper()
	if client == nil {
		client = &httpDryRunClient{getObj: httpDryRunLiveService(), patchObj: httpDryRunPatchedService()}
	}
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	meta := metadatafake.NewSimpleMetadataClient(scheme,
		partialMeta("v1", "Service", httpDryRunNS, httpDryRunName, httpDryRunUID),
		partialMeta("storage.k8s.io/v1", "StorageClass", "", "fast", "uid-sc-fast"),
	)
	disc := &discoveryfake.FakeDiscovery{Fake: &k8stesting.Fake{Resources: httpDryRunDiscovery()}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			svcGVR:     "ServiceList",
			secGVR:     "SecretList",
			storageGVR: "StorageClassList",
		})

	logs := &httpDryRunLog{}
	service, err := resourcecatalog.New(
		resourcecatalog.Clients{Metadata: meta, Discovery: disc, Dynamic: dyn, DryRun: client},
		resourcecatalog.Config{
			ClusterID:       testcluster.ClusterID,
			Allowlist:       []schema.GroupVersionResource{svcGVR, secGVR, storageGVR},
			RefreshInterval: time.Hour,
			Resync:          time.Hour,
			IndexInterval:   20 * time.Millisecond,
			SyncTimeout:     5 * time.Second,
			DetailRate:      100, DetailBurst: 50, DetailConcurrent: 4,
			DryRunEnabled:    enabled,
			DryRunAllowlist:  []schema.GroupVersionResource{svcGVR, storageGVR},
			DryRunRate:       100,
			DryRunBurst:      50,
			DryRunConcurrent: 4,
			Logger:           slog.New(slog.NewTextHandler(logs, nil)),
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
	waitForReady(t, service, svcGVR)
	waitForReady(t, service, storageGVR)

	f := newFixture(t, func(d *httpapi.Deps) {
		d.Resolver = scope.Static{S: sc}
		d.Resources = service
		d.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	return httpDryRunFixture{fixture: f, client: client, logs: logs}
}

func httpDryRunPath(group, version, resource string) string {
	return resourcePath("/" + group + "/" + version + "/" + resource + "/object/dry-run")
}

func httpServicePath() string { return httpDryRunPath("core", "v1", "services") }

// postDryRun은 본문을 그대로 보냅니다 — 잘못된 JSON도 보낼 수 있어야 합니다.
func (f fixture) postDryRun(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.postDryRunWithType(t, path, "application/json", body)
}

// postDryRunWithType은 Content-Type을 직접 정합니다.
// 빈 문자열이면 헤더를 아예 붙이지 않습니다(누락 경계).
func (f fixture) postDryRunWithType(t *testing.T, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func httpDryRunBody(overrides map[string]any) string {
	body := map[string]any{
		"apiVersion":      "v1",
		"kind":            "Service",
		"namespace":       httpDryRunNS,
		"name":            httpDryRunName,
		"uid":             httpDryRunUID,
		"resourceVersion": httpDryRunRV,
		"manifest": "apiVersion: v1\nkind: Service\nmetadata:\n  name: " + httpDryRunName +
			"\n  namespace: " + httpDryRunNS + "\nspec:\n  type: ClusterIP\n",
	}
	for k, v := range overrides {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func httpDryRunErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var apiErr contract.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("오류 응답이 APIError가 아닙니다: %v\n%s", err, rec.Body.String())
	}
	return apiErr.Code
}

/* ── 플랫폼 gate ─────────────────────────────────────────────────────────── */

// TestDryRunRouteReusesExplorerPlatformGate — 권한 근거는 Resource Explorer와
// 같습니다. 권한이 없으면 배포 형태와 무관하게 403이고, 권한이 있는 요청만 503을
// 봅니다. 어느 경우에도 클러스터로 나가지 않습니다.
func TestDryRunRouteReusesExplorerPlatformGate(t *testing.T) {
	t.Run("다른 클러스터", func(t *testing.T) {
		f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
		rec := f.postDryRun(t, "/api/v1/clusters/prod-frankfurt/resources/core/v1/services/object/dry-run",
			httpDryRunBody(nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d want 403", rec.Code)
		}
		if got := httpDryRunErrorCode(t, rec); got != "cluster_access_denied" {
			t.Errorf("code=%q", got)
		}
		if calls := f.client.calls(); len(calls) != 0 {
			t.Fatalf("클러스터로 나갔습니다: %v", calls)
		}
	})

	t.Run("capability 없음", func(t *testing.T) {
		sc := explorerScope(true)
		sc.CanExploreResources = false
		f := newHTTPDryRunFixture(t, sc, true, nil)
		rec := f.postDryRun(t, httpServicePath(), httpDryRunBody(nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d want 403", rec.Code)
		}
		if got := httpDryRunErrorCode(t, rec); got != "forbidden" {
			t.Errorf("code=%q", got)
		}
		if calls := f.client.calls(); len(calls) != 0 {
			t.Fatalf("클러스터로 나갔습니다: %v", calls)
		}
	})

	t.Run("central은 서비스가 없습니다", func(t *testing.T) {
		f := newFixture(t, func(d *httpapi.Deps) {
			d.Resolver = scope.Static{S: explorerScope(true)}
			d.Resources = nil
		})
		rec := f.postDryRun(t, httpServicePath(), httpDryRunBody(nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d want 503", rec.Code)
		}
		if got := httpDryRunErrorCode(t, rec); got != "resources_unavailable" {
			t.Errorf("code=%q", got)
		}
	})

	t.Run("검토만 꺼진 배포", func(t *testing.T) {
		f := newHTTPDryRunFixture(t, explorerScope(true), false, nil)
		rec := f.postDryRun(t, httpServicePath(), httpDryRunBody(nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d want 503", rec.Code)
		}
		if got := httpDryRunErrorCode(t, rec); got != "dryrun_unavailable" {
			t.Errorf("code=%q", got)
		}
		if calls := f.client.calls(); len(calls) != 0 {
			t.Fatalf("클러스터로 나갔습니다: %v", calls)
		}
	})

	t.Run("검토 대상이 아닌 GVR", func(t *testing.T) {
		f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
		rec := f.postDryRun(t, httpDryRunPath("core", "v1", "secrets"),
			httpDryRunBody(map[string]any{"kind": "Secret"}))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d want 403", rec.Code)
		}
		if got := httpDryRunErrorCode(t, rec); got != "dryrun_resource_denied" {
			t.Errorf("code=%q", got)
		}
		if calls := f.client.calls(); len(calls) != 0 {
			t.Fatalf("클러스터로 나갔습니다: %v", calls)
		}
	})
}

/* ── Scope ──────────────────────────────────────────────────────────────── */

// TestDryRunRouteEnforcesScopeFromBody — 본문의 namespace로 Scope를 넓힐 수 없습니다.
func TestDryRunRouteEnforcesScopeFromBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scope  scope.Scope
		path   string
		body   string
		status int
		code   string
	}{
		{
			name: "namespace 누락", scope: explorerScope(false, httpDryRunNS),
			path: httpServicePath(), body: httpDryRunBody(map[string]any{"namespace": nil}),
			status: http.StatusBadRequest, code: "invalid_filter",
		},
		{
			name: "Scope 밖 namespace", scope: explorerScope(false, httpDryRunNS),
			path: httpServicePath(), body: httpDryRunBody(map[string]any{"namespace": "media"}),
			status: http.StatusForbidden, code: "namespace_access_denied",
		},
		{
			name: "cluster 범위에 namespace 지정", scope: explorerScope(true),
			path: httpDryRunPath("storage.k8s.io", "v1", "storageclasses"),
			body: httpDryRunBody(map[string]any{
				"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
				"name": "fast", "uid": "uid-sc-fast", "namespace": httpDryRunNS,
			}),
			status: http.StatusBadRequest, code: "invalid_filter",
		},
		{
			name: "cluster 범위인데 부분 Scope", scope: explorerScope(false, httpDryRunNS),
			path: httpDryRunPath("storage.k8s.io", "v1", "storageclasses"),
			body: httpDryRunBody(map[string]any{
				"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
				"name": "fast", "uid": "uid-sc-fast", "namespace": nil,
			}),
			status: http.StatusForbidden, code: "cluster_scope_required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHTTPDryRunFixture(t, tc.scope, true, nil)
			rec := f.postDryRun(t, tc.path, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want %d\n%s", rec.Code, tc.status, rec.Body.String())
			}
			if got := httpDryRunErrorCode(t, rec); got != tc.code {
				t.Errorf("code=%q want %q", got, tc.code)
			}
			if calls := f.client.calls(); len(calls) != 0 {
				t.Fatalf("Scope 실패인데 클러스터로 나갔습니다: %v", calls)
			}
		})
	}
}

/* ── 본문 ───────────────────────────────────────────────────────────────── */

// TestDryRunRouteRequiresJSONContentType — 계약이 선언한 것은 application/json
// 하나뿐입니다.
//
// 검사하지 않으면 text/plain이나 form 인코딩으로도 같은 본문이 통과하고, 그 순간
// 이 POST는 브라우저의 **simple cross-origin 요청** 표면에 들어갑니다 — preflight
// 없이 도달할 수 있고, cookie 세션 배포에서는 그것만으로 실제 Kubernetes dry-run
// 호출이 일어납니다.
func TestDryRunRouteRequiresJSONContentType(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
	}{
		{"누락", ""},
		{"text/plain", "text/plain"},
		// simple cross-origin POST가 쓸 수 있는 나머지 두 가지입니다.
		{"form urlencoded", "application/x-www-form-urlencoded"},
		{"multipart", "multipart/form-data"},
		// 접두사만 같은 값도 통과하면 안 됩니다.
		{"json 접두사만", "application/jsonx"},
		{"파싱 불가", "@@@"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
			rec := f.postDryRunWithType(t, httpServicePath(), tc.contentType, httpDryRunBody(nil))

			// 상태가 무엇이든 **먼저** 확인합니다.
			if calls := f.client.calls(); len(calls) != 0 {
				t.Fatalf("Content-Type이 틀렸는데 클러스터로 나갔습니다: %v", calls)
			}
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status=%d want 415\n%s", rec.Code, rec.Body.String())
			}
			if got := httpDryRunErrorCode(t, rec); got != "unsupported_media_type" {
				t.Errorf("code=%q want unsupported_media_type", got)
			}
			// 헤더 원문도 본문 원문도 오류에 실리지 않습니다.
			if tc.contentType != "" && strings.Contains(rec.Body.String(), tc.contentType) {
				t.Errorf("오류에 Content-Type 원문이 실렸습니다: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), httpDryRunUID) {
				t.Errorf("오류에 요청 본문이 실렸습니다: %s", rec.Body.String())
			}
		})
	}

	// charset 파라미터는 흔히 붙고 값도 쓰지 않으므로 허용합니다 —
	// 기존 성공 경계가 그대로 유지되어야 합니다.
	t.Run("charset 파라미터는 허용", func(t *testing.T) {
		f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
		rec := f.postDryRunWithType(t, httpServicePath(), "application/json; charset=utf-8", httpDryRunBody(nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d want 200\n%s", rec.Code, rec.Body.String())
		}
		if calls := f.client.calls(); len(calls) != 2 || calls[0] != "get" || calls[1] != "patch" {
			t.Fatalf("호출이 (get, patch)가 아닙니다: %v", calls)
		}
	})
}

// TestDryRunRouteBoundsAndValidatesBody — 상한은 디코드 전에, JSON 값은 정확히 하나.
func TestDryRunRouteBoundsAndValidatesBody(t *testing.T) {
	valid := httpDryRunBody(nil)
	for _, tc := range []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"빈 본문", "", http.StatusBadRequest, "bad_request"},
		{"JSON이 아님", "not json", http.StatusBadRequest, "bad_request"},
		{"객체가 아님", `["a"]`, http.StatusBadRequest, "bad_request"},
		// 최상위 null은 **오류 없이** 디코드됩니다. 값 타입으로 받으면 모든 필드가
		// zero value인 요청이 되어 본문을 보내지 않은 것과 구분되지 않습니다.
		{"최상위 null", "null", http.StatusBadRequest, "bad_request"},
		{"공백 두른 null", "  null  ", http.StatusBadRequest, "bad_request"},
		{"모르는 필드", httpDryRunBody(map[string]any{"force": true}), http.StatusBadRequest, "bad_request"},
		{"두 번째 JSON 값", valid + valid, http.StatusBadRequest, "bad_request"},
		{"꼬리 쓰레기", valid + "trailing", http.StatusBadRequest, "bad_request"},
		{"꼬리 널바이트", valid + "\x00", http.StatusBadRequest, "bad_request"},
		{
			"봉투 상한 초과",
			httpDryRunBody(map[string]any{
				"manifest": strings.Repeat("a", contract.DefaultDryRunManifestBytes+contract.DryRunEnvelopeSlack+1024),
			}),
			http.StatusRequestEntityTooLarge, "manifest_too_large",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
			rec := f.postDryRun(t, httpServicePath(), tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want %d\n%s", rec.Code, tc.status, rec.Body.String())
			}
			if got := httpDryRunErrorCode(t, rec); got != tc.code {
				t.Errorf("code=%q want %q", got, tc.code)
			}
			if calls := f.client.calls(); len(calls) != 0 {
				t.Fatalf("본문 실패인데 클러스터로 나갔습니다: %v", calls)
			}
		})
	}
}

// TestDryRunRouteRejectsOversizeManifestWithinEnvelope — 봉투 안이지만 매니페스트
// 상한을 넘는 경우는 서비스가 413으로 거절합니다.
func TestDryRunRouteRejectsOversizeManifestWithinEnvelope(t *testing.T) {
	f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
	rec := f.postDryRun(t, httpServicePath(), httpDryRunBody(map[string]any{
		"manifest": strings.Repeat("a", contract.DefaultDryRunManifestBytes+16),
	}))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413\n%s", rec.Code, rec.Body.String())
	}
	if got := httpDryRunErrorCode(t, rec); got != "manifest_too_large" {
		t.Errorf("code=%q", got)
	}
	if calls := f.client.calls(); len(calls) != 0 {
		t.Fatalf("상한 초과인데 클러스터로 나갔습니다: %v", calls)
	}
}

/* ── 오류 매핑 ───────────────────────────────────────────────────────────── */

// TestDryRunRouteErrorStatusMatrix — 신원·매니페스트·upstream 실패의 상태·코드입니다.
func TestDryRunRouteErrorStatusMatrix(t *testing.T) {
	conflict := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403,
		Message: `services "api-a" is forbidden: cannot patch resource`,
	}}
	for _, tc := range []struct {
		name     string
		body     string
		patchErr error
		status   int
		code     string
	}{
		{
			name: "매니페스트 다중 문서",
			body: httpDryRunBody(map[string]any{
				"manifest": "apiVersion: v1\nkind: Service\nmetadata:\n  name: " + httpDryRunName +
					"\n---\napiVersion: v1\nkind: Service\n",
			}),
			status: http.StatusBadRequest, code: "invalid_manifest",
		},
		{
			name:   "본문 kind 불일치",
			body:   httpDryRunBody(map[string]any{"kind": "ConfigMap"}),
			status: http.StatusBadRequest, code: "manifest_mismatch",
		},
		{
			name:   "목록에 없는 이름",
			body:   httpDryRunBody(map[string]any{"name": "ghost", "manifest": "apiVersion: v1\nkind: Service\nmetadata:\n  name: ghost\n  namespace: " + httpDryRunNS + "\n"}),
			status: http.StatusNotFound, code: "not_found",
		},
		{
			name:   "UID 불일치",
			body:   httpDryRunBody(map[string]any{"uid": "uid-other"}),
			status: http.StatusConflict, code: "uid_mismatch",
		},
		{
			name:   "resourceVersion 낡음",
			body:   httpDryRunBody(map[string]any{"resourceVersion": "1"}),
			status: http.StatusConflict, code: "resource_version_mismatch",
		},
		{
			name: "서버 RBAC 부족", body: httpDryRunBody(nil), patchErr: conflict,
			status: http.StatusBadGateway, code: "dryrun_forbidden",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &httpDryRunClient{
				getObj: httpDryRunLiveService(), patchObj: httpDryRunPatchedService(), patchErr: tc.patchErr,
			}
			f := newHTTPDryRunFixture(t, explorerScope(true), true, client)
			rec := f.postDryRun(t, httpServicePath(), tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want %d\n%s", rec.Code, tc.status, rec.Body.String())
			}
			if got := httpDryRunErrorCode(t, rec); got != tc.code {
				t.Errorf("code=%q want %q", got, tc.code)
			}
		})
	}
}

/* ── 성공 응답 ───────────────────────────────────────────────────────────── */

// TestDryRunRouteReturnsSanitizedServerIdentity — 신원은 서비스가 돌려준 값이고
// 필수 배열은 null이 아닙니다.
func TestDryRunRouteReturnsSanitizedServerIdentity(t *testing.T) {
	f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
	rec := f.postDryRun(t, httpServicePath(), httpDryRunBody(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200\n%s", rec.Code, rec.Body.String())
	}
	if calls := f.client.calls(); len(calls) != 2 || calls[0] != "get" || calls[1] != "patch" {
		t.Fatalf("호출이 (get, patch)가 아닙니다: %v", calls)
	}

	var out contract.ResourceDryRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("응답을 해석하지 못했습니다: %v\n%s", err, rec.Body.String())
	}
	if out.ClusterID != testcluster.ClusterID || out.Group != "core" || out.Version != "v1" || out.Resource != "services" {
		t.Errorf("경로 신원이 다릅니다: %+v", out)
	}
	if out.APIVersion != "v1" || out.Kind != "Service" || out.Name != httpDryRunName ||
		out.Namespace != httpDryRunNS || out.UID != httpDryRunUID || out.ResourceVersion != httpDryRunRV {
		t.Errorf("객체 신원이 서비스 값과 다릅니다: %+v", out)
	}
	if out.GeneratedAt == "" {
		t.Error("generatedAt이 비어 있습니다")
	}
	if out.FieldManager != contract.ResourceDryRunFieldManager {
		t.Errorf("fieldManager=%q", out.FieldManager)
	}
	if out.Outcome != contract.DryRunChanged {
		t.Errorf("outcome=%q want changed", out.Outcome)
	}

	// 필수 배열은 JSON에서 null이 아니어야 합니다 — 타입 디코드로는 구분되지 않으므로
	// 원문을 봅니다.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"changes", "warnings", "violations", "redacted"} {
		value, has := raw[field]
		if !has {
			t.Errorf("%s가 응답에 없습니다", field)
			continue
		}
		if string(value) == "null" {
			t.Errorf("%s가 null입니다", field)
		}
	}
	// 응답에 원문 계열 필드가 생기면 안 됩니다.
	for _, forbidden := range []string{"manifest", "yaml", "object", "data", "stringData", "token"} {
		if _, has := raw[forbidden]; has {
			t.Errorf("응답에 %s가 실렸습니다", forbidden)
		}
	}
}

// TestDryRunRouteNeverEchoesManifestOrUpstream — 매니페스트 원문과 Kubernetes
// Status 원문은 응답에도 감사 로그에도 없어야 합니다.
func TestDryRunRouteNeverEchoesManifestOrUpstream(t *testing.T) {
	manifest := "apiVersion: v1\nkind: Service\nmetadata:\n  name: " + httpDryRunName +
		"\n  namespace: " + httpDryRunNS + "\n  annotations:\n    example.com/note: " + httpRawManifestTag +
		"\nspec:\n  type: ClusterIP\n"

	t.Run("성공 경로", func(t *testing.T) {
		f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
		rec := f.postDryRun(t, httpServicePath(), httpDryRunBody(map[string]any{"manifest": manifest}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d\n%s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), httpRawManifestTag) {
			t.Fatalf("응답에 매니페스트 원문이 실렸습니다: %s", rec.Body.String())
		}
		if strings.Contains(f.logs.String(), httpRawManifestTag) {
			t.Fatalf("감사 로그에 매니페스트 원문이 실렸습니다: %s", f.logs.String())
		}
		// 감사는 남되 bounded 값만 남습니다.
		if !strings.Contains(f.logs.String(), "resource-audit") ||
			!strings.Contains(f.logs.String(), "action=dryrun") {
			t.Errorf("검토 감사 기록이 없습니다: %s", f.logs.String())
		}
	})

	t.Run("upstream 실패 경로", func(t *testing.T) {
		client := &httpDryRunClient{
			getObj: httpDryRunLiveService(),
			patchErr: &apierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure, Reason: metav1.StatusReasonInvalid, Code: 422,
				Message: "rejected " + httpRawUpstreamTag,
				Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{
					Type:    metav1.CauseTypeFieldValueInvalid,
					Field:   "spec." + httpRawUpstreamTag,
					Message: httpRawUpstreamTag,
				}}},
			}},
		}
		f := newHTTPDryRunFixture(t, explorerScope(true), true, client)
		rec := f.postDryRun(t, httpServicePath(), httpDryRunBody(map[string]any{"manifest": manifest}))
		// 검증 거절은 200 검토 결과입니다.
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d want 200\n%s", rec.Code, rec.Body.String())
		}
		for _, marker := range []string{httpRawUpstreamTag, httpRawManifestTag} {
			if strings.Contains(rec.Body.String(), marker) {
				t.Fatalf("응답에 %s가 실렸습니다: %s", marker, rec.Body.String())
			}
			if strings.Contains(f.logs.String(), marker) {
				t.Fatalf("로그에 %s가 실렸습니다: %s", marker, f.logs.String())
			}
		}
	})
}

// TestDryRunRouteCancelWritesNothing — 취소된 요청에는 응답을 쓰지 않습니다.
func TestDryRunRouteCancelWritesNothing(t *testing.T) {
	f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, httpServicePath(), strings.NewReader(httpDryRunBody(nil))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Body.Len() != 0 {
		t.Fatalf("취소된 요청에 본문을 썼습니다: %s", rec.Body.String())
	}
	if calls := f.client.calls(); len(calls) != 0 {
		t.Fatalf("취소된 요청이 클러스터로 나갔습니다: %v", calls)
	}
}

/* ── 카탈로그 capability · 라우트 보존 ───────────────────────────────────── */

// TestResourceCatalogCarriesDryRunCapability — capability가 계약으로 흘러야 UI가
// 진입점을 만들지 말지 정할 수 있습니다.
func TestResourceCatalogCarriesDryRunCapability(t *testing.T) {
	check := func(t *testing.T, enabled bool, want map[string]bool) {
		t.Helper()
		f := newHTTPDryRunFixture(t, explorerScope(true), enabled, nil)
		var catalog contract.ResourceCatalogResponse
		rec := f.get(t, resourcePath(""), &catalog)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d\n%s", rec.Code, rec.Body.String())
		}
		seen := map[string]bool{}
		for _, item := range catalog.Items {
			seen[item.Resource] = item.DryRun
		}
		for resource, expect := range want {
			if seen[resource] != expect {
				t.Errorf("%s dryRun=%v want %v", resource, seen[resource], expect)
			}
		}
	}
	t.Run("켜진 배포", func(t *testing.T) {
		// secrets는 opt-in 목록에 없고 hard-deny이기도 합니다.
		check(t, true, map[string]bool{"services": true, "storageclasses": true, "secrets": false})
	})
	t.Run("꺼진 배포", func(t *testing.T) {
		check(t, false, map[string]bool{"services": false, "storageclasses": false, "secrets": false})
	})
}

// httpDryRunAllowsMethod는 Allow 헤더를 쉼표로 끊어 **토큰 하나가 정확히** 그
// 메서드인지 봅니다. 부분 문자열로 보면 "POSTX" 같은 값도 통과합니다.
func httpDryRunAllowsMethod(header, method string) bool {
	for _, token := range strings.Split(header, ",") {
		if strings.TrimSpace(token) == method {
			return true
		}
	}
	return false
}

// TestDryRunRouteRejectsNonPostVerbs — 이 경로에는 POST 하나뿐입니다.
//
// "200이 아니다"로는 부족합니다 — 경로가 통째로 사라져 404가 되어도 통과해 버립니다.
// 정확히 405이고 Allow가 POST를 알려야 라우트가 살아 있으면서 동사만 막혔다는 뜻입니다.
func TestDryRunRouteRejectsNonPostVerbs(t *testing.T) {
	f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(method, httpServicePath(), nil)
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)

		// 상태·본문이 무엇이든 **먼저** 확인합니다. 아래 단언에서 멈추더라도
		// "클러스터로 나가지 않았다"는 사실은 반드시 검사되어야 합니다.
		if calls := f.client.calls(); len(calls) != 0 {
			t.Fatalf("%s가 클러스터로 나갔습니다: %v", method, calls)
		}
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status=%d want 405\n%s", method, rec.Code, rec.Body.String())
			continue
		}
		if allow := rec.Header().Get("Allow"); !httpDryRunAllowsMethod(allow, http.MethodPost) {
			t.Errorf("%s Allow=%q에 POST 토큰이 없습니다", method, allow)
		}
		if got := httpDryRunErrorCode(t, rec); got != "method_not_allowed" {
			t.Errorf("%s code=%q want method_not_allowed", method, got)
		}
	}
}

// TestExistingResourceAndManageRoutesSurvive — 검토를 더하면서 기존 조회·관리
// 라우트가 사라지거나 바뀌면 안 됩니다.
func TestExistingResourceAndManageRoutesSurvive(t *testing.T) {
	f := newHTTPDryRunFixture(t, explorerScope(true), true, nil)
	// 조회 경로는 그대로 200입니다.
	if rec := f.get(t, resourcePath(""), nil); rec.Code != http.StatusOK {
		t.Errorf("카탈로그 status=%d", rec.Code)
	}
	if rec := f.get(t, resourcePath("/core/v1/services"), nil); rec.Code != http.StatusOK {
		t.Errorf("목록 status=%d", rec.Code)
	}
	// 관리 라우트는 라우터에 남아 있어야 합니다 — 권한이 없으면 403이지만
	// 404(경로 없음)여서는 안 됩니다.
	for _, path := range []string{
		"/api/v1/clusters/" + testcluster.ClusterID + "/deployments",
		"/api/v1/clusters/" + testcluster.ClusterID + "/secrets",
	} {
		rec := f.get(t, path, nil)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s 라우트가 사라졌습니다", path)
		}
	}
}
