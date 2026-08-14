package stream

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics는 스트림 계측입니다. queryprotect.Metrics처럼 의존성 없이
// Prometheus 텍스트 형식을 직접 씁니다.
//
// 라벨 규칙 — 값은 전부 **고정된 kind/reason 문자열**입니다.
// 주체·namespace·ID·질의·Last-Event-ID는 어떤 경우에도 라벨이 되지 않습니다.
// 카디널리티가 유계여야 /metrics 자체가 메모리 누수가 되지 않습니다.
type Metrics struct {
	connections atomic.Int64
	counters    sync.Map // 완성된 메트릭 라인 → *atomic.Uint64

	deliveryCount atomic.Uint64
	deliverySumUs atomic.Uint64 // 마이크로초 합 — 출력 시 초로 변환합니다.
}

func NewMetrics() *Metrics { return &Metrics{} }

func (m *Metrics) add(name string, delta uint64) {
	v, _ := m.counters.LoadOrStore(name, &atomic.Uint64{})
	v.(*atomic.Uint64).Add(delta)
}

func (m *Metrics) StreamOpened() {
	m.connections.Add(1)
	m.add("dashboard_stream_opened_total", 1)
}

func (m *Metrics) StreamClosed(reason string) {
	m.connections.Add(-1)
	m.add(fmt.Sprintf("dashboard_stream_closed_total{reason=%q}", reason), 1)
}

func (m *Metrics) StreamRejected(reason string) {
	m.add(fmt.Sprintf("dashboard_stream_rejected_total{reason=%q}", reason), 1)
}

func (m *Metrics) StreamDropped(reason string) {
	m.add(fmt.Sprintf("dashboard_stream_dropped_total{reason=%q}", reason), 1)
}

func (m *Metrics) StreamReplayed(n int) {
	if n > 0 {
		m.add("dashboard_stream_replayed_events_total", uint64(n))
	}
}

func (m *Metrics) StreamReset() { m.add("dashboard_stream_reset_total", 1) }

func (m *Metrics) StreamPublished(kind string) {
	m.add(fmt.Sprintf("dashboard_stream_events_total{kind=%q}", kind), 1)
}

// StreamDelivered는 발행부터 소켓 write 완료까지의 전달 지연 하나를 기록합니다.
func (m *Metrics) StreamDelivered(d time.Duration) {
	m.deliveryCount.Add(1)
	if d > 0 {
		m.deliverySumUs.Add(uint64(d.Microseconds()))
	}
}

var _ Observer = (*Metrics)(nil)

// WritePrometheus는 텍스트 형식으로 씁니다. 게이지 → 카운터 → 지연 순서입니다.
func (m *Metrics) WritePrometheus(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "dashboard_stream_connections %d\n", m.connections.Load()); err != nil {
		return err
	}
	keys := []string{}
	m.counters.Range(func(k, _ any) bool { keys = append(keys, k.(string)); return true })
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := m.counters.Load(k)
		if _, err := fmt.Fprintf(w, "%s %d\n", k, v.(*atomic.Uint64).Load()); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "dashboard_stream_delivery_seconds_count %d\ndashboard_stream_delivery_seconds_sum %.6f\n",
		m.deliveryCount.Load(), float64(m.deliverySumUs.Load())/1e6)
	return err
}
