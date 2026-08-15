package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterid"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/registry"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
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
	clusterID := os.Getenv("CLUSTER_ID")
	endpoint := os.Getenv("CLUSTER_STATE_REGISTRY_ENDPOINT")
	serverName := os.Getenv("CLUSTER_STATE_REGISTRY_SERVER_NAME")
	if !clusterid.Valid(clusterID) || endpoint == "" || serverName == "" {
		return fmt.Errorf("cluster ID, registry endpoint, and server name are required")
	}
	if e := validateRegistryAddress(endpoint, serverName); e != nil {
		return e
	}
	if e := validateIntEnv("CLUSTER_STATE_MAX_MESSAGE_BYTES", registry.MinProtocolMessageBytes, registry.MaxProtocolMessageBytes); e != nil {
		return e
	}
	if e := validateIntEnv("CLUSTER_STATE_MAX_RESOURCES", 1, registry.MaxProjectedResources); e != nil {
		return e
	}
	if e := validateIntEnv("CLUSTER_STATE_MAX_CHUNK_RESOURCES", 1, registry.MaxSnapshotChunkResources); e != nil {
		return e
	}
	max := envInt("CLUSTER_STATE_MAX_MESSAGE_BYTES", registry.MaxProtocolMessageBytes)
	maxResources := envInt("CLUSTER_STATE_MAX_RESOURCES", registry.MaxProjectedResources)
	maxChunkResources := envInt("CLUSTER_STATE_MAX_CHUNK_RESOURCES", registry.MaxSnapshotChunkResources)
	if maxChunkResources > maxResources {
		return fmt.Errorf("CLUSTER_STATE_MAX_CHUNK_RESOURCES must not exceed CLUSTER_STATE_MAX_RESOURCES")
	}
	files := transport.TLSFiles{CertFile: os.Getenv("CLUSTER_STATE_TLS_CERT_FILE"), KeyFile: os.Getenv("CLUSTER_STATE_TLS_KEY_FILE"), CAFile: os.Getenv("CLUSTER_STATE_TLS_CA_FILE"), TrustDomain: os.Getenv("CLUSTER_STATE_TRUST_DOMAIN")}
	if _, e := transport.ClientTLS(files, serverName); e != nil {
		return fmt.Errorf("invalid cluster-state TLS configuration: %w", e)
	}
	rest, e := clusterstate.RestConfig(clusterstate.ClientOptions{Kubeconfig: os.Getenv("KUBECONFIG"), QPS: 20, Burst: 30, UserAgent: "cluster-state-agent"})
	if e != nil {
		return e
	}
	clients, e := clusterstate.NewClients(rest)
	if e != nil {
		return e
	}
	store, e := clusterstate.New(clients, clusterstate.Options{ClusterID: clusterID, ClusterName: clusterID})
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	changes := make(chan struct{}, 1)
	if e = store.OnChange(func(clusterstate.Change) {
		select {
		case changes <- struct{}{}:
		default:
		}
	}); e != nil {
		return e
	}
	syncCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if e = store.StartAndWait(ctx, syncCtx); e != nil {
		return e
	}
	backoff := time.Second
	for ctx.Err() == nil {
		e = syncOnce(ctx, store, changes, clusterID, endpoint, serverName, files, max, maxResources, maxChunkResources, func() { backoff = time.Second })
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-time.After(retryDelay(backoff, randomSample())):
		case <-ctx.Done():
			return nil
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}

func validateRegistryAddress(endpoint, serverName string) error {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || !clusterid.ValidHost(host) {
		return fmt.Errorf("registry endpoint must have a nonempty host and port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("registry endpoint port must be between 1 and 65535")
	}
	if strings.ContainsAny(serverName, "/: *\t\r\n") || !clusterid.ValidHost(serverName) {
		return fmt.Errorf("registry server name is invalid")
	}
	return nil
}

func syncOnce(ctx context.Context, store *clusterstate.Store, changes <-chan struct{}, clusterID, endpoint, serverName string, files transport.TLSFiles, max, maxResources, maxChunkResources int, onReady func()) error {
	tlsCfg, e := transport.ClientTLS(files, serverName)
	if e != nil {
		return e
	}
	cc, e := grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(max), grpc.MaxCallSendMsgSize(max)))
	if e != nil {
		return e
	}
	defer cc.Close()
	stream, e := v1.NewClusterStateClient(cc).Sync(ctx)
	if e != nil {
		return e
	}
	exchange := func(f *v1.AgentFrame) (*v1.ServerFrame, error) {
		if proto.Size(f) > max {
			return nil, fmt.Errorf("frame exceeds configured maximum")
		}
		if e := stream.Send(f); e != nil {
			return nil, callerError(ctx, e)
		}
		reply, e := stream.Recv()
		if e != nil {
			return nil, callerError(ctx, e)
		}
		if n := reply.GetNack(); n != nil {
			return nil, fmt.Errorf("registry nack: %s", n.Code)
		}
		return reply, nil
	}
	send := func(f *v1.AgentFrame) error { _, e := exchange(f); return e }
	hello, e := exchange(&v1.AgentFrame{Frame: &v1.AgentFrame_Hello{Hello: &v1.Hello{ClusterId: clusterID, ProtocolVersion: v1.Version}}})
	if e != nil {
		return e
	}
	epoch := hello.GetAck().Epoch
	if epoch == 0 {
		return fmt.Errorf("registry returned invalid session epoch")
	}
	seq := uint64(0)
	current := map[string]*v1.Resource{}
	snapshot := func() error {
		resources, e := store.SafeProjection(maxResources)
		if e != nil {
			return e
		}
		if e = send(&v1.AgentFrame{Frame: &v1.AgentFrame_BeginSnapshot{BeginSnapshot: &v1.BeginSnapshot{Epoch: epoch, BaseSeq: seq}}}); e != nil {
			return e
		}
		chunks, e := chunkResources(resources, max, maxChunkResources)
		if e != nil {
			return e
		}
		for _, ch := range chunks {
			if e = send(&v1.AgentFrame{Frame: &v1.AgentFrame_SnapshotChunk{SnapshotChunk: &v1.SnapshotChunk{Resources: ch}}}); e != nil {
				return e
			}
		}
		if e = send(&v1.AgentFrame{Frame: &v1.AgentFrame_CommitSnapshot{CommitSnapshot: &v1.CommitSnapshot{Epoch: epoch}}}); e != nil {
			return e
		}
		current = index(resources)
		return nil
	}
	if e = snapshot(); e != nil {
		return e
	}
	if onReady != nil {
		onReady()
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	rotate := time.NewTimer(certReconnectDelay(files.CertFile))
	defer rotate.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rotate.C:
			return nil
		case <-heartbeat.C:
			if e = send(&v1.AgentFrame{Frame: &v1.AgentFrame_Heartbeat{Heartbeat: &v1.Heartbeat{Epoch: epoch, Seq: seq}}}); e != nil {
				return e
			}
		case <-changes:
			resources, e := store.SafeProjection(maxResources)
			if e != nil {
				return e
			}
			next := index(resources)
			for k, old := range current {
				if _, ok := next[k]; !ok {
					seq++
					if e = send(&v1.AgentFrame{Frame: &v1.AgentFrame_Delta{Delta: &v1.Delta{Epoch: epoch, Seq: seq, Resource: old, Deleted: true}}}); e != nil {
						return e
					}
				}
			}
			for k, x := range next {
				if old := current[k]; old == nil || !proto.Equal(old, x) {
					seq++
					if e = send(&v1.AgentFrame{Frame: &v1.AgentFrame_Delta{Delta: &v1.Delta{Epoch: epoch, Seq: seq, Resource: x}}}); e != nil {
						return e
					}
				}
			}
			current = next
		}
	}
}

func callerError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func chunkResources(in []*v1.Resource, max, maxCount int) ([][]*v1.Resource, error) {
	if max < 1 || maxCount < 1 || maxCount > registry.MaxSnapshotChunkResources {
		return nil, fmt.Errorf("invalid snapshot chunk limits")
	}
	var out [][]*v1.Resource
	cur := []*v1.Resource{}
	for _, r := range in {
		if len(cur) == maxCount {
			out = append(out, cur)
			cur = []*v1.Resource{}
		}
		candidate := append(cur, r)
		f := &v1.AgentFrame{Frame: &v1.AgentFrame_SnapshotChunk{SnapshotChunk: &v1.SnapshotChunk{Resources: candidate}}}
		if proto.Size(f) > max {
			if len(cur) == 0 {
				return nil, fmt.Errorf("resource exceeds message cap")
			}
			out = append(out, cur)
			cur = []*v1.Resource{r}
			if proto.Size(&v1.AgentFrame{Frame: &v1.AgentFrame_SnapshotChunk{SnapshotChunk: &v1.SnapshotChunk{Resources: cur}}}) > max {
				return nil, fmt.Errorf("resource exceeds message cap")
			}
		} else {
			cur = candidate
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out, nil
}
func index(in []*v1.Resource) map[string]*v1.Resource {
	m := make(map[string]*v1.Resource, len(in))
	for _, x := range in {
		m[x.Kind+"\x00"+x.Uid] = x
	}
	return m
}
func certReconnectDelay(path string) time.Duration {
	b, e := os.ReadFile(path)
	if e != nil {
		return time.Second
	}
	p, _ := pem.Decode(b)
	if p == nil {
		return time.Second
	}
	c, e := x509.ParseCertificate(p.Bytes)
	if e != nil {
		return time.Second
	}
	d := time.Until(c.NotAfter.Add(-5 * time.Minute))
	if d > 10*time.Minute {
		return 10 * time.Minute
	}
	if d < time.Second {
		return time.Second
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
func retryDelay(base time.Duration, sample uint64) time.Duration {
	if base < time.Second {
		base = time.Second
	}
	return time.Duration(float64(base) * (0.8 + float64(sample%401)/1000))
}
func randomSample() uint64 {
	var b [8]byte
	if _, e := rand.Read(b[:]); e != nil {
		return 200
	}
	return binary.LittleEndian.Uint64(b[:])
}
