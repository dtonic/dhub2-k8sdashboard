package resourcecatalog

// 변경 검토 dry-run (ADR 0019 Phase 1)
// --------------------------------------------------------------------------
// 사용자가 고친 매니페스트를 **적용하지 않고** 서버가 대신 물어봅니다 —
// "이걸 적용하면 무엇이 달라지고, 무엇이 막히는가".
//
// 적용·삭제·생성·change token·force는 이 파일에 없습니다. Kubernetes로 나가는
// 쓰기 동사는 dryRun=All이 붙은 서버사이드 apply **하나**뿐이고, 그 사실을
// dryrun_test.go가 실제 호출 인자로 증명합니다.
//
// 이 경로는 ADR 0004("요청 경로에서 API 서버를 호출하지 않는다")의 두 번째
// 명시적 예외입니다. 상세 조회와 같은 이유로 허용하되, 클라이언트·예산·timeout·
// 본문 상한을 상세와도 공유하지 않습니다.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// DryRunRequest는 검토 한 건입니다. 전부 **대조 대상**이고 힌트가 아닙니다.
//
// Scope(어떤 cluster·namespace를 볼 수 있는가)는 U3 핸들러가 정하고 여기 오기
// 전에 이미 강제됩니다. 이 계층은 그 위에서 "요청이 가리키는 객체가 목록에서
// 실제로 본 그 객체인가"를 봅니다.
type DryRunRequest struct {
	Group    string
	Version  string
	Resource string
	// Namespace는 namespaced 리소스에서만 채웁니다. cluster 범위는 비어 있어야 합니다.
	Namespace string
	Name      string
	// ExpectedUID/ExpectedResourceVersion은 사용자가 보고 있던 객체의 신원과 CAS 기준입니다.
	ExpectedUID             string
	ExpectedResourceVersion string
	// APIVersion/Kind는 본문이 선언한 대상입니다. 경로 GVR·매니페스트와 모두 같아야 합니다.
	APIVersion string
	Kind       string
	// Manifest는 YAML 또는 JSON 단일 문서입니다. **이 문자열은 결과·오류·로그
	// 어디에도 복사되지 않습니다.**
	Manifest string
}

// DryRunResult는 정제된 구조 diff와 검증 결과뿐입니다.
// 매니페스트 원문·dry-run 객체·Kubernetes Status 원문은 담지 않습니다.
type DryRunResult struct {
	APIVersion      string
	Kind            string
	Namespace       string
	Name            string
	UID             string
	ResourceVersion string
	FieldManager    string
	Outcome         contract.ResourceDryRunOutcome
	RejectedBy      contract.ResourceDryRunRejectedBy
	Changes         []contract.ResourceDryRunChange
	ChangeCount     int
	Truncated       bool
	Warnings        []string
	Violations      []contract.ResourceDryRunViolation
	Redacted        []string
}

// dryRunCapable은 이 GVR에 검토를 요청할 수 있는지입니다.
//
// **권한이 아니라 배포 능력입니다.** 네 가지가 모두 참이어야 합니다 —
// 기능이 켜져 있고, 전용 클라이언트가 있고, opt-in 목록에 있고, 캐시가 ready이며
// API가 get·patch를 제공합니다. 하나라도 아니면 UI는 진입점을 만들지 않습니다.
func (s *Service) dryRunCapable(gvr schema.GroupVersionResource, desc Descriptor) bool {
	if s == nil || !s.cfg.DryRunEnabled || s.clients.DryRun == nil {
		return false
	}
	if !s.dryRunAllow[gvr] {
		return false
	}
	if desc.State != StateReady {
		return false
	}
	return hasVerb(desc.Verbs, "get") && hasVerb(desc.Verbs, "patch")
}

// MaxManifestBytes는 이 배포에 설정된 매니페스트 상한입니다.
//
// 전송 계층이 **본문을 디코드하기 전에** 요청 봉투를 조이려면 이 값이 필요합니다.
// 절대 상한(contract.MaxDryRunManifestBytes)만 쓰면 256KiB로 설정한 배포에서도
// 봉투가 1MiB까지 열립니다. 읽기 전용이고 기동 후에는 바뀌지 않습니다.
func (s *Service) MaxManifestBytes() int {
	if s == nil || s.cfg.MaxManifestBytes <= 0 {
		return contract.DefaultDryRunManifestBytes
	}
	return s.cfg.MaxManifestBytes
}

// DryRun은 변경 검토 한 건을 수행합니다.
//
// 순서가 계약입니다. 아래 (1)~(8)에서 걸리면 **Kubernetes 요청이 0회**입니다 —
// 잘못된 요청·정책 위반·낡은 신원은 클러스터를 건드리지 않고 끝납니다.
func (s *Service) DryRun(ctx context.Context, req DryRunRequest) (DryRunResult, error) {
	// (1) 기능 가용성. central 모드는 서비스 자체가 없으므로 nil 리시버도 안전합니다.
	if !s.Available() {
		return DryRunResult{}, ErrUnavailable
	}
	if !s.cfg.DryRunEnabled || s.clients.DryRun == nil {
		return DryRunResult{}, ErrDryRunDisabled
	}

	gvr := schema.GroupVersionResource{Group: req.Group, Version: req.Version, Resource: req.Resource}
	if err := ValidateGVRSegments(gvr.Group, gvr.Version, gvr.Resource); err != nil {
		return DryRunResult{}, ErrInvalidFilter
	}
	// (2) 정책. hard-deny를 opt-in 목록보다 **먼저** 봅니다 — 목록에 잘못 들어와도
	// 여기서 다시 막힙니다(기동 검증과 이중 방어).
	if DryRunHardDenied(gvr) || !s.dryRunAllow[gvr] {
		return DryRunResult{}, ErrDryRunDenied
	}

	// (3) 카탈로그 상태와 API 능력.
	desc, err := s.Describe(gvr)
	if err != nil {
		return DryRunResult{}, err
	}
	if err := stateError(desc.State); err != nil {
		return DryRunResult{}, err
	}
	if !hasVerb(desc.Verbs, "get") || !hasVerb(desc.Verbs, "patch") {
		return DryRunResult{}, ErrUnsupported
	}

	// (4) 신원 문자열 형식. 임의 문자열이 경로·질의로 흘러가지 못하게 합니다.
	if err := validateDryRunIdentity(req, desc); err != nil {
		return DryRunResult{}, err
	}

	// (5) 본문 상한은 **파싱 전에** 적용합니다. 거대한 입력을 파서에 먹이는 것 자체가
	// CPU·메모리 공격면입니다.
	if req.Manifest == "" {
		return DryRunResult{}, ErrManifestInvalid
	}
	if len(req.Manifest) > s.cfg.MaxManifestBytes {
		return DryRunResult{}, ErrManifestTooLarge
	}

	// (6) 파싱. 단일 문서·중복 키 금지·anchor/alias 금지·깊이/노드/스칼라 상한.
	decoded, err := decodeManifest(req.Manifest)
	if err != nil {
		return DryRunResult{}, err
	}

	// (7) 본문 ↔ 요청 ↔ 경로 대조. 본문이 스스로를 다른 것이라고 말하면 여기서 끝입니다.
	if err := matchManifestIdentity(decoded, req, desc); err != nil {
		return DryRunResult{}, err
	}

	// (8) 로컬 metadata 인덱스 신원 게이트. 상세 조회와 **같은 근거**를 씁니다 —
	// 목록에서 실제로 본 행만 열립니다. 지어낸 이름·UID로는 live GET조차 없습니다.
	entry, ok := s.entries[gvr]
	if !ok {
		return DryRunResult{}, ErrNotAllowlisted
	}
	index := entry.baselineIndex()
	if index == nil {
		return DryRunResult{}, ErrSyncing
	}
	row, found := index.lookup(req.Namespace, req.Name)
	if !found {
		return DryRunResult{}, ErrObjectNotFound
	}
	if row.obj == nil || string(row.obj.UID) != req.ExpectedUID {
		return DryRunResult{}, ErrUIDMismatch
	}

	// ── 여기부터 Kubernetes 요청이 발생합니다 ────────────────────────────────
	// 이미 취소된 요청은 토큰도 슬롯도 쓰지 않습니다.
	if err := ctx.Err(); err != nil {
		return DryRunResult{}, err
	}
	release, err := s.dryRunGuard.acquire()
	if err != nil {
		// 검토 예산과 상세 예산은 다른 사유입니다. 코드가 섞이면 운영자가 어느
		// 쪽을 늘려야 하는지 알 수 없습니다.
		if errors.Is(err, ErrRateLimited) {
			return DryRunResult{}, ErrDryRunRateLimited
		}
		return DryRunResult{}, err
	}
	defer release()

	// 취소는 데이터소스 요청까지 전파합니다. timeout은 이 경로 전용입니다.
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.DryRunTimeout)
	defer cancel()

	// namespaced 여부에 따라 대상 인터페이스가 달라집니다. 타입을 넓은 쪽으로
	// 고정해 두 경로가 같은 호출부를 쓰게 합니다.
	var target dynamic.ResourceInterface = s.clients.DryRun.Resource(gvr)
	if desc.Namespaced {
		target = s.clients.DryRun.Resource(gvr).Namespace(req.Namespace)
	}

	// (9) live GET. 여기서 UID와 resourceVersion을 **둘 다** 다시 확인합니다.
	live, err := target.Get(callCtx, req.Name, metav1.GetOptions{})
	if err != nil {
		return DryRunResult{}, classifyDryRunError(err)
	}
	if live == nil {
		return DryRunResult{}, ErrObjectNotFound
	}
	if string(live.GetUID()) != req.ExpectedUID {
		return DryRunResult{}, ErrUIDMismatch
	}
	if live.GetResourceVersion() != req.ExpectedResourceVersion {
		return DryRunResult{}, ErrResourceVersionMismatch
	}

	// (10) patch 본문. 서버가 신원·CAS를 **덮어써서** 싣습니다 — 우리 GET 검사와
	// 별개로 API 서버가 한 번 더 강제하게 만드는 것이 목적입니다. 그 사이에 객체가
	// 바뀌면 우리가 못 봐도 API 서버가 conflict를 냅니다.
	applyObj := buildApplyObject(decoded, req, desc)
	data, err := json.Marshal(applyObj.Object)
	if err != nil {
		// 우리가 만든 map을 직렬화하지 못하는 경우입니다. 원문을 담지 않습니다.
		return DryRunResult{}, ErrManifestInvalid
	}
	if len(data) > s.cfg.MaxManifestBytes {
		return DryRunResult{}, ErrManifestTooLarge
	}

	// (11) **유일한 쓰기 동사.** dryRun=All·Strict·고정 fieldManager이고 Force는
	// 설정하지 않습니다(nil). subresource도 넘기지 않습니다.
	//
	// 경고 수집기는 **이 호출에만** 답니다. live GET에 달면 조회 경고까지 검토
	// 결과에 섞이고, 그러면 "검토가 경고를 만들었다"와 "객체가 원래 경고를 낸다"가
	// 구분되지 않습니다.
	sink := &warningSink{}
	patchCtx := withWarningSink(callCtx, sink)
	patched, err := target.Patch(patchCtx, req.Name, types.ApplyPatchType, data, metav1.PatchOptions{
		DryRun:          []string{metav1.DryRunAll},
		FieldManager:    contract.ResourceDryRunFieldManager,
		FieldValidation: metav1.FieldValidationStrict,
	})
	warnings := sink.take()
	if err != nil {
		if rejection, ok := classifyDryRunRejection(err); ok {
			rejection.APIVersion = req.APIVersion
			rejection.Kind = req.Kind
			rejection.Namespace = req.Namespace
			rejection.Name = req.Name
			rejection.UID = req.ExpectedUID
			rejection.ResourceVersion = req.ExpectedResourceVersion
			rejection.FieldManager = contract.ResourceDryRunFieldManager
			rejection.Warnings = warnings
			rejection.Changes = []contract.ResourceDryRunChange{}
			rejection.Redacted = []string{}
			return rejection, nil
		}
		return DryRunResult{}, classifyDryRunError(err)
	}
	if patched == nil {
		return DryRunResult{}, ErrDryRunUpstream
	}
	// (12) 응답 객체가 우리가 물어본 그 객체인지 마지막으로 확인합니다.
	// 다르면 diff를 만들지 않고 버립니다 — 다른 객체의 변경을 보여주는 것보다 낫습니다.
	if string(patched.GetUID()) != req.ExpectedUID {
		return DryRunResult{}, ErrUIDMismatch
	}

	// (13) 정제 후 비교. 두 객체 **모두** 같은 정책으로 지웁니다.
	//
	// 표현할 수 없는 결과(경로가 상한을 넘거나 순회 예산을 소진)는 잘라서 성공으로
	// 내보내지 않습니다. 부분 diff를 전체처럼 보여 주는 것이 가장 나쁜 실패입니다.
	diff, err := compareForReview(live, patched, gvr)
	if err != nil {
		return DryRunResult{}, err
	}

	outcome := contract.DryRunUnchanged
	if diff.total > 0 {
		outcome = contract.DryRunChanged
	}
	return DryRunResult{
		APIVersion:      req.APIVersion,
		Kind:            req.Kind,
		Namespace:       req.Namespace,
		Name:            req.Name,
		UID:             req.ExpectedUID,
		ResourceVersion: req.ExpectedResourceVersion,
		FieldManager:    contract.ResourceDryRunFieldManager,
		Outcome:         outcome,
		Changes:         diff.changes,
		ChangeCount:     diff.total,
		Truncated:       diff.truncated,
		Warnings:        warnings,
		Violations:      []contract.ResourceDryRunViolation{},
		Redacted:        diff.redacted,
	}, nil
}

/* ── 신원 검증 ───────────────────────────────────────────────────────────── */

// validateDryRunIdentity는 요청이 들고 온 신원 문자열의 형식을 봅니다.
func validateDryRunIdentity(req DryRunRequest, desc Descriptor) error {
	if req.Name == "" || len(req.Name) > maxCursorName || !safeCursorSegment(req.Name) {
		return ErrInvalidFilter
	}
	if desc.Namespaced {
		if req.Namespace == "" || len(req.Namespace) > maxCursorNSLen || !safeCursorSegment(req.Namespace) {
			return ErrInvalidFilter
		}
	} else if req.Namespace != "" {
		// cluster 범위 리소스에 namespace를 붙이면 "다른 대상"입니다.
		return ErrInvalidFilter
	}
	if req.ExpectedUID == "" || len(req.ExpectedUID) > 64 || !safeCursorSegment(req.ExpectedUID) {
		return ErrInvalidFilter
	}
	if req.ExpectedResourceVersion == "" || len(req.ExpectedResourceVersion) > 64 ||
		!safeCursorSegment(req.ExpectedResourceVersion) {
		return ErrInvalidFilter
	}
	// 본문이 선언한 apiVersion/kind가 경로 GVR과 다르면 대조할 것도 없습니다.
	if req.APIVersion != expectedAPIVersion(schema.GroupVersionResource{
		Group: desc.Group, Version: desc.Version, Resource: desc.Resource,
	}) {
		return ErrManifestMismatch
	}
	// kind는 **선택된 descriptor**와 대조합니다. 문자열이 "Secret"인지 보는 것이
	// 아니라, discovery가 이 GVR의 kind라고 알려준 값과 같은지 봅니다 —
	// 같은 이름의 CRD를 kind 텍스트만으로 막지 않기 위해서입니다.
	// descriptor가 kind를 모르면 대조할 근거가 없으므로 거절합니다(fail-closed).
	if req.Kind == "" || desc.Kind == "" || req.Kind != desc.Kind {
		return ErrManifestMismatch
	}
	return nil
}

// expectedAPIVersion은 GVR이 요구하는 apiVersion 문자열입니다.
func expectedAPIVersion(gvr schema.GroupVersionResource) string {
	if gvr.Group == "" {
		return gvr.Version
	}
	return gvr.Group + "/" + gvr.Version
}

// matchManifestIdentity는 파싱된 본문이 요청과 같은 대상을 가리키는지 봅니다.
//
// uid/resourceVersion은 본문에 **있어도 되지만 달라서는 안 됩니다.** 우리가
// 덮어쓰기 전에 어긋남을 먼저 드러내야 사용자가 낡은 편집본을 붙였다는 걸 압니다.
func matchManifestIdentity(obj map[string]any, req DryRunRequest, desc Descriptor) error {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	// req.Kind는 이미 descriptor와 대조되었으므로, 본문이 그것과 같으면 충분합니다.
	if apiVersion != req.APIVersion || kind != req.Kind {
		return ErrManifestMismatch
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return ErrManifestMismatch
	}
	name, _ := meta["name"].(string)
	if name != req.Name {
		return ErrManifestMismatch
	}
	ns, _ := meta["namespace"].(string)
	if desc.Namespaced {
		// namespace 생략은 허용합니다 — 서버가 대상 namespace를 채웁니다.
		// 하지만 **다른** namespace를 적으면 대상이 바뀌는 것이므로 거부합니다.
		if ns != "" && ns != req.Namespace {
			return ErrManifestMismatch
		}
	} else if ns != "" {
		return ErrManifestMismatch
	}
	if uid, ok := meta["uid"].(string); ok && uid != "" && uid != req.ExpectedUID {
		return ErrManifestMismatch
	}
	if rv, ok := meta["resourceVersion"].(string); ok && rv != "" && rv != req.ExpectedResourceVersion {
		return ErrManifestMismatch
	}
	// 필드 **이름**으로 거부하지 않습니다. `stringData`나 `data`를 정당하게 소유한
	// CRD가 있고, 이름만 보고 막으면 그런 리소스를 쓸 수 없게 됩니다.
	// Secret은 정확한 GVR과 descriptor kind 두 겹으로 이미 막혀 있습니다.
	return nil
}

// buildApplyObject는 patch에 실을 apply configuration을 만듭니다.
//
// 두 가지를 합니다 — 서버가 소유한 휘발성 필드를 **지우고**, 신원과 CAS 값을
// **덮어씁니다.** 덮어쓰는 것이 요점입니다: 사용자가 무엇을 적었든 patch는 우리가
// 검증한 객체 하나만 가리키고, API 서버가 uid 불변성과 resourceVersion 낙관적
// 동시성을 우리와 독립적으로 다시 강제합니다.
func buildApplyObject(decoded map[string]any, req DryRunRequest, desc Descriptor) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: decoded}
	delete(obj.Object, "status")
	meta, _ := obj.Object["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj.Object["metadata"] = meta
	}
	for _, field := range applyStrippedMetadata {
		delete(meta, field)
	}
	obj.Object["apiVersion"] = req.APIVersion
	obj.Object["kind"] = req.Kind
	meta["name"] = req.Name
	if desc.Namespaced {
		meta["namespace"] = req.Namespace
	} else {
		delete(meta, "namespace")
	}
	meta["uid"] = req.ExpectedUID
	meta["resourceVersion"] = req.ExpectedResourceVersion
	return obj
}

// applyStrippedMetadata는 apply configuration에 실으면 안 되는 서버 소유 필드입니다.
// uid/resourceVersion은 여기 없습니다 — 지우는 대신 **우리 값으로 덮어씁니다.**
var applyStrippedMetadata = []string{
	"managedFields", "creationTimestamp", "generation", "selfLink",
	"deletionTimestamp", "deletionGracePeriodSeconds",
}

/* ── 오류 분류 ────────────────────────────────────────────────────────────
   Kubernetes Status 원문은 여기서 끝납니다. 밖으로 나가는 것은 sentinel이거나
   서버가 새로 쓴 violation 문장뿐입니다. */

// classifyDryRunRejection은 "검토는 성공했고 답이 거절"인 경우를 가려냅니다.
// 두 번째 반환값이 false면 거절이 아니라 실패이므로 오류로 다뤄야 합니다.
func classifyDryRunRejection(err error) (DryRunResult, bool) {
	status := apierrors.APIStatus(nil)
	if !errors.As(err, &status) {
		return DryRunResult{}, false
	}
	details := status.Status().Details
	switch {
	case apierrors.IsConflict(err):
		// 필드 소유권 충돌만 거절입니다. 낙관적 동시성 실패는 "다시 읽어라"이므로
		// 검토 결과가 아니라 409입니다.
		violations := ownershipViolations(details)
		if len(violations) == 0 {
			return DryRunResult{}, false
		}
		return DryRunResult{
			Outcome:    contract.DryRunRejected,
			RejectedBy: contract.DryRunRejectedByConflict,
			Violations: violations,
		}, true
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return DryRunResult{
			Outcome:    contract.DryRunRejected,
			RejectedBy: contract.DryRunRejectedByValidation,
			Violations: validationViolations(details),
		}, true
	case apierrors.IsForbidden(err) && isAdmissionDenial(status):
		return DryRunResult{
			Outcome:    contract.DryRunRejected,
			RejectedBy: contract.DryRunRejectedByAdmission,
			Violations: []contract.ResourceDryRunViolation{{Message: msgAdmissionDenied}},
		}, true
	}
	return DryRunResult{}, false
}

// 사용자에게 보여줄 문장은 **여기 있는 것뿐**입니다. upstream 문자열을 그대로
// 옮기지 않으므로 내부 주소·webhook 이름·매니페스트 조각이 새어나갈 수 없습니다.
const (
	msgFieldOwnedByOther = "다른 field manager가 이 필드를 소유하고 있습니다. 이 대시보드는 강제 적용을 지원하지 않습니다."
	msgFieldInvalid      = "이 필드 값을 서버가 받아들이지 않았습니다."
	msgFieldUnknown      = "이 필드를 서버가 알지 못합니다. 오타이거나 이 버전에 없는 필드입니다."
	msgValidationFailed  = "서버 검증에서 거절되었습니다."
	msgAdmissionDenied   = "admission webhook이 이 변경을 거절했습니다."
)

/* Phase 1의 violation은 **서버가 쓴 문장 하나**뿐입니다.

   Status의 Causes에서 밖으로 나가는 것은 아무것도 없습니다 — Message는 물론
   Field도 upstream 원문입니다. Field는 구조적으로 보이지만 실제로는 서버가
   자유롭게 채우는 문자열이고(webhook이 만든 Status는 특히 그렇습니다), 거기에
   객체 값·내부 경로가 실려 오는 것을 우리가 막을 방법이 없습니다.

   그래서 Causes에서는 **개수와 타입**만 읽습니다. 개수는 "몇 개 필드가 막혔는가",
   타입은 "어떤 종류의 거절인가"이고 둘 다 열거형·정수입니다. Field/Manager는
   비워 둡니다 — 신뢰할 수 있는 타입 필드가 생기면 그때 채웁니다. */

// ownershipViolations는 소유권 충돌 원인 **개수만큼** 고정 문장을 만듭니다.
// cause.Field·cause.Message는 읽지 않습니다.
func ownershipViolations(details *metav1.StatusDetails) []contract.ResourceDryRunViolation {
	if details == nil {
		return nil
	}
	out := make([]contract.ResourceDryRunViolation, 0, len(details.Causes))
	for _, cause := range details.Causes {
		if cause.Type != metav1.CauseTypeFieldManagerConflict {
			continue
		}
		if len(out) >= contract.MaxDryRunViolations {
			break
		}
		out = append(out, contract.ResourceDryRunViolation{Message: msgFieldOwnedByOther})
	}
	return out
}

// validationViolations는 cause **타입**만 보고 고정 문장을 고릅니다.
// cause.Field·cause.Message는 읽지 않습니다.
func validationViolations(details *metav1.StatusDetails) []contract.ResourceDryRunViolation {
	if details == nil || len(details.Causes) == 0 {
		return []contract.ResourceDryRunViolation{{Message: msgValidationFailed}}
	}
	out := make([]contract.ResourceDryRunViolation, 0, len(details.Causes))
	for _, cause := range details.Causes {
		if len(out) >= contract.MaxDryRunViolations {
			break
		}
		message := msgFieldInvalid
		if cause.Type == metav1.CauseTypeFieldValueNotSupported || cause.Type == metav1.CauseTypeUnexpectedServerResponse {
			message = msgFieldUnknown
		}
		out = append(out, contract.ResourceDryRunViolation{Message: message})
	}
	if len(out) == 0 {
		return []contract.ResourceDryRunViolation{{Message: msgValidationFailed}}
	}
	return out
}

// isAdmissionDenial은 403이 webhook 거절인지 RBAC 부족인지 가릅니다.
//
// 두 경우가 같은 코드로 오기 때문에 문자열을 볼 수밖에 없습니다. 확신할 수 없으면
// **RBAC 쪽으로 판정**합니다 — 배포 설정 문제를 "정책이 막았다"로 감추는 것보다
// 운영자에게 502를 보여 주는 편이 낫습니다.
func isAdmissionDenial(status apierrors.APIStatus) bool {
	return strings.Contains(status.Status().Message, "admission webhook")
}

// classifyDryRunError는 upstream 실패를 sentinel로 바꿉니다.
// **원본 오류를 감싸지 않습니다** — 감싸면 Status 본문이 로그로 흘러갑니다.
func classifyDryRunError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return context.DeadlineExceeded
	case errors.Is(err, errBodyTooLarge), apierrors.IsRequestEntityTooLargeError(err):
		return ErrTooLarge
	case apierrors.IsNotFound(err):
		return ErrObjectNotFound
	case apierrors.IsConflict(err):
		return ErrResourceVersionMismatch
	case apierrors.IsTooManyRequests(err):
		return ErrDryRunRateLimited
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return ErrDryRunForbidden
	default:
		return ErrDryRunUpstream
	}
}

/* ── admission warning 신호 ───────────────────────────────────────────────

   **경고 원문은 어디에도 저장하지 않습니다.** Warning 헤더는 admission webhook이
   자유롭게 쓰는 문자열이라 대상 객체의 필드 값·내부 주소·정책 세부가 그대로 실려
   올 수 있습니다. 그것을 응답에 옮기면 정제 경계를 우회하는 통로가 됩니다.

   그래서 수집기는 **불리언 하나**만 들고 있고, 응답에는 서버가 직접 쓴 고정 문장
   하나만 나갑니다. 개수도 원문에서 오지 않습니다. */

// dryRunTextBytes는 violation 문자열·경로 하나의 상한입니다.
const dryRunTextBytes = 512

// msgUpstreamWarned는 경고가 있었다는 사실만 알리는 **서버 작성** 문장입니다.
// 이 상수 말고 다른 문자열이 Warnings에 들어가는 경로는 없습니다.
const msgUpstreamWarned = "API 서버가 검토 요청에 경고를 반환했습니다. 원문은 보안상 표시하지 않습니다."

type warningSinkKey struct{}

// warningSink는 "경고가 있었는가"만 기록합니다. 헤더 값은 읽지도 보관하지도 않습니다.
type warningSink struct {
	mu   sync.Mutex
	seen bool
}

func withWarningSink(ctx context.Context, sink *warningSink) context.Context {
	return context.WithValue(ctx, warningSinkKey{}, sink)
}

func warningSinkFrom(ctx context.Context) *warningSink {
	sink, _ := ctx.Value(warningSinkKey{}).(*warningSink)
	return sink
}

// add는 헤더의 **존재만** 봅니다. 내용을 파싱하지 않으므로 원문이 프로세스 안에서도
// 다른 자료구조로 옮겨지지 않습니다.
func (w *warningSink) add(present bool) {
	if w == nil || !present {
		return
	}
	w.mu.Lock()
	w.seen = true
	w.mu.Unlock()
}

// take는 경고 여부를 서버 작성 문장으로 바꿔 돌려주고 비웁니다.
// 언제나 non-nil입니다 — 계약이 필수 배열입니다.
func (w *warningSink) take() []string {
	if w == nil {
		return []string{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.seen {
		return []string{}
	}
	w.seen = false
	return []string{msgUpstreamWarned}
}

// warningTransport는 Warning 헤더가 **있었는지만** 그 요청의 수집기에 알립니다.
// 헤더 값은 읽지 않습니다. 수집기가 없으면 아무것도 하지 않습니다.
type warningTransport struct{ base http.RoundTripper }

func (t *warningTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if resp != nil {
		if sink := warningSinkFrom(req.Context()); sink != nil {
			sink.add(len(resp.Header.Values("Warning")) > 0)
		}
	}
	return resp, err
}

/* ── 문자열 유계화 ───────────────────────────────────────────────────────── */

// boundedText는 제어문자를 지우고 UTF-8 경계에서 자릅니다.
// 바이트 한가운데서 자르면 잘못된 UTF-8이 JSON으로 나갑니다.
func boundedText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	return truncateUTF8(s, max)
}

// truncateUTF8은 max 바이트를 넘지 않는 가장 긴 rune 경계까지 자릅니다.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8Start는 이 바이트가 rune의 첫 바이트인지입니다(continuation byte가 아닌지).
func utf8Start(b byte) bool { return b&0xc0 != 0x80 }
