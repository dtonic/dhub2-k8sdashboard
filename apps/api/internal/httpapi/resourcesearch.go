package httpapi

// 전역 리소스 검색 · 최근 항목 재해석 API (ADR 0023)
// --------------------------------------------------------------------------
// 조회 전용입니다. 두 경로 모두 프로세스 안의 metadata 인덱스에서만 읽고
// **요청 경로에서 Kubernetes API를 호출하지 않습니다.** 상세 live GET은 사용자가
// 항목을 실제로 열 때만 나가며 그 경로는 ADR 0018 그대로입니다.
//
// Scope는 UI가 보낸 값이 아니라 서버가 강제 삽입합니다. 검색에는 namespace
// 파라미터 자체가 없습니다 — 전역 검색의 범위는 언제나 "이 사용자가 볼 수 있는 전부"이고,
// 그보다 넓힐 방법도 좁혀서 다른 범위를 떠볼 방법도 두지 않습니다.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
)

// scopeFilter는 Scope를 인덱스가 이해하는 형태로 옮깁니다.
// 요청 파라미터는 여기에 끼어들지 못합니다.
func (rc *resourceCtx) scopeFilter() resourcecatalog.NamespaceFilter {
	return resourcecatalog.NamespaceFilter{All: rc.cluster.All, List: rc.cluster.Namespaces}
}

func (s *Server) handleResourceSearch(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.requireResources(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	req := resourcecatalog.SearchRequest{
		Query:      q.Get("q"),
		Cursor:     q.Get("cursor"),
		Namespaces: rc.scopeFilter(),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > resourcecatalog.MaxSearchPageSize {
			writeError(w, r, http.StatusBadRequest, "invalid_filter", "limit 값이 허용 범위를 벗어났습니다.")
			return
		}
		req.Limit = n
	}

	page, err := s.deps.Resources.Search(req)
	if err != nil {
		writeResourceError(w, r, err)
		return
	}
	out := contract.ResourceSearchResponse{
		ClusterID: rc.clusterID,
		// 계약이 약속한 값은 서버가 정규화한 질의어입니다. 원본을 그대로 되돌려주면
		// 대소문자·공백이 섞인 값이 계약의 minLength/maxLength와 어긋납니다.
		Query:        page.Query,
		GeneratedAt:  s.nowRFC3339(),
		AppliedScope: contract.AppliedScope{ClusterID: rc.clusterID, Namespaces: rc.cluster.NamespacesJSON()},
		Items:        make([]contract.ResourceSearchItem, 0, len(page.Items)),
		NextCursor:   page.NextCursor,
		Truncated:    page.Truncated,
		Degraded:     page.Degraded,
		Reason:       page.Reason,
	}
	if !page.ObservedAt.IsZero() {
		out.ObservedAt = page.ObservedAt.UTC().Format(time.RFC3339)
	}
	for _, item := range page.Items {
		out.Items = append(out.Items, contract.ResourceSearchItem{
			Group: item.Group, Version: item.Version, Resource: item.Resource,
			Kind: item.Kind, Namespaced: item.Namespaced,
			Namespace: item.Namespace, Name: item.Name, UID: item.UID,
			MatchedField: contract.ResourceMatchField(item.MatchedField),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleResourceRecent는 브라우저가 들고 있던 참조를 지금의 인덱스로 다시 확인합니다.
//
// 크기·구조 위반은 오류이고, 해석되지 않는 항목은 조용히 빠집니다. 그 선이
// 계약입니다 — 잘린 요청을 성공으로 답하면 클라이언트가 잘린 줄 모르고,
// 사라진 객체를 오류로 답하면 최근 목록이 오류 화면이 됩니다.
func (s *Server) handleResourceRecent(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.requireResources(w, r)
	if !ok {
		return
	}
	// 원본 query string 상한을 먼저 봅니다. 파싱 자체가 비용이기 때문입니다.
	// 웹은 이보다 낮은 6KiB에서 요청을 나눠 보내지만 서버는 그 약속을 믿지 않습니다.
	if len(r.URL.RawQuery) > resourcecatalog.MaxRecentQueryBytes {
		writeError(w, r, http.StatusBadRequest, "invalid_filter", "최근 항목 요청이 너무 큽니다. 나눠서 보내세요.")
		return
	}
	refs, err := resourcecatalog.ParseRecentRefs(r.URL.Query()["ref"])
	if err != nil {
		writeResourceError(w, r, err)
		return
	}
	items, err := s.deps.Resources.Recent(refs, rc.scopeFilter())
	if err != nil {
		writeResourceError(w, r, err)
		return
	}
	out := contract.ResourceRecentResponse{
		ClusterID:    rc.clusterID,
		GeneratedAt:  s.nowRFC3339(),
		AppliedScope: contract.AppliedScope{ClusterID: rc.clusterID, Namespaces: rc.cluster.NamespacesJSON()},
		Items:        make([]contract.ResourceRecentItem, 0, len(items)),
	}
	for _, item := range items {
		out.Items = append(out.Items, contract.ResourceRecentItem{
			Group: item.Group, Version: item.Version, Resource: item.Resource,
			Kind: item.Kind, Namespaced: item.Namespaced,
			Namespace: item.Namespace, Name: item.Name, UID: item.UID,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
