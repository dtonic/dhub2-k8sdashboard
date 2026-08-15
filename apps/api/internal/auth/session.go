package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

const (
	sessionCookie          = "__Host-k8s-dashboard"
	loginCookie            = "__Host-k8s-dashboard-login"
	csrfHeader             = "X-CSRF-Token"
	maxTokenBody           = 64 << 10
	maxLoginTransactions   = 4096
	maxLoginRecordBytes    = 8 << 10
	orphanReconcileBatch   = 64
	MaxSessionIdleTTL      = time.Hour
	MaxSessionAbsoluteTTL  = 24 * time.Hour
	MaxLoginTransactionTTL = 10 * time.Minute
	MaxRefreshSkew         = 15 * time.Minute
	DefaultMaxSessions     = 10_000
	MaxSessions            = 100_000
)

var ErrSessionUnavailable = errors.New("session store unavailable")
var ErrSessionExpired = errors.New("session expired")
var errTokenProviderUnavailable = errors.New("token provider unavailable")

type SessionConfig struct {
	Enabled        bool
	PublicOrigin   string
	RedirectURI    string
	ClientID       string
	ClientSecret   string
	EncryptionKey  string
	RedisAddr      string
	RedisTimeout   time.Duration
	TransactionTTL time.Duration
	IdleTTL        time.Duration
	AbsoluteTTL    time.Duration
	RefreshSkew    time.Duration
	MaxSessions    int
}

type loginTransaction struct {
	State, Nonce, Verifier, ReturnTo string
	ExpiresAt                        int64
}

type sessionRecord struct {
	Claims                         Claims
	RefreshToken                   string
	CSRF                           string
	ExpiresAt, AbsoluteAt, Version int64
}

type sessionEnvelope struct {
	Version    int64  `json:"version"`
	ExpiresAt  int64  `json:"expiresAt"`
	AbsoluteAt int64  `json:"absoluteAt"`
	Ciphertext string `json:"ciphertext"`
}

type sessionPayload struct {
	Claims       Claims `json:"claims"`
	RefreshToken string `json:"refreshToken,omitempty"`
	CSRF         string `json:"csrf"`
}

type sessionFlow struct {
	cfg    Config
	sc     SessionConfig
	doc    discoveryDoc
	client *http.Client
	keys   *keyStore
	redis  *redis.Client
	aead   cipher.AEAD
	random func() string
}

func newSessionFlow(cfg Config, doc discoveryDoc, client *http.Client, keys *keyStore) (*sessionFlow, error) {
	sc := cfg.Session
	if sc.TransactionTTL <= 0 {
		sc.TransactionTTL = 5 * time.Minute
	}
	if sc.IdleTTL <= 0 {
		sc.IdleTTL = 30 * time.Minute
	}
	if sc.AbsoluteTTL <= 0 {
		sc.AbsoluteTTL = 8 * time.Hour
	}
	if sc.RedisTimeout <= 0 {
		sc.RedisTimeout = 250 * time.Millisecond
	}
	if sc.RefreshSkew <= 0 {
		sc.RefreshSkew = 2 * time.Minute
	}
	if sc.MaxSessions == 0 {
		sc.MaxSessions = DefaultMaxSessions
	}
	if sc.IdleTTL > MaxSessionIdleTTL || sc.AbsoluteTTL > MaxSessionAbsoluteTTL || sc.IdleTTL > sc.AbsoluteTTL || sc.TransactionTTL > MaxLoginTransactionTTL || sc.RefreshSkew > MaxRefreshSkew || sc.MaxSessions < 1 || sc.MaxSessions > MaxSessions {
		return nil, errors.New("browser session TTL exceeds a safety bound")
	}
	origin, err := url.Parse(sc.PublicOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, errors.New("browser session requires an HTTPS origin without path")
	}
	if sc.RedirectURI != strings.TrimSuffix(sc.PublicOrigin, "/")+"/api/v1/auth/callback" {
		return nil, errors.New("OIDC redirect URI must equal public origin callback")
	}
	if sc.ClientID == "" || sc.RedisAddr == "" || doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return nil, errors.New("browser session requires client id, Redis, authorize and token endpoints")
	}
	key, err := base64.RawURLEncoding.DecodeString(sc.EncryptionKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("browser session requires a 32-byte base64url encryption key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("invalid session encryption key")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("invalid session encryption key")
	}
	for _, raw := range []string{doc.AuthorizationEndpoint, doc.TokenEndpoint} {
		u, err := url.Parse(raw)
		if err != nil || validateProviderURL(u, cfg.IssuerURL) != nil {
			return nil, errors.New("unsafe OIDC endpoint")
		}
	}
	rdb := redis.NewClient(&redis.Options{Addr: sc.RedisAddr, MaxRetries: -1, DialTimeout: sc.RedisTimeout, ReadTimeout: sc.RedisTimeout, WriteTimeout: sc.RedisTimeout})
	ctx, cancel := context.WithTimeout(context.Background(), sc.RedisTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("browser session Redis: %w", err)
	}
	return &sessionFlow{cfg: cfg, sc: sc, doc: doc, client: client, keys: keys, redis: rdb, aead: aead}, nil
}

func (r *Resolver) RegisterAuthRoutes(m *http.ServeMux) {
	if r.flow == nil {
		return
	}
	m.HandleFunc("GET /api/v1/auth/login", r.flow.login)
	m.HandleFunc("GET /api/v1/auth/callback", r.flow.callback)
	m.HandleFunc("GET /api/v1/auth/session", r.flow.session)
	m.HandleFunc("POST /api/v1/auth/refresh", r.flow.refresh)
	m.HandleFunc("POST /api/v1/auth/logout", r.flow.logout)
}

func (f *sessionFlow) resolve(r *http.Request) (Claims, error) {
	sid, ok := readCookie(r, sessionCookie)
	if !ok {
		return Claims{}, ErrInvalidToken
	}
	record, err := f.loadSession(r.Context(), sid)
	if err != nil {
		return Claims{}, err
	}
	if record.ExpiresAt <= f.cfg.Now().Unix() {
		return Claims{}, ErrSessionExpired
	}
	if unsafeMethod(r.Method) && (!sameString(r.Header.Get("Origin"), f.sc.PublicOrigin) || !sameString(r.Header.Get(csrfHeader), record.CSRF)) {
		return Claims{}, ErrInvalidToken
	}
	return record.Claims, nil
}

func (f *sessionFlow) login(w http.ResponseWriter, r *http.Request) {
	returnTo, ok := safeReturnTo(r.URL.Query().Get("returnTo"))
	if !ok {
		writeAuthError(w, http.StatusBadRequest, "bad_request", "invalid return path")
		return
	}
	txID, state, nonce, verifier, err := f.randomOpaque(), f.randomOpaque(), f.randomOpaque(), f.randomOpaque(), error(nil)
	if txID == "" || state == "" || nonce == "" || verifier == "" {
		err = errors.New("random source")
	}
	if err != nil {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "login unavailable")
		return
	}
	tx := loginTransaction{State: state, Nonce: nonce, Verifier: verifier, ReturnTo: returnTo, ExpiresAt: f.cfg.Now().Add(f.sc.TransactionTTL).Unix()}
	b, _ := json.Marshal(tx)
	sealedTx, err := f.seal(string(b), f.recordAAD("tx", f.txKey(txID), 1))
	if err != nil {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "login unavailable")
		return
	}
	if len(sealedTx) > 8192 {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "login unavailable")
		return
	}
	ctx, cancel := f.redisContext(r.Context())
	defer cancel()
	storeTx := `redis.call('ZREMRANGEBYSCORE',KEYS[2],'-inf',ARGV[1]); if redis.call('ZCARD',KEYS[2])>=tonumber(ARGV[2]) then local oldest=redis.call('ZRANGE',KEYS[2],0,tonumber(ARGV[7])-1); for _,k in ipairs(oldest) do local suffix=string.sub(k,string.len(ARGV[8])+1); if string.sub(k,1,string.len(ARGV[8]))~=ARGV[8] or string.len(suffix)~=64 or not string.match(suffix,'^[0-9a-f]+$') then redis.call('ZREM',KEYS[2],k) elseif redis.call('EXISTS',k)==0 then redis.call('ZREM',KEYS[2],k) end end end; if redis.call('ZCARD',KEYS[2])>=tonumber(ARGV[2]) then return 0 end; redis.call('SET',KEYS[1],ARGV[3],'PX',ARGV[4]); redis.call('ZADD',KEYS[2],ARGV[5],KEYS[1]); redis.call('PEXPIRE',KEYS[2],ARGV[6]); return 1`
	stored, storeErr := f.redis.Eval(ctx, storeTx, []string{f.txKey(txID), f.txIndexKey()}, strconv.FormatInt(f.cfg.Now().UnixMilli(), 10), strconv.Itoa(maxLoginTransactions), sealedTx, strconv.FormatInt(f.sc.TransactionTTL.Milliseconds(), 10), strconv.FormatInt(f.cfg.Now().Add(f.sc.TransactionTTL).UnixMilli(), 10), strconv.FormatInt(MaxLoginTransactionTTL.Milliseconds(), 10), strconv.Itoa(orphanReconcileBatch), f.txKeyPrefix()).Int()
	if storeErr != nil || stored != 1 {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "login unavailable")
		return
	}
	setCookie(w, loginCookie, txID, f.sc.TransactionTTL)
	challenge := sha256.Sum256([]byte(verifier))
	u, _ := url.Parse(f.doc.AuthorizationEndpoint)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", f.sc.ClientID)
	q.Set("redirect_uri", f.sc.RedirectURI)
	q.Set("scope", "openid profile email offline_access")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge_method", "S256")
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (f *sessionFlow) callback(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.RawQuery) > 4096 || len(r.URL.Query().Get("code")) > 2048 || len(r.URL.Query().Get("state")) != 43 {
		writeAuthError(w, http.StatusBadRequest, "bad_request", "login failed")
		return
	}
	txID, ok := readCookie(r, loginCookie)
	priorSID, hasPriorSession := readCookie(r, sessionCookie)
	if !ok || r.URL.Query().Get("code") == "" || r.URL.Query().Get("state") == "" {
		f.expire(w, loginCookie)
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "login failed")
		return
	}
	ctx, cancel := f.redisContext(r.Context())
	defer cancel()
	raw, err := f.redis.Eval(ctx, `local n=redis.call('STRLEN',KEYS[1]); if n==0 then return nil end; if n>tonumber(ARGV[1]) then redis.call('DEL',KEYS[1]); redis.call('ZREM',KEYS[2],KEYS[1]); return nil end; local v=redis.call('GET',KEYS[1]); redis.call('DEL',KEYS[1]); redis.call('ZREM',KEYS[2],KEYS[1]); return v`, []string{f.txKey(txID), f.txIndexKey()}, strconv.Itoa(maxLoginRecordBytes)).Text()
	f.expire(w, loginCookie)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			writeAuthError(w, http.StatusUnauthorized, "unauthorized", "login failed")
		} else {
			writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "login unavailable")
		}
		return
	}
	var tx loginTransaction
	plainTx, openErr := f.open(raw, f.recordAAD("tx", f.txKey(txID), 1), 4096)
	if openErr != nil || json.Unmarshal([]byte(plainTx), &tx) != nil || tx.ExpiresAt < f.cfg.Now().Unix() || !sameString(tx.State, r.URL.Query().Get("state")) {
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "login failed")
		return
	}
	sid, csrf := f.randomOpaque(), f.randomOpaque()
	if sid == "" || csrf == "" || (hasPriorSession && sameString(sid, priorSID)) {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "login unavailable")
		return
	}
	tokens, claims, err := f.exchange(r.Context(), url.Values{"grant_type": {"authorization_code"}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {f.sc.RedirectURI}, "client_id": {f.sc.ClientID}, "code_verifier": {tx.Verifier}}, tx.Nonce)
	if err != nil {
		if errors.Is(err, errTokenProviderUnavailable) {
			w.Header().Set("Retry-After", "1")
			writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "login unavailable")
		} else {
			writeAuthError(w, http.StatusUnauthorized, "unauthorized", "login failed")
		}
		return
	}
	now := f.cfg.Now()
	absolute := now.Add(f.sc.AbsoluteTTL)
	if tokens.RefreshExpiresIn != nil {
		absolute = refreshExpiry(now, absolute, *tokens.RefreshExpiresIn)
	}
	expires := claims.ExpiresAt
	if expires.After(absolute) {
		expires = absolute
	}
	record := sessionRecord{Claims: claims, RefreshToken: tokens.RefreshToken, CSRF: csrf, ExpiresAt: expires.Unix(), AbsoluteAt: absolute.Unix(), Version: 1}
	b, err := f.encodeSession(sid, record)
	if err != nil {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "login unavailable")
		return
	}
	ctx2, cancel2 := f.redisContext(r.Context())
	defer cancel2()
	newKey, oldKey, deleteOld := f.sessionKey(sid), f.sessionKey(sid), "0"
	if hasPriorSession {
		oldKey, deleteOld = f.sessionKey(priorSID), "1"
	}
	ttl := minDuration(f.sc.IdleTTL, absolute.Sub(f.cfg.Now()))
	if stored, err := f.storeSession(ctx2, newKey, oldKey, deleteOld, b, ttl); err != nil || stored != 1 {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "login unavailable")
		return
	}
	setCookie(w, sessionCookie, sid, f.sc.AbsoluteTTL)
	http.Redirect(w, r, tx.ReturnTo, http.StatusSeeOther)
}

func (f *sessionFlow) storeSession(ctx context.Context, newKey, oldKey, deleteOld string, record []byte, ttl time.Duration) (int, error) {
	if len(record) > maxTokenBody || ttl <= 0 {
		return 0, errors.New("invalid session store")
	}
	store := `redis.call('ZREMRANGEBYSCORE',KEYS[3],'-inf',ARGV[4]); local replacing=0; if ARGV[3]=='1' and KEYS[2]~=KEYS[1] and redis.call('ZSCORE',KEYS[3],KEYS[2]) then replacing=1 end; if redis.call('ZCARD',KEYS[3])-replacing>=tonumber(ARGV[5]) then local oldest=redis.call('ZRANGE',KEYS[3],0,tonumber(ARGV[8])-1); for _,k in ipairs(oldest) do local suffix=string.sub(k,string.len(ARGV[9])+1); if string.sub(k,1,string.len(ARGV[9]))~=ARGV[9] or string.len(suffix)~=64 or not string.match(suffix,'^[0-9a-f]+$') then redis.call('ZREM',KEYS[3],k) elseif redis.call('EXISTS',k)==0 then redis.call('ZREM',KEYS[3],k) end end end; if redis.call('ZCARD',KEYS[3])-replacing>=tonumber(ARGV[5]) then return 0 end; redis.call('SET',KEYS[1],ARGV[1],'PX',ARGV[2]); redis.call('ZADD',KEYS[3],ARGV[6],KEYS[1]); if ARGV[3]=='1' and KEYS[2]~=KEYS[1] then redis.call('DEL',KEYS[2]); redis.call('ZREM',KEYS[3],KEYS[2]) end; redis.call('PEXPIRE',KEYS[3],ARGV[7]); return 1`
	return f.redis.Eval(ctx, store, []string{newKey, oldKey, f.sessionIndexKey()}, string(record), strconv.FormatInt(ttl.Milliseconds(), 10), deleteOld, strconv.FormatInt(f.cfg.Now().UnixMilli(), 10), strconv.Itoa(f.sc.MaxSessions), strconv.FormatInt(f.cfg.Now().Add(ttl).UnixMilli(), 10), strconv.FormatInt(MaxSessionAbsoluteTTL.Milliseconds(), 10), strconv.Itoa(orphanReconcileBatch), f.sessionKeyPrefix()).Int()
}

type tokenResponse struct {
	IDToken          string `json:"id_token"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn *int64 `json:"refresh_expires_in,omitempty"`
}

func (f *sessionFlow) exchange(ctx context.Context, form url.Values, nonce string) (tokenResponse, Claims, error) {
	if f.sc.ClientSecret != "" {
		form.Set("client_secret", f.sc.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.doc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, Claims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := f.client.Do(req)
	if err != nil {
		return tokenResponse{}, Claims{}, errTokenProviderUnavailable
	}
	defer res.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(res.Body, maxTokenBody+1))
	if readErr != nil || len(body) > maxTokenBody {
		return tokenResponse{}, Claims{}, errors.New("invalid token response")
	}
	mediaType, _, mediaErr := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			return tokenResponse{}, Claims{}, errTokenProviderUnavailable
		}
		if mediaErr == nil && mediaType == "application/json" {
			var oauth struct {
				Error            string `json:"error"`
				ErrorDescription string `json:"error_description,omitempty"`
				ErrorURI         string `json:"error_uri,omitempty"`
			}
			decoder := json.NewDecoder(strings.NewReader(string(body)))
			decoder.DisallowUnknownFields()
			var trailing any
			if decoder.Decode(&oauth) == nil && decoder.Decode(&trailing) == io.EOF && oauth.Error != "" {
				switch oauth.Error {
				case "temporarily_unavailable", "server_error":
					return tokenResponse{}, Claims{}, errTokenProviderUnavailable
				case "invalid_grant":
					return tokenResponse{}, Claims{}, errors.New("token grant rejected")
				}
			}
		}
		// Other 4xx responses are permanent protocol/client failures. A refresh
		// session is invalidated because token rotation cannot be ruled out.
		return tokenResponse{}, Claims{}, errors.New("token exchange rejected")
	}
	if mediaErr != nil || mediaType != "application/json" {
		return tokenResponse{}, Claims{}, errors.New("invalid token response")
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil || tr.IDToken == "" || len(tr.RefreshToken) > maxJWTBytes || (tr.RefreshExpiresIn != nil && *tr.RefreshExpiresIn <= 0) {
		return tokenResponse{}, Claims{}, errors.New("invalid token response")
	}
	claims, err := verifyJWT(tr.IDToken, func(kid string) (any, bool) { return f.keys.get(ctx, kid) }, f.cfg.IssuerURL, f.sc.ClientID, f.cfg.RolesClaim, f.cfg.Leeway, f.cfg.Now())
	if err != nil || validateBrowserIDToken(claims, f.sc.ClientID, f.cfg.Now(), f.cfg.Leeway) != nil || (nonce != "" && !sameString(claims.Nonce, nonce)) {
		return tokenResponse{}, Claims{}, ErrInvalidToken
	}
	return tr, claims, nil
}

func (f *sessionFlow) session(w http.ResponseWriter, r *http.Request) {
	sid, ok := readCookie(r, sessionCookie)
	if !ok {
		writeSessionJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	record, err := f.loadSession(r.Context(), sid)
	if err != nil {
		if errors.Is(err, ErrSessionUnavailable) {
			writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "session unavailable")
		} else {
			writeSessionJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		}
		return
	}
	if record.ExpiresAt <= f.cfg.Now().Unix() {
		if record.RefreshToken != "" {
			writeSessionJSON(w, http.StatusOK, map[string]any{"authenticated": false, "refreshable": true, "csrfToken": record.CSRF})
		} else {
			writeSessionJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		}
		return
	}
	principal, sc := f.principal(record.Claims)
	displayName := principal.Username
	if !displaySafe(displayName) {
		displayName = principal.Email
	}
	if !displaySafe(displayName) {
		displayName = "Signed-in user"
	}
	expiresAt := time.Unix(record.ExpiresAt, 0).UTC()
	writeSessionJSON(w, http.StatusOK, map[string]any{"authenticated": true, "principal": map[string]string{"displayName": displayName}, "capabilities": map[string]bool{"canEditDashboard": sc.CanEditDashboard, "canPublishDashboard": sc.CanPublishDashboard}, "expiresAt": expiresAt.Format(time.RFC3339), "refreshAt": sessionRefreshAt(f.cfg.Now(), expiresAt, f.sc.RefreshSkew).Format(time.RFC3339Nano), "csrfToken": record.CSRF})
}

func displaySafe(value string) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 128 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func sessionRefreshAt(now, expires time.Time, configured time.Duration) time.Time {
	remaining := expires.Sub(now)
	if remaining <= 0 {
		return now
	}
	lead := minDuration(configured, remaining/2)
	if lead <= 0 {
		lead = remaining / 2
	}
	refresh := expires.Add(-lead)
	if !refresh.After(now) {
		refresh = now.Add(remaining / 2)
	}
	return refresh
}

func validateBrowserIDToken(claims Claims, clientID string, now time.Time, leeway time.Duration) error {
	if claims.IssuedAt.IsZero() || claims.IssuedAt.After(now.Add(leeway)) || claims.IssuedAt.Before(now.Add(-MaxLoginTransactionTTL-leeway)) {
		return ErrInvalidToken
	}
	if claims.AuthorizedParty != "" && !sameString(claims.AuthorizedParty, clientID) {
		return ErrInvalidToken
	}
	if len(claims.Audience) > 1 && claims.AuthorizedParty == "" {
		return ErrInvalidToken
	}
	return nil
}

func sameAudience(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, value := range a {
		seen[value]++
	}
	for _, value := range b {
		if seen[value] == 0 {
			return false
		}
		seen[value]--
	}
	return true
}

func sameBrowserIdentity(previous, refreshed Claims) bool {
	return sameString(previous.Issuer, refreshed.Issuer) && sameString(previous.Subject, refreshed.Subject) && sameString(previous.AuthorizedParty, refreshed.AuthorizedParty) && sameAudience(previous.Audience, refreshed.Audience)
}

func (f *sessionFlow) refresh(w http.ResponseWriter, r *http.Request) {
	sid, record, ok := f.authorizeRefresh(w, r)
	if !ok {
		return
	}
	if record.RefreshToken == "" {
		f.deleteSession(r.Context(), sid)
		f.expire(w, sessionCookie)
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "session expired")
		return
	}
	refreshToken := record.RefreshToken
	lock := f.sessionKey(sid) + ":refresh"
	owner := f.randomOpaque()
	if owner == "" {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "session unavailable")
		return
	}
	ctx, cancel := f.redisContext(r.Context())
	defer cancel()
	lockTTL := minDuration(30*time.Second, max(15*time.Second, f.cfg.HTTPTimeout+5*time.Second))
	locked, err := f.redis.SetNX(ctx, lock, owner, lockTTL).Result()
	if err != nil {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "session unavailable")
		return
	}
	if !locked {
		w.Header().Set("Retry-After", "1")
		writeAuthError(w, http.StatusConflict, "refresh_conflict", "refresh in progress")
		return
	}
	defer f.redis.Eval(context.Background(), `if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`, []string{lock}, owner)
	nextCSRF := f.randomOpaque()
	if nextCSRF == "" {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "session unavailable")
		return
	}
	tr, claims, err := f.exchange(r.Context(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {f.sc.ClientID}}, "")
	if err != nil {
		if errors.Is(err, errTokenProviderUnavailable) {
			w.Header().Set("Retry-After", "1")
			writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "session unavailable")
			return
		}
		f.deleteSession(r.Context(), sid)
		f.expire(w, sessionCookie)
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "session expired")
		return
	}
	if !sameBrowserIdentity(record.Claims, claims) {
		f.deleteSession(r.Context(), sid)
		f.expire(w, sessionCookie)
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "session expired")
		return
	}
	nextVersion := record.Version + 1
	if tr.RefreshToken != "" {
		record.RefreshToken = tr.RefreshToken
	}
	record.Claims = claims
	record.CSRF = nextCSRF
	record.Version = nextVersion
	expires := claims.ExpiresAt
	absolute := time.Unix(record.AbsoluteAt, 0)
	if tr.RefreshExpiresIn != nil {
		absolute = refreshExpiry(f.cfg.Now(), absolute, *tr.RefreshExpiresIn)
		record.AbsoluteAt = absolute.Unix()
	}
	if expires.After(absolute) {
		expires = absolute
	}
	record.ExpiresAt = expires.Unix()
	b, err := f.encodeSession(sid, record)
	if err != nil {
		f.invalidateRotatedSession(w, sid)
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "session unavailable")
		return
	}
	ctx2, cancel2 := f.redisContext(r.Context())
	defer cancel2()
	result, err := f.casSession(ctx2, sid, record.Version-1, b, minDuration(f.sc.IdleTTL, absolute.Sub(f.cfg.Now())))
	if err != nil || result != 1 {
		f.invalidateRotatedSession(w, sid)
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "session unavailable")
		return
	}
	setCookie(w, sessionCookie, sid, absolute.Sub(f.cfg.Now()))
	w.Header().Set(csrfHeader, nextCSRF)
	w.WriteHeader(http.StatusNoContent)
}

func (f *sessionFlow) authorizeRefresh(w http.ResponseWriter, r *http.Request) (string, sessionRecord, bool) {
	provided := r.Header.Get(csrfHeader)
	if !sameString(r.Header.Get("Origin"), f.sc.PublicOrigin) || !validOpaque(provided) {
		writeAuthError(w, http.StatusForbidden, "forbidden", "csrf rejected")
		return "", sessionRecord{}, false
	}
	sid, ok := readCookie(r, sessionCookie)
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return "", sessionRecord{}, false
	}
	record, err := f.loadSessionWithoutTouch(r.Context(), sid)
	if err != nil {
		if errors.Is(err, ErrSessionUnavailable) {
			writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "session unavailable")
		} else {
			writeAuthError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		}
		return "", sessionRecord{}, false
	}
	if !sameString(provided, record.CSRF) {
		w.Header().Set("Retry-After", "0")
		writeAuthError(w, http.StatusConflict, "refresh_conflict", "refresh generation changed")
		return "", sessionRecord{}, false
	}
	return sid, record, true
}

// Once an IdP rotates a refresh token, retaining the old Redis record can
// resurrect neither token safely. Delete it best-effort and expire the browser
// cookie on every post-exchange persistence failure.
func (f *sessionFlow) invalidateRotatedSession(w http.ResponseWriter, sid string) {
	_ = f.deleteSession(context.Background(), sid)
	f.expire(w, sessionCookie)
}

func (f *sessionFlow) casSession(ctx context.Context, sid string, version int64, record []byte, ttl time.Duration) (int, error) {
	if len(record) > maxTokenBody || ttl <= 0 {
		return 0, errors.New("invalid session CAS")
	}
	cas := `local n=redis.call('STRLEN',KEYS[1]); if n==0 then redis.call('ZREM',KEYS[2],KEYS[1]); return 0 end; if n>tonumber(ARGV[4]) then redis.call('DEL',KEYS[1]); redis.call('ZREM',KEYS[2],KEYS[1]); return -2 end; local v=redis.call('GET',KEYS[1]); local ok,o=pcall(cjson.decode,v); if not ok or type(o)~='table' or type(o.version)~='number' then redis.call('DEL',KEYS[1]); redis.call('ZREM',KEYS[2],KEYS[1]); return -2 end; if o.version~=tonumber(ARGV[1]) then return -1 end; redis.call('SET',KEYS[1],ARGV[2],'PX',ARGV[3]); redis.call('ZADD',KEYS[2],ARGV[5],KEYS[1]); redis.call('PEXPIRE',KEYS[2],ARGV[6]); return 1`
	return f.redis.Eval(ctx, cas, []string{f.sessionKey(sid), f.sessionIndexKey()}, strconv.FormatInt(version, 10), string(record), strconv.FormatInt(ttl.Milliseconds(), 10), strconv.Itoa(maxTokenBody), strconv.FormatInt(f.cfg.Now().Add(ttl).UnixMilli(), 10), strconv.FormatInt(MaxSessionAbsoluteTTL.Milliseconds(), 10)).Int()
}

func (f *sessionFlow) logout(w http.ResponseWriter, r *http.Request) {
	sid, _, ok := f.authorizeMutation(w, r)
	if !ok {
		return
	}
	if err := f.deleteSession(r.Context(), sid); err != nil {
		writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "session unavailable")
		return
	}
	f.expire(w, sessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

func (f *sessionFlow) authorizeMutation(w http.ResponseWriter, r *http.Request) (string, sessionRecord, bool) {
	sid, ok := readCookie(r, sessionCookie)
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return "", sessionRecord{}, false
	}
	record, err := f.loadSessionWithoutTouch(r.Context(), sid)
	if err != nil {
		if errors.Is(err, ErrSessionUnavailable) {
			writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "session unavailable")
		} else {
			writeAuthError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		}
		return "", sessionRecord{}, false
	}
	if !sameString(r.Header.Get("Origin"), f.sc.PublicOrigin) || !sameString(r.Header.Get(csrfHeader), record.CSRF) {
		writeAuthError(w, http.StatusForbidden, "forbidden", "csrf rejected")
		return "", sessionRecord{}, false
	}
	return sid, record, true
}

func (f *sessionFlow) principal(c Claims) (Principal, scope.Scope) {
	if f.cfg.Central {
		return ScopeForCentral(c, f.cfg.Clusters)
	}
	return ScopeFor(c, f.cfg.ClusterID, f.cfg.ClusterName)
}

func (f *sessionFlow) loadSession(parent context.Context, sid string) (sessionRecord, error) {
	return f.loadSessionMode(parent, sid, true)
}

func (f *sessionFlow) loadSessionWithoutTouch(parent context.Context, sid string) (sessionRecord, error) {
	return f.loadSessionMode(parent, sid, false)
}

func (f *sessionFlow) loadSessionMode(parent context.Context, sid string, touch bool) (sessionRecord, error) {
	ctx, cancel := f.redisContext(parent)
	defer cancel()
	script := `local n=redis.call('STRLEN',KEYS[1]); if n==0 then redis.call('ZREM',KEYS[2],KEYS[1]); return nil end; if n>tonumber(ARGV[4]) then redis.call('DEL',KEYS[1]); redis.call('ZREM',KEYS[2],KEYS[1]); return nil end; local v=redis.call('GET',KEYS[1]); local ok,o=pcall(cjson.decode,v); if not ok or type(o)~='table' or type(o.version)~='number' or type(o.expiresAt)~='number' or type(o.absoluteAt)~='number' or type(o.ciphertext)~='string' then redis.call('DEL',KEYS[1]); redis.call('ZREM',KEYS[2],KEYS[1]); return nil end; local now=tonumber(ARGV[1]); if o.absoluteAt<=now then redis.call('DEL',KEYS[1]); redis.call('ZREM',KEYS[2],KEYS[1]); return nil end; local ttl=math.min(tonumber(ARGV[2]),(o.absoluteAt-now)*1000); if ttl<=0 then redis.call('DEL',KEYS[1]); redis.call('ZREM',KEYS[2],KEYS[1]); return nil end; if ARGV[3]=='1' and o.expiresAt>now then redis.call('PEXPIRE',KEYS[1],ttl); redis.call('ZADD',KEYS[2],now*1000+ttl,KEYS[1]); redis.call('PEXPIRE',KEYS[2],ARGV[5]) end; return v`
	touchArg := "0"
	if touch {
		touchArg = "1"
	}
	raw, err := f.redis.Eval(ctx, script, []string{f.sessionKey(sid), f.sessionIndexKey()}, strconv.FormatInt(f.cfg.Now().Unix(), 10), strconv.FormatInt(f.sc.IdleTTL.Milliseconds(), 10), touchArg, strconv.Itoa(maxTokenBody), strconv.FormatInt(MaxSessionAbsoluteTTL.Milliseconds(), 10)).Text()
	if errors.Is(err, redis.Nil) {
		return sessionRecord{}, ErrInvalidToken
	}
	if err != nil {
		return sessionRecord{}, ErrSessionUnavailable
	}
	record, decodeErr := f.decodeSession(sid, []byte(raw))
	if decodeErr != nil {
		_ = f.deleteSession(parent, sid)
		return sessionRecord{}, ErrInvalidToken
	}
	return record, nil
}

func (f *sessionFlow) deleteSession(parent context.Context, sid string) error {
	ctx, cancel := f.redisContext(parent)
	defer cancel()
	if err := f.redis.Eval(ctx, `redis.call('DEL',KEYS[1]); redis.call('ZREM',KEYS[2],KEYS[1]); return 1`, []string{f.sessionKey(sid), f.sessionIndexKey()}).Err(); err != nil {
		return ErrSessionUnavailable
	}
	return nil
}
func (f *sessionFlow) redisContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, f.sc.RedisTimeout)
}
func (f *sessionFlow) redisNamespace() string {
	return hashOpaque(strings.Join([]string{f.cfg.IssuerURL, f.cfg.Audience, f.sc.ClientID, f.sc.PublicOrigin}, "\x00"))
}
func (f *sessionFlow) txKey(v string) string {
	return f.txKeyPrefix() + hashOpaque(v)
}
func (f *sessionFlow) txKeyPrefix() string { return "dashboard:auth:" + f.redisNamespace() + ":tx:" }
func (f *sessionFlow) txIndexKey() string {
	return "dashboard:auth:" + f.redisNamespace() + ":tx-index"
}
func (f *sessionFlow) sessionIndexKey() string {
	return "dashboard:auth:" + f.redisNamespace() + ":session-index"
}
func (f *sessionFlow) sessionKey(v string) string {
	return f.sessionKeyPrefix() + hashOpaque(v)
}
func (f *sessionFlow) sessionKeyPrefix() string {
	return "dashboard:auth:" + f.redisNamespace() + ":session:"
}
func hashOpaque(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func (f *sessionFlow) recordAAD(kind, redisKey string, version int64) []byte {
	return []byte(strings.Join([]string{"dashboard-auth", kind, redisKey, strconv.FormatInt(version, 10), f.cfg.IssuerURL, f.cfg.Audience, f.sc.ClientID}, "\x00"))
}

func (f *sessionFlow) seal(value string, aad []byte) (string, error) {
	if value == "" {
		return "", nil
	}
	nonce := make([]byte, f.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	if len(value) > maxTokenBody {
		return "", errors.New("plaintext exceeds bound")
	}
	ciphertext := f.aead.Seal(nil, nonce, []byte(value), aad)
	return "v1." + base64.RawURLEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}
func (f *sessionFlow) open(value string, aad []byte, maxPlaintext int) (string, error) {
	if !strings.HasPrefix(value, "v1.") {
		return "", errors.New("invalid encrypted value")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "v1."))
	if err != nil || len(raw) <= f.aead.NonceSize() {
		return "", errors.New("invalid encrypted value")
	}
	if len(raw) > maxTokenBody+f.aead.Overhead()+f.aead.NonceSize() {
		return "", errors.New("encrypted value exceeds bound")
	}
	plain, err := f.aead.Open(nil, raw[:f.aead.NonceSize()], raw[f.aead.NonceSize():], aad)
	if err != nil || len(plain) > maxPlaintext {
		return "", errors.New("invalid encrypted value")
	}
	return string(plain), nil
}
func randomValue() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (f *sessionFlow) randomOpaque() string {
	if f.random != nil {
		return f.random()
	}
	return randomValue()
}
func validOpaque(value string) bool {
	if len(value) != 43 {
		return false
	}
	for _, c := range value {
		if !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
func readCookie(r *http.Request, name string) (string, bool) {
	c, err := r.Cookie(name)
	if err != nil || !validOpaque(c.Value) {
		return "", false
	}
	return c.Value, true
}
func setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: max(1, int(ttl.Seconds()))})
}
func (f *sessionFlow) expire(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}
func sameString(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func unsafeMethod(m string) bool {
	return m == http.MethodPost || m == http.MethodPut || m == http.MethodPatch || m == http.MethodDelete
}
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func refreshExpiry(now, configuredAbsolute time.Time, seconds int64) time.Time {
	remaining := configuredAbsolute.Sub(now)
	if remaining <= 0 {
		return configuredAbsolute
	}
	ceilingSeconds := int64((remaining + time.Second - 1) / time.Second)
	if seconds >= ceilingSeconds {
		return configuredAbsolute
	}
	return now.Add(time.Duration(seconds) * time.Second)
}
func safeReturnTo(raw string) (string, bool) {
	if raw == "" {
		return "/", true
	}
	if len(raw) > 2048 || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
		return "", false
	}
	v := raw
	for i := 0; i < 3; i++ {
		for _, c := range v {
			if c < 0x20 || c == 0x7f {
				return "", false
			}
		}
		next, err := url.PathUnescape(v)
		if err != nil {
			return "", false
		}
		if next == v {
			break
		}
		v = next
		if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.Contains(v, "\\") {
			return "", false
		}
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.IsAbs() || u.Host != "" || strings.Contains(u.EscapedPath(), "%") || !allowedUIPath(u.Path) {
		return "", false
	}
	return raw, true
}

func allowedUIPath(path string) bool {
	if path == "/" {
		return true
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	switch segments[0] {
	case "namespaces", "pods", "dashboards", "dashboard-builder":
		return len(segments) <= 2
	case "workloads":
		return len(segments) == 3
	case "topology", "logs", "alerts":
		return len(segments) == 1
	default:
		return false
	}
}
func writeSessionJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	requestID := w.Header().Get("X-Request-ID")
	if requestID == "" {
		requestID = randomValue()
		w.Header().Set("X-Request-ID", requestID)
	}
	writeSessionJSON(w, status, map[string]string{"code": code, "message": message, "requestId": requestID})
}

func (f *sessionFlow) sessionAAD(sid string, version, expiresAt, absoluteAt int64) []byte {
	return []byte(strings.Join([]string{"dashboard-auth", "session", f.sessionKey(sid), strconv.FormatInt(version, 10), strconv.FormatInt(expiresAt, 10), strconv.FormatInt(absoluteAt, 10), f.cfg.IssuerURL, f.cfg.Audience, f.sc.ClientID}, "\x00"))
}
func (f *sessionFlow) encodeSession(sid string, record sessionRecord) ([]byte, error) {
	if !validOpaque(sid) || !validOpaque(record.CSRF) || len(record.RefreshToken) > maxJWTBytes || record.Version < 1 || record.ExpiresAt <= 0 || record.AbsoluteAt <= 0 {
		return nil, errors.New("invalid session record")
	}
	plain, err := json.Marshal(sessionPayload{Claims: record.Claims, RefreshToken: record.RefreshToken, CSRF: record.CSRF})
	if err != nil || len(plain) > maxTokenBody {
		return nil, errors.New("session payload exceeds bound")
	}
	sealed, err := f.seal(string(plain), f.sessionAAD(sid, record.Version, record.ExpiresAt, record.AbsoluteAt))
	if err != nil {
		return nil, errors.New("session encryption failed")
	}
	b, err := json.Marshal(sessionEnvelope{Version: record.Version, ExpiresAt: record.ExpiresAt, AbsoluteAt: record.AbsoluteAt, Ciphertext: sealed})
	if err != nil || len(b) > maxTokenBody {
		return nil, errors.New("session envelope exceeds bound")
	}
	return b, nil
}
func (f *sessionFlow) decodeSession(sid string, raw []byte) (sessionRecord, error) {
	if len(raw) > maxTokenBody {
		return sessionRecord{}, errors.New("session envelope exceeds bound")
	}
	var env sessionEnvelope
	if json.Unmarshal(raw, &env) != nil || env.Version < 1 || env.ExpiresAt <= 0 || env.AbsoluteAt <= 0 {
		return sessionRecord{}, errors.New("invalid session envelope")
	}
	plain, err := f.open(env.Ciphertext, f.sessionAAD(sid, env.Version, env.ExpiresAt, env.AbsoluteAt), maxTokenBody)
	if err != nil {
		return sessionRecord{}, errors.New("invalid session ciphertext")
	}
	var payload sessionPayload
	if json.Unmarshal([]byte(plain), &payload) != nil || payload.Claims.Subject == "" || payload.CSRF == "" || len(payload.RefreshToken) > maxJWTBytes {
		return sessionRecord{}, errors.New("invalid session payload")
	}
	return sessionRecord{Claims: payload.Claims, RefreshToken: payload.RefreshToken, CSRF: payload.CSRF, ExpiresAt: env.ExpiresAt, AbsoluteAt: env.AbsoluteAt, Version: env.Version}, nil
}
