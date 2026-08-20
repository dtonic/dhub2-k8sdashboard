// OIDC discovery와 JWKS 공개키 캐시입니다.
//
// 키는 메모리에 캐시하고, 모르는 kid를 만나면 **갱신 간격 하한** 안에서 한 번만
// 다시 받아옵니다. 하한이 없으면 위조 kid를 대량으로 보내는 것만으로 JWKS
// 엔드포인트를 두들기게 할 수 있습니다.
package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	pathpkg "path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxJWKSBytes는 JWKS 응답 크기 상한입니다.
const maxJWKSBytes = 1 << 20

type keyStore struct {
	httpClient *http.Client
	jwksURI    string

	minRefresh time.Duration
	now        func() time.Time

	mu          sync.Mutex
	keys        map[string]any // kid → *rsa.PublicKey | *ecdsa.PublicKey
	lastRefresh time.Time
}

// discoveryDoc은 발급자 메타데이터에서 이 서버가 쓰는 필드만 담습니다.
// Bearer 검증은 jwks_uri만, 브라우저 세션 code flow는 authorize/token 엔드포인트까지 씁니다.
type discoveryDoc struct {
	Issuer                string `json:"issuer"`
	JWKSURI               string `json:"jwks_uri"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// discover는 issuer의 /.well-known/openid-configuration에서 jwks_uri를 찾습니다.
func discover(ctx context.Context, client *http.Client, issuer string) (string, error) {
	doc, err := discoverDoc(ctx, client, issuer)
	if err != nil {
		return "", err
	}
	return doc.JWKSURI, nil
}

// discoverDoc은 발급자 메타데이터를 받아 jwks_uri의 transport 경계까지 검증합니다.
// authorize/token 엔드포인트 검증은 그 값을 실제로 쓰는 code flow 쪽에서 합니다.
func discoverDoc(ctx context.Context, client *http.Client, issuer string) (discoveryDoc, error) {
	discoveryURL := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return discoveryDoc{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return discoveryDoc{}, fmt.Errorf("OIDC discovery에 연결할 수 없습니다: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			target, err := res.Location()
			if err == nil {
				if err := validateProviderURL(target, issuer); err != nil {
					return discoveryDoc{}, fmt.Errorf("안전하지 않은 discovery redirect: %w", err)
				}
			}
		}
		return discoveryDoc{}, fmt.Errorf("OIDC discovery 응답 %d", res.StatusCode)
	}
	var doc discoveryDoc
	if err := json.NewDecoder(io.LimitReader(res.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return discoveryDoc{}, err
	}
	// 발급자 문서의 issuer가 설정과 다르면 잘못된 곳을 보고 있는 것입니다.
	if doc.Issuer != issuer {
		return discoveryDoc{}, fmt.Errorf("discovery issuer가 설정과 다릅니다")
	}
	if doc.JWKSURI == "" {
		return discoveryDoc{}, fmt.Errorf("jwks_uri가 없습니다")
	}
	jwksURL, err := url.Parse(doc.JWKSURI)
	if err != nil || !jwksURL.IsAbs() {
		return discoveryDoc{}, fmt.Errorf("jwks_uri는 절대 HTTP(S) URL이어야 합니다")
	}
	if err := validateProviderURL(jwksURL, issuer); err != nil {
		return discoveryDoc{}, fmt.Errorf("안전하지 않은 jwks_uri: %w", err)
	}
	return doc, nil
}

func validateIssuerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("OIDC issuer는 절대 HTTP(S) URL이어야 합니다")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !canonicalIssuerPath(u.Path) {
		return fmt.Errorf("OIDC issuer에는 userinfo, query, fragment, percent encoding, 비정규 경로를 사용할 수 없습니다")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("HTTP OIDC issuer는 loopback에서만 허용됩니다")
	}
	return nil
}

// validateProviderURL은 discovery/JWKS와 redirect 목적지의 transport 경계를 강제합니다.
func validateProviderURL(target *url.URL, issuer string) error {
	if target == nil || !target.IsAbs() || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return fmt.Errorf("provider URL은 절대 HTTP(S) URL이어야 합니다")
	}
	if target.User != nil || target.Fragment != "" {
		return fmt.Errorf("provider URL에는 userinfo 또는 fragment를 사용할 수 없습니다")
	}
	issuerURL, err := url.Parse(issuer)
	if err != nil {
		return err
	}
	if issuerURL.Scheme == "https" {
		if target.Scheme != "https" {
			return fmt.Errorf("HTTPS issuer의 HTTP downgrade는 허용되지 않습니다")
		}
		return nil // cross-host HTTPS JWKS/CDN은 정상 OIDC 배포 형태입니다.
	}
	if issuerURL.Scheme != "http" || !isLoopbackHost(issuerURL.Hostname()) || !isLoopbackHost(target.Hostname()) {
		return fmt.Errorf("HTTP provider는 loopback issuer와 loopback endpoint만 허용됩니다")
	}
	return nil
}

// canonicalIssuerPath permits an exact issuer path with one optional trailing
// slash. The configured spelling remains authoritative for discovery and JWT
// iss equality; alternate trailing-slash spellings are never normalized.
func canonicalIssuerPath(value string) bool {
	if value == "" || value == "/" {
		return true
	}
	return pathpkg.Clean(value) == strings.TrimSuffix(value, "/")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// get은 kid의 공개키를 돌려줍니다. 캐시에 없으면 갱신 하한 안에서 한 번 갱신합니다.
func (s *keyStore) get(ctx context.Context, kid string) (any, bool) {
	s.mu.Lock()
	if k, ok := s.keys[kid]; ok {
		s.mu.Unlock()
		return k, true
	}
	// 키 회전 직후일 수 있습니다. 하한이 지났으면 갱신을 시도합니다.
	if s.now().Sub(s.lastRefresh) < s.minRefresh {
		s.mu.Unlock()
		return nil, false
	}
	s.lastRefresh = s.now()
	s.mu.Unlock()

	if err := s.refresh(ctx); err != nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[kid]
	return k, ok
}

// refresh는 JWKS를 다시 받아 캐시를 통째로 바꿉니다.
func (s *keyStore) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.jwksURI, nil)
	if err != nil {
		return err
	}
	res, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS 응답 %d", res.StatusCode)
	}

	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return err
	}
	keys := map[string]any{}
	for _, k := range doc.Keys {
		pub, err := k.publicKey()
		if err != nil {
			continue // 지원하지 않는 키는 건너뜁니다. 전체 실패로 만들지 않습니다.
		}
		keys[k.Kid] = pub
	}
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
	return nil
}

// jwk는 JSON Web Key입니다. RSA(n·e)와 EC P-256(x·y)만 받습니다.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (k jwk) publicKey() (any, error) {
	if k.Use != "" && k.Use != "sig" {
		return nil, fmt.Errorf("서명용 키가 아닙니다")
	}
	switch k.Kty {
	case "RSA":
		if k.Alg != "" && k.Alg != "RS256" {
			return nil, fmt.Errorf("RSA 키의 alg가 RS256이 아닙니다")
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, err
		}
		modulus := new(big.Int).SetBytes(n)
		exponent := new(big.Int).SetBytes(e)
		if modulus.BitLen() < 2048 {
			return nil, fmt.Errorf("RSA modulus는 2048비트 이상이어야 합니다")
		}
		if !exponent.IsInt64() || exponent.Sign() <= 0 || exponent.Bit(0) == 0 || exponent.Cmp(big.NewInt(3)) < 0 {
			return nil, fmt.Errorf("RSA exponent가 올바르지 않습니다")
		}
		e64 := exponent.Int64()
		if strconv.IntSize == 32 && e64 > int64(^uint(0)>>1) {
			return nil, fmt.Errorf("RSA exponent가 int 범위를 넘습니다")
		}
		return &rsa.PublicKey{N: modulus, E: int(e64)}, nil
	case "EC":
		if k.Alg != "" && k.Alg != "ES256" {
			return nil, fmt.Errorf("EC 키의 alg가 ES256이 아닙니다")
		}
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("지원하지 않는 곡선 %q", k.Crv)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, err
		}
		y, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, err
		}
		pub := &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(x),
			Y:     new(big.Int).SetBytes(y),
		}
		if len(x) > 32 || len(y) > 32 || !pub.Curve.IsOnCurve(pub.X, pub.Y) {
			return nil, fmt.Errorf("EC point가 P-256 곡선 위에 있지 않습니다")
		}
		return pub, nil
	}
	return nil, fmt.Errorf("지원하지 않는 kty %q", k.Kty)
}
