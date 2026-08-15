package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/auth"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/registry"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/transport"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/config"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

type countedRegistryService struct {
	*transport.Service
	queries atomic.Int32
}

type pollCountingAlerts struct {
	total atomic.Int32
	mu    sync.Mutex
	by    map[string]int
}

func (p *pollCountingAlerts) List(_ context.Context, q datasource.AlertQuery) (datasource.AlertResult, error) {
	p.total.Add(1)
	p.mu.Lock()
	p.by[q.Target.ClusterID]++
	p.mu.Unlock()
	return datasource.AlertResult{}, nil
}

func (s *countedRegistryService) Query(ctx context.Context, q *v1.ScreenQuery) (*v1.ScreenReply, error) {
	s.queries.Add(1)
	return s.Service.Query(ctx, q)
}

func TestCentralRuntimePrivateCAGRPCQueryWatchSSEIsolationAndShutdown(t *testing.T) {
	now := time.Now().UTC()
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a", "b"}
	limits.StaleTTL = time.Second
	limits.HeartbeatTimeout = time.Second
	reg, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	reg.SetClock(func() time.Time { return now })
	for _, id := range []string{"a", "b"} {
		seedRegistryCluster(t, reg, id)
	}

	dir := t.TempDir()
	ca, caKey := testCA(t)
	caPath := writeTestPEM(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	serverCert, serverKey := testLeaf(t, ca, caKey, "registry", "")
	apiCert, apiKey := testLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-api/api-1")
	serverFiles := transport.TLSFiles{CertFile: writeTestCert(t, dir, "server", serverCert, serverKey), KeyFile: filepath.Join(dir, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	apiFiles := transport.TLSFiles{CertFile: writeTestCert(t, dir, "api", apiCert, apiKey), KeyFile: filepath.Join(dir, "api.key"), CAFile: caPath, TrustDomain: "example.test"}
	serverTLS, err := transport.ServerTLSForRole(serverFiles, "cluster-state-api")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := &countedRegistryService{Service: &transport.Service{Registry: reg, TrustDomain: "example.test", MaxMessageBytes: 4 << 20}}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.MaxRecvMsgSize(4<<20), grpc.MaxSendMsgSize(4<<20))
	v1.RegisterClusterStateServer(grpcServer, service)
	healthServer := health.NewServer()
	healthServer.SetServingStatus(v1.ClusterState_ServiceDesc.ServiceName, healthv1.HealthCheckResponse_SERVING)
	healthv1.RegisterHealthServer(grpcServer, healthServer)
	go grpcServer.Serve(listener)
	t.Cleanup(func() { grpcServer.Stop(); _ = listener.Close() })

	hub, err := stream.New(stream.Config{RingSize: 8, SubscriberBuffer: 8, ClusterIDs: []string{"a", "b"}, MaxClusters: 2, MaxRetainedEvents: 16}, nil)
	if err != nil {
		t.Fatal(err)
	}
	aSub, err := hub.Subscribe("a-viewer", stream.Filter{ClusterID: "a", All: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	bSub, err := hub.Subscribe("b-viewer", stream.Filter{ClusterID: "b", All: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime, err := newCentralRuntime(ctx, config.ClusterStateConfig{Mode: "central", RegistryEndpoint: listener.Addr().String(), RegistryServerName: "registry", Clusters: []string{"a", "b"}, TrustDomain: "example.test", CertFile: apiFiles.CertFile, KeyFile: apiFiles.KeyFile, CAFile: apiFiles.CAFile, MaxClusters: 2, MaxResources: 100, MaxChunkResources: 10, MaxMessageBytes: 4 << 20, StaleTTL: time.Second, HeartbeatTimeout: time.Second}, hub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = runtime.Close(); hub.Close() })
	waitUntil(t, time.Second*3, runtime.registry.Ready)
	waitUntil(t, time.Second*3, func() bool { return runtime.catalog.Available("a") && runtime.catalog.Available("b") })
	for id, sub := range map[string]*stream.Subscription{"a": aSub, "b": bSub} {
		select {
		case event := <-sub.Events():
			if event.Envelope.ClusterID != id || event.Envelope.Kind != "reset" {
				t.Fatalf("%s initial commit fanout=%+v", id, event.Envelope)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s initial commit did not emit one reset", id)
		}
	}

	api := httptest.NewServer(httpapi.NewServer(httpapi.Deps{
		ProviderRegistry: runtime.registry,
		Metrics:          datasource.Unavailable{}, Logs: datasource.Unavailable{}, Alerts: datasource.Unavailable{}, Topology: datasource.Unavailable{},
		Resolver: scope.Static{S: scope.Scope{Clusters: []scope.Cluster{{ID: "a", Name: "a", All: true}, {ID: "b", Name: "b", All: true}}}},
		Stream:   hub,
	}))
	t.Cleanup(api.Close)
	getStatus := func(cluster string) int {
		response, err := http.Get(api.URL + "/api/v1/clusters/" + cluster + "/overview?range=1h")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	before := service.queries.Load()
	if status := getStatus("unknown"); status != http.StatusForbidden || service.queries.Load() != before {
		t.Fatalf("unauthorized status=%d queries=%d/%d", status, before, service.queries.Load())
	}
	if status := getStatus("a"); status != http.StatusOK || service.queries.Load() != before+1 {
		t.Fatalf("authorized status=%d query delta=%d", status, service.queries.Load()-before)
	}

	_, nack := reg.Delta("b", &v1.Delta{Epoch: 1, Seq: 1, Resource: testPod("b-2")})
	if nack != nil {
		t.Fatal(nack)
	}
	select {
	case event := <-bSub.Events():
		if event.Envelope.ClusterID != "b" || event.Envelope.Kind != "pod" {
			t.Fatalf("B delta fanout=%+v", event.Envelope)
		}
	case <-time.After(time.Second):
		t.Fatal("B delta was not fanned out")
	}

	reg.Disconnect("a")
	now = now.Add(2 * time.Second)
	if pruned := reg.PruneExpired(); pruned != 1 {
		t.Fatalf("pruned=%d", pruned)
	}
	waitUntil(t, time.Second, func() bool { return !runtime.catalog.Available("a") })
	select {
	case event := <-aSub.Events():
		if event.Envelope.Kind != "reset" || event.Envelope.ClusterID != "a" {
			t.Fatalf("A expiry fanout=%+v", event.Envelope)
		}
	case <-time.After(time.Second):
		t.Fatal("A expiry did not emit reset")
	}
	if status := getStatus("a"); status != http.StatusServiceUnavailable {
		t.Fatalf("expired A status=%d", status)
	}
	start := time.Now()
	if status := getStatus("b"); status != http.StatusOK || time.Since(start) >= 500*time.Millisecond {
		t.Fatalf("healthy B status=%d latency=%s", status, time.Since(start))
	}
	ready, err := http.Get(api.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = ready.Body.Close()
	if ready.StatusCode != http.StatusOK {
		t.Fatalf("A expiry lowered global readiness: %d", ready.StatusCode)
	}
	cancel()
	start = time.Now()
	if err := runtime.Close(); err != nil || time.Since(start) >= time.Second {
		t.Fatalf("runtime shutdown err=%v latency=%s", err, time.Since(start))
	}
}

func TestServeCentralHTTPClosesOwnedListenerOnBackgroundFatalAndOccupiedPort(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	occupiedAddr := occupied.Addr().String()
	server := &http.Server{Addr: occupiedAddr, Handler: http.NewServeMux()}
	if err := serveCentralHTTP(context.Background(), server, make(chan error), make(chan error), nil); err == nil {
		t.Fatal("occupied port startup succeeded")
	}
	if conn, err := net.DialTimeout("tcp", occupiedAddr, time.Second); err != nil {
		t.Fatalf("startup failure closed a listener it did not own: %v", err)
	} else {
		_ = conn.Close()
	}
	_ = occupied.Close()

	for _, source := range []string{"watch", "usage"} {
		t.Run(source, func(t *testing.T) {
			probe, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := probe.Addr().String()
			_ = probe.Close()
			mux := http.NewServeMux()
			mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			watchErr := make(chan error, 1)
			usageErr := make(chan error, 1)
			done := make(chan error, 1)
			server := &http.Server{Addr: addr, Handler: mux}
			go func() { done <- serveCentralHTTP(context.Background(), server, watchErr, usageErr, nil) }()
			waitUntil(t, 3*time.Second, func() bool {
				response, err := http.Get("http://" + addr + "/healthz")
				if err != nil {
					return false
				}
				_ = response.Body.Close()
				return response.StatusCode == http.StatusOK
			})
			sentinel := errors.New("injected " + source + " fatal")
			if source == "watch" {
				watchErr <- sentinel
			} else {
				usageErr <- sentinel
			}
			select {
			case err := <-done:
				if !errors.Is(err, sentinel) {
					t.Fatalf("serve returned %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("serve did not return after background fatal")
			}
			if conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
				_ = conn.Close()
				t.Fatal("owned HTTP listener remained open after fatal")
			}
		})
	}
}

func TestCentralRuntimeConstructionAndShutdownFailClosed(t *testing.T) {
	if err := shutdownCentralHTTP(nil); err != nil {
		t.Fatal(err)
	}
	if err := (*centralRuntime)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if runtime, err := newCentralRuntime(ctx, config.ClusterStateConfig{Clusters: []string{"a"}, MaxResources: 10}, nil); err == nil || runtime != nil {
		t.Fatalf("nil hub runtime=%v err=%v", runtime, err)
	}
	hub, err := stream.New(stream.Config{RingSize: 8, SubscriberBuffer: 1, MaxConnections: 2, MaxPerSubject: 1}, stream.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	if runtime, err := newCentralRuntime(ctx, config.ClusterStateConfig{Clusters: []string{"INVALID"}, MaxResources: 10}, hub); err == nil || runtime != nil {
		t.Fatalf("invalid catalog runtime=%v err=%v", runtime, err)
	}
	invalidTLS := config.ClusterStateConfig{Clusters: []string{"a"}, MaxResources: 10, RegistryEndpoint: "127.0.0.1:1", RegistryServerName: "registry", TrustDomain: "example.test"}
	if runtime, err := newCentralRuntime(ctx, invalidTLS, hub); err == nil || runtime != nil {
		t.Fatalf("invalid TLS runtime=%v err=%v", runtime, err)
	}
}

func TestRunCentralRejectsEnabledAlertmanagerWithoutPreparedSource(t *testing.T) {
	cfg := config.Config{}
	cfg.Alertmanager.Enabled = true
	if err := runCentral(context.Background(), discardLogger(), cfg); err == nil {
		t.Fatal("enabled Alertmanager without a prepared source was accepted")
	}
}

func TestCentralAlertPollersDisabledUniqueJitterAndBoundedCancel(t *testing.T) {
	var disabled pollCountingAlerts
	stop := startCentralAlertPollers(context.Background(), config.Config{}, nil, &disabled, nil, discardLogger())
	stop()
	if disabled.total.Load() != 0 {
		t.Fatalf("disabled poller calls=%d", disabled.total.Load())
	}

	clusters := make([]string, 64)
	for i := range clusters {
		clusters[i] = fmt.Sprintf("c%02d", i)
	}
	catalog, err := clusterstate.NewRemoteCatalog(clusters, 1)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := stream.New(stream.Config{RingSize: 8, SubscriberBuffer: 1, MaxConnections: 2, MaxPerSubject: 1}, stream.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	alerts := &pollCountingAlerts{by: make(map[string]int)}
	cfg := config.Config{AlertPollInterval: 200 * time.Millisecond, AlertPollMaxBackoff: time.Second, AlertSnapshotMax: 10}
	cfg.Alertmanager.Enabled = true
	cfg.ClusterState.Clusters = append(append([]string(nil), clusters...), clusters[0])
	stop = startCentralAlertPollers(context.Background(), cfg, catalog, alerts, hub, discardLogger())
	deadline := time.Now().Add(2 * time.Second)
	for alerts.total.Load() < 64 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	stop()
	if time.Since(started) > time.Second {
		t.Fatal("central poller cancellation was not bounded")
	}
	before := alerts.total.Load()
	time.Sleep(250 * time.Millisecond)
	if before != 64 || alerts.total.Load() != before {
		t.Fatalf("unique/non-spin/post-cancel calls before=%d after=%d", before, alerts.total.Load())
	}
	alerts.mu.Lock()
	defer alerts.mu.Unlock()
	for _, cluster := range clusters {
		if alerts.by[cluster] != 1 {
			t.Fatalf("cluster %s calls=%d", cluster, alerts.by[cluster])
		}
	}
	for i := 0; i < 100; i++ {
		if d := randomAlertInitialDelay(time.Second); d < 0 || d > time.Second {
			t.Fatalf("jitter=%s", d)
		}
	}
}

func TestRunCentralProductionLifecycleWithOIDCAndPrivateCA(t *testing.T) {
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a"}
	reg, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistryCluster(t, reg, "a")
	dir := t.TempDir()
	ca, caKey := testCA(t)
	caPath := writeTestPEM(t, dir, "run-ca.pem", "CERTIFICATE", ca.Raw)
	serverCert, serverKey := testLeaf(t, ca, caKey, "registry", "")
	apiCert, apiKey := testLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-api/api-run")
	serverFiles := transport.TLSFiles{CertFile: writeTestCert(t, dir, "run-server", serverCert, serverKey), KeyFile: filepath.Join(dir, "run-server.key"), CAFile: caPath, TrustDomain: "example.test"}
	apiFiles := transport.TLSFiles{CertFile: writeTestCert(t, dir, "run-api", apiCert, apiKey), KeyFile: filepath.Join(dir, "run-api.key"), CAFile: caPath, TrustDomain: "example.test"}
	serverTLS, err := transport.ServerTLSForRole(serverFiles, "cluster-state-api")
	if err != nil {
		t.Fatal(err)
	}
	registryListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	v1.RegisterClusterStateServer(grpcServer, &transport.Service{Registry: reg, TrustDomain: "example.test", MaxMessageBytes: 4 << 20})
	healthServer := health.NewServer()
	healthServer.SetServingStatus(v1.ClusterState_ServiceDesc.ServiceName, healthv1.HealthCheckResponse_SERVING)
	healthv1.RegisterHealthServer(grpcServer, healthServer)
	go func() { _ = grpcServer.Serve(registryListener) }()
	t.Cleanup(func() { grpcServer.Stop(); _ = registryListener.Close() })

	idp, err := auth.StartMockIDP("127.0.0.1:0", "issue25-central", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idp.Close() })
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	apiAddr := probe.Addr().String()
	_ = probe.Close()
	cfg := config.Load()
	cfg.Addr = apiAddr
	cfg.UseDemoData = false
	cfg.Auth = config.AuthConfig{Mode: "oidc", Issuer: idp.Issuer, Audience: "issue25-central", RolesClaim: "roles", Leeway: time.Minute, JWKSMinRefresh: time.Second}
	redisServer := miniredis.RunT(t)
	enableCentralBrowserSession(&cfg, redisServer.Addr())
	cfg.ClusterState = config.ClusterStateConfig{Mode: "central", RegistryEndpoint: registryListener.Addr().String(), RegistryServerName: "registry", Clusters: []string{"a"}, TrustDomain: "example.test", CertFile: apiFiles.CertFile, KeyFile: apiFiles.KeyFile, CAFile: apiFiles.CAFile, MaxClusters: 1, MaxResources: 100, MaxChunkResources: 10, MaxMessageBytes: 4 << 20, StaleTTL: time.Minute, HeartbeatTimeout: time.Minute}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runCentral(ctx, slog.Default(), cfg) }()
	waitUntil(t, 5*time.Second, func() bool {
		response, err := http.Get("http://" + apiAddr + "/readyz")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("central production lifecycle did not stop")
	}
	if conn, err := net.DialTimeout("tcp", apiAddr, 50*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("central HTTP listener leaked after cancellation")
	}
	waitUntil(t, time.Second, func() bool { return redisServer.CurrentConnectionCount() == 0 })
}

func TestRunCentralClosesSessionRedisWhenHubConstructionFails(t *testing.T) {
	idp, err := auth.StartMockIDP("127.0.0.1:0", "central-close", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idp.Close() })
	redisServer := miniredis.RunT(t)
	cfg := config.Load()
	cfg.Auth = config.AuthConfig{Mode: "oidc", Issuer: idp.Issuer, Audience: "central-close", RolesClaim: "roles", Leeway: time.Minute, JWKSMinRefresh: time.Second}
	cfg.ClusterState = config.ClusterStateConfig{Mode: "central", Clusters: []string{"a"}, MaxClusters: 1}
	enableCentralBrowserSession(&cfg, redisServer.Addr())
	cfg.StreamReplayEvents = stream.MaxRingSize + 1
	if err := runCentral(context.Background(), discardLogger(), cfg); err == nil {
		t.Fatal("invalid hub configuration unexpectedly started")
	}
	waitUntil(t, time.Second, func() bool { return redisServer.CurrentConnectionCount() == 0 })
}

func enableCentralBrowserSession(cfg *config.Config, redisAddr string) {
	cfg.RedisAddr = redisAddr
	cfg.RedisOpTimeout = time.Second
	cfg.Auth.SessionEnabled = true
	cfg.Auth.PublicOrigin = "https://dashboard.example"
	cfg.Auth.RedirectURI = "https://dashboard.example/api/v1/auth/callback"
	cfg.Auth.ClientID = "browser-client"
	cfg.Auth.SessionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	cfg.Auth.LoginTransactionTTL = 2 * time.Minute
	cfg.Auth.SessionIdleTTL = 10 * time.Minute
	cfg.Auth.SessionAbsoluteTTL = time.Hour
	cfg.Auth.RefreshSkew = time.Minute
	cfg.Auth.SessionMaxSessions = 10
}

func seedRegistryCluster(t *testing.T, reg *registry.Registry, id string) {
	t.Helper()
	if err := reg.Connect(&v1.Hello{ClusterId: id, ProtocolVersion: v1.Version}, id); err != nil {
		t.Fatal(err)
	}
	if err := reg.Begin(id, &v1.BeginSnapshot{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Chunk(id, &v1.SnapshotChunk{Resources: []*v1.Resource{testPod(id + "-1")}}); err != nil {
		t.Fatal(err)
	}
	if _, nack := reg.Commit(id, &v1.CommitSnapshot{Epoch: 1}); nack != nil {
		t.Fatal(nack)
	}
}

func testPod(uid string) *v1.Resource {
	return &v1.Resource{Kind: v1.KindPod, Uid: uid, Namespace: "ns", Name: uid, Pod: &v1.PodProjection{Phase: "Running", CreatedUnixMs: 1}}
}

func waitUntil(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "issue25-runtime-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key
}

func testLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns, uri string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	if dns != "" {
		template.DNSNames = []string{dns}
	}
	if uri != "" {
		parsed, _ := url.Parse(uri)
		template.URIs = []*url.URL{parsed}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key
}

func writeTestPEM(t *testing.T, dir, name, typ string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestCert(t *testing.T, dir, name string, cert *x509.Certificate, key *ecdsa.PrivateKey) string {
	t.Helper()
	path := writeTestPEM(t, dir, name+".pem", "CERTIFICATE", cert.Raw)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	writeTestPEM(t, dir, name+".key", "PRIVATE KEY", der)
	return path
}
