// Package registry owns bounded, per-cluster normalized Kubernetes state.
package registry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterid"
	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"google.golang.org/protobuf/proto"
)

type Limits struct {
	MaxClusters, MaxResources, MaxChunkResources, MaxMessageBytes int
	MaxStateBytes                                                 int
	MaxTotalStateBytes                                            int64
	MaxLabels, MaxOwners, MaxContainers, MaxImages                int
	MaxStringBytes                                                int
	StaleTTL                                                      time.Duration
	HeartbeatTimeout                                              time.Duration
	IngressFrameRate, IngressByteRate                             float64
	IngressFrameBurst, IngressByteBurst                           int
	WatchMaxSubscribers, WatchMaxPerCluster, WatchQueueFrames     int
	WatchQueueBytes, WatchTotalQueueBytes                         int64
	AllowedClusters                                               []string
}

const (
	MaxConfiguredClusters     = 64
	MaxProjectedResources     = 100_000
	MinProtocolMessageBytes   = 1024
	MaxProtocolMessageBytes   = 4 << 20
	MaxStaleTTL               = 24 * time.Hour
	MaxHeartbeatTimeout       = time.Hour
	MinIngressFrameRate       = 1
	MaxIngressFrameRate       = 100_000
	MinIngressByteRate        = 1024
	MaxIngressByteRate        = 1 << 30
	MinIngressFrameBurst      = 1
	MaxIngressFrameBurst      = 100_000
	MinIngressByteBurst       = 1024
	MaxIngressByteBurst       = 1 << 30
	MaxSnapshotChunkResources = 1000
)

func DefaultLimits() Limits {
	return Limits{MaxClusters: MaxConfiguredClusters, MaxResources: MaxProjectedResources, MaxChunkResources: MaxSnapshotChunkResources, MaxMessageBytes: MaxProtocolMessageBytes, MaxStateBytes: 256 << 20, MaxTotalStateBytes: 512 << 20, MaxLabels: 32, MaxOwners: 4, MaxContainers: 64, MaxImages: 64, MaxStringBytes: 2048, StaleTTL: 5 * time.Minute, HeartbeatTimeout: 45 * time.Second, IngressFrameRate: 1000, IngressByteRate: 16 << 20, IngressFrameBurst: 2000, IngressByteBurst: 8 << 20, WatchMaxSubscribers: 16, WatchMaxPerCluster: 8, WatchQueueFrames: 256, WatchQueueBytes: 16 << 20, WatchTotalQueueBytes: 64 << 20}
}

type Snapshot struct {
	Epoch, Seq uint64
	Resources  map[string]*v1.Resource
	ObservedAt time.Time
	Bytes      int
	sizes      map[string]int
}
type cluster struct {
	mu                      sync.Mutex
	live                    *Snapshot
	staging                 *Snapshot
	pending                 []*v1.Delta
	pendingBytes            int
	connected               bool
	disconnectedAt          time.Time
	heartbeatAt             time.Time
	generation              uint64
	frameTokens, byteTokens float64
	ingressAt               time.Time
}
type Registry struct {
	limits      Limits
	now         func() time.Time
	mu          sync.RWMutex
	clusters    map[string]*cluster
	allowed     map[string]struct{}
	totalBytes  atomic.Int64
	watchMu     sync.Mutex
	watchers    map[*watcher]struct{}
	watchQueued atomic.Int64
}

// WatchChange is a bounded registry-to-API notification. Reset means the API
// replica must reconnect and replace its local cluster snapshot atomically.
type WatchChange struct {
	ClusterID string
	Epoch     uint64
	Seq       uint64
	Change    *v1.CatalogChange
	Reset     bool
	Heartbeat bool
	Expired   bool
	Observed  time.Time
}

// WatchSnapshot is the minimal bounded projection needed by API-local catalog
// and SSE identity. It intentionally excludes full Pod/container/workload state.
type WatchSnapshot struct {
	Epoch, Seq uint64
	Observed   time.Time
	Resources  []*v1.CatalogResource
	Bytes      int
}

func (r *Registry) CatalogSnapshot(id string, maxBytes int) (*WatchSnapshot, error) {
	if maxBytes < 1024 || maxBytes > 64<<20 {
		return nil, fmt.Errorf("invalid_watch_byte_limit")
	}
	c := r.get(id)
	if c == nil {
		return nil, fmt.Errorf("cluster_unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == nil {
		return nil, fmt.Errorf("cluster_unavailable")
	}
	out := &WatchSnapshot{Epoch: c.live.Epoch, Seq: c.live.Seq, Observed: c.live.ObservedAt, Resources: make([]*v1.CatalogResource, 0, len(c.live.Resources))}
	keys := make([]string, 0, len(c.live.Resources))
	for key, resource := range c.live.Resources {
		if !persistentCatalogKind(resource.Kind) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		resource := c.live.Resources[key]
		x := catalogResource(resource)
		size := proto.Size(x)
		if out.Bytes+size > maxBytes {
			return nil, fmt.Errorf("watch_snapshot_capacity")
		}
		out.Bytes += size
		out.Resources = append(out.Resources, x)
	}
	return out, nil
}

func persistentCatalogKind(kind string) bool {
	return kind == v1.KindPod || kind == v1.KindReplicaSet || kind == v1.KindDeployment || kind == v1.KindStatefulSet || kind == v1.KindDaemonSet || kind == v1.KindCronJob
}

func catalogResource(resource *v1.Resource) *v1.CatalogResource {
	x := &v1.CatalogResource{Kind: resource.Kind, Uid: resource.Uid, Namespace: resource.Namespace, Name: resource.Name, Owners: make([]*v1.CatalogOwner, 0, len(resource.Owners))}
	for _, owner := range resource.Owners {
		x.Owners = append(x.Owners, &v1.CatalogOwner{Uid: owner.Uid, Kind: owner.Kind, Name: owner.Name})
	}
	if resource.Kind == v1.KindPod && resource.Pod != nil {
		x.NodeName = resource.Pod.NodeName
	}
	return x
}

type watcher struct {
	clusters map[string]struct{}
	ch       chan queuedWatch
	bytes    atomic.Int64
}

type queuedWatch struct {
	change WatchChange
	bytes  int64
}
type WatchSubscription struct {
	registry *Registry
	watcher  *watcher
	once     sync.Once
}

func New(l Limits) (*Registry, error) {
	d := DefaultLimits()
	if l.MaxClusters == 0 {
		l = d
	}
	if l.MaxClusters < 1 || l.MaxClusters > MaxConfiguredClusters || len(l.AllowedClusters) > l.MaxClusters || l.MaxResources < 1 || l.MaxResources > MaxProjectedResources || l.MaxChunkResources < 1 || l.MaxChunkResources > MaxSnapshotChunkResources || l.MaxChunkResources > l.MaxResources || l.MaxMessageBytes < MinProtocolMessageBytes || l.MaxMessageBytes > MaxProtocolMessageBytes || l.MaxStateBytes < l.MaxMessageBytes || l.MaxTotalStateBytes < int64(l.MaxStateBytes) || l.MaxLabels < 0 || l.MaxOwners < 0 || l.MaxContainers < 0 || l.MaxImages < 0 || l.MaxStringBytes < 64 || l.StaleTTL <= 0 || l.StaleTTL > MaxStaleTTL || l.HeartbeatTimeout <= 0 || l.HeartbeatTimeout > MaxHeartbeatTimeout || l.HeartbeatTimeout > l.StaleTTL || l.IngressFrameRate < MinIngressFrameRate || l.IngressFrameRate > MaxIngressFrameRate || math.IsNaN(l.IngressFrameRate) || math.IsInf(l.IngressFrameRate, 0) || l.IngressByteRate < MinIngressByteRate || l.IngressByteRate > MaxIngressByteRate || math.IsNaN(l.IngressByteRate) || math.IsInf(l.IngressByteRate, 0) || l.IngressFrameBurst < MinIngressFrameBurst || l.IngressFrameBurst > MaxIngressFrameBurst || l.IngressByteBurst < l.MaxMessageBytes || l.IngressByteBurst > MaxIngressByteBurst || l.WatchMaxSubscribers < 1 || l.WatchMaxSubscribers > 256 || l.WatchMaxPerCluster < 1 || l.WatchMaxPerCluster > l.WatchMaxSubscribers || l.WatchQueueFrames < 1 || l.WatchQueueFrames > 4096 || l.WatchQueueBytes < 1024 || l.WatchTotalQueueBytes < l.WatchQueueBytes {
		return nil, errors.New("invalid cluster-state registry limits")
	}
	allowed := map[string]struct{}{}
	for _, id := range l.AllowedClusters {
		if !clusterid.Valid(id) {
			return nil, errors.New("invalid allowed cluster")
		}
		if _, ok := allowed[id]; ok {
			return nil, errors.New("duplicate allowed cluster")
		}
		allowed[id] = struct{}{}
	}
	return &Registry{limits: l, now: time.Now, clusters: map[string]*cluster{}, allowed: allowed, watchers: map[*watcher]struct{}{}}, nil
}

// Subscribe registers a bounded feed before callers take initial snapshots,
// so changes racing with snapshot transfer are queued and replayed by seq.
func (r *Registry) Subscribe(clusterIDs []string) (*WatchSubscription, error) {
	if len(clusterIDs) < 1 || len(clusterIDs) > r.limits.MaxClusters {
		return nil, fmt.Errorf("invalid_watch")
	}
	w := &watcher{clusters: make(map[string]struct{}, len(clusterIDs)), ch: make(chan queuedWatch, r.limits.WatchQueueFrames)}
	for _, id := range clusterIDs {
		if !clusterid.Valid(id) {
			return nil, fmt.Errorf("invalid_watch_cluster")
		}
		if len(r.allowed) > 0 {
			if _, ok := r.allowed[id]; !ok {
				return nil, fmt.Errorf("unknown_cluster")
			}
		}
		if _, duplicate := w.clusters[id]; duplicate {
			return nil, fmt.Errorf("duplicate_watch_cluster")
		}
		w.clusters[id] = struct{}{}
	}
	r.watchMu.Lock()
	if len(r.watchers) >= r.limits.WatchMaxSubscribers {
		r.watchMu.Unlock()
		return nil, fmt.Errorf("watch_capacity")
	}
	for id := range w.clusters {
		count := 0
		for existing := range r.watchers {
			if _, ok := existing.clusters[id]; ok {
				count++
			}
		}
		if count >= r.limits.WatchMaxPerCluster {
			r.watchMu.Unlock()
			return nil, fmt.Errorf("watch_cluster_capacity")
		}
	}
	r.watchers[w] = struct{}{}
	r.watchMu.Unlock()
	return &WatchSubscription{registry: r, watcher: w}, nil
}

func (s *WatchSubscription) Next(ctx context.Context) (WatchChange, bool) {
	select {
	case <-ctx.Done():
		return WatchChange{}, false
	case item, ok := <-s.watcher.ch:
		if !ok {
			return WatchChange{}, false
		}
		s.watcher.bytes.Add(-item.bytes)
		s.registry.watchQueued.Add(-item.bytes)
		return item.change, true
	}
}

func (s *WatchSubscription) Close() {
	s.once.Do(func() {
		r, w := s.registry, s.watcher
		r.watchMu.Lock()
		if _, ok := r.watchers[w]; ok {
			delete(r.watchers, w)
			close(w.ch)
		}
		r.watchMu.Unlock()
		for item := range w.ch {
			w.bytes.Add(-item.bytes)
			r.watchQueued.Add(-item.bytes)
		}
	})
}

func (r *Registry) reserveWatchBytes(n int64) bool {
	for {
		old := r.watchQueued.Load()
		if old+n > r.limits.WatchTotalQueueBytes {
			return false
		}
		if r.watchQueued.CompareAndSwap(old, old+n) {
			return true
		}
	}
}

func (r *Registry) WatchQueuedBytes() int64 { return r.watchQueued.Load() }
func (r *Registry) WatcherCount() int {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	return len(r.watchers)
}

func (r *Registry) publish(change WatchChange) {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	for w := range r.watchers {
		if _, ok := w.clusters[change.ClusterID]; !ok {
			continue
		}
		copyChange := change
		if change.Change != nil {
			copyChange.Change = proto.Clone(change.Change).(*v1.CatalogChange)
		}
		size := int64(64)
		if copyChange.Change != nil {
			size += int64(proto.Size(copyChange.Change))
		}
		if w.bytes.Load()+size > r.limits.WatchQueueBytes || !r.reserveWatchBytes(size) {
			delete(r.watchers, w)
			close(w.ch)
			continue
		}
		w.bytes.Add(size)
		select {
		case w.ch <- queuedWatch{change: copyChange, bytes: size}:
		default:
			w.bytes.Add(-size)
			r.watchQueued.Add(-size)
			delete(r.watchers, w)
			close(w.ch)
		}
	}
}

func (r *Registry) reserveBytes(delta int) bool {
	if delta <= 0 {
		r.totalBytes.Add(int64(delta))
		return true
	}
	for {
		old := r.totalBytes.Load()
		if old+int64(delta) > r.limits.MaxTotalStateBytes {
			return false
		}
		if r.totalBytes.CompareAndSwap(old, old+int64(delta)) {
			return true
		}
	}
}

func retainedBytes(c *cluster) int {
	n := c.pendingBytes
	if c.live != nil {
		n += c.live.Bytes
	}
	if c.staging != nil {
		n += c.staging.Bytes
	}
	return n
}

func (r *Registry) SetClock(now func() time.Time) { r.now = now }
func (r *Registry) ConsumeIngress(id string, bytes int) bool {
	return r.consumeIngress(id, 0, bytes)
}
func (r *Registry) ConsumeIngressSession(id string, g uint64, bytes int) bool {
	return r.consumeIngress(id, g, bytes)
}
func (r *Registry) consumeIngress(id string, g uint64, bytes int) bool {
	if bytes < 1 || bytes > r.limits.MaxMessageBytes {
		return false
	}
	c := r.get(id)
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if g != 0 && (!c.connected || c.generation != g) {
		return false
	}
	now := r.now()
	if c.ingressAt.IsZero() {
		c.frameTokens = float64(r.limits.IngressFrameBurst)
		c.byteTokens = float64(r.limits.IngressByteBurst)
	} else {
		seconds := now.Sub(c.ingressAt).Seconds()
		c.frameTokens = min(float64(r.limits.IngressFrameBurst), c.frameTokens+seconds*r.limits.IngressFrameRate)
		c.byteTokens = min(float64(r.limits.IngressByteBurst), c.byteTokens+seconds*r.limits.IngressByteRate)
	}
	c.ingressAt = now
	if c.frameTokens < 1 || c.byteTokens < float64(bytes) {
		return false
	}
	c.frameTokens--
	c.byteTokens -= float64(bytes)
	return true
}
func (r *Registry) Connect(h *v1.Hello, authenticatedClusterID string) error {
	_, err := r.OpenSession(h, authenticatedClusterID)
	return err
}
func (r *Registry) OpenSession(h *v1.Hello, authenticatedClusterID string) (uint64, error) {
	if h == nil {
		return 0, fmt.Errorf("invalid_hello")
	}
	if h.ProtocolVersion != v1.Version {
		return 0, fmt.Errorf("protocol_version")
	}
	if !clusterid.Valid(h.ClusterId) || h.ClusterId != authenticatedClusterID {
		return 0, fmt.Errorf("identity_mismatch")
	}
	if len(r.allowed) > 0 {
		if _, ok := r.allowed[h.ClusterId]; !ok {
			return 0, fmt.Errorf("unknown_cluster")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.clusters[h.ClusterId]
	if c == nil {
		if len(r.clusters) >= r.limits.MaxClusters {
			return 0, fmt.Errorf("cluster_capacity")
		}
		c = &cluster{}
		r.clusters[h.ClusterId] = c
	}
	c.mu.Lock()
	oldRetained := retainedBytes(c)
	c.staging = nil
	c.pending = nil
	c.pendingBytes = 0
	r.reserveBytes(retainedBytes(c) - oldRetained)
	c.generation++
	c.connected = true
	c.disconnectedAt = time.Time{}
	c.heartbeatAt = r.now()
	gen := c.generation
	c.mu.Unlock()
	return gen, nil
}
func (r *Registry) SessionValid(id string, g uint64) bool {
	c := r.get(id)
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected && c.generation == g
}
func (r *Registry) Heartbeat(id string, g uint64, h *v1.Heartbeat) (*v1.Ack, *v1.Nack) {
	if h == nil {
		return nil, &v1.Nack{Code: "invalid_heartbeat", FullResync: true}
	}
	c := r.get(id)
	if c == nil {
		return nil, &v1.Nack{Code: "unknown_cluster", FullResync: true}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected || c.generation != g {
		return nil, &v1.Nack{Code: "session_preempted", FullResync: true}
	}
	if c.live == nil {
		return nil, &v1.Nack{Code: "snapshot_required", FullResync: true}
	}
	if h.Epoch != c.live.Epoch || h.Seq != c.live.Seq {
		return nil, &v1.Nack{Code: "heartbeat_state_mismatch", FullResync: true}
	}
	c.heartbeatAt = r.now()
	observed := c.heartbeatAt
	if observed.Before(c.live.ObservedAt) {
		observed = c.live.ObservedAt
	}
	r.publish(WatchChange{ClusterID: id, Epoch: h.Epoch, Seq: h.Seq, Heartbeat: true, Observed: observed})
	return &v1.Ack{Epoch: h.Epoch, AppliedSeq: h.Seq}, nil
}
func (r *Registry) Disconnect(id string) {
	if c := r.get(id); c != nil {
		c.mu.Lock()
		oldRetained := retainedBytes(c)
		c.connected = false
		c.disconnectedAt = r.now()
		c.staging = nil
		c.pending = nil
		c.pendingBytes = 0
		r.reserveBytes(retainedBytes(c) - oldRetained)
		c.mu.Unlock()
	}
}
func (r *Registry) DisconnectSession(id string, g uint64) {
	if c := r.get(id); c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.generation == g {
			oldRetained := retainedBytes(c)
			c.connected = false
			c.disconnectedAt = r.now()
			c.staging = nil
			c.pending = nil
			c.pendingBytes = 0
			r.reserveBytes(retainedBytes(c) - oldRetained)
		}
	}
}

// PruneExpired releases last-known state after its stale serving window.
// The configured cluster/session bucket remains bounded by the fixed allowlist.
func (r *Registry) PruneExpired() int {
	now := r.now()
	pruned := 0
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, c := range r.clusters {
		c.mu.Lock()
		staleSince := c.disconnectedAt
		if c.connected && now.Sub(c.heartbeatAt) > r.limits.HeartbeatTimeout {
			staleSince = c.heartbeatAt.Add(r.limits.HeartbeatTimeout)
		}
		if c.live != nil && !staleSince.IsZero() && now.Sub(staleSince) > r.limits.StaleTTL {
			r.reserveBytes(-c.live.Bytes)
			c.live = nil
			pruned++
			r.publish(WatchChange{ClusterID: id, Expired: true, Observed: now})
		}
		c.mu.Unlock()
	}
	return pruned
}

func (r *Registry) RetainedBytes() int64 { return r.totalBytes.Load() }
func (r *Registry) Begin(id string, b *v1.BeginSnapshot) error {
	return r.begin(id, 0, b)
}
func (r *Registry) BeginSession(id string, g uint64, b *v1.BeginSnapshot) error {
	return r.begin(id, g, b)
}
func (r *Registry) begin(id string, g uint64, b *v1.BeginSnapshot) error {
	if b == nil {
		return fmt.Errorf("invalid_snapshot")
	}
	c := r.get(id)
	if c == nil {
		return fmt.Errorf("unknown_cluster")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if g != 0 && (!c.connected || c.generation != g) {
		return fmt.Errorf("session_preempted")
	}
	if b.Epoch == 0 {
		return fmt.Errorf("invalid_epoch")
	}
	oldRetained := retainedBytes(c)
	c.staging = &Snapshot{Epoch: b.Epoch, Seq: b.BaseSeq, Resources: map[string]*v1.Resource{}, sizes: map[string]int{}}
	c.pending = nil
	c.pendingBytes = 0
	r.reserveBytes(retainedBytes(c) - oldRetained)
	return nil
}
func (r *Registry) Chunk(id string, ch *v1.SnapshotChunk) error {
	return r.chunk(id, 0, ch)
}
func (r *Registry) ChunkSession(id string, g uint64, ch *v1.SnapshotChunk) error {
	return r.chunk(id, g, ch)
}
func (r *Registry) chunk(id string, g uint64, ch *v1.SnapshotChunk) error {
	if ch == nil {
		return fmt.Errorf("invalid_chunk")
	}
	c := r.get(id)
	if c == nil {
		return fmt.Errorf("unknown_cluster")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if g != 0 && (!c.connected || c.generation != g) {
		return fmt.Errorf("session_preempted")
	}
	if c.staging == nil {
		return fmt.Errorf("snapshot_not_started")
	}
	if len(ch.Resources) > r.limits.MaxChunkResources {
		return fmt.Errorf("chunk_limit")
	}
	for _, x := range ch.Resources {
		if err := r.validate(x); err != nil {
			return err
		}
		k := key(x)
		if _, exists := c.staging.Resources[k]; !exists && len(c.staging.Resources) >= r.limits.MaxResources {
			return fmt.Errorf("resource_capacity")
		}
		sz := proto.Size(x)
		next := c.staging.Bytes - c.staging.sizes[k] + sz
		if sz > r.limits.MaxMessageBytes || next > r.limits.MaxStateBytes {
			return fmt.Errorf("state_byte_capacity")
		}
		if !r.reserveBytes(next - c.staging.Bytes) {
			return fmt.Errorf("global_state_byte_capacity")
		}
		c.staging.Resources[k] = clone(x)
		c.staging.sizes[k] = sz
		c.staging.Bytes = next
	}
	return nil
}
func (r *Registry) Delta(id string, d *v1.Delta) (*v1.Ack, *v1.Nack) {
	return r.delta(id, 0, d)
}
func (r *Registry) DeltaSession(id string, g uint64, d *v1.Delta) (*v1.Ack, *v1.Nack) {
	return r.delta(id, g, d)
}
func (r *Registry) delta(id string, g uint64, d *v1.Delta) (*v1.Ack, *v1.Nack) {
	if d == nil {
		return nil, &v1.Nack{Code: "invalid_delta", FullResync: true}
	}
	c := r.get(id)
	if c == nil {
		return nil, &v1.Nack{Code: "unknown_cluster", FullResync: true}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if g != 0 && (!c.connected || c.generation != g) {
		return nil, &v1.Nack{Code: "session_preempted", FullResync: true}
	}
	if r.validate(d.Resource) != nil {
		return nil, &v1.Nack{Code: "invalid_resource", FullResync: true}
	}
	cur := c.live
	applied := uint64(0)
	epoch := d.Epoch
	if cur == nil && c.staging == nil {
		return nil, &v1.Nack{Code: "snapshot_required", FullResync: true}
	}
	if cur != nil {
		applied = cur.Seq
		if epoch != cur.Epoch && c.staging == nil {
			return nil, &v1.Nack{Code: "epoch_mismatch", FullResync: true}
		}
	}
	if d.Seq <= applied {
		return &v1.Ack{Epoch: epoch, AppliedSeq: applied, Duplicate: true}, nil
	}
	if d.Seq != applied+1 && c.staging == nil {
		return nil, &v1.Nack{Code: "sequence_gap", FullResync: true}
	}
	if c.staging != nil {
		if len(c.pending) >= r.limits.MaxChunkResources {
			return nil, &v1.Nack{Code: "pending_capacity", FullResync: true}
		}
		sz := proto.Size(d)
		if c.staging.Bytes+c.pendingBytes+sz > r.limits.MaxStateBytes {
			return nil, &v1.Nack{Code: "pending_byte_capacity", FullResync: true}
		}
		if !r.reserveBytes(sz) {
			return nil, &v1.Nack{Code: "global_state_byte_capacity", FullResync: true}
		}
		copyDelta := proto.Clone(d).(*v1.Delta)
		c.pending = append(c.pending, copyDelta)
		c.pendingBytes += sz
		return &v1.Ack{Epoch: d.Epoch, AppliedSeq: d.Seq}, nil
	}
	_, existed := cur.Resources[key(d.Resource)]
	byteDelta := -cur.sizes[key(d.Resource)]
	if !d.Deleted {
		byteDelta += proto.Size(d.Resource)
	}
	if byteDelta > 0 && !r.reserveBytes(byteDelta) {
		return nil, &v1.Nack{Code: "global_state_byte_capacity", FullResync: true}
	}
	if !r.apply(cur, d) {
		if byteDelta > 0 {
			r.reserveBytes(-byteDelta)
		}
		return nil, &v1.Nack{Code: "state_byte_capacity", FullResync: true}
	}
	if byteDelta < 0 {
		r.reserveBytes(byteDelta)
	}
	cur.ObservedAt = r.now()
	action := v1.CatalogAction_CATALOG_CREATED
	if d.Deleted {
		action = v1.CatalogAction_CATALOG_DELETED
	} else if existed {
		action = v1.CatalogAction_CATALOG_UPDATED
	}
	r.publish(WatchChange{ClusterID: id, Epoch: d.Epoch, Seq: d.Seq, Change: &v1.CatalogChange{Epoch: d.Epoch, Seq: d.Seq, Resource: catalogResource(d.Resource), Action: action}, Observed: cur.ObservedAt})
	return &v1.Ack{Epoch: cur.Epoch, AppliedSeq: cur.Seq}, nil
}
func (r *Registry) Commit(id string, x *v1.CommitSnapshot) (*v1.Ack, *v1.Nack) {
	return r.commit(id, 0, x)
}
func (r *Registry) CommitSession(id string, g uint64, x *v1.CommitSnapshot) (*v1.Ack, *v1.Nack) {
	return r.commit(id, g, x)
}
func (r *Registry) commit(id string, g uint64, x *v1.CommitSnapshot) (*v1.Ack, *v1.Nack) {
	if x == nil {
		return nil, &v1.Nack{Code: "invalid_commit", FullResync: true}
	}
	c := r.get(id)
	if c == nil {
		return nil, &v1.Nack{Code: "unknown_cluster", FullResync: true}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if g != 0 && (!c.connected || c.generation != g) {
		return nil, &v1.Nack{Code: "session_preempted", FullResync: true}
	}
	if c.staging == nil || c.staging.Epoch != x.Epoch {
		return nil, &v1.Nack{Code: "snapshot_mismatch", FullResync: true}
	}
	oldRetained := retainedBytes(c)
	for _, d := range c.pending {
		if d.Epoch != x.Epoch || d.Seq <= c.staging.Seq {
			continue
		}
		if d.Seq != c.staging.Seq+1 {
			c.staging = nil
			c.pending = nil
			c.pendingBytes = 0
			r.reserveBytes(-oldRetained + retainedBytes(c))
			return nil, &v1.Nack{Code: "sequence_gap", FullResync: true}
		}
		if !r.apply(c.staging, d) {
			c.staging = nil
			c.pending = nil
			c.pendingBytes = 0
			r.reserveBytes(-oldRetained + retainedBytes(c))
			return nil, &v1.Nack{Code: "state_byte_capacity", FullResync: true}
		}
	}
	c.staging.ObservedAt = r.now()
	if c.live != nil && c.staging.Epoch <= c.live.Epoch {
		c.staging = nil
		c.pending = nil
		c.pendingBytes = 0
		r.reserveBytes(-oldRetained + retainedBytes(c))
		return nil, &v1.Nack{Code: "epoch_rollback", FullResync: true}
	}
	c.live = c.staging
	a := &v1.Ack{Epoch: x.Epoch, AppliedSeq: c.staging.Seq}
	c.staging = nil
	c.pending = nil
	c.pendingBytes = 0
	r.reserveBytes(retainedBytes(c) - oldRetained)
	r.publish(WatchChange{ClusterID: id, Epoch: c.live.Epoch, Seq: c.live.Seq, Reset: true, Observed: c.live.ObservedAt})
	return a, nil
}
func (r *Registry) Snapshot(id string) (*Snapshot, bool, error) {
	c := r.get(id)
	if c == nil {
		return nil, false, fmt.Errorf("cluster_unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.live
	if s == nil {
		return nil, false, fmt.Errorf("cluster_unavailable")
	}
	stale := !c.connected || r.now().Sub(c.heartbeatAt) > r.limits.HeartbeatTimeout
	staleSince := c.disconnectedAt
	if c.connected {
		staleSince = c.heartbeatAt.Add(r.limits.HeartbeatTimeout)
	}
	if stale && r.now().Sub(staleSince) > r.limits.StaleTTL {
		return nil, false, fmt.Errorf("cluster_unavailable")
	}
	return copySnapshot(s, s.ObservedAt), stale, nil
}

// View performs a screen projection under the per-cluster boundary without
// cloning the complete resource map. Callers must not retain references.
func (r *Registry) View(id string, fn func(*Snapshot) error) (bool, error) {
	c := r.get(id)
	if c == nil {
		return false, fmt.Errorf("cluster_unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == nil {
		return false, fmt.Errorf("cluster_unavailable")
	}
	stale := !c.connected || r.now().Sub(c.heartbeatAt) > r.limits.HeartbeatTimeout
	staleSince := c.disconnectedAt
	if c.connected {
		staleSince = c.heartbeatAt.Add(r.limits.HeartbeatTimeout)
	}
	if stale && r.now().Sub(staleSince) > r.limits.StaleTTL {
		return false, fmt.Errorf("cluster_unavailable")
	}
	return stale, fn(c.live)
}
func (r *Registry) get(id string) *cluster { r.mu.RLock(); defer r.mu.RUnlock(); return r.clusters[id] }
func key(x *v1.Resource) string            { return x.Kind + "\x00" + x.Uid }
func copySnapshot(s *Snapshot, at time.Time) *Snapshot {
	n := &Snapshot{Epoch: s.Epoch, Seq: s.Seq, ObservedAt: at, Bytes: s.Bytes, Resources: make(map[string]*v1.Resource, len(s.Resources)), sizes: make(map[string]int, len(s.sizes))}
	for k, v := range s.Resources {
		n.Resources[k] = clone(v)
		n.sizes[k] = s.sizes[k]
	}
	return n
}
func (r *Registry) apply(s *Snapshot, d *v1.Delta) bool {
	if s.sizes == nil {
		s.sizes = make(map[string]int, len(s.Resources))
		for k, v := range s.Resources {
			s.sizes[k] = proto.Size(v)
			s.Bytes += s.sizes[k]
		}
	}
	if d.Deleted {
		k := key(d.Resource)
		s.Bytes -= s.sizes[k]
		delete(s.sizes, k)
		delete(s.Resources, k)
	} else {
		k := key(d.Resource)
		sz := proto.Size(d.Resource)
		next := s.Bytes - s.sizes[k] + sz
		if sz > r.limits.MaxMessageBytes || next > r.limits.MaxStateBytes {
			return false
		}
		s.Resources[k] = clone(d.Resource)
		s.sizes[k] = sz
		s.Bytes = next
	}
	s.Epoch = d.Epoch
	s.Seq = d.Seq
	return true
}
func clone(in *v1.Resource) *v1.Resource {
	return proto.Clone(in).(*v1.Resource)
}
func (r *Registry) validate(x *v1.Resource) error {
	if x == nil || x.Uid == "" || x.Name == "" || len(x.Uid) > 253 || len(x.Name) > 253 || len(x.Namespace) > 253 {
		return fmt.Errorf("invalid_identity")
	}
	if len(x.Owners) > r.limits.MaxOwners {
		return fmt.Errorf("metadata_limit")
	}
	for _, o := range x.Owners {
		if o == nil || o.Uid == "" || len(o.Uid) > 253 || len(o.Kind) > 64 || len(o.Name) > 253 {
			return fmt.Errorf("owner_limit")
		}
	}
	projections := 0
	if x.Pod != nil {
		projections++
		if len(x.Pod.Containers) > r.limits.MaxContainers {
			return fmt.Errorf("container_limit")
		}
		if len(x.Pod.Phase) > 64 || len(x.Pod.Reason) > r.limits.MaxStringBytes {
			return fmt.Errorf("pod_limit")
		}
		for _, c := range x.Pod.Containers {
			if c == nil || len(c.Name) > 253 || len(c.State) > 32 || len(c.Reason) > r.limits.MaxStringBytes || len(c.MaskedMessage) > r.limits.MaxStringBytes || containsSensitive(c.MaskedMessage) || len(c.LastTerminationReason) > r.limits.MaxStringBytes || len(c.Image) > r.limits.MaxStringBytes || len(c.ImageId) > r.limits.MaxStringBytes || c.Restarts < 0 {
				return fmt.Errorf("container_limit")
			}
		}
	}
	if x.Workload != nil {
		projections++
		if len(x.Workload.Images) > r.limits.MaxImages {
			return fmt.Errorf("image_limit")
		}
		if len(x.Workload.RolloutReason) > r.limits.MaxStringBytes || len(x.Workload.RolloutStatus) > 32 {
			return fmt.Errorf("workload_limit")
		}
		for _, image := range x.Workload.Images {
			if len(image) > r.limits.MaxStringBytes {
				return fmt.Errorf("image_limit")
			}
		}
	}
	if x.Node != nil {
		projections++
	}
	if x.Event != nil {
		projections++
		if len(x.Event.MaskedMessage) > r.limits.MaxStringBytes || containsSensitive(x.Event.MaskedMessage) {
			return fmt.Errorf("event_limit")
		}
		if len(x.Event.InvolvedUid) > 253 || len(x.Event.InvolvedKind) > 64 || len(x.Event.InvolvedName) > 253 || len(x.Event.Reason) > r.limits.MaxStringBytes || len(x.Event.Type) > 64 {
			return fmt.Errorf("event_limit")
		}
	}
	want := map[string]int{v1.KindPod: 1, v1.KindNode: 1, v1.KindDeployment: 1, v1.KindStatefulSet: 1, v1.KindDaemonSet: 1, v1.KindCronJob: 1, v1.KindEvent: 1, v1.KindReplicaSet: 0}
	p, ok := want[x.Kind]
	if !ok || projections != p {
		return fmt.Errorf("kind_projection_mismatch")
	}
	return nil
}

func containsSensitive(s string) bool {
	s = strings.ToLower(s)
	for _, p := range []string{"bearer ", "token=", "password=", "authorization:", "-----begin"} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
