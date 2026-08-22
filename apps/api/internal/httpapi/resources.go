package httpapi

// Resource Explorer API (ADR 0018) + 변경 검토 dry-run (ADR 0019 Phase 1)
// --------------------------------------------------------------------------
// 카탈로그와 목록은 프로세스 안의 discovery snapshot·metadata informer 인덱스에서만
// 읽고 **요청 경로에서 Kubernetes API를 호출하지 않습니다.** 격리된 dynamic client로
// 클러스터에 나가는 경로는 둘뿐입니다(ADR 0004의 명시적 예외) —
// 상세 live GET 하나와, 저장하지 않는 dry-run 검토 하나.
//
// **적용·생성·삭제 경로는 없습니다.** 기존 Deployment/Secret 관리 write는 별개
// 파일(workloads.go, ADR 0014)에 그대로 있고 이 파일과 섞이지 않습니다.
// Scope·allowlist·페이지·본문 상한은 전부 서버가 강제합니다.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
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
		// 변경 검토 capability입니다. 권한이 아니라 배포 설정이며, 서버가 정합니다.
		// UI는 이 값이 false면 검토 진입점을 만들지 않습니다. (ADR 0019)
		DryRun: d.DryRun,
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

/* ── 변경 검토 dry-run (ADR 0019 Phase 1) ─────────────────────────────────

   상세(object) 경계 바로 아래에 붙는 **검토 전용** POST 하나입니다. 적용·삭제·
   생성·change token·force는 없고, 기존 관리 write 라우트(ADR 0014)도 그대로입니다.

   플랫폼 권한 근거는 Resource Explorer와 **똑같습니다** — requireResources가
   보는 CanExploreResources(platform.admin 또는 AUTH_MODE=none)뿐이고, 사용자별
   Kubernetes RBAC·SAR·impersonation은 이 경로에 없습니다. */

func (s *Server) handleResourceDryRun(w http.ResponseWriter, r *http.Request) {
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

	// Content-Type은 **본문을 읽기 전에** 확인합니다.
	//
	// 계약이 선언한 것은 application/json 하나뿐인데, 검사하지 않으면 text/plain이나
	// form 인코딩으로도 같은 본문이 통과합니다. 그 순간 이 POST는 브라우저의
	// **simple cross-origin 요청** 표면에 들어갑니다 — preflight 없이 도달할 수 있고,
	// cookie 세션 배포에서는 그것만으로 원치 않는 검토 요청이 실제 Kubernetes
	// dry-run 호출이 됩니다.
	if !hasJSONContentType(r) {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"요청 본문은 application/json이어야 합니다.")
		return
	}

	// 본문 상한은 **디코드 전에** 겁니다. 거대한 입력을 파서에 먹이는 것 자체가
	// 공격면이므로, 읽는 도중에 끊어야 합니다. 상한은 배포가 설정한 매니페스트
	// 상한 + 나머지 필드 여유분입니다.
	limit := int64(s.deps.Resources.MaxManifestBytes()) + contract.DryRunEnvelopeSlack
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	// **포인터로 받는 것이 핵심입니다.** 값 타입으로 받으면 최상위 `null`이 오류 없이
	// 디코드되어 모든 필드가 zero value인 요청이 됩니다 — 본문을 보내지 않은 것과
	// 구분되지 않은 채 그대로 서비스까지 내려갑니다.
	var body *contract.ResourceDryRunRequest
	dec := json.NewDecoder(r.Body)
	// 계약이 닫힌 스키마이므로 모르는 필드는 조용히 버리지 않고 거절합니다.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeDryRunBodyError(w, r, err)
		return
	}
	if body == nil {
		// 최상위 null은 유효한 JSON 값이지만 객체가 아닙니다. 서비스로 내려보내지
		// 않고 여기서 끝냅니다.
		writeDryRunBodyError(w, r, nil)
		return
	}
	// 본문은 JSON 값 **하나**입니다. 두 번째 값도, 꼬리 쓰레기도 받지 않습니다 —
	// 정확히 io.EOF일 때만 통과합니다.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeDryRunBodyError(w, r, err)
		return
	}

	// Scope는 서버가 강제합니다. 본문의 namespace는 힌트가 아니라 **대조 대상**이고,
	// 통과한 값만 서비스로 넘어갑니다. (README §10)
	namespace := body.Namespace
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

	result, err := s.deps.Resources.DryRun(r.Context(), resourcecatalog.DryRunRequest{
		Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource,
		// 경로에서 온 GVR과 **검증을 통과한** namespace만 씁니다.
		Namespace: namespace, Name: body.Name,
		ExpectedUID: body.UID, ExpectedResourceVersion: body.ResourceVersion,
		APIVersion: body.APIVersion, Kind: body.Kind,
		// raw 매니페스트는 여기서 서비스로만 흘러갑니다. 응답·오류·감사 어디에도
		// 되돌아오지 않습니다.
		Manifest: body.Manifest,
	})
	if err != nil {
		// 사용자가 화면을 닫은 취소는 응답을 쓰지 않습니다.
		if r.Context().Err() != nil && errors.Is(err, context.Canceled) {
			return
		}
		writeDryRunError(w, r, err)
		return
	}

	// 신원은 **서비스가 돌려준 값**만 씁니다. 요청 본문의 값을 성공 응답에 다시
	// 베끼면, 서버가 확인한 것과 화면이 보는 것이 갈라질 수 있습니다.
	out := contract.ResourceDryRunResponse{
		ClusterID:       rc.clusterID,
		Group:           resourcecatalog.GroupSegment(gvr.Group),
		Version:         gvr.Version,
		Resource:        gvr.Resource,
		APIVersion:      result.APIVersion,
		Kind:            result.Kind,
		Namespace:       result.Namespace,
		Name:            result.Name,
		UID:             result.UID,
		ResourceVersion: result.ResourceVersion,
		GeneratedAt:     s.nowRFC3339(),
		FieldManager:    result.FieldManager,
		Outcome:         result.Outcome,
		RejectedBy:      result.RejectedBy,
		Changes:         result.Changes,
		ChangeCount:     result.ChangeCount,
		Truncated:       result.Truncated,
		Warnings:        result.Warnings,
		Violations:      result.Violations,
		Redacted:        result.Redacted,
	}
	// 필수 배열은 언제나 non-nil입니다. null이면 화면이 "검토 안 함"과 "변경 없음"을
	// 구분하지 못합니다.
	if out.Changes == nil {
		out.Changes = []contract.ResourceDryRunChange{}
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	if out.Violations == nil {
		out.Violations = []contract.ResourceDryRunViolation{}
	}
	if out.Redacted == nil {
		out.Redacted = []string{}
	}

	if !writeBoundedJSON(w, r, out, contract.MaxDryRunResponseBytes) {
		return
	}
	// 감사에는 **무엇을 검토했고 결과가 무엇인지**만 남깁니다. 매니페스트·diff 값·
	// warning·violation·upstream 문자열은 남기지 않습니다.
	s.deps.Logger.Info("resource-audit",
		"requestId", requestIDFrom(r.Context()),
		"action", "dryrun", "resource", resourcecatalog.FormatGVR(gvr),
		"namespace", namespace, "name", result.Name, "subject", rc.subject,
		"outcome", string(result.Outcome),
		"changeCount", result.ChangeCount, "truncated", result.Truncated)
}

// writeBoundedJSON은 **버퍼에 먼저 인코딩**하고 상한을 확인한 뒤에야 헤더를 씁니다.
//
// writeJSON은 헤더와 상태 코드를 먼저 내보냅니다. 그 뒤에 본문이 상한을 넘은 것을
// 알아도 되돌릴 방법이 없고, 잘린 JSON이 200과 함께 나갑니다. 검토 결과는 그렇게
// 새어 나가면 안 되므로 이 경로만 순서를 뒤집습니다.
//
// 반환값은 성공 응답을 실제로 썼는지입니다. false면 이미 고정 오류를 썼습니다.
func writeBoundedJSON(w http.ResponseWriter, r *http.Request, v any, max int) bool {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "응답을 만들지 못했습니다.")
		return false
	}
	if buf.Len() > max {
		// 부분 본문을 내보내는 대신 고정 오류입니다.
		writeError(w, r, http.StatusBadGateway, "object_too_large", "검토 결과가 응답 한도를 넘었습니다.")
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
	return true
}

// hasJSONContentType은 Content-Type의 media type이 정확히 application/json인지 봅니다.
//
// charset 같은 파라미터는 허용합니다 — 브라우저와 클라이언트가 흔히 붙이고, 값은
// 우리가 쓰지 않기 때문입니다. 헤더가 없거나 파싱되지 않거나 다른 media type이면
// 거절합니다. `application/jsonx`처럼 접두사만 같은 값도 여기서 걸립니다.
//
// 헤더 원문은 반환값에도 오류에도 담기지 않습니다.
func hasJSONContentType(r *http.Request) bool {
	raw := r.Header.Get("Content-Type")
	if raw == "" {
		return false
	}
	media, _, err := mime.ParseMediaType(raw)
	return err == nil && media == "application/json"
}

// writeDryRunBodyError는 본문 자체가 성립하지 않는 경우입니다.
// 본문 내용은 오류에 담지 않습니다 — 매니페스트 조각이 그대로 새어 나갑니다.
//
// err은 nil일 수 있습니다. 디코드는 성공했지만 값이 객체가 아닌 경우(최상위 null)가
// 그렇고, 그때도 같은 400을 냅니다.
func writeDryRunBodyError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "manifest_too_large", "검토 요청 본문이 허용 상한을 넘었습니다.")
		return
	}
	writeError(w, r, http.StatusBadRequest, "bad_request", "요청 본문은 알려진 필드만 담은 JSON 객체 하나여야 합니다.")
}

// writeDryRunError는 검토 전용 sentinel을 옮기고, 나머지는 공용 매핑에 넘깁니다.
// 매핑의 정본은 resourcecatalog/errors.go의 주석입니다.
func writeDryRunError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, resourcecatalog.ErrDryRunDisabled):
		writeError(w, r, http.StatusServiceUnavailable, "dryrun_unavailable", "이 배포에서는 변경 검토를 사용할 수 없습니다.")
	case errors.Is(err, resourcecatalog.ErrDryRunDenied):
		writeError(w, r, http.StatusForbidden, "dryrun_resource_denied", "이 리소스는 변경 검토 대상이 아닙니다.")
	case errors.Is(err, resourcecatalog.ErrDryRunForbidden):
		writeError(w, r, http.StatusBadGateway, "dryrun_forbidden", "서버에 이 리소스의 검토 권한이 없습니다.")
	case errors.Is(err, resourcecatalog.ErrDryRunRateLimited):
		w.Header().Set("Retry-After", "1")
		writeError(w, r, http.StatusTooManyRequests, "dryrun_rate_limited", "변경 검토 한도를 초과했습니다.")
	case errors.Is(err, resourcecatalog.ErrManifestTooLarge):
		writeError(w, r, http.StatusRequestEntityTooLarge, "manifest_too_large", "매니페스트가 허용 상한을 넘었습니다.")
	case errors.Is(err, resourcecatalog.ErrManifestInvalid):
		writeError(w, r, http.StatusBadRequest, "invalid_manifest",
			"매니페스트를 해석하지 못했습니다. 단일 문서여야 하고 중복 키·anchor·alias는 쓸 수 없습니다.")
	case errors.Is(err, resourcecatalog.ErrManifestMismatch):
		writeError(w, r, http.StatusBadRequest, "manifest_mismatch", "매니페스트가 가리키는 대상이 요청과 다릅니다.")
	case errors.Is(err, resourcecatalog.ErrResourceVersionMismatch):
		writeError(w, r, http.StatusConflict, "resource_version_mismatch", "객체가 그 사이에 바뀌었습니다. 다시 조회한 뒤 검토하세요.")
	case errors.Is(err, resourcecatalog.ErrDryRunUpstream):
		writeError(w, r, http.StatusBadGateway, "upstream_unavailable", "클러스터가 검토를 끝내지 못했습니다.")
	default:
		writeResourceError(w, r, err)
	}
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
	case errors.Is(err, resourcecatalog.ErrSearchDisabled):
		// Explorer는 살아 있고 검색만 꺼진 상태입니다. resources_unavailable과 구분합니다. (ADR 0023 롤백)
		writeError(w, r, http.StatusServiceUnavailable, "search_unavailable", "이 배포에서는 전역 검색을 사용할 수 없습니다.")
	case errors.Is(err, resourcecatalog.ErrInvalidQuery):
		writeError(w, r, http.StatusBadRequest, "invalid_query", "검색어는 2자 이상 64자 이하여야 합니다.")
	case errors.Is(err, resourcecatalog.ErrSearchTooBroad):
		writeError(w, r, http.StatusBadRequest, "invalid_query", "검색 범위가 너무 넓습니다. 더 긴 접두사를 입력하세요.")
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
