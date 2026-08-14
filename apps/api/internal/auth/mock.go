// 로컬 개발용 mock identity provider입니다. (#10 작업 범위)
//
// AUTH_MODE=mock일 때 loopback에 떠서 실제 OIDC provider처럼 discovery·JWKS를
// 제공하고, /token으로 원하는 역할의 서명된 토큰을 발급합니다. 검증 경로는
// 운영과 **완전히 같습니다** — mock이 우회하는 것은 IdP뿐이고, JWT 검증·역할
// 매핑·Scope 강제는 전부 실제 코드가 돕니다.
//
// 절대 운영에 노출하면 안 됩니다. 누구나 어떤 역할의 토큰이든 만들 수 있습니다.
// 기본 바인드가 127.0.0.1인 이유입니다.
package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// MockIDP는 개발용 발급자입니다.
type MockIDP struct {
	Issuer string

	key      *rsa.PrivateKey
	kid      string
	audience string
	server   *http.Server
	now      func() time.Time
}

// StartMockIDP는 addr(비우면 127.0.0.1의 임의 포트)에 mock IdP를 띄웁니다.
func StartMockIDP(addr, audience string, now func() time.Time) (*MockIDP, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if now == nil {
		now = time.Now
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mock IdP 리스너 실패: %w", err)
	}

	m := &MockIDP{
		Issuer:   "http://" + ln.Addr().String(),
		key:      key,
		kid:      "mock-1",
		audience: audience,
		now:      now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", m.handleDiscovery)
	mux.HandleFunc("GET /jwks", m.handleJWKS)
	mux.HandleFunc("POST /token", m.handleToken)
	m.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go m.server.Serve(ln) //nolint:errcheck // Close 시 ErrServerClosed

	return m, nil
}

// Rotate는 서명 키를 새로 만듭니다. 키 회전 시나리오(모르는 kid → JWKS 재조회)를
// 로컬에서 재현할 때 씁니다.
func (m *MockIDP) Rotate() error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	m.key = key
	m.kid = m.kid + "r"
	return nil
}

// Close는 리스너를 내립니다.
func (m *MockIDP) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := m.server.Shutdown(ctx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (m *MockIDP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"issuer":   m.Issuer,
		"jwks_uri": m.Issuer + "/jwks",
	})
}

func (m *MockIDP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := m.key.Public().(*rsa.PublicKey)
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"keys": []map[string]string{{
			"kty": "RSA", "kid": m.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(bigEndianE(pub.E)),
		}},
	})
}

// handleToken은 개발 토큰을 발급합니다.
//
//	curl -X POST 'http://127.0.0.1:.../token?sub=dev&roles=platform.admin'
func (m *MockIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sub := q.Get("sub")
	if sub == "" {
		sub = "dev-user"
	}
	var roles []string
	if v := q.Get("roles"); v != "" {
		roles = strings.Split(v, ",")
	}
	token, err := m.Token(sub, roles, 12*time.Hour)
	if err != nil {
		http.Error(w, "토큰 발급 실패", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"access_token": token, "token_type": "Bearer"}) //nolint:errcheck
}

// Token은 서명된 개발 토큰을 만듭니다. 테스트에서도 이 경로를 씁니다.
func (m *MockIDP) Token(sub string, roles []string, ttl time.Duration) (string, error) {
	return m.SignedToken(map[string]any{
		"iss":   m.Issuer,
		"sub":   sub,
		"aud":   m.audience,
		"exp":   m.now().Add(ttl).Unix(),
		"nbf":   m.now().Add(-time.Minute).Unix(),
		"roles": roles,
	})
}

// SignedToken은 임의 클레임을 RS256으로 서명합니다. 만료·발급자 오류 같은
// 비정상 토큰을 테스트에서 만들 때도 씁니다.
func (m *MockIDP) SignedToken(claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": m.kid})
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, m.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func bigEndianE(e int) []byte {
	out := []byte{byte(e >> 16), byte(e >> 8), byte(e)}
	for len(out) > 1 && out[0] == 0 {
		out = out[1:]
	}
	return out
}
