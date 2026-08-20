package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/registry"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/transport"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type rejectingSyncServer struct {
	v1.UnimplementedClusterStateServer
	sessions atomic.Int32
}

func (s *rejectingSyncServer) Sync(stream v1.ClusterState_SyncServer) error {
	mode := s.sessions.Add(1)
	frame, err := stream.Recv()
	if err != nil || frame.GetHello() == nil {
		return err
	}
	switch mode {
	case 1:
		return stream.Send(&v1.ServerFrame{Frame: &v1.ServerFrame_Nack{Nack: &v1.Nack{Code: "identity_mismatch", FullResync: true}}})
	case 2:
		return stream.Send(&v1.ServerFrame{Frame: &v1.ServerFrame_Ack{Ack: &v1.Ack{}}})
	default:
		if err := stream.Send(&v1.ServerFrame{Frame: &v1.ServerFrame_Ack{Ack: &v1.Ack{Epoch: 9}}}); err != nil {
			return err
		}
		frame, err = stream.Recv()
		if err != nil || frame.GetBeginSnapshot() == nil {
			return err
		}
		return stream.Send(&v1.ServerFrame{Frame: &v1.ServerFrame_Nack{Nack: &v1.Nack{Code: "state_byte_capacity", FullResync: true}}})
	}
}

func TestSyncOnceFailsClosedOnRegistryNackAndInvalidEpoch(t *testing.T) {
	storeCtx, stopStore := context.WithCancel(context.Background())
	defer stopStore()
	store, _ := testcluster.NewStore(t, storeCtx)
	dir := t.TempDir()
	ca, caKey := agentTestCA(t)
	caPath := agentWritePEM(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	serverCert, serverKey := agentTestLeaf(t, ca, caKey, "registry", "")
	agentCert, agentKey := agentTestLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-agent/"+testcluster.ClusterID)
	serverFiles := transport.TLSFiles{CertFile: agentWriteCert(t, dir, "server", serverCert, serverKey), KeyFile: filepath.Join(dir, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	agentFiles := transport.TLSFiles{CertFile: agentWriteCert(t, dir, "agent", agentCert, agentKey), KeyFile: filepath.Join(dir, "agent.key"), CAFile: caPath, TrustDomain: "example.test"}
	serverTLS, err := transport.ServerTLSForRole(serverFiles, "cluster-state-agent")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	v1.RegisterClusterStateServer(server, &rejectingSyncServer{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })

	for _, tc := range []struct {
		want         string
		maxResources int
	}{{"registry nack: identity_mismatch", 100_000}, {"invalid session epoch", 100_000}, {"registry nack: state_byte_capacity", 100_000}, {"resource limit", 1}} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = syncOnce(ctx, store, make(chan struct{}), testcluster.ClusterID, listener.Addr().String(), "registry", agentFiles, 4<<20, tc.maxResources, registry.MaxSnapshotChunkResources, nil)
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("want=%q err=%v", tc.want, err)
		}
		if strings.Contains(err.Error(), listener.Addr().String()) || strings.Contains(err.Error(), dir) {
			t.Fatalf("connection or TLS material leaked in error: %v", err)
		}
	}
}

func TestSyncOncePrivateCASnapshotAndCancellation(t *testing.T) {
	storeCtx, stopStore := context.WithCancel(context.Background())
	defer stopStore()
	store, fakes := testcluster.NewStore(t, storeCtx)

	dir := t.TempDir()
	ca, caKey := agentTestCA(t)
	caPath := agentWritePEM(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	serverCert, serverKey := agentTestLeaf(t, ca, caKey, "registry", "")
	agentCert, agentKey := agentTestLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-agent/"+testcluster.ClusterID)
	serverFiles := transport.TLSFiles{CertFile: agentWriteCert(t, dir, "server", serverCert, serverKey), KeyFile: filepath.Join(dir, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	agentFiles := transport.TLSFiles{CertFile: agentWriteCert(t, dir, "agent", agentCert, agentKey), KeyFile: filepath.Join(dir, "agent.key"), CAFile: caPath, TrustDomain: "example.test"}
	serverTLS, err := transport.ServerTLSForRole(serverFiles, "cluster-state-agent")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{testcluster.ClusterID}
	reg, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	v1.RegisterClusterStateServer(server, &transport.Service{Registry: reg, TrustDomain: "example.test", MaxMessageBytes: 4 << 20})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := 0
	changes := make(chan struct{}, 1)
	callbackErr := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			if snapshot, _, snapshotErr := reg.Snapshot(testcluster.ClusterID); snapshotErr == nil && snapshot.Seq >= 1 {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	err = syncOnce(ctx, store, changes, testcluster.ClusterID, listener.Addr().String(), "registry", agentFiles, 4<<20, 100_000, registry.MaxSnapshotChunkResources, func() {
		ready++
		pod, getErr := fakes.Typed.CoreV1().Pods("payments").Get(ctx, "payments-api-7f-aaa", metav1.GetOptions{})
		if getErr != nil {
			callbackErr <- getErr
			cancel()
			return
		}
		pod.Spec.NodeName = "node-2"
		if _, updateErr := fakes.Typed.CoreV1().Pods("payments").Update(ctx, pod, metav1.UpdateOptions{}); updateErr != nil {
			callbackErr <- updateErr
			cancel()
			return
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			cached, found, _ := store.Pod("payments", pod.Name, string(pod.UID))
			if found && cached.Spec.NodeName == "node-2" {
				changes <- struct{}{}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		callbackErr <- errors.New("updated pod did not reach informer cache")
		cancel()
	})
	if !errors.Is(err, context.Canceled) || ready != 1 {
		t.Fatalf("sync result err=%v ready=%d", err, ready)
	}
	select {
	case err := <-callbackErr:
		t.Fatal(err)
	default:
	}
	snapshot, stale, err := reg.Snapshot(testcluster.ClusterID)
	// The committed and updated snapshot remains queryable whether the server has already
	// observed the client-side close (stale) or is still processing it (fresh).
	pod := snapshot.Resources[v1.KindPod+"\x00"+testcluster.UIDPodHealthy]
	namespace := snapshot.Resources[v1.KindNamespace+"\x00namespace-media"]
	if err != nil || len(snapshot.Resources) == 0 || snapshot.Epoch == 0 || snapshot.Seq != 1 || pod == nil || pod.Pod.NodeName != "node-2" || namespace == nil || namespace.Name != "media" {
		t.Fatalf("registry resources=%d epoch=%d seq=%d stale=%v pod=%v namespace=%v err=%v", len(snapshot.Resources), snapshot.Epoch, snapshot.Seq, stale, pod, namespace, err)
	}
}

func agentTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "issue25-agent-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func agentTestLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns, uri string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	if dns != "" {
		template.DNSNames = []string{dns}
	}
	if uri != "" {
		parsed, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		template.URIs = []*url.URL{parsed}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func agentWritePEM(t *testing.T, dir, name, kind string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func agentWriteCert(t *testing.T, dir, name string, cert *x509.Certificate, key *ecdsa.PrivateKey) string {
	t.Helper()
	path := agentWritePEM(t, dir, name+".pem", "CERTIFICATE", cert.Raw)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	agentWritePEM(t, dir, name+".key", "PRIVATE KEY", der)
	return path
}

func TestChunkResourcesUsesActualProtoSize(t *testing.T) {
	in := []*v1.Resource{{Kind: v1.KindEvent, Uid: "a", Name: "a", Event: &v1.EventProjection{MaskedMessage: string(make([]byte, 500))}}, {Kind: v1.KindEvent, Uid: "b", Name: "b", Event: &v1.EventProjection{MaskedMessage: string(make([]byte, 500))}}}
	chunks, e := chunkResources(in, 700, registry.MaxSnapshotChunkResources)
	if e != nil || len(chunks) != 2 {
		t.Fatal(len(chunks), e)
	}
	for _, c := range chunks {
		f := &v1.AgentFrame{Frame: &v1.AgentFrame_SnapshotChunk{SnapshotChunk: &v1.SnapshotChunk{Resources: c}}}
		if proto.Size(f) > 700 {
			t.Fatal(proto.Size(f))
		}
	}
	tooLarge := &v1.Resource{Kind: v1.KindEvent, Uid: "large", Name: "large", Event: &v1.EventProjection{MaskedMessage: string(make([]byte, 2048))}}
	if chunks, err := chunkResources([]*v1.Resource{tooLarge}, 700, registry.MaxSnapshotChunkResources); err == nil || chunks != nil {
		t.Fatalf("oversized resource chunks=%v err=%v", chunks, err)
	}
}

func TestChunkResourcesCountBoundCommitsAtomicRegistrySnapshot(t *testing.T) {
	resources := make([]*v1.Resource, registry.MaxSnapshotChunkResources+1)
	for i := range resources {
		id := fmt.Sprintf("p-%04d", i)
		resources[i] = &v1.Resource{Kind: v1.KindPod, Uid: id, Namespace: "ns", Name: id, Pod: &v1.PodProjection{}}
	}
	chunks, err := chunkResources(resources, 4<<20, registry.MaxSnapshotChunkResources)
	if err != nil || len(chunks) != 2 || len(chunks[0]) != registry.MaxSnapshotChunkResources || len(chunks[1]) != 1 {
		t.Fatalf("chunks=%v sizes=%v/%v err=%v", len(chunks), len(chunks[0]), len(chunks[1]), err)
	}
	for _, chunk := range chunks {
		frame := &v1.AgentFrame{Frame: &v1.AgentFrame_SnapshotChunk{SnapshotChunk: &v1.SnapshotChunk{Resources: chunk}}}
		if len(chunk) > registry.MaxSnapshotChunkResources || proto.Size(frame) > 4<<20 {
			t.Fatalf("invalid chunk count=%d bytes=%d", len(chunk), proto.Size(frame))
		}
	}
	if chunks, err := chunkResources(resources[:registry.MaxSnapshotChunkResources], 4<<20, registry.MaxSnapshotChunkResources); err != nil || len(chunks) != 1 || len(chunks[0]) != registry.MaxSnapshotChunkResources {
		t.Fatalf("exact boundary chunks=%v err=%v", len(chunks), err)
	}
	if chunks, err := chunkResources(resources[:1], 4<<20, 0); err == nil || chunks != nil {
		t.Fatalf("zero count accepted: chunks=%v err=%v", chunks, err)
	}

	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a"}
	reg, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := reg.OpenSession(&v1.Hello{ClusterId: "a", ProtocolVersion: v1.Version}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err = reg.BeginSession("a", generation, &v1.BeginSnapshot{Epoch: generation}); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range chunks {
		if err = reg.ChunkSession("a", generation, &v1.SnapshotChunk{Resources: chunk}); err != nil {
			t.Fatal(err)
		}
	}
	if _, nack := reg.CommitSession("a", generation, &v1.CommitSnapshot{Epoch: generation}); nack != nil {
		t.Fatalf("bounded agent snapshot required resync: %+v", nack)
	}
	snapshot, _, err := reg.Snapshot("a")
	if err != nil || len(snapshot.Resources) != len(resources) {
		t.Fatalf("atomic snapshot resources=%d err=%v", len(snapshot.Resources), err)
	}
}

func TestRunRejectsConfigurationBeforeKubernetesClient(t *testing.T) {
	base := map[string]string{"CLUSTER_ID": "cluster-a", "CLUSTER_STATE_REGISTRY_ENDPOINT": "registry.test:9443", "CLUSTER_STATE_REGISTRY_SERVER_NAME": "registry.test"}
	for _, tc := range []struct{ name, key, value string }{
		{"cluster", "CLUSTER_ID", "UPPER"}, {"endpoint", "CLUSTER_STATE_REGISTRY_ENDPOINT", "bad"},
		{"empty-host", "CLUSTER_STATE_REGISTRY_ENDPOINT", ":9443"}, {"zero-port", "CLUSTER_STATE_REGISTRY_ENDPOINT", "registry.test:0"},
		{"large-port", "CLUSTER_STATE_REGISTRY_ENDPOINT", "registry.test:65536"}, {"server-name-slash", "CLUSTER_STATE_REGISTRY_SERVER_NAME", "bad/name"},
		{"server-name-colon", "CLUSTER_STATE_REGISTRY_SERVER_NAME", "bad:name"}, {"server-name-space", "CLUSTER_STATE_REGISTRY_SERVER_NAME", "bad name"},
		{"endpoint-scheme", "CLUSTER_STATE_REGISTRY_ENDPOINT", "https://registry.test:9443"}, {"endpoint-path", "CLUSTER_STATE_REGISTRY_ENDPOINT", "registry.test/path:9443"},
		{"endpoint-userinfo", "CLUSTER_STATE_REGISTRY_ENDPOINT", "user@registry.test:9443"}, {"server-name-wildcard", "CLUSTER_STATE_REGISTRY_SERVER_NAME", "*.test"},
		{"endpoint-empty-label", "CLUSTER_STATE_REGISTRY_ENDPOINT", "a..b:9443"}, {"endpoint-leading-hyphen", "CLUSTER_STATE_REGISTRY_ENDPOINT", "a.-b:9443"},
		{"server-name-empty-label", "CLUSTER_STATE_REGISTRY_SERVER_NAME", "a..b"}, {"server-name-trailing-hyphen", "CLUSTER_STATE_REGISTRY_SERVER_NAME", "a-.b"},
		{"server-name-long-label", "CLUSTER_STATE_REGISTRY_SERVER_NAME", strings.Repeat("a", 64) + ".test"}, {"server-name-total-long", "CLUSTER_STATE_REGISTRY_SERVER_NAME", strings.Repeat("a.", 127) + "a"},
		{"message", "CLUSTER_STATE_MAX_MESSAGE_BYTES", "bad"}, {"message-small", "CLUSTER_STATE_MAX_MESSAGE_BYTES", "1"},
		{"resources", "CLUSTER_STATE_MAX_RESOURCES", "100001"},
		{"chunk-zero", "CLUSTER_STATE_MAX_CHUNK_RESOURCES", "0"}, {"chunk-overflow", "CLUSTER_STATE_MAX_CHUNK_RESOURCES", "1001"},
		{"chunk-malformed", "CLUSTER_STATE_MAX_CHUNK_RESOURCES", "bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			t.Setenv(tc.key, tc.value)
			if run() == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	t.Run("chunk-exceeds-resources", func(t *testing.T) {
		for k, v := range base {
			t.Setenv(k, v)
		}
		t.Setenv("CLUSTER_STATE_MAX_RESOURCES", "1")
		t.Setenv("CLUSTER_STATE_MAX_CHUNK_RESOURCES", "2")
		if err := run(); err == nil || !strings.Contains(err.Error(), "must not exceed") {
			t.Fatalf("cross-field chunk limit accepted: %v", err)
		}
	})
	for k, v := range base {
		t.Setenv(k, v)
	}
	if run() == nil {
		t.Fatal("missing TLS files accepted")
	}

	// Valid TLS preflight still must reject kubeconfig before constructing
	// clients/informers or attempting a registry connection.
	dir := t.TempDir()
	ca, caKey := agentTestCA(t)
	caPath := agentWritePEM(t, dir, "preflight-ca.pem", "CERTIFICATE", ca.Raw)
	leaf, key := agentTestLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-agent/cluster-a")
	t.Setenv("CLUSTER_STATE_TLS_CERT_FILE", agentWriteCert(t, dir, "preflight-agent", leaf, key))
	t.Setenv("CLUSTER_STATE_TLS_KEY_FILE", filepath.Join(dir, "preflight-agent.key"))
	t.Setenv("CLUSTER_STATE_TLS_CA_FILE", caPath)
	t.Setenv("CLUSTER_STATE_TRUST_DOMAIN", "example.test")
	t.Setenv("KUBECONFIG", filepath.Join(dir, "missing-kubeconfig"))
	if err := run(); err == nil || !strings.Contains(err.Error(), "missing-kubeconfig") {
		t.Fatalf("missing kubeconfig passed preflight: %v", err)
	}
}

func TestCertificateReconnectDelayAndIndexEdges(t *testing.T) {
	if d := certReconnectDelay(filepath.Join(t.TempDir(), "missing")); d != time.Second {
		t.Fatal(d)
	}
	p := filepath.Join(t.TempDir(), "bad.pem")
	if e := os.WriteFile(p, []byte("bad"), 0600); e != nil {
		t.Fatal(e)
	}
	if d := certReconnectDelay(p); d != time.Second {
		t.Fatal(d)
	}
	if got := index([]*v1.Resource{{Kind: v1.KindPod, Uid: "u"}}); got[string(v1.KindPod)+"\x00u"] == nil {
		t.Fatal(got)
	}
	if chunks, e := chunkResources(nil, 1024, registry.MaxSnapshotChunkResources); e != nil || len(chunks) != 0 {
		t.Fatal(chunks, e)
	}
	dir := t.TempDir()
	ca, caKey := agentTestCA(t)
	leaf, key := agentTestLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-agent/a")
	certPath := agentWriteCert(t, dir, "rotating", leaf, key)
	if delay := certReconnectDelay(certPath); delay <= time.Minute || delay > time.Hour {
		t.Fatalf("certificate reconnect delay=%v", delay)
	}
}
func TestRetryDelayBoundedAndNonZero(t *testing.T) {
	for _, s := range []uint64{0, 200, 400} {
		d := retryDelay(time.Second, s)
		if d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatal(d)
		}
	}
	if delay := retryDelay(time.Second, randomSample()); delay < 800*time.Millisecond || delay > 1200*time.Millisecond {
		t.Fatalf("random jitter delay=%v", delay)
	}
}

func TestCallerCancellationWinsButServerStatusIsPreserved(t *testing.T) {
	serverCanceled := status.Error(codes.Canceled, "server canceled stream")
	for _, direction := range []string{"send", "recv"} {
		t.Run(direction+" caller cancellation", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := callerError(ctx, serverCanceled); !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation=%v", err)
			}
		})
		t.Run(direction+" server cancellation", func(t *testing.T) {
			if err := callerError(context.Background(), serverCanceled); status.Code(err) != codes.Canceled || errors.Is(err, context.Canceled) {
				t.Fatalf("server cancellation=%v", err)
			}
		})
	}
}
func TestValidateIntEnvRejectsPresentInvalid(t *testing.T) {
	for _, value := range []string{"bad", "0", "101"} {
		t.Setenv("ISSUE25_LIMIT", value)
		if validateIntEnv("ISSUE25_LIMIT", 1, 100) == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	t.Setenv("ISSUE25_LIMIT", "")
	if e := validateIntEnv("ISSUE25_LIMIT", 1, 100); e != nil {
		t.Fatal(e)
	}
}
