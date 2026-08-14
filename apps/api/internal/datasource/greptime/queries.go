// Target → 쿼리 카탈로그 Scope 변환입니다.
//
// 질의 템플릿과 escape는 internal/querycatalog(#9)에 있습니다. 이 파일이 하는 일은
// 하나입니다 — 화면의 신원(UID·워크로드)을 메트릭 라벨의 어휘(pod 이름)로
// 바꾸는 것. 변환은 항상 informer 카탈로그에서 신원을 빌려서 합니다. (CLAUDE.md)
package greptime

import (
	"strings"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
)

// scope는 Target을 카탈로그 Scope로 바꿉니다. 두 번째 반환값이 false면
// 대상 Pod가 카탈로그에 없다는 뜻입니다 — 질의하지 말고 빈 시리즈를 둡니다.
func (s *Source) scope(t datasource.Target) (querycatalog.Scope, bool) {
	sc := querycatalog.Scope{}

	switch {
	case t.Namespace != "":
		sc.Namespace = t.Namespace
	case len(t.Namespaces) > 0:
		sc.Namespaces = t.Namespaces
	}

	switch {
	case t.PodUID != "":
		pod, ok := s.podByUID(t)
		if !ok {
			return querycatalog.Scope{}, false
		}
		sc.PodName = pod.Name
		sc.Namespace = pod.Namespace
		sc.Namespaces = nil
	case t.WorkloadName != "":
		names := s.workloadPodNames(t)
		if len(names) == 0 {
			return querycatalog.Scope{}, false
		}
		sc.PodNames = names
	}

	return sc, true
}

func (s *Source) podByUID(t datasource.Target) (datasource.CatalogPod, bool) {
	for _, p := range s.catalog.CatalogPods(t.Namespace, 0) {
		if p.UID == t.PodUID {
			return p, true
		}
	}
	return datasource.CatalogPod{}, false
}

func (s *Source) workloadPodNames(t datasource.Target) []string {
	var names []string
	for _, p := range s.catalog.CatalogPods(t.Namespace, 0) {
		if p.WorkloadName != t.WorkloadName {
			continue
		}
		if t.WorkloadKind != "" && !strings.EqualFold(p.WorkloadKind, t.WorkloadKind) {
			continue
		}
		if !t.AllowsNamespace(p.Namespace) {
			continue
		}
		names = append(names, p.Name)
	}
	return names
}
