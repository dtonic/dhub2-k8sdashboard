package resourcecatalog_test

// 전역 검색·최근 항목의 서비스 경계 검증입니다. (ADR 0023)
//
//   - 검색·최근 요청은 Kubernetes를 호출하지 않는다.
//   - **Scope 밖 객체는 결과에도, truncated·cursor에도, 개수에도 나타나지 않는다.**
//   - 인덱스 보유·정점 바이트가 서비스 상한을 넘지 않는다.
//   - 목록과 검색은 같은 재구성이 만든 같은 시각을 말한다.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
)

var frozenNow = time.Date(2026, 8, 21, 4, 0, 0, 0, time.UTC)

// searchTuner는 검색을 켜고 시계를 고정합니다. 시계를 고정해야 두 서비스의
// 응답을 바이트 단위로 비교할 수 있습니다.
func searchTuner(maxBytes int64) func(*resourcecatalog.Config) {
	return func(c *resourcecatalog.Config) {
		c.SearchEnabled = true
		c.MaxSearchIndexBytes = maxBytes
		c.Now = func() time.Time { return frozenNow }
	}
}

func allNamespaces() resourcecatalog.NamespaceFilter {
	return resourcecatalog.NamespaceFilter{All: true}
}

func onlyNamespaces(ns ...string) resourcecatalog.NamespaceFilter {
	return resourcecatalog.NamespaceFilter{List: ns}
}

func searchOrFail(t *testing.T, h *harness, req resourcecatalog.SearchRequest) resourcecatalog.SearchPage {
	t.Helper()
	page, err := h.svc.Search(req)
	if err != nil {
		t.Fatalf("search %+v: %v", req, err)
	}
	return page
}

func itemKeys(page resourcecatalog.SearchPage) []string {
	out := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		out = append(out, fmt.Sprintf("%s/%s/%s/%s/%s", item.Group, item.Resource, item.Namespace, item.Name, item.UID))
	}
	return out
}

/* ── 질의 검증 ───────────────────────────────────────────────────────────── */

func TestSearchRejectsShortAndMalformedQueries(t *testing.T) {
	h := start(t, options{
		allowlist:   []schema.GroupVersionResource{serviceGVR},
		metaObjects: []runtime.Object{metaObject("v1", "Service", "payments", "payments-api", "uid-1", nil)},
		tune:        searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	for _, q := range []string{"", " ", "p", " p ", strings.Repeat("a", resourcecatalog.MaxQueryLen+1), "pay*", "pay ments"} {
		if _, err := h.svc.Search(resourcecatalog.SearchRequest{Query: q, Namespaces: allNamespaces()}); err != resourcecatalog.ErrInvalidQuery {
			t.Errorf("query %q: err=%v — ErrInvalidQuery여야 합니다", q, err)
		}
	}
	// 상한을 넘는 limit은 조용히 자르지 않고 거절합니다.
	if _, err := h.svc.Search(resourcecatalog.SearchRequest{
		Query: "pay", Limit: resourcecatalog.MaxSearchPageSize + 1, Namespaces: allNamespaces(),
	}); err != resourcecatalog.ErrInvalidFilter {
		t.Errorf("limit 초과 err=%v", err)
	}
}

func TestSearchIsDisabledWithoutOptIn(t *testing.T) {
	// 기본 harness는 검색을 켜지 않습니다 — Explorer는 살아 있고 검색만 없는 상태입니다.
	h := start(t, options{
		allowlist:   []schema.GroupVersionResource{serviceGVR},
		metaObjects: []runtime.Object{metaObject("v1", "Service", "payments", "payments-api", "uid-1", nil)},
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	if _, err := h.svc.Search(resourcecatalog.SearchRequest{Query: "pay", Namespaces: allNamespaces()}); err != resourcecatalog.ErrSearchDisabled {
		t.Fatalf("err=%v — ErrSearchDisabled여야 합니다", err)
	}
	// 목록·상세는 그대로 살아 있어야 합니다. 검색 스위치는 검색만 끕니다.
	if _, _, err := h.svc.List(resourcecatalog.ListRequest{
		Group: "", Version: "v1", Resource: "services", Namespaces: resourcecatalog.NamespaceFilter{All: true},
	}); err != nil {
		t.Fatalf("검색을 껐더니 목록도 죽었습니다: %v", err)
	}
}

/* ── Scope (P1-3) ────────────────────────────────────────────────────────── */

func TestSearchNeverLeavesScope(t *testing.T) {
	h := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR, storageClassGVR},
		metaObjects: []runtime.Object{
			metaObject("v1", "Service", "allowed", "payments-api", "uid-1", nil),
			metaObject("v1", "Service", "forbidden", "payments-worker", "uid-2", nil),
			metaObject("storage.k8s.io/v1", "StorageClass", "", "payments-ssd", "uid-3", nil),
		},
		tune: searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)
	waitForState(t, h.svc, storageClassGVR, resourcecatalog.StateReady)

	scoped := searchOrFail(t, h, resourcecatalog.SearchRequest{Query: "pay", Namespaces: onlyNamespaces("allowed")})
	for _, item := range scoped.Items {
		if item.Namespace != "allowed" {
			t.Errorf("Scope 밖 객체가 나왔습니다: %+v", item)
		}
		if !item.Namespaced {
			t.Errorf("클러스터 범위 리소스가 namespace Scope 사용자에게 나왔습니다: %+v", item)
		}
	}
	if len(scoped.Items) != 1 {
		t.Fatalf("허용된 항목 1건이어야 합니다: %v", itemKeys(scoped))
	}

	// 클러스터 전체 권한에서는 클러스터 범위 리소스도 보입니다.
	full := searchOrFail(t, h, resourcecatalog.SearchRequest{Query: "pay", Namespaces: allNamespaces()})
	if len(full.Items) != 3 {
		t.Fatalf("전체 권한에서 3건이어야 합니다: %v", itemKeys(full))
	}
}

// TestSearchIsInvariantToForbiddenObjects — **P1-3의 핵심 검증입니다.**
//
// 볼 수 없는 namespace에 같은 질의에 걸리는 객체를 아무리 많이 넣어도, 사용자가 받는
// 응답은 직렬화 바이트까지 완전히 같아야 합니다. items뿐 아니라 nextCursor와
// truncated까지 같아야 "몇 건이 숨어 있는지"가 새어나가지 않습니다.
func TestSearchIsInvariantToForbiddenObjects(t *testing.T) {
	// 허용된 namespace의 객체는 label 키 상한 안쪽에 있습니다.
	// 허용된 namespace의 객체는 label 키 상한 안쪽에 있고, label 값으로도 찾힙니다.
	visible := []runtime.Object{
		metaObject("v1", "Service", "allowed", "payments-a", "uid-a", map[string]string{"app": "payments", "tier": "gold"}),
		metaObject("v1", "Service", "allowed", "payments-b", "uid-b", map[string]string{"app": "payments", "tier": "gold"}),
		metaObject("v1", "Service", "allowed", "payments-c", "uid-c", map[string]string{"app": "payments", "tier": "gold"}),
	}
	// 숨겨진 namespace는 세 가지 경계를 **동시에** 넘깁니다.
	//   ① label 키 상한(MaxLabelKeysPerObject)을 넘는 객체
	//   ② 객체 수가 많음
	//   ③ **같은 접두사를 가진 고유 label 값**이 대량 — 예전 설계에서 GVR 전체 label
	//      생략을 촉발하던 바로 그 입력입니다(사전 크기를 부풀립니다).
	// 이 중 어느 것도 허용 namespace의 응답을 바꾸면 안 됩니다.
	withHidden := append([]runtime.Object{}, visible...)
	for i := 0; i < 500; i++ {
		fat := make(map[string]string, resourcecatalog.MaxLabelKeysPerObject+8)
		for k := 0; k < resourcecatalog.MaxLabelKeysPerObject+8; k++ {
			// 값이 객체마다 전부 다릅니다 — 중복 제거가 전혀 듣지 않는 최악의 사전입니다.
			fat[fmt.Sprintf("payments-key-%02d", k)] = fmt.Sprintf("gold-hidden-%04d-%02d", i, k)
		}
		withHidden = append(withHidden,
			metaObject("v1", "Service", "forbidden", fmt.Sprintf("payments-hidden-%04d", i), fmt.Sprintf("uid-h-%04d", i), fat))
	}

	pageFor := func(objs []runtime.Object, query, cursor string) resourcecatalog.SearchPage {
		h := start(t, options{
			allowlist:   []schema.GroupVersionResource{serviceGVR},
			metaObjects: objs,
			tune:        searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
		})
		waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)
		return searchOrFail(t, h, resourcecatalog.SearchRequest{
			Query: query, Limit: 2, Cursor: cursor, Namespaces: onlyNamespaces("allowed"),
		})
	}

	// "payments"는 이름과 label 값 양쪽에, "gold"는 **label 값에만** 걸립니다.
	// 숨겨진 객체의 label 값도 "gold" 접두사를 공유하므로 사전이 크게 겹칩니다.
	for _, query := range []string{"payments", "gold"} {
		clean := pageFor(visible, query, "")
		noisy := pageFor(withHidden, query, "")
		if len(clean.Items) == 0 {
			t.Fatalf("query %q: 허용 namespace 결과가 비어 검증이 의미가 없습니다", query)
		}
		cleanJSON, _ := json.Marshal(clean)
		noisyJSON, _ := json.Marshal(noisy)
		if string(cleanJSON) != string(noisyJSON) {
			t.Fatalf("query %q: Scope 밖 객체가 응답을 바꿨습니다\n  없을 때: %s\n  있을 때: %s", query, cleanJSON, noisyJSON)
		}
		if clean.Degraded || clean.Reason != "" {
			t.Fatalf("query %q: 완전한 검색이 degraded로 보고되었습니다: %+v", query, clean)
		}
		if clean.NextCursor == "" || !clean.Truncated {
			t.Fatalf("query %q: 두 번째 페이지가 있어야 검증이 의미가 있습니다: %+v", query, clean)
		}

		// 두 번째 페이지도 같아야 합니다 — cursor 재개 경로에서도 새지 않아야 합니다.
		cleanNext, _ := json.Marshal(pageFor(visible, query, clean.NextCursor))
		noisyNext, _ := json.Marshal(pageFor(withHidden, query, noisy.NextCursor))
		if string(cleanNext) != string(noisyNext) {
			t.Fatalf("query %q: cursor 재개에서 Scope 밖 객체가 응답을 바꿨습니다\n  없을 때: %s\n  있을 때: %s",
				query, cleanNext, noisyNext)
		}
	}
}

// TestSearchLabelHitsSurviveHiddenNamespacePressure — P1-A의 label 경계입니다.
//
// 숨겨진 namespace가 label 사전을 아무리 부풀려도 허용 namespace의 **label 적중**이
// 그대로여야 합니다. 예전 설계는 이 지점에서 GVR 전체의 label을 빼 버렸고,
// 그 결과 제한된 사용자가 자기 namespace의 label 검색 결과를 잃었습니다.
func TestSearchLabelHitsSurviveHiddenNamespacePressure(t *testing.T) {
	visible := []runtime.Object{
		metaObject("v1", "Service", "allowed", "api", "uid-a", map[string]string{"tier": "gold"}),
	}
	withHidden := append([]runtime.Object{}, visible...)
	for i := 0; i < 800; i++ {
		labels := make(map[string]string, resourcecatalog.MaxLabelKeysPerObject)
		for k := 0; k < resourcecatalog.MaxLabelKeysPerObject; k++ {
			labels[fmt.Sprintf("k-%02d", k)] = fmt.Sprintf("gold-%04d-%02d", i, k)
		}
		withHidden = append(withHidden,
			metaObject("v1", "Service", "forbidden", fmt.Sprintf("hidden-%04d", i), fmt.Sprintf("uid-h-%04d", i), labels))
	}

	hits := func(objs []runtime.Object) resourcecatalog.SearchPage {
		h := start(t, options{
			allowlist:   []schema.GroupVersionResource{serviceGVR},
			metaObjects: objs,
			tune:        searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
		})
		waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)
		return searchOrFail(t, h, resourcecatalog.SearchRequest{
			Query: "gold", Namespaces: onlyNamespaces("allowed"),
		})
	}

	clean, noisy := hits(visible), hits(withHidden)
	if len(clean.Items) != 1 || clean.Items[0].MatchedField != "label" {
		t.Fatalf("허용 namespace의 label 적중이 없습니다: %+v", clean.Items)
	}
	cleanJSON, _ := json.Marshal(clean)
	noisyJSON, _ := json.Marshal(noisy)
	if string(cleanJSON) != string(noisyJSON) {
		t.Fatalf("숨겨진 namespace가 label 적중을 바꿨습니다\n  없을 때: %s\n  있을 때: %s", cleanJSON, noisyJSON)
	}
	if noisy.Degraded {
		t.Fatalf("숨겨진 namespace의 압력이 degraded로 새어 나왔습니다: %q", noisy.Reason)
	}
}

// TestScopedSearchIsUnaffectedWhenHiddenDataBreaksTheIndexBudget — **P1-2의 예산 경계입니다.**
//
// 앞의 두 테스트는 넉넉한 예산에서만 돌아 이 경계를 넘지 않았습니다. 여기서는
// 볼 수 없는 namespace의 규모 때문에 **색인 자체가 만들어지지 못하는** 예산을 씁니다.
// 그 상황에서도 범위가 좁은 호출자의 응답은 페이지마다 바이트 단위로 같아야 합니다 —
// items·cursor·truncated·degraded·reason·observedAt 전부입니다.
func TestScopedSearchIsUnaffectedWhenHiddenDataBreaksTheIndexBudget(t *testing.T) {
	// GVR 몫은 2048바이트입니다. 아래 clean 픽스처의 색인은 그 안에 들어가고,
	// hidden을 얹으면 들어가지 않습니다.
	const budget = 4096

	visible := []runtime.Object{
		metaObject("v1", "Service", "allowed", "payments-a", "uid-a", map[string]string{"app": "payments", "tier": "gold"}),
		metaObject("v1", "Service", "allowed", "payments-b", "uid-b", map[string]string{"app": "payments", "tier": "gold"}),
		metaObject("v1", "Service", "allowed", "payments-c", "uid-c", map[string]string{"app": "payments", "tier": "gold"}),
	}
	withHidden := append([]runtime.Object{}, visible...)
	for i := 0; i < 300; i++ {
		labels := make(map[string]string, resourcecatalog.MaxLabelKeysPerObject)
		for k := 0; k < resourcecatalog.MaxLabelKeysPerObject; k++ {
			labels[fmt.Sprintf("hidden-key-%02d", k)] = fmt.Sprintf("gold-hidden-%04d-%02d", i, k)
		}
		withHidden = append(withHidden,
			metaObject("v1", "Service", "forbidden", fmt.Sprintf("payments-hidden-%04d", i), fmt.Sprintf("uid-h-%04d", i), labels))
	}

	newSvc := func(objs []runtime.Object) *harness {
		h := start(t, options{
			allowlist: []schema.GroupVersionResource{serviceGVR}, metaObjects: objs,
			tune: searchTuner(budget),
		})
		waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)
		return h
	}
	clean, noisy := newSvc(visible), newSvc(withHidden)

	// 전제 확인 — 이 픽스처가 실제로 경계를 넘지 않으면 검증이 의미가 없습니다.
	if _, states := searchMetricLine(t, clean.svc, "dashboard_resource_search_resource_state{"); len(states) != 1 || states[0] != string(resourcecatalog.SearchReady) {
		t.Fatalf("clean 변형은 색인되어야 합니다: %v", states)
	}
	if _, states := searchMetricLine(t, noisy.svc, "dashboard_resource_search_resource_state{"); len(states) != 1 || states[0] != string(resourcecatalog.SearchUnavailable) {
		t.Fatalf("hidden 변형은 색인 예산을 넘겨야 합니다: %v", states)
	}

	// "payments"는 이름과 label 값 양쪽에, "gold"는 label 값에만 걸립니다.
	for _, query := range []string{"payments", "gold"} {
		cleanCursor, noisyCursor, pages := "", "", 0
		for {
			pages++
			if pages > 10 {
				t.Fatalf("query %q: 페이지 순회가 끝나지 않습니다", query)
			}
			c := searchOrFail(t, clean, resourcecatalog.SearchRequest{
				Query: query, Limit: 2, Cursor: cleanCursor, Namespaces: onlyNamespaces("allowed"),
			})
			n := searchOrFail(t, noisy, resourcecatalog.SearchRequest{
				Query: query, Limit: 2, Cursor: noisyCursor, Namespaces: onlyNamespaces("allowed"),
			})
			cleanJSON, _ := json.Marshal(c)
			noisyJSON, _ := json.Marshal(n)
			if string(cleanJSON) != string(noisyJSON) {
				t.Fatalf("query %q 페이지 %d: 색인이 깨진 쪽의 응답이 다릅니다\n  색인: %s\n  스캔: %s",
					query, pages, cleanJSON, noisyJSON)
			}
			if c.Degraded || c.Reason != "" {
				t.Fatalf("query %q: 완전한 결과가 degraded로 보고되었습니다: %+v", query, c)
			}
			if pages == 1 && len(c.Items) == 0 {
				t.Fatalf("query %q: 허용 namespace 결과가 비어 검증이 의미가 없습니다", query)
			}
			if c.NextCursor == "" {
				break
			}
			cleanCursor, noisyCursor = c.NextCursor, n.NextCursor
		}
		if pages < 2 {
			t.Fatalf("query %q: 페이지가 하나뿐이라 cursor 경로를 검증하지 못했습니다", query)
		}
	}

	// 전체 접근 호출자는 원래대로 예산 사유를 봅니다 — 그에게는 숨겨진 데이터가 없습니다.
	full := searchOrFail(t, noisy, resourcecatalog.SearchRequest{Query: "payments", Namespaces: allNamespaces()})
	if !full.Degraded || !strings.Contains(full.Reason, "예산") {
		t.Fatalf("전체 접근에서 예산 사유가 사라졌습니다: %+v", full)
	}
}

/* ── 페이징 ──────────────────────────────────────────────────────────────── */

func TestSearchPagingCoversEveryMatchExactlyOnce(t *testing.T) {
	objs := make([]runtime.Object, 0, 60)
	want := make(map[string]bool, 60)
	for i := 0; i < 30; i++ {
		for _, ns := range []string{"alpha", "beta"} {
			name := fmt.Sprintf("payments-%03d", i)
			uid := fmt.Sprintf("uid-%s-%03d", ns, i)
			objs = append(objs, metaObject("v1", "Service", ns, name, uid, nil))
			want[fmt.Sprintf("core/services/%s/%s/%s", ns, name, uid)] = true
		}
	}
	h := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR}, metaObjects: objs,
		tune: searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	seen := map[string]int{}
	cursor, previous, guard := "", "", 0
	for {
		guard++
		if guard > 200 {
			t.Fatal("cursor 순회가 끝나지 않았습니다 — 전진하지 않는 페이지가 있습니다")
		}
		page := searchOrFail(t, h, resourcecatalog.SearchRequest{
			Query: "payments-", Limit: 7, Cursor: cursor, Namespaces: allNamespaces(),
		})
		for _, key := range itemKeys(page) {
			seen[key]++
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == previous {
			t.Fatal("같은 cursor가 반복되었습니다")
		}
		previous, cursor = page.NextCursor, page.NextCursor
	}
	for key := range want {
		if seen[key] != 1 {
			t.Errorf("%s를 %d번 봤습니다 — 정확히 1번이어야 합니다", key, seen[key])
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("본 항목 %d개, 기대 %d개", len(seen), len(want))
	}
}

func TestSearchCursorRejectsForeignQueryAndScope(t *testing.T) {
	objs := make([]runtime.Object, 0, 40)
	for i := 0; i < 40; i++ {
		objs = append(objs, metaObject("v1", "Service", "alpha", fmt.Sprintf("payments-%03d", i), fmt.Sprintf("uid-%03d", i), nil))
	}
	h := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR}, metaObjects: objs,
		tune: searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	first := searchOrFail(t, h, resourcecatalog.SearchRequest{Query: "payments", Limit: 5, Namespaces: allNamespaces()})
	if first.NextCursor == "" {
		t.Fatal("두 번째 페이지가 있어야 합니다")
	}
	// 질의어가 바뀐 cursor
	if _, err := h.svc.Search(resourcecatalog.SearchRequest{
		Query: "payment", Cursor: first.NextCursor, Namespaces: allNamespaces(),
	}); err != resourcecatalog.ErrInvalidCursor {
		t.Errorf("다른 질의어의 cursor err=%v", err)
	}
	// Scope가 바뀐 cursor
	if _, err := h.svc.Search(resourcecatalog.SearchRequest{
		Query: "payments", Cursor: first.NextCursor, Namespaces: onlyNamespaces("alpha"),
	}); err != resourcecatalog.ErrInvalidCursor {
		t.Errorf("다른 Scope의 cursor err=%v", err)
	}
	// 손상된 cursor
	if _, err := h.svc.Search(resourcecatalog.SearchRequest{
		Query: "payments", Cursor: "not-a-cursor!!", Namespaces: allNamespaces(),
	}); err != resourcecatalog.ErrInvalidCursor {
		t.Errorf("손상된 cursor err=%v", err)
	}
}

/* ── 예산·게시 (P1-1, P1-2) ──────────────────────────────────────────────── */

func TestSearchIndexStaysWithinServiceBudgetAndPeak(t *testing.T) {
	objs := make([]runtime.Object, 0, 400)
	for i := 0; i < 200; i++ {
		objs = append(objs,
			metaObject("v1", "Service", "alpha", fmt.Sprintf("service-%04d", i), fmt.Sprintf("uid-s-%04d", i),
				map[string]string{"app": fmt.Sprintf("app-%04d", i)}),
			metaObject("v1", "Secret", "alpha", fmt.Sprintf("secret-%04d", i), fmt.Sprintf("uid-c-%04d", i),
				map[string]string{"app": fmt.Sprintf("app-%04d", i)}))
	}
	const budget = int64(1) << 20
	h := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR, secretGVR}, metaObjects: objs,
		tune: searchTuner(budget),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)
	waitForState(t, h.svc, secretGVR, resourcecatalog.StateReady)

	if got := h.svc.SearchIndexBytes(); got <= 0 || got > h.svc.MaxSearchBytes() {
		t.Fatalf("보유 바이트 %d가 상한 %d를 벗어났습니다", got, h.svc.MaxSearchBytes())
	}
	// 정점은 (기존 + 신규 + 작업용)이 동시에 살아 있는 순간입니다. 상한은 보유 상한 + 3 × GVR 몫입니다.
	if peak, max := h.svc.SearchPeakBytes(), h.svc.MaxSearchPeakBytes(); peak <= 0 || peak > max {
		t.Fatalf("재구성 정점 %d가 상한 %d를 벗어났습니다", peak, max)
	}
	// GVR 몫이 전체를 독점하지 못하게 조여야 합니다 — 64개가 각자 상한을 쓰면 상한이 아닙니다.
	if h.svc.MaxSearchBytes() != budget {
		t.Fatalf("서비스 상한이 설정과 다릅니다: %d", h.svc.MaxSearchBytes())
	}
}

// TestSearchReportsDegradedWhenAResourceIsExcludedByBudget — 예산 초과는 **동기화 중이
// 아닙니다.** 두 상태가 같은 사유로 뭉개지면 팔레트가 "기다리면 된다"고 오해합니다. (P1-D)
func TestSearchReportsDegradedWhenAResourceIsExcludedByBudget(t *testing.T) {
	objs := make([]runtime.Object, 0, 300)
	for i := 0; i < 300; i++ {
		objs = append(objs, metaObject("v1", "Service", "alpha", fmt.Sprintf("payments-%04d", i), fmt.Sprintf("uid-%04d", i), nil))
	}
	// 어떤 토큰도 들어가지 않는 예산입니다. 검색에서 빠졌다는 사실을 응답이 말해야 합니다.
	h := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR}, metaObjects: objs,
		tune: searchTuner(4096),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	page := searchOrFail(t, h, resourcecatalog.SearchRequest{Query: "payments", Namespaces: allNamespaces()})
	if !page.Degraded || page.Reason == "" {
		t.Fatalf("색인에서 빠졌는데 완전한 검색인 것처럼 답했습니다: %+v", page)
	}
	if len(page.Items) != 0 {
		t.Fatalf("색인이 없는데 결과가 나왔습니다: %v", itemKeys(page))
	}
	// 예산 사유로 분류되어야 합니다 — "동기화 중"으로 보고되면 안 됩니다.
	if !strings.Contains(page.Reason, "예산") {
		t.Fatalf("예산 초과가 예산 사유로 분류되지 않았습니다: %q", page.Reason)
	}

	// informer가 아직 동기화 중인 상태와 **다른** 사유여야 합니다.
	syncing := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR},
		metaSetup: func(meta *metadatafake.FakeMetadataClient) {
			meta.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewGenericServerResponse(
					http.StatusNotAcceptable, "get", serviceGVR.GroupResource(), "", "not acceptable", 0, false)
			})
		},
		tune: func(c *resourcecatalog.Config) {
			searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes)(c)
			c.SyncTimeout = 2 * time.Second
		},
	})
	waitForState(t, syncing.svc, serviceGVR, resourcecatalog.StateUnsupported)
	other := searchOrFail(t, syncing, resourcecatalog.SearchRequest{Query: "payments", Namespaces: allNamespaces()})
	if other.Reason == page.Reason {
		t.Fatalf("예산 초과와 준비되지 않은 상태가 같은 사유입니다: %q", page.Reason)
	}
}

// TestListAndSearchShareOneObservedAt — P1-2의 검증입니다.
// 재구성이 하나이므로 두 화면이 다른 시각을 말할 수 없습니다.
func TestListAndSearchShareOneObservedAt(t *testing.T) {
	h := start(t, options{
		allowlist:   []schema.GroupVersionResource{serviceGVR},
		metaObjects: []runtime.Object{metaObject("v1", "Service", "alpha", "payments-api", "uid-1", nil)},
		tune:        searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	list, _, err := h.svc.List(resourcecatalog.ListRequest{
		Group: "", Version: "v1", Resource: "services", Namespaces: resourcecatalog.NamespaceFilter{All: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	page := searchOrFail(t, h, resourcecatalog.SearchRequest{Query: "payments", Namespaces: allNamespaces()})
	if !list.ObservedAt.Equal(page.ObservedAt) {
		t.Fatalf("목록 %v와 검색 %v가 다른 시각을 말합니다", list.ObservedAt, page.ObservedAt)
	}
	if !list.ObservedAt.Equal(frozenNow) {
		t.Fatalf("관측 시각이 재구성 시각과 다릅니다: %v", list.ObservedAt)
	}
}

/* ── Kubernetes 비호출 ───────────────────────────────────────────────────── */

func TestSearchAndRecentNeverCallKubernetes(t *testing.T) {
	h := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR},
		metaObjects: []runtime.Object{
			metaObject("v1", "Service", "alpha", "payments-api", "uid-1", nil),
		},
		tune: searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	before := h.actions()
	for i := 0; i < 20; i++ {
		searchOrFail(t, h, resourcecatalog.SearchRequest{Query: "payments", Namespaces: allNamespaces()})
	}
	refs := []resourcecatalog.RecentRef{
		{Group: "", Version: "v1", Resource: "services", Namespace: "alpha", Name: "payments-api", UID: "uid-1"},
		{Group: "", Version: "v1", Resource: "services", Namespace: "alpha", Name: "ghost", UID: "uid-ghost"},
	}
	for i := 0; i < 20; i++ {
		if _, err := h.svc.Recent(refs, allNamespaces()); err != nil {
			t.Fatal(err)
		}
	}
	if after := h.actions(); after != before {
		t.Fatalf("검색·최근 조회가 Kubernetes를 %d번 호출했습니다", after-before)
	}
}

/* ── 부분 상태 (P1-6) ────────────────────────────────────────────────────── */

// TestSearchReportsPartialStatesInsteadOfSilentZero — 준비되지 않은 리소스를 조용히
// 건너뛰면 팔레트가 "0건"과 "아직 못 봄"을 구분할 수 없습니다.
func TestSearchReportsPartialStatesInsteadOfSilentZero(t *testing.T) {
	unsupported := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR},
		metaSetup: func(meta *metadatafake.FakeMetadataClient) {
			meta.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewGenericServerResponse(
					http.StatusNotAcceptable, "get", serviceGVR.GroupResource(), "", "not acceptable", 0, false)
			})
		},
		tune: func(c *resourcecatalog.Config) {
			searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes)(c)
			c.SyncTimeout = 2 * time.Second
		},
	})
	waitForState(t, unsupported.svc, serviceGVR, resourcecatalog.StateUnsupported)
	page := searchOrFail(t, unsupported, resourcecatalog.SearchRequest{Query: "payments", Namespaces: allNamespaces()})
	if !page.Degraded || page.Reason == "" {
		t.Fatalf("metadata 미지원 리소스가 조용히 빠졌습니다: %+v", page)
	}
	unsupportedReason := page.Reason

	forbidden := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR},
		metaSetup: func(meta *metadatafake.FakeMetadataClient) {
			meta.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(serviceGVR.GroupResource(), "", errors.New("denied"))
			})
		},
		tune: func(c *resourcecatalog.Config) {
			searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes)(c)
			c.SyncTimeout = 2 * time.Second
		},
	})
	waitForState(t, forbidden.svc, serviceGVR, resourcecatalog.StateForbidden)
	forbiddenPage := searchOrFail(t, forbidden, resourcecatalog.SearchRequest{Query: "payments", Namespaces: allNamespaces()})
	if !forbiddenPage.Degraded || forbiddenPage.Reason == "" {
		t.Fatalf("권한 없는 리소스가 조용히 빠졌습니다: %+v", forbiddenPage)
	}
	// 두 상태는 서로 다른 사유여야 합니다 — 같은 문구면 구분이 사라집니다.
	if forbiddenPage.Reason == unsupportedReason {
		t.Fatalf("미지원과 권한 없음이 같은 사유입니다: %q", forbiddenPage.Reason)
	}
	// 원본 오류 문구는 새어 나가지 않습니다.
	for _, leak := range []string{"denied", "not acceptable", "forbidden:", "406"} {
		if strings.Contains(forbiddenPage.Reason, leak) || strings.Contains(unsupportedReason, leak) {
			t.Errorf("degraded 사유에 원본 오류가 실렸습니다: %q", leak)
		}
	}
}

func TestSearchDistinguishesNoResultsFromIncompleteSearch(t *testing.T) {
	h := start(t, options{
		allowlist:   []schema.GroupVersionResource{serviceGVR},
		metaObjects: []runtime.Object{metaObject("v1", "Service", "payments", "payments-api", "uid-1", nil)},
		tune:        searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	page := searchOrFail(t, h, resourcecatalog.SearchRequest{Query: "zzzz", Namespaces: allNamespaces()})
	if len(page.Items) != 0 {
		t.Fatalf("결과가 있으면 안 됩니다: %v", itemKeys(page))
	}
	if page.Degraded || page.Reason != "" {
		t.Fatalf("완전한 검색의 0건이 degraded로 보고되었습니다: %+v", page)
	}
}

/* ── 롤백 스위치 (P1-4) ──────────────────────────────────────────────────── */

func TestRecentIsDisabledTogetherWithSearch(t *testing.T) {
	h := start(t, options{
		allowlist:   []schema.GroupVersionResource{serviceGVR},
		metaObjects: []runtime.Object{metaObject("v1", "Service", "payments", "payments-api", "uid-1", nil)},
		// 검색을 켜지 않습니다 — 롤백 스위치가 내려간 배포입니다.
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)

	refs := []resourcecatalog.RecentRef{
		{Version: "v1", Resource: "services", Namespace: "payments", Name: "payments-api", UID: "uid-1"},
	}
	if _, err := h.svc.Recent(refs, allNamespaces()); err != resourcecatalog.ErrSearchDisabled {
		t.Fatalf("검색을 껐는데 최근 항목이 살아 있습니다: %v", err)
	}
	if _, err := h.svc.Search(resourcecatalog.SearchRequest{Query: "pay", Namespaces: allNamespaces()}); err != resourcecatalog.ErrSearchDisabled {
		t.Fatalf("검색 err=%v", err)
	}
	// 카탈로그·목록·상세는 그대로 살아 있어야 합니다.
	if len(h.svc.Catalog().Descriptors) == 0 {
		t.Fatal("검색을 껐더니 카탈로그가 비었습니다")
	}
	if _, _, err := h.svc.List(resourcecatalog.ListRequest{
		Group: "", Version: "v1", Resource: "services", Namespaces: resourcecatalog.NamespaceFilter{All: true},
	}); err != nil {
		t.Fatalf("검색을 껐더니 목록도 죽었습니다: %v", err)
	}
	// 인덱스도 만들지 않으므로 바이트를 붙잡지 않습니다.
	if got := h.svc.SearchIndexBytes(); got != 0 {
		t.Fatalf("검색이 꺼졌는데 인덱스가 %d바이트를 붙잡고 있습니다", got)
	}
}

/* ── 보유 바이트 회계 (P1-2) ─────────────────────────────────────────────── */

// searchMetrics는 계측 텍스트에서 (전체 보유 바이트, 리소스별 합)을 읽습니다.
// 계측이 곧 운영이 보는 값이므로 그 값으로 회계를 검증합니다.
func searchMetrics(t *testing.T, svc *resourcecatalog.Service) (total, sum int64) {
	t.Helper()
	var buf strings.Builder
	if err := svc.WriteSearchMetrics(&buf); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		space := strings.LastIndex(line, " ")
		if space < 0 {
			continue
		}
		value, err := strconv.ParseInt(line[space+1:], 10, 64)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "dashboard_resource_search_index_bytes "):
			total = value
		case strings.HasPrefix(line, "dashboard_resource_search_resource_bytes{"):
			sum += value
		}
	}
	return total, sum
}

// searchMetricLine은 계측 텍스트에서 특정 metric의 값을 읽습니다.
func searchMetricLine(t *testing.T, svc *resourcecatalog.Service, metric string) (values []int64, states []string) {
	t.Helper()
	var buf strings.Builder
	if err := svc.WriteSearchMetrics(&buf); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, metric) {
			continue
		}
		space := strings.LastIndex(line, " ")
		if space < 0 {
			continue
		}
		if value, err := strconv.ParseInt(line[space+1:], 10, 64); err == nil {
			values = append(values, value)
		}
		if i := strings.Index(line, `state="`); i >= 0 {
			rest := line[i+len(`state="`):]
			if j := strings.Index(rest, `"`); j >= 0 {
				states = append(states, rest[:j])
			}
		}
	}
	return values, states
}

// TestSearchMetricsReportLabelsIndexedOnlyWhenReady — 색인이 없는 리소스를 색인된
// 것처럼 보고하면 운영이 용량 문제를 놓칩니다. (P2)
func TestSearchMetricsReportLabelsIndexedOnlyWhenReady(t *testing.T) {
	objs := []runtime.Object{metaObject("v1", "Service", "alpha", "payments-api", "uid-1", map[string]string{"app": "checkout"})}

	ready := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR}, metaObjects: objs,
		tune: searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
	})
	waitForState(t, ready.svc, serviceGVR, resourcecatalog.StateReady)
	values, _ := searchMetricLine(t, ready.svc, "dashboard_resource_search_resource_labels_indexed{")
	if len(values) != 1 || values[0] != 1 {
		t.Fatalf("ready 리소스의 labels_indexed=%v, 1이어야 합니다", values)
	}

	// 검색을 끈 배포와 예산이 모자란 배포는 둘 다 0이어야 합니다.
	for _, tc := range []struct {
		name      string
		tune      func(*resourcecatalog.Config)
		wantState string
	}{
		{"disabled", func(c *resourcecatalog.Config) { c.SearchEnabled = false }, string(resourcecatalog.SearchDisabled)},
		// GVR 몫은 전체의 1/2이므로 512는 256바이트입니다. 스냅숏 하나의 **고정 비용**만으로도
		// 그 몫을 넘기므로, 객체가 몇 개든 이 예산에서는 색인이 만들어지지 않습니다.
		// (예전 값 2048은 몫이 1024라 객체 하나짜리 인덱스(469바이트)가 그대로 들어갔고,
		//  그래서 이 하위 테스트가 unavailable을 만들지 못한 채 ready를 관측했습니다.)
		{"unavailable", searchTuner(512), string(resourcecatalog.SearchUnavailable)},
	} {
		h := start(t, options{
			allowlist: []schema.GroupVersionResource{serviceGVR}, metaObjects: objs, tune: tc.tune,
		})
		waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)
		values, _ := searchMetricLine(t, h.svc, "dashboard_resource_search_resource_labels_indexed{")
		if len(values) != 1 || values[0] != 0 {
			t.Errorf("%s: labels_indexed=%v, 0이어야 합니다", tc.name, values)
		}
		_, states := searchMetricLine(t, h.svc, "dashboard_resource_search_resource_state{")
		if len(states) != 1 || states[0] != tc.wantState {
			t.Errorf("%s: state=%v, %q여야 합니다", tc.name, states, tc.wantState)
		}
		if got := h.svc.SearchIndexBytes(); got != 0 {
			t.Errorf("%s: 색인이 없는데 %d바이트를 붙잡고 있습니다", tc.name, got)
		}
	}
}

func TestSearchIndexBytesMatchPublishedResourcesExactly(t *testing.T) {
	objs := make([]runtime.Object, 0, 200)
	for i := 0; i < 100; i++ {
		objs = append(objs,
			metaObject("v1", "Service", "alpha", fmt.Sprintf("service-%04d", i), fmt.Sprintf("uid-s-%04d", i), nil),
			metaObject("v1", "Secret", "alpha", fmt.Sprintf("secret-%04d", i), fmt.Sprintf("uid-c-%04d", i), nil))
	}
	h := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR, secretGVR}, metaObjects: objs,
		tune: searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)
	waitForState(t, h.svc, secretGVR, resourcecatalog.StateReady)

	total, sum := searchMetrics(t, h.svc)
	if total != sum {
		t.Fatalf("보유 바이트 합계 %d가 리소스별 합 %d와 다릅니다 — 회계에 유령이 있습니다", total, sum)
	}
	if total != h.svc.SearchIndexBytes() || total <= 0 {
		t.Fatalf("계측 %d와 API %d가 다릅니다", total, h.svc.SearchIndexBytes())
	}
	if total > h.svc.MaxSearchBytes() {
		t.Fatalf("보유 %d가 상한 %d를 넘었습니다", total, h.svc.MaxSearchBytes())
	}
	if peak, max := h.svc.SearchPeakBytes(), h.svc.MaxSearchPeakBytes(); peak <= 0 || peak > max {
		t.Fatalf("재구성 정점 %d가 상한 %d를 벗어났습니다", peak, max)
	}

	// 서비스를 닫으면 게시된 인덱스도 그 회계도 남지 않아야 합니다.
	h.svc.Close()
	if got := h.svc.SearchIndexBytes(); got != 0 {
		t.Fatalf("Close 뒤에 보유 바이트가 %d 남았습니다", got)
	}
}
