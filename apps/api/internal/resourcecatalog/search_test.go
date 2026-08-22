package resourcecatalog

// 검색 인덱스의 성질을 검증합니다. (ADR 0023)
//
// 가장 중요한 다섯 가지입니다.
//   - 이름·namespace·kind·label을 대소문자 없이 접두사로 찾는다.
//   - 한 행이 여러 필드에 걸려도 **정확히 한 번만** 나간다(페이지 경계와 무관).
//   - 예산을 넘기는 빌드는 **큰 할당을 시작하기 전에** 물러난다.
//   - 보유 슬라이스에 여유(slack)가 없고, 회계가 실제 backing과 같다.
//   - label 절단은 namespace 안에서만 결정되고, namespace 밖으로 새지 않는다.

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// hugeBudget은 예산이 검증 대상이 아닌 테스트에서 쓰는 값입니다.
const hugeBudget = int64(1) << 40

func metaRow(namespace, name, uid string, labels map[string]string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID(uid), Labels: labels,
			CreationTimestamp: metav1.NewTime(indexBase),
		},
	}
}

func indexOf(objs ...*metav1.PartialObjectMetadata) *indexSnapshot {
	raw := make([]any, 0, len(objs))
	for _, o := range objs {
		raw = append(raw, o)
	}
	return buildIndexSnapshot(raw, indexBase)
}

func searchFixture(t *testing.T, budget int64, objs ...*metav1.PartialObjectMetadata) *searchSnapshot {
	t.Helper()
	result := buildSearchSnapshot(indexOf(objs...), "Service", true, budget, budget)
	if result.state != SearchReady || result.snapshot == nil {
		t.Fatalf("인덱스를 만들지 못했습니다: state=%s reason=%s", result.state, result.reason)
	}
	return result.snapshot
}

// nsIndexOf는 namespace 이름의 구간 첨자입니다.
func nsIndexOf(snap *searchSnapshot, ns string) int {
	for i, name := range snap.nsNames {
		if name == ns {
			return i
		}
	}
	return -1
}

// collect는 스냅숏 하나에서 접두사에 걸리는 (namespace/name, matchedField)를 모읍니다.
// allowed가 nil이면 전체 접근입니다. Service를 거치지 않고 인덱스 성질만 봅니다.
func collect(snap *searchSnapshot, query string, allowed []string) []string {
	loID, hiID, ok := snap.prefixRange(query)
	if !ok {
		return nil
	}
	indices := make([]int, 0, len(snap.nsNames))
	if allowed == nil {
		for i := range snap.nsNames {
			indices = append(indices, i)
		}
	} else {
		for _, ns := range allowed {
			if i := nsIndexOf(snap, ns); i >= 0 {
				indices = append(indices, i)
			}
		}
	}
	var out []string
	for _, ns := range indices {
		lo, end, ok := snap.postingRange(ns, loID, hiID)
		if !ok {
			continue
		}
		for i := lo; i < end; i++ {
			p := snap.postings[i]
			id, field, ok := snap.matchInRange(p.row, loID, hiID)
			if !ok || id != p.token {
				continue
			}
			row := &snap.base.rows[p.row]
			out = append(out, fmt.Sprintf("%s/%s:%s", row.namespace, row.name, matchedFieldNames[field]))
		}
	}
	return out
}

/* ── 매칭 ────────────────────────────────────────────────────────────────── */

func TestSearchMatchesNameNamespaceKindAndLabelPrefixes(t *testing.T) {
	snap := searchFixture(t, hugeBudget,
		metaRow("payments", "payments-api", "uid-1", map[string]string{"app": "checkout"}),
		metaRow("search", "indexer", "uid-2", map[string]string{"Tier": "Batch"}),
	)
	cases := []struct {
		query string
		want  []string
	}{
		// 이름과 namespace가 모두 걸리면 사전순이 아니라 **더 구체적인 필드**를 보고합니다.
		{"pay", []string{"payments/payments-api:name"}},
		{"sea", []string{"search/indexer:namespace"}},
		{"ser", []string{"payments/payments-api:kind", "search/indexer:kind"}},
		{"che", []string{"payments/payments-api:label"}},
		{"tie", []string{"search/indexer:label"}}, // label 키는 대소문자 없이 찾습니다.
		{"bat", []string{"search/indexer:label"}}, // label 값도 마찬가지입니다.
		{"zzz", nil}, // 없는 접두사는 0건입니다.
		{"indexer", []string{"search/indexer:name"}},
	}
	for _, tc := range cases {
		got := collect(snap, tc.query, nil)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("query %q: got %v want %v", tc.query, got, tc.want)
		}
	}
}

func TestSearchEmitsEachObjectExactlyOncePerQuery(t *testing.T) {
	// 이름·namespace·label 값이 모두 같은 접두사에 걸리는 최악의 경우입니다.
	snap := searchFixture(t, hugeBudget,
		metaRow("payments", "payments-api", "uid-1", map[string]string{"payments-role": "payments-edge"}),
	)
	got := collect(snap, "pay", nil)
	if len(got) != 1 {
		t.Fatalf("한 행이 %d번 나왔습니다: %v", len(got), got)
	}
	if got[0] != "payments/payments-api:name" {
		t.Fatalf("matchedField 우선순위가 어긋났습니다: %v", got)
	}
}

func TestSearchTokensAreCaseFoldedAndLengthBounded(t *testing.T) {
	long := strings.Repeat("a", 200) + "-tail"
	snap := searchFixture(t, hugeBudget, metaRow("payments", long, "uid-1", nil))
	for _, tok := range snap.tokens {
		if len(tok) > tokenPrefixBytes {
			t.Fatalf("토큰이 %d바이트입니다 — 상한은 %d입니다", len(tok), tokenPrefixBytes)
		}
		if tok != strings.ToLower(tok) {
			t.Fatalf("토큰이 소문자로 정규화되지 않았습니다: %q", tok)
		}
	}
	if got := collect(snap, strings.Repeat("a", MaxQueryLen), nil); len(got) != 1 {
		t.Fatalf("절단된 토큰으로 접두사를 찾지 못했습니다: %v", got)
	}
}

/* ── Scope (P1-3) ────────────────────────────────────────────────────────── */

func TestSearchNamespaceSpansIsolateForbiddenRows(t *testing.T) {
	snap := searchFixture(t, hugeBudget,
		metaRow("allowed", "payments-api", "uid-1", nil),
		metaRow("forbidden", "payments-worker", "uid-2", nil),
	)
	got := collect(snap, "pay", []string{"allowed"})
	if len(got) != 1 || !strings.HasPrefix(got[0], "allowed/") {
		t.Fatalf("Scope 밖 namespace가 후보에 들어왔습니다: %v", got)
	}
	// 허용된 namespace 구간이 forbidden 행을 하나도 포함하지 않아야 합니다.
	allowed := nsIndexOf(snap, "allowed")
	lo, hi := int(snap.nsPostings[allowed]), int(snap.nsPostings[allowed+1])
	for i := lo; i < hi; i++ {
		if snap.base.rows[snap.postings[i].row].namespace != "allowed" {
			t.Fatal("namespace 구간이 다른 namespace의 행을 담고 있습니다")
		}
	}
}

// TestSearchNamespaceLabelBoundaryIsIndependentOfOtherNamespaces — P1-3의 핵심입니다.
//
// 볼 수 없는 namespace가 label 키 상한을 아무리 넘겨도, 볼 수 있는 namespace의
// label 색인과 진단은 그대로여야 합니다.
func TestSearchNamespaceLabelBoundaryIsIndependentOfOtherNamespaces(t *testing.T) {
	clean := searchFixture(t, hugeBudget,
		metaRow("allowed", "payments-api", "uid-1", map[string]string{"app": "checkout"}),
	)
	noisyLabels := make(map[string]string, MaxLabelKeysPerObject+8)
	for i := 0; i < MaxLabelKeysPerObject+8; i++ {
		noisyLabels[fmt.Sprintf("key-%02d", i)] = fmt.Sprintf("value-%02d", i)
	}
	noisy := searchFixture(t, hugeBudget,
		metaRow("allowed", "payments-api", "uid-1", map[string]string{"app": "checkout"}),
		metaRow("forbidden", "payments-worker", "uid-2", noisyLabels),
		metaRow("forbidden", "payments-cache", "uid-3", noisyLabels),
	)
	for _, query := range []string{"pay", "che", "app"} {
		a := strings.Join(collect(clean, query, []string{"allowed"}), ",")
		b := strings.Join(collect(noisy, query, []string{"allowed"}), ",")
		if a != b {
			t.Errorf("query %q: 숨겨진 namespace가 허용 namespace 결과를 바꿨습니다\n  %q\n  %q", query, a, b)
		}
	}
	// 진단도 namespace 단위여야 합니다 — forbidden만 잘린 것으로 표시됩니다.
	if noisy.labelIncompleteIn(nsIndexOf(noisy, "allowed")) {
		t.Error("허용 namespace가 잘린 것으로 표시되었습니다")
	}
	if !noisy.labelIncompleteIn(nsIndexOf(noisy, "forbidden")) {
		t.Error("label 키 상한을 넘긴 namespace가 표시되지 않았습니다")
	}
}

/* ── 예산 (P1-1) ─────────────────────────────────────────────────────────── */

func manyRows(count int, ns string, labels map[string]string) []*metav1.PartialObjectMetadata {
	out := make([]*metav1.PartialObjectMetadata, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, metaRow(ns, fmt.Sprintf("workload-%04d", i), fmt.Sprintf("uid-%04d", i), labels))
	}
	return out
}

func TestSearchRefusesBeforeAllocatingWhenIdentityDoesNotFit(t *testing.T) {
	index := indexOf(manyRows(400, "payments", nil)...)
	tiny := int64(4096)
	result := buildSearchSnapshot(index, "Service", true, tiny, tiny)
	if result.state != SearchUnavailable || result.snapshot != nil {
		t.Fatalf("예산이 부족하면 반쪽 인덱스 없이 물러나야 합니다: state=%s", result.state)
	}
	if result.reason == "" {
		t.Fatal("제외 사유가 비어 있습니다 — 조용히 사라지면 안 됩니다")
	}
	if result.peak != 0 {
		t.Fatalf("실패한 빌드가 정점을 소비했습니다: %d", result.peak)
	}
	// 실패 경로는 큰 배열을 **잡기 전에** 끝나야 합니다. 측정 패스는 할당이 없습니다.
	allocs := testing.AllocsPerRun(20, func() {
		_ = buildSearchSnapshot(index, "Service", true, tiny, tiny)
	})
	if allocs > 8 {
		t.Fatalf("실패하는 빌드가 %v번 할당했습니다 — 예산 판정이 할당 뒤에 있습니다", allocs)
	}
}

func TestSearchRetainedSlicesHaveNoSlack(t *testing.T) {
	snap := searchFixture(t, hugeBudget, manyRows(200, "payments", map[string]string{"app": "checkout", "tier": "web"})...)
	checks := []struct {
		name     string
		length   int
		capacity int
	}{
		{"tokens", len(snap.tokens), cap(snap.tokens)},
		{"postings", len(snap.postings), cap(snap.postings)},
		{"ids", len(snap.ids), cap(snap.ids)},
		{"off", len(snap.off), cap(snap.off)},
		{"nsNames", len(snap.nsNames), cap(snap.nsNames)},
		{"nsPostings", len(snap.nsPostings), cap(snap.nsPostings)},
		{"nsLabelIncomplete", len(snap.nsLabelIncomplete), cap(snap.nsLabelIncomplete)},
	}
	for _, c := range checks {
		if c.length != c.capacity {
			t.Errorf("%s에 여유가 남았습니다: len=%d cap=%d — 회계가 backing과 어긋납니다", c.name, c.length, c.capacity)
		}
	}
}

func TestSearchRetainedBytesCountCapacityAndStrings(t *testing.T) {
	snap := searchFixture(t, hugeBudget,
		metaRow("payments", "payments-api", "uid-1", map[string]string{"app": "checkout"}),
	)
	var want int64 = fixedSnapshotBytes
	for _, tok := range snap.tokens {
		want += int64(len(tok)) + stringHeaderBytes
	}
	for _, ns := range snap.nsNames {
		want += int64(len(ns)) + stringHeaderBytes
	}
	want += int64(cap(snap.postings))*postingBytes + int64(cap(snap.ids))*csrBytes
	want += int64(cap(snap.off))*uint32Bytes + int64(cap(snap.nsPostings))*uint32Bytes
	want += int64(cap(snap.nsLabelIncomplete))
	if snap.bytes != want {
		t.Fatalf("보유 바이트 %d != 손계산 %d", snap.bytes, want)
	}
	// 여유가 생기면 회계가 그것을 세야 합니다.
	slacked := *snap
	slacked.postings = append(make([]posting, 0, len(snap.postings)+16), snap.postings...)
	if slacked.retainedBytes() <= snap.bytes {
		t.Fatal("append 여유가 회계에 잡히지 않았습니다")
	}
	// 문자열 본문도 실제로 계산에 들어가야 합니다.
	longer := searchFixture(t, hugeBudget,
		metaRow("payments", "payments-api-with-a-much-longer-name", "uid-1", map[string]string{"app": "checkout"}),
	)
	if longer.bytes <= snap.bytes {
		t.Fatalf("긴 토큰이 더 많은 바이트로 계산되지 않았습니다: %d <= %d", longer.bytes, snap.bytes)
	}
}

func TestSearchCostEstimateIsAnUpperBoundOnRetained(t *testing.T) {
	index := indexOf(manyRows(300, "payments", map[string]string{"app": "checkout", "tier": "web"})...)
	m := measureSearchInput(index, "service", true)
	estimated, peak := searchCost(m)
	result := buildSearchSnapshot(index, "Service", true, hugeBudget, hugeBudget)
	if result.snapshot == nil {
		t.Fatalf("state=%s", result.state)
	}
	if result.snapshot.bytes > estimated {
		t.Fatalf("실제 보유 %d가 추정 상한 %d를 넘었습니다 — 예산 판정이 무효입니다", result.snapshot.bytes, estimated)
	}
	if result.peak > peak {
		t.Fatalf("보고된 정점 %d가 추정 상한 %d를 넘었습니다", result.peak, peak)
	}
}

func TestSearchNeverInternsOrphanTokens(t *testing.T) {
	// label 키 상한과 namespace 상한을 함께 건드리는 픽스처입니다.
	labels := make(map[string]string, MaxLabelKeysPerObject+4)
	for i := 0; i < MaxLabelKeysPerObject+4; i++ {
		labels[fmt.Sprintf("key-%02d", i)] = fmt.Sprintf("value-%02d", i)
	}
	snap := searchFixture(t, hugeBudget, manyRows(50, "payments", labels)...)
	used := make([]bool, len(snap.tokens))
	for _, p := range snap.postings {
		used[p.token] = true
	}
	for id, ok := range used {
		if !ok {
			t.Fatalf("posting이 하나도 없는 토큰이 남았습니다: %q — 회계에만 잡히는 유령입니다", snap.tokens[id])
		}
	}
}

// TestSearchTightRetainedBudgetIsExplicitAndBounded — 보유 예산 경계입니다.
//
// 예전에는 "label만 빼고 identity는 남기는" 반쪽 상태를 확인했습니다. 그 상태는
// 이제 존재하지 않습니다 — 볼 수 없는 namespace의 객체 수가 볼 수 있는 namespace의
// label 적중률을 바꾸기 때문입니다(P1-A). 그래서 같은 경계에서 **더 강한 것**을 봅니다.
//   - 예산이 맞으면 identity와 label이 **둘 다 완전하다.**
//   - 예산이 1바이트라도 모자라면 명시적 unavailable + 사유이고, 스냅숏도 정점 소비도 없다.
//   - 어느 쪽이든 보유·정점 상한을 넘지 않는다.
func TestSearchTightRetainedBudgetIsExplicitAndBounded(t *testing.T) {
	rows := manyRows(300, "payments", map[string]string{"app": "application-name"})
	index := indexOf(rows...)
	full := buildSearchSnapshot(index, "Service", true, hugeBudget, hugeBudget)
	if full.state != SearchReady || full.snapshot == nil {
		t.Fatalf("넉넉한 예산에서 실패했습니다: state=%s reason=%s", full.state, full.reason)
	}
	// identity는 **완전해야** 합니다. 첫 행도 마지막 행도 이름으로 찾힙니다.
	for _, name := range []string{"workload-0000", "workload-0299"} {
		if got := collect(full.snapshot, name, nil); len(got) != 1 {
			t.Fatalf("이름 검색이 완전하지 않습니다(%s): %v", name, got)
		}
	}
	// label도 완전해야 합니다 — 예산이 맞는데 조용히 빠지는 경로가 없어야 합니다.
	if got := collect(full.snapshot, "application-name", nil); len(got) != len(rows) {
		t.Fatalf("label 색인이 완전하지 않습니다: %d건, %d건이어야 합니다", len(got), len(rows))
	}
	if full.snapshot.bytes > full.peak {
		t.Fatalf("보유 %d가 정점 %d를 넘었습니다", full.snapshot.bytes, full.peak)
	}

	// 보유 예산이 1바이트 모자란 경계입니다.
	tightBudget := full.snapshot.bytes - 1
	tight := buildSearchSnapshot(index, "Service", true, tightBudget, hugeBudget)
	if tight.state != SearchUnavailable {
		t.Fatalf("state=%s — 예산이 모자라면 명시적 unavailable이어야 합니다", tight.state)
	}
	if tight.reason == "" {
		t.Fatal("제외 사유가 비어 있습니다 — 잘린 검색을 완전한 검색처럼 보고했습니다")
	}
	if tight.snapshot != nil {
		t.Fatalf("예산을 넘겼는데 스냅숏을 돌려줬습니다: %d바이트", tight.snapshot.bytes)
	}
	// 실패한 빌드는 정점도 소비하지 않아야 합니다 — 먼저 먹고 나서 물러나면 상한이 아닙니다.
	if tight.peak != 0 {
		t.Fatalf("실패한 빌드가 정점 %d를 소비했습니다", tight.peak)
	}

	// 같은 입력이면 언제나 같은 결과여야 합니다.
	againFull := buildSearchSnapshot(index, "Service", true, hugeBudget, hugeBudget)
	if againFull.snapshot.bytes != full.snapshot.bytes || len(againFull.snapshot.postings) != len(full.snapshot.postings) {
		t.Fatalf("빌드가 결정적이지 않습니다: %d/%d vs %d/%d",
			full.snapshot.bytes, len(full.snapshot.postings), againFull.snapshot.bytes, len(againFull.snapshot.postings))
	}
	if againTight := buildSearchSnapshot(index, "Service", true, tightBudget, hugeBudget); againTight.state != tight.state {
		t.Fatalf("경계 판정이 결정적이지 않습니다: %s vs %s", tight.state, againTight.state)
	}
}

// TestSearchPeakAllowanceIsEnforcedBeforeAllocating — 정점 예산 경계입니다.
//
// 보유 예산이 넉넉해도 **작업용 배열까지 합한 정점**이 한도를 넘으면 빌드가 물러나야
// 합니다. 경계 바로 위/아래를 둘 다 봅니다 — 통과한 빌드는 `peak <= allowance`를
// 정확히 지키고, 실패한 빌드는 정점을 전혀 소비하지 않습니다.
func TestSearchPeakAllowanceIsEnforcedBeforeAllocating(t *testing.T) {
	index := indexOf(manyRows(300, "payments", map[string]string{"app": "application-name"})...)
	full := buildSearchSnapshot(index, "Service", true, hugeBudget, hugeBudget)
	if full.state != SearchReady || full.snapshot == nil {
		t.Fatalf("넉넉한 예산에서 실패했습니다: %s", full.state)
	}

	// 정확히 필요한 만큼만 주면 통과하고, 보고된 정점은 허용치를 넘지 않습니다.
	exact := buildSearchSnapshot(index, "Service", true, hugeBudget, full.peak)
	if exact.state != SearchReady || exact.snapshot == nil {
		t.Fatalf("필요한 정점을 그대로 줬는데 실패했습니다: %s", exact.state)
	}
	if exact.peak > full.peak {
		t.Fatalf("보고된 정점 %d가 허용치 %d를 넘었습니다", exact.peak, full.peak)
	}
	if exact.snapshot.bytes > exact.peak {
		t.Fatalf("보유 %d가 정점 %d를 넘었습니다", exact.snapshot.bytes, exact.peak)
	}

	// 1바이트라도 모자라면 반쪽 상태 없이 통째로 물러납니다.
	allowance := full.peak - 1
	result := buildSearchSnapshot(index, "Service", true, hugeBudget, allowance)
	if result.state != SearchUnavailable {
		t.Fatalf("state=%s — 정점 예산이 모자라면 명시적 unavailable이어야 합니다", result.state)
	}
	if result.reason == "" {
		t.Fatal("제외 사유가 비어 있습니다")
	}
	if result.snapshot != nil {
		t.Fatal("정점 예산을 넘겼는데 스냅숏을 돌려줬습니다")
	}
	if result.peak > allowance {
		t.Fatalf("실패한 빌드가 허용치 %d를 넘는 정점 %d를 보고했습니다", allowance, result.peak)
	}
	if result.peak != 0 {
		t.Fatalf("실패한 빌드가 정점 %d를 소비했습니다 — 먼저 먹고 물러나면 상한이 아닙니다", result.peak)
	}
}

// TestSearchBuildUsesNoUnreservedAllocation — P1-B의 회귀입니다.
//
// 빌더가 잡는 배열은 전부 preflight 계산에 들어 있어야 합니다. Go map처럼 증설
// 시점과 버킷 크기를 코드가 보장할 수 없는 자료구조를 쓰면 "하드 캡"이라고 말할 수
// 없습니다. 데이터가 커져도 할당 **횟수**가 비례해 늘지 않는다는 것으로 확인합니다.
func TestSearchBuildUsesNoUnreservedAllocation(t *testing.T) {
	small := indexOf(manyRows(50, "payments", map[string]string{"app": "checkout"})...)
	large := indexOf(manyRows(2000, "payments", map[string]string{"app": "checkout"})...)
	build := func(index *indexSnapshot) float64 {
		return testing.AllocsPerRun(3, func() {
			if r := buildSearchSnapshot(index, "Service", true, hugeBudget, hugeBudget); r.snapshot == nil {
				t.Fatalf("빌드 실패: %s", r.state)
			}
		})
	}
	smallAllocs, largeAllocs := build(small), build(large)
	// 행이 40배 늘어도 할당 횟수는 상수여야 합니다. map 버킷 증설이나 행별 정렬
	// 할당이 있으면 여기서 비례해서 늘어납니다.
	if largeAllocs > smallAllocs+8 {
		t.Fatalf("행 수에 비례하는 할당이 있습니다: %v → %v", smallAllocs, largeAllocs)
	}
}

/* ── 고정 용량 사전 (P1-Perf) ─────────────────────────────────────────────── */

func TestTokenDictResolvesCollisionsAndRefusesWhenFull(t *testing.T) {
	// 슬롯 4개에 토큰 4개 — 충돌과 선형 탐사가 반드시 일어나는 구성입니다.
	d := newTokenDict(4, 4)
	assigned := make(map[string]uint32, 4)
	for i := 0; i < 4; i++ {
		tok := fmt.Sprintf("t%d", i)
		hash, safe := hashSafeToken(tok)
		if !safe {
			t.Fatalf("%q가 안전하지 않다고 판정되었습니다", tok)
		}
		id, ok := d.intern(tok, hash)
		if !ok {
			t.Fatalf("%d번째 토큰이 들어가지 않았습니다", i)
		}
		assigned[tok] = id
	}
	if len(d.tokens) != 4 {
		t.Fatalf("distinct %d, 4여야 합니다", len(d.tokens))
	}
	// 탐사 경로가 길어져도 같은 토큰은 언제나 같은 id입니다.
	for tok, want := range assigned {
		hash, _ := hashSafeToken(tok)
		got, ok := d.intern(tok, hash)
		if !ok || got != want {
			t.Fatalf("%q: id %d(ok=%v) != %d", tok, got, ok, want)
		}
	}
	if len(d.tokens) != 4 {
		t.Fatalf("중복 intern이 사전을 늘렸습니다: %d", len(d.tokens))
	}
	// 용량을 넘기면 조용히 덮어쓰지 않고 실패를 알립니다. 빌더는 그때 물러납니다.
	hash, _ := hashSafeToken("overflow")
	if _, ok := d.intern("overflow", hash); ok {
		t.Fatal("가득 찬 테이블이 성공을 돌려줬습니다")
	}
}

func TestTokenDictIsDeterministic(t *testing.T) {
	build := func() []string {
		d := newTokenDict(dictSlots(64), 64)
		for i := 0; i < 32; i++ {
			tok := fmt.Sprintf("token-%02d", i)
			hash, _ := hashSafeToken(tok)
			d.intern(tok, hash)
			d.intern(tok, hash)
		}
		return append([]string(nil), d.tokens...)
	}
	first, second := build(), build()
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Fatalf("같은 입력이 다른 사전을 만들었습니다:\n  %v\n  %v", first, second)
	}
	if len(first) != 32 {
		t.Fatalf("distinct %d, 32여야 합니다", len(first))
	}
	// 해시는 시드가 없어야 합니다 — 실행마다 달라지면 빌드 결과가 재현되지 않습니다.
	a, _ := hashSafeToken("payments-api")
	b, _ := hashSafeToken("payments-api")
	if a != b {
		t.Fatal("같은 토큰이 다른 해시를 냈습니다")
	}
	if _, safe := hashSafeToken("Payments"); safe {
		t.Fatal("대문자 토큰이 안전하다고 판정되었습니다")
	}
}

// TestSortTokenKeysProducesLexicalOrder — 기수 정렬 결과가 사전순과 완전히 같아야
// 합니다. prefixRange가 tokens에 이분탐색을 하므로 여기가 틀리면 검색이 조용히 빕니다.
func TestSortTokenKeysProducesLexicalOrder(t *testing.T) {
	tokens := []string{
		// 앞 8바이트가 겹쳐 head만으로는 갈리지 않는 것들
		"obj-000001", "obj-000000", "obj-000002",
		// 접두사 관계
		"ab", "abc", "abcdefgh", "abcdefghi", "abcdefgha",
		// 짧은 것과 긴 것
		"a", "zz", "payments-api-with-a-much-longer-name",
		// 자리값이 전부 같아 패스를 건너뛰게 되는 짧은 묶음
		"k0", "k1", "k2",
	}
	run := func() []string {
		keys := make([]tokenSortKey, len(tokens))
		scratch := make([]tokenSortKey, len(tokens))
		for i := range keys {
			keys[i] = tokenSortKey{head: tokenHead(tokens[i]), id: uint32(i)}
		}
		sorted := sortTokenKeys(keys, scratch, tokens)
		out := make([]string, len(sorted))
		for i, k := range sorted {
			out[i] = tokens[k.id]
		}
		return out
	}
	want := append([]string(nil), tokens...)
	slices.Sort(want)

	got := run()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("사전순이 아닙니다:\n  got  %v\n  want %v", got, want)
	}
	// 같은 입력이면 언제나 같은 순서여야 합니다.
	if again := run(); strings.Join(again, ",") != strings.Join(got, ",") {
		t.Fatalf("정렬이 결정적이지 않습니다:\n  %v\n  %v", got, again)
	}
}

func TestDictSlotsStayPowerOfTwoAndHalfLoaded(t *testing.T) {
	for _, tokens := range []int64{0, 1, 15, 16, 17, 1000, 700_000} {
		slots := dictSlots(tokens)
		if slots < 16 || slots&(slots-1) != 0 {
			t.Fatalf("tokens=%d: 슬롯 %d가 2의 거듭제곱이 아닙니다", tokens, slots)
		}
		if slots < tokens*2 {
			t.Fatalf("tokens=%d: 슬롯 %d — 부하율이 0.5를 넘습니다", tokens, slots)
		}
	}
}

// TestSearchCostCountsDictionaryCapacity — 사전 슬롯은 빌드가 실제로 잡는 배열이므로
// 정점 계산에 들어 있어야 합니다. 빠지면 상한이 상한이 아닙니다.
func TestSearchCostCountsDictionaryCapacity(t *testing.T) {
	m := searchMeasurement{rows: 1000, namespaces: 2, tokens: 4000, tokenBytes: 40000, maxLabelKeys: 4, nsNameBytes: 20}
	retained, peak := searchCost(m)
	transient := peak - retained
	slotBytes := dictSlots(m.tokens) * uint32Bytes
	if transient < slotBytes {
		t.Fatalf("작업용 %d바이트에 사전 슬롯 %d바이트가 들어 있지 않습니다", transient, slotBytes)
	}
	// order·remap·사전 문자열도 함께 잡히므로 distinct 항까지 포함해야 합니다.
	if transient < slotBytes+m.tokens*traDistinctBytes {
		t.Fatalf("작업용 %d바이트가 사전(%d) + distinct(%d)에 못 미칩니다",
			transient, slotBytes, m.tokens*traDistinctBytes)
	}
}

func TestBuildDictionaryStaysWithinPreflightedCapacity(t *testing.T) {
	index := indexOf(manyRows(500, "payments", map[string]string{"app": "checkout", "tier": "web"})...)
	m := measureSearchInput(index, "service", true)
	result := buildSearchSnapshot(index, "Service", true, hugeBudget, hugeBudget)
	if result.snapshot == nil {
		t.Fatalf("빌드 실패: %s", result.state)
	}
	if int64(len(result.snapshot.tokens)) > m.tokens {
		t.Fatalf("distinct %d가 등장 횟수 상한 %d를 넘었습니다", len(result.snapshot.tokens), m.tokens)
	}
	// 중복이 실제로 접혔는지 — 접히지 않으면 정렬 비용을 줄인 의미가 없습니다.
	if int64(len(result.snapshot.tokens)) >= m.tokens {
		t.Fatalf("distinct %d가 등장 횟수 %d와 같습니다 — 중복 제거가 동작하지 않았습니다",
			len(result.snapshot.tokens), m.tokens)
	}
}

/* ── cursor ──────────────────────────────────────────────────────────────── */

func TestSearchCursorRoundTripAndRejection(t *testing.T) {
	fp := searchFingerprint("prod-seoul", "pay", false, []string{"payments"})
	key := searchCursorKey{mode: cursorModeIndex, token: "payments-api", namespace: "payments", name: "payments-api", gvr: "core/v1/services", uid: "uid-1"}
	encoded := encodeSearchCursor(key, fp)
	if len(encoded) > MaxSearchCursorLen {
		t.Fatalf("cursor가 %d자입니다 — 상한은 %d입니다", len(encoded), MaxSearchCursorLen)
	}
	got, err := decodeSearchCursor(encoded, fp, cursorModeIndex)
	if err != nil || got != key {
		t.Fatalf("왕복이 깨졌습니다: %+v %v", got, err)
	}
	for _, other := range []string{
		searchFingerprint("prod-seoul", "payx", false, []string{"payments"}),
		searchFingerprint("prod-seoul", "pay", true, nil),
		searchFingerprint("prod-seoul", "pay", false, []string{"payments", "search"}),
		searchFingerprint("other-cluster", "pay", false, []string{"payments"}),
	} {
		if _, err := decodeSearchCursor(encoded, other, cursorModeIndex); err != ErrInvalidCursor {
			t.Fatalf("다른 지문의 cursor를 받아들였습니다: %v", err)
		}
	}
	for _, bad := range []string{"!!!!", strings.Repeat("a", MaxSearchCursorLen+1)} {
		if _, err := decodeSearchCursor(bad, fp, cursorModeIndex); err != ErrInvalidCursor {
			t.Fatalf("잘못된 cursor %q를 받아들였습니다", bad)
		}
	}
	// **모드가 다르면 거절합니다.** Scope가 바뀌면 결과 순서가 바뀌므로, 색인 순서의
	// 위치를 순회 경로에 그대로 이어 붙이면 중복·누락이 생깁니다.
	if _, err := decodeSearchCursor(encoded, fp, cursorModeScan); err != ErrInvalidCursor {
		t.Fatalf("색인 cursor를 순회 모드로 받아들였습니다: %v", err)
	}

	// 순회 모드는 토큰을 쓰지 않고 label 카운터와 누락 비트가 의미를 가집니다.
	scan := searchCursorKey{mode: cursorModeScan, namespace: "payments", name: "payments-api",
		gvr: "core/v1/services", uid: "uid-1", nsLabelTokens: 1234, scanFlags: scanFlagLabelNs | scanFlagForbidden}
	scanEncoded := encodeSearchCursor(scan, fp)
	back, err := decodeSearchCursor(scanEncoded, fp, cursorModeScan)
	if err != nil || back != scan {
		t.Fatalf("순회 cursor 왕복이 깨졌습니다: %+v %v", back, err)
	}
	if _, err := decodeSearchCursor(scanEncoded, fp, cursorModeIndex); err != ErrInvalidCursor {
		t.Fatalf("순회 cursor를 색인 모드로 받아들였습니다: %v", err)
	}
	// 버전 1 형식(모드·카운터·비트 없음)은 해석하지 않습니다.
	v1 := base64.RawURLEncoding.EncodeToString([]byte(strings.Join(
		[]string{"1", fp, "payments-api", "payments", "payments-api", "core/v1/services", "uid-1"}, cursorSep)))
	if _, err := decodeSearchCursor(v1, fp, cursorModeIndex); err != ErrInvalidCursor {
		t.Fatalf("버전 1 cursor를 받아들였습니다: %v", err)
	}
}

// TestSearchCursorRejectsForgedFields — 서버가 만들 수 없는 조합은 전부 거절합니다.
func TestSearchCursorRejectsForgedFields(t *testing.T) {
	fp := searchFingerprint("prod-seoul", "pay", false, []string{"payments"})
	forge := func(parts ...string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, cursorSep)))
	}
	cases := []struct {
		name     string
		encoded  string
		wantMode string
	}{{
		// 순회 cursor의 namespace는 실재하는 허용 namespace라 빈 값일 수 없습니다.
		// 빈 값을 받아들이면 위조 cursor가 namespace 경계 밖에서 재개를 시도합니다.
		name:     "순회 모드 빈 namespace",
		encoded:  forge("2", fp, "s", "", "", "payments-api", "core/v1/services", "uid-1", "0", "0"),
		wantMode: cursorModeScan,
	}, {
		name:     "색인 모드에 누락 비트",
		encoded:  forge("2", fp, "i", "payments", "payments", "payments-api", "core/v1/services", "uid-1", "0", "1"),
		wantMode: cursorModeIndex,
	}, {
		name:     "색인 모드에 label 카운터",
		encoded:  forge("2", fp, "i", "payments", "payments", "payments-api", "core/v1/services", "uid-1", "7", "0"),
		wantMode: cursorModeIndex,
	}, {
		name:     "정의되지 않은 누락 비트",
		encoded:  forge("2", fp, "s", "", "payments", "payments-api", "core/v1/services", "uid-1", "0", "4294967295"),
		wantMode: cursorModeScan,
	}, {
		name:     "상한을 넘는 label 카운터",
		encoded:  forge("2", fp, "s", "", "payments", "payments-api", "core/v1/services", "uid-1", "999999999", "0"),
		wantMode: cursorModeScan,
	}, {
		name:     "순회 모드에 토큰",
		encoded:  forge("2", fp, "s", "payments", "payments", "payments-api", "core/v1/services", "uid-1", "0", "0"),
		wantMode: cursorModeScan,
	}, {
		name:     "알 수 없는 모드",
		encoded:  forge("2", fp, "x", "", "payments", "payments-api", "core/v1/services", "uid-1", "0", "0"),
		wantMode: "x",
	}}
	for _, tc := range cases {
		if _, err := decodeSearchCursor(tc.encoded, fp, tc.wantMode); err != ErrInvalidCursor {
			t.Errorf("%s: 위조 cursor를 받아들였습니다: %v", tc.name, err)
		}
	}

	// 색인 모드는 클러스터 범위 리소스 때문에 빈 namespace가 정상입니다.
	clusterScoped := searchCursorKey{mode: cursorModeIndex, token: "fast", name: "fast-ssd",
		gvr: "storage.k8s.io/v1/storageclasses", uid: "uid-sc"}
	encoded := encodeSearchCursor(clusterScoped, fp)
	if got, err := decodeSearchCursor(encoded, fp, cursorModeIndex); err != nil || got != clusterScoped {
		t.Fatalf("클러스터 범위 색인 cursor가 거절되었습니다: %+v %v", got, err)
	}
}

func TestSearchCursorMaxLengthCoversLargestRealKey(t *testing.T) {
	fp := searchFingerprint("prod-seoul", "aa", true, nil)
	largest := []searchCursorKey{{
		mode:      cursorModeIndex,
		token:     strings.Repeat("a", tokenPrefixBytes),
		namespace: strings.Repeat("n", k8sMaxNamespaceLen),
		name:      strings.Repeat("m", k8sMaxNameLen),
		gvr:       strings.Repeat("g", 200) + "/v1alpha1/" + strings.Repeat("r", 63),
		uid:       strings.Repeat("u", maxCursorUIDLen),
	}, {
		mode:          cursorModeScan,
		namespace:     strings.Repeat("n", k8sMaxNamespaceLen),
		name:          strings.Repeat("m", k8sMaxNameLen),
		gvr:           strings.Repeat("g", 200) + "/v1alpha1/" + strings.Repeat("r", 63),
		uid:           strings.Repeat("u", maxCursorUIDLen),
		nsLabelTokens: MaxLabelTokensPerNamespace,
		scanFlags:     scanFlagAll,
	}}
	for _, key := range largest {
		encoded := encodeSearchCursor(key, fp)
		if len(encoded) > MaxSearchCursorLen {
			t.Fatalf("최대 크기 cursor가 %d자입니다 — 계약 상한 %d를 넘습니다", len(encoded), MaxSearchCursorLen)
		}
		if got, err := decodeSearchCursor(encoded, fp, key.mode); err != nil || got != key {
			t.Fatalf("최대 크기 cursor를 다시 읽지 못했습니다: %v", err)
		}
	}
}

func TestSearchSeekAfterSkipsExactlyThePassedEntries(t *testing.T) {
	snap := searchFixture(t, hugeBudget,
		metaRow("payments", "payments-a", "uid-1", nil),
		metaRow("payments", "payments-b", "uid-2", nil),
		metaRow("payments", "payments-c", "uid-3", nil),
	)
	loID, hiID, ok := snap.prefixRange("payments-")
	if !ok {
		t.Fatal("접두사 구간을 찾지 못했습니다")
	}
	lo, end, _ := snap.postingRange(0, loID, hiID)
	st := &searchStream{gvrKey: "core/v1/services", snap: snap, ns: "payments", loID: loID, hiID: hiID, pos: lo, end: end}
	cursor := searchCursorKey{mode: cursorModeIndex, token: "payments-a", namespace: "payments", name: "payments-a", gvr: "core/v1/services", uid: "uid-1"}
	st.pos = snap.seekAfter(st, cursor)
	if !st.load() || st.curName != "payments-b" {
		t.Fatalf("cursor 다음 위치가 아닙니다: %q", st.curName)
	}
}

/* ── 벤치마크 (P1-2) ──────────────────────────────────────────────────────
   go test ./internal/resourcecatalog -run XXX -bench 'Resource(List|Search|Combined)' -benchmem -count=5 */

func benchObjects100k() []any {
	namespaces := []string{"alpha", "beta", "gamma", "delta"}
	objs := make([]any, 0, 100_000)
	for _, ns := range namespaces {
		for i := 0; i < 25_000; i++ {
			objs = append(objs, metaRow(ns, fmt.Sprintf("obj-%06d", i), fmt.Sprintf("uid-%s-%06d", ns, i),
				map[string]string{"app": fmt.Sprintf("app-%04d", i%1000), "tier": "web"}))
		}
	}
	return objs
}

// BenchmarkResourceListIndexBuild100kBaseline은 검색 이전의 목록 인덱스 비용입니다.
// 아래 Combined와 짝으로 읽어야 검색이 추가한 비용이 드러납니다.
func BenchmarkResourceListIndexBuild100kBaseline(b *testing.B) {
	objs := benchObjects100k()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if built := buildIndexSnapshot(objs, indexBase); len(built.rows) != len(objs) {
			b.Fatalf("rows=%d", len(built.rows))
		}
	}
}

// BenchmarkResourceCombinedIndexBuild100k은 목록 + 검색 인덱스를 한 번에 만드는 비용입니다.
// 재구성 경로는 이것 하나뿐이므로 이 값이 배경 루프가 지불하는 전부입니다.
func benchBudgets() (retained, peak int64) {
	retained = int64(DefaultMaxSearchIndexBytes) / searchPerResourceDivisor
	peak = int64(searchPeakMultiplier) * DefaultMaxSearchIndexBytes
	return retained, peak
}

func BenchmarkResourceCombinedIndexBuild100k(b *testing.B) {
	objs := benchObjects100k()
	budget, peak := benchBudgets()
	var retained int64
	var postings int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		index := buildIndexSnapshot(objs, indexBase)
		result := buildSearchSnapshot(index, "Service", true, budget, peak)
		if result.snapshot == nil {
			b.Fatalf("state=%s reason=%s", result.state, result.reason)
		}
		if result.peak > peak {
			b.Fatalf("정점 %d가 허용치 %d를 넘었습니다", result.peak, peak)
		}
		retained, postings = result.snapshot.bytes, len(result.snapshot.postings)
	}
	b.StopTimer()
	b.ReportMetric(float64(retained), "retained_bytes")
	b.ReportMetric(float64(retained)/float64(len(objs)), "retained_B/object")
	b.ReportMetric(float64(postings)/float64(len(objs)), "postings/object")
}

// benchObjects100kHighCardinality는 **중복 제거가 거의 듣지 않는** 10만 객체입니다.
//
// 이름도, label 키도, label 값도 객체마다 다릅니다(revision·checksum 라벨을 쓰는
// 클러스터가 실제로 이렇습니다). 그래서 distinct 토큰 수가 등장 횟수에 육박하고,
// distinct만 정렬한다는 설계의 이점이 가장 작아지는 최악의 입력이 됩니다.
// 등장 7 / 신규 distinct 5 per row → D/P ≈ 0.71, D ≈ 50만.
func benchObjects100kHighCardinality() []any {
	namespaces := []string{"alpha", "beta", "gamma", "delta"}
	objs := make([]any, 0, 100_000)
	for n, ns := range namespaces {
		for i := 0; i < 25_000; i++ {
			id := n*25_000 + i
			objs = append(objs, metaRow(ns,
				fmt.Sprintf("obj-%06d", id),
				fmt.Sprintf("uid-%s-%06d", ns, i),
				map[string]string{
					fmt.Sprintf("k0-%06d", id): fmt.Sprintf("v0-%06d", id),
					fmt.Sprintf("k1-%06d", id): fmt.Sprintf("v1-%06d", id),
				}))
		}
	}
	return objs
}

// BenchmarkResourceCombinedIndexBuild100kHighCardinality — 위 최악 입력의 재구성 비용입니다.
// 위 BenchmarkResourceCombinedIndexBuild100k와 같은 지표를 보고해 나란히 읽습니다.
func BenchmarkResourceCombinedIndexBuild100kHighCardinality(b *testing.B) {
	objs := benchObjects100kHighCardinality()
	budget, peak := benchBudgets()
	var retained int64
	var postings, distinct int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		index := buildIndexSnapshot(objs, indexBase)
		result := buildSearchSnapshot(index, "Service", true, budget, peak)
		if result.snapshot == nil {
			b.Fatalf("state=%s reason=%s", result.state, result.reason)
		}
		if result.peak > peak {
			b.Fatalf("정점 %d가 허용치 %d를 넘었습니다", result.peak, peak)
		}
		retained = result.snapshot.bytes
		postings, distinct = len(result.snapshot.postings), len(result.snapshot.tokens)
	}
	b.StopTimer()
	b.ReportMetric(float64(retained), "retained_bytes")
	b.ReportMetric(float64(retained)/float64(len(objs)), "retained_B/object")
	b.ReportMetric(float64(postings)/float64(len(objs)), "postings/object")
	// distinct/postings가 1에 가까울수록 정렬 부담이 큽니다. 이 값이 떨어지면
	// 픽스처가 조용히 카디널리티를 잃은 것이므로 함께 봅니다.
	b.ReportMetric(float64(distinct)/float64(postings), "distinct/posting")
}

func benchSearchSnapshot100k(b *testing.B) *searchSnapshot {
	b.Helper()
	index := buildIndexSnapshot(benchObjects100k(), indexBase)
	budget, peak := benchBudgets()
	result := buildSearchSnapshot(index, "Service", true, budget, peak)
	if result.snapshot == nil {
		b.Fatalf("state=%s reason=%s", result.state, result.reason)
	}
	return result.snapshot
}

// collectN은 한 페이지(최대 n건)만 모읍니다. 실제 Search가 스트림을 병합하며
// 상한에서 멈추는 것과 같은 유계 경로입니다.
func collectN(snap *searchSnapshot, query string, n int) int {
	loID, hiID, ok := snap.prefixRange(query)
	if !ok {
		return 0
	}
	found := 0
	for ns := range snap.nsNames {
		lo, end, ok := snap.postingRange(ns, loID, hiID)
		if !ok {
			continue
		}
		for i := lo; i < end && found < n; i++ {
			p := snap.postings[i]
			if id, _, ok := snap.matchInRange(p.row, loID, hiID); ok && id == p.token {
				found++
			}
		}
		if found >= n {
			break
		}
	}
	return found
}

// BenchmarkResourceSearchNarrowQuery100k은 좁은 접두사 한 페이지 비용입니다.
func BenchmarkResourceSearchNarrowQuery100k(b *testing.B) {
	snap := benchSearchSnapshot100k(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if collectN(snap, "obj-012345", MaxSearchPageSize) == 0 {
			b.Fatal("결과가 없습니다")
		}
	}
}

// BenchmarkResourceSearchWideQuery100k은 2자 접두사(가장 넓은 질의) 한 페이지 비용입니다.
// 결과 수가 아니라 페이지 상한이 비용을 정한다는 것을 보이는 것이 목적입니다.
func BenchmarkResourceSearchWideQuery100k(b *testing.B) {
	snap := benchSearchSnapshot100k(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if collectN(snap, "ob", MaxSearchPageSize) != MaxSearchPageSize {
			b.Fatal("한 페이지를 채우지 못했습니다")
		}
	}
}
