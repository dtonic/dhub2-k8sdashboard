package resourcecatalog

// 인덱스와 keyset 페이지의 성질을 검증합니다.
//
// 가장 중요한 두 가지입니다.
//   - 중복도 누락도 없이 전체를 순회한다.
//   - **Scope 밖 namespace는 한 줄도 나오지 않는다.**

import (
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

var indexBase = time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC)

// 픽스처는 **Kubernetes가 실제로 돌려줄 수 있는 크기**만 만듭니다.
// 이름 253자·namespace 63자를 넘는 행을 지어내면 cursor가 주소를 지정할 수 없는,
// 클러스터에 존재할 수 없는 상태를 검증하게 됩니다.
const (
	k8sMaxNameLen      = 253
	k8sMaxNamespaceLen = 63
)

type fixtureOptions struct {
	labels bool
	suffix string
}

func testSnapshot(namespaces []string, perNamespace int, opts fixtureOptions) *indexSnapshot {
	objs := make([]any, 0, len(namespaces)*perNamespace)
	for _, ns := range namespaces {
		if len(ns) > k8sMaxNamespaceLen {
			panic("픽스처 namespace가 Kubernetes 상한을 넘었습니다: " + ns)
		}
		for i := 0; i < perNamespace; i++ {
			name := fmt.Sprintf("obj-%06d%s", i, opts.suffix)
			if len(name) > k8sMaxNameLen {
				panic("픽스처 이름이 Kubernetes 상한을 넘었습니다: " + fmt.Sprint(len(name)))
			}
			meta := metav1.ObjectMeta{
				Namespace:         ns,
				Name:              name,
				UID:               types.UID(fmt.Sprintf("uid-%s-%06d", ns, i)),
				CreationTimestamp: metav1.NewTime(indexBase),
			}
			if opts.labels {
				tier := "web"
				if i%2 == 1 {
					tier = "batch"
				}
				meta.Labels = map[string]string{"tier": tier}
			}
			objs = append(objs, &metav1.PartialObjectMetadata{ObjectMeta: meta})
		}
	}
	return buildIndexSnapshot(objs, indexBase)
}

func baseRequest(limit int) resolvedRequest {
	return resolvedRequest{limit: limit, spanAll: true, fingerprint: "fixture0", maxBytes: MaxResponseBytes}
}

// pageAll은 cursor를 따라 끝까지 순회하며 방출된 키를 모읍니다.
func pageAll(t *testing.T, snap *indexSnapshot, req resolvedRequest) []string {
	t.Helper()
	var keys []string
	// 인코딩된 cursor는 ListRequest(바깥 계약)의 값이고, resolvedRequest는 이미
	// 해석된 위치만 들고 있습니다. 순회 진행 여부는 여기서 직접 추적합니다.
	previous := ""
	guard := 0
	for {
		guard++
		if guard > 10_000 {
			t.Fatal("cursor 순회가 끝나지 않았습니다 — 진행이 없는 페이지가 있습니다")
		}
		page := snap.page(req)
		for _, item := range page.Items {
			keys = append(keys, item.Namespace+"/"+item.Name)
		}
		if page.NextCursor == "" {
			return keys
		}
		if page.NextCursor == previous {
			t.Fatal("같은 cursor가 반복되었습니다 — 페이지가 전진하지 않습니다")
		}
		key, err := decodeCursor(page.NextCursor, req.fingerprint)
		if err != nil {
			t.Fatalf("서버가 만든 cursor를 다시 읽지 못했습니다: %v", err)
		}
		previous = page.NextCursor
		req.cursor, req.hasCursor = key, true
	}
}

func TestPagingCoversEveryRowExactlyOnce(t *testing.T) {
	snap := testSnapshot([]string{"media", "payments"}, 137, fixtureOptions{})
	got := pageAll(t, snap, baseRequest(7))
	if len(got) != len(snap.rows) {
		t.Fatalf("방출된 행 %d개, 인덱스 %d개", len(got), len(snap.rows))
	}
	seen := make(map[string]bool, len(got))
	for i, key := range got {
		if seen[key] {
			t.Fatalf("중복 행: %s", key)
		}
		seen[key] = true
		want := snap.rows[i].namespace + "/" + snap.rows[i].name
		if key != want {
			t.Fatalf("정렬이 깨졌습니다 %d: got=%s want=%s", i, key, want)
		}
	}
}

func TestDescendingPagingIsTheExactReverse(t *testing.T) {
	snap := testSnapshot([]string{"media", "payments"}, 41, fixtureOptions{})
	asc := pageAll(t, snap, baseRequest(9))
	req := baseRequest(9)
	req.descending = true
	desc := pageAll(t, snap, req)
	if len(asc) != len(desc) {
		t.Fatalf("정방향 %d행, 역방향 %d행", len(asc), len(desc))
	}
	for i := range asc {
		if asc[i] != desc[len(desc)-1-i] {
			t.Fatalf("역방향 순서가 다릅니다 %d: %s vs %s", i, asc[i], desc[len(desc)-1-i])
		}
	}
}

// TestScopeSpansNeverLeakOtherNamespaces — Scope 밖 namespace는 한 줄도 나오면 안 됩니다.
func TestScopeSpansNeverLeakOtherNamespaces(t *testing.T) {
	snap := testSnapshot([]string{"media", "payments", "secret-ops"}, 30, fixtureOptions{})
	req := baseRequest(11)
	req.spanAll = false
	req.namespaces = []string{"payments"}
	for _, key := range pageAll(t, snap, req) {
		if !strings.HasPrefix(key, "payments/") {
			t.Fatalf("Scope 밖 행이 나왔습니다: %s", key)
		}
	}
}

// TestEmptyScopeReturnsNothing — 볼 수 있는 namespace가 없으면 "전체"가 아니라 0건입니다.
// 여기가 뒤집히면 권한 없는 사용자가 클러스터 전체를 보게 됩니다.
func TestEmptyScopeReturnsNothing(t *testing.T) {
	snap := testSnapshot([]string{"media", "payments"}, 5, fixtureOptions{})
	req := baseRequest(50)
	req.spanAll = false
	req.namespaces = nil
	page := snap.page(req)
	if len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("빈 Scope가 %d행을 냈습니다 (cursor=%q)", len(page.Items), page.NextCursor)
	}
}

func TestNamePrefixAndLabelSelectorFilters(t *testing.T) {
	snap := testSnapshot([]string{"payments"}, 40, fixtureOptions{labels: true})
	req := baseRequest(50)
	req.namePrefix = "obj-00001"
	page := snap.page(req)
	for _, item := range page.Items {
		if !strings.HasPrefix(item.Name, "obj-00001") {
			t.Fatalf("prefix 필터가 새었습니다: %s", item.Name)
		}
	}
	if len(page.Items) != 10 {
		t.Fatalf("prefix obj-00001 행 %d개 (want 10)", len(page.Items))
	}

	selector, err := labels.Parse("tier=batch")
	if err != nil {
		t.Fatal(err)
	}
	req = baseRequest(50)
	req.selector = selector
	page = snap.page(req)
	if len(page.Items) != 20 {
		t.Fatalf("label 필터 행 %d개 (want 20)", len(page.Items))
	}
}

// TestFilteredPagingMakesProgressWithinScanBudget — 필터가 거의 걸리지 않아도
// scan 예산 안에서 cursor가 반드시 전진해야 합니다. 아니면 무한 루프입니다.
func TestFilteredPagingMakesProgressWithinScanBudget(t *testing.T) {
	snap := testSnapshot([]string{"payments"}, 12_000, fixtureOptions{})
	req := baseRequest(5)
	req.namePrefix = "obj-011999" // 마지막 한 건만 통과합니다.
	page := snap.page(req)
	if page.NextCursor == "" && len(page.Items) == 0 {
		t.Fatal("scan 예산에 걸렸는데 cursor가 없습니다 — 나머지를 볼 방법이 사라집니다")
	}
	got := pageAll(t, snap, req)
	if len(got) != 1 || got[0] != "payments/obj-011999" {
		t.Fatalf("필터 결과 %v", got)
	}
}

// TestMaxPageStaysWellUnderTheByteLimit — 200행 상한이 이미 1MiB를 보장합니다.
func TestMaxPageStaysWellUnderTheByteLimit(t *testing.T) {
	name := strings.Repeat("n", 240) // 실제 Kubernetes 이름 상한(253)에 가까운 값
	snap := testSnapshot([]string{"payments"}, MaxPageSize, fixtureOptions{suffix: "-" + name})
	page := snap.page(baseRequest(MaxPageSize))
	if len(page.Items) != MaxPageSize {
		t.Fatalf("행 %d개 (want %d)", len(page.Items), MaxPageSize)
	}
	total := 0
	for _, item := range page.Items {
		total += rowOverheadBytes + len(item.Namespace) + len(item.Name)
	}
	if total > MaxResponseBytes {
		t.Fatalf("최대 페이지 추정 크기가 상한을 넘었습니다: %d", total)
	}
}

// TestByteBudgetTruncatesAndKeepsACursor — byte 예산에 걸려 페이지가 잘려도
// 남은 행을 볼 cursor가 반드시 함께 나가고, 그 cursor로 전체를 빠짐없이 이어볼 수 있습니다.
//
// 행은 Kubernetes가 실제로 돌려줄 수 있는 최대 크기(이름 253자)입니다. 그 크기로는
// 200행 상한이 이미 1MiB를 보장하므로(위 테스트), 예산 경로 자체는 요청별 예산을
// 작게 잡아 검증합니다. 프로덕션 예산은 resolve()가 항상 MaxResponseBytes로 넣고
// page()가 다시 조입니다 — 아래 TestResponseBudgetIsAlwaysClampedToTheHardLimit 참고.
func TestByteBudgetTruncatesAndKeepsACursor(t *testing.T) {
	// "obj-%06d"(10자) + "-" + 242자 = 253자, Kubernetes 이름 상한과 정확히 같습니다.
	snap := testSnapshot([]string{"payments"}, MaxPageSize, fixtureOptions{suffix: "-" + strings.Repeat("n", 242)})
	if got := len(snap.rows[0].name); got != k8sMaxNameLen {
		t.Fatalf("픽스처 이름 길이=%d (want %d)", got, k8sMaxNameLen)
	}

	const budget = 4096
	req := baseRequest(MaxPageSize)
	req.maxBytes = budget
	page := snap.page(req)
	if len(page.Items) == 0 || len(page.Items) >= MaxPageSize {
		t.Fatalf("byte 예산이 적용되지 않았습니다: %d행", len(page.Items))
	}
	if !page.Truncated || page.NextCursor == "" {
		t.Fatalf("잘린 페이지에 cursor가 없습니다 (truncated=%v)", page.Truncated)
	}
	// 서버가 만든 cursor는 언제나 서버가 다시 읽을 수 있어야 합니다.
	if _, err := decodeCursor(page.NextCursor, req.fingerprint); err != nil {
		t.Fatalf("서버가 만든 cursor를 다시 읽지 못했습니다: %v", err)
	}
	total := 0
	for _, item := range page.Items {
		total += rowOverheadBytes + len(item.Namespace) + len(item.Name)
	}
	if total > budget {
		t.Fatalf("응답 추정 크기가 예산을 넘었습니다: %d > %d", total, budget)
	}

	// 잘린 뒤 이어보기가 전체를 중복·누락 없이 덮어야 합니다.
	got := pageAll(t, snap, req)
	if len(got) != len(snap.rows) {
		t.Fatalf("이어보기로 %d행만 나왔습니다 (want %d)", len(got), len(snap.rows))
	}
	seen := make(map[string]bool, len(got))
	for i, key := range got {
		if seen[key] {
			t.Fatalf("중복 행: %s", key)
		}
		seen[key] = true
		if want := snap.rows[i].namespace + "/" + snap.rows[i].name; key != want {
			t.Fatalf("이어보기 순서가 깨졌습니다 %d: got=%s want=%s", i, key, want)
		}
	}
}

// TestResponseBudgetIsAlwaysClampedToTheHardLimit — 요청별 예산 seam이 있어도
// 프로덕션 상한은 언제나 1MiB입니다. 예산을 키워 상한을 넘길 수 없습니다.
func TestResponseBudgetIsAlwaysClampedToTheHardLimit(t *testing.T) {
	for name, req := range map[string]resolvedRequest{
		"미지정":    {limit: 10},
		"0":      {limit: 10, maxBytes: 0},
		"음수":     {limit: 10, maxBytes: -1},
		"상한 초과":  {limit: 10, maxBytes: MaxResponseBytes * 4},
		"상한과 같음": {limit: 10, maxBytes: MaxResponseBytes},
	} {
		if got := req.responseBudget(); got != MaxResponseBytes {
			t.Errorf("%s: 예산=%d (want %d)", name, got, MaxResponseBytes)
		}
	}
	small := resolvedRequest{limit: 10, maxBytes: 4096}
	if got := small.responseBudget(); got != 4096 {
		t.Fatalf("작은 예산이 무시되었습니다: %d", got)
	}
}

// TestCursorAddressesEveryValidKubernetesIdentity — cursor 상한이 Kubernetes 신원
// 상한을 덮는지 확인합니다. 이것이 참인 한, 인덱스의 어떤 행에서 만든 cursor도
// 반드시 다시 읽힙니다 — 프로덕션에서 "읽을 수 없는 cursor"가 나올 수 없는 근거입니다.
func TestCursorAddressesEveryValidKubernetesIdentity(t *testing.T) {
	if maxCursorNSLen < k8sMaxNamespaceLen || maxCursorName < k8sMaxNameLen {
		t.Fatalf("cursor 상한이 Kubernetes 신원 상한보다 좁습니다: ns=%d name=%d", maxCursorNSLen, maxCursorName)
	}
	key := cursorKey{
		namespace: strings.Repeat("n", k8sMaxNamespaceLen),
		name:      strings.Repeat("o", k8sMaxNameLen),
	}
	encoded := encodeCursor(key, "fixture0")
	if len(encoded) > maxCursorLen {
		t.Fatalf("최대 신원의 cursor 길이가 상한을 넘습니다: %d > %d", len(encoded), maxCursorLen)
	}
	got, err := decodeCursor(encoded, "fixture0")
	if err != nil {
		t.Fatalf("최대 크기 신원의 cursor를 읽지 못했습니다: %v", err)
	}
	if got != key {
		t.Fatalf("round-trip 결과가 다릅니다: %+v", got)
	}
}

// TestEveryGeneratedCursorDecodesWhilePagingMaxSizedRows — 실제 최대 크기 행으로
// 끝까지 페이지를 넘기면서, 서버가 만든 모든 cursor가 다시 읽히는지 확인합니다.
func TestEveryGeneratedCursorDecodesWhilePagingMaxSizedRows(t *testing.T) {
	snap := testSnapshot([]string{strings.Repeat("p", k8sMaxNamespaceLen)}, 37,
		fixtureOptions{suffix: "-" + strings.Repeat("n", 242)})
	req := baseRequest(4)
	cursors := 0
	for {
		if cursors > 1_000 {
			t.Fatal("cursor 순회가 끝나지 않았습니다 — 페이지가 전진하지 않습니다")
		}
		page := snap.page(req)
		if page.NextCursor == "" {
			break
		}
		cursors++
		key, err := decodeCursor(page.NextCursor, req.fingerprint)
		if err != nil {
			t.Fatalf("%d번째 cursor를 읽지 못했습니다: %v", cursors, err)
		}
		req.cursor, req.hasCursor = key, true
	}
	if cursors == 0 {
		t.Fatal("cursor가 한 번도 나오지 않아 검증한 것이 없습니다")
	}
}

func TestCursorRejectsForeignFingerprintAndGarbage(t *testing.T) {
	snap := testSnapshot([]string{"payments"}, 10, fixtureOptions{})
	page := snap.page(baseRequest(3))
	if page.NextCursor == "" {
		t.Fatal("cursor가 없습니다")
	}
	if _, err := decodeCursor(page.NextCursor, "different"); err == nil {
		t.Fatal("다른 질의의 cursor가 통과했습니다 — 중복·누락이 조용히 생깁니다")
	}
	for _, bad := range []string{"!!!!", "", strings.Repeat("A", maxCursorLen+1), "YWJj"} {
		if _, err := decodeCursor(bad, "fixture0"); err == nil {
			t.Fatalf("잘못된 cursor가 통과했습니다: %q", bad)
		}
	}
}

func TestCursorRoundTripKeepsExactPosition(t *testing.T) {
	key := cursorKey{namespace: "payments", name: "obj-000042"}
	encoded := encodeCursor(key, "fixture0")
	got, err := decodeCursor(encoded, "fixture0")
	if err != nil {
		t.Fatal(err)
	}
	if got != key {
		t.Fatalf("cursor round-trip 실패: %+v", got)
	}
}

func TestFingerprintChangesWithFilterAndScope(t *testing.T) {
	base := fingerprint("core/v1/services", ListRequest{}, []string{"payments"}, false)
	cases := map[string]string{
		"scope":    fingerprint("core/v1/services", ListRequest{}, []string{"media"}, false),
		"all":      fingerprint("core/v1/services", ListRequest{}, []string{"payments"}, true),
		"gvr":      fingerprint("core/v1/secrets", ListRequest{}, []string{"payments"}, false),
		"prefix":   fingerprint("core/v1/services", ListRequest{NamePrefix: "a"}, []string{"payments"}, false),
		"selector": fingerprint("core/v1/services", ListRequest{LabelSelector: "a=b"}, []string{"payments"}, false),
		"order":    fingerprint("core/v1/services", ListRequest{Descending: true}, []string{"payments"}, false),
	}
	for name, got := range cases {
		if got == base {
			t.Fatalf("%s가 바뀌었는데 지문이 같습니다", name)
		}
	}
}

// TestPageCostDoesNotGrowWithIndexSize — 요청마다 전체 정렬·복사·순회를 하면
// 인덱스가 100배 커질 때 페이지 비용도 같이 커집니다. 여기가 그 회귀를 잡습니다.
func TestPageCostDoesNotGrowWithIndexSize(t *testing.T) {
	small := testSnapshot([]string{"a", "b", "c", "d"}, 250, fixtureOptions{})    // 1k
	large := testSnapshot([]string{"a", "b", "c", "d"}, 25_000, fixtureOptions{}) // 100k
	req := baseRequest(DefaultPageSize)
	smallAllocs := testing.AllocsPerRun(10, func() { _ = small.page(req) })
	largeAllocs := testing.AllocsPerRun(10, func() { _ = large.page(req) })
	if largeAllocs > smallAllocs*2+16 {
		t.Fatalf("인덱스가 100배 커지자 페이지 할당이 %v → %v로 늘었습니다", smallAllocs, largeAllocs)
	}
}

/* ── 벤치마크 ─────────────────────────────────────────────────────────────
   go test ./internal/resourcecatalog -run XXX -bench 'Resource' -benchmem -count=5 */

func benchSnapshot100k() *indexSnapshot {
	return testSnapshot([]string{"alpha", "beta", "gamma", "delta"}, 25_000, fixtureOptions{})
}

// BenchmarkResourceListPage100k — 100k 인덱스에서 필터 없는 한 페이지 비용입니다.
func BenchmarkResourceListPage100k(b *testing.B) {
	snap := benchSnapshot100k()
	req := baseRequest(DefaultPageSize)
	req.cursor = cursorKey{namespace: "gamma", name: "obj-012500"}
	req.hasCursor = true
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page := snap.page(req)
		if len(page.Items) != DefaultPageSize {
			b.Fatalf("page=%d", len(page.Items))
		}
	}
}

// BenchmarkResourceListFullScan100k — 100k 전체를 최대 페이지로 훑는 총비용입니다.
func BenchmarkResourceListFullScan100k(b *testing.B) {
	snap := benchSnapshot100k()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := baseRequest(MaxPageSize)
		rows := 0
		for {
			page := snap.page(req)
			rows += len(page.Items)
			if page.NextCursor == "" {
				break
			}
			key, err := decodeCursor(page.NextCursor, req.fingerprint)
			if err != nil {
				b.Fatal(err)
			}
			req.cursor, req.hasCursor = key, true
		}
		if rows != len(snap.rows) {
			b.Fatalf("rows=%d want=%d", rows, len(snap.rows))
		}
	}
}

// BenchmarkResourceIndexBuild100k — 인덱스 재구성 비용입니다.
// 이 비용은 요청이 아니라 배경 루프가 지불합니다.
func BenchmarkResourceIndexBuild100k(b *testing.B) {
	snap := benchSnapshot100k()
	objs := make([]any, 0, len(snap.rows))
	for i := range snap.rows {
		objs = append(objs, snap.rows[i].obj)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if built := buildIndexSnapshot(objs, indexBase); len(built.rows) != len(objs) {
			b.Fatalf("rows=%d", len(built.rows))
		}
	}
}
