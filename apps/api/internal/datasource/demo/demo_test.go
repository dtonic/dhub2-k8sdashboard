package demo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

func newDemo(t *testing.T) *demo.Source {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, _ := testcluster.NewStore(t, ctx)
	return demo.New(store)
}

func window() datasource.Window {
	return datasource.Window{
		From: testcluster.Now.Add(-time.Hour), To: testcluster.Now, Step: time.Minute,
	}
}

func target(ns string) datasource.Target {
	return datasource.Target{ClusterID: testcluster.ClusterID, Namespace: ns}
}

// TestTrendsAreDeterministic — 데모 값은 결정적입니다. 같은 입력이면 같은
// 출력이라 화면 확인과 스냅숏 비교에 쓸 수 있습니다.
func TestTrendsAreDeterministic(t *testing.T) {
	d := newDemo(t)
	a, err := d.Trends(context.Background(), target("payments"), window(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := d.Trends(context.Background(), target("payments"), window(), nil)

	if len(a) != 5 {
		t.Fatalf("기본 패널 5개(cpu·memory·network·io·restarts): %d", len(a))
	}
	for i := range a {
		if a[i].ID != b[i].ID || len(a[i].Series) != len(b[i].Series) {
			t.Fatal("패널 구조가 결정적이지 않습니다")
		}
		for si := range a[i].Series {
			pa, pb := a[i].Series[si].Points, b[i].Series[si].Points
			if len(pa) == 0 || len(pa) != len(pb) {
				t.Fatalf("%s/%s 포인트 수: %d vs %d", a[i].ID, a[i].Series[si].Key, len(pa), len(pb))
			}
			for pi := range pa {
				if pa[pi] != pb[pi] {
					t.Fatal("값이 결정적이지 않습니다")
				}
			}
		}
	}
	// 알 수 없는 패널 id는 조용히 빠집니다.
	only, _ := d.Trends(context.Background(), target("payments"), window(), []string{"cpu", "nope"})
	if len(only) != 1 || only[0].ID != "cpu" {
		t.Fatalf("패널 필터: %+v", only)
	}
}

// TestUsageCoversEveryCatalogPod — 사용량 스냅숏은 카탈로그의 모든 Pod UID를
// 키로 갖습니다. 화면의 request/limit 옆에 붙는 값입니다.
func TestUsageCoversEveryCatalogPod(t *testing.T) {
	d := newDemo(t)
	usage, err := d.Usage(context.Background(), testcluster.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{testcluster.UIDPodHealthy, testcluster.UIDPodCrashLoop, testcluster.UIDPodMedia} {
		u, ok := usage[uid]
		if !ok || u.CPUMilli <= 0 || u.MemoryMib <= 0 {
			t.Fatalf("%s 사용량이 없습니다: %+v", uid, u)
		}
	}
}

// TestSearchFiltersAndCursor — 레벨·Pod·검색어 필터와 커서 전진,
// 잘못된 커서 거절을 확인합니다.
func TestSearchFiltersAndCursor(t *testing.T) {
	d := newDemo(t)
	ctx := context.Background()

	q := datasource.LogQuery{Target: target("payments"), Window: window(), PageSize: 50}
	page1, err := d.Search(ctx, q)
	if err != nil || len(page1.Lines) != 50 || page1.Next == "" {
		t.Fatalf("1페이지: %d줄 next=%q err=%v", len(page1.Lines), page1.Next, err)
	}
	q.Cursor = page1.Next
	page2, err := d.Search(ctx, q)
	if err != nil || len(page2.Lines) == 0 {
		t.Fatal("2페이지가 비었습니다")
	}
	if page1.Lines[0].ID == page2.Lines[0].ID {
		t.Fatal("커서가 전진하지 않았습니다")
	}

	// 잘못된 커서는 거절합니다 — 조용히 처음부터 다시 주면 중복을 정상처럼 봅니다.
	q.Cursor = "not-base64!!!"
	if _, err := d.Search(ctx, q); err == nil {
		t.Fatal("깨진 커서가 통과했습니다")
	}

	// 레벨 필터
	q = datasource.LogQuery{Target: target("payments"), Window: window(), PageSize: 30,
		Levels: []contract.LogLevel{contract.LevelError}}
	errPage, _ := d.Search(ctx, q)
	for _, l := range errPage.Lines {
		if l.Level != contract.LevelError {
			t.Fatalf("레벨 필터가 깨졌습니다: %s", l.Level)
		}
	}

	// Pod UID 필터 — 신원은 카탈로그에서 빌린 UID입니다.
	q = datasource.LogQuery{Target: datasource.Target{
		ClusterID: testcluster.ClusterID, Namespace: "payments", PodUID: testcluster.UIDPodCrashLoop,
	}, Window: window(), PageSize: 30}
	podPage, _ := d.Search(ctx, q)
	if len(podPage.Lines) == 0 {
		t.Fatal("Pod 필터 결과가 비었습니다")
	}
	for _, l := range podPage.Lines {
		if l.PodUID != testcluster.UIDPodCrashLoop {
			t.Fatalf("다른 Pod의 로그가 나왔습니다: %s", l.PodUID)
		}
	}

	// 검색어 필터
	q = datasource.LogQuery{Target: target("payments"), Window: window(), PageSize: 30, Text: "GET"}
	textPage, _ := d.Search(ctx, q)
	for _, l := range textPage.Lines {
		if !strings.Contains(strings.ToLower(l.Message), "get") {
			t.Fatalf("검색어 필터가 깨졌습니다: %s", l.Message)
		}
	}
}

// TestLogsAreMaskedInDemoToo — 데모 로그에도 마스킹이 적용됩니다.
// 데모 메시지에 일부러 토큰·키가 섞여 있어 마스킹 UI를 확인할 수 있습니다.
func TestLogsAreMaskedInDemoToo(t *testing.T) {
	d := newDemo(t)
	q := datasource.LogQuery{Target: target("payments"), Window: window(), PageSize: 500}
	page, err := d.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	masked := 0
	for _, l := range page.Lines {
		if strings.Contains(l.Message, "sk-live-") || strings.Contains(l.Message, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9") {
			t.Fatalf("원문 시크릿이 나갔습니다: %s", l.Message)
		}
		masked += len(l.Masked)
	}
	if masked == 0 {
		t.Fatal("마스킹된 구간이 하나도 없습니다 — 데모 데이터가 마스킹 UI를 보여주지 못합니다")
	}
}

// TestHistogramAndFacets — 히스토그램 버킷과 facet의 신원 규칙을 확인합니다.
func TestHistogramAndFacets(t *testing.T) {
	d := newDemo(t)
	ctx := context.Background()
	q := datasource.LogQuery{Target: target("payments"), Window: window()}

	buckets, err := d.Histogram(ctx, q)
	if err != nil || len(buckets) == 0 {
		t.Fatalf("히스토그램: %d버킷 err=%v", len(buckets), err)
	}
	total := 0
	for _, b := range buckets {
		for _, c := range b.Counts {
			total += c
		}
	}
	if total == 0 {
		t.Fatal("히스토그램이 전부 0입니다")
	}

	facets, err := d.Facets(ctx, q)
	if err != nil || len(facets.Pods) == 0 || len(facets.Workloads) == 0 {
		t.Fatalf("facets: %+v err=%v", facets, err)
	}
	for _, p := range facets.Pods {
		if p.UID == "" {
			t.Fatalf("facet Pod %s의 UID가 비었습니다 — 신원은 카탈로그에서 빌려야 합니다", p.Name)
		}
	}
}

// TestAlertsSplitFiringAndResolved — 두 탭이 같은 형식인지 확인할 수 있도록
// firing과 resolved가 모두 만들어지고, 그룹 기준이 노출됩니다.
func TestAlertsSplitFiringAndResolved(t *testing.T) {
	d := newDemo(t)
	res, err := d.List(context.Background(), datasource.AlertQuery{Target: target("payments"), Window: window()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Firing) == 0 || len(res.Resolved) == 0 {
		t.Fatalf("firing %d · resolved %d — 둘 다 있어야 두 탭을 확인할 수 있습니다", len(res.Firing), len(res.Resolved))
	}
	if res.GroupingRule == "" {
		t.Fatal("그룹 기준이 비어 있습니다 — 화면에 노출되어야 합니다")
	}
	for _, a := range res.Resolved {
		if a.EndsAt == "" || a.Status != "resolved" {
			t.Fatalf("resolved 형식: %+v", a)
		}
	}
	for _, a := range res.Firing {
		if a.Entity == nil || a.Entity.PodUID == "" {
			t.Fatalf("알림에 Unified Entity 참조가 없습니다: %+v", a)
		}
	}
}

// TestTopologyGraphRules — 방향별 별도 엣지(A→B와 B→A), 라우트 정렬,
// 엣지 시계열을 확인합니다. (CLAUDE.md 토폴로지 규칙)
func TestTopologyGraphRules(t *testing.T) {
	d := newDemo(t)
	ctx := context.Background()
	g, err := d.Graph(ctx, target(""), window())
	if err != nil || len(g.Nodes) == 0 || len(g.Edges) == 0 {
		t.Fatalf("그래프: 노드 %d 엣지 %d err=%v", len(g.Nodes), len(g.Edges), err)
	}

	// A→B와 B→A는 별도의 엣지입니다.
	seen := map[string]bool{}
	for _, e := range g.Edges {
		if e.From == e.To {
			t.Fatal("자기 자신으로 가는 엣지가 있습니다")
		}
		key := e.From + "->" + e.To
		if seen[key] {
			t.Fatalf("중복 엣지: %s", key)
		}
		seen[key] = true
	}
	pair := 0
	for _, e := range g.Edges {
		if seen[e.To+"->"+e.From] {
			pair++
		}
	}
	if pair == 0 {
		t.Fatal("양방향이 별도 엣지로 존재하지 않습니다")
	}

	series, err := d.EdgeSeries(ctx, testcluster.ClusterID, g.Edges[0].ID, window())
	if err != nil || len(series) != 2 {
		t.Fatalf("엣지 시계열: %d err=%v", len(series), err)
	}
}

type staticPodCatalog []datasource.CatalogPod

func (c staticPodCatalog) CatalogPods(clusterID, namespace string, limit int) []datasource.CatalogPod {
	if clusterID != testcluster.ClusterID {
		return nil
	}
	out := make([]datasource.CatalogPod, 0, len(c))
	for _, pod := range c {
		if namespace != "" && pod.Namespace != namespace {
			continue
		}
		out = append(out, pod)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

func largeTopologyCatalog(workloads, podsPerWorkload int) staticPodCatalog {
	pods := make(staticPodCatalog, 0, workloads*podsPerWorkload)
	for workload := 0; workload < workloads; workload++ {
		for pod := 0; pod < podsPerWorkload; pod++ {
			pods = append(pods, datasource.CatalogPod{
				Namespace:    fmt.Sprintf("ns-%02d", workload%10),
				Name:         fmt.Sprintf("workload-%03d-pod-%d", workload, pod),
				UID:          fmt.Sprintf("pod-uid-%03d-%d", workload, pod),
				WorkloadKind: "Deployment",
				WorkloadName: fmt.Sprintf("workload-%03d", workload),
				WorkloadUID:  fmt.Sprintf("workload-uid-%03d", workload),
				Node:         fmt.Sprintf("node-%02d", workload%25),
			})
		}
	}
	return pods
}

// TestTopologyGraphDoesNotTruncateWorkloads — 전체 scope의 워크로드가 20개를
// 넘어도 알파벳 앞쪽 namespace만 남기지 않고, Pod 수를 접어서 모두 표시합니다. (#3)
func TestTopologyGraphDoesNotTruncateWorkloads(t *testing.T) {
	const (
		workloads       = 500
		podsPerWorkload = 2
	)
	d := demo.New(largeTopologyCatalog(workloads, podsPerWorkload))
	g, err := d.Graph(context.Background(), target(""), window())
	if err != nil {
		t.Fatal(err)
	}

	internal := 0
	foldedPods := 0
	maxColumn := 0
	namespaces := map[string]int{}
	workloadNames := map[string]bool{}
	for _, node := range g.Nodes {
		if node.External {
			continue
		}
		internal++
		foldedPods += node.PodCount
		namespaces[node.Namespace]++
		workloadNames[node.Ref.WorkloadName] = true
		if node.Column > maxColumn {
			maxColumn = node.Column
		}
	}
	if internal != workloads || len(workloadNames) != workloads {
		t.Fatalf("워크로드 노드가 절단됐습니다: nodes=%d unique=%d", internal, len(workloadNames))
	}
	if foldedPods != workloads*podsPerWorkload {
		t.Fatalf("접힌 Pod 합계: got=%d want=%d", foldedPods, workloads*podsPerWorkload)
	}
	if len(g.Nodes) != workloads+3 {
		t.Fatalf("외부 노드를 포함한 전체 노드: got=%d want=%d", len(g.Nodes), workloads+3)
	}
	for namespace := 0; namespace < 10; namespace++ {
		name := fmt.Sprintf("ns-%02d", namespace)
		if namespaces[name] != workloads/10 {
			t.Fatalf("namespace %s 워크로드 수: got=%d want=%d", name, namespaces[name], workloads/10)
		}
	}
	if maxColumn != 23 {
		t.Fatalf("대규모 동적 열 배치: max column=%d want=23", maxColumn)
	}
}

func BenchmarkTopologyGraph500Workloads(b *testing.B) {
	d := demo.New(largeTopologyCatalog(500, 2))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := d.Graph(context.Background(), target(""), window()); err != nil {
			b.Fatal(err)
		}
	}
}

// TestTopologyGraphEmptyNamespaceHasArraysNotNull — Pod가 없는 namespace의 그래프는
// nil 슬라이스가 아니라 빈 배열이어야 합니다. JSON null이 나가면 UI 렌더가 깨집니다. (#16)
func TestTopologyGraphEmptyNamespaceHasArraysNotNull(t *testing.T) {
	d := newDemo(t)
	g, err := d.Graph(context.Background(), target("no-pods-here"), window())
	if err != nil {
		t.Fatal(err)
	}
	if g.Nodes == nil || g.Edges == nil {
		t.Fatalf("nil 슬라이스가 반환되었습니다: nodes=%v edges=%v", g.Nodes == nil, g.Edges == nil)
	}
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("JSON에 null이 있습니다: %s", raw)
	}
}
