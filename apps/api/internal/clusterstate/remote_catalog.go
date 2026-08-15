package clusterstate

import (
	"fmt"
	"sort"
	"sync"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterid"
	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"google.golang.org/protobuf/proto"
)

// RemoteCatalog is the bounded, cluster-partitioned API-side projection fed by
// the registry Watch stream. Snapshot commit swaps atomically; deltas mutate
// only one cluster under its lock and never perform another registry RPC.
type RemoteCatalog struct {
	mu               sync.RWMutex
	clusters         map[string]*remoteCatalogCluster
	allowed          map[string]struct{}
	max              int
	maxBytes         int
	maxRetained      int
	maxRetainedBytes int
	totalResources   int
	totalBytes       int
	liveResources    int
	liveBytes        int
}

type remoteCatalogCluster struct {
	mu                  sync.RWMutex
	epoch               uint64
	seq                 uint64
	resources           map[string]*v1.CatalogResource
	staging             map[string]*v1.CatalogResource
	stageEpoch          uint64
	stageSeq            uint64
	stageBytes          int
	stageObservedUnixMs int64
	liveBytes           int
	available           bool
	resyncNeeded        bool
	observedUnixMs      int64
}

func NewRemoteCatalog(clusterIDs []string, maxResources int) (*RemoteCatalog, error) {
	if len(clusterIDs) == 0 || len(clusterIDs) > 64 || maxResources < 1 || maxResources > 100_000 {
		return nil, fmt.Errorf("invalid remote catalog limits")
	}
	r := &RemoteCatalog{clusters: make(map[string]*remoteCatalogCluster, len(clusterIDs)), allowed: make(map[string]struct{}, len(clusterIDs)), max: maxResources, maxBytes: 64 << 20, maxRetained: maxResources * 2, maxRetainedBytes: 128 << 20}
	for _, id := range clusterIDs {
		if !clusterid.Valid(id) {
			return nil, fmt.Errorf("invalid cluster")
		}
		if _, exists := r.allowed[id]; exists {
			return nil, fmt.Errorf("duplicate cluster")
		}
		r.allowed[id] = struct{}{}
		r.clusters[id] = &remoteCatalogCluster{resources: map[string]*v1.CatalogResource{}}
	}
	return r, nil
}

func (r *RemoteCatalog) AllowsCluster(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.allowed[id]
	return ok
}

func (r *RemoteCatalog) Available(id string) bool {
	r.mu.RLock()
	c := r.clusters[id]
	r.mu.RUnlock()
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.available
}

func (r *RemoteCatalog) HasPodUID(clusterID, uid string) bool {
	r.mu.RLock()
	c := r.clusters[clusterID]
	r.mu.RUnlock()
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.available {
		return false
	}
	_, ok := c.resources[v1.KindPod+"\x00"+uid]
	return ok
}

func (r *RemoteCatalog) Apply(frame *v1.WatchFrame) error {
	if frame == nil || proto.Size(frame) > 4<<20 {
		return fmt.Errorf("nil watch frame")
	}
	r.mu.RLock()
	c := r.clusters[frame.ClusterId]
	r.mu.RUnlock()
	if c == nil {
		return fmt.Errorf("unknown cluster")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch frame.Type {
	case v1.WatchFrameType_WATCH_RESET:
		if frame.Epoch != 0 || frame.Seq != 0 || len(frame.Resources) != 0 || frame.Change != nil {
			return fmt.Errorf("invalid reset")
		}
		r.releaseStageLocked(c)
		c.resyncNeeded = true
		return nil
	case v1.WatchFrameType_WATCH_HEARTBEAT:
		if len(frame.Resources) != 0 || frame.Change != nil || !c.available || frame.Epoch != c.epoch || frame.Seq != c.seq || frame.ObservedUnixMs <= 0 || frame.ObservedUnixMs < c.observedUnixMs {
			return fmt.Errorf("invalid heartbeat")
		}
		c.observedUnixMs = frame.ObservedUnixMs
		return nil
	case v1.WatchFrameType_WATCH_EXPIRED:
		if frame.Epoch != 0 || frame.Seq != 0 || frame.ObservedUnixMs <= 0 || len(frame.Resources) != 0 || frame.Change != nil {
			return fmt.Errorf("invalid expiry")
		}
		r.releaseCatalogLocked(c, true)
		c.resources = map[string]*v1.CatalogResource{}
		c.available = false
		c.resyncNeeded = true
		return nil
	case v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN:
		if frame.Epoch == 0 || frame.ObservedUnixMs <= 0 || len(frame.Resources) != 0 || frame.Change != nil {
			return fmt.Errorf("invalid snapshot epoch")
		}
		r.releaseStageLocked(c)
		c.staging = make(map[string]*v1.CatalogResource)
		c.stageEpoch = frame.Epoch
		c.stageSeq = frame.Seq
		c.stageBytes = 0
		c.stageObservedUnixMs = frame.ObservedUnixMs
	case v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK:
		if c.staging == nil || c.stageEpoch != frame.Epoch || c.stageSeq != frame.Seq || c.stageObservedUnixMs != frame.ObservedUnixMs || frame.ObservedUnixMs <= 0 || len(frame.Resources) == 0 || len(frame.Resources) > 1000 || frame.Change != nil {
			r.releaseStageLocked(c)
			return fmt.Errorf("snapshot mismatch")
		}
		for _, resource := range frame.Resources {
			if err := validateCatalogResource(resource, true); err != nil {
				r.releaseStageLocked(c)
				return fmt.Errorf("invalid resource")
			}
			key := resource.Kind + "\x00" + resource.Uid
			if _, exists := c.staging[key]; exists {
				r.releaseStageLocked(c)
				return fmt.Errorf("duplicate catalog resource")
			}
			if len(c.staging) >= r.max {
				r.releaseStageLocked(c)
				return fmt.Errorf("catalog capacity")
			}
			size := proto.Size(resource)
			r.mu.Lock()
			if r.totalResources+1 > r.maxRetained || r.totalBytes+size > r.maxRetainedBytes {
				r.mu.Unlock()
				r.releaseStageLocked(c)
				return fmt.Errorf("catalog byte capacity")
			}
			r.totalResources++
			r.totalBytes += size
			r.mu.Unlock()
			c.stageBytes += size
			c.staging[key] = proto.Clone(resource).(*v1.CatalogResource)
		}
	case v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT:
		if c.staging == nil || c.stageEpoch != frame.Epoch || c.stageSeq != frame.Seq || c.stageObservedUnixMs != frame.ObservedUnixMs || frame.ObservedUnixMs <= 0 || len(frame.Resources) != 0 || frame.Change != nil {
			r.releaseStageLocked(c)
			return fmt.Errorf("snapshot mismatch")
		}
		r.mu.Lock()
		if r.liveResources-len(c.resources)+len(c.staging) > r.max || r.liveBytes-c.liveBytes+c.stageBytes > r.maxBytes {
			r.mu.Unlock()
			r.releaseStageLocked(c)
			return fmt.Errorf("catalog live capacity")
		}
		r.totalResources -= len(c.resources)
		r.totalBytes -= c.liveBytes
		r.liveResources += len(c.staging) - len(c.resources)
		r.liveBytes += c.stageBytes - c.liveBytes
		r.mu.Unlock()
		c.resources, c.epoch, c.seq = c.staging, frame.Epoch, frame.Seq
		c.liveBytes = c.stageBytes
		c.available = true
		c.resyncNeeded = false
		c.observedUnixMs = frame.ObservedUnixMs
		c.staging = nil
		c.stageBytes = 0
	case v1.WatchFrameType_WATCH_DELTA:
		d := frame.Change
		if !c.available || c.resyncNeeded || frame.ObservedUnixMs <= 0 || frame.ObservedUnixMs < c.observedUnixMs || len(frame.Resources) != 0 || d == nil || d.Resource == nil || d.Epoch != frame.Epoch || d.Seq != frame.Seq || d.Epoch != c.epoch || d.Action != v1.CatalogAction_CATALOG_CREATED && d.Action != v1.CatalogAction_CATALOG_UPDATED && d.Action != v1.CatalogAction_CATALOG_DELETED || validateCatalogResource(d.Resource, false) != nil {
			return fmt.Errorf("delta mismatch")
		}
		if d.Seq <= c.seq {
			return nil
		}
		if d.Seq != c.seq+1 {
			r.releaseStageLocked(c)
			c.resyncNeeded = true
			return fmt.Errorf("delta gap")
		}
		if persistentCatalogResourceKind(d.Resource.Kind) {
			key := d.Resource.Kind + "\x00" + d.Resource.Uid
			old, exists := c.resources[key]
			if d.Action == v1.CatalogAction_CATALOG_CREATED && exists || d.Action != v1.CatalogAction_CATALOG_CREATED && !exists {
				return fmt.Errorf("catalog action mismatch")
			}
			oldSize := 0
			if exists {
				oldSize = proto.Size(old)
			}
			if d.Action == v1.CatalogAction_CATALOG_DELETED {
				delete(c.resources, key)
				r.mu.Lock()
				r.totalResources--
				r.totalBytes -= oldSize
				r.liveResources--
				r.liveBytes -= oldSize
				r.mu.Unlock()
				c.liveBytes -= oldSize
			} else {
				newSize := proto.Size(d.Resource)
				r.mu.Lock()
				resourceDelta := 0
				if !exists {
					resourceDelta = 1
				}
				if r.liveResources+resourceDelta > r.max || r.liveBytes-oldSize+newSize > r.maxBytes || r.totalResources+resourceDelta > r.maxRetained || r.totalBytes-oldSize+newSize > r.maxRetainedBytes {
					r.mu.Unlock()
					return fmt.Errorf("catalog capacity")
				}
				r.totalResources += resourceDelta
				r.totalBytes += newSize - oldSize
				r.liveResources += resourceDelta
				r.liveBytes += newSize - oldSize
				r.mu.Unlock()
				c.liveBytes += newSize - oldSize
				c.resources[key] = proto.Clone(d.Resource).(*v1.CatalogResource)
			}
		}
		c.seq = d.Seq
		c.observedUnixMs = frame.ObservedUnixMs
	default:
		return fmt.Errorf("invalid watch frame type")
	}
	return nil
}

func (r *RemoteCatalog) Retained() (resources, bytes int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.totalResources, r.totalBytes
}

func persistentCatalogResourceKind(kind string) bool {
	return kind == v1.KindPod || kind == v1.KindReplicaSet || kind == v1.KindDeployment || kind == v1.KindStatefulSet || kind == v1.KindDaemonSet || kind == v1.KindCronJob
}

func (r *RemoteCatalog) releaseStageLocked(c *remoteCatalogCluster) {
	if c.staging == nil {
		return
	}
	r.mu.Lock()
	r.totalResources -= len(c.staging)
	r.totalBytes -= c.stageBytes
	r.mu.Unlock()
	c.staging = nil
	c.stageBytes = 0
}

func (r *RemoteCatalog) releaseCatalogLocked(c *remoteCatalogCluster, live bool) {
	r.releaseStageLocked(c)
	if live && c.resources != nil {
		r.mu.Lock()
		r.totalResources -= len(c.resources)
		r.totalBytes -= c.liveBytes
		r.liveResources -= len(c.resources)
		r.liveBytes -= c.liveBytes
		r.mu.Unlock()
		c.liveBytes = 0
	}
}

func validateCatalogResource(resource *v1.CatalogResource, snapshot bool) error {
	if resource == nil || resource.Uid == "" || len(resource.Uid) > 253 || resource.Name == "" || len(resource.Name) > 253 || len(resource.Namespace) > 253 || len(resource.NodeName) > 253 || len(resource.Owners) > 4 {
		return fmt.Errorf("invalid identity")
	}
	allowed := map[string]bool{v1.KindPod: true, v1.KindReplicaSet: true, v1.KindDeployment: true, v1.KindStatefulSet: true, v1.KindDaemonSet: true, v1.KindCronJob: true, v1.KindNode: true, v1.KindEvent: true}
	if !allowed[resource.Kind] || snapshot && !persistentCatalogResourceKind(resource.Kind) || resource.Kind != v1.KindPod && resource.NodeName != "" {
		return fmt.Errorf("invalid kind")
	}
	switch resource.Kind {
	case v1.KindNode:
		if resource.Namespace != "" || len(resource.Owners) != 0 {
			return fmt.Errorf("invalid node identity")
		}
	case v1.KindEvent:
		if len(resource.Owners) != 0 {
			return fmt.Errorf("invalid event identity")
		}
	default:
		if resource.Namespace == "" {
			return fmt.Errorf("namespace required")
		}
	}
	ownerKinds := map[string]bool{v1.KindReplicaSet: true, v1.KindDeployment: true, v1.KindStatefulSet: true, v1.KindDaemonSet: true, v1.KindCronJob: true}
	for _, owner := range resource.Owners {
		if owner == nil || owner.Uid == "" || len(owner.Uid) > 253 || !ownerKinds[owner.Kind] || owner.Name == "" || len(owner.Name) > 253 {
			return fmt.Errorf("invalid owner")
		}
	}
	return nil
}

func (r *RemoteCatalog) CatalogPods(clusterID, namespace string, limit int) []datasource.CatalogPod {
	r.mu.RLock()
	c := r.clusters[clusterID]
	r.mu.RUnlock()
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.available {
		return nil
	}
	out := make([]datasource.CatalogPod, 0)
	for _, resource := range c.resources {
		if resource == nil || resource.Kind != v1.KindPod || namespace != "" && resource.Namespace != namespace {
			continue
		}
		kind, name, uid := remoteWorkload(resource, c.resources)
		out = append(out, datasource.CatalogPod{Namespace: resource.Namespace, Name: resource.Name, UID: resource.Uid, WorkloadKind: kind, WorkloadName: name, WorkloadUID: uid, Node: resource.NodeName})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].UID < out[j].UID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (r *RemoteCatalog) visitPods(clusterID, namespace string, visit func(datasource.CatalogPod)) {
	r.mu.RLock()
	c := r.clusters[clusterID]
	r.mu.RUnlock()
	if c == nil {
		return
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.available {
		return
	}
	for _, resource := range c.resources {
		if resource == nil || resource.Kind != v1.KindPod || namespace != "" && resource.Namespace != namespace {
			continue
		}
		kind, name, uid := remoteWorkload(resource, c.resources)
		visit(datasource.CatalogPod{Namespace: resource.Namespace, Name: resource.Name, UID: resource.Uid, WorkloadKind: kind, WorkloadName: name, WorkloadUID: uid, Node: resource.NodeName})
	}
}

func remoteWorkload(pod *v1.CatalogResource, resources map[string]*v1.CatalogResource) (string, string, string) {
	if pod == nil {
		return "", "", ""
	}
	if len(pod.Owners) == 0 {
		return "", "", ""
	}
	o := pod.Owners[0]
	if o.Kind != v1.KindReplicaSet {
		return o.Kind, o.Name, o.Uid
	}
	rs, ok := resources[v1.KindReplicaSet+"\x00"+o.Uid]
	if !ok || rs == nil || len(rs.Owners) == 0 {
		return o.Kind, o.Name, o.Uid
	}
	top := rs.Owners[0]
	return top.Kind, top.Name, top.Uid
}

func (r *RemoteCatalog) StreamEntityNamespaces(clusterID string) map[string]string {
	out := make(map[string]string)
	r.mu.RLock()
	c := r.clusters[clusterID]
	r.mu.RUnlock()
	if c != nil {
		c.mu.RLock()
		if c.available {
			for _, resource := range c.resources {
				if resource == nil {
					continue
				}
				if resource.Kind == v1.KindPod {
					out["pod:"+resource.Uid] = resource.Namespace
					_, _, workloadUID := remoteWorkload(resource, c.resources)
					if workloadUID != "" {
						out["workload:"+workloadUID] = resource.Namespace
					}
				} else if resource.Kind != v1.KindReplicaSet && resource.Uid != "" {
					out["workload:"+resource.Uid] = resource.Namespace
				}
			}
		}
		c.mu.RUnlock()
	}
	return out
}

var _ datasource.PodCatalog = (*RemoteCatalog)(nil)
