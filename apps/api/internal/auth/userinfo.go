// Bearer 토큰의 역할을 issuer /userinfo에서 보충 조회합니다. (#10)
//
// Dhub2.0(dhub2-auth)처럼 access token에는 identity claim을 싣지 않고 /userinfo로
// 제공하는 provider를 위한 opt-in 경로입니다(OIDC_USERINFO_ROLES). 서명·발급자·
// 대상 검증이 끝난 토큰에 대해서만 호출하며, 조회 실패는 fail-closed(401)입니다.
// 응답은 토큰 해시 기준으로 짧게 캐시해 요청마다 upstream을 두드리지 않습니다.
package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	maxUserinfoBytes        = 1 << 20
	userinfoCacheTTL        = 2 * time.Minute
	maxUserinfoCacheEntries = 4096
)

type userinfoCached struct {
	roles  []string
	expiry time.Time
}

type userinfoRoles struct {
	client   *http.Client
	endpoint string
	claim    string
	now      func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]userinfoCached
}

// rolesFor는 검증이 끝난 Bearer 토큰으로 userinfo를 조회해 역할 클레임 값을
// 돌려줍니다. 캐시 만료는 짧은 TTL과 토큰 만료 중 이른 쪽입니다 — 폐기된
// 세션의 역할이 토큰 수명보다 오래 살아남지 않습니다.
func (u *userinfoRoles) rolesFor(ctx context.Context, rawToken string, tokenExp time.Time) ([]string, error) {
	key := sha256.Sum256([]byte(rawToken))
	now := u.now()

	u.mu.Lock()
	if c, ok := u.cache[key]; ok && now.Before(c.expiry) {
		roles := c.roles
		u.mu.Unlock()
		return roles, nil
	}
	u.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+rawToken)
	req.Header.Set("Accept", "application/json")
	res, err := u.client.Do(req)
	if err != nil {
		// 에러 문자열에 토큰이 실리지 않도록 원문 err은 감쌉니다(로그 전용).
		return nil, fmt.Errorf("userinfo 요청 실패: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo 응답 %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxUserinfoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("userinfo 응답을 읽지 못했습니다: %w", err)
	}
	if len(body) > maxUserinfoBytes {
		return nil, fmt.Errorf("userinfo 응답이 %d bytes를 넘습니다", maxUserinfoBytes)
	}

	// userinfo 응답도 flat JSON 객체이므로 토큰과 같은 역할 파서를 씁니다.
	roles := parseRoles(body, u.claim)

	expiry := now.Add(userinfoCacheTTL)
	if !tokenExp.IsZero() && tokenExp.Before(expiry) {
		expiry = tokenExp
	}
	u.mu.Lock()
	if len(u.cache) >= maxUserinfoCacheEntries {
		for k, c := range u.cache {
			if !now.Before(c.expiry) {
				delete(u.cache, k)
			}
		}
		if len(u.cache) >= maxUserinfoCacheEntries {
			// 여전히 가득이면 전부 비웁니다 — 캐시는 정합성 원천이 아니라 절약 수단입니다.
			u.cache = map[[32]byte]userinfoCached{}
		}
	}
	u.cache[key] = userinfoCached{roles: roles, expiry: expiry}
	u.mu.Unlock()
	return roles, nil
}
