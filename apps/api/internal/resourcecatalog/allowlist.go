// Package resourcecatalog는 API discovery 기반 Resource Explorer의 서버 측 상태입니다. (ADR 0018)
//
// 세 가지 경계가 이 패키지의 존재 이유입니다.
//   - **요청 처리 경로에서 discovery도 Kubernetes LIST도 호출하지 않습니다.** 카탈로그와
//     목록은 프로세스 안의 불변 snapshot과 metadata informer 인덱스에서만 읽습니다. (ADR 0004)
//   - **allowlist에 등록된 GVR만** PartialObjectMetadata informer를 얻습니다. metadata를
//     지원하지 않는 aggregated API(406)는 full-object watch로 조용히 물러나지 않고
//     unsupported로 드러냅니다.
//   - 상세 조회만 격리된 dynamic client로 live GET합니다. ADR 0004의 명시적 예외이며
//     조회 경로와 client·rate limit·timeout을 공유하지 않습니다.
package resourcecatalog

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	apivalidation "k8s.io/apimachinery/pkg/util/validation"
)

// CoreGroupAlias는 URL 경로와 설정에서 core group(빈 문자열)을 가리키는 예약어입니다.
// 경로 세그먼트는 비어 있을 수 없으므로 "core"로 표기합니다.
const CoreGroupAlias = "core"

// MaxAllowlistEntries는 allowlist 크기 상한입니다. informer 하나당 watch 하나이므로
// 상한이 곧 이 기능이 클러스터에 주는 부하의 상한입니다.
const MaxAllowlistEntries = 64

// ErrInvalidGVR은 형식이 맞지 않는 group/version/resource입니다.
var ErrInvalidGVR = errors.New("invalid group/version/resource")

// builtinGroups는 Kubernetes가 직접 제공하는 API group입니다.
// 여기에 없는 group은 CRD로 보고 명시적 opt-in을 요구합니다. (ADR 0018 결정 3)
var builtinGroups = map[string]bool{
	"":                             true,
	"admissionregistration.k8s.io": true,
	"apiextensions.k8s.io":         true,
	"apiregistration.k8s.io":       true,
	"apps":                         true,
	"authentication.k8s.io":        true,
	"authorization.k8s.io":         true,
	"autoscaling":                  true,
	"batch":                        true,
	"certificates.k8s.io":          true,
	"coordination.k8s.io":          true,
	"discovery.k8s.io":             true,
	"events.k8s.io":                true,
	"flowcontrol.apiserver.k8s.io": true,
	"networking.k8s.io":            true,
	"node.k8s.io":                  true,
	"policy":                       true,
	"rbac.authorization.k8s.io":    true,
	"scheduling.k8s.io":            true,
	"storage.k8s.io":               true,
}

// IsBuiltinGroup은 Kubernetes 내장 API group인지 알려줍니다.
func IsBuiltinGroup(group string) bool { return builtinGroups[group] }

// DefaultAllowlist는 운영 진단에 자주 필요하면서 관측 informer(clusterstate)가 이미
// watch하지 않는 리소스만 담습니다. 같은 리소스에 informer를 두 번 붙이면 watch가
// 두 배가 됩니다. CRD는 들어 있지 않습니다 — 명시적 opt-in 대상입니다.
func DefaultAllowlist() []schema.GroupVersionResource {
	return mustParseAll(
		"core/v1/services",
		"core/v1/configmaps",
		"core/v1/secrets",
		"core/v1/serviceaccounts",
		"core/v1/persistentvolumeclaims",
		"core/v1/persistentvolumes",
		"discovery.k8s.io/v1/endpointslices",
		"batch/v1/jobs",
		"networking.k8s.io/v1/ingresses",
		"networking.k8s.io/v1/networkpolicies",
		"autoscaling/v2/horizontalpodautoscalers",
		"policy/v1/poddisruptionbudgets",
		"storage.k8s.io/v1/storageclasses",
	)
}

func mustParseAll(raw ...string) []schema.GroupVersionResource {
	out := make([]schema.GroupVersionResource, 0, len(raw))
	for _, r := range raw {
		gvr, err := ParseGVR(r)
		if err != nil {
			panic("resourcecatalog: 기본 allowlist가 잘못되었습니다: " + r)
		}
		out = append(out, gvr)
	}
	return out
}

// ParseGVR은 "group/version/resource"를 해석합니다. group은 "core" 또는 생략으로
// core group을 나타냅니다("v1/pods" == "core/v1/pods").
func ParseGVR(raw string) (schema.GroupVersionResource, error) {
	parts := strings.Split(strings.TrimSpace(raw), "/")
	var group, version, resource string
	switch len(parts) {
	case 2:
		version, resource = parts[0], parts[1]
	case 3:
		group, version, resource = parts[0], parts[1], parts[2]
	default:
		return schema.GroupVersionResource{}, fmt.Errorf("%w: %q", ErrInvalidGVR, raw)
	}
	if group == CoreGroupAlias {
		group = ""
	}
	if err := ValidateGVRSegments(group, version, resource); err != nil {
		return schema.GroupVersionResource{}, err
	}
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}, nil
}

// ValidateGVRSegments는 사용자가 보낸 경로 세그먼트를 검사합니다.
// allowlist 대조 전에 형식부터 막아, 임의 문자열이 discovery/informer 키가 되지 않게 합니다.
//
// 규칙은 직접 만들지 않고 Kubernetes가 쓰는 것을 그대로 씁니다.
//   - group: DNS1123 subdomain (core group은 빈 문자열)
//   - version: DNS1035 label — apiextensions가 CRD 버전 이름에 적용하는 규칙입니다
//   - resource(복수형): DNS1035 label — CRD 복수형은 하이픈을 포함할 수 있습니다
//
// 임의로 더 넓힌 정규식이 아니라 이 규칙을 쓰면, 클러스터가 실제로 제공할 수 있는
// CRD는 전부 통과하면서도 경로 안전성은 그대로입니다 — 두 label 규칙 모두 소문자
// 영숫자·하이픈(그리고 group의 점)만 허용하므로 `/`·`%`·`..`·대문자가 끼어들 수 없습니다.
func ValidateGVRSegments(group, version, resource string) error {
	if group != "" {
		if msgs := apivalidation.IsDNS1123Subdomain(group); len(msgs) > 0 {
			return fmt.Errorf("%w: group %q: %s", ErrInvalidGVR, group, strings.Join(msgs, "; "))
		}
	}
	if msgs := apivalidation.IsDNS1035Label(version); len(msgs) > 0 {
		return fmt.Errorf("%w: version %q: %s", ErrInvalidGVR, version, strings.Join(msgs, "; "))
	}
	if msgs := apivalidation.IsDNS1035Label(resource); len(msgs) > 0 {
		return fmt.Errorf("%w: resource %q: %s", ErrInvalidGVR, resource, strings.Join(msgs, "; "))
	}
	return nil
}

// GroupSegment는 경로·계약에 실을 group 표기입니다. core group은 "core"입니다.
func GroupSegment(group string) string {
	if group == "" {
		return CoreGroupAlias
	}
	return group
}

// FormatGVR은 설정·로그·cursor 지문에 쓰는 정규 표기입니다.
func FormatGVR(gvr schema.GroupVersionResource) string {
	return GroupSegment(gvr.Group) + "/" + gvr.Version + "/" + gvr.Resource
}

// NormalizeAllowlist는 중복을 제거하고 정렬한 allowlist를 만듭니다.
// allowCRDs가 false면 내장 group이 아닌 항목에서 실패합니다.
func NormalizeAllowlist(in []schema.GroupVersionResource, allowCRDs bool) ([]schema.GroupVersionResource, error) {
	if len(in) == 0 {
		return nil, errors.New("allowlist가 비어 있습니다")
	}
	seen := make(map[schema.GroupVersionResource]bool, len(in))
	out := make([]schema.GroupVersionResource, 0, len(in))
	for _, gvr := range in {
		if err := ValidateGVRSegments(gvr.Group, gvr.Version, gvr.Resource); err != nil {
			return nil, err
		}
		if !allowCRDs && !IsBuiltinGroup(gvr.Group) {
			return nil, fmt.Errorf("CRD group %q에는 명시적 opt-in이 필요합니다", gvr.Group)
		}
		if seen[gvr] {
			continue
		}
		seen[gvr] = true
		out = append(out, gvr)
	}
	if len(out) > MaxAllowlistEntries {
		return nil, fmt.Errorf("allowlist 항목이 %d개를 넘습니다: %d", MaxAllowlistEntries, len(out))
	}
	sort.Slice(out, func(i, j int) bool { return FormatGVR(out[i]) < FormatGVR(out[j]) })
	return out, nil
}

/* ── 변경 검토 dry-run 대상 (ADR 0019 Phase 1) ────────────────────────────

   검토 대상은 조회 allowlist의 **부분집합**입니다. 조회조차 하지 않는 리소스를
   검토 대상으로 삼을 수는 없고, 부분집합이어야 informer가 이미 들고 있는 metadata
   인덱스로 신원 게이트를 통과시킬 수 있습니다.

   그 위에 **설정으로 뚫을 수 없는** 거부 목록이 있습니다. 값 자체가 비밀이거나
   (Secret) 신원·권한·스키마의 근원인 리소스(ServiceAccount·Node·Namespace·
   RBAC API 전체·CustomResourceDefinition)는 opt-in 목록에 적어도 거부됩니다.
   그 외에는 막지 않습니다 — admission 설정·인증서·API 등록 같은 리소스는 이
   목록에 없고, 운영자가 명시적으로 켜면 검토할 수 있습니다. */

// MaxDryRunAllowlistEntries는 검토 opt-in 목록의 상한입니다.
const MaxDryRunAllowlistEntries = 64

// groupResource는 버전과 무관한 거부 키입니다. v1을 막고 v2를 여는 실수를 없앱니다.
type groupResource struct{ group, resource string }

// dryRunDeniedResources는 설정과 무관하게 거부하는 (group, resource)입니다.
//
// secrets/serviceaccounts는 값·신원 그 자체이고, nodes/namespaces는 클러스터
// 경계입니다. 버전을 명시하지 않는 것이 요점입니다 — 새 버전이 생겨도 계속 막힙니다.
var dryRunDeniedResources = map[groupResource]bool{
	{group: "", resource: "secrets"}:         true,
	{group: "", resource: "serviceaccounts"}: true,
	{group: "", resource: "nodes"}:           true,
	{group: "", resource: "namespaces"}:      true,
	// 스키마 자체를 바꾸는 리소스입니다. group 전체가 아니라 **이 하나**만
	// 막습니다 — 같은 group의 다른 리소스까지 통째로 거부할 이유가 없습니다.
	{group: "apiextensions.k8s.io", resource: "customresourcedefinitions"}: true,
}

// dryRunDeniedGroups는 group 전체를 거부합니다. **승인된 하나뿐입니다** —
// 권한을 정의하는 RBAC API입니다. (ADR 0019 결정 1)
//
// 여기에 없는 group을 임의로 더하지 않습니다. 넓게 막으면 정당한 운영 대상까지
// 사라지고, 그 사실이 UI에서는 "기능이 없다"로만 보입니다.
var dryRunDeniedGroups = map[string]bool{
	"rbac.authorization.k8s.io": true,
}

// DryRunHardDenied는 설정으로 되돌릴 수 없는 거부인지 알려줍니다.
//
// 판정 근거는 **정확한 GVR**뿐입니다. kind 텍스트로 판정하지 않습니다 —
// `Secret`이라는 이름을 쓰는 CRD를 코어 Secret과 같은 것으로 취급하면,
// 아무 관계 없는 사용자 리소스가 이유 없이 막힙니다. 본문 kind는 선택된
// descriptor의 kind와 대조하는 방식으로 따로 검증합니다.
func DryRunHardDenied(gvr schema.GroupVersionResource) bool {
	if dryRunDeniedGroups[gvr.Group] {
		return true
	}
	return dryRunDeniedResources[groupResource{group: gvr.Group, resource: gvr.Resource}]
}

// NormalizeDryRunAllowlist는 검토 대상 목록을 확정합니다.
//
//   - explorer allowlist의 부분집합이어야 합니다(아니면 오류).
//   - hard-deny 항목은 오류입니다 — 조용히 빼면 운영자가 켜졌다고 착각합니다.
//   - deny는 **빼기**입니다. 배포가 opt-in 목록을 좁히려고 쓰는 안전망이므로
//     겹치는 것은 오류가 아니라 제거입니다.
//   - 비어 있는 것은 오류가 아닙니다. 기능이 켜져 있어도 대상이 없을 뿐입니다.
func NormalizeDryRunAllowlist(in, explorer, deny []schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	if len(in) == 0 {
		return nil, nil
	}
	allowed := make(map[schema.GroupVersionResource]bool, len(explorer))
	for _, gvr := range explorer {
		allowed[gvr] = true
	}
	denied := make(map[schema.GroupVersionResource]bool, len(deny))
	for _, gvr := range deny {
		if err := ValidateGVRSegments(gvr.Group, gvr.Version, gvr.Resource); err != nil {
			return nil, err
		}
		denied[gvr] = true
	}
	seen := make(map[schema.GroupVersionResource]bool, len(in))
	out := make([]schema.GroupVersionResource, 0, len(in))
	for _, gvr := range in {
		if err := ValidateGVRSegments(gvr.Group, gvr.Version, gvr.Resource); err != nil {
			return nil, err
		}
		if DryRunHardDenied(gvr) {
			return nil, fmt.Errorf("%s는 변경 검토 대상이 될 수 없습니다", FormatGVR(gvr))
		}
		if !allowed[gvr] {
			return nil, fmt.Errorf("%s가 Resource Explorer allowlist에 없습니다", FormatGVR(gvr))
		}
		if denied[gvr] || seen[gvr] {
			continue
		}
		seen[gvr] = true
		out = append(out, gvr)
	}
	if len(out) > MaxDryRunAllowlistEntries {
		return nil, fmt.Errorf("변경 검토 대상이 %d개를 넘습니다: %d", MaxDryRunAllowlistEntries, len(out))
	}
	sort.Slice(out, func(i, j int) bool { return FormatGVR(out[i]) < FormatGVR(out[j]) })
	return out, nil
}
