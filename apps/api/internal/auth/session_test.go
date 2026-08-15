package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

func testSessionFlow(t *testing.T) *sessionFlow {
	t.Helper()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return &sessionFlow{cfg: Config{IssuerURL: "https://issuer.example", Audience: "dashboard"}, aead: aead}
}

func TestMalformedAuthorizationNeverFallsBackToCookie(t *testing.T) {
	r := &Resolver{flow: &sessionFlow{}}
	req := httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/scope", nil)
	req.Header.Set("Authorization", "Basic abc")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("s", 43)})
	if _, err := r.Resolve(req); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("malformed Authorization fallback: %v", err)
	}
}

func TestSafeReturnToRejectsEncodedAndAbsoluteBypasses(t *testing.T) {
	valid := []string{"", "/", "/namespaces?range=1h", "/namespaces/media", "/workloads/deployment/api", "/pods/api-1", "/topology", "/logs", "/alerts", "/dashboards/ops", "/dashboard-builder/draft-1"}
	invalid := []string{"https://evil.example", "//evil.example", "/%2f%2fevil.example", "/%252f%252fevil.example", "/\\evil", "/x\r\nLocation:x", "/api/v1/auth/callback", "/metrics", "/unknown", "/namespaces/a/extra", "/logs/extra", "/pods/%61pi"}
	for _, value := range valid {
		if _, ok := safeReturnTo(value); !ok {
			t.Errorf("valid returnTo rejected: %q", value)
		}
	}
	for _, value := range invalid {
		if _, ok := safeReturnTo(value); ok {
			t.Errorf("unsafe returnTo accepted: %q", value)
		}
	}
}

func TestSessionAEADBindsKindKeyVersionAndProvider(t *testing.T) {
	f := testSessionFlow(t)
	aad := f.recordAAD("session", "dashboard:auth:session:key-a", 1)
	one, err := f.seal("refresh-token", aad)
	if err != nil {
		t.Fatal(err)
	}
	two, err := f.seal("refresh-token", aad)
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Fatal("GCM nonce was reused")
	}
	if got, err := f.open(one, aad, 100); err != nil || got != "refresh-token" {
		t.Fatalf("open: %q %v", got, err)
	}
	for _, wrong := range [][]byte{f.recordAAD("tx", "dashboard:auth:session:key-a", 1), f.recordAAD("session", "dashboard:auth:session:key-b", 1), f.recordAAD("session", "dashboard:auth:session:key-a", 2)} {
		if _, err := f.open(one, wrong, 100); err == nil {
			t.Fatal("ciphertext accepted under different record boundary")
		}
	}
	f.cfg.IssuerURL = "https://other-issuer.example"
	if _, err := f.open(one, f.recordAAD("session", "dashboard:auth:session:key-a", 1), 100); err == nil {
		t.Fatal("ciphertext accepted for another issuer")
	}
	f.cfg.IssuerURL = "https://issuer.example"
	f.cfg.Audience = "other-audience"
	if _, err := f.open(one, f.recordAAD("session", "dashboard:auth:session:key-a", 1), 100); err == nil {
		t.Fatal("ciphertext accepted for another audience")
	}
	if _, err := f.open("v2."+strings.TrimPrefix(one, "v1."), aad, 100); err == nil {
		t.Fatal("unknown key version accepted")
	}
	corruptBytes := []byte(one)
	at := len(corruptBytes) / 2
	if corruptBytes[at] == 'A' {
		corruptBytes[at] = 'B'
	} else {
		corruptBytes[at] = 'A'
	}
	corrupt := string(corruptBytes)
	if _, err := f.open(corrupt, aad, 100); err == nil || strings.Contains(err.Error(), one) {
		t.Fatal("corrupt ciphertext was accepted or echoed")
	}
	if _, err := f.open(one, aad, 4); err == nil {
		t.Fatal("plaintext bound was not enforced")
	}
	if _, err := f.open("v1."+base64.RawURLEncoding.EncodeToString(make([]byte, maxTokenBody+100)), aad, maxTokenBody); err == nil {
		t.Fatal("ciphertext bound was not enforced")
	}
}

func TestHostCookieSecurityAttributes(t *testing.T) {
	w := httptest.NewRecorder()
	setCookie(w, sessionCookie, strings.Repeat("a", 43), 60)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%d", len(cookies))
	}
	c := cookies[0]
	if c.Name != "__Host-k8s-dashboard" || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != 2 {
		t.Fatalf("unsafe cookie: %#v", c)
	}
}

func TestAuthErrorsAreBoundedJSONWithMatchingRequestID(t *testing.T) {
	f := &sessionFlow{}
	req := httptest.NewRequest(http.MethodPost, "https://dashboard.example/api/v1/auth/logout", nil)
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-ID", "auth-request-1")
	if _, _, ok := f.authorizeMutation(rec, req); ok {
		t.Fatal("missing cookie was authorized")
	}
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("status/content-type: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "unauthorized" || body["requestId"] != rec.Header().Get("X-Request-ID") || len(rec.Body.Bytes()) > 512 {
		t.Fatalf("error contract: %#v", body)
	}
}

func TestShortTokenRefreshIsScheduledOnceInFuture(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	expires := now.Add(30 * time.Second)
	refresh := sessionRefreshAt(now, expires, 2*time.Minute)
	if !refresh.After(now) || !refresh.Before(expires) || !refresh.Equal(now.Add(15*time.Second)) {
		t.Fatalf("refresh=%s", refresh)
	}
}

func TestShortTokenRefreshKeepsSubsecondWirePrecision(t *testing.T) {
	now := time.Unix(1_800_000_000, 750_000_000)
	expires := time.Unix(1_800_000_001, 0)
	wire := sessionRefreshAt(now, expires, 2*time.Minute).Format(time.RFC3339Nano)
	refresh, err := time.Parse(time.RFC3339Nano, wire)
	if err != nil || !refresh.After(now) || !refresh.Before(expires) {
		t.Fatalf("wire refresh=%q parsed=%s err=%v", wire, refresh, err)
	}
}

func TestRefreshExpiryCapsLargePositiveWithoutOverflow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	absolute := now.Add(8 * time.Hour)
	if got := refreshExpiry(now, absolute, int64(^uint64(0)>>1)); !got.Equal(absolute) {
		t.Fatalf("large provider lifetime changed configured cap: %s", got)
	}
	if got := refreshExpiry(now, absolute, 60); !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("short provider lifetime not applied: %s", got)
	}
}

func TestSessionPublicOriginRejectsUserInfo(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	cfg := Config{
		IssuerURL: "https://issuer.example",
		Audience:  "dashboard",
		Session: SessionConfig{
			PublicOrigin:  "https://user@dashboard.example",
			RedirectURI:   "https://user@dashboard.example/api/v1/auth/callback",
			ClientID:      "dashboard",
			EncryptionKey: key,
			RedisAddr:     "127.0.0.1:1",
		},
	}
	doc := discoveryDoc{AuthorizationEndpoint: "https://issuer.example/authorize", TokenEndpoint: "https://issuer.example/token"}
	if _, err := newSessionFlow(cfg, doc, http.DefaultClient, nil); err == nil || !strings.Contains(err.Error(), "HTTPS origin") {
		t.Fatalf("userinfo origin accepted: %v", err)
	}
}

func TestNewSessionFlowDefaultsAndFailClosedBoundaries(t *testing.T) {
	redisServer := miniredis.RunT(t)
	key := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	base := Config{
		IssuerURL: "https://issuer.example/tenant",
		Audience:  "dashboard-api",
		Session: SessionConfig{
			PublicOrigin:  "https://dashboard.example",
			RedirectURI:   "https://dashboard.example/api/v1/auth/callback",
			ClientID:      "dashboard-web",
			EncryptionKey: key,
			RedisAddr:     redisServer.Addr(),
		},
	}
	doc := discoveryDoc{AuthorizationEndpoint: "https://issuer.example/authorize?tenant=ops", TokenEndpoint: "https://issuer.example/token"}
	flow, err := newSessionFlow(base, doc, http.DefaultClient, nil)
	if err != nil {
		t.Fatalf("default session flow: %v", err)
	}
	defer flow.redis.Close()
	if flow.sc.TransactionTTL != 5*time.Minute || flow.sc.IdleTTL != 30*time.Minute || flow.sc.AbsoluteTTL != 8*time.Hour || flow.sc.RedisTimeout != 250*time.Millisecond || flow.sc.RefreshSkew != 2*time.Minute || flow.sc.MaxSessions != DefaultMaxSessions {
		t.Fatalf("session defaults drifted: %+v", flow.sc)
	}

	tests := map[string]func(*Config, *discoveryDoc){
		"unsafe ttl":        func(c *Config, _ *discoveryDoc) { c.Session.IdleTTL = MaxSessionIdleTTL + time.Second },
		"invalid origin":    func(c *Config, _ *discoveryDoc) { c.Session.PublicOrigin = "https://dashboard.example/path" },
		"redirect mismatch": func(c *Config, _ *discoveryDoc) { c.Session.RedirectURI = "https://dashboard.example/other" },
		"missing client":    func(c *Config, _ *discoveryDoc) { c.Session.ClientID = "" },
		"invalid key":       func(c *Config, _ *discoveryDoc) { c.Session.EncryptionKey = "short" },
		"unsafe endpoint":   func(_ *Config, d *discoveryDoc) { d.TokenEndpoint = "http://issuer.example/token" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg, candidate := base, doc
			mutate(&cfg, &candidate)
			if got, err := newSessionFlow(cfg, candidate, http.DefaultClient, nil); err == nil || got != nil {
				if got != nil {
					_ = got.redis.Close()
				}
				t.Fatalf("invalid session flow passed: %v", err)
			}
		})
	}

	unreachable := base
	unreachable.Session.RedisAddr = "127.0.0.1:1"
	unreachable.Session.RedisTimeout = 10 * time.Millisecond
	if got, err := newSessionFlow(unreachable, doc, http.DefaultClient, nil); err == nil || got != nil {
		t.Fatalf("unreachable Redis passed: %v", err)
	}
}

func TestRegisterAuthRoutesAndSmallTimeHelpers(t *testing.T) {
	mux := http.NewServeMux()
	(&Resolver{}).RegisterAuthRoutes(mux)
	flow := testSessionFlow(t)
	(&Resolver{flow: flow}).RegisterAuthRoutes(mux)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/auth/login"},
		{http.MethodGet, "/api/v1/auth/callback"},
		{http.MethodGet, "/api/v1/auth/session"},
		{http.MethodPost, "/api/v1/auth/refresh"},
		{http.MethodPost, "/api/v1/auth/logout"},
	} {
		_, pattern := mux.Handler(httptest.NewRequest(route.method, route.path, nil))
		if pattern == "" {
			t.Errorf("route not registered: %s %s", route.method, route.path)
		}
	}
	a, b := time.Unix(1, 0), time.Unix(2, 0)
	if !minTime(a, b).Equal(a) || !minTime(b, a).Equal(a) {
		t.Fatal("minTime branch drift")
	}
}

func TestSessionCryptoAndPrincipalFailureBranches(t *testing.T) {
	f := testSessionFlow(t)
	if sealed, err := f.seal("", []byte("aad")); err != nil || sealed != "" {
		t.Fatalf("empty seal: %q %v", sealed, err)
	}
	if _, err := f.seal(strings.Repeat("x", maxTokenBody+1), []byte("aad")); err == nil {
		t.Fatal("oversized plaintext was encrypted")
	}

	now := time.Unix(1_800_000_000, 0)
	valid := sessionRecord{Claims: Claims{Subject: "user"}, RefreshToken: "refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Minute).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	for name, mutate := range map[string]func(*string, *sessionRecord){
		"sid":     func(sid *string, _ *sessionRecord) { *sid = "short" },
		"csrf":    func(_ *string, record *sessionRecord) { record.CSRF = "short" },
		"refresh": func(_ *string, record *sessionRecord) { record.RefreshToken = strings.Repeat("r", maxJWTBytes+1) },
		"version": func(_ *string, record *sessionRecord) { record.Version = 0 },
		"expiry":  func(_ *string, record *sessionRecord) { record.ExpiresAt = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			sid, record := strings.Repeat("s", 43), valid
			mutate(&sid, &record)
			if _, err := f.encodeSession(sid, record); err == nil {
				t.Fatal("invalid record encoded")
			}
		})
	}
	if _, err := f.decodeSession(strings.Repeat("s", 43), []byte(strings.Repeat("x", maxTokenBody+1))); err == nil {
		t.Fatal("oversized envelope decoded")
	}
	if _, err := f.decodeSession(strings.Repeat("s", 43), []byte("{}")); err == nil {
		t.Fatal("invalid envelope decoded")
	}
	sid := strings.Repeat("s", 43)
	sealed, err := f.seal(`{"claims":{},"csrf":""}`, f.sessionAAD(sid, 1, valid.ExpiresAt, valid.AbsoluteAt))
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := json.Marshal(sessionEnvelope{Version: 1, ExpiresAt: valid.ExpiresAt, AbsoluteAt: valid.AbsoluteAt, Ciphertext: sealed})
	if _, err := f.decodeSession(sid, envelope); err == nil {
		t.Fatal("invalid decrypted payload accepted")
	}

	claims := Claims{Roles: []string{"platform.admin", "dashboard.editor"}}
	f.cfg.ClusterID, f.cfg.ClusterName = "cluster-a", "Cluster A"
	if _, direct := f.principal(claims); !direct.CanEditDashboard {
		t.Fatal("direct principal scope was not derived")
	}
	f.cfg.Central, f.cfg.Clusters = true, []scope.Cluster{{ID: "cluster-a"}}
	if _, central := f.principal(claims); !central.CanEditDashboard {
		t.Fatal("central principal scope was not derived")
	}
}

func TestTokenSuccessRequiresExactJSONMediaType(t *testing.T) {
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/jsonp")
		_, _ = w.Write([]byte(`{"id_token":"not-used"}`))
	}))
	defer provider.Close()
	f := testSessionFlow(t)
	f.client = provider.Client()
	f.doc.TokenEndpoint = provider.URL
	if _, _, err := f.exchange(context.Background(), url.Values{"grant_type": {"refresh_token"}}, ""); err == nil || strings.Contains(err.Error(), "not-used") {
		t.Fatalf("non-JSON token response accepted or echoed: %v", err)
	}
}

func TestBrowserIDTokenProfileAndRefreshIdentity(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	valid := Claims{Issuer: "https://issuer.example", Subject: "user", Audience: []string{"client", "api"}, AuthorizedParty: "client", IssuedAt: now}
	if err := validateBrowserIDToken(valid, "client", now, time.Minute); err != nil {
		t.Fatalf("valid browser claims: %v", err)
	}
	for name, mutate := range map[string]func(*Claims){
		"missing iat":           func(c *Claims) { c.IssuedAt = time.Time{} },
		"old iat":               func(c *Claims) { c.IssuedAt = now.Add(-MaxLoginTransactionTTL - time.Minute - time.Second) },
		"future iat":            func(c *Claims) { c.IssuedAt = now.Add(time.Minute + time.Second) },
		"missing multi-aud azp": func(c *Claims) { c.AuthorizedParty = "" },
		"wrong azp":             func(c *Claims) { c.AuthorizedParty = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			claims := valid
			mutate(&claims)
			if validateBrowserIDToken(claims, "client", now, time.Minute) == nil {
				t.Fatal("invalid browser claims accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*Claims){
		"issuer":           func(c *Claims) { c.Issuer = "https://other.example" },
		"subject":          func(c *Claims) { c.Subject = "other" },
		"audience":         func(c *Claims) { c.Audience = []string{"client"} },
		"authorized party": func(c *Claims) { c.AuthorizedParty = "other" },
	} {
		t.Run("refresh "+name, func(t *testing.T) {
			claims := valid
			mutate(&claims)
			if sameBrowserIdentity(valid, claims) {
				t.Fatal("changed refresh identity accepted")
			}
		})
	}
}
