package resourcecatalog

// 최근 항목 재해석 (ADR 0023 결정 7)
// --------------------------------------------------------------------------
// 브라우저는 UID와 이동 경로만 들고 있고 **표시할 제목은 매번 서버가 다시 정합니다.**
// 그래야 권한이 사라졌거나 객체가 교체된 항목이 화면에 남지 않습니다.
//
// 해석은 전부 로컬 metadata 인덱스에서 이뤄집니다 — 참조 하나가 이분탐색 한 번이고,
// **Kubernetes API 호출은 0회**입니다. 상세 live GET은 사용자가 항목을 실제로 열 때만
// 나가며 그 경로는 ADR 0018 그대로입니다.
//
// 무엇이 오류이고 무엇이 조용한 제거인지 선을 분명히 둡니다.
//
//   - **크기·개수 위반은 오류입니다**(400). 요청 자체가 계약을 벗어났다는 뜻이므로
//     조용히 자르면 클라이언트가 잘린 줄 모릅니다.
//   - **구조 위반은 오류입니다**(400). 서버가 만든 적 없는 형식이므로 정상 경로가 아닙니다.
//   - **해석되지 않는 항목은 조용히 제거합니다.** allowlist에서 빠졌거나, 동기화 중이거나,
//     Scope 밖이거나, 인덱스에 없거나, UID가 바뀐 경우입니다. 이것이 ADR 결정 7이 말하는
//     "forbidden/stale은 조용히 제거"입니다.

import (
	"encoding/base64"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// MaxRecentRefs는 한 요청이 담을 수 있는 참조 수입니다.
	MaxRecentRefs = 20
	// MaxRecentRefLen은 인코딩된 참조 하나의 길이 상한입니다.
	//
	// 원문 최대치는 버전(1) + GVR(349) + namespace(63) + 이름(253) + UID(64) +
	// 구분자(4) = 734바이트이고 base64url로 980자입니다.
	MaxRecentRefLen = 1024
	// MaxRecentQueryBytes는 원본 query string 전체의 상한입니다.
	//
	// 웹은 이보다 낮은 6KiB에서 요청을 나눠 보냅니다(Phase 3). 서버는 그 약속을
	// 믿지 않고 자기 상한을 따로 강제합니다.
	MaxRecentQueryBytes = 8 << 10

	recentRefVersion = "1"
)

// RecentRef는 브라우저가 들고 있던 참조 하나입니다.
type RecentRef struct {
	Group     string
	Version   string
	Resource  string
	Namespace string
	Name      string
	UID       string
}

// RecentItem은 서버가 다시 확인해 준 항목입니다. 제목의 근거(kind·이름)는 전부 서버 값입니다.
type RecentItem struct {
	Group      string
	Version    string
	Resource   string
	Kind       string
	Namespaced bool
	Namespace  string
	Name       string
	UID        string
}

// EncodeRecentRef는 참조 하나를 compact base64url 문자열로 만듭니다.
// 서버가 만들 일은 없고 테스트와 문서가 같은 형식을 쓰기 위한 정본입니다.
func EncodeRecentRef(ref RecentRef) string {
	raw := strings.Join([]string{
		recentRefVersion,
		GroupSegment(ref.Group) + "/" + ref.Version + "/" + ref.Resource,
		ref.Namespace, ref.Name, ref.UID,
	}, cursorSep)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseRecentRef는 인코딩된 참조를 해석합니다. 구조가 어긋나면 ErrInvalidFilter입니다.
func ParseRecentRef(encoded string) (RecentRef, error) {
	if encoded == "" || len(encoded) > MaxRecentRefLen {
		return RecentRef{}, ErrInvalidFilter
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return RecentRef{}, ErrInvalidFilter
	}
	parts := strings.Split(string(raw), cursorSep)
	if len(parts) != 5 || parts[0] != recentRefVersion {
		return RecentRef{}, ErrInvalidFilter
	}
	gvr, err := ParseGVR(parts[1])
	if err != nil {
		return RecentRef{}, ErrInvalidFilter
	}
	ref := RecentRef{
		Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource,
		Namespace: parts[2], Name: parts[3], UID: parts[4],
	}
	switch {
	case len(ref.Namespace) > maxCursorNSLen || !safeCursorSegment(ref.Namespace):
		return RecentRef{}, ErrInvalidFilter
	case ref.Name == "" || len(ref.Name) > maxCursorName || !safeCursorSegment(ref.Name):
		return RecentRef{}, ErrInvalidFilter
	case ref.UID == "" || len(ref.UID) > maxCursorUIDLen || !safeCursorSegment(ref.UID):
		return RecentRef{}, ErrInvalidFilter
	}
	return ref, nil
}

// ParseRecentRefs는 요청의 참조 목록 전체를 해석합니다.
// 개수·길이 위반은 오류이고, 여기서 통과한 참조만 Recent가 해석합니다.
func ParseRecentRefs(encoded []string) ([]RecentRef, error) {
	if len(encoded) > MaxRecentRefs {
		return nil, ErrInvalidFilter
	}
	out := make([]RecentRef, 0, len(encoded))
	for _, raw := range encoded {
		ref, err := ParseRecentRef(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, nil
}

// Recent는 참조 목록을 지금의 인덱스로 다시 확인합니다.
//
// 순서는 **입력 순서 그대로**입니다 — 브라우저의 최신순을 서버가 뒤집지 않습니다.
// 웹이 요청을 여러 번으로 나눠 보내더라도 각 응답이 입력 순서를 지키므로
// 클라이언트가 그대로 이어 붙이면 원래 순서가 됩니다.
func (s *Service) Recent(refs []RecentRef, filter NamespaceFilter) ([]RecentItem, error) {
	if !s.Available() {
		return nil, ErrUnavailable
	}
	// 최근 항목은 팔레트의 일부입니다. 검색을 끈 배포에서 이 경로만 살아 있으면
	// 롤백 스위치가 "검색을 끈다"는 약속을 절반만 지키는 셈입니다. (ADR 0023 롤백)
	if !s.cfg.SearchEnabled {
		return nil, ErrSearchDisabled
	}
	if len(refs) > MaxRecentRefs {
		return nil, ErrInvalidFilter
	}
	allowed := sortedUnique(filter.List)
	out := make([]RecentItem, 0, len(refs))
	for _, ref := range refs {
		gvr := schema.GroupVersionResource{Group: ref.Group, Version: ref.Version, Resource: ref.Resource}
		desc, err := s.Describe(gvr)
		if err != nil || desc.State != StateReady {
			continue // allowlist 밖·동기화 중·미지원은 조용히 사라집니다.
		}
		if desc.Namespaced {
			if ref.Namespace == "" || !namespaceAllowed(ref.Namespace, filter.All, allowed) {
				continue
			}
		} else {
			// 클러스터 범위 리소스는 클러스터 전체 권한이 있을 때만 존재가 보입니다.
			if ref.Namespace != "" || !filter.All {
				continue
			}
		}
		// **해석은 목록 스냅숏 하나입니다.** 증분 검색 인덱스를 끌어들이면 최근 항목의
		// 해석이 그 인덱스의 회수 상태에 묶이고, 숨겨진 namespace의 stale이나 GVR 승급이
		// 허용된 참조의 결과를 바꿀 수 있습니다. Scope 안에서 보이는 것만으로 답합니다.
		index := s.entries[gvr].baselineIndex()
		if index == nil {
			continue
		}
		row, found := index.lookup(ref.Namespace, ref.Name)
		if !found || row.obj == nil || string(row.obj.UID) != ref.UID {
			continue // 삭제됐거나 같은 이름의 다른 객체로 교체된 항목입니다.
		}
		out = append(out, RecentItem{
			Group: GroupSegment(gvr.Group), Version: gvr.Version, Resource: gvr.Resource,
			Kind: desc.Kind, Namespaced: desc.Namespaced,
			Namespace: row.namespace, Name: row.name, UID: string(row.obj.UID),
		})
	}
	return out, nil
}

// namespaceAllowed는 Scope가 이 namespace를 허용하는지입니다.
func namespaceAllowed(ns string, all bool, allowed []string) bool {
	if all {
		return true
	}
	for _, a := range allowed {
		if a == ns {
			return true
		}
	}
	return false
}
