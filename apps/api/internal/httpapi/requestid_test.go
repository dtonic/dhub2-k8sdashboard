package httpapi_test

// 요청 상관관계 ID·에러 계약·취소 전파의 인수 증거입니다. (#5)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

/* ── 테스트 대역 ────────────────────────────────────────────────────────── */

// denyAll은 모든 요청의 인증을 거절하는 Resolver입니다. OIDC에서 토큰이 없는 상황입니다.
type denyAll struct{}

func (denyAll) Resolve(*http.Request) (scope.Scope, error) {
	return scope.Scope{}, errors.New("credential이 없습니다")
}

// panicMetrics는 Trends에서 패닉해 500 복구 경로를 검증합니다.
type panicMetrics struct{ datasource.Metrics }

func (panicMetrics) Trends(context.Context, datasource.Target, datasource.Window, []string) ([]contract.TrendPanel, error) {
	panic("테스트용 패닉")
}

// blockingLogs는 Search에서 ctx 취소를 기다립니다. 클라이언트 취소가
// 어댑터의 ctx.Done()까지 전파되는지 실제 HTTP 경로로 관찰합니다.
type blockingLogs struct {
	datasource.Unavailable
	entered  chan struct{}
	released chan struct{}
}

func (b *blockingLogs) Search(ctx context.Context, _ datasource.LogQuery) (datasource.LogPage, error) {
	close(b.entered)
	<-ctx.Done()
	close(b.released)
	return datasource.LogPage{}, ctx.Err()
}

func apiError(t *testing.T, body []byte) contract.APIError {
	t.Helper()
	var e contract.APIError
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("APIError 본문이 아닙니다: %v\n%s", err, body)
	}
	return e
}

/* ── 요청 ID 재사용·거절·생성 ───────────────────────────────────────────── */

func TestRequestIDReuseAndRejection(t *testing.T) {
	f := newFixture(t, func(d *httpapi.Deps) {
		d.NewRequestID = func() string { return "server-generated" }
	})

	// 안전한 인바운드 ID는 그대로 재사용합니다.
	safe := "trace-01.web_fe:req-9"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", safe)
	f.srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got != safe {
		t.Fatalf("안전한 인바운드 ID가 재사용되지 않았습니다: %q", got)
	}

	// 안전하지 않은 ID는 반사하지 않고 서버 소유 ID로 대체합니다.
	for name, bad := range map[string]string{
		"과다 길이":     strings.Repeat("a", 129),
		"공백":        "bad id",
		"CRLF 주입":   "abc\r\nX-Evil: 1",
		"비ASCII":    "요청아이디",
		"허용 외 문자":   "abc/def",
		"빈 값은 새 발급": "",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header["X-Request-ID"] = []string{bad}
		f.srv.ServeHTTP(rec, req)
		if got := rec.Header().Get("X-Request-ID"); got != "server-generated" {
			t.Errorf("%s: 안전하지 않은 ID %q가 대체되지 않았습니다: %q", name, bad, got)
		}
		if strings.Contains(rec.Body.String(), "X-Evil") {
			t.Errorf("%s: 주입된 값이 응답에 반사되었습니다", name)
		}
	}
}

func TestRequestIDDefaultGenerationIsHex128(t *testing.T) {
	// 기본 생성기(crypto/rand)는 소문자 hex 32자(128bit)입니다.
	f := newFixture(t)
	rec := f.get(t, "/healthz", nil)
	id := rec.Header().Get("X-Request-ID")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("생성된 ID 형식이 다릅니다: %q", id)
	}
	if second := f.get(t, "/healthz", nil).Header().Get("X-Request-ID"); second == id {
		t.Fatalf("요청마다 새 ID여야 합니다: %q", id)
	}
}

/* ── 에러 계약 — 헤더·본문 requestId 일치 ───────────────────────────────── */

// TestErrorContractCarriesRequestID — 대표 에러 클래스 전부에서
// {code,message,requestId} 본문과 X-Request-ID 헤더가 같은 값이어야 합니다.
func TestErrorContractCarriesRequestID(t *testing.T) {
	cases := []struct {
		name   string
		opts   []func(*httpapi.Deps)
		method string
		path   string
		status int
		code   string
	}{
		{"400 invalid_range", nil, http.MethodGet, base + "/overview?range=bogus", http.StatusBadRequest, "invalid_range"},
		{"401 unauthorized", []func(*httpapi.Deps){func(d *httpapi.Deps) { d.Resolver = denyAll{} }},
			http.MethodGet, base + "/overview?range=1h", http.StatusUnauthorized, "unauthorized"},
		{"403 forbidden", nil, http.MethodGet, "/api/v1/clusters/prod-frankfurt/overview?range=1h", http.StatusForbidden, "forbidden"},
		{"404 not_found", nil, http.MethodGet, "/api/v1/no-such-route", http.StatusNotFound, "not_found"},
		{"405 method_not_allowed", nil, http.MethodPost, base + "/overview?range=1h", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"405 HEAD method_not_allowed", nil, http.MethodHead, "/healthz", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"500 internal", []func(*httpapi.Deps){func(d *httpapi.Deps) { d.Metrics = panicMetrics{Metrics: d.Metrics} }},
			http.MethodGet, base + "/overview?range=1h", http.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.opts...)
			rec := httptest.NewRecorder()
			f.srv.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.status {
				t.Fatalf("status=%d, want %d\n%s", rec.Code, tc.status, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("에러가 JSON이 아닙니다: %q", ct)
			}
			e := apiError(t, rec.Body.Bytes())
			if e.Code != tc.code {
				t.Errorf("code=%q, want %q", e.Code, tc.code)
			}
			header := rec.Header().Get("X-Request-ID")
			if header == "" || e.RequestID != header {
				t.Errorf("헤더(%q)와 본문(%q)의 requestId가 다릅니다", header, e.RequestID)
			}
			if tc.status == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != http.MethodGet {
				t.Errorf("405의 Allow 헤더가 없습니다: %q", rec.Header().Get("Allow"))
			}
		})
	}
}

/* ── 운영 경로 — 인증 없이 · 요청 ID는 그대로 ───────────────────────────── */

func TestOperationalPathsSkipAuthButKeepRequestID(t *testing.T) {
	// 모든 인증을 거절하는 Resolver 아래에서도 probe·버전은 살아 있어야 합니다.
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Resolver = denyAll{}
		d.Version = contract.VersionInfo{Version: "v9.9.9", Commit: "abc1234", BuildDate: "2026-08-14"}
	})

	for _, path := range []string{"/healthz", "/readyz", "/version"} {
		rec := f.get(t, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status=%d, want 200 (인증 없이)", path, rec.Code)
		}
		if rec.Header().Get("X-Request-ID") == "" {
			t.Errorf("%s: X-Request-ID가 없습니다", path)
		}
	}

	// /api/v1/*의 인증 경계는 그대로입니다.
	if rec := f.get(t, "/api/v1/scope", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1/scope: status=%d, want 401", rec.Code)
	}

	var v contract.VersionInfo
	if rec := f.get(t, "/version", &v); rec.Code == http.StatusOK && v.Version != "v9.9.9" {
		t.Fatalf("버전 배선이 틀렸습니다: %+v", v)
	}
}

/* ── 구조화 로그 상관 ───────────────────────────────────────────────────── */

func TestAuditLogCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters/prod-frankfurt/overview?range=1h", nil)
	req.Header.Set("X-Request-ID", "corr-42")
	f.srv.ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-ID") != "corr-42" {
		t.Fatalf("헤더 ID가 다릅니다: %q", rec.Header().Get("X-Request-ID"))
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry["msg"] == "audit" && entry["requestId"] == "corr-42" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit 로그에 requestId=corr-42가 없습니다:\n%s", buf.String())
	}
}

func TestPanicErrorAndAuditLogsCarryRequestID(t *testing.T) {
	var buf bytes.Buffer
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
		d.Metrics = panicMetrics{Metrics: d.Metrics}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, base+"/overview?range=1h", nil)
	req.Header.Set("X-Request-ID", "panic-42")
	f.srv.ServeHTTP(rec, req)

	wantMessages := map[string]bool{"패닉": false, "audit": false}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["requestId"] == "panic-42" {
			if msg, ok := entry["msg"].(string); ok {
				if _, wanted := wantMessages[msg]; wanted {
					wantMessages[msg] = true
				}
			}
		}
	}
	for msg, found := range wantMessages {
		if !found {
			t.Errorf("%s 로그에 requestId=panic-42가 없습니다:\n%s", msg, buf.String())
		}
	}
}

/* ── 취소 전파 — 실제 HTTP 경로 ─────────────────────────────────────────── */

// TestClientCancellationReachesAdapter — httptest.Server를 통해 들어온 요청을
// 클라이언트가 중간에 취소하면, 블로킹 중인 어댑터의 ctx.Done()이 제한 시간 안에
// 닫혀야 합니다. 취소 전파가 실제 네트워크 경로에서 동작한다는 증거입니다.
func TestClientCancellationReachesAdapter(t *testing.T) {
	bl := &blockingLogs{entered: make(chan struct{}), released: make(chan struct{})}
	responseCache := cache.NewTTL(time.Minute)
	var logs bytes.Buffer
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Logs = bl
		d.Cache = responseCache
		d.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	})

	ts := httptest.NewServer(f.srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+base+"/logs?range=1h", nil)
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		errCh <- err
	}()

	select {
	case <-bl.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("어댑터에 요청이 도달하지 않았습니다")
	}
	cancel()

	select {
	case <-bl.released:
		// 취소가 어댑터까지 전파되었습니다.
	case <-time.After(2 * time.Second):
		t.Fatal("클라이언트 취소가 2초 안에 어댑터 ctx.Done()에 도달하지 않았습니다")
	}
	if err := <-errCh; err == nil {
		t.Fatal("취소된 요청이 성공으로 끝났습니다")
	}
	// Close waits for the server-side handler, making cache/log assertions race-free.
	ts.Close()
	if responseCache.Len() != 0 {
		t.Fatalf("취소 중 생성된 degraded 응답이 캐시되었습니다: len=%d", responseCache.Len())
	}
	if strings.Contains(logs.String(), "요청 처리 실패") || strings.Contains(logs.String(), `"code":"internal"`) {
		t.Fatalf("클라이언트 취소가 내부 오류로 기록되었습니다:\n%s", logs.String())
	}
}

/* ── 벤치마크 — 미들웨어 경로 오버헤드 ──────────────────────────────────── */

// BenchmarkOperationalProbe — 요청 ID 미들웨어 + 404/405 매칭 + 핸들러 전체 경로입니다.
// 고정 임계값은 두지 않습니다. ns/op·B/op·allocs 추이만 봅니다:
//
//	go test ./internal/httpapi -bench BenchmarkOperationalProbe -benchmem -count=5 -run ^$
func BenchmarkOperationalProbe(b *testing.B) {
	srv := httpapi.NewServer(httpapi.Deps{
		Resolver: scope.Static{},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			b.Fatal(rec.Code)
		}
	}
}
