package resourcecatalog

// 지속(persistent) 검색 자료구조 — Round 6 v4 / v4.1
// --------------------------------------------------------------------------
// 증분 갱신이 "이벤트 하나에 GVR 전체 행을 다시 훑지 않는다"를 만족하려면, 갱신
// 단위가 배열 전체가 아니라 **경로 하나**여야 합니다. 그래서 세 구조를 씁니다.
//
//	postTree  (token, fullName, uid) 정렬 B+트리. 리프는 8바이트 entry만 담고
//	          논리 키는 슬롯 → rowRecord에서 **유도**합니다. 그래서 posting당
//	          문자열 헤더가 0입니다.
//	rowDir    fullName 정렬 B+트리. 서브트리 집계(totalWeight/minPos/posCount)와
//	          리프 그룹 요약(groupWeight/groupMinPos)을 들어 namespace label 상한
//	          fold를 O(log R)에 유지합니다.
//	trie      slot(uint32) → *rowRecord. 32-way 5단계라 갱신이 경로 5노드입니다.
//
// 세 구조 모두 불변이고 경로 복사로만 바뀝니다. 게시된 {rowRoot, postRoot} 쌍은
// 함께 만들어졌으므로, 어떤 독자가 보는 posting도 그 쌍의 rowRoot에서 그 시점
// 살아 있던 행으로 해석됩니다. 그래서 entry에 세대(gen) 태그가 필요 없습니다.
// (배치 중 해제한 슬롯은 pendingFree에 모였다가 **게시 시점에만** 자유 목록에
// 합류하므로 같은 배치에서 재사용되지 않습니다.)
//
// 분리 키는 **fence**입니다: max(left) < sep <= min(right). 경계 항목을 지워
// min(right)가 커져도 fence 조건이 유지되므로 삭제 시 교체가 필요 없습니다.
// 분리 키 문자열은 분할 시점에 **소유 복사**하므로 지워진 행의 backing을 붙잡지
// 않습니다(그 바이트는 sepBytes로 계상합니다).

import "strings"

const (
	// postLeafMax/postLeafMin은 posting 리프의 항목 수 상한·하한입니다.
	//
	// postLeafSplit은 분할 후 채움 정도입니다. 상한의 절반으로 나누면 벌크 적재
	// (부트스트랩·회수) 직후 리프가 50%만 차서 **용량 비용이 두 배**가 됩니다.
	// 3/4로 두면 이후 삽입 여유를 남기면서도 보유 바이트가 그만큼 줄어듭니다.
	postLeafMax   = 256
	postLeafSplit = 192
	postLeafMin   = 64
	// rowLeafMax/rowLeafMin은 행 디렉터리 리프의 항목 수 상한·하한입니다.
	rowLeafMax   = 256
	rowLeafSplit = 192
	rowLeafMin   = 64
	// treeFanout/treeNodeMin은 내부 노드의 자식 수 상한·하한입니다.
	treeFanout  = 32
	treeNodeMin = 12

	// rowGroupSize/rowGroups는 리프 안의 미세 집계 단위입니다.
	// 그룹 요약이 없으면 findNext가 리프 하나에서 최대 rowLeafMax번 무게를 봅니다.
	rowGroupSize = 16
	rowGroups    = rowLeafMax / rowGroupSize

	// trieBits/trieWidth/trieDepth/maxSlot은 슬롯 테이블의 형태입니다.
	// 5비트 × 5단계 = 25비트이므로 maxSlot(2^24)을 덮습니다.
	trieBits  = 5
	trieWidth = 1 << trieBits
	trieDepth = 5
	maxSlot   = 1 << 24

	// noMinWeight는 "이 서브트리에 양수 무게 행이 없다"는 표식입니다.
	noMinWeight uint8 = 255

	// trieSpineBytes는 파티션마다 반드시 존재하는 트라이 척추(브랜치 4단계)입니다.
	// 행 하나짜리 파티션도 이 비용을 냅니다 — 회계가 과소 계상되지 않도록
	// 파티션 고정 비용에 포함합니다.
	trieSpineBytes = (trieDepth - 1) * trieBranchBytes
)

/* ── 행 레코드 ─────────────────────────────────────────────────────────────── */

// rowRecord는 검색 인덱스가 아는 행 하나입니다. **불변**입니다.
//
// label 토큰은 별도 슬라이스가 아니라 `길이 1바이트 + 바이트`가 이어진 blob 하나로
// 듭니다. 토큰이 tokenPrefixBytes(64)를 넘지 않으므로 길이는 1바이트면 충분하고,
// 오프셋 배열(슬라이스 헤더 24 + 백킹)을 통째로 없앨 수 있습니다.
//
// name/uid는 informer 객체의 문자열을 **차용**합니다. Go 문자열은 불변이라 안전하지만
// 그 바이트의 수명을 이 인덱스가 늘리므로 회계에 그대로 계상합니다.
type rowRecord struct {
	name    string // 16
	uid     string // 16
	nameTok string // 16  normalizeToken(name). 대개 name과 backing을 공유합니다.

	labelBlob string // 16  자체 소유 복사본
	weight    uint16 // 2   label 토큰 수 (0..2*MaxLabelKeysPerObject)
	flags     uint8  // 1
}

const (
	// rowFlagKeysTruncated는 이 행의 label 키가 MaxLabelKeysPerObject로 잘렸다는 뜻입니다.
	// **행 자신의 성질**이므로 다른 행이 이 값을 바꾸지 못합니다.
	rowFlagKeysTruncated uint8 = 1 << iota
)

// rowRecordFixedBytes는 rowRecord 구조체 자체의 크기입니다(64비트, 정렬 후).
const rowRecordFixedBytes = 72

// labelTokenAt은 blob에서 i번째 label 토큰을 꺼냅니다. 할당이 없습니다.
func (r *rowRecord) labelTokenAt(i int) string {
	if r == nil || i < 0 || i >= int(r.weight) {
		return ""
	}
	blob := r.labelBlob
	at := 0
	for k := 0; k < i; k++ {
		if at >= len(blob) {
			return ""
		}
		at += 1 + int(blob[at])
	}
	if at >= len(blob) {
		return ""
	}
	n := int(blob[at])
	if at+1+n > len(blob) {
		return ""
	}
	return blob[at+1 : at+1+n]
}

// eachLabelToken은 label 토큰을 순서대로 넘깁니다. 순회는 blob 한 번입니다.
func (r *rowRecord) eachLabelToken(fn func(i int, tok string) bool) {
	if r == nil {
		return
	}
	blob := r.labelBlob
	at, i := 0, 0
	for at < len(blob) {
		n := int(blob[at])
		if at+1+n > len(blob) {
			return
		}
		if !fn(i, blob[at+1:at+1+n]) {
			return
		}
		at += 1 + n
		i++
	}
}

// encodeLabelBlob은 정규 label 토큰 목록을 blob 하나로 만듭니다.
// 토큰은 이미 normalizeToken을 거쳐 64바이트 이하여야 합니다.
func encodeLabelBlob(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	total := 0
	for _, t := range tokens {
		total += 1 + len(t)
	}
	var b strings.Builder
	b.Grow(total)
	for _, t := range tokens {
		b.WriteByte(byte(len(t)))
		b.WriteString(t)
	}
	return b.String()
}

// retainedBytes는 이 행이 붙잡는 바이트입니다.
//
// 차용한 name/uid의 바이트도 셉니다 — 인덱스가 그 backing의 수명을 늘리기 때문입니다.
// nameTok은 name과 backing을 공유할 때가 대부분이라 헤더만 셉니다.
func (r *rowRecord) retainedBytes() int64 {
	if r == nil {
		return 0
	}
	total := int64(rowRecordFixedBytes)
	total += int64(len(r.name)) + int64(len(r.uid)) + int64(len(r.labelBlob))
	if r.nameTok != "" && !sharesPrefixBacking(r.name, r.nameTok) {
		total += int64(len(r.nameTok))
	}
	return total
}

// sharesPrefixBacking은 tok이 name의 접두사 substring인지(=새 할당이 아닌지) 봅니다.
// 소문자화가 일어나지 않았다면 언제나 참입니다.
func sharesPrefixBacking(name, tok string) bool {
	return len(tok) <= len(name) && strings.HasPrefix(name, tok)
}

/* ── 슬롯 트라이 ──────────────────────────────────────────────────────────── */

// trieLeaf는 슬롯 32개의 행 포인터입니다. unsafe.Sizeof = 256.
type trieLeaf struct {
	rows [trieWidth]*rowRecord
}

// trieBranch는 32갈래 내부 노드입니다. leafLevel이면 leafKids만 씁니다.
// unsafe.Sizeof = 520 (8 + 256 + 256).
type trieBranch struct {
	leafLevel  bool
	leafKids   [trieWidth]*trieLeaf
	branchKids [trieWidth]*trieBranch
}

const (
	trieLeafBytes   = 256
	trieBranchBytes = 520
)

// trieShift는 depth 단계에서 봐야 할 비트 위치입니다.
// depth 0(루트)이 최상위이고, trieDepth-1이 리프 바로 위입니다.
func trieShift(depth int) uint {
	return uint(trieBits * (trieDepth - 1 - depth))
}

func newTrieRoot() *trieBranch {
	// 깊이 5 = 브랜치 4단계 + 리프 1단계입니다. 루트는 depth 0입니다.
	return &trieBranch{leafLevel: trieDepth == 2}
}

// trieGet은 슬롯의 행을 돌려줍니다. 없으면 nil입니다.
func trieGet(root *trieBranch, slot uint32) *rowRecord {
	n := root
	for depth := 0; n != nil; depth++ {
		idx := (slot >> trieShift(depth)) & (trieWidth - 1)
		if depth == trieDepth-2 {
			leaf := n.leafKids[idx]
			if leaf == nil {
				return nil
			}
			return leaf.rows[slot&(trieWidth-1)]
		}
		n = n.branchKids[idx]
	}
	return nil
}

// trieSet은 슬롯에 행을 넣은 **새 루트**를 돌려줍니다. 경로 노드만 복사합니다.
// copied에는 복사된 노드의 바이트가 더해집니다(정점 회계용).
func trieSet(root *trieBranch, slot uint32, rec *rowRecord, copied *int64) *trieBranch {
	return trieSetAt(root, 0, slot, rec, copied)
}

func trieSetAt(n *trieBranch, depth int, slot uint32, rec *rowRecord, copied *int64) *trieBranch {
	var next trieBranch
	if n != nil {
		next = *n
	}
	next.leafLevel = depth == trieDepth-2
	if copied != nil {
		*copied += trieBranchBytes
	}
	idx := (slot >> trieShift(depth)) & (trieWidth - 1)
	if depth == trieDepth-2 {
		var leaf trieLeaf
		if old := next.leafKids[idx]; old != nil {
			leaf = *old
		}
		if copied != nil {
			*copied += trieLeafBytes
		}
		leaf.rows[slot&(trieWidth-1)] = rec
		next.leafKids[idx] = &leaf
		return &next
	}
	next.branchKids[idx] = trieSetAt(next.branchKids[idx], depth+1, slot, rec, copied)
	return &next
}

// trieNodeBytes는 트라이 전체가 붙잡는 노드 바이트입니다(행 자체는 제외).
// 회계는 증분으로 유지하므로 이 함수는 테스트 검증용입니다.
func trieNodeBytes(root *trieBranch) int64 {
	if root == nil {
		return 0
	}
	total := int64(trieBranchBytes)
	if root.leafLevel {
		for _, l := range root.leafKids {
			if l != nil {
				total += trieLeafBytes
			}
		}
		return total
	}
	for _, b := range root.branchKids {
		total += trieNodeBytes(b)
	}
	return total
}

/* ── 자유 슬롯 목록 ───────────────────────────────────────────────────────── */

// freeNode는 재사용 가능한 슬롯 묶음입니다. 불변입니다.
type freeNode struct {
	slots [32]uint32
	n     uint8
	next  *freeNode
}

const freeNodeBytes = 144

// freeStack은 배치가 쓰는 **사설** 자유 목록입니다.
//
// pop은 공유 노드를 만나면 **한 번만** 복제해 사설로 만든 뒤 그 안에서 소비합니다.
// 해제 슬롯은 여기가 아니라 pendingFree로 갑니다 — 그래서 같은 배치에서 재사용되지
// 않고, §1.3의 안정성 정리가 성립합니다.
type freeStack struct {
	top *freeNode
	// privateFrom은 top이 이미 사설인지입니다. 공유 노드를 두 번 복제하지 않습니다.
	private bool
	// copied는 이 배치가 만든 사설/중간 노드 바이트입니다(inflight 회계).
	copied int64
}

func newFreeStack(base *freeNode) *freeStack {
	return &freeStack{top: base}
}

// pop은 재사용 슬롯 하나를 꺼냅니다. 없으면 false입니다.
func (f *freeStack) pop() (uint32, bool) {
	for f.top != nil {
		if f.top.n == 0 {
			f.top, f.private = f.top.next, false
			continue
		}
		if !f.private {
			clone := *f.top
			f.top, f.private = &clone, true
			f.copied += freeNodeBytes
		}
		f.top.n--
		return f.top.slots[f.top.n], true
	}
	return 0, false
}

// publish는 pendingFree를 **완전히 pop된** 사설 top 앞에 붙인 최종 자유 목록입니다.
// 캡처한 base 앞에 붙이지 않습니다 — 그러면 이미 꺼내 쓴 슬롯이 되살아납니다.
func (f *freeStack) publish(pendingFree []uint32) *freeNode {
	head := f.top
	for i := 0; i < len(pendingFree); {
		node := &freeNode{next: head}
		for i < len(pendingFree) && node.n < 32 {
			node.slots[node.n] = pendingFree[i]
			node.n++
			i++
		}
		head = node
		f.copied += freeNodeBytes
	}
	return head
}

// freeListBytes는 자유 목록이 붙잡는 바이트입니다.
func freeListBytes(head *freeNode) int64 {
	var total int64
	for n := head; n != nil; n = n.next {
		total += freeNodeBytes
	}
	return total
}

/* ── posting B+트리 ──────────────────────────────────────────────────────── */

// postEntry는 posting 하나입니다. 정확히 8바이트입니다.
//
// 논리 키 (token, fullName, uid)는 slot과 tokIdx에서 유도합니다. 토큰을 여기 담지
// 않으므로 posting당 문자열 헤더가 0이고, 이름이 앞 64바이트를 공유해 토큰이 같아도
// 키의 2순위 성분이 **원문 이름**이라 신원이 갈립니다.
type postEntry struct {
	slot    uint32 // 4
	tokIdx  uint16 // 2  0=name, 1=namespace, 2=kind, 3+=label[i-3]
	field   uint8  // 1
	prevLCP uint8  // 1  이 행의 직전(더 작은) 토큰과의 공통 접두사 길이
}

const postEntryBytes = 8

// postKey는 posting의 논리 키입니다. **내부 노드의 fence로만** 실체화됩니다.
type postKey struct {
	token string
	name  string
	uid   string
}

const postKeyBytes = 48

func comparePostKey(a, b postKey) int {
	if c := strings.Compare(a.token, b.token); c != 0 {
		return c
	}
	if c := strings.Compare(a.name, b.name); c != 0 {
		return c
	}
	return strings.Compare(a.uid, b.uid)
}

// clonePostKey는 fence를 소유 복사본으로 만듭니다.
// 지워진 행의 backing을 분리 키가 붙잡지 못하게 하는 것이 목적입니다.
func clonePostKey(k postKey) postKey {
	return postKey{
		token: strings.Clone(k.token),
		name:  strings.Clone(k.name),
		uid:   strings.Clone(k.uid),
	}
}

func postKeyBytesOf(k postKey) int64 {
	return postKeyBytes + int64(len(k.token)+len(k.name)+len(k.uid))
}

// postKeyer는 entry에서 논리 키를 유도합니다.
//
// root는 **지금 만드는 중인** 트라이입니다. 배치는 행을 먼저 트라이에 넣고,
// 지워질 행의 슬롯은 게시 직전까지 비우지 않으므로 삭제 대상의 키도 유도됩니다.
type postKeyer struct {
	root      *trieBranch
	namespace string
	kindTok   string
}

func (k postKeyer) tokenOf(rec *rowRecord, idx uint16) string {
	switch idx {
	case 0:
		return rec.nameTok
	case 1:
		return k.namespace
	case 2:
		return k.kindTok
	default:
		return rec.labelTokenAt(int(idx) - 3)
	}
}

func (k postKeyer) keyOf(e postEntry) postKey {
	rec := trieGet(k.root, e.slot)
	if rec == nil {
		return postKey{}
	}
	return postKey{token: k.tokenOf(rec, e.tokIdx), name: rec.name, uid: rec.uid}
}

// postLeaf는 정렬된 entry 묶음입니다. 불변입니다.
type postLeaf struct {
	entries []postEntry
}

// postNode는 내부 노드입니다. leafLevel이면 leafKids를, 아니면 nodeKids를 씁니다.
// seps[i]는 kids[i+1]의 fence입니다: max(kids[i]) < seps[i] <= min(kids[i+1]).
type postNode struct {
	leafLevel bool
	seps      []postKey
	leafKids  []*postLeaf
	nodeKids  []*postNode
}

func (n *postNode) childCount() int {
	if n.leafLevel {
		return len(n.leafKids)
	}
	return len(n.nodeKids)
}

// newPostTree는 빈 트리(리프 하나)입니다.
func newPostTree() *postNode {
	return &postNode{leafLevel: true, leafKids: []*postLeaf{{}}}
}

// childIndexFor는 key가 속한 자식 번호입니다. fence 규칙이라 key >= seps[i]면 오른쪽입니다.
func (n *postNode) childIndexFor(key postKey) int {
	lo, hi := 0, len(n.seps)
	for lo < hi {
		mid := (lo + hi) / 2
		if comparePostKey(key, n.seps[mid]) >= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// postLowerBound는 리프 안에서 key 이상인 첫 위치입니다.
func postLowerBound(leaf *postLeaf, keyer postKeyer, key postKey) int {
	lo, hi := 0, len(leaf.entries)
	for lo < hi {
		mid := (lo + hi) / 2
		if comparePostKey(keyer.keyOf(leaf.entries[mid]), key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

/* ── posting 커서 ────────────────────────────────────────────────────────── */

// postCursor는 트리를 사전순으로 훑는 커서입니다. 스택 깊이는 트리 높이입니다.
type postCursor struct {
	keyer postKeyer
	path  []*postNode
	idx   []int
	leaf  *postLeaf
	pos   int
	ok    bool
}

// seekPost는 key 이상인 첫 posting에 커서를 놓습니다.
func seekPost(root *postNode, keyer postKeyer, key postKey, hasKey bool) *postCursor {
	c := &postCursor{keyer: keyer}
	n := root
	for n != nil {
		var i int
		if hasKey {
			i = n.childIndexFor(key)
		}
		c.path = append(c.path, n)
		c.idx = append(c.idx, i)
		if n.leafLevel {
			if i >= len(n.leafKids) {
				c.ok = false
				return c
			}
			c.leaf = n.leafKids[i]
			if hasKey {
				c.pos = postLowerBound(c.leaf, keyer, key)
			}
			c.ok = true
			c.skipEmpty()
			return c
		}
		if i >= len(n.nodeKids) {
			c.ok = false
			return c
		}
		n = n.nodeKids[i]
	}
	c.ok = false
	return c
}

// skipEmpty는 소진된 리프를 지나 다음 리프로 옮깁니다.
func (c *postCursor) skipEmpty() {
	for c.ok && (c.leaf == nil || c.pos >= len(c.leaf.entries)) {
		if !c.advanceLeaf() {
			c.ok = false
			return
		}
	}
}

// advanceLeaf는 다음 리프로 내려갑니다.
func (c *postCursor) advanceLeaf() bool {
	for d := len(c.path) - 1; d >= 0; d-- {
		n := c.path[d]
		c.idx[d]++
		if c.idx[d] < n.childCount() {
			c.path = c.path[:d+1]
			c.idx = c.idx[:d+1]
			cur := n
			i := c.idx[d]
			for {
				if cur.leafLevel {
					c.leaf, c.pos = cur.leafKids[i], 0
					return true
				}
				child := cur.nodeKids[i]
				c.path = append(c.path, child)
				c.idx = append(c.idx, 0)
				cur, i = child, 0
			}
		}
	}
	return false
}

func (c *postCursor) valid() bool { return c.ok && c.leaf != nil && c.pos < len(c.leaf.entries) }

func (c *postCursor) entry() postEntry { return c.leaf.entries[c.pos] }

func (c *postCursor) next() {
	if !c.ok {
		return
	}
	c.pos++
	c.skipEmpty()
}

/* ── posting 배치 적용 ───────────────────────────────────────────────────── */

// postOp는 정렬된 배치 연산 하나입니다.
type postOp struct {
	key    postKey
	entry  postEntry
	remove bool
}

// postStats는 한 번의 적용이 복사한 양입니다. 계측·정점 회계에 씁니다.
type postStats struct {
	// leafEntriesCopied는 복사한 리프 entry 수입니다(= directory_copies).
	leafEntriesCopied int64
	// nodesCopied는 복사한 내부 노드 수입니다.
	nodesCopied int64
	// sepBytes는 이번에 새로 만든 fence 문자열 바이트입니다.
	sepBytes int64
	// entryDelta는 트리 전체 entry 수 변화입니다.
	entryDelta int64
	// leafDelta는 리프 수 변화입니다.
	leafDelta int64
	// capBytesDelta는 리프 **실제 용량**의 증감(바이트)입니다.
	//
	// "리프 용량이 항목을 덮는다"는 가정은 리프가 가득 찼을 때만 참입니다.
	// 부분 충전 리프에서는 cap이 len에 가까워 항목이 곧 비용이므로, 추정이
	// 실제보다 작아집니다. 그래서 용량 변화를 그때그때 정확히 셉니다.
	capBytesDelta int64
}

// postLeafHeaderBytes는 리프 하나의 고정 비용입니다(구조체 + 슬라이스 헤더).
const postLeafHeaderBytes = 24 * 2

// postApply는 정렬된 ops를 경로 복사로 적용한 **새 루트**를 돌려줍니다.
//
// ops는 key 오름차순이어야 하고 같은 키가 두 번 나오지 않아야 합니다.
// 손대지 않은 서브트리는 그대로 공유합니다.
func postApply(root *postNode, keyer postKeyer, ops []postOp, st *postStats) *postNode {
	if len(ops) == 0 {
		return root
	}
	next := postApplyNode(root, keyer, ops, st)
	// 루트 붕괴: 자식이 하나뿐인 내부 루트는 그 자식이 루트가 됩니다.
	for !next.leafLevel && len(next.nodeKids) == 1 {
		next = next.nodeKids[0]
	}
	return next
}

// postApplyNode는 한 노드에 ops를 적용합니다.
func postApplyNode(n *postNode, keyer postKeyer, ops []postOp, st *postStats) *postNode {
	st.nodesCopied++
	out := &postNode{leafLevel: n.leafLevel}
	out.seps = append(out.seps, n.seps...)
	if n.leafLevel {
		out.leafKids = append(out.leafKids, n.leafKids...)
	} else {
		out.nodeKids = append(out.nodeKids, n.nodeKids...)
	}

	// ops를 자식별로 나눠 **뒤에서 앞으로** 적용합니다. 뒤에서 처리하면 앞쪽 자식의
	// 번호가 분할·병합으로 흔들리지 않습니다.
	type chunk struct {
		child int
		lo    int
		hi    int
	}
	chunks := make([]chunk, 0, 8)
	i := 0
	for i < len(ops) {
		c := out.childIndexFor(ops[i].key)
		j := i + 1
		for j < len(ops) && out.childIndexFor(ops[j].key) == c {
			j++
		}
		chunks = append(chunks, chunk{child: c, lo: i, hi: j})
		i = j
	}
	for k := len(chunks) - 1; k >= 0; k-- {
		ch := chunks[k]
		if out.leafLevel {
			applyToLeafChild(out, ch.child, keyer, ops[ch.lo:ch.hi], st)
			continue
		}
		child := postApplyNode(out.nodeKids[ch.child], keyer, ops[ch.lo:ch.hi], st)
		out.nodeKids[ch.child] = child
		rebalanceNodeChild(out, ch.child, keyer, st)
	}
	return out
}

// applyToLeafChild는 리프 하나에 ops를 적용하고 필요하면 분할·병합합니다.
func applyToLeafChild(parent *postNode, at int, keyer postKeyer, ops []postOp, st *postStats) {
	old := parent.leafKids[at]
	oldCap := int64(cap(old.entries))
	merged := make([]postEntry, 0, len(old.entries)+len(ops))
	oi, opi := 0, 0
	for oi < len(old.entries) || opi < len(ops) {
		if oi >= len(old.entries) {
			if !ops[opi].remove {
				merged = append(merged, ops[opi].entry)
			}
			opi++
			continue
		}
		if opi >= len(ops) {
			merged = append(merged, old.entries[oi])
			oi++
			continue
		}
		cmp := comparePostKey(keyer.keyOf(old.entries[oi]), ops[opi].key)
		switch {
		case cmp < 0:
			merged = append(merged, old.entries[oi])
			oi++
		case cmp > 0:
			if !ops[opi].remove {
				merged = append(merged, ops[opi].entry)
			}
			opi++
		default:
			// 같은 키: 삭제면 버리고, 삽입이면 새 값으로 덮습니다.
			if !ops[opi].remove {
				merged = append(merged, ops[opi].entry)
			}
			oi++
			opi++
		}
	}
	st.leafEntriesCopied += int64(len(old.entries) + len(merged))
	st.entryDelta += int64(len(merged)) - int64(len(old.entries))

	if len(merged) <= postLeafMax {
		parent.leafKids[at] = &postLeaf{entries: merged}
		st.capBytesDelta += (int64(cap(merged)) - oldCap) * postEntryBytes
		rebalanceLeafChild(parent, at, keyer, st)
		return
	}
	// 분할: postLeafSplit 단위로 나눕니다.
	parts := make([]*postLeaf, 0, len(merged)/postLeafSplit+1)
	for lo := 0; lo < len(merged); lo += postLeafSplit {
		hi := lo + postLeafSplit
		if hi > len(merged) {
			hi = len(merged)
		}
		seg := make([]postEntry, hi-lo)
		copy(seg, merged[lo:hi])
		parts = append(parts, &postLeaf{entries: seg})
	}
	newCap := int64(0)
	for _, p := range parts {
		newCap += int64(cap(p.entries))
	}
	st.capBytesDelta += (newCap - oldCap) * postEntryBytes
	st.leafDelta += int64(len(parts)) - 1
	newSeps := make([]postKey, 0, len(parent.seps)+len(parts)-1)
	newSeps = append(newSeps, parent.seps[:at]...)
	for p := 1; p < len(parts); p++ {
		fence := clonePostKey(keyer.keyOf(parts[p].entries[0]))
		st.sepBytes += postKeyBytesOf(fence)
		newSeps = append(newSeps, fence)
	}
	newSeps = append(newSeps, parent.seps[at:]...)
	newKids := make([]*postLeaf, 0, len(parent.leafKids)+len(parts)-1)
	newKids = append(newKids, parent.leafKids[:at]...)
	newKids = append(newKids, parts...)
	newKids = append(newKids, parent.leafKids[at+1:]...)
	parent.seps, parent.leafKids = newSeps, newKids
}

// rebalanceLeafChild는 하한 미만 리프를 형제와 병합하거나 재분배합니다.
func rebalanceLeafChild(parent *postNode, at int, keyer postKeyer, st *postStats) {
	if len(parent.leafKids) <= 1 {
		return
	}
	leaf := parent.leafKids[at]
	if len(leaf.entries) >= postLeafMin {
		return
	}
	// 오른쪽 형제를 먼저 봅니다. 없으면 왼쪽을 봅니다.
	right := at + 1
	left := at
	if right >= len(parent.leafKids) {
		right = at
		left = at - 1
	}
	a, b := parent.leafKids[left], parent.leafKids[right]
	total := len(a.entries) + len(b.entries)
	oldCap := int64(cap(a.entries) + cap(b.entries))
	st.leafEntriesCopied += int64(total)
	if total <= postLeafMax {
		joined := make([]postEntry, 0, total)
		joined = append(joined, a.entries...)
		joined = append(joined, b.entries...)
		kids := make([]*postLeaf, 0, len(parent.leafKids)-1)
		kids = append(kids, parent.leafKids[:left]...)
		kids = append(kids, &postLeaf{entries: joined})
		kids = append(kids, parent.leafKids[right+1:]...)
		seps := make([]postKey, 0, len(parent.seps)-1)
		seps = append(seps, parent.seps[:left]...)
		seps = append(seps, parent.seps[right:]...)
		parent.leafKids, parent.seps = kids, seps
		st.capBytesDelta += (int64(cap(joined)) - oldCap) * postEntryBytes
		st.leafDelta--
		return
	}
	// 재분배: 절반씩 나누고 fence를 새로 만듭니다.
	joined := make([]postEntry, 0, total)
	joined = append(joined, a.entries...)
	joined = append(joined, b.entries...)
	half := total / 2
	first := make([]postEntry, half)
	copy(first, joined[:half])
	second := make([]postEntry, total-half)
	copy(second, joined[half:])
	parent.leafKids[left] = &postLeaf{entries: first}
	parent.leafKids[right] = &postLeaf{entries: second}
	st.capBytesDelta += (int64(cap(first)+cap(second)) - oldCap) * postEntryBytes
	fence := clonePostKey(keyer.keyOf(second[0]))
	st.sepBytes += postKeyBytesOf(fence)
	parent.seps[left] = fence
}

// rebalanceNodeChild는 하한 미만 내부 노드를 형제와 병합하거나 재분배합니다.
func rebalanceNodeChild(parent *postNode, at int, keyer postKeyer, st *postStats) {
	if len(parent.nodeKids) <= 1 {
		return
	}
	child := parent.nodeKids[at]
	if child.childCount() >= treeNodeMin {
		if child.childCount() <= treeFanout {
			return
		}
		splitNodeChild(parent, at, st)
		return
	}
	right := at + 1
	left := at
	if right >= len(parent.nodeKids) {
		right = at
		left = at - 1
	}
	a, b := parent.nodeKids[left], parent.nodeKids[right]
	if a.leafLevel != b.leafLevel {
		return // 형태가 다르면 손대지 않습니다(발생하지 않아야 합니다).
	}
	total := a.childCount() + b.childCount()
	st.nodesCopied++
	joined := &postNode{leafLevel: a.leafLevel}
	joined.seps = append(joined.seps, a.seps...)
	joined.seps = append(joined.seps, parent.seps[left])
	joined.seps = append(joined.seps, b.seps...)
	if a.leafLevel {
		joined.leafKids = append(joined.leafKids, a.leafKids...)
		joined.leafKids = append(joined.leafKids, b.leafKids...)
	} else {
		joined.nodeKids = append(joined.nodeKids, a.nodeKids...)
		joined.nodeKids = append(joined.nodeKids, b.nodeKids...)
	}
	kids := make([]*postNode, 0, len(parent.nodeKids)-1)
	kids = append(kids, parent.nodeKids[:left]...)
	kids = append(kids, joined)
	kids = append(kids, parent.nodeKids[right+1:]...)
	seps := make([]postKey, 0, len(parent.seps)-1)
	seps = append(seps, parent.seps[:left]...)
	seps = append(seps, parent.seps[right:]...)
	parent.nodeKids, parent.seps = kids, seps
	if total > treeFanout {
		splitNodeChild(parent, left, st)
	}
}

// splitNodeChild는 fanout을 넘긴 내부 노드를 둘로 나눕니다.
func splitNodeChild(parent *postNode, at int, st *postStats) {
	child := parent.nodeKids[at]
	count := child.childCount()
	if count <= treeFanout {
		return
	}
	half := count / 2
	st.nodesCopied += 2
	left := &postNode{leafLevel: child.leafLevel}
	right := &postNode{leafLevel: child.leafLevel}
	left.seps = append(left.seps, child.seps[:half-1]...)
	up := child.seps[half-1]
	right.seps = append(right.seps, child.seps[half:]...)
	if child.leafLevel {
		left.leafKids = append(left.leafKids, child.leafKids[:half]...)
		right.leafKids = append(right.leafKids, child.leafKids[half:]...)
	} else {
		left.nodeKids = append(left.nodeKids, child.nodeKids[:half]...)
		right.nodeKids = append(right.nodeKids, child.nodeKids[half:]...)
	}
	kids := make([]*postNode, 0, len(parent.nodeKids)+1)
	kids = append(kids, parent.nodeKids[:at]...)
	kids = append(kids, left, right)
	kids = append(kids, parent.nodeKids[at+1:]...)
	seps := make([]postKey, 0, len(parent.seps)+1)
	seps = append(seps, parent.seps[:at]...)
	seps = append(seps, up)
	seps = append(seps, parent.seps[at:]...)
	parent.nodeKids, parent.seps = kids, seps
}

// balancePostRoot는 루트의 자식 수가 fanout을 넘으면 한 단계씩 위로 접습니다.
//
// 벌크 적재(부트스트랩·회수)는 리프 하나를 수천 개로 쪼개므로, 분할 한 번으로는
// 상한을 만족하지 못합니다. 자식을 fanout/2 단위로 묶어 새 단계를 만들고,
// 남은 개수가 상한 안에 들어올 때까지 되풀이합니다.
func balancePostRoot(root *postNode, st *postStats) *postNode {
	for root.childCount() > treeFanout {
		const group = treeFanout / 2
		count := root.childCount()
		levels := (count + group - 1) / group
		parent := &postNode{leafLevel: false}
		st.nodesCopied += int64(levels + 1)
		for g := 0; g < levels; g++ {
			lo := g * group
			hi := lo + group
			if hi > count {
				hi = count
			}
			node := &postNode{leafLevel: root.leafLevel}
			node.seps = append(node.seps, root.seps[lo:hi-1]...)
			if root.leafLevel {
				node.leafKids = append(node.leafKids, root.leafKids[lo:hi]...)
			} else {
				node.nodeKids = append(node.nodeKids, root.nodeKids[lo:hi]...)
			}
			if g > 0 {
				parent.seps = append(parent.seps, root.seps[lo-1])
			}
			parent.nodeKids = append(parent.nodeKids, node)
		}
		root = parent
	}
	return root
}

// postTreeBytes는 트리가 붙잡는 바이트입니다(행 자체는 제외).
func postTreeBytes(n *postNode) int64 {
	if n == nil {
		return 0
	}
	total := int64(24 * 3) // leafLevel + 세 슬라이스 헤더
	for _, s := range n.seps {
		total += postKeyBytesOf(s)
	}
	total += int64(cap(n.seps)-len(n.seps)) * postKeyBytes
	if n.leafLevel {
		total += int64(cap(n.leafKids)) * 8
		for _, l := range n.leafKids {
			total += 24 + int64(cap(l.entries))*postEntryBytes
		}
		return total
	}
	total += int64(cap(n.nodeKids)) * 8
	for _, c := range n.nodeKids {
		total += postTreeBytes(c)
	}
	return total
}

// postTreeCount는 트리의 entry 수입니다(테스트 검증용).
func postTreeCount(n *postNode) int {
	if n == nil {
		return 0
	}
	if n.leafLevel {
		total := 0
		for _, l := range n.leafKids {
			total += len(l.entries)
		}
		return total
	}
	total := 0
	for _, c := range n.nodeKids {
		total += postTreeCount(c)
	}
	return total
}

/* ── 행 디렉터리 B+트리 ──────────────────────────────────────────────────── */

// rowAgg는 자식 하나의 서브트리 집계입니다. unsafe.Sizeof = 16.
type rowAgg struct {
	total    int64 // 8  무게 합
	posCount int32 // 4  양수 무게 행 수
	minPos   uint8 // 1  양수 무게 최솟값(없으면 noMinWeight)
}

const rowAggBytes = 16

// rowLeaf는 이름 정렬 항목 묶음입니다. 그룹 요약이 findNext의 리프 스캔을 32회로 묶습니다.
type rowLeaf struct {
	names       []string
	slots       []uint32
	weights     []uint8
	groupWeight [rowGroups]uint16
	groupMinPos [rowGroups]uint8
}

// rowNode는 행 디렉터리 내부 노드입니다. seps[i]는 kids[i+1]의 fence입니다.
type rowNode struct {
	leafLevel bool
	seps      []string
	aggs      []rowAgg
	leafKids  []*rowLeaf
	nodeKids  []*rowNode
}

func (n *rowNode) childCount() int {
	if n.leafLevel {
		return len(n.leafKids)
	}
	return len(n.nodeKids)
}

func newRowTree() *rowNode {
	leaf := &rowLeaf{}
	leaf.finalize()
	return &rowNode{leafLevel: true, leafKids: []*rowLeaf{leaf}, aggs: []rowAgg{leaf.agg()}}
}

// finalize는 그룹 요약을 다시 계산합니다. 리프를 만들 때마다 한 번입니다.
func (l *rowLeaf) finalize() {
	for g := 0; g < rowGroups; g++ {
		l.groupWeight[g] = 0
		l.groupMinPos[g] = noMinWeight
	}
	for i, w := range l.weights {
		g := i / rowGroupSize
		if g >= rowGroups {
			g = rowGroups - 1
		}
		l.groupWeight[g] += uint16(w)
		if w > 0 && w < l.groupMinPos[g] {
			l.groupMinPos[g] = w
		}
	}
}

func (l *rowLeaf) agg() rowAgg {
	out := rowAgg{minPos: noMinWeight}
	for _, w := range l.weights {
		out.total += int64(w)
		if w > 0 {
			out.posCount++
			if w < out.minPos {
				out.minPos = w
			}
		}
	}
	return out
}

func (n *rowNode) agg() rowAgg {
	out := rowAgg{minPos: noMinWeight}
	for _, a := range n.aggs {
		out.total += a.total
		out.posCount += a.posCount
		if a.minPos < out.minPos {
			out.minPos = a.minPos
		}
	}
	return out
}

func (n *rowNode) childIndexFor(name string) int {
	lo, hi := 0, len(n.seps)
	for lo < hi {
		mid := (lo + hi) / 2
		if name >= n.seps[mid] {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func rowLowerBound(l *rowLeaf, name string) int {
	lo, hi := 0, len(l.names)
	for lo < hi {
		mid := (lo + hi) / 2
		if l.names[mid] < name {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// rowFind는 이름 하나의 슬롯과 무게입니다. 앞 64바이트를 공유하는 253바이트 이름도
// **원문 전체**로 비교하므로 정확히 갈립니다.
func rowFind(root *rowNode, name string) (slot uint32, weight uint8, ok bool) {
	n := root
	for n != nil {
		i := n.childIndexFor(name)
		if n.leafLevel {
			if i >= len(n.leafKids) {
				return 0, 0, false
			}
			l := n.leafKids[i]
			at := rowLowerBound(l, name)
			if at < len(l.names) && l.names[at] == name {
				return l.slots[at], l.weights[at], true
			}
			return 0, 0, false
		}
		if i >= len(n.nodeKids) {
			return 0, 0, false
		}
		n = n.nodeKids[i]
	}
	return 0, 0, false
}

// rowOp는 행 디렉터리 배치 연산입니다.
type rowOp struct {
	name   string
	slot   uint32
	weight uint8
	remove bool
}

// rowStats는 행 디렉터리 적용의 복사량입니다.
type rowStats struct {
	leafEntriesCopied int64
	nodesCopied       int64
	sepBytes          int64
	rowDelta          int64
	leafDelta         int64
	// capBytesDelta는 리프 실제 용량의 증감입니다(postStats와 같은 이유).
	capBytesDelta int64
}

const (
	// rowEntryBytes는 행 디렉터리 항목 하나의 용량 비용입니다(name·slot·weight).
	rowEntryBytes = stringHeaderBytes + 4 + 1
	// rowLeafHeaderBytes는 리프 하나의 고정 비용입니다(슬라이스 헤더 셋 + 그룹 요약).
	rowLeafHeaderBytes = 24*3 + rowGroups*3
)

// rowLeafCapOf는 리프 하나가 붙잡고 있는 용량 바이트입니다.
func rowLeafCapOf(l *rowLeaf) int64 {
	if l == nil {
		return 0
	}
	return int64(cap(l.names))*stringHeaderBytes + int64(cap(l.slots))*4 + int64(cap(l.weights))
}

// rowApply는 이름 오름차순 ops를 경로 복사로 적용합니다.
func rowApply(root *rowNode, ops []rowOp, st *rowStats) *rowNode {
	if len(ops) == 0 {
		return root
	}
	next := rowApplyNode(root, ops, st)
	for !next.leafLevel && len(next.nodeKids) == 1 {
		next = next.nodeKids[0]
	}
	return next
}

func rowApplyNode(n *rowNode, ops []rowOp, st *rowStats) *rowNode {
	st.nodesCopied++
	out := &rowNode{leafLevel: n.leafLevel}
	out.seps = append(out.seps, n.seps...)
	out.aggs = append(out.aggs, n.aggs...)
	if n.leafLevel {
		out.leafKids = append(out.leafKids, n.leafKids...)
	} else {
		out.nodeKids = append(out.nodeKids, n.nodeKids...)
	}

	type chunk struct{ child, lo, hi int }
	chunks := make([]chunk, 0, 8)
	i := 0
	for i < len(ops) {
		c := out.childIndexFor(ops[i].name)
		j := i + 1
		for j < len(ops) && out.childIndexFor(ops[j].name) == c {
			j++
		}
		chunks = append(chunks, chunk{child: c, lo: i, hi: j})
		i = j
	}
	for k := len(chunks) - 1; k >= 0; k-- {
		ch := chunks[k]
		if out.leafLevel {
			rowApplyToLeaf(out, ch.child, ops[ch.lo:ch.hi], st)
			continue
		}
		out.nodeKids[ch.child] = rowApplyNode(out.nodeKids[ch.child], ops[ch.lo:ch.hi], st)
		out.aggs[ch.child] = out.nodeKids[ch.child].agg()
		rowRebalanceNode(out, ch.child, st)
	}
	return out
}

func rowApplyToLeaf(parent *rowNode, at int, ops []rowOp, st *rowStats) {
	old := parent.leafKids[at]
	oldCap := rowLeafCapOf(old)
	names := make([]string, 0, len(old.names)+len(ops))
	slots := make([]uint32, 0, len(old.slots)+len(ops))
	weights := make([]uint8, 0, len(old.weights)+len(ops))
	oi, opi := 0, 0
	for oi < len(old.names) || opi < len(ops) {
		switch {
		case oi >= len(old.names):
			if !ops[opi].remove {
				names = append(names, ops[opi].name)
				slots = append(slots, ops[opi].slot)
				weights = append(weights, ops[opi].weight)
			}
			opi++
		case opi >= len(ops):
			names = append(names, old.names[oi])
			slots = append(slots, old.slots[oi])
			weights = append(weights, old.weights[oi])
			oi++
		case old.names[oi] < ops[opi].name:
			names = append(names, old.names[oi])
			slots = append(slots, old.slots[oi])
			weights = append(weights, old.weights[oi])
			oi++
		case old.names[oi] > ops[opi].name:
			if !ops[opi].remove {
				names = append(names, ops[opi].name)
				slots = append(slots, ops[opi].slot)
				weights = append(weights, ops[opi].weight)
			}
			opi++
		default:
			if !ops[opi].remove {
				names = append(names, ops[opi].name)
				slots = append(slots, ops[opi].slot)
				weights = append(weights, ops[opi].weight)
			}
			oi++
			opi++
		}
	}
	st.leafEntriesCopied += int64(len(old.names) + len(names))
	st.rowDelta += int64(len(names)) - int64(len(old.names))

	if len(names) <= rowLeafMax {
		l := &rowLeaf{names: names, slots: slots, weights: weights}
		l.finalize()
		parent.leafKids[at] = l
		parent.aggs[at] = l.agg()
		st.capBytesDelta += rowLeafCapOf(l) - oldCap
		rowRebalanceLeaf(parent, at, st)
		return
	}
	parts := make([]*rowLeaf, 0, len(names)/rowLeafSplit+1)
	for lo := 0; lo < len(names); lo += rowLeafSplit {
		hi := lo + rowLeafSplit
		if hi > len(names) {
			hi = len(names)
		}
		l := &rowLeaf{
			names:   append([]string(nil), names[lo:hi]...),
			slots:   append([]uint32(nil), slots[lo:hi]...),
			weights: append([]uint8(nil), weights[lo:hi]...),
		}
		l.finalize()
		parts = append(parts, l)
	}
	newCap := int64(0)
	for _, p := range parts {
		newCap += rowLeafCapOf(p)
	}
	st.capBytesDelta += newCap - oldCap
	st.leafDelta += int64(len(parts)) - 1
	newSeps := make([]string, 0, len(parent.seps)+len(parts)-1)
	newSeps = append(newSeps, parent.seps[:at]...)
	for p := 1; p < len(parts); p++ {
		fence := strings.Clone(parts[p].names[0])
		st.sepBytes += int64(len(fence)) + stringHeaderBytes
		newSeps = append(newSeps, fence)
	}
	newSeps = append(newSeps, parent.seps[at:]...)
	newKids := make([]*rowLeaf, 0, len(parent.leafKids)+len(parts)-1)
	newKids = append(newKids, parent.leafKids[:at]...)
	newKids = append(newKids, parts...)
	newKids = append(newKids, parent.leafKids[at+1:]...)
	newAggs := make([]rowAgg, 0, len(newKids))
	newAggs = append(newAggs, parent.aggs[:at]...)
	for _, p := range parts {
		newAggs = append(newAggs, p.agg())
	}
	newAggs = append(newAggs, parent.aggs[at+1:]...)
	parent.seps, parent.leafKids, parent.aggs = newSeps, newKids, newAggs
}

func rowRebalanceLeaf(parent *rowNode, at int, st *rowStats) {
	if len(parent.leafKids) <= 1 {
		return
	}
	if len(parent.leafKids[at].names) >= rowLeafMin {
		return
	}
	right := at + 1
	left := at
	if right >= len(parent.leafKids) {
		right = at
		left = at - 1
	}
	a, b := parent.leafKids[left], parent.leafKids[right]
	total := len(a.names) + len(b.names)
	oldCap := rowLeafCapOf(a) + rowLeafCapOf(b)
	st.leafEntriesCopied += int64(total)
	names := make([]string, 0, total)
	slots := make([]uint32, 0, total)
	weights := make([]uint8, 0, total)
	names = append(append(names, a.names...), b.names...)
	slots = append(append(slots, a.slots...), b.slots...)
	weights = append(append(weights, a.weights...), b.weights...)

	if total <= rowLeafMax {
		l := &rowLeaf{names: names, slots: slots, weights: weights}
		l.finalize()
		kids := make([]*rowLeaf, 0, len(parent.leafKids)-1)
		kids = append(kids, parent.leafKids[:left]...)
		kids = append(kids, l)
		kids = append(kids, parent.leafKids[right+1:]...)
		aggs := make([]rowAgg, 0, len(kids))
		aggs = append(aggs, parent.aggs[:left]...)
		aggs = append(aggs, l.agg())
		aggs = append(aggs, parent.aggs[right+1:]...)
		seps := make([]string, 0, len(parent.seps)-1)
		seps = append(seps, parent.seps[:left]...)
		seps = append(seps, parent.seps[right:]...)
		parent.leafKids, parent.aggs, parent.seps = kids, aggs, seps
		st.capBytesDelta += rowLeafCapOf(l) - oldCap
		st.leafDelta--
		return
	}
	half := total / 2
	first := &rowLeaf{
		names:   append([]string(nil), names[:half]...),
		slots:   append([]uint32(nil), slots[:half]...),
		weights: append([]uint8(nil), weights[:half]...),
	}
	second := &rowLeaf{
		names:   append([]string(nil), names[half:]...),
		slots:   append([]uint32(nil), slots[half:]...),
		weights: append([]uint8(nil), weights[half:]...),
	}
	first.finalize()
	second.finalize()
	parent.leafKids[left], parent.leafKids[right] = first, second
	parent.aggs[left], parent.aggs[right] = first.agg(), second.agg()
	st.capBytesDelta += rowLeafCapOf(first) + rowLeafCapOf(second) - oldCap
	fence := strings.Clone(second.names[0])
	st.sepBytes += int64(len(fence)) + stringHeaderBytes
	parent.seps[left] = fence
}

func rowRebalanceNode(parent *rowNode, at int, st *rowStats) {
	if len(parent.nodeKids) <= 1 {
		return
	}
	child := parent.nodeKids[at]
	if child.childCount() >= treeNodeMin {
		if child.childCount() <= treeFanout {
			return
		}
		rowSplitNode(parent, at, st)
		return
	}
	right := at + 1
	left := at
	if right >= len(parent.nodeKids) {
		right = at
		left = at - 1
	}
	a, b := parent.nodeKids[left], parent.nodeKids[right]
	if a.leafLevel != b.leafLevel {
		return
	}
	st.nodesCopied++
	joined := &rowNode{leafLevel: a.leafLevel}
	joined.seps = append(joined.seps, a.seps...)
	joined.seps = append(joined.seps, parent.seps[left])
	joined.seps = append(joined.seps, b.seps...)
	joined.aggs = append(joined.aggs, a.aggs...)
	joined.aggs = append(joined.aggs, b.aggs...)
	if a.leafLevel {
		joined.leafKids = append(joined.leafKids, a.leafKids...)
		joined.leafKids = append(joined.leafKids, b.leafKids...)
	} else {
		joined.nodeKids = append(joined.nodeKids, a.nodeKids...)
		joined.nodeKids = append(joined.nodeKids, b.nodeKids...)
	}
	kids := make([]*rowNode, 0, len(parent.nodeKids)-1)
	kids = append(kids, parent.nodeKids[:left]...)
	kids = append(kids, joined)
	kids = append(kids, parent.nodeKids[right+1:]...)
	aggs := make([]rowAgg, 0, len(kids))
	aggs = append(aggs, parent.aggs[:left]...)
	aggs = append(aggs, joined.agg())
	aggs = append(aggs, parent.aggs[right+1:]...)
	seps := make([]string, 0, len(parent.seps)-1)
	seps = append(seps, parent.seps[:left]...)
	seps = append(seps, parent.seps[right:]...)
	parent.nodeKids, parent.aggs, parent.seps = kids, aggs, seps
	if joined.childCount() > treeFanout {
		rowSplitNode(parent, left, st)
	}
}

func rowSplitNode(parent *rowNode, at int, st *rowStats) {
	child := parent.nodeKids[at]
	count := child.childCount()
	if count <= treeFanout {
		return
	}
	half := count / 2
	st.nodesCopied += 2
	left := &rowNode{leafLevel: child.leafLevel}
	right := &rowNode{leafLevel: child.leafLevel}
	left.seps = append(left.seps, child.seps[:half-1]...)
	up := child.seps[half-1]
	right.seps = append(right.seps, child.seps[half:]...)
	left.aggs = append(left.aggs, child.aggs[:half]...)
	right.aggs = append(right.aggs, child.aggs[half:]...)
	if child.leafLevel {
		left.leafKids = append(left.leafKids, child.leafKids[:half]...)
		right.leafKids = append(right.leafKids, child.leafKids[half:]...)
	} else {
		left.nodeKids = append(left.nodeKids, child.nodeKids[:half]...)
		right.nodeKids = append(right.nodeKids, child.nodeKids[half:]...)
	}
	kids := make([]*rowNode, 0, len(parent.nodeKids)+1)
	kids = append(kids, parent.nodeKids[:at]...)
	kids = append(kids, left, right)
	kids = append(kids, parent.nodeKids[at+1:]...)
	aggs := make([]rowAgg, 0, len(kids))
	aggs = append(aggs, parent.aggs[:at]...)
	aggs = append(aggs, left.agg(), right.agg())
	aggs = append(aggs, parent.aggs[at+1:]...)
	seps := make([]string, 0, len(parent.seps)+1)
	seps = append(seps, parent.seps[:at]...)
	seps = append(seps, up)
	seps = append(seps, parent.seps[at:]...)
	parent.nodeKids, parent.aggs, parent.seps = kids, aggs, seps
}

// balanceRowRoot는 루트의 자식 수가 fanout을 넘으면 한 단계씩 위로 접습니다.
// 이유는 balancePostRoot와 같습니다(벌크 적재).
func balanceRowRoot(root *rowNode, st *rowStats) *rowNode {
	for root.childCount() > treeFanout {
		const group = treeFanout / 2
		count := root.childCount()
		levels := (count + group - 1) / group
		parent := &rowNode{leafLevel: false}
		st.nodesCopied += int64(levels + 1)
		for g := 0; g < levels; g++ {
			lo := g * group
			hi := lo + group
			if hi > count {
				hi = count
			}
			node := &rowNode{leafLevel: root.leafLevel}
			node.seps = append(node.seps, root.seps[lo:hi-1]...)
			node.aggs = append(node.aggs, root.aggs[lo:hi]...)
			if root.leafLevel {
				node.leafKids = append(node.leafKids, root.leafKids[lo:hi]...)
			} else {
				node.nodeKids = append(node.nodeKids, root.nodeKids[lo:hi]...)
			}
			if g > 0 {
				parent.seps = append(parent.seps, root.seps[lo-1])
			}
			parent.nodeKids = append(parent.nodeKids, node)
			parent.aggs = append(parent.aggs, node.agg())
		}
		root = parent
	}
	return root
}

/* ── fold 질의: 접두사 경계와 다음 자격 행 ────────────────────────────────── */

// rowBoundary는 접두사 무게 합이 처음 cap을 넘는 지점입니다.
//
//	acc      경계 **직전까지의** 무게 합 (= 부트스트랩 fold의 A)
//	name     경계 행의 이름(=처음 넘긴 행). overflow가 false면 의미 없음
//	weight   그 행의 무게
//	visited  이 판정이 실제로 본 행 레코드 수(계측)
type rowBoundary struct {
	acc      int64
	name     string
	slot     uint32
	weight   uint8
	overflow bool
	visited  int64
	compares int64
}

// rowPrefixBoundary는 서브트리 무게 합으로 하강해 경계를 O(log R)에 찾습니다.
func rowPrefixBoundary(root *rowNode, capTokens int64) rowBoundary {
	var out rowBoundary
	n := root
	for n != nil {
		if n.leafLevel {
			for ci, l := range n.leafKids {
				a := n.aggs[ci]
				out.compares++
				if out.acc+a.total <= capTokens {
					out.acc += a.total
					continue
				}
				// 이 리프 안에 경계가 있습니다. 그룹 요약으로 좁힙니다.
				at := 0
				for g := 0; g < rowGroups; g++ {
					out.compares++
					if l.groupWeight[g] == 0 {
						continue
					}
					if out.acc+int64(l.groupWeight[g]) <= capTokens {
						out.acc += int64(l.groupWeight[g])
						at = (g + 1) * rowGroupSize
						continue
					}
					at = g * rowGroupSize
					break
				}
				for i := at; i < len(l.weights); i++ {
					out.compares++
					w := l.weights[i]
					if w == 0 {
						continue
					}
					out.visited++
					if out.acc+int64(w) > capTokens {
						out.name, out.slot, out.weight, out.overflow = l.names[i], l.slots[i], w, true
						return out
					}
					out.acc += int64(w)
				}
				return out
			}
			return out
		}
		var chosen *rowNode
		for ci, child := range n.nodeKids {
			a := n.aggs[ci]
			out.compares++
			if out.acc+a.total <= capTokens {
				out.acc += a.total
				continue
			}
			chosen = child
			break
		}
		if chosen == nil {
			return out
		}
		n = chosen
	}
	return out
}

// rowNextFitting은 afterName **보다 큰** 이름 중 1 <= weight <= budget인 첫 행입니다.
// minPos 집계 덕분에 자격 없는 서브트리는 아예 내려가지 않습니다.
type rowHit struct {
	name     string
	slot     uint32
	weight   uint8
	found    bool
	visited  int64
	compares int64
}

func rowNextFitting(root *rowNode, afterName string, budget uint8, hasAfter bool) rowHit {
	var out rowHit
	var walk func(n *rowNode) bool
	walk = func(n *rowNode) bool {
		if n == nil {
			return false
		}
		if n.leafLevel {
			for ci, l := range n.leafKids {
				a := n.aggs[ci]
				out.compares++
				if a.posCount == 0 || a.minPos > budget {
					continue
				}
				start := 0
				if hasAfter {
					start = rowLowerBound(l, afterName)
					if start < len(l.names) && l.names[start] == afterName {
						start++
					}
				}
				if start >= len(l.names) {
					continue
				}
				for g := start / rowGroupSize; g < rowGroups; g++ {
					out.compares++
					if l.groupMinPos[g] > budget {
						continue
					}
					lo := g * rowGroupSize
					if lo < start {
						lo = start
					}
					hi := (g + 1) * rowGroupSize
					if hi > len(l.weights) {
						hi = len(l.weights)
					}
					for i := lo; i < hi; i++ {
						out.compares++
						w := l.weights[i]
						if w == 0 || w > budget {
							continue
						}
						out.visited++
						out.name, out.slot, out.weight, out.found = l.names[i], l.slots[i], w, true
						return true
					}
				}
			}
			return false
		}
		for ci, child := range n.nodeKids {
			a := n.aggs[ci]
			out.compares++
			if a.posCount == 0 || a.minPos > budget {
				continue
			}
			if walk(child) {
				return true
			}
		}
		return false
	}
	walk(root)
	return out
}

// rowEachPositive는 [loName, hiName) 구간의 양수 무게 행을 이름 순으로 넘깁니다.
// posCount가 0인 서브트리는 내려가지 않습니다.
func rowEachPositive(root *rowNode, loName, hiName string, hasHi bool, fn func(name string, slot uint32, weight uint8) bool) int64 {
	var visited int64
	var walk func(n *rowNode) bool
	walk = func(n *rowNode) bool {
		if n == nil {
			return true
		}
		if n.leafLevel {
			for ci, l := range n.leafKids {
				if n.aggs[ci].posCount == 0 {
					continue
				}
				start := rowLowerBound(l, loName)
				for i := start; i < len(l.names); i++ {
					if hasHi && l.names[i] >= hiName {
						return false
					}
					if l.weights[i] == 0 {
						continue
					}
					visited++
					if !fn(l.names[i], l.slots[i], l.weights[i]) {
						return false
					}
				}
			}
			return true
		}
		for ci, child := range n.nodeKids {
			if n.aggs[ci].posCount == 0 {
				continue
			}
			if !walk(child) {
				return false
			}
		}
		return true
	}
	walk(root)
	return visited
}

// rowTreeBytes는 행 디렉터리가 붙잡는 바이트입니다.
func rowTreeBytes(n *rowNode) int64 {
	if n == nil {
		return 0
	}
	total := int64(24 * 4)
	for _, s := range n.seps {
		total += int64(len(s)) + stringHeaderBytes
	}
	total += int64(cap(n.seps)-len(n.seps)) * stringHeaderBytes
	total += int64(cap(n.aggs)) * rowAggBytes
	if n.leafLevel {
		total += int64(cap(n.leafKids)) * 8
		for _, l := range n.leafKids {
			total += 24*3 + rowGroups*3
			total += int64(cap(l.names)) * stringHeaderBytes
			total += int64(cap(l.slots)) * 4
			total += int64(cap(l.weights))
		}
		return total
	}
	total += int64(cap(n.nodeKids)) * 8
	for _, c := range n.nodeKids {
		total += rowTreeBytes(c)
	}
	return total
}

// rowTreeCount는 행 수입니다(테스트 검증용).
func rowTreeCount(n *rowNode) int {
	if n == nil {
		return 0
	}
	if n.leafLevel {
		total := 0
		for _, l := range n.leafKids {
			total += len(l.names)
		}
		return total
	}
	total := 0
	for _, c := range n.nodeKids {
		total += rowTreeCount(c)
	}
	return total
}
