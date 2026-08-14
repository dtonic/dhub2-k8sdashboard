package clusterstate

// observe.go의 억제 규칙·변환·informer 연동 테스트입니다. (#12)
// 패키지 내부 테스트인 이유: changeHandlers·podChange 같은 unexported 규칙을
// informer 없이 직접 검증하고, testcluster(순환 참조)를 피하기 위해서입니다.

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/tools/cache"
)

func observePod(ns, name, uid, rv string) *corev1.Pod {
	yes := true
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns, UID: types.UID(uid), ResourceVersion: rv,
		OwnerReferences: []metav1.OwnerReference{{
			Kind: "ReplicaSet", Name: "checkout-7f", UID: "rs-checkout-7f", Controller: &yes,
		}},
	}}
}

func observeRSMeta(ns string) *metav1.PartialObjectMetadata {
	yes := true
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-7f", Namespace: ns, UID: "rs-checkout-7f",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Deployment", Name: "checkout", UID: "dep-checkout", Controller: &yes,
			}},
		},
	}
}

// newObserveStore는 testcluster 없이(순환 참조 방지) 최소 픽스처 스토어를 만듭니다.
// 최초 LIST 억제를 검증하기 위해 Pod 하나가 **이미 존재하는 채로** 동기화합니다.
func newObserveStore(t *testing.T) (*Store, *fake.Clientset, <-chan Change) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	typed := fake.NewSimpleClientset(observePod("payments", "checkout-7f-aaa", "pod-aaa", "1"))
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	meta := metadatafake.NewSimpleMetadataClient(scheme, []runtime.Object{observeRSMeta("payments")}...)

	store, err := New(Clients{Typed: typed, Metadata: meta},
		Options{Resync: 0, EventFieldSelector: "", ClusterID: "prod-seoul"})
	if err != nil {
		t.Fatal(err)
	}

	changes := make(chan Change, 64)
	if err := store.OnChange(func(c Change) { changes <- c }); err != nil {
		t.Fatal(err)
	}
	if err := store.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return store, typed, changes
}

func waitChange(t *testing.T, ch <-chan Change) Change {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("변경 수신 타임아웃")
	}
	return Change{}
}

func assertNoChange(t *testing.T, ch <-chan Change) {
	t.Helper()
	select {
	case c := <-ch:
		t.Fatalf("억제되어야 할 변경이 나왔습니다: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
}

/* ── informer 연동 (실제 fake watch 경로) ───────────────────────────────── */

// TestInitialListIsSuppressed — 동기화가 끝나도 기존 객체는 변경으로 나오지 않습니다.
func TestInitialListIsSuppressed(t *testing.T) {
	_, _, changes := newObserveStore(t)
	assertNoChange(t, changes)
}

// TestCreateUpdateDeleteFlowThroughInformer — add/update/delete가 UID 우선
// 신원·resourceVersion과 함께 나오고, 워크로드 신원은 owner 체인에서 빌려옵니다.
func TestCreateUpdateDeleteFlowThroughInformer(t *testing.T) {
	_, typed, changes := newObserveStore(t)
	ctx := context.Background()
	pods := typed.CoreV1().Pods("payments")

	created := observePod("payments", "checkout-7f-bbb", "pod-bbb", "2")
	if _, err := pods.Create(ctx, created, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c := waitChange(t, changes)
	if c.Kind != ChangePod || c.Action != ChangeAdded {
		t.Fatalf("added 기대: %+v", c)
	}
	if c.Entity.PodUID != "pod-bbb" || c.Namespace != "payments" || c.ResourceVersion != "2" {
		t.Fatalf("신원이 틀렸습니다: %+v", c)
	}
	// 워크로드 신원은 ReplicaSet이 아니라 소유 Deployment로 올려서 실립니다.
	if c.Entity.WorkloadKind != "Deployment" || c.Entity.WorkloadUID != "dep-checkout" {
		t.Fatalf("워크로드 신원: %+v", c.Entity)
	}

	updated := created.DeepCopy()
	updated.ResourceVersion = "3"
	if _, err := pods.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	c = waitChange(t, changes)
	if c.Action != ChangeUpdated || c.ResourceVersion != "3" {
		t.Fatalf("updated 기대: %+v", c)
	}

	if err := pods.Delete(ctx, created.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	c = waitChange(t, changes)
	if c.Action != ChangeDeleted || c.Entity.PodUID != "pod-bbb" {
		t.Fatalf("deleted 기대: %+v", c)
	}
}

/* ── 억제 규칙 (informer 없이 직접) ─────────────────────────────────────── */

func collectHandlers() (cache.ResourceEventHandlerDetailedFuncs, *[]Change) {
	var got []Change
	h := changeHandlers(
		func(obj any) (Change, bool) {
			p, ok := obj.(*corev1.Pod)
			if !ok {
				return Change{}, false
			}
			return Change{Kind: ChangePod, Namespace: p.Namespace, ResourceVersion: p.ResourceVersion}, true
		},
		func(c Change) { got = append(got, c) },
	)
	return h, &got
}

func TestChangeHandlersSuppressInitialListAdd(t *testing.T) {
	h, got := collectHandlers()
	p := observePod("payments", "a", "u", "1")
	h.AddFunc(p, true) // 최초 LIST — 억제
	if len(*got) != 0 {
		t.Fatalf("초기 목록이 변경으로 나왔습니다: %+v", *got)
	}
	h.AddFunc(p, false)
	if len(*got) != 1 || (*got)[0].Action != ChangeAdded {
		t.Fatalf("added 기대: %+v", *got)
	}
}

func TestChangeHandlersSuppressResyncNoOp(t *testing.T) {
	h, got := collectHandlers()
	oldPod := observePod("payments", "a", "u", "7")
	samePod := observePod("payments", "a", "u", "7")
	h.UpdateFunc(oldPod, samePod) // 같은 RV — resync no-op
	if len(*got) != 0 {
		t.Fatalf("resync no-op이 변경으로 나왔습니다: %+v", *got)
	}
	newPod := observePod("payments", "a", "u", "8")
	h.UpdateFunc(oldPod, newPod)
	if len(*got) != 1 || (*got)[0].Action != ChangeUpdated || (*got)[0].ResourceVersion != "8" {
		t.Fatalf("updated 기대: %+v", *got)
	}
}

func TestChangeHandlersUnwrapTombstone(t *testing.T) {
	h, got := collectHandlers()
	p := observePod("payments", "a", "u", "9")
	h.DeleteFunc(cache.DeletedFinalStateUnknown{Key: "payments/a", Obj: p})
	if len(*got) != 1 || (*got)[0].Action != ChangeDeleted || (*got)[0].Namespace != "payments" {
		t.Fatalf("tombstone에서 deleted 기대: %+v", *got)
	}
	// 변환 불가능한 tombstone은 조용히 무시합니다 — 패닉하지 않습니다.
	h.DeleteFunc(cache.DeletedFinalStateUnknown{Key: "x", Obj: "쓰레기"})
	if len(*got) != 1 {
		t.Fatalf("변환 불가 객체가 변경으로 나왔습니다: %+v", *got)
	}
}

/* ── 변환 규칙 ──────────────────────────────────────────────────────────── */

func TestWorkloadChangeCarriesUIDFirstIdentity(t *testing.T) {
	conv := workloadChange("prod-seoul", "Deployment")
	dep := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name: "checkout", Namespace: "payments", UID: "dep-checkout", ResourceVersion: "42",
	}}
	c, ok := conv(dep)
	if !ok {
		t.Fatal("변환 실패")
	}
	e := c.Entity
	if c.Kind != ChangeWorkload || e.WorkloadKind != "Deployment" ||
		e.WorkloadName != "checkout" || e.WorkloadUID != "dep-checkout" ||
		e.Namespace != "payments" || e.ClusterID != "prod-seoul" || c.ResourceVersion != "42" {
		t.Fatalf("워크로드 변환: %+v", c)
	}
	if _, ok := conv("쓰레기"); ok {
		t.Fatal("비객체가 변환되었습니다")
	}
}

func TestEventChangeMapsInvolvedObject(t *testing.T) {
	store, _, _ := newObserveStore(t)

	podEvent := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "payments", ResourceVersion: "5"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "checkout-7f-aaa", UID: "pod-aaa"},
		Type:           corev1.EventTypeWarning, Reason: "BackOff",
		Message: "Back-off restarting failed container",
	}
	c, ok := store.eventChange(podEvent)
	if !ok || c.Kind != ChangeKubeEvent || c.Entity.PodUID != "pod-aaa" || c.Namespace != "payments" {
		t.Fatalf("Pod event 변환: %+v", c)
	}

	depEvent := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "payments", ResourceVersion: "6"},
		InvolvedObject: corev1.ObjectReference{Kind: "Deployment", Name: "checkout", UID: "dep-checkout"},
		Type:           corev1.EventTypeWarning, Reason: "FailedCreate",
	}
	c, ok = store.eventChange(depEvent)
	if !ok || c.Entity.WorkloadKind != "Deployment" || c.Entity.WorkloadUID != "dep-checkout" {
		t.Fatalf("Deployment event 변환: %+v", c)
	}
}
