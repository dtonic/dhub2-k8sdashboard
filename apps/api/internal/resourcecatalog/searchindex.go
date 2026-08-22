package resourcecatalog

// 증분 검색 인덱스 — Round 6 v4 / v4.1
// --------------------------------------------------------------------------
// searchIndex는 GVR 하나의 검색 인덱스이고, 교체 단위는 **파티션(nsPart)** 이며
// 파티션 내부는 지속 트리라 이벤트 하나가 경로만 복사합니다. (searchtree.go)
//
// namespace label 상한 fold는 부트스트랩과 **바이트 단위로 같은 규칙**입니다.
// 이름 순으로 접으며 `acc + w <= cap`이면 포함하고 아니면 그 행의 label만 뺍니다.
// 이 fold의 결과는
//
//	포함 = [처음 ~ 경계) 의 모든 행  ∪  S
//	S    = 경계 이후에서 budget(=cap-acc) 안에 들어가는 행들의 greedy 열, |S| <= 31
//
// 로 완전히 특징지어집니다(정리 2: 첫 생략에서 remaining < w_b <= 32 이므로 <= 31).
// 그래서 이벤트 하나가 바꾸는 것은 **경계 대칭차(<=63 토큰)** 와 S_old/S_new(각 <=31)
// 뿐이고, 파티션 전체를 다시 접을 필요가 없습니다.

import (
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// 진단 비트입니다. nsAgg가 서브트리 OR로 들고 있어 페이지마다 O(1)에 읽힙니다.
const (
	// nsReasonLabelNs는 이 파티션의 label 색인이 상한으로 잘렸다는 뜻입니다.
	nsReasonLabelNs uint16 = 1 << iota
	// nsReasonStale은 드롭된 이벤트 때문에 이 파티션을 믿을 수 없다는 뜻입니다.
	nsReasonStale
)

// sentinelAfterName은 어떤 유효한 Kubernetes 이름보다도 큰 경계값입니다.
// 이름은 RFC1123(소문자 영숫자·'-'·'.')이라 0x7f가 나올 수 없습니다.
const sentinelAfterName = "\x7f"

// maxSuffixRows는 S 집합의 상한입니다(정리 2).
const maxSuffixRows = 31

/* ── namespace 디렉터리 ──────────────────────────────────────────────────── */

// nsAgg는 서브트리 집계입니다. unsafe.Sizeof = 16.
//
// staleCount를 따로 두지 않습니다 — stale 여부는 reasonMask의 비트입니다.
// (v4.1: 필드를 그대로 두면 18바이트가 24로 정렬되어 낭비입니다.)
type nsAgg struct {
	oldestUpdated int64  // 8  unix nano, 서브트리 최솟값 (0이면 없음)
	nsCount       int32  // 4
	reasonMask    uint16 // 2
}

const nsAggBytes = 16

func (a *nsAgg) merge(b nsAgg) {
	a.nsCount += b.nsCount
	a.reasonMask |= b.reasonMask
	if b.oldestUpdated != 0 && (a.oldestUpdated == 0 || b.oldestUpdated < a.oldestUpdated) {
		a.oldestUpdated = b.oldestUpdated
	}
}

// nsPageSize는 페이지 하나가 담는 파티션 수입니다.
// 파티션 하나를 교체하면 그 페이지(<=128 포인터)와 페이지 디렉터리(N/64)만 복사합니다.
const (
	nsPageSize = 64
	nsPageMax  = 128
)

// nsPage는 정렬된 파티션 묶음입니다. 불변입니다.
type nsPage struct {
	names []string
	parts []*nsPart
	agg   nsAgg
}

func (p *nsPage) recompute() {
	p.agg = nsAgg{}
	for _, part := range p.parts {
		p.agg.merge(part.agg())
	}
}

// nsDir는 페이지 디렉터리입니다. 루트 집계를 유지하므로 All 진단이 O(1)입니다.
//
// total은 파티션 보유 바이트의 합을 **증분으로** 유지합니다. 갱신마다 전체를
// 다시 합치면 파티션 수에 비례하는 비용이 이벤트마다 붙습니다.
type nsDir struct {
	pages []*nsPage
	first []string // 각 페이지의 첫 namespace
	agg   nsAgg
	total int64
}

func newNsDir() *nsDir {
	return &nsDir{pages: []*nsPage{{}}, first: []string{""}}
}

func (d *nsDir) pageIndexFor(ns string) int {
	// first[0]은 의미가 없습니다(첫 페이지는 언제나 하한).
	lo, hi := 1, len(d.first)
	for lo < hi {
		mid := (lo + hi) / 2
		if ns >= d.first[mid] {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo - 1
}

func (d *nsDir) find(ns string) *nsPart {
	if len(d.pages) == 0 {
		return nil
	}
	p := d.pages[d.pageIndexFor(ns)]
	i := sort.SearchStrings(p.names, ns)
	if i < len(p.names) && p.names[i] == ns {
		return p.parts[i]
	}
	return nil
}

// each는 namespace 사전순으로 파티션을 넘깁니다.
func (d *nsDir) each(fn func(part *nsPart) bool) {
	for _, p := range d.pages {
		for _, part := range p.parts {
			if !fn(part) {
				return
			}
		}
	}
}

func (d *nsDir) recomputeRoot() {
	d.agg = nsAgg{}
	for _, p := range d.pages {
		d.agg.merge(p.agg)
	}
}

// upsert는 파티션 하나를 교체(또는 삽입)한 **새 디렉터리**입니다.
// 손대지 않은 페이지는 그대로 공유합니다.
func (d *nsDir) upsert(part *nsPart, copied *int64) *nsDir {
	pi := d.pageIndexFor(part.namespace)
	old := d.pages[pi]
	i := sort.SearchStrings(old.names, part.namespace)

	totalDelta := part.bytes
	page := &nsPage{}
	if i < len(old.names) && old.names[i] == part.namespace {
		totalDelta -= old.parts[i].bytes
		page.names = append(page.names, old.names...)
		page.parts = append(page.parts, old.parts...)
		page.parts[i] = part
	} else {
		page.names = make([]string, 0, len(old.names)+1)
		page.parts = make([]*nsPart, 0, len(old.parts)+1)
		page.names = append(page.names, old.names[:i]...)
		page.names = append(page.names, part.namespace)
		page.names = append(page.names, old.names[i:]...)
		page.parts = append(page.parts, old.parts[:i]...)
		page.parts = append(page.parts, part)
		page.parts = append(page.parts, old.parts[i:]...)
	}
	page.recompute()
	if copied != nil {
		*copied += int64(len(page.names))*(stringHeaderBytes+8) + nsAggBytes
	}

	next := &nsDir{total: d.total + totalDelta}
	next.pages = append(next.pages, d.pages...)
	next.first = append(next.first, d.first...)
	next.pages[pi] = page

	if len(page.names) > nsPageMax {
		next.splitPage(pi)
	} else if len(page.names) > 0 {
		next.first[pi] = page.names[0]
		if pi == 0 {
			next.first[0] = ""
		}
	}
	next.recomputeRoot()
	if copied != nil {
		*copied += int64(len(next.pages)) * (8 + stringHeaderBytes)
	}
	return next
}

func (d *nsDir) splitPage(pi int) {
	page := d.pages[pi]
	half := len(page.names) / 2
	left := &nsPage{
		names: append([]string(nil), page.names[:half]...),
		parts: append([]*nsPart(nil), page.parts[:half]...),
	}
	right := &nsPage{
		names: append([]string(nil), page.names[half:]...),
		parts: append([]*nsPart(nil), page.parts[half:]...),
	}
	left.recompute()
	right.recompute()
	pages := make([]*nsPage, 0, len(d.pages)+1)
	pages = append(pages, d.pages[:pi]...)
	pages = append(pages, left, right)
	pages = append(pages, d.pages[pi+1:]...)
	first := make([]string, 0, len(d.first)+1)
	first = append(first, d.first[:pi]...)
	first = append(first, left.names[0], right.names[0])
	first = append(first, d.first[pi+1:]...)
	if pi == 0 {
		first[0] = ""
	}
	d.pages, d.first = pages, first
}

// bytes는 O(1)입니다 — 파티션 합은 total이 증분으로 들고 있고, 구조 비용은
// 페이지 수와 파티션 수만으로 계산됩니다.
func (d *nsDir) bytes() int64 {
	pages := int64(len(d.pages))
	entries := int64(d.agg.nsCount)
	structural := int64(24*2 + nsAggBytes)
	structural += pages * (24*2 + nsAggBytes + 8 + stringHeaderBytes)
	structural += entries * (stringHeaderBytes + 8)
	return structural + d.total
}

// recomputeTotal은 total을 다시 합칩니다(검증 경로 전용).
func (d *nsDir) recomputeTotal() int64 {
	var total int64
	for _, p := range d.pages {
		for _, part := range p.parts {
			total += part.bytes
		}
	}
	return total
}

/* ── 파티션 ─────────────────────────────────────────────────────────────── */

// rowRef는 S 집합이 기억하는 행 하나입니다.
type rowRef struct {
	name string
	slot uint32
}

// nsPart는 (GVR, namespace) 파티션입니다. 전부 불변이고 교체 단위입니다.
type nsPart struct {
	namespace string
	// nsTok은 namespace의 정규 토큰입니다. 키 유도와 색인 생성이 **같은 값**을 써야
	// 삭제 연산이 기존 항목과 정확히 맞아떨어집니다.
	nsTok   string
	kindTok string
	// updatedAt은 **이 파티션의** 마지막 적용 시각입니다(unix nano).
	updatedAt int64

	rowRoot  *trieBranch
	postRoot *postNode
	rowDir   *rowNode
	freeTop  *freeNode

	slotHigh uint32
	liveRows int32

	// fold 상태 — 부트스트랩 fold와 정확히 같은 결과를 재현합니다.
	nsLabelTok int64
	boundName  string
	boundValid bool // 경계 행이 존재하는지(=상한을 넘겼는지)
	suffix     []rowRef
	truncRows  int32

	reasonMask  uint16
	partVersion uint64

	postEntries int64
	bytes       int64
}

const (
	// nsPartStructBytes는 파티션 구조체 자체의 크기입니다.
	nsPartStructBytes = 224
	// nsPartFixedBytes는 **추정용** 고정 비용입니다. 구조체에 더해 반드시 생기는
	// 트라이 척추(브랜치 4단계)를 미리 얹습니다 — 추정은 트라이를 걸어볼 수
	// 없으므로 이 몫을 상수로 들고 있어야 과소 계상이 되지 않습니다.
	//
	// recomputeBytes는 트라이를 직접 걸어 세므로 **이 상수를 쓰지 않습니다.**
	// 둘 다 쓰면 같은 바이트를 두 번 세게 됩니다.
	nsPartFixedBytes = nsPartStructBytes + trieSpineBytes
)

func (p *nsPart) agg() nsAgg {
	return nsAgg{oldestUpdated: p.updatedAt, nsCount: 1, reasonMask: p.reasonMask}
}

func (p *nsPart) keyer() postKeyer {
	return postKeyer{root: p.rowRoot, namespace: p.nsTok, kindTok: p.kindTok}
}

// includedByFold는 fold가 이 이름의 label을 색인했는지입니다.
func (p *nsPart) includedByFold(name string) bool {
	if !p.boundValid || name < p.boundName {
		return true
	}
	for _, r := range p.suffix {
		if r.name == name {
			return true
		}
	}
	return false
}

// recomputeBytes는 파티션 보유 바이트를 다시 잽니다.
// 증분 회계와 대조하는 검증 경로에서도 씁니다.
func (p *nsPart) recomputeBytes() int64 {
	// 트라이는 아래에서 직접 걸어 세므로 척추 상수를 더하지 않습니다.
	total := int64(nsPartStructBytes)
	total += int64(len(p.namespace)) + stringHeaderBytes
	total += postTreeBytes(p.postRoot)
	total += rowTreeBytes(p.rowDir)
	total += trieNodeBytes(p.rowRoot)
	total += freeListBytes(p.freeTop)
	total += int64(cap(p.suffix)) * (stringHeaderBytes + 4)
	total += int64(len(p.boundName)) + stringHeaderBytes
	// 행 레코드 자체.
	rowEachAll(p.rowDir, func(_ string, slot uint32) bool {
		total += trieGet(p.rowRoot, slot).retainedBytes()
		return true
	})
	return total
}

// rowEachAll은 모든 행을 이름 순으로 넘깁니다(회계·검증용).
func rowEachAll(n *rowNode, fn func(name string, slot uint32) bool) {
	if n == nil {
		return
	}
	if n.leafLevel {
		for _, l := range n.leafKids {
			for i := range l.names {
				if !fn(l.names[i], l.slots[i]) {
					return
				}
			}
		}
		return
	}
	for _, c := range n.nodeKids {
		rowEachAll(c, fn)
	}
}

/* ── 행 입력과 정규 토큰 ─────────────────────────────────────────────────── */

// rowInput은 델타·부트스트랩이 넘기는 행 하나입니다.
// labels는 이미 정규화·정렬·MaxLabelKeysPerObject 적용이 끝난 토큰 목록입니다.
type rowInput struct {
	name          string
	uid           string
	labels        []string
	keysTruncated bool
}

func (in rowInput) record() *rowRecord {
	rec := &rowRecord{
		name:      in.name,
		uid:       in.uid,
		nameTok:   normalizeToken(in.name),
		labelBlob: encodeLabelBlob(in.labels),
		weight:    uint16(len(in.labels)),
	}
	if in.keysTruncated {
		rec.flags |= rowFlagKeysTruncated
	}
	return rec
}

// labelTokensOf는 행 하나의 정규 label 토큰을 (정렬된 키 순서로) 만듭니다.
// 색인 빌드와 순회가 같은 규칙을 쓰도록 sortedLabelKeys/labelTokenCount와 짝을 맞춥니다.
func labelTokensOf(row *indexRow, buf []string, out []string) ([]string, bool, []string) {
	out = out[:0]
	if row.obj == nil || len(row.obj.Labels) == 0 {
		return out, false, buf
	}
	keys, truncated := sortedLabelKeys(row, buf)
	for _, k := range keys {
		out = append(out, normalizeToken(k))
		if v := row.obj.Labels[k]; v != "" {
			out = append(out, normalizeToken(v))
		}
	}
	return out, truncated, keys[:0]
}

// tokenSlot은 정규화 전의 (토큰, 필드, tokIdx) 하나입니다.
type tokenSlot struct {
	tok    string
	field  uint8
	tokIdx uint16
}

// canonicalTokens는 행 하나의 **중복 제거된 오름차순** 토큰 목록입니다.
//
// 같은 토큰이 여러 필드에서 나오면 name < namespace < kind < label 순으로
// **가장 우선순위 높은 필드 하나만** 남깁니다. 그래서 posting은 토큰당 하나이고
// prevLCP가 잘 정의됩니다.
// nsTok은 **이미 정규화된** namespace 토큰이어야 합니다(nsPart.nsTok).
func canonicalTokens(rec *rowRecord, nsTok, kindTok string, namespaced, withLabels bool, buf []tokenSlot) []tokenSlot {
	buf = buf[:0]
	if rec.nameTok != "" && safeToken(rec.nameTok) {
		buf = append(buf, tokenSlot{tok: rec.nameTok, field: uint8(fieldName), tokIdx: 0})
	}
	if namespaced && nsTok != "" && safeToken(nsTok) {
		buf = append(buf, tokenSlot{tok: nsTok, field: uint8(fieldNamespace), tokIdx: 1})
	}
	if kindTok != "" && safeToken(kindTok) {
		buf = append(buf, tokenSlot{tok: kindTok, field: uint8(fieldKind), tokIdx: 2})
	}
	if withLabels {
		rec.eachLabelToken(func(i int, tok string) bool {
			if safeToken(tok) {
				buf = append(buf, tokenSlot{tok: tok, field: uint8(fieldLabel), tokIdx: uint16(3 + i)})
			}
			return true
		})
	}
	sort.Slice(buf, func(i, j int) bool {
		if buf[i].tok != buf[j].tok {
			return buf[i].tok < buf[j].tok
		}
		return buf[i].field < buf[j].field
	})
	w := 0
	for i := range buf {
		if i > 0 && buf[i].tok == buf[w-1].tok {
			continue // 같은 토큰: 우선순위가 가장 높은 첫 항목만 남습니다.
		}
		buf[w] = buf[i]
		w++
	}
	return buf[:w]
}

// commonPrefixLen은 두 토큰의 공통 접두사 길이입니다(<= tokenPrefixBytes).
func commonPrefixLen(a, b string) uint8 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	if i > tokenPrefixBytes {
		i = tokenPrefixBytes
	}
	return uint8(i)
}

// buildRowOps는 행 하나의 posting 연산 목록을 만듭니다.
// remove가 true면 같은 목록을 삭제 연산으로 만듭니다.
func buildRowOps(rec *rowRecord, slot uint32, nsTok, kindTok string, namespaced, withLabels, remove bool,
	buf []tokenSlot, out []postOp) ([]tokenSlot, []postOp) {

	buf = canonicalTokens(rec, nsTok, kindTok, namespaced, withLabels, buf)
	for i, ts := range buf {
		var lcp uint8
		if i > 0 {
			lcp = commonPrefixLen(buf[i-1].tok, ts.tok)
		}
		out = append(out, postOp{
			key:    postKey{token: ts.tok, name: rec.name, uid: rec.uid},
			entry:  postEntry{slot: slot, tokIdx: ts.tokIdx, field: ts.field, prevLCP: lcp},
			remove: remove,
		})
	}
	return buf, out
}

// replaceRowOps는 label 멤버십이 뒤집힌 행의 **정규 posting 집합 전체**를 교체합니다.
//
// label 토큰만 넣고 빼면 안 됩니다. prevLCP는 그 행의 정규 토큰열 **전체**에 대해
// 정의되므로, label이 끼거나 빠지면 이름·namespace·kind 토큰의 prevLCP까지 달라집니다.
// 예를 들어 이름 "zz"에 label "za"가 붙으면 정규열은 [za, zz]가 되어 zz의 prevLCP가
// 0에서 1로 바뀝니다. 그 값을 갱신하지 않으면 질의 "z"에서 같은 행이 두 번 나갑니다.
//
// 그래서 옛 멤버십 기준 집합을 통째로 지우고 새 멤버십 기준 집합을 통째로 넣습니다.
// 같은 키가 양쪽에 있으면 정렬이 안정이라 **뒤의 삽입**이 남고, 그 값이 새 prevLCP입니다.
func replaceRowOps(rec *rowRecord, slot uint32, nsTok, kindTok string, namespaced, before, after bool,
	buf []tokenSlot, out []postOp) ([]tokenSlot, []postOp) {

	buf, out = buildRowOps(rec, slot, nsTok, kindTok, namespaced, before, true, buf, out)
	buf, out = buildRowOps(rec, slot, nsTok, kindTok, namespaced, after, false, buf, out)
	return buf, out
}

/* ── fold 재계산 ────────────────────────────────────────────────────────── */

// foldState는 한 번의 fold 결과입니다.
type foldState struct {
	acc        int64
	boundName  string
	boundValid bool
	suffix     []rowRef
	visited    int64
	compares   int64
}

// recomputeFold는 rowDir 집계로 부트스트랩 fold와 같은 결과를 O(log R)에 다시 냅니다.
func recomputeFold(dir *rowNode) foldState {
	var out foldState
	b := rowPrefixBoundary(dir, MaxLabelTokensPerNamespace)
	out.acc, out.visited, out.compares = b.acc, b.visited, b.compares
	if !b.overflow {
		return out
	}
	out.boundName, out.boundValid = b.name, true
	budget := MaxLabelTokensPerNamespace - out.acc
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

func (f foldState) includes(name string) bool {
	if !f.boundValid || name < f.boundName {
		return true
	}
	for _, r := range f.suffix {
		if r.name == name {
			return true
		}
	}
	return false
}

/* ── 파티션 배치 적용 ────────────────────────────────────────────────────── */

// partOp는 파티션 하나에 적용할 행 변경입니다. input이 nil이면 삭제입니다.
type partOp struct {
	name  string
	input *rowInput
}

// applyStats는 한 번의 적용이 만든 계측입니다.
type applyStats struct {
	visitedRows        int64
	directoryCopies    int64
	postingsChanged    int64
	identityLookups    int64
	foldCompares       int64
	nodesCopied        int64
	transientBytes     int64
	sepBytes           int64
	coalescedKeyCount  int64
	slotExhausted      bool
	postEntryDelta     int64
	compactionRequired bool
}

// applyPartOps는 정렬된 partOp를 적용한 **새 파티션**을 돌려줍니다.
//
// 순서가 중요합니다.
//  1. 새 행을 트라이에 넣습니다(새 슬롯). 옛 슬롯은 아직 비우지 않습니다 —
//     삭제 대상 posting의 논리 키를 유도해야 하기 때문입니다.
//  2. posting 연산(옛 행 전체 삭제 + 새 행 전체 삽입)을 모읍니다.
//  3. rowDir를 갱신하고 fold를 다시 계산합니다.
//  4. fold 멤버십이 뒤집힌 행의 **label 전용** posting을 더합니다.
//  5. postTree에 한 번에 적용합니다.
//  6. 옛 슬롯을 트라이에서 비우고 pendingFree에 넣습니다.
//  7. pendingFree를 **완전히 pop된** 사설 자유 목록 앞에 붙여 게시합니다.
func applyPartOps(old *nsPart, namespaced bool, now time.Time, ops []partOp, st *applyStats) *nsPart {
	if len(ops) == 0 {
		return old
	}
	next := *old
	next.partVersion = old.partVersion + 1
	next.updatedAt = now.UnixNano()

	free := newFreeStack(old.freeTop)
	var pendingFree []uint32
	var postOps []postOp
	var rowOps []rowOp
	var tokBuf []tokenSlot
	var truncDelta int32
	var liveDelta int32
	// rowBytesDelta는 행 레코드 자체(차용 문자열 포함)의 증감입니다.
	var rowBytesDelta int64
	var newSlots int64

	type touched struct {
		name    string
		oldSlot uint32
		oldRec  *rowRecord
		newSlot uint32
		newRec  *rowRecord
		hadRow  bool
		removed bool
	}
	changes := make([]touched, 0, len(ops))

	for _, op := range ops {
		st.identityLookups++
		oldSlot, oldWeight, had := rowFind(next.rowDir, op.name)
		_ = oldWeight
		t := touched{name: op.name, hadRow: had}
		if had {
			t.oldSlot = oldSlot
			t.oldRec = trieGet(next.rowRoot, oldSlot)
			st.visitedRows++
		}
		if op.input == nil {
			t.removed = true
			if had {
				liveDelta--
				rowBytesDelta -= t.oldRec.retainedBytes()
				if t.oldRec != nil && t.oldRec.flags&rowFlagKeysTruncated != 0 {
					truncDelta--
				}
				rowOps = append(rowOps, rowOp{name: op.name, remove: true})
			}
			changes = append(changes, t)
			continue
		}
		// upsert: 언제나 **새 슬롯**을 씁니다. 그래야 옛 posting의 키가 옛 레코드로
		// 유도되어 삭제가 정확히 맞아떨어집니다.
		slot, ok := free.pop()
		if !ok {
			if next.slotHigh >= maxSlot {
				st.slotExhausted = true
				st.compactionRequired = true
				return old
			}
			slot = next.slotHigh
			next.slotHigh++
			newSlots++
		}
		rec := op.input.record()
		next.rowRoot = trieSet(next.rowRoot, slot, rec, &st.transientBytes)
		t.newSlot, t.newRec = slot, rec
		rowBytesDelta += rec.retainedBytes()
		if !had {
			liveDelta++
		} else {
			rowBytesDelta -= t.oldRec.retainedBytes()
			if t.oldRec != nil && t.oldRec.flags&rowFlagKeysTruncated != 0 {
				truncDelta--
			}
		}
		if rec.flags&rowFlagKeysTruncated != 0 {
			truncDelta++
		}
		rowOps = append(rowOps, rowOp{name: op.name, slot: slot, weight: uint8(rec.weight)})
		changes = append(changes, t)
	}

	// 옛 행의 posting은 **옛 멤버십 그대로** 지웁니다.
	for _, t := range changes {
		if !t.hadRow || t.oldRec == nil {
			continue
		}
		withLabels := old.includedByFold(t.name)
		tokBuf, postOps = buildRowOps(t.oldRec, t.oldSlot, old.nsTok, old.kindTok,
			namespaced, withLabels, true, tokBuf, postOps)
	}

	// rowDir 갱신 → fold 재계산.
	var rst rowStats
	next.rowDir = rowApply(next.rowDir, sortRowOps(rowOps), &rst)
	next.rowDir = balanceRowRoot(next.rowDir, &rst)
	st.directoryCopies += rst.leafEntriesCopied
	st.nodesCopied += rst.nodesCopied
	st.sepBytes += rst.sepBytes

	fold := recomputeFold(next.rowDir)
	st.visitedRows += fold.visited
	st.foldCompares += fold.compares

	// 새 행의 posting은 **새 멤버십**으로 넣습니다.
	for _, t := range changes {
		if t.removed || t.newRec == nil {
			continue
		}
		tokBuf, postOps = buildRowOps(t.newRec, t.newSlot, old.nsTok, old.kindTok,
			namespaced, fold.includes(t.name), false, tokBuf, postOps)
	}

	// 멤버십이 뒤집힌 **다른** 행의 label 전용 posting.
	changedNames := make(map[string]struct{}, len(changes))
	for _, t := range changes {
		changedNames[t.name] = struct{}{}
	}
	loName, hiName, hasHi := foldDiffRange(old, fold)
	if loName != "" || hasHi || !old.boundValid || !fold.boundValid {
		st.visitedRows += rowEachPositive(next.rowDir, loName, hiName, hasHi,
			func(name string, slot uint32, _ uint8) bool {
				if _, ok := changedNames[name]; ok {
					return true
				}
				before, after := old.includedByFold(name), fold.includes(name)
				if before == after {
					return true
				}
				rec := trieGet(next.rowRoot, slot)
				if rec == nil {
					return true
				}
				tokBuf, postOps = replaceRowOps(rec, slot, old.nsTok, old.kindTok,
					namespaced, before, after, tokBuf, postOps)
				return true
			})
	}
	// S 집합의 들고남도 확인합니다(경계 구간 밖일 수 있습니다).
	for _, r := range append(append([]rowRef(nil), old.suffix...), fold.suffix...) {
		if _, ok := changedNames[r.name]; ok {
			continue
		}
		before, after := old.includedByFold(r.name), fold.includes(r.name)
		if before == after {
			continue
		}
		slot, _, ok := rowFind(next.rowDir, r.name)
		if !ok {
			continue
		}
		rec := trieGet(next.rowRoot, slot)
		if rec == nil {
			continue
		}
		st.visitedRows++
		tokBuf, postOps = replaceRowOps(rec, slot, old.nsTok, old.kindTok,
			namespaced, before, after, tokBuf, postOps)
	}

	// postTree 적용.
	keyer := postKeyer{root: next.rowRoot, namespace: next.nsTok, kindTok: next.kindTok}
	var pst postStats
	next.postRoot = postApply(next.postRoot, keyer, sortPostOps(postOps), &pst)
	next.postRoot = balancePostRoot(next.postRoot, &pst)
	st.directoryCopies += pst.leafEntriesCopied
	st.nodesCopied += pst.nodesCopied
	st.sepBytes += pst.sepBytes
	st.postingsChanged += int64(len(postOps))
	st.postEntryDelta += pst.entryDelta
	next.postEntries = old.postEntries + pst.entryDelta

	// 옛 슬롯 비우기 + pendingFree.
	for _, t := range changes {
		if !t.hadRow {
			continue
		}
		next.rowRoot = trieSet(next.rowRoot, t.oldSlot, nil, &st.transientBytes)
		pendingFree = append(pendingFree, t.oldSlot)
	}
	next.freeTop = free.publish(pendingFree)
	st.transientBytes += free.copied

	next.liveRows = old.liveRows + liveDelta
	next.truncRows = old.truncRows + truncDelta
	next.nsLabelTok = fold.acc
	next.boundName, next.boundValid = fold.boundName, fold.boundValid
	next.suffix = fold.suffix
	next.reasonMask = old.reasonMask &^ nsReasonLabelNs
	if next.truncRows > 0 || next.boundValid {
		next.reasonMask |= nsReasonLabelNs
	}
	// 슬롯이 살아 있는 행 수의 4배를 넘으면 압축이 필요합니다(유계 회수 대상).
	if next.liveRows > 1024 && int64(next.slotHigh) > 4*int64(next.liveRows) {
		st.compactionRequired = true
	}
	// 보유 바이트는 **증분**으로 유지합니다. 이벤트마다 파티션 전체를 다시 재면
	// 유계 갱신이라는 성질 자체가 사라집니다. recomputeBytes는 검증용으로 남습니다.
	//
	// 리프 비용은 **실제 용량 변화(capBytesDelta)** 로 셉니다.
	//
	// "리프 용량이 그 안의 항목을 덮는다"는 가정은 리프가 가득 찼을 때만 참입니다.
	// 부분 충전 리프에서는 cap이 len에 가까워 항목이 곧 비용이므로, 리프 수 변화만
	// 세면 리프가 쪼개지지 않는 삽입에서 추정이 실제보다 작아집니다.
	// 그래서 용량은 정확히 세고, 리프 헤더와 상위 노드 몫만 리프 수로 얹습니다.
	next.bytes = old.bytes + rowBytesDelta +
		pst.capBytesDelta + pst.leafDelta*(postLeafHeaderBytes+postNodePerLeafBytes) +
		rst.capBytesDelta + rst.leafDelta*(rowLeafHeaderBytes+rowNodePerLeafBytes) +
		pst.sepBytes + rst.sepBytes +
		newSlots*trieBytesPerSlot +
		int64(len(pendingFree))*4
	if next.bytes < nsPartFixedBytes {
		next.bytes = nsPartFixedBytes
	}
	return &next
}

// 지속 구조의 단위 비용입니다. 회계와 preflight 추정이 **같은 상수**를 씁니다 —
// 두 곳이 갈라지면 예산 판정이 조용히 틀립니다.
const (
	// postLeafCapBytes는 posting 리프 하나의 용량 비용입니다(슬라이스 헤더 포함).
	postLeafCapBytes = 24*2 + postLeafMax*postEntryBytes
	// postNodePerLeafBytes는 리프 하나에 얹히는 상위 노드 몫입니다.
	// 내부 노드 하나가 자식 treeFanout/2개를 덮는다고 봅니다(분할 직후 채움).
	// 내부 노드가 하한(treeNodeMin)만큼만 찼다고 보수적으로 가정합니다 —
	// 자식이 많을수록 리프당 몫은 줄어드니 과소 계상이 되지 않습니다.
	postNodePerLeafBytes = (24*3 + treeFanout*(postKeyBytes+8)) / treeNodeMin
	// rowLeafCapBytes는 행 디렉터리 리프 하나의 용량 비용입니다.
	rowLeafCapBytes = 24*3 + rowGroups*3 + rowLeafMax*(stringHeaderBytes+4+1)
	// rowNodePerLeafBytes는 행 디렉터리의 상위 노드 몫입니다.
	rowNodePerLeafBytes = (24*4 + treeFanout*(stringHeaderBytes+rowAggBytes+8)) / treeNodeMin
	// trieBytesPerSlot은 슬롯 하나가 차지하는 트라이 비용입니다(리프 몫 + 분기 몫).
	trieBytesPerSlot = trieLeafBytes/trieWidth + 1
)

// foldDiffRange는 두 fold의 접두사 대칭차 구간 [lo, hi)입니다.
func foldDiffRange(old *nsPart, next foldState) (lo, hi string, hasHi bool) {
	switch {
	case old.boundValid && next.boundValid:
		if old.boundName <= next.boundName {
			return old.boundName, next.boundName, true
		}
		return next.boundName, old.boundName, true
	case old.boundValid && !next.boundValid:
		return old.boundName, "", false
	case !old.boundValid && next.boundValid:
		return next.boundName, "", false
	default:
		return "", "", true // 둘 다 상한 미만이면 대칭차가 없습니다.
	}
}

// postOpBytes는 정렬 임시 배열 회계에 쓰는 postOp 하나의 크기입니다.
const postOpBytes = 64

// maxRowTokens는 행 하나가 만들 수 있는 정규 토큰 수 상한입니다.
// 이름 + namespace + kind + label(키·값 각각).
const maxRowTokens = 3 + 2*MaxLabelKeysPerObject

// radixSortPostOps는 토큰 앞 8바이트에 대한 LSD 기수 정렬입니다.
//
// 벌크 적재는 파티션 하나에 수십만 개의 op가 한 번에 들어오는데, 전부 문자열
// 비교로 정렬하면 Round 5에서 없앤 O(P log P) 문자열 비교가 그대로 돌아옵니다.
// 기수 정렬은 비교가 없고, head가 같은 묶음만 전체 키로 다시 정렬하므로
// **최종 순서는 comparePostKey 그대로**입니다.
//
// 각 패스는 **안정**이고 묶음 정렬도 SliceStable이므로, 같은 키에 삭제와 삽입이
// 함께 온 경우 원래 순서(삭제 → 삽입)가 보존됩니다.
func radixSortPostOps(ops []postOp) []postOp {
	n := len(ops)
	heads := make([]uint64, n)
	for i := range ops {
		heads[i] = tokenHead(ops[i].key.token)
	}
	scratch := make([]postOp, n)
	scratchHeads := make([]uint64, n)
	var counts [256]int
	for shift := 0; shift < 64; shift += 8 {
		for i := range counts {
			counts[i] = 0
		}
		for _, h := range heads {
			counts[byte(h>>uint(shift))]++
		}
		if counts[byte(heads[0]>>uint(shift))] == n {
			continue // 이 자리 값이 전부 같습니다. 옮길 이유가 없습니다.
		}
		sum := 0
		for i := 0; i < 256; i++ {
			c := counts[i]
			counts[i] = sum
			sum += c
		}
		for i, h := range heads {
			b := byte(h >> uint(shift))
			scratch[counts[b]] = ops[i]
			scratchHeads[counts[b]] = h
			counts[b]++
		}
		ops, scratch = scratch, ops
		heads, scratchHeads = scratchHeads, heads
	}
	for i := 0; i < n; {
		j := i + 1
		for j < n && heads[j] == heads[i] {
			j++
		}
		if j-i > 1 {
			seg := ops[i:j]
			sort.SliceStable(seg, func(a, b int) bool {
				return comparePostKey(seg[a].key, seg[b].key) < 0
			})
		}
		i = j
	}
	return ops
}

func sortPostOps(ops []postOp) []postOp {
	if len(ops) > 1 {
		ops = radixSortPostOps(ops)
	}
	// 같은 키가 두 번 나오면 뒤의 것이 이깁니다(삭제 후 삽입).
	w := 0
	for i := range ops {
		if w > 0 && comparePostKey(ops[w-1].key, ops[i].key) == 0 {
			ops[w-1] = ops[i]
			continue
		}
		ops[w] = ops[i]
		w++
	}
	return ops[:w]
}

// dedupePartOps는 이름 오름차순으로 정렬하고 같은 이름은 **마지막 것만** 남깁니다.
// 안정 정렬이라 입력 순서의 "마지막"이 그대로 승자입니다(last-event-wins).
func dedupePartOps(ops []partOp) []partOp {
	if len(ops) < 2 {
		return ops
	}
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].name < ops[j].name })
	w := 0
	for i := range ops {
		if w > 0 && ops[w-1].name == ops[i].name {
			ops[w-1] = ops[i]
			continue
		}
		ops[w] = ops[i]
		w++
	}
	return ops[:w]
}

func sortRowOps(ops []rowOp) []rowOp {
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].name < ops[j].name })
	w := 0
	for i := range ops {
		if w > 0 && ops[w-1].name == ops[i].name {
			ops[w-1] = ops[i]
			continue
		}
		ops[w] = ops[i]
		w++
	}
	return ops[:w]
}

/* ── 인덱스 ─────────────────────────────────────────────────────────────── */

// searchIndex는 GVR 하나의 증분 검색 인덱스입니다.
type searchIndex struct {
	kind       string
	kindTok    string
	namespaced bool
	version    uint64
	dir        *nsDir
	gvrStale   bool
	bytes      int64
}

func newSearchIndex(kind string, namespaced bool) *searchIndex {
	return &searchIndex{
		kind: kind, kindTok: normalizeToken(kind), namespaced: namespaced,
		dir: newNsDir(),
	}
}

func newNsPart(namespace, kindTok string, now time.Time) *nsPart {
	p := &nsPart{
		namespace: namespace,
		nsTok:     normalizeToken(namespace),
		kindTok:   kindTok,
		updatedAt: now.UnixNano(),
		rowRoot:   newTrieRoot(),
		postRoot:  newPostTree(),
		rowDir:    newRowTree(),
	}
	// 증분 회계의 출발점은 **추정 기준**입니다. 아직 만들어지지 않은 트라이 척추까지
	// 미리 얹어 두어야, 슬롯이 처음 채워질 때 회계가 실제보다 뒤처지지 않습니다.
	p.bytes = p.recomputeBytes() + trieSpineBytes
	return p
}

// applyOps는 namespace별로 묶인 연산을 적용한 새 인덱스입니다.
func (idx *searchIndex) applyOps(now time.Time, byNS map[string][]partOp, st *applyStats) *searchIndex {
	if len(byNS) == 0 {
		return idx
	}
	names := make([]string, 0, len(byNS))
	for ns := range byNS {
		names = append(names, ns)
	}
	sort.Strings(names)

	next := *idx
	next.version = idx.version + 1
	dir := idx.dir
	for _, ns := range names {
		// **이름은 반드시 유일해야 합니다.** 같은 이름이 두 번 들어오면 두 번째가
		// 첫 번째가 만든 새 슬롯을 보지 못해 옛 posting이 남고 슬롯이 샙니다.
		// 상류(합치기·보류 병합)가 이미 눌러 두지만, 여기서 한 번 더 방어합니다.
		ops := dedupePartOps(byNS[ns])
		part := dir.find(ns)
		if part == nil {
			part = newNsPart(ns, idx.kindTok, now)
		}
		updated := applyPartOps(part, idx.namespaced, now, ops, st)
		dir = dir.upsert(updated, &st.transientBytes)
	}
	next.dir = dir
	next.bytes = dir.bytes() + int64(len(idx.kind)+len(idx.kindTok)) + 128
	return &next
}

// staleNamespaces는 이 인덱스에서 신뢰할 수 없다고 표시된 파티션 수입니다.
func (idx *searchIndex) staleNamespaces() int {
	if idx == nil || idx.dir == nil {
		return 0
	}
	count := 0
	idx.dir.each(func(p *nsPart) bool {
		if p.reasonMask&nsReasonStale != 0 {
			count++
		}
		return true
	})
	return count
}

// markStale은 파티션 하나를 stale로 표시한 새 인덱스입니다.
func (idx *searchIndex) markStale(ns string, now time.Time) *searchIndex {
	part := idx.dir.find(ns)
	next := *idx
	next.version = idx.version + 1
	if part == nil {
		part = newNsPart(ns, idx.kindTok, now)
	}
	updated := *part
	updated.reasonMask |= nsReasonStale
	updated.partVersion = part.partVersion + 1
	var copied int64
	next.dir = idx.dir.upsert(&updated, &copied)
	next.bytes = next.dir.bytes() + int64(len(idx.kind)+len(idx.kindTok)) + 128
	return &next
}

// partitionCount는 파티션 수입니다(루트 집계에서 O(1)).
func (idx *searchIndex) partitionCount() int {
	if idx == nil || idx.dir == nil {
		return 0
	}
	return int(idx.dir.agg.nsCount)
}

// rootAgg는 이 인덱스의 루트 집계입니다. All 진단이 이것만 읽습니다.
func (idx *searchIndex) rootAgg() nsAgg {
	if idx == nil || idx.dir == nil {
		return nsAgg{}
	}
	return idx.dir.agg
}

// lookupIdentity는 (namespace, name)의 신원입니다.
//
// 반환값의 의미가 셋으로 나뉩니다.
//
//	authoritative  이 파티션이 신선해서 **이 답이 최종**입니다(없으면 정말 없는 것).
//	uid/found      찾았을 때의 신원.
//
// authoritative가 참이면 목록 스냅숏이 2초 뒤처져 같은 이름의 옛 행을 들고 있어도
// 그 값을 쓰지 않습니다 — 추가·UID 교체·삭제가 곧바로 반영되어야 하기 때문입니다.
func (idx *searchIndex) lookupIdentity(namespace, name string) (uid string, authoritative bool, found bool) {
	if idx == nil || idx.dir == nil || idx.gvrStale {
		return "", false, false
	}
	part := idx.dir.find(namespace)
	if part == nil {
		// 파티션 자체가 없습니다. GVR이 신선하면 "그 namespace에 아무것도 없다"가
		// 최종 답입니다.
		return "", true, false
	}
	fresh := part.reasonMask&nsReasonStale == 0
	slot, _, ok := rowFind(part.rowDir, name)
	if !ok {
		return "", fresh, false
	}
	rec := trieGet(part.rowRoot, slot)
	if rec == nil {
		return "", fresh, false
	}
	return rec.uid, fresh, true
}

/* ── 부트스트랩 빌드 ─────────────────────────────────────────────────────── */

/* ── preflight: 지속 구조 전용 크기 측정 ─────────────────────────────────── */

// persistentMeasure는 지속 구조의 보유 바이트를 좌우하는 **단조 증가 요인 전부**입니다.
// 배열 표현의 searchCost와 모양이 다르므로 그 값을 배수로 늘려 쓰지 않습니다.
type persistentMeasure struct {
	rows       int64
	namespaces int64
	// postings는 canonical 중복 제거 **전**의 토큰 등장 횟수입니다(상한).
	postings int64
	// maxNsPostings는 파티션 하나가 갖는 최대 posting 수입니다.
	// 정렬 임시 배열은 파티션 단위로만 살아 있으므로 정점 회계는 이 값을 씁니다.
	maxNsPostings int64
	// labelBytes는 blob 바이트입니다(길이 접두사 1바이트 포함).
	labelBytes int64
	// idBytes는 차용한 name·uid 바이트입니다. 인덱스가 그 수명을 늘리므로 계상합니다.
	idBytes int64
	// sepBytes는 fence 문자열의 보수적 추정입니다.
	sepBytes    int64
	nsNameBytes int64
}

func measurePersistentInput(index *indexSnapshot, kindTok string, namespaced bool) persistentMeasure {
	m := persistentMeasure{rows: int64(len(index.rows))}
	kindOK := safeToken(kindTok)
	var buf []string
	var nsPostings, nsLabelTokens int64
	flushNS := func() {
		if nsPostings > m.maxNsPostings {
			m.maxNsPostings = nsPostings
		}
		nsPostings, nsLabelTokens = 0, 0
	}
	for i := range index.rows {
		row := &index.rows[i]
		if i == 0 || row.namespace != index.rows[i-1].namespace {
			if i > 0 {
				flushNS()
			}
			m.namespaces++
			m.nsNameBytes += int64(len(row.namespace))
		}
		m.idBytes += int64(len(row.name)) + int64(len(rowUID(row)))
		// 이름·namespace·kind 토큰.
		m.postings++
		nsPostings++
		if namespaced && row.namespace != "" {
			m.postings++
			nsPostings++
		}
		if kindOK {
			m.postings++
			nsPostings++
		}
		if row.obj == nil || len(row.obj.Labels) == 0 {
			continue
		}
		keys, _ := sortedLabelKeys(row, buf)
		buf = keys[:0]
		var rowTokens int64
		for _, k := range keys {
			rowTokens++
			m.labelBytes += tokenBytesOf(k) + 1
			if v := row.obj.Labels[k]; v != "" {
				rowTokens++
				m.labelBytes += tokenBytesOf(v) + 1
			}
		}
		// label posting은 namespace 상한 안에서만 생깁니다. blob 바이트는 상한과
		// 무관하게 언제나 보관하므로 위에서 이미 더했습니다.
		if nsLabelTokens+rowTokens <= MaxLabelTokensPerNamespace {
			m.postings += rowTokens
			nsPostings += rowTokens
			nsLabelTokens += rowTokens
		}
	}
	if m.rows > 0 {
		flushNS()
	}
	// fence는 리프마다 하나씩 생기고, 그 키는 (token, name, uid) 소유 복사본입니다.
	leaves := m.postings/postLeafSplit + 1
	avgKey := int64(tokenPrefixBytes)
	if m.rows > 0 {
		avgKey = (m.idBytes / m.rows) + 16
	}
	m.sepBytes = leaves * avgKey
	return m
}

// persistentSearchCost는 지속 구조의 (보유, 보유+임시) 바이트 상한입니다.
//
// **증분 회계와 같은 단위 상수**를 씁니다. 두 곳이 갈라지면 preflight가 통과시킨
// 빌드가 게시 단계에서 거절되거나 그 반대가 됩니다.
func persistentSearchCost(m persistentMeasure) (retained, peak int64) {
	postLeaves := m.postings/postLeafSplit + 1
	rowLeaves := m.rows/rowLeafSplit + 1

	retained = int64(fixedSnapshotBytes)
	retained += m.rows*rowRecordFixedBytes + m.labelBytes + m.idBytes
	retained += postLeaves * (postLeafCapBytes + postNodePerLeafBytes)
	retained += rowLeaves * (rowLeafCapBytes + rowNodePerLeafBytes)
	retained += m.sepBytes
	retained += m.rows * trieBytesPerSlot
	// 파티션·페이지 디렉터리.
	retained += m.namespaces*(nsPartFixedBytes+2*stringHeaderBytes+8) + 2*m.nsNameBytes
	retained += (m.namespaces/nsPageSize + 2) * (24*2 + nsAggBytes)

	// 임시 ①: 파티션 하나의 op 배열 + 기수 정렬 scratch + head 배열(양쪽).
	transient := m.maxNsPostings * (2*postOpBytes + 16)
	// 임시 ②: 부트스트랩이 **한꺼번에** 들고 있는 byNS/rowInput/label 사본.
	// 스트리밍으로 바꾸는 대신 실제 정점을 그대로 예약합니다(과소 계상 금지).
	transient += m.rows*(bootstrapRowInputBytes+bootstrapPartOpBytes) + m.labelBytes
	return retained, retained + transient
}

const (
	// bootstrapRowInputBytes는 rowInput 하나와 그 label 슬라이스 헤더의 비용입니다.
	bootstrapRowInputBytes = 96
	// bootstrapPartOpBytes는 partOp 하나와 맵 슬라이스 몫입니다.
	bootstrapPartOpBytes = 40
)

// buildSearchIndex는 목록 스냅숏 하나에서 증분 인덱스를 통째로 세웁니다.
//
// 부트스트랩과 명시적 회수에서만 호출됩니다. 정상 델타 flush 경로에는 없습니다.
// 예산 판정은 기존 measureSearchInput/searchCost를 그대로 재사용해 **할당 전에** 합니다.
func buildSearchIndex(index *indexSnapshot, kind string, namespaced bool,
	retainedBudget, peakAllowance int64) searchIndexResult {

	idx := newSearchIndex(kind, namespaced)
	if index == nil || len(index.rows) == 0 {
		idx.bytes = idx.dir.bytes() + 128
		return searchIndexResult{index: idx, state: SearchReady, peak: idx.bytes}
	}
	pm := measurePersistentInput(index, idx.kindTok, namespaced)
	retained, peak := persistentSearchCost(pm)
	if retained > retainedBudget || peak > peakAllowance {
		return searchIndexResult{state: SearchUnavailable, reason: reasonBudget, needed: retained}
	}

	now := index.builtAt
	byNS := make(map[string][]partOp, pm.namespaces)
	var keyBuf []string
	var tokBuf []string
	for i := range index.rows {
		row := &index.rows[i]
		var truncated bool
		tokBuf, truncated, keyBuf = labelTokensOf(row, keyBuf, tokBuf)
		labels := make([]string, len(tokBuf))
		copy(labels, tokBuf)
		byNS[row.namespace] = append(byNS[row.namespace], partOp{
			name: row.name,
			input: &rowInput{
				name: row.name, uid: rowUID(row),
				labels: labels, keysTruncated: truncated,
			},
		})
	}
	var st applyStats
	built := idx.applyOps(now, byNS, &st)
	if st.slotExhausted {
		return searchIndexResult{state: SearchUnavailable, reason: reasonBudget, needed: retained}
	}
	if built.bytes > retainedBudget {
		return searchIndexResult{state: SearchUnavailable, reason: reasonBudget, needed: built.bytes}
	}
	return searchIndexResult{index: built, state: SearchReady, peak: built.bytes + st.transientBytes}
}

// searchIndexResult는 증분 인덱스 빌드 결과입니다.
type searchIndexResult struct {
	index  *searchIndex
	state  SearchState
	reason string
	peak   int64
	// needed는 실패했을 때 필요했던 보유 바이트 추정입니다. 회로 판정에 씁니다.
	needed int64
}

/* ── 질의: 파티션 스트림 ─────────────────────────────────────────────────── */

// partStream은 파티션 하나의 접두사 구간 순회입니다.
type partStream struct {
	gvrKey     string
	group      string
	version    string
	resource   string
	kind       string
	namespaced bool

	part   *nsPart
	cursor *postCursor
	query  string

	curToken string
	curName  string
	curUID   string
	curRec   *rowRecord
	curEntry postEntry
}

// load는 현재 위치의 값을 읽습니다. 접두사 구간을 벗어나면 false입니다.
func (s *partStream) load() bool {
	for s.cursor.valid() {
		e := s.cursor.entry()
		rec := trieGet(s.part.rowRoot, e.slot)
		if rec == nil {
			s.cursor.next()
			continue
		}
		tok := s.cursor.keyer.tokenOf(rec, e.tokIdx)
		if !strings.HasPrefix(tok, s.query) {
			return false
		}
		s.curToken, s.curName, s.curUID, s.curRec, s.curEntry = tok, rec.name, rec.uid, rec, e
		return true
	}
	return false
}

func (s *partStream) less(o *partStream) bool {
	return searchKeyLess(s.curToken, s.part.namespace, s.curName, s.gvrKey, s.curUID,
		o.curToken, o.part.namespace, o.curName, o.gvrKey, o.curUID)
}

// isRowMinimum은 이 posting이 그 행의 **구간 최소 토큰**인지입니다.
//
// 질의 접두사를 가진 토큰은 연속 블록이므로, 직전 토큰과의 공통 접두사가 질의보다
// 짧으면 직전 토큰은 접두사를 갖지 못하고 이 항목이 블록의 첫 항목입니다.
func (s *partStream) isRowMinimum() bool {
	return len(s.query) > int(s.curEntry.prevLCP)
}

// matchedField는 이 행의 **모든** 일치 토큰 중 가장 우선순위 높은 필드입니다.
// prevLCP는 중복 제거만 증명하므로 필드는 여기서 따로 정합니다(<=35회 접두사 검사).
func (s *partStream) matchedField() uint32 {
	best := uint32(s.curEntry.field)
	rec := s.curRec
	if rec == nil {
		return best
	}
	check := func(tok string, f uint32) {
		if tok != "" && strings.HasPrefix(tok, s.query) && f < best {
			best = f
		}
	}
	check(rec.nameTok, fieldName)
	if s.namespaced && s.part.nsTok != "" {
		check(s.part.nsTok, fieldNamespace)
	}
	check(s.part.kindTok, fieldKind)
	if s.part.includedByFold(rec.name) {
		rec.eachLabelToken(func(_ int, tok string) bool {
			check(tok, fieldLabel)
			return best != fieldName
		})
	}
	return best
}

func (s *partStream) next() bool {
	s.cursor.next()
	return s.load()
}

/* ── 클러스터 전체 접근 질의 (지속 구조) ─────────────────────────────────── */

type partHeap []*partStream

func (h partHeap) down(i int) {
	n := len(h)
	for {
		l, r, small := 2*i+1, 2*i+2, i
		if l < n && h[l].less(h[small]) {
			small = l
		}
		if r < n && h[r].less(h[small]) {
			small = r
		}
		if small == i {
			return
		}
		h[i], h[small] = h[small], h[i]
		i = small
	}
}

func (h partHeap) init() {
	for i := len(h)/2 - 1; i >= 0; i-- {
		h.down(i)
	}
}

// searchPersistent는 지속 구조 인덱스 위의 클러스터 전체 접근 경로입니다.
//
// 진단과 ObservedAt은 **cursor 접미사 필터링 전에** 각 GVR의 nsTree 루트 집계에서
// 모읍니다. 그래서 2페이지 cursor보다 완전히 앞에 있는 stale·최고령 파티션도
// 모든 페이지에 그대로 반영됩니다. 루트 집계 읽기는 GVR당 O(1)입니다.
func (s *Service) searchPersistent(query string, limit int, cursor searchCursorKey, hasCursor bool,
	fingerprint string, page *SearchPage, diag *searchDiagnostics, view *searchView) (SearchPage, error) {

	// ① 진단·ObservedAt·스트림 수를 먼저 셉니다(순회 전).
	type participant struct {
		gvr  schema.GroupVersionResource
		desc Descriptor
		idx  *searchIndex
	}
	parts := make([]participant, 0, len(s.order))
	totalStreams := 0
	for i, gvr := range s.order {
		// **요청 뷰에서만** 집습니다. 진단·ObservedAt·스트림도 전부 이 요청이
		// 빌린 세대에서만 나옵니다. 카운트는 baseline에서 옵니다.
		es := view.searchAt(i)
		desc, err := s.describeWithIndex(gvr, view.baseAt(i))
		if err != nil {
			continue
		}
		if desc.State != StateReady {
			diag.note(searchStateReason(desc.State))
			continue
		}
		if es == nil {
			diag.note(reasonSyncing)
			continue
		}
		switch es.searchState {
		case SearchReady:
			if es.sindex == nil {
				diag.note(reasonSyncing)
				continue
			}
		case SearchUnavailable:
			if es.searchReason != "" {
				diag.note(es.searchReason)
			} else {
				diag.note(reasonBudget)
			}
			continue
		case SearchDisabled:
			continue
		default:
			diag.note(reasonSyncing)
			continue
		}
		agg := es.sindex.rootAgg()
		if agg.nsCount == 0 {
			continue
		}
		if agg.reasonMask&nsReasonLabelNs != 0 {
			diag.note(reasonLabelNs)
		}
		// 인덱스에 반영된 stale과 **아직 큐에만 있는** 회수 대기를 함께 봅니다.
		staleParts, gvrStale := s.staleSummary(gvr)
		if agg.reasonMask&nsReasonStale != 0 || es.sindex.gvrStale || staleParts > 0 || gvrStale {
			diag.note(reasonSearchStale)
		}
		if agg.oldestUpdated != 0 {
			// 파티션은 시각을 unix nano로 들고 있습니다. time.Unix는 **지역 시간대**를
			// 붙이므로 그대로 실으면 같은 순간인데도 직렬 문자열이 배열 색인 경로와
			// 달라집니다(UTC "…Z" 대 "+09:00"). 순간은 그대로 두고 UTC로 정규화합니다.
			at := time.Unix(0, agg.oldestUpdated).UTC()
			if page.ObservedAt.IsZero() || at.Before(page.ObservedAt) {
				page.ObservedAt = at
			}
		}
		totalStreams += int(agg.nsCount)
		parts = append(parts, participant{gvr: gvr, desc: desc, idx: es.sindex})
	}
	// ② 스트림 상한은 **순회를 시작하기 전에** 강제합니다.
	if totalStreams > MaxSearchStreams {
		return SearchPage{}, ErrSearchTooBroad
	}
	diag.apply(page)

	// ③ 스트림을 엽니다.
	streams := make(partHeap, 0, 32)
	for _, p := range parts {
		gvrKey := FormatGVR(p.gvr)
		p.idx.dir.each(func(part *nsPart) bool {
			c := openPartStream(part, query, cursor, hasCursor, gvrKey)
			st := &partStream{
				gvrKey: gvrKey, group: GroupSegment(p.gvr.Group), version: p.gvr.Version,
				resource: p.gvr.Resource, kind: p.desc.Kind, namespaced: p.desc.Namespaced,
				part: part, cursor: c, query: query,
			}
			if st.load() {
				streams = append(streams, st)
			}
			return true
		})
	}
	if len(streams) == 0 {
		return *page, nil
	}
	streams.init()

	scanBudget := limit * searchScanFactor
	if scanBudget > maxSearchScan {
		scanBudget = maxSearchScan
	}
	if scanBudget < limit {
		scanBudget = limit
	}
	byteBudget := MaxSearchResponseBytes
	examined := 0
	var lastKey searchCursorKey
	haveKey := false
	stop := func() (SearchPage, error) {
		page.Truncated = true
		if haveKey {
			page.NextCursor = encodeSearchCursor(lastKey, fingerprint)
		}
		return *page, nil
	}

	for len(streams) > 0 {
		if len(page.Items) >= limit || examined >= scanBudget {
			return stop()
		}
		st := streams[0]
		examined++
		// 한 행이 여러 필드에 걸려도 **구간 최소 토큰에서 한 번만** 나갑니다.
		if st.isRowMinimum() {
			cost := searchRowOverheadBytes + len(st.part.namespace) + len(st.curName) + len(st.curUID) + len(st.gvrKey)
			if len(page.Items) > 0 && cost > byteBudget {
				return stop()
			}
			byteBudget -= cost
			page.Items = append(page.Items, SearchItem{
				Group: st.group, Version: st.version, Resource: st.resource,
				Kind: st.kind, Namespaced: st.namespaced,
				Namespace: st.part.namespace, Name: st.curName, UID: st.curUID,
				MatchedField: matchedFieldNames[st.matchedField()],
			})
		}
		lastKey = searchCursorKey{
			mode: cursorModeIndex, token: st.curToken, namespace: st.part.namespace,
			name: st.curName, gvr: st.gvrKey, uid: st.curUID,
		}
		haveKey = true

		if st.next() {
			streams.down(0)
			continue
		}
		last := len(streams) - 1
		streams[0], streams[last] = streams[last], nil
		streams = streams[:last]
		if len(streams) > 0 {
			streams.down(0)
		}
	}
	return *page, nil
}

// openPartStream은 파티션 하나에서 접두사 구간 커서를 엽니다.
//
// 전역 순서는 **(token, namespace, name, gvr, uid)** 입니다. 이 파티션은 (gvr, namespace)가
// 고정되어 있으므로, 전역 cursor를 이 파티션의 지역 하한 (token, name, uid)으로 옮길 때
// 그 두 성분이 순서를 어디서 가르는지를 그대로 반영해야 합니다.
//
//	namespace <  cursor.ns   이 토큰의 모든 항목이 앞 → 토큰을 통째로 건너뜁니다.
//	namespace >  cursor.ns   이 토큰의 모든 항목이 뒤 → 토큰 하한에서 시작합니다.
//	namespace == cursor.ns   이제 gvr이 순서를 가릅니다.
//	    gvr <  cursor.gvr    그 이름의 **모든 UID**가 앞 → 이름을 통째로 건너뜁니다.
//	    gvr == cursor.gvr    정확히 그 UID **다음**부터입니다.
//	    gvr >  cursor.gvr    그 이름의 **첫 UID**부터입니다.
//
// 예전에는 마지막 세 갈래를 "일단 (token,name,uid)로 내려가서 한 칸 밀기"로 뭉갰습니다.
// 파티션 안에서 (name)이 유일하다는 성질에 기대던 것이라, 그 성질이 흔들리면 조용히
// 중복이나 누락이 생깁니다. 이제는 갈래를 그대로 씁니다.
func openPartStream(part *nsPart, query string, cursor searchCursorKey, hasCursor bool, gvrKey string) *postCursor {
	keyer := part.keyer()
	if !hasCursor {
		return seekPost(part.postRoot, keyer, postKey{token: query}, true)
	}
	switch {
	case part.namespace < cursor.namespace:
		return seekPost(part.postRoot, keyer,
			postKey{token: cursor.token, name: sentinelAfterName}, true)
	case part.namespace > cursor.namespace:
		return seekPost(part.postRoot, keyer, postKey{token: cursor.token}, true)
	}
	switch {
	case gvrKey < cursor.gvr:
		// 이름은 같아도 gvr이 앞서므로 그 이름의 모든 UID가 cursor보다 앞입니다.
		return seekPost(part.postRoot, keyer,
			postKey{token: cursor.token, name: cursor.name, uid: sentinelAfterName}, true)
	case gvrKey > cursor.gvr:
		// gvr이 뒤서므로 그 이름의 첫 UID부터가 모두 cursor보다 뒤입니다.
		return seekPost(part.postRoot, keyer,
			postKey{token: cursor.token, name: cursor.name}, true)
	}
	// 같은 gvr: 그 UID 다음부터입니다. 키가 유일하므로 한 칸이면 충분하지만,
	// 전역 순서 비교로 확인하며 밀어 그 성질에 기대지 않습니다.
	c := seekPost(part.postRoot, keyer,
		postKey{token: cursor.token, name: cursor.name, uid: cursor.uid}, true)
	for c.valid() {
		e := c.entry()
		rec := trieGet(part.rowRoot, e.slot)
		if rec == nil {
			c.next()
			continue
		}
		tok := keyer.tokenOf(rec, e.tokIdx)
		if searchKeyLess(cursor.token, cursor.namespace, cursor.name, cursor.gvr, cursor.uid,
			tok, part.namespace, rec.name, gvrKey, rec.uid) {
			break // cursor보다 뒤입니다. 여기서 이어갑니다.
		}
		c.next()
	}
	return c
}
