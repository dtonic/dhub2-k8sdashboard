package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterid"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Service struct {
	v1.UnimplementedClusterStateServer
	Registry        *registry.Registry
	TrustDomain     string
	MaxMessageBytes int
	MaxReplyBytes   int
	watchSubscribed func(string) // deterministic subscribe/snapshot race tests
}

func (s *Service) Sync(stream v1.ClusterState_SyncServer) error {
	if s.Registry == nil {
		return fmt.Errorf("registry unavailable")
	}
	max := s.MaxMessageBytes
	if max <= 0 {
		max = 4 << 20
	}
	id, err := agentID(stream.Context(), s.TrustDomain)
	if err != nil {
		return err
	}
	connected := false
	var generation uint64
	defer func() {
		if connected {
			s.Registry.DisconnectSession(id, generation)
		}
	}()
	for {
		f, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		size := proto.Size(f)
		if size > max {
			return status.Error(codes.ResourceExhausted, "frame exceeds maximum")
		}
		firstHello := false
		if f.GetHello() != nil && !connected {
			if generation, err = s.Registry.OpenSession(f.GetHello(), id); err != nil {
				return status.Error(codes.PermissionDenied, "agent identity rejected")
			}
			connected = true
			firstHello = true
		}
		if !connected {
			return status.Error(codes.FailedPrecondition, "hello required")
		}
		if f.Frame == nil {
			return status.Error(codes.InvalidArgument, "empty frame")
		}
		if !s.Registry.ConsumeIngressSession(id, generation, size) {
			return status.Error(codes.ResourceExhausted, "frame rate exceeded")
		}
		ack := &v1.Ack{}
		var nack *v1.Nack
		switch {
		case f.GetHello() != nil:
			if !firstHello {
				return status.Error(codes.InvalidArgument, "duplicate hello")
			}
			ack.Epoch = generation
			ack.NamespaceResources = true
		case f.GetBeginSnapshot() != nil:
			err = s.Registry.BeginSession(id, generation, f.GetBeginSnapshot())
		case f.GetSnapshotChunk() != nil:
			err = s.Registry.ChunkSession(id, generation, f.GetSnapshotChunk())
		case f.GetCommitSnapshot() != nil:
			ack, nack = s.Registry.CommitSession(id, generation, f.GetCommitSnapshot())
		case f.GetDelta() != nil:
			ack, nack = s.Registry.DeltaSession(id, generation, f.GetDelta())
		case f.GetHeartbeat() != nil:
			ack, nack = s.Registry.Heartbeat(id, generation, f.GetHeartbeat())
		default:
			err = fmt.Errorf("unsupported frame")
		}
		if err != nil {
			code := codes.FailedPrecondition
			if strings.Contains(err.Error(), "capacity") || strings.Contains(err.Error(), "limit") {
				code = codes.ResourceExhausted
			}
			return status.Error(code, "registry rejected frame")
		}
		out := &v1.ServerFrame{}
		if nack != nil {
			out.Frame = &v1.ServerFrame_Nack{Nack: nack}
		} else {
			out.Frame = &v1.ServerFrame_Ack{Ack: ack}
		}
		if err = stream.Send(out); err != nil {
			return err
		}
	}
}

func (s *Service) Query(ctx context.Context, q *v1.ScreenQuery) (*v1.ScreenReply, error) {
	if _, e := roleID(ctx, s.TrustDomain, "cluster-state-api"); e != nil {
		return nil, e
	}
	if s.Registry == nil {
		return nil, status.Error(codes.Unavailable, "registry unavailable")
	}
	req, err := screenRequest(q)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid screen query")
	}
	var projection *clusterstate.ScreenProjection
	var observed time.Time
	stale, err := s.Registry.View(q.ClusterId, func(snapshot *registry.Snapshot) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		observed = snapshot.ObservedAt
		projection, err = clusterstate.ProjectScreenContext(ctx, req, snapshot.Resources, time.Now())
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.Unavailable, "cluster state unavailable")
	}
	b, err := json.Marshal(projection)
	if err != nil {
		return nil, status.Error(codes.Internal, "screen projection failed")
	}
	maxReply := s.MaxReplyBytes
	if maxReply <= 0 {
		maxReply = 4 << 20
	}
	if len(b) > maxReply {
		return nil, status.Error(codes.ResourceExhausted, "screen reply exceeds maximum")
	}
	return &v1.ScreenReply{CanonicalJson: b, Stale: stale, ObservedUnixMs: observed.UnixMilli(), Accepted: proto.Clone(q).(*v1.ScreenQuery)}, nil
}

// Watch serves one configured cluster per stream. Per-cluster streams prevent
// a large snapshot or noisy agent from head-of-line blocking another cluster.
func (s *Service) Watch(req *v1.WatchRequest, stream v1.ClusterState_WatchServer) error {
	if _, err := roleID(stream.Context(), s.TrustDomain, "cluster-state-api"); err != nil {
		return err
	}
	if s.Registry == nil {
		return status.Error(codes.Unavailable, "registry unavailable")
	}
	if req == nil || req.ProtocolVersion != v1.Version || len(req.ClusterIds) != 1 || !clusterid.Valid(req.ClusterIds[0]) {
		return status.Error(codes.InvalidArgument, "invalid watch request")
	}
	id := req.ClusterIds[0]
	subscription, err := s.Registry.Subscribe([]string{id})
	if err != nil {
		return status.Error(codes.InvalidArgument, "invalid watch cluster")
	}
	defer subscription.Close()
	if s.watchSubscribed != nil {
		s.watchSubscribed(id)
	}
	var sentEpoch, sentSeq uint64
	send := func(frame *v1.WatchFrame) error {
		max := s.MaxMessageBytes
		if max <= 0 {
			max = 4 << 20
		}
		if proto.Size(frame) > max {
			return status.Error(codes.ResourceExhausted, "watch frame exceeds maximum")
		}
		return stream.Send(frame)
	}
	sendSnapshot := func() error {
		snapshot, err := s.Registry.CatalogSnapshot(id, 64<<20)
		if err != nil {
			return send(&v1.WatchFrame{ClusterId: id, Type: v1.WatchFrameType_WATCH_RESET})
		}
		if err = send(&v1.WatchFrame{ClusterId: id, Epoch: snapshot.Epoch, Seq: snapshot.Seq, Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: snapshot.Observed.UnixMilli()}); err != nil {
			return err
		}
		chunk := make([]*v1.CatalogResource, 0, 1000)
		flush := func() error {
			if len(chunk) == 0 {
				return nil
			}
			frame := &v1.WatchFrame{ClusterId: id, Epoch: snapshot.Epoch, Seq: snapshot.Seq, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, Resources: chunk, ObservedUnixMs: snapshot.Observed.UnixMilli()}
			if err := send(frame); err != nil {
				return err
			}
			chunk = make([]*v1.CatalogResource, 0, 1000)
			return nil
		}
		for _, x := range snapshot.Resources {
			candidate := append(chunk, x)
			frame := &v1.WatchFrame{ClusterId: id, Epoch: snapshot.Epoch, Seq: snapshot.Seq, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, Resources: candidate}
			max := s.MaxMessageBytes
			if max <= 0 {
				max = 4 << 20
			}
			if len(chunk) >= 1000 || proto.Size(frame) > max {
				if err := flush(); err != nil {
					return err
				}
			}
			chunk = append(chunk, x)
		}
		if err := flush(); err != nil {
			return err
		}
		if err := send(&v1.WatchFrame{ClusterId: id, Epoch: snapshot.Epoch, Seq: snapshot.Seq, Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: snapshot.Observed.UnixMilli()}); err != nil {
			return err
		}
		sentEpoch, sentSeq = snapshot.Epoch, snapshot.Seq
		return nil
	}
	if err := sendSnapshot(); err != nil {
		return err
	}
	for {
		change, ok := subscription.Next(stream.Context())
		if !ok {
			if stream.Context().Err() != nil {
				return status.FromContextError(stream.Context().Err()).Err()
			}
			return status.Error(codes.ResourceExhausted, "watch queue overflow")
		}
		if change.Expired {
			if err := send(&v1.WatchFrame{ClusterId: id, Type: v1.WatchFrameType_WATCH_EXPIRED, ObservedUnixMs: change.Observed.UnixMilli()}); err != nil {
				return err
			}
			sentEpoch, sentSeq = 0, 0
			continue
		}
		if change.Heartbeat {
			if err := send(&v1.WatchFrame{ClusterId: id, Epoch: change.Epoch, Seq: change.Seq, Type: v1.WatchFrameType_WATCH_HEARTBEAT, ObservedUnixMs: change.Observed.UnixMilli()}); err != nil {
				return err
			}
			continue
		}
		if change.Reset {
			if change.Epoch == sentEpoch && change.Seq <= sentSeq {
				continue
			}
			if err := send(&v1.WatchFrame{ClusterId: id, Type: v1.WatchFrameType_WATCH_RESET, ObservedUnixMs: change.Observed.UnixMilli()}); err != nil {
				return err
			}
			if err := sendSnapshot(); err != nil {
				return err
			}
			continue
		}
		if change.Change == nil {
			continue
		}
		if change.Change.Epoch == sentEpoch && change.Change.Seq <= sentSeq {
			continue
		}
		if change.Change.Epoch != sentEpoch || change.Change.Seq != sentSeq+1 {
			if err := send(&v1.WatchFrame{ClusterId: id, Type: v1.WatchFrameType_WATCH_RESET, ObservedUnixMs: change.Observed.UnixMilli()}); err != nil {
				return err
			}
			if err := sendSnapshot(); err != nil {
				return err
			}
			continue
		}
		if err := send(&v1.WatchFrame{ClusterId: id, Epoch: change.Change.Epoch, Seq: change.Change.Seq, Type: v1.WatchFrameType_WATCH_DELTA, Change: change.Change, ObservedUnixMs: change.Observed.UnixMilli()}); err != nil {
			return err
		}
		sentEpoch, sentSeq = change.Change.Epoch, change.Change.Seq
	}
}

var (
	namespacePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
)

func screenRequest(q *v1.ScreenQuery) (clusterstate.ScreenRequest, error) {
	if q == nil || !clusterid.Valid(q.ClusterId) {
		return clusterstate.ScreenRequest{}, fmt.Errorf("cluster")
	}
	screens := map[string]bool{"overview": true, "namespace-list": true, "namespace": true, "workload": true, "pod": true, "topology": true, "logs": true}
	if !screens[q.Screen] || q.Scope == nil || q.EventLimit == 0 || q.EventLimit > 1000 || q.UnhealthyLimit == 0 || q.UnhealthyLimit > 1000 || len(q.EntityUid) > 253 || len(q.Kind) > 64 || len(q.Name) > 253 {
		return clusterstate.ScreenRequest{}, fmt.Errorf("shape")
	}
	f := clusterstate.NamespaceFilter{All: q.Scope.All}
	if q.Scope.All {
		if len(q.Scope.Namespaces) != 0 {
			return clusterstate.ScreenRequest{}, fmt.Errorf("scope")
		}
	} else {
		if len(q.Scope.Namespaces) == 0 || len(q.Scope.Namespaces) > 256 || !sort.StringsAreSorted(q.Scope.Namespaces) {
			return clusterstate.ScreenRequest{}, fmt.Errorf("scope")
		}
		total := 0
		for i, ns := range q.Scope.Namespaces {
			if !namespacePattern.MatchString(ns) || (i > 0 && ns == q.Scope.Namespaces[i-1]) {
				return clusterstate.ScreenRequest{}, fmt.Errorf("scope")
			}
			total += len(ns)
		}
		if total > 8192 {
			return clusterstate.ScreenRequest{}, fmt.Errorf("scope")
		}
		f.List = append([]string(nil), q.Scope.Namespaces...)
	}
	if q.RequestedNamespace != "" && (!namespacePattern.MatchString(q.RequestedNamespace) || !f.Allows(q.RequestedNamespace)) {
		return clusterstate.ScreenRequest{}, fmt.Errorf("namespace")
	}
	if (q.Screen == "namespace" || q.Screen == "workload" || q.Screen == "pod") && q.RequestedNamespace == "" {
		return clusterstate.ScreenRequest{}, fmt.Errorf("namespace")
	}
	if q.Screen == "workload" && (q.Kind == "" || q.Name == "") || q.Screen == "pod" && q.Name == "" {
		return clusterstate.ScreenRequest{}, fmt.Errorf("entity")
	}
	return clusterstate.ScreenRequest{ClusterID: q.ClusterId, Screen: q.Screen, Namespaces: f, RequestedNamespace: q.RequestedNamespace, EntityUID: q.EntityUid, Kind: q.Kind, Name: q.Name, From: time.UnixMilli(q.FromUnixMs).UTC(), EventLimit: int(q.EventLimit), UnhealthyLimit: int(q.UnhealthyLimit)}, nil
}
func agentID(ctx context.Context, trust string) (string, error) {
	return roleID(ctx, trust, "cluster-state-agent")
}
func roleID(ctx context.Context, trust, role string) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", fmt.Errorf("peer missing")
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", fmt.Errorf("mTLS required")
	}
	return SPIFFEIdentity(ti.State, trust, role)
}
