package stream

import (
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
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
