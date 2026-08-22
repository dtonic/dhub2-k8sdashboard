package httpapi_test

// 전역 검색·최근 항목 엔드포인트의 경계 검증입니다. (ADR 0023)
//
//   - 권한 없는 요청은 배포 형태와 무관하게 403이고, 권한이 있는 요청만 503을 봅니다.
//   - 요청으로 Scope를 넓힐 수 없습니다. 검색에는 namespace 파라미터 자체가 없습니다.
//   - 결과는 **기존 상세 엔드포인트로 그대로 열립니다** — 신원이 어긋나면 deep link가 죽습니다.
//   - 크기 위반은 조용히 자르지 않고 400입니다.

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

func searchPath(query string) string {
	return resourcePath("/search?q=" + url.QueryEscape(query))
}

func searchOK(t *testing.T, f fixture, path string) contract.ResourceSearchResponse {
	t.Helper()
	var out contract.ResourceSearchResponse
	rec := explorerGet(t, f, path, &out)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s → %d (%s)", path, rec.Code, rec.Body.String())
	}
	return out
}

/* ── 권한 ─────────────────────────────────────────────────────────────────── */

func TestResourceSearchRequiresExploreCapability(t *testing.T) {
	viewer := explorerScope(true)
	viewer.CanExploreResources = false
	f := resourceFixtureFor(t, viewer, true)

	for _, path := range []string{searchPath("api"), resourcePath("/recent")} {
		rec := explorerGet(t, f, path, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s → %d, 403이어야 합니다", path, rec.Code)
		}
		if code := explorerErrorCode(t, rec); code != "forbidden" {
			t.Errorf("%s → code=%q", path, code)
		}
	}
}

func TestResourceSearchDeniesForeignClusterBeforeAvailability(t *testing.T) {
	// 서비스가 아예 없는 배포에서도 **권한 판정이 먼저**입니다.
	f := resourceFixtureFor(t, explorerScope(true), false)
	for _, path := range []string{
		"/api/v1/clusters/other-cluster/resources/search?q=api",
		"/api/v1/clusters/other-cluster/resources/recent",
	} {
		rec := explorerGet(t, f, path, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s → %d, 403이어야 합니다", path, rec.Code)
		}
		if code := explorerErrorCode(t, rec); code != "cluster_access_denied" {
			t.Errorf("%s → code=%q", path, code)
		}
	}
}

func TestResourceSearchIsStable503WhenServiceIsAbsent(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), false)
	for _, path := range []string{searchPath("api"), resourcePath("/recent")} {
		rec := explorerGet(t, f, path, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s → %d, 503이어야 합니다", path, rec.Code)
		}
		if code := explorerErrorCode(t, rec); code != "resources_unavailable" {
			t.Errorf("%s → code=%q", path, code)
		}
	}
}

/* ── 입력 검증 ────────────────────────────────────────────────────────────── */

func TestResourceSearchRejectsMalformedInput(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	cases := []struct{ path, code string }{
		{resourcePath("/search"), "invalid_query"},                                        // q 없음
		{searchPath("a"), "invalid_query"},                                                // 1자
		{searchPath(strings.Repeat("a", resourcecatalog.MaxQueryLen+1)), "invalid_query"}, // 길이 초과
		{searchPath("api*"), "invalid_query"},                                             // 허용되지 않는 문자
		{searchPath("api") + "&limit=0", "invalid_filter"},                                // 범위 밖
		{searchPath("api") + fmt.Sprintf("&limit=%d", resourcecatalog.MaxSearchPageSize+1), "invalid_filter"},
		{searchPath("api") + "&limit=abc", "invalid_filter"},
		{searchPath("api") + "&cursor=not-a-cursor", "invalid_cursor"},
	}
	for _, tc := range cases {
		rec := explorerGet(t, f, tc.path, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s → %d, 400이어야 합니다 (%s)", tc.path, rec.Code, rec.Body.String())
			continue
		}
		if code := explorerErrorCode(t, rec); code != tc.code {
			t.Errorf("%s → code=%q, %q여야 합니다", tc.path, code, tc.code)
		}
	}
}

/* ── Scope ────────────────────────────────────────────────────────────────── */

func TestResourceSearchCannotEscapeScope(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(false, "payments"), true)

	// 검색에는 namespace 파라미터가 없습니다. 붙여 보내도 Scope를 넓히지 못합니다.
	for _, path := range []string{searchPath("cdn"), searchPath("cdn") + "&ns=media", searchPath("cdn") + "&namespace=media"} {
		out := searchOK(t, f, path)
		if len(out.Items) != 0 {
			t.Errorf("%s → Scope 밖 media namespace 객체가 나왔습니다: %+v", path, out.Items)
		}
	}
	// 허용된 namespace는 그대로 보입니다.
	allowed := searchOK(t, f, searchPath("api-"))
	if len(allowed.Items) == 0 {
		t.Fatal("허용된 namespace 결과가 비었습니다")
	}
	for _, item := range allowed.Items {
		if item.Namespace != "payments" {
			t.Errorf("Scope 밖 객체가 나왔습니다: %+v", item)
		}
	}
	// appliedScope는 요청이 아니라 서버가 채웁니다.
	if allowed.AppliedScope.ClusterID != testcluster.ClusterID {
		t.Errorf("appliedScope.clusterId=%q", allowed.AppliedScope.ClusterID)
	}
	namespaces, ok := allowed.AppliedScope.Namespaces.([]any)
	if !ok || len(namespaces) != 1 || namespaces[0] != "payments" {
		t.Errorf("appliedScope.namespaces=%v — 서버 Scope 그대로여야 합니다", allowed.AppliedScope.Namespaces)
	}

	// 클러스터 범위 리소스는 클러스터 전체 권한에서만 보입니다.
	if out := searchOK(t, f, searchPath("fast")); len(out.Items) != 0 {
		t.Errorf("namespace Scope 사용자에게 클러스터 범위 리소스가 나왔습니다: %+v", out.Items)
	}
	full := resourceFixtureFor(t, explorerScope(true), true)
	if out := searchOK(t, full, searchPath("fast")); len(out.Items) != 1 {
		t.Errorf("클러스터 전체 권한에서 StorageClass가 보여야 합니다: %+v", out.Items)
	}
}

/* ── 페이징과 deep link ───────────────────────────────────────────────────── */

func TestResourceSearchPagingHasNoDuplicatesOrGaps(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	seen := map[string]int{}
	cursor, previous, guard := "", "", 0
	for {
		guard++
		if guard > 100 {
			t.Fatal("cursor 순회가 끝나지 않았습니다")
		}
		path := searchPath("api-") + "&limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		out := searchOK(t, f, path)
		if len(out.Items) > 2 {
			t.Fatalf("limit을 넘겼습니다: %d", len(out.Items))
		}
		for _, item := range out.Items {
			seen[item.UID]++
		}
		if out.NextCursor == "" {
			if out.Truncated {
				t.Fatal("cursor 없이 truncated를 보고했습니다 — 이어볼 방법이 없습니다")
			}
			break
		}
		if out.NextCursor == previous {
			t.Fatal("같은 cursor가 반복되었습니다")
		}
		previous, cursor = out.NextCursor, out.NextCursor
	}
	// exploreObjects는 payments에 api-a..api-g 7개를 만듭니다.
	if len(seen) != 7 {
		t.Fatalf("본 항목 %d개, 7개여야 합니다: %v", len(seen), seen)
	}
	for uid, count := range seen {
		if count != 1 {
			t.Errorf("%s를 %d번 봤습니다", uid, count)
		}
	}
}

// TestResourceSearchResultsDeepLinkToExistingDetail — 검색 결과의 신원이 기존 상세
// 엔드포인트에서 그대로 통해야 합니다. 여기가 어긋나면 팔레트의 deep link가 404가 됩니다.
func TestResourceSearchResultsDeepLinkToExistingDetail(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	out := searchOK(t, f, searchPath("db"))
	if len(out.Items) != 1 {
		t.Fatalf("Secret db 1건이어야 합니다: %+v", out.Items)
	}
	item := out.Items[0]
	if item.MatchedField != contract.ResourceMatchName {
		t.Errorf("matchedField=%q — 이름으로 걸렸어야 합니다", item.MatchedField)
	}

	// 검색이 돌려준 필드만으로 상세 URL을 만듭니다.
	detail := resourcePath(fmt.Sprintf("/%s/%s/%s/object?namespace=%s&name=%s&uid=%s",
		item.Group, item.Version, item.Resource,
		url.QueryEscape(item.Namespace), url.QueryEscape(item.Name), url.QueryEscape(item.UID)))
	var manifest contract.ResourceDetailResponse
	rec := explorerGet(t, f, detail, &manifest)
	if rec.Code != http.StatusOK {
		t.Fatalf("검색 결과로 만든 상세 링크가 %d입니다: %s", rec.Code, rec.Body.String())
	}
	if manifest.UID != item.UID || manifest.Name != item.Name {
		t.Fatalf("상세가 다른 객체를 열었습니다: %+v", manifest)
	}
	// 검색은 Secret **값**을 여는 경로를 새로 만들지 않습니다. 기존 정제 규칙 그대로입니다.
	if strings.Contains(manifest.YAML, "czNjcjN0") {
		t.Fatal("Secret 값이 노출되었습니다")
	}
}

// TestResourceSearchEchoesTheNormalizedQuery — 계약이 약속한 값은 서버가 정규화한
// 질의어입니다. 원본을 그대로 되돌려주면 대소문자·공백이 계약과 어긋납니다.
func TestResourceSearchEchoesTheNormalizedQuery(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)
	out := searchOK(t, f, searchPath("  API-  "))
	if out.Query != "api-" {
		t.Fatalf("query=%q — 정규화된 질의어여야 합니다", out.Query)
	}
	if len(out.Items) == 0 {
		t.Fatal("대문자·공백이 섞인 질의가 정규화되어 조회되어야 합니다")
	}
}

// TestResourceSearchAndRecentShareTheRollbackSwitch — 검색을 끈 배포에서는 두 경로가
// 함께 사라져야 합니다. 하나만 살아 있으면 롤백이 약속을 절반만 지키는 셈입니다.
func TestResourceSearchAndRecentShareTheRollbackSwitch(t *testing.T) {
	f := resourceFixtureWithSearchOff(t, explorerScope(true))

	for _, path := range []string{
		searchPath("api-"),
		recentQuery(resourcecatalog.RecentRef{
			Version: "v1", Resource: "services", Namespace: "payments", Name: "api-a", UID: "uid-payments-a",
		}),
	} {
		rec := explorerGet(t, f, path, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s → %d, 503이어야 합니다 (%s)", path, rec.Code, rec.Body.String())
			continue
		}
		// resources_unavailable(배포에 Explorer가 없음)과 구분되는 코드여야 합니다.
		if code := explorerErrorCode(t, rec); code != "search_unavailable" {
			t.Errorf("%s → code=%q, search_unavailable이어야 합니다", path, code)
		}
	}

	// 카탈로그·목록·상세는 그대로 살아 있어야 합니다.
	var catalog contract.ResourceCatalogResponse
	if rec := explorerGet(t, f, resourcePath(""), &catalog); rec.Code != http.StatusOK || len(catalog.Items) == 0 {
		t.Fatalf("검색을 껐더니 카탈로그가 %d입니다", rec.Code)
	}
	var list contract.ResourceListResponse
	if rec := explorerGet(t, f, resourcePath("/core/v1/services"), &list); rec.Code != http.StatusOK {
		t.Fatalf("검색을 껐더니 목록이 %d입니다: %s", rec.Code, rec.Body.String())
	}
	if len(list.Items) == 0 {
		t.Fatal("검색을 껐더니 목록이 비었습니다")
	}
}

// TestResourceSearchClassifiesBudgetExclusionNotSyncing — 예산 초과로 색인되지 않은
// 리소스는 "동기화 중"이 아닙니다. 두 상태가 같은 문구로 뭉개지면 팔레트가
// "기다리면 된다"고 오해합니다. (P1-D)
func TestResourceSearchClassifiesBudgetExclusionNotSyncing(t *testing.T) {
	f := resourceFixtureWithTinySearchBudget(t, explorerScope(true))
	out := searchOK(t, f, searchPath("api-"))
	if len(out.Items) != 0 {
		t.Fatalf("색인이 없는데 결과가 나왔습니다: %+v", out.Items)
	}
	if !out.Degraded || out.Reason == "" {
		t.Fatalf("색인에서 빠졌는데 완전한 검색인 것처럼 답했습니다: %+v", out)
	}
	if !strings.Contains(out.Reason, "예산") {
		t.Fatalf("예산 초과가 예산 사유로 분류되지 않았습니다: %q", out.Reason)
	}
	if strings.Contains(out.Reason, "동기화") {
		t.Fatalf("예산 초과가 동기화 중으로 보고되었습니다: %q", out.Reason)
	}

	// 예산이 넉넉한 배포에서는 같은 질의가 완전한 검색으로 나옵니다.
	full := resourceFixtureFor(t, explorerScope(true), true)
	ok := searchOK(t, full, searchPath("api-"))
	if ok.Degraded || ok.Reason != "" {
		t.Fatalf("완전한 검색이 degraded로 보고되었습니다: %+v", ok)
	}
	if len(ok.Items) == 0 {
		t.Fatal("예산이 넉넉한데 결과가 비었습니다")
	}
}

func TestResourceSearchDoesNotReportStatus(t *testing.T) {
	// PartialObjectMetadata에는 status가 없습니다. 계약에 없는 필드를 만들어
	// 채우지 않았는지 응답 본문에서 직접 확인합니다.
	f := resourceFixtureFor(t, explorerScope(true), true)
	rec := explorerGet(t, f, searchPath("api-"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"status"`) {
		t.Fatalf("검색 응답이 status를 담았습니다: %s", rec.Body.String())
	}
}

/* ── 최근 항목 ────────────────────────────────────────────────────────────── */

func recentQuery(refs ...resourcecatalog.RecentRef) string {
	values := url.Values{}
	for _, ref := range refs {
		values.Add("ref", resourcecatalog.EncodeRecentRef(ref))
	}
	return resourcePath("/recent?" + values.Encode())
}

func TestResourceRecentResolvesTitlesAndDropsForbidden(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(false, "payments"), true)
	path := recentQuery(
		resourcecatalog.RecentRef{Version: "v1", Resource: "services", Namespace: "media", Name: "cdn-a", UID: "uid-media-a"},
		resourcecatalog.RecentRef{Version: "v1", Resource: "services", Namespace: "payments", Name: "api-a", UID: "uid-payments-a"},
		resourcecatalog.RecentRef{Version: "v1", Resource: "services", Namespace: "payments", Name: "api-a", UID: "uid-replaced"},
		resourcecatalog.RecentRef{Version: "v1", Resource: "services", Namespace: "payments", Name: "ghost", UID: "uid-ghost"},
	)
	var out contract.ResourceRecentResponse
	rec := explorerGet(t, f, path, &out)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	if len(out.Items) != 1 {
		t.Fatalf("Scope 밖·UID 교체·삭제 항목이 조용히 빠져야 합니다: %+v", out.Items)
	}
	got := out.Items[0]
	if got.Kind != "Service" || got.Group != "core" || got.Namespace != "payments" || got.Name != "api-a" {
		t.Fatalf("서버가 제목의 근거를 다시 채우지 않았습니다: %+v", got)
	}
}

func TestResourceRecentRejectsOversizedRequests(t *testing.T) {
	f := resourceFixtureFor(t, explorerScope(true), true)

	// 개수 초과: 조용히 20개만 처리하면 클라이언트는 잘린 줄 모릅니다.
	refs := make([]resourcecatalog.RecentRef, 0, resourcecatalog.MaxRecentRefs+1)
	for i := 0; i <= resourcecatalog.MaxRecentRefs; i++ {
		refs = append(refs, resourcecatalog.RecentRef{
			Version: "v1", Resource: "services", Namespace: "payments",
			Name: fmt.Sprintf("api-%03d", i), UID: fmt.Sprintf("uid-%03d", i),
		})
	}
	rec := explorerGet(t, f, recentQuery(refs...), nil)
	if rec.Code != http.StatusBadRequest || explorerErrorCode(t, rec) != "invalid_filter" {
		t.Errorf("참조 %d개 → %d (%s)", len(refs), rec.Code, rec.Body.String())
	}

	// query string 전체 크기 초과: 파싱 전에 막습니다.
	huge := resourcePath("/recent?ref=" + strings.Repeat("a", resourcecatalog.MaxRecentQueryBytes+1))
	rec = explorerGet(t, f, huge, nil)
	if rec.Code != http.StatusBadRequest || explorerErrorCode(t, rec) != "invalid_filter" {
		t.Errorf("거대 query → %d (%s)", rec.Code, rec.Body.String())
	}

	// 구조가 깨진 참조도 400입니다 — 서버가 만든 적 없는 형식입니다.
	rec = explorerGet(t, f, resourcePath("/recent?ref=!!!"), nil)
	if rec.Code != http.StatusBadRequest || explorerErrorCode(t, rec) != "invalid_filter" {
		t.Errorf("손상된 참조 → %d (%s)", rec.Code, rec.Body.String())
	}

	// 참조가 없으면 빈 목록입니다. 오류가 아닙니다.
	var empty contract.ResourceRecentResponse
	if rec := explorerGet(t, f, resourcePath("/recent"), &empty); rec.Code != http.StatusOK || len(empty.Items) != 0 {
		t.Errorf("빈 요청 → %d, items=%d", rec.Code, len(empty.Items))
	}
}
