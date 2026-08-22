package resourcecatalog

// 매니페스트 파서와 구조 diff 테스트 (ADR 0019 Phase 1)
//
// 파서 쪽은 "무엇을 거부하는가"가 전부입니다 — 거부하지 못하는 문서 하나가
// 요청 하나로 CPU·메모리를 태우거나, 검증이 본 값과 다른 값을 적용하게 만듭니다.
// diff 쪽은 "같은 입력에서 같은 순서·같은 상한"이 전부입니다.

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// dryRunTestRawSecretMarker는 민감 값에만 존재하는 표식입니다. diff 결과 어디에서든
// 이 문자열이 보이면 정제 경계가 뚫린 것입니다.
const dryRunTestRawSecretMarker = "RAW_SECRET_VALUE_MARKER"

/* ── 파서 ────────────────────────────────────────────────────────────────── */

// TestDecodeManifestAcceptsOneWellFormedDocument — YAML과 JSON 모두 받아들이고,
// 값 타입은 unstructured가 받는 것으로만 만듭니다.
func TestDecodeManifestAcceptsOneWellFormedDocument(t *testing.T) {
	yamlDoc := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api\n" +
		"spec:\n  replicas: 3\n  ratio: 0.5\n  enabled: true\n  note: null\n  tags:\n  - a\n  - b\n"
	obj, err := decodeManifest(yamlDoc)
	if err != nil {
		t.Fatalf("정상 YAML이 거부되었습니다: %v", err)
	}
	spec, _ := obj["spec"].(map[string]any)
	if _, ok := spec["replicas"].(int64); !ok {
		t.Errorf("정수가 int64가 아닙니다: %T", spec["replicas"])
	}
	if _, ok := spec["ratio"].(float64); !ok {
		t.Errorf("실수가 float64가 아닙니다: %T", spec["ratio"])
	}
	if _, ok := spec["enabled"].(bool); !ok {
		t.Errorf("불리언이 bool이 아닙니다: %T", spec["enabled"])
	}
	if spec["note"] != nil {
		t.Errorf("null이 nil이 아닙니다: %v", spec["note"])
	}
	if list, ok := spec["tags"].([]any); !ok || len(list) != 2 {
		t.Errorf("목록이 []any가 아닙니다: %T", spec["tags"])
	}
	// unstructured가 받는 타입만 나왔는지는 JSON 왕복으로 확인합니다.
	if _, err := json.Marshal(obj); err != nil {
		t.Fatalf("결과를 직렬화하지 못했습니다: %v", err)
	}

	// JSON도 같은 결과를 냅니다 — JSON은 YAML의 부분집합입니다.
	fromJSON, err := decodeManifest(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"api"}}`)
	if err != nil {
		t.Fatalf("정상 JSON이 거부되었습니다: %v", err)
	}
	if fromJSON["kind"] != "ConfigMap" {
		t.Errorf("JSON 해석이 틀렸습니다: %v", fromJSON)
	}

	// 앞뒤 문서 구분자 하나는 여전히 단일 문서입니다.
	if _, err := decodeManifest("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api\n"); err != nil {
		t.Errorf("선행 구분자가 있는 단일 문서가 거부되었습니다: %v", err)
	}
}

// TestDecodeManifestRejectsHostileDocuments — 거부해야 하는 문서들입니다.
func TestDecodeManifestRejectsHostileDocuments(t *testing.T) {
	valid := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api\n"
	nodes := "apiVersion: v1\nkind: ConfigMap\ndata: [" +
		strings.Repeat("1,", maxManifestNodes+10) + "1]\n"

	for _, tc := range []struct {
		name string
		in   string
	}{
		{"빈 문서", ""},
		{"공백뿐", "   \n\n"},
		{"구분자뿐", "---\n---\n"},
		{"다중 문서", valid + "---\n" + valid},
		{"최상위가 목록", "- a\n- b\n"},
		{"최상위가 스칼라", "just-a-string\n"},
		{"중복 키", "apiVersion: v1\nkind: ConfigMap\nkind: Secret\n"},
		{"중첩 중복 키", "metadata:\n  name: a\n  name: b\n"},
		{"anchor", "metadata: &anchor\n  name: api\n"},
		{"alias", "a: &x\n  name: api\nb: *x\n"},
		{"병합 키", "a: &x\n  name: api\nb:\n  <<: *x\n"},
		{"깊은 중첩", "data:\n  a: " + strings.Repeat("[", maxManifestDepth+4) + strings.Repeat("]", maxManifestDepth+4) + "\n"},
		{"노드 폭증", nodes},
		{"거대 스칼라", "data:\n  a: " + strings.Repeat("x", maxManifestScalarBytes+1) + "\n"},
		{"거대 키", "data:\n  " + strings.Repeat("k", maxManifestScalarBytes+1) + ": v\n"},
		{"해석 불가 태그", "data:\n  blob: !!binary aGVsbG8=\n"},
		{"파싱 실패", "a: [1, 2\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeManifest(tc.in); !errors.Is(err, ErrManifestInvalid) {
				t.Fatalf("거부되어야 하는데 %v입니다", err)
			}
		})
	}
}

// TestDecodeManifestKeepsTimestampsAsStrings — 따옴표 없는 날짜를 거부하지 않되,
// Kubernetes JSON과 같은 문자열로 만듭니다.
func TestDecodeManifestKeepsTimestampsAsStrings(t *testing.T) {
	obj, err := decodeManifest("metadata:\n  creationTimestamp: 2026-08-01T00:00:00Z\n")
	if err != nil {
		t.Fatalf("타임스탬프가 거부되었습니다: %v", err)
	}
	meta, _ := obj["metadata"].(map[string]any)
	if _, ok := meta["creationTimestamp"].(string); !ok {
		t.Fatalf("타임스탬프가 문자열이 아닙니다: %T", meta["creationTimestamp"])
	}
}

/* ── diff 헬퍼 ───────────────────────────────────────────────────────────── */

var dryRunTestDiffGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

func dryRunTestObject(extra map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": "api", "namespace": "payments",
			"uid": "u-1", "resourceVersion": "1", "creationTimestamp": "2026-08-01T00:00:00Z",
		},
	}
	for k, v := range extra {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}

func dryRunTestPaths(changes []contract.ResourceDryRunChange) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Path)
	}
	return out
}

// dryRunTestCompare는 "성공해야 하는 비교"용 헬퍼입니다. 실패 경로는 각 테스트가
// compareForReview를 직접 불러 오류를 확인합니다.
func dryRunTestCompare(t *testing.T, before, after *unstructured.Unstructured, gvr schema.GroupVersionResource) reviewDiff {
	t.Helper()
	diff, err := compareForReview(before, after, gvr)
	if err != nil {
		t.Fatalf("비교가 실패했습니다: %v", err)
	}
	return diff
}

/* ── diff ────────────────────────────────────────────────────────────────── */

// TestCompareIgnoresVolatileAndReportsRedaction — 휘발성 필드는 변경이 아니고,
// 제외한 경로는 목록으로 알립니다.
func TestCompareIgnoresVolatileAndReportsRedaction(t *testing.T) {
	before := dryRunTestObject(map[string]any{"data": map[string]any{"LOG_LEVEL": "info"}})
	after := dryRunTestObject(map[string]any{"data": map[string]any{"LOG_LEVEL": "info"}})
	// dry-run 결과에서 늘 달라지는 것들입니다.
	afterMeta, _ := after.Object["metadata"].(map[string]any)
	afterMeta["resourceVersion"] = "2"
	afterMeta["generation"] = int64(9)
	afterMeta["managedFields"] = []any{map[string]any{"manager": "k8s-dashboard-dryrun"}}
	after.Object["status"] = map[string]any{"phase": "Bound"}

	diff := dryRunTestCompare(t, before, after, dryRunTestDiffGVR)
	if diff.total != 0 {
		t.Fatalf("휘발성 필드가 변경으로 셌습니다: %+v", diff.changes)
	}
	if diff.truncated {
		t.Error("잘리지 않았는데 truncated입니다")
	}
	joined := strings.Join(diff.redacted, ",")
	for _, want := range []string{"status", "metadata.managedFields", "metadata.resourceVersion", "metadata.uid"} {
		if !strings.Contains(joined, want) {
			t.Errorf("제외 경로에 %s가 없습니다: %v", want, diff.redacted)
		}
	}
	// 입력 객체는 건드리지 않습니다.
	if _, has := before.Object["metadata"].(map[string]any)["uid"]; !has {
		t.Error("입력 객체가 변형되었습니다")
	}
}

// TestCompareIsDeterministicAndSorted — 같은 입력이면 같은 순서를 냅니다.
// map 순회 순서에 결과가 흔들리면 화면이 갱신마다 다르게 보입니다.
func TestCompareIsDeterministicAndSorted(t *testing.T) {
	before := dryRunTestObject(map[string]any{"data": map[string]any{
		"z": "1", "a": "1", "m": "1", "b": "1", "y": "1",
	}})
	after := dryRunTestObject(map[string]any{"data": map[string]any{
		"z": "2", "a": "2", "m": "2", "b": "2", "y": "2",
	}})

	first := dryRunTestCompare(t, before, after, dryRunTestDiffGVR)
	for i := 0; i < 20; i++ {
		again := dryRunTestCompare(t, before, after, dryRunTestDiffGVR)
		if !dryRunTestEqualChanges(first.changes, again.changes) {
			t.Fatalf("같은 입력이 다른 순서를 냈습니다:\n%v\n%v", dryRunTestPaths(first.changes), dryRunTestPaths(again.changes))
		}
	}
	paths := dryRunTestPaths(first.changes)
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("경로가 정렬되어 있지 않습니다: %v", paths)
	}
	if first.total != 5 || len(first.changes) != 5 {
		t.Fatalf("변경 수가 5가 아닙니다: %d/%d", first.total, len(first.changes))
	}
	for _, c := range first.changes {
		if c.Op != contract.DryRunChangeChanged || c.Before != `"1"` || c.After != `"2"` {
			t.Errorf("변경 표현이 틀렸습니다: %+v", c)
		}
	}
}

func dryRunTestEqualChanges(a, b []contract.ResourceDryRunChange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCompareMatchesListsByName — 컨테이너 하나를 앞에 끼워 넣었을 뿐인데 뒤쪽
// 전부가 변경으로 보이면 검토가 쓸모없어집니다.
func TestCompareMatchesListsByName(t *testing.T) {
	container := func(name, image string) map[string]any {
		return map[string]any{"name": name, "image": image}
	}
	before := dryRunTestObject(map[string]any{"spec": map[string]any{
		"containers": []any{container("api", "api:1"), container("sidecar", "envoy:1")},
	}})
	after := dryRunTestObject(map[string]any{"spec": map[string]any{
		"containers": []any{container("init", "busybox:1"), container("api", "api:2"), container("sidecar", "envoy:1")},
	}})

	diff := dryRunTestCompare(t, before, after, dryRunTestDiffGVR)
	got := dryRunTestPaths(diff.changes)
	want := []string{"spec.containers[api].image", "spec.containers[init]"}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("name 짝짓기가 되지 않았습니다: %v", got)
	}
	// 추가된 원소는 **항목 하나**입니다. 안쪽 leaf를 펼치지 않습니다.
	for _, c := range diff.changes {
		if c.Path == "spec.containers[init]" && c.Op != contract.DryRunChangeAdded {
			t.Errorf("추가가 added가 아닙니다: %+v", c)
		}
	}
}

// TestCompareFallsBackToIndexForUnnamedLists — name이 없거나 겹치면 인덱스입니다.
func TestCompareFallsBackToIndexForUnnamedLists(t *testing.T) {
	before := dryRunTestObject(map[string]any{"spec": map[string]any{"ports": []any{int64(80), int64(443)}}})
	after := dryRunTestObject(map[string]any{"spec": map[string]any{"ports": []any{int64(80), int64(8443), int64(9000)}}})

	diff := dryRunTestCompare(t, before, after, dryRunTestDiffGVR)
	got := dryRunTestPaths(diff.changes)
	sort.Strings(got)
	want := []string{"spec.ports[1]", "spec.ports[2]"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("인덱스 짝짓기가 되지 않았습니다: %v", got)
	}
}

// TestCompareTreatsSubtreesAsOneChange — 추가·삭제된 하위 트리는 항목 하나입니다.
func TestCompareTreatsSubtreesAsOneChange(t *testing.T) {
	before := dryRunTestObject(nil)
	after := dryRunTestObject(map[string]any{"spec": map[string]any{
		"a": map[string]any{"b": map[string]any{"c": "1", "d": "2", "e": "3"}},
	}})
	diff := dryRunTestCompare(t, before, after, dryRunTestDiffGVR)
	if diff.total != 1 || len(diff.changes) != 1 {
		t.Fatalf("하위 트리가 펼쳐졌습니다: %v", dryRunTestPaths(diff.changes))
	}
	c := diff.changes[0]
	if c.Path != "spec" || c.Op != contract.DryRunChangeAdded || c.Before != "" {
		t.Fatalf("추가 표현이 틀렸습니다: %+v", c)
	}
	if !strings.Contains(c.After, `"c":"1"`) {
		t.Errorf("추가된 값이 실리지 않았습니다: %q", c.After)
	}
}

// TestCompareBoundsValuesOnRuneBoundary — 값은 512바이트에서, 그러나 rune 경계에서
// 잘립니다. 바이트 중간에서 자르면 잘못된 UTF-8이 JSON으로 나갑니다.
func TestCompareBoundsValuesOnRuneBoundary(t *testing.T) {
	before := dryRunTestObject(map[string]any{"data": map[string]any{"note": "short"}})
	after := dryRunTestObject(map[string]any{"data": map[string]any{"note": strings.Repeat("가", 400)}})

	diff := dryRunTestCompare(t, before, after, dryRunTestDiffGVR)
	if len(diff.changes) != 1 {
		t.Fatalf("변경이 1개가 아닙니다: %v", dryRunTestPaths(diff.changes))
	}
	c := diff.changes[0]
	if len(c.After) > contract.MaxDryRunValueBytes {
		t.Fatalf("값 상한을 넘었습니다: %d", len(c.After))
	}
	if !c.ValueTruncated {
		t.Error("잘렸는데 valueTruncated가 false입니다")
	}
	for _, r := range c.After {
		if r == 0xFFFD {
			t.Fatalf("UTF-8이 깨졌습니다: %q", c.After)
		}
	}
}

// TestCompareCapsChangesButCountsAllOfThem — 상한을 넘겨도 전체 개수는 정확해야
// 합니다. 잘렸다는 사실을 숨기면 화면이 부분 diff를 전체로 그립니다.
func TestCompareCapsChangesButCountsAllOfThem(t *testing.T) {
	const n = contract.MaxDryRunChanges + 137
	beforeData := make(map[string]any, n)
	afterData := make(map[string]any, n)
	for i := 0; i < n; i++ {
		key := "k" + strconv.Itoa(100000+i)
		beforeData[key] = "before"
		afterData[key] = "after"
	}
	before := dryRunTestObject(map[string]any{"data": beforeData})
	after := dryRunTestObject(map[string]any{"data": afterData})

	diff := dryRunTestCompare(t, before, after, dryRunTestDiffGVR)
	if diff.total != n {
		t.Fatalf("전체 변경 수가 %d가 아닙니다: %d", n, diff.total)
	}
	if len(diff.changes) != contract.MaxDryRunChanges {
		t.Fatalf("보유 개수가 상한이 아닙니다: %d", len(diff.changes))
	}
	if !diff.truncated {
		t.Error("잘렸는데 truncated가 false입니다")
	}
	paths := dryRunTestPaths(diff.changes)
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("절단 결과가 정렬되어 있지 않습니다")
	}
	// 남는 것은 **경로 순 앞쪽**입니다. 두 번 돌려도 같아야 합니다.
	if paths[0] != "data.k100000" {
		t.Errorf("절단 기준이 경로 순이 아닙니다: %s", paths[0])
	}
	again := dryRunTestCompare(t, before, after, dryRunTestDiffGVR)
	if !dryRunTestEqualChanges(diff.changes, again.changes) {
		t.Error("절단 결과가 실행마다 다릅니다")
	}
	// 응답 본문 상한 안에 들어가는지 대략 확인합니다.
	rendered, err := json.Marshal(diff.changes)
	if err != nil {
		t.Fatalf("직렬화 실패: %v", err)
	}
	if len(rendered) > contract.MaxDryRunResponseBytes {
		t.Fatalf("변경 목록만으로 응답 상한을 넘었습니다: %d", len(rendered))
	}
}

// TestCompareReportsSensitiveChangeWithoutValues — 민감 경로는 "바뀌었다"만
// 알리고 값은 싣지 않습니다. 정제 후에 비교하면 이 사실 자체가 사라집니다.
func TestCompareReportsSensitiveChangeWithoutValues(t *testing.T) {
	withAnnotation := func(value string) *unstructured.Unstructured {
		obj := dryRunTestObject(map[string]any{"data": map[string]any{"LOG_LEVEL": "info"}})
		meta, _ := obj.Object["metadata"].(map[string]any)
		meta["annotations"] = map[string]any{
			"team":                  "payments",
			"example.com/api-token": value,
			"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"LOG_LEVEL":"info"}}`,
		}
		return obj
	}
	diff := dryRunTestCompare(t,
		withAnnotation("old-"+dryRunTestRawSecretMarker),
		withAnnotation("new-"+dryRunTestRawSecretMarker),
		dryRunTestDiffGVR)

	var found *contract.ResourceDryRunChange
	for i := range diff.changes {
		if strings.Contains(diff.changes[i].Path, "api-token") {
			found = &diff.changes[i]
		}
	}
	if found == nil {
		t.Fatalf("민감 필드 변경이 보고되지 않았습니다: %v", dryRunTestPaths(diff.changes))
	}
	if !found.ValueRedacted {
		t.Error("valueRedacted가 아닙니다")
	}
	if found.Before != "" || found.After != "" {
		t.Errorf("민감 값이 실렸습니다: %+v", *found)
	}
	// reviewDiff는 비공개 필드라 그대로 직렬화하면 빈 객체가 됩니다 —
	// 실제로 밖으로 나가는 조각(changes·redacted)을 직접 확인합니다.
	rendered, _ := json.Marshal(struct {
		Changes  []contract.ResourceDryRunChange
		Redacted []string
	}{diff.changes, diff.redacted})
	if strings.Contains(string(rendered), dryRunTestRawSecretMarker) {
		t.Errorf("결과에 민감 값이 남았습니다: %s", rendered)
	}
	// 민감하지 않은 annotation은 그대로 비교됩니다.
	if strings.Contains(strings.Join(diff.redacted, ","), "annotations[team]") {
		t.Error("일반 annotation이 정제되었습니다")
	}
}

// TestCompareRemovesSecretValuesEvenIfReached — Secret은 GVR·kind 두 겹으로 이미
// 막히지만, 그래도 값이 diff로 새지 않는지 마지막 겹을 확인합니다.
func TestCompareRemovesSecretValuesEvenIfReached(t *testing.T) {
	before := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "db", "namespace": "payments"},
		"data":     map[string]any{"password": "old-" + dryRunTestRawSecretMarker},
	}}
	after := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "db", "namespace": "payments"},
		"data":     map[string]any{"password": "new-" + dryRunTestRawSecretMarker},
	}}
	diff := dryRunTestCompare(t, before, after, secretGVR)
	rendered, _ := json.Marshal(struct {
		Changes  []contract.ResourceDryRunChange
		Redacted []string
	}{diff.changes, diff.redacted})
	for _, leak := range []string{dryRunTestRawSecretMarker} {
		if strings.Contains(string(rendered), leak) {
			t.Fatalf("Secret 값이 diff로 새어나갔습니다: %s", rendered)
		}
	}
	if diff.total != 1 || !diff.changes[0].ValueRedacted {
		t.Fatalf("변경 사실은 남아야 합니다: %+v", diff.changes)
	}
}

// TestBoundedPathsAreSortedAndCapped — 정제 경로 목록도 유계입니다.
func TestBoundedPathsAreSortedAndCapped(t *testing.T) {
	set := map[string]bool{}
	for i := 0; i < contract.MaxDryRunRedacted*3; i++ {
		set["metadata.annotations[p"+strconv.Itoa(1000+i)+"]"] = true
	}
	set[strings.Repeat("x", 2000)] = true
	got := boundedPaths(set)
	if len(got) > contract.MaxDryRunRedacted {
		t.Fatalf("경로 개수 상한을 넘었습니다: %d", len(got))
	}
	if !sort.StringsAreSorted(got) {
		t.Error("경로가 정렬되어 있지 않습니다")
	}
	for _, p := range got {
		if len(p) > dryRunTextBytes {
			t.Errorf("경로 길이 상한을 넘었습니다: %d", len(p))
		}
	}
}

// TestDiffPathEncodingIsInjective — 서로 다른 키 계열은 서로 다른 경로가 되어야
// 합니다. 같은 경로가 되면 사용자는 **다른 필드의 변경**을 보고 승인하게 됩니다.
func TestDiffPathEncodingIsInjective(t *testing.T) {
	// annotation·label·ConfigMap data에서 실제로 나올 수 있는 키들입니다.
	keys := []string{
		"a", "a.b", "a-b", "a_b", "0", "01", "1a", "",
		"a[0]", "a]b", "a[b", `a"b`, `a\b`, "a\tb", "a\nb", "a b",
		"example.com/token", "가", "a.b.c", "a\\x5db",
	}
	parents := []string{"", "root", "root.child", `["x.y"]`}
	seen := map[string]string{}
	for _, parent := range parents {
		for _, key := range keys {
			path := joinDiffPath(parent, key)
			id := parent + "\x00" + key
			if prev, dup := seen[path]; dup {
				t.Fatalf("서로 다른 키가 같은 경로가 되었습니다: %q vs %q → %q", prev, id, path)
			}
			seen[path] = id
		}
	}

	// 목록 인덱스와 목록 이름도 섞이면 안 됩니다.
	if indexPathSegment(0) == namePathSegment("0") {
		t.Fatal("인덱스 0과 이름 \"0\"이 같은 세그먼트입니다")
	}
	segSeen := map[string]string{}
	for i := 0; i < 12; i++ {
		segSeen[indexPathSegment(i)] = "index"
	}
	for _, name := range keys {
		seg := namePathSegment(name)
		if prev, dup := segSeen[seg]; dup {
			t.Fatalf("목록 세그먼트가 겹칩니다: %q(%s) → %q", name, prev, seg)
		}
		segSeen[seg] = "name:" + name
	}

	// 단순 식별자는 읽기 쉬운 점 경로를 유지합니다(UX 회귀 방지).
	if got := joinDiffPath("spec.template", "image"); got != "spec.template.image" {
		t.Errorf("단순 키 표기가 바뀌었습니다: %q", got)
	}
	if got := namePathSegment("api"); got != "[api]" {
		t.Errorf("단순 목록 이름 표기가 바뀌었습니다: %q", got)
	}
	// 특수 키는 유계 따옴표 표현입니다.
	if got := joinDiffPath("metadata.annotations", "example.com/token"); got != `metadata.annotations["example.com/token"]` {
		t.Errorf("특수 키 표기가 틀렸습니다: %q", got)
	}
}

// TestDiffPathsDistinguishFlatAndNestedKeys — 끝단 확인입니다. 키 `a.b` 하나와
// 중첩된 `a`→`b`가 같은 경로가 되면 안 됩니다.
func TestDiffPathsDistinguishFlatAndNestedKeys(t *testing.T) {
	empty := dryRunTestObject(map[string]any{"data": map[string]any{}})
	flat := dryRunTestObject(map[string]any{"data": map[string]any{"a.b": "1"}})
	nested := dryRunTestObject(map[string]any{"data": map[string]any{"a": map[string]any{"b": "1"}}})

	flatDiff := dryRunTestCompare(t, empty, flat, dryRunTestDiffGVR)
	nestedDiff := dryRunTestCompare(t, empty, nested, dryRunTestDiffGVR)
	if len(flatDiff.changes) != 1 || len(nestedDiff.changes) != 1 {
		t.Fatalf("각각 변경 1개여야 합니다: %v / %v",
			dryRunTestPaths(flatDiff.changes), dryRunTestPaths(nestedDiff.changes))
	}
	if flatDiff.changes[0].Path == nestedDiff.changes[0].Path {
		t.Fatalf("평면 키와 중첩 키가 같은 경로입니다: %q", flatDiff.changes[0].Path)
	}
	if flatDiff.changes[0].Path != `data["a.b"]` {
		t.Errorf("평면 키가 escape되지 않았습니다: %q", flatDiff.changes[0].Path)
	}
	if nestedDiff.changes[0].Path != "data.a" {
		t.Errorf("중첩 키 표기가 바뀌었습니다: %q", nestedDiff.changes[0].Path)
	}
	// 결정성은 escape 뒤에도 그대로입니다.
	again := dryRunTestCompare(t, empty, flat, dryRunTestDiffGVR)
	if !dryRunTestEqualChanges(flatDiff.changes, again.changes) {
		t.Error("escape된 경로가 실행마다 다릅니다")
	}
}

// TestRedactedPathsUseTheSameEscapePolicy — 정제 목록도 같은 표기를 씁니다.
// 원문 키를 그대로 이어 붙이면 diff와 다른 규칙의 경로가 한 응답에 섞입니다.
func TestRedactedPathsUseTheSameEscapePolicy(t *testing.T) {
	withAnnotation := func() *unstructured.Unstructured {
		obj := dryRunTestObject(map[string]any{"data": map[string]any{"LOG_LEVEL": "info"}})
		meta, _ := obj.Object["metadata"].(map[string]any)
		meta["annotations"] = map[string]any{"example.com/api-token": "v"}
		return obj
	}
	diff := dryRunTestCompare(t, withAnnotation(), withAnnotation(), dryRunTestDiffGVR)
	joined := strings.Join(diff.redacted, ",")
	if !strings.Contains(joined, `metadata.annotations["example.com/api-token"]`) {
		t.Fatalf("정제 경로가 escape 정책을 쓰지 않았습니다: %v", diff.redacted)
	}
	// 원문 키를 그대로 붙인 옛 표기가 남아 있으면 안 됩니다.
	if strings.Contains(joined, "metadata.annotations[example.com/api-token]") {
		t.Errorf("raw 키가 그대로 조합되었습니다: %v", diff.redacted)
	}
}

// TestCompareFailsClosedOnUnrepresentablePath — 경로가 상한을 넘으면 잘라서
// 성공으로 내보내지 않습니다. 잘린 경로는 **다른 필드를 가리키는 것처럼** 보이고,
// 그 오해가 곧 잘못된 검토입니다.
func TestCompareFailsClosedOnUnrepresentablePath(t *testing.T) {
	hugeKey := strings.Repeat("k", dryRunTextBytes+50)
	before := dryRunTestObject(map[string]any{"data": map[string]any{hugeKey: "1"}})
	after := dryRunTestObject(map[string]any{"data": map[string]any{hugeKey: "2"}})
	if _, err := compareForReview(before, after, dryRunTestDiffGVR); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("긴 경로는 ErrTooLarge여야 하는데 %v입니다", err)
	}

	// 목록 name 키도 경로가 됩니다. 같은 규칙이 걸려야 합니다.
	hugeName := strings.Repeat("n", dryRunTextBytes+50)
	beforeList := dryRunTestObject(map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{"name": hugeName, "image": "a:1"}},
	}})
	afterList := dryRunTestObject(map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{"name": hugeName, "image": "a:2"}},
	}})
	if _, err := compareForReview(beforeList, afterList, dryRunTestDiffGVR); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("긴 목록 이름은 ErrTooLarge여야 하는데 %v입니다", err)
	}

	// 경계 바로 아래는 통과해야 합니다 — 상한이 실제로 상한인지 확인합니다.
	okKey := strings.Repeat("k", dryRunTextBytes-len("data.")-1)
	okBefore := dryRunTestObject(map[string]any{"data": map[string]any{okKey: "1"}})
	okAfter := dryRunTestObject(map[string]any{"data": map[string]any{okKey: "2"}})
	diff := dryRunTestCompare(t, okBefore, okAfter, dryRunTestDiffGVR)
	if diff.total != 1 || len(diff.changes[0].Path) > dryRunTextBytes {
		t.Fatalf("경계 아래 경로가 거부되었습니다: %+v", diff.changes)
	}
}

// TestCompareFailsClosedWhenNodeBudgetExhausts — 순회를 끝내지 못하면 changeCount가
// 전체 수가 아닙니다. 근사치를 성공으로 내보내지 않고 실패합니다.
func TestCompareFailsClosedWhenNodeBudgetExhausts(t *testing.T) {
	// 20만 노드짜리 객체를 실제로 만들지 않고 seam으로 예산만 낮춥니다.
	saved := maxDiffNodes
	maxDiffNodes = 16
	t.Cleanup(func() { maxDiffNodes = saved })

	wide := make(map[string]any, 64)
	for i := 0; i < 64; i++ {
		wide["k"+strconv.Itoa(1000000+i)] = "v"
	}
	before := dryRunTestObject(map[string]any{"data": wide})
	after := dryRunTestObject(map[string]any{"data": wide})
	if _, err := compareForReview(before, after, dryRunTestDiffGVR); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("노드 예산 소진은 ErrTooLarge여야 하는데 %v입니다", err)
	}
	// 예산을 되돌리면 같은 입력이 정상 처리됩니다 — 상한이 실제로 상한입니다.
	maxDiffNodes = saved
	if _, err := compareForReview(before, after, dryRunTestDiffGVR); err != nil {
		t.Fatalf("정상 예산에서 실패했습니다: %v", err)
	}
}

// TestScalarEqualNeverPanicsOnCompositeValues — interface 비교에 map·slice가
// 섞이면 `==`는 런타임 panic입니다. 그 경로가 없는지 확인합니다.
func TestScalarEqualNeverPanicsOnCompositeValues(t *testing.T) {
	values := []any{nil, "s", true, int64(1), 1.5, []any{1}, map[string]any{"a": 1}}
	for _, a := range values {
		for _, b := range values {
			_ = scalarEqual(a, b) // panic하면 테스트가 실패합니다.
		}
	}
	if !scalarEqual(int64(3), int64(3)) || scalarEqual(int64(3), 3.0) {
		t.Error("스칼라 비교가 타입을 무시했습니다")
	}
}
