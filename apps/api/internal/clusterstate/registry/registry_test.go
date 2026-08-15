package registry

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
)

func TestNewRejectsUnsafeDurationAndRateLimits(t *testing.T) {
	mutations := map[string]func(*Limits){
		"stale above maximum":     func(l *Limits) { l.StaleTTL = MaxStaleTTL + time.Nanosecond },
		"heartbeat above maximum": func(l *Limits) { l.HeartbeatTimeout = MaxHeartbeatTimeout + time.Nanosecond },
		"heartbeat above stale":   func(l *Limits) { l.StaleTTL, l.HeartbeatTimeout = time.Second, 2*time.Second },
		"frame rate above maximum": func(l *Limits) {
			l.IngressFrameRate = MaxIngressFrameRate + 0.1
		},
		"byte rate above maximum": func(l *Limits) { l.IngressByteRate = MaxIngressByteRate + 1 },
		"frame rate NaN":          func(l *Limits) { l.IngressFrameRate = math.NaN() },
		"byte rate infinity":      func(l *Limits) { l.IngressByteRate = math.Inf(1) },
		"frame rate negative infinity": func(l *Limits) {
			l.IngressFrameRate = math.Inf(-1)
		},
		"frame burst above maximum": func(l *Limits) {
			l.IngressFrameBurst = MaxIngressFrameBurst + 1
		},
		"byte burst above maximum": func(l *Limits) { l.IngressByteBurst = MaxIngressByteBurst + 1 },
		"byte burst below message": func(l *Limits) { l.IngressByteBurst = l.MaxMessageBytes - 1 },
		"message above maximum":    func(l *Limits) { l.MaxMessageBytes = MaxProtocolMessageBytes + 1 },
		"resources above maximum":  func(l *Limits) { l.MaxResources = MaxProjectedResources + 1 },
		"clusters above maximum":   func(l *Limits) { l.MaxClusters = MaxConfiguredClusters + 1 },
		"allowlist above capacity": func(l *Limits) { l.MaxClusters, l.AllowedClusters = 1, []string{"a", "b"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			limits := DefaultLimits()
			mutate(&limits)
			if _, err := New(limits); err == nil {
				t.Fatal("unsafe limits accepted")
			}
		})
	}
	limits := DefaultLimits()
	limits.StaleTTL = MaxStaleTTL
	limits.HeartbeatTimeout = MaxHeartbeatTimeout
	limits.IngressFrameRate = MaxIngressFrameRate
	limits.IngressByteRate = MaxIngressByteRate
	limits.IngressFrameBurst = MaxIngressFrameBurst
	limits.IngressByteBurst = MaxIngressByteBurst
	if _, err := New(limits); err != nil {
		t.Fatalf("safe maxima rejected: %v", err)
	}
	limits = DefaultLimits()
	limits.IngressByteBurst = limits.MaxMessageBytes
	if _, err := New(limits); err != nil {
		t.Fatalf("message/burst equality rejected: %v", err)
	}
	limits = DefaultLimits()
	limits.AllowedClusters = make([]string, MaxConfiguredClusters)
	for i := range limits.AllowedClusters {
		limits.AllowedClusters[i] = fmt.Sprintf("c-%02d", i)
	}
	if _, err := New(limits); err != nil {
		t.Fatalf("exact cluster capacity rejected: %v", err)
	}
}

func TestCatalogSnapshot100kBudgetsAndConcurrentReplicas(t *testing.T) {
	r := newRegistry(t)
	_ = r.Connect(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	_ = r.Begin("a", &v1.BeginSnapshot{Epoch: 1})
	for start := 0; start < 100_000; start += 1000 {
		resources := make([]*v1.Resource, 1000)
		for i := range resources {
			id := fmt.Sprintf("p-%06d", start+i)
			resources[i] = &v1.Resource{Kind: v1.KindPod, Uid: id, Namespace: "ns", Name: id, Pod: &v1.PodProjection{NodeName: "node"}}
		}
		if err := r.Chunk("a", &v1.SnapshotChunk{Resources: resources}); err != nil {
			t.Fatal(err)
		}
	}
	if _, nack := r.Commit("a", &v1.CommitSnapshot{Epoch: 1}); nack != nil {
		t.Fatal(nack)
	}
	for sample := 0; sample < 3; sample++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		started := time.Now()
		snapshot, err := r.CatalogSnapshot("a", 64<<20)
		elapsed := time.Since(started)
		runtime.ReadMemStats(&after)
		if err != nil || len(snapshot.Resources) != 100_000 || snapshot.Bytes > 64<<20 {
			t.Fatalf("sample=%d snapshot=%v err=%v", sample, snapshot, err)
		}
		if (!raceEnabled && elapsed > 1500*time.Millisecond) || after.TotalAlloc-before.TotalAlloc > 64<<20 {
			t.Fatalf("sample=%d elapsed=%v alloc=%d bytes=%d", sample, elapsed, after.TotalAlloc-before.TotalAlloc, snapshot.Bytes)
		}
		t.Logf("sample=%d latency=%v alloc=%d wireBytes<=%d", sample+1, elapsed, after.TotalAlloc-before.TotalAlloc, snapshot.Bytes+len(snapshot.Resources)*6+128)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			if _, err := r.CatalogSnapshot("a", 64<<20); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	time.Sleep(time.Millisecond)
	deltaStarted := time.Now()
	if _, nack := r.Delta("a", &v1.Delta{Epoch: 1, Seq: 1, Resource: pod("changed")}); nack != nil {
		t.Fatal(nack)
	}
	deltaWait := time.Since(deltaStarted)
	wg.Wait()
	runtime.ReadMemStats(&after)
	if (!raceEnabled && deltaWait > 1500*time.Millisecond) || after.TotalAlloc-before.TotalAlloc > 128<<20 {
		t.Fatalf("replicas deltaWait=%v alloc=%d", deltaWait, after.TotalAlloc-before.TotalAlloc)
	}
	t.Logf("twoReplicas alloc=%d deltaLockWait=%v", after.TotalAlloc-before.TotalAlloc, deltaWait)
}

func TestSessionIngressHeartbeatAndDisconnectAreGenerationBound(t *testing.T) {
	l := DefaultLimits()
	l.AllowedClusters = []string{"a"}
	l.IngressFrameBurst = 4
	l.IngressFrameRate, l.IngressByteRate = MinIngressFrameRate, MinIngressByteRate
	r, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000, 0)
	r.SetClock(func() time.Time { return now })
	hello := &v1.Hello{ClusterId: "a", ProtocolVersion: v1.Version}
	g1, err := r.OpenSession(hello, "a")
	if err != nil {
		t.Fatal(err)
	}
	if r.ConsumeIngressSession("a", g1+1, 8) || !r.ConsumeIngressSession("a", g1, 8) {
		t.Fatal("ingress accepted a preempted generation or rejected the active generation")
	}
	c := r.get("a")
	c.mu.Lock()
	framesBefore, bytesBefore := c.frameTokens, c.byteTokens
	c.mu.Unlock()
	g2, err := r.OpenSession(hello, "a")
	if err != nil || g2 == g1 {
		t.Fatalf("reconnect generation=%d err=%v", g2, err)
	}
	if r.ConsumeIngressSession("a", g1, 8) {
		t.Fatal("old generation consumed ingress capacity")
	}
	c.mu.Lock()
	if c.frameTokens != framesBefore || c.byteTokens != bytesBefore {
		t.Fatalf("reconnect reset ingress bucket: frames=%v bytes=%v", c.frameTokens, c.byteTokens)
	}
	c.mu.Unlock()
	now = now.Add(time.Second)
	if !r.ConsumeIngressSession("a", g2, 8) {
		t.Fatal("elapsed time did not refill the active generation bucket")
	}
	if _, nack := r.Heartbeat("a", g2, &v1.Heartbeat{Epoch: 1}); nack == nil || nack.Code != "snapshot_required" {
		t.Fatalf("pre-snapshot heartbeat=%+v", nack)
	}

	watch, err := r.Subscribe([]string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	if err := r.BeginSession("a", g2, &v1.BeginSnapshot{Epoch: 7}); err != nil {
		t.Fatal(err)
	}
	if err := r.ChunkSession("a", g2, &v1.SnapshotChunk{Resources: []*v1.Resource{pod("p")}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.CommitSession("a", g2, &v1.CommitSnapshot{Epoch: 7}); nack != nil {
		t.Fatal(nack)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if change, ok := watch.Next(ctx); !ok || !change.Reset {
		t.Fatalf("commit watch change=%+v ok=%v", change, ok)
	}
	cases := map[string]struct {
		generation uint64
		heartbeat  *v1.Heartbeat
	}{
		"old generation": {g1, &v1.Heartbeat{Epoch: 7}},
		"wrong epoch":    {g2, &v1.Heartbeat{Epoch: 8}},
		"future seq":     {g2, &v1.Heartbeat{Epoch: 7, Seq: 1}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if ack, nack := r.Heartbeat("a", tc.generation, tc.heartbeat); ack != nil || nack == nil || !nack.FullResync {
				t.Fatalf("ack=%+v nack=%+v", ack, nack)
			}
		})
	}
	// A clock rollback cannot move the trusted observation time backwards.
	now = now.Add(-time.Minute)
	ack, nack := r.Heartbeat("a", g2, &v1.Heartbeat{Epoch: 7})
	if nack != nil || ack == nil || ack.Epoch != 7 || ack.AppliedSeq != 0 {
		t.Fatalf("ack=%+v nack=%+v", ack, nack)
	}
	change, ok := watch.Next(ctx)
	if !ok || !change.Heartbeat || change.Epoch != 7 || change.Seq != 0 || change.Observed.Before(time.Unix(1_000, 0)) {
		t.Fatalf("heartbeat change=%+v ok=%v", change, ok)
	}

	if err := r.BeginSession("a", g2, &v1.BeginSnapshot{Epoch: 8}); err != nil {
		t.Fatal(err)
	}
	if err := r.ChunkSession("a", g2, &v1.SnapshotChunk{Resources: []*v1.Resource{pod("replacement")}}); err != nil {
		t.Fatal(err)
	}
	retainedWithStaging := r.RetainedBytes()
	r.DisconnectSession("a", g1)
	if !r.SessionValid("a", g2) || r.RetainedBytes() != retainedWithStaging {
		t.Fatal("old disconnect changed active generation")
	}
	r.DisconnectSession("a", g2)
	if r.SessionValid("a", g2) {
		t.Fatal("active generation remained connected")
	}
	live, _, err := r.Snapshot("a")
	if err != nil || live.Epoch != 7 || len(live.Resources) != 1 || r.RetainedBytes() >= retainedWithStaging {
		t.Fatalf("last-good snapshot/accounting lost: live=%+v retained=%d err=%v", live, r.RetainedBytes(), err)
	}
}

func TestFailedReplacementCommitKeepsLastGoodAndReleasesRetainedBytes(t *testing.T) {
	r := newRegistry(t)
	if err := r.Connect(&v1.Hello{ClusterId: "a", ProtocolVersion: v1.Version}, "a"); err != nil {
		t.Fatal(err)
	}
	if err := r.Begin("a", &v1.BeginSnapshot{Epoch: 7}); err != nil {
		t.Fatal(err)
	}
	if err := r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{pod("last-good")}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.Commit("a", &v1.CommitSnapshot{Epoch: 7}); nack != nil {
		t.Fatal(nack)
	}
	wantRetained := r.RetainedBytes()

	// Deltas that arrive during a replacement are staged, but a gap aborts the
	// replacement atomically and releases both staging and pending bytes.
	if err := r.Begin("a", &v1.BeginSnapshot{Epoch: 8}); err != nil {
		t.Fatal(err)
	}
	if err := r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{pod("replacement")}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.Delta("a", &v1.Delta{Epoch: 8, Seq: 2, Resource: pod("gap")}); nack != nil {
		t.Fatal(nack)
	}
	if _, nack := r.Commit("a", &v1.CommitSnapshot{Epoch: 8}); nack == nil || nack.Code != "sequence_gap" {
		t.Fatalf("gap commit=%+v", nack)
	}
	if r.RetainedBytes() != wantRetained {
		t.Fatalf("gap retained=%d want=%d", r.RetainedBytes(), wantRetained)
	}

	// A restarted agent cannot roll the authoritative epoch backwards.
	if err := r.Begin("a", &v1.BeginSnapshot{Epoch: 7}); err != nil {
		t.Fatal(err)
	}
	if err := r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{pod("rollback")}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.Commit("a", &v1.CommitSnapshot{Epoch: 7}); nack == nil || nack.Code != "epoch_rollback" {
		t.Fatalf("rollback commit=%+v", nack)
	}
	snapshot, _, err := r.Snapshot("a")
	if err != nil || snapshot.Epoch != 7 || snapshot.Resources[v1.KindPod+"\x00last-good"] == nil || r.RetainedBytes() != wantRetained {
		t.Fatalf("last-good changed: snapshot=%+v retained=%d err=%v", snapshot, r.RetainedBytes(), err)
	}
}

func TestDeleteDeltaPublishesClosedIdentityAndDuplicateIsSideEffectFree(t *testing.T) {
	r := newRegistry(t)
	generation, err := r.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: v1.Version}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.BeginSession("a", generation, &v1.BeginSnapshot{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.ChunkSession("a", generation, &v1.SnapshotChunk{Resources: []*v1.Resource{pod("p")}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.CommitSession("a", generation, &v1.CommitSnapshot{Epoch: 1}); nack != nil {
		t.Fatal(nack)
	}
	subscription, err := r.Subscribe([]string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	deleted := &v1.Delta{Epoch: 1, Seq: 1, Deleted: true, Resource: pod("p")}
	ack, nack := r.DeltaSession("a", generation, deleted)
	if nack != nil || ack == nil || ack.AppliedSeq != 1 {
		t.Fatalf("delete ack=%+v nack=%+v", ack, nack)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	change, ok := subscription.Next(ctx)
	if !ok || change.Change == nil || change.Change.Action != v1.CatalogAction_CATALOG_DELETED || change.Change.Resource == nil || change.Change.Resource.Uid != "p" {
		t.Fatalf("delete change=%+v ok=%v", change, ok)
	}
	snapshot, _, err := r.Snapshot("a")
	if err != nil || len(snapshot.Resources) != 0 || snapshot.Seq != 1 || snapshot.Bytes != 0 || r.RetainedBytes() != 0 {
		t.Fatalf("deleted snapshot=%+v retained=%d err=%v", snapshot, r.RetainedBytes(), err)
	}
	ack, nack = r.DeltaSession("a", generation, deleted)
	if nack != nil || ack == nil || !ack.Duplicate || ack.AppliedSeq != 1 || r.RetainedBytes() != 0 {
		t.Fatalf("duplicate delete ack=%+v nack=%+v retained=%d", ack, nack, r.RetainedBytes())
	}
	short, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if duplicateChange, ok := subscription.Next(short); ok {
		t.Fatalf("duplicate delete published %+v", duplicateChange)
	}
}

func TestWatchCapacityAccountingOverflowAndClusterIsolation(t *testing.T) {
	l := DefaultLimits()
	l.AllowedClusters = []string{"a", "b"}
	l.WatchMaxSubscribers, l.WatchMaxPerCluster = 2, 1
	l.WatchQueueFrames, l.WatchQueueBytes, l.WatchTotalQueueBytes = 1, 1024, 2048
	r, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.Subscribe([]string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := r.Subscribe([]string{"a"}); err == nil {
		t.Fatal("per-cluster watcher cap accepted")
	}
	b, err := r.Subscribe([]string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, id := range []string{"a", "b"} {
		_ = r.Connect(&v1.Hello{ClusterId: id, ProtocolVersion: 1}, id)
		_ = r.Begin(id, &v1.BeginSnapshot{Epoch: 1})
		_ = r.Chunk(id, &v1.SnapshotChunk{Resources: []*v1.Resource{pod(id)}})
		_, _ = r.Commit(id, &v1.CommitSnapshot{Epoch: 1})
	}
	started := time.Now()
	if _, nack := r.Delta("a", &v1.Delta{Epoch: 1, Seq: 1, Resource: pod("a2")}); nack != nil {
		t.Fatal(nack)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("slow watcher blocked agent mutation")
	}
	// A's full queue closes only A. B remains subscribed and its queued reset is intact.
	if r.WatcherCount() != 1 {
		t.Fatalf("watchers=%d", r.WatcherCount())
	}
	if change, ok := b.Next(context.Background()); !ok || !change.Reset || change.ClusterID != "b" {
		t.Fatalf("B watcher affected: %+v %v", change, ok)
	}
	a.Close()
	b.Close()
	if got := r.WatchQueuedBytes(); got != 0 {
		t.Fatalf("queued byte leak=%d", got)
	}
}

func pod(uid string) *v1.Resource {
	return &v1.Resource{Kind: v1.KindPod, Uid: uid, Namespace: "ns", Name: uid, Pod: &v1.PodProjection{Phase: "Running"}}
}
func newRegistry(t *testing.T) *Registry {
	t.Helper()
	r, e := New(DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestSnapshotDeltaIsolationAndTTL(t *testing.T) {
	r := newRegistry(t)
	now := time.Unix(100, 0)
	r.SetClock(func() time.Time { return now })
	for _, id := range []string{"a", "b"} {
		if e := r.Connect(&v1.Hello{ClusterId: id, ProtocolVersion: 1}, id); e != nil {
			t.Fatal(e)
		}
		_ = r.Begin(id, &v1.BeginSnapshot{Epoch: 1})
		_ = r.Chunk(id, &v1.SnapshotChunk{Resources: []*v1.Resource{pod(id + "1")}})
		if _, n := r.Commit(id, &v1.CommitSnapshot{Epoch: 1}); n != nil {
			t.Fatal(n)
		}
	}
	if _, n := r.Delta("a", &v1.Delta{Epoch: 1, Seq: 2, Resource: pod("a2")}); n == nil || !n.FullResync {
		t.Fatal("gap must resync")
	}
	b, _, _ := r.Snapshot("b")
	if len(b.Resources) != 1 {
		t.Fatal("cluster b changed")
	}
	r.Disconnect("a")
	if _, stale, e := r.Snapshot("a"); e != nil || !stale {
		t.Fatalf("last known state: %v %v", stale, e)
	}
	now = now.Add(6 * time.Minute)
	if _, _, e := r.Snapshot("a"); e == nil {
		t.Fatal("expired state served")
	}
}
func TestSnapshotReplaysDuringSnapshotDeltaExactlyOnce(t *testing.T) {
	r := newRegistry(t)
	_ = r.Connect(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	_ = r.Begin("a", &v1.BeginSnapshot{Epoch: 7, BaseSeq: 10})
	_ = r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{pod("p1")}})
	if a, n := r.Delta("a", &v1.Delta{Epoch: 7, Seq: 11, Resource: pod("p2")}); n != nil || a.AppliedSeq != 11 {
		t.Fatal(a, n)
	}
	a, n := r.Commit("a", &v1.CommitSnapshot{Epoch: 7})
	if n != nil || a.AppliedSeq != 11 {
		t.Fatal(a, n)
	}
	s, _, _ := r.Snapshot("a")
	if len(s.Resources) != 2 {
		t.Fatal("loss or duplicate")
	}
	a, n = r.Delta("a", &v1.Delta{Epoch: 7, Seq: 11, Resource: pod("p2")})
	if n != nil || !a.Duplicate {
		t.Fatal("duplicate not ignored")
	}
}
func TestFailClosedIdentityAndShape(t *testing.T) {
	r := newRegistry(t)
	if r.Connect(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "b") == nil {
		t.Fatal("frame identity accepted")
	}
	_ = r.Connect(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	_ = r.Begin("a", &v1.BeginSnapshot{Epoch: 1})
	bad := pod("p")
	bad.Event = &v1.EventProjection{}
	if r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{bad}}) == nil {
		t.Fatal("ambiguous projection accepted")
	}
}

func TestResourceValidationRejectsEverySensitiveBoundedShape(t *testing.T) {
	r := newRegistry(t)
	long := string(make([]byte, r.limits.MaxStringBytes+1))
	valid := func() *v1.Resource { return pod("p") }
	cases := map[string]func(*v1.Resource){
		"nil-owner":  func(x *v1.Resource) { x.Owners = []*v1.OwnerRef{nil} },
		"owner-uid":  func(x *v1.Resource) { x.Owners = []*v1.OwnerRef{{Uid: ""}} },
		"containers": func(x *v1.Resource) { x.Pod.Containers = make([]*v1.ContainerStatus, r.limits.MaxContainers+1) },
		"pod-reason": func(x *v1.Resource) { x.Pod.Reason = long },
		"container-secret": func(x *v1.Resource) {
			x.Pod.Containers = []*v1.ContainerStatus{{Name: "c", MaskedMessage: "token=secret"}}
		},
		"container-restarts": func(x *v1.Resource) { x.Pod.Containers = []*v1.ContainerStatus{{Name: "c", Restarts: -1}} },
		"images": func(x *v1.Resource) {
			x.Pod = nil
			x.Kind = v1.KindDeployment
			x.Workload = &v1.WorkloadProjection{Images: make([]string, r.limits.MaxImages+1)}
		},
		"rollout": func(x *v1.Resource) {
			x.Pod = nil
			x.Kind = v1.KindDeployment
			x.Workload = &v1.WorkloadProjection{RolloutReason: long}
		},
		"image-string": func(x *v1.Resource) {
			x.Pod = nil
			x.Kind = v1.KindDeployment
			x.Workload = &v1.WorkloadProjection{Images: []string{long}}
		},
		"event-secret": func(x *v1.Resource) {
			x.Pod = nil
			x.Kind = v1.KindEvent
			x.Event = &v1.EventProjection{MaskedMessage: "Bearer secret"}
		},
		"event-identity": func(x *v1.Resource) {
			x.Pod = nil
			x.Kind = v1.KindEvent
			x.Event = &v1.EventProjection{InvolvedUid: string(make([]byte, 254))}
		},
		"unknown-kind": func(x *v1.Resource) { x.Kind = "Secret" },
		"ambiguous":    func(x *v1.Resource) { x.Node = &v1.NodeProjection{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			x := valid()
			mutate(x)
			if r.validate(x) == nil {
				t.Fatal("accepted invalid resource")
			}
		})
	}
	if r.validate(&v1.Resource{Kind: v1.KindReplicaSet, Uid: "rs", Name: "rs"}) != nil {
		t.Fatal("narrow ReplicaSet identity rejected")
	}
}
func TestSessionPreemption(t *testing.T) {
	r := newRegistry(t)
	g1, e := r.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	if e != nil {
		t.Fatal(e)
	}
	g2, e := r.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	if e != nil || g2 <= g1 || r.SessionValid("a", g1) || !r.SessionValid("a", g2) {
		t.Fatal(g1, g2, e)
	}
}
func TestPreemptedSessionCannotMutate(t *testing.T) {
	r := newRegistry(t)
	g1, _ := r.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	if e := r.BeginSession("a", g1, &v1.BeginSnapshot{Epoch: 1}); e != nil {
		t.Fatal(e)
	}
	g2, _ := r.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	if e := r.ChunkSession("a", g1, &v1.SnapshotChunk{Resources: []*v1.Resource{pod("old")}}); e == nil {
		t.Fatal("old chunk mutated")
	}
	if _, n := r.DeltaSession("a", g1, &v1.Delta{Epoch: 1, Seq: 1, Resource: pod("old")}); n == nil || n.Code != "session_preempted" {
		t.Fatal(n)
	}
	if e := r.BeginSession("a", g2, &v1.BeginSnapshot{Epoch: 2}); e != nil {
		t.Fatal(e)
	}
	_ = r.ChunkSession("a", g2, &v1.SnapshotChunk{Resources: []*v1.Resource{pod("new")}})
	if _, n := r.CommitSession("a", g2, &v1.CommitSnapshot{Epoch: 2}); n != nil {
		t.Fatal(n)
	}
	s, _, _ := r.Snapshot("a")
	if len(s.Resources) != 1 {
		t.Fatal(s)
	}
}
func TestAllowedClustersDoNotAllocateUnknown(t *testing.T) {
	l := DefaultLimits()
	l.AllowedClusters = []string{"a"}
	r, e := New(l)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.OpenSession(&v1.Hello{ClusterId: "b", ProtocolVersion: 1}, "b"); e == nil {
		t.Fatal("unknown accepted")
	}
	if len(r.clusters) != 0 {
		t.Fatal("unknown allocated state")
	}
}
func TestNestedAndAggregateByteLimits(t *testing.T) {
	l := DefaultLimits()
	l.MaxMessageBytes = 1024
	l.MaxStateBytes = 1200
	l.MaxStringBytes = 900
	r, e := New(l)
	if e != nil {
		t.Fatal(e)
	}
	_ = r.Connect(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	_ = r.Begin("a", &v1.BeginSnapshot{Epoch: 1})
	bad := pod("p")
	bad.Owners = []*v1.OwnerRef{{Uid: "u", Kind: string(make([]byte, 65)), Name: "n"}}
	if r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{bad}}) == nil {
		t.Fatal("nested overflow accepted")
	}
	_ = r.Begin("a", &v1.BeginSnapshot{Epoch: 2})
	event := func(id string) *v1.Resource {
		return &v1.Resource{Kind: v1.KindEvent, Uid: id, Name: id, Event: &v1.EventProjection{MaskedMessage: string(make([]byte, 700))}}
	}
	if e := r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{event("e1"), event("e2")}}); e == nil {
		t.Fatal("aggregate overflow accepted")
	}
}
func TestRejectedDeltaDoesNotAdvanceAndPendingIsCloned(t *testing.T) {
	l := DefaultLimits()
	l.MaxMessageBytes = 1024
	l.MaxStateBytes = 1024
	l.MaxStringBytes = 1000
	r, _ := New(l)
	_ = r.Connect(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	_ = r.Begin("a", &v1.BeginSnapshot{Epoch: 1})
	_ = r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{pod("p")}})
	_, _ = r.Commit("a", &v1.CommitSnapshot{Epoch: 1})
	big := &v1.Resource{Kind: v1.KindEvent, Uid: "e", Name: "e", Event: &v1.EventProjection{MaskedMessage: string(make([]byte, 1000))}}
	if _, n := r.Delta("a", &v1.Delta{Epoch: 1, Seq: 1, Resource: big}); n == nil {
		t.Fatal("oversize accepted")
	}
	s, _, _ := r.Snapshot("a")
	if s.Seq != 0 {
		t.Fatal("rejected delta advanced")
	}
	_ = r.Begin("a", &v1.BeginSnapshot{Epoch: 2, BaseSeq: 5})
	d := v1.Delta{Epoch: 2, Seq: 6, Resource: pod("before")}
	_, _ = r.Delta("a", &d)
	d.Resource.Name = "after"
	if _, n := r.Commit("a", &v1.CommitSnapshot{Epoch: 2}); n != nil {
		t.Fatal(n)
	}
	s, _, _ = r.Snapshot("a")
	if s.Resources[key(pod("before"))].Name != "before" {
		t.Fatal("pending pointer mutation leaked")
	}
}

func TestSnapshotChunkClonesCallerResource(t *testing.T) {
	r, err := New(DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Connect(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a"); err != nil {
		t.Fatal(err)
	}
	if err = r.Begin("a", &v1.BeginSnapshot{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	input := pod("original")
	if err = r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{input}}); err != nil {
		t.Fatal(err)
	}
	input.Name = "mutated"
	if _, nack := r.Commit("a", &v1.CommitSnapshot{Epoch: 1}); nack != nil {
		t.Fatal(nack)
	}
	snapshot, _, err := r.Snapshot("a")
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Resources[key(pod("original"))].Name; got != "original" {
		t.Fatalf("caller mutation leaked into retained snapshot: %q", got)
	}
}

func TestHeartbeatTimeoutStaleThenExpires(t *testing.T) {
	l := DefaultLimits()
	l.HeartbeatTimeout = 10 * time.Second
	l.StaleTTL = 20 * time.Second
	r, _ := New(l)
	now := time.Unix(100, 0)
	r.SetClock(func() time.Time { return now })
	g, _ := r.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	_ = r.BeginSession("a", g, &v1.BeginSnapshot{Epoch: 1})
	_ = r.ChunkSession("a", g, &v1.SnapshotChunk{Resources: []*v1.Resource{pod("p")}})
	_, _ = r.CommitSession("a", g, &v1.CommitSnapshot{Epoch: 1})
	now = now.Add(11 * time.Second)
	if _, stale, e := r.Snapshot("a"); e != nil || !stale {
		t.Fatal(stale, e)
	}
	now = now.Add(20 * time.Second)
	if _, _, e := r.Snapshot("a"); e == nil {
		t.Fatal("expired heartbeat state served")
	}
}

func TestHeartbeatRequiresLiveStateAndExpiryForcesSameEpochResnapshot(t *testing.T) {
	l := DefaultLimits()
	l.AllowedClusters = []string{"a"}
	l.HeartbeatTimeout = 10 * time.Second
	l.StaleTTL = 20 * time.Second
	r, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	r.SetClock(func() time.Time { return now })
	sub, err := r.Subscribe([]string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	generation, err := r.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: v1.Version}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, nack := r.Heartbeat("a", generation, &v1.Heartbeat{Epoch: 1}); nack == nil || nack.Code != "snapshot_required" || !nack.FullResync {
		t.Fatalf("heartbeat before snapshot=%+v", nack)
	}
	if err = r.BeginSession("a", generation, &v1.BeginSnapshot{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err = r.ChunkSession("a", generation, &v1.SnapshotChunk{Resources: []*v1.Resource{pod("old")}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.CommitSession("a", generation, &v1.CommitSnapshot{Epoch: 1}); nack != nil {
		t.Fatal(nack)
	}
	if change, ok := sub.Next(context.Background()); !ok || !change.Reset || change.Epoch != 1 {
		t.Fatalf("initial reset=%+v ok=%v", change, ok)
	}

	now = now.Add(31 * time.Second)
	if got := r.PruneExpired(); got != 1 {
		t.Fatalf("pruned=%d", got)
	}
	if change, ok := sub.Next(context.Background()); !ok || !change.Expired {
		t.Fatalf("expiry=%+v ok=%v", change, ok)
	}
	if _, nack := r.Heartbeat("a", generation, &v1.Heartbeat{Epoch: 1}); nack == nil || nack.Code != "snapshot_required" {
		t.Fatalf("late heartbeat revived expired state: %+v", nack)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if change, ok := sub.Next(ctx); ok {
		t.Fatalf("rejected heartbeat was published: %+v", change)
	}

	// Pruning removes live state, so the agent can restore the same server-issued
	// epoch/sequence without being trapped by the normal rollback protection.
	if err = r.BeginSession("a", generation, &v1.BeginSnapshot{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err = r.ChunkSession("a", generation, &v1.SnapshotChunk{Resources: []*v1.Resource{pod("restored")}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.CommitSession("a", generation, &v1.CommitSnapshot{Epoch: 1}); nack != nil {
		t.Fatal(nack)
	}
	if change, ok := sub.Next(context.Background()); !ok || !change.Reset || change.Epoch != 1 || change.Seq != 0 {
		t.Fatalf("same-epoch recovery reset=%+v ok=%v", change, ok)
	}
}

func TestGlobalByteAdmissionIsolatesClusters(t *testing.T) {
	l := DefaultLimits()
	l.MaxMessageBytes = 1024
	l.MaxStateBytes = 1200
	l.MaxTotalStateBytes = 1200
	l.MaxStringBytes = 900
	l.AllowedClusters = []string{"a", "b"}
	r, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		_ = r.Connect(&v1.Hello{ClusterId: id, ProtocolVersion: 1}, id)
		_ = r.Begin(id, &v1.BeginSnapshot{Epoch: 1})
	}
	if err := r.Chunk("b", &v1.SnapshotChunk{Resources: []*v1.Resource{pod("b")}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.Commit("b", &v1.CommitSnapshot{Epoch: 1}); nack != nil {
		t.Fatal(nack)
	}
	large := func(id string, n int) *v1.Resource {
		return &v1.Resource{Kind: v1.KindEvent, Uid: id, Name: id, Event: &v1.EventProjection{MaskedMessage: string(make([]byte, n))}}
	}
	if err := r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{large("a1", 850)}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{large("a2", 300)}}); err == nil || err.Error() != "global_state_byte_capacity" {
		t.Fatalf("global overflow accepted: %v", err)
	}
	var bCount int
	if _, err := r.View("b", func(s *Snapshot) error { bCount = len(s.Resources); return nil }); err != nil || bCount != 1 {
		t.Fatalf("cluster b query affected: count=%d err=%v", bCount, err)
	}
}

func TestDisconnectAndExpiryReleaseRetainedBytes(t *testing.T) {
	l := DefaultLimits()
	l.AllowedClusters = []string{"a"}
	l.StaleTTL = time.Minute
	r, _ := New(l)
	now := time.Unix(100, 0)
	r.SetClock(func() time.Time { return now })
	g, _ := r.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	_ = r.BeginSession("a", g, &v1.BeginSnapshot{Epoch: g})
	_ = r.ChunkSession("a", g, &v1.SnapshotChunk{Resources: []*v1.Resource{pod("staged")}})
	if r.RetainedBytes() <= 0 {
		t.Fatal("staging not accounted")
	}
	r.DisconnectSession("a", g)
	if r.RetainedBytes() != 0 {
		t.Fatalf("staging leak: %d", r.RetainedBytes())
	}
	g, _ = r.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	_ = r.BeginSession("a", g, &v1.BeginSnapshot{Epoch: g})
	_ = r.ChunkSession("a", g, &v1.SnapshotChunk{Resources: []*v1.Resource{pod("live")}})
	_, _ = r.CommitSession("a", g, &v1.CommitSnapshot{Epoch: g})
	r.DisconnectSession("a", g)
	if r.RetainedBytes() <= 0 {
		t.Fatal("live state missing")
	}
	now = now.Add(2 * time.Minute)
	if got := r.PruneExpired(); got != 1 || r.RetainedBytes() != 0 {
		t.Fatalf("prune=%d retained=%d", got, r.RetainedBytes())
	}
}

func TestInvalidStateTransitionsFailClosedWithoutAccountingDrift(t *testing.T) {
	r := newRegistry(t)
	if _, err := r.OpenSession(nil, "a"); err == nil {
		t.Fatal("nil hello accepted")
	}
	for _, hello := range []*v1.Hello{
		{ClusterId: "a", ProtocolVersion: v1.Version + 1},
		{ClusterId: "INVALID", ProtocolVersion: v1.Version},
		{ClusterId: "a", ProtocolVersion: v1.Version},
	} {
		authenticated := "a"
		if hello.ClusterId == "a" && hello.ProtocolVersion == v1.Version {
			authenticated = "b"
		}
		if _, err := r.OpenSession(hello, authenticated); err == nil {
			t.Fatalf("invalid hello accepted: %+v", hello)
		}
	}
	if err := r.Connect(&v1.Hello{ClusterId: "a", ProtocolVersion: v1.Version}, "a"); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() bool{
		"nil begin":          func() bool { return r.Begin("a", nil) == nil },
		"unknown begin":      func() bool { return r.Begin("b", &v1.BeginSnapshot{Epoch: 1}) == nil },
		"zero epoch":         func() bool { return r.Begin("a", &v1.BeginSnapshot{}) == nil },
		"nil chunk":          func() bool { return r.Chunk("a", nil) == nil },
		"unknown chunk":      func() bool { return r.Chunk("b", &v1.SnapshotChunk{}) == nil },
		"chunk before begin": func() bool { return r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{pod("p")}}) == nil },
		"nil commit": func() bool {
			_, nack := r.Commit("a", nil)
			return nack == nil
		},
		"unknown commit": func() bool {
			_, nack := r.Commit("b", &v1.CommitSnapshot{Epoch: 1})
			return nack == nil
		},
		"commit before begin": func() bool {
			_, nack := r.Commit("a", &v1.CommitSnapshot{Epoch: 1})
			return nack == nil
		},
		"nil delta": func() bool {
			_, nack := r.Delta("a", nil)
			return nack == nil
		},
		"unknown delta": func() bool {
			_, nack := r.Delta("b", &v1.Delta{Epoch: 1, Seq: 1, Resource: pod("p")})
			return nack == nil
		},
		"delta before snapshot": func() bool {
			_, nack := r.Delta("a", &v1.Delta{Epoch: 1, Seq: 1, Resource: pod("p")})
			return nack == nil || nack.Code != "snapshot_required"
		},
		"nil heartbeat": func() bool {
			_, nack := r.Heartbeat("a", 0, nil)
			return nack == nil
		},
		"unknown heartbeat": func() bool {
			_, nack := r.Heartbeat("b", 0, &v1.Heartbeat{Epoch: 1})
			return nack == nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := r.RetainedBytes()
			if call() {
				t.Fatal("invalid transition accepted")
			}
			if after := r.RetainedBytes(); after != before {
				t.Fatalf("accounting changed: before=%d after=%d", before, after)
			}
		})
	}
	if r.ConsumeIngress("a", 0) || r.ConsumeIngress("a", -1) || r.ConsumeIngress("a", r.limits.MaxMessageBytes+1) {
		t.Fatal("invalid ingress byte count accepted")
	}
	c := r.get("a")
	c.mu.Lock()
	if c.frameTokens != 0 || c.byteTokens != 0 || !c.ingressAt.IsZero() {
		t.Fatalf("rejected ingress changed token bucket: frames=%v bytes=%v at=%v", c.frameTokens, c.byteTokens, c.ingressAt)
	}
	c.mu.Unlock()
	if !r.ConsumeIngress("a", 1) {
		t.Fatal("valid ingress unexpectedly rejected")
	}
	if snapshot, _, err := r.Snapshot("a"); err == nil || snapshot != nil || r.RetainedBytes() != 0 {
		t.Fatalf("rejected pre-snapshot delta exposed state: snapshot=%v err=%v retained=%d", snapshot, err, r.RetainedBytes())
	}
}
