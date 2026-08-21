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
	} {
		if !strings.Contains(ts, expected) {
			t.Errorf("TypeScript 계약이 어긋났습니다: %s", expected)
		}
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
