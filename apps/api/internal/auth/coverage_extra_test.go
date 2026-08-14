package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

/* ── mock IdP의 토큰 엔드포인트 ─────────────────────────────────────────── */

// TestMockTokenEndpointIssuesUsableTokens — 개발자가 curl로 받는 토큰이
// 실제 검증 경로를 통과하는지 확인합니다.
func TestMockTokenEndpointIssuesUsableTokens(t *testing.T) {
	idp, r := newIDPAndResolver(t, "k8s-dashboard")

	res, err := http.Post(idp.Issuer+"/token?sub=dev&roles=namespace.viewer:payments,dashboard.editor", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || out.TokenType != "Bearer" {
		t.Fatalf("토큰 응답: %+v %v", out, err)
	}

	sc, err := r.Resolve(request(out.AccessToken))
	if err != nil {
		t.Fatalf("발급 토큰이 검증을 통과하지 못했습니다: %v", err)
	}
	if c, _ := sc.Cluster("seoul"); len(c.Namespaces) != 1 || c.Namespaces[0] != "payments" {
		t.Fatalf("Scope: %+v", sc)
	}

	// sub 없는 요청은 기본 사용자를 씁니다.
	res2, err := http.Post(idp.Issuer+"/token", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res2.Body) //nolint:errcheck
	res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("기본 토큰 발급 실패: %d", res2.StatusCode)
	}
}

/* ── ES256과 키 형식 ───────────────────────────────────────────────────── */

func es256Token(t *testing.T, key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": kid})
	payload, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	rr, ss, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	rr.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// TestES256VerificationAndKeyAlgMismatch — EC 키의 정상 검증과,
// 키 종류·alg가 어긋난 토큰의 거절을 확인합니다.
func TestES256VerificationAndKeyAlgMismatch(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]any{"ec-1": &ecKey.PublicKey}
	keyOf := func(kid string) (any, bool) { k, ok := keys[kid]; return k, ok }

	claims := map[string]any{
		"iss": "https://idp", "sub": "kim", "aud": "app",
		"exp": testNow.Add(time.Hour).Unix(), "roles": []string{"platform.admin"},
	}
	token := es256Token(t, ecKey, "ec-1", claims)
	got, err := verifyJWT(token, keyOf, "https://idp", "app", "roles", time.Minute, testNow)
	if err != nil {
		t.Fatalf("정상 ES256이 거절되었습니다: %v", err)
	}
	if got.Subject != "kim" || len(got.Roles) != 1 {
		t.Fatalf("클레임: %+v", got)
	}

	// EC 키인데 RS256 헤더 — 키와 alg 불일치입니다.
	parts := strings.Split(token, ".")
	rsHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"ec-1"}`))
	if _, err := verifyJWT(rsHeader+"."+parts[1]+"."+parts[2], keyOf, "https://idp", "app", "roles", time.Minute, testNow); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("키-alg 불일치가 통과했습니다")
	}

	// 서명 길이가 64바이트가 아니면 거절합니다.
	short := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte("short"))
	if _, err := verifyJWT(short, keyOf, "https://idp", "app", "roles", time.Minute, testNow); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("짧은 ES256 서명이 통과했습니다")
	}

	// 지원하지 않는 키 형식(문자열)을 돌려주는 keyOf — 거절합니다.
	badKeyOf := func(string) (any, bool) { return "not-a-key", true }
	if _, err := verifyJWT(token, badKeyOf, "https://idp", "app", "roles", time.Minute, testNow); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("지원하지 않는 키 형식이 통과했습니다")
	}
}

/* ── JWK 해석 ──────────────────────────────────────────────────────────── */

// TestJWKPublicKeyParsing — RSA·EC 키 해석과 거절 조건입니다.
func TestJWKPublicKeyParsing(t *testing.T) {
	// RSA 정상 — mock IdP의 JWKS를 그대로 씁니다.
	idp, _ := newIDPAndResolver(t, "app")
	res, err := http.Get(idp.Issuer + "/jwks")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil || len(doc.Keys) != 1 {
		t.Fatalf("JWKS: %v %v", doc, err)
	}
	if _, err := doc.Keys[0].publicKey(); err != nil {
		t.Fatalf("RSA JWK 해석 실패: %v", err)
	}

	// EC 정상
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	x := make([]byte, 32)
	y := make([]byte, 32)
	ecKey.X.FillBytes(x)
	ecKey.Y.FillBytes(y)
	good := jwk{Kty: "EC", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(x),
		Y: base64.RawURLEncoding.EncodeToString(y)}
	if _, err := good.publicKey(); err != nil {
		t.Fatalf("EC JWK 해석 실패: %v", err)
	}

	for name, bad := range map[string]jwk{
		"암호화용 키":   {Kty: "RSA", Use: "enc", N: "AQAB", E: "AQAB"},
		"모르는 kty":  {Kty: "oct"},
		"모르는 곡선":   {Kty: "EC", Crv: "P-521", X: good.X, Y: good.Y},
		"깨진 RSA n": {Kty: "RSA", N: "!!!", E: "AQAB"},
		"깨진 RSA e": {Kty: "RSA", N: "AQAB", E: "!!!"},
		"깨진 EC x":  {Kty: "EC", Crv: "P-256", X: "!!!", Y: good.Y},
		"깨진 EC y":  {Kty: "EC", Crv: "P-256", X: good.X, Y: "!!!"},
	} {
		if _, err := bad.publicKey(); err == nil {
			t.Fatalf("%s가 통과했습니다", name)
		}
	}
}

/* ── discovery 실패 ────────────────────────────────────────────────────── */

// TestResolverStartupFailsFast — discovery가 깨져 있으면 서버를 띄우지 않습니다.
// 인증이 깨진 채 뜬 서버는 전부 401이 되어 장애처럼 보입니다.
func TestResolverStartupFailsFast(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(new(strings.Builder), nil))

	// 404 discovery
	notFound := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notFound.Close)
	if _, err := NewResolver(context.Background(), Config{IssuerURL: notFound.URL, ClusterID: "c"}, logger); err == nil {
		t.Fatal("404 discovery가 통과했습니다")
	}

	// issuer 불일치 문서 — 잘못된 곳을 보고 있는 것입니다.
	liar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": "https://someone-else", "jwks_uri": "http://x/jwks"}) //nolint:errcheck
	}))
	t.Cleanup(liar.Close)
	if _, err := NewResolver(context.Background(), Config{IssuerURL: liar.URL, ClusterID: "c"}, logger); err == nil {
		t.Fatal("issuer 불일치 문서가 통과했습니다")
	}

	// jwks_uri 없음
	noJWKS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": "http://" + r.Host}) //nolint:errcheck
	}))
	t.Cleanup(noJWKS.Close)
	if _, err := NewResolver(context.Background(), Config{IssuerURL: noJWKS.URL, ClusterID: "c"}, logger); err == nil {
		t.Fatal("jwks_uri 없는 문서가 통과했습니다")
	}

	// 발급자가 아예 없음
	if _, err := NewResolver(context.Background(), Config{ClusterID: "c"}, logger); err == nil {
		t.Fatal("빈 issuer가 통과했습니다")
	}
}

// TestUnknownKidThrottledWithinMinRefresh — 하한 안의 반복 재조회를 막습니다.
// 위조 kid를 대량으로 보내는 것만으로 JWKS를 두들기게 할 수 없습니다.
func TestUnknownKidThrottledWithinMinRefresh(t *testing.T) {
	idp, r := newIDPAndResolver(t, "k8s-dashboard")

	// 1차 회전 → 첫 모르는 kid가 재조회를 소비합니다.
	if err := idp.Rotate(); err != nil {
		t.Fatal(err)
	}
	token1, _ := idp.Token("kim", []string{"platform.admin"}, time.Hour)
	if _, err := r.Resolve(request(token1)); err != nil {
		t.Fatal(err)
	}

	// 2차 회전 — 하한(5분) 안이므로 재조회 없이 거절되어야 합니다.
	if err := idp.Rotate(); err != nil {
		t.Fatal(err)
	}
	token2, _ := idp.Token("kim", []string{"platform.admin"}, time.Hour)
	if _, err := r.Resolve(request(token2)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("하한 안의 재조회가 일어났습니다 — JWKS 두들김 방어가 없습니다")
	}
}

/* ── 클레임 해석 ───────────────────────────────────────────────────────── */

// TestParseAudienceForms — aud는 문자열·배열 둘 다 옵니다. 깨진 형식은 무시합니다.
func TestParseAudienceForms(t *testing.T) {
	if got := parseAudience(json.RawMessage(`"one"`)); len(got) != 1 || got[0] != "one" {
		t.Fatalf("문자열 aud: %v", got)
	}
	if got := parseAudience(json.RawMessage(`["a","b"]`)); len(got) != 2 {
		t.Fatalf("배열 aud: %v", got)
	}
	if got := parseAudience(json.RawMessage(`123`)); got != nil {
		t.Fatalf("숫자 aud: %v", got)
	}
	if got := parseAudience(nil); got != nil {
		t.Fatalf("빈 aud: %v", got)
	}
}

// TestPrincipalName — 감사 로그 표기 우선순위: username → email → sub.
func TestPrincipalName(t *testing.T) {
	if (Principal{Username: "u", Email: "e", Subject: "s"}).Name() != "u" {
		t.Fatal("username 우선이어야 합니다")
	}
	if (Principal{Email: "e", Subject: "s"}).Name() != "e" {
		t.Fatal("email이 다음이어야 합니다")
	}
	if (Principal{Subject: "s"}).Name() != "s" {
		t.Fatal("sub가 마지막이어야 합니다")
	}
}
