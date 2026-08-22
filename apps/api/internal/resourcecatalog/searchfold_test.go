package resourcecatalog

// namespace label 상한 fold 검증 (Round 6 v4.1 §1)
// --------------------------------------------------------------------------
// 증분 fold는 부트스트랩 fold와 **결과가 같아야** 합니다. 여기서 그것을
// ① 작은 상한에 대한 전수 성질 검사, ② v4.1 반례 고정 케이스,
// ③ 100k 포화 namespace의 정정된 상한 카운터로 못박습니다.

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

/* ── 참조 fold ──────────────────────────────────────────────────────────── */

// referenceFold는 부트스트랩이 하는 그대로입니다. 이름 순으로 접으며
// acc+w <= cap이면 포함하고 아니면 그 행의 label만 뺍니다.
func referenceFold(names []string, weights map[string]uint8, capTokens int64) map[string]bool {
	out := make(map[string]bool, len(names))
	var acc int64
	for _, n := range names {
		w := int64(weights[n])
		if w == 0 {
			out[n] = true // label이 없으므로 상한과 무관합니다.
			continue
		}
		if acc+w <= capTokens {
			out[n] = true
			acc += w
			continue
		}
		out[n] = false
	}
	return out
}

// foldDirOf는 무게 목록으로 rowDir를 세웁니다.
func foldDirOf(names []string, weights map[string]uint8) *rowNode {
	dir := newRowTree()
	ops := make([]rowOp, 0, len(names))
	for i, n := range names {
		ops = append(ops, rowOp{name: n, slot: uint32(i), weight: weights[n]})
	}
	var st rowStats
	dir = rowApply(dir, sortRowOps(ops), &st)
	return balanceRowRoot(dir, &st)
}

// foldMembership은 recomputeFold 결과를 이름 → 포함 여부로 폅니다.
func foldMembership(f foldState, names []string, weights map[string]uint8) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		if weights[n] == 0 {
			out[n] = true
			continue
		}
		out[n] = f.includes(n)
	}
	return out
}

/* ── ① 작은 상한 전수 성질 ──────────────────────────────────────────────── */

// TestFoldPropertyExhaustiveSmallCap — 작은 상한·작은 무게 조합을 **전부** 돌면서
// 증분 fold가 부트스트랩 fold와 같은지 봅니다. 조합 폭발을 막으려고 행 수와
// 무게 범위를 작게 두되, 모든 단일 변형(삽입·삭제·무게 변경)을 함께 적용합니다.
func TestFoldPropertyExhaustiveSmallCap(t *testing.T) {
	const rows = 5
	const maxW = 5
	names := make([]string, rows)
	for i := range names {
		names[i] = fmt.Sprintf("r%d", i)
	}
	combos := 1
	for i := 0; i < rows; i++ {
		combos *= maxW + 1
	}
	checked := 0
	for capTokens := int64(1); capTokens <= 12; capTokens++ {
		for code := 0; code < combos; code++ {
			weights := map[string]uint8{}
			c := code
			for i := 0; i < rows; i++ {
				weights[names[i]] = uint8(c % (maxW + 1))
				c /= maxW + 1
			}
			dir := foldDirOf(names, weights)
			got := foldMembership(recomputeFold2(dir, capTokens), names, weights)
			want := referenceFold(names, weights, capTokens)
			for _, n := range names {
				if got[n] != want[n] {
					t.Fatalf("cap=%d weights=%v: %q 포함이 %v입니다 — %v여야 합니다",
						capTokens, weights, n, got[n], want[n])
				}
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("아무 조합도 검사하지 않았습니다")
	}
}

// recomputeFold2는 상한을 주입할 수 있는 recomputeFold입니다.
// 프로덕션 상한(2^18)으로는 작은 조합을 만들 수 없어 테스트에서만 씁니다.
func recomputeFold2(dir *rowNode, capTokens int64) foldState {
	var out foldState
	b := rowPrefixBoundary(dir, capTokens)
	out.acc, out.visited, out.compares = b.acc, b.visited, b.compares
	if !b.overflow {
		return out
	}
	out.boundName, out.boundValid = b.name, true
	budget := capTokens - out.acc
	after := b.name
	for len(out.suffix) < maxSuffixRows && budget > 0 {
		cap8 := budget
		if cap8 > 255 {
			cap8 = 255
		}
		hit := rowNextFitting(dir, after, uint8(cap8), true)
		out.visited += hit.visited
		out.compares += hit.compares
		if !hit.found {
			break
		}
		out.suffix = append(out.suffix, rowRef{name: hit.name, slot: hit.slot})
		out.acc += int64(hit.weight)
		budget -= int64(hit.weight)
		after = hit.name
	}
	return out
}

/* ── ② v4.1 반례 고정 케이스 ────────────────────────────────────────────── */

// TestFoldCounterexampleCap100 — v4의 "신규 포함 무게 합 <= w" 정리가 거짓임을
// 보인 그 입력입니다. cap=100, 무게 [23,23,23,31,1]에 맨 앞 32를 넣으면
// **기존 54 토큰이 접두사 경계를 넘어 빠집니다.**
func TestFoldCounterexampleCap100(t *testing.T) {
	const capTokens = int64(100)
	base := []string{"r1", "r2", "r3", "r4", "r5"}
	weights := map[string]uint8{"r1": 23, "r2": 23, "r3": 23, "r4": 31, "r5": 1}

	before := recomputeFold2(foldDirOf(base, weights), capTokens)
	beforeSet := foldMembership(before, base, weights)
	for _, n := range []string{"r1", "r2", "r3", "r4"} {
		if !beforeSet[n] {
			t.Fatalf("삽입 전 %q가 빠져 있습니다", n)
		}
	}
	if beforeSet["r5"] {
		t.Fatal("삽입 전 r5가 포함돼 있습니다 — acc가 정확히 100이어야 합니다")
	}
	if before.acc != 100 {
		t.Fatalf("삽입 전 acc=%d — 100이어야 합니다", before.acc)
	}

	// 맨 앞에 무게 32를 넣습니다("r0"이 사전순으로 앞섭니다).
	names := append([]string{"r0"}, base...)
	weights["r0"] = 32
	after := recomputeFold2(foldDirOf(names, weights), capTokens)
	afterSet := foldMembership(after, names, weights)

	wantIncluded := map[string]bool{"r0": true, "r1": true, "r2": true, "r3": false, "r4": false, "r5": true}
	for n, want := range wantIncluded {
		if afterSet[n] != want {
			t.Fatalf("%q 포함이 %v입니다 — %v여야 합니다 (fold=%+v)", n, afterSet[n], want, after)
		}
	}
	// 참조와도 같아야 합니다.
	ref := referenceFold(names, weights, capTokens)
	for n := range wantIncluded {
		if afterSet[n] != ref[n] {
			t.Fatalf("%q가 참조 fold와 다릅니다: %v vs %v", n, afterSet[n], ref[n])
		}
	}
	// 경계를 넘어 빠진 기존 토큰 합이 54여야 합니다(23 + 31).
	crossed := int64(0)
	for _, n := range base {
		if beforeSet[n] && !afterSet[n] {
			crossed += int64(weights[n])
		}
	}
	if crossed != 54 {
		t.Fatalf("경계를 넘어 빠진 토큰이 %d입니다 — 54여야 합니다", crossed)
	}
	// 정정된 상한(63)은 지켜야 합니다.
	if crossed > 63 {
		t.Fatalf("접두사 대칭차 %d가 정정된 상한 63을 넘었습니다", crossed)
	}
}

// TestFoldPrefixSymmetricDifferenceStaysBounded — 무작위 포화 상태에서
// 단일 변형의 접두사 대칭차가 **63 토큰**을 넘지 않아야 합니다(정리 1).
func TestFoldPrefixSymmetricDifferenceStaysBounded(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	const capTokens = int64(1000)
	const rows = 400
	names := make([]string, rows)
	weights := map[string]uint8{}
	for i := range names {
		names[i] = fmt.Sprintf("r%04d", i)
		weights[names[i]] = uint8(1 + rng.Intn(32))
	}
	before := recomputeFold2(foldDirOf(names, weights), capTokens)
	beforeSet := foldMembership(before, names, weights)
	if !before.boundValid {
		t.Fatal("픽스처가 포화되지 않았습니다")
	}

	for trial := 0; trial < 60; trial++ {
		next := map[string]uint8{}
		for k, v := range weights {
			next[k] = v
		}
		target := names[rng.Intn(rows)]
		switch rng.Intn(3) {
		case 0:
			next[target] = 0 // 삭제와 같은 효과(무게 0)
		case 1:
			next[target] = uint8(1 + rng.Intn(32))
		default:
			next[target] = 32
		}
		afterSet := foldMembership(recomputeFold2(foldDirOf(names, next), capTokens), names, next)
		var crossed int64
		for _, n := range names {
			if n == target {
				continue
			}
			if beforeSet[n] != afterSet[n] {
				crossed += int64(weights[n])
			}
		}
		if crossed > 63+31+31 {
			t.Fatalf("trial %d: 뒤집힌 토큰이 %d입니다 — 63+31+31을 넘었습니다", trial, crossed)
		}
	}
}

/* ── ③ 100k 포화 namespace의 정정된 상한 ────────────────────────────────── */

// saturatedPart는 상한을 넘긴 큰 파티션 하나를 만듭니다.
func saturatedPart(t *testing.T, rows int, labelsPerRow int) *nsPart {
	t.Helper()
	part := newNsPart("prod", "service", indexBase)
	ops := make([]partOp, 0, rows)
	for i := 0; i < rows; i++ {
		labels := make([]string, 0, labelsPerRow)
		for k := 0; k < labelsPerRow; k++ {
			labels = append(labels, fmt.Sprintf("lk%02d", k), fmt.Sprintf("lv%02d-%06d", k, i))
		}
		name := fmt.Sprintf("row-%06d", i)
		ops = append(ops, partOp{
			name:  name,
			input: &rowInput{name: name, uid: fmt.Sprintf("uid-%06d", i), labels: labels},
		})
	}
	var st applyStats
	out := applyPartOps(part, true, indexBase, ops, &st)
	if st.slotExhausted {
		t.Fatal("픽스처가 슬롯을 고갈시켰습니다")
	}
	return out
}

// TestSaturatedHotNamespaceSingleEventBounds — v4.1 §1의 정정된 카운터입니다.
//
//	visited_rows            <= 195 + 5*depth
//	postings_changed        <= 195
//	directory_copies        <= 195 * postLeafMax
//	findnext/prefix 비교     <= 2,048 + 32
func TestSaturatedHotNamespaceSingleEventBounds(t *testing.T) {
	// 8192행 × 32토큰 = 2^18 = 상한을 정확히 채우고, 그 뒤 행이 넘깁니다.
	const rows = 8300
	part := saturatedPart(t, rows, MaxLabelKeysPerObject)
	if !part.boundValid {
		t.Fatalf("픽스처가 포화되지 않았습니다: nsLabelTok=%d", part.nsLabelTok)
	}

	var st applyStats
	next := applyPartOps(part, true, indexBase, []partOp{{
		name: "row-000000",
		input: &rowInput{
			name: "row-000000", uid: "uid-000000",
			labels: []string{"changed", "value"},
		},
	}}, &st)
	if next == part {
		t.Fatal("적용이 되지 않았습니다")
	}

	// 멤버십이 뒤집힌 행은 **정규 집합 전체**를 교체합니다(base 토큰의 prevLCP가
	// 달라지기 때문입니다). 그래서 posting 연산 상한은 v4.1의 195가 아니라
	//
	//	(접두사 대칭차 63 + S_old 31 + S_new 31 + 변경 행 1) × (제거 35 + 삽입 35)
	//
	// 입니다. 행 수 상한(63/31/31)은 그대로이고, 행마다 드는 비용만 커집니다.
	const maxFlippedRows = 63 + maxSuffixRows + maxSuffixRows + 1
	const maxPostings = maxFlippedRows * 2 * maxRowTokens
	const maxVisited = maxFlippedRows + 5*8
	if st.postingsChanged > maxPostings {
		t.Errorf("postings_changed=%d — 상한 %d", st.postingsChanged, maxPostings)
	}
	if st.visitedRows > maxVisited {
		t.Errorf("visited_rows=%d — 상한 %d", st.visitedRows, maxVisited)
	}
	if st.directoryCopies > maxPostings*postLeafMax {
		t.Errorf("directory_copies=%d — 상한 %d", st.directoryCopies, maxPostings*postLeafMax)
	}
	if st.foldCompares > 2048+32 {
		t.Errorf("fold 비교=%d — 상한 %d", st.foldCompares, 2048+32)
	}
	// 무엇보다: 전체 행을 훑지 않아야 합니다.
	if st.visitedRows >= int64(rows) {
		t.Fatalf("visited_rows=%d — 전체 행 %d을 훑었습니다", st.visitedRows, rows)
	}
}

// TestClusterScopedPartitionBehavesTheSame — namespace=""도 같은 경로여야 합니다.
func TestClusterScopedPartitionBehavesTheSame(t *testing.T) {
	part := newNsPart("", "node", indexBase)
	if part.nsTok != "" {
		t.Fatalf("cluster-scoped 토큰이 %q입니다", part.nsTok)
	}
	ops := []partOp{
		{name: "node-a", input: &rowInput{name: "node-a", uid: "uid-a", labels: []string{"role", "worker"}}},
		{name: "node-b", input: &rowInput{name: "node-b", uid: "uid-b"}},
	}
	var st applyStats
	// namespaced=false이므로 namespace 토큰이 색인되지 않아야 합니다.
	out := applyPartOps(part, false, indexBase, ops, &st)
	if out.liveRows != 2 {
		t.Fatalf("행이 %d개입니다", out.liveRows)
	}
	keyer := out.keyer()
	var tokens []string
	c := seekPost(out.postRoot, keyer, postKey{}, false)
	for c.valid() {
		tokens = append(tokens, keyer.keyOf(c.entry()).token)
		c.next()
	}
	joined := strings.Join(tokens, ",")
	if strings.Contains(joined, ",,") || joined == "" {
		t.Fatalf("토큰 목록이 이상합니다: %q", joined)
	}
	// 삭제도 같은 경로입니다.
	var st2 applyStats
	gone := applyPartOps(out, false, indexBase, []partOp{{name: "node-a"}}, &st2)
	if gone.liveRows != 1 {
		t.Fatalf("삭제 후 행이 %d개입니다", gone.liveRows)
	}
	if _, _, ok := rowFind(gone.rowDir, "node-a"); ok {
		t.Fatal("삭제한 행이 남아 있습니다")
	}
	// 옛 파티션은 불변이어야 합니다.
	if _, _, ok := rowFind(out.rowDir, "node-a"); !ok {
		t.Fatal("옛 파티션이 변형됐습니다")
	}
}

/* ── 증분 == 부트스트랩 ──────────────────────────────────────────────────── */

// partSignature는 파티션의 관측 가능한 전부를 문자열 하나로 만듭니다.
func partSignature(p *nsPart) string {
	var b strings.Builder
	keyer := p.keyer()
	c := seekPost(p.postRoot, keyer, postKey{}, false)
	for c.valid() {
		k := keyer.keyOf(c.entry())
		e := c.entry()
		fmt.Fprintf(&b, "%s|%s|%s|f%d|l%d\n", k.token, k.name, k.uid, e.field, e.prevLCP)
		c.next()
	}
	fmt.Fprintf(&b, "live=%d trunc=%d acc=%d bound=%q valid=%v suffix=%d\n",
		p.liveRows, p.truncRows, p.nsLabelTok, p.boundName, p.boundValid, len(p.suffix))
	return b.String()
}

// TestIncrementalEqualsBootstrapRandomOps — 무작위 연산을 한 번에 적용한 것과
// 하나씩 적용한 것이 **완전히 같은 파티션**이어야 합니다.
func TestIncrementalEqualsBootstrapRandomOps(t *testing.T) {
	rng := rand.New(rand.NewSource(777))
	const universe = 200

	final := map[string]*rowInput{}
	var history []partOp
	for round := 0; round < 500; round++ {
		name := fmt.Sprintf("row-%03d", rng.Intn(universe))
		if rng.Intn(4) == 0 {
			history = append(history, partOp{name: name})
			delete(final, name)
			continue
		}
		labels := make([]string, 0, 6)
		for k := 0; k < rng.Intn(4); k++ {
			labels = append(labels, fmt.Sprintf("k%d", k), fmt.Sprintf("v%d-%d", k, rng.Intn(5)))
		}
		in := &rowInput{name: name, uid: fmt.Sprintf("uid-%s-%d", name, rng.Intn(3)), labels: labels}
		history = append(history, partOp{name: name, input: in})
		final[name] = in
	}

	// ① 하나씩 적용(증분).
	step := newNsPart("prod", "service", indexBase)
	for _, op := range history {
		var st applyStats
		step = applyPartOps(step, true, indexBase, []partOp{op}, &st)
	}
	// ② 최종 상태를 한 번에 적용(부트스트랩과 같은 모양).
	bulkOps := make([]partOp, 0, len(final))
	for name, in := range final {
		bulkOps = append(bulkOps, partOp{name: name, input: in})
	}
	sortPartOpsByName(bulkOps)
	bulk := newNsPart("prod", "service", indexBase)
	var bst applyStats
	bulk = applyPartOps(bulk, true, indexBase, bulkOps, &bst)

	if got, want := partSignature(step), partSignature(bulk); got != want {
		t.Fatalf("증분 결과가 부트스트랩과 다릅니다\n--- 증분 ---\n%s\n--- 부트스트랩 ---\n%s", got, want)
	}
}

func sortPartOpsByName(ops []partOp) {
	for i := 1; i < len(ops); i++ {
		for j := i; j > 0 && ops[j].name < ops[j-1].name; j-- {
			ops[j], ops[j-1] = ops[j-1], ops[j]
		}
	}
}

// TestSameNameNewUIDReplacesIdentity — 같은 이름 새 UID는 옛 행을 완전히 대체해야 합니다.
func TestSameNameNewUIDReplacesIdentity(t *testing.T) {
	part := newNsPart("prod", "service", indexBase)
	var st applyStats
	part = applyPartOps(part, true, indexBase, []partOp{
		{name: "payments-api", input: &rowInput{name: "payments-api", uid: "uid-old", labels: []string{"app", "old"}}},
	}, &st)
	var st2 applyStats
	part = applyPartOps(part, true, indexBase, []partOp{
		{name: "payments-api", input: &rowInput{name: "payments-api", uid: "uid-new", labels: []string{"app", "new"}}},
	}, &st2)

	keyer := part.keyer()
	c := seekPost(part.postRoot, keyer, postKey{}, false)
	seen := map[string]bool{}
	for c.valid() {
		k := keyer.keyOf(c.entry())
		seen[k.uid+"/"+k.token] = true
		c.next()
	}
	for id := range seen {
		if strings.HasPrefix(id, "uid-old/") {
			t.Fatalf("옛 UID의 posting이 남아 있습니다: %s", id)
		}
	}
	if !seen["uid-new/new"] {
		t.Fatalf("새 label 토큰이 색인되지 않았습니다: %v", seen)
	}
	if part.liveRows != 1 {
		t.Fatalf("행이 %d개입니다 — 1개여야 합니다", part.liveRows)
	}
}

// TestKeysTruncatedBitIsCarriedAndCounted — 행 단위 절단 비트는 델타가 명시적으로
// 옮기고, 마지막 절단 행이 사라지면 진단도 사라져야 합니다.
func TestKeysTruncatedBitIsCarriedAndCounted(t *testing.T) {
	part := newNsPart("prod", "service", indexBase)
	var st applyStats
	part = applyPartOps(part, true, indexBase, []partOp{
		{name: "fat", input: &rowInput{name: "fat", uid: "uid-fat", labels: []string{"a", "b"}, keysTruncated: true}},
		{name: "thin", input: &rowInput{name: "thin", uid: "uid-thin", labels: []string{"c", "d"}}},
	}, &st)
	if part.truncRows != 1 || part.reasonMask&nsReasonLabelNs == 0 {
		t.Fatalf("절단 진단이 서지 않았습니다: trunc=%d mask=%d", part.truncRows, part.reasonMask)
	}
	var st2 applyStats
	part = applyPartOps(part, true, indexBase, []partOp{{name: "fat"}}, &st2)
	if part.truncRows != 0 || part.reasonMask&nsReasonLabelNs != 0 {
		t.Fatalf("마지막 절단 행을 지웠는데 진단이 남았습니다: trunc=%d mask=%d", part.truncRows, part.reasonMask)
	}
}

// TestDeleteEarlyCapRowPromotesLateLabel — 초기 상한 소모 행을 지우면
// 뒤쪽 행의 label이 **다시 검색 가능**해져야 합니다(승격).
func TestDeleteEarlyCapRowPromotesLateLabel(t *testing.T) {
	const capTokens = int64(10)
	names := []string{"a", "b", "c"}
	weights := map[string]uint8{"a": 8, "b": 4, "c": 2}

	before := foldMembership(recomputeFold2(foldDirOf(names, weights), capTokens), names, weights)
	if !before["a"] || before["b"] {
		t.Fatalf("픽스처가 기대와 다릅니다: %v", before)
	}
	// c(무게 2)는 남은 예산 2에 들어가므로 S에 포함됩니다.
	if !before["c"] {
		t.Fatalf("c가 S에 들어가야 합니다: %v", before)
	}

	// 초기 소모 행 a를 지웁니다.
	delete(weights, "a")
	rest := []string{"b", "c"}
	after := foldMembership(recomputeFold2(foldDirOf(rest, weights), capTokens), rest, weights)
	if !after["b"] || !after["c"] {
		t.Fatalf("승격되지 않았습니다: %v", after)
	}
	// 참조 fold와도 같아야 합니다.
	ref := referenceFold(rest, weights, capTokens)
	for _, n := range rest {
		if after[n] != ref[n] {
			t.Fatalf("%q가 참조와 다릅니다: %v vs %v", n, after[n], ref[n])
		}
	}
}
