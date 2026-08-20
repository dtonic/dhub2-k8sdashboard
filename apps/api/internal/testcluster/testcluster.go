// Package testcluster는 테스트에서 쓰는 가짜 클러스터입니다.
//
// 여러 패키지의 테스트가 **같은 사실**을 보고 있어야 비교가 됩니다.
// 그래서 픽스처를 한 곳에 모읍니다. 프로덕션 코드는 이 패키지를 임포트하지 않습니다.
package testcluster

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
)

// Now는 픽스처의 기준 시각입니다. 웹 목 데이터(NOW_MS)와 같은 시각을 씁니다.
var Now = time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC)

// ClusterID는 테스트 클러스터 식별자입니다.
const ClusterID = "prod-seoul"

// 고정 UID. 테스트가 이름이 아니라 **UID로** 검증하도록 상수로 둡니다.
const (
	UIDDeploymentPaymentsAPI = "dep-payments-api"
	UIDReplicaSetCurrent     = "rs-payments-api-7f"
	UIDReplicaSetPrevious    = "rs-payments-api-6a"
	UIDPodHealthy            = "pod-payments-api-7f-aaa"
	UIDPodCrashLoop          = "pod-payments-api-7f-bbb"
	UIDPodPrevGeneration     = "pod-payments-api-6a-ccc"
	UIDCronJobBatchSync      = "cj-batch-sync"
	UIDPodBatchSync          = "pod-batch-sync-qq81z"
	UIDDeploymentMediaAPI    = "dep-media-api"
	UIDPodMedia              = "pod-media-api-zzz"
	// UIDPodImagePull은 통합 E2E 시나리오 corpus(#22)의 ImagePullBackOff 루트입니다.
	// 기본 픽스처가 아니라 ScenarioObjects()로만 추가됩니다 — 기존 테스트의
	// 개수·집계 기대값을 흔들지 않기 위해서입니다.
	UIDPodImagePull = "pod-media-api-1a-eee"
)

// Fakes는 테스트가 API 서버 호출 횟수를 셀 수 있도록 가짜 클라이언트를 함께 돌려줍니다.
type Fakes struct {
	Typed    *fake.Clientset
	Metadata *metadatafake.FakeMetadataClient
}

// NewStore는 픽스처가 담긴 informer 캐시를 만들고 동기화까지 마칩니다.
func NewStore(t *testing.T, ctx context.Context) (*clusterstate.Store, Fakes) {
	t.Helper()
	store, fakes, err := Build(ctx)
	if err != nil {
		t.Fatalf("픽스처 store 생성 실패: %v", err)
	}
	return store, fakes
}

// Build는 NewStore의 에러 반환 버전입니다. testing.T가 없는 곳(#22 E2E fixture 등)이
// 같은 픽스처 사실을 쓰기 위한 통로이며, extra로 시나리오 객체를 얹을 수 있습니다.
// 일반 테스트는 계속 NewStore 래퍼를 씁니다.
func Build(ctx context.Context, extra ...runtime.Object) (*clusterstate.Store, Fakes, error) {
	typed := fake.NewSimpleClientset(append(typedObjects(), extra...)...)

	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		return nil, Fakes{}, fmt.Errorf("스킴 등록 실패: %w", err)
	}
	meta := metadatafake.NewSimpleMetadataClient(scheme, metadataObjects()...)

	store, err := clusterstate.New(
		clusterstate.Clients{Typed: typed, Metadata: meta},
		clusterstate.Options{
			Resync: 0,
			// 가짜 클라이언트는 필드 셀렉터를 해석하지 않으므로 비워 둡니다.
			// 실서버 기본값(type=Warning)은 config에서 검증합니다.
			EventFieldSelector: "",
			ClusterID:          ClusterID,
			ClusterName:        "Seoul Production",
		})
	if err != nil {
		return nil, Fakes{}, fmt.Errorf("store 생성 실패: %w", err)
	}
	store.SetClock(func() time.Time { return Now })

	if err := store.Start(ctx); err != nil {
		return nil, Fakes{}, fmt.Errorf("informer 동기화 실패: %w", err)
	}
	return store, Fakes{Typed: typed, Metadata: meta}, nil
}

// ScenarioObjects는 통합 E2E(#22)의 네 장애 시나리오를 완성하는 추가 객체입니다.
// 기본 픽스처에 이미 있는 CrashLoopBackOff Pod(BackOff 이벤트 포함)와
// Error 로그 루트(media-api-zzz · Unhealthy 이벤트 포함)에 더해,
// ImagePullBackOff Pod와 CPU spike 루트의 상관 이벤트를 채웁니다.
func ScenarioObjects() []runtime.Object {
	return []runtime.Object{
		imagePullPod("media", "media-api-1a-eee", UIDPodImagePull, "ReplicaSet", "media-api-1a", "rs-media-api-1a"),
		warningEvent("media", "media-api-1a-eee", UIDPodImagePull, "Failed",
			`Failed to pull image "registry.example.com/media-api:1.43.0": manifest unknown`),
		// CPU 포화 시 liveness가 시간 초과로 실패하는 실제 패턴을 상관 이벤트로 둡니다.
		warningEvent("payments", "batch-sync-qq81z", UIDPodBatchSync, "Unhealthy",
			"Liveness probe failed: context deadline exceeded"),
	}
}

func typedObjects() []runtime.Object {
	return []runtime.Object{
		namespace("media"),
		namespace("payments"),
		node("node-1", true, false, false),
		node("node-2", false, true, false),

		deployment("payments", "payments-api", UIDDeploymentPaymentsAPI, 3, 2),
		deployment("media", "media-api", UIDDeploymentMediaAPI, 1, 1),
		cronJob("payments", "batch-sync", UIDCronJobBatchSync),

		healthyPod("payments", "payments-api-7f-aaa", UIDPodHealthy, "ReplicaSet", "payments-api-7f", UIDReplicaSetCurrent),
		crashLoopPod("payments", "payments-api-7f-bbb", UIDPodCrashLoop, "ReplicaSet", "payments-api-7f", UIDReplicaSetCurrent),
		healthyPod("payments", "payments-api-6a-ccc", UIDPodPrevGeneration, "ReplicaSet", "payments-api-6a", UIDReplicaSetPrevious),
		healthyPod("payments", "batch-sync-qq81z", UIDPodBatchSync, "CronJob", "batch-sync", UIDCronJobBatchSync),
		healthyPod("media", "media-api-zzz", UIDPodMedia, "ReplicaSet", "media-api-1a", "rs-media-api-1a"),

		warningEvent("payments", "payments-api-7f-bbb", UIDPodCrashLoop, "BackOff", "Back-off restarting failed container"),
		warningEvent("media", "media-api-zzz", UIDPodMedia, "Unhealthy", "Readiness probe failed"),
	}
}

func namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("namespace-" + name)}}
}

// metadataObjects는 metadata-only informer가 받는 것과 같은 모양입니다.
// spec/status가 없다는 점이 중요합니다 — 소유 관계와 애노테이션만으로 세대를 판단해야 합니다.
func metadataObjects() []runtime.Object {
	return []runtime.Object{
		replicaSetMeta("payments", "payments-api-7f", UIDReplicaSetCurrent, "2", UIDDeploymentPaymentsAPI),
		replicaSetMeta("payments", "payments-api-6a", UIDReplicaSetPrevious, "1", UIDDeploymentPaymentsAPI),
		replicaSetMeta("media", "media-api-1a", "rs-media-api-1a", "1", UIDDeploymentMediaAPI),
	}
}

func node(name string, ready, memPressure, unschedulable bool) *corev1.Node {
	cond := func(t corev1.NodeConditionType, v bool) corev1.NodeCondition {
		s := corev1.ConditionFalse
		if v {
			s = corev1.ConditionTrue
		}
		return corev1.NodeCondition{Type: t, Status: s}
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("node-" + name)},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			cond(corev1.NodeReady, ready),
			cond(corev1.NodeMemoryPressure, memPressure),
		}},
	}
}

func deployment(ns, name, uid string, desired, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: types.UID(uid),
			CreationTimestamp: metav1.NewTime(Now.Add(-72 * time.Hour)),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &desired,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/" + name + ":1.42.0"}},
			}},
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: ready, AvailableReplicas: ready, UpdatedReplicas: ready,
		},
	}
}

func cronJob(ns, name, uid string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: types.UID(uid),
			CreationTimestamp: metav1.NewTime(Now.Add(-240 * time.Hour)),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/batch-sync:0.9.1"}},
				}},
			}},
		},
		Status: batchv1.CronJobStatus{Active: []corev1.ObjectReference{{Name: "batch-sync-28100"}}},
	}
}

func podBase(ns, name, uid, ownerKind, ownerName, ownerUID string) *corev1.Pod {
	yes := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: types.UID(uid),
			CreationTimestamp: metav1.NewTime(Now.Add(-6 * time.Hour)),
			OwnerReferences: []metav1.OwnerReference{{
				Kind: ownerKind, Name: ownerName, UID: types.UID(ownerUID), Controller: &yes,
			}},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "registry.example.com/app:1.42.0",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
				ReadinessProbe: &corev1.Probe{},
				LivenessProbe:  &corev1.Probe{},
			}},
		},
	}
}

// NewPod는 테스트가 동기화 이후에 추가할 정상 Pod를 만듭니다. (#12 SSE 테스트 등)
// 픽스처와 같은 규칙으로 만들어 신원·소유 체인이 화면 조회와 일치합니다.
func NewPod(ns, name, uid, ownerKind, ownerName, ownerUID string) *corev1.Pod {
	return healthyPod(ns, name, uid, ownerKind, ownerName, ownerUID)
}

func healthyPod(ns, name, uid, ownerKind, ownerName, ownerUID string) *corev1.Pod {
	p := podBase(ns, name, uid, ownerKind, ownerName, ownerUID)
	started := true
	p.Status = corev1.PodStatus{
		Phase:     corev1.PodRunning,
		StartTime: &metav1.Time{Time: Now.Add(-6 * time.Hour)},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", Image: p.Spec.Containers[0].Image, Ready: true, Started: &started,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
	return p
}

func crashLoopPod(ns, name, uid, ownerKind, ownerName, ownerUID string) *corev1.Pod {
	p := podBase(ns, name, uid, ownerKind, ownerName, ownerUID)
	started := false
	p.Status = corev1.PodStatus{
		Phase:     corev1.PodRunning,
		StartTime: &metav1.Time{Time: Now.Add(-6 * time.Hour)},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", Image: p.Spec.Containers[0].Image, Ready: false, Started: &started,
			RestartCount: 7,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container",
			}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Error", ExitCode: 137, FinishedAt: metav1.NewTime(Now.Add(-3 * time.Minute)),
			}},
		}},
	}
	return p
}

// imagePullPod는 이미지를 받아오지 못해 Pending에 머무는 Pod입니다. (#22 시나리오)
func imagePullPod(ns, name, uid, ownerKind, ownerName, ownerUID string) *corev1.Pod {
	p := podBase(ns, name, uid, ownerKind, ownerName, ownerUID)
	p.Spec.Containers[0].Image = "registry.example.com/media-api:1.43.0"
	started := false
	p.Status = corev1.PodStatus{
		Phase:     corev1.PodPending,
		StartTime: &metav1.Time{Time: Now.Add(-25 * time.Minute)},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", Image: p.Spec.Containers[0].Image, Ready: false, Started: &started,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason:  "ImagePullBackOff",
				Message: `Back-off pulling image "registry.example.com/media-api:1.43.0"`,
			}},
		}},
	}
	return p
}

func warningEvent(ns, involvedName, involvedUID, reason, message string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: involvedName + "." + reason, Namespace: ns,
			UID: types.UID("ev-" + involvedUID + "-" + reason),
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Name: involvedName, Namespace: ns, UID: types.UID(involvedUID),
		},
		Type: corev1.EventTypeWarning, Reason: reason, Message: message, Count: 12,
		LastTimestamp: metav1.NewTime(Now.Add(-4 * time.Minute)),
	}
}

func replicaSetMeta(ns, name, uid, revision, ownerUID string) *metav1.PartialObjectMetadata {
	yes := true
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: types.UID(uid),
			Annotations:       map[string]string{"deployment.kubernetes.io/revision": revision},
			CreationTimestamp: metav1.NewTime(Now.Add(-48 * time.Hour)),
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Deployment", Name: name[:len(name)-3], UID: types.UID(ownerUID), Controller: &yes,
			}},
		},
	}
}
