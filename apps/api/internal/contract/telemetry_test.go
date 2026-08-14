package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CorrelationKey 우선순위·재생성 구분과 필드 경로 검증 동작 테스트입니다.
// TS(packages/contracts/test/telemetry.test.mjs)와 같은 기대값을 씁니다.

var podEntity = EntityRef{
	ClusterID:    "prod-seoul",
	Namespace:    "payments",
	WorkloadKind: "Deployment",
	WorkloadName: "payments-api",
	WorkloadUID:  "wl-uid-1",
	PodName:      "payments-api-7f9c6b-x2k4q",
	PodUID:       "pod-uid-1",
}

func TestCorrelationKeyPrefersUIDsAndIncludesCluster(t *testing.T) {
	if got := CorrelationKey(podEntity); got != "prod-seoul|pod-uid|pod-uid-1" {
		t.Errorf("pod uid 키=%q", got)
	}
	if got := CorrelationKey(EntityRef{ClusterID: "prod-seoul", WorkloadUID: "wl-uid-1"}); got != "prod-seoul|workload-uid|wl-uid-1" {
		t.Errorf("workload uid 키=%q", got)
	}
	nsKind := EntityRef{ClusterID: "prod-seoul", Namespace: "payments", WorkloadKind: "Deployment", WorkloadName: "payments-api"}
	if got := CorrelationKey(nsKind); got != "prod-seoul|workload|payments|Deployment|payments-api" {
		t.Errorf("ns+kind+name 키=%q", got)
	}
	if got := CorrelationKey(EntityRef{ClusterID: "prod-seoul", Namespace: "payments", PodName: "adhoc-pod"}); got != "prod-seoul|pod-name|payments|adhoc-pod" {
		t.Errorf("pod name 키=%q", got)
	}
	// 클러스터가 다르면 같은 UID라도 다른 키입니다.
	other := podEntity
	other.ClusterID = "stage-seoul"
	if CorrelationKey(podEntity) == CorrelationKey(other) {
		t.Error("클러스터 신원이 키에 반영되지 않았습니다")
	}
}

func TestSamePodRecordsCorrelateToSameKey(t *testing.T) {
	// 레코드마다 scope를 **따로** 만들어, 각 레코드의 scope에서 유도한 키가
	// 서로 같은지를 확인합니다. 공유 변수를 자기 자신과 비교하면 증명이 아닙니다.
	count := 3
	value := 412.0
	message := "실패"
	metric := MetricRecord{Type: "metric", Scope: TelemetryScope{Entity: podEntity}, Name: "container_cpu_usage", Unit: UnitMillicores, TimestampMs: 1765689600000, Value: &value}
	log := LogRecord{Type: "log", Scope: TelemetryScope{Entity: podEntity}, TimestampMs: 1765689600120, Level: LevelError, Message: &message}
	event := EventRecord{Type: "event", Scope: TelemetryScope{Entity: podEntity}, TimestampMs: 1765689601000, EventType: "Warning", Reason: "BackOff", Count: &count}

	for i, errs := range [][]TelemetryFieldError{metric.Validate(), log.Validate(), event.Validate()} {
		if len(errs) != 0 {
			t.Errorf("record #%d 검증 실패: %v", i, errs)
		}
	}
	keys := map[string]struct{}{
		CorrelationKey(metric.Scope.Entity): {},
		CorrelationKey(log.Scope.Entity):    {},
		CorrelationKey(event.Scope.Entity):  {},
	}
	if len(keys) != 1 {
		t.Errorf("같은 Pod 레코드의 키가 다릅니다: %v", keys)
	}
	for k := range keys {
		if k == "" {
			t.Error("상관키가 비어 있습니다")
		}
	}
}

func TestRecreatedPodWithSameNameGetsDifferentKey(t *testing.T) {
	recreated := podEntity
	recreated.PodUID = "pod-uid-2"
	if recreated.PodName != podEntity.PodName {
		t.Fatal("전제: 이름이 같아야 합니다")
	}
	if CorrelationKey(podEntity) == CorrelationKey(recreated) {
		t.Error("재생성된 Pod(UID 변경)가 같은 키로 상관되었습니다")
	}
}

func errPaths(errs []TelemetryFieldError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Path
	}
	return out
}

func hasPath(errs []TelemetryFieldError, path string) bool {
	for _, e := range errs {
		if e.Path == path {
			return true
		}
	}
	return false
}

func TestValidationReportsFieldPaths(t *testing.T) {
	// Pod 신원 정합성: UID만 있으면 podName 경로로, namespace 부재는 1건으로 보고됩니다.
	zero := 0.0
	m := MetricRecord{Type: "metric", Scope: TelemetryScope{Entity: EntityRef{ClusterID: "c1", PodUID: "u1"}}, Name: "m", Unit: UnitCores, TimestampMs: 1, Value: &zero}
	if got := errPaths(m.Validate()); len(got) != 2 || got[0] != "scope.entity.podName" || got[1] != "scope.entity.namespace" {
		t.Errorf("paths=%v, want [scope.entity.podName scope.entity.namespace]", got)
	}
	// container는 Pod 신원, service namespace/version은 serviceName이 필요합니다.
	e := EntityRef{ClusterID: "c1", ContainerName: "app", ServiceVersion: "1.0.0"}
	errs := ValidateEntityRef(e, "entity")
	if !hasPath(errs, "entity.containerName") || !hasPath(errs, "entity.serviceVersion") {
		t.Errorf("정합성 오류 경로 누락: %v", errs)
	}
	// 예약 라벨 키와 timestampMs 규칙.
	a := AlertRecord{Type: "alert", Scope: TelemetryScope{Entity: EntityRef{ClusterID: "c1"}, Labels: map[string]string{"message": "x"}}, TimestampMs: 0, Name: "a", Severity: "critical", Status: "firing"}
	errs = a.Validate()
	if !hasPath(errs, "scope.labels.message") || !hasPath(errs, "timestampMs") {
		t.Errorf("paths=%v", errPaths(errs))
	}
}

func TestLabelValidationIsBounded(t *testing.T) {
	labels := make(map[string]string, 40)
	for i := 0; i < 40; i++ {
		labels[strings.Repeat("k", i+1)] = "v"
	}
	// 상한 초과는 항목별 검사 없이 상한 오류 하나로 유계 처리됩니다 (O(n)).
	errs := validateLabels(labels, "scope.labels")
	if len(errs) != 1 || errs[0].Path != "scope.labels" {
		t.Errorf("errs=%v, want 상한 오류 1건", errs)
	}
	// 한도 내에서는 키 길이·값 길이 위반이 항목 경로로 보고됩니다.
	errs = validateLabels(map[string]string{
		strings.Repeat("k", 65): "v",
		"zone":                  strings.Repeat("v", 257),
	}, "scope.labels")
	if len(errs) != 2 {
		t.Errorf("errs=%v, want 2건", errs)
	}
}

func TestEntityHierarchyInvariants(t *testing.T) {
	// README §5 계층(Cluster → Namespace → Workload → Pod → Container)과
	// fallback 신원(ns+kind+name 삼중쌍) 불변식입니다.
	// TS(test/telemetry.test.mjs)와 같은 기대 경로를 씁니다.
	cases := []struct {
		name   string
		entity EntityRef
		want   []string
	}{
		{"kind-only는 workloadName 경로로 거부", EntityRef{ClusterID: "c1", Namespace: "payments", WorkloadKind: "Deployment"}, []string{"entity.workloadName"}},
		{"name-only는 workloadKind 경로로 거부", EntityRef{ClusterID: "c1", Namespace: "payments", WorkloadName: "payments-api"}, []string{"entity.workloadKind"}},
		{"namespace 없는 workload는 거부", EntityRef{ClusterID: "c1", WorkloadKind: "Deployment", WorkloadName: "payments-api"}, []string{"entity.namespace"}},
		{"namespace 없는 workloadUid는 거부", EntityRef{ClusterID: "c1", WorkloadUID: "wl-1"}, []string{"entity.namespace"}},
		{"namespace 없는 pod는 거부", EntityRef{ClusterID: "c1", PodName: "p-1", PodUID: "u-1"}, []string{"entity.namespace"}},
		{"namespace 없는 container는 거부", EntityRef{ClusterID: "c1", PodName: "p-1", PodUID: "u-1", ContainerName: "app"}, []string{"entity.namespace"}},
		{"cluster-only는 허용", EntityRef{ClusterID: "c1"}, nil},
		{"namespace-only는 허용", EntityRef{ClusterID: "c1", Namespace: "payments"}, nil},
		{"workloadUid+namespace는 허용 (UID-only여도 namespace는 필요)", EntityRef{ClusterID: "c1", Namespace: "payments", WorkloadUID: "wl-1"}, nil},
	}
	for _, c := range cases {
		got := errPaths(ValidateEntityRef(c.entity, "entity"))
		if len(got) != len(c.want) {
			t.Errorf("%s: paths=%v, want %v", c.name, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("%s: paths=%v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}

func TestSourceAttributeAliasesAreRejectedAsLabels(t *testing.T) {
	// EntityRef로 흡수되는 원본 속성 별칭(README 매핑 표)은 정규화된 이름과
	// 마찬가지로 라벨로 밀반입할 수 없고, 항목 필드 경로가 붙습니다.
	errs := validateLabels(map[string]string{
		"service.name": "payments-api",
		"k8s.pod.uid":  "u1",
	}, "scope.labels")
	if !hasPath(errs, "scope.labels.service.name") || !hasPath(errs, "scope.labels.k8s.pod.uid") {
		t.Errorf("원본 별칭 라벨이 거부되지 않았습니다: %v", errs)
	}
	if len(errs) != 2 {
		t.Errorf("오류 수=%d, want 2 (%v)", len(errs), errs)
	}
}

func TestLabelLengthCountsUnicodeCodePoints(t *testing.T) {
	// JSON Schema maxLength는 유니코드 코드포인트 단위입니다. 이모지는 UTF-8 4바이트·
	// UTF-16 2유닛이므로, 바이트나 UTF-16 유닛으로 세면 스키마와 판정이 갈립니다.
	// TS(test/telemetry.test.mjs)와 같은 경계값을 씁니다.
	cases := []struct {
		name   string
		labels map[string]string
		want   int
	}{
		{"이모지 키 64코드포인트는 허용", map[string]string{strings.Repeat("😀", 64): "v"}, 0},
		{"이모지 키 65코드포인트는 거부", map[string]string{strings.Repeat("😀", 65): "v"}, 1},
		{"한글 값 256코드포인트는 허용", map[string]string{"zone": strings.Repeat("한", 256)}, 0},
		{"한글 값 257코드포인트는 거부", map[string]string{"zone": strings.Repeat("한", 257)}, 1},
	}
	for _, c := range cases {
		if errs := validateLabels(c.labels, "scope.labels"); len(errs) != c.want {
			t.Errorf("%s: errs=%v, want %d건", c.name, errs, c.want)
		}
	}
}

func TestJSONDecodeDistinguishesAbsenceFromZero(t *testing.T) {
	// 스키마 required는 "필드 존재"입니다. Go 제로값(0, "")이 부재를 삼키면
	// TS·스키마와 필수성 판정이 달라지므로 포인터로 구분합니다.
	scope := `"scope":{"entity":{"clusterId":"c1"}}`

	var missingValue MetricRecord
	mustUnmarshal(t, json.RawMessage(`{"type":"metric",`+scope+`,"name":"m","unit":"cores","timestampMs":1}`), &missingValue)
	if !hasPath(missingValue.Validate(), "value") {
		t.Error("value 부재가 거부되지 않았습니다")
	}
	var zeroValue MetricRecord
	mustUnmarshal(t, json.RawMessage(`{"type":"metric",`+scope+`,"name":"m","unit":"cores","timestampMs":1,"value":0}`), &zeroValue)
	if errs := zeroValue.Validate(); len(errs) != 0 {
		t.Errorf("명시적 value=0이 거부되었습니다: %v", errs)
	}

	var missingMessage LogRecord
	mustUnmarshal(t, json.RawMessage(`{"type":"log",`+scope+`,"timestampMs":1,"level":"INFO"}`), &missingMessage)
	if !hasPath(missingMessage.Validate(), "message") {
		t.Error("message 부재가 거부되지 않았습니다")
	}
	var emptyMessage LogRecord
	mustUnmarshal(t, json.RawMessage(`{"type":"log",`+scope+`,"timestampMs":1,"level":"INFO","message":""}`), &emptyMessage)
	if errs := emptyMessage.Validate(); len(errs) != 0 {
		t.Errorf("명시적 message=\"\"가 거부되었습니다: %v", errs)
	}
}

func TestCanonicalExamplePayloadIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash("../../../../packages/contracts/schema/telemetry.example.json"))
	if err != nil {
		t.Fatalf("예시 페이로드를 읽지 못했습니다: %v", err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("예시 레코드 수=%d, want 4", len(items))
	}
	keys := map[string]struct{}{}
	for i, item := range items {
		var head struct {
			Type  string         `json:"type"`
			Scope TelemetryScope `json:"scope"`
		}
		if err := json.Unmarshal(item, &head); err != nil {
			t.Fatal(err)
		}
		var errs []TelemetryFieldError
		switch head.Type {
		case "metric":
			var r MetricRecord
			mustUnmarshal(t, item, &r)
			errs = r.Validate()
		case "log":
			var r LogRecord
			mustUnmarshal(t, item, &r)
			errs = r.Validate()
		case "event":
			var r EventRecord
			mustUnmarshal(t, item, &r)
			errs = r.Validate()
		case "alert":
			var r AlertRecord
			mustUnmarshal(t, item, &r)
			errs = r.Validate()
		default:
			t.Fatalf("알 수 없는 type: %q", head.Type)
		}
		if len(errs) != 0 {
			t.Errorf("예시 #%d(%s) 검증 실패: %v", i, head.Type, errs)
		}
		keys[CorrelationKey(head.Scope.Entity)] = struct{}{}
	}
	// 예시의 metric/log/event는 같은 Pod → 같은 키, alert는 workload 수준 → 다른 키.
	if len(keys) != 2 {
		t.Errorf("예시 상관키 수=%d, want 2 (%v)", len(keys), keys)
	}
}

func mustUnmarshal(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("역직렬화 실패: %v", err)
	}
}

func TestInvalidRecordsAreRejected(t *testing.T) {
	one := 1.0
	valid := MetricRecord{Type: "metric", Scope: TelemetryScope{Entity: podEntity}, Name: "m", Unit: UnitCores, TimestampMs: 1, Value: &one}
	if errs := valid.Validate(); len(errs) != 0 {
		t.Fatalf("전제: 유효해야 합니다: %v", errs)
	}
	cases := map[string]MetricRecord{
		"name 누락":        func() MetricRecord { r := valid; r.Name = ""; return r }(),
		"unit 미허용":       func() MetricRecord { r := valid; r.Unit = "gigabytes"; return r }(),
		"timestampMs 누락": func() MetricRecord { r := valid; r.TimestampMs = 0; return r }(),
		"clusterId 누락":   func() MetricRecord { r := valid; r.Scope.Entity.ClusterID = ""; return r }(),
		"value 부재":       func() MetricRecord { r := valid; r.Value = nil; return r }(),
	}
	for name, r := range cases {
		if errs := r.Validate(); len(errs) == 0 {
			t.Errorf("%s: 잘못된 레코드가 통과했습니다", name)
		}
	}
}

// BenchmarkCorrelationKey는 대표 엔티티 5종에 대한 키 생성 비용입니다.
func BenchmarkCorrelationKey(b *testing.B) {
	entities := []EntityRef{
		podEntity,
		{ClusterID: "prod-seoul", WorkloadUID: "wl-uid-1"},
		{ClusterID: "prod-seoul", Namespace: "payments", WorkloadKind: "Deployment", WorkloadName: "payments-api"},
		{ClusterID: "prod-seoul", Namespace: "payments", PodName: "adhoc-pod"},
		{ClusterID: "prod-seoul"},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, e := range entities {
			_ = CorrelationKey(e)
		}
	}
}
