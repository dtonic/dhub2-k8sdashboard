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

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
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

	problems = append(problems, dryRunContractErrors(doc)...)
	return problems
}

/* ── 변경 검토 dry-run 계약 (ADR 0019 Phase 1) ────────────────────────────

   범위는 **이 dry-run 계약 안**입니다. apply·delete·change token·force는 Phase 1
   범위에 없으므로(deferred) 요청·응답 스키마에 그 속성이 없다는 것과, 경로가
   POST 하나뿐이라는 것을 고정합니다. 저장소 전체에서 그런 이름의 스키마·타입이
   영원히 없어야 한다는 검사는 하지 않습니다 — 그것은 별도 ADR의 결정이지
   이 계약이 정할 일이 아닙니다.

   매니페스트 원문과 Kubernetes Status 원문이 응답에 없다는 것은 범위 밖 금지가
   아니라 이 계약 자체의 보안 경계이므로 그대로 고정합니다. */

// dryRunHTTPMethods는 path item에서 operation으로 셀 키입니다.
var dryRunHTTPMethods = map[string]bool{
	"get": true, "head": true, "post": true, "put": true, "patch": true, "delete": true, "options": true, "trace": true,
}

func dryRunContractErrors(doc map[string]any) []string {
	var problems []string
	require := func(ok bool, message string) {
		if !ok {
			problems = append(problems, message)
		}
	}
	schemas := asMap(asMap(doc["components"])["schemas"])
	responses := asMap(asMap(doc["components"])["responses"])

	// 경로는 기존 상세(object) 경계 **아래**에 붙습니다. 신원 규칙(UID 필수·목록에서
	// 본 행만)을 상세와 공유한다는 사실을 경로 모양으로 드러냅니다.
	require(contract.ResourceDryRunPath ==
		"/api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}/object/dry-run",
		"dry-run path constant drift")
	require(contract.ResourceDryRunFieldManager == "k8s-dashboard-dryrun", "fixed fieldManager drift")

	// 라우터가 등록되었으므로(U3) 이제 **경로가 반드시 있어야** 합니다.
	// 삭제도 실패로 잡힙니다. 이름이 비슷한 다른 경로가 생기는 것도 막습니다.
	require(asMap(doc["paths"])[contract.ResourceDryRunPath] != nil,
		"dry-run path is missing: "+contract.ResourceDryRunPath)
	for path, raw := range asMap(doc["paths"]) {
		if !strings.Contains(path, "dry-run") && !strings.Contains(path, "dryrun") {
			continue
		}
		require(path == contract.ResourceDryRunPath, "dry-run path drift: "+path)
		item := asMap(raw)
		for method := range item {
			if !dryRunHTTPMethods[method] {
				continue
			}
			// 이 경로에는 조회도 삭제도 없습니다. 검토 제출 하나뿐입니다.
			require(method == "post", "dry-run path must expose POST only, saw "+method)
		}
		post := asMap(item["post"])
		require(post["operationId"] == contract.ResourceDryRunOperationID, "dry-run operationId drift")
		body := asMap(post["requestBody"])
		require(body["required"] == true, "dry-run requestBody must be required")
		require(asMap(asMap(asMap(body["content"])["application/json"])["schema"])["$ref"] ==
			"#/components/schemas/ResourceDryRunRequest", "dry-run requestBody schema drift")
		actual := asMap(post["responses"])
		// 415가 빠지면 핸들러가 내는 상태가 계약에 없는 채로 배포됩니다.
		// Content-Type 검사는 이 경로의 cross-origin 표면을 좁히는 장치이므로
		// 계약에서 사라지면 그 사실 자체가 보이지 않게 됩니다.
		for _, status := range []string{
			"200", "400", "401", "403", "404", "405", "409", "413", "415", "429", "500", "502", "503", "504",
		} {
			require(actual[status] != nil, "dry-run missing status "+status)
		}
		require(asMap(asMap(asMap(asMap(actual["200"])["content"])["application/json"])["schema"])["$ref"] ==
			"#/components/schemas/ResourceDryRunResponse", "dry-run 200 schema drift")
	}

	request := asMap(schemas["ResourceDryRunRequest"])
	require(request != nil, "ResourceDryRunRequest missing")
	require(request["additionalProperties"] == false, "dry-run request schema must be closed")
	requestRequired := map[string]bool{}
	for _, field := range asSlice(request["required"]) {
		requestRequired[fmt.Sprint(field)] = true
	}
	// 본문은 신원 여섯 가지를 전부 들고 와야 합니다. 서버가 GVR·매니페스트·Scope와
	// 대조할 수 있어야 하고, 빠진 필드는 "대조하지 않는 필드"가 됩니다.
	for _, field := range []string{"apiVersion", "kind", "name", "uid", "resourceVersion", "manifest"} {
		require(requestRequired[field], "dry-run request must always carry "+field)
	}
	requestProps := asMap(request["properties"])
	require(asMap(requestProps["manifest"])["maxLength"] == contract.MaxDryRunManifestBytes,
		"manifest absolute cap drift")
	require(asMap(requestProps["namespace"]) != nil, "dry-run request must declare namespace for scope matching")
	// 동사·강제·토큰·값은 요청에 존재할 수 없습니다.
	for _, forbidden := range []string{
		"force", "token", "changeToken", "fieldManager", "dryRun", "propagationPolicy",
		"gracePeriodSeconds", "data", "stringData", "verb", "operation", "apply", "delete",
	} {
		require(requestProps[forbidden] == nil, "dry-run request must never declare "+forbidden)
	}

	response := asMap(schemas["ResourceDryRunResponse"])
	require(response != nil, "ResourceDryRunResponse missing")
	require(response["additionalProperties"] == false, "dry-run response schema must be closed")
	require(response["x-max-response-bytes"] == contract.MaxDryRunResponseBytes, "dry-run response byte bound drift")
	responseRequired := map[string]bool{}
	for _, field := range asSlice(response["required"]) {
		responseRequired[fmt.Sprint(field)] = true
	}
	// truncated·changeCount가 선택이 되면 UI가 잘린 diff를 완전한 diff로 그립니다.
	// warnings·violations·redacted도 같습니다 — 없으면 "검토 안 함"과 구분되지 않습니다.
	for _, field := range []string{
		"uid", "resourceVersion", "fieldManager", "outcome",
		"changes", "changeCount", "truncated", "warnings", "violations", "redacted",
	} {
		require(responseRequired[field], "dry-run response must always carry "+field)
	}
	responseProps := asMap(response["properties"])
	require(asMap(responseProps["fieldManager"])["const"] == contract.ResourceDryRunFieldManager,
		"fieldManager must stay a contract constant")
	require(asMap(responseProps["changes"])["maxItems"] == contract.MaxDryRunChanges, "diff change cap drift")
	require(asMap(responseProps["warnings"])["maxItems"] == contract.MaxDryRunWarnings, "warning cap drift")
	require(asMap(responseProps["violations"])["maxItems"] == contract.MaxDryRunViolations, "violation cap drift")
	// 정제 목록도 유계입니다 — 개수와 경로 길이 둘 다 없으면 응답 상한을 밀어냅니다.
	redacted := asMap(responseProps["redacted"])
	require(redacted["maxItems"] == contract.MaxDryRunRedacted, "redacted path count bound drift")
	require(asMap(redacted["items"])["maxLength"] == 512, "redacted path length bound drift")
	// raw 매니페스트·dry-run 객체·Secret 값·Status 원문은 응답에 자리가 없습니다.
	for _, forbidden := range []string{
		"yaml", "manifest", "object", "data", "stringData", "token", "force",
		"applied", "patch", "status", "details", "causes", "code",
	} {
		require(responseProps[forbidden] == nil, "dry-run response must never declare "+forbidden)
	}

	change := asMap(schemas["ResourceDryRunChange"])
	require(change["additionalProperties"] == false, "dry-run change schema must be closed")
	changeProps := asMap(change["properties"])
	require(asMap(changeProps["path"])["maxLength"] == 512, "diff path bound drift")
	for _, field := range []string{"before", "after"} {
		require(asMap(changeProps[field])["maxLength"] == contract.MaxDryRunValueBytes, "diff value bound drift: "+field)
	}
	require(asMap(changeProps["valueRedacted"]) != nil, "diff must be able to say a value was redacted")

	violation := asMap(schemas["ResourceDryRunViolation"])
	require(violation["additionalProperties"] == false, "dry-run violation schema must be closed")
	violationProps := asMap(violation["properties"])
	// Kubernetes Status 원문을 그대로 실어 나르는 필드가 생기면 내부 사유가 새어 나갑니다.
	for _, forbidden := range []string{"causes", "details", "reason", "code", "status"} {
		require(violationProps[forbidden] == nil, "violation must never carry raw Status field "+forbidden)
	}

	outcomes := asSlice(asMap(schemas["ResourceDryRunOutcome"])["enum"])
	require(fmt.Sprint(outcomes) == "[unchanged changed rejected]", "dry-run outcome vocabulary drift")
	rejected := asSlice(asMap(schemas["ResourceDryRunRejectedBy"])["enum"])
	require(fmt.Sprint(rejected) == "[validation admission conflict]", "dry-run rejection vocabulary drift")
	ops := asSlice(asMap(schemas["ResourceDryRunChangeOp"])["enum"])
	require(fmt.Sprint(ops) == "[added removed changed]", "dry-run change op vocabulary drift")

	// 카탈로그의 dryRun은 **선택** 필드입니다 — 필수로 만들면 기존 클라이언트가 깨집니다.
	descriptor := asMap(schemas["ResourceDescriptor"])
	require(asMap(asMap(descriptor["properties"])["dryRun"])["type"] == "boolean", "descriptor dryRun flag missing")
	for _, field := range asSlice(descriptor["required"]) {
		require(fmt.Sprint(field) != "dryRun", "descriptor dryRun must stay optional")
	}

	tooLarge := asMap(responses["ResourceManifestTooLarge"])
	require(tooLarge != nil, "shared response ResourceManifestTooLarge missing")
	require(asMap(asMap(tooLarge["headers"])["X-Request-ID"]) != nil, "ResourceManifestTooLarge missing X-Request-ID")

	codes := map[string]bool{}
	for _, code := range asSlice(asMap(asMap(asMap(schemas["APIError"])["properties"])["code"])["enum"]) {
		codes[fmt.Sprint(code)] = true
	}
	for _, code := range []string{
		"dryrun_unavailable", "dryrun_resource_denied", "dryrun_rate_limited", "dryrun_forbidden",
		"invalid_manifest", "manifest_mismatch", "manifest_too_large", "resource_version_mismatch",
	} {
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
		// 변경 검토 dry-run (ADR 0019 Phase 1)
		{"dry-run path drifts off the object boundary", func(d map[string]any) {
			asMap(d["paths"])["/api/v1/clusters/{clusterId}/resources/{group}/{version}/{resource}/dryrun"] =
				map[string]any{"post": map[string]any{"operationId": "x", "responses": map[string]any{}}}
		}},
		{"dry-run path removed", func(d map[string]any) {
			delete(asMap(d["paths"]), contract.ResourceDryRunPath)
		}},
		{"dry-run route gains a second verb", func(d map[string]any) {
			asMap(asMap(d["paths"])[contract.ResourceDryRunPath])["delete"] =
				map[string]any{"operationId": "deleteResource", "responses": map[string]any{}}
		}},
		{"dry-run requestBody becomes optional", func(d map[string]any) {
			asMap(asMap(asMap(d["paths"])[contract.ResourceDryRunPath])["post"])["requestBody"] =
				map[string]any{"required": false}
		}},
		{"dry-run 415 removed", func(d map[string]any) {
			delete(asMap(asMap(asMap(asMap(d["paths"])[contract.ResourceDryRunPath])["post"])["responses"]), "415")
		}},
		{"dry-run response leaks the manifest", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["ResourceDryRunResponse"])["properties"])["yaml"] = map[string]any{"type": "string"}
		}},
		{"dry-run request accepts force", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["ResourceDryRunRequest"])["properties"])["force"] = map[string]any{"type": "boolean"}
		}},
		{"dry-run request accepts a change token", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["ResourceDryRunRequest"])["properties"])["token"] = map[string]any{"type": "string"}
		}},
		{"manifest cap drift", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["ResourceDryRunRequest"])["properties"])["manifest"] = map[string]any{"maxLength": 16 << 20}
		}},
		{"fieldManager stops being a constant", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["ResourceDryRunResponse"])["properties"])["fieldManager"] = map[string]any{"type": "string"}
		}},
		{"diff change cap drift", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["ResourceDryRunResponse"])["properties"])["changes"] = map[string]any{"maxItems": 100_000}
		}},
		{"truncated becomes optional", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["schemas"])["ResourceDryRunResponse"])["required"] =
				[]any{"clusterId", "uid", "resourceVersion", "fieldManager", "outcome", "changes", "changeCount", "warnings", "violations", "redacted"}
		}},
		{"dry-run outcome vocabulary drift", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["schemas"])["ResourceDryRunOutcome"])["enum"] = []any{"ok", "failed"}
		}},
		{"violation carries raw Status causes", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["ResourceDryRunViolation"])["properties"])["causes"] = map[string]any{"type": "array"}
		}},
		{"redacted list becomes unbounded", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["ResourceDryRunResponse"])["properties"])["redacted"] =
				map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string"}}
		}},
		{"descriptor dryRun becomes required", func(d map[string]any) {
			descriptor := asMap(asMap(asMap(d["components"])["schemas"])["ResourceDescriptor"])
			descriptor["required"] = append(asSlice(descriptor["required"]), "dryRun")
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
		// 변경 검토 dry-run (ADR 0019 Phase 1)
		"export interface ResourceDryRunRequest {",
		"export interface ResourceDryRunChange {",
		"export interface ResourceDryRunViolation {",
		"export interface ResourceDryRunResponse {",
		`export type ResourceDryRunChangeOp = "added" | "removed" | "changed";`,
		`export type ResourceDryRunOutcome = "unchanged" | "changed" | "rejected";`,
		`export type ResourceDryRunRejectedBy = "validation" | "admission" | "conflict";`,
		"dryRun?: boolean;",
		"changeCount: number;",
	} {
		if !strings.Contains(ts, expected) {
			t.Errorf("TypeScript 계약이 어긋났습니다: %s", expected)
		}
	}
	// dry-run 응답도 값 필드를 선언하지 않습니다 — raw 매니페스트는 요청에만 있습니다.
	dryRunStart := strings.Index(ts, "export interface ResourceDryRunResponse {")
	if dryRunStart < 0 {
		t.Fatal("ResourceDryRunResponse 선언을 찾지 못했습니다")
	}
	dryRun := ts[dryRunStart:]
	if end := strings.Index(dryRun, "\n}"); end >= 0 {
		dryRun = dryRun[:end]
	}
	for _, forbidden := range []string{"yaml:", "manifest:", "object:", "data:", "stringData:", "token:", "force:"} {
		if strings.Contains(dryRun, forbidden) {
			t.Errorf("ResourceDryRunResponse가 %q를 선언했습니다", forbidden)
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
