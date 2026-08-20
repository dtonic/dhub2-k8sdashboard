package clusterstate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestQueriesNeverCallTheAPIServer(t *testing.T) {
	// ADR 0004의 1번 규칙입니다. **요청 처리 경로에서 Kubernetes API를 호출하지 않습니다.**
	// 여기가 깨지면 증상은 "대시보드가 느리다"가 아니라 "대시보드가 클러스터를 흔든다"입니다.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, fakes := testcluster.NewStore(t, ctx)

	before := len(fakes.Typed.Actions())
	beforeMeta := len(fakes.Metadata.Actions())

	all := clusterstate.NamespaceFilter{All: true}
	if _, err := store.NodeHealth(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PodHealth(all); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WorkloadHealth(all); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Unhealthy(all, 20); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NamespaceSummaries(all); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Events(all, testcluster.Now.Add(-time.Hour), 50); err != nil {
		t.Fatal(err)
	}
	wl, found, err := store.Workload("payments", "Deployment", "payments-api")
	if err != nil || !found {
		t.Fatalf("워크로드를 찾지 못했습니다: %v", err)
	}
	if _, err := store.PodsForWorkload("payments", "Deployment", "payments-api", wl.Ref.WorkloadUID); err != nil {
		t.Fatal(err)
	}
	store.WorkloadOwnerChain("payments", "Deployment", "payments-api", wl.Ref.WorkloadUID)
	store.CatalogPods(testcluster.ClusterID, "", 0)
	store.NamespaceNames(testcluster.ClusterID)

	if got := len(fakes.Typed.Actions()) - before; got != 0 {
		t.Errorf("조회 중 API 서버 호출 %d회 발생 (want 0): %v", got, fakes.Typed.Actions()[before:])
	}
	if got := len(fakes.Metadata.Actions()) - beforeMeta; got != 0 {
		t.Errorf("조회 중 metadata API 호출 %d회 발생 (want 0)", got)
	}
}

func TestNamespaceCatalogIncludesEmptyNamespaceAndIsSorted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _, err := testcluster.Build(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "empty", UID: types.UID("namespace-empty")}})
	if err != nil {
		t.Fatal(err)
	}

	names := store.NamespaceNames(testcluster.ClusterID)
	if got, want := strings.Join(names, ","), "empty,media,payments"; got != want {
		t.Fatalf("namespace catalog=%q, want %q", got, want)
	}
	if got := store.NamespaceNames("another-cluster"); got != nil {
		t.Fatalf("another cluster leaked namespaces: %v", got)
	}
	projection, err := store.SafeProjection(100_000, true)
	if err != nil {
		t.Fatal(err)
	}
	foundProjectedEmpty := false
	for _, resource := range projection {
		foundProjectedEmpty = foundProjectedEmpty || resource.Kind == v1.KindNamespace && resource.Name == "empty"
	}
	if !foundProjectedEmpty {
		t.Fatal("empty namespace missing from central projection")
	}
	legacyProjection, err := store.SafeProjection(100_000, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range legacyProjection {
		if resource.Kind == v1.KindNamespace {
			t.Fatal("namespace resource emitted without negotiated capability")
		}
	}

	summaries, err := store.NamespaceSummaries(clusterstate.NamespaceFilter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range summaries {
		if summary.Name == "empty" {
			if summary.Pods.Total != 0 || summary.Workloads.Total != 0 || summary.Issues == nil {
				t.Fatalf("empty namespace summary=%+v", summary)
			}
			return
		}
	}
	t.Fatal("empty namespace missing from summaries")
}

func TestInitialSyncUsesWatchNotPolling(t *testing.T) {
	// 최초 LIST 1회 + watch 1회가 리소스당 전부여야 합니다.
	// 폴링이 섞이면 사용자가 없어도 상수 부하가 생깁니다.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, fakes := testcluster.NewStore(t, ctx)

	lists, watches := map[string]int{}, map[string]int{}
	for _, a := range fakes.Typed.Actions() {
		switch a.GetVerb() {
		case "list":
			lists[a.GetResource().Resource]++
		case "watch":
			watches[a.GetResource().Resource]++
		default:
			t.Errorf("예상하지 못한 동작: %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
	for res, n := range lists {
		if n != 1 {
			t.Errorf("%s: LIST %d회, want 1회", res, n)
		}
		if watches[res] != 1 {
			t.Errorf("%s: WATCH %d회, want 1회", res, watches[res])
		}
	}
	if len(lists) == 0 {
		t.Fatal("LIST가 한 번도 없었습니다")
	}
}

func TestPodIdentityIsUIDNotName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	pod, found, err := store.Pod("payments", "payments-api-7f-bbb", testcluster.UIDPodCrashLoop)
	if err != nil || !found {
		t.Fatalf("UID로 Pod를 찾지 못했습니다: %v", err)
	}
	if string(pod.UID) != testcluster.UIDPodCrashLoop {
		t.Fatalf("uid=%s", pod.UID)
	}

	// 존재하지 않는 UID는 이름이 맞아도 찾히면 안 됩니다.
	// 이름만 보면 재생성된 Pod를 같은 인스턴스로 취급하게 됩니다.
	if _, found, _ := store.Pod("payments", "payments-api-7f-bbb", "uid-of-a-deleted-instance"); found {
		t.Error("다른 UID인데 이름만으로 매칭되었습니다")
	}
}

func TestOwnerChainMarksCurrentGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	chain := store.WorkloadOwnerChain("payments", "Deployment", "payments-api", testcluster.UIDDeploymentPaymentsAPI)
	if len(chain) != 2 {
		t.Fatalf("체인 길이=%d, want 2 (%+v)", len(chain), chain)
	}
	if !chain[0].Current || chain[1].Current {
		t.Errorf("현재 세대 표시가 잘못되었습니다: %+v", chain)
	}
	if chain[0].UID != testcluster.UIDReplicaSetCurrent {
		t.Errorf("현재 세대=%s, want %s", chain[0].UID, testcluster.UIDReplicaSetCurrent)
	}
	if chain[0].Revision != "2" || chain[1].Revision != "1" {
		t.Errorf("revision이 세대 순서와 다릅니다: %+v", chain)
	}
	if chain[0].Pods != 2 || chain[1].Pods != 1 {
		t.Errorf("세대별 Pod 수=%d/%d, want 2/1", chain[0].Pods, chain[1].Pods)
	}
}

func TestDeploymentPodsAreFoundThroughReplicaSet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	pods, err := store.PodsForWorkload("payments", "Deployment", "payments-api", testcluster.UIDDeploymentPaymentsAPI)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 3 {
		t.Fatalf("Pod 수=%d, want 3", len(pods))
	}
	// 가장 나쁜 Pod가 맨 위에 있어야 합니다.
	if pods[0].Severity != contract.SeverityCritical {
		t.Errorf("정렬이 심각도 순이 아닙니다: %s", pods[0].Severity)
	}
	for _, p := range pods {
		if p.Ref.WorkloadName != "payments-api" || p.Ref.WorkloadKind != "Deployment" {
			t.Errorf("ReplicaSet이 화면에 그대로 노출됩니다: %+v", p.Ref)
		}
	}
}

func TestNamespaceFilterKeepsOtherNamespacesOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	f := clusterstate.NamespaceFilter{List: []string{"payments"}}
	summaries, err := store.NamespaceSummaries(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Name != "payments" {
		t.Fatalf("범위 밖 namespace가 섞였습니다: %+v", summaries)
	}

	evs, err := store.Events(f, testcluster.Now.Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Namespace != "payments" {
			t.Errorf("범위 밖 이벤트가 나왔습니다: %+v", e)
		}
	}
}

// TestNodeSummariesGroupPodsByNode — Nodes 화면 집계: 노드가 심각도순으로 정렬되고,
// Pod가 스케줄된 노드로 묶이며(신원은 UID), Pod 없는 노드도 pods가 null이 아닌
// 빈 배열이어야 합니다(계약은 배열 — null이면 UI가 깨집니다).
func TestNodeSummariesGroupPodsByNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	nodes, err := store.NodeSummaries()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("fixture 노드는 2개여야 합니다: %d", len(nodes))
	}
	if nodes[0].Name != "node-2" || !nodes[0].Ready == false {
		// node-2는 NotReady(critical)라 심각도 정렬에서 먼저 와야 합니다.
		if nodes[0].Name != "node-2" {
			t.Fatalf("심각도 정렬이 틀렸습니다: 첫 노드 %q", nodes[0].Name)
		}
	}
	byName := map[string]int{}
	for i, n := range nodes {
		byName[n.Name] = i
		if n.Pods == nil {
			t.Errorf("노드 %q의 pods가 nil입니다 — JSON null로 마샬됩니다", n.Name)
		}
		if n.PodsTotal != len(n.Pods) {
			t.Errorf("노드 %q podsTotal(%d) != len(pods)(%d)", n.Name, n.PodsTotal, len(n.Pods))
		}
	}
	n1 := nodes[byName["node-1"]]
	if len(n1.Pods) == 0 {
		t.Fatal("node-1에 스케줄된 fixture Pod가 보여야 합니다")
	}
	for _, p := range n1.Pods {
		if p.UID == "" {
			t.Errorf("pod %s/%s의 UID가 비었습니다 — 신원은 UID여야 합니다", p.Namespace, p.Name)
		}
	}
	n2 := nodes[byName["node-2"]]
	if n2.Severity != contract.SeverityCritical || n2.Ready {
		t.Fatalf("NotReady 노드는 critical이어야 합니다: %+v", n2.Severity)
	}
}

// TestNamespaceIssuesAreNeverNil — 문제 없는 namespace의 issues가 nil로 남으면
// JSON null이 되어(계약은 배열) UI의 필터·렌더가 깨집니다. 항상 배열이어야 합니다.
func TestNamespaceIssuesAreNeverNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	summaries, err := store.NamespaceSummaries(clusterstate.NamespaceFilter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) == 0 {
		t.Fatal("fixture namespace가 없습니다")
	}
	for _, s := range summaries {
		if s.Issues == nil {
			t.Errorf("namespace %q의 issues가 nil입니다 — JSON null로 마샬됩니다", s.Name)
		}
	}
}

func TestUnhealthyIsSortedBySeverityThenDuration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	list, err := store.Unhealthy(clusterstate.NamespaceFilter{All: true}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("이상 엔티티가 하나도 없습니다")
	}
	if list[0].Severity != contract.SeverityCritical {
		t.Errorf("맨 위가 critical이 아닙니다: %+v", list[0])
	}
	prev := 99
	for _, u := range list {
		r := rankOf(u.Severity)
		if r > prev {
			t.Fatalf("심각도 순서가 깨졌습니다: %+v", list)
		}
		prev = r
	}
}

func TestEventsForUIDUsesIndex(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	evs, err := store.EventsForUID(testcluster.UIDPodCrashLoop, testcluster.Now.Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("이벤트 수=%d, want 1", len(evs))
	}
	if evs[0].Involved.PodUID != testcluster.UIDPodCrashLoop {
		t.Errorf("다른 객체의 이벤트가 왔습니다: %+v", evs[0])
	}
}

func TestCatalogPodsBorrowRealIdentity(t *testing.T) {
	// 데이터소스가 Pod 이름·UID를 지어내면 딥링크가 404가 됩니다.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	pods := store.CatalogPods(testcluster.ClusterID, "payments", 0)
	if len(pods) != 4 {
		t.Fatalf("Pod 수=%d, want 4", len(pods))
	}
	for _, p := range pods {
		if p.UID == "" || p.Name == "" {
			t.Fatalf("신원이 비어 있습니다: %+v", p)
		}
		if _, found, _ := store.Pod(p.Namespace, p.Name, p.UID); !found {
			t.Errorf("카탈로그의 Pod를 캐시에서 찾을 수 없습니다: %+v", p)
		}
	}
}

func rankOf(s contract.Severity) int {
	switch s {
	case contract.SeverityCritical:
		return 5
	case contract.SeverityDegraded:
		return 4
	case contract.SeverityWarning:
		return 3
	case contract.SeverityProgressing:
		return 2
	case contract.SeverityUnknown:
		return 1
	}
	return 0
}
