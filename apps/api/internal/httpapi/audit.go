package httpapi

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

// 감사 로그입니다. (#10)
//
// 화면 요청마다 **누가 · 무엇을 · 어떤 범위로 · 결과가 무엇이었는지**를 남깁니다.
// 화면 하나 = 요청 하나 = 카탈로그의 고정된 쿼리 집합이므로(ADR 0002, #9),
// route와 쿼리 파라미터가 곧 실행된 queryRef 집합을 식별합니다.
//
// 마스킹 정책 — 감사 로그에 절대 남지 않는 것:
//   - Authorization 헤더와 토큰 원문 (아예 읽지 않습니다)
//   - 이름이 token·secret·password·key·auth를 담은 쿼리 파라미터의 값
//
// 로그 본문의 민감정보는 datasource/mask가, 여기는 **요청 메타데이터**의
// 민감정보를 맡습니다.

// auditSkip은 감사에서 제외할 경로입니다. probe가 로그를 도배하면
// 정작 봐야 할 기록이 묻힙니다.
var auditSkip = map[string]bool{"/healthz": true, "/readyz": true, "/version": true, "/metrics": true}

// sensitiveParam은 값을 가려야 하는 파라미터 이름입니다.
var sensitiveParam = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[_-]?key|auth)|^(?i:q|cursor)$`)

func (s *Server) audit(r *http.Request, sc scope.Scope, status int, started time.Time) {
	if auditSkip[r.URL.Path] {
		return
	}
	user := sc.Subject
	if user == "" {
		user = "-"
	}

	decision := "allowed"
	switch {
	case status == http.StatusUnauthorized:
		decision = "unauthorized"
	case status == http.StatusForbidden:
		decision = "forbidden"
	case status >= 400:
		decision = "error"
	}

	s.deps.Logger.Info("audit",
		"requestId", requestIDFrom(r.Context()),
		"user", user,
		"route", r.URL.Path,
		"params", sanitizeQuery(r.URL.Query()),
		"scope", scopeText(sc),
		"decision", decision,
		"status", status,
		"durMs", s.deps.Now().Sub(started).Milliseconds(),
	)
}

// sanitizeQuery는 쿼리 파라미터를 정렬된 한 줄로 만들되 민감한 값을 가립니다.
func sanitizeQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		if sensitiveParam.MatchString(k) {
			b.WriteString("[REDACTED]")
			continue
		}
		b.WriteString(strings.Join(q[k], ","))
	}
	return b.String()
}

// scopeText는 Scope를 감사 로그용 짧은 표기로 만듭니다. 예: seoul:payments+media, seoul:*
func scopeText(sc scope.Scope) string {
	if len(sc.Clusters) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(sc.Clusters))
	for _, c := range sc.Clusters {
		if c.All {
			parts = append(parts, c.ID+":*")
			continue
		}
		if len(c.Namespaces) == 0 {
			parts = append(parts, c.ID+":none")
			continue
		}
		parts = append(parts, c.ID+":"+strings.Join(c.Namespaces, "+"))
	}
	return strings.Join(parts, " ")
}
