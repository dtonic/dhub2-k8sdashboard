//go:build e2efixture

package e2efixture

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const indexMarker = "e2e-fixture-index"

func writeDist(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	index := "<!doctype html><html><head><title>K8s Dashboard</title></head><body>" + indexMarker + "</body></html>"
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log('dashboard')"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func startFixture(t *testing.T, cfg Config, logger *slog.Logger) *Fixture {
	t.Helper()
	if cfg.DistDir == "" {
		cfg.DistDir = writeDist(t)
	}
	f, err := Start(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("픽스처 기동 실패: %v", err)
	}
	t.Cleanup(f.Close)
	return f
}

func fetchToken(t *testing.T, base, sub, roles string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"sub": sub, "roles": strings.Split(roles, ",")})
	res, err := http.Post(base+"/e2e/token", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("토큰 발급 실패: %v", err)
	}
	defer res.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.AccessToken == "" {
		t.Fatalf("토큰 응답이 비었습니다 (err=%v)", err)
	}
	return body.AccessToken
}

func getJSON(t *testing.T, url, token string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("요청 실패 %s: %v", url, err)
	}
	defer res.Body.Close()
	if out != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("응답 해석 실패 %s: %v", url, err)
		}
	}
	return res.StatusCode
}

/* ── 기동 fail-fast ────────────────────────────────────────────────────── */

func TestStartFailsFast(t *testing.T) {
	dist := writeDist(t)
	cases := []struct {
		name string
		cfg  Config
	}{
		{"unknown outage", Config{DistDir: dist, Outages: []string{"redis"}}},
		{"unknown scenario", Config{DistDir: dist, Scenarios: []string{"nope"}}},
		{"missing dist", Config{DistDir: filepath.Join(dist, "does-not-exist")}},
		{"empty dist", Config{}},
		{"non-loopback addr", Config{DistDir: dist, Addr: "0.0.0.0:0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Start(context.Background(), tc.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil {
				f.Close()
				t.Fatal("잘못된 설정이 기동을 통과했습니다 — fail-fast여야 합니다")
			}
		})
	}
}

/* ── 단일 오리진 · 인증 경계 ───────────────────────────────────────────── */

func TestSingleOriginServesBundleAndAPI(t *testing.T) {
	f := startFixture(t, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// SPA: 루트와 클라이언트 라우트 모두 index.html이 나옵니다.
	for _, p := range []string{"/", "/namespaces/payments", "/pods/payments-api-7f-bbb"} {
		res, err := http.Get(f.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !strings.Contains(string(b), indexMarker) {
			t.Fatalf("SPA fallback 실패: %s → %d", p, res.StatusCode)
		}
	}
	// 정적 자산은 실제 파일이 나옵니다.
	if code := getJSON(t, f.URL+"/assets/app.js", "", nil); code != 200 {
		t.Fatalf("정적 자산 실패: %d", code)
	}
	// 경로 탈출은 거절(400)되거나 index로 접힙니다 — dist 밖 파일이 나가면 안 됩니다.
	res, err := http.Get(f.URL + "/..%2f..%2fetc%2fpasswd")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest && !strings.Contains(string(b), indexMarker) {
		t.Fatalf("경로 탈출 요청이 차단되지 않았습니다: %d %q", res.StatusCode, string(b))
	}

	// readiness는 즉시 통과합니다 — informer 동기화가 끝난 뒤에만 리스너가 열립니다.
	if code := getJSON(t, f.URL+"/readyz", "", nil); code != 200 {
		t.Fatalf("readyz 실패: %d", code)
	}

	// 인증 경계: 토큰 없는 API는 401, 유효 토큰은 200 + 서버 강제 Scope입니다.
	if code := getJSON(t, f.URL+"/api/v1/scope", "", nil); code != 401 {
		t.Fatalf("무인증 API가 %d를 반환했습니다 (401이어야 합니다)", code)
	}
	admin := fetchToken(t, f.URL, "admin", "platform.admin")
	var sc struct {
		Clusters []struct {
			ID         string `json:"id"`
			Namespaces any    `json:"namespaces"`
		} `json:"clusters"`
	}
	if code := getJSON(t, f.URL+"/api/v1/scope", admin, &sc); code != 200 {
		t.Fatalf("admin scope 실패: %d", code)
	}
	if len(sc.Clusters) != 1 || sc.Clusters[0].ID != "prod-seoul" {
		t.Fatalf("scope 응답이 예상과 다릅니다: %+v", sc)
	}

	viewer := fetchToken(t, f.URL, "media-viewer", "namespace.viewer:media")
	// namespace 격리: 허용 밖 namespace 조회는 403입니다.
	if code := getJSON(t, f.URL+"/api/v1/clusters/prod-seoul/namespaces/payments?range=1h", viewer, nil); code != 403 {
		t.Fatalf("scope 밖 namespace가 %d를 반환했습니다 (403이어야 합니다)", code)
	}
	if code := getJSON(t, f.URL+"/api/v1/clusters/prod-seoul/namespaces/media?range=1h", viewer, nil); code != 200 {
		t.Fatalf("허용 namespace가 %d를 반환했습니다", code)
	}
}

func TestTokenEndpointIsPostOnly(t *testing.T) {
	f := startFixture(t, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := http.Get(f.URL + "/e2e/token")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /e2e/token이 %d를 반환했습니다 (405여야 합니다)", res.StatusCode)
	}
	// 신원·역할을 쿼리스트링으로 보내는 경로는 거절합니다 — 접근 로그에 남습니다.
	res, err = http.Post(f.URL+"/e2e/token?sub=admin&roles=platform.admin", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("쿼리 파라미터 발급이 %d를 반환했습니다 (400이어야 합니다)", res.StatusCode)
	}
}

func TestTokenEndpointRejectsUnapprovedAndMalformedClaims(t *testing.T) {
	f := startFixture(t, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cases := []struct {
		name, contentType, body string
		want                    int
	}{
		{"arbitrary subject", "application/json", `{"sub":"attacker","roles":["platform.admin"]}`, http.StatusForbidden},
		{"arbitrary role", "application/json", `{"sub":"admin","roles":["namespace.viewer:media"]}`, http.StatusForbidden},
		{"unknown field", "application/json", `{"sub":"admin","roles":["platform.admin"],"token":"raw"}`, http.StatusBadRequest},
		{"multiple values", "application/json", `{"sub":"admin","roles":["platform.admin"]}{}`, http.StatusBadRequest},
		{"wrong content type", "text/plain", `{"sub":"admin","roles":["platform.admin"]}`, http.StatusUnsupportedMediaType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := http.Post(f.URL+"/e2e/token", tc.contentType, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d", res.StatusCode, tc.want)
			}
		})
	}
}

func TestStaticMethodsAndSymlinksAreSafe(t *testing.T) {
	dist := writeDist(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("must-not-leak"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dist, "assets", "outside.txt")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink permission unavailable: %v", err)
		}
		t.Fatal(err)
	}
	f := startFixture(t, Config{DistDir: dist}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(method, f.URL+"/assets/app.js", nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s status=%d", method, res.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodHead, f.URL+"/assets/app.js", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status=%d", res.StatusCode)
	}
	res, err = http.Get(f.URL + "/assets/outside.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || strings.Contains(string(body), "must-not-leak") {
		t.Fatalf("symlink escaped dist: status=%d body=%q", res.StatusCode, body)
	}
	f.Close()
	f.Close()
}

// 토큰 원문·access_token이 픽스처 로그에 남으면 안 됩니다.
func TestNoTokenInLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := startFixture(t, Config{}, logger)

	token := fetchToken(t, f.URL, "admin", "platform.admin")
	if code := getJSON(t, f.URL+"/api/v1/clusters/prod-seoul/overview?range=1h", token, nil); code != 200 {
		t.Fatalf("overview 실패: %d", code)
	}
	// 실패 경로(무효 토큰)도 원문을 남기지 않아야 합니다.
	getJSON(t, f.URL+"/api/v1/scope", "eyJinvalid.invalid.invalid", nil)

	out := buf.String()
	for _, banned := range []string{"access_token", "eyJ", token} {
		if strings.Contains(out, banned) {
			t.Fatalf("로그에 토큰 흔적이 남았습니다: %q", banned)
		}
	}
}

/* ── 데이터소스 중단 독립성 ────────────────────────────────────────────── */

type section struct {
	Status string `json:"status"`
	Source string `json:"source"`
}

func TestOutagesDegradeOnlyRelevantSections(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := []struct {
		outage string
		check  func(t *testing.T, f *Fixture, token string)
	}{
		{OutageGreptime, func(t *testing.T, f *Fixture, token string) {
			var out struct {
				Trends    section `json:"trends"`
				Pods      section `json:"pods"`
				Unhealthy section `json:"unhealthy"`
				Alerts    section `json:"alerts"`
			}
			if code := getJSON(t, f.URL+"/api/v1/clusters/prod-seoul/overview?range=1h", token, &out); code != 200 {
				t.Fatalf("overview %d", code)
			}
			if out.Trends.Status != "degraded" || out.Trends.Source != "greptimedb" {
				t.Fatalf("메트릭 섹션이 greptimedb degraded가 아닙니다: %+v", out.Trends)
			}
			if out.Pods.Status != "ok" || out.Unhealthy.Status != "ok" || out.Alerts.Status != "ok" {
				t.Fatalf("무관한 섹션까지 강등되었습니다: %+v %+v %+v", out.Pods, out.Unhealthy, out.Alerts)
			}
		}},
		{OutageQuickwit, func(t *testing.T, f *Fixture, token string) {
			var out struct {
				Lines  section `json:"lines"`
				Events section `json:"events"`
			}
			if code := getJSON(t, f.URL+"/api/v1/clusters/prod-seoul/logs?range=1h", token, &out); code != 200 {
				t.Fatalf("logs %d", code)
			}
			if out.Lines.Status != "degraded" || out.Lines.Source != "quickwit" {
				t.Fatalf("로그 섹션이 quickwit degraded가 아닙니다: %+v", out.Lines)
			}
			if out.Events.Status != "ok" {
				t.Fatalf("Kubernetes 이벤트 섹션까지 강등되었습니다: %+v", out.Events)
			}
		}},
		{OutageAlerts, func(t *testing.T, f *Fixture, token string) {
			var out struct {
				Alerts    section `json:"alerts"`
				Pods      section `json:"pods"`
				Trends    section `json:"trends"`
				Unhealthy section `json:"unhealthy"`
			}
			if code := getJSON(t, f.URL+"/api/v1/clusters/prod-seoul/overview?range=1h", token, &out); code != 200 {
				t.Fatalf("overview %d", code)
			}
			if out.Alerts.Status != "degraded" || out.Alerts.Source != "alertmanager" {
				t.Fatalf("알림 섹션이 alertmanager degraded가 아닙니다: %+v", out.Alerts)
			}
			if out.Pods.Status != "ok" || out.Trends.Status != "ok" || out.Unhealthy.Status != "ok" {
				t.Fatalf("무관한 섹션까지 강등되었습니다: %+v %+v %+v", out.Pods, out.Trends, out.Unhealthy)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.outage, func(t *testing.T) {
			f := startFixture(t, Config{Outages: []string{tc.outage}}, logger)
			token := fetchToken(t, f.URL, "admin", "platform.admin")
			tc.check(t, f, token)
		})
	}
}

/* ── 정리 ──────────────────────────────────────────────────────────────── */

func TestCloseReleasesListener(t *testing.T) {
	f := startFixture(t, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	addr := strings.TrimPrefix(f.URL, "http://")
	f.Close()

	// 포트가 실제로 반납되어야 다음 픽스처가 같은 포트를 쓸 수 있습니다.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Close 후에도 포트가 잡혀 있습니다: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestShortTokenTTLIsConsumedByOneIDToken(t *testing.T) {
	idp, err := startCodeIDP()
	if err != nil {
		t.Fatal(err)
	}
	defer idp.Close()
	idp.nextTokenTTL.Store(int64(4 * time.Second))
	lifetime := func(raw string) int64 {
		parts := strings.Split(raw, ".")
		if len(parts) != 3 {
			t.Fatalf("JWT parts=%d", len(parts))
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims struct {
			ExpiresAt int64 `json:"exp"`
			IssuedAt  int64 `json:"iat"`
		}
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		return claims.ExpiresAt - claims.IssuedAt
	}
	first, err := idp.jwt("nonce")
	if err != nil {
		t.Fatal(err)
	}
	second, err := idp.jwt("")
	if err != nil {
		t.Fatal(err)
	}
	if got := lifetime(first); got != 4 {
		t.Fatalf("first lifetime=%ds", got)
	}
	if got := lifetime(second); got != 300 {
		t.Fatalf("second lifetime=%ds", got)
	}
}
