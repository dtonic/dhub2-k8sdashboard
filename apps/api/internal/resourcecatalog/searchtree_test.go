package resourcecatalog

// 지속 자료구조 검증 (Round 6 v4 / v4.1)
// --------------------------------------------------------------------------
// 트리는 "그럴듯하게 동작"으로는 부족합니다. 참조 구현(정렬 슬라이스)과 **완전히
// 같은 답**을 내야 하고, 분할·병합·fence 교체가 어떤 순서로 일어나도 그래야 합니다.

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"unsafe"
)

/* ── 레이아웃 고정 ───────────────────────────────────────────────────────── */

// TestStructLayoutIsStable — 회계 식이 참조하는 크기가 실제 크기와 같아야 합니다.
// 여기가 어긋나면 예산이 조용히 틀립니다.
func TestStructLayoutIsStable(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"postEntry", unsafe.Sizeof(postEntry{}), postEntryBytes},
		{"postKey", unsafe.Sizeof(postKey{}), postKeyBytes},
		{"rowAgg", unsafe.Sizeof(rowAgg{}), rowAggBytes},
		{"nsAgg", unsafe.Sizeof(nsAgg{}), nsAggBytes},
		{"trieLeaf", unsafe.Sizeof(trieLeaf{}), trieLeafBytes},
		{"trieBranch", unsafe.Sizeof(trieBranch{}), trieBranchBytes},
		{"rowRecord", unsafe.Sizeof(rowRecord{}), rowRecordFixedBytes},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s 크기가 %d입니다 — 회계는 %d로 계산합니다", c.name, c.got, c.want)
		}
	}
	// nsAgg는 staleCount를 뺀 16바이트여야 합니다. 되돌리면 24로 정렬돼 낭비입니다.
	if unsafe.Sizeof(nsAgg{}) != 16 {
		t.Fatalf("nsAgg가 16바이트가 아닙니다: %d", unsafe.Sizeof(nsAgg{}))
	}
}

/* ── label blob ─────────────────────────────────────────────────────────── */

func TestLabelBlobRoundTrips(t *testing.T) {
	tokens := []string{"app", "payments", "tier", "backend", strings.Repeat("z", tokenPrefixBytes)}
	rec := &rowRecord{labelBlob: encodeLabelBlob(tokens), weight: uint16(len(tokens))}
	for i, want := range tokens {
		if got := rec.labelTokenAt(i); got != want {
			t.Fatalf("%d번째 토큰이 %q입니다 — %q여야 합니다", i, got, want)
		}
	}
	if rec.labelTokenAt(len(tokens)) != "" {
		t.Fatal("범위 밖 토큰이 값을 돌려줬습니다")
	}
	var seen []string
	rec.eachLabelToken(func(_ int, tok string) bool {
		seen = append(seen, tok)
		return true
	})
	if strings.Join(seen, ",") != strings.Join(tokens, ",") {
		t.Fatalf("순회 결과가 다릅니다: %v", seen)
	}
}

/* ── 자유 목록 게시 ──────────────────────────────────────────────────────── */

// TestFreeListPopPublishBase56Alloc5Free7 — v4.1이 요구한 정확한 회귀입니다.
//
// base [5,6]에서 5를 할당하고 7을 해제하면 게시본은 [7,6]이어야 하고,
// **어떤 후속 스냅숏도 5를 다시 내주면 안 됩니다**(이 배치가 쓰고 있습니다).
func TestFreeListPopPublishBase56Alloc5Free7(t *testing.T) {
	base := &freeNode{}
	base.slots[0], base.slots[1], base.n = 6, 5, 2 // pop은 뒤에서 꺼내므로 5가 먼저 나옵니다.

	f := newFreeStack(base)
	got, ok := f.pop()
	if !ok || got != 5 {
		t.Fatalf("첫 할당이 %d(ok=%v)입니다 — 5여야 합니다", got, ok)
	}
	published := f.publish([]uint32{7})

	var order []uint32
	for n := published; n != nil; n = n.next {
		for i := int(n.n) - 1; i >= 0; i-- {
			order = append(order, n.slots[i])
		}
	}
	if len(order) != 2 || order[0] != 7 || order[1] != 6 {
		t.Fatalf("게시된 자유 목록이 %v입니다 — [7 6]이어야 합니다", order)
	}
	// 원본은 불변이어야 합니다.
	if base.n != 2 || base.slots[1] != 5 {
		t.Fatalf("캡처한 base가 변형됐습니다: n=%d slots=%v", base.n, base.slots[:2])
	}
	// 다음 배치에서 5가 다시 나오면 안 됩니다.
	next := newFreeStack(published)
	for {
		slot, ok := next.pop()
		if !ok {
			break
		}
		if slot == 5 {
			t.Fatal("이미 쓰고 있는 슬롯 5가 다시 할당됐습니다")
		}
	}
}

/* ── postTree 참조 대조 ──────────────────────────────────────────────────── */

// treeModel은 참조 구현입니다. 정렬 슬라이스 하나뿐이라 틀릴 여지가 없습니다.
type treeModel struct {
	keys []postKey
	ent  map[string]postEntry
}

func newTreeModel() *treeModel {
	return &treeModel{ent: map[string]postEntry{}}
}

func modelKeyOf(k postKey) string { return k.token + "\x00" + k.name + "\x00" + k.uid }

func (m *treeModel) apply(ops []postOp) {
	for _, op := range ops {
		id := modelKeyOf(op.key)
		_, had := m.ent[id]
		if op.remove {
			if had {
				delete(m.ent, id)
				for i, k := range m.keys {
					if modelKeyOf(k) == id {
						m.keys = append(m.keys[:i], m.keys[i+1:]...)
						break
					}
				}
			}
			continue
		}
		m.ent[id] = op.entry
		if !had {
			m.keys = append(m.keys, op.key)
			sort.Slice(m.keys, func(i, j int) bool { return comparePostKey(m.keys[i], m.keys[j]) < 0 })
		}
	}
}

// treeHarness는 슬롯 → 행 트라이와 postTree를 함께 든 테스트용 파티션입니다.
type treeHarness struct {
	root   *postNode
	trie   *trieBranch
	keyer  postKeyer
	model  *treeModel
	nextID uint32
}

func newTreeHarness() *treeHarness {
	h := &treeHarness{root: newPostTree(), trie: newTrieRoot(), model: newTreeModel()}
	h.keyer = postKeyer{root: h.trie, namespace: "prod", kindTok: "service"}
	return h
}

// addRow는 행 하나를 트라이에 넣고 그 슬롯을 돌려줍니다.
//
// labels는 그 행이 실제로 가진 label 토큰입니다. **postEntry의 tokIdx는 이 목록으로
// 해석되므로**, 테스트가 쓰는 토큰이 여기 없으면 유도된 키가 빈 문자열이 됩니다.
// (tokIdx 0=name, 1=namespace, 2=kind, 3+=labels[i-3])
func (h *treeHarness) addRow(name, uid string, labels []string) uint32 {
	rec := &rowRecord{
		name: name, uid: uid, nameTok: normalizeToken(name),
		labelBlob: encodeLabelBlob(labels), weight: uint16(len(labels)),
	}
	slot := h.nextID
	h.nextID++
	var copied int64
	h.trie = trieSet(h.trie, slot, rec, &copied)
	h.keyer.root = h.trie
	return slot
}

// assertKeysDerivable는 op의 키가 entry에서 **그대로 유도되는지** 확인합니다.
// 이 불변식이 깨지면 트리는 정렬도 조회도 할 수 없습니다.
func (h *treeHarness) assertKeysDerivable(t *testing.T, ops []postOp) {
	t.Helper()
	for _, op := range ops {
		if op.remove {
			continue
		}
		got := h.keyer.keyOf(op.entry)
		if comparePostKey(got, op.key) != 0 {
			t.Fatalf("entry에서 유도한 키 %+v가 op 키 %+v와 다릅니다", got, op.key)
		}
	}
}

func (h *treeHarness) apply(ops []postOp) *postStats {
	sorted := sortPostOps(append([]postOp(nil), ops...))
	var st postStats
	h.root = postApply(h.root, h.keyer, sorted, &st)
	h.root = balancePostRoot(h.root, &st)
	h.model.apply(sorted)
	return &st
}

func (h *treeHarness) walk() []postKey {
	out := make([]postKey, 0, 64)
	c := seekPost(h.root, h.keyer, postKey{}, false)
	for c.valid() {
		out = append(out, h.keyer.keyOf(c.entry()))
		c.next()
	}
	return out
}

func (h *treeHarness) assertMatchesModel(t *testing.T, when string) {
	t.Helper()
	got := h.walk()
	if len(got) != len(h.model.keys) {
		t.Fatalf("%s: 항목 수가 %d입니다 — 참조는 %d입니다", when, len(got), len(h.model.keys))
	}
	for i := range got {
		if comparePostKey(got[i], h.model.keys[i]) != 0 {
			t.Fatalf("%s: %d번째가 %+v입니다 — 참조는 %+v입니다", when, i, got[i], h.model.keys[i])
		}
	}
}

// TestPostTreeMatchesCanonicalModel — 무작위 삽입·삭제 뒤에도 순서와 내용이
// 참조 구현과 완전히 같아야 합니다. 분할·병합·fence 교체가 전부 여기서 밟힙니다.
func TestPostTreeMatchesCanonicalModel(t *testing.T) {
	rng := rand.New(rand.NewSource(20260821))
	h := newTreeHarness()

	const rows = 900
	const tokensPerRow = 24
	// 각 행이 t00..t23을 label 토큰으로 가집니다. tokIdx = 3+k가 곧 "t%02d"입니다.
	rowLabels := make([]string, tokensPerRow)
	for k := range rowLabels {
		rowLabels[k] = fmt.Sprintf("t%02d", k)
	}
	slots := make([]uint32, rows)
	names := make([]string, rows)
	for i := 0; i < rows; i++ {
		names[i] = fmt.Sprintf("obj-%04d", i)
		slots[i] = h.addRow(names[i], fmt.Sprintf("uid-%04d", i), rowLabels)
	}
	live := map[string]bool{}

	for round := 0; round < 120; round++ {
		ops := make([]postOp, 0, 32)
		for n := 0; n < 24; n++ {
			i := rng.Intn(rows)
			k := rng.Intn(tokensPerRow)
			key := postKey{token: rowLabels[k], name: names[i], uid: fmt.Sprintf("uid-%04d", i)}
			id := modelKeyOf(key)
			remove := live[id] && rng.Intn(3) == 0
			ops = append(ops, postOp{
				key:    key,
				entry:  postEntry{slot: slots[i], tokIdx: uint16(3 + k), field: uint8(fieldLabel)},
				remove: remove,
			})
			live[id] = !remove
		}
		h.assertKeysDerivable(t, ops)
		h.apply(ops)
		h.assertMatchesModel(t, fmt.Sprintf("round %d", round))
	}

	// 전부 지우고도 순서가 유지되어야 합니다(병합·루트 붕괴 경로).
	all := make([]postOp, 0, len(h.model.keys))
	for _, k := range h.model.keys {
		all = append(all, postOp{key: k, remove: true})
	}
	h.apply(all)
	h.assertMatchesModel(t, "전부 삭제")
	if postTreeCount(h.root) != 0 {
		t.Fatalf("삭제 후에도 %d개가 남았습니다", postTreeCount(h.root))
	}
}

// TestPostTreeSeparatorBoundaryDeleteReinsert — fence가 가리키던 **경계 항목**을
// 지웠다가 다시 넣어도 하강이 정확해야 합니다. fence는 하한이라 교체하지 않습니다.
func TestPostTreeSeparatorBoundaryDeleteReinsert(t *testing.T) {
	h := newTreeHarness()
	const n = 1200
	keys := make([]postKey, n)
	ops := make([]postOp, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("row-%05d", i)
		tok := fmt.Sprintf("tok-%05d", i)
		// 행이 그 토큰을 실제로 가져야 entry에서 같은 키가 유도됩니다.
		slot := h.addRow(name, fmt.Sprintf("uid-%05d", i), []string{tok})
		keys[i] = postKey{token: tok, name: name, uid: fmt.Sprintf("uid-%05d", i)}
		ops = append(ops, postOp{key: keys[i], entry: postEntry{slot: slot, tokIdx: 3}})
	}
	h.assertKeysDerivable(t, ops)
	h.apply(ops)
	h.assertMatchesModel(t, "적재")

	// 모든 내부 fence를 모아, 그 키들을 지웠다가 되살립니다.
	fences := collectFences(h.root)
	if len(fences) == 0 {
		t.Fatal("fence가 하나도 없습니다 — 분할이 일어나지 않았습니다")
	}
	del := make([]postOp, 0, len(fences))
	for _, f := range fences {
		del = append(del, postOp{key: f, remove: true})
	}
	h.apply(del)
	h.assertMatchesModel(t, "fence 삭제")

	back := make([]postOp, 0, len(fences))
	for _, f := range fences {
		slot, _, ok := findSlotByName(h, f.name)
		if !ok {
			t.Fatalf("행 %q를 찾지 못했습니다", f.name)
		}
		back = append(back, postOp{key: f, entry: postEntry{slot: slot, tokIdx: 3}})
	}
	h.assertKeysDerivable(t, back)
	h.apply(back)
	h.assertMatchesModel(t, "fence 재삽입")

	// 점 조회도 정확해야 합니다.
	for _, f := range fences {
		c := seekPost(h.root, h.keyer, f, true)
		if !c.valid() || comparePostKey(h.keyer.keyOf(c.entry()), f) != 0 {
			t.Fatalf("fence %+v를 다시 찾지 못했습니다", f)
		}
	}
}

func collectFences(n *postNode) []postKey {
	if n == nil {
		return nil
	}
	out := append([]postKey(nil), n.seps...)
	if !n.leafLevel {
		for _, c := range n.nodeKids {
			out = append(out, collectFences(c)...)
		}
	}
	return out
}

func findSlotByName(h *treeHarness, name string) (uint32, *rowRecord, bool) {
	for slot := uint32(0); slot < h.nextID; slot++ {
		if rec := trieGet(h.trie, slot); rec != nil && rec.name == name {
			return slot, rec, true
		}
	}
	return 0, nil, false
}

// TestSamePrefix64ByteTokenNames — 앞 **64바이트가 정확히 같은** 253바이트 이름들은
// 토큰이 동일합니다. 키의 2순위 성분이 원문 이름이라 신원이 정확히 갈려야 합니다.
func TestSamePrefix64ByteTokenNames(t *testing.T) {
	h := newTreeHarness()
	prefix := strings.Repeat("a", tokenPrefixBytes) // 정확히 64바이트
	const n = 512
	names := make([]string, n)
	ops := make([]postOp, 0, n)
	for i := 0; i < n; i++ {
		tail := fmt.Sprintf("-%d", i)
		names[i] = prefix + strings.Repeat("b", 253-len(prefix)-len(tail)) + tail
		if len(names[i]) != 253 {
			t.Fatalf("이름 길이가 %d입니다", len(names[i]))
		}
		slot := h.addRow(names[i], fmt.Sprintf("uid-%04d", i), nil)
		rec := trieGet(h.trie, slot)
		if rec.nameTok != prefix {
			t.Fatalf("토큰이 %q입니다 — 앞 64바이트 %q여야 합니다", rec.nameTok, prefix)
		}
		ops = append(ops, postOp{
			key:   postKey{token: prefix, name: names[i], uid: fmt.Sprintf("uid-%04d", i)},
			entry: postEntry{slot: slot, tokIdx: 0, field: uint8(fieldName)},
		})
	}
	h.apply(ops)
	h.assertMatchesModel(t, "동일 토큰 적재")

	// 점 조회: 이름 하나하나가 정확히 자기 항목에 닿아야 합니다.
	for i, name := range names {
		want := postKey{token: prefix, name: name, uid: fmt.Sprintf("uid-%04d", i)}
		c := seekPost(h.root, h.keyer, want, true)
		if !c.valid() {
			t.Fatalf("%d번째 이름을 찾지 못했습니다", i)
		}
		if got := h.keyer.keyOf(c.entry()); comparePostKey(got, want) != 0 {
			t.Fatalf("%d번째 조회가 %+v로 갔습니다 — %+v여야 합니다", i, got, want)
		}
	}
	// 접두사 구간 순회: 전부 나와야 하고 사전순이어야 합니다.
	got := h.walk()
	if len(got) != n {
		t.Fatalf("구간 순회가 %d건입니다 — %d건이어야 합니다", len(got), n)
	}
	for i := 1; i < len(got); i++ {
		if comparePostKey(got[i-1], got[i]) >= 0 {
			t.Fatalf("%d번째에서 순서가 어긋났습니다", i)
		}
	}
}

/* ── rowDir 참조 대조와 집계 ─────────────────────────────────────────────── */

type rowModel struct {
	names   []string
	weights map[string]uint8
}

func (m *rowModel) apply(ops []rowOp) {
	if m.weights == nil {
		m.weights = map[string]uint8{}
	}
	for _, op := range ops {
		_, had := m.weights[op.name]
		if op.remove {
			if had {
				delete(m.weights, op.name)
				for i, n := range m.names {
					if n == op.name {
						m.names = append(m.names[:i], m.names[i+1:]...)
						break
					}
				}
			}
			continue
		}
		m.weights[op.name] = op.weight
		if !had {
			m.names = append(m.names, op.name)
			sort.Strings(m.names)
		}
	}
}

// TestRowDirMatchesCanonicalModel — 이름 정렬·무게·집계가 참조와 같아야 합니다.
func TestRowDirMatchesCanonicalModel(t *testing.T) {
	rng := rand.New(rand.NewSource(4242))
	dir := newRowTree()
	model := &rowModel{}

	for round := 0; round < 150; round++ {
		ops := make([]rowOp, 0, 32)
		for k := 0; k < 20; k++ {
			name := fmt.Sprintf("row-%04d", rng.Intn(1500))
			if rng.Intn(4) == 0 {
				ops = append(ops, rowOp{name: name, remove: true})
				continue
			}
			ops = append(ops, rowOp{name: name, slot: uint32(rng.Intn(1 << 16)), weight: uint8(rng.Intn(33))})
		}
		sorted := sortRowOps(append([]rowOp(nil), ops...))
		var st rowStats
		dir = rowApply(dir, sorted, &st)
		dir = balanceRowRoot(dir, &st)
		model.apply(sorted)

		var got []string
		rowEachAll(dir, func(name string, _ uint32) bool {
			got = append(got, name)
			return true
		})
		if strings.Join(got, ",") != strings.Join(model.names, ",") {
			t.Fatalf("round %d: 이름 목록이 다릅니다 (%d vs %d)", round, len(got), len(model.names))
		}
		for _, n := range model.names {
			_, w, ok := rowFind(dir, n)
			if !ok || w != model.weights[n] {
				t.Fatalf("round %d: %q 무게가 %d(ok=%v)입니다 — %d여야 합니다", round, n, w, ok, model.weights[n])
			}
		}
	}
}

// TestRowAggregatesDriveBoundaryAndFindNext — 집계가 실제 fold 판정을 지탱해야 합니다.
func TestRowAggregatesDriveBoundaryAndFindNext(t *testing.T) {
	dir := newRowTree()
	ops := make([]rowOp, 0, 600)
	total := int64(0)
	for i := 0; i < 600; i++ {
		w := uint8(1 + i%32)
		ops = append(ops, rowOp{name: fmt.Sprintf("row-%04d", i), slot: uint32(i), weight: w})
		total += int64(w)
	}
	var st rowStats
	dir = rowApply(dir, sortRowOps(ops), &st)
	dir = balanceRowRoot(dir, &st)

	// 상한을 전체 무게보다 크게 두면 경계가 없습니다.
	if b := rowPrefixBoundary(dir, total+1); b.overflow {
		t.Fatalf("상한을 넘지 않았는데 경계가 잡혔습니다: %+v", b)
	}
	// 상한을 낮추면 정확한 위치에서 넘겨야 합니다(참조는 단순 누적).
	for _, capTokens := range []int64{1, 33, 200, total - 1} {
		b := rowPrefixBoundary(dir, capTokens)
		var acc int64
		var wantName string
		var found bool
		rowEachAll(dir, func(name string, slot uint32) bool {
			_, w, _ := rowFind(dir, name)
			if acc+int64(w) > capTokens {
				wantName, found = name, true
				return false
			}
			acc += int64(w)
			return true
		})
		if b.overflow != found || b.acc != acc || (found && b.name != wantName) {
			t.Fatalf("cap=%d 경계가 %+v입니다 — acc=%d name=%q overflow=%v여야 합니다",
				capTokens, b, acc, wantName, found)
		}
	}
	// findNext는 budget 이하 무게의 **다음** 행을 정확히 찾아야 합니다.
	hit := rowNextFitting(dir, "row-0000", 1, true)
	if !hit.found || hit.weight != 1 {
		t.Fatalf("budget=1에서 %+v를 찾았습니다", hit)
	}
	if hit.name <= "row-0000" {
		t.Fatalf("afterName 뒤가 아닙니다: %q", hit.name)
	}
	if none := rowNextFitting(dir, "row-0599", 32, true); none.found {
		t.Fatalf("마지막 행 뒤에서 %+v를 찾았습니다", none)
	}
}

/* ── 슬롯 트라이 ─────────────────────────────────────────────────────────── */

// TestTrieCoversMaxSlotAndPathCopies — 최대 슬롯까지 닿고, 갱신이 경로만 복사해야 합니다.
func TestTrieCoversMaxSlotAndPathCopies(t *testing.T) {
	root := newTrieRoot()
	var copied int64
	probes := []uint32{0, 1, 31, 32, 1023, 1024, 1 << 20, maxSlot - 1}
	recs := make(map[uint32]*rowRecord, len(probes))
	for _, slot := range probes {
		rec := &rowRecord{name: fmt.Sprintf("n-%d", slot), uid: fmt.Sprintf("u-%d", slot)}
		recs[slot] = rec
		root = trieSet(root, slot, rec, &copied)
	}
	for _, slot := range probes {
		if got := trieGet(root, slot); got != recs[slot] {
			t.Fatalf("슬롯 %d를 다시 읽지 못했습니다", slot)
		}
	}
	if trieGet(root, 12345) != nil {
		t.Fatal("넣지 않은 슬롯이 값을 돌려줬습니다")
	}
	// 한 번의 갱신은 깊이(5) × 노드 크기 정도만 복사해야 합니다.
	before := copied
	root = trieSet(root, 777, &rowRecord{name: "x"}, &copied)
	if step := copied - before; step > int64(trieDepth)*(trieBranchBytes+trieLeafBytes) {
		t.Fatalf("한 번 갱신에 %d바이트를 복사했습니다 — 경로 복사가 아닙니다", step)
	}
	// 불변: 옛 루트는 그대로여야 합니다.
	oldRoot := newTrieRoot()
	var c2 int64
	first := trieSet(oldRoot, 5, &rowRecord{name: "a"}, &c2)
	_ = trieSet(first, 5, &rowRecord{name: "b"}, &c2)
	if got := trieGet(first, 5); got == nil || got.name != "a" {
		t.Fatal("옛 트라이가 새 값으로 오염됐습니다")
	}
}

/* ── 슬롯 고갈 ──────────────────────────────────────────────────────────── */

// TestSlotExhaustionIsExplicit — 슬롯이 바닥나면 조용히 낡지 않고 압축을 요구해야 합니다.
func TestSlotExhaustionIsExplicit(t *testing.T) {
	part := newNsPart("prod", "service", indexBase)
	part.slotHigh = maxSlot // 인위적으로 고갈시킵니다.
	var st applyStats
	out := applyPartOps(part, true, indexBase, []partOp{
		{name: "row-a", input: &rowInput{name: "row-a", uid: "uid-a"}},
	}, &st)
	if !st.slotExhausted || !st.compactionRequired {
		t.Fatalf("고갈을 알리지 않았습니다: %+v", st)
	}
	if out != part {
		t.Fatal("고갈했는데 반쪽 파티션을 게시했습니다")
	}
}

/* ── 회계 ───────────────────────────────────────────────────────────────── */

// TestCapacityAccountingNeverUnderestimates — 증분 보유 회계는 **절대 실제보다
// 작으면 안 됩니다.**
//
// 예산은 "이만큼은 쓰고 있다"를 근거로 거절합니다. 과소 계상이면 이미 넘긴 뒤에도
// 통과시키므로 상한이 상한이 아니게 됩니다. 그래서 하한은 `>= 실제`로 못박고,
// 상한은 여유가 무한정 부풀지 않도록 좁게(2배) 둡니다.
func TestCapacityAccountingNeverUnderestimates(t *testing.T) {
	const overheadFactor = 2
	part := newNsPart("prod", "service", indexBase)
	for round := 0; round < 12; round++ {
		ops := make([]partOp, 0, 64)
		for i := 0; i < 64; i++ {
			name := fmt.Sprintf("row-%04d", round*64+i)
			ops = append(ops, partOp{
				name: name,
				input: &rowInput{
					name: name, uid: fmt.Sprintf("uid-%s-0123456789abcdef01234567", name),
					labels: []string{"app", "payments", "tier", "backend"},
				},
			})
		}
		var st applyStats
		part = applyPartOps(part, true, indexBase, ops, &st)
		if part.bytes <= 0 {
			t.Fatalf("round %d: 보유 회계가 %d입니다", round, part.bytes)
		}
		actual := part.recomputeBytes()
		if part.bytes < actual {
			t.Fatalf("round %d: 증분 회계 %d가 실제 %d보다 **작습니다** — 예산이 상한이 아니게 됩니다",
				round, part.bytes, actual)
		}
		if part.bytes > actual*overheadFactor {
			t.Fatalf("round %d: 증분 회계 %d가 실제 %d의 %d배를 넘습니다 — 여유가 과합니다",
				round, part.bytes, actual, overheadFactor)
		}
	}
	// 전부 지워도 회계가 음수가 되면 안 됩니다.
	var names []string
	rowEachAll(part.rowDir, func(name string, _ uint32) bool {
		names = append(names, name)
		return true
	})
	del := make([]partOp, 0, len(names))
	for _, n := range names {
		del = append(del, partOp{name: n})
	}
	var st applyStats
	part = applyPartOps(part, true, indexBase, del, &st)
	if part.bytes < 0 {
		t.Fatalf("삭제 후 회계가 음수입니다: %d", part.bytes)
	}
	if part.liveRows != 0 {
		t.Fatalf("삭제 후 행이 %d개 남았습니다", part.liveRows)
	}
}

// TestNsDirTotalIsIncrementalAndExact — 페이지 디렉터리의 합계는 증분으로
// 유지되지만 **다시 합친 값과 정확히 같아야** 합니다.
func TestNsDirTotalIsIncrementalAndExact(t *testing.T) {
	dir := newNsDir()
	var copied int64
	for i := 0; i < 300; i++ {
		p := newNsPart(fmt.Sprintf("ns-%04d", i), "service", indexBase)
		p.bytes = int64(1000 + i)
		dir = dir.upsert(p, &copied)
	}
	if got, want := dir.total, dir.recomputeTotal(); got != want {
		t.Fatalf("증분 합계 %d — 실제 %d", got, want)
	}
	// 같은 namespace를 다시 넣으면 옛 값이 빠져야 합니다.
	p := newNsPart("ns-0000", "service", indexBase)
	p.bytes = 50_000
	dir = dir.upsert(p, &copied)
	if got, want := dir.total, dir.recomputeTotal(); got != want {
		t.Fatalf("교체 후 증분 합계 %d — 실제 %d", got, want)
	}
	if int(dir.agg.nsCount) != 300 {
		t.Fatalf("파티션 수가 %d입니다 — 300이어야 합니다", dir.agg.nsCount)
	}
}

/* ── 커서 ───────────────────────────────────────────────────────────────── */

// TestPostCursorLowerBoundAndRange — lower_bound·구간 순회가 참조와 같아야 합니다.
func TestPostCursorLowerBoundAndRange(t *testing.T) {
	h := newTreeHarness()
	ops := make([]postOp, 0, 400)
	for i := 0; i < 400; i++ {
		name := fmt.Sprintf("row-%03d", i)
		tok := fmt.Sprintf("tok-%03d", i)
		slot := h.addRow(name, fmt.Sprintf("uid-%03d", i), []string{tok})
		ops = append(ops, postOp{
			key:   postKey{token: tok, name: name, uid: fmt.Sprintf("uid-%03d", i)},
			entry: postEntry{slot: slot, tokIdx: 3},
		})
	}
	h.assertKeysDerivable(t, ops)
	h.apply(ops)

	for _, probe := range []string{"tok-000", "tok-199", "tok-2", "tok-399", "tok-zzz", ""} {
		c := seekPost(h.root, h.keyer, postKey{token: probe}, true)
		want := sort.Search(len(h.model.keys), func(i int) bool {
			return comparePostKey(h.model.keys[i], postKey{token: probe}) >= 0
		})
		if want == len(h.model.keys) {
			if c.valid() {
				t.Fatalf("%q: 끝이어야 하는데 항목이 있습니다", probe)
			}
			continue
		}
		if !c.valid() {
			t.Fatalf("%q: 항목이 있어야 하는데 끝입니다", probe)
		}
		if got := h.keyer.keyOf(c.entry()); comparePostKey(got, h.model.keys[want]) != 0 {
			t.Fatalf("%q: lower_bound가 %+v입니다 — %+v여야 합니다", probe, got, h.model.keys[want])
		}
	}
}
