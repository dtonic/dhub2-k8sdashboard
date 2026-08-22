package resourcecatalog

// Round 10 게이트 회귀 (소유권 패치)
// --------------------------------------------------------------------------
// (a) 거절된 첫 큐/회로/티켓은 **큰 할당을 하지 않고**, 원장은 내장·선계상 몫을 포함
// (b) GVR당 512건 채움→완전 드레인→재채움을 64 GVR에서 반복해도 계상이 도달
//     가능한 용량/버킷 아래로 내려가지 않고 live <= 3*Max
//     (8192건×64는 기본 I-C를 넘겨 픽스처가 비결정적이 되므로 쓰지 않습니다.
//      거절 경로는 같은 테스트 끝에서 **따로** 검사합니다.)
// (c) 2,001행 GVR 안의 1행 namespace 대상이 **원본 핀 + 측면 할당 전체**를 덮고,
//     거절은 빌더 할당보다 먼저 일어남
//     (10만 행 경계는 recoveryReserveBytes 순수 함수로 searchround10에서 별도 검증)
// (d) 회수 중 무관한 namespace 마커는 대상 ack을 막지 않고, 같은 대상 마커는 살아남음
// (e) 넘침 대상 둘이 서로의 회로를 reset하지 못하고 회수·전체빌드 회전이 없음
// (f) 폐기·취소 경합에서 큐·티켓·원본·측면 예약이 정확히 한 번 풀림

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

/* ── (a) 거절은 할당보다 먼저 ───────────────────────────────────────────── */

// TestRejectedControlObjectsAllocateNothing — 큐·회로·티켓은 **승인 없이 만들어지지
// 않습니다.** 거절된 경로는 큰 구조를 하나도 잡지 않고, 원장은 살아 있는 제어
// 객체(큐 고정·회로 항목·회로 맵 씨앗·티켓)를 빠짐없이 포함해야 합니다.
func TestRejectedControlObjectsAllocateNothing(t *testing.T) {
	// peak(=3*max)이 **가장 작은 제어 구조(회로 맵 씨앗)보다도** 작아야 합니다.
	// 큐 고정 몫만 기준으로 잡으면 회로(4KiB+256B)는 여유롭게 통과해서
	// "거절된 제어 객체"를 전혀 검사하지 못합니다.
	s := tinyBudgetService(t, int64(circuitsMapSeedBytes)/4)
	if s.budget.peakLimit() >= circuitsMapSeedBytes {
		t.Fatalf("픽스처가 느슨합니다: peak=%d >= 회로 맵 씨앗=%d",
			s.budget.peakLimit(), int64(circuitsMapSeedBytes))
	}
	if s.budget.peakLimit() >= deltaQueueFixedBytes {
		t.Fatalf("픽스처가 느슨합니다: peak=%d >= 큐 고정 몫=%d",
			s.budget.peakLimit(), int64(deltaQueueFixedBytes))
	}
	e := s.entries[scopedGVR]
	b := &handlerBinding{entry: e, packed: e.tokenPacked.Load(), namespaced: true}

	s.enqueueKey(b, "prod", "row-0")
	s.delta.mu.Lock()
	queues := len(s.delta.queues)
	circuit, _ := s.delta.circuitFor(recoveryTarget{gvr: scopedGVR, namespace: "prod"})
	circuits := len(s.delta.circuits)
	seed := s.delta.circuitsSeed
	ticket := s.pickRecoveryLocked(indexBase)
	s.delta.mu.Unlock()

	if queues != 0 {
		t.Fatalf("거절됐는데 큐를 %d개 만들었습니다", queues)
	}
	if circuit != nil || circuits != 0 || seed != 0 {
		t.Fatalf("거절됐는데 회로=%v 개수=%d 씨앗=%d입니다", circuit != nil, circuits, seed)
	}
	if ticket != nil {
		t.Fatal("거절됐는데 티켓을 만들었습니다")
	}
	if got := s.budget.queued.Load(); got != 0 {
		t.Fatalf("만들지 않은 구조가 %d 계상되었습니다", got)
	}
	if live, peak := s.budget.live.Load(), s.budget.peakLimit(); live > peak {
		t.Fatalf("I-C 위반 live=%d > %d", live, peak)
	}
	assertLedgerMatchesStructure(t, s, "rejected-control-objects")

	// 상한이 풀리면 셋 다 만들어지고 **전부 원장에 나타나야** 합니다.
	s.cfg.MaxSearchIndexBytes = DefaultMaxSearchIndexBytes
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	s.enqueueKey(b, "prod", "row-0")
	s.requestResync(scopedGVR, "prod", 9)
	s.delta.mu.Lock()
	s.delta.circuitFor(recoveryTarget{gvr: scopedGVR, namespace: "prod"})
	s.delta.cooldownUntil = time.Time{}
	live := s.pickRecoveryLocked(indexBase)
	s.delta.ticket = live
	hasSeed := s.delta.circuitsSeed > 0
	s.delta.mu.Unlock()

	if live == nil || !live.structCharged {
		t.Fatal("티켓이 만들어지지 않았거나 계상되지 않았습니다")
	}
	if !hasSeed {
		t.Fatal("회로 맵 씨앗이 계상되지 않았습니다")
	}
	assertLedgerMatchesStructure(t, s, "admitted-control-objects")

	// 티켓을 걷으면 그 몫만 정확히 빠집니다.
	before := s.budget.queued.Load()
	s.delta.mu.Lock()
	s.dropTicketLocked(live)
	s.delta.mu.Unlock()
	if got := s.budget.queued.Load(); got != before-recoveryTicketBytes {
		t.Fatalf("티켓 해제가 %d만큼입니다 — %d여야 합니다", before-got, int64(recoveryTicketBytes))
	}
	assertLedgerMatchesStructure(t, s, "ticket-dropped")
}

/* ── (b) 채움 → 드레인 → 재채움에서 계상이 실제를 따라갑니다 ────────────── */

// TestFillDrainRefillKeepsReachableStorageAccounted — 큐를 상한까지 채우고 완전히
// 비운 뒤 다시 채웁니다. 비운 순간에도 배열 용량·색인 버킷이 살아 있으면 그만큼
// 계상되어야 하고(또는 압축으로 실제가 줄어야 하고), live는 언제나 3*Max 이하입니다.
func TestFillDrainRefillKeepsReachableStorageAccounted(t *testing.T) {
	const (
		resources = 64
		// **서비스 상한 안에서 결정적으로** 담기는 크기입니다.
		//
		// GVR당 8192건은 64개를 곱하면 이벤트 예약만 120MB가 넘고 배열·버킷까지
		// 더하면 기본 I-C(3*64MiB)를 넘습니다. 그러면 뒤쪽 GVR의 큐 생성이 거절되어
		// 픽스처 자체가 비결정적이 됩니다. 용량 계상을 증명하는 데 필요한 것은
		// "64개 GVR이 동시에 도달 가능한 용량을 들고 있다"는 사실이지 8192라는
		// 숫자가 아니므로, 상한 안에 확실히 들어가는 값으로 고정합니다.
		// 거절 경로는 아래에서 **따로, 명시적으로** 검사합니다.
		fill = 512
	)
	order := make([]schema.GroupVersionResource, 0, resources)
	entries := make(map[schema.GroupVersionResource]*resourceEntry, resources)
	for i := 0; i < resources; i++ {
		gvr := schema.GroupVersionResource{Group: fmt.Sprintf("g%02d", i), Version: "v1", Resource: "things"}
		order = append(order, gvr)
		e := &resourceEntry{gvr: gvr}
		e.lifecycle = 1
		e.tokenPacked.Store(packToken(1, 0))
		e.setStatus(StateReady, "")
		entries[gvr] = e
	}
	s := &Service{
		cfg: Config{
			SearchEnabled: true, SearchIncremental: true,
			MaxSearchIndexBytes: DefaultMaxSearchIndexBytes,
			Now:                 func() time.Time { return indexBase },
		},
		order: order, entries: entries,
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	s.budget.max.Store(DefaultMaxSearchIndexBytes)

	bindings := make(map[schema.GroupVersionResource]*handlerBinding, resources)
	for _, gvr := range order {
		bindings[gvr] = &handlerBinding{
			entry: entries[gvr], packed: entries[gvr].tokenPacked.Load(), namespaced: true,
		}
	}

	// 원장이 **도달 가능한 저장 용량** 아래로 내려가지 않는지 직접 셉니다.
	reachable := func() int64 {
		s.delta.mu.Lock()
		defer s.delta.mu.Unlock()
		var total int64
		for _, q := range s.delta.queues {
			total += int64(cap(q.events)) * deltaEventStructBytes
			total += int64(cap(q.hold)) * deltaEventStructBytes
			total += int64(len(q.index)) * deltaIndexEntryBytes
		}
		return total
	}
	checkCaps := func(where string) {
		t.Helper()
		if live, peak := s.budget.live.Load(), s.budget.peakLimit(); live > peak {
			t.Fatalf("%s: I-C 위반 live=%d > %d", where, live, peak)
		}
		if got, want := s.budget.queued.Load(), reachable(); got < want {
			t.Fatalf("%s: 원장 %d가 도달 가능한 저장 용량 %d보다 적습니다", where, got, want)
		}
		assertLedgerMatchesStructure(t, s, where)
	}

	for round := 0; round < 2; round++ {
		for gi, gvr := range order {
			b := bindings[gvr]
			for i := 0; i < fill; i++ {
				s.enqueueKey(b, "prod", fmt.Sprintf("row-%05d", i))
			}
			if gi%16 == 0 {
				checkCaps(fmt.Sprintf("round%d-fill-%02d", round, gi))
			}
		}
		checkCaps(fmt.Sprintf("round%d-filled", round))

		// 이 픽스처에서는 **모든 GVR이 승인되어야** 합니다. 하나라도 거절되면
		// 아래 드레인이 무엇을 증명하는지 알 수 없습니다.
		s.delta.mu.Lock()
		made := len(s.delta.queues)
		var totalPending int
		for _, gvr := range order {
			q := s.delta.queueOf(gvr)
			if q == nil {
				s.delta.mu.Unlock()
				t.Fatalf("round%d: %s의 큐가 거절되었습니다 — 픽스처가 상한을 넘었습니다",
					round, FormatGVR(gvr))
			}
			totalPending += len(q.events)
		}
		s.delta.mu.Unlock()
		if made != resources {
			t.Fatalf("round%d: 큐가 %d개입니다 — %d개여야 합니다", round, made, resources)
		}
		if totalPending != resources*fill {
			t.Fatalf("round%d: 대기 이벤트가 %d건입니다 — %d건이어야 합니다",
				round, totalPending, resources*fill)
		}

		// **완전히 드레인**합니다(적용 없이 큐만 비웁니다 — 스냅숏이 없으므로).
		s.delta.mu.Lock()
		for _, gvr := range order {
			q := s.delta.queueOf(gvr)
			if q == nil {
				continue
			}
			for _, ev := range q.events {
				s.budget.releaseQueued(ev.reserved)
			}
			q.events = q.events[:0]
			q.reindex()
			s.delta.compactQueueLocked(q)
		}
		s.delta.mu.Unlock()
		checkCaps(fmt.Sprintf("round%d-drained", round))
	}

	// 종단: 전부 폐기하면 0으로 수렴합니다.
	for _, gvr := range order {
		entries[gvr].discard(s)
	}
	if got := s.budget.queued.Load(); got != 0 {
		t.Fatalf("폐기 뒤 queued가 %d입니다", got)
	}
	if got := s.budget.live.Load(); got != 0 {
		t.Fatalf("폐기 뒤 live가 %d입니다", got)
	}
	assertNoNegativeAccounting(t, s, "fill-drain-refill")

	// ── 거절 경로: **명시적 상태**로 남고 과소 계상되지 않아야 합니다. ──────
	//
	// 원장이 0인 지금 상한을 큐 고정 몫 아래로 좁힙니다. 그러면 큐 생성 자체가
	// 거절되고, 그 GVR은 조용히 낡는 대신 **전체 재구성 예약**으로 남아야 합니다.
	s.cfg.MaxSearchIndexBytes = int64(deltaQueueFixedBytes) / 4
	s.budget.max.Store(int64(deltaQueueFixedBytes) / 4)
	victim := order[0]
	ve := entries[victim]
	ve.bootstrapped.Store(true)
	ve.dirty.Store(false)
	// 폐기가 세대를 올렸으므로 콜백 신원을 다시 맞춥니다.
	rejectBinding := &handlerBinding{entry: ve, packed: ve.tokenPacked.Load(), namespaced: true}

	s.enqueueKey(rejectBinding, "prod", "row-00000")

	s.delta.mu.Lock()
	_, exists := s.delta.queues[victim]
	circuits, seed := len(s.delta.circuits), s.delta.circuitsSeed
	s.delta.mu.Unlock()

	if exists {
		t.Fatal("상한 밖인데 큐를 만들었습니다")
	}
	if circuits != 0 || seed != 0 {
		t.Fatalf("거절 경로가 회로=%d 씨앗=%d를 남겼습니다", circuits, seed)
	}
	if ve.bootstrapped.Load() {
		t.Fatal("증분을 쓸 수 없는데 부트스트랩 완료 상태로 남았습니다")
	}
	if !ve.dirty.Load() {
		t.Fatal("거절이 전체 재구성 예약으로 이어지지 않았습니다 — 조용히 낡습니다")
	}
	if got := s.budget.queued.Load(); got != 0 {
		t.Fatalf("만들지 않은 구조가 %d 계상되었습니다", got)
	}
	checkCaps("rejected-gvr")
}

/* ── (c) 2,001행 GVR 안의 1행 대상 ──────────────────────────────────────── */

// TestNamespaceTargetPreflightCoversPinAndSide — **2,001행** GVR 안의 1행 namespace
// 대상이라도, 예약은 그 대상의 실제 데이터(253자 이름, 36자 UID, 63자 키/값 16쌍)를
// 덮어야 하고, 예산이 없으면 **측면 빌더를 만들기 전에** 거절되어야 합니다.
//
// 10만 행 경계는 여기서 만들지 않습니다 — searchround10의
// TestRecoveryReservationCoversWholeGVRRebuild가 순수 함수로 따로 검증합니다.
func TestNamespaceTargetPreflightCoversPinAndSide(t *testing.T) {
	longName := strings.Repeat("n", 253)
	longUID := strings.Repeat("u", 36)
	labels := make(map[string]string, MaxLabelKeysPerObject)
	for i := 0; i < MaxLabelKeysPerObject; i++ {
		labels[fmt.Sprintf("%s/%s%02d", strings.Repeat("d", 63), strings.Repeat("k", 60), i)] =
			strings.Repeat("v", 63)
	}

	rows := make([]*metav1.PartialObjectMetadata, 0, 2001)
	for i := 0; i < 2000; i++ {
		rows = append(rows, metaRow("bulk", fmt.Sprintf("row-%05d", i), fmt.Sprintf("uid-%05d", i), nil))
	}
	target := metaRow("target-ns", longName, longUID, labels)
	rows = append(rows, target)

	h := newDeltaHarnessFor(t, scopedGVR, "Service", true, rows...)
	s := h.svc
	idx := h.entry.baselineIndex()
	if idx == nil {
		t.Fatal("목록 스냅숏이 없습니다")
	}
	sp := idx.namespaceSpan("target-ns")
	if sp.hi-sp.lo != 1 {
		t.Fatalf("대상 구간이 %d행입니다 — 1행이어야 합니다", sp.hi-sp.lo)
	}

	owned, side := recoverySpanCost(idx, sp.lo, sp.hi)

	// 이 대상이 실제로 붙잡는 텍스트를 독립적으로 다시 셉니다.
	var text int64
	text += int64(len("target-ns") + len(longName) + len(longUID))
	for k, v := range labels {
		text += int64(len(k) + len(v))
	}
	if text <= 0 {
		t.Fatal("대상 텍스트가 0입니다")
	}
	if owned < text {
		t.Fatalf("소유 사본 예약 %d가 텍스트 %d보다 작습니다 — 이름/UID/label이 빠졌습니다", owned, text)
	}
	if side < 2*text {
		t.Fatalf("측면 예약 %d가 정규화 토큰 몫 %d보다 작습니다", side, 2*text)
	}
	// 행 수만 곱하던 옛 식보다 반드시 커야 합니다(1행이므로 그 식은 텍스트를 못 봅니다).
	if legacy := int64(1) * (rowRecordFixedBytes + 2*stringHeaderBytes + bootstrapRowInputBytes); owned <= legacy {
		t.Fatalf("소유 사본 예약이 %d로 옛 행 수 기반 식(%d) 이하입니다", owned, legacy)
	}

	// **거절은 빌더 할당보다 먼저**입니다.
	s.requestResync(scopedGVR, "target-ns", 1)
	s.budget.max.Store(1) // 어떤 회수도 예약을 받을 수 없습니다.
	s.cfg.MaxSearchIndexBytes = 1
	s.delta.mu.Lock()
	s.delta.cooldownUntil = time.Time{}
	s.delta.mu.Unlock()

	beforeLive := s.budget.live.Load()
	s.advanceRecovery(context.Background())

	s.delta.mu.Lock()
	tk := s.delta.ticket
	var side0 bool
	if tk != nil {
		side0 = tk.side != nil
	}
	stale, gvrStale := s.delta.queueOf(scopedGVR).staleNS.count, s.delta.queueOf(scopedGVR).gvrStale
	s.delta.mu.Unlock()

	if side0 {
		t.Fatal("예산이 없는데 측면 빌더를 만들었습니다 — 거절이 할당보다 늦습니다")
	}
	if stale == 0 && !gvrStale {
		t.Fatal("거절이 명시적 낡음으로 남지 않았습니다")
	}
	if got := s.budget.live.Load(); got > beforeLive {
		t.Fatalf("거절됐는데 live가 %d → %d로 늘었습니다", beforeLive, got)
	}
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("거절됐는데 회수 예약이 %d 남았습니다", got)
	}
	assertNoNegativeAccounting(t, s, "preflight-reject")
}

// TestNamespaceRecoveryDoesNotPinWholeGVRBacking — namespace 대상 티켓은 GVR 전체
// 행 배열을 붙잡지 않아야 합니다(구간만 소유 사본으로 뜹니다).
func TestNamespaceRecoveryDoesNotPinWholeGVRBacking(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 501)
	for i := 0; i < 500; i++ {
		rows = append(rows, metaRow("bulk", fmt.Sprintf("row-%05d", i), fmt.Sprintf("uid-%05d", i), nil))
	}
	rows = append(rows, metaRow("solo", "only-row", "uid-solo", nil))

	h := newDeltaHarnessFor(t, scopedGVR, "Service", true, rows...)
	pinTicket(t, h, "solo")

	h.svc.delta.mu.Lock()
	tk := h.svc.delta.ticket
	var srcRows, hi int
	var owned recoveryRow
	if tk != nil && len(tk.src) > 0 {
		srcRows, hi = len(tk.src), tk.hi
		owned = tk.src[0]
	}
	h.svc.delta.mu.Unlock()

	if tk == nil {
		t.Fatal("회수 티켓이 잡히지 않았습니다")
	}
	if srcRows != 1 {
		t.Fatalf("소유 사본이 %d행입니다 — 대상 구간(1행)만이어야 합니다", srcRows)
	}
	if hi != 1 {
		t.Fatalf("티켓 hi가 %d입니다 — 소유 사본 기준 1이어야 합니다", hi)
	}
	// 소유 사본은 **검색 전용**입니다: 이름·UID·정규화 토큰만 들고,
	// 원본 객체(annotations·ownerRefs·finalizers)는 붙잡지 않습니다.
	if owned.namespace != "solo" || owned.name != "only-row" || owned.uid != "uid-solo" {
		t.Fatalf("소유 사본의 신원이 어긋납니다: %+v", owned)
	}
}

/* ── (d) 무관한 namespace가 대상 ack을 막지 않습니다 ────────────────────── */

// TestUnrelatedNamespaceDoesNotBlockTargetAck — 회수 중 **다른** namespace에서
// 이벤트가 떨어져도 대상의 ack은 성립해야 하고, **같은** namespace의 더 새로운
// 이벤트는 ack을 막아야 합니다.
func TestUnrelatedNamespaceDoesNotBlockTargetAck(t *testing.T) {
	newHarness := func() *deltaHarness {
		return newDeltaHarnessFor(t, scopedGVR, "Service", true,
			metaRow("target", "row-a", "uid-a", nil),
			metaRow("other", "row-b", "uid-b", nil),
		)
	}

	// ① 무관한 namespace의 드롭이 끼어들어도 대상은 깨끗해집니다.
	h := newHarness()
	s := h.svc
	s.requestResync(scopedGVR, "target", 10)
	s.delta.mu.Lock()
	s.delta.cooldownUntil = time.Time{}
	s.delta.recoveryRR = 0
	tk := s.pickRecoveryLocked(indexBase)
	s.delta.ticket = tk
	s.delta.mu.Unlock()
	if tk == nil || tk.namespace != "target" {
		t.Fatalf("대상 티켓이 잡히지 않았습니다: %+v", tk)
	}
	if tk.markerSeq != 10 {
		t.Fatalf("티켓 마커가 %d입니다 — 대상의 마커(10)여야 합니다", tk.markerSeq)
	}

	// 회수가 도는 동안 **무관한** namespace가 낡습니다.
	s.requestResync(scopedGVR, "other", 77)

	s.finishRecovery(tk, recoveryPublished)
	s.delta.mu.Lock()
	q := s.delta.queueOf(scopedGVR)
	targetStale := q.staleNS.has("target")
	otherStale := q.staleNS.has("other")
	s.delta.mu.Unlock()
	if targetStale {
		t.Fatal("무관한 namespace 이벤트가 대상 ack을 무효로 만들었습니다")
	}
	if !otherStale {
		t.Fatal("무관한 namespace가 함께 지워졌습니다 — 아직 회수하지 않았습니다")
	}

	// ② **같은** namespace의 더 새로운 이벤트는 살아남아야 합니다.
	h2 := newHarness()
	s2 := h2.svc
	s2.requestResync(scopedGVR, "target", 10)
	s2.delta.mu.Lock()
	s2.delta.cooldownUntil = time.Time{}
	s2.delta.recoveryRR = 0
	tk2 := s2.pickRecoveryLocked(indexBase)
	s2.delta.ticket = tk2
	s2.delta.mu.Unlock()
	if tk2 == nil {
		t.Fatal("두 번째 티켓이 잡히지 않았습니다")
	}
	// 같은 대상의 새 입력.
	s2.requestResync(scopedGVR, "target", 99)
	s2.finishRecovery(tk2, recoveryPublished)
	s2.delta.mu.Lock()
	q2 := s2.delta.queueOf(scopedGVR)
	stillStale := q2.staleNS.has("target")
	mark := q2.markerFor(recoveryTarget{gvr: scopedGVR, namespace: "target"})
	s2.delta.mu.Unlock()
	if !stillStale {
		t.Fatal("같은 namespace의 더 새로운 이벤트가 ack에 지워졌습니다 — 유실입니다")
	}
	if mark != 99 {
		t.Fatalf("대상 마커가 %d입니다 — 99여야 합니다", mark)
	}
}

/* ── (e) 넘침 대상이 서로를 reset하지 못합니다 ──────────────────────────── */

// TestOverflowTargetsDoNotResetEachOther — 회로 상한을 넘긴 두 대상이 **같은 가변
// 회로를 공유하지 않아야** 합니다. 공유하면 옆 대상의 다른 마커가 이쪽 회로를
// reset해 백오프 없는 재시도 루프가 됩니다.
func TestOverflowTargetsDoNotResetEachOther(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	s := h.svc
	now := indexBase
	s.cfg.Now = func() time.Time { return now }

	// 회로 맵을 상한까지 채웁니다(모두 열림 + 먼 백오프).
	filler := schema.GroupVersionResource{Group: "filler", Version: "v1", Resource: "things"}
	s.delta.mu.Lock()
	for i := 0; len(s.delta.circuits) < maxCircuits; i++ {
		c, _ := s.delta.circuitFor(recoveryTarget{gvr: filler, namespace: fmt.Sprintf("ns-%05d", i)})
		if c == nil {
			s.delta.mu.Unlock()
			t.Fatal("회로 맵을 채우지 못했습니다")
		}
		c.open, c.notBefore, c.lastNeeded = true, now.Add(time.Hour), 1<<40
	}
	// 처음 보는 GVR 둘이 상한에 부딪힙니다.
	gvrA := schema.GroupVersionResource{Group: "aaa", Version: "v1", Resource: "things"}
	gvrB := schema.GroupVersionResource{Group: "bbb", Version: "v1", Resource: "things"}
	cA, _ := s.delta.circuitFor(recoveryTarget{gvr: gvrA, namespace: "x"})
	cB, _ := s.delta.circuitFor(recoveryTarget{gvr: gvrB, namespace: "y"})
	circuits := len(s.delta.circuits)
	s.delta.mu.Unlock()

	if circuits > maxCircuits {
		t.Fatalf("회로가 %d개입니다 — 상한 %d", circuits, maxCircuits)
	}
	if cA != nil && cA == cB {
		t.Fatal("서로 다른 대상이 **같은 가변 회로**를 공유합니다 — 지문이 서로를 지웁니다")
	}
	if cA == nil || cB == nil {
		return // 자리를 못 만들면 아무 회로도 빌려 주지 않습니다(이것도 통과 조건).
	}

	// A를 열어 두고 B에 **다른 마커**로 실패를 기록합니다. A가 흔들리면 안 됩니다.
	cA.fail(now, 11, 100, 1<<40, s.budget.limit(), 0, true)
	backoffA, markerA, neededA := cA.backoff, cA.lastMarker, cA.lastNeeded
	cB.fail(now, 22, 200, 1<<30, s.budget.limit(), 0, true)

	if !cA.open || cA.lastMarker != markerA || cA.backoff != backoffA || cA.lastNeeded != neededA {
		t.Fatalf("B의 실패가 A의 회로를 흔들었습니다: open=%v marker=%d backoff=%v needed=%d",
			cA.open, cA.lastMarker, cA.backoff, cA.lastNeeded)
	}
	// 지문은 **대상별로** 남아야 합니다.
	if cA.lastMarker != 11 || cB.lastMarker != 22 {
		t.Fatalf("대상별 마커가 섞였습니다: A=%d B=%d", cA.lastMarker, cB.lastMarker)
	}
	if cA.lastRows != 100 || cB.lastRows != 200 {
		t.Fatalf("대상별 행 수가 섞였습니다: A=%d B=%d", cA.lastRows, cB.lastRows)
	}
	later := now.Add(recoveryBackoffMax * 8)

	// 회전 없음: 같은 상태에서 반복 선택해도 회수·전체 빌드가 늘지 않습니다.
	baseAttempts := s.delta.recoveryAttempts.Load()
	baseBoot := s.delta.fullBootstraps.Load()
	for i := 0; i < 8; i++ {
		s.delta.mu.Lock()
		s.delta.cooldownUntil = time.Time{}
		s.delta.ticket = nil
		s.pickRecoveryLocked(later)
		s.delta.mu.Unlock()
	}
	if got := s.delta.fullBootstraps.Load() - baseBoot; got != 0 {
		t.Fatalf("선택만으로 전체 빌드가 %d회 늘었습니다", got)
	}
	if got := s.delta.recoveryAttempts.Load() - baseAttempts; got != 0 {
		t.Fatalf("핀도 하지 않았는데 회수 시도가 %d회 늘었습니다", got)
	}
}

/* ── (f) 폐기·취소 경합에서 정확히 한 번 ────────────────────────────────── */

// TestDiscardCancelRaceReleasesEverythingOnce — 회수가 도는 도중 폐기와 취소가
// 겹쳐도 큐·티켓·원본·측면 예약이 정확히 한 번 풀려야 합니다.
func TestDiscardCancelRaceReleasesEverythingOnce(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 400)
	for i := 0; i < 400; i++ {
		rows = append(rows, metaRow("prod", fmt.Sprintf("row-%04d", i),
			fmt.Sprintf("uid-%04d", i), map[string]string{"app": "payments"}))
	}
	h := newDeltaHarness(t, rows...)
	s := h.svc

	var wg sync.WaitGroup
	stop := make(chan struct{})
	recoveryDone := make(chan struct{})

	// 회수를 계속 돌립니다.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(recoveryDone)
		for i := 0; i < 200; i++ {
			ctx, cancel := context.WithCancel(context.Background())
			if i%3 == 0 {
				cancel() // 취소된 컨텍스트로도 들어갑니다.
			}
			s.requestResync(h.gvr, "prod", uint64(i+1))
			s.delta.mu.Lock()
			s.delta.cooldownUntil = time.Time{}
			s.delta.mu.Unlock()
			s.advanceRecovery(ctx)
			cancel()
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	// 동시에 큐를 채우고 폐기를 반복합니다.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.enqueueKey(h.binding, "prod", fmt.Sprintf("row-%04d", i%400))
			if i%17 == 0 {
				h.entry.discard(s)
				h.entry.install(s, &fakeInformer{store: h.store, synced: true}, nil, nil)
				h.binding = &handlerBinding{entry: h.entry, packed: h.entry.tokenPacked.Load()}
			}
		}
	}()

	<-recoveryDone
	close(stop)
	wg.Wait()

	// 마지막으로 한 번 더 폐기해 모든 소유를 걷습니다.
	h.entry.discard(s)
	h.entry.discard(s)

	assertNoNegativeAccounting(t, s, "discard-cancel-race")
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("회수 예약이 %d 남았습니다", got)
	}
	if got := s.budget.inflight.Load(); got != 0 {
		t.Fatalf("적용 예약이 %d 남았습니다", got)
	}
	if got := s.budget.queued.Load(); got != 0 {
		t.Fatalf("큐 예약이 %d 남았습니다", got)
	}
	s.delta.mu.Lock()
	queues, circuits := len(s.delta.queues), len(s.delta.circuits)
	ticket := s.delta.ticket
	s.delta.mu.Unlock()
	if queues != 0 {
		t.Fatalf("폐기 뒤 큐가 %d개 남았습니다", queues)
	}
	if circuits != 0 {
		t.Fatalf("폐기 뒤 회로가 %d개 남았습니다", circuits)
	}
	if ticket != nil {
		t.Fatal("폐기 뒤 티켓이 남았습니다")
	}
}

// TestDeltaQueueFixedIsAConstantMatchingAllocation — 승인에 쓰는 상수가 실제 할당과
// 어긋나면 "할당 전 승인"이 거짓말이 됩니다.
func TestDeltaQueueFixedIsAConstantMatchingAllocation(t *testing.T) {
	q := newDeltaQueue()
	if q.fixed != deltaQueueFixedBytes {
		t.Fatalf("큐 고정 몫이 %d인데 승인 상수는 %d입니다", q.fixed, int64(deltaQueueFixedBytes))
	}
	want := int64(deltaQueueStructBytes) +
		int64(len(q.staleNS.slots))*(stringHeaderBytes+1+8) +
		int64(q.staleNS.limit)*(stringHeaderBytes+8) + 48 +
		int64(deltaIndexSeedEntries)*deltaIndexEntryBytes
	if q.fixed != want {
		t.Fatalf("승인 상수 %d가 실제 구조 %d와 다릅니다", q.fixed, want)
	}
	if got := cap(q.staleNS.scratch); got != maxStaleTracked {
		t.Fatalf("재사용 버퍼 용량이 %d입니다 — %d여야 합니다", got, maxStaleTracked)
	}
}
