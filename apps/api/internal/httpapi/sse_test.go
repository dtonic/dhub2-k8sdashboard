package httpapi_test

// 상태 변경 SSE의 HTTP 계약 테스트입니다. (#12)
//
// httptest.ResponseRecorder가 아니라 **실 리스너**(httptest.NewServer)를 씁니다 —
// flush·write deadline·클라이언트 취소는 실 연결에서만 진짜로 검증됩니다.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/auth"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/queryprotect"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

const streamPath = "/api/v1/clusters/" + testcluster.ClusterID + "/events/stream"

func allScope() scope.Scope {
	return scope.Scope{Clusters: []scope.Cluster{{ID: testcluster.ClusterID, Name: "Seoul", All: true}}}
}

type sseFixture struct {
	srv     *httpapi.Server
	hub     *stream.Hub
	metrics *stream.Metrics
	store   *clusterstate.Store
	fakes   testcluster.Fakes
	ts      *httptest.Server
}

// newSSEFixture는 informer(가짜 클러스터) → 허브 → 서버 전체를 실제 배선으로 세웁니다.
func newSSEFixture(t *testing.T, hubCfg stream.Config, opts httpapi.StreamOptions,
	resolver scope.Resolver, guard *queryprotect.Guard, tweak func(*httptest.Server)) *sseFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, fakes := testcluster.NewStore(t, ctx)
	metrics := stream.NewMetrics()
	hub, err := stream.New(hubCfg, metrics)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hub.Close)
	if err := store.OnChange(func(c clusterstate.Change) {
		hub.Publish(stream.EnvelopeFromChange(testcluster.ClusterID, c))
	}); err != nil {
		t.Fatal(err)
	}

	if resolver == nil {
		resolver = scope.Static{S: allScope()}
	}
	d := demo.New(store)
	srv := httpapi.NewServer(httpapi.Deps{
		Store: store, Metrics: d, Logs: d, Alerts: d, Topology: d,
		Resolver: resolver, Guard: guard,
		Stream: hub, StreamMetrics: metrics, StreamOptions: opts,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewUnstartedServer(srv)
	if tweak != nil {
		tweak(ts)
	}
	ts.Start()
	t.Cleanup(ts.Close)
	return &sseFixture{srv: srv, hub: hub, metrics: metrics, store: store, fakes: fakes, ts: ts}
}

/* ── SSE 클라이언트 ─────────────────────────────────────────────────────── */

type sseFrame struct {
	ID, Event, Data, Comment, Retry string
}

type sseConn struct {
	resp   *http.Response
	cancel context.CancelFunc
	frames <-chan sseFrame
	closed <-chan struct{}
}

func (c *sseConn) Close() { c.cancel() }

// next는 comment(heartbeat)를 포함한 다음 프레임을 기다립니다.
func (c *sseConn) next(t *testing.T, timeout time.Duration) (sseFrame, bool) {
	t.Helper()
	select {
	case f, ok := <-c.frames:
		return f, ok
	case <-time.After(timeout):
		t.Fatal("SSE 프레임 수신 타임아웃")
	}
	return sseFrame{}, false
}

// nextEvent는 comment·retry를 건너뛰고 data가 있는 이벤트만 돌려줍니다.
func (c *sseConn) nextEvent(t *testing.T, timeout time.Duration) contract.EventEnvelope {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			t.Fatal("SSE 이벤트 수신 타임아웃")
		}
		f, ok := c.next(t, remain)
		if !ok {
			t.Fatal("스트림이 이벤트 전에 닫혔습니다")
		}
		if f.Data == "" {
			continue
		}
		var env contract.EventEnvelope
		if err := json.Unmarshal([]byte(f.Data), &env); err != nil {
			t.Fatalf("EventEnvelope 파싱 실패: %v — %q", err, f.Data)
		}
		if f.ID != env.ID || f.Event != string(env.Kind) {
			t.Fatalf("SSE 필드와 봉투 불일치: id %q/%q event %q/%q", f.ID, env.ID, f.Event, env.Kind)
		}
		return env
	}
}

func dialSSE(t *testing.T, url, token, lastEventID string) (*sseConn, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	if resp.StatusCode != http.StatusOK {
		return &sseConn{resp: resp, cancel: cancel}, resp
	}

	frames := make(chan sseFrame, 64)
	closed := make(chan struct{})
	go func() {
		defer close(frames)
		defer close(closed)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var cur sseFrame
		dirty := false
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if dirty {
					frames <- cur
					cur, dirty = sseFrame{}, false
				}
			case strings.HasPrefix(line, "id: "):
				cur.ID, dirty = line[4:], true
			case strings.HasPrefix(line, "event: "):
				cur.Event, dirty = line[7:], true
			case strings.HasPrefix(line, "data: "):
				cur.Data, dirty = line[6:], true
			case strings.HasPrefix(line, "retry: "):
				cur.Retry, dirty = line[7:], true
			case strings.HasPrefix(line, ":"):
				cur.Comment, dirty = strings.TrimSpace(line[1:]), true
			}
		}
	}()
	return &sseConn{resp: resp, cancel: cancel, frames: frames, closed: closed}, resp
}

func decodeAPIError(t *testing.T, resp *http.Response) contract.APIError {
	t.Helper()
	defer resp.Body.Close()
	var e contract.APIError
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func waitConnections(t *testing.T, hub *stream.Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if conns, _ := hub.Stats(); conns == want {
			return
		}
		if time.Now().After(deadline) {
			conns, _ := hub.Stats()
			t.Fatalf("연결 수가 %d로 돌아오지 않았습니다: %d", want, conns)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

/* ── 본 계약 ────────────────────────────────────────────────────────────── */

// TestSSELiveEventFromInformerAndHeaders — informer 변경이 실 HTTP 스트림으로
// 도착하고, SSE 헤더·retry 힌트 계약을 지킵니다.
func TestSSELiveEventFromInformerAndHeaders(t *testing.T) {
	f := newSSEFixture(t, stream.Config{}, httpapi.StreamOptions{}, nil, nil, nil)
	conn, resp := dialSSE(t, f.ts.URL+streamPath, "", "")
	defer conn.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type: %q", ct)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID가 없습니다")
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control: %q", cc)
	}
	first, _ := conn.next(t, 2*time.Second)
	if first.Retry != "3000" {
		t.Fatalf("retry 힌트: %+v", first)
	}

	// informer 캐시에 이미 있던 픽스처는 스트림에 나오지 않아야 합니다(초기 LIST 억제).
	// 새 Pod 생성만 도착합니다.
	pod := testcluster.NewPod("payments", "checkout-7f-xyz", "pod-sse-live", "ReplicaSet", "payments-api-7f", testcluster.UIDReplicaSetCurrent)
	pod.ResourceVersion = "100"
	if _, err := f.fakes.Typed.CoreV1().Pods("payments").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	env := conn.nextEvent(t, 3*time.Second)
	if env.Kind != contract.StreamKindPod || env.Action != contract.StreamActionAdded {
		t.Fatalf("pod added 기대: %+v", env)
	}
	if env.Namespace != "payments" || env.Entity == nil || env.Entity.PodUID != "pod-sse-live" {
		t.Fatalf("신원이 틀렸습니다: %+v", env)
	}
	if env.ClusterID != testcluster.ClusterID || env.ResourceVersion != "100" {
		t.Fatalf("클러스터·RV: %+v", env)
	}
}

// TestSSEScopeIsolationOverRealOIDC — #10 SSE 수용 기준. 실서명 mock IdP →
// 실제 OIDC resolver → Scope → 스트림 필터 전체 경로에서, 같은 Last-Event-ID로
// 재연결해도 namespace 사용자는 다른 namespace 봉투를 한 개도 받지 않습니다.
func TestSSEScopeIsolationOverRealOIDC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	idp, err := auth.StartMockIDP("", "k8s-dashboard", func() time.Time { return testcluster.Now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idp.Close() })
	resolver, err := auth.NewResolver(ctx, auth.Config{
		IssuerURL: idp.Issuer, Audience: "k8s-dashboard",
		ClusterID: testcluster.ClusterID, ClusterName: "Seoul Production",
		Now: func() time.Time { return testcluster.Now },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f := newSSEFixture(t, stream.Config{}, httpapi.StreamOptions{}, resolver, nil, nil)

	admin, _ := idp.Token("kim", []string{"platform.admin"}, time.Hour)
	viewer, _ := idp.Token("lee", []string{"namespace.viewer:payments"}, time.Hour)

	// 토큰 없음 → 401. 인증 경계는 화면 라우트와 같습니다.
	_, resp := dialSSE(t, f.ts.URL+streamPath, "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("무토큰: got %d want 401", resp.StatusCode)
	}
	resp.Body.Close()
	if connections, retained := f.hub.Stats(); connections != 0 || retained != 0 {
		t.Fatalf("unauthorized request reached stream hub: connections=%d retained=%d", connections, retained)
	}

	adminConn, _ := dialSSE(t, f.ts.URL+streamPath, admin, "")
	viewerConn, _ := dialSSE(t, f.ts.URL+streamPath, viewer, "")
	defer adminConn.Close()
	defer viewerConn.Close()
	waitConnections(t, f.hub, 2)

	publish := func(ns string) {
		f.hub.Publish(contract.EventEnvelope{
			Kind: contract.StreamKindPod, Action: contract.StreamActionUpdated,
			ClusterID: testcluster.ClusterID, Namespace: ns,
			Entity: &contract.EntityRef{ClusterID: testcluster.ClusterID, Namespace: ns},
		})
	}
	publish("payments")
	publish("media")
	publish("payments") // 마커

	adminFirst := adminConn.nextEvent(t, 2*time.Second)
	if got := adminFirst.Namespace; got != "payments" {
		t.Fatalf("admin 1: %s", got)
	}
	if got := adminConn.nextEvent(t, 2*time.Second).Namespace; got != "media" {
		t.Fatalf("admin 2: %s", got)
	}
	v1 := viewerConn.nextEvent(t, 2*time.Second)
	v2 := viewerConn.nextEvent(t, 2*time.Second)
	if v1.Namespace != "payments" || v2.Namespace != "payments" {
		t.Fatalf("viewer에게 범위 밖 봉투: %s, %s", v1.Namespace, v2.Namespace)
	}
	lastID := v1.ID // 같은 Last-Event-ID를 두 사용자 모두에 사용합니다.

	adminConn.Close()
	viewerConn.Close()
	waitConnections(t, f.hub, 0)

	adminConn2, _ := dialSSE(t, f.ts.URL+streamPath, admin, adminFirst.ID)
	viewerConn2, _ := dialSSE(t, f.ts.URL+streamPath, viewer, lastID)
	defer adminConn2.Close()
	defer viewerConn2.Close()

	// admin 재생: media + 마커 payments. viewer 재생: 마커 payments 하나뿐.
	if got := adminConn2.nextEvent(t, 2*time.Second).Namespace; got != "media" {
		t.Fatalf("admin 재생 1: %s", got)
	}
	if got := adminConn2.nextEvent(t, 2*time.Second).Namespace; got != "payments" {
		t.Fatalf("admin 재생 2: %s", got)
	}
	if got := viewerConn2.nextEvent(t, 2*time.Second).Namespace; got != "payments" {
		t.Fatalf("viewer 재생: %s", got)
	}
	// viewer에게 더 오는 이벤트가 없어야 media 미유출이 증명됩니다.
	publish("payments") // 라이브 마커
	if got := viewerConn2.nextEvent(t, 2*time.Second).Namespace; got != "payments" {
		t.Fatalf("viewer 라이브: %s", got)
	}
}

// TestSSEReconnectReplayExactOrder — 실 HTTP 재연결에서 놓친 구간이 순서대로,
// 중복 없이 재생됩니다.
func TestSSEReconnectReplayExactOrder(t *testing.T) {
	f := newSSEFixture(t, stream.Config{}, httpapi.StreamOptions{}, nil, nil, nil)
	conn, _ := dialSSE(t, f.ts.URL+streamPath, "", "")

	pub := func(ns string) {
		f.hub.Publish(contract.EventEnvelope{
			Kind: contract.StreamKindWorkload, Action: contract.StreamActionUpdated,
			ClusterID: testcluster.ClusterID, Namespace: ns,
		})
	}
	pub("payments")
	last := conn.nextEvent(t, 2*time.Second)
	conn.Close()
	waitConnections(t, f.hub, 0)

	pub("media")
	pub("batch")

	conn2, _ := dialSSE(t, f.ts.URL+streamPath, "", last.ID)
	defer conn2.Close()
	r1 := conn2.nextEvent(t, 2*time.Second)
	r2 := conn2.nextEvent(t, 2*time.Second)
	if r1.Namespace != "media" || r2.Namespace != "batch" {
		t.Fatalf("재생 순서: %s, %s", r1.Namespace, r2.Namespace)
	}
	if r1.ID == last.ID || r2.ID == r1.ID {
		t.Fatal("재생에 중복이 있습니다")
	}
	pub("payments")
	live := conn2.nextEvent(t, 2*time.Second)
	if live.Namespace != "payments" || live.ID == r2.ID {
		t.Fatalf("재생→라이브 연속성: %+v", live)
	}
}

// TestSSEResetOnUnknownInstance — 다른 인스턴스(재시작·replica 이동)의 ID는
// 조용한 재생 주장 대신 reset 신호를 받습니다.
func TestSSEResetOnUnknownInstance(t *testing.T) {
	f := newSSEFixture(t, stream.Config{}, httpapi.StreamOptions{}, nil, nil, nil)
	for _, id := range []string{"deadbeefdeadbeefdeadbeef.0000000000000000-3"} {
		conn, resp := dialSSE(t, f.ts.URL+streamPath, "", id)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d", id, resp.StatusCode)
		}
		env := conn.nextEvent(t, 2*time.Second)
		if env.Kind != contract.StreamKindReset || env.Action != contract.StreamActionReset {
			t.Fatalf("%s: reset 기대, got %+v", id, env)
		}
		conn.Close()
	}
}

// TestSSEBadLastEventIDRejectedBeforeSubscription — 형식 오류·상한 초과는
// 구독 자원 없이 400으로 끝납니다.
func TestSSEBadLastEventIDRejectedBeforeSubscription(t *testing.T) {
	f := newSSEFixture(t, stream.Config{}, httpapi.StreamOptions{}, nil, nil, nil)
	for _, id := range []string{"not-an-id!", strings.Repeat("a", contract.MaxStreamEventIDLen+1)} {
		_, resp := dialSSE(t, f.ts.URL+streamPath, "", id)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%q: got %d want 400", id, resp.StatusCode)
		}
		if e := decodeAPIError(t, resp); e.Code != "bad_last_event_id" {
			t.Fatalf("code: %s", e.Code)
		}
	}
	if conns, _ := f.hub.Stats(); conns != 0 {
		t.Fatalf("거절 요청이 구독을 남겼습니다: %d", conns)
	}
}

// TestSSECapacity429WithRetryAfter — 연결 상한 초과는 429 + Retry-After입니다.
func TestSSECapacity429WithRetryAfter(t *testing.T) {
	f := newSSEFixture(t, stream.Config{MaxConnections: 1, MaxPerSubject: 1}, httpapi.StreamOptions{}, nil, nil, nil)
	conn, _ := dialSSE(t, f.ts.URL+streamPath, "", "")
	defer conn.Close()
	waitConnections(t, f.hub, 1)

	_, resp := dialSSE(t, f.ts.URL+streamPath, "", "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %d want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("Retry-After가 없습니다")
	}
	if e := decodeAPIError(t, resp); e.Code != "stream_capacity" {
		t.Fatalf("code: %s", e.Code)
	}
}

// TestSSESurvivesQueryBudgetAndWriteTimeout — 12s 질의 budget(여기서는 80ms로
// 줄임)과 http.Server.WriteTimeout(250ms)을 넘겨도 heartbeat로 살아 있습니다.
func TestSSESurvivesQueryBudgetAndWriteTimeout(t *testing.T) {
	guardCfg := queryprotect.DefaultConfig()
	guardCfg.QueryTimeout = 80 * time.Millisecond
	guard := queryprotect.New(guardCfg, queryprotect.NewMetrics())
	f := newSSEFixture(t, stream.Config{},
		httpapi.StreamOptions{Heartbeat: 50 * time.Millisecond, WriteIdleTimeout: 200 * time.Millisecond},
		nil, guard, func(ts *httptest.Server) { ts.Config.WriteTimeout = 250 * time.Millisecond })

	conn, _ := dialSSE(t, f.ts.URL+streamPath, "", "")
	defer conn.Close()

	// 질의 budget(80ms)·WriteTimeout(250ms)보다 훨씬 긴 800ms 동안 스트림이 살아
	// 있어야 합니다. heartbeat가 write deadline을 계속 앞으로 밀기 때문입니다.
	deadline := time.Now().Add(800 * time.Millisecond)
	heartbeats := 0
	for time.Now().Before(deadline) {
		f, ok := conn.next(t, 2*time.Second)
		if !ok {
			t.Fatal("스트림이 타임아웃 창 안에서 죽었습니다")
		}
		if f.Comment != "" {
			if f.ID != "" {
				t.Fatalf("heartbeat advanced Last-Event-ID cursor: %q", f.ID)
			}
			heartbeats++
		}
	}
	if heartbeats < 5 {
		t.Fatalf("heartbeat가 부족합니다: %d", heartbeats)
	}
	// 스트림 생존 후에도 이벤트가 정상 전달됩니다.
	f.hub.Publish(contract.EventEnvelope{Kind: contract.StreamKindPod, Action: contract.StreamActionUpdated,
		ClusterID: testcluster.ClusterID, Namespace: "payments"})
	if env := conn.nextEvent(t, 2*time.Second); env.Namespace != "payments" {
		t.Fatalf("생존 후 이벤트: %+v", env)
	}
}

/* ── 막힌 writer · 정리 ─────────────────────────────────────────────────── */

// blockingWriter는 write deadline을 지원하지만 Write가 데드라인까지 막히는
// 최악의 소켓을 흉내 냅니다.
type blockingWriter struct {
	hdr      http.Header
	mu       sync.Mutex
	deadline time.Time
}

func (w *blockingWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *blockingWriter) WriteHeader(int) {}
func (w *blockingWriter) Flush()          {}
func (w *blockingWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = t
	return nil
}
func (w *blockingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	d := w.deadline
	w.mu.Unlock()
	if d.IsZero() {
		return len(p), nil
	}
	if wait := time.Until(d); wait > 0 {
		time.Sleep(wait)
	}
	return 0, os.ErrDeadlineExceeded
}

// TestSSEBlockedWriterEndsWithinIdleDeadline — 소켓이 완전히 막혀도 핸들러는
// write idle 데드라인 안에서 끝나고 구독을 정리합니다.
func TestSSEBlockedWriterEndsWithinIdleDeadline(t *testing.T) {
	f := newSSEFixture(t, stream.Config{},
		httpapi.StreamOptions{Heartbeat: 20 * time.Millisecond, WriteIdleTimeout: 100 * time.Millisecond},
		nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, streamPath, nil)
	done := make(chan struct{})
	start := time.Now()
	go func() {
		f.srv.ServeHTTP(&blockingWriter{}, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("막힌 writer가 데드라인 안에 끝나지 않았습니다")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("종료까지 %v — 유계가 아닙니다", elapsed)
	}
	waitConnections(t, f.hub, 0)
}

// TestSSEClientCancelAndHubCloseCleanup — 클라이언트 취소·허브 종료 모두
// 구독·고루틴이 0으로 돌아옵니다.
func TestSSEClientCancelAndHubCloseCleanup(t *testing.T) {
	f := newSSEFixture(t, stream.Config{}, httpapi.StreamOptions{}, nil, nil, nil)

	conn, _ := dialSSE(t, f.ts.URL+streamPath, "", "")
	waitConnections(t, f.hub, 1)
	conn.Close() // 클라이언트 취소
	waitConnections(t, f.hub, 0)

	conn2, _ := dialSSE(t, f.ts.URL+streamPath, "", "")
	waitConnections(t, f.hub, 1)
	f.hub.Close() // 서버 종료 경로 (main은 Shutdown 전에 이걸 부릅니다)
	select {
	case <-conn2.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("허브 종료 후에도 스트림이 열려 있습니다")
	}
	waitConnections(t, f.hub, 0)

	_, resp := dialSSE(t, f.ts.URL+streamPath, "", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("closed hub status=%d want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Fatalf("closed hub must not advertise capacity retry: %q", got)
	}
	if e := decodeAPIError(t, resp); e.Code != "upstream_unavailable" {
		t.Fatalf("closed hub code=%q", e.Code)
	}
}

// TestSSEMetricsExposedWithoutSensitiveValues — /metrics에 스트림 지표가 나가되
// 주체·namespace·Last-Event-ID가 라벨·값으로 새지 않습니다.
func TestSSEMetricsExposedWithoutSensitiveValues(t *testing.T) {
	f := newSSEFixture(t, stream.Config{}, httpapi.StreamOptions{}, nil, nil, nil)
	conn, _ := dialSSE(t, f.ts.URL+streamPath, "", "")
	waitConnections(t, f.hub, 1)
	f.hub.Publish(contract.EventEnvelope{Kind: contract.StreamKindPod, Action: contract.StreamActionUpdated,
		ClusterID: testcluster.ClusterID, Namespace: "payments"})
	conn.nextEvent(t, 2*time.Second)
	// 형식 오류 거절도 지표에 남습니다.
	if _, resp := dialSSE(t, f.ts.URL+streamPath, "", "bad!!"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d", resp.StatusCode)
	}

	resp, err := http.Get(f.ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := string(body)
	for _, want := range []string{
		"dashboard_stream_connections 1",
		"dashboard_stream_opened_total 1",
		`dashboard_stream_events_total{kind="pod"} 1`,
		`dashboard_stream_rejected_total{reason="bad_last_event_id"} 1`,
		"dashboard_stream_delivery_seconds_count 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics에 %q가 없습니다:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"payments", "auth-none", "Last-Event-ID", "bad!!"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("/metrics에 민감·가변 값 %q가 노출되었습니다", forbidden)
		}
	}
}

// TestSSEForbiddenClusterIs403 — Scope 밖 클러스터는 구독 없이 403입니다.
func TestSSEForbiddenClusterIs403(t *testing.T) {
	f := newSSEFixture(t, stream.Config{}, httpapi.StreamOptions{}, nil, nil, nil)
	_, resp := dialSSE(t, f.ts.URL+"/api/v1/clusters/other-cluster/events/stream", "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d want 403", resp.StatusCode)
	}
	if e := decodeAPIError(t, resp); e.Code != "forbidden" {
		t.Fatalf("code: %s", e.Code)
	}
	if conns, _ := f.hub.Stats(); conns != 0 {
		t.Fatalf("403이 구독을 남겼습니다: %d", conns)
	}
}
