package resourcecatalog

// Round 15 (보강) — 행 디렉터리 재균형·신원 조회·정규 집합 교체·회계 보조
// --------------------------------------------------------------------------
// 앞선 파일이 큐·회수·회로의 상태 전이를 봤다면, 여기서는 그 아래 자료구조와
// 회계 보조가 지키는 약속을 봅니다.
//
//	① 행 디렉터리는 여러 단계로 갈라졌다가 다시 합쳐져도 **행 수와 순서**가 정확하다.
//	② 신원 조회는 "없다"와 "모른다"를 구분한다(권위 있는 부재 vs 낡은 파티션).
//	③ label 멤버십이 뒤집히면 **정규 집합 전체**를 교체해 prevLCP가 다시 계산된다.
//	④ 예산 거절 뒤 쿨다운은 곧바로 같은 일을 다시 시작하지 못하게 막는다.

import (
	"fmt"
	"testing"
	"time"
)

/* ── ① 행 디렉터리 분할·재균형 ──────────────────────────────────────────── */

// TestRowDirectorySplitsRebalancesAndCountsExactly — 리프 상한(256)과 노드
// fanout(32)을 모두 넘겨 **여러 단계 트리**를 만든 뒤 대부분을 지웁니다.
//
// 삭제는 리프를 하한(64) 아래로 떨어뜨려 병합·재분배를 부르고, 그 결과 내부
// 노드도 하한(12) 아래로 내려가 노드 병합이 일어납니다. 그 과정에서 행이 하나도
// 사라지거나 되살아나면 안 되고, 이름 순서도 그대로여야 합니다.
func TestRowDirectorySplitsRebalancesAndCountsExactly(t *testing.T) {
	const inserted = 9000 // 9000/192 = 47 리프 → fanout 32를 넘겨 단계가 하나 더 생깁니다
	const kept = 300

	name := func(i int) string { return fmt.Sprintf("name-%04d", i) }

	insertOps := make([]rowOp, 0, inserted)
	for i := 0; i < inserted; i++ {
		insertOps = append(insertOps, rowOp{name: name(i), slot: uint32(i), weight: 1})
	}

	var st rowStats
	tree := rowApply(newRowTree(), sortRowOps(insertOps), &st)
	tree = balanceRowRoot(tree, &st)

	if got := rowTreeCount(tree); got != inserted {
		t.Fatalf("삽입 뒤 행 수가 %d입니다 — %d이어야 합니다", got, inserted)
	}
	if st.rowDelta != inserted {
		t.Fatalf("rowDelta가 %d입니다 — 삽입 %d건과 같아야 합니다", st.rowDelta, inserted)
	}
	if tree.leafLevel {
		t.Fatalf("루트가 아직 리프 단계입니다 — fanout %d를 넘겼으면 단계가 늘어야 합니다", treeFanout)
	}
	if got := tree.childCount(); got > treeFanout {
		t.Fatalf("루트 자식이 %d개입니다 — fanout %d 이하로 접혀야 합니다", got, treeFanout)
	}
	bytesFull := rowTreeBytes(tree)
	if bytesFull <= 0 {
		t.Fatalf("행 디렉터리 바이트가 %d입니다", bytesFull)
	}

	// 삽입한 모든 이름이 자기 슬롯으로 찾아져야 합니다.
	for _, i := range []int{0, 1, 191, 192, 4095, 8998, 8999} {
		slot, weight, ok := rowFind(tree, name(i))
		if !ok || slot != uint32(i) || weight != 1 {
			t.Fatalf("%s 조회가 (slot=%d weight=%d ok=%v)입니다", name(i), slot, weight, ok)
		}
	}

	// ── 대부분을 지웁니다: 리프 병합 → 노드 병합 ─────────────────────────
	removeOps := make([]rowOp, 0, inserted-kept)
	for i := 0; i < inserted-kept; i++ {
		removeOps = append(removeOps, rowOp{name: name(i), remove: true})
	}
	var dst rowStats
	tree = rowApply(tree, sortRowOps(removeOps), &dst)
	tree = balanceRowRoot(tree, &dst)

	if got := rowTreeCount(tree); got != kept {
		t.Fatalf("삭제 뒤 행 수가 %d입니다 — %d이어야 합니다", got, kept)
	}
	if want := int64(-(inserted - kept)); dst.rowDelta != want {
		t.Fatalf("rowDelta가 %d입니다 — %d이어야 합니다", dst.rowDelta, want)
	}
	if dst.leafDelta >= 0 {
		t.Fatalf("leafDelta가 %d입니다 — 병합이 일어났다면 리프가 줄어야 합니다", dst.leafDelta)
	}
	if got := rowTreeBytes(tree); got >= bytesFull {
		t.Fatalf("삭제 뒤 바이트가 %d로 %d에서 줄지 않았습니다 — 용량이 실제로 풀리지 않았습니다",
			got, bytesFull)
	}

	// 남은 것만, 정확히, 이름 순으로 남아야 합니다.
	var seen []string
	rowEachAll(tree, func(n string, slot uint32) bool {
		seen = append(seen, n)
		return true
	})
	if len(seen) != kept {
		t.Fatalf("순회가 %d건을 냈습니다 — %d건이어야 합니다", len(seen), kept)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] >= seen[i] {
			t.Fatalf("순회 순서가 어긋났습니다: %q 뒤에 %q", seen[i-1], seen[i])
		}
	}
	for i := 0; i < kept; i++ {
		want := name(inserted - kept + i)
		if seen[i] != want {
			t.Fatalf("%d번째 이름이 %q입니다 — %q여야 합니다", i, seen[i], want)
		}
		slot, _, ok := rowFind(tree, want)
		if !ok || slot != uint32(inserted-kept+i) {
			t.Fatalf("%s 조회가 (slot=%d ok=%v)입니다 — 병합이 슬롯을 흔들었습니다", want, slot, ok)
		}
	}
	// 지운 이름은 하나도 되살아나지 않습니다.
	for _, i := range []int{0, 100, 4095, inserted - kept - 1} {
		if _, _, ok := rowFind(tree, name(i)); ok {
			t.Fatalf("지운 %s가 다시 찾아집니다 — 병합이 엉뚱한 항목을 남겼습니다", name(i))
		}
	}
}

/* ── ② 신원 조회: "없다"와 "모른다"의 구분 ──────────────────────────────── */

// TestLookupIdentityDistinguishesFreshAbsenceFromStale — 신원 조회는 세 가지를
// 구분해서 답해야 합니다.
//
//	(uid, true, true)   찾았고 이 답이 최종입니다
//	("", true, false)   **정말 없습니다**(파티션이 신선합니다)
//	("", false, _)      모릅니다(파티션·GVR이 낡았습니다)
//
// 이 구분이 무너지면 목록 스냅숏이 2초 뒤처진 사이에 삭제된 객체가 되살아나거나,
// 회수 대기 중인 파티션의 옛 UID가 최종 답으로 나갑니다.
func TestLookupIdentityDistinguishesFreshAbsenceFromStale(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-old", nil))
	s := h.svc
	sindexOf := func() *searchIndex { return h.entry.load().sindex }

	// ① 부트스트랩된 신원.
	uid, auth, found := sindexOf().lookupIdentity("prod", "payments-api")
	if !found || !auth || uid != "uid-old" {
		t.Fatalf("초기 조회가 (uid=%q auth=%v found=%v)입니다", uid, auth, found)
	}

	// ② 같은 이름이 다른 UID로 교체되면 **새 UID**가 최종 답입니다.
	h.upsert(t, metaRow("prod", "payments-api", "uid-new", nil))
	if got := h.flush(t); got != 1 {
		t.Fatalf("flush=%d", got)
	}
	uid, auth, found = sindexOf().lookupIdentity("prod", "payments-api")
	if !found || !auth || uid != "uid-new" {
		t.Fatalf("교체 뒤 조회가 (uid=%q auth=%v found=%v)입니다 — 옛 UID가 남았습니다", uid, auth, found)
	}

	// ③ 삭제된 이름은 **권위 있는 부재**입니다(목록이 뒤처져도 되살리지 않습니다).
	h.remove(t, metaRow("prod", "payments-api", "uid-new", nil))
	if got := h.flush(t); got != 1 {
		t.Fatalf("삭제 flush=%d", got)
	}
	// 여기까지 드롭이 하나도 없었으므로 이 파티션은 실제로 신선합니다 —
	// 그래야 아래의 authoritative=true가 우연이 아니라 근거 있는 답입니다.
	if s.namespaceStale(h.gvr, "prod") {
		t.Fatal("정상 flush만 했는데 파티션이 회수 대기입니다 — 권위 판정의 전제가 무너집니다")
	}
	uid, auth, found = sindexOf().lookupIdentity("prod", "payments-api")
	if found || !auth || uid != "" {
		t.Fatalf("삭제 뒤 조회가 (uid=%q auth=%v found=%v)입니다 — 권위 있는 부재여야 합니다", uid, auth, found)
	}

	// ④ 파티션 자체가 없는 namespace도 신선한 GVR에서는 권위 있는 부재입니다.
	if uid, auth, found = sindexOf().lookupIdentity("never-seen", "x"); found || !auth || uid != "" {
		t.Fatalf("없는 파티션 조회가 (uid=%q auth=%v found=%v)입니다", uid, auth, found)
	}

	// ⑤ 파티션이 낡으면 같은 질문에 "모른다"고 답해야 합니다.
	base := sindexOf()
	stale := base.markStale("prod", indexBase)
	if _, auth, _ = stale.lookupIdentity("prod", "payments-api"); auth {
		t.Fatal("낡은 파티션인데 권위 있는 답이라고 말했습니다")
	}
	if _, auth, _ = stale.lookupIdentity("never-seen", "x"); !auth {
		t.Fatal("한 파티션이 낡았다고 다른 namespace의 답까지 흔들렸습니다")
	}

	// ⑥ GVR 전체가 낡으면 아무것도 단언하지 않습니다.
	whole := *base
	whole.gvrStale = true
	if uid, auth, found = whole.lookupIdentity("prod", "payments-api"); found || auth || uid != "" {
		t.Fatalf("GVR이 낡았는데 (uid=%q auth=%v found=%v)로 답했습니다", uid, auth, found)
	}
	var nilIdx *searchIndex
	if uid, auth, found = nilIdx.lookupIdentity("prod", "payments-api"); found || auth || uid != "" {
		t.Fatalf("인덱스가 없는데 (uid=%q auth=%v found=%v)로 답했습니다", uid, auth, found)
	}
}

// TestStaleNamespacesCountsOnlyMarkedPartitions — 낡음 표시는 파티션 단위로
// 쌓이고, 표시된 것만 세어야 합니다. 없던 namespace를 표시하면 그 파티션이
// 만들어지므로 파티션 수도 함께 늘어납니다.
func TestStaleNamespacesCountsOnlyMarkedPartitions(t *testing.T) {
	h := newDeltaHarness(t,
		metaRow("prod", "payments-api", "uid-1", nil),
		metaRow("billing", "ledger", "uid-2", nil),
	)
	base := h.entry.load().sindex

	if got := base.staleNamespaces(); got != 0 {
		t.Fatalf("부트스트랩 직후 낡은 파티션이 %d개입니다", got)
	}
	parts := base.partitionCount()
	if parts != 2 {
		t.Fatalf("파티션이 %d개입니다 — prod·billing 둘이어야 합니다", parts)
	}

	one := base.markStale("prod", indexBase)
	if got := one.staleNamespaces(); got != 1 {
		t.Fatalf("한 파티션을 표시했는데 %d개가 낡았습니다", got)
	}
	if got := base.staleNamespaces(); got != 0 {
		t.Fatalf("불변 인덱스인데 원본이 %d개로 바뀌었습니다 — 경로 복사가 아닙니다", got)
	}

	two := one.markStale("billing", indexBase)
	if got := two.staleNamespaces(); got != 2 {
		t.Fatalf("두 파티션을 표시했는데 %d개가 낡았습니다", got)
	}
	if two.version <= one.version || one.version <= base.version {
		t.Fatalf("버전이 단조 증가하지 않았습니다: %d → %d → %d",
			base.version, one.version, two.version)
	}

	// 없던 namespace를 표시하면 파티션이 생기고, 그것도 낡음으로 셉니다.
	fresh := two.markStale("never-seen", indexBase)
	if got := fresh.staleNamespaces(); got != 3 {
		t.Fatalf("새 파티션까지 %d개가 낡아야 합니다", got)
	}
	if got := fresh.partitionCount(); got != parts+1 {
		t.Fatalf("파티션이 %d개입니다 — %d개여야 합니다", got, parts+1)
	}
	var nilIdx *searchIndex
	if got := nilIdx.staleNamespaces(); got != 0 {
		t.Fatalf("인덱스가 없는데 %d개가 낡았다고 답했습니다", got)
	}
}

/* ── ③ label 멤버십이 뒤집히면 정규 집합 전체를 교체 ────────────────────── */

// TestReplaceRowOpsSwapsWholeCanonicalSetAndPrevLCP — label 토큰만 넣고 빼면
// 안 됩니다. prevLCP는 그 행의 **정규 토큰열 전체**에 대해 정의되므로, label이
// 끼거나 빠지면 이름 토큰의 prevLCP까지 달라집니다.
//
// 이름 "zz"에 label "za"가 붙으면 정규열은 [za, zz]가 되어 zz의 prevLCP가
// 0에서 1로 바뀝니다. 그 값을 갱신하지 않으면 질의 "z"에서 같은 행이 두 번 나갑니다.
func TestReplaceRowOpsSwapsWholeCanonicalSetAndPrevLCP(t *testing.T) {
	rec := rowInput{name: "zz", uid: "uid-1", labels: []string{"za"}}.record()
	if rec.weight != 1 {
		t.Fatalf("label 토큰 수가 %d입니다", rec.weight)
	}
	const slot = uint32(7)

	// label이 **붙는** 방향: 없는 집합을 지우고 있는 집합을 넣습니다.
	_, ops := replaceRowOps(rec, slot, "", "", false, false, true, nil, nil)
	if len(ops) != 3 {
		t.Fatalf("연산이 %d개입니다 — 옛 집합 1 + 새 집합 2여야 합니다: %+v", len(ops), ops)
	}
	if !ops[0].remove || ops[0].key.token != "zz" || ops[0].entry.prevLCP != 0 {
		t.Fatalf("첫 연산이 %+v입니다 — 옛 이름 토큰(prevLCP 0) 삭제여야 합니다", ops[0])
	}
	if ops[1].remove || ops[1].key.token != "za" || ops[1].entry.field != uint8(fieldLabel) {
		t.Fatalf("둘째 연산이 %+v입니다 — label 토큰 삽입이어야 합니다", ops[1])
	}
	if ops[2].remove || ops[2].key.token != "zz" || ops[2].entry.field != uint8(fieldName) {
		t.Fatalf("셋째 연산이 %+v입니다 — 이름 토큰 재삽입이어야 합니다", ops[2])
	}
	// **핵심**: 이름 토큰이 지워졌다가 **다른 prevLCP로** 다시 들어갑니다.
	if ops[2].entry.prevLCP != 1 {
		t.Fatalf("재삽입된 이름 토큰의 prevLCP가 %d입니다 — \"za\"와 1글자를 공유하므로 1이어야 합니다",
			ops[2].entry.prevLCP)
	}
	for _, op := range ops {
		if op.key.name != "zz" || op.key.uid != "uid-1" || op.entry.slot != slot {
			t.Fatalf("연산이 다른 행을 가리킵니다: %+v", op)
		}
	}

	// label이 **빠지는** 방향: 대칭이어야 합니다.
	_, back := replaceRowOps(rec, slot, "", "", false, true, false, nil, nil)
	if len(back) != 3 {
		t.Fatalf("역방향 연산이 %d개입니다: %+v", len(back), back)
	}
	if !back[0].remove || back[0].key.token != "za" {
		t.Fatalf("역방향 첫 연산이 %+v입니다 — label 토큰 삭제여야 합니다", back[0])
	}
	if !back[1].remove || back[1].key.token != "zz" || back[1].entry.prevLCP != 1 {
		t.Fatalf("역방향 둘째 연산이 %+v입니다 — 옛 prevLCP 1로 지워야 정확히 맞아떨어집니다", back[1])
	}
	if back[2].remove || back[2].key.token != "zz" || back[2].entry.prevLCP != 0 {
		t.Fatalf("역방향 셋째 연산이 %+v입니다 — prevLCP 0으로 되돌아가야 합니다", back[2])
	}

	// namespace·kind 토큰이 있으면 그것들까지 같은 규칙으로 함께 교체됩니다.
	_, wide := replaceRowOps(rec, slot, "zprod", "service", true, false, true, nil, nil)
	removes, inserts := 0, 0
	for _, op := range wide {
		if op.remove {
			removes++
			continue
		}
		inserts++
	}
	if removes != 3 || inserts != 4 {
		t.Fatalf("삭제 %d개·삽입 %d개입니다 — label 없는 3개를 지우고 label 포함 4개를 넣어야 합니다: %+v",
			removes, inserts, wide)
	}
}

/* ── ④ 예산 거절 뒤 쿨다운 ──────────────────────────────────────────────── */

// TestBudgetCooldownBlocksImmediateRetry — 부트스트랩이 예산으로 거절되면
// 곧바로 같은 일을 다시 시작하면 안 됩니다. 쿨다운 안에서는 회수를 잡지 않고,
// 쿨다운이 지나면 다시 잡을 수 있어야 합니다.
func TestBudgetCooldownBlocksImmediateRetry(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "seed", "uid-seed", nil))
	s := h.svc

	// 회수 후보를 하나 만들어 둡니다 — 쿨다운이 없으면 곧바로 잡힙니다.
	s.requestResync(h.gvr, "prod", 0)
	s.delta.mu.Lock()
	ready := s.pickRecoveryLocked(indexBase)
	s.delta.mu.Unlock()
	if ready == nil {
		t.Fatal("쿨다운이 없는데 회수 후보를 잡지 못했습니다 — 대조군이 성립하지 않습니다")
	}

	s.startBudgetCooldown()

	s.delta.mu.Lock()
	cooldown := s.delta.cooldownUntil
	during := s.pickRecoveryLocked(indexBase)
	after := s.pickRecoveryLocked(indexBase.Add(recoveryCooldown))
	s.delta.mu.Unlock()

	if want := indexBase.Add(recoveryCooldown); !cooldown.Equal(want) {
		t.Fatalf("쿨다운이 %v입니다 — %v여야 합니다", cooldown, want)
	}
	if during != nil {
		t.Fatal("쿨다운 안인데 회수를 다시 잡았습니다 — 예산 거절 직후 재시도를 막지 못합니다")
	}
	if after == nil {
		t.Fatal("쿨다운이 지났는데도 회수를 잡지 못했습니다 — 영구히 멈췄습니다")
	}

	// delta가 없는 서비스에서도 안전해야 합니다(구성 중·정지 상태).
	bare := &Service{cfg: Config{Now: func() time.Time { return indexBase }}}
	bare.startBudgetCooldown()
}

// TestSnapshotRetainedAccountingMatchesLease — 회계 보조가 말하는 보유 바이트는
// **실제로 승인받아 원장에 실린 값**과 같아야 합니다. 둘이 갈라지면 어느 쪽이
// 상한을 판정하는지 알 수 없게 됩니다.
func TestSnapshotRetainedAccountingMatchesLease(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))

	if got := sindexBytesOf(nil); got != 0 {
		t.Fatalf("스냅숏이 없는데 %d바이트라고 답했습니다", got)
	}
	if got := sindexBytesOf(&entrySnapshot{}); got != 0 {
		t.Fatalf("증분 인덱스가 없는데 %d바이트라고 답했습니다", got)
	}

	snap := h.entry.load()
	if snap == nil || snap.sindex == nil {
		t.Fatal("하네스가 증분 인덱스를 게시하지 않았습니다")
	}
	if got, want := sindexBytesOf(snap), snap.sindex.bytes; got != want || got <= 0 {
		t.Fatalf("sindexBytesOf=%d — 인덱스가 말하는 %d와 같아야 합니다", got, want)
	}

	lease := h.entry.leasePtr.Load()
	if lease == nil {
		t.Fatal("게시된 세대가 없습니다")
	}
	retained := entryRetained(snap)
	if retained <= 0 {
		t.Fatalf("보유 바이트가 %d입니다", retained)
	}
	if retained != lease.bytes {
		t.Fatalf("스냅숏 보유 %d와 원장에 실린 %d가 다릅니다 — 상한 판정 근거가 갈라집니다",
			retained, lease.bytes)
	}
	if got := h.svc.searchBytes.Load(); got != lease.bytes {
		t.Fatalf("계측 합계가 %d입니다 — 세대 하나뿐이면 %d여야 합니다", got, lease.bytes)
	}
}
