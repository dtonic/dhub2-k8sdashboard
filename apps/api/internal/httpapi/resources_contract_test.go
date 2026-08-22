package httpapi_test

// Resource Explorer 계약 드리프트 가드입니다. (ADR 0018)
//
// Go 상수 · OpenAPI · TypeScript가 같은 상한과 같은 상태 어휘를 말해야 합니다.
// 한 곳만 바꾸면 여기서 실패합니다.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
)

func resourceContractErrors(doc map[string]any) []string {
	var problems []string
	require := func(ok bool, message string) {
		if !ok {
			problems = append(problems, message)
		}
	}
	components := asMap(doc["components"])
	schemas := asMap(components["schemas"])
	params := asMap(components["parameters"])
	responses := asMap(components["responses"])
	ops := specOperationsFromDoc(doc)

	// 라우트와 상태 코드
	for operation, statuses := range map[string][]string{
		"GET /api/v1/clusters/{clusterId}/resources":                                     {"200", "401", "403", "404", "405", "500", "503"},
		"GET /api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}":        {"200", "400", "401", "403", "404", "405", "500", "502", "503"},
		"GET /api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}/object": {"200", "400", "401", "403", "404", "405", "409", "429", "500", "502", "503", "504"},
	} {
		definition := ops[operation]
		require(definition != nil, operation+" is missing")
		actual := asMap(definition["responses"])
		for _, status := range statuses {
			require(actual[status] != nil, operation+" missing status "+status)
		}
	}

	// 페이지·응답 상한이 Go 상수와 같아야 합니다.
	limit := asMap(asMap(params["ResourceLimit"])["schema"])
	require(limit["maximum"] == resourcecatalog.MaxPageSize, "list limit bound drift")
	require(limit["default"] == resourcecatalog.DefaultPageSize, "list default page bound drift")
	require(asMap(params["ResourceNamePrefix"])["schema"] != nil, "name prefix parameter missing")
	require(asMap(asMap(params["ResourceNamePrefix"])["schema"])["maxLength"] == resourcecatalog.MaxNameFilterLen, "name filter bound drift")
	require(asMap(asMap(params["ResourceLabelSelector"])["schema"])["maxLength"] == resourcecatalog.MaxSelectorLen, "label selector bound drift")
	cursor := asMap(asMap(params["ResourceCursor"])["schema"])
	require(cursor["maxLength"] == 512 && cursor["pattern"] == "^[A-Za-z0-9_-]+$", "cursor parameter drift")
	sortParam := asSlice(asMap(asMap(params["ResourceSort"])["schema"])["enum"])
	require(fmt.Sprint(sortParam) == "[name]", "sort key must stay the single indexed key")
	require(asMap(params["ResourceObjectUID"])["required"] == true, "detail uid must be required")
	require(asMap(params["ResourceObjectName"])["required"] == true, "detail name must be required")

	listSchema := asMap(schemas["ResourceListResponse"])
	require(listSchema["x-max-response-bytes"] == resourcecatalog.MaxResponseBytes, "list response byte bound drift")
	listProps := asMap(listSchema["properties"])
	require(asMap(listProps["items"])["maxItems"] == resourcecatalog.MaxPageSize, "list item bound drift")
	require(asMap(listProps["nextCursor"])["pattern"] == "^[A-Za-z0-9_-]+$", "list cursor drift")

	catalog := asMap(schemas["ResourceCatalogResponse"])
	require(asMap(asMap(catalog["properties"])["items"])["maxItems"] == resourcecatalog.MaxAllowlistEntries, "catalog bound drift")

	states := asSlice(asMap(schemas["ResourceState"])["enum"])
	require(fmt.Sprint(states) == "[ready syncing unsupported forbidden missing]", "resource state vocabulary drift")

	detail := asMap(schemas["ResourceDetailResponse"])
	detailRequired := map[string]bool{}
	for _, field := range asSlice(detail["required"]) {
		detailRequired[fmt.Sprint(field)] = true
	}
	for _, field := range []string{"uid", "resourceVersion", "yaml"} {
		require(detailRequired[field], "detail must always carry "+field)
	}
	detailProps := asMap(detail["properties"])
	for _, forbidden := range []string{"data", "stringData"} {
		require(detailProps[forbidden] == nil, "detail must never declare "+forbidden)
	}
	require(detail["additionalProperties"] == false, "detail schema must be closed")

	scopeProps := asMap(asMap(schemas["ScopeResponse"])["properties"])
	require(asMap(scopeProps["canExploreResources"])["type"] == "boolean", "scope capability drift")
	require(asMap(scopeProps["canManageWorkloads"])["type"] == "boolean", "existing manage capability disappeared")

	codes := map[string]bool{}
	for _, code := range asSlice(asMap(asMap(asMap(schemas["APIError"])["properties"])["code"])["enum"]) {
		codes[fmt.Sprint(code)] = true
	}
	for _, code := range []string{
		"resources_unavailable", "resource_not_allowlisted", "resource_not_served",
		"resource_unsupported", "resource_forbidden", "resource_syncing",
		"invalid_filter", "invalid_cursor", "detail_rate_limited", "uid_mismatch",
		"object_too_large", "namespace_access_denied", "cluster_scope_required",
	} {
		require(codes[code], "APIError enum missing "+code)
	}

	for _, name := range []string{"ResourceUnavailable", "ResourceUpstream", "ResourceConflict", "ResourceRateLimited", "ResourceTimeout"} {
		response := asMap(responses[name])
		require(response != nil, "shared response "+name+" missing")
		require(asMap(asMap(response["headers"])["X-Request-ID"]) != nil, name+" missing X-Request-ID")
	}
	require(asMap(asMap(asMap(responses["ResourceRateLimited"])["headers"])["Retry-After"]) != nil, "rate limited response must advertise Retry-After")

	/* ── 전역 검색·최근 항목 (ADR 0023) ──────────────────────────────────── */

	for operation, statuses := range map[string][]string{
		"GET /api/v1/clusters/{clusterId}/resources/search": {"200", "400", "401", "403", "404", "405", "500", "503"},
		"GET /api/v1/clusters/{clusterId}/resources/recent": {"200", "400", "401", "403", "404", "405", "500", "503"},
	} {
		definition := ops[operation]
		require(definition != nil, operation+" is missing")
		actual := asMap(definition["responses"])
		for _, status := range statuses {
			require(actual[status] != nil, operation+" missing status "+status)
		}
	}

	query := asMap(asMap(params["ResourceQuery"])["schema"])
	require(query["minLength"] == resourcecatalog.MinQueryLen, "search query minimum drift")
	require(query["maxLength"] == resourcecatalog.MaxQueryLen, "search query maximum drift")
	require(asMap(params["ResourceQuery"])["required"] == true, "search query must be required")

	searchLimit := asMap(asMap(params["ResourceSearchLimit"])["schema"])
	require(searchLimit["maximum"] == resourcecatalog.MaxSearchPageSize, "search limit bound drift")
	require(searchLimit["default"] == resourcecatalog.DefaultSearchPageSize, "search default page bound drift")

	searchCursor := asMap(asMap(params["ResourceSearchCursor"])["schema"])
	require(searchCursor["maxLength"] == resourcecatalog.MaxSearchCursorLen, "search cursor length drift")
	require(searchCursor["pattern"] == "^[A-Za-z0-9_-]+$", "search cursor must stay opaque base64url")

	refs := asMap(asMap(params["ResourceRecentRefs"])["schema"])
	require(refs["maxItems"] == resourcecatalog.MaxRecentRefs, "recent ref count bound drift")
	require(asMap(refs["items"])["maxLength"] == resourcecatalog.MaxRecentRefLen, "recent ref length bound drift")

	matchFields := asSlice(asMap(schemas["ResourceMatchField"])["enum"])
	require(fmt.Sprint(matchFields) == fmt.Sprint(resourcecatalog.MatchedFieldNames()), "matched field vocabulary drift")

	searchItem := asMap(schemas["ResourceSearchItem"])
	require(searchItem["additionalProperties"] == false, "search item schema must be closed")
	searchItemProps := asMap(searchItem["properties"])
	// status는 PartialObjectMetadata에 없습니다. 계약에 선언되는 순간 서버가
	// 없는 값을 지어내야 하므로, 없다는 것 자체를 고정합니다.
	require(searchItemProps["status"] == nil, "search item must never declare status")
	for _, field := range []string{"group", "version", "resource", "kind", "namespaced", "name", "uid", "matchedField"} {
		require(searchItemProps[field] != nil, "search item missing "+field)
	}

	searchResponse := asMap(schemas["ResourceSearchResponse"])
	require(searchResponse["x-max-response-bytes"] == resourcecatalog.MaxSearchResponseBytes, "search response byte bound drift")
	searchProps := asMap(searchResponse["properties"])
	require(asMap(searchProps["items"])["maxItems"] == resourcecatalog.MaxSearchPageSize, "search item bound drift")
	require(asMap(searchProps["nextCursor"])["maxLength"] == resourcecatalog.MaxSearchCursorLen, "search next cursor drift")
	searchRequired := map[string]bool{}
	for _, field := range asSlice(searchResponse["required"]) {
		searchRequired[fmt.Sprint(field)] = true
	}
	// degraded와 truncated는 선택 필드가 되면 안 됩니다 — 없으면 UI가 잘린 검색을
	// 완전한 검색으로 그립니다.
	for _, field := range []string{"appliedScope", "items", "truncated", "degraded"} {
		require(searchRequired[field], "search response must always carry "+field)
	}

	recentResponse := asMap(schemas["ResourceRecentResponse"])
	require(asMap(asMap(recentResponse["properties"])["items"])["maxItems"] == resourcecatalog.MaxRecentRefs, "recent response bound drift")

	for _, code := range []string{"search_unavailable", "invalid_query"} {
		require(codes[code], "APIError enum missing "+code)
	}
	return problems
}

func TestResourceOpenAPIContractMatchesServerBounds(t *testing.T) {
	doc := loadSpec(t)
	if problems := resourceContractErrors(doc); len(problems) != 0 {
		t.Fatal(strings.Join(problems, "; "))
	}

	// 검사가 헛돌지 않는지 확인합니다 — 각 변형은 반드시 실패를 만들어야 합니다.
	mutations := []struct {
		name string
		edit func(map[string]any)
	}{
		{"detail route removed", func(d map[string]any) {
			delete(asMap(d["paths"]), "/api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}/object")
		}},
		{"page cap drift", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["parameters"])["ResourceLimit"])["schema"] = map[string]any{"maximum": 500}
		}},
		{"state vocabulary drift", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["schemas"])["ResourceState"])["enum"] = []any{"ready", "missing"}
		}},
		{"detail exposes secret data", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["ResourceDetailResponse"])["properties"])["data"] = map[string]any{"type": "object"}
		}},
		{"scope capability removed", func(d map[string]any) {
			delete(asMap(asMap(asMap(asMap(d["components"])["schemas"])["ScopeResponse"])["properties"]), "canExploreResources")
		}},
		{"409 removed", func(d map[string]any) {
			delete(asMap(asMap(asMap(asMap(d["paths"])["/api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}/object"])["get"])["responses"]), "409")
		}},
		{"free-form sort key", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["parameters"])["ResourceSort"])["schema"] = map[string]any{"type": "string"}
		}},
		{"uid no longer required", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["parameters"])["ResourceObjectUID"])["required"] = false
		}},
		{"search route removed", func(d map[string]any) {
			delete(asMap(d["paths"]), "/api/v1/clusters/{clusterId}/resources/search")
		}},
		{"single character query allowed", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["parameters"])["ResourceQuery"])["schema"] = map[string]any{"minLength": 1, "maxLength": 64}
		}},
		{"search page cap drift", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["parameters"])["ResourceSearchLimit"])["schema"] = map[string]any{"maximum": 500, "default": 20}
		}},
		{"matched field vocabulary drift", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["schemas"])["ResourceMatchField"])["enum"] = []any{"name", "label"}
		}},
		{"search item invents status", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["ResourceSearchItem"])["properties"])["status"] = map[string]any{"type": "string"}
		}},
		{"degraded becomes optional", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["schemas"])["ResourceSearchResponse"])["required"] = []any{"clusterId", "query", "generatedAt", "appliedScope", "items", "truncated"}
		}},
		{"recent ref count drift", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["parameters"])["ResourceRecentRefs"])["schema"] = map[string]any{"maxItems": 100, "items": map[string]any{"maxLength": 1024}}
		}},
	}
	for _, mutation := range mutations {
		raw, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var changed map[string]any
		if err := yaml.Unmarshal(raw, &changed); err != nil {
			t.Fatal(err)
		}
		mutation.edit(changed)
		if problems := resourceContractErrors(changed); len(problems) == 0 {
			t.Errorf("negative mutation was masked: %s", mutation.name)
		}
	}
}

func TestResourceTypeScriptContractMatchesGo(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "packages", "contracts", "src", "index.ts"))
	if err != nil {
		t.Fatal(err)
	}
	ts := string(raw)
	for _, expected := range []string{
		`export type ResourceState = "ready" | "syncing" | "unsupported" | "forbidden" | "missing";`,
		"export interface ResourceDescriptor {",
		"export interface ResourceCatalogResponse {",
		"export interface ResourceListItem {",
		"export interface ResourceListResponse {",
		"export interface ResourceDetailResponse {",
		"canExploreResources?: boolean;",
		"canManageWorkloads?: boolean;",
		"nextCursor?: string;",
		"redacted?: string[];",
		// 전역 검색 (ADR 0023)
		`export type ResourceMatchField = "name" | "namespace" | "kind" | "label";`,
		"export interface ResourceSearchItem {",
		"export interface ResourceSearchResponse {",
		"export interface ResourceRecentItem {",
		"export interface ResourceRecentResponse {",
		"matchedField: ResourceMatchField;",
		"degraded: boolean;",
	} {
		if !strings.Contains(ts, expected) {
			t.Errorf("TypeScript 계약이 어긋났습니다: %s", expected)
		}
	}
	// 검색 결과 계약도 status를 선언하지 않습니다.
	searchStart := strings.Index(ts, "export interface ResourceSearchItem {")
	if searchStart < 0 {
		t.Fatal("ResourceSearchItem 선언을 찾지 못했습니다")
	}
	searchItem := ts[searchStart:]
	if end := strings.Index(searchItem, "\n}"); end >= 0 {
		searchItem = searchItem[:end]
	}
	if strings.Contains(searchItem, "status") {
		t.Errorf("ResourceSearchItem이 status를 선언했습니다")
	}
	// 상세 계약은 값 필드를 절대 선언하지 않습니다.
	start := strings.Index(ts, "export interface ResourceDetailResponse {")
	if start < 0 {
		t.Fatal("ResourceDetailResponse 선언을 찾지 못했습니다")
	}
	detail := ts[start:]
	end := strings.Index(detail, "\n}")
	if end < 0 {
		t.Fatal("ResourceDetailResponse 선언이 닫히지 않았습니다")
	}
	detail = detail[:end]
	for _, forbidden := range []string{"data:", "stringData:"} {
		if strings.Contains(detail, forbidden) {
			t.Errorf("ResourceDetailResponse가 %q를 선언했습니다", forbidden)
		}
	}
}
