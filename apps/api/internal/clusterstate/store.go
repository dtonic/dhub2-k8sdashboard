// Package clusterstate는 Kubernetes 상태를 **watch 기반 informer 캐시**로 들고 있습니다.
//
// ADR 0004의 핵심 규칙이 여기에 구현됩니다.
//   - 요청 처리 경로에서 API 서버를 호출하지 않습니다. 항상 lister에서 읽습니다.
//   - 폴링하지 않습니다. 최초 LIST 1회 + 이후 변경분 스트리밍이 전부입니다.
//   - 관계만 필요한 ReplicaSet은 metadata-only informer를 씁니다.
//
// 사용자가 100명이어도 API 서버가 받는 부하는 같습니다.
package clusterstate

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	appslisters "k8s.io/client-go/listers/apps/v1"
	batchlisters "k8s.io/client-go/listers/batch/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"
)

// 인덱스 이름. lister 위에 직접 얹어 요청 시 전체 순회를 피합니다.
const (
	IndexPodByOwner        = "podByOwner"
	IndexReplicaSetByOwner = "replicaSetByOwner"
	IndexEventByInvolved   = "eventByInvolved"
)

var replicaSetGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}

// Options는 watch 범위와 resync 주기입니다. 둘 다 클러스터 부하에 직접 영향을 줍니다.
type Options struct {
	// Resync는 informer가 캐시 전체를 다시 흘려보내는 주기입니다.
	// **API 서버를 다시 치지는 않지만 CPU를 씁니다.** 기본 10분. (ADR 0004 구현 규칙 3)
	Resync time.Duration
	// EventFieldSelector는 Event informer에 적용할 필드 셀렉터입니다.
	// 기본값 `type=Warning` — Event는 대부분의 클러스터에서 가장 수가 많은 리소스라
	// 전부 watch하면 대시보드가 클러스터 부하의 주범이 됩니다.
	EventFieldSelector string
	// ClusterID/ClusterName은 EntityRef를 만들 때 씁니다.
	ClusterID   string
	ClusterName string
}

func (o *Options) setDefaults() {
	if o.Resync == 0 {
		o.Resync = 10 * time.Minute
	}
	if o.EventFieldSelector == "" {
		o.EventFieldSelector = "type=Warning"
	}
	if o.ClusterID == "" {
		o.ClusterID = "default"
	}
	if o.ClusterName == "" {
		o.ClusterName = o.ClusterID
	}
}

// Store는 informer 캐시와 그 위의 조회 함수 묶음입니다.
type Store struct {
	opts Options

	factory      informers.SharedInformerFactory
	eventFactory informers.SharedInformerFactory
	metaFactory  metadatainformer.SharedInformerFactory

	pods         corelisters.PodLister
	nodes        corelisters.NodeLister
	events       corelisters.EventLister
	deployments  appslisters.DeploymentLister
	statefulSets appslisters.StatefulSetLister
	daemonSets   appslisters.DaemonSetLister
	cronJobs     batchlisters.CronJobLister

	podIndexer   cache.Indexer
	rsIndexer    cache.Indexer
	eventIndexer cache.Indexer

	synced []cache.InformerSynced

	// usage는 메트릭 데이터소스에서 온 현재 사용량 조회원입니다. 없으면 request/limit만 채웁니다.
	usage UsageFunc

	// now는 테스트에서 시간을 고정하기 위한 훅입니다.
	now func() time.Time
}

// New는 informer를 구성합니다. 아직 watch를 시작하지는 않습니다.
func New(c Clients, opts Options) (*Store, error) {
	opts.setDefaults()

	s := &Store{opts: opts, now: time.Now}

	s.factory = informers.NewSharedInformerFactory(c.Typed, opts.Resync)
	s.eventFactory = informers.NewSharedInformerFactoryWithOptions(c.Typed, opts.Resync,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = opts.EventFieldSelector
		}))
	s.metaFactory = metadatainformer.NewSharedInformerFactory(c.Metadata, opts.Resync)

	podInformer := s.factory.Core().V1().Pods()
	nodeInformer := s.factory.Core().V1().Nodes()
	depInformer := s.factory.Apps().V1().Deployments()
	stsInformer := s.factory.Apps().V1().StatefulSets()
	dsInformer := s.factory.Apps().V1().DaemonSets()
	cjInformer := s.factory.Batch().V1().CronJobs()
	evInformer := s.eventFactory.Core().V1().Events()
	rsInformer := s.metaFactory.ForResource(replicaSetGVR)

	s.pods = podInformer.Lister()
	s.nodes = nodeInformer.Lister()
	s.events = evInformer.Lister()
	s.deployments = depInformer.Lister()
	s.statefulSets = stsInformer.Lister()
	s.daemonSets = dsInformer.Lister()
	s.cronJobs = cjInformer.Lister()

	s.podIndexer = podInformer.Informer().GetIndexer()
	s.rsIndexer = rsInformer.Informer().GetIndexer()
	s.eventIndexer = evInformer.Informer().GetIndexer()

	if err := s.podIndexer.AddIndexers(cache.Indexers{IndexPodByOwner: ownerUIDIndex}); err != nil {
		return nil, fmt.Errorf("pod 인덱스 등록 실패: %w", err)
	}
	if err := s.rsIndexer.AddIndexers(cache.Indexers{IndexReplicaSetByOwner: ownerUIDIndex}); err != nil {
		return nil, fmt.Errorf("replicaset 인덱스 등록 실패: %w", err)
	}
	if err := s.eventIndexer.AddIndexers(cache.Indexers{IndexEventByInvolved: eventInvolvedIndex}); err != nil {
		return nil, fmt.Errorf("event 인덱스 등록 실패: %w", err)
	}

	s.synced = []cache.InformerSynced{
		podInformer.Informer().HasSynced,
		nodeInformer.Informer().HasSynced,
		depInformer.Informer().HasSynced,
		stsInformer.Informer().HasSynced,
		dsInformer.Informer().HasSynced,
		cjInformer.Informer().HasSynced,
		evInformer.Informer().HasSynced,
		rsInformer.Informer().HasSynced,
	}
	return s, nil
}

// Start는 watch를 시작하고 최초 동기화가 끝날 때까지 기다립니다.
//
// 동기화 전에 요청을 받으면 "Pod 0개"처럼 **틀린 값이 정상처럼** 보입니다.
// 그래서 준비되기 전에는 서버가 degraded를 내려보내야 합니다. (HasSynced 참고)
func (s *Store) Start(ctx context.Context) error {
	s.factory.Start(ctx.Done())
	s.eventFactory.Start(ctx.Done())
	s.metaFactory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), s.synced...) {
		return fmt.Errorf("informer 캐시 동기화에 실패했습니다")
	}
	return nil
}

// HasSynced는 모든 informer가 최초 동기화를 마쳤는지입니다.
func (s *Store) HasSynced() bool {
	for _, f := range s.synced {
		if !f() {
			return false
		}
	}
	return true
}

// ClusterID/ClusterName은 응답에 실을 클러스터 식별자입니다.
func (s *Store) ClusterID() string   { return s.opts.ClusterID }
func (s *Store) ClusterName() string { return s.opts.ClusterName }

// SetClock은 테스트에서 시간을 고정합니다.
func (s *Store) SetClock(f func() time.Time) { s.now = f }

// ownerUIDIndex는 OwnerReference의 UID로 인덱싱합니다.
// Pod → ReplicaSet, ReplicaSet → Deployment 조회가 O(1)이 됩니다.
func ownerUIDIndex(obj any) ([]string, error) {
	m, ok := obj.(metav1.Object)
	if !ok {
		return nil, nil
	}
	refs := m.GetOwnerReferences()
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, string(r.UID))
	}
	return out, nil
}

// eventInvolvedIndex는 Event를 대상 객체 UID로 인덱싱합니다.
func eventInvolvedIndex(obj any) ([]string, error) {
	e, ok := obj.(*corev1.Event)
	if !ok {
		return nil, nil
	}
	if e.InvolvedObject.UID == "" {
		return nil, nil
	}
	return []string{string(e.InvolvedObject.UID)}, nil
}

// 아래는 lister 접근자입니다. 조회 로직(query.go)에서만 씁니다.

func (s *Store) listPods(ns string) ([]*corev1.Pod, error) {
	if ns == "" {
		return s.pods.List(labelsEverything)
	}
	return s.pods.Pods(ns).List(labelsEverything)
}

func (s *Store) listDeployments(ns string) ([]*appsv1.Deployment, error) {
	if ns == "" {
		return s.deployments.List(labelsEverything)
	}
	return s.deployments.Deployments(ns).List(labelsEverything)
}

func (s *Store) listStatefulSets(ns string) ([]*appsv1.StatefulSet, error) {
	if ns == "" {
		return s.statefulSets.List(labelsEverything)
	}
	return s.statefulSets.StatefulSets(ns).List(labelsEverything)
}

func (s *Store) listDaemonSets(ns string) ([]*appsv1.DaemonSet, error) {
	if ns == "" {
		return s.daemonSets.List(labelsEverything)
	}
	return s.daemonSets.DaemonSets(ns).List(labelsEverything)
}

func (s *Store) listCronJobs(ns string) ([]*batchv1.CronJob, error) {
	if ns == "" {
		return s.cronJobs.List(labelsEverything)
	}
	return s.cronJobs.CronJobs(ns).List(labelsEverything)
}
