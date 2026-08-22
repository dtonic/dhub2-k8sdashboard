package resourcecatalog

// Round 8 P1-A/B/C 회귀
// --------------------------------------------------------------------------
// A. I-C를 **진짜 하드 캡**으로 (구조 메모리 승인, 동적 문자열 계상, 회로 상한)
// B. 델타·회수의 **모든 메모리를 할당 전에** 예약
// C. 용량을 실제로 보는 무회전 재시도
//
// 원장 검증은 pendingBytes를 자기 자신과 비교하지 않습니다. 구조에서
// **독립적으로 다시 계산한 하한**과 비교합니다.

import (
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

/* ── 독립 재계산 ────────────────────────────────────────────────────────── */

// recomputeQueuedLowerBound는 원장을 **보지 않고** 큐·회로 상태만으로 queued 항의
// 하한을 다시 계산합니다.
//
// pendingBytes()를 그대로 쓰면 "자기 자신과 비교"가 되어 아무것도 증명하지
// 못합니다. 여기서는 구조체 크기·슬롯 수·문자열 길이·이벤트 예약을 직접 더합니다.
func recomputeQueuedLowerBound(t testing.TB, s *Service) int64 {
	t.Helper()
	var total int64
	for _, q := range s.delta.queues {
		// ① 고정 구조: 큐 본체 + staleNS 슬롯/사용비트/마커/재사용 버퍼 + 색인 씨앗
		slots := int64(len(q.staleNS.slots))
		total += deltaQueueStructBytes
		total += slots*(stringHeaderBytes+1+8) +
			int64(q.staleNS.limit)*(stringHeaderBytes+8) + 48
		total += deltaIndexSeedEntries * deltaIndexEntryBytes
		// ② 동적: staleNS가 붙잡은 namespace 문자열 본문
		for i, used := range q.staleNS.used {
			if used {
				total += int64(len(q.staleNS.slots[i]))
			}
		}
		// ③ **도달 가능한 저장 용량**: 배열 용량 + 색인 고수위(씨앗 초과분)
		total += int64(q.eventCap) * deltaEventStructBytes
		total += int64(q.holdCap) * deltaEventStructBytes
		total += int64(q.indexCharged-deltaIndexSeedEntries) * deltaIndexEntryBytes
		// ④ 대기·보류 이벤트 예약
		for _, ev := range q.events {
			total += ev.reserved
		}
		for _, ev := range q.hold {
			total += ev.reserved
		}
	}
	// ⑤ 회로 항목 + 회로 맵 씨앗 + 살아 있는 티켓
	total += int64(len(s.delta.circuits))*recoveryCircuitBytes + s.delta.circuitsSeed
	if s.delta.ticket != nil && s.delta.ticket.structCharged {
		total += recoveryTicketBytes
	}
	return total
}

// assertLedgerMatchesStructure는 원장이 **독립 재계산과 정확히 같은지**와
// I-C를 넘지 않는지를 함께 봅니다.
func assertLedgerMatchesStructure(t *testing.T, s *Service, where string) {
	t.Helper()
	s.delta.mu.Lock()
	want := recomputeQueuedLowerBound(t, s)
	s.delta.mu.Unlock()
	if got := s.budget.queued.Load(); got != want {
		t.Fatalf("%s: budget.queued=%d, 구조에서 다시 센 값=%d", where, got, want)
	}
	if live, peak := s.budget.live.Load(), s.budget.peakLimit(); live > peak {
		t.Fatalf("%s: I-C 위반 live=%d > %d", where, live, peak)
	}
}

// tinyBudgetService는 상한을 아주 좁게 준 단일 GVR 서비스입니다.
// 구조 메모리조차 들어가지 않는 상황을 만들기 위한 픽스처입니다.
func tinyBudgetService(t *testing.T, max int64) *Service {
	t.Helper()
	gvr := scopedGVR
	s := &Service{
		cfg: Config{
			ClusterID: "prod-seoul", SearchEnabled: true, SearchIncremental: true,
			MaxSearchIndexBytes: max,
			Now:                 func() time.Time { return indexBase },
		},
		order:   []schema.GroupVersionResource{gvr},
		entries: map[schema.GroupVersionResource]*resourceEntry{gvr: {gvr: gvr}},
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	s.budget.max.Store(max)
	s.started.Store(true)
	e := s.entries[gvr]
	e.setStatus(StateReady, "")
	e.lifecycle, e.generation = 1, 0
	e.tokenPacked.Store(packToken(1, 0))
	e.bootstrapped.Store(true)
	return s
}

/* ── P1-A ①: 첫 큐 생성이 I-C를 넘지 못합니다 ───────────────────────────── */

// TestFirstQueueCreationCannotExceedPeak — 큐 구조 하나조차 들어가지 않는 상한에서
// enqueue는 **큐를 만들지 않고** 전체 재구성으로 되돌려야 합니다. live는 절대
// peakLimit을 넘지 않습니다.
func TestFirstQueueCreationCannotExceedPeak(t *testing.T) {
	// 고정 몫은 **상수**입니다 — 할당하지 않고도 알 수 있어야 승인이 먼저 옵니다.
	const fixed = deltaQueueFixedBytes
	max := int64(fixed) / 4
	s := tinyBudgetService(t, max)
	if s.budget.peakLimit() >= fixed {
		t.Fatalf("픽스처가 느슨합니다: peak=%d >= 고정 몫=%d", s.budget.peakLimit(), int64(fixed))
	}
	e := s.entries[scopedGVR]
	b := &handlerBinding{entry: e, packed: e.tokenPacked.Load(), namespaced: true}

	for i := 0; i < 32; i++ {
		s.enqueueKey(b, "prod", fmt.Sprintf("row-%02d", i))
		if live, peak := s.budget.live.Load(), s.budget.peakLimit(); live > peak {
			t.Fatalf("%d번째 enqueue에서 I-C 위반: live=%d > %d", i, live, peak)
		}
	}
	s.delta.mu.Lock()
	queues := len(s.delta.queues)
	s.delta.mu.Unlock()
	if queues != 0 {
		t.Fatalf("상한을 넘겨서까지 큐를 %d개 만들었습니다", queues)
	}
	if got := s.budget.queued.Load(); got != 0 {
		t.Fatalf("만들지도 않은 구조가 %d 계상되었습니다", got)
	}
	// 증분을 포기했으면 **전체 재구성으로 되돌려야** 합니다.
	if e.bootstrapped.Load() {
		t.Fatal("증분을 쓸 수 없는데 부트스트랩 완료 상태로 남았습니다")
	}
	if !e.dirty.Load() {
		t.Fatal("전체 재구성이 예약되지 않았습니다 — 조용히 낡습니다")
	}
	assertLedgerMatchesStructure(t, s, "first-queue-rejected")

	// 상한이 넉넉해지면 그때는 만들어져야 합니다(영구 차단이 아닙니다).
	s.cfg.MaxSearchIndexBytes = DefaultMaxSearchIndexBytes
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	s.enqueueKey(b, "prod", "row-ok")
	s.delta.mu.Lock()
	queues = len(s.delta.queues)
	s.delta.mu.Unlock()
	if queues != 1 {
		t.Fatalf("상한이 풀렸는데 큐가 %d개입니다", queues)
	}
	assertLedgerMatchesStructure(t, s, "first-queue-admitted")
}

/* ── P1-A ②: 첫 회로 생성도 승인 대상입니다 ─────────────────────────────── */

// TestFirstCircuitCreationCannotExceedPeak — 회로 항목 하나도 승인 없이 실리지
// 않습니다. 자리가 없으면 **공유 넘침 회로**를 돌려주고 항목을 늘리지 않습니다.
func TestFirstCircuitCreationCannotExceedPeak(t *testing.T) {
	s := tinyBudgetService(t, recoveryCircuitBytes/8) // peak(=3*max) < recoveryCircuitBytes
	if s.budget.peakLimit() >= recoveryCircuitBytes {
		t.Fatalf("픽스처가 느슨합니다: peak=%d", s.budget.peakLimit())
	}
	target := recoveryTarget{gvr: scopedGVR, namespace: "prod"}

	s.delta.mu.Lock()
	c, _ := s.delta.circuitFor(target)
	circuits := len(s.delta.circuits)
	s.delta.mu.Unlock()

	// 승인받지 못하면 **만들지 않습니다.** 남의 회로를 빌려 주지도 않습니다 —
	// 공유 가변 회로는 대상별 marker/needed/backoff를 보존하지 못합니다.
	if c != nil {
		t.Fatal("승인 없이 회로를 만들었거나 남의 회로를 빌려 줬습니다")
	}
	if circuits != 0 {
		t.Fatalf("승인 없이 회로를 %d개 만들었습니다", circuits)
	}
	if got := s.budget.queued.Load(); got != 0 {
		t.Fatalf("만들지도 않은 회로가 %d 계상되었습니다", got)
	}
	if live, peak := s.budget.live.Load(), s.budget.peakLimit(); live > peak {
		t.Fatalf("I-C 위반 live=%d > %d", live, peak)
	}
	// 회로가 없어도 호출자가 터지지 않아야 합니다(nil 수신자 안전).
	c.noteRows(1)
	if !c.allows(indexBase, 0, s.budget.limit(), 0) {
		t.Fatal("회로가 없는데 막았습니다 — 막을 근거가 없습니다")
	}
	c.fail(indexBase, 1, 1, 1, 1, 1, true)

	// 상한이 풀리면 그때는 만들어지고 계상됩니다.
	s.cfg.MaxSearchIndexBytes = DefaultMaxSearchIndexBytes
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	s.delta.mu.Lock()
	made, _ := s.delta.circuitFor(target)
	circuits = len(s.delta.circuits)
	s.delta.mu.Unlock()
	if made == nil || circuits != 1 {
		t.Fatalf("상한이 풀렸는데 회로가 %d개입니다", circuits)
	}
	assertLedgerMatchesStructure(t, s, "circuit-admitted")
}

/* ── P1-A ③: 1024개 namespace의 문자열 본문까지 계상 ────────────────────── */

// TestStaleNamespaceStringBackingIsAccounted — staleNS가 붙잡는 namespace 문자열
// 본문은 입력에 따라 자랍니다. 그 성장이 원장에 그대로 나타나야 하고, 회수가
// 지우면 정확히 그만큼 빠져야 합니다.
func TestStaleNamespaceStringBackingIsAccounted(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	s := h.svc

	// 큐를 먼저 만들고 기준선을 잡습니다.
	s.delta.mu.Lock()
	q := s.delta.queueFor(h.gvr)
	base := s.budget.queued.Load()
	s.delta.mu.Unlock()
	if q.dynamic != 0 {
		t.Fatalf("시작부터 동적 몫이 %d입니다", q.dynamic)
	}

	// 서로 다른 1024개 namespace를 낡음으로 만듭니다(추적 상한과 같은 수).
	names := make([]string, 0, maxStaleTracked)
	var wantDyn int64
	for i := 0; i < maxStaleTracked; i++ {
		ns := fmt.Sprintf("tenant-%05d-namespace", i) // 길이가 있는 실제 형태
		names = append(names, ns)
		wantDyn += int64(len(ns))
		s.requestResync(h.gvr, ns, uint64(100+i))
	}

	s.delta.mu.Lock()
	q = s.delta.queueFor(h.gvr)
	gotDyn, count, gvrStale := q.dynamic, q.staleNS.count, q.gvrStale
	s.delta.mu.Unlock()

	if gvrStale {
		t.Fatalf("상한과 같은 수(%d)인데 GVR 전체로 승급했습니다", maxStaleTracked)
	}
	if count != maxStaleTracked {
		t.Fatalf("추적된 namespace가 %d개입니다 — %d개여야 합니다", count, maxStaleTracked)
	}
	if gotDyn != wantDyn {
		t.Fatalf("동적 몫이 %d입니다 — 문자열 본문 합 %d여야 합니다", gotDyn, wantDyn)
	}
	if got := s.budget.queued.Load(); got != base+wantDyn {
		t.Fatalf("원장이 %d입니다 — 기준선 %d + 문자열 %d여야 합니다", got, base, wantDyn)
	}
	assertLedgerMatchesStructure(t, s, "stale-strings")

	// 마커도 슬롯에 함께 살아야 합니다(별도 맵이 자라지 않습니다).
	s.delta.mu.Lock()
	mark := q.staleNS.mark(names[7])
	s.delta.mu.Unlock()
	if mark != uint64(107) {
		t.Fatalf("슬롯 마커가 %d입니다 — 107이어야 합니다", mark)
	}

	// 하나를 회수하면 그 문자열 몫만 빠집니다.
	s.delta.mu.Lock()
	s.delta.removeStaleLocked(q, names[7])
	afterDyn := q.dynamic
	s.delta.mu.Unlock()
	if want := wantDyn - int64(len(names[7])); afterDyn != want {
		t.Fatalf("하나를 뺀 뒤 동적 몫이 %d입니다 — %d여야 합니다", afterDyn, want)
	}
	if got := s.budget.queued.Load(); got != base+afterDyn {
		t.Fatalf("원장이 %d입니다 — %d여야 합니다", got, base+afterDyn)
	}
	assertLedgerMatchesStructure(t, s, "stale-strings-removed")

	// 전부 지우면 정확히 기준선으로 돌아옵니다.
	s.delta.mu.Lock()
	s.delta.clearStaleLocked(q)
	s.delta.mu.Unlock()
	if got := s.budget.queued.Load(); got != base {
		t.Fatalf("전부 지운 뒤 원장이 %d입니다 — 기준선 %d여야 합니다", got, base)
	}
	assertNoNegativeAccounting(t, s, "stale-strings-cleared")
}

/* ── P1-A ④: 64 GVR 동시 생성 + 종단 이중 폐기 ─────────────────────────── */

// TestSixtyFourGVRStructuralAdmissionAndDoubleDiscard — 64개 GVR이 동시에 큐를
// 만들어도 원장은 구조 재계산과 정확히 같고 I-C를 넘지 않으며, 폐기를 두 번 해도
// 정확히 한 번만 빠집니다.
func TestSixtyFourGVRStructuralAdmissionAndDoubleDiscard(t *testing.T) {
	const resources = 64
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

	for i, gvr := range order {
		b := &handlerBinding{entry: entries[gvr], packed: entries[gvr].tokenPacked.Load(), namespaced: true}
		for k := 0; k < 200; k++ {
			s.enqueueKey(b, fmt.Sprintf("ns-%02d", k%8), fmt.Sprintf("row-%04d", k))
		}
		// 낡음도 섞어 동적 몫이 실제로 생기게 합니다.
		s.requestResync(gvr, fmt.Sprintf("tenant-%03d-namespace", i), uint64(1000+i))
		assertLedgerMatchesStructure(t, s, fmt.Sprintf("gvr-%02d", i))
	}
	s.delta.mu.Lock()
	queues := len(s.delta.queues)
	s.delta.mu.Unlock()
	if queues != resources {
		t.Fatalf("큐가 %d개입니다 — %d개여야 합니다", queues, resources)
	}

	// 종단: 두 번 폐기해도 정확히 한 번만 빠집니다.
	for round := 0; round < 2; round++ {
		for _, gvr := range order {
			entries[gvr].discard(s)
		}
		if got := s.budget.queued.Load(); got != 0 {
			t.Fatalf("%d번째 폐기 뒤 queued가 %d입니다", round, got)
		}
		if got := s.budget.live.Load(); got != 0 {
			t.Fatalf("%d번째 폐기 뒤 live가 %d입니다", round, got)
		}
		assertNoNegativeAccounting(t, s, fmt.Sprintf("discard-%d", round))
	}
	s.delta.mu.Lock()
	left, circuits := len(s.delta.queues), len(s.delta.circuits)
	s.delta.mu.Unlock()
	if left != 0 || circuits != 0 {
		t.Fatalf("폐기 뒤 큐=%d 회로=%d가 남았습니다", left, circuits)
	}
}

/* ── P1-A ⑤: 처음 보는 GVR이 회로 상한에 부딪혀도 상한을 넘지 못합니다 ──── */

// TestUnseenGVRAtMaxCircuitsStaysBounded — 회로 맵이 가득 찬 상태에서 **처음 보는
// GVR**이 회로를 요청하면, 접을 것이 없으므로 예전 구현은 항목을 하나 더 만들어
// 상한을 넘겼습니다. 이제는 넘지 않고, 열린 회로도 잃지 않아야 합니다.
func TestUnseenGVRAtMaxCircuitsStaysBounded(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	s := h.svc
	now := indexBase

	// 회로 맵을 정확히 상한까지 채웁니다(모두 열린 상태 + 먼 백오프).
	filler := schema.GroupVersionResource{Group: "filler", Version: "v1", Resource: "things"}
	openUntil := now.Add(time.Hour)
	s.delta.mu.Lock()
	for i := 0; len(s.delta.circuits) < maxCircuits; i++ {
		c, _ := s.delta.circuitFor(recoveryTarget{gvr: filler, namespace: fmt.Sprintf("ns-%05d", i)})
		c.open, c.notBefore, c.lastNeeded = true, openUntil, 1<<40
		if i > maxCircuits*2 {
			s.delta.mu.Unlock()
			t.Fatal("회로 맵을 상한까지 채우지 못했습니다")
		}
	}
	full := len(s.delta.circuits)
	s.delta.mu.Unlock()
	if full != maxCircuits {
		t.Fatalf("회로가 %d개입니다 — 상한 %d까지 채워야 합니다", full, maxCircuits)
	}
	assertLedgerMatchesStructure(t, s, "circuits-full")

	// **처음 보는 GVR**이 회로를 요청합니다.
	unseen := schema.GroupVersionResource{Group: "unseen", Version: "v1", Resource: "things"}
	s.delta.mu.Lock()
	c, _ := s.delta.circuitFor(recoveryTarget{gvr: unseen, namespace: "prod"})
	after := len(s.delta.circuits)
	s.delta.mu.Unlock()

	if c == nil {
		t.Fatal("회로를 돌려주지 않았습니다")
	}
	if after > maxCircuits {
		t.Fatalf("처음 보는 GVR 때문에 회로가 %d개가 되었습니다 — 상한 %d", after, maxCircuits)
	}
	// 열린 회로가 조용히 사라지지 않아야 합니다 — 접혔다면 GVR 회로가 물려받습니다.
	s.delta.mu.Lock()
	var openLeft int
	for _, cc := range s.delta.circuits {
		if cc.open {
			openLeft++
		}
	}
	whole, hasWhole := s.delta.circuits[recoveryTarget{gvr: filler, whole: true}]
	s.delta.mu.Unlock()
	if openLeft == 0 && !hasWhole {
		t.Fatal("열린 회로가 흔적 없이 사라졌습니다")
	}
	if hasWhole && !whole.open {
		t.Fatal("승급된 회로가 열림 상태를 물려받지 않았습니다")
	}
	assertLedgerMatchesStructure(t, s, "circuits-unseen-gvr")
}

/* ── P1-A ⑤': ack이 마커까지 지웁니다 ──────────────────────────────────── */

// TestAckCoveredClearsNamespaceMarkers — 목록이 마커를 덮으면 staleNS와 함께
// **namespace별 마커도** 사라져야 합니다. 남으면 다음 드롭이 옛 지문과 비교되어
// 입력이 바뀌었는데도 "그대로"로 읽힙니다.
func TestAckCoveredClearsNamespaceMarkers(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	s := h.svc
	s.eventSeq.Store(50)
	s.requestResync(h.gvr, "prod", 50)

	s.delta.mu.Lock()
	q := s.delta.queueFor(h.gvr)
	before := q.markerFor(recoveryTarget{gvr: h.gvr, namespace: "prod"})
	dynBefore := q.dynamic
	s.delta.mu.Unlock()
	if before != 50 {
		t.Fatalf("마커가 %d입니다 — 50이어야 합니다", before)
	}
	if dynBefore == 0 {
		t.Fatal("동적 몫이 계상되지 않았습니다")
	}

	// 목록이 그 마커를 덮습니다.
	s.snapMu.Lock()
	s.ackCoveredLocked(h.gvr, 60)
	s.snapMu.Unlock()

	s.delta.mu.Lock()
	q = s.delta.queueFor(h.gvr)
	after := q.markerFor(recoveryTarget{gvr: h.gvr, namespace: "prod"})
	count, dynAfter := q.staleNS.count, q.dynamic
	s.delta.mu.Unlock()

	if count != 0 {
		t.Fatalf("ack 뒤에도 낡은 namespace가 %d개입니다", count)
	}
	if after != 0 {
		t.Fatalf("ack 뒤에도 namespace 마커가 %d로 남았습니다 — 다음 드롭이 옛 지문과 비교됩니다", after)
	}
	if dynAfter != 0 {
		t.Fatalf("ack 뒤에도 동적 몫이 %d 남았습니다", dynAfter)
	}
	assertLedgerMatchesStructure(t, s, "ack-cleared")
}

/* ── P1-B: 예약이 실제 할당을 덮는지 ───────────────────────────────────── */

// observedApplyBytes는 적용이 **실제로 만든** 후보 live 바이트를 계측에서 되짚습니다.
//
// 복사된 리프 entry는 posting(8B)과 행(21B)이 섞여 있으므로 큰 쪽으로 셉니다.
// 복사된 내부 노드도 큰 쪽(행 디렉터리 노드)으로 셉니다. 과대 추정이므로
// "예약 >= 이 값"이 성립하면 예약은 확실히 실제를 덮습니다.
func observedApplyBytes(s *Service, base [3]int64) int64 {
	entries := s.delta.directoryCopies.Load() - base[0]
	nodes := s.delta.nodesCopied.Load() - base[1]
	seps := s.delta.sepBytes.Load() - base[2]
	entryBytes := int64(rowEntryBytes)
	if postEntryBytes > entryBytes {
		entryBytes = postEntryBytes
	}
	nodeBytes := int64(postInternalBytes)
	if rowInternalBytes > nodeBytes {
		nodeBytes = rowInternalBytes
	}
	return entries*entryBytes + nodes*nodeBytes + seps
}

func applyCounterBase(s *Service) [3]int64 {
	return [3]int64{
		s.delta.directoryCopies.Load(),
		s.delta.nodesCopied.Load(),
		s.delta.sepBytes.Load(),
	}
}

// TestDeltaReservationCoversObservedCopies — 여러 형태의 배치에서 예약이
// **관측된 경로 복사 바이트 이상**이어야 합니다. 벤치의 B/op가 아니라 구조
// 계측(복사한 entry·노드·fence)에서 직접 되짚습니다.
func TestDeltaReservationCoversObservedCopies(t *testing.T) {
	saturated := make(map[string]string, MaxLabelKeysPerObject)
	for i := 0; i < MaxLabelKeysPerObject; i++ {
		saturated[fmt.Sprintf("app.kubernetes.io/part-of-%02d", i)] = fmt.Sprintf("payments-tier-%02d", i)
	}

	cases := []struct {
		name       string
		namespaced bool
		seed       func() []*metav1.PartialObjectMetadata
		mutate     func(h *deltaHarness, t *testing.T) int
	}{
		{
			name:       "분산 배치 1건",
			namespaced: true,
			seed: func() []*metav1.PartialObjectMetadata {
				rows := make([]*metav1.PartialObjectMetadata, 0, 512)
				for i := 0; i < 512; i++ {
					rows = append(rows, metaRow(fmt.Sprintf("ns-%03d", i%64),
						fmt.Sprintf("row-%04d", i), fmt.Sprintf("uid-%04d", i), nil))
				}
				return rows
			},
			mutate: func(h *deltaHarness, t *testing.T) int {
				h.upsert(t, metaRow("ns-007", "row-0007", "uid-0007", map[string]string{"app": "payments"}))
				return 1
			},
		},
		{
			name:       "라벨 포화 배치",
			namespaced: true,
			seed: func() []*metav1.PartialObjectMetadata {
				rows := make([]*metav1.PartialObjectMetadata, 0, 128)
				for i := 0; i < 128; i++ {
					rows = append(rows, metaRow("prod", fmt.Sprintf("row-%04d", i),
						fmt.Sprintf("uid-%04d", i), saturated))
				}
				return rows
			},
			mutate: func(h *deltaHarness, t *testing.T) int {
				for i := 0; i < 16; i++ {
					next := map[string]string{}
					for k, v := range saturated {
						next[k] = v + "-next"
					}
					h.upsert(t, metaRow("prod", fmt.Sprintf("row-%04d", i),
						fmt.Sprintf("uid-%04d", i), next))
				}
				return 16
			},
		},
		{
			name:       "뜨거운 namespace",
			namespaced: true,
			seed: func() []*metav1.PartialObjectMetadata {
				rows := make([]*metav1.PartialObjectMetadata, 0, 2000)
				for i := 0; i < 2000; i++ {
					rows = append(rows, metaRow("hot", fmt.Sprintf("row-%05d", i),
						fmt.Sprintf("uid-%05d", i), map[string]string{"app": "payments"}))
				}
				return rows
			},
			mutate: func(h *deltaHarness, t *testing.T) int {
				for i := 0; i < 200; i++ {
					h.upsert(t, metaRow("hot", fmt.Sprintf("row-%05d", i),
						fmt.Sprintf("uid-%05d", i), map[string]string{"app": "ledger"}))
				}
				return 200
			},
		},
		{
			name:       "cluster-scoped",
			namespaced: false,
			seed: func() []*metav1.PartialObjectMetadata {
				rows := make([]*metav1.PartialObjectMetadata, 0, 300)
				for i := 0; i < 300; i++ {
					rows = append(rows, metaRow("", fmt.Sprintf("node-%04d", i),
						fmt.Sprintf("uid-%04d", i), map[string]string{"role": "worker"}))
				}
				return rows
			},
			mutate: func(h *deltaHarness, t *testing.T) int {
				for i := 0; i < 32; i++ {
					h.upsert(t, metaRow("", fmt.Sprintf("node-%04d", i),
						fmt.Sprintf("uid-%04d", i), map[string]string{"role": "control-plane"}))
				}
				return 32
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gvr := scopedGVR
			kind := "Service"
			if !tc.namespaced {
				// cluster-scoped 케이스는 이 테스트가 직접 만드는 GVR을 씁니다.
				// 하네스가 자기 discovery를 세우므로 전역 픽스처가 필요 없습니다.
				gvr = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
				kind = "Node"
			}
			h := newDeltaHarnessFor(t, gvr, kind, tc.namespaced, tc.seed()...)
			n := tc.mutate(h, t)

			base := applyCounterBase(h.svc)
			if got := h.flush(t); got != n {
				t.Fatalf("적용된 키가 %d건입니다 — %d건이어야 합니다", got, n)
			}
			observed := observedApplyBytes(h.svc, base)
			reserved := deltaTransientBytes(n)
			if reserved < observed {
				t.Fatalf("예약이 실제 복사보다 작습니다: 예약=%d < 관측=%d (키 %d건)",
					reserved, observed, n)
			}
			// 예약이 관측을 덮되, 터무니없이 크지도 않아야 합니다(회귀 감지용 상한).
			if observed > 0 && reserved > observed*4096 {
				t.Fatalf("예약이 관측의 %d배입니다 — 상한이 좁은 서비스에서 배치가 통째로 막힙니다",
					reserved/observed)
			}
			assertNoNegativeAccounting(t, h.svc, tc.name)
		})
	}
}

// TestRecoveryReservationCoversWholeGVRRebuild — 10만 행 GVR 전체 회수의 예약이
// **고정 원본 + 완성된 측면 인덱스 + 조각 scratch**를 모두 덮어야 합니다.
//
// 실제로 10만 행을 만들지 않고, 같은 구조 상수로 독립 재계산한 하한과 비교합니다.
func TestRecoveryReservationCoversWholeGVRRebuild(t *testing.T) {
	for _, rows := range []int{1, 1000, 100_000} {
		reserved := recoveryReserveBytes(rows)

		// 독립 재계산: 고정 원본 + 측면 인덱스 보유 + 조각 scratch/COW.
		pinned := int64(rows) * (rowRecordFixedBytes + 2*stringHeaderBytes + bootstrapRowInputBytes)
		side := int64(rows) * (maxRowTokens*postEntryBytes*postLeafMax/postLeafSplit +
			rowEntryBytes*rowLeafMax/rowLeafSplit + trieBytesPerSlot + rowRecordFixedBytes)
		chunk := int64(recoveryChunkRows)*(bootstrapRowInputBytes+bootstrapPartOpBytes) + deltaCOWPerKeyBytes
		want := pinned + side + chunk

		if reserved < want {
			t.Fatalf("%d행: 예약=%d < 재계산=%d", rows, reserved, want)
		}
		// 세 항이 모두 들어 있어야 합니다 — 하나라도 빠지면 이 하한을 못 넘습니다.
		if reserved < pinned {
			t.Fatalf("%d행: 고정 원본 몫(%d)이 빠졌습니다", rows, pinned)
		}
		if reserved-pinned < side {
			t.Fatalf("%d행: 측면 인덱스 몫(%d)이 빠졌습니다", rows, side)
		}
		if reserved-pinned-side < chunk {
			t.Fatalf("%d행: 조각 scratch 몫(%d)이 빠졌습니다", rows, chunk)
		}
	}
}

// TestHeldEventApplyIsReservedBeforeAllocation — 보류분 적용도 할당입니다.
// 회수가 끝날 때 보류 키가 있으면, 그 몫이 티켓 예약에 **미리** 들어가야 합니다.
func TestHeldEventApplyIsReservedBeforeAllocation(t *testing.T) {
	h := newDeltaHarness(t,
		metaRow("prod", "payments-api", "uid-0", nil),
		metaRow("prod", "ledger", "uid-l", nil),
	)
	s := h.svc
	pinTicket(t, h, "prod")

	s.delta.mu.Lock()
	ticket := s.delta.ticket
	s.delta.mu.Unlock()
	if ticket == nil {
		t.Fatal("회수 티켓이 잡히지 않았습니다")
	}

	// 보류분을 만듭니다.
	const heldKeys = 4
	for i := 0; i < heldKeys; i++ {
		obj := metaRow("prod", fmt.Sprintf("held-%d", i), fmt.Sprintf("uid-h%d", i), nil)
		if err := h.store.Add(obj); err != nil {
			t.Fatal(err)
		}
		s.enqueueKey(h.binding, "prod", obj.Name)
		h.flush(t)
	}
	s.delta.mu.Lock()
	held := len(s.delta.queueFor(h.gvr).hold)
	beforeReserved := ticket.reserved
	beforeChunk := ticket.chunkReserved
	step := ticket.step
	s.delta.mu.Unlock()
	if held == 0 {
		t.Fatal("보류분이 만들어지지 않았습니다")
	}

	// 보류 키 수만큼 조각 예약을 늘려야 합니다(할당 전에).
	if ok := s.growTicketReservation(ticket, step, held); !ok {
		t.Fatal("보류분 예약이 거절되었습니다")
	}
	s.delta.mu.Lock()
	afterReserved := ticket.reserved
	afterChunk := ticket.chunkReserved
	s.delta.mu.Unlock()

	want := int64(held)*(bootstrapRowInputBytes+bootstrapPartOpBytes) + deltaCOWPerKeyBytes
	if afterChunk < want {
		t.Fatalf("조각 예약이 %d입니다 — 보류 %d건에 필요한 %d 이상이어야 합니다",
			afterChunk, held, want)
	}
	if afterReserved < beforeReserved {
		t.Fatalf("예약이 줄었습니다: %d → %d", beforeReserved, afterReserved)
	}
	if delta := afterChunk - beforeChunk; delta > 0 && afterReserved-beforeReserved != delta {
		t.Fatalf("티켓 예약이 조각 증가분(%d)과 어긋납니다: %d", delta, afterReserved-beforeReserved)
	}
	if got := s.budget.recovery.Load(); got < afterReserved {
		t.Fatalf("원장 회수 예약(%d)이 티켓 예약(%d)보다 작습니다", got, afterReserved)
	}
	// 같은 크기를 다시 요청하면 **추가 예약이 없어야** 합니다(중복 예약 금지).
	if ok := s.growTicketReservation(ticket, step, held); !ok {
		t.Fatal("두 번째 요청이 거절되었습니다")
	}
	s.delta.mu.Lock()
	twice := ticket.reserved
	s.delta.mu.Unlock()
	if twice != afterReserved {
		t.Fatalf("같은 크기인데 예약이 %d → %d로 늘었습니다", afterReserved, twice)
	}
	assertNoNegativeAccounting(t, s, "held-reserve")
}

/* ── P1-C: 용량을 실제로 보는 무회전 재시도 ─────────────────────────────── */

// TestPartialReliefDoesNotTriggerRebuild — 1바이트짜리 부분 완화는 전체 재구성을
// 부르지 못하고, 필요했던 만큼이 실제로 풀렸을 때 **정확히 한 번** 성공해야 합니다.
func TestPartialReliefDoesNotTriggerRebuild(t *testing.T) {
	hogGVR := schema.GroupVersionResource{Group: "hog.example.com", Version: "v1", Resource: "things"}
	targetGVR := schema.GroupVersionResource{Group: "target.example.com", Version: "v1", Resource: "things"}

	hogRows := make([]*metav1.PartialObjectMetadata, 0, 512)
	for i := 0; i < 512; i++ {
		hogRows = append(hogRows, metaRow("prod", fmt.Sprintf("hog-%04d", i),
			fmt.Sprintf("uid-hog-%04d", i), nil))
	}
	targetRows := []*metav1.PartialObjectMetadata{
		metaRow("prod", "payments-api", "uid-target-1", nil),
		metaRow("prod", "payments-worker", "uid-target-2", nil),
	}
	h := newMultiHarness(t, map[schema.GroupVersionResource][]*metav1.PartialObjectMetadata{
		hogGVR:    hogRows,
		targetGVR: targetRows,
	})
	s := h.svc
	for _, gvr := range h.order {
		store := cache.NewStore(cache.MetaNamespaceKeyFunc)
		rows := hogRows
		if gvr == targetGVR {
			rows = targetRows
		}
		for _, r := range rows {
			if err := store.Add(r); err != nil {
				t.Fatal(err)
			}
		}
		s.entries[gvr].informer = &fakeInformer{store: store, synced: true}
	}
	hog, target := s.entries[hogGVR], s.entries[targetGVR]

	s.snapMu.Lock()
	s.installLeaseLocked(target, nil)
	target.setSnap(&entrySnapshot{index: indexOf(targetRows...)})
	s.snapMu.Unlock()
	target.bootstrapped.Store(false)
	target.dirty.Store(true)

	hogBytes := hog.ownerRetained.Load()
	if hogBytes <= 0 {
		t.Fatal("경쟁 GVR이 아무 바이트도 붙잡고 있지 않습니다")
	}
	setSearchBudget(s, hogBytes+1024)

	s.rebuildIndexes(false)
	if target.bootstrapped.Load() {
		t.Fatal("예산이 없는데 부트스트랩 완료로 표시되었습니다")
	}
	need := target.lastNeeded.Load()
	if need <= 0 {
		t.Fatal("필요했던 바이트가 기록되지 않았습니다")
	}
	baseBoot := s.delta.fullBootstraps.Load()

	// **부분 완화**: 필요한 양보다 훨씬 적게(1바이트) 상한을 올립니다.
	for i := 0; i < 5; i++ {
		setSearchBudget(s, s.budget.limit()+1)
		target.dirty.Store(true)
		s.rebuildIndexes(false)
	}
	if got := s.delta.fullBootstraps.Load() - baseBoot; got != 0 {
		t.Fatalf("1바이트 완화가 전체 빌드를 %d회 불렀습니다 — 회전입니다", got)
	}
	if target.bootstrapped.Load() {
		t.Fatal("부분 완화로 설치되었습니다")
	}

	// **충분한 완화**: 경쟁 GVR이 세대를 놓습니다(설정 상한은 그대로).
	beforeMax := s.budget.limit()
	s.snapMu.Lock()
	s.installLeaseLocked(hog, nil)
	s.snapMu.Unlock()
	if s.budget.limit() != beforeMax {
		t.Fatal("테스트가 설정 상한을 건드렸습니다")
	}
	if avail := s.availableRetained(target); avail < need {
		t.Fatalf("가용 용량 %d가 필요량 %d에 못 미칩니다 — 픽스처가 잘못되었습니다", avail, need)
	}

	target.dirty.Store(true)
	s.rebuildIndexes(false)
	if got := s.delta.fullBootstraps.Load() - baseBoot; got != 1 {
		t.Fatalf("충분한 완화 뒤 전체 빌드가 %d회입니다 — 정확히 1회여야 합니다", got)
	}
	if !target.bootstrapped.Load() {
		t.Fatal("충분히 풀렸는데 부트스트랩이 완료되지 않았습니다")
	}
	if st := target.currentStatus(); st.state != StateReady {
		t.Fatalf("설치 뒤 상태가 %s입니다", st.state)
	}

	// 성공한 뒤에는 더 이상 빌드하지 않습니다.
	for i := 0; i < 3; i++ {
		target.dirty.Store(true)
		s.rebuildIndexes(false)
	}
	if got := s.delta.fullBootstraps.Load() - baseBoot; got != 1 {
		t.Fatalf("성공 뒤에도 전체 빌드가 %d회입니다", got)
	}
	assertNoNegativeAccounting(t, s, "partial-relief")
}

// TestRecoveryCircuitObservesActualCapacity — 회수 회로도 설정 상한이 아니라
// **실제 가용 용량**을 봅니다. 필요량 미만의 부분 완화는 열지 않고, 필요량만큼
// 풀리면 쿨다운 뒤 닫혀야 합니다.
func TestRecoveryCircuitObservesActualCapacity(t *testing.T) {
	now := indexBase
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	s := h.svc
	s.cfg.Now = func() time.Time { return now }

	target := recoveryTarget{gvr: h.gvr, namespace: "prod"}
	s.requestResync(h.gvr, "prod", 10)

	// 지금 용량으로는 감당할 수 없는 필요량으로 실패를 기록합니다.
	avail := s.availableFor(h.gvr)
	need := avail * 2
	if need <= 0 {
		t.Fatalf("가용 용량이 %d입니다", avail)
	}
	s.delta.mu.Lock()
	q := s.delta.queueFor(h.gvr)
	c, _ := s.delta.circuitFor(target)
	c.fail(now, q.markerFor(target), 100, need, s.budget.limit(), avail, true)
	s.delta.mu.Unlock()

	// 백오프가 지나도, 부분 완화(필요량의 절반 미만)로는 열리지 않습니다.
	later := now.Add(recoveryBackoffMax * 4)
	s.delta.mu.Lock()
	partial := c.allows(later, q.markerFor(target), s.budget.limit(), need/4)
	s.delta.mu.Unlock()
	if partial {
		t.Fatal("필요량에 못 미치는 부분 완화가 회로를 열었습니다 — 폭풍이 됩니다")
	}

	// 쿨다운 전이면 필요량이 채워져도 열리지 않습니다(백오프가 먼저입니다).
	s.delta.mu.Lock()
	early := c.allows(now, q.markerFor(target), s.budget.limit(), need)
	s.delta.mu.Unlock()
	if early {
		t.Fatal("백오프가 끝나기 전에 열렸습니다")
	}

	// 필요량만큼 풀리면 쿨다운 뒤 닫힙니다.
	s.delta.mu.Lock()
	opened := c.allows(later, q.markerFor(target), s.budget.limit(), need)
	stillOpen := c.open
	s.delta.mu.Unlock()
	if !opened {
		t.Fatal("필요했던 만큼 풀렸는데 회로가 닫힌 채입니다")
	}
	if stillOpen {
		t.Fatal("열린 뒤에도 회로가 open으로 남았습니다")
	}

	// 지문이 없으면(필요량 미상) 용량만으로는 열지 않습니다.
	s.delta.mu.Lock()
	c2, _ := s.delta.circuitFor(recoveryTarget{gvr: h.gvr, namespace: "other"})
	c2.open, c2.lastNeeded, c2.lastMarker, c2.lastMax = true, 0, 0, s.budget.limit()
	c2.notBefore = time.Time{}
	blind := c2.allows(later, 0, s.budget.limit(), 1<<40)
	s.delta.mu.Unlock()
	if blind {
		t.Fatal("필요량을 모르는 회로가 용량만으로 열렸습니다 — 근거 없는 재시도입니다")
	}
}

// TestSteadyDeltaStillHasNoStoreListOrFullBuild — P1-A/B/C 변경 뒤에도 정상 델타
// 경로는 목록 재조회·전체 빌드를 하지 않아야 합니다.
func TestSteadyDeltaStillHasNoStoreListOrFullBuild(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 256)
	for i := 0; i < 256; i++ {
		rows = append(rows, metaRow("prod", fmt.Sprintf("row-%04d", i),
			fmt.Sprintf("uid-%04d", i), map[string]string{"app": "payments"}))
	}
	h := newDeltaHarness(t, rows...)
	baseList := h.svc.delta.storeListCalls.Load()
	baseBoot := h.svc.delta.fullBootstraps.Load()
	baseFull := h.svc.delta.fullRecoveries.Load()

	for round := 0; round < 4; round++ {
		for i := 0; i < 64; i++ {
			h.upsert(t, metaRow("prod", fmt.Sprintf("row-%04d", i),
				fmt.Sprintf("uid-%04d", i), map[string]string{"app": fmt.Sprintf("v%d", round)}))
		}
		for h.queueLen() > 0 {
			if h.flush(t) == 0 {
				break
			}
		}
	}
	if got := h.svc.delta.storeListCalls.Load() - baseList; got != 0 {
		t.Fatalf("정상 델타가 Store.List를 %d회 불렀습니다", got)
	}
	if got := h.svc.delta.fullBootstraps.Load() - baseBoot; got != 0 {
		t.Fatalf("정상 델타가 전체 빌드를 %d회 했습니다", got)
	}
	if got := h.svc.delta.fullRecoveries.Load() - baseFull; got != 0 {
		t.Fatalf("정상 델타가 전체 회수를 %d회 했습니다", got)
	}
	assertLedgerMatchesStructure(t, h.svc, "steady-delta")
	if got := h.svc.budget.inflight.Load(); got != 0 {
		t.Fatalf("적용 예약이 %d 남았습니다", got)
	}
}
