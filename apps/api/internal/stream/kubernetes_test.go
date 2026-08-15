package stream

import (
	"testing"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

func TestEnvelopeFromWatchFrameUsesSingleResetForBulkBoundaries(t *testing.T) {
	for _, typ := range []v1.WatchFrameType{
		v1.WatchFrameType_WATCH_EXPIRED,
		v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT,
	} {
		env, ok := EnvelopeFromWatchFrame(&v1.WatchFrame{ClusterId: "a", Type: typ, ObservedUnixMs: 1_000})
		if !ok || env.Kind != contract.StreamKindReset || env.Action != contract.StreamActionReset || env.ClusterID != "a" {
			t.Fatalf("type %s mapped to %+v ok=%v", typ, env, ok)
		}
	}
	for _, typ := range []v1.WatchFrameType{
		v1.WatchFrameType_WATCH_RESET,
		v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN,
		v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK,
		v1.WatchFrameType_WATCH_HEARTBEAT,
	} {
		if _, ok := EnvelopeFromWatchFrame(&v1.WatchFrame{ClusterId: "a", Type: typ, ObservedUnixMs: 1_000, Resources: []*v1.CatalogResource{{Kind: v1.KindPod, Uid: "p", Namespace: "ns", Name: "p"}}}); ok {
			t.Fatalf("bulk/heartbeat type %s expanded into SSE", typ)
		}
	}
}

func TestWatchSnapshotSequenceEmitsExactlyOneAtomicReset(t *testing.T) {
	sequences := map[string][]*v1.WatchFrame{
		"initial": {{ClusterId: "a", Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1}, {ClusterId: "a", Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: 2}},
		"resync":  {{ClusterId: "a", Type: v1.WatchFrameType_WATCH_RESET}, {ClusterId: "a", Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1}, {ClusterId: "a", Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, ObservedUnixMs: 1, Resources: make([]*v1.CatalogResource, 100_000)}, {ClusterId: "a", Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: 2}},
	}
	for name, frames := range sequences {
		count := 0
		for _, frame := range frames {
			if env, ok := EnvelopeFromWatchFrame(frame); ok {
				if env.Kind != contract.StreamKindReset {
					t.Fatalf("%s emitted non-reset %+v", name, env)
				}
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%s emitted %d reset events, want 1", name, count)
		}
	}
}

func TestEnvelopeFromWatchDeltaMapsOnlyExistingInvalidationKinds(t *testing.T) {
	tests := []struct {
		kind       string
		action     v1.CatalogAction
		wantKind   contract.StreamEventKind
		wantAction contract.StreamEventAction
	}{
		{v1.KindPod, v1.CatalogAction_CATALOG_CREATED, contract.StreamKindPod, contract.StreamActionAdded},
		{v1.KindDeployment, v1.CatalogAction_CATALOG_UPDATED, contract.StreamKindWorkload, contract.StreamActionUpdated},
		{v1.KindEvent, v1.CatalogAction_CATALOG_DELETED, contract.StreamKindKubeEvent, contract.StreamActionDeleted},
	}
	for _, tc := range tests {
		resource := &v1.CatalogResource{Kind: tc.kind, Uid: "uid", Namespace: "ns", Name: "name"}
		frame := &v1.WatchFrame{ClusterId: "a", Epoch: 1, Seq: 2, Type: v1.WatchFrameType_WATCH_DELTA, ObservedUnixMs: 1_000, Change: &v1.CatalogChange{Epoch: 1, Seq: 2, Action: tc.action, Resource: resource}}
		env, ok := EnvelopeFromWatchFrame(frame)
		if !ok || env.Kind != tc.wantKind || env.Action != tc.wantAction || env.ClusterID != "a" || env.Namespace != "ns" {
			t.Fatalf("%s/%s mapped to %+v ok=%v", tc.kind, tc.action, env, ok)
		}
		if tc.kind == v1.KindPod && (env.Entity == nil || env.Entity.PodUID != "uid" || env.Entity.PodName != "name") {
			t.Fatalf("pod identity lost: %+v", env.Entity)
		}
		if tc.kind == v1.KindDeployment && (env.Entity == nil || env.Entity.WorkloadUID != "uid" || env.Entity.WorkloadKind != v1.KindDeployment || env.Entity.WorkloadName != "name") {
			t.Fatalf("workload identity lost: %+v", env.Entity)
		}
	}
	for _, frame := range []*v1.WatchFrame{
		nil,
		{ClusterId: "a", Type: v1.WatchFrameType_WATCH_DELTA},
		{ClusterId: "a", Type: v1.WatchFrameType_WATCH_DELTA, Change: &v1.CatalogChange{Action: v1.CatalogAction_CATALOG_CREATED, Resource: &v1.CatalogResource{Kind: v1.KindReplicaSet, Uid: "rs", Namespace: "ns", Name: "rs"}}},
	} {
		if _, ok := EnvelopeFromWatchFrame(frame); ok {
			t.Fatalf("invalid/internal frame emitted SSE: %+v", frame)
		}
	}
}
