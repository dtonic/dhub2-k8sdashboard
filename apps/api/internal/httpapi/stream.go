package httpapi

// 상태 변경 SSE 핸들러입니다. (#12, ADR 0005)
//
// 이 경로는 화면 조회와 규칙이 다릅니다.
//   - 인증·클러스터 권한·감사는 다른 /api/v1 경로와 **똑같이** 지나갑니다.
//   - #11 질의 보호(12초 타임아웃·rate·캐시·slow 집계)는 **타지 않습니다** —
//     수 분짜리 연결을 질의 예산으로 재면 전부 오탐입니다. 대신 스트림 전용
//     연결 상한(stream.Hub)이 자원을 지킵니다.
//   - http.Server.WriteTimeout(응답 전체 상한)은 SSE에 맞지 않으므로,
//     write마다 ResponseController로 유휴 데드라인을 앞으로 밉니다.
//     막힌 writer는 그 데드라인에서 끊깁니다 — 무한정 잡고 있지 않습니다.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
)

// streamRoute는 SSE 경로 패턴입니다. ServeHTTP가 이 패턴을 질의 보호에서 제외합니다.
const streamRoute = "GET /api/v1/clusters/{clusterId}/events/stream"

// StreamOptions는 SSE 전송 동작입니다. 0이면 기본값을 씁니다.
type StreamOptions struct {
	// Heartbeat는 comment 한 줄을 보내는 주기입니다. 중간 프록시의 유휴 종단을 막고
	// 죽은 연결을 write 오류로 드러냅니다. WriteIdleTimeout보다 짧아야 합니다.
	Heartbeat time.Duration
	// WriteIdleTimeout은 write 하나가 막혀 있을 수 있는 상한입니다.
	WriteIdleTimeout time.Duration
	// RetryHintMs는 브라우저 EventSource 재연결 지연 힌트입니다.
	RetryHintMs int
	// RetryAfterSeconds는 연결 상한 초과(429) 응답의 Retry-After입니다.
	RetryAfterSeconds int
}

func (o *StreamOptions) setDefaults() {
	if o.Heartbeat <= 0 {
		o.Heartbeat = 15 * time.Second
	}
	if o.WriteIdleTimeout <= 0 {
		o.WriteIdleTimeout = 45 * time.Second
	}
	if o.RetryHintMs <= 0 {
		o.RetryHintMs = 3000
	}
	if o.RetryAfterSeconds <= 0 {
		o.RetryAfterSeconds = 5
	}
}

func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	if s.deps.Stream == nil {
		writeError(w, r, http.StatusServiceUnavailable, "upstream_unavailable", "스트림이 준비되지 않았습니다.")
		return
	}
	// 권한은 다른 경로와 같은 관문 하나입니다. Scope 밖 클러스터는 여기서 끝납니다.
	c, err := s.authorize(r.Context(), r.PathValue("clusterId"))
	if err != nil {
		writeError(w, r, http.StatusForbidden, "forbidden", "이 클러스터에 대한 접근 권한이 없습니다.")
		return
	}

	subject := scope.From(r.Context()).Subject
	if subject == "" {
		subject = "auth-none"
	}
	// 필터는 요청 파라미터가 아니라 서버가 확정한 Scope에서 만듭니다. (README §10)
	sub, err := s.deps.Stream.Subscribe(subject, stream.Filter{
		ClusterID:  c.ID,
		All:        c.All,
		Namespaces: c.Namespaces,
	}, r.Header.Get("Last-Event-ID"))
	if err != nil {
		if errors.Is(err, stream.ErrBadLastEventID) {
			writeError(w, r, http.StatusBadRequest, "bad_last_event_id", "Last-Event-ID 형식이 유효하지 않습니다.")
			return
		}
		if errors.Is(err, stream.ErrClosed) {
			writeError(w, r, http.StatusServiceUnavailable, "upstream_unavailable", "스트림이 종료되었습니다.")
			return
		}
		w.Header().Set("Retry-After", strconv.Itoa(s.deps.StreamOptions.RetryAfterSeconds))
		writeError(w, r, http.StatusTooManyRequests, "stream_capacity", "스트림 연결 한도를 초과했습니다.")
		return
	}
	defer sub.Close()

	opts := s.deps.StreamOptions
	rc := http.NewResponseController(w)
	// 데드라인·지연 측정은 벽시계(time.Now)를 씁니다 — deps.Now는 테스트에서
	// 고정되는 화면용 시계라 과거 데드라인을 만들 수 있습니다.
	extend := func() bool {
		return rc.SetWriteDeadline(time.Now().Add(opts.WriteIdleTimeout)) == nil
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// 중간 프록시(nginx 등)의 응답 버퍼링은 SSE를 침묵시킵니다.
	h.Set("X-Accel-Buffering", "no")
	if !extend() {
		return
	}
	w.WriteHeader(http.StatusOK)

	if _, err := fmt.Fprintf(w, "retry: %d\n\n", opts.RetryHintMs); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	writeEvent := func(ev stream.Event) bool {
		data, err := json.Marshal(ev.Envelope)
		if err != nil {
			return false
		}
		if !extend() {
			return false
		}
		if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.Envelope.ID, ev.Envelope.Kind, data); err != nil {
			return false
		}
		if err := rc.Flush(); err != nil {
			return false
		}
		if m := s.deps.StreamMetrics; m != nil {
			m.StreamDelivered(time.Since(ev.EnqueuedAt))
		}
		return true
	}

	// 재생 구간을 먼저 소비합니다. Subscribe가 같은 잠금에서 잘라 두어
	// 라이브 채널과 순서가 겹치거나 빠지지 않습니다.
	for _, ev := range sub.Replay() {
		if !writeEvent(ev) {
			return
		}
	}

	heartbeat := time.NewTicker(opts.Heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			// 클라이언트 취소·서버 Shutdown. defer가 구독을 해제합니다.
			return
		case ev, ok := <-sub.Events():
			if !ok {
				// 허브 Close(종료) 또는 느린 구독자 강제 해제. 브라우저가
				// Last-Event-ID로 재연결하면 놓친 구간은 재생 또는 reset이 됩니다.
				return
			}
			if !writeEvent(ev) {
				return
			}
		case <-heartbeat.C:
			if !extend() {
				return
			}
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
