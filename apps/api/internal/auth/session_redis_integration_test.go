package auth

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type redisCommandCounter struct{ count atomic.Int64 }

func (h *redisCommandCounter) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}
func (h *redisCommandCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { h.count.Add(1); return next(ctx, cmd) }
}
func (h *redisCommandCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error { h.count.Add(1); return next(ctx, cmds) }
}

func sessionTestRedis(t *testing.T) string {
	t.Helper()
	if addr := os.Getenv("AUTH_SESSION_REDIS_TEST_ADDR"); addr != "" {
		return addr
	}
	return miniredis.RunT(t).Addr()
}

func issue1SignedToken(t *testing.T, key *rsa.PrivateKey, now time.Time, nonce string) string {
	return issue1SignedTokenFor(t, key, now, nonce, "user")
}

func issue1SignedTokenFor(t *testing.T, key *rsa.PrivateKey, now time.Time, nonce, subject string) string {
	return issue1SignedTokenIdentity(t, key, now, nonce, subject, "operator", "")
}

func issue1SignedTokenIdentity(t *testing.T, key *rsa.PrivateKey, now time.Time, nonce, subject, username, email string) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "issue1"})
	payload, _ := json.Marshal(map[string]any{"iss": "https://issuer.example", "sub": subject, "aud": "dashboard", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": nonce, "roles": []string{"platform.admin"}, "preferred_username": username, "email": email})
	a := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(a))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return a + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func redisFlow(t *testing.T, addr string, now time.Time) *sessionFlow {
	t.Helper()
	block, _ := aes.NewCipher(make([]byte, 32))
	aead, _ := cipher.NewGCM(block)
	// PoolTimeout is test-only: race instrumentation must still queue all 1024
	// admissions inside the explicit 15-second test budget. Production clients
	// are constructed in newSessionFlow and retain their bounded defaults.
	return &sessionFlow{cfg: Config{IssuerURL: "https://issuer.example", Audience: "dashboard", Now: func() time.Time { return now }}, sc: SessionConfig{IdleTTL: time.Minute, AbsoluteTTL: time.Hour, RedisTimeout: time.Second, MaxSessions: DefaultMaxSessions}, redis: redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, PoolTimeout: 15 * time.Second}), aead: aead}
}

func uniqueRedisTestOrigin(prefix string) string {
	return fmt.Sprintf("https://%s-%d-%d.example", prefix, os.Getpid(), time.Now().UnixNano())
}

func TestRedisSessionCrossResolverCASDeleteAndOutage(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	a := redisFlow(t, addr, now)
	b := redisFlow(t, addr, now)
	t.Cleanup(func() { a.redis.Close(); b.redis.Close() })
	sid := strings.Repeat("s", 43)
	record := sessionRecord{Claims: Claims{Issuer: a.cfg.IssuerURL, Subject: "user", Audience: []string{"dashboard"}, ExpiresAt: now.Add(time.Hour)}, RefreshToken: "refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Hour).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	raw, err := a.encodeSession(sid, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.redis.Set(context.Background(), a.sessionKey(sid), raw, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	keys, err := a.redis.Keys(context.Background(), "dashboard:auth:"+a.redisNamespace()+":session:*").Result()
	if err != nil || len(keys) != 1 || strings.Contains(keys[0], sid) {
		t.Fatalf("opaque keys: %v %v", keys, err)
	}
	got, err := b.loadSession(context.Background(), sid)
	if err != nil || got.Claims.Subject != "user" {
		t.Fatalf("cross resolver load: %+v %v", got, err)
	}
	stored, _ := a.redis.Get(context.Background(), a.sessionKey(sid)).Result()
	if strings.Contains(stored, "user") || strings.Contains(stored, "refresh") || strings.Contains(stored, strings.Repeat("c", 43)) {
		t.Fatal("sensitive session payload is plaintext in Redis")
	}
	next := record
	next.Version = 2
	nextRaw, _ := a.encodeSession(sid, next)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, _ := a.casSession(context.Background(), sid, 1, nextRaw, time.Minute)
			if result == 1 {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("CAS winners=%d", winners.Load())
	}
	if err := a.deleteSession(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if _, err := b.loadSession(context.Background(), sid); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("cross resolver delete: %v", err)
	}
	for name, bad := range map[string]string{"malformed": "{", "oversize": strings.Repeat("x", maxTokenBody+1), "unknown-version": strings.Replace(string(raw), "v1.", "v2.", 1), "tampered-expiry": strings.Replace(string(raw), `"expiresAt":1800003600`, `"expiresAt":1800003599`, 1)} {
		t.Run(name, func(t *testing.T) {
			if err := a.redis.Set(context.Background(), a.sessionKey(sid), bad, time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
			if _, err := b.loadSession(context.Background(), sid); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("classification: %v", err)
			}
			exists, _ := a.redis.Exists(context.Background(), a.sessionKey(sid)).Result()
			if exists != 0 {
				t.Fatal("corrupt record was not deleted")
			}
		})
	}
	broken := redisFlow(t, "127.0.0.1:1", now)
	broken.sc.RedisTimeout = 20 * time.Millisecond
	broken.redis.Options().DialTimeout = 20 * time.Millisecond
	defer broken.redis.Close()
	if _, err := broken.loadSession(context.Background(), sid); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("outage classification: %v", err)
	}
}

func TestRefreshOnlyConvergesValidSameOriginStaleCSRF(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := redisFlow(t, sessionTestRedis(t), now)
	defer f.redis.Close()
	f.sc.PublicOrigin = uniqueRedisTestOrigin("refresh-csrf")
	sid, current := strings.Repeat("s", 43), strings.Repeat("n", 43)
	record := sessionRecord{Claims: Claims{Subject: "user"}, RefreshToken: "refresh", CSRF: current, ExpiresAt: now.Add(time.Minute).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 2}
	raw, err := f.encodeSession(sid, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.redis.Set(context.Background(), f.sessionKey(sid), raw, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}

	request := func(origin, csrf string, withCookie bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if csrf != "" {
			r.Header.Set(csrfHeader, csrf)
		}
		if withCookie {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		}
		w := httptest.NewRecorder()
		if _, _, ok := f.authorizeRefresh(w, r); ok {
			t.Fatal("unexpected authorization")
		}
		return w
	}
	for _, tc := range []struct {
		name, origin, csrf string
		status             int
	}{
		{"missing origin", "", strings.Repeat("o", 43), http.StatusForbidden},
		{"wrong origin", "https://evil.example", strings.Repeat("o", 43), http.StatusForbidden},
		{"missing csrf", f.sc.PublicOrigin, "", http.StatusForbidden},
		{"malformed csrf", f.sc.PublicOrigin, "short", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := request(tc.origin, tc.csrf, true).Code; got != tc.status {
				t.Fatalf("status=%d", got)
			}
		})
	}
	stale := request(f.sc.PublicOrigin, strings.Repeat("o", 43), true)
	if stale.Code != http.StatusConflict || stale.Header().Get("Retry-After") != "0" || !strings.Contains(stale.Body.String(), `"code":"refresh_conflict"`) {
		t.Fatalf("stale response=%d %q %s", stale.Code, stale.Header().Get("Retry-After"), stale.Body.String())
	}
	missing := request(f.sc.PublicOrigin, strings.Repeat("o", 43), false)
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing status=%d", missing.Code)
	}
	loaded, err := f.loadSessionWithoutTouch(context.Background(), sid)
	if err != nil || loaded.Version != 2 || loaded.CSRF != current {
		t.Fatalf("session mutated: %#v err=%v", loaded, err)
	}
}

func TestRedisCallbackRefreshAndCrossReplicaLogout(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	nonce := "nonce-value"
	var refreshes atomic.Int32
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad", 400)
			return
		}
		isRefresh := r.Form.Get("grant_type") == "refresh_token"
		if isRefresh {
			refreshes.Add(1)
			nonce = ""
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{IDToken: issue1SignedToken(t, key, now, nonce), RefreshToken: "rotated-refresh", RefreshExpiresIn: ptr(int64(math.MaxInt64))})
	}))
	defer tokenServer.Close()
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	f.cfg.Audience = "api-resource"
	f.client = tokenServer.Client()
	f.doc.TokenEndpoint = tokenServer.URL
	f.keys = &keyStore{keys: map[string]any{"issue1": &key.PublicKey}, now: func() time.Time { return now }}
	f.sc.PublicOrigin = "https://dashboard.example"
	f.sc.RedirectURI = "https://dashboard.example/api/v1/auth/callback"
	f.sc.ClientID = "dashboard"
	f.sc.TransactionTTL = time.Minute
	txID := strings.Repeat("t", 43)
	state := strings.Repeat("q", 43)
	tx := loginTransaction{State: state, Nonce: "nonce-value", Verifier: "verifier", ReturnTo: "/namespaces", ExpiresAt: now.Add(time.Minute).Unix()}
	txJSON, _ := json.Marshal(tx)
	sealed, _ := f.seal(string(txJSON), f.recordAAD("tx", f.txKey(txID), 1))
	if err := f.redis.Set(context.Background(), f.txKey(txID), sealed, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	priorSID := strings.Repeat("o", 43)
	prior := sessionRecord{Claims: Claims{Issuer: f.cfg.IssuerURL, Subject: "user", Audience: []string{"dashboard"}}, RefreshToken: "prior-refresh", CSRF: strings.Repeat("p", 43), ExpiresAt: now.Add(time.Hour).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	priorRaw, _ := f.encodeSession(priorSID, prior)
	if err := f.redis.Set(context.Background(), f.sessionKey(priorSID), priorRaw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/auth/callback?code=code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: loginCookie, Value: txID})
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: priorSID})
	rec := httptest.NewRecorder()
	f.callback(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/namespaces" {
		t.Fatalf("callback status=%d location=%q body=%q", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	var session *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == sessionCookie {
			session = cookie
		}
	}
	if session == nil || !session.Secure || !session.HttpOnly {
		t.Fatal("secure session cookie missing")
	}
	if session.Value == priorSID || f.redis.Exists(context.Background(), f.sessionKey(priorSID)).Val() != 0 {
		t.Fatal("callback did not atomically rotate the prior session")
	}
	prior.Version++
	staleRaw, _ := f.encodeSession(priorSID, prior)
	if result, err := f.casSession(context.Background(), priorSID, 1, staleRaw, time.Minute); err != nil || result != 0 || f.redis.Exists(context.Background(), f.sessionKey(session.Value)).Val() != 1 {
		t.Fatalf("stale pre-rotation refresh recreated old session or deleted new: result=%d err=%v", result, err)
	}
	b := *f
	b.redis = redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	defer b.redis.Close()
	record, err := b.loadSession(context.Background(), session.Value)
	if err != nil {
		t.Fatal(err)
	}
	if record.AbsoluteAt != now.Add(f.sc.AbsoluteTTL).Unix() {
		t.Fatalf("large refresh lifetime bypassed configured absolute cap: %d", record.AbsoluteAt)
	}
	sessionReq := httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/auth/session", nil)
	sessionReq.AddCookie(session)
	sessionRec := httptest.NewRecorder()
	b.session(sessionRec, sessionReq)
	if sessionRec.Code != http.StatusOK || !strings.Contains(sessionRec.Body.String(), `"displayName":"operator"`) || strings.Contains(sessionRec.Body.String(), `"subject"`) {
		t.Fatalf("safe session response: %d %s", sessionRec.Code, sessionRec.Body.String())
	}
	unsafe := httptest.NewRequest(http.MethodPost, "https://dashboard.example/api/v1/dashboard-drafts", nil)
	unsafe.AddCookie(session)
	if _, err := b.resolve(unsafe); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("missing CSRF accepted: %v", err)
	}
	unsafe.Header.Set("Origin", b.sc.PublicOrigin)
	unsafe.Header.Set(csrfHeader, record.CSRF)
	if claims, err := b.resolve(unsafe); err != nil || claims.Subject != "user" {
		t.Fatalf("valid cookie resolve: %+v %v", claims, err)
	}
	refreshReq := httptest.NewRequest(http.MethodPost, "https://dashboard.example/api/v1/auth/refresh", nil)
	refreshReq.AddCookie(session)
	refreshReq.Header.Set("Origin", f.sc.PublicOrigin)
	refreshReq.Header.Set(csrfHeader, record.CSRF)
	refreshRec := httptest.NewRecorder()
	f.refresh(refreshRec, refreshReq)
	if refreshRec.Code != http.StatusNoContent || refreshes.Load() != 1 {
		t.Fatalf("refresh status=%d calls=%d body=%q", refreshRec.Code, refreshes.Load(), refreshRec.Body.String())
	}
	rotatedCSRF := refreshRec.Header().Get(csrfHeader)
	if !validOpaque(rotatedCSRF) || rotatedCSRF == record.CSRF {
		t.Fatal("refresh did not return a fresh bounded CSRF generation")
	}
	var refreshedCookie *http.Cookie
	for _, cookie := range refreshRec.Result().Cookies() {
		if cookie.Name == sessionCookie {
			refreshedCookie = cookie
		}
	}
	if refreshedCookie == nil || refreshedCookie.Value != session.Value || refreshedCookie.MaxAge <= 0 || refreshedCookie.MaxAge > int(f.sc.AbsoluteTTL.Seconds()) {
		t.Fatalf("refresh cookie not renewed/capped: %#v", refreshedCookie)
	}
	updated, err := b.loadSession(context.Background(), session.Value)
	if err != nil || updated.Version != 2 || updated.CSRF != rotatedCSRF {
		t.Fatalf("cross replica refreshed session: version=%d err=%v", updated.Version, err)
	}
	logoutReq := httptest.NewRequest(http.MethodPost, "https://dashboard.example/api/v1/auth/logout", nil)
	logoutReq.AddCookie(session)
	logoutReq.Header.Set("Origin", f.sc.PublicOrigin)
	logoutReq.Header.Set(csrfHeader, updated.CSRF)
	logoutRec := httptest.NewRecorder()
	b.logout(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("logout=%d", logoutRec.Code)
	}
	if _, err := f.loadSession(context.Background(), session.Value); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("cross replica logout not visible: %v", err)
	}
}

func TestRedisLoginTransactionAndUnauthenticatedSession(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	f.doc.AuthorizationEndpoint = "https://issuer.example/authorize"
	f.sc.PublicOrigin = "https://dashboard.example"
	f.sc.RedirectURI = "https://dashboard.example/api/v1/auth/callback"
	f.sc.ClientID = "dashboard"
	f.sc.TransactionTTL = time.Minute
	req := httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/auth/login?returnTo=%2Flogs%3Frange%3D1h", nil)
	rec := httptest.NewRecorder()
	f.login(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login=%d %s", rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || location.Query().Get("code_challenge_method") != "S256" || len(location.Query().Get("state")) != 43 || len(location.Query().Get("nonce")) != 43 {
		t.Fatalf("authorize redirect: %s %v", location, err)
	}
	keys, err := f.redis.Keys(context.Background(), "dashboard:auth:"+f.redisNamespace()+":tx:*").Result()
	if err != nil || len(keys) != 1 || strings.Contains(keys[0], location.Query().Get("state")) {
		t.Fatalf("bounded opaque tx: %v %v", keys, err)
	}
	stored, _ := f.redis.Get(context.Background(), keys[0]).Result()
	if strings.Contains(stored, "verifier") || strings.Contains(stored, "/logs") {
		t.Fatal("login transaction plaintext in Redis")
	}
	unauthReq := httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/auth/session", nil)
	unauthRec := httptest.NewRecorder()
	f.session(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusOK || unauthRec.Body.String() != "{\"authenticated\":false}\n" {
		t.Fatalf("unauth session: %d %q", unauthRec.Code, unauthRec.Body.String())
	}
}

func TestPostRotationPersistenceFailureInvalidatesSession(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	sid := strings.Repeat("r", 43)
	record := sessionRecord{Claims: Claims{Subject: "user"}, RefreshToken: "old-refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Minute).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	raw, err := f.encodeSession(sid, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.redis.Set(context.Background(), f.sessionKey(sid), raw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.invalidateRotatedSession(rec, sid)
	if f.redis.Exists(context.Background(), f.sessionKey(sid)).Val() != 0 {
		t.Fatal("rotated-token session survived persistence failure")
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie || cookies[0].MaxAge >= 0 {
		t.Fatalf("session cookie was not expired: %#v", cookies)
	}
}

func TestMutationValidationDoesNotExtendIdleTTL(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	f.sc.IdleTTL = time.Minute
	sid := strings.Repeat("m", 43)
	record := sessionRecord{Claims: Claims{Subject: "user"}, CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Hour).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	raw, _ := f.encodeSession(sid, record)
	if err := f.redis.Set(context.Background(), f.sessionKey(sid), raw, 10*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.loadSessionWithoutTouch(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	noTouch := f.redis.PTTL(context.Background(), f.sessionKey(sid)).Val()
	if noTouch > 20*time.Second {
		t.Fatalf("mutation extended idle TTL: %s", noTouch)
	}
	if _, err := f.loadSession(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	touched := f.redis.PTTL(context.Background(), f.sessionKey(sid)).Val()
	if touched < 50*time.Second {
		t.Fatalf("normal resolve did not touch idle TTL: %s", touched)
	}
}

func TestRefreshProviderFailuresRetainOnlyTransientSession(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Now().UTC().Truncate(time.Second)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"server_error"}`))
		case 3:
			_ = json.NewEncoder(w).Encode(tokenResponse{IDToken: issue1SignedToken(t, key, now, ""), RefreshToken: "rotated"})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		}
	}))
	defer provider.Close()
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	f.client = provider.Client()
	f.doc.TokenEndpoint = provider.URL
	f.keys = &keyStore{keys: map[string]any{"issue1": &key.PublicKey}, now: func() time.Time { return now }}
	f.sc.PublicOrigin = "https://dashboard.example"
	f.sc.ClientID = "dashboard"
	f.cfg.HTTPTimeout = time.Second
	sid := strings.Repeat("p", 43)
	record := sessionRecord{Claims: Claims{Issuer: "https://issuer.example", Subject: "user", Audience: []string{"dashboard"}}, RefreshToken: "refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Hour).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	put := func() {
		raw, _ := f.encodeSession(sid, record)
		if err := f.redis.Set(context.Background(), f.sessionKey(sid), raw, 10*time.Second).Err(); err != nil {
			t.Fatal(err)
		}
	}
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "https://dashboard.example/api/v1/auth/refresh", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		req.Header.Set("Origin", f.sc.PublicOrigin)
		req.Header.Set(csrfHeader, record.CSRF)
		rec := httptest.NewRecorder()
		f.refresh(rec, req)
		return rec
	}
	put()
	for attempt := 1; attempt <= 2; attempt++ {
		transient := call()
		if transient.Code != http.StatusServiceUnavailable || transient.Header().Get("Retry-After") != "1" || f.redis.Exists(context.Background(), f.sessionKey(sid)).Val() != 1 || f.redis.PTTL(context.Background(), f.sessionKey(sid)).Val() > 20*time.Second {
			t.Fatalf("transient outage %d mutated session: status=%d ttl=%s", attempt, transient.Code, f.redis.PTTL(context.Background(), f.sessionKey(sid)).Val())
		}
	}
	recovered := call()
	if recovered.Code != http.StatusNoContent {
		t.Fatalf("provider recovery did not refresh: status=%d body=%q", recovered.Code, recovered.Body.String())
	}
	put()
	permanent := call()
	if permanent.Code != http.StatusUnauthorized || f.redis.Exists(context.Background(), f.sessionKey(sid)).Val() != 0 {
		t.Fatalf("invalid grant retained session: status=%d", permanent.Code)
	}
	cookies := permanent.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Fatalf("invalid grant cookie: %#v", cookies)
	}
}

func TestBrowserIDTokenUsesClientAudienceNotAPIBearerAudience(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{IDToken: issue1SignedToken(t, key, now, ""), RefreshToken: "refresh"})
	}))
	defer provider.Close()
	f := redisFlow(t, sessionTestRedis(t), now)
	defer f.redis.Close()
	f.client = provider.Client()
	f.doc.TokenEndpoint = provider.URL
	f.keys = &keyStore{keys: map[string]any{"issue1": &key.PublicKey}, now: func() time.Time { return now }}
	f.cfg.Audience = "api-resource"
	f.sc.ClientID = "wrong-browser-client"
	if _, _, err := f.exchange(context.Background(), url.Values{"grant_type": {"refresh_token"}}, ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong browser audience accepted: %v", err)
	}
	f.sc.ClientID = "dashboard"
	if _, claims, err := f.exchange(context.Background(), url.Values{"grant_type": {"refresh_token"}}, ""); err != nil || claims.Subject != "user" {
		t.Fatalf("client audience rejected because API audience differs: %+v %v", claims, err)
	}
}

func TestRefreshChangedSubjectDeletesSessionBeforeCAS(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{IDToken: issue1SignedTokenFor(t, key, now, "", "attacker"), RefreshToken: "rotated"})
	}))
	defer provider.Close()
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	f.client = provider.Client()
	f.doc.TokenEndpoint = provider.URL
	f.keys = &keyStore{keys: map[string]any{"issue1": &key.PublicKey}, now: func() time.Time { return now }}
	f.sc.PublicOrigin = "https://dashboard.example"
	f.sc.ClientID = "dashboard"
	sid := strings.Repeat("s", 43)
	record := sessionRecord{Claims: Claims{Issuer: f.cfg.IssuerURL, Subject: "user", Audience: []string{"dashboard"}}, RefreshToken: "refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Minute).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	raw, _ := f.encodeSession(sid, record)
	if err := f.redis.Set(context.Background(), f.sessionKey(sid), raw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://dashboard.example/api/v1/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	req.Header.Set("Origin", f.sc.PublicOrigin)
	req.Header.Set(csrfHeader, record.CSRF)
	rec := httptest.NewRecorder()
	f.refresh(rec, req)
	if rec.Code != http.StatusUnauthorized || f.redis.Exists(context.Background(), f.sessionKey(sid)).Val() != 0 {
		t.Fatalf("changed subject survived refresh: status=%d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Fatalf("changed subject cookie not expired: %#v", cookies)
	}
}

func TestSessionResponseBoundsSignedDisplayIdentity(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	token := issue1SignedTokenIdentity(t, key, now, "", "private-subject", strings.Repeat("界", 129), "safe@example.test")
	claims, err := verifyJWT(token, func(string) (any, bool) { return &key.PublicKey, true }, "https://issuer.example", "dashboard", "roles", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	sid := strings.Repeat("d", 43)
	record := sessionRecord{Claims: claims, RefreshToken: "refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: claims.ExpiresAt.Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	raw, _ := f.encodeSession(sid, record)
	if err := f.redis.Set(context.Background(), f.sessionKey(sid), raw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec := httptest.NewRecorder()
	f.session(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"displayName":"safe@example.test"`) || strings.Contains(rec.Body.String(), "private-subject") || strings.Contains(rec.Body.String(), strings.Repeat("界", 129)) || strings.Contains(rec.Body.String(), "platform.admin") {
		t.Fatalf("unsafe session identity response: %s", rec.Body.String())
	}
}

func TestExpiredSessionWithoutRefreshTokenIsNotRefreshable(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	sid := strings.Repeat("n", 43)
	record := sessionRecord{Claims: Claims{Subject: "user"}, CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(-time.Second).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	raw, err := f.encodeSession(sid, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.redis.Set(context.Background(), f.sessionKey(sid), raw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec := httptest.NewRecorder()
	f.session(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"authenticated\":false}\n" {
		t.Fatalf("expired session without refresh token leaked recovery state: %d %q", rec.Code, rec.Body.String())
	}
}

func TestEntropyFailureAllocatesNoSessionCookieOrRefreshLock(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	f.random = func() string { return "" }
	f.sc.PublicOrigin = "https://dashboard.example"
	f.sc.RedirectURI = "https://dashboard.example/api/v1/auth/callback"
	namespacePattern := "dashboard:auth:" + f.redisNamespace() + ":*"
	defer func() {
		keys, _ := f.redis.Keys(context.Background(), namespacePattern).Result()
		if len(keys) > 0 {
			_ = f.redis.Del(context.Background(), keys...).Err()
		}
	}()

	loginRec := httptest.NewRecorder()
	f.login(loginRec, httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/auth/login", nil))
	loginKeys, _ := f.redis.Keys(context.Background(), namespacePattern).Result()
	if loginRec.Code != http.StatusServiceUnavailable || len(loginRec.Result().Cookies()) != 0 || len(loginKeys) != 0 {
		t.Fatalf("login entropy failure allocated state: status=%d cookies=%d keys=%d", loginRec.Code, len(loginRec.Result().Cookies()), len(loginKeys))
	}

	txID, state := strings.Repeat("t", 43), strings.Repeat("q", 43)
	tx := loginTransaction{State: state, Nonce: strings.Repeat("n", 43), Verifier: strings.Repeat("v", 43), ReturnTo: "/", ExpiresAt: now.Add(time.Minute).Unix()}
	txJSON, _ := json.Marshal(tx)
	sealed, _ := f.seal(string(txJSON), f.recordAAD("tx", f.txKey(txID), 1))
	if err := f.redis.Set(context.Background(), f.txKey(txID), sealed, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	priorSID := strings.Repeat("o", 43)
	prior := sessionRecord{Claims: Claims{Subject: "user"}, RefreshToken: "prior", CSRF: strings.Repeat("p", 43), ExpiresAt: now.Add(time.Minute).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	priorRaw, _ := f.encodeSession(priorSID, prior)
	if err := f.redis.Set(context.Background(), f.sessionKey(priorSID), priorRaw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	callbackReq := httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/auth/callback?code=code&state="+state, nil)
	callbackReq.AddCookie(&http.Cookie{Name: loginCookie, Value: txID})
	callbackReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: priorSID})
	callbackRec := httptest.NewRecorder()
	f.callback(callbackRec, callbackReq)
	sessionKeys, _ := f.redis.Keys(context.Background(), "dashboard:auth:"+f.redisNamespace()+":session:*").Result()
	if callbackRec.Code != http.StatusServiceUnavailable || len(sessionKeys) != 1 || f.redis.Exists(context.Background(), f.sessionKey(priorSID)).Val() != 1 {
		t.Fatalf("callback entropy failure allocated session: status=%d", callbackRec.Code)
	}
	for _, cookie := range callbackRec.Result().Cookies() {
		if cookie.Name == sessionCookie {
			t.Fatal("session cookie set after entropy failure")
		}
	}

	sid := strings.Repeat("s", 43)
	record := sessionRecord{Claims: Claims{Subject: "user"}, RefreshToken: "refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Minute).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
	raw, _ := f.encodeSession(sid, record)
	if err := f.redis.Set(context.Background(), f.sessionKey(sid), raw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	refreshReq := httptest.NewRequest(http.MethodPost, "https://dashboard.example/api/v1/auth/refresh", nil)
	refreshReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	refreshReq.Header.Set("Origin", f.sc.PublicOrigin)
	refreshReq.Header.Set(csrfHeader, record.CSRF)
	refreshRec := httptest.NewRecorder()
	f.refresh(refreshRec, refreshReq)
	if refreshRec.Code != http.StatusServiceUnavailable || f.redis.Exists(context.Background(), f.sessionKey(sid)+":refresh").Val() != 0 || f.redis.Exists(context.Background(), f.sessionKey(sid)).Val() != 1 {
		t.Fatalf("refresh entropy failure changed state: status=%d", refreshRec.Code)
	}
}

func TestRedisNamespacesIsolateBrowserClientsOnSharedStore(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	a := redisFlow(t, addr, now)
	defer a.redis.Close()
	a.sc.PublicOrigin = "https://a.example"
	a.sc.ClientID = "client-a"
	b := redisFlow(t, addr, now)
	defer b.redis.Close()
	b.sc.PublicOrigin = "https://b.example"
	b.sc.ClientID = "client-b"
	b.cfg.Audience = "api-b"
	if a.redisNamespace() == b.redisNamespace() || a.txIndexKey() == b.txIndexKey() {
		t.Fatal("distinct browser security boundaries share a Redis namespace")
	}
	sid := strings.Repeat("s", 43)
	for _, flow := range []*sessionFlow{a, b} {
		record := sessionRecord{Claims: Claims{Subject: "user"}, RefreshToken: "refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Minute).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
		raw, err := flow.encodeSession(sid, record)
		if err != nil {
			t.Fatal(err)
		}
		if err := flow.redis.Set(context.Background(), flow.sessionKey(sid), raw, time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		if err := flow.redis.ZAdd(context.Background(), flow.txIndexKey(), redis.Z{Score: float64(now.Add(time.Minute).UnixMilli()), Member: flow.txKey(sid)}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.deleteSession(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if a.redis.Exists(context.Background(), a.sessionKey(sid)).Val() != 0 || b.redis.Exists(context.Background(), b.sessionKey(sid)).Val() != 1 || a.redis.ZCard(context.Background(), a.txIndexKey()).Val() != 1 || b.redis.ZCard(context.Background(), b.txIndexKey()).Val() != 1 {
		t.Fatal("one browser namespace mutated another namespace")
	}
}

func TestRedisLiveSessionCapAndAtomicReplacement(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	f.sc.PublicOrigin = uniqueRedisTestOrigin("cap")
	f.sc.MaxSessions = 1
	makeRecord := func(sid string) []byte {
		record := sessionRecord{Claims: Claims{Subject: "user"}, RefreshToken: "refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Minute).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
		raw, err := f.encodeSession(sid, record)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	a, b := strings.Repeat("a", 43), strings.Repeat("b", 43)
	ctx := context.Background()
	if result, err := f.storeSession(ctx, f.sessionKey(a), f.sessionKey(a), "0", makeRecord(a), time.Minute); err != nil || result != 1 {
		t.Fatalf("first admission: %d %v", result, err)
	}
	if result, err := f.storeSession(ctx, f.sessionKey(b), f.sessionKey(b), "0", makeRecord(b), time.Minute); err != nil || result != 0 || f.redis.Exists(ctx, f.sessionKey(a)).Val() != 1 || f.redis.Exists(ctx, f.sessionKey(b)).Val() != 0 {
		t.Fatalf("cap failure mutated state: %d %v", result, err)
	}
	if result, err := f.storeSession(ctx, f.sessionKey(b), f.sessionKey(a), "1", makeRecord(b), time.Minute); err != nil || result != 1 || f.redis.Exists(ctx, f.sessionKey(a)).Val() != 0 || f.redis.Exists(ctx, f.sessionKey(b)).Val() != 1 || f.redis.ZCard(ctx, f.sessionIndexKey()).Val() != 1 {
		t.Fatalf("atomic replacement: %d %v", result, err)
	}

	// A missing session with a stale index member still counts as the replaced
	// slot. Successful rotation must remove that member regardless of EXISTS.
	f.sc.PublicOrigin = uniqueRedisTestOrigin("stale-cap")
	stale, replacement := strings.Repeat("x", 43), strings.Repeat("y", 43)
	if err := f.redis.ZAdd(ctx, f.sessionIndexKey(), redis.Z{Score: float64(now.Add(time.Minute).UnixMilli()), Member: f.sessionKey(stale)}).Err(); err != nil {
		t.Fatal(err)
	}
	if result, err := f.storeSession(ctx, f.sessionKey(replacement), f.sessionKey(stale), "1", makeRecord(replacement), time.Minute); err != nil || result != 1 || f.redis.ZCard(ctx, f.sessionIndexKey()).Val() != 1 || f.redis.ZScore(ctx, f.sessionIndexKey(), f.sessionKey(stale)).Err() != redis.Nil {
		t.Fatalf("stale indexed replacement: result=%d err=%v members=%d", result, err, f.redis.ZCard(ctx, f.sessionIndexKey()).Val())
	}
	unknown := strings.Repeat("z", 43)
	if result, err := f.storeSession(ctx, f.sessionKey(unknown), f.sessionKey(strings.Repeat("q", 43)), "1", makeRecord(unknown), time.Minute); err != nil || result != 0 || f.redis.Exists(ctx, f.sessionKey(replacement)).Val() != 1 || f.redis.Exists(ctx, f.sessionKey(unknown)).Val() != 0 {
		t.Fatalf("unindexed old bypassed cap or mutated state: result=%d err=%v", result, err)
	}

	// Admission reconciles only a fixed oldest batch when the index is full.
	f.sc.PublicOrigin = uniqueRedisTestOrigin("orphan-cap")
	f.sc.MaxSessions = 1
	orphan := f.sessionKey(strings.Repeat("o", 43))
	if err := f.redis.ZAdd(ctx, f.sessionIndexKey(), redis.Z{Score: float64(now.Add(time.Minute).UnixMilli()), Member: orphan}).Err(); err != nil {
		t.Fatal(err)
	}
	if result, err := f.storeSession(ctx, f.sessionKey(b), f.sessionKey(b), "0", makeRecord(b), time.Minute); err != nil || result != 1 || f.redis.ZCard(ctx, f.sessionIndexKey()).Val() != 1 {
		t.Fatalf("orphan admission: result=%d err=%v count=%d", result, err, f.redis.ZCard(ctx, f.sessionIndexKey()).Val())
	}

	f.sc.PublicOrigin = uniqueRedisTestOrigin("bounded-orphans")
	f.sc.MaxSessions = orphanReconcileBatch + 1
	members := make([]redis.Z, f.sc.MaxSessions)
	for i := range members {
		members[i] = redis.Z{Score: float64(now.Add(time.Minute).UnixMilli()) + float64(i), Member: f.sessionKey(fmt.Sprintf("orphan-%d", i))}
	}
	if err := f.redis.ZAdd(ctx, f.sessionIndexKey(), members...).Err(); err != nil {
		t.Fatal(err)
	}
	if result, err := f.storeSession(ctx, f.sessionKey(b), f.sessionKey(b), "0", makeRecord(b), time.Minute); err != nil || result != 1 || f.redis.ZCard(ctx, f.sessionIndexKey()).Val() != 2 {
		t.Fatalf("bounded reconciliation: result=%d err=%v count=%d", result, err, f.redis.ZCard(ctx, f.sessionIndexKey()).Val())
	}

	f.sc.PublicOrigin = uniqueRedisTestOrigin("corrupt-member")
	f.sc.MaxSessions = 1
	if err := f.redis.Set(ctx, "unrelated-victim", "preserve", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := f.redis.ZAdd(ctx, f.sessionIndexKey(), redis.Z{Score: float64(now.Add(time.Minute).UnixMilli()), Member: "unrelated-victim"}).Err(); err != nil {
		t.Fatal(err)
	}
	if result, err := f.storeSession(ctx, f.sessionKey(b), f.sessionKey(b), "0", makeRecord(b), time.Minute); err != nil || result != 1 || f.redis.Get(ctx, "unrelated-victim").Val() != "preserve" || f.redis.ZScore(ctx, f.sessionIndexKey(), "unrelated-victim").Err() != redis.Nil {
		t.Fatalf("corrupt member reconciliation: result=%d err=%v victim=%q", result, err, f.redis.Get(ctx, "unrelated-victim").Val())
	}

	// A corrupt index type aborts before either session key is changed.
	f.sc.PublicOrigin = uniqueRedisTestOrigin("wrong-index")
	old, next := strings.Repeat("m", 43), strings.Repeat("n", 43)
	if err := f.redis.Set(ctx, f.sessionKey(old), makeRecord(old), time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := f.redis.Set(ctx, f.sessionIndexKey(), "not-a-zset", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.storeSession(ctx, f.sessionKey(next), f.sessionKey(old), "1", makeRecord(next), time.Minute); err == nil || f.redis.Exists(ctx, f.sessionKey(old)).Val() != 1 || f.redis.Exists(ctx, f.sessionKey(next)).Val() != 0 {
		t.Fatalf("wrong-type index did not fail atomically: err=%v", err)
	}
}

func TestRedisSessionIndexUsesAuthoritativeCleanupHorizon(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	f.sc.PublicOrigin = uniqueRedisTestOrigin("rolling-config")
	f.sc.AbsoluteTTL = MaxSessionAbsoluteTTL
	f.sc.IdleTTL = time.Hour
	sid := strings.Repeat("r", 43)
	record := sessionRecord{Claims: Claims{Issuer: f.cfg.IssuerURL, Subject: "user", Audience: []string{"dashboard"}, ExpiresAt: now.Add(time.Hour)}, RefreshToken: "refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Hour).Unix(), AbsoluteAt: now.Add(MaxSessionAbsoluteTTL).Unix(), Version: 1}
	raw, err := f.encodeSession(sid, record)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := f.storeSession(context.Background(), f.sessionKey(sid), f.sessionKey(sid), "0", raw, time.Hour); err != nil || result != 1 {
		t.Fatalf("old-config store: result=%d err=%v", result, err)
	}
	counter := &redisCommandCounter{}
	f.redis.AddHook(counter)
	counter.count.Store(0)

	// Simulate a rolling replica with a shorter configured absolute TTL. A
	// touch must not shorten the shared index below the longest valid session.
	f.sc.AbsoluteTTL = 8 * time.Hour
	if _, err := f.loadSession(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if got := counter.count.Load(); got != 1 {
		t.Fatalf("session resolve used %d Redis commands, want one", got)
	}
	ttl := f.redis.PTTL(context.Background(), f.sessionIndexKey()).Val()
	if ttl < MaxSessionAbsoluteTTL-time.Minute || ttl > MaxSessionAbsoluteTTL {
		t.Fatalf("index cleanup horizon=%s, want within one minute of %s", ttl, MaxSessionAbsoluteTTL)
	}
}

func TestRedisSessionCapConcurrentAdmissionIsBounded(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	f := redisFlow(t, addr, now)
	defer f.redis.Close()
	f.sc.PublicOrigin = uniqueRedisTestOrigin("concurrent-cap")
	f.sc.MaxSessions = 32
	ctx := context.Background()
	const attempts = 1024
	started := time.Now()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var admitted atomic.Int64
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			digest := sha256.Sum256([]byte(strconv.Itoa(index)))
			sid := base64.RawURLEncoding.EncodeToString(digest[:])
			record := sessionRecord{Claims: Claims{Subject: "user"}, RefreshToken: "refresh", CSRF: strings.Repeat("c", 43), ExpiresAt: now.Add(time.Minute).Unix(), AbsoluteAt: now.Add(time.Hour).Unix(), Version: 1}
			raw, err := f.encodeSession(sid, record)
			if err != nil {
				t.Errorf("encode %d: %v", index, err)
				return
			}
			result, err := f.storeSession(ctx, f.sessionKey(sid), f.sessionKey(sid), "0", raw, time.Minute)
			if err != nil {
				t.Errorf("store %d: %v", index, err)
				return
			}
			if result == 1 {
				admitted.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("1024 concurrent admissions exceeded local Redis budget: %s", elapsed)
	} else {
		t.Logf("1024 concurrent admissions completed in %s", elapsed)
	}
	if admitted.Load() != int64(f.sc.MaxSessions) || f.redis.ZCard(ctx, f.sessionIndexKey()).Val() != int64(f.sc.MaxSessions) {
		t.Fatalf("concurrent cap: admitted=%d index=%d cap=%d", admitted.Load(), f.redis.ZCard(ctx, f.sessionIndexKey()).Val(), f.sc.MaxSessions)
	}
	keys, err := f.redis.Keys(ctx, "dashboard:auth:"+f.redisNamespace()+":session:*").Result()
	if err != nil || len(keys) != f.sc.MaxSessions {
		t.Fatalf("bounded session keys: count=%d err=%v", len(keys), err)
	}
}

func TestRedisTransactionIndexUsesAuthoritativeCleanupHorizon(t *testing.T) {
	addr := sessionTestRedis(t)
	now := time.Unix(1_800_000_000, 0)
	f := redisFlow(t, addr, now)
	f.sc.PublicOrigin = uniqueRedisTestOrigin("rolling-tx")
	f.sc.RedirectURI = f.sc.PublicOrigin + "/api/v1/auth/callback"
	f.sc.ClientID = "browser-client"
	f.sc.TransactionTTL = MaxLoginTransactionTTL
	f.doc.AuthorizationEndpoint = "https://issuer.example/authorize"
	t.Cleanup(func() { _ = f.redis.Close() })
	login := func() {
		recorder := httptest.NewRecorder()
		f.login(recorder, httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/auth/login", nil))
		if recorder.Code != http.StatusFound {
			t.Fatalf("login status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	login()
	f.sc.TransactionTTL = time.Minute
	login()
	ttl := f.redis.PTTL(context.Background(), f.txIndexKey()).Val()
	if ttl < MaxLoginTransactionTTL-time.Minute || ttl > MaxLoginTransactionTTL || f.redis.ZCard(context.Background(), f.txIndexKey()).Val() != 2 {
		t.Fatalf("transaction index: ttl=%s count=%d", ttl, f.redis.ZCard(context.Background(), f.txIndexKey()).Val())
	}

	// A full live index rejects without mutation; after the keys are evicted,
	// admission reconciles at most the fixed oldest batch in the same Lua call.
	f.sc.PublicOrigin = uniqueRedisTestOrigin("tx-orphans")
	f.sc.RedirectURI = f.sc.PublicOrigin + "/api/v1/auth/callback"
	ctx := context.Background()
	pipe := f.redis.Pipeline()
	keys := make([]string, maxLoginTransactions)
	for i := range keys {
		keys[i] = f.txKey(fmt.Sprintf("full-%d", i))
		pipe.Set(ctx, keys[i], "live", MaxLoginTransactionTTL)
		pipe.ZAdd(ctx, f.txIndexKey(), redis.Z{Score: float64(now.Add(time.Minute).UnixMilli()) + float64(i), Member: keys[i]})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	rejected := httptest.NewRecorder()
	f.login(rejected, httptest.NewRequest(http.MethodGet, "https://dashboard.example/api/v1/auth/login", nil))
	if rejected.Code != http.StatusServiceUnavailable || f.redis.ZCard(ctx, f.txIndexKey()).Val() != maxLoginTransactions {
		t.Fatalf("live tx cap: status=%d count=%d", rejected.Code, f.redis.ZCard(ctx, f.txIndexKey()).Val())
	}
	pipe = f.redis.Pipeline()
	for _, key := range keys {
		pipe.Del(ctx, key)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	login()
	if got := f.redis.ZCard(ctx, f.txIndexKey()).Val(); got != maxLoginTransactions-orphanReconcileBatch+1 {
		t.Fatalf("bounded tx reconciliation count=%d", got)
	}

	f.sc.PublicOrigin = uniqueRedisTestOrigin("tx-corrupt")
	f.sc.RedirectURI = f.sc.PublicOrigin + "/api/v1/auth/callback"
	if err := f.redis.Set(ctx, "unrelated-tx-victim", "preserve", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	bad := make([]redis.Z, maxLoginTransactions)
	bad[0] = redis.Z{Score: float64(now.Add(time.Minute).UnixMilli()), Member: "unrelated-tx-victim"}
	for i := 1; i < len(bad); i++ {
		bad[i] = redis.Z{Score: float64(now.Add(time.Minute).UnixMilli()) + float64(i), Member: fmt.Sprintf("invalid-member-%d", i)}
	}
	if err := f.redis.ZAdd(ctx, f.txIndexKey(), bad...).Err(); err != nil {
		t.Fatal(err)
	}
	login()
	if got := f.redis.ZCard(ctx, f.txIndexKey()).Val(); got != maxLoginTransactions-orphanReconcileBatch+1 || f.redis.Get(ctx, "unrelated-tx-victim").Val() != "preserve" {
		t.Fatalf("corrupt tx reconciliation count=%d victim=%q", got, f.redis.Get(ctx, "unrelated-tx-victim").Val())
	}
}

func TestResolverCloseIsNilSafeAndIdempotent(t *testing.T) {
	addr := sessionTestRedis(t)
	f := redisFlow(t, addr, time.Unix(1_800_000_000, 0))
	r := &Resolver{flow: f}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := f.redis.Ping(context.Background()).Err(); err == nil {
		t.Fatal("owned Redis client remained usable after resolver close")
	}
	var nilResolver *Resolver
	if err := nilResolver.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (&Resolver{}).Close(); err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](value T) *T { return &value }
