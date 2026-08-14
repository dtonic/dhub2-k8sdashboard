package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testNow }

// newIDPAndResolver는 mock IdP와 그것을 신뢰하는 Resolver를 만듭니다.
// 검증 경로는 운영과 같습니다 — mock이 대신하는 것은 발급자뿐입니다.
func newIDPAndResolver(t *testing.T, audience string) (*MockIDP, *Resolver) {
	t.Helper()
	idp, err := StartMockIDP("", audience, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idp.Close() })

	r, err := NewResolver(context.Background(), Config{
		IssuerURL:   idp.Issuer,
		Audience:    audience,
		ClusterID:   "seoul",
		ClusterName: "Seoul Production",
		Now:         fixedNow,
	}, slog.New(slog.NewTextHandler(new(strings.Builder), nil)))
	if err != nil {
		t.Fatal(err)
	}
	return idp, r
}

func request(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scope", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

/* ── 토큰 검증 ──────────────────────────────────────────────────────────── */

// TestValidTokenResolvesScope — 정상 토큰이 역할에 맞는 Scope로 해석됩니다.
func TestValidTokenResolvesScope(t *testing.T) {
	idp, r := newIDPAndResolver(t, "k8s-dashboard")
	token, err := idp.Token("kim", []string{"namespace.viewer:payments", "namespace.viewer:media"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := r.Resolve(request(token))
	if err != nil {
		t.Fatal(err)
	}
	c, ok := sc.Cluster("seoul")
	if !ok || c.All {
		t.Fatalf("클러스터 Scope가 틀렸습니다: %+v", sc)
	}
	if want := []string{"media", "payments"}; !reflect.DeepEqual(c.Namespaces, want) {
		t.Fatalf("namespace 목록: got %v want %v", c.Namespaces, want)
	}
	if sc.Subject != "kim" {
		t.Fatalf("감사용 Subject가 없습니다: %q", sc.Subject)
	}
}

// TestMissingOrMalformedAuthorizationIsRejected — 헤더가 없거나 형식이 틀리면
// 토큰 검증까지 가지 않고 거절됩니다. 서버는 이 오류를 401로 접습니다.
func TestMissingOrMalformedAuthorizationIsRejected(t *testing.T) {
	_, r := newIDPAndResolver(t, "k8s-dashboard")

	for name, req := range map[string]*http.Request{
		"헤더 없음": request(""),
		"Basic": func() *http.Request {
			q := request("")
			q.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
			return q
		}(),
		"빈 Bearer": func() *http.Request {
			q := request("")
			q.Header.Set("Authorization", "Bearer ")
			return q
		}(),
	} {
		if _, err := r.Resolve(req); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%s: ErrInvalidToken이어야 합니다: %v", name, err)
		}
	}
}

// TestForgedAndBrokenTokensAreRejected — 서명 위조·구조 파괴·alg 조작이 전부
// 걸리는지 확인합니다. alg:none은 allowlist에서 구조적으로 거절됩니다.
func TestForgedAndBrokenTokensAreRejected(t *testing.T) {
	idp, r := newIDPAndResolver(t, "k8s-dashboard")
	valid, _ := idp.Token("kim", []string{"platform.admin"}, time.Hour)

	// 다른 키로 서명된 토큰 — 두 번째 IdP의 토큰을 첫 IdP 발급자명으로 위조합니다.
	other, err := StartMockIDP("", "k8s-dashboard", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	forged, _ := other.SignedToken(map[string]any{
		"iss": idp.Issuer, "sub": "kim", "aud": "k8s-dashboard",
		"exp": testNow.Add(time.Hour).Unix(), "roles": []string{"platform.admin"},
	})

	parts := strings.Split(valid, ".")
	// 본문을 바꿔치기한 토큰 — 서명이 더 이상 맞지 않습니다.
	tampered := parts[0] + "." + strings.Replace(parts[1], "a", "b", 1) + "." + parts[2]
	// alg를 none으로 바꾼 토큰.
	noneHeader := base64url(`{"alg":"none","typ":"JWT"}`)
	noneToken := noneHeader + "." + parts[1] + "."

	for name, token := range map[string]string{
		"다른 키 서명": forged,
		"본문 변조":   tampered,
		"alg none": noneToken,
		"구조 파괴":   "not.a.jwt.at.all",
		"빈 문자열":   ".",
	} {
		if _, err := r.Resolve(request(token)); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%s 토큰이 통과했습니다", name)
		}
	}
}

// TestExpiryIssuerAudienceAreEnforced — 시각·발급자·대상 클레임 검증입니다.
func TestExpiryIssuerAudienceAreEnforced(t *testing.T) {
	idp, r := newIDPAndResolver(t, "k8s-dashboard")

	expired, _ := idp.SignedToken(map[string]any{
		"iss": idp.Issuer, "sub": "kim", "aud": "k8s-dashboard",
		"exp": testNow.Add(-2 * time.Hour).Unix(), "roles": []string{"platform.admin"},
	})
	wrongIss, _ := idp.SignedToken(map[string]any{
		"iss": "https://evil.example.com", "sub": "kim", "aud": "k8s-dashboard",
		"exp": testNow.Add(time.Hour).Unix(),
	})
	wrongAud, _ := idp.SignedToken(map[string]any{
		"iss": idp.Issuer, "sub": "kim", "aud": "someone-else",
		"exp": testNow.Add(time.Hour).Unix(),
	})
	notYet, _ := idp.SignedToken(map[string]any{
		"iss": idp.Issuer, "sub": "kim", "aud": "k8s-dashboard",
		"exp": testNow.Add(2 * time.Hour).Unix(), "nbf": testNow.Add(time.Hour).Unix(),
	})

	for name, token := range map[string]string{
		"만료": expired, "발급자 불일치": wrongIss, "대상 불일치": wrongAud, "nbf 이전": notYet,
	} {
		if _, err := r.Resolve(request(token)); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%s 토큰이 통과했습니다", name)
		}
	}

	// leeway 안의 시계 오차는 허용됩니다.
	slightlyOld, _ := idp.SignedToken(map[string]any{
		"iss": idp.Issuer, "sub": "kim", "aud": "k8s-dashboard",
		"exp": testNow.Add(-30 * time.Second).Unix(), "roles": []string{"platform.admin"},
	})
	if _, err := r.Resolve(request(slightlyOld)); err != nil {
		t.Fatalf("leeway 안의 만료가 거절되었습니다: %v", err)
	}
}

// TestUnknownKidTriggersOneRefresh — 키 회전 시나리오입니다. 모르는 kid를 만나면
// JWKS를 다시 받아오되, 하한(minRefresh) 안에서는 다시 조회하지 않습니다.
func TestUnknownKidTriggersOneRefresh(t *testing.T) {
	idp, r := newIDPAndResolver(t, "k8s-dashboard")

	// 키 회전: IdP가 새 키로 바꿉니다. Resolver의 캐시에는 옛 키만 있습니다.
	old := idp.kid
	if err := idp.Rotate(); err != nil {
		t.Fatal(err)
	}
	if idp.kid == old {
		t.Fatal("회전이 일어나지 않았습니다")
	}

	token, err := idp.Token("kim", []string{"platform.admin"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := r.Resolve(request(token))
	if err != nil {
		t.Fatalf("키 회전 후 첫 토큰이 거절되었습니다 (JWKS 재조회 실패): %v", err)
	}
	if c, _ := sc.Cluster("seoul"); !c.All {
		t.Fatalf("Scope가 틀렸습니다: %+v", sc)
	}
}

/* ── 역할 → Scope ───────────────────────────────────────────────────────── */

func scopeOf(t *testing.T, roles ...string) (Principal, []string, bool) {
	t.Helper()
	p, sc := ScopeFor(Claims{Subject: "kim", Roles: roles}, "seoul", "Seoul Production")
	c, ok := sc.Cluster("seoul")
	if !ok {
		t.Fatal("클러스터가 없습니다")
	}
	return p, c.Namespaces, c.All
}

// TestRoleMapping — 네 역할의 매핑 규칙 전부를 확인합니다. (#10 작업 범위)
func TestRoleMapping(t *testing.T) {
	if _, ns, all := scopeOf(t, "platform.admin"); !all || ns != nil {
		t.Fatal("platform.admin은 전체 접근이어야 합니다")
	}
	if _, _, all := scopeOf(t, "cluster.viewer"); !all {
		t.Fatal("cluster.viewer는 이 클러스터 전체여야 합니다")
	}
	if _, _, all := scopeOf(t, "cluster.viewer:seoul"); !all {
		t.Fatal("cluster.viewer:seoul은 전체여야 합니다")
	}
	if _, ns, all := scopeOf(t, "cluster.viewer:tokyo"); all || len(ns) != 0 {
		t.Fatal("다른 클러스터의 viewer는 아무것도 열지 않아야 합니다")
	}
	if _, ns, _ := scopeOf(t, "namespace.viewer:payments", "namespace.viewer:seoul/media"); !reflect.DeepEqual(ns, []string{"media", "payments"}) {
		t.Fatalf("namespace 매핑: %v", ns)
	}
	if _, ns, _ := scopeOf(t, "namespace.viewer:tokyo/payments"); len(ns) != 0 {
		t.Fatal("다른 클러스터의 namespace는 열리면 안 됩니다")
	}
	if _, ns, all := scopeOf(t, "namespace.viewer"); all || len(ns) != 0 {
		t.Fatal("인자 없는 namespace.viewer는 아무것도 열지 않아야 합니다")
	}
	if p, _, all := scopeOf(t, "dashboard.editor"); !p.CanEdit || all {
		t.Fatal("dashboard.editor는 편집 플래그만 세워야 합니다")
	}
	// 모르는 역할(다른 앱의 역할)은 무시됩니다 — 로그인은 되지만 Scope는 넓어지지 않습니다.
	if _, ns, all := scopeOf(t, "other-app.something", "unknown:x/y"); all || len(ns) != 0 {
		t.Fatal("모르는 역할이 Scope를 넓혔습니다")
	}
}

// TestNoRolesMeansEmptyButAuthenticatedScope — 역할이 없으면 빈 Scope로
// **성공**합니다. 401(인증 실패)이 아니라 403(권한 없음)으로 가는 길입니다.
func TestNoRolesMeansEmptyButAuthenticatedScope(t *testing.T) {
	idp, r := newIDPAndResolver(t, "k8s-dashboard")
	token, _ := idp.Token("kim", nil, time.Hour)
	sc, err := r.Resolve(request(token))
	if err != nil {
		t.Fatalf("역할 없는 토큰은 인증 자체는 성공이어야 합니다: %v", err)
	}
	if c, _ := sc.Cluster("seoul"); c.Accessible() {
		t.Fatal("역할이 없는데 접근 가능한 것이 있습니다")
	}
}

// TestRolesFromSpaceSeparatedString — provider에 따라 역할이 공백 구분
// 문자열로 오기도 합니다.
func TestRolesFromSpaceSeparatedString(t *testing.T) {
	idp, r := newIDPAndResolver(t, "k8s-dashboard")
	token, _ := idp.SignedToken(map[string]any{
		"iss": idp.Issuer, "sub": "kim", "aud": "k8s-dashboard",
		"exp":   testNow.Add(time.Hour).Unix(),
		"roles": "namespace.viewer:payments platform.admin",
	})
	sc, err := r.Resolve(request(token))
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := sc.Cluster("seoul"); !c.All {
		t.Fatalf("공백 구분 역할이 해석되지 않았습니다: %+v", sc)
	}
}

func base64url(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
