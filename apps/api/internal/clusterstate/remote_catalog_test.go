package clusterstate

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
)

func TestRemoteCatalog100kApplyAndQueryBudgets(t *testing.T) {
	catalog, err := NewRemoteCatalog([]string{"a"}, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	chunks := make([][]*v1.CatalogResource, 100)
	for chunk := range chunks {
		chunks[chunk] = make([]*v1.CatalogResource, 1000)
		for i := range chunks[chunk] {
			id := fmt.Sprintf("p-%06d", chunk*1000+i)
			chunks[chunk][i] = &v1.CatalogResource{Kind: v1.KindPod, Uid: id, Namespace: "ns", Name: id, NodeName: "node"}
		}
	}
	for sample := 1; sample <= 3; sample++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		started := time.Now()
		if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: uint64(sample), Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1000}); err != nil {
			t.Fatal(err)
		}
		for _, resources := range chunks {
			if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: uint64(sample), Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, Resources: resources, ObservedUnixMs: 1000}); err != nil {
				t.Fatal(err)
			}
		}
		if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: uint64(sample), Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: 1000}); err != nil {
			t.Fatal(err)
		}
		applyLatency := time.Since(started)
		runtime.ReadMemStats(&after)
		applyAlloc := after.TotalAlloc - before.TotalAlloc
		if (!raceEnabled && applyLatency > 1500*time.Millisecond) || applyAlloc > 64<<20 {
			t.Fatalf("sample=%d apply latency=%v alloc=%d", sample, applyLatency, applyAlloc)
		}
		runtime.GC()
		runtime.ReadMemStats(&before)
		started = time.Now()
		pods := catalog.CatalogPods("a", "", 0)
		queryLatency := time.Since(started)
		runtime.ReadMemStats(&after)
		queryAlloc := after.TotalAlloc - before.TotalAlloc
		if len(pods) != 100_000 || (!raceEnabled && queryLatency > time.Second) || queryAlloc > 64<<20 {
			t.Fatalf("sample=%d pods=%d query=%v alloc=%d", sample, len(pods), queryLatency, queryAlloc)
		}
		t.Logf("sample=%d apply=%v/%dB catalogQuery=%v/%dB", sample, applyLatency, applyAlloc, queryLatency, queryAlloc)
	}
}

func applyCatalogSnapshot(t *testing.T, catalog *RemoteCatalog, id string, epoch, seq uint64, resources ...*v1.CatalogResource) {
	t.Helper()
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: id, Epoch: epoch, Seq: seq, Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if len(resources) > 0 {
		if err := catalog.Apply(&v1.WatchFrame{ClusterId: id, Epoch: epoch, Seq: seq, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, Resources: resources, ObservedUnixMs: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: id, Epoch: epoch, Seq: seq, Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: 1000}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteCatalogAtomicIsolationIdentityAndReset(t *testing.T) {
	catalog, err := NewRemoteCatalog([]string{"a", "b"}, 20)
	if err != nil {
		t.Fatal(err)
	}
	workload := &v1.CatalogResource{Kind: v1.KindDeployment, Uid: "same-workload", Namespace: "ns-a", Name: "api"}
	rs := &v1.CatalogResource{Kind: v1.KindReplicaSet, Uid: "rs-a", Namespace: "ns-a", Name: "api-rs", Owners: []*v1.CatalogOwner{{Kind: v1.KindDeployment, Uid: workload.Uid, Name: workload.Name}}}
	pod := &v1.CatalogResource{Kind: v1.KindPod, Uid: "same-pod", Namespace: "ns-a", Name: "pod", NodeName: "node-a", Owners: []*v1.CatalogOwner{{Kind: v1.KindReplicaSet, Uid: rs.Uid, Name: rs.Name}}}
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, Resources: []*v1.CatalogResource{workload, rs, pod}, ObservedUnixMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if got := catalog.CatalogPods("a", "", 0); got != nil {
		t.Fatalf("partial snapshot visible: %v", got)
	}
	pod.Name, pod.NodeName, pod.Owners[0].Uid = "mutated", "mutated", "mutated"
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: 1000}); err != nil {
		t.Fatal(err)
	}
	applyCatalogSnapshot(t, catalog, "b", 1, 0, &v1.CatalogResource{Kind: v1.KindPod, Uid: "same-pod", Namespace: "ns-b", Name: "pod", NodeName: "node-b"})
	a, b := catalog.CatalogPods("a", "", 0), catalog.CatalogPods("b", "", 0)
	if len(a) != 1 || a[0].Name != "pod" || a[0].Node != "node-a" || a[0].WorkloadUID != "same-workload" || len(b) != 1 || b[0].Namespace != "ns-b" {
		t.Fatalf("catalog leakage/mutation: A=%v B=%v", a, b)
	}
	if ns := catalog.StreamEntityNamespaces("a"); ns["workload:same-workload"] != "ns-a" {
		t.Fatalf("zero-pod workload missing: %v", ns)
	}
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Type: v1.WatchFrameType_WATCH_RESET}); err != nil {
		t.Fatal(err)
	}
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].UID != "same-pod" {
		t.Fatalf("reset dropped last-good A: %v", got)
	}
	if got := catalog.CatalogPods("b", "", 0); len(got) != 1 {
		t.Fatalf("reset A changed B: %v", got)
	}
}

func TestRemoteCatalogGapActionAndGlobalAdmission(t *testing.T) {
	catalog, _ := NewRemoteCatalog([]string{"a", "b"}, 3)
	applyCatalogSnapshot(t, catalog, "b", 1, 0, &v1.CatalogResource{Kind: v1.KindPod, Uid: "b", Namespace: "ns", Name: "b"})
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, Resources: []*v1.CatalogResource{{Kind: v1.KindPod, Uid: "a1", Namespace: "ns", Name: "a1"}, {Kind: v1.KindPod, Uid: "a2", Namespace: "ns", Name: "a2"}, {Kind: v1.KindPod, Uid: "a3", Namespace: "ns", Name: "a3"}}, ObservedUnixMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: 1000}); err == nil {
		t.Fatal("global live capacity accepted")
	}
	if resources, _ := catalog.Retained(); resources != 1 || len(catalog.CatalogPods("b", "", 0)) != 1 {
		t.Fatalf("failed stage leaked/changed B: retained=%d", resources)
	}
	applyCatalogSnapshot(t, catalog, "a", 2, 1, &v1.CatalogResource{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a"})
	// A full-size replacement is admitted while the old generation remains live.
	applyCatalogSnapshot(t, catalog, "a", 3, 1, &v1.CatalogResource{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a"})
	badCreate := &v1.CatalogChange{Epoch: 3, Seq: 2, Action: v1.CatalogAction_CATALOG_CREATED, Resource: &v1.CatalogResource{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a"}}
	if catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 3, Seq: 2, Type: v1.WatchFrameType_WATCH_DELTA, Change: badCreate, ObservedUnixMs: 1001}) == nil {
		t.Fatal("created existing accepted")
	}
	gap := &v1.CatalogChange{Epoch: 3, Seq: 3, Action: v1.CatalogAction_CATALOG_UPDATED, Resource: &v1.CatalogResource{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a"}}
	if catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 3, Seq: 3, Type: v1.WatchFrameType_WATCH_DELTA, Change: gap, ObservedUnixMs: 1001}) == nil {
		t.Fatal("gap accepted")
	}
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].UID != "a" {
		t.Fatalf("gap dropped last-good view: %v", got)
	}
}

func TestRemoteCatalogHeartbeatExpiryAndAtomicSameEpochRestore(t *testing.T) {
	catalog, err := NewRemoteCatalog([]string{"a", "b"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	applyCatalogSnapshot(t, catalog, "a", 1, 0, &v1.CatalogResource{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a", NodeName: "old"})
	applyCatalogSnapshot(t, catalog, "b", 1, 0, &v1.CatalogResource{Kind: v1.KindPod, Uid: "b", Namespace: "ns", Name: "b"})

	validHeartbeat := &v1.WatchFrame{ClusterId: "a", Epoch: 1, Seq: 0, Type: v1.WatchFrameType_WATCH_HEARTBEAT, ObservedUnixMs: 1001}
	if err = catalog.Apply(validHeartbeat); err != nil {
		t.Fatal(err)
	}
	for name, frame := range map[string]*v1.WatchFrame{
		"future sequence": {ClusterId: "a", Epoch: 1, Seq: 1, Type: v1.WatchFrameType_WATCH_HEARTBEAT, ObservedUnixMs: 1002},
		"wrong epoch":     {ClusterId: "a", Epoch: 2, Seq: 0, Type: v1.WatchFrameType_WATCH_HEARTBEAT, ObservedUnixMs: 1002},
		"zero timestamp":  {ClusterId: "a", Epoch: 1, Seq: 0, Type: v1.WatchFrameType_WATCH_HEARTBEAT},
		"old timestamp":   {ClusterId: "a", Epoch: 1, Seq: 0, Type: v1.WatchFrameType_WATCH_HEARTBEAT, ObservedUnixMs: 999},
	} {
		t.Run(name, func(t *testing.T) {
			if err := catalog.Apply(frame); err == nil {
				t.Fatal("invalid heartbeat accepted")
			}
		})
	}

	if err = catalog.Apply(&v1.WatchFrame{ClusterId: "a", Type: v1.WatchFrameType_WATCH_EXPIRED, ObservedUnixMs: 2000}); err != nil {
		t.Fatal(err)
	}
	if got := catalog.CatalogPods("a", "", 0); got != nil {
		t.Fatalf("expired catalog remained visible: %v", got)
	}
	if got := catalog.StreamEntityNamespaces("a"); len(got) != 0 {
		t.Fatalf("expired identities remained visible: %v", got)
	}
	if got := catalog.CatalogPods("b", "", 0); len(got) != 1 {
		t.Fatalf("A expiry affected B: %v", got)
	}
	if err = catalog.Apply(validHeartbeat); err == nil {
		t.Fatal("heartbeat revived expired catalog")
	}

	if err = catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Seq: 0, Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 3000}); err != nil {
		t.Fatal(err)
	}
	if err = catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Seq: 0, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, ObservedUnixMs: 3000, Resources: []*v1.CatalogResource{{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a", NodeName: "restored"}}}); err != nil {
		t.Fatal(err)
	}
	if got := catalog.CatalogPods("a", "", 0); got != nil {
		t.Fatalf("partial recovery became visible: %v", got)
	}
	if err = catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Seq: 0, Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: 3000}); err != nil {
		t.Fatal(err)
	}
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].Node != "restored" {
		t.Fatalf("same-epoch recovery failed: %v", got)
	}
}

func TestRemoteCatalogRejectsMalformedFramesAndResources(t *testing.T) {
	validPod := func() *v1.CatalogResource {
		return &v1.CatalogResource{Kind: v1.KindPod, Uid: "pod", Namespace: "ns", Name: "pod"}
	}
	tests := []struct {
		name  string
		frame *v1.WatchFrame
	}{
		{name: "unknown cluster", frame: &v1.WatchFrame{ClusterId: "unknown", Type: v1.WatchFrameType_WATCH_RESET}},
		{name: "unknown type", frame: &v1.WatchFrame{ClusterId: "a", Type: v1.WatchFrameType(99)}},
		{name: "reset payload", frame: &v1.WatchFrame{ClusterId: "a", Type: v1.WatchFrameType_WATCH_RESET, Resources: []*v1.CatalogResource{validPod()}}},
		{name: "begin resources", frame: &v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1, Resources: []*v1.CatalogResource{validPod()}}},
		{name: "begin zero epoch", frame: &v1.WatchFrame{ClusterId: "a", Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1}},
		{name: "chunk before begin", frame: &v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, ObservedUnixMs: 1, Resources: []*v1.CatalogResource{validPod()}}},
		{name: "commit before begin", frame: &v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: 1}},
		{name: "expiry payload", frame: &v1.WatchFrame{ClusterId: "a", Type: v1.WatchFrameType_WATCH_EXPIRED, ObservedUnixMs: 1, Change: &v1.CatalogChange{}}},
		{name: "delta before snapshot", frame: &v1.WatchFrame{ClusterId: "a", Epoch: 1, Seq: 1, Type: v1.WatchFrameType_WATCH_DELTA, ObservedUnixMs: 1, Change: &v1.CatalogChange{Epoch: 1, Seq: 1, Action: v1.CatalogAction_CATALOG_CREATED, Resource: validPod()}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog, err := NewRemoteCatalog([]string{"a"}, 10)
			if err != nil {
				t.Fatal(err)
			}
			if err := catalog.Apply(tt.frame); err == nil {
				t.Fatal("malformed frame accepted")
			}
			if resources, bytes := catalog.Retained(); resources != 0 || bytes != 0 {
				t.Fatalf("malformed frame retained state: resources=%d bytes=%d", resources, bytes)
			}
		})
	}

	resources := []*v1.CatalogResource{
		nil,
		{Kind: "Secret", Uid: "secret", Namespace: "ns", Name: "secret"},
		{Kind: v1.KindPod, Uid: strings.Repeat("u", 254), Namespace: "ns", Name: "pod"},
		{Kind: v1.KindPod, Uid: "pod", Name: "pod"},
		{Kind: v1.KindNode, Uid: "node", Namespace: "ns", Name: "node"},
		{Kind: v1.KindEvent, Uid: "event", Namespace: "ns", Name: "event", Owners: []*v1.CatalogOwner{{Kind: v1.KindDeployment, Uid: "owner", Name: "owner"}}},
		{Kind: v1.KindPod, Uid: "pod", Namespace: "ns", Name: "pod", Owners: []*v1.CatalogOwner{nil}},
		{Kind: v1.KindPod, Uid: "pod", Namespace: "ns", Name: "pod", Owners: []*v1.CatalogOwner{{Kind: v1.KindNode, Uid: "node", Name: "node"}}},
	}
	for i, resource := range resources {
		t.Run(fmt.Sprintf("invalid resource %d", i), func(t *testing.T) {
			catalog, _ := NewRemoteCatalog([]string{"a"}, 10)
			if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1}); err != nil {
				t.Fatal(err)
			}
			if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, ObservedUnixMs: 1, Resources: []*v1.CatalogResource{resource}}); err == nil {
				t.Fatal("invalid resource accepted")
			}
			if resources, bytes := catalog.Retained(); resources != 0 || bytes != 0 {
				t.Fatalf("rejected resource leaked: resources=%d bytes=%d", resources, bytes)
			}
		})
	}
}

func TestRemoteCatalogDeltaLifecycleIsAtomicAndImmutable(t *testing.T) {
	catalog, err := NewRemoteCatalog([]string{"a"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	applyCatalogSnapshot(t, catalog, "a", 1, 0)
	pod := &v1.CatalogResource{Kind: v1.KindPod, Uid: "pod", Namespace: "ns", Name: "created", NodeName: "node-a"}
	apply := func(seq uint64, action v1.CatalogAction, resource *v1.CatalogResource) {
		t.Helper()
		change := &v1.CatalogChange{Epoch: 1, Seq: seq, Action: action, Resource: resource}
		if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Seq: seq, Type: v1.WatchFrameType_WATCH_DELTA, ObservedUnixMs: int64(1000 + seq), Change: change}); err != nil {
			t.Fatal(err)
		}
	}
	apply(1, v1.CatalogAction_CATALOG_CREATED, pod)
	pod.Name, pod.NodeName = "caller-mutated", "caller-mutated"
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].Name != "created" || got[0].Node != "node-a" {
		t.Fatalf("caller mutation changed retained delta: %v", got)
	}
	updated := &v1.CatalogResource{Kind: v1.KindPod, Uid: "pod", Namespace: "ns", Name: "updated", NodeName: "node-b"}
	apply(2, v1.CatalogAction_CATALOG_UPDATED, updated)
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].Name != "updated" || got[0].Node != "node-b" {
		t.Fatalf("update was not atomic: %v", got)
	}
	apply(3, v1.CatalogAction_CATALOG_DELETED, updated)
	if got := catalog.CatalogPods("a", "", 0); len(got) != 0 {
		t.Fatalf("delete remained visible: %v", got)
	}
	if resources, bytes := catalog.Retained(); resources != 0 || bytes != 0 {
		t.Fatalf("delete did not release accounting: resources=%d bytes=%d", resources, bytes)
	}
}
