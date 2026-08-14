package contract

// SSE 상태 변경 스트림 계약 — 이슈 #12, ADR 0005.
//
// 기계가 읽는 정본은 packages/contracts/schema/stream.schema.json입니다.
// Go 타입과 스키마의 동등성은 stream_parity_test.go가 reflection으로 증명하고,
// TypeScript(packages/contracts/src/index.ts)와 스키마의 동등성은
// packages/contracts/test/stream-parity.test.mjs가 소스 대조로 증명합니다.
//
// Envelope는 **무효화 신호**입니다. Kubernetes 원본 객체·Alert annotation·
// 시계열 샘플을 싣지 않습니다 — 화면은 이 신호를 받고 기존 HTTP 계약으로
// 다시 조회합니다. (README §10, ADR 0005)

// StreamEventKind는 무효화 대상의 종류입니다. reset은 제어 신호입니다 —
// 서버가 재생을 보장할 수 없으니 화면 전체를 HTTP로 다시 조회하라는 뜻입니다.
type StreamEventKind string

const (
	StreamKindPod       StreamEventKind = "pod"
	StreamKindWorkload  StreamEventKind = "workload"
	StreamKindKubeEvent StreamEventKind = "kubeevent"
	StreamKindAlert     StreamEventKind = "alert"
	StreamKindReset     StreamEventKind = "reset"
)

// StreamEventKinds는 스키마 StreamEventKind enum과 같은 순서의 전체 목록입니다.
var StreamEventKinds = []StreamEventKind{
	StreamKindPod, StreamKindWorkload, StreamKindKubeEvent, StreamKindAlert, StreamKindReset,
}

// StreamEventAction은 변경의 방향입니다.
type StreamEventAction string

const (
	StreamActionAdded   StreamEventAction = "added"
	StreamActionUpdated StreamEventAction = "updated"
	StreamActionDeleted StreamEventAction = "deleted"
	StreamActionReset   StreamEventAction = "reset"
)

// StreamEventActions는 스키마 StreamEventAction enum과 같은 순서의 전체 목록입니다.
var StreamEventActions = []StreamEventAction{
	StreamActionAdded, StreamActionUpdated, StreamActionDeleted, StreamActionReset,
}

// MaxStreamEventIDLen은 EventEnvelope.ID와 인바운드 Last-Event-ID의 길이 상한입니다.
// 상한을 넘는 입력은 구독 자원을 배정하기 전에 거절합니다.
const MaxStreamEventIDLen = 64

// EventEnvelope는 SSE로 내려가는 무효화 이벤트 하나입니다.
//
// ID는 프로세스 인스턴스 식별자를 포함한 불투명 단조 증가 문자열입니다.
// 브라우저는 이 값을 Last-Event-ID로 되돌려 보내고, 서버는 같은 인스턴스가
// 보존 중인 구간만 재생합니다. 인스턴스가 바뀌었거나 구간을 벗어나면
// kind=reset을 보내 완전 재생을 조용히 주장하지 않습니다.
type EventEnvelope struct {
	ID         string            `json:"id"`
	Kind       StreamEventKind   `json:"kind"`
	Action     StreamEventAction `json:"action"`
	ClusterID  string            `json:"clusterId"`
	ObservedAt string            `json:"observedAt"`
	// Namespace가 비어 있으면 클러스터 범위 신호입니다. 전체(All) Scope에게만 전달됩니다.
	Namespace string `json:"namespace,omitempty"`
	// Entity는 UID 우선 식별자입니다. 원본 객체가 아니라 참조만 싣습니다.
	Entity          *EntityRef `json:"entity,omitempty"`
	ResourceVersion string     `json:"resourceVersion,omitempty"`
}
