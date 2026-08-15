package clusterstate

import (
	"sort"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

// CatalogPods는 데이터소스 어댑터가 화면과 **같은 Pod 신원**을 쓰도록 informer 캐시를 빌려줍니다.
//
// 어댑터가 Pod 이름이나 UID를 지어내면 로그 한 줄에서 Pod 상세로 가는 링크가 404가 됩니다.
// 신원의 출처는 항상 여기 하나입니다.
func (s *Store) CatalogPods(clusterID, namespace string, limit int) []datasource.CatalogPod {
	if s == nil || clusterID != s.opts.ClusterID {
		return nil
	}
	pods, err := s.listPods(namespace)
	if err != nil {
		return nil
	}
	out := make([]datasource.CatalogPod, 0, len(pods))
	for _, p := range pods {
		kind, name, uid := s.workloadOfPod(p)
		out = append(out, datasource.CatalogPod{
			Namespace:    p.Namespace,
			Name:         p.Name,
			UID:          string(p.UID),
			WorkloadKind: kind,
			WorkloadName: name,
			WorkloadUID:  uid,
			Node:         p.Spec.NodeName,
		})
	}
	// 안정적인 순서여야 로그·토폴로지 결과가 갱신마다 흔들리지 않습니다.
	sort.Slice(out, func(a, b int) bool {
		if out[a].Namespace != out[b].Namespace {
			return out[a].Namespace < out[b].Namespace
		}
		return out[a].Name < out[b].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// StreamEntityNamespaces returns a server-owned UID-to-namespace snapshot for
// alert stream authorization. Alert labels and EntityRef.Namespace are upstream
// input and must never establish an SSE namespace boundary.
func (s *Store) StreamEntityNamespaces() map[string]string {
	out := make(map[string]string)
	for _, p := range s.CatalogPods(s.opts.ClusterID, "", 0) {
		if p.UID != "" {
			out["pod:"+p.UID] = p.Namespace
		}
		if p.WorkloadUID != "" {
			out["workload:"+p.WorkloadUID] = p.Namespace
		}
	}
	if workloads, err := s.Workloads(NamespaceFilter{All: true}); err == nil {
		for _, w := range workloads {
			if w.Ref.WorkloadUID != "" {
				out["workload:"+w.Ref.WorkloadUID] = w.Namespace
			}
		}
	}
	return out
}

var _ datasource.PodCatalog = (*Store)(nil)
