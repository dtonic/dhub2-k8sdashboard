package clusterstate

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterid"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

// usageEntryOverhead conservatively covers the map bucket, string header and
// value storage in addition to UID bytes. Performance tests compare it with
// actual TotalAlloc; it is admission accounting, not an RSS measurement.
const usageEntryOverhead = 128

type UsageLimits struct {
	PerClusterEntries int
	TotalEntries      int
	PerClusterBytes   int
	TotalBytes        int
	PeakEntries       int
	PeakBytes         int
}

type usageSnapshot struct {
	values map[string]contract.ContainerUsage
	bytes  int
}

type usageCluster struct {
	updateMu sync.Mutex
	value    atomic.Pointer[usageSnapshot]
}

// UsageStore owns immutable, cluster-partitioned metric snapshots. Reads never
// take the aggregate admission lock and a failed refresh preserves last-good.
type UsageStore struct {
	mu              sync.Mutex
	clusters        map[string]*usageCluster
	limits          UsageLimits
	totalEntries    int
	totalBytes      int
	buildEntries    int
	buildBytes      int
	reservedEntries int
	reservedBytes   int
}

func NewUsageStore(clusterIDs []string, maxEntries int) (*UsageStore, error) {
	return NewUsageStoreWithLimits(clusterIDs, UsageLimits{
		PerClusterEntries: maxEntries,
		TotalEntries:      maxEntries,
		PerClusterBytes:   maxEntries * (253 + usageEntryOverhead),
		TotalBytes:        maxEntries * (253 + usageEntryOverhead),
		PeakEntries:       maxEntries * 2,
		PeakBytes:         maxEntries * (253 + usageEntryOverhead) * 2,
	})
}

func NewUsageStoreWithLimits(clusterIDs []string, limits UsageLimits) (*UsageStore, error) {
	if len(clusterIDs) == 0 || len(clusterIDs) > 64 || limits.PerClusterEntries < 1 || limits.PerClusterEntries > 100_000 || limits.TotalEntries < limits.PerClusterEntries || limits.TotalEntries > 1_000_000 || limits.PerClusterBytes < usageEntryOverhead || limits.TotalBytes < limits.PerClusterBytes || limits.TotalBytes > 64<<20 || limits.PeakEntries < limits.TotalEntries || limits.PeakEntries > 2_000_000 || limits.PeakBytes < limits.TotalBytes || limits.PeakBytes > 128<<20 {
		return nil, fmt.Errorf("invalid usage limits")
	}
	u := &UsageStore{clusters: make(map[string]*usageCluster, len(clusterIDs)), limits: limits}
	for _, id := range clusterIDs {
		if !clusterid.Valid(id) {
			return nil, fmt.Errorf("invalid cluster")
		}
		if _, ok := u.clusters[id]; ok {
			return nil, fmt.Errorf("duplicate cluster")
		}
		u.clusters[id] = &usageCluster{}
	}
	return u, nil
}

func (u *UsageStore) Update(clusterID string, values map[string]contract.ContainerUsage) error {
	c := u.clusters[clusterID]
	if c == nil || len(values) > u.limits.PerClusterEntries {
		return fmt.Errorf("invalid usage snapshot")
	}
	bytes := 0
	for uid, value := range values {
		if uid == "" || len(uid) > 253 || value.CPUMilli < 0 || value.CPUMilli > 1_000_000_000 || value.MemoryMib < 0 || value.MemoryMib > 1_000_000_000 {
			return fmt.Errorf("invalid usage value")
		}
		bytes += len(uid) + usageEntryOverhead
		if bytes > u.limits.PerClusterBytes {
			return fmt.Errorf("usage byte capacity")
		}
	}

	c.updateMu.Lock()
	defer c.updateMu.Unlock()
	old := c.value.Load()
	if sameUsageSnapshot(old, values, bytes) {
		return nil
	}
	oldEntries, oldBytes := 0, 0
	if old != nil {
		oldEntries, oldBytes = len(old.values), old.bytes
	}
	u.mu.Lock()
	entryDelta := len(values) - oldEntries
	byteDelta := bytes - oldBytes
	reserveEntries := max(0, entryDelta)
	reserveBytes := max(0, byteDelta)
	if u.totalEntries+u.reservedEntries+reserveEntries > u.limits.TotalEntries || u.totalBytes+u.reservedBytes+reserveBytes > u.limits.TotalBytes || u.totalEntries+u.buildEntries+len(values) > u.limits.PeakEntries || u.totalBytes+u.buildBytes+bytes > u.limits.PeakBytes {
		u.mu.Unlock()
		return fmt.Errorf("global usage capacity")
	}
	u.reservedEntries += reserveEntries
	u.reservedBytes += reserveBytes
	u.buildEntries += len(values)
	u.buildBytes += bytes
	u.mu.Unlock()
	next := &usageSnapshot{values: make(map[string]contract.ContainerUsage, len(values)), bytes: bytes}
	for uid, value := range values {
		next.values[uid] = value
	}
	u.mu.Lock()
	u.buildEntries -= len(values)
	u.buildBytes -= bytes
	u.reservedEntries -= reserveEntries
	u.reservedBytes -= reserveBytes
	u.totalEntries += entryDelta
	u.totalBytes += byteDelta
	c.value.Store(next)
	u.mu.Unlock()
	return nil
}

func sameUsageSnapshot(old *usageSnapshot, values map[string]contract.ContainerUsage, bytes int) bool {
	if old == nil || old.bytes != bytes || len(old.values) != len(values) {
		return false
	}
	for uid, value := range values {
		if old.values[uid] != value {
			return false
		}
	}
	return true
}

func (u *UsageStore) Lookup(clusterID, uid string) (contract.ContainerUsage, bool) {
	c := u.clusters[clusterID]
	if c == nil {
		return contract.ContainerUsage{}, false
	}
	snapshot := c.value.Load()
	if snapshot == nil {
		return contract.ContainerUsage{}, false
	}
	value, ok := snapshot.values[uid]
	return value, ok
}

func (u *UsageStore) Retained() (entries, bytes int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.totalEntries, u.totalBytes
}

// EnrichUsage joins an API-local metric snapshot into one bounded screen
// projection without another registry RPC.
func EnrichUsage(data *ScreenProjection, catalog *RemoteCatalog, store *UsageStore) {
	if data == nil || catalog == nil || store == nil || !catalog.Available(data.Request.ClusterID) {
		return
	}
	add := func(target *contract.ResourceUsage, uid string) {
		if value, ok := store.Lookup(data.Request.ClusterID, uid); ok {
			target.CPUMilli += value.CPUMilli
			target.MemoryMib += value.MemoryMib
			target.Normalize()
		}
	}
	switch data.Request.Screen {
	case "pod":
		if data.PodSummaryValue != nil {
			add(&data.PodSummaryValue.Usage, data.ResolvedUID)
		}
	case "workload":
		for i := range data.PodsList {
			add(&data.PodsList[i].Usage, data.PodsList[i].UID)
		}
		if data.WorkloadValue != nil {
			catalog.visitPods(data.Request.ClusterID, data.Request.RequestedNamespace, func(pod datasource.CatalogPod) {
				if pod.WorkloadUID == data.ResolvedUID {
					add(&data.WorkloadValue.Usage, pod.UID)
				}
			})
		}
	case "namespace":
		workloadIndex := make(map[string]int, len(data.WorkloadsList))
		for i := range data.WorkloadsList {
			workloadIndex[data.WorkloadsList[i].Ref.WorkloadUID] = i
		}
		catalog.visitPods(data.Request.ClusterID, data.Request.RequestedNamespace, func(pod datasource.CatalogPod) {
			if data.Namespace != nil {
				add(&data.Namespace.Usage, pod.UID)
			}
			if i, ok := workloadIndex[pod.WorkloadUID]; ok {
				add(&data.WorkloadsList[i].Usage, pod.UID)
			}
		})
	case "namespace-list":
		byNamespace := make(map[string]int, len(data.Namespaces))
		for i := range data.Namespaces {
			byNamespace[data.Namespaces[i].Name] = i
		}
		catalog.visitPods(data.Request.ClusterID, "", func(pod datasource.CatalogPod) {
			if i, ok := byNamespace[pod.Namespace]; ok {
				add(&data.Namespaces[i].Usage, pod.UID)
			}
		})
	}
}
