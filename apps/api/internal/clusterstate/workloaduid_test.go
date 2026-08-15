package clusterstate_test

import (
	"context"
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

// Pod EntityRef가 owner 체인(Deployment → ReplicaSet → Pod)에서 Workload UID를
// 이어받는지 확인합니다. 이름만 있으면 재생성·동명 워크로드와 상관이 섞입니다. (이슈 #4)
func TestPodRefCarriesWorkloadUIDFromOwnerChain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	pods, err := store.PodsForWorkload("payments", "Deployment", "payments-api", testcluster.UIDDeploymentPaymentsAPI)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) == 0 {
		t.Fatal("전제: Pod가 있어야 합니다")
	}
	for _, p := range pods {
		if p.Ref.WorkloadUID != testcluster.UIDDeploymentPaymentsAPI {
			t.Errorf("pod %s의 WorkloadUID=%q, want %q", p.Name, p.Ref.WorkloadUID, testcluster.UIDDeploymentPaymentsAPI)
		}
	}
}

// 데이터소스 어댑터가 빌려 쓰는 CatalogPods에도 같은 Workload UID가 실려야 합니다.
func TestCatalogPodsCarryWorkloadUID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := testcluster.NewStore(t, ctx)

	found := false
	for _, cp := range store.CatalogPods(testcluster.ClusterID, "payments", 0) {
		if cp.WorkloadKind == "Deployment" && cp.WorkloadName == "payments-api" {
			found = true
			if cp.WorkloadUID != testcluster.UIDDeploymentPaymentsAPI {
				t.Errorf("catalog pod %s의 WorkloadUID=%q, want %q", cp.Name, cp.WorkloadUID, testcluster.UIDDeploymentPaymentsAPI)
			}
		}
	}
	if !found {
		t.Fatal("전제: payments-api 소속 Pod가 카탈로그에 있어야 합니다")
	}
}
