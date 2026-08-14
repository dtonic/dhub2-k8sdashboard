package clusterstate_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

func int32p(v int32) *int32 { return &v }

// TestNormalizeStatefulSetRollout — StatefulSet은 Progressing 조건이 없어
// revision 비교로 롤아웃을 판단합니다.
func TestNormalizeStatefulSetRollout(t *testing.T) {
	sts := &appsv1.StatefulSet{}
	sts.Spec.Replicas = int32p(3)
	sts.Status.ReadyReplicas = 3
	sts.Status.AvailableReplicas = 3
	sts.Status.UpdatedReplicas = 3
	sts.Status.CurrentRevision = "rev-1"
	sts.Status.UpdateRevision = "rev-1"

	st := clusterstate.NormalizeStatefulSet(sts)
	if st.Rollout.Status != "Complete" || st.Severity != contract.SeverityHealthy {
		t.Fatalf("정착 상태: %+v", st)
	}

	// 새 리비전 롤아웃 중 + ready 부족
	sts.Status.UpdateRevision = "rev-2"
	sts.Status.ReadyReplicas = 1
	st = clusterstate.NormalizeStatefulSet(sts)
	if st.Rollout.Status != "Progressing" {
		t.Fatalf("롤아웃 진행이 감지되지 않았습니다: %+v", st.Rollout)
	}
	if st.Severity == contract.SeverityHealthy {
		t.Fatal("replica 부족이 심각도에 반영되지 않았습니다")
	}

	// replicas nil이면 기본 1입니다.
	empty := &appsv1.StatefulSet{}
	if got := clusterstate.NormalizeStatefulSet(empty).Replicas.Desired; got != 1 {
		t.Fatalf("기본 desired: %d", got)
	}
}

// TestNormalizeDaemonSetRollout — DaemonSet의 desired는 노드 수입니다.
func TestNormalizeDaemonSetRollout(t *testing.T) {
	ds := &appsv1.DaemonSet{}
	ds.Status.DesiredNumberScheduled = 4
	ds.Status.NumberReady = 4
	ds.Status.NumberAvailable = 4
	ds.Status.UpdatedNumberScheduled = 4
	st := clusterstate.NormalizeDaemonSet(ds)
	if st.Rollout.Status != "Complete" || st.Replicas.Desired != 4 {
		t.Fatalf("정착 상태: %+v", st)
	}

	ds.Status.UpdatedNumberScheduled = 2
	ds.Status.NumberReady = 2
	st = clusterstate.NormalizeDaemonSet(ds)
	if st.Rollout.Status != "Progressing" {
		t.Fatalf("노드 롤아웃이 감지되지 않았습니다: %+v", st.Rollout)
	}
}

// TestStoreIdentityAndReadiness — 부팅 상태·클러스터 신원 접근자입니다.
func TestStoreIdentityAndReadiness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, _ := testcluster.NewStore(t, ctx)

	if !store.HasSynced() {
		t.Fatal("픽스처 store는 동기화되어 있어야 합니다")
	}
	if store.ClusterID() != testcluster.ClusterID || store.ClusterName() == "" {
		t.Fatalf("클러스터 신원: %s %s", store.ClusterID(), store.ClusterName())
	}
}

// TestNamespaceSummaryLookup — 단일 namespace 요약과 없는 namespace의 구분입니다.
func TestNamespaceSummaryLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, _ := testcluster.NewStore(t, ctx)

	sum, found, err := store.NamespaceSummary("payments")
	if err != nil || !found {
		t.Fatalf("payments 요약: %v %v", found, err)
	}
	if sum.Name != "payments" || sum.Pods.Total == 0 {
		t.Fatalf("요약 내용: %+v", sum)
	}
	if _, found, _ := store.NamespaceSummary("does-not-exist"); found {
		t.Fatal("없는 namespace가 발견되었습니다 — empty와 구분되어야 합니다")
	}
}

// TestPodSummaryAndOwnerChain — Pod 요약과 소유 체인, 사용량 병합입니다.
func TestPodSummaryAndOwnerChain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, _ := testcluster.NewStore(t, ctx)

	// 사용량 스냅숏을 붙이면 Pod 요약에 실사용량이 병합됩니다.
	store.SetUsage(func(uid string) (contract.ContainerUsage, bool) {
		if uid == testcluster.UIDPodHealthy {
			return contract.ContainerUsage{CPUMilli: 123, MemoryMib: 456}, true
		}
		return contract.ContainerUsage{}, false
	})

	pod, found, err := store.Pod("payments", "payments-api-7f-aaa", testcluster.UIDPodHealthy)
	if err != nil || !found {
		t.Fatalf("Pod 조회: %v %v", found, err)
	}
	sum := store.PodSummary(pod)
	if sum.UID != testcluster.UIDPodHealthy || sum.Usage.CPUMilli != 123 {
		t.Fatalf("Pod 요약: %+v", sum)
	}

	chain := store.PodOwnerChain(pod)
	if len(chain) == 0 {
		t.Fatal("소유 체인이 비었습니다")
	}
	kinds := make([]string, 0, len(chain))
	for _, o := range chain {
		kinds = append(kinds, o.Kind)
	}
	joined := strings.Join(kinds, ">")
	if !strings.Contains(joined, "Deployment") || !strings.Contains(joined, "ReplicaSet") {
		t.Fatalf("소유 체인: %s", joined)
	}

	// UID가 다르면 같은 이름이라도 다른 Pod입니다. (README §5)
	if _, found, _ := store.Pod("payments", "payments-api-7f-aaa", "uid-of-restarted-pod"); found {
		t.Fatal("UID 불일치가 조회되었습니다")
	}
}

func TestSetUsagePublishesConcurrentSnapshotsSafely(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, _ := testcluster.NewStore(t, ctx)
	pod, found, err := store.Pod("payments", "payments-api-7f-aaa", testcluster.UIDPodHealthy)
	if err != nil || !found {
		t.Fatalf("Pod 조회: found=%v err=%v", found, err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			value := i
			store.SetUsage(func(string) (contract.ContainerUsage, bool) {
				return contract.ContainerUsage{CPUMilli: value}, true
			})
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			_ = store.PodSummary(pod)
		}
	}()
	close(start)
	wg.Wait()
}

// TestTopologyPods — 토폴로지 화면의 Pod 목록과 limit 적용입니다.
func TestTopologyPods(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, _ := testcluster.NewStore(t, ctx)

	all, err := store.TopologyPods(clusterstate.NamespaceFilter{All: true}, 100)
	if err != nil || all.Total == 0 {
		t.Fatalf("토폴로지 Pod: %+v %v", all, err)
	}
	if all.Healthy+all.Unhealthy != all.Total {
		t.Fatalf("건강 집계가 어긋납니다: %+v", all)
	}
	if all.Unhealthy == 0 || len(all.UnhealthyList) == 0 {
		t.Fatal("픽스처의 비정상 Pod(CrashLoop)가 집계되지 않았습니다")
	}

	limited, _ := store.TopologyPods(clusterstate.NamespaceFilter{All: true}, 1)
	if len(limited.UnhealthyList) > 1 {
		t.Fatalf("limit이 무시되었습니다: %d", len(limited.UnhealthyList))
	}
	if limited.Total != all.Total {
		t.Fatal("Total은 limit과 무관하게 전체 수여야 합니다")
	}

	scoped, _ := store.TopologyPods(clusterstate.NamespaceFilter{List: []string{"payments"}}, 100)
	for _, p := range scoped.UnhealthyList {
		if p.Ref.Namespace != "payments" {
			t.Fatalf("Scope 밖 Pod: %+v", p.Ref)
		}
	}
}

// TestShortName — 화면 라벨 축약입니다. 자르면 말줄임표가 붙습니다.
func TestShortName(t *testing.T) {
	if got := clusterstate.ShortName("short", 10); got != "short" {
		t.Fatalf("짧은 이름이 잘렸습니다: %s", got)
	}
	got := clusterstate.ShortName("very-long-workload-name", 10)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > 10 {
		t.Fatalf("축약 형식: %q", got)
	}
}

// TestEventsUseWarningFixtures — 이벤트 조회가 involved 참조·횟수·시각을
// 채우는지 확인합니다. (eventTime·toClusterEvent 경로)
func TestEventsUseWarningFixtures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, _ := testcluster.NewStore(t, ctx)

	events, err := store.Events(clusterstate.NamespaceFilter{All: true}, testcluster.Now.Add(-24*3600*1e9), 50)
	if err != nil || len(events) == 0 {
		t.Fatalf("이벤트: %d %v", len(events), err)
	}
	for _, e := range events {
		if e.LastSeen == "" || e.Reason == "" || e.InvolvedName == "" {
			t.Fatalf("이벤트 필드: %+v", e)
		}
	}

	// 대상 UID로 좁힌 조회
	forPod, err := store.EventsForUID(testcluster.UIDPodCrashLoop, testcluster.Now.Add(-24*3600*1e9), 10)
	if err != nil || len(forPod) == 0 {
		t.Fatalf("UID 이벤트: %d %v", len(forPod), err)
	}
	for _, e := range forPod {
		if e.InvolvedName != "payments-api-7f-bbb" {
			t.Fatalf("다른 대상의 이벤트: %+v", e)
		}
	}
}

// TestCatalogPodsBorrowsIdentity — 데이터소스가 빌려 쓰는 Pod 신원입니다.
// limit과 namespace 필터가 동작해야 합니다.
func TestCatalogPodsBorrowsIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, _ := testcluster.NewStore(t, ctx)

	all := store.CatalogPods("", 0)
	if len(all) == 0 {
		t.Fatal("카탈로그가 비었습니다")
	}
	for _, p := range all {
		if p.UID == "" || p.Name == "" || p.Namespace == "" {
			t.Fatalf("신원 필드 누락: %+v", p)
		}
	}
	one := store.CatalogPods("payments", 1)
	if len(one) != 1 || one[0].Namespace != "payments" {
		t.Fatalf("limit·namespace 필터: %+v", one)
	}
}

var _ = corev1.Pod{} // normalize 테스트와의 임포트 일관성 유지

// TestRestConfigAppliesLoadControls — kubeconfig 기반 접속 설정에 protobuf
// 협상·rate limit·UserAgent가 적용되는지 확인합니다. (ADR 0004)
func TestRestConfigAppliesLoadControls(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte(`
apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster: { server: "https://127.0.0.1:6443" }
contexts:
  - name: test
    context: { cluster: test, user: test }
current-context: test
users:
  - name: test
    user: { token: "test-token" }
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := clusterstate.RestConfig(clusterstate.ClientOptions{
		Kubeconfig: kubeconfig, QPS: 20, Burst: 30, UserAgent: "k8s-dashboard-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContentType != clusterstate.ProtobufContentType {
		t.Fatalf("protobuf 협상이 꺼져 있습니다: %q", cfg.ContentType)
	}
	if cfg.UserAgent != "k8s-dashboard-test" || cfg.QPS != 20 || cfg.RateLimiter == nil {
		t.Fatalf("부하 통제 설정: %+v", cfg)
	}

	// protobuf를 끄는 스위치 — aggregated API 뒤에서만 씁니다.
	plain, err := clusterstate.RestConfig(clusterstate.ClientOptions{Kubeconfig: kubeconfig, DisableProtobuf: true})
	if err != nil {
		t.Fatal(err)
	}
	if plain.ContentType == clusterstate.ProtobufContentType {
		t.Fatal("DisableProtobuf가 무시되었습니다")
	}

	// 깨진 kubeconfig는 생성 시점에 거절됩니다.
	if _, err := clusterstate.RestConfig(clusterstate.ClientOptions{Kubeconfig: "/no/such/file"}); err == nil {
		t.Fatal("없는 kubeconfig가 통과했습니다")
	}

	// 클라이언트 생성 — typed와 metadata 두 종류가 만들어집니다.
	clients, err := clusterstate.NewClients(cfg)
	if err != nil || clients.Typed == nil || clients.Metadata == nil {
		t.Fatalf("클라이언트 생성: %+v %v", clients, err)
	}
}
