package resourcecatalog

// Round 8 소유권·수명·상한 검증
// --------------------------------------------------------------------------
// 회수 티켓의 소유권(P1-1), 보류분의 단일 소유권과 중복 제거(P1-2), 실제 상한과
// 세대 수명(P1-3), 게시 결과 구분(P1-4), tombstone 키 해석(P1-5),
// Scope·기존 동작 보존(P1-6)을 강제 교차로 확인합니다.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

/* ── P1-5 tombstone 키 해석 ─────────────────────────────────────────────── */

// TestSplitMetaKeyIsStrictPerResourceShape — key-only tombstone은 문자열 하나뿐이라
// 형식을 느슨하게 보면 조용히 다른 객체를 지웁니다.
func TestSplitMetaKeyIsStrictPerResourceShape(t *testing.T) {
	cases := []struct {
		key        string
		namespaced bool
		ns, name   string
		ok         bool
		why        string
	}{
		{"prod/payments-api", true, "prod", "payments-api", true, "정상 namespaced"},
		{"node-a", false, "", "node-a", true, "정상 cluster"},
		{"", true, "", "", false, "빈 키"},
		{"", false, "", "", false, "빈 키(cluster)"},
		{"/payments-api", true, "", "", false, "앞이 빈 슬래시"},
		{"prod/", true, "", "", false, "뒤가 빈 슬래시"},
		{"prod/sub/name", true, "", "", false, "슬래시 둘"},
		{"payments-api", true, "", "", false, "namespaced인데 슬래시 없음"},
		{"prod/node-a", false, "", "", false, "cluster인데 슬래시 있음"},
		{"/", true, "", "", false, "슬래시 하나뿐"},
	}
	for _, c := range cases {
		ns, name, ok := splitMetaKey(c.key, c.namespaced)
		if ns != c.ns || name != c.name || ok != c.ok {
			t.Fatalf("%s: (%q,%q,%v) — (%q,%q,%v)여야 합니다",
				c.why, ns, name, ok, c.ns, c.name, c.ok)
		}
	}
}

// TestMalformedTombstoneEscalatesInsteadOfDeletingClusterScoped — 형식이 어긋난
// 키를 ns=""로 뭉개면 cluster-scoped 객체를 지우게 됩니다. GVR stale로 승급해야 합니다.
func TestMalformedTombstoneEscalatesInsteadOfDeletingClusterScoped(t *testing.T) {
	h := newDeltaHarness(t,
		metaRow("prod", "payments-api", "uid-1", nil),
		metaRow("prod", "ledger", "uid-2", nil),
	)
	h.binding.namespaced = true

	before := h.queueLen()
	h.svc.enqueueObject(h.binding, cache.DeletedFinalStateUnknown{Key: "prod/sub/name"})
	if got := h.queueLen(); got != before {
		t.Fatalf("형식이 어긋난 키가 큐에 들어갔습니다: %d → %d", before, got)
	}
	if _, gvrStale := h.staleCount(); !gvrStale {
		t.Fatal("형식 오류가 GVR stale로 승급되지 않았습니다")
	}

	// 정상 key-only tombstone은 그대로 삭제로 이어져야 합니다.
	h2 := newDeltaHarness(t,
		metaRow("prod", "payments-api", "uid-1", nil),
		metaRow("prod", "ledger", "uid-2", nil),
	)
	h2.binding.namespaced = true
	if err := h2.store.Delete(metaRow("prod", "ledger", "uid-2", nil)); err != nil {
		t.Fatal(err)
	}
	h2.svc.enqueueObject(h2.binding, cache.DeletedFinalStateUnknown{Key: "prod/ledger"})
	h2.flush(t)
	page, err := h2.svc.Search(SearchRequest{Query: "ledger", Namespaces: allNS()})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("key-only tombstone이 삭제로 이어지지 않았습니다: %+v", page.Items)
	}
}

// TestClusterScopedKeyOnlyTombstone — cluster-scoped는 슬래시 없는 이름 하나입니다.
func TestClusterScopedKeyOnlyTombstone(t *testing.T) {
	nodeGVR := schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	h := newDeltaHarnessFor(t, nodeGVR, "Node", false,
		metaRow("", "node-a", "uid-a", nil),
		metaRow("", "node-b", "uid-b", nil),
	)
	h.binding.namespaced = false
	if err := h.store.Delete(metaRow("", "node-a", "uid-a", nil)); err != nil {
		t.Fatal(err)
	}
	h.svc.enqueueObject(h.binding, cache.DeletedFinalStateUnknown{Key: "node-a"})
	h.flush(t)

	page, err := h.svc.Search(SearchRequest{Query: "node", Namespaces: allNS()})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, it := range page.Items {
		names[it.Name] = true
	}
	if names["node-a"] || !names["node-b"] {
		t.Fatalf("cluster-scoped tombstone 결과가 어긋났습니다: %+v", page.Items)
	}
}

// TestRenameEnqueuesBothKeys — 이름이 바뀌면 옛 키와 새 키가 모두 들어가야 합니다.
func TestRenameEnqueuesBothKeys(t *testing.T) {
	oldNS, oldName := metaKeyOf(metaRow("prod", "old-name", "uid-1", nil), true)
	newNS, newName := metaKeyOf(metaRow("prod", "new-name", "uid-1", nil), true)
	if oldNS != "prod" || oldName != "old-name" || newNS != "prod" || newName != "new-name" {
		t.Fatalf("키 추출이 어긋났습니다: %q/%q → %q/%q", oldNS, oldName, newNS, newName)
	}
	h := newDeltaHarness(t, metaRow("prod", "old-name", "uid-1", nil))
	if err := h.store.Delete(metaRow("prod", "old-name", "uid-1", nil)); err != nil {
		t.Fatal(err)
	}
	renamed := metaRow("prod", "new-name", "uid-1", nil)
	if err := h.store.Add(renamed); err != nil {
		t.Fatal(err)
	}
	h.svc.enqueueKey(h.binding, oldNS, oldName)
	h.svc.enqueueKey(h.binding, newNS, newName)
	h.flush(t)

	// 검색은 **접두사** 일치이므로 "name"으로는 두 이름 중 어느 쪽도 걸리지 않습니다.
	// 옛 이름과 새 이름을 각각 접두사로 물어봐야 rename이 실제로 관찰됩니다.
	oldPage, err := h.svc.Search(SearchRequest{Query: "old", Namespaces: allNS()})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldPage.Items) != 0 {
		t.Fatalf("옛 이름이 남아 있습니다: %+v", oldPage.Items)
	}
	newPage, err := h.svc.Search(SearchRequest{Query: "new", Namespaces: allNS()})
	if err != nil {
		t.Fatal(err)
	}
	if len(newPage.Items) != 1 {
		t.Fatalf("새 이름이 %d건입니다 — 정확히 1건이어야 합니다: %+v", len(newPage.Items), newPage.Items)
	}
	if got := newPage.Items[0]; got.Name != "new-name" || got.UID != "uid-1" ||
		got.Namespace != "prod" || got.MatchedField != "name" {
		t.Fatalf("rename 결과가 어긋났습니다: %+v", got)
	}
}

/* ── P1-1 회수 소유권 강제 교차 ─────────────────────────────────────────── */

// pinTicket은 회수 티켓을 핀 단계까지 진행시킵니다.
//
// 티켓의 mutable field는 delta.mu 아래에서만 읽습니다 — 그것이 이 라운드의 계약입니다.
func pinTicket(t *testing.T, h *deltaHarness, ns string) *recoveryTicket {
	t.Helper()
	h.svc.requestResync(h.gvr, ns, 0)
	h.svc.advanceRecovery(context.Background()) // 핀
	h.svc.delta.mu.Lock()
	ticket := h.svc.delta.ticket
	var pinned, holding bool
	if ticket != nil {
		pinned = ticket.phase == recoveryBuilding
		holding = ticket.holdActive
	}
	h.svc.delta.mu.Unlock()
	if ticket == nil {
		t.Fatal("회수 티켓이 잡히지 않았습니다")
	}
	if !pinned {
		t.Fatalf("티켓이 핀 단계로 넘어가지 않았습니다: phase=%v", ticket.phase)
	}
	if !holding {
		t.Fatal("보류가 켜지지 않았습니다")
	}
	return ticket
}

// ticketProgress는 티켓의 진행 상태를 잠금 아래에서 읽습니다.
func ticketProgress(s *Service, t *recoveryTicket) (nextRow, hi int, dead bool) {
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	return t.nextRow, t.hi, t.dead
}

func budgetSnapshot(s *Service) (live, retained, queued, inflight, recovery int64) {
	return s.budget.live.Load(), s.budget.retained.Load(), s.budget.queued.Load(),
		s.budget.inflight.Load(), s.budget.recovery.Load()
}

func assertNoNegativeAccounting(t *testing.T, s *Service, when string) {
	t.Helper()
	live, retained, queued, inflight, recovery := budgetSnapshot(s)
	for name, v := range map[string]int64{
		"live": live, "retained": retained, "queued": queued,
		"inflight": inflight, "recovery": recovery,
	} {
		if v < 0 {
			t.Fatalf("%s: %s 회계가 음수입니다(%d)", when, name, v)
		}
	}
}

// TestDiscardBeforePinDuringMidChunkAndBeforePublish — 폐기를 세 지점에서 강제해도
// panic·부활·이중 해제·음수 회계가 없어야 합니다.
func TestDiscardBeforePinDuringMidChunkAndBeforePublish(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 6000)
	for i := 0; i < 6000; i++ {
		rows = append(rows, metaRow("prod", fmt.Sprintf("row-%05d", i),
			fmt.Sprintf("uid-%05d", i), map[string]string{"app": "payments"}))
	}

	t.Run("before-pin", func(t *testing.T) {
		h := newDeltaHarness(t, rows...)
		h.svc.requestResync(h.gvr, "prod", 0)
		h.entry.discard(h.svc)
		h.svc.advanceRecovery(context.Background())
		h.svc.delta.mu.Lock()
		alive := h.svc.delta.ticket
		h.svc.delta.mu.Unlock()
		if alive != nil && alive.phase == recoveryBuilding {
			t.Fatal("폐기 뒤에 핀이 진행되었습니다")
		}
		assertNoNegativeAccounting(t, h.svc, "before-pin")
		if got := h.svc.budget.recovery.Load(); got != 0 {
			t.Fatalf("회수 예약이 %d 남았습니다", got)
		}
	})

	t.Run("mid-chunk", func(t *testing.T) {
		h := newDeltaHarness(t, rows...)
		ticket := pinTicket(t, h, "prod")
		h.svc.advanceRecovery(context.Background()) // 첫 조각
		mid, _, _ := ticketProgress(h.svc, ticket)
		if mid == 0 {
			t.Fatal("조각이 진행되지 않았습니다")
		}
		h.entry.discard(h.svc)
		// 폐기 뒤 조각을 더 돌려도 죽은 티켓이 되살아나면 안 됩니다.
		h.svc.advanceRecovery(context.Background())
		_, _, dead := ticketProgress(h.svc, ticket)
		h.svc.delta.mu.Lock()
		alive := h.svc.delta.ticket
		h.svc.delta.mu.Unlock()
		if !dead {
			t.Fatal("폐기가 티켓을 죽이지 않았습니다")
		}
		if alive == ticket {
			t.Fatal("죽은 티켓이 되살아났습니다")
		}
		assertNoNegativeAccounting(t, h.svc, "mid-chunk")
		if got := h.svc.budget.recovery.Load(); got != 0 {
			t.Fatalf("회수 예약이 %d 남았습니다", got)
		}
	})

	t.Run("before-publish", func(t *testing.T) {
		small := rows[:200]
		h := newDeltaHarness(t, small...)
		ticket := pinTicket(t, h, "prod")
		// 조각을 끝까지 돌려 게시 직전 상태로 만듭니다.
		for i := 0; i < 8; i++ {
			nextRow, hi, _ := ticketProgress(h.svc, ticket)
			if nextRow >= hi {
				break
			}
			h.svc.advanceRecovery(context.Background())
		}
		h.entry.discard(h.svc)
		h.svc.advanceRecovery(context.Background())
		assertNoNegativeAccounting(t, h.svc, "before-publish")
		if got := h.svc.budget.recovery.Load(); got != 0 {
			t.Fatalf("회수 예약이 %d 남았습니다", got)
		}
		h.svc.delta.mu.Lock()
		alive := h.svc.delta.ticket
		h.svc.delta.mu.Unlock()
		if alive == ticket {
			t.Fatal("폐기된 티켓이 살아 있습니다")
		}
	})
}

// TestConcurrentRecoveryAndDiscard — race 검출기와 함께 도는 것이 목적입니다.
// 티켓의 mutable field는 전부 delta.mu 아래에서만 움직여야 합니다.
func TestConcurrentRecoveryAndDiscard(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 2000)
	for i := 0; i < 2000; i++ {
		rows = append(rows, metaRow("prod", fmt.Sprintf("row-%05d", i),
			fmt.Sprintf("uid-%05d", i), nil))
	}
	h := newDeltaHarness(t, rows...)

	// 회수 고루틴은 stop이 닫혀야 끝나고, 폐기 고루틴은 200회를 마치면 스스로 끝납니다.
	// 그래서 **폐기 완료를 먼저 관찰하고**, 그때 stop을 닫고, 마지막에 둘 다 기다립니다.
	// 둘을 한 WaitGroup으로 묶어 먼저 기다리면 회수 고루틴이 영원히 끝나지 않습니다.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	discardDone := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.svc.requestResync(h.gvr, "prod", 0)
			h.svc.advanceRecovery(context.Background())
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(discardDone)
		for i := 0; i < 200; i++ {
			// discard가 **프로덕션 잠금 아래에서** generation·lifecycle을 함께 올리고
			// 그 신원을 tokenPacked에 실어 둡니다. 여기서 같은 필드를 다시 건드리면
			// 회수 쪽이 snapMu 아래에서 읽는 값과 경합할 뿐 아니라, 소유권 규칙
			// (수명 신원은 snapMu 아래에서만 움직인다)을 테스트가 어깁니다.
			h.entry.discard(h.svc)
		}
	}()

	// 폐기 200회가 끝날 때까지 두 고루틴이 실제로 겹쳐 돕니다.
	<-discardDone
	close(stop)
	wg.Wait()

	assertNoNegativeAccounting(t, h.svc, "concurrent")
	if got := h.svc.budget.recovery.Load(); got != 0 {
		t.Fatalf("회수 예약이 %d 남았습니다", got)
	}
}

/* ── P1-2 보류분 소유권과 중복 제거 ─────────────────────────────────────── */

// TestHeldKeysAreAppliedOnceAndSlotsStayUnique — 회수 중에 같은 키의 UID 교체가
// 여러 번 일어나고 마지막에 삭제되면, 게시 뒤에 그 행은 없어야 하고 큐·보류가
// 비어 있어야 하며 예약이 기준선으로 돌아와야 합니다.
func TestHeldKeysAreAppliedOnceAndSlotsStayUnique(t *testing.T) {
	h := newDeltaHarness(t,
		metaRow("prod", "payments-api", "uid-0", nil),
		metaRow("prod", "ledger", "uid-l", nil),
	)
	baseLive, baseRetained, _, _, _ := budgetSnapshot(h.svc)

	pinTicket(t, h, "prod")

	// 회수 중 같은 키의 UID 교체 3회 + 마지막 삭제.
	for i := 1; i <= 3; i++ {
		obj := metaRow("prod", "payments-api", fmt.Sprintf("uid-%d", i), nil)
		if err := h.store.Add(obj); err != nil {
			t.Fatal(err)
		}
		h.svc.enqueueKey(h.binding, "prod", "payments-api")
		h.flush(t) // 보류로 들어갑니다.
	}
	if err := h.store.Delete(metaRow("prod", "payments-api", "uid-3", nil)); err != nil {
		t.Fatal(err)
	}
	h.svc.enqueueKey(h.binding, "prod", "payments-api")
	h.flush(t)

	// 조각을 끝까지 돌려 게시합니다.
	for i := 0; i < 8; i++ {
		h.svc.delta.mu.Lock()
		done := h.svc.delta.ticket == nil
		h.svc.delta.mu.Unlock()
		if done {
			break
		}
		h.svc.advanceRecovery(context.Background())
	}

	h.svc.delta.mu.Lock()
	q := h.svc.delta.queueFor(h.gvr)
	pending, held := len(q.events), len(q.hold)
	h.svc.delta.mu.Unlock()
	if pending != 0 || held != 0 {
		t.Fatalf("게시 뒤에 큐=%d 보류=%d가 남았습니다", pending, held)
	}

	// 삭제된 행은 검색에 없어야 합니다.
	page, err := h.svc.Search(SearchRequest{Query: "payments", Namespaces: allNS()})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("삭제된 행이 남아 있습니다: %+v", page.Items)
	}
	// 남은 행은 그대로여야 합니다.
	if page, err = h.svc.Search(SearchRequest{Query: "ledger", Namespaces: allNS()}); err != nil {
		t.Fatal(err)
	} else if len(page.Items) != 1 {
		t.Fatalf("무관한 행이 사라졌습니다: %+v", page.Items)
	}

	// 살아 있는 큐는 자기 고정 구조를 **정당하게 소유합니다.** 돌아와야 하는 것은
	// 이벤트·보류 몫이고, 원장은 그 큐의 고정 몫과 정확히 같아야 합니다.
	assertQueueAccountingAtBaseline(t, h.svc, h.gvr, "held-keys")
	if got := h.svc.budget.inflight.Load(); got != 0 {
		t.Fatalf("적용 예약이 %d 남았습니다", got)
	}
	if got := h.svc.budget.recovery.Load(); got != 0 {
		t.Fatalf("회수 예약이 %d 남았습니다", got)
	}
	_ = baseLive
	_ = baseRetained

	// 이후 새로 만든 두 행은 서로 다른 슬롯을 받아야 합니다(슬롯 누수·중복 없음).
	for _, name := range []string{"fresh-a", "fresh-b"} {
		obj := metaRow("prod", name, "uid-"+name, nil)
		if err := h.store.Add(obj); err != nil {
			t.Fatal(err)
		}
		h.svc.enqueueKey(h.binding, "prod", name)
	}
	h.flush(t)
	snap := h.entry.load()
	if snap == nil || snap.sindex == nil {
		t.Fatal("인덱스가 없습니다")
	}
	part := snap.sindex.dir.find("prod")
	if part == nil {
		t.Fatal("파티션이 없습니다")
	}
	slotA, _, okA := rowFind(part.rowDir, "fresh-a")
	slotB, _, okB := rowFind(part.rowDir, "fresh-b")
	if !okA || !okB {
		t.Fatalf("새 행을 찾지 못했습니다: a=%v b=%v", okA, okB)
	}
	if slotA == slotB {
		t.Fatalf("두 행이 같은 슬롯 %d을 씁니다", slotA)
	}
	// 슬롯이 유일한지 전수 확인합니다.
	seen := map[uint32]string{}
	rowEachAll(part.rowDir, func(name string, slot uint32) bool {
		if prev, dup := seen[slot]; dup {
			t.Fatalf("슬롯 %d을 %q와 %q가 함께 씁니다", slot, prev, name)
		}
		seen[slot] = name
		return true
	})
}

// TestDedupePartOpsKeepsLast — 같은 이름이 두 번 들어오면 마지막 것만 남아야 합니다.
func TestDedupePartOpsKeepsLast(t *testing.T) {
	ops := []partOp{
		{name: "b", input: &rowInput{name: "b", uid: "uid-b1"}},
		{name: "a", input: &rowInput{name: "a", uid: "uid-a1"}},
		{name: "b", input: &rowInput{name: "b", uid: "uid-b2"}},
		{name: "a"},
	}
	out := dedupePartOps(ops)
	if len(out) != 2 {
		t.Fatalf("중복이 남았습니다: %+v", out)
	}
	if out[0].name != "a" || out[0].input != nil {
		t.Fatalf("a의 마지막 연산(삭제)이 남지 않았습니다: %+v", out[0])
	}
	if out[1].name != "b" || out[1].input == nil || out[1].input.uid != "uid-b2" {
		t.Fatalf("b의 마지막 연산이 남지 않았습니다: %+v", out[1])
	}
}

/* ── P1-3 상한·세대 수명 ───────────────────────────────────────────────── */

// TestRetiredSearchGenerationsStayAccountedUntilLastReader — 은퇴한 세대는
// 마지막 독자가 놓을 때까지 회계에 남아야 합니다.
func TestRetiredSearchGenerationsStayAccountedUntilLastReader(t *testing.T) {
	// 회계는 **승인+설치**를 통해서만 움직입니다. 상한이 0이면 승인 자체가 거절되므로
	// 프로덕션과 같은 최소 설정을 갖춥니다.
	s := lifetimeService()
	e := &resourceEntry{}
	first := &entrySnapshot{sindex: &searchIndex{bytes: 1000}}
	second := &entrySnapshot{sindex: &searchIndex{bytes: 300}}

	s.snapMu.Lock()
	if !s.publishLeaseLocked(e, first) {
		s.snapMu.Unlock()
		t.Fatal("첫 세대가 승인되지 않았습니다")
	}
	s.snapMu.Unlock()
	if got := s.searchBytes.Load(); got != 1000 {
		t.Fatalf("첫 세대 회계가 %d입니다", got)
	}

	lease, leased := e.acquireSearch(s) // 독자가 1세대를 빌립니다.
	if lease == nil {
		t.Fatal("세대를 빌리지 못했습니다")
	}
	if leased != first {
		t.Fatal("빌린 세대의 스냅숏이 그 세대의 것이 아닙니다")
	}

	s.snapMu.Lock()
	if !s.publishLeaseLocked(e, second) {
		s.snapMu.Unlock()
		t.Fatal("두 번째 세대가 승인되지 않았습니다")
	}
	s.snapMu.Unlock()
	// 독자가 아직 1세대를 들고 있으므로 두 세대가 함께 계상되어야 합니다.
	if got := s.searchBytes.Load(); got != 1300 {
		t.Fatalf("은퇴 세대가 먼저 빠졌습니다: %d — 1300이어야 합니다", got)
	}

	s.releaseSearch(lease)
	if got := s.searchBytes.Load(); got != 300 {
		t.Fatalf("마지막 독자가 놓았는데 회계가 %d입니다", got)
	}
	if got := s.budget.retained.Load(); got != 300 {
		t.Fatalf("retained가 %d입니다", got)
	}
	if s.budget.live.Load() != 300 {
		t.Fatalf("live가 %d입니다", s.budget.live.Load())
	}
	// 이중 해제가 없어야 합니다.
	s.releaseSearch(lease)
	if got := s.searchBytes.Load(); got != 300 {
		t.Fatalf("이중 해제가 일어났습니다: %d", got)
	}
}

// publishGeneration은 새 검색 세대를 게시합니다(원장 경로 그대로).
func publishGeneration(t *testing.T, h *deltaHarness, rows ...*metav1.PartialObjectMetadata) *searchIndex {
	t.Helper()
	index := indexOf(rows...)
	r := buildSearchIndex(index, "Service", true, hugeBudget, hugeBudget)
	if r.state != SearchReady {
		t.Fatalf("세대 빌드 실패: %s %s", r.state, r.reason)
	}
	cur := h.entry.load()
	next := &entrySnapshot{
		index: index, sindex: r.index, searchState: SearchReady,
		indexVer: cur.indexVer + 1, searchVer: cur.searchVer + 1,
	}
	h.svc.snapMu.Lock()
	admitted := h.svc.publishLeaseLocked(h.entry, next)
	h.entry.setSnap(next)
	h.svc.snapMu.Unlock()
	if !admitted {
		t.Fatal("새 세대가 예산 승인을 받지 못했습니다")
	}
	return r.index
}

// TestSearchRequestViewIsPinnedToAcquiredLease — 요청이 빌린 세대와 실제로 훑는
// 세대가 **반드시 같아야** 합니다.
//
// 빌린 뒤에 snapPtr을 다시 읽으면, 그 사이의 게시로 빌리지 않은 새 세대를 훑으면서
// 회계는 아무도 쓰지 않는 옛 세대를 붙잡습니다. 훑는 것과 붙잡는 것이 어긋나면
// 상한도 수명도 의미를 잃습니다.
func TestSearchRequestViewIsPinnedToAcquiredLease(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-old", nil))
	oldSnap := h.entry.load()
	oldBytes := h.svc.searchBytes.Load()
	if oldBytes <= 0 {
		t.Fatalf("초기 회계가 %d입니다", oldBytes)
	}

	// ① 요청 뷰를 먼저 확보합니다.
	view := h.svc.acquireView()
	if len(view.leases) != 1 {
		t.Fatalf("빌린 세대가 %d개입니다 — 1개여야 합니다", len(view.leases))
	}
	if view.searchAt(0) != oldSnap {
		t.Fatal("뷰가 빌린 세대의 스냅숏을 담지 않았습니다")
	}

	// ② 순회 전에 새 세대를 게시하고 옛 세대를 은퇴시킵니다.
	newIdx := publishGeneration(t, h, metaRow("prod", "payments-api", "uid-new", nil))
	if h.entry.load() == oldSnap {
		t.Fatal("새 세대가 게시되지 않았습니다")
	}
	// 옛 세대는 아직 독자가 들고 있으므로 **두 세대가 함께** 계상되어야 합니다.
	if got := h.svc.searchBytes.Load(); got != oldBytes+newIdx.bytes {
		t.Fatalf("은퇴 세대가 먼저 빠졌습니다: %d — %d여야 합니다", got, oldBytes+newIdx.bytes)
	}

	// ③ 그 뷰로 순회하면 **빌린(옛) 세대의 결과**가 나와야 합니다.
	page := SearchPage{Query: "payments", Items: make([]SearchItem, 0, 10)}
	var diag searchDiagnostics
	got, err := h.svc.searchPersistent("payments", 10, searchCursorKey{}, false,
		searchFingerprint("prod-seoul", "payments", true, nil), &page, &diag, view)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("결과가 %d건입니다: %+v", len(got.Items), got.Items)
	}
	if got.Items[0].UID != "uid-old" {
		t.Fatalf("빌리지 않은 세대를 훑었습니다: UID=%q — uid-old여야 합니다", got.Items[0].UID)
	}

	// ④ 놓으면 옛 세대가 **정확히 한 번** 빠집니다.
	h.svc.releaseView(view)
	if got := h.svc.searchBytes.Load(); got != newIdx.bytes {
		t.Fatalf("놓은 뒤 회계가 %d입니다 — %d여야 합니다", got, newIdx.bytes)
	}
	// 같은 뷰를 다시 놓아도 이중 해제가 없어야 합니다.
	h.svc.releaseView(view)
	if got := h.svc.searchBytes.Load(); got != newIdx.bytes {
		t.Fatalf("이중 해제가 일어났습니다: %d", got)
	}
	if got := h.svc.budget.retained.Load(); got != newIdx.bytes {
		t.Fatalf("retained가 %d입니다 — %d여야 합니다", got, newIdx.bytes)
	}
	if got := h.svc.budget.live.Load(); got != newIdx.bytes {
		t.Fatalf("live가 %d입니다 — %d여야 합니다", got, newIdx.bytes)
	}

	// ⑤ 다음 요청은 **새 세대**를 봅니다.
	after, err := h.svc.Search(SearchRequest{Query: "payments", Namespaces: allNS()})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Items) != 1 || after.Items[0].UID != "uid-new" {
		t.Fatalf("다음 요청이 새 세대를 보지 않았습니다: %+v", after.Items)
	}
	// 요청이 끝나면 세대 수와 회계는 그대로여야 합니다(refcount 누수 없음).
	if got := h.svc.searchBytes.Load(); got != newIdx.bytes {
		t.Fatalf("요청 뒤 회계가 %d입니다", got)
	}
	if l := h.entry.leasePtr.Load(); l == nil || l.refs.Load() != 1 {
		t.Fatalf("요청이 세대를 놓지 않았습니다: %v", l)
	}
}

// TestSearchViewWithoutLeaseStillFreezesSnapshot — 세대가 설치되지 않은 항목도
// 요청 뷰가 스냅숏을 **한 번** 고정해야 합니다(빌릴 것이 없어도 시점은 하나입니다).
func TestSearchViewWithoutLeaseStillFreezesSnapshot(t *testing.T) {
	s := scopedService(t, SearchUnavailable, []*metav1.PartialObjectMetadata{
		metaRow("allowed", "row-a", "uid-a", nil),
	})
	if l := s.entries[scopedGVR].leasePtr.Load(); l != nil {
		t.Fatal("이 하네스는 세대를 설치하지 않아야 합니다")
	}
	view := s.acquireView()
	if len(view.leases) != 0 {
		t.Fatalf("빌린 세대가 %d개입니다 — 0개여야 합니다", len(view.leases))
	}
	frozen := view.searchAt(0)
	if frozen == nil {
		t.Fatal("뷰가 스냅숏을 고정하지 않았습니다")
	}
	// 뷰를 만든 뒤 스냅숏이 바뀌어도 뷰는 그대로여야 합니다.
	s.entries[scopedGVR].setSnap(&entrySnapshot{index: indexOf()})
	if view.searchAt(0) != frozen {
		t.Fatal("뷰가 나중 게시를 따라갔습니다")
	}
	s.releaseView(view) // 빌린 것이 없으므로 아무 일도 없어야 합니다.
	if got := s.searchBytes.Load(); got != 0 {
		t.Fatalf("빌리지 않았는데 회계가 움직였습니다: %d", got)
	}
}

// TestRepeatedPublishWithBlockedReadersRespectsCaps — 여러 세대를 붙잡은 독자가
// 있는 채로 반복 게시해도 세 불변식이 깨지면 안 됩니다.
func TestRepeatedPublishWithBlockedReadersRespectsCaps(t *testing.T) {
	s := lifetimeService()
	e := &resourceEntry{}

	pinned := make([]*searchLease, 0, 8)
	for i := 0; i < 8; i++ {
		snap := &entrySnapshot{sindex: &searchIndex{bytes: int64(1024 * (i + 1))}}
		s.snapMu.Lock()
		admitted := s.publishLeaseLocked(e, snap)
		s.snapMu.Unlock()
		if !admitted {
			t.Fatalf("%d번째 세대가 승인되지 않았습니다", i)
		}
		if l, leased := e.acquireSearch(s); l != nil {
			if leased != snap {
				t.Fatalf("%d번째: 빌린 세대와 스냅숏이 어긋났습니다", i)
			}
			pinned = append(pinned, l)
		}
		if got := s.budget.retained.Load(); got > s.budget.limit() {
			t.Fatalf("I-A 위반: retained=%d > %d", got, s.budget.limit())
		}
		if got := s.budget.live.Load(); got > s.budget.peakLimit() {
			t.Fatalf("I-C 위반: live=%d > %d", got, s.budget.peakLimit())
		}
	}
	for _, l := range pinned {
		s.releaseSearch(l)
	}
	// 마지막 세대만 남아야 합니다.
	if got, want := s.searchBytes.Load(), int64(1024*8); got != want {
		t.Fatalf("모두 놓은 뒤 회계가 %d입니다 — %d여야 합니다", got, want)
	}
	// 폐기하면 0으로 수렴합니다.
	s.snapMu.Lock()
	s.installLeaseLocked(e, nil)
	s.snapMu.Unlock()
	if got := s.searchBytes.Load(); got != 0 {
		t.Fatalf("폐기 뒤 회계가 %d 남았습니다", got)
	}
	if got := s.budget.live.Load(); got != 0 {
		t.Fatalf("폐기 뒤 live가 %d 남았습니다", got)
	}
}

// TestHotDeltaKeepsAccountingBounded — 1000건 델타를 반복해도 회계가 기준선으로
// 돌아오고 상한을 넘지 않아야 합니다.
func TestHotDeltaKeepsAccountingBounded(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 1000)
	for i := 0; i < 1000; i++ {
		rows = append(rows, metaRow("prod", fmt.Sprintf("row-%04d", i),
			fmt.Sprintf("uid-%04d", i), map[string]string{"app": "payments"}))
	}
	h := newDeltaHarness(t, rows...)
	for round := 0; round < 5; round++ {
		for _, row := range rows {
			updated := metaRow(row.Namespace, row.Name, string(row.UID),
				map[string]string{"app": fmt.Sprintf("payments-%d", round)})
			if err := h.store.Add(updated); err != nil {
				t.Fatal(err)
			}
			h.svc.enqueueKey(h.binding, row.Namespace, row.Name)
		}
		for h.queueLen() > 0 {
			if h.flush(t) == 0 {
				break
			}
		}
		if got := h.svc.budget.inflight.Load(); got != 0 {
			t.Fatalf("round %d: inflight가 %d 남았습니다", round, got)
		}
		if got := h.svc.budget.retained.Load(); got > h.svc.budget.limit() {
			t.Fatalf("round %d: I-A 위반 retained=%d", round, got)
		}
		if got := h.svc.budget.live.Load(); got > h.svc.budget.peakLimit() {
			t.Fatalf("round %d: I-C 위반 live=%d", round, got)
		}
		assertNoNegativeAccounting(t, h.svc, fmt.Sprintf("round %d", round))
	}
	// 1000건을 다섯 바퀴 돌려도 원장은 **그 큐의 고정 몫 하나**로 정확히 돌아옵니다.
	assertQueueAccountingAtBaseline(t, h.svc, h.gvr, "hot-delta")
}

// assertQueueAccountingAtBaseline은 단일 큐 하네스의 종료 상태를 못박습니다.
//
//   - 이벤트·보류 예약은 0으로 돌아옵니다.
//   - 큐가 살아 있는 동안 그 고정 구조는 원장에 남아 있어야 합니다(누수가 아니라 소유).
//   - q.pendingBytes()와 budget.queued는 **정확히 같습니다** — 예약한 것과 세는 것이
//     갈라지면 상한이 상한이 아니게 됩니다.
func assertQueueAccountingAtBaseline(t *testing.T, s *Service, gvr schema.GroupVersionResource, where string) {
	t.Helper()
	s.delta.mu.Lock()
	q := s.delta.queueFor(gvr)
	events, hold, fixed := len(q.events), len(q.hold), q.fixed
	structural := q.fixed + q.dynamic + q.capCharged
	capLeft := q.capCharged
	pending := q.pendingBytes()
	decomposed := s.delta.queuedBytesLocked()
	s.delta.mu.Unlock()

	if events != 0 || hold != 0 {
		t.Fatalf("%s: 큐=%d 보류=%d가 남았습니다", where, events, hold)
	}
	// 완전히 비었으면 **도달 가능한 저장 용량**도 돌아와야 합니다.
	// 배열·버킷이 고수위에 남아 있는데 원장만 0이면 그것이 바로 과소 계상입니다.
	if capLeft != 0 {
		t.Fatalf("%s: 비었는데 저장 용량 계상이 %d 남았습니다 — 압축되지 않았습니다", where, capLeft)
	}
	if fixed <= 0 {
		t.Fatalf("%s: 큐 고정 몫이 %d입니다 — 계상되지 않았습니다", where, fixed)
	}
	if pending != structural {
		t.Fatalf("%s: pendingBytes=%d인데 구조 몫은 %d입니다 — 이벤트 몫이 남았습니다",
			where, pending, structural)
	}
	// 원장은 **구조적 분해와 정확히 같아야** 합니다(큐 + 회로).
	if got := s.budget.queued.Load(); got != decomposed {
		t.Fatalf("%s: budget.queued=%d, 분해=%d — 예약과 계상이 갈라졌습니다",
			where, got, decomposed)
	}
}

// TestSixtyFourGVRQueuesStayWithinCaps — 64개 GVR이 동시에 큐를 채워도 예약이
// I-C를 넘지 않고, 폐기하면 전부 0으로 돌아와야 합니다.
func TestSixtyFourGVRQueuesStayWithinCaps(t *testing.T) {
	const resources = 64
	order := make([]schema.GroupVersionResource, 0, resources)
	entries := make(map[schema.GroupVersionResource]*resourceEntry, resources)
	for i := 0; i < resources; i++ {
		gvr := schema.GroupVersionResource{Group: fmt.Sprintf("g%02d", i), Version: "v1", Resource: "things"}
		order = append(order, gvr)
		e := &resourceEntry{gvr: gvr}
		e.lifecycle = 1
		e.tokenPacked.Store(packToken(1, 0))
		entries[gvr] = e
	}
	s := &Service{
		cfg:   Config{SearchEnabled: true, SearchIncremental: true, MaxSearchIndexBytes: DefaultMaxSearchIndexBytes},
		order: order, entries: entries,
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	s.budget.max.Store(DefaultMaxSearchIndexBytes)

	for _, gvr := range order {
		b := &handlerBinding{entry: entries[gvr], packed: entries[gvr].tokenPacked.Load(), namespaced: true}
		for i := 0; i < 500; i++ {
			s.enqueueKey(b, "prod", fmt.Sprintf("row-%04d", i))
		}
		if got := s.budget.live.Load(); got > s.budget.peakLimit() {
			t.Fatalf("I-C 위반: live=%d > %d", got, s.budget.peakLimit())
		}
	}
	// 64개 큐가 동시에 살아 있는 동안, 원장의 queued는 **모든 큐의 pendingBytes 합**과
	// 정확히 같아야 합니다(고정 구조 + 이벤트). 이것이 64 GVR 분해의 하한입니다.
	s.delta.mu.Lock()
	var wholeFixed int64
	for _, q := range s.delta.queues {
		wholeFixed += q.fixed
	}
	decomposed := s.delta.queuedBytesLocked()
	queues := len(s.delta.queues)
	s.delta.mu.Unlock()
	if queues != resources {
		t.Fatalf("큐가 %d개입니다 — %d개여야 합니다", queues, resources)
	}
	if got := s.budget.queued.Load(); got != decomposed {
		t.Fatalf("64 GVR 분해가 어긋났습니다: queued=%d, 분해=%d", got, decomposed)
	}
	if wholeFixed <= 0 {
		t.Fatal("큐 고정 구조가 원장에 전혀 실리지 않았습니다")
	}

	// **종단 경로**: 멈춘 리소스는 고정 몫까지 정확히 한 번 되돌리고 0으로 수렴합니다.
	for _, gvr := range order {
		s.snapMu.Lock()
		s.purgeQueueLocked(gvr, true)
		s.snapMu.Unlock()
	}
	s.delta.mu.Lock()
	remaining := len(s.delta.queues)
	s.delta.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("멈춘 뒤에도 큐가 %d개 남았습니다", remaining)
	}
	if got := s.budget.queued.Load(); got != 0 {
		t.Fatalf("폐기 뒤 큐 예약이 %d 남았습니다", got)
	}
	if got := s.budget.live.Load(); got != 0 {
		t.Fatalf("폐기 뒤 live가 %d 남았습니다", got)
	}
	// 두 번 불러도 이중 해제가 없어야 합니다.
	for _, gvr := range order {
		s.snapMu.Lock()
		s.purgeQueueLocked(gvr, true)
		s.snapMu.Unlock()
	}
	assertNoNegativeAccounting(t, s, "purge-twice")
	if got := s.budget.queued.Load(); got != 0 {
		t.Fatalf("두 번째 폐기 뒤 큐 예약이 %d입니다", got)
	}
}

/* ── P1-4 최종 게시 거절 ───────────────────────────────────────────────── */

// TestFinalRetainedRejectionBecomesStaleNotRequeueLoop — 빌드 뒤 게시 직전에
// 예산이 차면, 되돌려 다시 시도하는 대신 정확한 namespace stale + 쿨다운이
// 되어야 하고 이후 tick에서 같은 일을 반복하지 않아야 합니다.
func TestFinalRetainedRejectionBecomesStaleNotRequeueLoop(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))

	h.upsert(t, metaRow("prod", "brand-new", "uid-2", map[string]string{"app": "payments"}))
	// 게시 직전에 보유를 상한까지 채웁니다.
	h.svc.budget.retained.Store(h.svc.budget.limit())

	before := h.svc.delta.publishBudgetRejects.Load()
	if got := h.flush(t); got != 1 {
		t.Fatalf("flush가 %d건을 처리했습니다", got)
	}
	if h.svc.delta.publishBudgetRejects.Load() == before {
		t.Fatal("최종 예산 거절이 집계되지 않았습니다")
	}
	if h.queueLen() != 0 {
		t.Fatalf("예산 거절인데 %d건이 되돌아왔습니다 — 무한 반복이 됩니다", h.queueLen())
	}
	count, _ := h.staleCount()
	if count != 1 {
		t.Fatalf("정확한 namespace stale이 남지 않았습니다: %d", count)
	}
	if got := h.svc.budget.inflight.Load(); got != 0 {
		t.Fatalf("거절된 배치의 예약이 %d 남았습니다", got)
	}
	// 다음 여러 tick에서도 다시 적용하려 들지 않아야 합니다.
	for i := 0; i < 5; i++ {
		if got := h.flush(t); got != 0 {
			t.Fatalf("%d번째 tick에서 %d건을 다시 적용했습니다", i, got)
		}
	}
}

/* ── P1-6 Scope 불변성과 기존 동작 ─────────────────────────────────────── */

// TestHiddenOverflowDoesNotChangeRecentOrDetail — 숨겨진 쪽에서 1025개 namespace가
// 넘쳐 GVR stale로 승급해도, 허용된 참조의 최근 항목·상세 결과와 외부 GET 횟수가
// 그대로여야 합니다.
func TestHiddenOverflowDoesNotChangeRecentOrDetail(t *testing.T) {
	visible := []*metav1.PartialObjectMetadata{
		metaRow("allowed", "payments-api", "uid-1", nil),
		metaRow("allowed", "ledger", "uid-2", nil),
	}
	ref := RecentRef{Version: "v1", Resource: "services",
		Namespace: "allowed", Name: "payments-api", UID: "uid-1"}
	scope := NamespaceFilter{List: []string{"allowed"}}

	clean := newDeltaHarness(t, visible...)
	noisy := newDeltaHarness(t, visible...)
	// 숨겨진 쪽에서 추적 상한을 넘겨 GVR stale로 승급시킵니다.
	for i := 0; i < maxStaleTracked+1; i++ {
		noisy.svc.requestResync(noisy.gvr, fmt.Sprintf("hidden-%05d", i), uint64(i+1))
	}
	if _, gvrStale := noisy.staleCount(); !gvrStale {
		t.Fatal("1025개 namespace가 GVR stale로 승급되지 않았습니다")
	}
	// 숨겨진 쪽에서 add·UID 교체·delete도 일으킵니다.
	noisy.upsert(t, metaRow("hidden-0", "secret", "uid-h1", nil))
	noisy.upsert(t, metaRow("hidden-0", "secret", "uid-h2", nil))
	noisy.remove(t, metaRow("hidden-0", "secret", "uid-h2", nil))
	noisy.flush(t)

	cleanItems, err := clean.svc.Recent([]RecentRef{ref}, scope)
	if err != nil {
		t.Fatal(err)
	}
	noisyItems, err := noisy.svc.Recent([]RecentRef{ref}, scope)
	if err != nil {
		t.Fatal(err)
	}
	cleanJSON, _ := json.Marshal(cleanItems)
	noisyJSON, _ := json.Marshal(noisyItems)
	if string(cleanJSON) != string(noisyJSON) {
		t.Fatalf("숨겨진 쪽 오버플로가 허용 참조를 바꿨습니다\n%s\n%s", cleanJSON, noisyJSON)
	}
	if len(cleanItems) != 1 {
		t.Fatalf("허용 참조가 해석되지 않았습니다: %+v", cleanItems)
	}

	// 상세 신원 판정도 같아야 합니다(외부 GET에 도달하기 전 단계까지).
	cleanIdx := clean.entry.baselineIndex()
	noisyIdx := noisy.entry.baselineIndex()
	for _, name := range []string{"payments-api", "ledger", "missing"} {
		_, cOK := cleanIdx.lookup("allowed", name)
		_, nOK := noisyIdx.lookup("allowed", name)
		if cOK != nOK {
			t.Fatalf("%q의 상세 신원 판정이 갈렸습니다: clean=%v noisy=%v", name, cOK, nOK)
		}
	}
}

// TestBaselineReadsDoNotPinSearchGeneration — 목록 경로는 검색 세대를 붙잡지
// 않아야 합니다. 붙잡으면 목록 요청 하나가 은퇴한 인덱스를 회계에 남깁니다.
func TestBaselineReadsDoNotPinSearchGeneration(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	if h.entry.baselineIndex() == nil {
		t.Fatal("baseline 목록 스냅숏이 없습니다")
	}
	before := h.entry.leasePtr.Load()
	_ = h.entry.baselineIndex()
	_, _ = h.svc.describeSnapshot(h.gvr, h.entry.load())
	after := h.entry.leasePtr.Load()
	if before != after {
		t.Fatal("목록 읽기가 검색 세대를 바꿨습니다")
	}
	if after != nil && after.refs.Load() != 1 {
		t.Fatalf("목록 읽기가 검색 세대를 붙잡았습니다: refs=%d", after.refs.Load())
	}
}
