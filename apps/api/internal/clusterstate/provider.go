package clusterstate

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// Provider is a request-local, screen-scoped cluster projection. A central
// implementation materializes it with one Query RPC before a handler starts;
// its methods are local reads and cannot create per-widget RPC fanout.
type Provider interface {
	HasSynced() bool
	NodeHealth() (contract.NodeHealth, error)
	PodHealth(NamespaceFilter) (contract.PodHealth, error)
	WorkloadHealth(NamespaceFilter) (contract.WorkloadHealth, error)
	Unhealthy(NamespaceFilter, int) ([]contract.UnhealthyEntity, error)
	Events(NamespaceFilter, time.Time, int) ([]contract.ClusterEvent, error)
	EventsForUID(string, time.Time, int) ([]contract.ClusterEvent, error)
	NodeSummaries() ([]contract.NodeSummary, error)
	NamespaceSummaries(NamespaceFilter) ([]contract.NamespaceSummary, error)
	NamespaceSummary(string) (contract.NamespaceSummary, bool, error)
	Workloads(NamespaceFilter) ([]contract.WorkloadSummary, error)
	Workload(string, string, string) (contract.WorkloadSummary, bool, error)
	PodsForWorkload(string, string, string, string) ([]contract.PodSummary, error)
	Pod(string, string, string) (*corev1.Pod, bool, error)
	PodSummary(*corev1.Pod) contract.PodSummary
	PodOwnerChain(*corev1.Pod) []contract.OwnerRef
	WorkloadOwnerChain(string, string, string, string) []contract.OwnerRef
	TopologyPods(NamespaceFilter, int) (contract.TopologyPods, error)
}

var _ Provider = (*Store)(nil)

// NamespaceCatalog is the request-free namespace name projection used by the
// scope selector. Implementations must return a sorted, de-duplicated snapshot
// from an informer/watch cache and must never call Kubernetes on the request path.
type NamespaceCatalog interface {
	NamespaceNames(clusterID string) []string
}

var _ NamespaceCatalog = (*Store)(nil)

// ProviderRegistry resolves exactly one projection after HTTP authorization.
type ProviderRegistry interface {
	ForScreen(context.Context, ScreenRequest) (Provider, error)
	Ready() bool
}

type DirectRegistry struct{ Store *Store }
type ScreenRequest struct {
	ClusterID, Screen, RequestedNamespace, EntityUID, Kind, Name string
	Namespaces                                                   NamespaceFilter
	From                                                         time.Time
	EventLimit, UnhealthyLimit                                   int
}

func (r DirectRegistry) ForScreen(_ context.Context, req ScreenRequest) (Provider, error) {
	if r.Store == nil || req.ClusterID != r.Store.ClusterID() {
		return nil, fmt.Errorf("cluster unavailable")
	}
	return r.Store, nil
}
func (r DirectRegistry) Ready() bool { return r.Store != nil && r.Store.HasSynced() }
