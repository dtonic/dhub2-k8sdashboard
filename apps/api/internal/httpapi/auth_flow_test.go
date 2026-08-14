package httpapi_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/auth"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

// authFixture는 mock IdP + 실제 OIDC Resolver로 서버를 세웁니다.
// 인증 경로 전체(토큰 검증 → 역할 → Scope → 핸들러의 강제)가 실코드입니다.
type authFixture struct {
	srv *httpapi.Server
	idp *auth.MockIDP
	log *bytes.Buffer
}

func newAuthFixture(t *testing.T) authFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	idp, err := auth.StartMockIDP("", "k8s-dashboard", func() time.Time { return testcluster.Now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idp.Close() })

	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	resolver, err := auth.NewResolver(ctx, auth.Config{
		IssuerURL:   idp.Issuer,
		Audience:    "k8s-dashboard",
		ClusterID:   testcluster.ClusterID,
		ClusterName: "Seoul Production",
		Now:         func() time.Time { return testcluster.Now },
	}, logger)
	if err != nil {
		t.Fatal(err)
	}

	store, _ := testcluster.NewStore(t, ctx)
	d := demo.New(store)
	srv := httpapi.NewServer(httpapi.Deps{
		Store: store, Metrics: d, Logs: d, Alerts: d, Topology: d,
		Resolver: resolver,
		Cache:    cache.NewTTL(time.Nanosecond),
		Logger:   logger,
		Now:      func() time.Time { return testcluster.Now },
	})
	return authFixture{srv: srv, idp: idp, log: logBuf}
}

func (f authFixture) get(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

// TestAuthnAndAuthzAreDistinguished — 완료 기준의 "401/403 구분"입니다. (#10)
//
//	토큰 없음/깨짐          → 401 (+ WWW-Authenticate)
//	유효 토큰 · 역할 없음    → 403 (인증은 됐고 권한이 없음)
//	유효 토큰 · 범위 밖 ns  → 403 (요청 변조로 데이터가 나가지 않음)
//	유효 토큰 · 범위 안     → 200
func TestAuthnAndAuthzAreDistinguished(t *testing.T) {
	f := newAuthFixture(t)

	// 토큰 없음 → 401
	rec := f.get(t, base+"/overview?range=1h", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("토큰 없음: got %d want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401에 WWW-Authenticate가 없습니다")
	}

	// 깨진 토큰 → 401
	if rec := f.get(t, base+"/overview?range=1h", "garbage.token.here"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("깨진 토큰: got %d want 401", rec.Code)
	}

	// 유효 토큰 · 역할 없음 → 403 (401이 아닙니다)
	noRoles, _ := f.idp.Token("no-role-user", nil, time.Hour)
	if rec := f.get(t, base+"/overview?range=1h", noRoles); rec.Code != http.StatusForbidden {
		t.Fatalf("역할 없는 사용자: got %d want 403", rec.Code)
	}

	// payments만 보는 사용자
	viewer, _ := f.idp.Token("kim", []string{"namespace.viewer:payments"}, time.Hour)

	// 범위 안 → 200
	if rec := f.get(t, base+"/namespaces/payments?range=1h", viewer); rec.Code != http.StatusOK {
		t.Fatalf("범위 안 요청: got %d want 200\n%s", rec.Code, rec.Body.String())
	}
	// 범위 밖 ns를 URL로 변조 → 403, 부분 데이터도 없음
	rec = f.get(t, base+"/namespaces/media?range=1h", viewer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("범위 밖 요청: got %d want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "media-worker") {
		t.Fatal("403 응답에 범위 밖 데이터가 실렸습니다")
	}
	// 로그 화면도 같은 규칙입니다
	if rec := f.get(t, base+"/logs?ns=media&range=1h", viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("범위 밖 로그: got %d want 403", rec.Code)
	}
}

// TestAuditLogRecordsWhoDidWhatWithWhatResult — 감사 로그에 사용자·범위·
// 요청(route+params)·결과가 남고, 토큰 원문은 절대 남지 않습니다. (#10 완료 기준)
func TestAuditLogRecordsWhoDidWhatWithWhatResult(t *testing.T) {
	f := newAuthFixture(t)
	viewer, _ := f.idp.Token("kim", []string{"namespace.viewer:payments"}, time.Hour)

	f.get(t, base+"/namespaces/payments?range=1h", viewer) // 200 allowed
	f.get(t, base+"/namespaces/media?range=1h", viewer)    // 403 forbidden
	f.get(t, base+"/overview?range=1h", "")                // 401 unauthorized

	logs := f.log.String()
	for _, want := range []string{
		"decision=allowed", "decision=forbidden", "decision=unauthorized",
		"user=kim", "route=" + base + "/namespaces/payments",
		"scope=" + testcluster.ClusterID + ":payments",
		"status=200", "status=403", "status=401",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("감사 로그에 %q가 없습니다:\n%s", want, logs)
		}
	}
	// 토큰 원문이 로그에 남으면 감사 로그 자체가 유출 경로가 됩니다.
	if strings.Contains(logs, viewer) || strings.Contains(logs, "Bearer ") {
		t.Fatal("감사 로그에 토큰이 남았습니다")
	}
}

// TestSensitiveQueryParamsAreMaskedInAudit — 이름에 token·key 등이 들어간
// 파라미터 값은 감사 로그에서 가려집니다. (#10 마스킹 정책)
func TestSensitiveQueryParamsAreMaskedInAudit(t *testing.T) {
	f := newAuthFixture(t)
	admin, _ := f.idp.Token("admin", []string{"platform.admin"}, time.Hour)

	f.get(t, base+"/overview?range=1h&access_token=super-secret-value", admin)

	logs := f.log.String()
	if strings.Contains(logs, "super-secret-value") {
		t.Fatalf("민감 파라미터 값이 감사 로그에 남았습니다:\n%s", logs)
	}
	if !strings.Contains(logs, "access_token=") {
		t.Fatal("파라미터 이름은 남아야 합니다 (무엇이 가려졌는지 보여야 합니다)")
	}
}

// TestScopeEndpointReflectsRoles — 로그인 직후 UI가 부르는 /scope가
// 사용자의 실제 접근 범위를 돌려주는지 확인합니다.
func TestScopeEndpointReflectsRoles(t *testing.T) {
	f := newAuthFixture(t)
	viewer, _ := f.idp.Token("kim", []string{"namespace.viewer:payments"}, time.Hour)

	rec := f.get(t, "/api/v1/scope", viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("scope: got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"payments"`) {
		t.Fatalf("허용 namespace가 없습니다: %s", body)
	}
	if strings.Contains(body, `"media"`) {
		t.Fatalf("허용하지 않은 namespace가 노출되었습니다: %s", body)
	}
}
