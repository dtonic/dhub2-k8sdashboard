package contract

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Unified Telemetry Model — 이슈 #4
//
// 기계가 읽는 정본은 packages/contracts/schema/telemetry.schema.json입니다.
// 이 파일의 타입과 스키마의 동등성(속성 이름·필수 여부)은 telemetry_parity_test.go가
// reflection으로 증명합니다. TypeScript 구현(packages/contracts/src/telemetry.*)과
// 검증 규칙·필드 경로·상관키 형식이 같아야 합니다.
//
// 시각 규칙: 정본 텔레메트리 계약의 시각은 epoch milliseconds(TimestampMs)입니다.
// 기존 화면 DTO의 RFC3339 문자열은 바꾸지 않습니다.

// MetricUnit은 Query Catalog(defaults/*.yaml)가 실제로 쓰는 모든 단위를 포함합니다.
type MetricUnit string

const (
	UnitPercent     MetricUnit = "percent"
	UnitBytes       MetricUnit = "bytes"
	UnitBytesPerSec MetricUnit = "bytes_per_sec"
	UnitCount       MetricUnit = "count"
	UnitCores       MetricUnit = "cores"
	UnitMillicores  MetricUnit = "millicores"
	UnitMebibytes   MetricUnit = "mebibytes"
)

// MetricUnits는 스키마 MetricUnit enum과 같은 순서의 전체 목록입니다.
var MetricUnits = []MetricUnit{
	UnitPercent, UnitBytes, UnitBytesPerSec, UnitCount,
	UnitCores, UnitMillicores, UnitMebibytes,
}

// 라벨 상한. 검증은 라벨 수 n에 대해 O(n)으로 유계입니다.
const (
	MaxTelemetryLabels        = 32
	MaxTelemetryLabelKeyLen   = 64
	MaxTelemetryLabelValueLen = 256
)

// ReservedLabelKeys는 신원·서비스·트레이스·메시지 필드입니다. 임의 라벨로 쓸 수 없습니다.
// 정규화된 DTO 이름과 함께, EntityRef로 흡수되는 원본 속성 별칭
// (cluster.id, k8s.*, service.*, trace_id/span_id)도 거부합니다.
var ReservedLabelKeys = []string{
	"clusterId", "namespace", "workloadKind", "workloadName", "workloadUid",
	"podName", "podUid", "containerName",
	"serviceName", "serviceNamespace", "serviceVersion",
	"traceId", "spanId", "message",
	"cluster.id",
	"k8s.namespace.name", "k8s.workload.kind", "k8s.workload.name", "k8s.workload.uid",
	"k8s.pod.name", "k8s.pod.uid", "k8s.container.name",
	"service.name", "service.namespace", "service.version",
	"trace_id", "span_id",
}

var reservedLabelSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(ReservedLabelKeys))
	for _, k := range ReservedLabelKeys {
		m[k] = struct{}{}
	}
	return m
}()

var workloadKindSet = map[string]struct{}{
	"Deployment": {}, "StatefulSet": {}, "DaemonSet": {}, "ReplicaSet": {}, "CronJob": {},
}

// TelemetryScope는 모든 텔레메트리 레코드가 공유하는 관측 범위입니다.
type TelemetryScope struct {
	Entity  EntityRef         `json:"entity"`
	Labels  map[string]string `json:"labels,omitempty"`
	TraceID string            `json:"traceId,omitempty"`
	SpanID  string            `json:"spanId,omitempty"`
}

// MetricRecord는 시계열 관측치 하나입니다.
// Value는 0이 유효한 관측치이므로, JSON에서 필드 **부재**와 명시적 0을 구분하기 위해
// 포인터입니다(스키마 required와 동등). nil이면 Validate가 거부합니다.
type MetricRecord struct {
	Type        string         `json:"type"`
	Scope       TelemetryScope `json:"scope"`
	Name        string         `json:"name"`
	Unit        MetricUnit     `json:"unit"`
	TimestampMs int64          `json:"timestampMs"`
	Value       *float64       `json:"value"`
}

// LogRecord는 로그 한 줄입니다. Message는 빈 문자열이 유효하므로, 필드 부재와
// 명시적 ""를 구분하기 위해 포인터입니다(스키마 required와 동등).
type LogRecord struct {
	Type        string         `json:"type"`
	Scope       TelemetryScope `json:"scope"`
	TimestampMs int64          `json:"timestampMs"`
	Level       LogLevel       `json:"level"`
	Message     *string        `json:"message"`
}

// EventRecord는 Kubernetes Event 하나입니다.
type EventRecord struct {
	Type        string         `json:"type"`
	Scope       TelemetryScope `json:"scope"`
	TimestampMs int64          `json:"timestampMs"`
	EventType   string         `json:"eventType"`
	Reason      string         `json:"reason"`
	Message     string         `json:"message,omitempty"`
	Count       *int           `json:"count,omitempty"`
}

// AlertRecord는 정규화된 알림 상태 하나입니다.
type AlertRecord struct {
	Type        string         `json:"type"`
	Scope       TelemetryScope `json:"scope"`
	TimestampMs int64          `json:"timestampMs"`
	Name        string         `json:"name"`
	Severity    string         `json:"severity"`
	Status      string         `json:"status"`
}

// TelemetryFieldError는 필드 경로가 붙은 검증 오류 하나입니다.
type TelemetryFieldError struct {
	Path    string
	Message string
}

func (e TelemetryFieldError) Error() string { return e.Path + ": " + e.Message }

// CorrelationKey는 UID 우선순위(README §5)로 엔티티를 하나의 키로 접습니다.
// Pod UID → Workload UID → ns+kind+name → Pod Name. 클러스터 신원을 항상 포함하므로
// 같은 Pod의 metric/log/event는 같은 키가 되고, 이름이 같아도 UID가 다르면
// (재생성된 인스턴스) 다른 키가 됩니다. TS의 correlationKey와 형식이 같습니다.
func CorrelationKey(e EntityRef) string {
	if e.ClusterID == "" {
		return ""
	}
	switch {
	case e.PodUID != "":
		return e.ClusterID + "|pod-uid|" + e.PodUID
	case e.WorkloadUID != "":
		return e.ClusterID + "|workload-uid|" + e.WorkloadUID
	case e.Namespace != "" && e.WorkloadKind != "" && e.WorkloadName != "":
		return e.ClusterID + "|workload|" + e.Namespace + "|" + e.WorkloadKind + "|" + e.WorkloadName
	case e.PodName != "":
		return e.ClusterID + "|pod-name|" + e.Namespace + "|" + e.PodName
	}
	return e.ClusterID
}

// ValidateEntityRef는 EntityRef의 정합성 규칙을 필드 경로와 함께 검사합니다.
func ValidateEntityRef(e EntityRef, path string) []TelemetryFieldError {
	var errs []TelemetryFieldError
	add := func(field, msg string) {
		errs = append(errs, TelemetryFieldError{Path: path + "." + field, Message: msg})
	}
	if e.ClusterID == "" {
		add("clusterId", "clusterId는 필수입니다")
	}
	if e.WorkloadKind != "" {
		if _, ok := workloadKindSet[e.WorkloadKind]; !ok {
			add("workloadKind", "허용되지 않는 workloadKind입니다: "+e.WorkloadKind)
		}
	}
	// README §5의 fallback 신원은 Namespace + Kind + Name 삼중쌍입니다.
	// kind와 name은 함께 있어야 합니다 (UID-only 워크로드는 kind/name 없이 유효).
	if e.WorkloadKind != "" && e.WorkloadName == "" {
		add("workloadName", "workloadKind가 있으면 workloadName도 있어야 합니다")
	}
	if e.WorkloadName != "" && e.WorkloadKind == "" {
		add("workloadKind", "workloadName이 있으면 workloadKind도 있어야 합니다")
	}
	// Pod 수준 신원은 이름·UID가 함께 있어야 정합합니다. 이름만 있으면 재생성된
	// 인스턴스와 섞이고, UID만 있으면 화면에 보여줄 이름이 없습니다.
	if e.PodName != "" && e.PodUID == "" {
		add("podUid", "podName이 있으면 podUid도 있어야 합니다")
	}
	if e.PodUID != "" && e.PodName == "" {
		add("podName", "podUid가 있으면 podName도 있어야 합니다")
	}
	if e.ContainerName != "" && (e.PodName == "" || e.PodUID == "") {
		add("containerName", "containerName은 Pod 신원(podName+podUid)이 있어야 합니다")
	}
	// README §5 계층: Cluster → Namespace → Workload → Pod → Container.
	// Workload/Pod/Container 신원이 하나라도 있으면 namespace가 필요합니다 (오류는 1건만).
	if e.Namespace == "" &&
		(e.WorkloadKind != "" || e.WorkloadName != "" || e.WorkloadUID != "" ||
			e.PodName != "" || e.PodUID != "" || e.ContainerName != "") {
		add("namespace", "Workload/Pod/Container 신원에는 namespace가 있어야 합니다")
	}
	if e.ServiceNamespace != "" && e.ServiceName == "" {
		add("serviceNamespace", "serviceNamespace는 serviceName이 있어야 합니다")
	}
	if e.ServiceVersion != "" && e.ServiceName == "" {
		add("serviceVersion", "serviceVersion은 serviceName이 있어야 합니다")
	}
	return errs
}

// validateLabels는 라벨 수 n에 대해 O(n)으로 유계입니다. 상한 초과 시 개별 검사를 중단합니다.
func validateLabels(labels map[string]string, path string) []TelemetryFieldError {
	if labels == nil {
		return nil
	}
	if len(labels) > MaxTelemetryLabels {
		return []TelemetryFieldError{{
			Path:    path,
			Message: fmt.Sprintf("라벨은 최대 %d개입니다", MaxTelemetryLabels),
		}}
	}
	var errs []TelemetryFieldError
	for k, v := range labels {
		// 길이는 JSON Schema maxLength와 같은 단위인 유니코드 코드포인트로 셉니다.
		switch {
		case len(k) == 0 || utf8.RuneCountInString(k) > MaxTelemetryLabelKeyLen:
			errs = append(errs, TelemetryFieldError{
				Path:    path + "." + k,
				Message: fmt.Sprintf("라벨 키는 1~%d자입니다", MaxTelemetryLabelKeyLen),
			})
		case isReservedLabelKey(k):
			errs = append(errs, TelemetryFieldError{
				Path:    path + "." + k,
				Message: "신원·서비스·트레이스·메시지 필드는 라벨로 쓸 수 없습니다",
			})
		case utf8.RuneCountInString(v) > MaxTelemetryLabelValueLen:
			errs = append(errs, TelemetryFieldError{
				Path:    path + "." + k,
				Message: fmt.Sprintf("라벨 값은 최대 %d자입니다", MaxTelemetryLabelValueLen),
			})
		}
	}
	return errs
}

func isReservedLabelKey(k string) bool {
	_, ok := reservedLabelSet[k]
	return ok
}

func validateScope(s TelemetryScope, path string) []TelemetryFieldError {
	errs := ValidateEntityRef(s.Entity, path+".entity")
	errs = append(errs, validateLabels(s.Labels, path+".labels")...)
	return errs
}

func validateCommon(typ, wantType string, scope TelemetryScope, ts int64) []TelemetryFieldError {
	var errs []TelemetryFieldError
	if typ != wantType {
		errs = append(errs, TelemetryFieldError{Path: "type", Message: "type은 " + wantType + "이어야 합니다"})
	}
	errs = append(errs, validateScope(scope, "scope")...)
	if ts < 1 {
		errs = append(errs, TelemetryFieldError{Path: "timestampMs", Message: "timestampMs는 1 이상의 epoch milliseconds 정수여야 합니다"})
	}
	return errs
}

func oneOf(value string, allowed ...string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

// Validate는 MetricRecord의 정합성을 검사합니다. 비어 있으면 유효합니다.
func (r MetricRecord) Validate() []TelemetryFieldError {
	errs := validateCommon(r.Type, "metric", r.Scope, r.TimestampMs)
	if r.Name == "" {
		errs = append(errs, TelemetryFieldError{Path: "name", Message: "비어 있지 않은 문자열이어야 합니다"})
	}
	if !oneOf(string(r.Unit), unitStrings()...) {
		errs = append(errs, TelemetryFieldError{Path: "unit", Message: "unit은 " + strings.Join(unitStrings(), "|") + " 중 하나여야 합니다"})
	}
	if r.Value == nil {
		errs = append(errs, TelemetryFieldError{Path: "value", Message: "value는 필수입니다"})
	} else if math.IsNaN(*r.Value) || math.IsInf(*r.Value, 0) {
		errs = append(errs, TelemetryFieldError{Path: "value", Message: "value는 유한한 숫자여야 합니다"})
	}
	return errs
}

// Validate는 LogRecord의 정합성을 검사합니다.
func (r LogRecord) Validate() []TelemetryFieldError {
	errs := validateCommon(r.Type, "log", r.Scope, r.TimestampMs)
	if !oneOf(string(r.Level), "ERROR", "WARN", "INFO", "DEBUG") {
		errs = append(errs, TelemetryFieldError{Path: "level", Message: "level은 ERROR|WARN|INFO|DEBUG 중 하나여야 합니다"})
	}
	if r.Message == nil {
		errs = append(errs, TelemetryFieldError{Path: "message", Message: "message는 필수 문자열입니다"})
	}
	return errs
}

// Validate는 EventRecord의 정합성을 검사합니다.
func (r EventRecord) Validate() []TelemetryFieldError {
	errs := validateCommon(r.Type, "event", r.Scope, r.TimestampMs)
	if !oneOf(r.EventType, "Normal", "Warning") {
		errs = append(errs, TelemetryFieldError{Path: "eventType", Message: "eventType은 Normal|Warning 중 하나여야 합니다"})
	}
	if r.Reason == "" {
		errs = append(errs, TelemetryFieldError{Path: "reason", Message: "비어 있지 않은 문자열이어야 합니다"})
	}
	if r.Count != nil && *r.Count < 1 {
		errs = append(errs, TelemetryFieldError{Path: "count", Message: "count는 1 이상의 정수여야 합니다"})
	}
	return errs
}

// Validate는 AlertRecord의 정합성을 검사합니다.
func (r AlertRecord) Validate() []TelemetryFieldError {
	errs := validateCommon(r.Type, "alert", r.Scope, r.TimestampMs)
	if r.Name == "" {
		errs = append(errs, TelemetryFieldError{Path: "name", Message: "비어 있지 않은 문자열이어야 합니다"})
	}
	if !oneOf(r.Severity, "critical", "warning", "info") {
		errs = append(errs, TelemetryFieldError{Path: "severity", Message: "severity는 critical|warning|info 중 하나여야 합니다"})
	}
	if !oneOf(r.Status, "firing", "resolved", "pending") {
		errs = append(errs, TelemetryFieldError{Path: "status", Message: "status는 firing|resolved|pending 중 하나여야 합니다"})
	}
	return errs
}

func unitStrings() []string {
	out := make([]string, len(MetricUnits))
	for i, u := range MetricUnits {
		out[i] = string(u)
	}
	return out
}
