package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clusterstate "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type recordingWatchServer struct {
	ctx    context.Context
	cancel context.CancelFunc
	frames []*v1.WatchFrame
}

type channelWatchServer struct {
	ctx    context.Context
	frames chan *v1.WatchFrame
}

func (s *channelWatchServer) Send(frame *v1.WatchFrame) error { s.frames <- frame; return nil }
func (s *channelWatchServer) SetHeader(metadata.MD) error     { return nil }
func (s *channelWatchServer) SendHeader(metadata.MD) error    { return nil }
func (s *channelWatchServer) SetTrailer(metadata.MD)          {}
func (s *channelWatchServer) Context() context.Context        { return s.ctx }
func (s *channelWatchServer) SendMsg(any) error               { return nil }
func (s *channelWatchServer) RecvMsg(any) error               { return io.EOF }

func newRecordingWatchServer(ctx context.Context) *recordingWatchServer {
	ctx, cancel := context.WithCancel(ctx)
	return &recordingWatchServer{ctx: ctx, cancel: cancel}
}

func (s *recordingWatchServer) Send(frame *v1.WatchFrame) error {
	s.frames = append(s.frames, frame)
	if frame.Type == v1.WatchFrameType_WATCH_RESET || frame.Type == v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT {
		s.cancel()
	}
	return nil
}
func (s *recordingWatchServer) SetHeader(metadata.MD) error  { return nil }
func (s *recordingWatchServer) SendHeader(metadata.MD) error { return nil }
func (s *recordingWatchServer) SetTrailer(metadata.MD)       {}
func (s *recordingWatchServer) Context() context.Context     { return s.ctx }
func (s *recordingWatchServer) SendMsg(any) error            { return nil }
func (s *recordingWatchServer) RecvMsg(any) error            { return io.EOF }

type scriptedSyncStream struct {
	ctx     context.Context
	recv    []*v1.AgentFrame
	recvErr error
	sendErr error
	sent    []*v1.ServerFrame
}

type plainAuthInfo struct{}

func (plainAuthInfo) AuthType() string { return "plain" }

func (s *scriptedSyncStream) Recv() (*v1.AgentFrame, error) {
	if len(s.recv) == 0 {
		if s.recvErr != nil {
			return nil, s.recvErr
		}
		return nil, io.EOF
	}
	frame := s.recv[0]
	s.recv = s.recv[1:]
	return frame, nil
}
func (s *scriptedSyncStream) Send(frame *v1.ServerFrame) error {
	s.sent = append(s.sent, frame)
	return s.sendErr
}
func (s *scriptedSyncStream) SetHeader(metadata.MD) error  { return nil }
func (s *scriptedSyncStream) SendHeader(metadata.MD) error { return nil }
func (s *scriptedSyncStream) SetTrailer(metadata.MD)       {}
func (s *scriptedSyncStream) Context() context.Context     { return s.ctx }
func (s *scriptedSyncStream) SendMsg(any) error            { return nil }
func (s *scriptedSyncStream) RecvMsg(any) error            { return io.EOF }

func TestSyncAndQueryMapBoundaryFailuresWithoutPanics(t *testing.T) {
	agentCtx := agentRoleContext(context.Background(), "a")
	if err := (&Service{}).Sync(&scriptedSyncStream{ctx: agentCtx}); err == nil {
		t.Fatal("nil registry sync succeeded")
	}
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a"}
	r, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Registry: r, TrustDomain: "example.test"}
	if err := svc.Sync(&scriptedSyncStream{ctx: agentCtx}); err != nil {
		t.Fatalf("clean pre-session EOF=%v", err)
	}
	recvErr := errors.New("receive failed")
	if err := svc.Sync(&scriptedSyncStream{ctx: agentCtx, recvErr: recvErr}); !errors.Is(err, recvErr) {
		t.Fatalf("recv err=%v", err)
	}
	hello := &v1.AgentFrame{Frame: &v1.AgentFrame_Hello{Hello: &v1.Hello{ClusterId: "a", ProtocolVersion: v1.Version}}}
	if err := (&Service{Registry: r, TrustDomain: "example.test", MaxMessageBytes: 1}).Sync(&scriptedSyncStream{ctx: agentCtx, recv: []*v1.AgentFrame{hello}}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversize status=%v err=%v", status.Code(err), err)
	}
	if err := svc.Sync(&scriptedSyncStream{ctx: agentCtx, recv: []*v1.AgentFrame{{}}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("pre-hello empty status=%v err=%v", status.Code(err), err)
	}
	stream := &scriptedSyncStream{ctx: agentCtx, recv: []*v1.AgentFrame{hello, {}}}
	if err := svc.Sync(stream); status.Code(err) != codes.InvalidArgument || len(stream.sent) != 1 || stream.sent[0].GetAck() == nil {
		t.Fatalf("post-hello empty status=%v sent=%+v err=%v", status.Code(err), stream.sent, err)
	}
	sendErr := errors.New("send failed")
	if err := svc.Sync(&scriptedSyncStream{ctx: agentCtx, recv: []*v1.AgentFrame{hello}, sendErr: sendErr}); !errors.Is(err, sendErr) {
		t.Fatalf("send err=%v", err)
	}

	apiCtx := apiRoleContext(context.Background())
	query := &v1.ScreenQuery{ClusterId: "a", Screen: "overview", Scope: &v1.NamespaceScope{All: true}, EventLimit: 50, UnhealthyLimit: 20}
	if _, err := (&Service{TrustDomain: "example.test"}).Query(apiCtx, query); status.Code(err) != codes.Unavailable {
		t.Fatalf("nil registry query status=%v err=%v", status.Code(err), err)
	}
	if _, err := svc.Query(context.Background(), query); err == nil {
		t.Fatal("query without API identity succeeded")
	}
	plainCtx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: plainAuthInfo{}})
	if _, err := svc.Query(plainCtx, query); err == nil {
		t.Fatal("query without TLS auth info succeeded")
	}
	if _, err := svc.Query(apiCtx, nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil query status=%v err=%v", status.Code(err), err)
	}
	if _, err := svc.Query(apiCtx, query); status.Code(err) != codes.Unavailable {
		t.Fatalf("unavailable query status=%v err=%v", status.Code(err), err)
	}
}

func TestWatchRejectsAuthShapeUnavailableAndOversizedFrames(t *testing.T) {
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a"}
	r, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Registry: r, TrustDomain: "example.test"}
	valid := &v1.WatchRequest{ProtocolVersion: v1.Version, ClusterIds: []string{"a"}}
	if err := svc.Watch(valid, newRecordingWatchServer(context.Background())); err == nil {
		t.Fatal("watch without API mTLS identity succeeded")
	}
	if err := (&Service{TrustDomain: "example.test"}).Watch(valid, newRecordingWatchServer(apiRoleContext(context.Background()))); status.Code(err) != codes.Unavailable {
		t.Fatalf("nil registry watch status=%v err=%v", status.Code(err), err)
	}
	for name, req := range map[string]*v1.WatchRequest{
		"nil":         nil,
		"version":     {ProtocolVersion: v1.Version + 1, ClusterIds: []string{"a"}},
		"empty":       {ProtocolVersion: v1.Version},
		"many":        {ProtocolVersion: v1.Version, ClusterIds: []string{"a", "b"}},
		"invalid id":  {ProtocolVersion: v1.Version, ClusterIds: []string{"A"}},
		"not allowed": {ProtocolVersion: v1.Version, ClusterIds: []string{"b"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := svc.Watch(req, newRecordingWatchServer(apiRoleContext(context.Background())))
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("status=%v err=%v", status.Code(err), err)
			}
		})
	}

	// A configured cluster with no committed state receives one bounded reset;
	// canceling the stream must release the subscription immediately.
	stream := newRecordingWatchServer(apiRoleContext(context.Background()))
	if err := svc.Watch(valid, stream); status.Code(err) != codes.Canceled {
		t.Fatalf("unavailable watch status=%v err=%v", status.Code(err), err)
	}
	if len(stream.frames) != 1 || stream.frames[0].Type != v1.WatchFrameType_WATCH_RESET {
		t.Fatalf("unavailable frames=%+v", stream.frames)
	}

	seedRegistry(t, r, "a", []*v1.Resource{{Kind: v1.KindPod, Uid: "p", Namespace: "ns", Name: "p", Pod: &v1.PodProjection{Phase: "Running"}}})
	tooSmall := &Service{Registry: r, TrustDomain: "example.test", MaxMessageBytes: 1}
	if err := tooSmall.Watch(valid, newRecordingWatchServer(apiRoleContext(context.Background()))); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized snapshot status=%v err=%v", status.Code(err), err)
	}
	stream = newRecordingWatchServer(apiRoleContext(context.Background()))
	if err := svc.Watch(valid, stream); status.Code(err) != codes.Canceled {
		t.Fatalf("snapshot watch status=%v err=%v", status.Code(err), err)
	}
	want := []v1.WatchFrameType{v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT}
	if len(stream.frames) != len(want) {
		t.Fatalf("snapshot frames=%+v", stream.frames)
	}
	for i := range want {
		if stream.frames[i].Type != want[i] || stream.frames[i].ClusterId != "a" {
			t.Fatalf("frame[%d]=%+v", i, stream.frames[i])
		}
	}
}

func TestWatchPublishesHeartbeatDeltaAtomicResetAndExpiry(t *testing.T) {
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a"}
	limits.HeartbeatTimeout = time.Second
	limits.StaleTTL = time.Second
	r, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000, 0)
	r.SetClock(func() time.Time { return now })
	generation, err := r.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: v1.Version}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.BeginSession("a", generation, &v1.BeginSnapshot{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.ChunkSession("a", generation, &v1.SnapshotChunk{Resources: []*v1.Resource{{Kind: v1.KindPod, Uid: "p", Namespace: "ns", Name: "p", Pod: &v1.PodProjection{Phase: "Running"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.CommitSession("a", generation, &v1.CommitSnapshot{Epoch: 1}); nack != nil {
		t.Fatal(nack)
	}
	ctx, cancel := context.WithCancel(apiRoleContext(context.Background()))
	stream := &channelWatchServer{ctx: ctx, frames: make(chan *v1.WatchFrame, 16)}
	done := make(chan error, 1)
	go func() {
		done <- (&Service{Registry: r, TrustDomain: "example.test"}).Watch(&v1.WatchRequest{ProtocolVersion: v1.Version, ClusterIds: []string{"a"}}, stream)
	}()
	next := func(want v1.WatchFrameType) *v1.WatchFrame {
		t.Helper()
		select {
		case frame := <-stream.frames:
			if frame.Type != want {
				t.Fatalf("frame=%s want=%s", frame.Type, want)
			}
			return frame
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
			return nil
		}
	}
	next(v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN)
	next(v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK)
	next(v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT)
	if _, nack := r.Heartbeat("a", generation, &v1.Heartbeat{Epoch: 1}); nack != nil {
		t.Fatal(nack)
	}
	next(v1.WatchFrameType_WATCH_HEARTBEAT)
	if _, nack := r.DeltaSession("a", generation, &v1.Delta{Epoch: 1, Seq: 1, Resource: &v1.Resource{Kind: v1.KindPod, Uid: "p", Namespace: "ns", Name: "p", Pod: &v1.PodProjection{Phase: "Pending"}}}); nack != nil {
		t.Fatal(nack)
	}
	if frame := next(v1.WatchFrameType_WATCH_DELTA); frame.Change == nil || frame.Change.Resource == nil || frame.Change.Resource.Uid != "p" || frame.Change.Seq != 1 {
		t.Fatalf("delta lost closed catalog identity or sequence: %+v", frame)
	}
	if err := r.BeginSession("a", generation, &v1.BeginSnapshot{Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	if err := r.ChunkSession("a", generation, &v1.SnapshotChunk{Resources: []*v1.Resource{{Kind: v1.KindPod, Uid: "q", Namespace: "ns", Name: "q", Pod: &v1.PodProjection{Phase: "Running"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.CommitSession("a", generation, &v1.CommitSnapshot{Epoch: 2}); nack != nil {
		t.Fatal(nack)
	}
	next(v1.WatchFrameType_WATCH_RESET)
	next(v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN)
	next(v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK)
	next(v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT)
	r.DisconnectSession("a", generation)
	now = now.Add(3 * time.Second)
	if pruned := r.PruneExpired(); pruned != 1 {
		t.Fatalf("pruned=%d", pruned)
	}
	next(v1.WatchFrameType_WATCH_EXPIRED)
	cancel()
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Fatalf("watch cancel status=%v err=%v", status.Code(err), err)
	}
}

func TestStartWatchReloadsCAAndClientCertificateOnRealTCPReconnect(t *testing.T) {
	d := t.TempDir()
	ca, caKey := makeCA(t)
	caPath := writePEM(t, d, "ca.pem", "CERTIFICATE", ca.Raw)
	sc, sk := makeLeaf(t, ca, caKey, "registry", "")
	serverFiles := TLSFiles{CertFile: writeCert(t, d, "server", sc, sk), KeyFile: filepath.Join(d, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	apiCert, apiKey := makeLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-api/api-1")
	apiFiles := TLSFiles{CertFile: writeCert(t, d, "api", apiCert, apiKey), KeyFile: filepath.Join(d, "api.key"), CAFile: caPath, TrustDomain: "example.test"}
	oldClientTLS, _ := ClientTLS(apiFiles, "registry")
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a"}
	r, _ := registry.New(limits)
	seedRegistry(t, r, "a", []*v1.Resource{{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a", Pod: &v1.PodProjection{NodeName: "old"}}})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	serve := func(listener net.Listener) *grpc.Server {
		tlsConfig, err := ServerTLSForRole(serverFiles, "cluster-state-api")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.MaxRecvMsgSize(4<<20))
		v1.RegisterClusterStateServer(server, &Service{Registry: r, TrustDomain: "example.test"})
		go func() { _ = server.Serve(listener) }()
		return server
	}
	server := serve(listener)
	catalog, _ := clusterstate.NewRemoteCatalog([]string{"a"}, 100)
	var dials atomic.Int32
	remote := &clusterstate.GRPCRegistry{WatchDelay: func(time.Duration) time.Duration { return 10 * time.Millisecond }}
	remote.WatchDial = func(context.Context) (v1.ClusterStateClient, io.Closer, error) {
		dials.Add(1)
		clientTLS, err := ClientTLS(apiFiles, "registry")
		if err != nil {
			return nil, nil, err
		}
		conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4<<20)))
		if err != nil {
			return nil, nil, err
		}
		return v1.NewClusterStateClient(conn), conn, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = remote.StartWatch(ctx, []string{"a"}, catalog, nil); close(done) }()
	waitCatalogNode := func(want string) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			pods := catalog.CatalogPods("a", "", 0)
			if len(pods) == 1 && pods[0].Node == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("catalog did not reach node %q: %v", want, catalog.CatalogPods("a", "", 0))
	}
	waitCatalogNode("old")
	server.Stop()
	if pods := catalog.CatalogPods("a", "", 0); len(pods) != 1 || pods[0].Node != "old" {
		t.Fatalf("disconnect dropped last-good: %v", pods)
	}
	ca2, key2 := makeCA(t)
	writePEM(t, d, "ca.pem", "CERTIFICATE", ca2.Raw)
	sc2, sk2 := makeLeaf(t, ca2, key2, "registry", "")
	writeCert(t, d, "server", sc2, sk2)
	api2, apiKey2 := makeLeaf(t, ca2, key2, "", "spiffe://example.test/cluster-state-api/api-1")
	writeCert(t, d, "api", api2, apiKey2)
	newServerTLS, _ := ServerTLSForRole(serverFiles, "cluster-state-api")
	if err := actualHandshake(newServerTLS, oldClientTLS); err == nil {
		t.Fatal("old CA/client leaf accepted after rotation")
	}
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server = serve(listener)
	defer server.Stop()
	if _, nack := r.Delta("a", &v1.Delta{Epoch: 1, Seq: 1, Resource: &v1.Resource{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a", Pod: &v1.PodProjection{NodeName: "rotated"}}}); nack != nil {
		t.Fatal(nack)
	}
	waitCatalogNode("rotated")
	if dials.Load() < 2 {
		t.Fatalf("TLS files were not redialed: %d", dials.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch did not stop")
	}
}

func TestQueryCancellationReleasesClusterAndIsolatesOtherClusters(t *testing.T) {
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a", "b"}
	r, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, r, "a", largeTransportFixture(100_000))
	seedRegistry(t, r, "b", largeTransportFixture(1))
	svc := &Service{Registry: r, TrustDomain: "example.test"}
	q := &v1.ScreenQuery{ClusterId: "a", Screen: "overview", Scope: &v1.NamespaceScope{All: true}, EventLimit: 50, UnhealthyLimit: 20}
	ctx, cancel := context.WithCancel(apiRoleContext(context.Background()))
	done := make(chan error, 1)
	go func() { _, e := svc.Query(ctx, q); done <- e }()
	time.Sleep(2 * time.Millisecond)
	cancel()
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Fatalf("cancel status=%v err=%v", status.Code(err), err)
	}
	started := time.Now()
	if _, nack := r.Delta("a", &v1.Delta{Epoch: 1, Seq: 1, Resource: &v1.Resource{Kind: v1.KindPod, Uid: "p-000000", Namespace: "ns", Name: "changed", Pod: &v1.PodProjection{Phase: "Running"}}}); nack != nil {
		t.Fatalf("A lock remained blocked: %v", nack)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("A lock release=%v", elapsed)
	}
	started = time.Now()
	if _, err := svc.Query(apiRoleContext(context.Background()), &v1.ScreenQuery{ClusterId: "b", Screen: "overview", Scope: &v1.NamespaceScope{All: true}, EventLimit: 50, UnhealthyLimit: 20}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("B query delayed by A: %v", elapsed)
	}
	deadline, stop := context.WithDeadline(apiRoleContext(context.Background()), time.Now().Add(-time.Millisecond))
	defer stop()
	if _, err := svc.Query(deadline, q); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("deadline status=%v err=%v", status.Code(err), err)
	}
}

func TestActualWatchSkipsSnapshotDuplicateAndIsolatesReset(t *testing.T) {
	d := t.TempDir()
	ca, caKey := makeCA(t)
	caPath := writePEM(t, d, "ca.pem", "CERTIFICATE", ca.Raw)
	sc, sk := makeLeaf(t, ca, caKey, "registry", "")
	serverFiles := TLSFiles{CertFile: writeCert(t, d, "server", sc, sk), KeyFile: filepath.Join(d, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	apiCert, apiKey := makeLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-api/api-1")
	apiFiles := TLSFiles{CertFile: writeCert(t, d, "api", apiCert, apiKey), KeyFile: filepath.Join(d, "api.key"), CAFile: caPath, TrustDomain: "example.test"}
	st, _ := ServerTLSForRole(serverFiles, "cluster-state-api")
	ct, _ := ClientTLS(apiFiles, "registry")
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a", "b"}
	r, _ := registry.New(limits)
	seedRegistry(t, r, "a", []*v1.Resource{{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a", Pod: &v1.PodProjection{NodeName: "n"}}})
	seedRegistry(t, r, "b", []*v1.Resource{{Kind: v1.KindPod, Uid: "b", Namespace: "ns", Name: "b", Pod: &v1.PodProjection{NodeName: "n"}}})
	var once sync.Once
	svc := &Service{Registry: r, TrustDomain: "example.test", watchSubscribed: func(id string) {
		if id == "a" {
			once.Do(func() {
				if _, nack := r.Delta("a", &v1.Delta{Epoch: 1, Seq: 1, Resource: &v1.Resource{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a", Pod: &v1.PodProjection{NodeName: "n2"}}}); nack != nil {
					t.Error(nack)
				}
			})
		}
	}}
	client, stop := dialBufService(t, st, ct, svc)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	aw, err := client.Watch(ctx, &v1.WatchRequest{ClusterIds: []string{"a"}, ProtocolVersion: v1.Version})
	if err != nil {
		t.Fatal(err)
	}
	var commit *v1.WatchFrame
	for commit == nil {
		frame, err := aw.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if frame.ClusterId != "a" {
			t.Fatal("cross-cluster frame")
		}
		if frame.Type == v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT {
			commit = frame
		}
	}
	if commit.Seq != 1 {
		t.Fatalf("snapshot did not include raced delta: seq=%d", commit.Seq)
	}
	if _, nack := r.Delta("a", &v1.Delta{Epoch: 1, Seq: 2, Resource: &v1.Resource{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a", Pod: &v1.PodProjection{NodeName: "n3"}}}); nack != nil {
		t.Fatal(nack)
	}
	next, err := aw.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if next.Type != v1.WatchFrameType_WATCH_DELTA || next.Seq != 2 {
		t.Fatalf("queued duplicate was emitted: %+v", next)
	}
	bw, err := client.Watch(ctx, &v1.WatchRequest{ClusterIds: []string{"b"}, ProtocolVersion: v1.Version})
	if err != nil {
		t.Fatal(err)
	}
	for {
		frame, err := bw.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT {
			break
		}
	}
	_ = r.Begin("a", &v1.BeginSnapshot{Epoch: 2, BaseSeq: 2})
	_ = r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a", Pod: &v1.PodProjection{NodeName: "n4"}}}})
	if _, nack := r.Commit("a", &v1.CommitSnapshot{Epoch: 2}); nack != nil {
		t.Fatal(nack)
	}
	if _, nack := r.Delta("b", &v1.Delta{Epoch: 1, Seq: 1, Resource: &v1.Resource{Kind: v1.KindPod, Uid: "b", Namespace: "ns", Name: "b", Pod: &v1.PodProjection{NodeName: "n2"}}}); nack != nil {
		t.Fatal(nack)
	}
	started := time.Now()
	bdelta, err := bw.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if bdelta.Type != v1.WatchFrameType_WATCH_DELTA || bdelta.Seq != 1 || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("A reset delayed B: %+v", bdelta)
	}
	areset, err := aw.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if areset.Type != v1.WatchFrameType_WATCH_RESET {
		t.Fatalf("reset=%+v", areset)
	}
}

func TestActualWatchExpiryAndSameEpochSnapshotRestoresCatalog(t *testing.T) {
	d := t.TempDir()
	ca, caKey := makeCA(t)
	caPath := writePEM(t, d, "ca.pem", "CERTIFICATE", ca.Raw)
	serverCert, serverKey := makeLeaf(t, ca, caKey, "registry", "")
	serverTLS, err := ServerTLSForRole(TLSFiles{CertFile: writeCert(t, d, "server", serverCert, serverKey), KeyFile: filepath.Join(d, "server.key"), CAFile: caPath, TrustDomain: "example.test"}, "cluster-state-api")
	if err != nil {
		t.Fatal(err)
	}
	apiCert, apiKey := makeLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-api/api-1")
	clientTLS, err := ClientTLS(TLSFiles{CertFile: writeCert(t, d, "api", apiCert, apiKey), KeyFile: filepath.Join(d, "api.key"), CAFile: caPath, TrustDomain: "example.test"}, "registry")
	if err != nil {
		t.Fatal(err)
	}
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a", "b"}
	limits.HeartbeatTimeout = 10 * time.Second
	limits.StaleTTL = 20 * time.Second
	r, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	r.SetClock(func() time.Time { return now })
	seedRegistry(t, r, "a", []*v1.Resource{{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a", Pod: &v1.PodProjection{NodeName: "old"}}})
	seedRegistry(t, r, "b", []*v1.Resource{{Kind: v1.KindPod, Uid: "b", Namespace: "ns", Name: "b", Pod: &v1.PodProjection{NodeName: "stable"}}})
	client, stop := dialBufService(t, serverTLS, clientTLS, &Service{Registry: r, TrustDomain: "example.test"})
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Watch(ctx, &v1.WatchRequest{ClusterIds: []string{"a"}, ProtocolVersion: v1.Version})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := clusterstate.NewRemoteCatalog([]string{"a", "b"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	recvUntil := func(w v1.ClusterState_WatchClient, want v1.WatchFrameType) {
		t.Helper()
		for {
			frame, recvErr := w.Recv()
			if recvErr != nil {
				t.Fatal(recvErr)
			}
			if applyErr := catalog.Apply(frame); applyErr != nil {
				t.Fatal(applyErr)
			}
			if frame.Type == want {
				return
			}
		}
	}
	recvUntil(stream, v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT)
	bStream, err := client.Watch(ctx, &v1.WatchRequest{ClusterIds: []string{"b"}, ProtocolVersion: v1.Version})
	if err != nil {
		t.Fatal(err)
	}
	recvUntil(bStream, v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT)
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].Node != "old" {
		t.Fatalf("initial catalog=%v", got)
	}

	now = now.Add(31 * time.Second)
	if err = r.Connect(&v1.Hello{ClusterId: "b", ProtocolVersion: v1.Version}, "b"); err != nil {
		t.Fatal(err)
	}
	if got := r.PruneExpired(); got != 1 {
		t.Fatalf("pruned=%d", got)
	}
	recvUntil(stream, v1.WatchFrameType_WATCH_EXPIRED)
	if got := catalog.CatalogPods("a", "", 0); got != nil {
		t.Fatalf("expired catalog=%v", got)
	}
	if err = r.Begin("a", &v1.BeginSnapshot{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err = r.Chunk("a", &v1.SnapshotChunk{Resources: []*v1.Resource{{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a", Pod: &v1.PodProjection{NodeName: "restored"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := r.Commit("a", &v1.CommitSnapshot{Epoch: 1}); nack != nil {
		t.Fatal(nack)
	}
	recvUntil(stream, v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT)
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].Node != "restored" {
		t.Fatalf("same epoch did not restore catalog: %v", got)
	}
	if got := catalog.CatalogPods("b", "", 0); len(got) != 1 || got[0].Node != "stable" {
		t.Fatalf("A expiry changed B: %v", got)
	}
}

func TestActualSlowWatchOverflowReconnectsOnlyNoisyCluster(t *testing.T) {
	d := t.TempDir()
	ca, caKey := makeCA(t)
	caPath := writePEM(t, d, "ca.pem", "CERTIFICATE", ca.Raw)
	sc, sk := makeLeaf(t, ca, caKey, "registry", "")
	serverFiles := TLSFiles{CertFile: writeCert(t, d, "server", sc, sk), KeyFile: filepath.Join(d, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	apiCert, apiKey := makeLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-api/api-1")
	apiFiles := TLSFiles{CertFile: writeCert(t, d, "api", apiCert, apiKey), KeyFile: filepath.Join(d, "api.key"), CAFile: caPath, TrustDomain: "example.test"}
	st, _ := ServerTLSForRole(serverFiles, "cluster-state-api")
	ct, _ := ClientTLS(apiFiles, "registry")
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a", "b"}
	limits.WatchQueueFrames = 1
	limits.WatchQueueBytes = 1024
	limits.WatchTotalQueueBytes = 2048
	r, _ := registry.New(limits)
	longName := strings.Repeat("x", 253)
	for _, id := range []string{"a", "b"} {
		seedRegistry(t, r, id, []*v1.Resource{{Kind: v1.KindPod, Uid: id, Namespace: "ns", Name: longName, Pod: &v1.PodProjection{NodeName: "old"}}})
	}
	client, stop := dialBufService(t, st, ct, &Service{Registry: r, TrustDomain: "example.test"})
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	aw, _ := client.Watch(ctx, &v1.WatchRequest{ClusterIds: []string{"a"}, ProtocolVersion: v1.Version})
	bw, _ := client.Watch(ctx, &v1.WatchRequest{ClusterIds: []string{"b"}, ProtocolVersion: v1.Version})
	catalog, _ := clusterstate.NewRemoteCatalog([]string{"a"}, 100)
	recvCommit := func(stream v1.ClusterState_WatchClient, apply bool) *v1.WatchFrame {
		for {
			f, e := stream.Recv()
			if e != nil {
				t.Fatal(e)
			}
			if apply {
				if e = catalog.Apply(f); e != nil {
					t.Fatal(e)
				}
			}
			if f.Type == v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT {
				return f
			}
		}
	}
	recvCommit(aw, true)
	recvCommit(bw, false)
	for seq := uint64(1); seq <= 10_000; seq++ {
		node := fmt.Sprintf("%0250d", seq)
		if _, n := r.Delta("a", &v1.Delta{Epoch: 1, Seq: seq, Resource: &v1.Resource{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: longName, Pod: &v1.PodProjection{NodeName: node}}}); n != nil {
			t.Fatal(n)
		}
		if r.WatcherCount() == 1 {
			break
		}
	}
	if r.WatcherCount() != 1 {
		t.Fatal("slow A watcher did not overflow")
	}
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].Node != "old" {
		t.Fatalf("overflow dropped old live: %v", got)
	}
	if _, n := r.Delta("b", &v1.Delta{Epoch: 1, Seq: 1, Resource: &v1.Resource{Kind: v1.KindPod, Uid: "b", Namespace: "ns", Name: longName, Pod: &v1.PodProjection{NodeName: "new"}}}); n != nil {
		t.Fatal(n)
	}
	started := time.Now()
	bf, e := bw.Recv()
	if e != nil || bf.Type != v1.WatchFrameType_WATCH_DELTA || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("B delayed frame=%v err=%v", bf, e)
	}
	for {
		_, e = aw.Recv()
		if e != nil {
			break
		}
	}
	if status.Code(e) != codes.ResourceExhausted {
		t.Fatalf("A overflow status=%v err=%v", status.Code(e), e)
	}
	aw, _ = client.Watch(ctx, &v1.WatchRequest{ClusterIds: []string{"a"}, ProtocolVersion: v1.Version})
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Type: v1.WatchFrameType_WATCH_RESET}); err != nil {
		t.Fatal(err)
	}
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].Node != "old" {
		t.Fatalf("old live not retained during resync: %v", got)
	}
	commit := recvCommit(aw, true)
	if commit.Seq == 0 {
		t.Fatal("reconnect snapshot not current")
	}
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].Node == "old" {
		t.Fatalf("snapshot did not swap: %v", got)
	}
}

func seedRegistry(t *testing.T, r *registry.Registry, id string, resources []*v1.Resource) {
	t.Helper()
	generation, err := r.OpenSession(&v1.Hello{ClusterId: id, ProtocolVersion: 1}, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.BeginSession(id, generation, &v1.BeginSnapshot{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	for start := 0; start < len(resources); start += 1000 {
		end := start + 1000
		if end > len(resources) {
			end = len(resources)
		}
		if err = r.ChunkSession(id, generation, &v1.SnapshotChunk{Resources: resources[start:end]}); err != nil {
			t.Fatal(err)
		}
	}
	if _, nack := r.CommitSession(id, generation, &v1.CommitSnapshot{Epoch: 1}); nack != nil {
		t.Fatalf("commit: %v", nack)
	}
}

func largeTransportFixture(n int) []*v1.Resource {
	out := make([]*v1.Resource, n)
	for i := range out {
		id := fmt.Sprintf("p-%06d", i)
		out[i] = &v1.Resource{Kind: v1.KindPod, Uid: id, Namespace: "ns", Name: id, Pod: &v1.PodProjection{Phase: "Running", CreatedUnixMs: 1}}
	}
	return out
}

func apiRoleContext(ctx context.Context) context.Context {
	u, _ := url.Parse("spiffe://example.test/cluster-state-api/api-1")
	leaf := &x509.Certificate{URIs: []*url.URL{u}}
	return peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}}}})
}

func agentRoleContext(ctx context.Context, clusterID string) context.Context {
	u, _ := url.Parse("spiffe://example.test/cluster-state-agent/" + clusterID)
	leaf := &x509.Certificate{URIs: []*url.URL{u}}
	return peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}}}})
}

func TestActualMTLSGRPCSnapshotAndDelta(t *testing.T) {
	d := t.TempDir()
	ca, caKey := makeCA(t)
	caPath := writePEM(t, d, "ca.pem", "CERTIFICATE", ca.Raw)
	serverCert, serverKey := makeLeaf(t, ca, caKey, "registry", "")
	server := TLSFiles{CertFile: writeCert(t, d, "server", serverCert, serverKey), KeyFile: filepath.Join(d, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	agentCert, agentKey := makeLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-agent/a")
	agent := TLSFiles{CertFile: writeCert(t, d, "agent", agentCert, agentKey), KeyFile: filepath.Join(d, "agent.key"), CAFile: caPath, TrustDomain: "example.test"}
	stls, e := ServerTLS(server)
	if e != nil {
		t.Fatal(e)
	}
	ctls, e := ClientTLS(agent, "registry")
	if e != nil {
		t.Fatal(e)
	}
	r, e := registry.New(registry.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(stls)), grpc.MaxRecvMsgSize(4<<20))
	v1.RegisterClusterStateServer(gs, &Service{Registry: r, TrustDomain: "example.test"})
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cc, e := grpc.NewClient("passthrough:///registry", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(credentials.NewTLS(ctls)))
	if e != nil {
		t.Fatal(e)
	}
	defer cc.Close()
	sync, e := v1.NewClusterStateClient(cc).Sync(ctx)
	if e != nil {
		t.Fatal(e)
	}
	frames := []*v1.AgentFrame{{Frame: &v1.AgentFrame_Hello{Hello: &v1.Hello{ClusterId: "a", ProtocolVersion: 1}}}, {Frame: &v1.AgentFrame_BeginSnapshot{BeginSnapshot: &v1.BeginSnapshot{Epoch: 1, BaseSeq: 10}}}, {Frame: &v1.AgentFrame_SnapshotChunk{SnapshotChunk: &v1.SnapshotChunk{Resources: []*v1.Resource{{Kind: v1.KindPod, Uid: "p1", Namespace: "ns", Name: "p1", Pod: &v1.PodProjection{Phase: "Running"}}}}}}, {Frame: &v1.AgentFrame_Delta{Delta: &v1.Delta{Epoch: 1, Seq: 11, Resource: &v1.Resource{Kind: v1.KindPod, Uid: "p2", Namespace: "ns", Name: "p2", Pod: &v1.PodProjection{Phase: "Running"}}}}}, {Frame: &v1.AgentFrame_CommitSnapshot{CommitSnapshot: &v1.CommitSnapshot{Epoch: 1}}}}
	for _, f := range frames {
		if e = sync.Send(f); e != nil {
			t.Fatal(e)
		}
		if _, e = sync.Recv(); e != nil {
			t.Fatal(e)
		}
	}
	s, _, e := r.Snapshot("a")
	if e != nil || len(s.Resources) != 2 || s.Seq != 11 {
		t.Fatalf("snapshot=%v err=%v", s, e)
	}

	assertStatus := func(name string, frames []*v1.AgentFrame, want codes.Code) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			stream, err := v1.NewClusterStateClient(cc).Sync(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for i, frame := range frames {
				if err = stream.Send(frame); err != nil {
					break
				}
				_, err = stream.Recv()
				if i < len(frames)-1 && err != nil {
					t.Fatalf("frame %d failed early: %v", i, err)
				}
			}
			if status.Code(err) != want {
				t.Fatalf("status=%v want=%v err=%v", status.Code(err), want, err)
			}
		})
	}
	hello := func(cluster string) *v1.AgentFrame {
		return &v1.AgentFrame{Frame: &v1.AgentFrame_Hello{Hello: &v1.Hello{ClusterId: cluster, ProtocolVersion: v1.Version}}}
	}
	assertStatus("frame before hello", []*v1.AgentFrame{{Frame: &v1.AgentFrame_BeginSnapshot{BeginSnapshot: &v1.BeginSnapshot{Epoch: 1}}}}, codes.FailedPrecondition)
	assertStatus("identity mismatch", []*v1.AgentFrame{hello("b")}, codes.PermissionDenied)
	assertStatus("duplicate hello", []*v1.AgentFrame{hello("a"), hello("a")}, codes.InvalidArgument)
	assertStatus("chunk before begin", []*v1.AgentFrame{hello("a"), {Frame: &v1.AgentFrame_SnapshotChunk{SnapshotChunk: &v1.SnapshotChunk{Resources: []*v1.Resource{podResource("p")}}}}}, codes.FailedPrecondition)
	assertStatus("zero epoch", []*v1.AgentFrame{hello("a"), {Frame: &v1.AgentFrame_BeginSnapshot{BeginSnapshot: &v1.BeginSnapshot{}}}}, codes.FailedPrecondition)

	duplicateStream, err := v1.NewClusterStateClient(cc).Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sendRecv(t, duplicateStream, hello("a"))
	if err = duplicateStream.Send(&v1.AgentFrame{Frame: &v1.AgentFrame_Delta{Delta: &v1.Delta{Epoch: 1, Seq: 1, Resource: podResource("p")}}}); err != nil {
		t.Fatal(err)
	}
	reply, err := duplicateStream.Recv()
	if err != nil || !reply.GetAck().GetDuplicate() || reply.GetAck().GetAppliedSeq() != 11 {
		t.Fatalf("duplicate delta reply=%v err=%v", reply, err)
	}
}

func podResource(uid string) *v1.Resource {
	return &v1.Resource{Kind: v1.KindPod, Uid: uid, Namespace: "ns", Name: uid, Pod: &v1.PodProjection{Phase: "Running"}}
}

func TestActualTLSFileRotationReconnect(t *testing.T) {
	d := t.TempDir()
	ca, caKey := makeCA(t)
	caPath := writePEM(t, d, "ca.pem", "CERTIFICATE", ca.Raw)
	sc, sk := makeLeaf(t, ca, caKey, "registry", "")
	sf := TLSFiles{CertFile: writeCert(t, d, "server", sc, sk), KeyFile: filepath.Join(d, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	a1, k1 := makeLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-agent/a")
	af := TLSFiles{CertFile: writeCert(t, d, "agent", a1, k1), KeyFile: filepath.Join(d, "agent.key"), CAFile: caPath, TrustDomain: "example.test"}
	st, _ := ServerTLS(sf)
	ct, _ := ClientTLS(af, "registry")
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	tlsLn := tls.NewListener(ln, st)
	errs := make(chan error, 3)
	go func() {
		for i := 0; i < 3; i++ {
			c, e := tlsLn.Accept()
			if e == nil {
				e = c.(*tls.Conn).Handshake()
				c.Close()
			}
			errs <- e
		}
	}()
	dial := func(cfg *tls.Config, want bool) {
		c, e := tls.Dial("tcp", ln.Addr().String(), cfg)
		if c != nil {
			c.Close()
		}
		serverErr := <-errs
		if want && (e != nil || serverErr != nil) {
			t.Fatalf("client=%v server=%v", e, serverErr)
		}
		if !want && e == nil && serverErr == nil {
			t.Fatal("old CA accepted")
		}
	}
	dial(ct, true)
	ca2, key2 := makeCA(t)
	writePEM(t, d, "ca.pem", "CERTIFICATE", ca2.Raw)
	sc2, sk2 := makeLeaf(t, ca2, key2, "registry", "")
	writeCert(t, d, "server", sc2, sk2)
	a2, k2 := makeLeaf(t, ca2, key2, "", "spiffe://example.test/cluster-state-agent/a")
	writeCert(t, d, "agent", a2, k2)
	dial(ct, false)
	newCT, e := ClientTLS(af, "registry")
	if e != nil {
		t.Fatal(e)
	}
	dial(newCT, true)
}

func TestFloodIsolationAndRoleSeparation(t *testing.T) {
	d := t.TempDir()
	ca, caKey := makeCA(t)
	caPath := writePEM(t, d, "ca.pem", "CERTIFICATE", ca.Raw)
	sc, sk := makeLeaf(t, ca, caKey, "registry", "")
	sf := TLSFiles{CertFile: writeCert(t, d, "server", sc, sk), KeyFile: filepath.Join(d, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	limits := registry.DefaultLimits()
	limits.IngressFrameRate = registry.MinIngressFrameRate
	limits.IngressFrameBurst = 4
	r, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	r.SetClock(func() time.Time { return time.Unix(1_000, 0) })
	agentClient := func(id string) *tls.Config {
		c, k := makeLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-agent/"+id)
		f := TLSFiles{CertFile: writeCert(t, d, "agent-"+id, c, k), KeyFile: filepath.Join(d, "agent-"+id+".key"), CAFile: caPath, TrustDomain: "example.test"}
		x, e := ClientTLS(f, "registry")
		if e != nil {
			t.Fatal(e)
		}
		return x
	}
	st, _ := ServerTLS(sf)
	svc := &Service{Registry: r, TrustDomain: "example.test"}
	a, stop := dialBufService(t, st, agentClient("a"), svc)
	defer stop()
	as, _ := a.Sync(context.Background())
	sendRecv(t, as, &v1.AgentFrame{Frame: &v1.AgentFrame_Hello{Hello: &v1.Hello{ClusterId: "a", ProtocolVersion: 1}}})
	sendRecv(t, as, &v1.AgentFrame{Frame: &v1.AgentFrame_Heartbeat{Heartbeat: &v1.Heartbeat{}}})
	sendRecv(t, as, &v1.AgentFrame{Frame: &v1.AgentFrame_Heartbeat{Heartbeat: &v1.Heartbeat{}}})
	sendRecv(t, as, &v1.AgentFrame{Frame: &v1.AgentFrame_Heartbeat{Heartbeat: &v1.Heartbeat{}}})
	if e := as.Send(&v1.AgentFrame{Frame: &v1.AgentFrame_Heartbeat{Heartbeat: &v1.Heartbeat{}}}); e == nil {
		_, e = as.Recv()
		if status.Code(e) != codes.ResourceExhausted {
			t.Fatalf("flood=%v", e)
		}
	}
	reconnect, _ := a.Sync(context.Background())
	_ = reconnect.Send(&v1.AgentFrame{Frame: &v1.AgentFrame_Hello{Hello: &v1.Hello{ClusterId: "a", ProtocolVersion: 1}}})
	if _, e := reconnect.Recv(); status.Code(e) != codes.ResourceExhausted {
		t.Fatalf("reconnect reset limiter: %v", e)
	}
	b, stopB := dialBufService(t, st, agentClient("b"), svc)
	defer stopB()
	bs, _ := b.Sync(context.Background())
	sendRecv(t, bs, &v1.AgentFrame{Frame: &v1.AgentFrame_Hello{Hello: &v1.Hello{ClusterId: "b", ProtocolVersion: 1}}})
	sendRecv(t, bs, &v1.AgentFrame{Frame: &v1.AgentFrame_BeginSnapshot{BeginSnapshot: &v1.BeginSnapshot{Epoch: 1}}})
	sendRecv(t, bs, &v1.AgentFrame{Frame: &v1.AgentFrame_SnapshotChunk{SnapshotChunk: &v1.SnapshotChunk{Resources: []*v1.Resource{{Kind: v1.KindPod, Uid: "b1", Namespace: "ns", Name: "b1", Pod: &v1.PodProjection{Phase: "Running"}}}}}})
	sendRecv(t, bs, &v1.AgentFrame{Frame: &v1.AgentFrame_CommitSnapshot{CommitSnapshot: &v1.CommitSnapshot{Epoch: 1}}})
	if snap, _, e := r.Snapshot("b"); e != nil || len(snap.Resources) != 1 {
		t.Fatalf("B state: %v %v", snap, e)
	}
	apiCert, apiKey := makeLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-api/api-1")
	apiFiles := TLSFiles{CertFile: writeCert(t, d, "client-api", apiCert, apiKey), KeyFile: filepath.Join(d, "client-api.key"), CAFile: caPath, TrustDomain: "example.test"}
	apiTLS, _ := ClientTLS(apiFiles, "registry")
	apiServerTLS, _ := ServerTLSForRole(sf, "cluster-state-api")
	api, stopAPI := dialBufService(t, apiServerTLS, apiTLS, svc)
	defer stopAPI()
	if _, e := api.Query(context.Background(), &v1.ScreenQuery{ClusterId: "b", Screen: "overview", Scope: &v1.NamespaceScope{All: true}, EventLimit: 50, UnhealthyLimit: 20}); e != nil {
		t.Fatal(e)
	}
	wrongSync, _ := api.Sync(context.Background())
	_ = wrongSync.Send(&v1.AgentFrame{Frame: &v1.AgentFrame_Hello{Hello: &v1.Hello{ClusterId: "a", ProtocolVersion: 1}}})
	if _, e := wrongSync.Recv(); e == nil {
		t.Fatal("API certificate synced")
	}
	if _, e := a.Query(context.Background(), &v1.ScreenQuery{ClusterId: "a"}); e == nil {
		t.Fatal("agent certificate queried")
	}
}

func TestActualTLSRejectsMissingUntrustedWrongRoleExpired(t *testing.T) {
	d := t.TempDir()
	ca, key := makeCA(t)
	caPath := writePEM(t, d, "ca.pem", "CERTIFICATE", ca.Raw)
	sc, sk := makeLeaf(t, ca, key, "registry", "")
	sf := TLSFiles{CertFile: writeCert(t, d, "server", sc, sk), KeyFile: filepath.Join(d, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	st, _ := ServerTLS(sf)
	pool, _ := loadPool(caPath)
	clients := map[string]*tls.Config{"missing": {MinVersion: tls.VersionTLS13, ServerName: "registry", RootCAs: pool}, "wrong-role": clientForLeaf(t, d, "wrong", caPath, ca, key, "spiffe://example.test/cluster-state-api/x"), "expired": clientForLeafValidity(t, d, "expired", caPath, ca, key, "spiffe://example.test/cluster-state-agent/a", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))}
	otherCA, otherKey := makeCA(t)
	clients["untrusted"] = clientForLeaf(t, d, "untrusted", caPath, otherCA, otherKey, "spiffe://example.test/cluster-state-agent/a")
	for name, c := range clients {
		t.Run(name, func(t *testing.T) {
			if e := actualHandshake(st, c); e == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func actualHandshake(st, ct *tls.Config) error {
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return e
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, e := ln.Accept()
		if e == nil {
			tc := tls.Server(c, st)
			e = tc.Handshake()
			tc.Close()
		}
		done <- e
	}()
	c, e := tls.Dial("tcp", ln.Addr().String(), ct)
	if e == nil {
		c.Close()
	}
	se := <-done
	if e != nil {
		return e
	}
	return se
}
func clientForLeaf(t *testing.T, d, name, caPath string, ca *x509.Certificate, key *ecdsa.PrivateKey, uri string) *tls.Config {
	return clientForLeafValidity(t, d, name, caPath, ca, key, uri, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
}
func clientForLeafValidity(t *testing.T, d, name, caPath string, ca *x509.Certificate, key *ecdsa.PrivateKey, uri string, nb, na time.Time) *tls.Config {
	c, k := makeLeafValidity(t, ca, key, "", uri, nb, na)
	f := TLSFiles{CertFile: writeCert(t, d, name, c, k), KeyFile: filepath.Join(d, name+".key"), CAFile: caPath, TrustDomain: "example.test"}
	x, _ := ClientTLS(f, "registry")
	return x
}

func dialBufService(t *testing.T, st, ct *tls.Config, svc *Service) (v1.ClusterStateClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(st)), grpc.MaxRecvMsgSize(4<<20))
	v1.RegisterClusterStateServer(gs, svc)
	go gs.Serve(lis)
	cc, e := grpc.NewClient("passthrough:///registry", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(credentials.NewTLS(ct)))
	if e != nil {
		t.Fatal(e)
	}
	return v1.NewClusterStateClient(cc), func() { cc.Close(); gs.Stop() }
}
func sendRecv(t *testing.T, s v1.ClusterState_SyncClient, f *v1.AgentFrame) {
	t.Helper()
	if e := s.Send(f); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Recv(); e != nil {
		t.Fatal(e)
	}
}

func makeCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "issue25-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if e != nil {
		t.Fatal(e)
	}
	c, _ := x509.ParseCertificate(der)
	return c, k
}
func makeLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns, uri string) (*x509.Certificate, *ecdsa.PrivateKey) {
	return makeLeafValidity(t, ca, caKey, dns, uri, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
}
func makeLeafValidity(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns, uri string, notBefore, notAfter time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: notBefore, NotAfter: notAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	if dns != "" {
		tpl.DNSNames = []string{dns}
	}
	if uri != "" {
		u, _ := url.Parse(uri)
		tpl.URIs = []*url.URL{u}
	}
	der, e := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey)
	if e != nil {
		t.Fatal(e)
	}
	c, _ := x509.ParseCertificate(der)
	return c, k
}
func writePEM(t *testing.T, d, name, typ string, b []byte) string {
	t.Helper()
	p := filepath.Join(d, name)
	if e := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func writeCert(t *testing.T, d, name string, c *x509.Certificate, k *ecdsa.PrivateKey) string {
	t.Helper()
	cp := writePEM(t, d, name+".pem", "CERTIFICATE", c.Raw)
	kb, _ := x509.MarshalPKCS8PrivateKey(k)
	writePEM(t, d, name+".key", "PRIVATE KEY", kb)
	return cp
}
