// Package auth는 OIDC 인증과 역할 → Scope 계산입니다. (#10)
//
// 표준 OIDC provider(MS Entra 포함)가 발급한 Bearer JWT를 검증하고,
// 토큰의 역할 클레임에서 이 프로세스가 노출할 Cluster/Namespace Scope를
// 계산합니다. 핸들러는 여전히 scope.Resolver 뒤만 봅니다 — 인증 방식이
// 바뀌어도 화면 코드는 바뀌지 않습니다.
//
// 외부 JWT 라이브러리를 쓰지 않습니다. 필요한 것은 compact JWS 검증
// (RS256/ES256)뿐이고, 표준 라이브러리 crypto로 충분합니다. 의존성이
// 늘수록 공급망과 업그레이드 비용이 늘어납니다.
//
// 검증 순서 — 어느 하나라도 실패하면 토큰 전체가 무효입니다.
//  1. compact JWS 구조와 base64url 해석
//  2. alg allowlist (RS256·ES256 — "none"은 구조적으로 거절)
//  3. kid로 JWKS 공개키 조회 (모르는 kid는 1회 갱신 후 재시도)
//  4. 서명 검증
//  5. iss·aud 일치, exp·nbf 시각 (leeway 허용)
package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// ErrInvalidToken은 검증 실패입니다. 이유는 로그에만 남기고 응답에는 싣지
// 않습니다 — 공격자에게 어느 단계에서 걸렸는지 알려줄 이유가 없습니다.
var ErrInvalidToken = errors.New("토큰을 검증할 수 없습니다")

// Claims는 검증이 끝난 토큰에서 꺼낸 값입니다.
type Claims struct {
	Issuer   string
	Subject  string
	Audience []string
	Email    string
	Username string
	// Roles는 역할 클레임의 값입니다. 문자열 배열 또는 공백 구분 문자열을 받습니다.
	Roles []string

	ExpiresAt time.Time
	NotBefore time.Time
}

// rawClaims는 JSON 해석용 중간 형태입니다.
type rawClaims struct {
	Issuer   string          `json:"iss"`
	Subject  string          `json:"sub"`
	Audience json.RawMessage `json:"aud"`
	Email    string          `json:"email"`
	Username string          `json:"preferred_username"`
	Exp      float64         `json:"exp"`
	Nbf      float64         `json:"nbf"`
}

type jwsHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// verifyJWT는 서명과 시각·발급자·대상 클레임을 검증합니다.
// keyOf는 (kid, alg)에 맞는 공개키를 돌려줍니다.
func verifyJWT(token string, keyOf func(kid string) (any, bool), issuer, audience, rolesClaim string, leeway time.Duration, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: 형식", ErrInvalidToken)
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: 헤더", ErrInvalidToken)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: 본문", ErrInvalidToken)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: 서명", ErrInvalidToken)
	}

	var header jwsHeader
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return Claims{}, fmt.Errorf("%w: 헤더", ErrInvalidToken)
	}
	// alg allowlist — 토큰이 고른 알고리즘을 따라가지 않습니다.
	// "none"도 HS256(비밀키 혼동 공격)도 여기서 끝납니다.
	if header.Alg != "RS256" && header.Alg != "ES256" {
		return Claims{}, fmt.Errorf("%w: 허용되지 않은 alg %q", ErrInvalidToken, header.Alg)
	}

	key, ok := keyOf(header.Kid)
	if !ok {
		return Claims{}, fmt.Errorf("%w: 알 수 없는 kid", ErrInvalidToken)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch pub := key.(type) {
	case *rsa.PublicKey:
		if header.Alg != "RS256" {
			return Claims{}, fmt.Errorf("%w: 키와 alg 불일치", ErrInvalidToken)
		}
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
			return Claims{}, fmt.Errorf("%w: 서명 불일치", ErrInvalidToken)
		}
	case *ecdsa.PublicKey:
		if header.Alg != "ES256" {
			return Claims{}, fmt.Errorf("%w: 키와 alg 불일치", ErrInvalidToken)
		}
		// JWT의 ES256 서명은 r||s 64바이트 원시 형식입니다.
		if len(sig) != 64 {
			return Claims{}, fmt.Errorf("%w: 서명 길이", ErrInvalidToken)
		}
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(pub, digest[:], r, s) {
			return Claims{}, fmt.Errorf("%w: 서명 불일치", ErrInvalidToken)
		}
	default:
		return Claims{}, fmt.Errorf("%w: 지원하지 않는 키 형식", ErrInvalidToken)
	}

	var rc rawClaims
	if err := json.Unmarshal(payloadRaw, &rc); err != nil {
		return Claims{}, fmt.Errorf("%w: 클레임", ErrInvalidToken)
	}
	c := Claims{
		Issuer:   rc.Issuer,
		Subject:  rc.Subject,
		Email:    rc.Email,
		Username: rc.Username,
		Audience: parseAudience(rc.Audience),
	}
	if rc.Exp > 0 {
		c.ExpiresAt = time.Unix(int64(rc.Exp), 0)
	}
	if rc.Nbf > 0 {
		c.NotBefore = time.Unix(int64(rc.Nbf), 0)
	}

	if c.Issuer != issuer {
		return Claims{}, fmt.Errorf("%w: 발급자 불일치", ErrInvalidToken)
	}
	if audience != "" && !contains(c.Audience, audience) {
		return Claims{}, fmt.Errorf("%w: 대상 불일치", ErrInvalidToken)
	}
	if c.ExpiresAt.IsZero() || now.After(c.ExpiresAt.Add(leeway)) {
		return Claims{}, fmt.Errorf("%w: 만료", ErrInvalidToken)
	}
	if !c.NotBefore.IsZero() && now.Add(leeway).Before(c.NotBefore) {
		return Claims{}, fmt.Errorf("%w: 아직 유효하지 않음", ErrInvalidToken)
	}

	c.Roles = parseRoles(payloadRaw, rolesClaim)
	return c, nil
}

// parseAudience는 aud가 문자열이든 배열이든 받습니다. OIDC 명세가 둘 다 허용합니다.
func parseAudience(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

// parseRoles는 역할 클레임을 꺼냅니다. 배열(["a","b"])과
// 공백 구분 문자열("a b") 둘 다 받습니다 — provider마다 다릅니다.
func parseRoles(payload []byte, claim string) []string {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(payload, &all); err != nil {
		return nil
	}
	raw, ok := all[claim]
	if !ok {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var joined string
	if err := json.Unmarshal(raw, &joined); err == nil && joined != "" {
		return strings.Fields(joined)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
