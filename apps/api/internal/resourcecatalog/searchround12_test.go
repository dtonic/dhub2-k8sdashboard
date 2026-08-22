package resourcecatalog

// Round 11 게이트 회귀 (소유권·수명·단조성·지속 회로)
// --------------------------------------------------------------------------
// 1. 회수 원본 수명   — 캡처 중 게시/은퇴가 겹쳐도 세대가 빌려진 채로 남습니다.
// 2. 깊은 소유 사본   — 큰 annotations/owners/finalizers가 회수에서 도달 불가.
// 4. 마커 단조성      — 11 뒤에 지연된 10이 와도 ack(covers=10)이 11을 지우지 않음.
// 5. 지속 회로        — 회로 씨앗을 못 만들어도 백오프가 남아 반복 회수가 없음.
// 6. 접기 대상 선택   — 단일/다중이 섞인 가득 찬 맵에서 반드시 자리를 만들고 백오프 보존.
//
// (보류 적용 예약은 searchround13_test.go가 실제 publishRecovery 경로로 검사합니다.)

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

/* ── 1. 캡처 중 게시/은퇴가 겹쳐도 세대는 빌려진 채로 ────────────────────── */

// TestRecoveryCaptureHoldsLeaseAcrossPublishRetire — 회수가 원본을 뜨는 동안
// 새 세대가 게시되어 옛 세대가 은퇴해도, **빌린 세대의 바이트는 마지막 독자가
// 놓을 때까지** retained/owner/live에 남아 있어야 합니다.
func TestRecoveryCaptureHoldsLeaseAcrossPublishRetire(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 200)
	for i := 0; i < 200; i++ {
		rows = append(rows, metaRow("prod", fmt.Sprintf("row-%04d", i),
			fmt.Sprintf("uid-%04d", i), map[string]string{"app": "payments"}))
	}
	h := newDeltaHarness(t, rows...)
	s := h.svc
	e := h.entry

	first := e.leasePtr.Load()
	if first == nil {
		t.Fatal("게시된 세대가 없습니다")
	}
	firstBytes := first.bytes

	// 회수가 원본을 빌린 상태를 만듭니다(핀 단계가 lease를 잡습니다).
	s.requestResync(h.gvr, "prod", 0)

	// **먼저** 1세대를 빌립니다(이 독자가 은퇴 세대를 붙잡습니다).
	reader, leased := e.acquireSearch(s)
	if reader == nil || leased == nil {
		t.Fatal("독자가 세대를 빌리지 못했습니다")
	}
	if reader != first {
		t.Fatal("독자가 1세대를 빌리지 못했습니다")
	}

	// 캡처와 게시를 겹칩니다.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.advanceRecovery(context.Background())
	}()
	built := buildSearchIndex(indexOf(rows...), "Service", true, hugeBudget, hugeBudget)
	if built.state != SearchReady {
		t.Fatalf("재빌드 실패: %s", built.state)
	}
	cur := e.load()
	next := &entrySnapshot{
		index: cur.index, sindex: built.index, searchState: SearchReady,
		indexVer: cur.indexVer + 1, searchVer: cur.searchVer + 1,
	}
	s.snapMu.Lock()
	published := s.publishLeaseLocked(e, next)
	if published {
		e.setSnap(next)
	}
	s.snapMu.Unlock()
	wg.Wait()

	if !published {
		t.Fatal("새 세대 게시가 거절되었습니다 — 픽스처가 상한을 넘었습니다")
	}
	// 옛 세대는 은퇴했지만 독자가 붙잡고 있으므로 **아직 회계에 남아야** 합니다.
	if !first.retired.Load() {
		t.Fatal("새 세대가 게시됐는데 옛 세대가 은퇴하지 않았습니다")
	}
	if got := s.searchBytes.Load(); got < firstBytes {
		t.Fatalf("독자가 붙잡고 있는데 회계가 %d로 줄었습니다(옛 세대 %d)", got, firstBytes)
	}
	if got := e.ownerRetained.Load(); got < firstBytes {
		t.Fatalf("소유자 원장이 %d입니다 — 은퇴 세대 %d를 포함해야 합니다", got, firstBytes)
	}
	if got := s.budget.retained.Load(); got < firstBytes {
		t.Fatalf("retained가 %d입니다 — 은퇴 세대를 포함해야 합니다", got)
	}

	// 놓으면 정확히 그만큼 빠집니다.
	beforeRelease := s.searchBytes.Load()
	s.releaseSearch(reader)
	if got := s.searchBytes.Load(); got != beforeRelease-firstBytes {
		t.Fatalf("해제가 %d만큼입니다 — %d여야 합니다", beforeRelease-got, firstBytes)
	}
	assertNoNegativeAccounting(t, s, "capture-publish-retire")
}

/* ── 2. 큰 부가 메타데이터는 회수 원본에서 도달 불가 ────────────────────── */

// TestRecoverySourceDropsHeavyObjectMetadata — 회수 소유 사본은 검색이 쓰는 것만
// 들고, annotations·ownerReferences·finalizers 같은 큰 부가 정보는 붙잡지
// 않아야 합니다(얕은 indexRow 복사는 그것을 전부 살려 둡니다).
func TestRecoverySourceDropsHeavyObjectMetadata(t *testing.T) {
	heavy := metaRow("solo", "only-row", "uid-solo", map[string]string{"app": "payments"})
	heavy.Annotations = map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": strings.Repeat("A", 128<<10),
	}
	heavy.Finalizers = []string{strings.Repeat("f", 4096)}
	heavy.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "ReplicaSet",
		Name: strings.Repeat("o", 4096), UID: "uid-owner",
	}}

	h := newDeltaHarnessFor(t, scopedGVR, "Service", true,
		metaRow("bulk", "row-0", "uid-0", nil),
		heavy,
	)
	pinTicket(t, h, "solo")

	h.svc.delta.mu.Lock()
	tk := h.svc.delta.ticket
	var src []recoveryRow
	if tk != nil {
		src = tk.src
	}
	h.svc.delta.mu.Unlock()

	if len(src) != 1 {
		t.Fatalf("소유 사본이 %d행입니다 — 1행이어야 합니다", len(src))
	}
	row := src[0]
	if row.uid != "uid-solo" || row.name != "only-row" {
		t.Fatalf("소유 사본 신원이 어긋납니다: %+v", row)
	}
	// 소유 사본이 붙잡는 문자열 총량은 **검색 토큰 수준**이어야 합니다.
	// 원본 객체를 붙잡고 있었다면 128KiB 주석이 그대로 살아 있습니다.
	var held int
	held += len(row.namespace) + len(row.name) + len(row.uid)
	for _, tok := range row.labels {
		held += len(tok)
	}
	if held > 4096 {
		t.Fatalf("소유 사본이 %d바이트를 붙잡고 있습니다 — 검색 토큰만 들어야 합니다", held)
	}
	if len(row.labels) > maxRowTokens {
		t.Fatalf("정규화 토큰이 %d개입니다 — 상한 %d", len(row.labels), maxRowTokens)
	}
}

/* ── 4. 마커 단조성 ─────────────────────────────────────────────────────── */

// TestDelayedOlderMarkerCannotErodeNewer — 마커 11이 기록된 뒤 지연된 10이
// 도착해도 마커는 11로 남아야 하고, covers=10짜리 ack은 그것을 지우면 안 됩니다.
func TestDelayedOlderMarkerCannotErodeNewer(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	s := h.svc

	s.requestResync(h.gvr, "alpha", 11)
	s.requestResync(h.gvr, "beta", 10) // 늦게 도착한 **더 오래된** 이벤트

	s.delta.mu.Lock()
	q := s.delta.queueFor(h.gvr)
	global := q.markerSeq
	alpha := q.markerFor(recoveryTarget{gvr: h.gvr, namespace: "alpha"})
	beta := q.markerFor(recoveryTarget{gvr: h.gvr, namespace: "beta"})
	s.delta.mu.Unlock()

	if global != 11 {
		t.Fatalf("전역 마커가 %d입니다 — 단조 증가라면 11이어야 합니다", global)
	}
	if alpha != 11 || beta != 10 {
		t.Fatalf("namespace 마커가 alpha=%d beta=%d입니다 — 11/10이어야 합니다", alpha, beta)
	}

	// covers=10은 **아직 11을 덮지 못했습니다.** ack이 성립하면 안 됩니다.
	s.snapMu.Lock()
	s.ackCoveredLocked(h.gvr, 10)
	s.snapMu.Unlock()

	s.delta.mu.Lock()
	q = s.delta.queueFor(h.gvr)
	stillAlpha := q.staleNS.has("alpha")
	stillGlobal := q.markerSeq
	s.delta.mu.Unlock()

	if !stillAlpha {
		t.Fatal("covers=10이 마커 11짜리 회수를 지웠습니다 — 유실입니다")
	}
	if stillGlobal != 11 {
		t.Fatalf("ack 뒤 전역 마커가 %d입니다 — 11이어야 합니다", stillGlobal)
	}

	// 진짜 최신 마커를 덮으면 그때 지워집니다.
	s.snapMu.Lock()
	s.ackCoveredLocked(h.gvr, 11)
	s.snapMu.Unlock()
	s.delta.mu.Lock()
	q = s.delta.queueFor(h.gvr)
	cleared := q.staleNS.count == 0 && q.markerSeq == 0
	s.delta.mu.Unlock()
	if !cleared {
		t.Fatal("진짜 최신 마커를 덮었는데도 지워지지 않았습니다")
	}
}

/* ── 5. 회로 씨앗을 못 만들어도 백오프가 남습니다 ───────────────────────── */

// TestCircuitSeedFailureStillBacksOff — 회로 맵 씨앗(4KiB)을 승인받지 못하는
// 좁은 상한에서도, 낡은 대상에게는 **큐 내장 회로**가 지속 백오프를 줍니다.
// 반복 tick이 같은 회수를 몇 번이고 다시 시도하면 안 됩니다.
func TestCircuitSeedFailureStillBacksOff(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	s := h.svc
	now := indexBase
	s.cfg.Now = func() time.Time { return now }

	// 큐는 이미 있습니다(내장 회로도 함께 선계상되어 있습니다).
	s.requestResync(h.gvr, "prod", 5)

	// 남은 용량을 회로 씨앗(4096)보다 **적게** 만듭니다.
	s.delta.mu.Lock()
	q := s.delta.queueFor(h.gvr)
	live := s.budget.live.Load()
	s.delta.mu.Unlock()
	if q == nil {
		t.Fatal("큐가 없습니다")
	}
	// peak = live + 여유(512..4351). 씨앗은 못 만들고 티켓은 만들 수 있는 구간입니다.
	const slack = 2048
	s.budget.max.Store((live + slack) / searchPeakMultiplier)
	if avail := s.budget.peakLimit() - s.budget.live.Load(); avail >= circuitsMapSeedBytes {
		t.Fatalf("픽스처가 느슨합니다: 여유 %d >= 씨앗 %d", avail, int64(circuitsMapSeedBytes))
	}

	s.delta.mu.Lock()
	c, fingerprint := s.delta.circuitForQ(q, recoveryTarget{gvr: h.gvr, namespace: "prod"})
	circuits, seed := len(s.delta.circuits), s.delta.circuitsSeed
	s.delta.mu.Unlock()

	if circuits != 0 || seed != 0 {
		t.Fatalf("씨앗을 못 만들어야 하는데 회로=%d 씨앗=%d입니다", circuits, seed)
	}
	if c == nil {
		t.Fatal("낡은 대상에 지속 회로가 없습니다 — 백오프를 기억할 곳이 없습니다")
	}
	if c != &q.fallback {
		t.Fatal("내장 회로가 아닌 것을 돌려줬습니다")
	}
	if !fingerprint.whole {
		t.Fatal("내장 회로는 GVR 전체 의미여야 합니다")
	}

	// 실패를 기록하면 **지속되는** 백오프가 생겨야 합니다.
	c.fail(now, q.markerFor(fingerprint), 1, 1<<40, s.budget.limit(), 0, true)
	if !q.fallback.open || q.fallback.backoff <= 0 {
		t.Fatalf("내장 회로에 백오프가 남지 않았습니다: open=%v backoff=%v",
			q.fallback.open, q.fallback.backoff)
	}

	// 입력·용량이 그대로면 반복 tick이 회수를 다시 잡지 않아야 합니다.
	picked := 0
	for i := 0; i < 8; i++ {
		s.delta.mu.Lock()
		s.delta.cooldownUntil = time.Time{}
		s.delta.ticket = nil
		if tk := s.pickRecoveryLocked(now.Add(time.Duration(i) * time.Millisecond)); tk != nil {
			picked++
		}
		s.delta.mu.Unlock()
	}
	if picked != 0 {
		t.Fatalf("같은 입력·용량인데 회수를 %d회 다시 잡았습니다 — 폭풍입니다", picked)
	}

	// 내장 회로는 **GVR마다 하나**입니다. 다른 GVR과 상태를 공유하면 안 됩니다.
	other := newDeltaHarness(t, metaRow("prod", "other", "uid-2", nil))
	other.svc.delta.mu.Lock()
	oq := other.svc.delta.queueFor(other.gvr)
	other.svc.delta.mu.Unlock()
	if oq != nil && (&oq.fallback == &q.fallback) {
		t.Fatal("서로 다른 GVR이 같은 내장 회로를 공유합니다")
	}
}

/* ── 6. 가득 찬 회로 맵에서 반드시 자리를 만듭니다 ──────────────────────── */

// TestFoldChoosesVictimWithTwoCircuits — 단일 회로 GVR과 다중 회로 GVR이 섞여
// 가득 찬 맵에서, 접기는 **둘 이상 가진 GVR**을 골라 실제로 자리를 만들어야 하고
// 접힌 회로의 백오프·열림 상태는 승급된 회로가 물려받아야 합니다.
func TestFoldChoosesVictimWithTwoCircuits(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	s := h.svc
	now := indexBase

	single := schema.GroupVersionResource{Group: "aaa-single", Version: "v1", Resource: "things"}
	multi := schema.GroupVersionResource{Group: "zzz-multi", Version: "v1", Resource: "things"}

	s.delta.mu.Lock()
	// 단일 회로 GVR을 **사전순 앞쪽**에 둡니다. 개수를 보지 않으면 이쪽이 뽑힙니다.
	sc, _ := s.delta.circuitFor(recoveryTarget{gvr: single, namespace: "only"})
	if sc == nil {
		s.delta.mu.Unlock()
		t.Fatal("단일 회로를 만들지 못했습니다")
	}
	// 나머지를 multi로 채워 상한에 닿게 합니다.
	far := now.Add(time.Hour)
	for i := 0; len(s.delta.circuits) < maxCircuits; i++ {
		c, _ := s.delta.circuitFor(recoveryTarget{gvr: multi, namespace: fmt.Sprintf("ns-%05d", i)})
		if c == nil {
			s.delta.mu.Unlock()
			t.Fatal("회로 맵을 채우지 못했습니다")
		}
		c.open, c.notBefore, c.backoff = true, far, recoveryBackoffMax
	}
	full := len(s.delta.circuits)
	multiBefore := s.delta.foldableCountLocked(multi)
	s.delta.mu.Unlock()

	if full != maxCircuits {
		t.Fatalf("회로가 %d개입니다 — 상한 %d까지 채워야 합니다", full, maxCircuits)
	}
	if multiBefore < 2 {
		t.Fatalf("multi GVR의 회로가 %d개입니다 — 둘 이상이어야 합니다", multiBefore)
	}

	// 처음 보는 GVR이 회로를 요청합니다 → 자리를 만들어야 합니다.
	unseen := schema.GroupVersionResource{Group: "mmm-unseen", Version: "v1", Resource: "things"}
	s.delta.mu.Lock()
	got, _ := s.delta.circuitFor(recoveryTarget{gvr: unseen, namespace: "x"})
	after := len(s.delta.circuits)
	singleLeft := s.delta.foldableCountLocked(single)
	whole, hasWhole := s.delta.circuits[recoveryTarget{gvr: multi, whole: true}]
	s.delta.mu.Unlock()

	if got == nil {
		t.Fatal("가득 찬 맵에서 자리를 만들지 못했습니다 — 단일 회로만 접었을 수 있습니다")
	}
	if after > maxCircuits {
		t.Fatalf("회로가 %d개가 되었습니다 — 상한 %d", after, maxCircuits)
	}
	if singleLeft != 1 {
		t.Fatalf("단일 회로 GVR이 %d개로 바뀌었습니다 — 접어도 자리가 생기지 않는 대상입니다", singleLeft)
	}
	if !hasWhole {
		t.Fatal("다중 회로 GVR이 전체 회로로 승급되지 않았습니다")
	}
	if !whole.open {
		t.Fatal("승급된 회로가 열림 상태를 물려받지 않았습니다")
	}
	if whole.notBefore.Before(far) {
		t.Fatalf("승급된 회로의 백오프가 %v로 앞당겨졌습니다 — %v 이후여야 합니다", whole.notBefore, far)
	}
	if whole.backoff < recoveryBackoffMax {
		t.Fatalf("승급된 회로의 backoff가 %v입니다 — 가장 큰 값을 물려받아야 합니다", whole.backoff)
	}
	assertLedgerMatchesStructure(t, s, "fold-victim")
}
