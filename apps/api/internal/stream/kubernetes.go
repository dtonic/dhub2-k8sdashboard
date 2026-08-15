package stream

import (
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// EnvelopeFromChange는 informer 변경(clusterstate.Change)을 SSE 봉투로 바꿉니다.
//
// 방향이 중요합니다 — stream이 clusterstate를 임포트하지, 그 반대가 아닙니다.
// informer 계층은 자신이 SSE로 나가는지 모릅니다. (ADR 0004)
func EnvelopeFromChange(clusterID string, c clusterstate.Change) contract.EventEnvelope {
	entity := c.Entity
	return contract.EventEnvelope{
		Kind:            contract.StreamEventKind(c.Kind),
		Action:          contract.StreamEventAction(c.Action),
		ClusterID:       clusterID,
		Namespace:       c.Namespace,
		Entity:          &entity,
		ResourceVersion: c.ResourceVersion,
	}
}

// EnvelopeFromWatchFrame converts the registry's minimized catalog feed to the
// same invalidation-only SSE contract used by direct informer callbacks. Bulk
// snapshot data is intentionally never expanded into per-resource events.
func EnvelopeFromWatchFrame(frame *v1.WatchFrame) (contract.EventEnvelope, bool) {
	if frame == nil {
		return contract.EventEnvelope{}, false
	}
	observedAt := time.UnixMilli(frame.ObservedUnixMs).UTC().Format(time.RFC3339Nano)
	reset := func() (contract.EventEnvelope, bool) {
		return contract.EventEnvelope{Kind: contract.StreamKindReset, Action: contract.StreamActionReset, ClusterID: frame.ClusterId, ObservedAt: observedAt}, true
	}
	switch frame.Type {
	case v1.WatchFrameType_WATCH_EXPIRED, v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT:
		return reset()
	case v1.WatchFrameType_WATCH_DELTA:
	default:
		return contract.EventEnvelope{}, false
	}
	change := frame.Change
	if change == nil || change.Resource == nil {
		return contract.EventEnvelope{}, false
	}
	action := contract.StreamActionUpdated
	switch change.Action {
	case v1.CatalogAction_CATALOG_CREATED:
		action = contract.StreamActionAdded
	case v1.CatalogAction_CATALOG_UPDATED:
	case v1.CatalogAction_CATALOG_DELETED:
		action = contract.StreamActionDeleted
	default:
		return contract.EventEnvelope{}, false
	}
	r := change.Resource
	entity := &contract.EntityRef{ClusterID: frame.ClusterId, Namespace: r.Namespace}
	kind := contract.StreamKindWorkload
	switch r.Kind {
	case v1.KindPod:
		kind = contract.StreamKindPod
		entity.PodUID, entity.PodName = r.Uid, r.Name
	case v1.KindDeployment, v1.KindStatefulSet, v1.KindDaemonSet, v1.KindCronJob:
		entity.WorkloadUID, entity.WorkloadKind, entity.WorkloadName = r.Uid, r.Kind, r.Name
	case v1.KindEvent:
		kind = contract.StreamKindKubeEvent
		entity = nil // the narrow catalog feed deliberately carries no Event payload
	default:
		return contract.EventEnvelope{}, false
	}
	return contract.EventEnvelope{Kind: kind, Action: action, ClusterID: frame.ClusterId, Namespace: r.Namespace, Entity: entity, ObservedAt: observedAt}, true
}

// PublishWatchFrame is the callback passed to each API replica's independent
// registry Watch. Hub.Publish keeps slow-client backpressure local to that
// replica and cluster.
func (h *Hub) PublishWatchFrame(frame *v1.WatchFrame) {
	if env, ok := EnvelopeFromWatchFrame(frame); ok {
		h.Publish(env)
	}
}
