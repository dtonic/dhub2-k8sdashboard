package upstream_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/upstream"
)

type fakeUpstream struct {
	hits    atomic.Int32
	status  atomic.Int32 // 0이면 200
	body    string
	gotAuth string
	gotHdr  string
	gotBody string
	gotPath string
}

func TestCircuitOpensHalfOpenSingleProbeAndRecovers(t *testing.T) {
	f := &fakeUpstream{}
	f.status.Store(500)
	now := time.Unix(1, 0)
	c := newClient(t, f, func(cfg *upstream.Config) {
		cfg.CircuitThreshold = 2
		cfg.CircuitCooldown = time.Second
		cfg.Now = func() time.Time { return now }
	})
	for i := 0; i < 2; i++ {
		if err := c.GetJSON(context.Background(), "/x", nil, nil); !errors.Is(err, datasource.ErrUnavailable) {
			t.Fatal(err)
		}
	}
	hits := f.hits.Load()
	if err := c.GetJSON(context.Background(), "/x", nil, nil); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatal(err)
	}
	if f.hits.Load() != hits {
		t.Fatal("open circuit reached network")
	}
	f.status.Store(0)
	f.body = `{}`
	now = now.Add(time.Second)
	if err := c.GetJSON(context.Background(), "/x", nil, &map[string]any{}); err != nil {
		t.Fatalf("half-open probe: %v", err)
	}
	if err := c.GetJSON(context.Background(), "/x", nil, &map[string]any{}); err != nil {
		t.Fatalf("recovered circuit: %v", err)
	}
}

func TestNoRetryRequestGetsFullLogicalTimeout(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := upstream.New(upstream.Config{BaseURL: srv.URL, What: "scroll", Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.GetJSONBodyOnce(context.Background(), "/scroll", map[string]string{"id": "x"}, &map[string]any{}); err != nil {
		t.Fatalf("non-retry request lost half its budget: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

func TestResponseLimitAndTrailingPayloadAreStrict(t *testing.T) {
	const capBytes = 32 << 20
	tests := []struct {
		name, body string
		wantErr    bool
	}{
		{"exact", `"` + strings.Repeat("a", capBytes-2) + `"`, false},
		{"cap-plus-one", `"` + strings.Repeat("a", capBytes-1) + `"`, true},
		{"oversized-whitespace", `{}` + strings.Repeat(" ", capBytes), true},
		{"trailing-json", `{} {}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeUpstream{body: tt.body}
			c := newClient(t, f)
			var out any
			err := c.GetJSON(context.Background(), "/x", nil, &out)
			if tt.wantErr != errors.Is(err, datasource.ErrUnavailable) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestHalfOpenAllowsOneConcurrentProbe(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "x", 500)
			return
		}
		entered <- struct{}{}
		<-gate
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	now := time.Unix(1, 0)
	c, err := upstream.New(upstream.Config{BaseURL: srv.URL, What: "x", Timeout: time.Second, CircuitThreshold: 1, CircuitCooldown: time.Second, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.GetJSON(context.Background(), "/x", nil, nil)
	fail.Store(false)
	now = now.Add(time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- c.GetJSON(context.Background(), "/x", nil, &map[string]any{}) }()
	}
	<-entered
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	if hits.Load() != 2 {
		t.Fatalf("network hits=%d, want initial failure + one probe", hits.Load())
	}
}

func (f *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.gotAuth = r.Header.Get("Authorization")
		f.gotHdr = r.Header.Get("X-Custom")
		f.gotPath = r.URL.Path + "?" + r.URL.RawQuery
		if r.Body != nil {
			b := make([]byte, 1024)
			n, _ := r.Body.Read(b)
			f.gotBody = string(b[:n])
		}
		if s := f.status.Load(); s != 0 {
			http.Error(w, "err", int(s))
			return
		}
		w.Write([]byte(f.body)) //nolint:errcheck
	})
}

func newClient(t *testing.T, f *fakeUpstream, mutate ...func(*upstream.Config)) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	cfg := upstream.Config{
		BaseURL: srv.URL, What: "TestSource",
		Username: "u", Password: "p",
		Headers: map[string]string{"X-Custom": "v1"},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := upstream.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestGetJSONCarriesAuthHeadersAndQuery — Basic 인증·고정 헤더·쿼리가 실리는지,
// JSON이 해석되는지 확인합니다. Credential은 서버 → 데이터소스 방향으로만 갑니다.
func TestGetJSONCarriesAuthHeadersAndQuery(t *testing.T) {
	f := &fakeUpstream{body: `{"a": 42}`}
	c := newClient(t, f)

	var out struct {
		A int `json:"a"`
	}
	q := url.Values{"db": {"metrics"}}
	if err := c.GetJSON(context.Background(), "/v1/x", q, &out); err != nil {
		t.Fatal(err)
	}
	if out.A != 42 {
		t.Fatalf("응답 해석: %+v", out)
	}
	if !strings.HasPrefix(f.gotAuth, "Basic ") {
		t.Fatal("Basic 인증이 없습니다")
	}
	if f.gotHdr != "v1" || !strings.Contains(f.gotPath, "db=metrics") {
		t.Fatalf("헤더·쿼리 전달: %s %s", f.gotHdr, f.gotPath)
	}
}

// TestPostJSONSendsBody — 조회용 POST의 본문 직렬화입니다.
func TestPostJSONSendsBody(t *testing.T) {
	f := &fakeUpstream{body: `{}`}
	c := newClient(t, f)
	if err := c.PostJSON(context.Background(), "/search", map[string]any{"size": 5}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.gotBody, `"size":5`) {
		t.Fatalf("본문: %s", f.gotBody)
	}
}

func TestPostJSONQueryAndGetJSONBodyOnce(t *testing.T) {
	f := &fakeUpstream{body: `{}`}
	c := newClient(t, f)
	if err := c.PostJSONQuery(context.Background(), "/search", url.Values{"scroll": {"1m"}}, map[string]any{"size": 5}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.gotPath, "scroll=1m") {
		t.Fatalf("scroll query가 없습니다: %s", f.gotPath)
	}
	f.status.Store(http.StatusServiceUnavailable)
	if err := c.GetJSONBodyOnce(context.Background(), "/scroll", map[string]any{"scroll_id": "opaque"}, nil); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatalf("scroll 오류 분류: %v", err)
	}
	if !strings.Contains(f.gotBody, `"scroll_id":"opaque"`) {
		t.Fatalf("GET body가 없습니다: %s", f.gotBody)
	}
	if got := f.hits.Load(); got != 2 {
		t.Fatalf("scroll GET은 재시도 없이 1회여야 합니다: 총 %d회", got)
	}
}

// TestTransientErrorsRetryExactlyOnce — 503·429는 1회만 재시도합니다.
// 데이터소스가 이미 힘들 때 재시도 폭풍을 만들지 않습니다.
func TestTransientErrorsRetryExactlyOnce(t *testing.T) {
	for _, status := range []int{502, 503, 504, 429} {
		f := &fakeUpstream{}
		f.status.Store(int32(status))
		c := newClient(t, f)
		err := c.GetJSON(context.Background(), "/x", nil, nil)
		if !errors.Is(err, datasource.ErrUnavailable) {
			t.Fatalf("%d: 표준 오류가 아닙니다: %v", status, err)
		}
		if got := f.hits.Load(); got != 2 {
			t.Fatalf("%d: 호출 %d회 (원 요청 1 + 재시도 1이어야 합니다)", status, got)
		}
	}
}

// TestPlain500DoesNotRetry — 일시 오류가 아닌 5xx는 재시도하지 않습니다.
// 같은 요청을 다시 보내도 같은 이유로 실패할 뿐입니다.
func TestPlain500DoesNotRetry(t *testing.T) {
	f := &fakeUpstream{}
	f.status.Store(500)
	c := newClient(t, f)
	if err := c.GetJSON(context.Background(), "/x", nil, nil); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatalf("분류: %v", err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("500이 재시도되었습니다: %d회", f.hits.Load())
	}
}

// TestClientErrorIsBadQuery — 4xx는 우리 질의의 버그이지 데이터소스 장애가
// 아닙니다. 다르게 분류되어야 원인을 옳은 곳에서 찾습니다.
func TestClientErrorIsBadQuery(t *testing.T) {
	f := &fakeUpstream{}
	f.status.Store(400)
	c := newClient(t, f)
	err := c.GetJSON(context.Background(), "/x", nil, nil)
	if !errors.Is(err, upstream.ErrBadQuery) {
		t.Fatalf("4xx 분류: %v", err)
	}
	if errors.Is(err, datasource.ErrUnavailable) {
		t.Fatal("4xx가 장애로 분류되었습니다")
	}
	if f.hits.Load() != 1 {
		t.Fatal("4xx가 재시도되었습니다")
	}
}

// TestBrokenJSONIsClassified — 200이지만 JSON이 깨진 응답도 장애입니다.
func TestBrokenJSONIsClassified(t *testing.T) {
	f := &fakeUpstream{body: `{"broken":`}
	c := newClient(t, f)
	var out map[string]any
	if err := c.GetJSON(context.Background(), "/x", nil, &out); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatalf("깨진 JSON 분류: %v", err)
	}
}

// TestConnectionFailureIsUnavailable — 연결 자체가 안 되면 표준 장애이고,
// 에러 문자열에 주소가 없어야 합니다 (degraded 사유로 노출될 수 있습니다).
func TestConnectionFailureIsUnavailable(t *testing.T) {
	c, err := upstream.New(upstream.Config{
		BaseURL: "http://127.0.0.1:1", What: "TestSource", Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = c.GetJSON(context.Background(), "/x", nil, nil)
	if !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatalf("연결 실패 분류: %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("에러 문자열에 주소가 있습니다: %s", err)
	}
}

// TestCancelledContextStopsRetry — 사용자가 떠났으면 재시도하지 않습니다.
// 요청 취소는 데이터소스까지 전파됩니다. (README §11)
func TestCancelledContextStopsRetry(t *testing.T) {
	f := &fakeUpstream{}
	f.status.Store(503)
	c := newClient(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.GetJSON(ctx, "/x", nil, nil)
	if err == nil {
		t.Fatal("취소된 컨텍스트가 성공했습니다")
	}
	if f.hits.Load() > 1 {
		t.Fatalf("취소 후에도 재시도했습니다: %d회", f.hits.Load())
	}
}

// TestInvalidBaseURLIsRejectedAtConstruction — 잘못된 주소는 요청 시점이
// 아니라 생성 시점에 거절됩니다.
func TestInvalidBaseURLIsRejectedAtConstruction(t *testing.T) {
	for _, bad := range []string{"", "   ", "not-a-url", "//missing-scheme"} {
		if _, err := upstream.New(upstream.Config{BaseURL: bad, What: "X"}); err == nil {
			t.Fatalf("%q가 통과했습니다", bad)
		}
	}
}
