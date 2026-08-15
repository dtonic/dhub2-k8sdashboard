package clusterstate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"
)

// GRPCRegistry resolves a request-local provider with exactly one Query RPC.
type GRPCRegistry struct {
	Client        v1.ClusterStateClient
	Health        healthv1.HealthClient
	MaxReplyBytes int
	Catalog       *RemoteCatalog
	Usage         *UsageStore
	WatchDial     func(context.Context) (v1.ClusterStateClient, io.Closer, error)
	WatchDelay    func(time.Duration) time.Duration
	ready         atomic.Bool
}

// StartWatch maintains one independent stream per cluster. It blocks until ctx
// cancellation; callers normally run it in one owned goroutine.
func (r *GRPCRegistry) StartWatch(ctx context.Context, clusterIDs []string, catalog *RemoteCatalog, onFrame func(*v1.WatchFrame)) error {
	if r == nil || r.Client == nil && r.WatchDial == nil || catalog == nil {
		return fmt.Errorf("watch unavailable")
	}
	if len(clusterIDs) == 0 || len(clusterIDs) > 64 {
		return fmt.Errorf("invalid watch clusters")
	}
	seen := make(map[string]struct{}, len(clusterIDs))
	for _, id := range clusterIDs {
		if !catalog.AllowsCluster(id) {
			return fmt.Errorf("unknown watch cluster")
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("duplicate watch cluster")
		}
		seen[id] = struct{}{}
	}
	var wg sync.WaitGroup
	for _, clusterID := range clusterIDs {
		id := clusterID
		wg.Add(1)
		go func() {
			defer wg.Done()
			backoff := time.Second
			for ctx.Err() == nil {
				client := r.Client
				var closer io.Closer
				var err error
				if r.WatchDial != nil {
					client, closer, err = r.WatchDial(ctx)
				}
				if err == nil && client == nil {
					err = fmt.Errorf("watch client unavailable")
				}
				var stream v1.ClusterState_WatchClient
				if err == nil {
					stream, err = client.Watch(ctx, &v1.WatchRequest{ClusterIds: []string{id}, ProtocolVersion: v1.Version})
				}
				if err == nil {
					for {
						frame, recvErr := stream.Recv()
						if recvErr != nil {
							err = recvErr
							break
						}
						if frame == nil || frame.ClusterId != id || proto.Size(frame) > 4<<20 {
							err = fmt.Errorf("invalid watch frame")
							break
						}
						if applyErr := catalog.Apply(frame); applyErr != nil {
							err = applyErr
							break
						}
						if frame.Type == v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT {
							backoff = time.Second
						}
						if onFrame != nil && (frame.Type == v1.WatchFrameType_WATCH_DELTA || frame.Type == v1.WatchFrameType_WATCH_EXPIRED || frame.Type == v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT) {
							onFrame(proto.Clone(frame).(*v1.WatchFrame))
						}
					}
				}
				if closer != nil {
					_ = closer.Close()
				}
				if ctx.Err() != nil {
					return
				}
				_ = catalog.Apply(&v1.WatchFrame{ClusterId: id, Type: v1.WatchFrameType_WATCH_RESET})
				delay := backoff
				if r.WatchDelay != nil {
					delay = r.WatchDelay(backoff)
				} else {
					delay += time.Duration(rand.Int64N(max(1, int64(backoff/5))))
				}
				if delay < time.Millisecond || delay > 36*time.Second {
					delay = backoff
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				if backoff < 30*time.Second {
					backoff *= 2
					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}
				}
			}
		}()
	}
	wg.Wait()
	return nil
}

func (r *GRPCRegistry) Ready() bool { return r != nil && r.Client != nil && r.ready.Load() }

func (r *GRPCRegistry) StartHealth(ctx context.Context, interval, timeout time.Duration) {
	if r == nil {
		return
	}
	if interval <= 0 {
		interval = time.Second
	}
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	check := func() {
		if r.Health == nil {
			r.ready.Store(false)
			return
		}
		checkCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res, err := r.Health.Check(checkCtx, &healthv1.HealthCheckRequest{Service: v1.ClusterState_ServiceDesc.ServiceName})
		r.ready.Store(err == nil && res.Status == healthv1.HealthCheckResponse_SERVING)
	}
	check()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.ready.Store(false)
			return
		case <-ticker.C:
			check()
		}
	}
}

func (r *GRPCRegistry) ForScreen(ctx context.Context, req ScreenRequest) (Provider, error) {
	if !r.Ready() {
		return nil, fmt.Errorf("registry unavailable")
	}
	namespaces := append([]string(nil), req.Namespaces.List...)
	sort.Strings(namespaces)
	req.Namespaces.List = namespaces
	req.From = time.UnixMilli(req.From.UnixMilli()).UTC()
	q := &v1.ScreenQuery{ClusterId: req.ClusterID, Screen: req.Screen, Scope: &v1.NamespaceScope{All: req.Namespaces.All, Namespaces: namespaces}, RequestedNamespace: req.RequestedNamespace, EntityUid: req.EntityUID, FromUnixMs: req.From.UnixMilli(), EventLimit: uint32(req.EventLimit), UnhealthyLimit: uint32(req.UnhealthyLimit), Kind: req.Kind, Name: req.Name}
	reply, err := r.Client.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	maxReply := r.MaxReplyBytes
	if maxReply <= 0 {
		maxReply = 4 << 20
	}
	if reply == nil {
		return nil, fmt.Errorf("invalid registry reply")
	}
	observed := time.UnixMilli(reply.GetObservedUnixMs())
	if !proto.Equal(q, reply.Accepted) || len(reply.CanonicalJson) == 0 || len(reply.CanonicalJson) > maxReply || bytes.Equal(bytes.TrimSpace(reply.CanonicalJson), []byte("null")) || reply.GetObservedUnixMs() <= 0 || observed.After(time.Now().Add(time.Minute)) {
		return nil, fmt.Errorf("invalid registry reply")
	}
	if err := rejectDuplicateKeys(reply.CanonicalJson); err != nil {
		return nil, fmt.Errorf("invalid registry reply")
	}
	if err := validateProjectionJSON(reply.CanonicalJson, req.Screen); err != nil {
		return nil, fmt.Errorf("invalid registry reply")
	}
	var data ScreenProjection
	dec := json.NewDecoder(bytes.NewReader(reply.CanonicalJson))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&data); err != nil {
		return nil, fmt.Errorf("invalid registry reply")
	}
	if err := requireJSONEOF(dec); err != nil || !reflect.DeepEqual(data.Request, req) {
		return nil, fmt.Errorf("registry reply request mismatch")
	}
	canonical, err := json.Marshal(data)
	if err != nil || !bytes.Equal(canonical, reply.CanonicalJson) {
		return nil, fmt.Errorf("non-canonical registry reply")
	}
	EnrichUsage(&data, r.Catalog, r.Usage)
	return &RemoteProvider{Data: &data, Stale: reply.Stale, Observed: observed}, nil
}

func validateProjectionJSON(b []byte, screen string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	required := map[string][]string{
		"overview":       {"request", "nodes", "podsHealth", "workloadsHealth", "unhealthy", "events"},
		"namespace-list": {"request", "namespaces"},
		"namespace":      {"request", "namespace", "workloads", "events"},
		"workload":       {"request", "resolvedUid", "workload", "workloadOwners", "workloadPods", "events"},
		"pod":            {"request", "resolvedUid", "pod", "podSummary", "podOwners", "events"},
		"topology":       {"request", "topology"},
		"logs":           {"request", "events"},
	}[screen]
	if len(required) == 0 || len(fields) != len(required) {
		return fmt.Errorf("projection shape")
	}
	nullable := map[string]bool{"namespace": true, "workload": true, "pod": true, "podSummary": true}
	for _, key := range required {
		raw, ok := fields[key]
		if !ok {
			return fmt.Errorf("projection field missing")
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && !nullable[key] {
			return fmt.Errorf("projection field null")
		}
	}
	return nil
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

func rejectDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	return scanJSONValue(dec)
}

func scanJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("invalid object key")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return fmt.Errorf("unexpected delimiter")
	}
}

var _ ProviderRegistry = (*GRPCRegistry)(nil)
