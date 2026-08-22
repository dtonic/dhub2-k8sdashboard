package resourcecatalog

// 범위 제한 순회의 경계 검증입니다. (P1-Scope)
//
// 범위가 제한된 Scope는 **색인 상태와 무관하게 언제나** 목록 스냅숏의 허용 namespace
// 구간만 훑습니다. 그래서 확인해야 할 것이 분명합니다.
//
//   - 색인이 준비됐든(clean) 예산을 넘겨 없든(hidden이 밀어낸 unavailable) 응답이
//     **모든 페이지에서 바이트 단위로 같다.**
//   - 숨겨진 namespace는 결과·scan 예산·cursor·truncated·degraded·observedAt 어디에도
//     참여하지 않는다.
//   - 훑기 창(20k행)과 4096건을 넘는 결과도 페이지를 넘기면 전부 닿는다.
//   - 한 행이 여러 필드에 걸려도 정확히 한 번만 나온다.
//   - 행 단위 label 키 상한과 namespace 단위 label 토큰 상한을 색인 빌드와 똑같이 적용한다.
//
// fake 클러스터를 끼우지 않고 스냅숏을 직접 세웁니다 — 2만 행 이상을 다뤄야 하는데
// informer를 태우면 검증하려는 성질이 아니라 fake 클라이언트 속도를 재게 됩니다.

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	scopedGVR = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	jobsGVR   = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
)

// scopedResource는 서비스에 심을 리소스 하나입니다.
type scopedResource struct {
	gvr    schema.GroupVersionResource
	kind   string
	state  State       // informer 상태
	search SearchState // 검색 인덱스 상태
	rows   []*metav1.PartialObjectMetadata
	// builtAt이 0이면 indexBase를 씁니다. 순회 경로가 이 값을 응답에 싣지 않는다는
	// 것을 보이려면 변형마다 다른 값을 넣어야 합니다.
	builtAt time.Time
}

func indexAt(at time.Time, objs []*metav1.PartialObjectMetadata) *indexSnapshot {
	raw := make([]any, 0, len(objs))
	for _, o := range objs {
		raw = append(raw, o)
	}
	return buildIndexSnapshot(raw, at)
}

// scopedServiceWith는 여러 리소스를 든 서비스를 세웁니다.
// order는 allowlist와 같은 규칙(FormatGVR 사전순)으로 정렬합니다.
func scopedServiceWith(t *testing.T, resources ...scopedResource) *Service {
	t.Helper()
	slices.SortFunc(resources, func(a, b scopedResource) int {
		if FormatGVR(a.gvr) < FormatGVR(b.gvr) {
			return -1
		}
		return 1
	})
	s := &Service{
		cfg: Config{
			ClusterID: "prod-seoul", SearchEnabled: true,
			MaxSearchIndexBytes: DefaultMaxSearchIndexBytes,
		},
		order:   make([]schema.GroupVersionResource, 0, len(resources)),
		entries: make(map[schema.GroupVersionResource]*resourceEntry, len(resources)),
	}
	s.started.Store(true)
	disc := &discoverySnapshot{byGVR: map[schema.GroupVersionResource]int{}}
	for _, r := range resources {
		disc.byGVR[r.gvr] = len(disc.entries)
		disc.entries = append(disc.entries, discoveryEntry{
			gvr: r.gvr, kind: r.kind, namespaced: true, served: true,
			verbs: []string{"get", "list", "watch"},
		})
		s.order = append(s.order, r.gvr)
		s.entries[r.gvr] = &resourceEntry{gvr: r.gvr}
	}
	s.disc.Store(disc)

	for _, r := range resources {
		e := s.entries[r.gvr]
		e.setStatus(r.state, "")
		if r.state != StateReady {
			continue // 준비되지 않은 리소스는 스냅숏이 없습니다.
		}
		at := r.builtAt
		if at.IsZero() {
			at = indexBase
		}
		index := indexAt(at, r.rows)
		es := &entrySnapshot{index: index, searchState: r.search}
		if r.search == SearchReady {
			result := buildSearchSnapshot(index, r.kind, true, hugeBudget, hugeBudget)
			if result.snapshot == nil {
				t.Fatalf("색인 빌드가 실패했습니다: %s", result.state)
			}
			es.search = result.snapshot
		} else {
			es.searchReason = reasonBudget
		}
		e.setSnap(es)
	}
	return s
}

// scopedService는 목록 스냅숏 하나를 든 서비스를 세웁니다.
// state가 SearchReady면 실제 색인까지 만들고, 아니면 색인 없이 그 상태만 답니다.
func scopedService(t *testing.T, state SearchState, rows []*metav1.PartialObjectMetadata) *Service {
	t.Helper()
	return scopedServiceWith(t, scopedResource{
		gvr: scopedGVR, kind: "Service", state: StateReady, search: state, rows: rows,
	})
}

// pageAllScoped는 모든 페이지를 순회하며 직렬화된 응답과 방출된 키를 모읍니다.
func pageAllScoped(t *testing.T, s *Service, query string, allowed []string, limit int) (pages []string, keys []string) {
	t.Helper()
	cursor, previous := "", ""
	for round := 0; ; round++ {
		if round > 4096 {
			t.Fatal("페이지 순회가 끝나지 않습니다")
		}
		page, err := s.Search(SearchRequest{
			Query: query, Limit: limit, Cursor: cursor,
			Namespaces: NamespaceFilter{List: allowed},
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		encoded, _ := json.Marshal(page)
		pages = append(pages, string(encoded))
		for _, item := range page.Items {
			keys = append(keys, item.Namespace+"/"+item.Name+"/"+item.UID+"/"+item.MatchedField)
		}
		if page.NextCursor == "" {
			if page.Truncated {
				t.Fatal("cursor 없이 truncated를 보고했습니다 — 이어볼 방법이 없습니다")
			}
			return pages, keys
		}
		if page.NextCursor == previous {
			t.Fatal("같은 cursor가 반복되었습니다")
		}
		previous, cursor = page.NextCursor, page.NextCursor
	}
}

func assertSamePages(t *testing.T, label string, a, b []string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: 페이지 수가 다릅니다 %d vs %d", label, len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("%s: %d번째 페이지가 다릅니다\n  색인: %s\n  순회: %s", label, i, a[i], b[i])
		}
	}
}

func assertNoDuplicatesOrGaps(t *testing.T, keys []string, want int) {
	t.Helper()
	if len(keys) != want {
		t.Fatalf("방출 %d건, %d건이어야 합니다", len(keys), want)
	}
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if seen[k] {
			t.Fatalf("중복 방출: %s", k)
		}
		seen[k] = true
	}
}

/* ── 경계 1·4: label 키 절단과 다중 필드 중복 ─────────────────────────────── */

func TestScopedTraversalCrossesLabelKeyTruncation(t *testing.T) {
	fat := make(map[string]string, MaxLabelKeysPerObject+8)
	for i := 0; i < MaxLabelKeysPerObject+8; i++ {
		// 사전순 뒤쪽 키는 상한에 잘려 색인·순회 어느 쪽에서도 보이지 않습니다.
		fat[fmt.Sprintf("zz-key-%02d", i)] = fmt.Sprintf("zz-value-%02d", i)
	}
	fat["aa-key"] = "payments-edge"

	visible := []*metav1.PartialObjectMetadata{
		metaRow("allowed", "payments-a", "uid-a", fat),
		metaRow("allowed", "payments-b", "uid-b", fat),
		metaRow("allowed", "payments-c", "uid-c", fat),
	}
	hidden := append([]*metav1.PartialObjectMetadata{}, visible...)
	for i := 0; i < 400; i++ {
		hidden = append(hidden, metaRow("forbidden", fmt.Sprintf("payments-hidden-%04d", i), fmt.Sprintf("uid-h-%04d", i), fat))
	}

	clean := scopedService(t, SearchReady, visible)
	noisy := scopedService(t, SearchUnavailable, hidden)

	for _, query := range []string{"payments", "zz-key-00", "zz-key-19"} {
		cleanPages, cleanKeys := pageAllScoped(t, clean, query, []string{"allowed"}, 2)
		noisyPages, noisyKeys := pageAllScoped(t, noisy, query, []string{"allowed"}, 2)
		assertSamePages(t, "query "+query, cleanPages, noisyPages)
		if len(cleanKeys) != len(noisyKeys) {
			t.Fatalf("query %q: 방출 수가 다릅니다", query)
		}
	}

	// 상한 안쪽 키는 찾히고, 잘려 나간 키는 찾히지 않습니다. 잘렸다는 사실은 알립니다.
	page, err := clean.Search(SearchRequest{Query: "payments", Namespaces: NamespaceFilter{List: []string{"allowed"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("3건이어야 합니다: %+v", page.Items)
	}
	if !page.Degraded || page.Reason != reasonLabelNs {
		t.Fatalf("label 키 절단을 알리지 않았습니다: %+v", page)
	}
	// 다중 필드 중복: 이름·label 키·label 값이 모두 걸려도 행마다 한 번만 나갑니다.
	assertNoDuplicatesOrGaps(t, []string{
		page.Items[0].Namespace + "/" + page.Items[0].Name,
		page.Items[1].Namespace + "/" + page.Items[1].Name,
		page.Items[2].Namespace + "/" + page.Items[2].Name,
	}, 3)
	for _, item := range page.Items {
		if item.MatchedField != "name" {
			t.Fatalf("가장 구체적인 필드를 보고해야 합니다: %+v", item)
		}
	}
	if late, err := clean.Search(SearchRequest{Query: "zz-value-19", Namespaces: NamespaceFilter{List: []string{"allowed"}}}); err != nil {
		t.Fatal(err)
	} else if len(late.Items) != 0 {
		t.Fatalf("상한에 잘린 label이 찾혔습니다: %+v", late.Items)
	}
}

/* ── 이어보기 진단과 observedAt ───────────────────────────────────────────── */

// decodeScanCursor는 순회 cursor를 테스트에서 열어 봅니다.
func decodeScanCursor(t *testing.T, encoded, query string, allowed []string) searchCursorKey {
	t.Helper()
	fp := searchFingerprint("prod-seoul", query, false, allowed)
	key, err := decodeSearchCursor(encoded, fp, cursorModeScan)
	if err != nil {
		t.Fatalf("서버가 만든 cursor를 다시 읽지 못했습니다: %v", err)
	}
	return key
}

// TestScopedDiagnosticsSurviveContinuation — 1페이지에서만 만난 누락이 2페이지에서
// 사라지면 degraded가 참에서 거짓으로 뒤집혀 "완전한 결과"라고 말하게 됩니다.
func TestScopedDiagnosticsSurviveContinuation(t *testing.T) {
	fat := make(map[string]string, MaxLabelKeysPerObject+8)
	for i := 0; i < MaxLabelKeysPerObject+8; i++ {
		fat[fmt.Sprintf("zz-%02d", i)] = fmt.Sprintf("zzv-%02d", i)
	}
	// 검색은 **접두사** 일치이므로 질의 "row"가 이름의 접두사여야 합니다.
	// 행 순서는 (namespace, name)이라 row-a가 첫 페이지에 옵니다.
	rows := []*metav1.PartialObjectMetadata{
		// 잘리는 행은 **첫 페이지에만** 있습니다.
		metaRow("allowed", "row-a", "uid-a", fat),
		metaRow("allowed", "row-b", "uid-b", map[string]string{"one": "two"}),
		metaRow("allowed", "row-c", "uid-c", map[string]string{"one": "two"}),
	}
	svc := scopedService(t, SearchUnavailable, rows)

	pages, keys := pageAllScoped(t, svc, "row", []string{"allowed"}, 1)
	if len(pages) < 3 {
		t.Fatalf("페이지가 %d개뿐이라 이어보기를 검증하지 못했습니다", len(pages))
	}
	assertNoDuplicatesOrGaps(t, keys, 3)
	for i, encoded := range pages {
		var page SearchPage
		if err := json.Unmarshal([]byte(encoded), &page); err != nil {
			t.Fatal(err)
		}
		if !page.Degraded || page.Reason != reasonLabelNs {
			t.Fatalf("%d번째 페이지에서 잘림 사유가 사라졌습니다: degraded=%v reason=%q",
				i, page.Degraded, page.Reason)
		}
	}
}

// TestScopedDiagnosticsKeepSkippedResourceReason — cursor가 건너뛴 리소스의 사유도
// 이어져야 합니다. 건너뛰었다고 해서 그 리소스를 못 본 사실이 사라지지는 않습니다.
func TestScopedDiagnosticsKeepSkippedResourceReason(t *testing.T) {
	svc := scopedServiceWith(t,
		// FormatGVR 사전순으로 앞에 오므로, 첫 페이지 뒤 cursor가 이 리소스를 건너뜁니다.
		scopedResource{gvr: jobsGVR, kind: "Job", state: StateForbidden},
		// 질의 "row"가 접두사로 걸리도록 이름을 row-*로 둡니다.
		scopedResource{gvr: scopedGVR, kind: "Service", state: StateReady, search: SearchUnavailable,
			rows: []*metav1.PartialObjectMetadata{
				metaRow("allowed", "row-a", "uid-a", nil),
				metaRow("allowed", "row-b", "uid-b", nil),
			}},
	)

	pages, keys := pageAllScoped(t, svc, "row", []string{"allowed"}, 1)
	assertNoDuplicatesOrGaps(t, keys, 2)
	if len(pages) < 2 {
		t.Fatalf("페이지가 %d개뿐입니다", len(pages))
	}
	for i, encoded := range pages {
		var page SearchPage
		if err := json.Unmarshal([]byte(encoded), &page); err != nil {
			t.Fatal(err)
		}
		if !page.Degraded || page.Reason != reasonForbidden {
			t.Fatalf("%d번째 페이지에서 건너뛴 리소스 사유가 사라졌습니다: degraded=%v reason=%q",
				i, page.Degraded, page.Reason)
		}
	}
	// 원본 오류나 숨겨진 개수는 실리지 않습니다 — 고정 문구 하나뿐입니다.
	if reasonForbidden == "" {
		t.Fatal("사유가 비었습니다")
	}
}

// TestScopedSearchOmitsObservedAt — 목록 스냅숏의 builtAt은 숨겨진 namespace의
// 변경으로도 갱신됩니다. 그 값을 실으면 볼 수 없는 데이터의 변경 시각이 새어 나갑니다.
func TestScopedSearchOmitsObservedAt(t *testing.T) {
	visible := []*metav1.PartialObjectMetadata{
		metaRow("allowed", "row-a", "uid-a", nil),
		metaRow("allowed", "row-b", "uid-b", nil),
		metaRow("allowed", "row-c", "uid-c", nil),
	}
	hidden := append([]*metav1.PartialObjectMetadata{}, visible...)
	for i := 0; i < 200; i++ {
		hidden = append(hidden, metaRow("forbidden", fmt.Sprintf("h-row-%03d", i), fmt.Sprintf("uid-h-%03d", i), nil))
	}
	// 두 변형의 재구성 시각을 **일부러 다르게** 둡니다.
	clean := scopedServiceWith(t, scopedResource{
		gvr: scopedGVR, kind: "Service", state: StateReady, search: SearchReady,
		rows: visible, builtAt: indexBase,
	})
	noisy := scopedServiceWith(t, scopedResource{
		gvr: scopedGVR, kind: "Service", state: StateReady, search: SearchUnavailable,
		rows: hidden, builtAt: indexBase.Add(72 * time.Hour),
	})

	cleanPages, cleanKeys := pageAllScoped(t, clean, "row", []string{"allowed"}, 2)
	noisyPages, noisyKeys := pageAllScoped(t, noisy, "row", []string{"allowed"}, 2)
	assertSamePages(t, "observedAt", cleanPages, noisyPages)
	assertNoDuplicatesOrGaps(t, cleanKeys, 3)
	assertNoDuplicatesOrGaps(t, noisyKeys, 3)
	if len(cleanPages) < 2 {
		t.Fatal("이어보기 페이지가 없어 검증이 부족합니다")
	}
	for i, encoded := range cleanPages {
		var page SearchPage
		if err := json.Unmarshal([]byte(encoded), &page); err != nil {
			t.Fatal(err)
		}
		if !page.ObservedAt.IsZero() {
			t.Fatalf("%d번째 페이지가 observedAt %v를 실었습니다", i, page.ObservedAt)
		}
	}
}

/* ── 경계 2: namespace 단위 label 토큰 상한 (실제 순회) ────────────────────
   8192행 × 16 키/값 = 2^18 토큰으로 상한을 **정확히** 채우고, 그다음 행이 넘깁니다. */

func namespaceCapRows() []*metav1.PartialObjectMetadata {
	pad := make(map[string]string, MaxLabelKeysPerObject)
	for i := 0; i < MaxLabelKeysPerObject; i++ {
		pad[fmt.Sprintf("p%02d", i)] = fmt.Sprintf("pv%02d", i)
	}
	// 첫 행만 질의에 걸리는 label 값을 답니다. 키 수는 그대로 16개입니다.
	first := make(map[string]string, MaxLabelKeysPerObject)
	for k, v := range pad {
		first[k] = v
	}
	first["p00"] = "gold-value"

	const filler = MaxLabelTokensPerNamespace / (MaxLabelKeysPerObject * 2) // 8192
	rows := make([]*metav1.PartialObjectMetadata, 0, filler+1)
	rows = append(rows, metaRow("allowed", "fill-00000", "uid-f-00000", first))
	for i := 1; i < filler; i++ {
		rows = append(rows, metaRow("allowed", fmt.Sprintf("fill-%05d", i), fmt.Sprintf("uid-f-%05d", i), pad))
	}
	// 상한을 정확히 채운 뒤의 행입니다. label로만 걸리므로 상한에 막히면 사라집니다.
	rows = append(rows, metaRow("allowed", "zz-late", "uid-late", map[string]string{"gold-key": "gold-late"}))
	return rows
}

func TestScopedTraversalEnforcesNamespaceLabelCapAcrossPages(t *testing.T) {
	visible := namespaceCapRows()
	hidden := append([]*metav1.PartialObjectMetadata{}, visible...)
	for i := 0; i < 500; i++ {
		hidden = append(hidden, metaRow("forbidden", fmt.Sprintf("gold-hidden-%04d", i), fmt.Sprintf("uid-h-%04d", i),
			map[string]string{"gold-key": "gold-hidden"}))
	}
	clean := scopedService(t, SearchReady, visible)
	noisy := scopedService(t, SearchUnavailable, hidden)

	cleanPages, cleanKeys := pageAllScoped(t, clean, "gold", []string{"allowed"}, 1)
	noisyPages, noisyKeys := pageAllScoped(t, noisy, "gold", []string{"allowed"}, 1)
	assertSamePages(t, "namespace 상한", cleanPages, noisyPages)

	// 상한 안쪽의 label 일치 1건만 나옵니다. 상한을 넘긴 zz-late는 빠집니다.
	//
	// 이것이 곧 cursor 카운터 왕복의 증명입니다 — 2페이지가 카운터를 0에서 다시 세면
	// 8191행분(262,112)만 쌓여 zz-late가 상한 안에 들어오고 2건이 됩니다.
	assertNoDuplicatesOrGaps(t, cleanKeys, 1)
	assertNoDuplicatesOrGaps(t, noisyKeys, 1)
	if cleanKeys[0] != "allowed/fill-00000/uid-f-00000/label" {
		t.Fatalf("상한 안쪽 label 일치가 어긋났습니다: %s", cleanKeys[0])
	}

	var first SearchPage
	if err := json.Unmarshal([]byte(cleanPages[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("이어보기 cursor가 없습니다")
	}
	// cursor가 "여기까지 센 label 토큰 수"를 담고 있어야 합니다.
	key := decodeScanCursor(t, first.NextCursor, "gold", []string{"allowed"})
	if key.nsLabelTokens != MaxLabelKeysPerObject*2 {
		t.Fatalf("cursor label 카운터가 %d입니다 — 첫 행의 %d여야 합니다",
			key.nsLabelTokens, MaxLabelKeysPerObject*2)
	}
	if key.namespace != "allowed" || key.name != "fill-00000" {
		t.Fatalf("cursor 위치가 어긋났습니다: %+v", key)
	}

	// 마지막 페이지는 상한에 걸려 빠진 것이 있다고 알려야 합니다.
	var last SearchPage
	if err := json.Unmarshal([]byte(cleanPages[len(cleanPages)-1]), &last); err != nil {
		t.Fatal(err)
	}
	if !last.Degraded || last.Reason != reasonLabelNs {
		t.Fatalf("상한 초과를 알리지 않았습니다: %+v", last)
	}
}

/* ── 순수 판정 (경계 2 보조) ───────────────────────────────────────────────
   위 순회 테스트가 실제 8192행으로 상한을 넘습니다. 여기서는 같은 판정을 순수 함수로
   한 번 더 못박아, 경계 바로 위/아래에서 카운터가 어떻게 움직이는지를 직접 봅니다. */

func TestScopedMatchAppliesNamespaceLabelCapLikeTheBuild(t *testing.T) {
	row := &indexOf(metaRow("allowed", "filler", "uid-1", map[string]string{"gold": "bar"})).rows[0]
	keys, _ := sortedLabelKeys(row, nil)
	rowTokens := labelTokenCount(row, keys)
	if rowTokens != 2 {
		t.Fatalf("이 픽스처는 토큰 2개여야 합니다: %d", rowTokens)
	}

	// 상한 바로 아래 — label을 봅니다.
	under := int64(MaxLabelTokensPerNamespace) - rowTokens
	_, field, matched, incomplete, _ := scopedMatch(row, "service", "gold", nil, &under)
	if !matched || field != fieldLabel || incomplete {
		t.Fatalf("상한 안에서는 label이 걸려야 합니다: matched=%v field=%d incomplete=%v", matched, field, incomplete)
	}
	if under != MaxLabelTokensPerNamespace {
		t.Fatalf("카운터가 %d입니다 — 상한까지 올라야 합니다", under)
	}

	// 상한을 넘기는 지점 — label을 보지 않고 잘렸다고 알립니다.
	over := int64(MaxLabelTokensPerNamespace) - rowTokens + 1
	before := over
	_, _, matched, incomplete, _ = scopedMatch(row, "service", "gold", nil, &over)
	if matched {
		t.Fatal("상한을 넘겼는데 label로 찾혔습니다 — 색인 빌드와 판정이 어긋납니다")
	}
	if !incomplete {
		t.Fatal("잘렸는데 알리지 않았습니다")
	}
	if over != before {
		t.Fatalf("건너뛴 행이 카운터를 %d → %d로 올렸습니다", before, over)
	}

	// 이름이 걸리는 행은 label을 건너뛰어도 결과에서 사라지지 않습니다.
	named := &indexOf(metaRow("allowed", "gold-api", "uid-2", map[string]string{"gold": "bar"})).rows[0]
	over = int64(MaxLabelTokensPerNamespace)
	_, field, matched, incomplete, _ = scopedMatch(named, "service", "gold", nil, &over)
	if !matched || field != fieldName || !incomplete {
		t.Fatalf("이름 일치가 사라졌습니다: matched=%v field=%d incomplete=%v", matched, field, incomplete)
	}
}

/* ── 경계 3·4: 훑기 창과 4096건을 넘는 결과 ───────────────────────────────── */

// scanWindowRows는 훑기 창을 확실히 넘기는 행 수입니다.
const scanWindowRows = maxScopedScanRows + 50

func TestScopedTraversalReachesMatchesBeyondScanWindow(t *testing.T) {
	const matches = 5000 // 4096건 상한을 넘깁니다.
	visible := make([]*metav1.PartialObjectMetadata, 0, scanWindowRows+matches)
	for i := 0; i < scanWindowRows; i++ {
		// 질의에 걸리지 않는 채움 행입니다 — 창을 소진시키는 것이 목적입니다.
		visible = append(visible, metaRow("allowed", fmt.Sprintf("filler-%06d", i), fmt.Sprintf("uid-f-%06d", i), nil))
	}
	for i := 0; i < matches; i++ {
		// 이름·label 키·label 값이 모두 걸리는 다중 필드 행입니다.
		visible = append(visible, metaRow("allowed", fmt.Sprintf("payments-%06d", i), fmt.Sprintf("uid-p-%06d", i),
			map[string]string{"payments-key": "payments-value"}))
	}
	hidden := append([]*metav1.PartialObjectMetadata{}, visible...)
	for i := 0; i < 30000; i++ {
		// 같은 질의에 걸리는 숨겨진 행입니다. 예산도, 페이지 경계도 건드리면 안 됩니다.
		hidden = append(hidden, metaRow("forbidden", fmt.Sprintf("payments-hidden-%06d", i), fmt.Sprintf("uid-h-%06d", i),
			map[string]string{"payments-key": "payments-value"}))
	}

	clean := scopedService(t, SearchReady, visible)
	noisy := scopedService(t, SearchUnavailable, hidden)

	cleanPages, cleanKeys := pageAllScoped(t, clean, "payments", []string{"allowed"}, MaxSearchPageSize)
	noisyPages, noisyKeys := pageAllScoped(t, noisy, "payments", []string{"allowed"}, MaxSearchPageSize)

	assertSamePages(t, "창 경계", cleanPages, noisyPages)
	assertNoDuplicatesOrGaps(t, cleanKeys, matches)
	assertNoDuplicatesOrGaps(t, noisyKeys, matches)

	// 첫 페이지는 창이 닫혀 0건이지만 **이어보기 cursor와 정직한 사유**가 있어야 합니다.
	var first SearchPage
	if err := json.Unmarshal([]byte(cleanPages[0]), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 0 {
		t.Fatalf("채움 행만 훑은 첫 페이지에 %d건이 나왔습니다", len(first.Items))
	}
	if !first.Truncated || first.NextCursor == "" {
		t.Fatalf("창이 닫혔는데 이어볼 방법이 없습니다: %+v", first)
	}
	if !first.Degraded || first.Reason != reasonScopedScan {
		t.Fatalf("창이 닫힌 사실을 알리지 않았습니다: %+v", first)
	}
	if len(cleanPages) < 2 {
		t.Fatal("페이지가 하나뿐이라 창 경계를 넘지 못했습니다")
	}
	// 다중 필드 행이지만 필드는 가장 구체적인 이름으로 보고됩니다.
	for _, key := range cleanKeys {
		if len(key) < 5 || key[len(key)-5:] != "/name" {
			t.Fatalf("matchedField가 어긋났습니다: %s", key)
		}
	}
}

// TestScopedTraversalIsIdenticalWhetherIndexIsReady — 같은 허용 데이터라면 색인이
// 있든 없든 응답이 같아야 합니다. 이것이 성립해야 숨겨진 데이터가 색인 가용성을
// 통해 결과에 새어드는 경로가 없습니다.
func TestScopedTraversalIsIdenticalWhetherIndexIsReady(t *testing.T) {
	rows := []*metav1.PartialObjectMetadata{
		metaRow("allowed", "payments-a", "uid-a", map[string]string{"app": "payments", "tier": "gold"}),
		metaRow("allowed", "payments-b", "uid-b", map[string]string{"app": "payments", "tier": "gold"}),
		metaRow("other", "payments-z", "uid-z", map[string]string{"app": "payments"}),
	}
	ready := scopedService(t, SearchReady, rows)
	absent := scopedService(t, SearchUnavailable, rows)

	for _, query := range []string{"payments", "gold", "allowed", "service"} {
		readyPages, readyKeys := pageAllScoped(t, ready, query, []string{"allowed"}, 1)
		absentPages, absentKeys := pageAllScoped(t, absent, query, []string{"allowed"}, 1)
		assertSamePages(t, "query "+query, readyPages, absentPages)
		if len(readyKeys) != len(absentKeys) {
			t.Fatalf("query %q: 방출 수가 다릅니다", query)
		}
		for _, key := range readyKeys {
			if len(key) < 8 || key[:8] != "allowed/" {
				t.Fatalf("query %q: Scope 밖 행이 나왔습니다: %s", query, key)
			}
		}
	}
}
