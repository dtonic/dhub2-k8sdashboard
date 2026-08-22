package resourcecatalog

// 변경 검토 dry-run 코어 테스트 (ADR 0019 Phase 1)
//
// 증명해야 하는 것은 두 종류입니다.
//   1. **나가는 요청이 정확히 무엇인가** — apply patch 하나, dryRun=All, Strict,
//      고정 fieldManager, Force 미설정, subresource 없음. fake 라이브러리의 내부
//      동작에 기대지 않고 dynamic.Interface를 직접 구현해 인자를 그대로 받습니다.
//      create/update/delete/apply는 구현체가 즉시 테스트를 실패시킵니다.
//   2. **언제 나가지 않는가** — preflight에서 걸린 요청은 호출 기록이 0이어야 합니다.
//
// 이 파일의 식별자는 전부 dryRun 접두사를 씁니다 — 같은 패키지의 다른 테스트 파일과
// 이름이 겹치면 안 됩니다.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

/* ── 기록형 dynamic client ────────────────────────────────────────────────
   실제 인터페이스를 그대로 구현합니다. 허용하지 않는 동사는 호출되는 순간
   기록되고 오류가 되므로, "쓰기 동사는 dry-run patch 하나뿐"이 구조로 증명됩니다. */

type dryRunRecordedCall struct {
	verb         string
	namespace    string
	name         string
	patchType    types.PatchType
	body         []byte
	patchOptions metav1.PatchOptions
	subresources []string
}

type dryRunRecordingClient struct {
	getObj   *unstructured.Unstructured
	getErr   error
	patchObj *unstructured.Unstructured
	patchErr error
	// warnHeader가 있으면 patch가 "경고 있었음" 신호를 수집기에 넣습니다.
	// 값 자체는 서버가 절대 읽지 않아야 하므로 여기서도 내용을 전달하지 않습니다.
	warnHeader string
	// onPatch가 있으면 patch 직전에 부릅니다(취소·경합 시나리오용).
	onPatch func()

	// 경고 수집기가 어느 호출에 달렸는지. GET에 달리면 안 됩니다.
	getSawWarningSink   bool
	patchSawWarningSink bool

	mu        sync.Mutex
	calls     []dryRunRecordedCall
	forbidden []string
}

func (r *dryRunRecordingClient) record(c dryRunRecordedCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *dryRunRecordingClient) deny(verb string) error {
	r.mu.Lock()
	r.forbidden = append(r.forbidden, verb)
	r.mu.Unlock()
	return errors.New("이 경로는 " + verb + "를 호출할 수 없습니다")
}

func (r *dryRunRecordingClient) snapshot() ([]dryRunRecordedCall, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]dryRunRecordedCall(nil), r.calls...), append([]string(nil), r.forbidden...)
}

func (r *dryRunRecordingClient) countOf(verb string) int {
	calls, _ := r.snapshot()
	n := 0
	for _, c := range calls {
		if c.verb == verb {
			n++
		}
	}
	return n
}

func (r *dryRunRecordingClient) Resource(schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &dryRunRecordingResource{parent: r}
}

type dryRunRecordingResource struct {
	parent    *dryRunRecordingClient
	namespace string
}

func (r *dryRunRecordingResource) Namespace(ns string) dynamic.ResourceInterface {
	return &dryRunRecordingResource{parent: r.parent, namespace: ns}
}

func (r *dryRunRecordingResource) Get(ctx context.Context, name string, _ metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	r.parent.record(dryRunRecordedCall{verb: "get", namespace: r.namespace, name: name, subresources: subresources})
	if warningSinkFrom(ctx) != nil {
		r.parent.getSawWarningSink = true
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.parent.getErr != nil {
		return nil, r.parent.getErr
	}
	return r.parent.getObj, nil
}

func (r *dryRunRecordingResource) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if r.parent.onPatch != nil {
		r.parent.onPatch()
	}
	r.parent.record(dryRunRecordedCall{
		verb: "patch", namespace: r.namespace, name: name, patchType: pt,
		body: append([]byte(nil), data...), patchOptions: options, subresources: subresources,
	})
	if sink := warningSinkFrom(ctx); sink != nil {
		r.parent.patchSawWarningSink = true
		// 실제 transport와 같은 계약입니다 — **존재만** 알립니다.
		sink.add(r.parent.warnHeader != "")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.parent.patchErr != nil {
		return nil, r.parent.patchErr
	}
	return r.parent.patchObj, nil
}

// 아래는 전부 **호출되면 안 되는** 동사입니다. 하나라도 불리면 테스트가 실패합니다.
func (r *dryRunRecordingResource) Create(context.Context, *unstructured.Unstructured, metav1.CreateOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, r.parent.deny("create")
}
func (r *dryRunRecordingResource) Update(context.Context, *unstructured.Unstructured, metav1.UpdateOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, r.parent.deny("update")
}
func (r *dryRunRecordingResource) UpdateStatus(context.Context, *unstructured.Unstructured, metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	return nil, r.parent.deny("updateStatus")
}
func (r *dryRunRecordingResource) Delete(context.Context, string, metav1.DeleteOptions, ...string) error {
	return r.parent.deny("delete")
}
func (r *dryRunRecordingResource) DeleteCollection(context.Context, metav1.DeleteOptions, metav1.ListOptions) error {
	return r.parent.deny("deleteCollection")
}
func (r *dryRunRecordingResource) List(context.Context, metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return nil, r.parent.deny("list")
}
func (r *dryRunRecordingResource) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return nil, r.parent.deny("watch")
}
func (r *dryRunRecordingResource) Apply(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, r.parent.deny("apply")
}
func (r *dryRunRecordingResource) ApplyStatus(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	return nil, r.parent.deny("applyStatus")
}

/* ── fixture ─────────────────────────────────────────────────────────────── */

var dryRunTestGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

const (
	dryRunTestNS   = "payments"
	dryRunTestName = "api-config"
	dryRunTestUID  = "11111111-2222-3333-4444-555555555555"
	dryRunTestRV   = "4242"
)

type dryRunFixtureOptions struct {
	enabled    bool
	allow      []schema.GroupVersionResource
	verbs      []string
	state      State
	client     *dryRunRecordingClient
	rate       float64
	burst      int
	concurrent int
}

func dryRunLiveObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": dryRunTestName, "namespace": dryRunTestNS,
			"uid": dryRunTestUID, "resourceVersion": dryRunTestRV,
			"creationTimestamp": "2026-08-01T00:00:00Z",
			"annotations":       map[string]any{"team": "payments"},
			"managedFields":     []any{map[string]any{"manager": "kubectl"}},
		},
		"data": map[string]any{"LOG_LEVEL": "info"},
	}}
}

func dryRunPatchedObject(data map[string]any) *unstructured.Unstructured {
	obj := dryRunLiveObject()
	obj.Object["data"] = data
	meta, _ := obj.Object["metadata"].(map[string]any)
	// 휘발성 필드는 dry-run 결과에서 늘 달라집니다. diff에 나오면 안 됩니다.
	meta["resourceVersion"] = "4243"
	return obj
}

func newDryRunFixture(t *testing.T, o dryRunFixtureOptions) *Service {
	t.Helper()
	if o.client == nil {
		o.client = &dryRunRecordingClient{
			getObj:   dryRunLiveObject(),
			patchObj: dryRunPatchedObject(map[string]any{"LOG_LEVEL": "debug"}),
		}
	}
	if o.verbs == nil {
		o.verbs = []string{"get", "list", "patch", "watch"}
	}
	if o.state == "" {
		o.state = StateReady
	}
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	cfg := Config{
		ClusterID:     "prod-seoul",
		Allowlist:     []schema.GroupVersionResource{dryRunTestGVR},
		DryRunEnabled: o.enabled,
		DryRunRate:    o.rate, DryRunBurst: o.burst, DryRunConcurrent: o.concurrent,
		Now: func() time.Time { return now },
	}
	cfg.setDefaults()

	s := &Service{
		cfg:     cfg,
		clients: Clients{DryRun: o.client},
		order:   []schema.GroupVersionResource{dryRunTestGVR},
		entries: map[schema.GroupVersionResource]*resourceEntry{},
		guard: &detailGuard{rate: cfg.DetailRate, burst: cfg.DetailBurst, maxInflight: cfg.DetailConcurrent,
			tokens: float64(cfg.DetailBurst), last: now, now: cfg.Now},
		dryRunGuard: &detailGuard{rate: cfg.DryRunRate, burst: cfg.DryRunBurst, maxInflight: cfg.DryRunConcurrent,
			tokens: float64(cfg.DryRunBurst), last: now, now: cfg.Now},
		dryRunAllow: map[schema.GroupVersionResource]bool{},
	}
	for _, gvr := range o.allow {
		s.dryRunAllow[gvr] = true
	}
	s.started.Store(true)
	s.disc.Store(&discoverySnapshot{
		refreshedAt: now,
		entries: []discoveryEntry{{
			gvr: dryRunTestGVR, kind: "ConfigMap", namespaced: true,
			verbs: o.verbs, preferred: "v1", served: true,
		}},
		byGVR: map[schema.GroupVersionResource]int{dryRunTestGVR: 0},
	})
	e := &resourceEntry{gvr: dryRunTestGVR}
	e.setStatus(o.state, "")
	e.baseline.Store(buildIndexSnapshot([]any{
		&metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
			Namespace: dryRunTestNS, Name: dryRunTestName, UID: types.UID(dryRunTestUID),
		}},
	}, now))
	s.entries[dryRunTestGVR] = e
	return s
}

func dryRunValidRequest() DryRunRequest {
	return DryRunRequest{
		Group: "", Version: "v1", Resource: "configmaps",
		Namespace: dryRunTestNS, Name: dryRunTestName,
		ExpectedUID: dryRunTestUID, ExpectedResourceVersion: dryRunTestRV,
		APIVersion: "v1", Kind: "ConfigMap",
		Manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + dryRunTestName +
			"\n  namespace: " + dryRunTestNS + "\ndata:\n  LOG_LEVEL: debug\n",
	}
}

func enabledDryRunFixture(t *testing.T) *Service {
	t.Helper()
	return newDryRunFixture(t, dryRunFixtureOptions{enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}})
}

/* ── 나가는 요청 ─────────────────────────────────────────────────────────── */

// TestDryRunSendsExactlyOneApplyPatch — 검토 한 건이 만드는 Kubernetes 요청은
// live GET 하나와 dry-run apply patch 하나뿐이고, 그 옵션이 정확해야 합니다.
func TestDryRunSendsExactlyOneApplyPatch(t *testing.T) {
	s := enabledDryRunFixture(t)
	client := s.clients.DryRun.(*dryRunRecordingClient)

	result, err := s.DryRun(context.Background(), dryRunValidRequest())
	if err != nil {
		t.Fatalf("검토가 실패했습니다: %v", err)
	}
	calls, forbidden := client.snapshot()
	if len(forbidden) != 0 {
		t.Fatalf("금지된 동사가 호출되었습니다: %v", forbidden)
	}
	if len(calls) != 2 || calls[0].verb != "get" || calls[1].verb != "patch" {
		t.Fatalf("호출이 (get, patch) 둘이 아닙니다: %+v", calls)
	}
	patch := calls[1]
	if patch.patchType != types.ApplyPatchType {
		t.Errorf("patch 타입이 server-side apply가 아닙니다: %q", patch.patchType)
	}
	if got := patch.patchOptions.DryRun; len(got) != 1 || got[0] != metav1.DryRunAll {
		t.Errorf("dryRun=All이 아닙니다: %v", got)
	}
	if patch.patchOptions.FieldValidation != metav1.FieldValidationStrict {
		t.Errorf("fieldValidation이 Strict가 아닙니다: %q", patch.patchOptions.FieldValidation)
	}
	if patch.patchOptions.FieldManager != contract.ResourceDryRunFieldManager {
		t.Errorf("fieldManager가 고정값이 아닙니다: %q", patch.patchOptions.FieldManager)
	}
	// force는 **설정 자체를 하지 않습니다.** false로 명시하는 것과 다릅니다 —
	// nil이어야 어떤 경로로도 true가 될 수 없습니다.
	if patch.patchOptions.Force != nil {
		t.Errorf("force가 설정되었습니다: %v", *patch.patchOptions.Force)
	}
	if len(patch.subresources) != 0 {
		t.Errorf("subresource로 호출했습니다: %v", patch.subresources)
	}
	if patch.namespace != dryRunTestNS || patch.name != dryRunTestName {
		t.Errorf("대상이 다릅니다: %s/%s", patch.namespace, patch.name)
	}
	if result.FieldManager != contract.ResourceDryRunFieldManager {
		t.Errorf("응답 fieldManager가 다릅니다: %q", result.FieldManager)
	}
	if result.Outcome != contract.DryRunChanged {
		t.Errorf("변경이 있는데 outcome이 %q입니다", result.Outcome)
	}
	if result.ResourceVersion != dryRunTestRV || result.UID != dryRunTestUID {
		t.Errorf("결과 신원이 검토 기준과 다릅니다: %+v", result)
	}
}

// TestDryRunPatchBodyCarriesServerEnforcedIdentity — patch 본문의 신원은 서버 값이고
// 서버 소유 필드는 실려 나가지 않습니다.
func TestDryRunPatchBodyCarriesServerEnforcedIdentity(t *testing.T) {
	s := enabledDryRunFixture(t)
	client := s.clients.DryRun.(*dryRunRecordingClient)

	req := dryRunValidRequest()
	req.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + dryRunTestName +
		"\n  creationTimestamp: \"2020-01-01T00:00:00Z\"\n  generation: 7\n" +
		"  managedFields:\n  - manager: attacker\n" +
		"status:\n  phase: Bogus\n" +
		"data:\n  LOG_LEVEL: debug\n"
	if _, err := s.DryRun(context.Background(), req); err != nil {
		t.Fatalf("검토가 실패했습니다: %v", err)
	}
	calls, _ := client.snapshot()
	var body map[string]any
	if err := json.Unmarshal(calls[1].body, &body); err != nil {
		t.Fatalf("patch 본문이 JSON이 아닙니다: %v", err)
	}
	if body["apiVersion"] != "v1" || body["kind"] != "ConfigMap" {
		t.Errorf("apiVersion/kind가 서버 값이 아닙니다: %v", body)
	}
	if _, has := body["status"]; has {
		t.Error("patch 본문에 status가 실렸습니다")
	}
	meta, _ := body["metadata"].(map[string]any)
	if meta["name"] != dryRunTestName || meta["namespace"] != dryRunTestNS {
		t.Errorf("대상이 서버 값이 아닙니다: %v", meta)
	}
	if meta["uid"] != dryRunTestUID {
		t.Errorf("uid가 주입되지 않았습니다: %v", meta["uid"])
	}
	if meta["resourceVersion"] != dryRunTestRV {
		t.Errorf("resourceVersion(CAS)이 주입되지 않았습니다: %v", meta["resourceVersion"])
	}
	for _, forbidden := range []string{"managedFields", "creationTimestamp", "generation", "selfLink"} {
		if _, has := meta[forbidden]; has {
			t.Errorf("서버 소유 필드가 실렸습니다: %s", forbidden)
		}
	}
}

/* ── preflight: Kubernetes 요청 0회 ──────────────────────────────────────── */

// TestDryRunPreflightFailuresNeverReachKubernetes — 아래 어느 경우에도 클러스터로
// 나가는 요청이 없어야 합니다. 하나라도 새면 잘못된 입력이 API 서버 부하가 됩니다.
func TestDryRunPreflightFailuresNeverReachKubernetes(t *testing.T) {
	deep := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + dryRunTestName + "\ndata:\n  a: " +
		strings.Repeat("[", maxManifestDepth+4) + strings.Repeat("]", maxManifestDepth+4) + "\n"
	only := []schema.GroupVersionResource{dryRunTestGVR}

	cases := []struct {
		name    string
		enabled bool
		allow   []schema.GroupVersionResource
		mutate  func(*DryRunRequest)
		want    error
	}{
		{name: "기능 비활성", enabled: false, allow: only, want: ErrDryRunDisabled},
		{name: "opt-in 아님", enabled: true, allow: nil, want: ErrDryRunDenied},
		{
			name: "hard-deny가 opt-in을 이깁니다", enabled: true,
			allow:  []schema.GroupVersionResource{{Group: "", Version: "v1", Resource: "secrets"}},
			mutate: func(r *DryRunRequest) { r.Resource = "secrets"; r.Kind = "Secret" },
			want:   ErrDryRunDenied,
		},
		{name: "apiVersion 불일치", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) { r.APIVersion = "apps/v1" }, want: ErrManifestMismatch},
		{name: "kind 불일치", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) { r.Kind = "Deployment" }, want: ErrManifestMismatch},
		{name: "본문 name 불일치", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) {
				r.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other\n"
			}, want: ErrManifestMismatch},
		{name: "본문 namespace 불일치", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) {
				r.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + dryRunTestName + "\n  namespace: other\n"
			}, want: ErrManifestMismatch},
		{name: "본문 uid 불일치", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) {
				r.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + dryRunTestName + "\n  uid: other-uid\n"
			}, want: ErrManifestMismatch},
		{name: "본문 resourceVersion 불일치", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) {
				r.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + dryRunTestName + "\n  resourceVersion: \"1\"\n"
			}, want: ErrManifestMismatch},
		{name: "namespace 누락", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) { r.Namespace = "" }, want: ErrInvalidFilter},
		{name: "uid 누락", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) { r.ExpectedUID = "" }, want: ErrInvalidFilter},
		{name: "resourceVersion 누락", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) { r.ExpectedResourceVersion = "" }, want: ErrInvalidFilter},
		{name: "매니페스트 없음", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) { r.Manifest = "" }, want: ErrManifestInvalid},
		{name: "다중 문서", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) {
				r.Manifest += "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + dryRunTestName + "\n"
			}, want: ErrManifestInvalid},
		{name: "중복 키", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) {
				r.Manifest = "apiVersion: v1\nkind: ConfigMap\nkind: Secret\nmetadata:\n  name: " + dryRunTestName + "\n"
			}, want: ErrManifestInvalid},
		{name: "anchor/alias", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) {
				r.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata: &m\n  name: " + dryRunTestName + "\ndata: *m\n"
			}, want: ErrManifestInvalid},
		{name: "과도한 중첩", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) { r.Manifest = deep }, want: ErrManifestInvalid},
		{name: "매니페스트 상한 초과", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) {
				r.Manifest = strings.Repeat("a", contract.DefaultDryRunManifestBytes+1)
			}, want: ErrManifestTooLarge},
		{name: "목록에 없는 이름", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) {
				r.Name = "ghost"
				r.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ghost\n  namespace: " + dryRunTestNS + "\n"
			}, want: ErrObjectNotFound},
		{name: "인덱스 UID 불일치", enabled: true, allow: only,
			mutate: func(r *DryRunRequest) { r.ExpectedUID = "99999999-2222-3333-4444-555555555555" },
			want:   ErrUIDMismatch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &dryRunRecordingClient{getObj: dryRunLiveObject(), patchObj: dryRunLiveObject()}
			s := newDryRunFixture(t, dryRunFixtureOptions{enabled: tc.enabled, allow: tc.allow, client: client})
			req := dryRunValidRequest()
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			_, err := s.DryRun(context.Background(), req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("오류가 %v여야 하는데 %v입니다", tc.want, err)
			}
			if calls, forbidden := client.snapshot(); len(calls) != 0 || len(forbidden) != 0 {
				t.Fatalf("Kubernetes 요청이 발생했습니다: %+v %v", calls, forbidden)
			}
		})
	}
}

// TestDryRunDoesNotRejectByFieldName — `stringData`처럼 Secret에서 쓰이는 이름을
// 정당하게 소유한 리소스가 있습니다. 필드 **이름**으로 막으면 그런 리소스를 쓸 수
// 없게 됩니다. Secret은 정확한 GVR과 descriptor kind로만 막힙니다.
func TestDryRunDoesNotRejectByFieldName(t *testing.T) {
	client := &dryRunRecordingClient{
		getObj:   dryRunLiveObject(),
		patchObj: dryRunPatchedObject(map[string]any{"LOG_LEVEL": "debug"}),
	}
	s := newDryRunFixture(t, dryRunFixtureOptions{
		enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}, client: client,
	})
	req := dryRunValidRequest()
	req.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + dryRunTestName +
		"\n  namespace: " + dryRunTestNS + "\nstringData:\n  anything: value\n"
	if _, err := s.DryRun(context.Background(), req); err != nil {
		t.Fatalf("필드 이름만으로 거부되었습니다: %v", err)
	}
	if client.countOf("patch") != 1 {
		t.Fatalf("patch가 1번 나가야 합니다: %d", client.countOf("patch"))
	}
}

// TestDryRunRequiresReadyCatalogAndPatchVerb — 캐시가 준비되지 않았거나 API가
// patch를 제공하지 않으면 요청 없이 끝나고 capability도 꺼집니다.
func TestDryRunRequiresReadyCatalogAndPatchVerb(t *testing.T) {
	for _, tc := range []struct {
		name  string
		verbs []string
		state State
		want  error
	}{
		{name: "동기화 중", verbs: []string{"get", "list", "patch", "watch"}, state: StateSyncing, want: ErrSyncing},
		{name: "patch verb 없음", verbs: []string{"get", "list", "watch"}, state: StateReady, want: ErrUnsupported},
		{name: "get verb 없음", verbs: []string{"list", "patch", "watch"}, state: StateReady, want: ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &dryRunRecordingClient{getObj: dryRunLiveObject()}
			s := newDryRunFixture(t, dryRunFixtureOptions{
				enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR},
				verbs: tc.verbs, state: tc.state, client: client,
			})
			if _, err := s.DryRun(context.Background(), dryRunValidRequest()); !errors.Is(err, tc.want) {
				t.Fatalf("오류가 %v여야 하는데 %v입니다", tc.want, err)
			}
			if calls, _ := client.snapshot(); len(calls) != 0 {
				t.Fatalf("Kubernetes 요청이 발생했습니다: %+v", calls)
			}
			if desc, err := s.Describe(dryRunTestGVR); err == nil && desc.DryRun {
				t.Error("검토할 수 없는 상태인데 capability가 켜져 있습니다")
			}
		})
	}
}

/* ── live GET 이후의 신원 ────────────────────────────────────────────────── */

// TestDryRunStaleIdentityStopsBeforePatch — live 객체가 우리가 본 것과 다르면
// patch를 보내지 않습니다.
func TestDryRunStaleIdentityStopsBeforePatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		live func() *unstructured.Unstructured
		want error
	}{
		{
			name: "UID가 바뀜",
			live: func() *unstructured.Unstructured {
				obj := dryRunLiveObject()
				obj.Object["metadata"].(map[string]any)["uid"] = "99999999-2222-3333-4444-555555555555"
				return obj
			},
			want: ErrUIDMismatch,
		},
		{
			name: "resourceVersion이 앞섬",
			live: func() *unstructured.Unstructured {
				obj := dryRunLiveObject()
				obj.Object["metadata"].(map[string]any)["resourceVersion"] = "9999"
				return obj
			},
			want: ErrResourceVersionMismatch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &dryRunRecordingClient{getObj: tc.live(), patchObj: dryRunLiveObject()}
			s := newDryRunFixture(t, dryRunFixtureOptions{
				enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}, client: client,
			})
			if _, err := s.DryRun(context.Background(), dryRunValidRequest()); !errors.Is(err, tc.want) {
				t.Fatalf("오류가 %v여야 하는데 %v입니다", tc.want, err)
			}
			if got := client.countOf("patch"); got != 0 {
				t.Fatalf("patch가 %d번 나갔습니다", got)
			}
			if got := client.countOf("get"); got != 1 {
				t.Fatalf("get이 %d번 나갔습니다", got)
			}
		})
	}
}

// TestDryRunDiscardsResultWithDifferentUID — 응답 객체가 다른 객체면 diff를
// 만들지 않고 버립니다.
func TestDryRunDiscardsResultWithDifferentUID(t *testing.T) {
	swapped := dryRunLiveObject()
	swapped.Object["metadata"].(map[string]any)["uid"] = "88888888-2222-3333-4444-555555555555"
	client := &dryRunRecordingClient{getObj: dryRunLiveObject(), patchObj: swapped}
	s := newDryRunFixture(t, dryRunFixtureOptions{
		enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}, client: client,
	})
	if _, err := s.DryRun(context.Background(), dryRunValidRequest()); !errors.Is(err, ErrUIDMismatch) {
		t.Fatalf("오류가 ErrUIDMismatch여야 하는데 %v입니다", err)
	}
}

/* ── 거절과 오류의 분리 ──────────────────────────────────────────────────── */

func dryRunStatusError(reason metav1.StatusReason, code int32, message string, causes ...metav1.StatusCause) *apierrors.StatusError {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Reason: reason, Code: code, Message: message,
		Details: &metav1.StatusDetails{Causes: causes},
	}}
}

// TestDryRunClassifiesUpstreamOutcomes — 무엇이 200 거절이고 무엇이 오류인지,
// 그리고 어느 쪽도 Kubernetes 원문을 노출하지 않는지 봅니다.
func TestDryRunClassifiesUpstreamOutcomes(t *testing.T) {
	secretish := "internal-apiserver.svc:6443 rejected token=s3cr3t"
	only := []schema.GroupVersionResource{dryRunTestGVR}

	t.Run("소유권 충돌은 200 거절", func(t *testing.T) {
		client := &dryRunRecordingClient{
			getObj: dryRunLiveObject(),
			patchErr: dryRunStatusError(metav1.StatusReasonConflict, 409, "Apply failed with 1 conflict "+secretish,
				metav1.StatusCause{
					Type:    metav1.CauseTypeFieldManagerConflict,
					Field:   ".data.LOG_LEVEL",
					Message: `conflict with "kubectl-client-side-apply" using v1 ` + secretish,
				}),
		}
		s := newDryRunFixture(t, dryRunFixtureOptions{enabled: true, allow: only, client: client})
		result, err := s.DryRun(context.Background(), dryRunValidRequest())
		if err != nil {
			t.Fatalf("충돌은 오류가 아니라 검토 결과여야 합니다: %v", err)
		}
		if result.Outcome != contract.DryRunRejected || result.RejectedBy != contract.DryRunRejectedByConflict {
			t.Fatalf("결과가 conflict 거절이 아닙니다: %+v", result)
		}
		if len(result.Violations) != 1 {
			t.Fatalf("violation이 1개여야 합니다: %+v", result.Violations)
		}
		v := result.Violations[0]
		// Field·Manager는 둘 다 upstream 원문이므로 Phase 1에서는 비어 있습니다.
		if v.Field != "" || v.Manager != "" {
			t.Errorf("upstream cause 값이 복사되었습니다: %+v", v)
		}
		if v.Message != msgFieldOwnedByOther {
			t.Errorf("서버가 다시 쓴 문장이 아닙니다: %q", v.Message)
		}
		if result.Changes == nil || result.Redacted == nil || result.Warnings == nil {
			t.Error("필수 배열이 nil입니다")
		}
		assertNoDryRunLeak(t, result, secretish)
	})

	t.Run("낙관적 동시성 실패는 409", func(t *testing.T) {
		client := &dryRunRecordingClient{
			getObj:   dryRunLiveObject(),
			patchErr: dryRunStatusError(metav1.StatusReasonConflict, 409, "the object has been modified"),
		}
		s := newDryRunFixture(t, dryRunFixtureOptions{enabled: true, allow: only, client: client})
		if _, err := s.DryRun(context.Background(), dryRunValidRequest()); !errors.Is(err, ErrResourceVersionMismatch) {
			t.Fatalf("오류가 ErrResourceVersionMismatch여야 하는데 %v입니다", err)
		}
	})

	t.Run("Strict 검증 실패는 200 거절", func(t *testing.T) {
		client := &dryRunRecordingClient{
			getObj: dryRunLiveObject(),
			patchErr: dryRunStatusError(metav1.StatusReasonInvalid, 422, "ConfigMap cannot be handled "+secretish,
				metav1.StatusCause{Type: metav1.CauseTypeFieldValueNotSupported, Field: "data.typo", Message: secretish}),
		}
		s := newDryRunFixture(t, dryRunFixtureOptions{enabled: true, allow: only, client: client})
		result, err := s.DryRun(context.Background(), dryRunValidRequest())
		if err != nil {
			t.Fatalf("검증 실패는 검토 결과여야 합니다: %v", err)
		}
		if result.RejectedBy != contract.DryRunRejectedByValidation {
			t.Fatalf("rejectedBy가 validation이 아닙니다: %q", result.RejectedBy)
		}
		if result.Violations[0].Message != msgFieldUnknown {
			t.Errorf("문장이 서버 것이 아닙니다: %q", result.Violations[0].Message)
		}
		assertNoDryRunLeak(t, result, secretish)
	})

	t.Run("admission webhook 거절은 200 거절", func(t *testing.T) {
		client := &dryRunRecordingClient{
			getObj: dryRunLiveObject(),
			patchErr: dryRunStatusError(metav1.StatusReasonForbidden, 403,
				`admission webhook "policy.example.com" denied the request: `+secretish),
		}
		s := newDryRunFixture(t, dryRunFixtureOptions{enabled: true, allow: only, client: client})
		result, err := s.DryRun(context.Background(), dryRunValidRequest())
		if err != nil {
			t.Fatalf("admission 거절은 검토 결과여야 합니다: %v", err)
		}
		if result.RejectedBy != contract.DryRunRejectedByAdmission {
			t.Fatalf("rejectedBy가 admission이 아닙니다: %q", result.RejectedBy)
		}
		assertNoDryRunLeak(t, result, secretish)
	})

	t.Run("RBAC 부족은 sentinel", func(t *testing.T) {
		client := &dryRunRecordingClient{
			getObj: dryRunLiveObject(),
			patchErr: dryRunStatusError(metav1.StatusReasonForbidden, 403,
				`configmaps "api-config" is forbidden: User "system:serviceaccount:obs:api" cannot patch resource`),
		}
		s := newDryRunFixture(t, dryRunFixtureOptions{enabled: true, allow: only, client: client})
		_, err := s.DryRun(context.Background(), dryRunValidRequest())
		if !errors.Is(err, ErrDryRunForbidden) {
			t.Fatalf("오류가 ErrDryRunForbidden이어야 하는데 %v입니다", err)
		}
		if strings.Contains(err.Error(), "serviceaccount") {
			t.Errorf("오류 문자열에 upstream 원문이 남았습니다: %v", err)
		}
	})

	t.Run("분류되지 않은 실패는 원문을 감싸지 않습니다", func(t *testing.T) {
		client := &dryRunRecordingClient{getObj: dryRunLiveObject(), patchErr: errors.New(secretish)}
		s := newDryRunFixture(t, dryRunFixtureOptions{enabled: true, allow: only, client: client})
		_, err := s.DryRun(context.Background(), dryRunValidRequest())
		if !errors.Is(err, ErrDryRunUpstream) {
			t.Fatalf("오류가 ErrDryRunUpstream이어야 하는데 %v입니다", err)
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Fatalf("오류에 upstream 원문이 실렸습니다: %v", err)
		}
	})

	t.Run("live GET 실패도 sentinel", func(t *testing.T) {
		client := &dryRunRecordingClient{getErr: errors.New(secretish)}
		s := newDryRunFixture(t, dryRunFixtureOptions{enabled: true, allow: only, client: client})
		_, err := s.DryRun(context.Background(), dryRunValidRequest())
		if !errors.Is(err, ErrDryRunUpstream) {
			t.Fatalf("오류가 ErrDryRunUpstream이어야 하는데 %v입니다", err)
		}
		if client.countOf("patch") != 0 {
			t.Error("GET이 실패했는데 patch가 나갔습니다")
		}
	})
}

// assertNoDryRunLeak은 결과 어디에도 upstream 원문 조각이 없는지 봅니다.
func assertNoDryRunLeak(t *testing.T, result DryRunResult, needle string) {
	t.Helper()
	rendered, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("결과를 직렬화하지 못했습니다: %v", err)
	}
	for _, fragment := range []string{needle, "s3cr3t", "internal-apiserver"} {
		if strings.Contains(string(rendered), fragment) {
			t.Fatalf("결과에 upstream 원문이 남았습니다(%q): %s", fragment, rendered)
		}
	}
}

/* ── 예산·취소 ───────────────────────────────────────────────────────────── */

// TestDryRunGuardIsIsolatedFromDetail — 검토가 예산을 다 써도 상세 조회 예산은
// 그대로여야 합니다. 하나를 공유하면 검토 한 번이 상세 화면을 막습니다.
func TestDryRunGuardIsIsolatedFromDetail(t *testing.T) {
	client := &dryRunRecordingClient{
		getObj:   dryRunLiveObject(),
		patchObj: dryRunPatchedObject(map[string]any{"LOG_LEVEL": "debug"}),
	}
	s := newDryRunFixture(t, dryRunFixtureOptions{
		enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}, client: client,
		rate: 0.0001, burst: 1, concurrent: 1,
	})
	if _, err := s.DryRun(context.Background(), dryRunValidRequest()); err != nil {
		t.Fatalf("첫 검토가 실패했습니다: %v", err)
	}
	_, err := s.DryRun(context.Background(), dryRunValidRequest())
	if !errors.Is(err, ErrDryRunRateLimited) {
		t.Fatalf("두 번째 검토는 ErrDryRunRateLimited여야 하는데 %v입니다", err)
	}
	if errors.Is(err, ErrRateLimited) {
		t.Error("상세 조회 예산 오류와 구분되지 않습니다")
	}
	release, detailErr := s.guard.acquire()
	if detailErr != nil {
		t.Fatalf("상세 예산이 함께 소진되었습니다: %v", detailErr)
	}
	release()
}

// TestDryRunCancelledContextSkipsKubernetes — 이미 취소된 요청은 토큰도 슬롯도
// 쓰지 않고 클러스터로 나가지도 않습니다.
func TestDryRunCancelledContextSkipsKubernetes(t *testing.T) {
	client := &dryRunRecordingClient{getObj: dryRunLiveObject()}
	s := newDryRunFixture(t, dryRunFixtureOptions{
		enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}, client: client,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.DryRun(ctx, dryRunValidRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("오류가 context.Canceled여야 하는데 %v입니다", err)
	}
	if calls, _ := client.snapshot(); len(calls) != 0 {
		t.Fatalf("취소된 요청이 클러스터로 나갔습니다: %+v", calls)
	}
	release, err := s.dryRunGuard.acquire()
	if err != nil {
		t.Fatalf("취소가 검토 예산을 소모했습니다: %v", err)
	}
	release()
}

// TestDryRunCancelDuringPatchPropagates — patch 도중 취소는 그대로 전파됩니다.
func TestDryRunCancelDuringPatchPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &dryRunRecordingClient{getObj: dryRunLiveObject(), patchObj: dryRunLiveObject(), onPatch: cancel}
	s := newDryRunFixture(t, dryRunFixtureOptions{
		enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}, client: client,
	})
	if _, err := s.DryRun(ctx, dryRunValidRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("오류가 context.Canceled여야 하는데 %v입니다", err)
	}
}

/* ── 정책·capability·클라이언트 격리 ─────────────────────────────────────── */

// TestDryRunAllowlistRejectsIneligibleTargets — 기동 검증이 hard-deny와 범위를
// 잡아냅니다. 조용히 빼면 운영자가 켜졌다고 착각합니다.
func TestDryRunAllowlistRejectsIneligibleTargets(t *testing.T) {
	explorer := []schema.GroupVersionResource{
		dryRunTestGVR,
		{Group: "", Version: "v1", Resource: "secrets"},
		{Group: "", Version: "v1", Resource: "serviceaccounts"},
		{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
	}
	for _, tc := range []struct {
		name string
		in   []schema.GroupVersionResource
	}{
		{"secrets", []schema.GroupVersionResource{{Group: "", Version: "v1", Resource: "secrets"}}},
		{"serviceaccounts", []schema.GroupVersionResource{{Group: "", Version: "v1", Resource: "serviceaccounts"}}},
		{"nodes", []schema.GroupVersionResource{{Group: "", Version: "v1", Resource: "nodes"}}},
		{"namespaces", []schema.GroupVersionResource{{Group: "", Version: "v1", Resource: "namespaces"}}},
		{"rbac", []schema.GroupVersionResource{{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}}},
		{"crd", []schema.GroupVersionResource{{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}}},
	} {
		if _, err := NormalizeDryRunAllowlist(tc.in, explorer, nil); err == nil {
			t.Errorf("%s는 검토 대상이 될 수 없어야 합니다", tc.name)
		}
	}
	outside := []schema.GroupVersionResource{{Group: "batch", Version: "v1", Resource: "jobs"}}
	if _, err := NormalizeDryRunAllowlist(outside, explorer, nil); err == nil {
		t.Error("조회 allowlist 밖 GVR이 통과했습니다")
	}
	// deny는 오류가 아니라 빼기입니다.
	got, err := NormalizeDryRunAllowlist(
		[]schema.GroupVersionResource{dryRunTestGVR, {Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}},
		explorer,
		[]schema.GroupVersionResource{{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}},
	)
	if err != nil {
		t.Fatalf("deny 적용이 실패했습니다: %v", err)
	}
	if len(got) != 1 || got[0] != dryRunTestGVR {
		t.Fatalf("deny가 적용되지 않았습니다: %v", got)
	}
	if list, err := NormalizeDryRunAllowlist(nil, explorer, nil); err != nil || list != nil {
		t.Fatalf("빈 목록은 오류가 아니어야 합니다: %v %v", list, err)
	}
	// 버전이 달라도 계속 막힙니다.
	if !DryRunHardDenied(schema.GroupVersionResource{Group: "", Version: "v2", Resource: "secrets"}) {
		t.Error("버전만 바꾸면 hard-deny를 우회할 수 있습니다")
	}
	// **승인된 목록 밖은 막지 않습니다.** 넓게 막으면 정당한 운영 대상이 사라지고,
	// 그 사실이 UI에서는 "기능이 없다"로만 보입니다.
	for _, gvr := range []schema.GroupVersionResource{
		{Group: "certificates.k8s.io", Version: "v1", Resource: "certificatesigningrequests"},
		{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"},
		{Group: "apiregistration.k8s.io", Version: "v1", Resource: "apiservices"},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "flowschemas"},
		{Group: "authentication.k8s.io", Version: "v1", Resource: "tokenreviews"},
		{Group: "authorization.k8s.io", Version: "v1", Resource: "subjectaccessreviews"},
	} {
		if DryRunHardDenied(gvr) {
			t.Errorf("승인 목록에 없는 %s가 통째로 막혔습니다", FormatGVR(gvr))
		}
	}
	// apiextensions는 **group 전체가 아니라 CRD 하나**만 막습니다. 같은 group의
	// 다른 리소스는 막지 않고, CRD는 버전이 달라도 막힙니다.
	if !DryRunHardDenied(schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1beta1", Resource: "customresourcedefinitions"}) {
		t.Error("CRD가 버전에 따라 통과합니다")
	}
	if DryRunHardDenied(schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "somethingelse"}) {
		t.Error("apiextensions group이 통째로 막혔습니다")
	}
	// RBAC은 반대로 group 전체입니다.
	for _, resource := range []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"} {
		if !DryRunHardDenied(schema.GroupVersionResource{
			Group: "rbac.authorization.k8s.io", Version: "v1", Resource: resource}) {
			t.Errorf("RBAC %s가 통과합니다", resource)
		}
	}
	// 이름이 같은 사용자 리소스는 코어 리소스와 다릅니다. GVR로만 판정합니다.
	for _, gvr := range []schema.GroupVersionResource{
		{Group: "vault.example.com", Version: "v1", Resource: "secrets"},
		{Group: "example.com", Version: "v1", Resource: "nodes"},
	} {
		if DryRunHardDenied(gvr) {
			t.Errorf("이름만 같은 CRD %s가 막혔습니다", FormatGVR(gvr))
		}
	}
}

// TestDryRunValidatesKindAgainstDescriptor — kind는 텍스트가 아니라 선택된
// descriptor와 대조합니다. 같은 이름의 CRD를 kind 문자열만으로 막지 않습니다.
func TestDryRunValidatesKindAgainstDescriptor(t *testing.T) {
	desc := Descriptor{Group: "", Version: "v1", Resource: "configmaps", Kind: "ConfigMap", Namespaced: true}
	req := dryRunValidRequest()
	if err := validateDryRunIdentity(req, desc); err != nil {
		t.Fatalf("정상 요청이 거부되었습니다: %v", err)
	}
	// descriptor가 kind를 모르면 대조할 근거가 없으므로 거절합니다.
	unknown := desc
	unknown.Kind = ""
	if err := validateDryRunIdentity(req, unknown); !errors.Is(err, ErrManifestMismatch) {
		t.Errorf("kind를 모르면 fail-closed여야 합니다: %v", err)
	}
	// descriptor가 Secret이라고 말하는 CRD라면 kind 텍스트가 아니라 그 값이 기준입니다.
	crd := Descriptor{Group: "vault.example.com", Version: "v1", Resource: "secrets", Kind: "Secret", Namespaced: true}
	crdReq := req
	crdReq.APIVersion = "vault.example.com/v1"
	crdReq.Kind = "Secret"
	if err := validateDryRunIdentity(crdReq, crd); err != nil {
		t.Errorf("이름만 같은 CRD가 kind 텍스트로 막혔습니다: %v", err)
	}
}

// TestDryRunCapabilityIsNotPermission — capability는 배포 설정입니다.
func TestDryRunCapabilityIsNotPermission(t *testing.T) {
	off := newDryRunFixture(t, dryRunFixtureOptions{enabled: false, allow: []schema.GroupVersionResource{dryRunTestGVR}})
	if desc, _ := off.Describe(dryRunTestGVR); desc.DryRun {
		t.Error("기능이 꺼졌는데 capability가 켜져 있습니다")
	}
	notOptedIn := newDryRunFixture(t, dryRunFixtureOptions{enabled: true})
	if desc, _ := notOptedIn.Describe(dryRunTestGVR); desc.DryRun {
		t.Error("opt-in 밖인데 capability가 켜져 있습니다")
	}
	on := enabledDryRunFixture(t)
	if desc, _ := on.Describe(dryRunTestGVR); !desc.DryRun {
		t.Error("검토 가능한 GVR인데 capability가 꺼져 있습니다")
	}
	if snapshot := on.Catalog(); len(snapshot.Descriptors) != 1 || !snapshot.Descriptors[0].DryRun {
		t.Error("카탈로그가 capability를 싣지 않았습니다")
	}
	// 전용 클라이언트가 없으면(=배포에 기능 없음) 무조건 fail-closed입니다.
	on.clients.DryRun = nil
	if desc, _ := on.Describe(dryRunTestGVR); desc.DryRun {
		t.Error("전용 클라이언트가 없는데 capability가 켜져 있습니다")
	}
	if _, err := on.DryRun(context.Background(), dryRunValidRequest()); !errors.Is(err, ErrDryRunDisabled) {
		t.Errorf("클라이언트가 없으면 fail-closed여야 합니다: %v", err)
	}
}

// TestNewClientsBuildsDryRunClientOnlyWhenEnabled — 기본값은 검토 클라이언트를
// 만들지 않습니다. 기존 호출자·테스트가 영향을 받지 않아야 합니다.
func TestNewClientsBuildsDryRunClientOnlyWhenEnabled(t *testing.T) {
	base := &rest.Config{Host: "https://127.0.0.1:1"}

	off, err := NewClients(base, ClientOptions{})
	if err != nil {
		t.Fatalf("기본 클라이언트 생성이 실패했습니다: %v", err)
	}
	if off.DryRun != nil {
		t.Error("기본값인데 검토 클라이언트가 만들어졌습니다")
	}
	if off.Metadata == nil || off.Discovery == nil || off.Dynamic == nil {
		t.Error("기존 클라이언트가 사라졌습니다")
	}

	on, err := NewClients(base, ClientOptions{DryRunEnabled: true})
	if err != nil {
		t.Fatalf("검토 클라이언트 생성이 실패했습니다: %v", err)
	}
	if on.DryRun == nil {
		t.Fatal("켰는데 검토 클라이언트가 없습니다")
	}
	// 상세 client와 **다른 인스턴스**여야 합니다. 같으면 예산·timeout을 공유합니다.
	if on.DryRun == on.Dynamic {
		t.Error("검토 클라이언트가 상세 클라이언트와 같은 인스턴스입니다")
	}

	var o ClientOptions
	o.setDefaults()
	if o.DryRunTimeout != DefaultDryRunTimeout || o.DryRunBurst != DefaultDryRunBurst ||
		o.MaxDryRunObjectBytes != DefaultMaxObjectBytes {
		t.Errorf("검토 클라이언트 기본 상한이 다릅니다: %+v", o)
	}
}

/* ── warning 신호·문자열 유계화 ──────────────────────────────────────────── */

// dryRunTestRawWarningMarker/dryRunTestRawConflictMarker는 upstream 원문에만
// 존재하는 표식입니다. 결과·오류 어디에서든 이 문자열이 보이면 원문이 샌 것입니다.
const (
	dryRunTestRawWarningMarker  = "RAW_WARNING_SECRET_MARKER"
	dryRunTestRawConflictMarker = "RAW_CONFLICT_SECRET_MARKER"
)

// TestDryRunWarningSinkNeverStoresHeaderText — 수집기는 헤더 값을 보관하지도
// 파싱하지도 않습니다. 개수도 헤더에서 오지 않습니다.
func TestDryRunWarningSinkNeverStoresHeaderText(t *testing.T) {
	sink := &warningSink{}
	if got := sink.take(); got == nil || len(got) != 0 {
		t.Fatalf("빈 수집기는 빈 배열이어야 합니다: %v", got)
	}
	// 경고가 여러 번 있었어도 결과는 서버 문장 **하나**입니다.
	sink.add(true)
	sink.add(true)
	sink.add(true)
	got := sink.take()
	if len(got) != 1 {
		t.Fatalf("서버 문장 하나여야 하는데 %d개입니다: %v", len(got), got)
	}
	if got[0] != msgUpstreamWarned {
		t.Fatalf("서버가 쓴 고정 문장이 아닙니다: %q", got[0])
	}
	if len(got[0]) > dryRunTextBytes {
		t.Errorf("문장 길이 상한을 넘었습니다: %d", len(got[0]))
	}
	if again := sink.take(); len(again) != 0 {
		t.Errorf("take는 비워야 합니다: %v", again)
	}
	// 경고가 없었으면 빈 배열입니다.
	fresh := &warningSink{}
	fresh.add(false)
	if got := fresh.take(); len(got) != 0 {
		t.Errorf("경고가 없는데 값이 생겼습니다: %v", got)
	}
}

// TestDryRunWarningHeaderTextCannotEscape — 실제 transport 경로로 Warning 헤더
// 원문을 흘려보내도 결과에 그 표식이 나타나지 않아야 합니다.
func TestDryRunWarningHeaderTextCannotEscape(t *testing.T) {
	sink := &warningSink{}
	transport := &warningTransport{base: dryRunTestRoundTripper(func(req *http.Request) *http.Response {
		header := http.Header{}
		header.Add("Warning", `299 - "`+dryRunTestRawWarningMarker+`"`)
		return &http.Response{StatusCode: 200, Header: header, Body: http.NoBody, Request: req}
	})}
	req, err := http.NewRequestWithContext(
		withWarningSink(context.Background(), sink), http.MethodPatch, "https://example.invalid/x", nil)
	if err != nil {
		t.Fatalf("요청을 만들지 못했습니다: %v", err)
	}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("transport가 실패했습니다: %v", err)
	}
	got := sink.take()
	if len(got) != 1 || got[0] != msgUpstreamWarned {
		t.Fatalf("서버 문장 하나가 아닙니다: %v", got)
	}
	for _, item := range got {
		if strings.Contains(item, dryRunTestRawWarningMarker) {
			t.Fatalf("Warning 원문이 새어 나갔습니다: %q", item)
		}
	}
}

type dryRunTestRoundTripper func(*http.Request) *http.Response

func (f dryRunTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

// TestDryRunResultNeverCarriesRawUpstreamText — 경고와 충돌 원문이 동시에
// 들어와도 결과 어디에도 표식이 남지 않고, Manager도 비어 있어야 합니다.
func TestDryRunResultNeverCarriesRawUpstreamText(t *testing.T) {
	client := &dryRunRecordingClient{
		getObj: dryRunLiveObject(),
		// 표식을 Status의 **세 자리 모두**에 넣습니다 — top message, cause.Message,
		// cause.Field. 어느 하나라도 복사되면 결과 JSON에서 잡힙니다.
		patchErr: dryRunStatusError(metav1.StatusReasonConflict, 409,
			"Apply failed with 1 conflict "+dryRunTestRawConflictMarker,
			metav1.StatusCause{
				Type:    metav1.CauseTypeFieldManagerConflict,
				Field:   ".data." + dryRunTestRawConflictMarker,
				Message: `conflict with "kubectl" using v1 ` + dryRunTestRawConflictMarker,
			}),
		warnHeader: dryRunTestRawWarningMarker,
	}
	s := newDryRunFixture(t, dryRunFixtureOptions{
		enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}, client: client,
	})
	result, err := s.DryRun(context.Background(), dryRunValidRequest())
	if err != nil {
		t.Fatalf("충돌은 검토 결과여야 합니다: %v", err)
	}
	if len(result.Violations) != 1 {
		t.Fatalf("violation이 1개여야 합니다: %+v", result.Violations)
	}
	v := result.Violations[0]
	if v.Message != msgFieldOwnedByOther {
		t.Errorf("문장이 서버 것이 아닙니다: %q", v.Message)
	}
	// Field·Manager는 둘 다 upstream cause 원문이므로 Phase 1에서는 비어 있습니다.
	if v.Field != "" || v.Manager != "" {
		t.Errorf("upstream cause 값이 복사되었습니다: %+v", v)
	}
	// 경고는 서버 문장 하나뿐입니다.
	if len(result.Warnings) != 1 || result.Warnings[0] != msgUpstreamWarned {
		t.Errorf("경고가 서버 문장 하나가 아닙니다: %v", result.Warnings)
	}
	assertNoDryRunLeak(t, result, dryRunTestRawConflictMarker)
	assertNoDryRunLeak(t, result, dryRunTestRawWarningMarker)
}

// TestDryRunValidationCausesNeverReachTheResponse — 검증 거절 경로도 같습니다.
// cause.Field·cause.Message·top message 어디에 표식을 넣어도 나오지 않아야 합니다.
func TestDryRunValidationCausesNeverReachTheResponse(t *testing.T) {
	client := &dryRunRecordingClient{
		getObj: dryRunLiveObject(),
		patchErr: dryRunStatusError(metav1.StatusReasonInvalid, 422,
			"rejected "+dryRunTestRawConflictMarker,
			metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueNotSupported,
				Field:   "data." + dryRunTestRawConflictMarker,
				Message: "bad value " + dryRunTestRawConflictMarker,
			},
			metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Field:   "spec." + dryRunTestRawConflictMarker,
				Message: dryRunTestRawConflictMarker,
			}),
	}
	s := newDryRunFixture(t, dryRunFixtureOptions{
		enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}, client: client,
	})
	result, err := s.DryRun(context.Background(), dryRunValidRequest())
	if err != nil {
		t.Fatalf("검증 거절은 검토 결과여야 합니다: %v", err)
	}
	// 개수와 타입은 남습니다 — 둘 다 열거형·정수라 원문이 아닙니다.
	if len(result.Violations) != 2 {
		t.Fatalf("cause 개수만큼 나와야 합니다: %+v", result.Violations)
	}
	if result.Violations[0].Message != msgFieldUnknown || result.Violations[1].Message != msgFieldInvalid {
		t.Errorf("타입별 고정 문장이 아닙니다: %+v", result.Violations)
	}
	for _, v := range result.Violations {
		if v.Field != "" || v.Manager != "" {
			t.Errorf("upstream cause 값이 복사되었습니다: %+v", v)
		}
	}
	assertNoDryRunLeak(t, result, dryRunTestRawConflictMarker)
}

// TestDryRunUpstreamTextNeverReachesErrors — 응답뿐 아니라 **오류 문자열**로도
// 우회하지 않아야 합니다. 오류는 로그로 나가므로 같은 경계가 걸립니다.
func TestDryRunUpstreamTextNeverReachesErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch error
	}{
		{"RBAC 거절", dryRunStatusError(metav1.StatusReasonForbidden, 403, "cannot patch "+dryRunTestRawConflictMarker)},
		{"낙관적 동시성", dryRunStatusError(metav1.StatusReasonConflict, 409, "modified "+dryRunTestRawConflictMarker)},
		{"분류 불가", errors.New(dryRunTestRawConflictMarker)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &dryRunRecordingClient{getObj: dryRunLiveObject(), patchErr: tc.patch}
			s := newDryRunFixture(t, dryRunFixtureOptions{
				enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}, client: client,
			})
			_, err := s.DryRun(context.Background(), dryRunValidRequest())
			if err == nil {
				t.Fatal("오류가 나와야 합니다")
			}
			if strings.Contains(err.Error(), dryRunTestRawConflictMarker) {
				t.Fatalf("오류 문자열에 upstream 원문이 실렸습니다: %v", err)
			}
		})
	}
}

// TestDryRunWarningsComeOnlyFromPatch — live GET이 낸 경고는 검토 결과가 아닙니다.
// 수집기를 GET에 달면 "객체가 원래 내는 경고"와 "이 변경이 만든 경고"가 섞입니다.
func TestDryRunWarningsComeOnlyFromPatch(t *testing.T) {
	client := &dryRunRecordingClient{
		getObj:   dryRunLiveObject(),
		patchObj: dryRunPatchedObject(map[string]any{"LOG_LEVEL": "debug"}),
	}
	s := newDryRunFixture(t, dryRunFixtureOptions{
		enabled: true, allow: []schema.GroupVersionResource{dryRunTestGVR}, client: client,
	})
	if _, err := s.DryRun(context.Background(), dryRunValidRequest()); err != nil {
		t.Fatalf("검토가 실패했습니다: %v", err)
	}
	if client.getSawWarningSink {
		t.Error("live GET에 경고 수집기가 달렸습니다")
	}
	if !client.patchSawWarningSink {
		t.Error("patch에 경고 수집기가 달리지 않았습니다")
	}
}

// TestDryRunBoundedTextCutsOnRuneBoundary — 멀티바이트 문자가 바이트 중간에서
// 잘리면 잘못된 UTF-8이 JSON으로 나갑니다.
func TestDryRunBoundedTextCutsOnRuneBoundary(t *testing.T) {
	got := boundedText(strings.Repeat("가", 300), 512) // 900 bytes
	if len(got) > 512 {
		t.Fatalf("바이트 상한을 넘었습니다: %d", len(got))
	}
	if len(got)%3 != 0 {
		t.Fatalf("rune 경계에서 자르지 않았습니다: %d바이트", len(got))
	}
	if strings.ContainsRune(boundedText("a\x07b", 16), '\x07') {
		t.Error("제어문자가 남았습니다")
	}
}
