package httpapi

// Resource Explorer API (ADR 0018)
// --------------------------------------------------------------------------
// 조회 전용입니다. 카탈로그와 목록은 프로세스 안의 discovery snapshot·metadata
// informer 인덱스에서만 읽고 **요청 경로에서 Kubernetes API를 호출하지 않습니다.**
// 상세 하나만 격리된 dynamic client로 live GET하며(ADR 0004의 명시적 예외),
// Scope·allowlist·페이지·본문 상한은 전부 서버가 강제합니다.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

// resourceCtx는 권한 판정이 끝난 요청 문맥입니다.
type resourceCtx struct {
	cluster   scope.Cluster
	clusterID string
	subject   string
}

// requireResources는 클러스터 접근 → 탐색 capability → 기능 가용성 순으로 검사합니다.
//
// 순서가 계약입니다. 권한이 없는 요청은 배포 형태와 무관하게 403이고, 권한이 있는
// 요청만 "이 배포에는 기능이 없다"는 503을 봅니다. central 배포에서도 같습니다.
func (s *Server) requireResources(w http.ResponseWriter, r *http.Request) (*resourceCtx, bool) {
	sc := scope.From(r.Context())
	clusterID := r.PathValue("clusterId")
	cl, ok := sc.Cluster(clusterID)
	if !ok || !cl.Accessible() {
		writeError(w, r, http.StatusForbidden, "cluster_access_denied", "이 클러스터에 대한 권한이 없습니다.")
		return nil, false
	}
	if !sc.CanExploreResources {
		writeError(w, r, http.StatusForbidden, "forbidden", "리소스 탐색 권한이 없습니다.")
		return nil, false
	}
	if !s.deps.Resources.Available() {
		writeError(w, r, http.StatusServiceUnavailable, "resources_unavailable", "이 배포에서는 리소스 탐색을 사용할 수 없습니다.")
		return nil, false
	}
	return &resourceCtx{cluster: cl, clusterID: clusterID, subject: sc.Subject}, true
}

// gvrFromPath는 경로 세그먼트를 GVR로 바꿉니다. core group은 "core"로 표기합니다.
func gvrFromPath(r *http.Request) (schema.GroupVersionResource, error) {
	group := r.PathValue("group")
	if group == resourcecatalog.CoreGroupAlias {
		group = ""
	}
	version, resource := r.PathValue("version"), r.PathValue("resource")
	if err := resourcecatalog.ValidateGVRSegments(group, version, resource); err != nil {
		return schema.GroupVersionResource{}, err
	}
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}, nil
}

/* ── 카탈로그 ─────────────────────────────────────────────────────────── */

func (s *Server) handleResourceCatalog(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.requireResources(w, r)
	if !ok {
		return
	}
	snapshot := s.deps.Resources.Catalog()
	out := contract.ResourceCatalogResponse{
		ClusterID:   rc.clusterID,
		GeneratedAt: s.nowRFC3339(),
		Degraded:    snapshot.Failure != "",
		Reason:      snapshot.Failure,
		Items:       make([]contract.ResourceDescriptor, 0, len(snapshot.Descriptors)),
	}
	if !snapshot.RefreshedAt.IsZero() {
		out.RefreshedAt = snapshot.RefreshedAt.UTC().Format(time.RFC3339)
	}
	for _, d := range snapshot.Descriptors {
		out.Items = append(out.Items, toContractDescriptor(d))
	}
	writeJSON(w, http.StatusOK, out)
}

func toContractDescriptor(d resourcecatalog.Descriptor) contract.ResourceDescriptor {
	verbs := d.Verbs
	if verbs == nil {
		verbs = []string{}
	}
	return contract.ResourceDescriptor{
		Group:            resourcecatalog.GroupSegment(d.Group),
		Version:          d.Version,
		Resource:         d.Resource,
		Kind:             d.Kind,
		Namespaced:       d.Namespaced,
		Verbs:            verbs,
		PreferredVersion: d.PreferredVersion,
		State:            contract.ResourceState(d.State),
		Reason:           d.Reason,
		Count:            d.Count,
	}
}

/* ── 목록 ─────────────────────────────────────────────────────────────── */

func (s *Server) handleResourceList(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.requireResources(w, r)
	if !ok {
		return
	}
	gvr, err := gvrFromPath(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_filter", "리소스 경로가 올바르지 않습니다.")
		return
	}
	desc, err := s.deps.Resources.Describe(gvr)
	if err != nil {
		writeResourceError(w, r, err)
		return
	}

	q := r.URL.Query()
	// 정렬 키는 (namespace, name) 하나뿐입니다. 인덱스 없는 정렬을 허용하면
	// 100k 리소스에서 요청마다 전체 정렬이 됩니다.
	if sortKey := q.Get("sort"); sortKey != "" && sortKey != "name" {
		writeError(w, r, http.StatusBadRequest, "invalid_filter", "지원하지 않는 정렬 키입니다.")
		return
	}
	order := q.Get("order")
	if order != "" && order != "asc" && order != "desc" {
		writeError(w, r, http.StatusBadRequest, "invalid_filter", "정렬 방향이 올바르지 않습니다.")
		return
	}
	req := resourcecatalog.ListRequest{
		Group:         gvr.Group,
		Version:       gvr.Version,
		Resource:      gvr.Resource,
		NamePrefix:    q.Get("name"),
		LabelSelector: q.Get("labelSelector"),
		Cursor:        q.Get("cursor"),
		Descending:    order == "desc",
	}
	if raw := q.Get("limit"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 || n > resourcecatalog.MaxPageSize {
			writeError(w, r, http.StatusBadRequest, "invalid_filter", "limit 값이 허용 범위를 벗어났습니다.")
			return
		}
		req.Limit = n
	}

	// Scope는 서버가 강제 삽입합니다. 요청의 ns는 힌트일 뿐입니다. (README §10)
	requested := q.Get("ns")
	if requested == "all" {
		requested = "" // 다른 화면과 같은 어휘입니다 — 비우거나 all이면 Scope 전체입니다.
	}
	applied := rc.cluster.NamespacesJSON()
	if desc.Namespaced {
		if requested != "" {
			if !rc.cluster.AllowsNamespace(requested) {
				writeError(w, r, http.StatusForbidden, "namespace_access_denied", "이 namespace에 대한 권한이 없습니다.")
				return
			}
			req.Namespaces = resourcecatalog.NamespaceFilter{List: []string{requested}}
			applied = []string{requested}
		} else {
			req.Namespaces = resourcecatalog.NamespaceFilter{All: rc.cluster.All, List: rc.cluster.Namespaces}
		}
	} else {
		if requested != "" {
			writeError(w, r, http.StatusBadRequest, "invalid_filter", "클러스터 범위 리소스에는 namespace를 지정할 수 없습니다.")
			return
		}
		// 클러스터 범위 리소스는 클러스터 전체 권한이 필요합니다. namespace 사용자에게
		// 0건을 보여주는 대신 권한 없음으로 구분합니다.
		if !rc.cluster.All {
			writeError(w, r, http.StatusForbidden, "cluster_scope_required", "클러스터 범위 리소스는 클러스터 전체 권한이 필요합니다.")
			return
		}
	}

	page, listed, err := s.deps.Resources.List(req)
	if err != nil {
		writeResourceError(w, r, err)
		return
	}
	out := contract.ResourceListResponse{
		ClusterID:    rc.clusterID,
		Group:        resourcecatalog.GroupSegment(gvr.Group),
		Version:      gvr.Version,
		Resource:     gvr.Resource,
		Kind:         listed.Kind,
		Namespaced:   listed.Namespaced,
		GeneratedAt:  s.nowRFC3339(),
		AppliedScope: contract.AppliedScope{ClusterID: rc.clusterID, Namespaces: applied},
		Items:        make([]contract.ResourceListItem, 0, len(page.Items)),
		NextCursor:   page.NextCursor,
		Truncated:    page.Truncated,
		Total:        page.Total,
	}
	if !page.ObservedAt.IsZero() {
		out.ObservedAt = page.ObservedAt.UTC().Format(time.RFC3339)
	}
	for _, item := range page.Items {
		out.Items = append(out.Items, contract.ResourceListItem{
			Namespace: item.Namespace, Name: item.Name, UID: item.UID, CreatedAt: item.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

/* ── 상세 ─────────────────────────────────────────────────────────────── */

func (s *Server) handleResourceDetail(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.requireResources(w, r)
	if !ok {
		return
	}
	gvr, err := gvrFromPath(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_filter", "리소스 경로가 올바르지 않습니다.")
		return
	}
	desc, err := s.deps.Resources.Describe(gvr)
	if err != nil {
		writeResourceError(w, r, err)
		return
	}
	q := r.URL.Query()
	namespace, name, uid := q.Get("namespace"), q.Get("name"), q.Get("uid")
	if desc.Namespaced {
		if namespace == "" {
			writeError(w, r, http.StatusBadRequest, "invalid_filter", "namespace가 필요합니다.")
			return
		}
		if !rc.cluster.AllowsNamespace(namespace) {
			writeError(w, r, http.StatusForbidden, "namespace_access_denied", "이 namespace에 대한 권한이 없습니다.")
			return
		}
	} else {
		if namespace != "" {
			writeError(w, r, http.StatusBadRequest, "invalid_filter", "클러스터 범위 리소스에는 namespace를 지정할 수 없습니다.")
			return
		}
		if !rc.cluster.All {
			writeError(w, r, http.StatusForbidden, "cluster_scope_required", "클러스터 범위 리소스는 클러스터 전체 권한이 필요합니다.")
			return
		}
	}

	detail, err := s.deps.Resources.Get(r.Context(), resourcecatalog.DetailRequest{
		Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource,
		Namespace: namespace, Name: name, ExpectedUID: uid,
	})
	if err != nil {
		// 사용자가 화면을 닫은 취소는 응답을 쓰지 않습니다.
		if r.Context().Err() != nil && errors.Is(err, context.Canceled) {
			return
		}
		writeResourceError(w, r, err)
		return
	}
	// 감사에는 무엇을 열었는지만 남깁니다. 매니페스트 내용은 남기지 않습니다.
	s.deps.Logger.Info("resource-audit",
		"requestId", requestIDFrom(r.Context()),
		"action", "read-manifest", "resource", resourcecatalog.FormatGVR(gvr),
		"namespace", namespace, "name", name, "subject", rc.subject)

	writeJSON(w, http.StatusOK, contract.ResourceDetailResponse{
		ClusterID:       rc.clusterID,
		Group:           resourcecatalog.GroupSegment(gvr.Group),
		Version:         gvr.Version,
		Resource:        gvr.Resource,
		APIVersion:      detail.APIVersion,
		Kind:            detail.Kind,
		Namespace:       detail.Namespace,
		Name:            detail.Name,
		UID:             detail.UID,
		ResourceVersion: detail.ResourceVersion,
		GeneratedAt:     s.nowRFC3339(),
		YAML:            detail.YAML,
		Redacted:        detail.Redacted,
	})
}

/* ── 오류 매핑 ────────────────────────────────────────────────────────────
   "데이터 없음 · 권한 없음 · 미지원 · 동기화 중"을 같은 빈 화면으로 만들지 않습니다. */

func writeResourceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, resourcecatalog.ErrUnavailable):
		writeError(w, r, http.StatusServiceUnavailable, "resources_unavailable", "이 배포에서는 리소스 탐색을 사용할 수 없습니다.")
	case errors.Is(err, resourcecatalog.ErrNotAllowlisted):
		writeError(w, r, http.StatusNotFound, "resource_not_allowlisted", "이 리소스는 탐색 대상으로 등록되어 있지 않습니다.")
	case errors.Is(err, resourcecatalog.ErrNotServed):
		writeError(w, r, http.StatusNotFound, "resource_not_served", "클러스터가 이 API를 제공하지 않습니다.")
	case errors.Is(err, resourcecatalog.ErrUnsupported):
		writeError(w, r, http.StatusBadGateway, "resource_unsupported", "이 API는 metadata 전용 조회를 지원하지 않습니다.")
	case errors.Is(err, resourcecatalog.ErrForbidden):
		writeError(w, r, http.StatusBadGateway, "resource_forbidden", "서버에 이 리소스의 조회 권한이 없습니다.")
	case errors.Is(err, resourcecatalog.ErrSyncing):
		w.Header().Set("Retry-After", "5")
		writeError(w, r, http.StatusServiceUnavailable, "resource_syncing", "리소스 캐시를 동기화하는 중입니다.")
	case errors.Is(err, resourcecatalog.ErrInvalidCursor):
		writeError(w, r, http.StatusBadRequest, "invalid_cursor", "cursor가 올바르지 않습니다.")
	case errors.Is(err, resourcecatalog.ErrInvalidFilter):
		writeError(w, r, http.StatusBadRequest, "invalid_filter", "요청 필터가 올바르지 않습니다.")
	case errors.Is(err, resourcecatalog.ErrRateLimited):
		w.Header().Set("Retry-After", "1")
		writeError(w, r, http.StatusTooManyRequests, "detail_rate_limited", "상세 조회 한도를 초과했습니다.")
	case errors.Is(err, resourcecatalog.ErrObjectNotFound):
		// 목록에 없는 항목은 API 서버로 나가지 않고 여기서 끝납니다.
		writeError(w, r, http.StatusNotFound, "not_found", "목록에 없는 항목입니다. 목록을 새로고침하세요.")
	case errors.Is(err, resourcecatalog.ErrUIDMismatch):
		writeError(w, r, http.StatusConflict, "uid_mismatch", "같은 이름의 다른 객체로 교체되었습니다. 목록을 새로고침하세요.")
	case errors.Is(err, resourcecatalog.ErrTooLarge):
		writeError(w, r, http.StatusBadGateway, "object_too_large", "객체 크기가 응답 한도를 넘었습니다.")
	case apierrors.IsNotFound(err):
		writeError(w, r, http.StatusNotFound, "not_found", "객체를 찾을 수 없습니다.")
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		writeError(w, r, http.StatusBadGateway, "resource_forbidden", "서버에 이 리소스의 조회 권한이 없습니다.")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, http.StatusGatewayTimeout, "upstream_timeout", "클러스터 응답 시간이 초과되었습니다.")
	default:
		writeError(w, r, http.StatusBadGateway, "upstream_unavailable", "클러스터에서 객체를 읽지 못했습니다.")
	}
}
