package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/registry"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	for _, x := range []struct {
		k        string
		min, max int
	}{{"CLUSTER_STATE_MAX_MESSAGE_BYTES", registry.MinProtocolMessageBytes, registry.MaxProtocolMessageBytes}, {"CLUSTER_STATE_MAX_CLUSTERS", 1, registry.MaxConfiguredClusters}, {"CLUSTER_STATE_MAX_RESOURCES", 1, registry.MaxProjectedResources}, {"CLUSTER_STATE_MAX_CHUNK_RESOURCES", 1, registry.MaxSnapshotChunkResources}, {"CLUSTER_STATE_MAX_STATE_BYTES", registry.MinProtocolMessageBytes, 1 << 30}, {"CLUSTER_STATE_MAX_TOTAL_STATE_BYTES", registry.MinProtocolMessageBytes, 1 << 30}, {"CLUSTER_STATE_INGRESS_FRAME_BURST", registry.MinIngressFrameBurst, registry.MaxIngressFrameBurst}, {"CLUSTER_STATE_INGRESS_BYTE_BURST", registry.MinIngressByteBurst, registry.MaxIngressByteBurst}} {
		if e := validateIntEnv(x.k, x.min, x.max); e != nil {
			return e
		}
	}
	for _, x := range []struct {
		k        string
		min, max float64
	}{{"CLUSTER_STATE_INGRESS_FRAME_RATE", registry.MinIngressFrameRate, registry.MaxIngressFrameRate}, {"CLUSTER_STATE_INGRESS_BYTE_RATE", registry.MinIngressByteRate, registry.MaxIngressByteRate}} {
		if e := validateFloatEnv(x.k, x.min, x.max); e != nil {
			return e
		}
	}
	for _, x := range []struct {
		k   string
		max time.Duration
	}{{"CLUSTER_STATE_STALE_TTL", registry.MaxStaleTTL}, {"CLUSTER_STATE_HEARTBEAT_TIMEOUT", registry.MaxHeartbeatTimeout}} {
		if e := validateDurationEnv(x.k, x.max); e != nil {
			return e
		}
	}
	agentAddr, queryAddr := env("CLUSTER_STATE_AGENT_ADDR", ":9443"), env("CLUSTER_STATE_QUERY_ADDR", ":9444")
	if agentAddr == queryAddr {
		return fmt.Errorf("cluster-state listener addresses must be distinct")
	}
	if _, _, e := net.SplitHostPort(agentAddr); e != nil {
		return fmt.Errorf("invalid agent listener address")
	}
	if _, _, e := net.SplitHostPort(queryAddr); e != nil {
		return fmt.Errorf("invalid query listener address")
	}
	trust := os.Getenv("CLUSTER_STATE_TRUST_DOMAIN")
	files := transport.TLSFiles{CertFile: os.Getenv("CLUSTER_STATE_TLS_CERT_FILE"), KeyFile: os.Getenv("CLUSTER_STATE_TLS_KEY_FILE"), CAFile: os.Getenv("CLUSTER_STATE_TLS_CA_FILE"), TrustDomain: trust}
	max := envInt("CLUSTER_STATE_MAX_MESSAGE_BYTES", registry.MaxProtocolMessageBytes)
	limits := registry.DefaultLimits()
	limits.MaxMessageBytes = max
	limits.MaxClusters = envInt("CLUSTER_STATE_MAX_CLUSTERS", registry.MaxConfiguredClusters)
	limits.MaxResources = envInt("CLUSTER_STATE_MAX_RESOURCES", registry.MaxProjectedResources)
	limits.MaxChunkResources = envInt("CLUSTER_STATE_MAX_CHUNK_RESOURCES", registry.MaxSnapshotChunkResources)
	limits.MaxStateBytes = envInt("CLUSTER_STATE_MAX_STATE_BYTES", 256<<20)
	limits.MaxTotalStateBytes = int64(envInt("CLUSTER_STATE_MAX_TOTAL_STATE_BYTES", 512<<20))
	limits.StaleTTL = envDuration("CLUSTER_STATE_STALE_TTL", 5*time.Minute)
	limits.HeartbeatTimeout = envDuration("CLUSTER_STATE_HEARTBEAT_TIMEOUT", 45*time.Second)
	limits.IngressFrameRate = envFloat("CLUSTER_STATE_INGRESS_FRAME_RATE", 1000)
	limits.IngressByteRate = envFloat("CLUSTER_STATE_INGRESS_BYTE_RATE", 16<<20)
	limits.IngressFrameBurst = envInt("CLUSTER_STATE_INGRESS_FRAME_BURST", 2000)
	limits.IngressByteBurst = envInt("CLUSTER_STATE_INGRESS_BYTE_BURST", 8<<20)
	limits.AllowedClusters = split(os.Getenv("CLUSTER_STATE_CLUSTERS"))
	if len(limits.AllowedClusters) == 0 {
		return fmt.Errorf("CLUSTER_STATE_CLUSTERS is required")
	}
	r, e := registry.New(limits)
	if e != nil {
		return e
	}
	svc := &transport.Service{Registry: r, TrustDomain: trust, MaxMessageBytes: max}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go pruneLoop(ctx, r, 30*time.Second)
	return serve(ctx, svc, files, agentAddr, queryAddr, max)
}
func pruneLoop(ctx context.Context, r *registry.Registry, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.PruneExpired()
		}
	}
}
func serve(ctx context.Context, svc *transport.Service, files transport.TLSFiles, agentAddr, queryAddr string, max int) error {
	agentTLS, e := transport.ServerTLSForRole(files, "cluster-state-agent")
	if e != nil {
		return e
	}
	apiTLS, e := transport.ServerTLSForRole(files, "cluster-state-api")
	if e != nil {
		return e
	}
	type running struct {
		s *grpc.Server
		l net.Listener
	}
	configs := []struct {
		addr string
		cfg  *tls.Config
	}{{agentAddr, agentTLS}, {queryAddr, apiTLS}}
	runs := make([]running, 0, 2)
	for _, x := range configs {
		l, e := net.Listen("tcp", x.addr)
		if e != nil {
			for _, opened := range runs {
				opened.l.Close()
				opened.s.Stop()
			}
			return e
		}
		s := grpc.NewServer(grpc.Creds(credentials.NewTLS(x.cfg)), grpc.MaxRecvMsgSize(max), grpc.MaxSendMsgSize(max))
		v1.RegisterClusterStateServer(s, svc)
		hs := health.NewServer()
		hs.SetServingStatus(v1.ClusterState_ServiceDesc.ServiceName, healthv1.HealthCheckResponse_SERVING)
		healthv1.RegisterHealthServer(s, hs)
		runs = append(runs, running{s, l})
	}
	errCh := make(chan error, len(runs))
	for _, x := range runs {
		go func(x running) { errCh <- x.s.Serve(x.l) }(x)
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
	}
	for _, x := range runs {
		x.l.Close()
	}
	done := make(chan struct{})
	go func() {
		for _, x := range runs {
			x.s.GracefulStop()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		for _, x := range runs {
			x.s.Stop()
		}
		<-done
	}
	return serveErr
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func envInt(k string, d int) int {
	v, e := strconv.Atoi(os.Getenv(k))
	if e != nil || v < 1 {
		return d
	}
	return v
}
func envFloat(k string, d float64) float64 {
	v, e := strconv.ParseFloat(os.Getenv(k), 64)
	if e != nil || v <= 0 {
		return d
	}
	return v
}
func envDuration(k string, d time.Duration) time.Duration {
	v, e := time.ParseDuration(os.Getenv(k))
	if e != nil || v <= 0 {
		return d
	}
	return v
}
func validateIntEnv(k string, min, max int) error {
	v := os.Getenv(k)
	if v == "" {
		return nil
	}
	n, e := strconv.Atoi(v)
	if e != nil || n < min || n > max {
		return fmt.Errorf("%s is invalid", k)
	}
	return nil
}
func validateFloatEnv(k string, min, max float64) error {
	v := os.Getenv(k)
	if v == "" {
		return nil
	}
	n, e := strconv.ParseFloat(v, 64)
	if e != nil || n < min || n > max || math.IsNaN(n) || math.IsInf(n, 0) {
		return fmt.Errorf("%s is invalid", k)
	}
	return nil
}
func validateDurationEnv(k string, max time.Duration) error {
	v := os.Getenv(k)
	if v == "" {
		return nil
	}
	n, e := time.ParseDuration(v)
	if e != nil || n <= 0 || n > max {
		return fmt.Errorf("%s is invalid", k)
	}
	return nil
}
func split(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
