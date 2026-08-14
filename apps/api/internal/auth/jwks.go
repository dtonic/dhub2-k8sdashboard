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
	"net/http"
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

// discover는 issuer의 /.well-known/openid-configuration에서 jwks_uri를 찾습니다.
func discover(ctx context.Context, client *http.Client, issuer string) (string, error) {
	url := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("OIDC discovery에 연결할 수 없습니다: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OIDC discovery 응답 %d", res.StatusCode)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return "", err
	}
	// 발급자 문서의 issuer가 설정과 다르면 잘못된 곳을 보고 있는 것입니다.
	if doc.Issuer != issuer {
		return "", fmt.Errorf("discovery issuer가 설정과 다릅니다")
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("jwks_uri가 없습니다")
	}
	return doc.JWKSURI, nil
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
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, err
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}, nil
	case "EC":
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
		return &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(x),
			Y:     new(big.Int).SetBytes(y),
		}, nil
	}
	return nil, fmt.Errorf("지원하지 않는 kty %q", k.Kty)
}
