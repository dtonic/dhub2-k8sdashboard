package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/observability"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

// 감사 로그입니다. (#10)
//
// 화면 요청마다 고정 route, bounded Scope 요약, 결과와 등록 catalog queryRef 실행 계획을 남깁니다.
// cache miss에서는 어댑터가 실제 호출한 ref도 같은 bounded collector에 합칩니다.
// Subject, 원문 경로, 쿼리 파라미터 이름·값은 기록하지 않습니다.
//
// 마스킹 정책 — 감사 로그에 절대 남지 않는 것:
//   - Authorization 헤더와 토큰 원문 (아예 읽지 않습니다)
//   - 모든 path 값과 query 파라미터 이름·값(q, cursor 포함; 필드 자체 미기록)
//
// 로그 본문의 민감정보는 datasource/mask가, 여기는 **요청 메타데이터**의
// 민감정보를 맡습니다.

// auditSkip은 감사에서 제외할 경로입니다. probe가 로그를 도배하면
// 정작 봐야 할 기록이 묻힙니다.
var auditSkip = map[string]bool{"/healthz": true, "/readyz": true, "/version": true, "/metrics": true}

func (s *Server) audit(r *http.Request, sc scope.Scope, status int, started time.Time) {
	if auditSkip[r.URL.Path] {
		return
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

	args := []any{
		"requestId", requestIDFrom(r.Context()),
		"route", routeName(s.mux, r),
		"scope", scopeText(sc),
		"decision", decision,
		"status", status,
		"durMs", s.deps.Now().Sub(started).Milliseconds(),
	}
	if trace := observability.TraceFrom(r.Context()); trace != nil && status < 400 {
		refs, overflow := trace.Summary()
		args = append(args, "queryRefs", refs, "queryRefsOverflow", overflow)
	}
	s.deps.Logger.Info("audit", args...)
}

// scopeText exposes only bounded authorization shape, never subjects or namespace names.
func scopeText(sc scope.Scope) string {
	if len(sc.Clusters) == 0 {
		return "-"
	}
	limit, all, namespaces := len(sc.Clusters), 0, 0
	if limit > 8 {
		limit = 8
	}
	for _, c := range sc.Clusters[:limit] {
		if c.All {
			all++
		} else {
			namespaces += len(c.Namespaces)
		}
	}
	return "clusters=" + strconv.Itoa(len(sc.Clusters)) + " all=" + strconv.Itoa(all) + " namespaces=" + strconv.Itoa(namespaces) + " overflow=" + strconv.FormatBool(len(sc.Clusters) > limit)
}
