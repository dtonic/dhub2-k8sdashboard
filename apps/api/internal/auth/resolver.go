// scope.Resolver 구현입니다. 핸들러는 이 뒤를 보지 않습니다.
package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

// Config는 OIDC 검증 설정입니다.
type Config struct {
	// IssuerURL은 OIDC 발급자입니다. 예: https://login.microsoftonline.com/<tenant>/v2.0
	IssuerURL string
	// Audience는 이 API의 client id입니다. 비우면 aud 검사를 건너뜁니다 —
	// 개발 편의용이며 운영에서는 반드시 설정합니다.
	Audience string
	// RolesClaim은 역할이 실린 클레임 이름입니다. 기본 "roles"입니다.
	RolesClaim string
	// Leeway는 시계 오차 허용입니다. 기본 60초입니다.
	Leeway time.Duration
	// JWKSMinRefresh는 모르는 kid로 인한 JWKS 재조회의 하한입니다. 기본 5분입니다.
	JWKSMinRefresh time.Duration
	// HTTPTimeout은 discovery·JWKS 요청 상한입니다. 기본 10초입니다.
	HTTPTimeout time.Duration

	// ClusterID·ClusterName은 역할 인자를 이 프로세스와 대조할 때 씁니다.
	ClusterID   string
	ClusterName string

	// Now는 테스트에서 시간을 고정합니다.
	Now func() time.Time
}

// Resolver는 Bearer JWT를 검증하고 Scope를 계산합니다. scope.Resolver 구현입니다.
type Resolver struct {
	cfg    Config
	keys   *keyStore
	logger *slog.Logger
}

// NewResolver는 issuer discovery로 JWKS 위치를 찾고 최초 키를 받아옵니다.
// 시작 시점에 실패하면 서버를 띄우지 않습니다 — 인증이 깨진 채로 뜬 서버는
// 전부 401이 되어 장애처럼 보입니다.
func NewResolver(ctx context.Context, cfg Config, logger *slog.Logger) (*Resolver, error) {
	if cfg.IssuerURL == "" {
		return nil, fmt.Errorf("OIDC issuer가 비어 있습니다")
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

	client := &http.Client{Timeout: cfg.HTTPTimeout}
	jwksURI, err := discover(ctx, client, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	ks := &keyStore{
		httpClient: client,
		jwksURI:    jwksURI,
		minRefresh: cfg.JWKSMinRefresh,
		now:        cfg.Now,
	}
	if err := ks.refresh(ctx); err != nil {
		return nil, fmt.Errorf("JWKS를 받아오지 못했습니다: %w", err)
	}
	return &Resolver{cfg: cfg, keys: ks, logger: logger}, nil
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
		return scope.Scope{}, fmt.Errorf("%w: Authorization 헤더 없음", ErrInvalidToken)
	}
	claims, err := verifyJWT(raw, func(kid string) (any, bool) {
		return r.keys.get(req.Context(), kid)
	}, r.cfg.IssuerURL, r.cfg.Audience, r.cfg.RolesClaim, r.cfg.Leeway, r.cfg.Now())
	if err != nil {
		// 이유는 운영 로그에만 — 응답으로는 401 하나입니다. 토큰 원문은 남기지 않습니다.
		r.logger.Warn("토큰 검증 실패", "reason", err.Error())
		return scope.Scope{}, err
	}
	_, sc := ScopeFor(claims, r.cfg.ClusterID, r.cfg.ClusterName)
	return sc, nil
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
