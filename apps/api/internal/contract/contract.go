// Package contract는 packages/contracts(TypeScript)와 **같은 JSON**을 만드는 Go 타입입니다.
//
// 필드 이름과 optional 여부가 프런트 계약과 1:1로 대응해야 합니다.
// 계약이 바뀌면 두 곳을 같이 고칩니다. (README §4.5)
package contract

import "time"

/* ── 공통 ────────────────────────────────────────────────────────────────── */

// RangeKey는 시간 범위 프리셋입니다. Custom의 최대 폭 30일은 서버가 강제합니다.
type RangeKey string

const (
	Range1h     RangeKey = "1h"
	Range1d     RangeKey = "1d"
	Range7d     RangeKey = "7d"
	Range30d    RangeKey = "30d"
	RangeCustom RangeKey = "custom"
)

// Severity는 정규화된 심각도입니다. design-system의 status 토큰에 대응합니다.
type Severity string

const (
	SeverityHealthy     Severity = "healthy"
	SeverityProgressing Severity = "progressing"
	SeverityWarning     Severity = "warning"
	SeverityDegraded    Severity = "degraded"
	SeverityCritical    Severity = "critical"
	SeverityUnknown     Severity = "unknown"
)

// rank는 심각도 비교용 순서입니다. 큰 쪽이 더 나쁩니다.
var rank = map[Severity]int{
	SeverityHealthy:     0,
	SeverityUnknown:     1,
	SeverityProgressing: 2,
	SeverityWarning:     3,
	SeverityDegraded:    4,
	SeverityCritical:    5,
}

// WorseOf는 둘 중 더 나쁜 심각도를 돌려줍니다. 집계 화면에서 롤업에 씁니다.
func WorseOf(a, b Severity) Severity {
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// SectionStatus는 패널 하나의 상태입니다.
//
// "결과 0건"과 "권한 없음"과 "데이터소스 장애"는 화면에서 서로 다르게 보여야 하므로
// 에러가 아니라 값으로 표현합니다. (ADR 0002)
type SectionStatus string

const (
	StatusOK        SectionStatus = "ok"
	StatusEmpty     SectionStatus = "empty"
	StatusForbidden SectionStatus = "forbidden"
	StatusDegraded  SectionStatus = "degraded"
)

// Source는 문제를 일으킨 데이터소스입니다. 화면 문구에 그대로 노출됩니다.
type Source string

const (
	SourceGreptimeDB   Source = "greptimedb"
	SourceQuickwit     Source = "quickwit"
	SourceKubernetes   Source = "kubernetes"
	SourceAlertmanager Source = "alertmanager"
)

// Section은 패널 단위 봉투입니다. 한 데이터소스가 죽어도 나머지 섹션은 살아 있습니다.
type Section[T any] struct {
	Status SectionStatus `json:"status"`
	// degraded일 때도 마지막으로 성공한 값이 있으면 함께 내려갑니다.
	Data *T `json:"data,omitempty"`
	// 어떤 데이터소스가 문제인지.
	Source Source `json:"source,omitempty"`
	// 사람이 읽는 사유. 스택트레이스나 내부 식별자를 담지 않습니다. (README §10)
	Reason string `json:"reason,omitempty"`
	// 이 섹션 값이 만들어진 시각. stale 판정에 씁니다.
	ObservedAt string `json:"observedAt,omitempty"`
}

// OK는 값이 있는 섹션을 만듭니다. 슬라이스가 비어 있으면 자동으로 empty가 됩니다.
func OK[T any](v T) Section[T] {
	s := Section[T]{Status: StatusOK, Data: &v, ObservedAt: nowRFC3339()}
	if isEmpty(v) {
		s.Status = StatusEmpty
	}
	return s
}

// Empty는 조회는 성공했지만 결과가 0건인 섹션입니다.
func Empty[T any]() Section[T] {
	return Section[T]{Status: StatusEmpty, ObservedAt: nowRFC3339()}
}

// Forbidden은 Scope 밖이라 조회 자체가 거절된 섹션입니다. 데이터를 절대 담지 않습니다.
func Forbidden[T any](reason string) Section[T] {
	return Section[T]{Status: StatusForbidden, Reason: reason, ObservedAt: nowRFC3339()}
}

// Degraded는 데이터소스 장애입니다. stale 값이 있으면 함께 내려보냅니다.
func Degraded[T any](src Source, reason string, stale *T) Section[T] {
	return Section[T]{Status: StatusDegraded, Source: src, Reason: reason, Data: stale, ObservedAt: nowRFC3339()}
}

// nowRFC3339는 테스트에서 고정할 수 있도록 변수로 둡니다.
var nowRFC3339 = func() string { return time.Now().UTC().Format(time.RFC3339) }

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []UnhealthyEntity:
		return len(t) == 0
	case []ClusterEvent:
		return len(t) == 0
	case []NamespaceSummary:
		return len(t) == 0
	case []WorkloadSummary:
		return len(t) == 0
	case []PodSummary:
		return len(t) == 0
	case []ContainerStatus:
		return len(t) == 0
	case []OwnerRef:
		return len(t) == 0
	case []TrendPanel:
		return len(t) == 0
	case []TrendSeries:
		return len(t) == 0
	case []LogLine:
		return len(t) == 0
	case []AlertInstance:
		return len(t) == 0
	case []LogHistogramBucket:
		return len(t) == 0
	}
	return false
}

// TimeWindow는 응답에 실리는 적용된 시간 범위입니다.
type TimeWindow struct {
	Key         RangeKey `json:"key"`
	From        string   `json:"from"`
	To          string   `json:"to"`
	StepSeconds int      `json:"stepSeconds"`
}

// EntityRef는 Unified Entity Model 참조입니다.
// 식별 우선순위: Pod UID → Workload UID → ns+kind+name → Pod Name (README §5)
type EntityRef struct {
	ClusterID     string `json:"clusterId"`
	Namespace     string `json:"namespace,omitempty"`
	WorkloadKind  string `json:"workloadKind,omitempty"`
	WorkloadName  string `json:"workloadName,omitempty"`
	WorkloadUID   string `json:"workloadUid,omitempty"`
	PodName       string `json:"podName,omitempty"`
	PodUID        string `json:"podUid,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
	// OpenTelemetry service.* 대응. namespace/version은 serviceName이 있어야 의미가 있습니다.
	ServiceName      string `json:"serviceName,omitempty"`
	ServiceNamespace string `json:"serviceNamespace,omitempty"`
	ServiceVersion   string `json:"serviceVersion,omitempty"`
}

/* ── Overview ───────────────────────────────────────────────────────────── */

type NodeHealth struct {
	Total         int `json:"total"`
	Ready         int `json:"ready"`
	NotReady      int `json:"notReady"`
	Pressure      int `json:"pressure"`
	Unschedulable int `json:"unschedulable"`
}

type PodHealth struct {
	Total            int `json:"total"`
	Running          int `json:"running"`
	Pending          int `json:"pending"`
	Failed           int `json:"failed"`
	CrashLoopBackOff int `json:"crashLoopBackOff"`
	ImagePullBackOff int `json:"imagePullBackOff"`
	Restarts         int `json:"restarts"`
}

type WorkloadHealth struct {
	Total           int `json:"total"`
	Available       int `json:"available"`
	ReplicaMismatch int `json:"replicaMismatch"`
	RolloutStalled  int `json:"rolloutStalled"`
}

type TrendPoint struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

type TrendSeries struct {
	Key    string       `json:"key"`
	Label  string       `json:"label"`
	Unit   string       `json:"unit"`
	Points []TrendPoint `json:"points"`
}

type TrendPanel struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	StepSeconds int           `json:"stepSeconds"`
	Series      []TrendSeries `json:"series"`
}

type UnhealthyEntity struct {
	Ref        EntityRef `json:"ref"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Namespace  string    `json:"namespace"`
	Severity   Severity  `json:"severity"`
	Reason     string    `json:"reason"`
	Restarts   int       `json:"restarts"`
	ForSeconds int       `json:"forSeconds"`
}

type ClusterEvent struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Reason       string    `json:"reason"`
	Message      string    `json:"message"`
	Involved     EntityRef `json:"involved"`
	InvolvedName string    `json:"involvedName"`
	Namespace    string    `json:"namespace"`
	Count        int       `json:"count"`
	LastSeen     string    `json:"lastSeen"`
}

type AlertSeverityCounts struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
}

type AlertTop struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Severity    string `json:"severity"`
	Namespace   string `json:"namespace"`
	ActiveSince string `json:"activeSince"`
}

type AlertSummary struct {
	BySeverity AlertSeverityCounts `json:"bySeverity"`
	Top        []AlertTop          `json:"top"`
}

type TopologyEdgeSummary struct {
	From              string   `json:"from"`
	To                string   `json:"to"`
	Protocol          string   `json:"protocol"`
	RequestsPerSecond float64  `json:"requestsPerSecond"`
	ErrorRate         float64  `json:"errorRate"`
	Severity          Severity `json:"severity"`
}

type TopologySummary struct {
	Pods         int                   `json:"pods"`
	Edges        int                   `json:"edges"`
	ProblemEdges []TopologyEdgeSummary `json:"problemEdges"`
}

type AppliedScope struct {
	ClusterID string `json:"clusterId"`
	// 전체 접근이면 "all" 문자열, 아니면 namespace 배열입니다. TS의 `string[] | "all"`와 같습니다.
	Namespaces any `json:"namespaces"`
}

type ClusterOverviewResponse struct {
	ClusterID    string       `json:"clusterId"`
	ClusterName  string       `json:"clusterName"`
	AppliedScope AppliedScope `json:"appliedScope"`
	Range        TimeWindow   `json:"range"`
	GeneratedAt  string       `json:"generatedAt"`

	Nodes     Section[NodeHealth]        `json:"nodes"`
	Pods      Section[PodHealth]         `json:"pods"`
	Workloads Section[WorkloadHealth]    `json:"workloads"`
	Trends    Section[[]TrendPanel]      `json:"trends"`
	Unhealthy Section[[]UnhealthyEntity] `json:"unhealthy"`
	Events    Section[[]ClusterEvent]    `json:"events"`
	Alerts    Section[AlertSummary]      `json:"alerts"`
	Topology  Section[TopologySummary]   `json:"topology"`
}

type ScopeCluster struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// 접근 가능한 namespace 목록. 전체면 "all".
	Namespaces any  `json:"namespaces"`
	Accessible bool `json:"accessible"`
}

type ScopeResponse struct {
	Clusters []ScopeCluster `json:"clusters"`
}

// APIError는 화면 전체가 실패했을 때만 씁니다. 섹션 단위 실패는 Section으로 표현합니다.
//
// RequestID는 응답 헤더 X-Request-ID와 항상 같은 값입니다. 문의·로그 대조는
// 이 값 하나로 합니다. (#5)
type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}

// VersionInfo는 GET /version 응답입니다. 값은 빌드 시 ldflags로 주입되며
// 로컬 빌드 기본값은 dev/unknown입니다. (#5)
type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
}

/* ── Drill-down ─────────────────────────────────────────────────────────── */

type ResourceUsage struct {
	CPUMilli         int      `json:"cpuMilli"`
	CPURequestMilli  int      `json:"cpuRequestMilli"`
	CPULimitMilli    *int     `json:"cpuLimitMilli"`
	MemoryMib        int      `json:"memoryMib"`
	MemoryRequestMib int      `json:"memoryRequestMib"`
	MemoryLimitMib   *int     `json:"memoryLimitMib"`
	CPUVsRequest     float64  `json:"cpuVsRequest"`
	CPUVsLimit       *float64 `json:"cpuVsLimit"`
	MemoryVsRequest  float64  `json:"memoryVsRequest"`
	MemoryVsLimit    *float64 `json:"memoryVsLimit"`
}

// Normalize는 비율 필드를 request/limit에서 다시 계산합니다.
// 비율 계산을 UI에 맡기면 화면마다 결과가 달라집니다. 서버가 한 번만 계산합니다.
func (u *ResourceUsage) Normalize() {
	u.CPUVsRequest = ratio(u.CPUMilli, u.CPURequestMilli)
	u.MemoryVsRequest = ratio(u.MemoryMib, u.MemoryRequestMib)
	u.CPUVsLimit = ratioPtr(u.CPUMilli, u.CPULimitMilli)
	u.MemoryVsLimit = ratioPtr(u.MemoryMib, u.MemoryLimitMib)
}

func ratio(v, base int) float64 {
	if base <= 0 {
		return 0
	}
	return float64(v) / float64(base)
}

func ratioPtr(v int, base *int) *float64 {
	if base == nil || *base <= 0 {
		return nil
	}
	r := float64(v) / float64(*base)
	return &r
}

// Add는 Pod 단위 사용량을 Namespace/Workload 합계로 누적합니다.
func (u *ResourceUsage) Add(o ResourceUsage) {
	u.CPUMilli += o.CPUMilli
	u.CPURequestMilli += o.CPURequestMilli
	u.MemoryMib += o.MemoryMib
	u.MemoryRequestMib += o.MemoryRequestMib
	u.CPULimitMilli = addPtr(u.CPULimitMilli, o.CPULimitMilli)
	u.MemoryLimitMib = addPtr(u.MemoryLimitMib, o.MemoryLimitMib)
}

// addPtr는 limit 합계입니다. 한쪽이라도 limit이 없으면 합계도 없습니다 —
// "limit 없음"을 0으로 접으면 화면에서 과사용으로 잘못 보입니다.
func addPtr(a, b *int) *int {
	if a == nil || b == nil {
		return nil
	}
	s := *a + *b
	return &s
}

type IssueReason string

const (
	IssueCrashLoopBackOff IssueReason = "CrashLoopBackOff"
	IssueImagePullBackOff IssueReason = "ImagePullBackOff"
	IssuePending          IssueReason = "Pending"
	IssueReplicaMismatch  IssueReason = "ReplicaMismatch"
	IssueRolloutStalled   IssueReason = "RolloutStalled"
	IssueRestarting       IssueReason = "Restarting"
	IssueOOMKilled        IssueReason = "OOMKilled"
	IssueProbeFailed      IssueReason = "ProbeFailed"
)

type NamespacePodCounts struct {
	Total    int `json:"total"`
	Running  int `json:"running"`
	Pending  int `json:"pending"`
	Failed   int `json:"failed"`
	Restarts int `json:"restarts"`
}

type NamespaceWorkloadCounts struct {
	Total     int `json:"total"`
	Unhealthy int `json:"unhealthy"`
}

type NamespaceSummary struct {
	Name      string                  `json:"name"`
	Severity  Severity                `json:"severity"`
	Workloads NamespaceWorkloadCounts `json:"workloads"`
	Pods      NamespacePodCounts      `json:"pods"`
	Usage     ResourceUsage           `json:"usage"`
	Issues    []IssueReason           `json:"issues"`
}

type NamespaceListResponse struct {
	ClusterID   string                      `json:"clusterId"`
	Range       TimeWindow                  `json:"range"`
	GeneratedAt string                      `json:"generatedAt"`
	Namespaces  Section[[]NamespaceSummary] `json:"namespaces"`
}

type ReplicaCounts struct {
	Desired   int `json:"desired"`
	Ready     int `json:"ready"`
	Available int `json:"available"`
	Updated   int `json:"updated"`
}

type RolloutStatus struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type WorkloadSummary struct {
	Ref        EntityRef     `json:"ref"`
	Name       string        `json:"name"`
	Kind       string        `json:"kind"`
	Namespace  string        `json:"namespace"`
	Severity   Severity      `json:"severity"`
	Replicas   ReplicaCounts `json:"replicas"`
	Rollout    RolloutStatus `json:"rollout"`
	Restarts   int           `json:"restarts"`
	Usage      ResourceUsage `json:"usage"`
	Images     []string      `json:"images"`
	Issues     []IssueReason `json:"issues"`
	AgeSeconds int           `json:"ageSeconds"`
}

type NamespaceDetailResponse struct {
	ClusterID   string                     `json:"clusterId"`
	Namespace   string                     `json:"namespace"`
	Range       TimeWindow                 `json:"range"`
	GeneratedAt string                     `json:"generatedAt"`
	Summary     Section[NamespaceSummary]  `json:"summary"`
	Workloads   Section[[]WorkloadSummary] `json:"workloads"`
	Trends      Section[[]TrendPanel]      `json:"trends"`
	Events      Section[[]ClusterEvent]    `json:"events"`
}

type OwnerRef struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	UID      string `json:"uid"`
	Current  bool   `json:"current,omitempty"`
	Pods     int    `json:"pods,omitempty"`
	Revision string `json:"revision,omitempty"`
}

type ContainerTermination struct {
	Reason     string `json:"reason"`
	ExitCode   int    `json:"exitCode"`
	FinishedAt string `json:"finishedAt"`
}

type ContainerUsage struct {
	CPUMilli  int `json:"cpuMilli"`
	MemoryMib int `json:"memoryMib"`
}

type ProbeState struct {
	Liveness  string `json:"liveness"`
	Readiness string `json:"readiness"`
}

type ContainerStatus struct {
	Name           string                `json:"name"`
	Image          string                `json:"image"`
	ImageID        string                `json:"imageId,omitempty"`
	Ready          bool                  `json:"ready"`
	Started        bool                  `json:"started"`
	Restarts       int                   `json:"restarts"`
	State          string                `json:"state"`
	Reason         string                `json:"reason,omitempty"`
	Message        string                `json:"message,omitempty"`
	LastTerminated *ContainerTermination `json:"lastTerminated,omitempty"`
	Usage          *ContainerUsage       `json:"usage,omitempty"`
	Probes         ProbeState            `json:"probes"`
}

type PodSummary struct {
	Ref        EntityRef     `json:"ref"`
	Name       string        `json:"name"`
	UID        string        `json:"uid"`
	Namespace  string        `json:"namespace"`
	Phase      string        `json:"phase"`
	Severity   Severity      `json:"severity"`
	Ready      string        `json:"ready"`
	Restarts   int           `json:"restarts"`
	Node       string        `json:"node"`
	Owner      *OwnerRef     `json:"owner,omitempty"`
	Issues     []IssueReason `json:"issues"`
	Usage      ResourceUsage `json:"usage"`
	StartedAt  string        `json:"startedAt"`
	FinishedAt string        `json:"finishedAt,omitempty"`
}

type WorkloadDetailResponse struct {
	ClusterID   string                   `json:"clusterId"`
	Namespace   string                   `json:"namespace"`
	Range       TimeWindow               `json:"range"`
	GeneratedAt string                   `json:"generatedAt"`
	Workload    Section[WorkloadSummary] `json:"workload"`
	OwnerChain  Section[[]OwnerRef]      `json:"ownerChain"`
	Pods        Section[[]PodSummary]    `json:"pods"`
	Trends      Section[[]TrendPanel]    `json:"trends"`
	Events      Section[[]ClusterEvent]  `json:"events"`
}

type PodDetailResponse struct {
	ClusterID   string                     `json:"clusterId"`
	Namespace   string                     `json:"namespace"`
	Range       TimeWindow                 `json:"range"`
	GeneratedAt string                     `json:"generatedAt"`
	Pod         Section[PodSummary]        `json:"pod"`
	OwnerChain  Section[[]OwnerRef]        `json:"ownerChain"`
	Containers  Section[[]ContainerStatus] `json:"containers"`
	Trends      Section[[]TrendPanel]      `json:"trends"`
	Events      Section[[]ClusterEvent]    `json:"events"`
}

/* ── Logs ───────────────────────────────────────────────────────────────── */

type LogLevel string

const (
	LevelError LogLevel = "ERROR"
	LevelWarn  LogLevel = "WARN"
	LevelInfo  LogLevel = "INFO"
	LevelDebug LogLevel = "DEBUG"
)

// LevelOrder는 UI의 LEVEL_ORDER와 같은 순서입니다.
var LevelOrder = []LogLevel{LevelError, LevelWarn, LevelInfo, LevelDebug}

// MaskedSpan은 가려진 구간입니다. 위치와 종류만 내려가고 원문은 서버 밖으로 나가지 않습니다.
type MaskedSpan struct {
	Start  int    `json:"start"`
	Length int    `json:"length"`
	Kind   string `json:"kind"`
}

type LogLine struct {
	ID            string            `json:"id"`
	T             int64             `json:"t"`
	Level         LogLevel          `json:"level"`
	Message       string            `json:"message"`
	Masked        []MaskedSpan      `json:"masked"`
	Namespace     string            `json:"namespace"`
	PodName       string            `json:"podName"`
	PodUID        string            `json:"podUid"`
	ContainerName string            `json:"containerName"`
	WorkloadKind  string            `json:"workloadKind,omitempty"`
	WorkloadName  string            `json:"workloadName,omitempty"`
	NodeName      string            `json:"nodeName,omitempty"`
	TraceID       string            `json:"traceId,omitempty"`
	SpanID        string            `json:"spanId,omitempty"`
	Attributes    map[string]string `json:"attributes,omitempty"`
}

type LogCursor struct {
	Next     *string `json:"next"`
	PageSize int     `json:"pageSize"`
}

type LogHistogramBucket struct {
	T      int64            `json:"t"`
	Counts map[LogLevel]int `json:"counts"`
}

type LogFacetWorkload struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type LogFacetPod struct {
	Name  string `json:"name"`
	UID   string `json:"uid"`
	Count int    `json:"count"`
}

type LogFacetContainer struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type LogFacets struct {
	Workloads  []LogFacetWorkload  `json:"workloads"`
	Pods       []LogFacetPod       `json:"pods"`
	Containers []LogFacetContainer `json:"containers"`
}

type LogApplied struct {
	ClusterID string  `json:"clusterId"`
	Namespace *string `json:"namespace"`
	From      string  `json:"from"`
	To        string  `json:"to"`
	Truncated bool    `json:"truncated"`
	MaxLines  int     `json:"maxLines"`
}

type LogSearchResponse struct {
	Lines       Section[[]LogLine]            `json:"lines"`
	Cursor      LogCursor                     `json:"cursor"`
	Histogram   Section[[]LogHistogramBucket] `json:"histogram"`
	Events      Section[[]ClusterEvent]       `json:"events"`
	Facets      Section[LogFacets]            `json:"facets"`
	Applied     LogApplied                    `json:"applied"`
	GeneratedAt string                        `json:"generatedAt"`
}

/* ── Topology ───────────────────────────────────────────────────────────── */

type TopologyNode struct {
	ID        string    `json:"id"`
	Ref       EntityRef `json:"ref"`
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Severity  Severity  `json:"severity"`
	Column    int       `json:"column"`
	Row       int       `json:"row"`
	// External은 클러스터 밖 엔티티(Ingress Gateway·External Client 등)입니다.
	// Pod 신원이 없으므로 상세 화면으로 연결하지 않습니다. (#29)
	External bool `json:"external,omitempty"`
}

type TopologyRoute struct {
	Protocol   string `json:"protocol"`
	Route      string `json:"route"`
	Count      int    `json:"count"`
	ErrorCount int    `json:"errorCount"`
}

type TopologyEdge struct {
	ID         string          `json:"id"`
	From       string          `json:"from"`
	To         string          `json:"to"`
	Severity   Severity        `json:"severity"`
	TotalCount int             `json:"totalCount"`
	ErrorRate  float64         `json:"errorRate"`
	Protocols  []string        `json:"protocols"`
	Routes     []TopologyRoute `json:"routes"`
}

type TopologyPods struct {
	Total         int               `json:"total"`
	Healthy       int               `json:"healthy"`
	Unhealthy     int               `json:"unhealthy"`
	UnhealthyList []UnhealthyEntity `json:"unhealthyList"`
}

type TopologyGraph struct {
	Nodes []TopologyNode `json:"nodes"`
	Edges []TopologyEdge `json:"edges"`
}

// TopologyNodePosition은 관리자가 저장한 노드 좌표입니다. (#28)
type TopologyNodePosition struct {
	ID string  `json:"id"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
}

// TopologyLayout은 모든 사용자가 공유하는 저장 배치입니다.
type TopologyLayout struct {
	Positions []TopologyNodePosition `json:"positions"`
	UpdatedAt string                 `json:"updatedAt"`
}

type TopologyResponse struct {
	ClusterID   string                 `json:"clusterId"`
	Namespace   *string                `json:"namespace"`
	Range       TimeWindow             `json:"range"`
	GeneratedAt string                 `json:"generatedAt"`
	Pods        Section[TopologyPods]  `json:"pods"`
	Graph       Section[TopologyGraph] `json:"graph"`
	// Layout은 관리자가 저장한 공유 배치입니다. null이면 기본 배치를 씁니다. (#28)
	Layout *TopologyLayout `json:"layout"`
	// CanEditLayout은 이 요청 사용자가 배치를 저장할 수 있는지입니다.
	CanEditLayout bool `json:"canEditLayout"`
}

type TopologyEdgeSeriesResponse struct {
	EdgeID      string                 `json:"edgeId"`
	Range       TimeWindow             `json:"range"`
	GeneratedAt string                 `json:"generatedAt"`
	Series      Section[[]TrendSeries] `json:"series"`
}

/* ── Alerts ─────────────────────────────────────────────────────────────── */

type AlertInstance struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Severity    string            `json:"severity"`
	Status      string            `json:"status"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt,omitempty"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Entity      *EntityRef        `json:"entity,omitempty"`
	EntityName  string            `json:"entityName,omitempty"`
	SourceURL   string            `json:"sourceUrl,omitempty"`
	Source      string            `json:"source"`
	GroupSize   int               `json:"groupSize"`
	GroupKey    string            `json:"groupKey"`
}

type AlertCount struct {
	Firing   int `json:"firing"`
	Resolved int `json:"resolved"`
}

type AlertListResponse struct {
	ClusterID    string                         `json:"clusterId"`
	Range        TimeWindow                     `json:"range"`
	GeneratedAt  string                         `json:"generatedAt"`
	Firing       Section[[]AlertInstance]       `json:"firing"`
	Resolved     Section[[]AlertInstance]       `json:"resolved"`
	Counts       Section[map[string]AlertCount] `json:"counts"`
	GroupingRule string                         `json:"groupingRule"`
}
