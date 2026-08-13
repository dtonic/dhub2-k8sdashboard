// Package datasource는 관측 데이터의 **바깥 경계**입니다.
//
// GreptimeDB(메트릭), Quickwit(로그), Alertmanager/Grafana(알림)는 이미 있는 것을
// 그대로 씁니다. 우리는 새 저장소를 만들지 않고 어댑터만 갖습니다. (README §2)
//
// 어댑터가 죽어도 화면 전체가 죽지 않아야 하므로, 실패는 에러로 올리되
// 핸들러가 그것을 **섹션 단위 degraded**로 바꿔 내려보냅니다. (ADR 0002)
package datasource

import (
	"context"
	"errors"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// ErrUnavailable은 데이터소스에 붙지 못했을 때입니다.
var ErrUnavailable = errors.New("데이터소스에 연결할 수 없습니다")

// Window는 어댑터에 넘기는 확정된 조회 구간입니다.
type Window struct {
	From time.Time
	To   time.Time
	Step time.Duration
}

// Target은 무엇에 대한 데이터인지입니다. 비어 있는 필드는 제한 없음입니다.
type Target struct {
	ClusterID    string
	Namespace    string
	WorkloadKind string
	WorkloadName string
	PodUID       string
}

// Metrics는 시계열 어댑터입니다.
type Metrics interface {
	// Trends는 화면에 그릴 패널 묶음을 한 번에 돌려줍니다.
	// 패널마다 호출하면 화면 하나가 데이터소스에 N번 나갑니다.
	Trends(ctx context.Context, t Target, w Window, panels []string) ([]contract.TrendPanel, error)
	// Usage는 Pod UID → 현재 사용량입니다. request/limit는 Kubernetes에서 옵니다.
	Usage(ctx context.Context, clusterID string) (map[string]contract.ContainerUsage, error)
}

// LogQuery는 로그 조회 조건입니다. Raw Query는 받지 않습니다 —
// 프런트에서 만든 질의를 그대로 실행하면 조회 범위를 서버가 통제할 수 없습니다. (README §10)
type LogQuery struct {
	Target    Target
	Window    Window
	Levels    []contract.LogLevel
	Container string
	// Text는 전문 검색어입니다. 어댑터가 이스케이프해서 씁니다.
	Text string
	// Cursor는 (timestamp, id) 복합키를 인코딩한 불투명 문자열입니다.
	Cursor   string
	PageSize int
}

// LogPage는 한 페이지 분량의 로그와 다음 커서입니다.
type LogPage struct {
	Lines []contract.LogLine
	// Next가 비어 있으면 더 없음입니다.
	Next      string
	Truncated bool
	MaxLines  int
}

// Logs는 로그 어댑터입니다.
type Logs interface {
	Search(ctx context.Context, q LogQuery) (LogPage, error)
	// Histogram은 범위 전체의 레벨별 분포입니다. 페이지와 무관하게 한 번만 조회합니다.
	Histogram(ctx context.Context, q LogQuery) ([]contract.LogHistogramBucket, error)
	// Facets는 현재 Scope에서 실제로 관측된 필터 후보입니다.
	Facets(ctx context.Context, q LogQuery) (contract.LogFacets, error)
}

// AlertQuery는 알림 조회 조건입니다.
type AlertQuery struct {
	Target Target
	Window Window
}

// AlertResult는 진행 중/해소된 알림입니다.
type AlertResult struct {
	Firing   []contract.AlertInstance
	Resolved []contract.AlertInstance
	// GroupingRule은 무엇을 기준으로 묶었는지입니다. 화면에 그대로 노출합니다.
	GroupingRule string
}

// Alerts는 알림 어댑터입니다. 자체 평가 엔진을 만들지 않고 조회만 합니다.
type Alerts interface {
	List(ctx context.Context, q AlertQuery) (AlertResult, error)
}

// Topology는 통신 그래프 어댑터입니다.
type Topology interface {
	Graph(ctx context.Context, t Target, w Window) (contract.TopologyGraph, error)
	EdgeSeries(ctx context.Context, clusterID, edgeID string, w Window) ([]contract.TrendSeries, error)
}

// CatalogPod는 데이터소스가 화면과 **같은 Pod 신원**을 쓰도록 넘겨받는 최소 정보입니다.
//
// 여기서 이름이나 UID를 새로 지어내면 로그 → Pod 상세 딥링크가 404가 됩니다.
// 실제로 한 번 겪은 문제라서 규칙으로 굳혔습니다. (CLAUDE.md)
type CatalogPod struct {
	Namespace    string
	Name         string
	UID          string
	WorkloadKind string
	WorkloadName string
	Node         string
}

// PodCatalog는 클러스터 상태에서 Pod 신원을 빌려오는 통로입니다.
type PodCatalog interface {
	CatalogPods(namespace string, limit int) []CatalogPod
}

/* ── 연결되지 않은 데이터소스 ──────────────────────────────────────────── */

// Unavailable은 설정되지 않은 데이터소스입니다.
// nil 인터페이스를 두면 핸들러마다 nil 검사가 흩어지므로, 항상 실패하는 구현을 둡니다.
type Unavailable struct{ Reason string }

func (u Unavailable) err() error {
	if u.Reason == "" {
		return ErrUnavailable
	}
	return errors.New(u.Reason)
}

func (u Unavailable) Trends(context.Context, Target, Window, []string) ([]contract.TrendPanel, error) {
	return nil, u.err()
}

func (u Unavailable) Usage(context.Context, string) (map[string]contract.ContainerUsage, error) {
	return nil, u.err()
}

func (u Unavailable) Search(context.Context, LogQuery) (LogPage, error) { return LogPage{}, u.err() }

func (u Unavailable) Histogram(context.Context, LogQuery) ([]contract.LogHistogramBucket, error) {
	return nil, u.err()
}

func (u Unavailable) Facets(context.Context, LogQuery) (contract.LogFacets, error) {
	return contract.LogFacets{}, u.err()
}

func (u Unavailable) List(context.Context, AlertQuery) (AlertResult, error) {
	return AlertResult{}, u.err()
}

func (u Unavailable) Graph(context.Context, Target, Window) (contract.TopologyGraph, error) {
	return contract.TopologyGraph{}, u.err()
}

func (u Unavailable) EdgeSeries(context.Context, string, string, Window) ([]contract.TrendSeries, error) {
	return nil, u.err()
}
