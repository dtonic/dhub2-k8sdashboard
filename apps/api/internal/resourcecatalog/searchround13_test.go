package resourcecatalog

// Round 12 P1 회귀
// --------------------------------------------------------------------------
// 1. 초기 복사 구간도 **작업자**입니다 — 폐기가 끼어들어도 예약을 먼저 놓지 못합니다.
// 2. 회로 승급은 **대상 자체**를 바꿉니다 — 행 수·마커·티켓·보류·ack이 전부 전체 의미.
// 3. 보류 적용은 실제 이벤트·Store 재해석·publishRecovery까지 통과합니다.

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

/* ── 1. 초기 복사 구간의 소유권 ─────────────────────────────────────────── */

// TestInitialCopyHoldsReservationAgainstDiscard — 초기 소유 사본을 뜨는 도중
// 게시/은퇴와 폐기가 겹쳐도, **복사가 끝날 때까지** 회수 예약과 옛 세대가
// 회계에 남아 있어야 합니다. 복사가 끝나면 정확히 0으로 수렴합니다.
func TestInitialCopyHoldsReservationAgainstDiscard(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 300)
	for i := 0; i < 300; i++ {
		rows = append(rows, metaRow("prod", fmt.Sprintf("row-%04d", i),
			fmt.Sprintf("uid-%04d", i), map[string]string{"app": "payments"}))
	}
	h := newDeltaHarness(t, rows...)
	s := h.svc
	e := h.entry

	oldLease := e.leasePtr.Load()
	if oldLease == nil {
		t.Fatal("게시된 세대가 없습니다")
	}
	oldBytes := oldLease.bytes

	// 복사 구간을 붙잡는 장벽을 심습니다(테스트 seam).
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.delta.mu.Lock()
	s.delta.copyBarrier = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	s.delta.mu.Unlock()

	s.requestResync(h.gvr, "prod", 0)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.advanceRecovery(context.Background())
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("초기 복사 구간에 진입하지 않았습니다")
	}

	// 복사가 붙잡힌 상태입니다. 예약이 이미 잡혀 있어야 합니다.
	s.delta.mu.Lock()
	tk := s.delta.ticket
	var reserved int64
	var workers int
	if tk != nil {
		reserved, workers = tk.reserved, tk.workers
	}
	s.delta.mu.Unlock()
	if tk == nil {
		close(release)
		wg.Wait()
		t.Fatal("티켓이 만들어지지 않았습니다")
	}
	if reserved <= 0 {
		close(release)
		wg.Wait()
		t.Fatalf("회수 예약이 %d입니다 — 복사 전에 잡혀 있어야 합니다", reserved)
	}
	if workers != 1 {
		close(release)
		wg.Wait()
		t.Fatalf("작업자 수가 %d입니다 — 초기 복사도 작업자여야 합니다", workers)
	}

	// 같은 순간 새 세대를 게시하고(옛 세대 은퇴) 폐기까지 겹칩니다.
	//
	// **무관한 요청 독자를 심지 않습니다.** 옛 세대를 붙잡고 있는 것은 회수 캡처
	// 자신이어야 합니다 — 그것이 이 회귀가 증명하려는 소유권입니다.
	built := buildSearchIndex(indexOf(rows...), "Service", true, hugeBudget, hugeBudget)
	if built.state != SearchReady {
		close(release)
		wg.Wait()
		t.Fatalf("재빌드 실패: %s", built.state)
	}
	cur := e.load()
	next := &entrySnapshot{
		index: cur.index, sindex: built.index, searchState: SearchReady,
		indexVer: cur.indexVer + 1, searchVer: cur.searchVer + 1,
	}
	s.snapMu.Lock()
	if s.publishLeaseLocked(e, next) {
		e.setSnap(next)
	}
	s.snapMu.Unlock()
	e.discard(s)

	// **아직 복사 중입니다.** 예약은 풀리면 안 됩니다.
	if got := s.budget.recovery.Load(); got < reserved {
		close(release)
		wg.Wait()
		t.Fatalf("복사 중인데 회수 예약이 %d로 줄었습니다(예약 %d)", got, reserved)
	}
	// 옛 세대는 **회수 캡처가 빌려 두었으므로** 은퇴했어도 회계에 남아야 합니다.
	if oldLease.released.Load() {
		close(release)
		wg.Wait()
		t.Fatal("회수가 캡처 중인데 옛 세대가 회계에서 빠졌습니다 — 소유권 없이 스냅숏을 쓰고 있습니다")
	}
	if got := s.searchBytes.Load(); got < oldBytes {
		close(release)
		wg.Wait()
		t.Fatalf("캡처 중인데 회계가 %d로 줄었습니다(옛 세대 %d)", got, oldBytes)
	}

	// 장벽을 풉니다.
	close(release)
	wg.Wait()

	// 캡처가 끝나면 빌린 세대가 정확히 한 번 풀립니다.
	if !oldLease.released.Load() {
		t.Fatal("캡처가 끝났는데 빌린 세대가 풀리지 않았습니다")
	}
	if got := s.searchBytes.Load(); got != 0 {
		t.Fatalf("폐기·해제 뒤 검색 회계가 %d입니다", got)
	}

	s.delta.mu.Lock()
	after := s.delta.ticket
	s.delta.mu.Unlock()
	if after != nil {
		t.Fatal("폐기된 뒤에 티켓이 되살아났습니다")
	}
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("복사가 끝났는데 회수 예약이 %d 남았습니다", got)
	}
	assertNoNegativeAccounting(t, s, "initial-copy-barrier")

	// 한 번 더 폐기해도 이중 해제가 없어야 합니다.
	e.discard(s)
	assertNoNegativeAccounting(t, s, "initial-copy-double-discard")
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("두 번째 폐기 뒤 회수 예약이 %d입니다", got)
	}
}

/* ── 2. 승급된 회로는 대상 자체를 전체로 바꿉니다 ───────────────────────── */

// fillNamespaceCircuits는 그 GVR의 namespace 회로로 맵을 상한까지 채웁니다.
func fillNamespaceCircuits(t *testing.T, s *Service, gvr schema.GroupVersionResource, open bool, until time.Time) {
	t.Helper()
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	for i := 0; len(s.delta.circuits) < maxCircuits; i++ {
		c, _ := s.delta.circuitFor(recoveryTarget{gvr: gvr, namespace: fmt.Sprintf("fill-%05d", i)})
		if c == nil {
			t.Fatalf("회로 맵을 채우지 못했습니다(%d개)", len(s.delta.circuits))
		}
		if open {
			c.open, c.notBefore, c.backoff = true, until, recoveryBackoffMax
		}
	}
}

// TestPromotedCircuitProducesWholeGVRTicket — 같은 GVR의 namespace 회로로 맵이
// 가득 찬 상태에서 새 namespace를 요청하면, 접기로 승급되어 **전체 GVR 티켓**이
// 나와야 합니다. 전역 마커와 전체 행 수를 들어야 하고, 실패·성공 처리도 전부
// 전체 의미를 따라야 합니다.
func TestPromotedCircuitProducesWholeGVRTicket(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 40)
	for i := 0; i < 20; i++ {
		rows = append(rows, metaRow("alpha", fmt.Sprintf("a-%03d", i), fmt.Sprintf("uid-a-%03d", i), nil))
		rows = append(rows, metaRow("beta", fmt.Sprintf("b-%03d", i), fmt.Sprintf("uid-b-%03d", i), nil))
	}
	h := newDeltaHarness(t, rows...)
	s := h.svc
	now := indexBase
	s.cfg.Now = func() time.Time { return now }

	// 이 GVR의 namespace 회로로 맵을 가득 채웁니다.
	fillNamespaceCircuits(t, s, h.gvr, false, now)

	// 아직 회로가 없는 namespace 하나를 낡음으로 만듭니다.
	s.requestResync(h.gvr, "alpha", 42)

	s.delta.mu.Lock()
	s.delta.cooldownUntil = time.Time{}
	s.delta.recoveryRR = 0
	tk := s.pickRecoveryLocked(now)
	s.delta.ticket = tk
	q := s.delta.queueOf(h.gvr)
	globalMarker := q.markerSeq
	s.delta.mu.Unlock()

	if tk == nil {
		t.Fatal("회수를 잡지 못했습니다")
	}
	/* (a) 승급된 티켓은 **전체 GVR**입니다. */
	if !tk.wholeGVR || tk.namespace != "" {
		t.Fatalf("승급됐는데 티켓이 whole=%v namespace=%q입니다", tk.wholeGVR, tk.namespace)
	}
	if tk.markerSeq != globalMarker {
		t.Fatalf("티켓 마커가 %d입니다 — 전역 마커 %d여야 합니다", tk.markerSeq, globalMarker)
	}
	if span := s.targetRowSpan(tk.target()); span != len(rows) {
		t.Fatalf("대상 행 수가 %d입니다 — 전체 %d행이어야 합니다", span, len(rows))
	}
	// 전체 대상은 **모든 namespace의 이벤트를 보류**합니다(핀이 활성화하는 플래그).
	s.delta.mu.Lock()
	tk.holdActive = true
	s.delta.mu.Unlock()
	if !tk.holds(h.gvr, "beta") {
		t.Fatal("전체 티켓이 다른 namespace를 보류하지 않습니다")
	}

	/* (b) 실패하면 전체 회로에 백오프가 남고, 같은 입력에서 다시 잡히지 않습니다. */
	s.finishRecovery(tk, recoveryBudgetRejected)
	whole := recoveryTarget{gvr: h.gvr, whole: true}
	s.delta.mu.Lock()
	c := s.delta.circuits[whole]
	var open bool
	var lastMarker uint64
	var lastRows int
	if c != nil {
		open, lastMarker, lastRows = c.open, c.lastMarker, c.lastRows
	}
	s.delta.mu.Unlock()
	if c == nil {
		t.Fatal("전체 대상 회로가 없습니다")
	}
	if !open {
		t.Fatal("예산 거절인데 전체 회로가 열리지 않았습니다")
	}
	if lastMarker != globalMarker {
		t.Fatalf("전체 회로 지문이 %d입니다 — 전역 마커 %d여야 합니다(namespace 마커 금지)",
			lastMarker, globalMarker)
	}
	_ = lastRows

	// **백오프가 지난 뒤에도** 잡히지 않아야 합니다. 시계를 멈춰 두면 거절 사유가
	// 회로가 아니라 시간이 되어 아무것도 증명하지 못합니다.
	later := now.Add(recoveryBackoffMax * 8)
	picked := 0
	for i := 0; i < 6; i++ {
		s.delta.mu.Lock()
		s.delta.ticket = nil
		if got := s.pickRecoveryLocked(later); got != nil {
			picked++
			s.delta.ticket = got
		}
		s.delta.mu.Unlock()
	}
	if picked != 0 {
		t.Fatalf("입력·용량이 그대로인데 %d회 다시 잡았습니다", picked)
	}

	/* (c) 성공하면 전체 키 회로가 사라집니다. */
	s.delta.mu.Lock()
	s.delta.ticket = nil
	s.delta.cooldownUntil = time.Time{}
	s.delta.circuits[whole].reset()
	q = s.delta.queueOf(h.gvr)
	fresh := &recoveryTicket{
		gvr: h.gvr, wholeGVR: true, epoch: q.staleEpoch,
		markerSeq: q.markerSeq, markerCaptured: true,
	}
	s.delta.ticket = fresh
	s.delta.mu.Unlock()

	s.finishRecovery(fresh, recoveryPublished)
	s.delta.mu.Lock()
	_, stillThere := s.delta.circuits[whole]
	fallbackOpen := s.delta.queueOf(h.gvr).fallback.open
	s.delta.mu.Unlock()
	if stillThere {
		t.Fatal("전체 회수가 성공했는데 전체 키 회로가 남았습니다")
	}
	if fallbackOpen {
		t.Fatal("성공했는데 내장 회로가 열린 채입니다")
	}
}

// TestSeedRejectionYieldsWholeGVRTicket — 회로 맵 씨앗을 승인받지 못하면 내장
// 회로로 되돌아갑니다. 그때 나오는 티켓은 **지문만 whole**이 아니라 실제로
// 전체 GVR 티켓이어야 합니다.
func TestSeedRejectionYieldsWholeGVRTicket(t *testing.T) {
	rows := []*metav1.PartialObjectMetadata{
		metaRow("alpha", "a-0", "uid-a", nil),
		metaRow("beta", "b-0", "uid-b", nil),
	}
	h := newDeltaHarness(t, rows...)
	s := h.svc
	now := indexBase
	s.cfg.Now = func() time.Time { return now }

	s.requestResync(h.gvr, "alpha", 7)

	// 남은 용량을 회로 씨앗보다 적게, 티켓 구조보다는 크게 둡니다.
	live := s.budget.live.Load()
	const slack = 2048 // 512..4351 구간
	s.budget.max.Store((live + slack) / searchPeakMultiplier)
	if avail := s.budget.peakLimit() - s.budget.live.Load(); avail >= circuitsMapSeedBytes || avail < recoveryTicketBytes {
		t.Fatalf("픽스처가 어긋났습니다: 여유 %d (씨앗 %d, 티켓 %d)",
			avail, int64(circuitsMapSeedBytes), int64(recoveryTicketBytes))
	}

	s.delta.mu.Lock()
	s.delta.cooldownUntil = time.Time{}
	s.delta.recoveryRR = 0
	tk := s.pickRecoveryLocked(now)
	circuits, seed := len(s.delta.circuits), s.delta.circuitsSeed
	q := s.delta.queueOf(h.gvr)
	globalMarker := q.markerSeq
	s.delta.mu.Unlock()

	if circuits != 0 || seed != 0 {
		t.Fatalf("씨앗을 못 만들어야 하는데 회로=%d 씨앗=%d입니다", circuits, seed)
	}
	if tk == nil {
		t.Fatal("내장 회로로도 회수를 잡지 못했습니다")
	}
	if !tk.wholeGVR || tk.namespace != "" {
		t.Fatalf("내장 회로 경로의 티켓이 whole=%v namespace=%q입니다 — 실제 전체 티켓이어야 합니다",
			tk.wholeGVR, tk.namespace)
	}
	if tk.markerSeq != globalMarker {
		t.Fatalf("티켓 마커가 %d입니다 — 전역 마커 %d여야 합니다", tk.markerSeq, globalMarker)
	}
	if span := s.targetRowSpan(tk.target()); span != len(rows) {
		t.Fatalf("대상 행 수가 %d입니다 — 전체 %d행이어야 합니다", span, len(rows))
	}
	s.delta.mu.Lock()
	s.delta.ticket = tk
	tk.holdActive = true
	s.delta.mu.Unlock()
	if !tk.holds(h.gvr, "beta") {
		t.Fatal("전체 티켓이 다른 namespace를 보류하지 않습니다")
	}

	// 실패하면 내장 회로에 백오프가 남고 반복 선택이 없어야 합니다.
	s.finishRecovery(tk, recoveryBudgetRejected)
	s.delta.mu.Lock()
	fb := s.delta.queueOf(h.gvr).fallback
	s.delta.mu.Unlock()
	if !fb.open || fb.backoff <= 0 {
		t.Fatalf("내장 회로에 백오프가 남지 않았습니다: open=%v backoff=%v", fb.open, fb.backoff)
	}
	// **백오프가 지난 뒤에도** 잡히지 않아야 합니다(거절 사유는 시간이 아니라 회로).
	later := now.Add(recoveryBackoffMax * 8)
	picked := 0
	for i := 0; i < 6; i++ {
		s.delta.mu.Lock()
		s.delta.ticket = nil
		if got := s.pickRecoveryLocked(later); got != nil {
			picked++
			s.delta.ticket = got
		}
		s.delta.mu.Unlock()
	}
	if picked != 0 {
		t.Fatalf("입력·용량이 그대로인데 %d회 다시 잡았습니다", picked)
	}
}

/* ── 3. 보류 적용: 실제 이벤트 → Store 재해석 → publishRecovery ─────────── */

// pinWholeGVRTicket은 **프로덕션 경로만으로** 전체 GVR 회수를 핀합니다.
//
// markGVRStale은 새 eventSeq로 마커를 세웁니다. 목록 스냅숏의 coversThroughSeq가
// 그 마커를 덮기 전에는 회수가 정당하게 기다리므로(recoveryWaitingCover), 여기서
// **실제 목록 재구성**을 한 번 돌려 커버리지 장벽을 통과시킨 뒤 핀합니다.
// 재구성은 이미 부트스트랩된 항목이라 검색 절반을 그대로 옮겨 담습니다
// (목록만 갱신 → ack 없음 → 낡음 마커가 살아남습니다).
// **쿨다운·백오프를 손으로 지우지 않습니다.** 시계를 앞으로 돌려 스케줄러가
// 정당하게 허용하는 상태를 만든 뒤 tick을 돌립니다.
func pinWholeGVRTicket(t *testing.T, h *deltaHarness) *recoveryTicket {
	t.Helper()
	h.svc.markGVRStale(h.gvr)
	h.svc.rebuildIndexes(true)

	for i := 0; i < 8; i++ {
		h.svc.advanceRecovery(context.Background())

		h.svc.delta.mu.Lock()
		tk := h.svc.delta.ticket
		ready := tk != nil && tk.phase == recoveryBuilding && tk.wholeGVR && tk.src != nil
		h.svc.delta.mu.Unlock()
		if ready {
			return tk
		}
	}

	h.svc.delta.mu.Lock()
	tk := h.svc.delta.ticket
	var phase recoveryPhase
	var covers, marker uint64
	var open bool
	var lastMarker uint64
	var notBefore, cooldown time.Time
	if tk != nil {
		phase = tk.phase
	}
	if q := h.svc.delta.queueOf(h.gvr); q != nil {
		marker = q.markerSeq
		if c, eff := h.svc.delta.circuitForQ(q, recoveryTarget{gvr: h.gvr, whole: true}); c != nil {
			open, lastMarker, notBefore = c.open, c.lastMarker, c.notBefore
			_ = eff
		}
	}
	cooldown = h.svc.delta.cooldownUntil
	h.svc.delta.mu.Unlock()
	if snap := h.entry.load(); snap != nil {
		covers = snap.coversThroughSeq
	}
	t.Fatalf("전체 회수가 핀되지 않았습니다: ticket=%v phase=%v covers=%d marker=%d "+
		"circuit(open=%v lastMarker=%d notBefore=%v) cooldown=%v now=%v",
		tk != nil, phase, covers, marker, open, lastMarker, notBefore, cooldown, h.svc.nowOrDefault())
	return nil
}

// TestHeldReservationIsIdempotentAndReleasedOnce — 보류 예약 API 자체의 성질입니다.
//
// **실제로 핀된 티켓**에 대해: 같은 크기를 두 번 요청해도 늘지 않고, 티켓을 걷으면
// 보류 몫까지 정확히 한 번 풀립니다(수동으로 죽은 티켓을 되살리지 않습니다).
func TestHeldReservationIsIdempotentAndReleasedOnce(t *testing.T) {
	h := newDeltaHarness(t,
		metaRow("ns-0", "seed-0", "uid-seed-0", nil),
		metaRow("ns-1", "seed-1", "uid-seed-1", nil),
	)
	s := h.svc
	tk := pinWholeGVRTicket(t, h)

	s.delta.mu.Lock()
	step, beforeReserved := tk.step, tk.reserved
	s.delta.mu.Unlock()
	if beforeReserved <= 0 {
		t.Fatalf("핀 예약이 %d입니다", beforeReserved)
	}

	const held = 6
	if ok := s.growHeldReservation(tk, step, held); !ok {
		t.Fatal("용량이 충분한데 보류 예약이 거절되었습니다")
	}
	s.delta.mu.Lock()
	afterHeld, afterReserved := tk.heldReserved, tk.reserved
	s.delta.mu.Unlock()

	want := heldApplyReserveBytes(held)
	if afterHeld != want {
		t.Fatalf("보류 예약이 %d입니다 — %d여야 합니다", afterHeld, want)
	}
	if afterReserved != beforeReserved+want {
		t.Fatalf("티켓 예약이 %d입니다 — %d + %d여야 합니다", afterReserved, beforeReserved, want)
	}
	if got := s.budget.recovery.Load(); got < afterReserved {
		t.Fatalf("원장 회수 예약(%d)이 티켓 예약(%d)보다 작습니다", got, afterReserved)
	}

	// 같은 크기를 다시 요청해도 **늘지 않습니다**(중복 예약 금지).
	if ok := s.growHeldReservation(tk, step, held); !ok {
		t.Fatal("두 번째 요청이 거절되었습니다")
	}
	s.delta.mu.Lock()
	twice := tk.reserved
	s.delta.mu.Unlock()
	if twice != afterReserved {
		t.Fatalf("같은 크기인데 예약이 %d → %d로 늘었습니다", afterReserved, twice)
	}

	// 더 큰 요청은 **차이만** 늘립니다.
	if ok := s.growHeldReservation(tk, step, held*2); !ok {
		t.Fatal("더 큰 보류 예약이 거절되었습니다")
	}
	s.delta.mu.Lock()
	grown := tk.reserved
	grownHeld := tk.heldReserved
	s.delta.mu.Unlock()
	bigger := heldApplyReserveBytes(held * 2)
	if grownHeld != bigger {
		t.Fatalf("보류 예약이 %d입니다 — %d여야 합니다", grownHeld, bigger)
	}
	if grown != afterReserved+(bigger-want) {
		t.Fatalf("티켓 예약이 %d입니다 — 차이(%d)만 늘어야 합니다", grown, bigger-want)
	}

	// 티켓을 걷으면 보류 몫까지 **정확히 한 번** 풀립니다.
	s.delta.mu.Lock()
	s.dropTicketLocked(tk)
	leftHeld, leftReserved := tk.heldReserved, tk.reserved
	s.delta.mu.Unlock()
	if leftHeld != 0 || leftReserved != 0 {
		t.Fatalf("폐기 뒤 보류=%d 예약=%d가 남았습니다", leftHeld, leftReserved)
	}
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("폐기 뒤 회수 예약이 %d 남았습니다", got)
	}
	assertNoNegativeAccounting(t, s, "held-reserve-api")
}

// TestHeldApplyReservationCoversMultiNamespaceCreates — 여러 namespace에 걸친
// 최대 label 보류 **생성**이, 용량이 모자라면 publishRecovery에 들어가기 전에
// 거절되고(적용 할당 없음), 충분하면 게시된 뒤 예약이 정확히 0으로 돌아옵니다.
func TestHeldApplyReservationCoversMultiNamespaceCreates(t *testing.T) {
	labels := make(map[string]string, MaxLabelKeysPerObject)
	for i := 0; i < MaxLabelKeysPerObject; i++ {
		labels[fmt.Sprintf("%s/key%02d", strings.Repeat("d", 63), i)] = strings.Repeat("v", 63)
	}

	// 예약 식은 **델타 배치 식 + 루트 공존**이어야 합니다. 조각 식(입력만)으로는
	// 키당 정규화 토큰 사본·맵/슬라이스·다중 파티션 COW·새 행을 덮지 못합니다.
	const probe = 8
	if got, batch := heldApplyReserveBytes(probe), deltaTransientBytes(probe); got <= batch {
		t.Fatalf("보류 예약 %d가 델타 배치 예약 %d 이하입니다 — 루트 공존 몫이 빠졌습니다", got, batch)
	}
	if got, chunk := heldApplyReserveBytes(probe), recoveryChunkReserveBytes(probe); got <= chunk {
		t.Fatalf("보류 예약 %d가 조각 예약 %d 이하입니다 — 토큰·맵·COW가 빠졌습니다", got, chunk)
	}

	seed := []*metav1.PartialObjectMetadata{
		metaRow("ns-0", "seed-0", "uid-seed-0", nil),
		metaRow("ns-1", "seed-1", "uid-seed-1", nil),
	}
	h := newDeltaHarness(t, seed...)
	s := h.svc
	// **움직이는 시계**를 씁니다. 하네스 기본값은 고정 시계라 거절이 남긴
	// 백오프·쿨다운이 영원히 지나가지 않습니다 — 그러면 두 번째 회수가 잡히지
	// 않는 것이 정상입니다(프로덕션이 옳고 픽스처가 시간을 멈춘 것입니다).
	now := indexBase
	s.cfg.Now = func() time.Time { return now }

	// 전체 GVR 회수를 잡습니다(모든 namespace의 이벤트를 보류합니다).
	// **실제 목록 재구성으로 커버리지 장벽을 통과시킵니다** — markGVRStale이 세운
	// 마커를 목록 스냅숏이 덮기 전에는 회수가 정당하게 대기합니다.
	tk := pinWholeGVRTicket(t, h)
	if !tk.holdActive || len(tk.src) != len(seed) {
		t.Fatalf("핀 상태가 어긋났습니다: holdActive=%v src=%d행", tk.holdActive, len(tk.src))
	}
	// 전체 대상이므로 모든 namespace의 이벤트를 보류합니다.
	if !tk.holds(h.gvr, "ns-2") {
		t.Fatal("전체 티켓이 다른 namespace를 보류하지 않습니다")
	}

	// **실제 보류 이벤트**를 여러 namespace에 만듭니다(Store에도 넣어 재해석되게).
	const heldCount = 6
	for i := 0; i < heldCount; i++ {
		ns := fmt.Sprintf("ns-%d", i%3)
		obj := metaRow(ns, fmt.Sprintf("held-%02d", i), fmt.Sprintf("uid-held-%02d", i), labels)
		if err := h.store.Add(obj); err != nil {
			t.Fatal(err)
		}
		s.enqueueKey(h.binding, ns, obj.Name)
		h.flush(t) // 보류로 들어갑니다.
	}
	s.delta.mu.Lock()
	held := len(s.delta.queueOf(h.gvr).hold)
	s.delta.mu.Unlock()
	if held != heldCount {
		t.Fatalf("보류가 %d건입니다 — %d건이어야 합니다", held, heldCount)
	}

	// ① 용량을 조여 **보류 적용 예약**만 실패하게 합니다.
	//    조각 예약은 핀에서 이미 확보했으므로 조각은 통과하고, 보류 예약에서 걸립니다.
	beforeVer := h.entry.load().searchVer
	beforeResync := s.delta.partitionResyncs.Load() + s.delta.fullRecoveries.Load()
	s.budget.max.Store(1)

	for i := 0; i < 6; i++ {
		s.delta.mu.Lock()
		alive := s.delta.ticket != nil
		s.delta.mu.Unlock()
		if !alive {
			break
		}
		s.advanceRecovery(context.Background())
	}

	s.delta.mu.Lock()
	afterTicket := s.delta.ticket
	gvrStale := s.delta.queueOf(h.gvr).gvrStale
	s.delta.mu.Unlock()

	if afterTicket != nil {
		t.Fatal("보류 예약이 거절됐는데 티켓이 남았습니다")
	}
	if !gvrStale {
		t.Fatal("거절이 명시적 낡음으로 남지 않았습니다")
	}
	if got := h.entry.load().searchVer; got != beforeVer {
		t.Fatalf("거절됐는데 검색 세대가 %d → %d로 게시되었습니다", beforeVer, got)
	}
	if got := s.delta.partitionResyncs.Load() + s.delta.fullRecoveries.Load(); got != beforeResync {
		t.Fatalf("거절됐는데 회수 성공이 %d회 집계되었습니다", got-beforeResync)
	}
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("거절 뒤 회수 예약이 %d 남았습니다", got)
	}
	assertNoNegativeAccounting(t, s, "held-reject")

	// ② 용량을 되돌리면 보류가 **실제로 게시되고** 예약이 정확히 0으로 돌아옵니다.
	//
	// 거절 경로는 티켓을 걷으면서 보류분의 예약을 정확히 한 번 풀고 그 이벤트를
	// 버립니다(정확성은 gvrStale이 보장합니다). 그러므로 두 번째 라운드는
	// **새 커버리지 장벽을 다시 통과해** 핀하고, 그 뒤에 도착한 이벤트를
	// 보류로 만들어 publishRecovery의 보류 적용 경로를 그대로 태웁니다.
	// 거절은 **전체 대상 회로**에 백오프와 입력 지문을 남겼습니다.
	// 이미 있는 항목을 맵에서 직접 읽습니다 — circuitFor로 읽으면 없을 때 만들어져
	// 관측이 상태를 바꿉니다.
	whole := recoveryTarget{gvr: h.gvr, whole: true}
	s.delta.mu.Lock()
	c0 := s.delta.circuits[whole]
	var openBefore bool
	var fingerprint uint64
	var backoff time.Duration
	if c0 != nil {
		openBefore, fingerprint, backoff = c0.open, c0.lastMarker, c0.backoff
	}
	markerBefore := s.delta.queueOf(h.gvr).markerSeq
	s.delta.mu.Unlock()
	if c0 == nil {
		t.Fatal("예산 거절이 전체 대상 회로에 기록되지 않았습니다")
	}
	if !openBefore || backoff <= 0 {
		t.Fatalf("거절인데 백오프가 남지 않았습니다: open=%v backoff=%v", openBefore, backoff)
	}
	if fingerprint != markerBefore {
		t.Fatalf("회로 지문이 %d입니다 — 전역 마커 %d여야 합니다(전체 대상)", fingerprint, markerBefore)
	}

	// 이제 용량을 되돌리고, 남은 백오프·쿨다운은 **시간으로** 지나가게 합니다.
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	s.cfg.MaxSearchIndexBytes = DefaultMaxSearchIndexBytes
	now = now.Add(recoveryBackoffMax * 8)

	tk2 := pinWholeGVRTicket(t, h)
	if tk2 == nil {
		t.Fatal("두 번째 회수가 핀되지 않았습니다")
	}
	// **재개 사유를 못박습니다**: markGVRStale이 세운 새 입력 마커가 회로가 기억하던
	// 지문과 달라 회로가 닫혔습니다(용량 회복도 독립적으로 같은 효과를 냅니다).
	s.delta.mu.Lock()
	markerAfter := s.delta.queueOf(h.gvr).markerSeq
	reopened := !c0.open
	s.delta.mu.Unlock()
	if markerAfter <= markerBefore {
		t.Fatalf("입력 마커가 %d → %d입니다 — 재개 사유가 없습니다", markerBefore, markerAfter)
	}
	if markerAfter == fingerprint {
		t.Fatalf("새 마커가 회로 지문(%d)과 같습니다 — 입력이 달라지지 않았습니다", fingerprint)
	}
	if !reopened {
		t.Fatal("재개했는데 회로가 열린 채입니다 — 지문 변화가 회로를 닫지 않았습니다")
	}
	if tk2.markerSeq != markerAfter {
		t.Fatalf("티켓이 마커 %d를 들었습니다 — 전역 마커 %d여야 합니다", tk2.markerSeq, markerAfter)
	}

	// 핀 **이후**에 도착한 이벤트만 보류가 됩니다 — 원본 구간에 없던 생성입니다.
	const lateCount = 3
	for i := 0; i < lateCount; i++ {
		ns := fmt.Sprintf("ns-%d", i%3)
		obj := metaRow(ns, fmt.Sprintf("late-%02d", i), fmt.Sprintf("uid-late-%02d", i), labels)
		if err := h.store.Add(obj); err != nil {
			t.Fatal(err)
		}
		s.enqueueKey(h.binding, ns, obj.Name)
		h.flush(t) // 보류로 들어갑니다.
	}
	s.delta.mu.Lock()
	heldAgain := len(s.delta.queueOf(h.gvr).hold)
	s.delta.mu.Unlock()
	if heldAgain != lateCount {
		t.Fatalf("두 번째 라운드 보류가 %d건입니다 — %d건이어야 합니다", heldAgain, lateCount)
	}

	beforeVer2 := h.entry.load().searchVer
	for i := 0; i < 12; i++ {
		s.delta.mu.Lock()
		done := s.delta.ticket == nil
		s.delta.mu.Unlock()
		if done {
			break
		}
		s.advanceRecovery(context.Background())
	}

	if got := s.delta.fullRecoveries.Load(); got == 0 {
		t.Fatal("용량이 충분한데 전체 회수가 게시되지 않았습니다")
	}
	if got := h.entry.load().searchVer; got <= beforeVer2 {
		t.Fatalf("게시됐다는데 검색 세대가 %d 그대로입니다", got)
	}
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("게시 뒤 회수 예약이 %d 남았습니다", got)
	}
	if got := s.budget.inflight.Load(); got != 0 {
		t.Fatalf("게시 뒤 적용 예약이 %d 남았습니다", got)
	}
	s.delta.mu.Lock()
	leftHold := len(s.delta.queueOf(h.gvr).hold)
	s.delta.mu.Unlock()
	if leftHold != 0 {
		t.Fatalf("게시 뒤 보류가 %d건 남았습니다", leftHold)
	}
	assertNoNegativeAccounting(t, s, "held-publish")

	// **보류로만 들어온 행**이 검색됩니다 — Store 재해석과 보류 적용이 통했다는 증거입니다.
	page, err := s.Search(SearchRequest{Query: "late", Namespaces: allNS()})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != lateCount {
		t.Fatalf("보류로 만들어진 행이 %d건 검색됩니다 — %d건이어야 합니다", len(page.Items), lateCount)
	}
	// 1라운드에서 버려졌던 키도 전체 회수가 원본에서 되살립니다.
	if page, err = s.Search(SearchRequest{Query: "held", Namespaces: allNS()}); err != nil {
		t.Fatal(err)
	} else if len(page.Items) != heldCount {
		t.Fatalf("원본에서 되살린 행이 %d건입니다 — %d건이어야 합니다", len(page.Items), heldCount)
	}
}
