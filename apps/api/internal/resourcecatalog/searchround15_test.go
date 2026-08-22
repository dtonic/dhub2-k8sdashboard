package resourcecatalog

// Round 15 — 델타 루프·회수 종단·회로 접기·트리 재균형
// --------------------------------------------------------------------------
// 여기서 보는 것은 **상태 전이와 소유권**입니다. 줄을 밟기 위한 호출이 아니라,
// 각 테스트가 하나씩 증명합니다.
//
//	① 루프는 주입된 ticker로만 돌고, ctx가 끝나면 ticker를 정확히 한 번 멈춘다.
//	② 예산이 한 건도 감당하지 못하면 조용히 낡지 않고 **명시적 stale**로 바뀐다.
//	③ 회수 예약은 버려지든 실패하든 **정확히 한 번** 풀린다.
//	④ 회로 접기는 **가장 보수적인 지문**을 물려받고 자리를 실제로 만든다.
//	⑤ 리프 상한을 넘는 삽입·삭제가 중복도 누락도 만들지 않는다.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

/* ── 공용 도우미 ────────────────────────────────────────────────────────── */

// drainQueueFully는 큐가 빌 때까지 flush를 반복합니다(유계).
//
// 한 번의 flush는 maxBatchPerResource와 예산에 따라 배치를 줄일 수 있으므로,
// "몇 번에 끝나는가"가 아니라 "끝난다"를 봅니다.
func drainQueueFully(t *testing.T, h *deltaHarness) int {
	t.Helper()
	total := 0
	for round := 0; round < 64; round++ {
		if h.queueLen() == 0 {
			return total
		}
		n := h.flush(t)
		total += n
		if n == 0 {
			t.Fatalf("큐에 %d건이 남았는데 flush가 한 건도 처리하지 못했습니다", h.queueLen())
		}
	}
	t.Fatalf("64회 flush 뒤에도 큐에 %d건이 남았습니다", h.queueLen())
	return total
}

// currentTicket은 지금 살아 있는 티켓과 그 step을 잠금 아래에서 집습니다.
func currentTicket(s *Service) (*recoveryTicket, uint64) {
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	t := s.delta.ticket
	if t == nil {
		return nil, 0
	}
	return t, t.step
}

// pinRecoveryTicket은 회수 티켓 하나를 **실제 생산 경로로** 만들어 고정 단계까지
// 진행합니다. seq 0은 하네스 스냅숏이 이미 덮고 있으므로 커버 게이트를 통과합니다.
func pinRecoveryTicket(t *testing.T, h *deltaHarness, namespace string) *recoveryTicket {
	t.Helper()
	h.svc.requestResync(h.gvr, namespace, 0)
	h.svc.advanceRecovery(context.Background())
	tk, _ := currentTicket(h.svc)
	if tk == nil {
		t.Fatal("회수 티켓이 만들어지지 않았습니다 — 회로·쿨다운이 막고 있습니다")
	}
	if !tk.holdActive {
		t.Fatal("티켓이 보류를 켜지 않았습니다 — 회수 중 델타가 게시를 어긋나게 합니다")
	}
	if tk.reserved <= 0 {
		t.Fatalf("고정 단계인데 회수 예약이 %d입니다", tk.reserved)
	}
	return tk
}

// assertMembershipExact는 여러 페이지 크기로 훑어 **개수와 중복 없음**을 함께 봅니다.
// 페이지 경계에서 같은 행이 두 번 나가거나 빠지는 것은 개수만으로도 드러납니다.
func assertMembershipExact(t *testing.T, s *Service, query string, want int) {
	t.Helper()
	for _, limit := range []int{7, MaxSearchPageSize} {
		got := foldPage(t, s, query, limit)
		if len(got) != want {
			t.Fatalf("limit=%d 결과가 %d건입니다 — %d건이어야 합니다", limit, len(got), want)
		}
		seen := make(map[string]bool, len(got))
		for _, key := range got {
			if seen[key] {
				t.Fatalf("limit=%d에서 %s가 두 번 나왔습니다", limit, key)
			}
			seen[key] = true
		}
	}
}

/* ── ① 100ms 루프와 ticker seam ─────────────────────────────────────────── */

// TestDeltaLoopDrainsOnTickAndStopsTickerOnCancel — 루프는 **주입된 ticker가
// 울릴 때만** 일하고, ctx가 끝나면 곧바로 빠져나오면서 ticker를 정확히 한 번
// 멈춰야 합니다. 멈추지 않으면 서비스를 내린 뒤에도 타이머가 남습니다.
func TestDeltaLoopDrainsOnTickAndStopsTickerOnCancel(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "seed", "uid-seed", nil))
	s := h.svc

	ticks := make(chan time.Time)
	var stops atomic.Int32
	var interval atomic.Int64
	s.cfg.NewTicker = func(d time.Duration) (<-chan time.Time, func()) {
		interval.Store(int64(d))
		return ticks, func() { stops.Add(1) }
	}

	h.upsert(t, metaRow("prod", "payments-api", "uid-1", map[string]string{"app": "payments"}))
	if h.queueLen() != 1 {
		t.Fatalf("큐에 %d건입니다 — 콜백이 키를 담지 못했습니다", h.queueLen())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runDeltaLoop(ctx)
	}()

	// 채널이 무버퍼이므로 **두 번째 전송이 받아들여졌다**는 것은 첫 tick 처리가
	// 끝나고 루프가 select로 돌아왔다는 뜻입니다. 벽시계를 기다리지 않습니다.
	ticks <- indexBase
	ticks <- indexBase

	if got := h.queueLen(); got != 0 {
		t.Fatalf("tick 뒤에도 큐에 %d건이 남았습니다 — 루프가 flush를 돌리지 않았습니다", got)
	}
	if got := time.Duration(interval.Load()); got != DeltaTickInterval {
		t.Fatalf("ticker 간격이 %v입니다 — 합치기 창은 %v입니다", got, DeltaTickInterval)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx가 끝났는데 루프가 멈추지 않았습니다")
	}
	if got := stops.Load(); got != 1 {
		t.Fatalf("ticker stop이 %d회 호출됐습니다 — 정확히 한 번이어야 합니다", got)
	}

	// 루프가 실제로 반영했는지 결과로 확인합니다.
	names := foldPage(t, s, "payments", MaxSearchPageSize)
	if len(names) != 1 {
		t.Fatalf("tick이 반영한 결과가 %d건입니다: %v", len(names), names)
	}
}

// TestDefaultTickerFactoryProvidesChannelAndStop — seam이 없으면 기본 ticker를
// 씁니다. 채널과 멈춤 함수를 함께 돌려줘야 하고, 멈춤이 즉시 유효해야 합니다.
func TestDefaultTickerFactoryProvidesChannelAndStop(t *testing.T) {
	c, stop := defaultTickerFactory(time.Hour)
	if c == nil || stop == nil {
		t.Fatal("기본 ticker 팩토리가 채널·멈춤 함수를 돌려주지 않았습니다")
	}
	select {
	case <-c:
		t.Fatal("1시간 ticker가 즉시 울렸습니다")
	default:
	}
	stop()
}

// TestDeltaTickIsNoOpOnCancelledContext — 이미 끝난 ctx로는 아무것도 처리하지
// 않아야 합니다. 종료 중에 배치를 시작하면 게시가 반쯤 진행된 채로 남습니다.
func TestDeltaTickIsNoOpOnCancelledContext(t *testing.T) {
	h := newDeltaHarness(t)
	h.upsert(t, metaRow("prod", "payments-api", "uid-1", nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.svc.deltaTick(ctx)

	if got := h.queueLen(); got != 1 {
		t.Fatalf("취소된 ctx인데 큐가 %d건으로 바뀌었습니다 — 종료 중에 배치를 시작했습니다", got)
	}
}

/* ── ② 예산 거절은 명시적 낡음 ──────────────────────────────────────────── */

// TestFlushBudgetRejectionBecomesExplicitStale — 한 건조차 임시 예약을 받지
// 못하면 되돌림 루프에 빠지지 않고 **그 namespace를 stale로 남기고** 쿨다운에
// 들어가야 합니다. 예약은 queued에서 정확히 한 번 풀립니다.
func TestFlushBudgetRejectionBecomesExplicitStale(t *testing.T) {
	h := newDeltaHarness(t)
	s := h.svc

	const ns, name = "prod", "payments-api"
	h.upsert(t, metaRow(ns, name, "uid-1", nil))
	queuedBefore := s.budget.queued.Load()

	// 정점 여유를 아주 좁게 만듭니다. 배치 임시 예약(수십 KB)은 실패하지만,
	// 낡음 표시가 쓰는 몇 바이트는 통과합니다.
	live := s.budget.live.Load()
	s.budget.max.Store((live + 1024) / searchPeakMultiplier)

	if got := h.flush(t); got != 1 {
		t.Fatalf("flush가 %d을 돌려줬습니다 — 거절한 1건을 보고해야 합니다", got)
	}
	if got := h.queueLen(); got != 0 {
		t.Fatalf("거절 뒤에도 큐에 %d건이 남았습니다 — 되돌림 루프입니다", got)
	}
	parts, whole := h.staleCount()
	if parts != 1 || whole {
		t.Fatalf("stale 상태가 parts=%d whole=%v입니다 — 그 namespace 하나만 낡아야 합니다", parts, whole)
	}
	if !s.namespaceStale(h.gvr, ns) {
		t.Fatalf("%q가 회수 대기로 남지 않았습니다 — 조용히 낡았습니다", ns)
	}

	s.delta.mu.Lock()
	q := s.delta.queueOf(h.gvr)
	dropped, marker, cooldown := q.dropped, q.markerSeq, s.delta.cooldownUntil
	s.delta.mu.Unlock()

	if dropped != 1 {
		t.Fatalf("드롭 계측이 %d입니다 — 거절 1건이어야 합니다", dropped)
	}
	if marker == 0 {
		t.Fatal("마커 seq가 남지 않았습니다 — 회수 게이트가 성립하지 않습니다")
	}
	if want := indexBase.Add(recoveryCooldown); !cooldown.Equal(want) {
		t.Fatalf("쿨다운이 %v입니다 — %v여야 합니다", cooldown, want)
	}
	if got := s.budget.inflight.Load(); got != 0 {
		t.Fatalf("드레인 전에 거절했는데 inflight=%d입니다", got)
	}
	// 이벤트 예약은 정확히 한 번 풀리고, 낡음 표시가 그 namespace 문자열만큼 잡습니다.
	want := queuedBefore - deltaEventBytes(ns, name) + int64(len(ns))
	if got := s.budget.queued.Load(); got != want {
		t.Fatalf("queued=%d — 기대 %d(이벤트 예약 해제 + 낡음 문자열)", got, want)
	}
}

/* ── ③ 회수 예약 요청 경로 ──────────────────────────────────────────────── */

// TestResyncTargetsOneNamespaceAndGVRResyncEscalates — namespace 회수는 그
// namespace만, GVR 회수는 전체를 낡게 만들어야 합니다. 빈 문자열은 "전체"가
// 아니라 **cluster-scoped 파티션**입니다.
func TestResyncTargetsOneNamespaceAndGVRResyncEscalates(t *testing.T) {
	h := newDeltaHarness(t,
		metaRow("prod", "payments-api", "uid-1", nil),
		metaRow("billing", "ledger", "uid-2", nil),
	)
	s := h.svc

	s.requestResync(h.gvr, "prod", 7)
	if !s.namespaceStale(h.gvr, "prod") {
		t.Fatal("prod가 회수 대기로 남지 않았습니다")
	}
	if s.namespaceStale(h.gvr, "billing") {
		t.Fatal("billing까지 낡았습니다 — namespace 회수가 옆 파티션을 끌어들였습니다")
	}
	if parts, whole := s.staleSummary(h.gvr); parts != 1 || whole {
		t.Fatalf("요약이 parts=%d whole=%v입니다", parts, whole)
	}

	// cluster-scoped 파티션은 빈 이름을 가진 **하나의 파티션**입니다.
	s.requestResync(h.gvr, "", 8)
	if !s.namespaceStale(h.gvr, "") {
		t.Fatal("cluster-scoped 파티션이 회수 대기로 남지 않았습니다")
	}
	if parts, whole := s.staleSummary(h.gvr); parts != 2 || whole {
		t.Fatalf("빈 namespace가 GVR 전체로 번졌습니다: parts=%d whole=%v", parts, whole)
	}
	list, whole := s.staleList(h.gvr)
	if whole {
		t.Fatal("staleList가 GVR 전체라고 답했습니다")
	}
	if len(list) != 2 || list[0] != "" || list[1] != "prod" {
		t.Fatalf("낡음 목록이 %q입니다 — 정렬된 [\"\", \"prod\"]여야 합니다", list)
	}

	// 형식 불명 tombstone·슬롯 압축은 GVR 전체를 낡게 만듭니다.
	s.requestGVRResync(h.gvr, 9)
	if _, whole := s.staleSummary(h.gvr); !whole {
		t.Fatal("GVR 회수 요청이 전체 비트를 세우지 않았습니다")
	}
	if !s.namespaceStale(h.gvr, "never-seen") {
		t.Fatal("GVR이 통째로 낡았는데 처음 보는 namespace가 깨끗하다고 답했습니다")
	}
	if _, whole := s.staleList(h.gvr); !whole {
		t.Fatal("staleList가 GVR 전체 비트를 전하지 않았습니다")
	}

	samples := s.sampleDeltaMetrics()
	if len(samples) != 1 {
		t.Fatalf("계측 표본이 %d개입니다", len(samples))
	}
	if samples[0].resource != FormatGVR(h.gvr) {
		t.Fatalf("표본 리소스가 %q입니다", samples[0].resource)
	}
	if samples[0].staleParts != 2 || samples[0].gvrStale != 1 {
		t.Fatalf("표본이 staleParts=%d gvrStale=%d입니다", samples[0].staleParts, samples[0].gvrStale)
	}
}

// TestResyncFallsBackToFullRebuildWhenQueueStructureRejected — 큐 구조조차
// 상한 안에 만들 수 없으면, 조용히 낡는 대신 **전체 재구성**으로 되돌리고
// 아무것도 할당하지 않아야 합니다.
func TestResyncFallsBackToFullRebuildWhenQueueStructureRejected(t *testing.T) {
	s := &Service{
		cfg: Config{
			SearchEnabled: true, SearchIncremental: true,
			MaxSearchIndexBytes: DefaultMaxSearchIndexBytes,
			Now:                 func() time.Time { return indexBase },
		},
		order:   []schema.GroupVersionResource{scopedGVR},
		entries: map[schema.GroupVersionResource]*resourceEntry{scopedGVR: {gvr: scopedGVR}},
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	// 정점 여유가 큐 고정 구조보다 작습니다 — 승인 없이 만들지 않습니다.
	s.budget.max.Store(1)

	e := s.entries[scopedGVR]
	rebuilt := func(what string) {
		t.Helper()
		if e.bootstrapped.Load() || !e.dirty.Load() {
			t.Fatalf("%s: 큐를 못 만들었는데 전체 재구성으로 돌아가지 않았습니다 (bootstrapped=%v dirty=%v)",
				what, e.bootstrapped.Load(), e.dirty.Load())
		}
		e.bootstrapped.Store(true)
		e.dirty.Store(false)
	}

	e.bootstrapped.Store(true)
	e.dirty.Store(false)
	s.requestResync(scopedGVR, "prod", 1)
	rebuilt("requestResync")
	s.requestGVRResync(scopedGVR, 2)
	rebuilt("requestGVRResync")
	s.markGVRStale(scopedGVR)
	rebuilt("markGVRStale")
	s.enqueueKey(bindingFor(s, scopedGVR), "prod", "payments-api")
	rebuilt("enqueueKey")

	s.delta.mu.Lock()
	queues := len(s.delta.queues)
	s.delta.mu.Unlock()
	if queues != 0 {
		t.Fatalf("거절된 구조가 %d개 남았습니다 — 승인 없이 할당했습니다", queues)
	}
	if got := s.budget.queued.Load(); got != 0 {
		t.Fatalf("거절됐는데 queued=%d입니다 — 원장이 기준선으로 돌아오지 않았습니다", got)
	}
}

// TestStaleTrackingPromotesToGVRBitAndReleasesDynamicBytes — namespace 추적이
// 상한을 넘으면 **유계 GVR 비트 하나**로 승급하고, 그때까지 붙잡던 문자열
// 바이트를 정확히 되돌려야 합니다. 승급 뒤에도 안전 성질(그 namespace가
// 낡았다는 사실)은 유지됩니다.
func TestStaleTrackingPromotesToGVRBitAndReleasesDynamicBytes(t *testing.T) {
	h := newDeltaHarness(t)
	s := h.svc

	s.delta.mu.Lock()
	q := s.delta.queueFor(h.gvr)
	for i := 0; i < maxStaleTracked; i++ {
		s.delta.addStaleLocked(q, fmt.Sprintf("ns-%05d", i), uint64(i+1))
	}
	countBefore, dynBefore, promotedBefore := q.staleNS.count, q.dynamic, q.gvrStale
	s.delta.addStaleLocked(q, "ns-overflow", uint64(maxStaleTracked+1))
	countAfter, dynAfter, promoted := q.staleNS.count, q.dynamic, q.gvrStale
	s.delta.mu.Unlock()

	if countBefore != maxStaleTracked || promotedBefore {
		t.Fatalf("상한 직전 상태가 count=%d whole=%v입니다", countBefore, promotedBefore)
	}
	if dynBefore <= 0 {
		t.Fatalf("추적 문자열이 원장에 %d로 잡혔습니다 — 예약 밖 메모리입니다", dynBefore)
	}
	if !promoted {
		t.Fatal("추적 상한을 넘겼는데 GVR 비트로 승급하지 않았습니다 — 상한이 상한이 아닙니다")
	}
	if countAfter != 0 || dynAfter != 0 {
		t.Fatalf("승급 뒤 count=%d dynamic=%d입니다 — 문자열 몫이 남았습니다", countAfter, dynAfter)
	}
	if !s.namespaceStale(h.gvr, "ns-00000") {
		t.Fatal("승급 뒤 이미 낡았던 namespace가 깨끗하다고 답했습니다")
	}
	if !s.namespaceStale(h.gvr, "ns-overflow") {
		t.Fatal("승급을 유발한 namespace가 깨끗하다고 답했습니다")
	}
}

/* ── ④ 회수 종단: 포기·중단·실패·성공 ──────────────────────────────────── */

// TestAbandonRecoveryReturnsHeldToQueueAndMarksStale — 보류 상한을 넘겨 회수를
// 포기할 때, 보류분은 **버리지 않고 큐로 되돌리고** 티켓 예약은 정확히 한 번
// 풀려야 합니다. 보류 배열의 용량 계상도 함께 사라집니다.
func TestAbandonRecoveryReturnsHeldToQueueAndMarksStale(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "seed", "uid-seed", nil))
	s := h.svc

	tk := pinRecoveryTicket(t, h, "prod")
	if tk.wholeGVR {
		t.Fatal("namespace 대상 티켓이 아닙니다")
	}

	h.upsert(t, metaRow("prod", "held-1", "uid-h1", nil))
	h.upsert(t, metaRow("prod", "held-2", "uid-h2", nil))
	if got := h.flush(t); got != 0 {
		t.Fatalf("회수 대상 파티션의 키가 %d건 드레인됐습니다 — 보류되어야 합니다", got)
	}

	s.delta.mu.Lock()
	q := s.delta.queueOf(h.gvr)
	heldLen := len(q.hold)
	s.delta.mu.Unlock()
	if heldLen != 2 {
		t.Fatalf("보류가 %d건입니다 — 두 건 모두 보류되어야 합니다", heldLen)
	}

	failuresBefore := s.delta.recoveryFailures.Load()
	s.delta.mu.Lock()
	s.abandonRecoveryLocked(q, 999)
	events, hold, holdCap := len(q.events), len(q.hold), q.holdCap
	marker, staleHas := q.markerSeq, q.staleNS.has("prod")
	live := s.delta.ticket
	s.delta.mu.Unlock()

	if events != 2 || hold != 0 {
		t.Fatalf("포기 뒤 events=%d hold=%d입니다 — 보류분은 큐로 돌아가야 합니다", events, hold)
	}
	if holdCap != 0 {
		t.Fatalf("보류 배열 용량 계상이 %d로 남았습니다 — 배열은 큐를 떠났습니다", holdCap)
	}
	if marker != 999 {
		t.Fatalf("마커가 %d입니다 — 포기 시점 seq로 전진해야 합니다", marker)
	}
	if !staleHas {
		t.Fatal("포기한 파티션이 낡음으로 남지 않았습니다 — 조용히 낡았습니다")
	}
	if live != nil || !tk.dead {
		t.Fatalf("티켓이 걷히지 않았습니다: live=%v dead=%v", live != nil, tk.dead)
	}
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("회수 예약이 %d로 남았습니다 — 종단에서 정확히 한 번 풀려야 합니다", got)
	}
	if got := s.delta.recoveryFailures.Load() - failuresBefore; got != 1 {
		t.Fatalf("실패 계측이 %d 늘었습니다 — 포기는 실패 한 번입니다", got)
	}

	// 되돌아온 보류분은 다음 flush에서 정상적으로 반영됩니다.
	if got := drainQueueFully(t, h); got != 2 {
		t.Fatalf("되돌아온 보류분이 %d건만 반영됐습니다", got)
	}
	if names := foldPage(t, s, "held", MaxSearchPageSize); len(names) != 2 {
		t.Fatalf("되돌아온 키가 인덱스에 %d건만 있습니다: %v", len(names), names)
	}
}

// TestAbortRecoveryDropsHeldAndReleasesReservation — 진행할 수 없는 티켓을
// 걷을 때는 보류분을 **버리고** 그 예약까지 정확히 한 번 풀어야 합니다.
// 포기(abandon)와 달리 큐로 되돌리지 않습니다.
func TestAbortRecoveryDropsHeldAndReleasesReservation(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "seed", "uid-seed", nil))
	s := h.svc

	tk := pinRecoveryTicket(t, h, "prod")
	h.upsert(t, metaRow("prod", "held-1", "uid-h1", nil))
	h.flush(t)

	s.delta.mu.Lock()
	q := s.delta.queueOf(h.gvr)
	heldLen := len(q.hold)
	heldReserved := int64(0)
	for _, ev := range q.hold {
		heldReserved += ev.reserved
	}
	holdCapBytes := int64(q.holdCap) * deltaEventStructBytes
	queuedBefore := s.budget.queued.Load()
	step := tk.step
	s.delta.mu.Unlock()
	if heldLen != 1 || heldReserved <= 0 {
		t.Fatalf("보류가 %d건(예약 %d)입니다", heldLen, heldReserved)
	}

	s.abortRecovery(tk, step)

	s.delta.mu.Lock()
	events, hold, holdCap := len(q.events), len(q.hold), q.holdCap
	live := s.delta.ticket
	s.delta.mu.Unlock()

	if hold != 0 || holdCap != 0 {
		t.Fatalf("중단 뒤 hold=%d holdCap=%d입니다 — 보류 저장소가 남았습니다", hold, holdCap)
	}
	if events != 0 {
		t.Fatalf("중단인데 보류분이 큐로 %d건 돌아왔습니다 — 중단은 되돌리지 않습니다", events)
	}
	if live != nil || !tk.dead {
		t.Fatal("중단 뒤에도 티켓이 살아 있습니다")
	}
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("회수 예약이 %d로 남았습니다", got)
	}
	// 보류 이벤트 예약 + 보류 배열 용량 + 티켓 구조 계상이 함께 풀립니다.
	want := queuedBefore - heldReserved - holdCapBytes - recoveryTicketBytes
	if got := s.budget.queued.Load(); got != want {
		t.Fatalf("queued=%d — 기대 %d(보류 예약·배열 용량·티켓 구조 해제)", got, want)
	}
}

// TestFailRecoveryOpensCircuitAndBacksOff — 예산 거절로 끝난 회수는 **회로를
// 열고** 지수 백오프를 남겨야 합니다. 티켓이 사라져도 회로는 남으므로 같은
// 입력·같은 예산에서 100ms마다 같은 회수를 반복하지 않습니다.
func TestFailRecoveryOpensCircuitAndBacksOff(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "seed", "uid-seed", nil))
	s := h.svc

	tk := pinRecoveryTicket(t, h, "prod")
	_, step := currentTicket(s)
	failuresBefore := s.delta.recoveryFailures.Load()

	s.failRecovery(tk, step, recoveryBudgetRejected)

	s.delta.mu.Lock()
	live := s.delta.ticket
	cooldown := s.delta.cooldownUntil
	c := s.delta.circuits[recoveryTarget{gvr: h.gvr, namespace: "prod"}]
	q := s.delta.queueOf(h.gvr)
	restaked := q.staleNS.has("prod")
	s.delta.mu.Unlock()

	if live != nil || !tk.dead {
		t.Fatal("실패 뒤에도 티켓이 살아 있습니다")
	}
	if c == nil {
		t.Fatal("회로가 남지 않았습니다 — 티켓과 함께 사라지면 백오프가 없습니다")
	}
	if !c.open {
		t.Fatal("예산 거절인데 회로가 열리지 않았습니다 — 같은 입력으로 곧바로 재시도합니다")
	}
	if c.attempts != 1 {
		t.Fatalf("시도 횟수가 %d입니다", c.attempts)
	}
	if want := 2 * recoveryBackoffMin; c.backoff != want {
		t.Fatalf("백오프가 %v입니다 — 첫 실패는 %v입니다", c.backoff, want)
	}
	if want := indexBase.Add(c.backoff); !c.notBefore.Equal(want) {
		t.Fatalf("회로 notBefore가 %v입니다 — %v여야 합니다", c.notBefore, want)
	}
	if want := indexBase.Add(c.backoff); !cooldown.Equal(want) {
		t.Fatalf("쿨다운이 %v입니다 — 회로 백오프 %v를 따라야 합니다", cooldown, want)
	}
	if !restaked {
		t.Fatal("실패한 대상이 낡음으로 되돌아가지 않았습니다")
	}
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("회수 예약이 %d로 남았습니다", got)
	}
	if got := s.delta.recoveryFailures.Load() - failuresBefore; got != 1 {
		t.Fatalf("실패 계측이 %d 늘었습니다", got)
	}

	// 회로가 열려 있으므로 쿨다운이 지나도 같은 입력에서는 다시 잡지 않습니다.
	s.delta.mu.Lock()
	picked := s.pickRecoveryLocked(indexBase.Add(recoveryCooldown + time.Hour))
	s.delta.mu.Unlock()
	if picked != nil {
		t.Fatal("입력도 예산도 그대로인데 회수를 다시 잡았습니다 — 회로가 폭풍을 막지 못합니다")
	}
}

// TestFinishRecoveryPublishedClearsOnlyThatPartition — namespace 회수가
// 성공하면 **그 파티션 하나만** 깨끗해져야 합니다. 하나가 나머지를 지우면
// 아직 회수하지 않은 namespace가 조용히 "깨끗함"으로 바뀝니다.
func TestFinishRecoveryPublishedClearsOnlyThatPartition(t *testing.T) {
	h := newDeltaHarness(t,
		metaRow("prod", "payments-api", "uid-1", nil),
		metaRow("billing", "ledger", "uid-2", nil),
	)
	s := h.svc

	s.requestResync(h.gvr, "prod", 5)
	s.requestResync(h.gvr, "billing", 6)
	s.advanceRecovery(context.Background())
	tk, _ := currentTicket(s)
	if tk == nil {
		t.Fatal("회수 티켓이 만들어지지 않았습니다")
	}
	// 마커가 목록 스냅숏보다 앞서 있으므로 이 단계는 **커버 대기**로 남습니다.
	if tk.phase != recoveryWaitingCover {
		t.Fatalf("phase=%d입니다 — 덮이지 않은 마커는 커버 대기여야 합니다", tk.phase)
	}
	picked := tk.namespace
	other := "prod"
	if picked == "prod" {
		other = "billing"
	}
	resyncsBefore := s.delta.partitionResyncs.Load()

	s.finishRecovery(tk, recoveryPublished)

	if s.namespaceStale(h.gvr, picked) {
		t.Fatalf("회수한 %q가 아직 낡았다고 답합니다", picked)
	}
	if !s.namespaceStale(h.gvr, other) {
		t.Fatalf("회수하지 않은 %q가 깨끗해졌습니다 — 하나가 나머지를 지웠습니다", other)
	}
	if got := s.delta.partitionResyncs.Load() - resyncsBefore; got != 1 {
		t.Fatalf("파티션 회수 계측이 %d 늘었습니다", got)
	}

	s.delta.mu.Lock()
	c := s.delta.circuits[recoveryTarget{gvr: h.gvr, namespace: picked}]
	cooldown := s.delta.cooldownUntil
	live := s.delta.ticket
	s.delta.mu.Unlock()

	if c != nil {
		t.Fatal("성공한 대상의 회로가 남았습니다 — 회로 상한만 갉아먹습니다")
	}
	if want := indexBase.Add(recoveryCooldown); !cooldown.Equal(want) {
		t.Fatalf("성공 뒤 쿨다운이 %v입니다 — %v여야 합니다", cooldown, want)
	}
	if live != nil || !tk.dead {
		t.Fatal("성공 뒤에도 티켓이 살아 있습니다")
	}
	if got := s.budget.recovery.Load(); got != 0 {
		t.Fatalf("회수 예약이 %d로 남았습니다", got)
	}
}

/* ── ⑤ 회로 접기 ────────────────────────────────────────────────────────── */

// TestCircuitEscalationInheritsMostConservativeFingerprint — GVR 하나의
// namespace 회로를 전체 회로로 접을 때, 접는 행위 자체가 재시도를 앞당기면
// 안 됩니다. 가장 늦은 notBefore·가장 큰 backoff·가장 **낮은** 가용 용량을
// 물려받아야 합니다. 두 회로 중 어느 쪽이 재사용되든 결과가 같아야 하므로
// 맵 순회 순서에 의존하지 않습니다.
func TestCircuitEscalationInheritsMostConservativeFingerprint(t *testing.T) {
	h := newDeltaHarness(t)
	s := h.svc
	other := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	s.delta.mu.Lock()
	c1, _ := s.delta.circuitForLocked(recoveryTarget{gvr: other, namespace: "ns-a"})
	c2, _ := s.delta.circuitForLocked(recoveryTarget{gvr: other, namespace: "ns-b"})
	if c1 == nil || c2 == nil {
		s.delta.mu.Unlock()
		t.Fatal("회로를 만들지 못했습니다")
	}
	c1.open, c1.attempts, c1.lastNeeded = true, 3, 100
	c1.backoff, c1.notBefore, c1.lastAvail = 4*time.Second, indexBase.Add(30*time.Second), 900
	c2.open, c2.attempts, c2.lastNeeded = false, 1, 50
	c2.backoff, c2.notBefore, c2.lastAvail = 2*time.Second, indexBase.Add(10*time.Second), 400

	foldable := s.delta.foldableCountLocked(other)
	queuedBefore := s.budget.queued.Load()
	s.delta.escalateCircuitsLocked(other)
	merged := s.delta.circuits[recoveryTarget{gvr: other, whole: true}]
	left := s.delta.foldableCountLocked(other)
	queuedAfter := s.budget.queued.Load()
	s.delta.mu.Unlock()

	if foldable != 2 {
		t.Fatalf("접을 수 있는 회로가 %d개입니다", foldable)
	}
	if left != 0 {
		t.Fatalf("접은 뒤에도 namespace 회로가 %d개 남았습니다 — 자리가 생기지 않습니다", left)
	}
	if merged == nil {
		t.Fatal("전체 회로가 만들어지지 않았습니다")
	}
	if !merged.open {
		t.Fatal("열린 회로를 접었는데 전체 회로가 닫혀 있습니다 — 접기가 재시도를 앞당깁니다")
	}
	if merged.backoff != 4*time.Second {
		t.Fatalf("백오프가 %v입니다 — 가장 큰 값을 물려받아야 합니다", merged.backoff)
	}
	if want := indexBase.Add(30 * time.Second); !merged.notBefore.Equal(want) {
		t.Fatalf("notBefore가 %v입니다 — 가장 늦은 값이어야 합니다", merged.notBefore)
	}
	if merged.attempts != 3 || merged.lastNeeded != 100 {
		t.Fatalf("attempts=%d lastNeeded=%d — 가장 보수적인 값이어야 합니다", merged.attempts, merged.lastNeeded)
	}
	if merged.lastAvail != 400 {
		t.Fatalf("lastAvail이 %d입니다 — 가용 용량은 **가장 낮은** 쪽이 보수적입니다", merged.lastAvail)
	}
	if want := queuedBefore - recoveryCircuitBytes; queuedAfter != want {
		t.Fatalf("queued=%d — 기대 %d(접힌 회로 하나 해제)", queuedAfter, want)
	}

	// 같은 GVR에 큐가 있으면, 접기는 그 큐의 namespace 마커도 함께 접습니다.
	// 그러지 않으면 회로는 전체만 보는데 큐는 namespace 후보를 계속 냅니다.
	s.requestResync(h.gvr, "prod", 3)
	s.delta.mu.Lock()
	s.delta.circuitForLocked(recoveryTarget{gvr: h.gvr, namespace: "prod"})
	s.delta.circuitForLocked(recoveryTarget{gvr: h.gvr, namespace: "billing"})
	s.delta.escalateCircuitsLocked(h.gvr)
	q := s.delta.queueOf(h.gvr)
	staleCount, gvrStale := q.staleNS.count, q.gvrStale
	s.delta.mu.Unlock()

	if staleCount != 0 || !gvrStale {
		t.Fatalf("접힌 GVR의 큐가 staleNS=%d gvrStale=%v입니다 — 회로와 큐가 어긋났습니다",
			staleCount, gvrStale)
	}
}

// TestDropCircuitsForGVRReturnsSeedExactlyOnce — 멈추는 GVR의 회로는 전부
// 걷히고, 회로가 하나도 남지 않으면 **맵 씨앗까지** 되돌아와야 합니다.
// 멈춘 서비스가 0으로 수렴하려면 이 몫도 사라져야 합니다.
func TestDropCircuitsForGVRReturnsSeedExactlyOnce(t *testing.T) {
	h := newDeltaHarness(t)
	s := h.svc
	before := s.budget.queued.Load()

	s.delta.mu.Lock()
	s.delta.circuitForLocked(recoveryTarget{gvr: h.gvr, namespace: "a"})
	s.delta.circuitForLocked(recoveryTarget{gvr: h.gvr, namespace: "b"})
	n, seed := len(s.delta.circuits), s.delta.circuitsSeed
	s.delta.mu.Unlock()

	if n != 2 || seed != circuitsMapSeedBytes {
		t.Fatalf("회로 %d개 seed=%d입니다", n, seed)
	}
	if want := before + circuitsMapSeedBytes + 2*recoveryCircuitBytes; s.budget.queued.Load() != want {
		t.Fatalf("회로 계상이 queued=%d입니다 — 기대 %d", s.budget.queued.Load(), want)
	}

	s.delta.mu.Lock()
	s.delta.dropCircuitsForLocked(h.gvr)
	nilled, seedAfter := s.delta.circuits == nil, s.delta.circuitsSeed
	s.delta.mu.Unlock()

	if !nilled || seedAfter != 0 {
		t.Fatalf("회로 맵이 남았습니다: nil=%v seed=%d", nilled, seedAfter)
	}
	if got := s.budget.queued.Load(); got != before {
		t.Fatalf("queued=%d — 기준선 %d으로 정확히 돌아와야 합니다", got, before)
	}
}

/* ── ⑥ 리프 상한을 넘는 삽입·삭제 ───────────────────────────────────────── */

// TestDeltaAcrossLeafBoundariesKeepsExactMembership — posting/행 디렉터리
// 리프 상한(256)을 넘겨 분할을 만들고, 다시 대부분을 지워 병합·재분배를
// 일으켜도 결과가 **중복도 누락도 없이** 정확해야 합니다.
//
// 신원은 큐가 아니라 **같은 세대의 Store**에서 다시 해석되므로, 드레인한 키
// 수만큼만 조회가 일어납니다.
func TestDeltaAcrossLeafBoundariesKeepsExactMembership(t *testing.T) {
	h := newDeltaHarness(t)
	s := h.svc

	const total = 600
	const removed = 500
	row := func(i int) *metav1.PartialObjectMetadata {
		return metaRow("prod", fmt.Sprintf("row-%04d", i), fmt.Sprintf("uid-%04d", i),
			map[string]string{"app": "payments"})
	}

	lookupsBefore := s.delta.identityLookups.Load()
	for i := 0; i < total; i++ {
		h.upsert(t, row(i))
	}
	if got := drainQueueFully(t, h); got != total {
		t.Fatalf("삽입 %d건 중 %d건만 반영됐습니다", total, got)
	}
	if got := s.delta.identityLookups.Load() - lookupsBefore; got != total {
		t.Fatalf("신원 조회가 %d회입니다 — 드레인한 키 수(%d)와 같아야 합니다", got, total)
	}
	assertMembershipExact(t, s, "row-", total)

	lookupsMid := s.delta.identityLookups.Load()
	for i := 0; i < removed; i++ {
		h.remove(t, row(i))
	}
	if got := drainQueueFully(t, h); got != removed {
		t.Fatalf("삭제 %d건 중 %d건만 반영됐습니다", removed, got)
	}
	if got := s.delta.identityLookups.Load() - lookupsMid; got != removed {
		t.Fatalf("삭제 경로의 신원 조회가 %d회입니다 — %d회여야 합니다", got, removed)
	}
	assertMembershipExact(t, s, "row-", total-removed)

	// 남은 것이 정확히 뒤쪽 구간인지까지 봅니다(병합이 엉뚱한 행을 지우지 않았는지).
	kept := foldPage(t, s, fmt.Sprintf("row-%04d", removed), MaxSearchPageSize)
	if len(kept) != 1 {
		t.Fatalf("경계 행이 %d건입니다: %v", len(kept), kept)
	}
	if gone := foldPage(t, s, fmt.Sprintf("row-%04d", removed-1), MaxSearchPageSize); len(gone) != 0 {
		t.Fatalf("지운 경계 행이 남아 있습니다: %v", gone)
	}
}

// TestDeltaResolvesIdentityFromStoreNotQueue — 큐에는 키만 담기므로, 같은
// 이름이 다른 UID로 교체되면 **적용 시점의 Store**가 답을 정합니다.
// Store에서 사라진 키는 같은 경로로 삭제가 됩니다.
func TestDeltaResolvesIdentityFromStoreNotQueue(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-old", nil))
	s := h.svc

	h.upsert(t, metaRow("prod", "payments-api", "uid-new", nil))
	if got := h.flush(t); got != 1 {
		t.Fatalf("flush=%d", got)
	}
	swapped := foldPage(t, s, "payments", MaxSearchPageSize)
	if len(swapped) != 1 || swapped[0] != "prod/payments-api/uid-new/name" {
		t.Fatalf("UID 교체가 반영되지 않았습니다: %v", swapped)
	}

	h.remove(t, metaRow("prod", "payments-api", "uid-new", nil))
	if got := h.flush(t); got != 1 {
		t.Fatalf("삭제 flush=%d", got)
	}
	if left := foldPage(t, s, "payments", MaxSearchPageSize); len(left) != 0 {
		t.Fatalf("삭제한 행이 남아 있습니다: %v", left)
	}
}

/* ── ⑦ tombstone 키 해석 ────────────────────────────────────────────────── */

// TestSplitMetaKeyRejectsAmbiguousShapes — key-only tombstone은 문자열 하나뿐
// 이라 여기서 틀리면 **엉뚱한 객체를 지웁니다.** 그래서 형식을 정확히 요구합니다.
func TestSplitMetaKeyRejectsAmbiguousShapes(t *testing.T) {
	cases := []struct {
		key        string
		namespaced bool
		ns, name   string
		ok         bool
	}{
		{"prod/payments-api", true, "prod", "payments-api", true},
		{"payments-api", false, "", "payments-api", true},
		{"", true, "", "", false},
		{"", false, "", "", false},
		{"payments-api", true, "", "", false},       // 슬래시가 없습니다
		{"prod/", true, "", "", false},              // 이름이 비었습니다
		{"/payments-api", true, "", "", false},      // namespace가 비었습니다
		{"prod/a/b", true, "", "", false},           // 슬래시가 둘입니다
		{"prod/payments-api", false, "", "", false}, // 클러스터 범위인데 슬래시가 있습니다
	}
	for _, tc := range cases {
		ns, name, ok := splitMetaKey(tc.key, tc.namespaced)
		if ok != tc.ok || ns != tc.ns || name != tc.name {
			t.Fatalf("splitMetaKey(%q, namespaced=%v) = (%q, %q, %v) — 기대 (%q, %q, %v)",
				tc.key, tc.namespaced, ns, name, ok, tc.ns, tc.name, tc.ok)
		}
	}
}

// TestEnqueueObjectEscalatesUnresolvableShapes — 해석할 수 없는 이벤트는
// ns=""로 뭉개지 않고 **유계 GVR 비트**로 승급해 회수에 맡겨야 합니다.
func TestEnqueueObjectEscalatesUnresolvableShapes(t *testing.T) {
	h := newDeltaHarness(t)
	s := h.svc
	// 이 GVR은 namespaced입니다 — 콜백 신원도 같은 성질을 알아야 키를 나눌 수 있습니다.
	h.binding.namespaced = true

	clearGVRStale := func() {
		s.delta.mu.Lock()
		s.delta.queueFor(h.gvr).gvrStale = false
		s.delta.mu.Unlock()
	}

	// ① 형식이 어긋난 key-only tombstone.
	s.enqueueObject(h.binding, cache.DeletedFinalStateUnknown{Key: "prod/a/b"})
	if _, whole := s.staleSummary(h.gvr); !whole {
		t.Fatal("형식 불명 tombstone이 GVR 비트로 승급하지 않았습니다")
	}
	if got := h.queueLen(); got != 0 {
		t.Fatalf("해석하지 못한 키가 큐에 %d건 담겼습니다", got)
	}

	// ② 알 수 없는 형태.
	clearGVRStale()
	s.enqueueObject(h.binding, "이건 객체가 아닙니다")
	if _, whole := s.staleSummary(h.gvr); !whole {
		t.Fatal("알 수 없는 이벤트 형태가 GVR 비트로 승급하지 않았습니다")
	}
	if got := h.queueLen(); got != 0 {
		t.Fatalf("알 수 없는 형태가 큐에 %d건 담겼습니다", got)
	}

	// ③ 형식이 맞는 tombstone은 그 키 하나만 큐에 담깁니다.
	clearGVRStale()
	s.enqueueObject(h.binding, cache.DeletedFinalStateUnknown{Key: "prod/payments-api"})
	if got := h.queueLen(); got != 1 {
		t.Fatalf("정상 tombstone이 큐에 %d건 담겼습니다", got)
	}
	if _, whole := s.staleSummary(h.gvr); whole {
		t.Fatal("정상 tombstone이 GVR 전체를 낡게 만들었습니다")
	}

	// ④ 객체가 실린 tombstone은 그 객체의 신원을 그대로 씁니다.
	s.enqueueObject(h.binding, cache.DeletedFinalStateUnknown{
		Obj: metaRow("prod", "ledger", "uid-2", nil),
	})
	if got := h.queueLen(); got != 2 {
		t.Fatalf("객체가 실린 tombstone 뒤 큐가 %d건입니다", got)
	}
}
