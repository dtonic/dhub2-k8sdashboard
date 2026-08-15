//go:build e2efixture

package e2efixture

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/auth"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/alertmanager"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

// AuthSessionConfig configures the browser-auth acceptance fixture. It only
// composes public production handlers; authentication behavior is not mocked in
// the API process.
type AuthSessionConfig struct {
	DistDir       string
	RedisAddr     string
	EncryptionKey string
	PublicOrigin  string
	CertFile      string
	KeyFile       string
	BackendAddr   string
	Alertmanager  alertmanager.Config
}

type AuthSessionFixture struct {
	URL           string
	Issuer        string
	APIURLs       [2]string
	server        *httptest.Server
	api           [2]*httptest.Server
	resolver      [2]*auth.Resolver
	idp           *codeIDP
	replica       [2]atomic.Uint64
	hub           *stream.Hub
	streamMetrics *stream.Metrics
	alerts        *alertmanager.Source
	closeOnce     sync.Once
}

func StartAuthSession(ctx context.Context, cfg AuthSessionConfig, logger *slog.Logger) (*AuthSessionFixture, error) {
	if cfg.RedisAddr == "" || cfg.EncryptionKey == "" {
		return nil, errors.New("auth fixture requires Redis and an encryption key")
	}
	dist, err := filepath.Abs(cfg.DistDir)
	if err != nil {
		return nil, err
	}
	index, err := os.ReadFile(filepath.Join(dist, "index.html"))
	if err != nil {
		return nil, fmt.Errorf("read production index: %w", err)
	}
	const authMeta = `<meta name="k8s-auth-session" content="disabled" />`
	if !strings.Contains(string(index), authMeta) {
		return nil, errors.New("production index lacks the inert auth placeholder")
	}
	if logger == nil {
		logger = slog.Default()
	}

	idp, err := startCodeIDP()
	if err != nil {
		return nil, err
	}
	f := &AuthSessionFixture{idp: idp, Issuer: idp.URL()}
	if cfg.PublicOrigin != "" {
		idp.authorizationURL = strings.TrimRight(cfg.PublicOrigin, "/") + "/e2e/idp/authorize"
	}
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, errors.New("both fixture certificate paths are required")
		}
		if err := idp.writeTLSFiles(cfg.CertFile, cfg.KeyFile); err != nil {
			return nil, err
		}
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()

	var ready atomic.Bool
	var rr atomic.Uint64
	var apiProxies [2]*httputil.ReverseProxy
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || operationalPath[r.URL.Path] {
			apiProxies[rr.Add(1)%uint64(len(apiProxies))].ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/e2e/idp/authorize" && r.Method == http.MethodGet {
			idp.handleAuthorize(w, r)
			return
		}
		if r.URL.Path == "/e2e/evidence" {
			w.Header().Set("Content-Type", "application/json")
			evidence := idp.evidence()
			evidence["replica0"] = f.replica[0].Load()
			evidence["replica1"] = f.replica[1].Load()
			var metrics bytes.Buffer
			_ = f.streamMetrics.WritePrometheus(&metrics)
			evidence["streamConnections"] = metricUint(metrics.String(), "dashboard_stream_connections")
			evidence["streamDeliveries"] = metricUint(metrics.String(), "dashboard_stream_delivery_seconds_count")
			_ = json.NewEncoder(w).Encode(evidence)
			return
		}
		if r.URL.Path == "/e2e/publish" && r.Method == http.MethodPost {
			f.hub.Publish(contract.EventEnvelope{Kind: contract.StreamKindPod, Action: contract.StreamActionUpdated, ClusterID: testcluster.ClusterID, Namespace: "media", Entity: &contract.EntityRef{ClusterID: testcluster.ClusterID, Namespace: "media"}})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/e2e/token-ttl" && r.Method == http.MethodPost {
			seconds, err := time.ParseDuration(r.URL.Query().Get("value"))
			if err != nil || seconds < 2*time.Second || seconds > 5*time.Minute {
				http.Error(w, "invalid token ttl", http.StatusBadRequest)
				return
			}
			f.idp.nextTokenTTL.Store(int64(seconds))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/e2e/refresh-delay" && r.Method == http.MethodPost {
			delay, err := time.ParseDuration(r.URL.Query().Get("value"))
			if err != nil || delay < 0 || delay > 5*time.Second {
				http.Error(w, "invalid refresh delay", http.StatusBadRequest)
				return
			}
			f.idp.nextRefreshDelay.Store(int64(delay))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/e2e/next-role" && r.Method == http.MethodPost {
			if r.URL.Query().Get("value") != "viewer" {
				http.Error(w, "invalid role", http.StatusBadRequest)
				return
			}
			f.idp.nextViewer.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		name := filepath.Join(dist, filepath.FromSlash(strings.TrimPrefix(r.URL.Path, "/")))
		if r.URL.Path != "/" {
			if st, statErr := os.Stat(name); statErr == nil && st.Mode().IsRegular() {
				http.ServeFile(w, r, name)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		enabled := strings.Replace(string(index), authMeta, `<meta name="k8s-auth-session" content="enabled" />`, 1)
		_, _ = io.WriteString(w, enabled)
	})
	f.server = httptest.NewUnstartedServer(proxy)
	if cfg.BackendAddr != "" {
		host, port, splitErr := net.SplitHostPort(cfg.BackendAddr)
		if splitErr != nil || host != "0.0.0.0" || port != "9444" {
			return nil, errors.New("auth fixture container backend must use 0.0.0.0:9444")
		}
		listener, listenErr := net.Listen("tcp", cfg.BackendAddr)
		if listenErr != nil {
			return nil, listenErr
		}
		f.server.Listener = listener
	}
	f.server.StartTLS()
	f.URL = f.server.URL
	publicOrigin := cfg.PublicOrigin
	if publicOrigin == "" {
		publicOrigin = f.URL
	}
	idp.callback = publicOrigin + "/api/v1/auth/callback"

	roots := x509.NewCertPool()
	roots.AddCert(f.idp.Certificate())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	store, _, err := testcluster.Build(ctx, testcluster.ScenarioObjects()...)
	if err != nil {
		return nil, err
	}
	scenarios, err := scenariosFor(nil)
	if err != nil {
		return nil, err
	}
	source := NewSource(store, scenarios)
	var alerts datasource.Alerts = source
	if cfg.Alertmanager.Enabled {
		f.alerts, err = alertmanager.New(cfg.Alertmanager, store)
		if err != nil {
			return nil, err
		}
		alerts = f.alerts
	}
	streamMetrics := stream.NewMetrics()
	hub, err := stream.New(stream.Config{ClusterIDs: []string{testcluster.ClusterID}}, streamMetrics)
	if err != nil {
		return nil, err
	}
	f.hub, f.streamMetrics = hub, streamMetrics
	if _, err := querycatalog.LoadPath(""); err != nil {
		return nil, err
	}
	for i := range f.api {
		resolver, resolveErr := auth.NewResolver(ctx, auth.Config{
			IssuerURL: idp.URL(), Audience: "auth-session-e2e", ClusterID: testcluster.ClusterID,
			ClusterName: store.ClusterName(), HTTPClient: client,
			Session: auth.SessionConfig{Enabled: true, PublicOrigin: publicOrigin,
				RedirectURI: publicOrigin + "/api/v1/auth/callback", ClientID: "dashboard-e2e",
				EncryptionKey: cfg.EncryptionKey, RedisAddr: cfg.RedisAddr,
				TransactionTTL: 2 * time.Minute, IdleTTL: 10 * time.Minute, AbsoluteTTL: time.Hour,
				RefreshSkew: 2 * time.Minute, RedisTimeout: time.Second},
		}, logger)
		if resolveErr != nil {
			return nil, resolveErr
		}
		f.resolver[i] = resolver
		api := httpapi.NewServer(httpapi.Deps{Store: store, Metrics: source, Logs: source, Alerts: alerts,
			Topology: source, Resolver: resolver, Logger: logger, Now: time.Now,
			Stream: hub, StreamMetrics: streamMetrics,
			Version: contract.VersionInfo{Version: "auth-e2e", Commit: "none", BuildDate: time.Now().UTC().Format(time.RFC3339)}})
		replica := i
		f.api[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.replica[replica].Add(1)
			api.ServeHTTP(w, r)
		}))
		f.APIURLs[i] = f.api[i].URL
		target, parseErr := url.Parse(f.api[i].URL)
		if parseErr != nil {
			return nil, parseErr
		}
		apiProxies[i] = httputil.NewSingleHostReverseProxy(target)
		apiProxies[i].FlushInterval = -1
		apiProxies[i].ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
	}
	ready.Store(true)
	ok = true
	return f, nil
}

func (f *AuthSessionFixture) Close() {
	f.closeOnce.Do(func() {
		if f.server != nil {
			f.server.Close()
		}
		for _, server := range f.api {
			if server != nil {
				server.Close()
			}
		}
		if f.alerts != nil {
			_ = f.alerts.Close()
		}
		for _, resolver := range f.resolver {
			if resolver != nil {
				_ = resolver.Close()
			}
		}
		if f.idp != nil {
			f.idp.Close()
		}
		if f.hub != nil {
			f.hub.Close()
		}
	})
}

type authCode struct{ Challenge, Nonce string }
type codeIDP struct {
	server                      *httptest.Server
	key                         *rsa.PrivateKey
	mu                          sync.Mutex
	codes                       map[string]authCode
	refresh                     map[string]bool
	callback                    string
	authorizationURL            string
	authorize, token, refreshed atomic.Uint64
	nextTokenTTL                atomic.Int64
	nextRefreshDelay            atomic.Int64
	nextViewer                  atomic.Bool
}

func startCodeIDP() (*codeIDP, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	m := &codeIDP{key: key, codes: map[string]authCode{}, refresh: map[string]bool{}}
	m.nextTokenTTL.Store(int64(5 * time.Minute))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", m.discovery)
	mux.HandleFunc("GET /jwks", m.jwks)
	mux.HandleFunc("GET /authorize", m.handleAuthorize)
	mux.HandleFunc("POST /token", m.handleToken)
	m.server = httptest.NewTLSServer(mux)
	return m, nil
}
func (m *codeIDP) URL() string                    { return m.server.URL }
func (m *codeIDP) Certificate() *x509.Certificate { return m.server.Certificate() }
func (m *codeIDP) writeTLSFiles(certFile, keyFile string) error {
	certificate := m.server.TLS.Certificates[0]
	if len(certificate.Certificate) == 0 {
		return errors.New("fixture TLS certificate missing")
	}
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600)
}
func (m *codeIDP) Close() { m.server.Close() }
func (m *codeIDP) discovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	authorizationURL := m.authorizationURL
	if authorizationURL == "" {
		authorizationURL = m.URL() + "/authorize"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"issuer": m.URL(), "jwks_uri": m.URL() + "/jwks", "authorization_endpoint": authorizationURL, "token_endpoint": m.URL() + "/token"})
}
func (m *codeIDP) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := &m.key.PublicKey
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "e2e-1", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(bigEndian(pub.E))}}})
}
func (m *codeIDP) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "https" || q.Get("client_id") != "dashboard-e2e" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(q.Get("state")) != 43 || len(q.Get("nonce")) != 43 || len(q.Get("code_challenge")) != 43 {
		http.Error(w, "invalid authorize request", 400)
		return
	}
	code := randomFixtureValue()
	m.mu.Lock()
	m.codes[code] = authCode{Challenge: q.Get("code_challenge"), Nonce: q.Get("nonce")}
	m.mu.Unlock()
	m.authorize.Add(1)
	values := redirect.Query()
	values.Set("code", code)
	values.Set("state", q.Get("state"))
	redirect.RawQuery = values.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}
func (m *codeIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if r.Form.Get("client_id") != "dashboard-e2e" || len(r.Form["client_id"]) != 1 {
		http.Error(w, "invalid client", http.StatusBadRequest)
		return
	}
	grant := r.Form.Get("grant_type")
	var nonce string
	switch grant {
	case "authorization_code":
		if !exactFormKeys(r.Form, "grant_type", "code", "redirect_uri", "client_id", "code_verifier") || r.Form.Get("redirect_uri") != m.callback {
			http.Error(w, "invalid token request", http.StatusBadRequest)
			return
		}
		code := r.Form.Get("code")
		m.mu.Lock()
		tx, ok := m.codes[code]
		delete(m.codes, code)
		m.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		if !ok || subtle.ConstantTimeCompare([]byte(challenge), []byte(tx.Challenge)) != 1 {
			http.Error(w, "invalid grant", 400)
			return
		}
		nonce = tx.Nonce
		m.token.Add(1)
	case "refresh_token":
		if !exactFormKeys(r.Form, "grant_type", "refresh_token", "client_id") {
			http.Error(w, "invalid token request", http.StatusBadRequest)
			return
		}
		old := r.Form.Get("refresh_token")
		m.mu.Lock()
		ok := m.refresh[old]
		delete(m.refresh, old)
		m.mu.Unlock()
		if !ok {
			http.Error(w, "invalid grant", 400)
			return
		}
		if delay := time.Duration(m.nextRefreshDelay.Swap(0)); delay > 0 {
			time.Sleep(delay)
		}
		m.refreshed.Add(1)
	default:
		http.Error(w, "unsupported grant", 400)
		return
	}
	refresh := randomFixtureValue()
	m.mu.Lock()
	m.refresh[refresh] = true
	m.mu.Unlock()
	id, err := m.jwt(nonce)
	if err != nil {
		http.Error(w, "unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"id_token": id, "access_token": "access-" + randomFixtureValue(), "refresh_token": refresh, "token_type": "Bearer", "expires_in": 300})
}
func (m *codeIDP) jwt(nonce string) (string, error) {
	now := time.Now()
	ttl := time.Duration(m.nextTokenTTL.Swap(int64(5 * time.Minute)))
	roles := []string{"platform.admin"}
	if m.nextViewer.Swap(false) {
		roles = []string{"platform.viewer"}
	}
	return m.signed(map[string]any{"iss": m.URL(), "sub": "browser-admin", "aud": "dashboard-e2e", "exp": now.Add(ttl).Unix(), "nbf": now.Add(-time.Minute).Unix(), "iat": now.Unix(), "nonce": nonce, "preferred_username": "Browser Admin", "roles": roles})
}
func (m *codeIDP) signed(claims map[string]any) (string, error) {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "e2e-1"})
	p, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	d := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, m.key, crypto.SHA256, d[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
func (m *codeIDP) evidence() map[string]uint64 {
	return map[string]uint64{"authorize": m.authorize.Load(), "token": m.token.Load(), "refresh": m.refreshed.Load()}
}
func randomFixtureValue() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func bigEndian(e int) []byte {
	out := []byte{byte(e >> 16), byte(e >> 8), byte(e)}
	for len(out) > 1 && out[0] == 0 {
		out = out[1:]
	}
	return out
}

func exactFormKeys(form url.Values, keys ...string) bool {
	if len(form) != len(keys) {
		return false
	}
	for _, key := range keys {
		if len(form[key]) != 1 {
			return false
		}
	}
	return true
}

func metricUint(metrics, name string) uint64 {
	for _, line := range strings.Split(metrics, "\n") {
		var key string
		var value uint64
		if _, err := fmt.Sscanf(line, "%s %d", &key, &value); err == nil && key == name {
			return value
		}
	}
	return 0
}
