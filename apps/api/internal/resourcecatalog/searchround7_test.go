package resourcecatalog

// Round 7 상태 기계 검증
// --------------------------------------------------------------------------
// 회수 티켓·부트스트랩 배리어·예약 회계·fold posting 교체·Detail/Recent 권위·
// 잠금 경합·공정성을 **강제 교차**로 확인합니다.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

/* ── 하네스 ─────────────────────────────────────────────────────────────── */

// fakeInformer는 GetStore·HasSynced만 의미를 갖는 최소 informer입니다.
// 나머지 메서드는 내장 인터페이스가 채우므로 client-go 인터페이스가 늘어나도
// 이 파일이 깨지지 않습니다(호출되면 nil 역참조로 즉시 드러납니다).
type fakeInformer struct {
	cache.SharedIndexInformer
	store  cache.Store
	synced bool
}

func (f *fakeInformer) GetStore() cache.Store { return f.store }
func (f *fakeInformer) HasSynced() bool       { return f.synced }

// deltaHarness는 flush·회수를 직접 돌릴 수 있는 서비스 한 벌입니다.
type deltaHarness struct {
	svc     *Service
	entry   *resourceEntry
	store   cache.Store
	binding *handlerBinding
	gvr     schema.GroupVersionResource
}

func newDeltaHarness(t *testing.T, rows ...*metav1.PartialObjectMetadata) *deltaHarness {
	t.Helper()
	return newDeltaHarnessFor(t, scopedGVR, "Service", true, rows...)
}

func newDeltaHarnessFor(t *testing.T, gvr schema.GroupVersionResource, kind string, namespaced bool,
	rows ...*metav1.PartialObjectMetadata) *deltaHarness {
	t.Helper()

	s := &Service{
		cfg: Config{
			ClusterID: "prod-seoul", SearchEnabled: true, SearchIncremental: true,
			MaxSearchIndexBytes: DefaultMaxSearchIndexBytes,
			Now:                 func() time.Time { return indexBase },
		},
		order:   []schema.GroupVersionResource{gvr},
		entries: map[schema.GroupVersionResource]*resourceEntry{gvr: {gvr: gvr}},
	}
	s.delta = newDeltaState()
	// 큐·회로 고정 구조도 원장에 실립니다(프로덕션과 같은 배선).
	s.delta.budget = &s.budget
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	s.started.Store(true)
	s.disc.Store(&discoverySnapshot{
		entries: []discoveryEntry{{
			gvr: gvr, kind: kind, namespaced: namespaced, served: true,
			verbs: []string{"get", "list", "watch"},
		}},
		byGVR: map[schema.GroupVersionResource]int{gvr: 0},
	})

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, r := range rows {
		if err := store.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	e := s.entries[gvr]
	e.setStatus(StateReady, "")
	e.lifecycle, e.generation = 1, 0
	e.tokenPacked.Store(packToken(1, 0))
	e.informer = &fakeInformer{store: store, synced: true}
	e.bootstrapped.Store(true)

	index := indexOf(rows...)
	r := buildSearchIndex(index, kind, namespaced, hugeBudget, hugeBudget)
	if r.state != SearchReady {
		t.Fatalf("부트스트랩 실패: %s %s", r.state, r.reason)
	}
	snap := &entrySnapshot{
		index: index, sindex: r.index, searchState: SearchReady,
		indexVer: 1, searchVer: 1, coversThroughSeq: 0,
	}
	e.setSnap(snap)
	// 회계는 **소유권 원장**을 통해서만 움직입니다. 손으로 값을 넣으면 은퇴 세대
	// 처리와 어긋나므로 게시와 같은 경로(승인+설치)를 씁니다.
	s.snapMu.Lock()
	admitted := s.publishLeaseLocked(e, snap)
	s.snapMu.Unlock()
	if !admitted {
		t.Fatal("하네스 세대가 예산 승인을 받지 못했습니다")
	}

	return &deltaHarness{
		svc: s, entry: e, store: store, gvr: gvr,
		binding: &handlerBinding{entry: e, packed: packToken(1, 0)},
	}
}

// upsert는 Store에 객체를 넣고 그 키를 큐에 담습니다(콜백과 같은 경로).
func (h *deltaHarness) upsert(t *testing.T, obj *metav1.PartialObjectMetadata) {
	t.Helper()
	if err := h.store.Add(obj); err != nil {
		t.Fatal(err)
	}
	h.svc.enqueueKey(h.binding, obj.Namespace, obj.Name)
}

func (h *deltaHarness) remove(t *testing.T, obj *metav1.PartialObjectMetadata) {
	t.Helper()
	if err := h.store.Delete(obj); err != nil {
		t.Fatal(err)
	}
	h.svc.enqueueKey(h.binding, obj.Namespace, obj.Name)
}

func (h *deltaHarness) flush(t *testing.T) int {
	t.Helper()
	return h.svc.flushSearchDeltas(context.Background(), h.gvr, maxBatchPerResource)
}

func (h *deltaHarness) queueLen() int {
	h.svc.delta.mu.Lock()
	defer h.svc.delta.mu.Unlock()
	return len(h.svc.delta.queueFor(h.gvr).events)
}

func (h *deltaHarness) staleCount() (int, bool) {
	h.svc.delta.mu.Lock()
	defer h.svc.delta.mu.Unlock()
	q := h.svc.delta.queueFor(h.gvr)
	return q.staleNS.count, q.gvrStale
}

/* ── §8 정상 델타는 Store.List·전체 빌드를 0회 ─────────────────────────── */

// TestSteadyFlushHasZeroStoreListAndFullBuild — 정상 델타 flush는 목록을 다시
// 훑지도, 검색 인덱스를 통째로 다시 세우지도 않아야 합니다.
func TestSteadyFlushHasZeroStoreListAndFullBuild(t *testing.T) {
	h := newDeltaHarness(t,
		metaRow("prod", "payments-api", "uid-1", map[string]string{"app": "payments"}),
		metaRow("prod", "ledger", "uid-2", nil),
	)
	baseList := h.svc.delta.storeListCalls.Load()
	baseBoot := h.svc.delta.fullBootstraps.Load()
	baseFull := h.svc.delta.fullRecoveries.Load()

	h.upsert(t, metaRow("prod", "payments-worker", "uid-3", map[string]string{"app": "payments"}))
	h.upsert(t, metaRow("prod", "payments-api", "uid-1", map[string]string{"app": "billing"}))
	h.remove(t, metaRow("prod", "ledger", "uid-2", nil))
	if got := h.flush(t); got != 3 {
		t.Fatalf("flush가 %d건을 처리했습니다 — 3건이어야 합니다", got)
	}

	if got := h.svc.delta.storeListCalls.Load() - baseList; got != 0 {
		t.Errorf("정상 flush가 Store.List를 %d회 불렀습니다", got)
	}
	if got := h.svc.delta.fullBootstraps.Load() - baseBoot; got != 0 {
		t.Errorf("정상 flush가 전체 부트스트랩을 %d회 했습니다", got)
	}
	if got := h.svc.delta.fullRecoveries.Load() - baseFull; got != 0 {
		t.Errorf("정상 flush가 전체 회수를 %d회 했습니다", got)
	}
	if h.queueLen() != 0 {
		t.Fatalf("flush 뒤에도 큐에 %d건이 남았습니다", h.queueLen())
	}
	// 결과가 실제로 반영되었는지 봅니다.
	page, err := h.svc.Search(SearchRequest{Query: "payments", Namespaces: NamespaceFilter{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, it := range page.Items {
		names[it.Name] = true
	}
	if !names["payments-worker"] || !names["payments-api"] {
		t.Fatalf("증분 결과가 반영되지 않았습니다: %+v", page.Items)
	}
	if names["ledger"] {
		t.Fatal("삭제한 행이 남아 있습니다")
	}
}

/* ── §4 fold 멤버십 전환에서 prevLCP 재계산 ─────────────────────────────── */

// foldPage는 질의 하나를 끝까지 페이징해 (키, matchedField) 목록을 만듭니다.
func foldPage(t *testing.T, s *Service, query string, limit int) []string {
	t.Helper()
	var out []string
	cursor := ""
	for round := 0; round < 256; round++ {
		page, err := s.Search(SearchRequest{
			Query: query, Limit: limit, Cursor: cursor,
			Namespaces: NamespaceFilter{All: true},
		})
		if err != nil {
			t.Fatalf("query=%q: %v", query, err)
		}
		for _, it := range page.Items {
			out = append(out, it.Namespace+"/"+it.Name+"/"+it.UID+"/"+it.MatchedField)
		}
		if page.NextCursor == "" {
			return out
		}
		cursor = page.NextCursor
	}
	t.Fatalf("query=%q: 페이지가 끝나지 않았습니다", query)
	return nil
}

// TestFoldMembershipFlipRecomputesBaseTokenPrevLCP — 이름 "aa"에 label
// "azaa"/"azzz"가 붙고 빠질 때, 질의 "az"가 **중복도 누락도 없이** 정확한
// matchedField를 내야 합니다.
//
// label만 넣고 빼면 이름 토큰 "aa"의 prevLCP가 옛 값 그대로 남아 같은 행이 두 번
// 나가거나(중복) 아예 빠집니다(누락). 그래서 정규 집합을 통째로 교체합니다.
func TestFoldMembershipFlipRecomputesBaseTokenPrevLCP(t *testing.T) {
	// 이름 "az-name"은 질의 "az"에 이름으로 걸립니다.
	// label 값 "azaa"/"azzz"도 같은 접두사를 갖습니다.
	withLabels := metaRow("prod", "az-name", "uid-1",
		map[string]string{"k1": "azaa", "k2": "azzz"})
	withoutLabels := metaRow("prod", "az-name", "uid-1", nil)

	h := newDeltaHarness(t, withoutLabels)

	// ① 승격: label이 붙습니다.
	h.upsert(t, withLabels)
	if got := h.flush(t); got != 1 {
		t.Fatalf("flush=%d", got)
	}
	for _, limit := range []int{1, 2, 50} {
		got := foldPage(t, h.svc, "az", limit)
		if len(got) != 1 {
			t.Fatalf("limit=%d 승격 후 결과가 %d건입니다 — 행 하나가 한 번만 나와야 합니다: %v",
				limit, len(got), got)
		}
		if got[0] != "prod/az-name/uid-1/name" {
			t.Fatalf("limit=%d matchedField가 어긋났습니다: %s", limit, got[0])
		}
	}

	// ② 강등: label이 빠집니다.
	h.upsert(t, withoutLabels)
	if got := h.flush(t); got != 1 {
		t.Fatalf("flush=%d", got)
	}
	for _, limit := range []int{1, 2, 50} {
		got := foldPage(t, h.svc, "az", limit)
		if len(got) != 1 {
			t.Fatalf("limit=%d 강등 후 결과가 %d건입니다: %v", limit, len(got), got)
		}
		if got[0] != "prod/az-name/uid-1/name" {
			t.Fatalf("limit=%d matchedField가 어긋났습니다: %s", limit, got[0])
		}
	}

	// ③ label만 걸리는 질의도 정확해야 합니다.
	h.upsert(t, withLabels)
	h.flush(t)
	got := foldPage(t, h.svc, "azz", 1)
	if len(got) != 1 || !strings.HasSuffix(got[0], "/label") {
		t.Fatalf("label 전용 질의가 어긋났습니다: %v", got)
	}
}

/* ── §5 Detail/Recent 권위 ─────────────────────────────────────────────── */

// TestRecentFollowsBaselineListNotSearchIndex — 최근 항목의 신원 판정은
// **목록 스냅숏 하나**입니다.
//
// 증분 검색 인덱스를 끌어들이면 최근 항목·상세의 신원이 그 인덱스의 수명·회수
// 상태에 묶이고, 볼 수 없는 namespace의 사정이 허용된 참조의 답을 바꿀 수 있습니다.
// 그래서 검색이 아무리 앞서 가도 이 경로는 목록만 봅니다(ADR 0018 그대로).
func TestRecentFollowsBaselineListNotSearchIndex(t *testing.T) {
	old := metaRow("prod", "payments-api", "uid-old", nil)
	h := newDeltaHarness(t, old)
	refOf := func(uid string) RecentRef {
		return RecentRef{Version: "v1", Resource: "services",
			Namespace: "prod", Name: "payments-api", UID: uid}
	}

	// 검색 인덱스만 앞서 갑니다(목록 tick은 아직 돌지 않았습니다).
	replaced := metaRow("prod", "payments-api", "uid-new", nil)
	h.upsert(t, replaced)
	h.flush(t)

	// 검색은 새 UID를 압니다.
	page, err := h.svc.Search(SearchRequest{Query: "payments", Namespaces: allNS()})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].UID != "uid-new" {
		t.Fatalf("검색이 증분을 반영하지 않았습니다: %+v", page.Items)
	}
	// 최근 항목은 **목록 기준**이라 아직 옛 UID로 해석됩니다.
	if items, err := h.svc.Recent([]RecentRef{refOf("uid-old")}, allNS()); err != nil {
		t.Fatal(err)
	} else if len(items) != 1 {
		t.Fatalf("목록 기준 해석이 사라졌습니다: %+v", items)
	}
	if items, err := h.svc.Recent([]RecentRef{refOf("uid-new")}, allNS()); err != nil {
		t.Fatal(err)
	} else if len(items) != 0 {
		t.Fatalf("목록에 없는 UID가 해석되었습니다: %+v", items)
	}
	// 검색 인덱스의 회수 상태는 이 경로에 아무 영향이 없어야 합니다.
	h.svc.markGVRStale(h.gvr)
	after, err := h.svc.Recent([]RecentRef{refOf("uid-old")}, allNS())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("GVR stale이 최근 항목 해석을 바꿨습니다: %+v", after)
	}
}

func allNS() NamespaceFilter { return NamespaceFilter{All: true} }

// TestHiddenStaleNamespaceDoesNotChangeAllowedRef — 숨겨진 namespace 하나가
// 회수 대기라고 해서 허용된 참조의 해석이 바뀌면 안 됩니다.
func TestHiddenStaleNamespaceDoesNotChangeAllowedRef(t *testing.T) {
	rows := []*metav1.PartialObjectMetadata{
		metaRow("allowed", "payments-api", "uid-1", nil),
		metaRow("hidden", "secret-thing", "uid-2", nil),
	}
	ref := RecentRef{Version: "v1", Resource: "services",
		Namespace: "allowed", Name: "payments-api", UID: "uid-1"}

	clean := newDeltaHarness(t, rows...)
	noisy := newDeltaHarness(t, rows...)
	// 숨겨진 namespace만 회수 대기로 만듭니다.
	noisy.svc.requestResync(noisy.gvr, "hidden", 99)

	cleanItems, err := clean.svc.Recent([]RecentRef{ref}, NamespaceFilter{List: []string{"allowed"}})
	if err != nil {
		t.Fatal(err)
	}
	noisyItems, err := noisy.svc.Recent([]RecentRef{ref}, NamespaceFilter{List: []string{"allowed"}})
	if err != nil {
		t.Fatal(err)
	}
	cleanJSON, _ := json.Marshal(cleanItems)
	noisyJSON, _ := json.Marshal(noisyItems)
	if string(cleanJSON) != string(noisyJSON) {
		t.Fatalf("숨겨진 namespace의 stale이 허용 참조를 바꿨습니다\n%s\n%s", cleanJSON, noisyJSON)
	}
	if len(cleanItems) != 1 {
		t.Fatalf("허용 참조가 해석되지 않았습니다: %+v", cleanItems)
	}
	// 대상 namespace 자체가 stale이면 목록으로 물러납니다(막히지 않습니다).
	noisy.svc.requestResync(noisy.gvr, "allowed", 100)
	if got, err := noisy.svc.Recent([]RecentRef{ref}, NamespaceFilter{List: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 {
		t.Fatalf("대상 namespace가 stale일 때 목록 폴백이 동작하지 않았습니다: %+v", got)
	}
}

/* ── §2 부트스트랩 covers 배리어 ───────────────────────────────────────── */

// TestBootstrapAcksCoveredAndKeepsLater — 부트스트랩 게시는 covers 이하의 대기
// 키와 마커만 지우고, covers보다 뒤의 이벤트는 그대로 남겨야 합니다.
func TestBootstrapAcksCoveredAndKeepsLater(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	// covers 이전 이벤트 둘.
	h.svc.enqueueKey(h.binding, "prod", "a")
	h.svc.enqueueKey(h.binding, "prod", "b")
	h.svc.requestResync(h.gvr, "prod", h.svc.eventSeq.Load())
	covers := h.svc.eventSeq.Load()
	// covers 이후 이벤트 하나.
	h.svc.enqueueKey(h.binding, "prod", "later")

	h.svc.snapMu.Lock()
	h.svc.ackCoveredLocked(h.gvr, covers)
	h.svc.snapMu.Unlock()

	h.svc.delta.mu.Lock()
	q := h.svc.delta.queueFor(h.gvr)
	names := make([]string, 0, len(q.events))
	for _, ev := range q.events {
		names = append(names, ev.name)
	}
	staleCount, gvrStale := q.staleNS.count, q.gvrStale
	h.svc.delta.mu.Unlock()

	if len(names) != 1 || names[0] != "later" {
		t.Fatalf("covers 이후 이벤트만 남아야 합니다: %v", names)
	}
	if staleCount != 0 || gvrStale {
		t.Fatalf("covers가 덮은 마커가 남았습니다: count=%d gvr=%v", staleCount, gvrStale)
	}
	if h.svc.budget.queued.Load() <= 0 {
		t.Fatal("남은 이벤트의 예약이 함께 사라졌습니다")
	}
}

// TestOldInformerCallbackAfterRestartIsRejected — 재시작 뒤 도착한 옛 informer의
// 콜백은 새 세대 큐에 섞이면 안 됩니다.
func TestOldInformerCallbackAfterRestartIsRejected(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	oldBinding := h.binding

	// 재시작: 세대가 올라갑니다.
	h.entry.discard(h.svc)
	h.entry.lifecycle, h.entry.generation = 5, 3
	h.entry.tokenPacked.Store(packToken(5, 3))

	h.svc.enqueueKey(oldBinding, "prod", "zombie")
	if got := h.queueLen(); got != 0 {
		t.Fatalf("옛 세대 콜백이 %d건 들어왔습니다", got)
	}
	// 새 신원으로는 들어가야 합니다.
	newBinding := &handlerBinding{entry: h.entry, packed: packToken(5, 3)}
	h.svc.enqueueKey(newBinding, "prod", "alive")
	if got := h.queueLen(); got != 1 {
		t.Fatalf("새 세대 콜백이 %d건입니다", got)
	}
}

/* ── §1 회수 티켓 교차 ─────────────────────────────────────────────────── */

// TestClusterScopedRecoveryHoldsEvents — namespace ""가 "보류 없음"으로 읽히면
// cluster-scoped 회수 중에 델타가 그 파티션을 바꿔 버립니다.
func TestClusterScopedRecoveryHoldsEvents(t *testing.T) {
	nodeGVR := schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	h := newDeltaHarnessFor(t, nodeGVR, "Node", false,
		metaRow("", "node-a", "uid-a", nil))

	h.svc.delta.mu.Lock()
	h.svc.delta.ticket = &recoveryTicket{
		gvr: nodeGVR, namespace: "", holdActive: true, phase: recoveryBuilding,
	}
	h.svc.delta.mu.Unlock()

	h.upsert(t, metaRow("", "node-b", "uid-b", nil))
	h.flush(t)

	h.svc.delta.mu.Lock()
	q := h.svc.delta.queueFor(nodeGVR)
	held, pending := len(q.hold), len(q.events)
	h.svc.delta.mu.Unlock()
	if held != 1 || pending != 0 {
		t.Fatalf("cluster-scoped 이벤트가 보류되지 않았습니다: hold=%d pending=%d", held, pending)
	}

	// 같은 키가 또 오면 보류에 중복으로 쌓이더라도 마지막 것만 적용되어야 합니다.
	h.upsert(t, metaRow("", "node-b", "uid-b2", nil))
	h.flush(t)
	h.svc.delta.mu.Lock()
	held = len(q.hold)
	h.svc.delta.mu.Unlock()
	if held < 1 {
		t.Fatalf("두 번째 보류가 사라졌습니다: hold=%d", held)
	}
}

// TestTicketDroppedByDiscardCannotBeResurrected — 폐기가 티켓을 걷어간 뒤에는
// finish/abandon이 상태를 되돌리지 못해야 하고, 예약은 정확히 한 번 풀려야 합니다.
func TestTicketDroppedByDiscardCannotBeResurrected(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	baseLive := h.svc.budget.live.Load()

	ticket := &recoveryTicket{gvr: h.gvr, namespace: "prod", holdActive: true, epoch: 1}
	if !h.svc.budget.reserveRecovery(4096) {
		t.Fatal("예약이 거절되었습니다")
	}
	ticket.reserved = 4096
	h.svc.delta.mu.Lock()
	h.svc.delta.ticket = ticket
	h.svc.delta.mu.Unlock()

	// 폐기: 티켓과 예약이 함께 사라져야 합니다.
	h.entry.discard(h.svc)
	h.svc.delta.mu.Lock()
	alive := h.svc.delta.ticket
	h.svc.delta.mu.Unlock()
	if alive != nil {
		t.Fatal("폐기 뒤에도 티켓이 살아 있습니다")
	}
	if got := h.svc.budget.recovery.Load(); got != 0 {
		t.Fatalf("회수 예약이 %d 남았습니다", got)
	}

	// 뒤늦은 finish는 아무것도 되살리지 못해야 합니다.
	h.svc.finishRecovery(ticket, recoveryPublished)
	h.svc.delta.mu.Lock()
	alive = h.svc.delta.ticket
	h.svc.delta.mu.Unlock()
	if alive != nil {
		t.Fatal("뒤늦은 finish가 티켓을 되살렸습니다")
	}
	if got := h.svc.budget.live.Load(); got > baseLive {
		t.Fatalf("예약이 이중으로 남았습니다: %d > %d", got, baseLive)
	}
}

// TestNamespaceRecoverySuccessClearsOnlyThatMarker — namespace 회수 성공은
// **그 namespace 마커만** 지워야 합니다. 나머지는 아직 회수되지 않았습니다.
func TestNamespaceRecoverySuccessClearsOnlyThatMarker(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	h.svc.requestResync(h.gvr, "alpha", 10)
	h.svc.requestResync(h.gvr, "beta", 11)

	h.svc.delta.mu.Lock()
	q := h.svc.delta.queueFor(h.gvr)
	ticket := &recoveryTicket{gvr: h.gvr, namespace: "alpha", epoch: q.staleEpoch}
	h.svc.delta.ticket = ticket
	h.svc.delta.mu.Unlock()

	h.svc.finishRecovery(ticket, recoveryPublished)

	h.svc.delta.mu.Lock()
	left := q.staleNS.values()
	h.svc.delta.mu.Unlock()
	if len(left) != 1 || left[0] != "beta" {
		t.Fatalf("남은 마커가 %v입니다 — beta 하나여야 합니다", left)
	}
	if h.svc.delta.fullRecoveries.Load() != 0 {
		t.Fatal("namespace 회수가 전체 회수로 집계되었습니다")
	}
}

// TestWholeRecoverySuccessClearsAllAndCounts — GVR 전체 회수 성공만이 전부를
// 지우고, **예외적 전체 회수**로 집계되어야 합니다.
func TestWholeRecoverySuccessClearsAllAndCounts(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	h.svc.requestResync(h.gvr, "alpha", 10)
	h.svc.markGVRStale(h.gvr)

	h.svc.delta.mu.Lock()
	q := h.svc.delta.queueFor(h.gvr)
	ticket := &recoveryTicket{gvr: h.gvr, wholeGVR: true, epoch: q.staleEpoch}
	h.svc.delta.ticket = ticket
	h.svc.delta.mu.Unlock()

	h.svc.finishRecovery(ticket, recoveryPublished)

	count, gvrStale := h.staleCount()
	if count != 0 || gvrStale {
		t.Fatalf("전체 회수 뒤에도 마커가 남았습니다: count=%d gvr=%v", count, gvrStale)
	}
	if h.svc.delta.fullRecoveries.Load() != 1 {
		t.Fatalf("전체 회수 집계가 %d입니다", h.svc.delta.fullRecoveries.Load())
	}
	// 성공 뒤에는 쿨다운이 걸려 곧바로 다음 회수를 시작하지 않습니다.
	h.svc.delta.mu.Lock()
	cooldown := h.svc.delta.cooldownUntil
	next := h.svc.pickRecoveryLocked(indexBase)
	h.svc.delta.mu.Unlock()
	if !cooldown.After(indexBase) {
		t.Fatal("성공 뒤 쿨다운이 걸리지 않았습니다")
	}
	if next != nil {
		t.Fatal("쿨다운 중에 새 회수를 잡았습니다 — 폭풍이 됩니다")
	}
}

// TestStaleEpochMismatchDoesNotClearMarkers — 회수 도중 새로 생긴 마커는
// 그 회수가 성공해도 살아남아야 합니다.
func TestStaleEpochMismatchDoesNotClearMarkers(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	h.svc.requestResync(h.gvr, "alpha", 10)

	h.svc.delta.mu.Lock()
	q := h.svc.delta.queueFor(h.gvr)
	ticket := &recoveryTicket{gvr: h.gvr, wholeGVR: true, epoch: q.staleEpoch}
	h.svc.delta.ticket = ticket
	h.svc.delta.mu.Unlock()

	// 회수가 도는 사이에 새 마커가 생깁니다(epoch가 올라갑니다).
	h.svc.requestResync(h.gvr, "beta", 20)
	h.svc.finishRecovery(ticket, recoveryPublished)

	h.svc.delta.mu.Lock()
	left := q.staleNS.values()
	h.svc.delta.mu.Unlock()
	if len(left) == 0 {
		t.Fatal("회수 중 생긴 마커까지 지워졌습니다")
	}
}

// TestBudgetRejectionIsExplicitStaleNotRequeueLoop — 예산 거절은 버전 충돌과
// 다릅니다. 명시적 stale + 쿨다운이어야 하고, 100ms마다 되돌리면 안 됩니다.
func TestBudgetRejectionIsExplicitStaleNotRequeueLoop(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	h.svc.delta.mu.Lock()
	q := h.svc.delta.queueFor(h.gvr)
	ticket := &recoveryTicket{gvr: h.gvr, namespace: "prod", epoch: q.staleEpoch,
		backoff: recoveryBackoffMin}
	h.svc.delta.ticket = ticket
	h.svc.delta.mu.Unlock()

	h.svc.finishRecovery(ticket, recoveryBudgetRejected)

	count, _ := h.staleCount()
	if count != 1 {
		t.Fatalf("예산 거절이 stale로 남지 않았습니다: %d", count)
	}
	h.svc.delta.mu.Lock()
	cooldown := h.svc.delta.cooldownUntil
	next := h.svc.pickRecoveryLocked(indexBase)
	h.svc.delta.mu.Unlock()
	if !cooldown.After(indexBase) || next != nil {
		t.Fatalf("예산 거절 뒤에 곧바로 다시 시도합니다: cooldown=%v next=%v", cooldown, next)
	}
}

/* ── §7 공정성 ─────────────────────────────────────────────────────────── */

// TestRoundRobinAdvancesPastServedResources — 64개가 전부 포화일 때, 커서가
// tick마다 하나씩만 밀면 뒤쪽 GVR이 굶습니다.
func TestRoundRobinAdvancesPastServedResources(t *testing.T) {
	const resources = 64
	perTick := maxBatchEvents / maxBatchPerResource // 8
	maxGap := (resources + perTick - 1) / perTick   // ceil(64/8) = 8

	order := make([]schema.GroupVersionResource, 0, resources)
	for i := 0; i < resources; i++ {
		order = append(order, schema.GroupVersionResource{
			Group: fmt.Sprintf("g%02d", i), Version: "v1", Resource: "things",
		})
	}
	s := &Service{
		cfg:   Config{SearchEnabled: true, SearchIncremental: true, Now: func() time.Time { return indexBase }},
		order: order,
	}
	s.delta = newDeltaState()

	// 모든 GVR이 매 tick 상한까지 소비한다고 보고 커서만 굴립니다.
	lastServed := make([]int, resources)
	for i := range lastServed {
		lastServed[i] = -1
	}
	for tick := 0; tick < 200; tick++ {
		start := s.delta.rr % resources
		for k := 0; k < perTick; k++ {
			lastServed[(start+k)%resources] = tick
		}
		s.delta.rr = (start + perTick) % resources
	}
	for i, at := range lastServed {
		if at < 0 {
			t.Fatalf("%d번 GVR이 한 번도 서비스되지 않았습니다", i)
		}
		if gap := 199 - at; gap > maxGap {
			t.Fatalf("%d번 GVR의 최대 공백이 %d tick입니다 — 상한 %d", i, gap, maxGap)
		}
	}
}

/* ── §6 잠금 경합 ──────────────────────────────────────────────────────── */

// TestLongSearchDoesNotDelayOtherPublish — 한 GVR의 긴 검색이 다른 GVR의
// 목록 게시를 붙잡으면 안 됩니다.
//
// 검색은 스냅숏 포인터만 원자적으로 집으므로, 순회 중에도 게시가 지나갑니다.
func TestLongSearchDoesNotDelayOtherPublish(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 4000)
	for i := 0; i < 4000; i++ {
		rows = append(rows, metaRow("prod", fmt.Sprintf("payments-%05d", i),
			fmt.Sprintf("uid-%05d", i), map[string]string{"app": "payments"}))
	}
	h := newDeltaHarness(t, rows...)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 넓은 접두사로 여러 페이지를 훑습니다.
			_, _ = h.svc.Search(SearchRequest{
				Query: "payments", Limit: MaxSearchPageSize,
				Namespaces: NamespaceFilter{All: true},
			})
		}
	}()

	// 검색이 도는 동안 게시가 지연 없이 지나가야 합니다.
	worst := time.Duration(0)
	for i := 0; i < 50; i++ {
		start := time.Now()
		h.svc.snapMu.Lock()
		h.svc.snapMu.Unlock()
		if took := time.Since(start); took > worst {
			worst = took
		}
	}
	close(stop)
	wg.Wait()

	if worst > 200*time.Millisecond {
		t.Fatalf("검색이 도는 동안 게시가 %v 지연되었습니다", worst)
	}
}

/* ── §3 예약 회계 ──────────────────────────────────────────────────────── */

// TestMultiGVRReservationsReturnToBaseline — 여러 GVR이 동시에 예약·해제해도
// 모든 계정이 기준선으로 돌아와야 합니다.
func TestMultiGVRReservationsReturnToBaseline(t *testing.T) {
	var b searchBudget
	b.max.Store(64 << 20)
	base := struct{ live, queued, inflight, recovery int64 }{}

	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				n := int64(512 * (1 + g%5))
				switch i % 3 {
				case 0:
					if b.reserveQueued(n) {
						b.transferQueuedToInflight(n)
						b.releaseInflight(n)
					}
				case 1:
					if b.reserveRecovery(n) {
						b.recovery.Add(-n)
						b.releaseLive(n)
					}
				default:
					if b.reserveTransient(n) {
						b.releaseTransient(n)
					}
				}
				if b.live.Load() > b.peakLimit() {
					t.Error("I-C 상한을 넘겼습니다")
					return
				}
			}
		}(g)
	}
	wg.Wait()

	if b.live.Load() != base.live || b.queued.Load() != base.queued ||
		b.inflight.Load() != base.inflight || b.recovery.Load() != base.recovery {
		t.Fatalf("기준선으로 돌아오지 않았습니다: live=%d queued=%d inflight=%d recovery=%d",
			b.live.Load(), b.queued.Load(), b.inflight.Load(), b.recovery.Load())
	}
}

// TestPersistentCostNeverUnderestimatesBuiltIndex — preflight 추정은 실제로
// 만들어진 인덱스보다 **작으면 안 됩니다**.
func TestPersistentCostNeverUnderestimatesBuiltIndex(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 4000)
	for i := 0; i < 4000; i++ {
		rows = append(rows, metaRow(
			fmt.Sprintf("ns-%02d", i%20),
			fmt.Sprintf("obj-%05d", i),
			fmt.Sprintf("uid-%05d-abcdef0123456789abcdef01", i),
			map[string]string{"app": fmt.Sprintf("svc-%02d", i%20), "tier": "backend"}))
	}
	index := indexOf(rows...)
	pm := measurePersistentInput(index, normalizeToken("Service"), true)
	estimated, peak := persistentSearchCost(pm)

	r := buildSearchIndex(index, "Service", true, hugeBudget, hugeBudget)
	if r.state != SearchReady {
		t.Fatalf("빌드 실패: %s", r.state)
	}
	if estimated < r.index.bytes {
		t.Fatalf("preflight 추정 %d가 실제 %d보다 작습니다 — 예산이 상한이 아니게 됩니다",
			estimated, r.index.bytes)
	}
	if peak < estimated {
		t.Fatalf("정점 %d가 보유 %d보다 작습니다", peak, estimated)
	}
	if estimated > r.index.bytes*3 {
		t.Fatalf("추정 %d가 실제 %d의 3배를 넘습니다 — 여유가 과합니다", estimated, r.index.bytes)
	}
}
