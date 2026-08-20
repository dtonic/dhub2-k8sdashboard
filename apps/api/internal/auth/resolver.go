// scope.Resolver 구현입니다. 핸들러는 이 뒤를 보지 않습니다.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

// Config는 OIDC 검증 설정입니다.
type Config struct {
	// IssuerURL은 OIDC 발급자입니다. 예: https://login.microsoftonline.com/<tenant>/v2.0
	IssuerURL string
	// Audience는 이 API의 client id입니다. 빈 값은 안전하지 않으므로 거절합니다.
	Audience string
	// RolesClaim은 역할이 실린 클레임 이름입니다. 기본 "roles"입니다.
	RolesClaim string
	// RoleMap은 IdP 역할 이름 → 내부 역할 이름 변환표입니다. 비우면 변환하지 않습니다.
	RoleMap map[string]string
	// UserinfoRoles가 켜지면 역할 클레임이 빈 Bearer 토큰에 한해 issuer의
	// userinfo 엔드포인트에서 역할을 보충 조회합니다(access token에 identity
	// claim을 싣지 않는 Dhub2.0 같은 provider용). 조회 실패는 fail-closed입니다.
	UserinfoRoles bool
	// Leeway는 시계 오차 허용입니다. 기본 60초입니다.
	Leeway time.Duration
	// JWKSMinRefresh는 모르는 kid로 인한 JWKS 재조회의 하한입니다. 기본 5분입니다.
	JWKSMinRefresh time.Duration
	// HTTPTimeout은 discovery·JWKS 요청 상한입니다. 기본 10초입니다.
	HTTPTimeout time.Duration
	// HTTPClient optionally supplies trusted roots for private enterprise IdPs.
	// Redirect handling and the configured timeout are still enforced below.
	HTTPClient *http.Client

	// ClusterID·ClusterName은 역할 인자를 이 프로세스와 대조할 때 씁니다.
	ClusterID   string
	ClusterName string
	Clusters    []scope.Cluster
	Central     bool

	// Now는 테스트에서 시간을 고정합니다.
	Now func() time.Time

	// Session is an explicit, server-side browser session opt-in.
	Session SessionConfig
}

// Resolver는 Bearer JWT를 검증하고 Scope를 계산합니다. scope.Resolver 구현입니다.
type Resolver struct {
	cfg       Config
	keys      *keyStore
	userinfo  *userinfoRoles
	logger    *slog.Logger
	flow      *sessionFlow
	closeOnce sync.Once
	closeErr  error
}

// Close releases resources owned by an enabled browser-session flow. It is
// nil-safe and idempotent so startup rollback and normal shutdown can share it.
func (r *Resolver) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		if r.flow != nil && r.flow.redis != nil {
			r.closeErr = r.flow.redis.Close()
		}
	})
	return r.closeErr
}

// NewResolver는 issuer discovery로 JWKS 위치를 찾고 최초 키를 받아옵니다.
// 시작 시점에 실패하면 서버를 띄우지 않습니다 — 인증이 깨진 채로 뜬 서버는
// 전부 401이 되어 장애처럼 보입니다.
func NewResolver(ctx context.Context, cfg Config, logger *slog.Logger) (*Resolver, error) {
	if cfg.IssuerURL == "" {
		return nil, fmt.Errorf("OIDC issuer가 비어 있습니다")
	}
	if cfg.Audience == "" {
		return nil, fmt.Errorf("OIDC audience가 비어 있습니다")
	}
	if err := validateIssuerURL(cfg.IssuerURL); err != nil {
		return nil, err
	}
	if cfg.RolesClaim == "" {
		cfg.RolesClaim = "roles"
	}
	if cfg.Leeway <= 0 {
		cfg.Leeway = time.Minute
	}
	if cfg.JWKSMinRefresh <= 0 {
		cfg.JWKSMinRefresh = 5 * time.Minute
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	} else {
		clone := *client
		client = &clone
	}
	client.Timeout = cfg.HTTPTimeout
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("OIDC redirect limit exceeded")
		}
		return validateProviderURL(req.URL, cfg.IssuerURL)
	}
	doc, err := discoverDoc(ctx, client, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	ks := &keyStore{
		httpClient: client,
		jwksURI:    doc.JWKSURI,
		minRefresh: cfg.JWKSMinRefresh,
		now:        cfg.Now,
	}
	if err := ks.refresh(ctx); err != nil {
		return nil, fmt.Errorf("JWKS를 받아오지 못했습니다: %w", err)
	}
	var userinfo *userinfoRoles
	if cfg.UserinfoRoles {
		if doc.UserinfoEndpoint == "" {
			return nil, fmt.Errorf("OIDC_USERINFO_ROLES에는 issuer discovery의 userinfo_endpoint가 필요합니다")
		}
		endpointURL, err := url.Parse(doc.UserinfoEndpoint)
		if err != nil {
			return nil, fmt.Errorf("userinfo_endpoint가 유효한 URL이 아닙니다: %w", err)
		}
		if err := validateProviderURL(endpointURL, cfg.IssuerURL); err != nil {
			return nil, fmt.Errorf("userinfo_endpoint 검증 실패: %w", err)
		}
		userinfo = &userinfoRoles{
			client:   client,
			endpoint: doc.UserinfoEndpoint,
			claim:    cfg.RolesClaim,
			now:      cfg.Now,
			cache:    map[[32]byte]userinfoCached{},
		}
	}
	r := &Resolver{cfg: cfg, keys: ks, userinfo: userinfo, logger: logger}
	if cfg.Session.Enabled {
		tokenClient := *client
		tokenClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		flow, err := newSessionFlow(cfg, doc, &tokenClient, ks)
		if err != nil {
			return nil, err
		}
		r.flow = flow
	}
	return r, nil
}

// Resolve는 요청의 Bearer 토큰에서 Scope를 계산합니다. scope.Resolver 구현입니다.
//
// 실패는 전부 ErrInvalidToken 계열이고 서버는 401로 접습니다. 검증에 통과했지만
// 역할이 없는 사용자는 **빈 Scope로 성공**합니다 — 그 뒤의 화면 요청이 403이
// 됩니다. 이 구분이 완료 기준의 "401/403 구분"입니다. (#10)
//
// 실패 이유는 로그에만 남기고, 토큰 원문은 어디에도 남기지 않습니다.
func (r *Resolver) Resolve(req *http.Request) (scope.Scope, error) {
	raw, ok := bearerToken(req)
	if !ok {
		// A present Authorization header is authoritative. Malformed/unsupported
		// schemes must never downgrade to a valid browser cookie.
		if req.Header.Get("Authorization") != "" {
			return scope.Scope{}, ErrInvalidToken
		}
		if r.flow == nil {
			return scope.Scope{}, fmt.Errorf("%w: Authorization 헤더 없음", ErrInvalidToken)
		}
		claims, err := r.flow.resolve(req)
		if err != nil {
			reason := "invalid"
			if errors.Is(err, ErrSessionUnavailable) {
				reason = "store_unavailable"
			}
			r.logger.Warn("browser_session_auth_failed", "reason", reason)
			return scope.Scope{}, err
		}
		return r.scopeFor(claims), nil
	}
	claims, err := verifyJWT(raw, func(kid string) (any, bool) {
		return r.keys.get(req.Context(), kid)
	}, r.cfg.IssuerURL, r.cfg.Audience, r.cfg.RolesClaim, r.cfg.Leeway, r.cfg.Now())
	if err != nil {
		// 이유는 운영 로그에만 — 응답으로는 401 하나입니다. 토큰 원문은 남기지 않습니다.
		r.logger.Warn("토큰 검증 실패", "reason", err.Error())
		return scope.Scope{}, err
	}
	claims.Roles = MapRoles(claims.Roles, r.cfg.RoleMap)
	if len(claims.Roles) == 0 && r.userinfo != nil {
		fetched, err := r.userinfo.rolesFor(req.Context(), raw, claims.ExpiresAt)
		if err != nil {
			// 역할을 확인할 수 없으면 빈 Scope(403)가 아니라 인증 실패(401)로 접습니다 —
			// upstream 장애와 권한 없음을 같은 화면으로 만들지 않기 위해서입니다.
			r.logger.Warn("userinfo 역할 조회 실패", "reason", err.Error())
			return scope.Scope{}, ErrInvalidToken
		}
		claims.Roles = MapRoles(fetched, r.cfg.RoleMap)
	}
	return r.scopeFor(claims), nil
}

func (r *Resolver) scopeFor(claims Claims) scope.Scope {
	var sc scope.Scope
	if r.cfg.Central {
		_, sc = ScopeForCentral(claims, r.cfg.Clusters)
	} else {
		_, sc = ScopeFor(claims, r.cfg.ClusterID, r.cfg.ClusterName)
	}
	return sc
}

// bearerToken은 Authorization 헤더에서 토큰을 꺼냅니다. 대소문자를 가리지 않습니다.
func bearerToken(req *http.Request) (string, bool) {
	h := req.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
