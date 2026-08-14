package httpapi_test

// OpenAPI 문서(packages/contracts/openapi.yaml)와 라우터의 드리프트를 기계적으로
// 잡습니다. (#5) 라우터를 리플렉션하는 대신 server.go 소스의 HandleFunc 등록을
// 읽습니다 — 프로덕션 코드를 테스트용으로 고치지 않기 위해서입니다.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/dashboard"
	"gopkg.in/yaml.v3"
)

func asSlice(v any) []any { s, _ := v.([]any); return s }

// routePattern은 s.routes()의 등록 형식과 맞물립니다. 등록 방식을 바꾸면
// 이 테스트가 라우트 0개로 실패해 함께 고치게 됩니다.
var routePattern = regexp.MustCompile(`HandleFunc\("(GET|HEAD|POST|PUT|PATCH|DELETE) ([^"]+)"`)

func routerRoutes(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]bool{}
	for _, m := range routePattern.FindAllStringSubmatch(string(src), -1) {
		routes[m[1]+" "+m[2]] = true
	}
	if len(routes) == 0 {
		t.Fatal("server.go에서 라우트를 찾지 못했습니다 — 등록 형식이 바뀌었으면 routePattern을 갱신하세요")
	}
	return routes
}

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "packages", "contracts", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// specOperations는 "METHOD /path" → operation 맵입니다.
func specOperationsFromDoc(doc map[string]any) map[string]map[string]any {
	ops := map[string]map[string]any{}
	for path, item := range asMap(doc["paths"]) {
		for method, op := range asMap(item) {
			switch method {
			case "get", "head", "post", "put", "patch", "delete":
				ops[strings.ToUpper(method)+" "+path] = asMap(op)
			}
		}
	}
	return ops
}

func specOperations(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	return specOperationsFromDoc(doc)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestOpenAPIMatchesRouter — 라우터와 문서의 메서드·경로가 양방향으로 일치해야 합니다.
func TestOpenAPIMatchesRouter(t *testing.T) {
	routes := routerRoutes(t)
	ops := specOperations(t, loadSpec(t))

	for _, r := range sortedKeys(routes) {
		if _, ok := ops[r]; !ok {
			t.Errorf("라우터에는 있는데 openapi.yaml에 없습니다: %s", r)
		}
	}
	for _, o := range sortedKeys(ops) {
		if !routes[o] {
			t.Errorf("openapi.yaml에는 있는데 라우터에 없습니다: %s", o)
		}
	}
}

// TestOpenAPIOperationContract — 모든 operation은 안정된 operationId를 갖고,
// 모든 응답은 X-Request-ID 헤더를, 에러 응답은 APIError 스키마를 선언해야 합니다.
func TestOpenAPIOperationContract(t *testing.T) {
	doc := loadSpec(t)
	components := asMap(doc["components"])
	sharedResponses := asMap(components["responses"])

	seenIDs := map[string]string{}
	for op, def := range specOperations(t, doc) {
		id, _ := def["operationId"].(string)
		if id == "" {
			t.Errorf("%s: operationId가 없습니다", op)
		} else if prev, dup := seenIDs[id]; dup {
			t.Errorf("%s: operationId %q가 %s와 중복입니다", op, id, prev)
		} else {
			seenIDs[id] = op
		}

		for status, resp := range asMap(def["responses"]) {
			r := asMap(resp)
			// 공유 응답($ref)은 components.responses의 정의로 풀어서 검사합니다.
			if ref, ok := r["$ref"].(string); ok {
				name := ref[strings.LastIndex(ref, "/")+1:]
				r = asMap(sharedResponses[name])
				if r == nil {
					t.Errorf("%s %s: %s가 components.responses에 없습니다", op, status, ref)
					continue
				}
			}
			if asMap(asMap(r["headers"])["X-Request-ID"]) == nil {
				t.Errorf("%s %s: X-Request-ID 응답 헤더가 없습니다", op, status)
			}
			// readyz의 503은 에러 계약이 아니라 준비 상태 신호입니다. 기존 의미를 유지합니다.
			if op == "GET /readyz" && status == "503" {
				continue
			}
			if status[0] == '4' || status[0] == '5' {
				schema := asMap(asMap(asMap(asMap(r["content"])["application/json"])["schema"]))
				if ref, _ := schema["$ref"].(string); ref != "#/components/schemas/APIError" {
					t.Errorf("%s %s: 에러 응답이 APIError를 참조하지 않습니다: %v", op, status, schema)
				}
			}
		}
	}
}

// TestOpenAPIErrorAndAuthBoundary — APIError 필수 필드와 인증 경계입니다.
// 운영 경로(/healthz·/readyz·/version)는 security: []로 인증이 없고,
// /api/v1/*는 전역 bearerAuth를 그대로 상속해야 합니다.
func TestOpenAPIErrorAndAuthBoundary(t *testing.T) {
	doc := loadSpec(t)

	apiErr := asMap(asMap(asMap(doc["components"])["schemas"])["APIError"])
	required, _ := apiErr["required"].([]any)
	want := map[string]bool{"code": false, "message": false, "requestId": false}
	for _, f := range required {
		if name, ok := f.(string); ok {
			want[name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("APIError.required에 %q가 없습니다", name)
		}
	}

	global, _ := doc["security"].([]any)
	if len(global) == 0 {
		t.Error("전역 security(bearerAuth)가 없습니다")
	}
	for op, def := range specOperations(t, doc) {
		path := op[strings.Index(op, " ")+1:]
		security, declared := def["security"].([]any)
		operational := path == "/healthz" || path == "/readyz" || path == "/version" || path == "/metrics"
		if operational && (!declared || len(security) != 0) {
			t.Errorf("%s: 운영 경로는 security: []여야 합니다", op)
		}
		if !operational && declared {
			t.Errorf("%s: /api/v1 경로가 전역 인증을 덮어썼습니다", op)
		}
	}
}

func TestOpenAPITimeParametersMatchParser(t *testing.T) {
	params := asMap(asMap(loadSpec(t)["components"])["parameters"])
	for _, name := range []string{"From", "To"} {
		param := asMap(params[name])
		schema := asMap(param["schema"])
		variants, ok := schema["oneOf"].([]any)
		if !ok || len(variants) != 2 {
			t.Errorf("%s must declare RFC3339 and epoch-millisecond string variants", name)
			continue
		}
		var dateTime, epochMillis bool
		for _, raw := range variants {
			variant := asMap(raw)
			dateTime = dateTime || (variant["type"] == "string" && variant["format"] == "date-time")
			epochMillis = epochMillis || (variant["type"] == "string" && variant["pattern"] == "^-?[0-9]+$")
		}
		if !dateTime || !epochMillis {
			t.Errorf("%s schema does not match timerange.Parse: %v", name, schema)
		}
	}
}

func dashboardContractErrors(doc map[string]any) []string {
	var errors []string
	components := asMap(doc["components"])
	schemas := asMap(components["schemas"])
	params := asMap(components["parameters"])
	responses := asMap(components["responses"])
	ops := specOperationsFromDoc(doc)
	require := func(ok bool, message string) {
		if !ok {
			errors = append(errors, message)
		}
	}
	requiredSet := func(schema map[string]any) string {
		fields := make([]string, 0)
		for _, field := range asSlice(schema["required"]) {
			fields = append(fields, fmt.Sprint(field))
		}
		sort.Strings(fields)
		return strings.Join(fields, ",")
	}
	capabilities := asMap(schemas["DashboardCapabilities"])
	require(requiredSet(capabilities) == "canEdit,canPublish,enabled,maxDrafts,maxWidgets", "capabilities required fields drift")
	capProps := asMap(capabilities["properties"])
	require(asMap(capProps["maxDrafts"])["const"] == dashboard.MaxDraftsPerOwner, "maxDrafts drift")
	require(asMap(capProps["maxWidgets"])["const"] == 24, "maxWidgets drift")
	definition := asMap(schemas["DashboardDefinition"])
	defProps := asMap(definition["properties"])
	require(asMap(defProps["schemaVersion"])["const"] == dashboard.SchemaVersion, "schemaVersion drift")
	require(asMap(defProps["widgets"])["maxItems"] == 24, "widget bound drift")
	require(definition["x-canonical-max-bytes"] == dashboard.MaxDefinitionBytes, "canonical byte bound drift")
	definitionRequest := asMap(schemas["DashboardDefinitionRequest"])
	require(requiredSet(definitionRequest) == "definition", "definition request required fields drift")
	draft := asMap(schemas["DashboardDraft"])
	require(requiredSet(draft) == "createdAt,definition,id,owned,revision,schemaVersion,state,updatedAt", "draft required fields drift")
	draftProps := asMap(draft["properties"])
	states := asSlice(asMap(draftProps["state"])["enum"])
	require(fmt.Sprint(states) == "[draft submitted approved]", "draft state drift")
	require(asMap(draftProps["revision"])["minimum"] == 1, "draft revision drift")
	require(asMap(draftProps["owned"])["type"] == "boolean", "draft ownership drift")
	require(asMap(draftProps["definition"])["$ref"] == "#/components/schemas/DashboardDefinition", "draft definition drift")
	page := asMap(schemas["DashboardDraftPage"])
	require(requiredSet(page) == "items", "page required fields drift")
	pageProps := asMap(page["properties"])
	require(asMap(pageProps["items"])["maxItems"] == dashboard.MaxListPage, "page bound drift")
	require(asMap(asMap(params["DashboardListLimit"])["schema"])["maximum"] == dashboard.MaxListPage, "limit bound drift")
	require(asMap(params["IfMatchRevision"])["required"] == true, "If-Match must be required")
	ifNoneMatch := asMap(params["IfNoneMatchExport"])
	exportETagSchema := asMap(asMap(asMap(components["headers"])["DashboardExportETag"])["schema"])
	require(ifNoneMatch["name"] == "If-None-Match" && ifNoneMatch["required"] == false, "If-None-Match export parameter drift")
	require(asMap(ifNoneMatch["schema"])["pattern"] == exportETagSchema["pattern"], "export ETag pattern drift")

	for operation, statuses := range map[string][]string{
		"POST /api/v1/dashboard-drafts":              {"201", "400", "403", "409", "413", "415", "503"},
		"PUT /api/v1/dashboard-drafts/{id}":          {"200", "400", "403", "404", "409", "413", "415", "428", "503"},
		"GET /api/v1/dashboard-drafts":               {"200", "400", "403", "503"},
		"POST /api/v1/dashboard-drafts/{id}/approve": {"200", "403", "404", "409", "428", "503"},
		"GET /api/v1/dashboard-drafts/{id}/export":   {"200", "304", "403", "404", "503"},
	} {
		actual := asMap(ops[operation]["responses"])
		for _, status := range statuses {
			require(actual[status] != nil, operation+" missing status "+status)
		}
	}
	for _, operation := range []string{"POST /api/v1/dashboard-drafts", "PUT /api/v1/dashboard-drafts/{id}"} {
		require(asMap(ops[operation]["requestBody"])["$ref"] == "#/components/requestBodies/DashboardDefinitionRequest", operation+" requestBody drift")
	}
	listParams := asSlice(ops["GET /api/v1/dashboard-drafts"]["parameters"])
	require(len(listParams) == 2, "list parameters drift")
	for _, response := range []string{"DashboardCreated", "DashboardDraftOK"} {
		require(asMap(asMap(responses[response])["headers"])["ETag"] != nil, response+" revision ETag missing")
	}
	exportHeaders := asMap(asMap(responses["DashboardExportOK"])["headers"])
	require(exportHeaders["ETag"] != nil && exportHeaders["Content-Disposition"] != nil, "export headers missing")
	return errors
}

func TestDashboardOpenAPIAndTypeDriftGuards(t *testing.T) {
	doc := loadSpec(t)
	if errors := dashboardContractErrors(doc); len(errors) != 0 {
		t.Fatal(strings.Join(errors, "; "))
	}
	ts, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "packages", "contracts", "src", "index.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`export type DashboardDraftState = "draft" | "submitted" | "approved"`,
		`export interface DashboardCapabilities { enabled:boolean; canEdit:boolean; canPublish:boolean; maxDrafts:number; maxWidgets:number; }`,
		`export interface DashboardDefinitionRequest { definition:DashboardDefinition; }`,
		`export interface DashboardDraft { id:string; revision:number; state:DashboardDraftState; owned:boolean; schemaVersion:1; definition:DashboardDefinition; createdAt:string; updatedAt:string; }`,
		`export interface DashboardDraftPage { items:DashboardDraft[]; nextCursor?:string; }`,
	} {
		if !strings.Contains(string(ts), expected) {
			t.Errorf("TypeScript dashboard contract drifted: %s", expected)
		}
	}

	mutations := []struct {
		name string
		edit func(map[string]any)
	}{
		{"request body removed", func(d map[string]any) {
			delete(asMap(asMap(asMap(d["paths"])["/api/v1/dashboard-drafts"])["post"]), "requestBody")
		}},
		{"state enum drift", func(d map[string]any) {
			asMap(asMap(asMap(asMap(asMap(d["components"])["schemas"])["DashboardDraft"])["properties"])["state"])["enum"] = []any{"draft", "approved"}
		}},
		{"widget cap drift", func(d map[string]any) {
			asMap(asMap(asMap(asMap(d["components"])["schemas"])["DashboardDefinition"])["properties"])["widgets"] = map[string]any{"type": "array", "maxItems": 25}
		}},
		{"415 removed", func(d map[string]any) {
			delete(asMap(asMap(asMap(asMap(d["paths"])["/api/v1/dashboard-drafts/{id}"])["put"])["responses"]), "415")
		}},
		{"If-Match optional", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["parameters"])["IfMatchRevision"])["required"] = false
		}},
		{"page cap drift", func(d map[string]any) {
			asMap(asMap(asMap(asMap(asMap(d["components"])["schemas"])["DashboardDraftPage"])["properties"])["items"])["maxItems"] = 51
		}},
		{"export ETag removed", func(d map[string]any) {
			delete(asMap(asMap(asMap(asMap(d["components"])["responses"])["DashboardExportOK"])["headers"]), "ETag")
		}},
		{"draft definition no longer required", func(d map[string]any) {
			asMap(asMap(asMap(d["components"])["schemas"])["DashboardDraft"])["required"] = []any{"id", "revision", "state", "owned", "schemaVersion", "createdAt", "updatedAt"}
		}},
		{"export conditional request removed", func(d map[string]any) { delete(asMap(asMap(d["components"])["parameters"]), "IfNoneMatchExport") }},
	}
	for _, mutation := range mutations {
		raw, _ := yaml.Marshal(doc)
		var changed map[string]any
		if err := yaml.Unmarshal(raw, &changed); err != nil {
			t.Fatal(err)
		}
		mutation.edit(changed)
		if errors := dashboardContractErrors(changed); len(errors) == 0 {
			t.Errorf("negative mutation was masked: %s", mutation.name)
		}
	}
}
