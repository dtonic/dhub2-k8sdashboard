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
