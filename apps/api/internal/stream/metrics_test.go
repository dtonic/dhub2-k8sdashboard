package stream

import (
	"strings"
	"testing"
	"time"
)

// TestMetricsFixedLabelsOnly — 라벨은 고정된 kind/reason뿐이고, 주체·namespace·
// ID·Last-Event-ID 같은 가변 값이 /metrics에 나가지 않아야 합니다.
func TestMetricsFixedLabelsOnly(t *testing.T) {
	m := NewMetrics()
	m.StreamOpened()
	m.StreamOpened()
	m.StreamClosed("client")
	m.StreamRejected("capacity")
	m.StreamRejected("bad_last_event_id")
	m.StreamDropped("slow_subscriber")
	m.StreamReplayed(7)
	m.StreamReplayed(0) // no-op이어야 합니다
	m.StreamReset()
	m.StreamPublished("pod")
	m.StreamPublished("alert")
	m.StreamDelivered(1500 * time.Microsecond)
	m.StreamDelivered(500 * time.Microsecond)

	var sb strings.Builder
	if err := m.WritePrometheus(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	for _, want := range []string{
		"dashboard_stream_connections 1",
		"dashboard_stream_opened_total 2",
		`dashboard_stream_closed_total{reason="client"} 1`,
		`dashboard_stream_rejected_total{reason="capacity"} 1`,
		`dashboard_stream_rejected_total{reason="bad_last_event_id"} 1`,
		`dashboard_stream_dropped_total{reason="slow_subscriber"} 1`,
		"dashboard_stream_replayed_events_total 7",
		"dashboard_stream_reset_total 1",
		`dashboard_stream_events_total{kind="pod"} 1`,
		`dashboard_stream_events_total{kind="alert"} 1`,
		"dashboard_stream_delivery_seconds_count 2",
		"dashboard_stream_delivery_seconds_sum 0.002000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("메트릭에 %q가 없습니다:\n%s", want, out)
		}
	}
	// 허용 라벨 키는 kind·reason뿐입니다.
	for _, forbidden := range []string{"subject=", "namespace=", "id=", "lastEventId=", "user="} {
		if strings.Contains(out, forbidden) {
			t.Errorf("금지된 라벨 %q가 노출되었습니다:\n%s", forbidden, out)
		}
	}
}

// TestHubReportsToObserver — 허브 경로가 옵저버 훅을 실제로 부르는지 확인합니다.
func TestHubReportsToObserver(t *testing.T) {
	m := NewMetrics()
	h := newTestHub(t, Config{SubscriberBuffer: 1}, m)

	sub, err := h.Subscribe("u", allFilter(), "")
	if err != nil {
		t.Fatal(err)
	}
	// 소비하지 않는 구독자를 버퍼 초과로 끊어 dropped를 유발합니다.
	h.Publish(podEnv("payments"))
	h.Publish(podEnv("payments"))
	_ = sub

	if _, err := h.Subscribe("u2", allFilter(), "zzz"); err == nil {
		t.Fatal("형식 오류가 통과했습니다")
	}

	var sb strings.Builder
	if err := m.WritePrometheus(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"dashboard_stream_opened_total 1",
		`dashboard_stream_events_total{kind="pod"} 2`,
		`dashboard_stream_dropped_total{reason="slow_subscriber"} 1`,
		`dashboard_stream_rejected_total{reason="bad_last_event_id"} 1`,
		"dashboard_stream_connections 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("메트릭에 %q가 없습니다:\n%s", want, out)
		}
	}
}
