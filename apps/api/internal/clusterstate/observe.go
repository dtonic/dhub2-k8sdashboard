package clusterstate

// 상태 변경 관찰 훅 — 이슈 #12.
//
// informer가 받은 변경을 **밖으로 알리기만** 합니다. clusterstate는 stream을
// 임포트하지 않습니다 — 전달·재생·연결 관리는 관찰자(stream.Hub)의 일입니다.
// 핸들러는 New에서 한 번 붙고, 관찰자는 atomic으로 언제든 등록할 수 있습니다.
//
// 억제 규칙:
//   - 최초 LIST가 만드는 add는 변경이 아니라 현재 상태입니다. 내보내지 않습니다.
//     내보내면 기동 직후 구독자마다 클러스터 전체 크기의 이벤트 홍수가 생깁니다.
//   - resync가 만드는 no-op update(같은 resourceVersion)도 내보내지 않습니다.
//   - watch가 끊겼다 이어지며 생기는 DeletedFinalStateUnknown은 안의 객체로 풉니다.

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// ChangeAction은 변경의 방향입니다. contract.StreamEventAction과 같은 문자열을 씁니다.
type ChangeAction string

const (
	ChangeAdded   ChangeAction = "added"
	ChangeUpdated ChangeAction = "updated"
	ChangeDeleted ChangeAction = "deleted"
)

// ChangeKind는 변경 대상의 종류입니다. contract.StreamEventKind와 같은 문자열을 씁니다.
type ChangeKind string

const (
	ChangePod       ChangeKind = "pod"
	ChangeWorkload  ChangeKind = "workload"
	ChangeKubeEvent ChangeKind = "kubeevent"
)

// Change는 informer가 관측한 변경 하나입니다. 원본 객체는 싣지 않습니다 —
// UID 우선 참조와 resourceVersion만 내보냅니다. (README §5·§10)
type Change struct {
	Kind            ChangeKind
	Action          ChangeAction
	Namespace       string
	Entity          contract.EntityRef
	ResourceVersion string
}

// ChangeObserver는 변경을 받는 좁은 훅입니다.
//
// informer 콜백 고루틴에서 불립니다 — **절대 막히면 안 됩니다**.
// stream.Hub.Publish처럼 논블로킹 구현만 등록하세요.
type ChangeObserver interface {
	ObserveChange(Change)
}

// ChangeObserverFunc는 함수 하나를 관찰자로 씁니다.
type ChangeObserverFunc func(Change)

func (f ChangeObserverFunc) ObserveChange(c Change) { f(c) }

// OnChange는 관찰자를 등록합니다. informer 핸들러는 New에서 이미 붙어 있으므로
// Start 전후 어느 시점에 등록해도 안전합니다. 등록 전의 변경은 버려집니다 —
// 초기 상태는 어차피 HTTP 조회가 담당합니다.
func (s *Store) OnChange(h func(Change)) error {
	if h == nil {
		return errors.New("변경 핸들러가 nil입니다")
	}
	var o ChangeObserver = ChangeObserverFunc(h)
	s.observer.Store(&o)
	return nil
}

func (s *Store) dispatchChange(c Change) {
	if o := s.observer.Load(); o != nil {
		(*o).ObserveChange(c)
	}
}

// changeSource는 informer 하나와 그 객체를 Change로 바꾸는 함수입니다.
type changeSource struct {
	informer cache.SharedIndexInformer
	convert  func(obj any) (Change, bool)
}

// registerChangeSources는 New가 informer마다 변경 핸들러를 답니다.
func (s *Store) registerChangeSources() error {
	for _, src := range s.changeSources {
		if err := registerChange(src.informer, src.convert, s.dispatchChange); err != nil {
			return fmt.Errorf("변경 핸들러 등록 실패: %w", err)
		}
	}
	return nil
}

func registerChange(inf cache.SharedIndexInformer, convert func(obj any) (Change, bool), h func(Change)) error {
	_, err := inf.AddEventHandler(changeHandlers(convert, h))
	return err
}

// changeHandlers는 억제 규칙이 담긴 informer 핸들러를 만듭니다.
// 별도 함수인 이유는 단위 테스트가 informer 없이 규칙을 직접 검증하기 위해서입니다.
func changeHandlers(convert func(obj any) (Change, bool), h func(Change)) cache.ResourceEventHandlerDetailedFuncs {
	return cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj any, isInInitialList bool) {
			if isInInitialList {
				return // 최초 LIST 억제 — 변경이 아니라 현재 상태입니다.
			}
			if c, ok := convert(obj); ok {
				c.Action = ChangeAdded
				h(c)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldMeta, okOld := oldObj.(metav1.Object)
			newMeta, okNew := newObj.(metav1.Object)
			if okOld && okNew && oldMeta.GetResourceVersion() == newMeta.GetResourceVersion() {
				return // resync no-op 억제 — 아무것도 바뀌지 않았습니다.
			}
			if c, ok := convert(newObj); ok {
				c.Action = ChangeUpdated
				h(c)
			}
		},
		DeleteFunc: func(obj any) {
			if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			if c, ok := convert(obj); ok {
				c.Action = ChangeDeleted
				h(c)
			}
		},
	}
}

// podChange는 Pod를 UID 우선 참조로 접습니다. 워크로드 신원은 화면과 같은
// 출처(workloadOfPod)에서 빌립니다 — 각자 지어내면 deep link가 404가 됩니다.
func (s *Store) podChange(obj any) (Change, bool) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return Change{}, false
	}
	kind, name, uid := s.workloadOfPod(p)
	return Change{
		Kind:            ChangePod,
		Namespace:       p.Namespace,
		Entity:          s.podRef(p, kind, name, uid),
		ResourceVersion: p.ResourceVersion,
	}, true
}

// workloadChange는 Deployment·StatefulSet·DaemonSet·CronJob 공용입니다.
// ReplicaSet은 화면에 단독으로 노출되지 않는 구현 세부사항이라 내보내지 않습니다 —
// Pod 변경이 이미 소유 Deployment 신원을 싣고 나갑니다.
func workloadChange(clusterID, kind string) func(obj any) (Change, bool) {
	return func(obj any) (Change, bool) {
		m, ok := obj.(metav1.Object)
		if !ok {
			return Change{}, false
		}
		return Change{
			Kind:      ChangeWorkload,
			Namespace: m.GetNamespace(),
			Entity: contract.EntityRef{
				ClusterID:    clusterID,
				Namespace:    m.GetNamespace(),
				WorkloadKind: kind,
				WorkloadName: m.GetName(),
				WorkloadUID:  string(m.GetUID()),
			},
			ResourceVersion: m.GetResourceVersion(),
		}, true
	}
}

// eventChange는 Kubernetes Event를 관련 객체의 참조로 접습니다.
// Event 메시지 원문은 싣지 않습니다 — 본문은 화면 단위 HTTP 조회가 담당합니다.
func (s *Store) eventChange(obj any) (Change, bool) {
	e, ok := obj.(*corev1.Event)
	if !ok {
		return Change{}, false
	}
	ref := contract.EntityRef{ClusterID: s.opts.ClusterID, Namespace: e.Namespace}
	switch e.InvolvedObject.Kind {
	case "Pod":
		ref.PodName = e.InvolvedObject.Name
		ref.PodUID = string(e.InvolvedObject.UID)
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "CronJob":
		ref.WorkloadKind = e.InvolvedObject.Kind
		ref.WorkloadName = e.InvolvedObject.Name
		ref.WorkloadUID = string(e.InvolvedObject.UID)
	}
	return Change{
		Kind:            ChangeKubeEvent,
		Namespace:       e.Namespace,
		Entity:          ref,
		ResourceVersion: e.ResourceVersion,
	}, true
}
