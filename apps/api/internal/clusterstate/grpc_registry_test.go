package clusterstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"
)

type queryReplyClient struct {
	v1.ClusterStateClient
	reply func(*v1.ScreenQuery) (*v1.ScreenReply, error)
}

func (c queryReplyClient) Query(_ context.Context, q *v1.ScreenQuery, _ ...grpc.CallOption) (*v1.ScreenReply, error) {
	return c.reply(proto.Clone(q).(*v1.ScreenQuery))
}

func TestGRPCRegistryReadinessTracksInfrastructureTCPNotClusterFreshness(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoff.Config{BaseDelay: 10 * time.Millisecond, Multiplier: 1.2, Jitter: 0, MaxDelay: 50 * time.Millisecond}, MinConnectTimeout: 100 * time.Millisecond}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := &GRPCRegistry{Client: v1.NewClusterStateClient(conn), Health: healthv1.NewHealthClient(conn)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.StartHealth(ctx, 20*time.Millisecond, 50*time.Millisecond); close(done) }()
	if r.Ready() {
		t.Fatal("initially ready before a health response")
	}
	server := serveHealth(t, listener)
	waitReady(t, r, true)
	// No cluster has a snapshot (equivalent to unavailable/expired), but central
	// infrastructure readiness must remain independent from cluster freshness.
	if _, err := r.ForScreen(context.Background(), ScreenRequest{ClusterID: "expired", Screen: "overview", Namespaces: NamespaceFilter{All: true}, From: time.Unix(1, 0), EventLimit: 50, UnhealthyLimit: 20}); err == nil || !r.Ready() {
		t.Fatalf("cluster availability changed infrastructure readiness: err=%v ready=%v", err, r.Ready())
	}
	server.Stop()
	waitReady(t, r, false)
	listener = listenSameAddress(t, address)
	server = serveHealth(t, listener)
	waitReady(t, r, true)
	server.Stop()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("health monitor leaked after cancellation")
	}
	if r.Ready() {
		t.Fatal("ready remained true after monitor shutdown")
	}
}

func listenSameAddress(t *testing.T, address string) net.Listener {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		listener, err := net.Listen("tcp", address)
		if err == nil {
			return listener
		}
		if time.Now().After(deadline) {
			t.Fatalf("rebind %s after server stop: %v", address, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func serveHealth(t *testing.T, listener net.Listener) *grpc.Server {
	t.Helper()
	s := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus(v1.ClusterState_ServiceDesc.ServiceName, healthv1.HealthCheckResponse_SERVING)
	healthv1.RegisterHealthServer(s, hs)
	go func() { _ = s.Serve(listener) }()
	return s
}

func waitReady(t *testing.T, r *GRPCRegistry, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.Ready() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ready=%v want=%v", r.Ready(), want)
}

func TestProjectionCanonicalShapeAndOrdering(t *testing.T) {
	req := ScreenRequest{ClusterID: "a", Screen: "overview", Namespaces: NamespaceFilter{All: true}, From: time.Unix(1, 0).UTC(), EventLimit: 50, UnhealthyLimit: 20}
	a := &v1.Resource{Kind: v1.KindEvent, Uid: "a", Name: "a", Namespace: "ns", Event: &v1.EventProjection{InvolvedUid: "p", InvolvedKind: "Pod", InvolvedName: "p", LastSeenUnixMs: 1000}}
	b := &v1.Resource{Kind: v1.KindEvent, Uid: "b", Name: "b", Namespace: "ns", Event: &v1.EventProjection{InvolvedUid: "p", InvolvedKind: "Pod", InvolvedName: "p", LastSeenUnixMs: 1000}}
	one, _ := ProjectScreen(req, map[string]*v1.Resource{"b": b, "a": a}, time.Unix(2, 0))
	two, _ := ProjectScreen(req, map[string]*v1.Resource{"a": a, "b": b}, time.Unix(2, 0))
	j1, _ := json.Marshal(one)
	j2, _ := json.Marshal(two)
	if !bytes.Equal(j1, j2) {
		t.Fatalf("non-deterministic canonical JSON\n%s\n%s", j1, j2)
	}
	if err := validateProjectionJSON(j1, "overview"); err != nil {
		t.Fatal(err)
	}
	var decoded ScreenProjection
	if err := json.Unmarshal(j1, &decoded); err != nil {
		t.Fatal(err)
	}
	round, _ := json.Marshal(decoded)
	if !bytes.Equal(j1, round) {
		t.Fatalf("roundtrip drift\n%s\n%s", j1, round)
	}
}

func TestProjectScreenCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resources := largeProjectionFixture()
	started := time.Now()
	_, err := ProjectScreenContext(ctx, screenFixtureRequest("overview"), resources, time.Unix(2, 0))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cancel release too slow: %v", elapsed)
	}
}

func TestProjectionSemanticShapeRejectsMutations(t *testing.T) {
	valid := []byte(`{"request":{},"nodes":{},"podsHealth":{},"workloadsHealth":{},"unhealthy":[],"events":[]}`)
	if err := validateProjectionJSON(valid, "overview"); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"missing":      []byte(`{"request":{},"nodes":{},"podsHealth":{},"workloadsHealth":{},"unhealthy":[]}`),
		"null_array":   []byte(`{"request":{},"nodes":{},"podsHealth":{},"workloadsHealth":{},"unhealthy":null,"events":[]}`),
		"wrong_screen": []byte(`{"request":{},"nodes":{},"podsHealth":{},"workloadsHealth":{},"unhealthy":[],"events":[],"pod":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if validateProjectionJSON(body, "overview") == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
	if rejectDuplicateKeys([]byte(`{"request":{},"request":{}}`)) == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestGRPCRegistryForScreenStrictReplyBoundary(t *testing.T) {
	req := ScreenRequest{ClusterID: "a", Screen: "overview", Namespaces: NamespaceFilter{List: []string{"z", "a"}}, From: time.UnixMilli(1234), EventLimit: 50, UnhealthyLimit: 20}
	normalized := req
	normalized.Namespaces.List = []string{"a", "z"}
	normalized.From = time.UnixMilli(1234).UTC()
	projection, err := ProjectScreen(normalized, map[string]*v1.Resource{}, time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	validReply := func(q *v1.ScreenQuery) *v1.ScreenReply {
		return &v1.ScreenReply{CanonicalJson: append([]byte(nil), canonical...), Accepted: proto.Clone(q).(*v1.ScreenQuery), ObservedUnixMs: time.Now().Add(-time.Second).UnixMilli(), Stale: true}
	}
	tests := []struct {
		name   string
		mutate func(*v1.ScreenReply) *v1.ScreenReply
	}{
		{name: "nil reply", mutate: func(*v1.ScreenReply) *v1.ScreenReply { return nil }},
		{name: "accepted mismatch", mutate: func(r *v1.ScreenReply) *v1.ScreenReply { r.Accepted.ClusterId = "b"; return r }},
		{name: "empty JSON", mutate: func(r *v1.ScreenReply) *v1.ScreenReply { r.CanonicalJson = nil; return r }},
		{name: "null JSON", mutate: func(r *v1.ScreenReply) *v1.ScreenReply { r.CanonicalJson = []byte("null"); return r }},
		{name: "zero observed", mutate: func(r *v1.ScreenReply) *v1.ScreenReply { r.ObservedUnixMs = 0; return r }},
		{name: "future observed", mutate: func(r *v1.ScreenReply) *v1.ScreenReply {
			r.ObservedUnixMs = time.Now().Add(2 * time.Minute).UnixMilli()
			return r
		}},
		{name: "duplicate key", mutate: func(r *v1.ScreenReply) *v1.ScreenReply {
			r.CanonicalJson = []byte(`{"request":{},"request":{}}`)
			return r
		}},
		{name: "missing required", mutate: func(r *v1.ScreenReply) *v1.ScreenReply { r.CanonicalJson = []byte(`{"request":{}}`); return r }},
		{name: "required null", mutate: func(r *v1.ScreenReply) *v1.ScreenReply {
			r.CanonicalJson = []byte(`{"request":{},"nodes":null,"podsHealth":{},"workloadsHealth":{},"unhealthy":[],"events":[]}`)
			return r
		}},
		{name: "unknown nested", mutate: func(r *v1.ScreenReply) *v1.ScreenReply {
			r.CanonicalJson = bytes.Replace(canonical, []byte(`"request":{`), []byte(`"request":{"unknown":1,`), 1)
			return r
		}},
		{name: "trailing JSON", mutate: func(r *v1.ScreenReply) *v1.ScreenReply {
			r.CanonicalJson = append(r.CanonicalJson, []byte(` {}`)...)
			return r
		}},
		{name: "request mismatch", mutate: func(r *v1.ScreenReply) *v1.ScreenReply {
			var changed ScreenProjection
			if err := json.Unmarshal(canonical, &changed); err != nil {
				panic(err)
			}
			changed.Request.ClusterID = "b"
			r.CanonicalJson, _ = json.Marshal(changed)
			return r
		}},
		{name: "non canonical", mutate: func(r *v1.ScreenReply) *v1.ScreenReply { r.CanonicalJson = append([]byte(" "), canonical...); return r }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := queryReplyClient{reply: func(q *v1.ScreenQuery) (*v1.ScreenReply, error) { return tt.mutate(validReply(q)), nil }}
			registry := &GRPCRegistry{Client: client, MaxReplyBytes: 4 << 20}
			registry.ready.Store(true)
			if _, err := registry.ForScreen(context.Background(), req); err == nil {
				t.Fatal("invalid registry reply accepted")
			}
		})
	}

	registry := &GRPCRegistry{Client: queryReplyClient{reply: func(q *v1.ScreenQuery) (*v1.ScreenReply, error) { return validReply(q), nil }}}
	registry.ready.Store(true)
	provider, err := registry.ForScreen(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	remote, ok := provider.(*RemoteProvider)
	if !ok || !remote.Stale || remote.HasSynced() || remote.ObservedAt().IsZero() {
		t.Fatalf("freshness was not preserved: provider=%T stale=%v synced=%v observed=%v", provider, remote.Stale, remote.HasSynced(), remote.ObservedAt())
	}

	for name, registry := range map[string]*GRPCRegistry{
		"not ready":   {Client: queryReplyClient{}},
		"query error": {Client: queryReplyClient{reply: func(*v1.ScreenQuery) (*v1.ScreenReply, error) { return nil, fmt.Errorf("unavailable") }}},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "query error" {
				registry.ready.Store(true)
			}
			if _, err := registry.ForScreen(context.Background(), req); err == nil {
				t.Fatal("failure path succeeded")
			}
		})
	}
}
