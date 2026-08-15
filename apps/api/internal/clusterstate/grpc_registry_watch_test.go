package clusterstate

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type scriptedWatchClient struct {
	mu       sync.Mutex
	attempts int
}

func (*scriptedWatchClient) Sync(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[v1.AgentFrame, v1.ServerFrame], error) {
	return nil, io.EOF
}
func (*scriptedWatchClient) Query(context.Context, *v1.ScreenQuery, ...grpc.CallOption) (*v1.ScreenReply, error) {
	return nil, io.EOF
}
func (c *scriptedWatchClient) Watch(ctx context.Context, _ *v1.WatchRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[v1.WatchFrame], error) {
	c.mu.Lock()
	attempt := c.attempts
	c.attempts++
	c.mu.Unlock()
	valid := []*v1.WatchFrame{{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1000}, {ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, Resources: []*v1.CatalogResource{{Kind: v1.KindPod, Uid: "p", Namespace: "ns", Name: "p"}}, ObservedUnixMs: 1000}, {ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: 1000}}
	if attempt == 0 {
		return &scriptedWatchStream{ctx: ctx, frames: valid}, nil
	}
	if attempt == 1 {
		return &scriptedWatchStream{ctx: ctx, frames: []*v1.WatchFrame{{ClusterId: "wrong", Type: v1.WatchFrameType_WATCH_RESET}}}, nil
	}
	return &scriptedWatchStream{ctx: ctx, block: true}, nil
}
func (c *scriptedWatchClient) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.attempts }

type scriptedWatchStream struct {
	ctx    context.Context
	frames []*v1.WatchFrame
	block  bool
}

func (s *scriptedWatchStream) Recv() (*v1.WatchFrame, error) {
	if len(s.frames) > 0 {
		x := s.frames[0]
		s.frames = s.frames[1:]
		return x, nil
	}
	if s.block {
		<-s.ctx.Done()
		return nil, s.ctx.Err()
	}
	return nil, io.EOF
}
func (*scriptedWatchStream) Header() (metadata.MD, error) { return nil, nil }
func (*scriptedWatchStream) Trailer() metadata.MD         { return nil }
func (*scriptedWatchStream) CloseSend() error             { return nil }
func (s *scriptedWatchStream) Context() context.Context   { return s.ctx }
func (*scriptedWatchStream) SendMsg(any) error            { return nil }
func (*scriptedWatchStream) RecvMsg(any) error            { return io.EOF }

type countCloser struct{ n *atomic.Int32 }

func (c countCloser) Close() error { c.n.Add(1); return nil }

func TestStartWatchReconnectStrictFrameAndCancellationLifecycle(t *testing.T) {
	catalog, _ := NewRemoteCatalog([]string{"a"}, 10)
	client := &scriptedWatchClient{}
	var closes atomic.Int32
	r := &GRPCRegistry{WatchDial: func(context.Context) (v1.ClusterStateClient, io.Closer, error) {
		return client, countCloser{&closes}, nil
	}, WatchDelay: func(time.Duration) time.Duration { return time.Millisecond }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var callbacks atomic.Int32
	var callbackType atomic.Int32
	go func() {
		_ = r.StartWatch(ctx, []string{"a"}, catalog, func(frame *v1.WatchFrame) {
			callbacks.Add(1)
			callbackType.Store(int32(frame.Type))
		})
		close(done)
	}()
	if err := r.StartWatch(context.Background(), []string{"a", "a"}, catalog, nil); err == nil {
		t.Fatal("duplicate cluster workers accepted")
	}
	if err := r.StartWatch(context.Background(), []string{"unknown"}, catalog, nil); err == nil {
		t.Fatal("unknown cluster worker accepted")
	}
	deadline := time.Now().Add(time.Second)
	for client.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := catalog.CatalogPods("a", "", 0); len(got) != 1 || got[0].UID != "p" {
		t.Fatalf("invalid reconnect replaced last-good: %v", got)
	}
	if callbacks.Load() != 1 || callbackType.Load() != int32(v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT) {
		t.Fatalf("snapshot emitted callbacks=%d last=%s, want one atomic commit", callbacks.Load(), v1.WatchFrameType(callbackType.Load()))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch goroutine/stream/timer leaked")
	}
	if client.count() < 3 || closes.Load() != int32(client.count()) {
		t.Fatalf("attempts=%d closes=%d", client.count(), closes.Load())
	}
}

func TestWatchAndHealthFailClosedBeforeStartingWorkers(t *testing.T) {
	catalog, err := NewRemoteCatalog([]string{"a"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = "a"
	}
	for name, tc := range map[string]struct {
		registry *GRPCRegistry
		ids      []string
		catalog  *RemoteCatalog
	}{
		"nil receiver":   {nil, []string{"a"}, catalog},
		"no client":      {&GRPCRegistry{}, []string{"a"}, catalog},
		"nil catalog":    {&GRPCRegistry{Client: &scriptedWatchClient{}}, []string{"a"}, nil},
		"empty clusters": {&GRPCRegistry{Client: &scriptedWatchClient{}}, nil, catalog},
		"too many":       {&GRPCRegistry{Client: &scriptedWatchClient{}}, tooMany, catalog},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.registry.StartWatch(context.Background(), tc.ids, tc.catalog, nil); err == nil {
				t.Fatal("invalid watch configuration started workers")
			}
		})
	}

	// Dial failures and nil clients use bounded retry and stop with the caller;
	// neither path can expose or clear the last-good catalog.
	for name, dial := range map[string]func(context.Context) (v1.ClusterStateClient, io.Closer, error){
		"dial error": func(context.Context) (v1.ClusterStateClient, io.Closer, error) {
			return nil, nil, io.ErrUnexpectedEOF
		},
		"nil client": func(context.Context) (v1.ClusterStateClient, io.Closer, error) {
			return nil, nil, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			attempts := atomic.Int32{}
			r := &GRPCRegistry{WatchDial: func(ctx context.Context) (v1.ClusterStateClient, io.Closer, error) {
				attempts.Add(1)
				return dial(ctx)
			}, WatchDelay: func(time.Duration) time.Duration { return time.Millisecond }}
			done := make(chan error, 1)
			go func() { done <- r.StartWatch(ctx, []string{"a"}, catalog, nil) }()
			deadline := time.Now().Add(time.Second)
			for attempts.Load() < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			cancel()
			if err := <-done; err != nil || attempts.Load() < 2 {
				t.Fatalf("attempts=%d err=%v", attempts.Load(), err)
			}
		})
	}

	var nilRegistry *GRPCRegistry
	nilRegistry.StartHealth(context.Background(), 0, 0)
	if nilRegistry.Ready() || (&GRPCRegistry{}).Ready() {
		t.Fatal("registry without client/health reported ready")
	}
	healthCtx, cancel := context.WithCancel(context.Background())
	healthDone := make(chan struct{})
	r := &GRPCRegistry{Client: &scriptedWatchClient{}}
	go func() { r.StartHealth(healthCtx, 0, 0); close(healthDone) }()
	cancel()
	select {
	case <-healthDone:
	case <-time.After(time.Second):
		t.Fatal("health loop did not stop")
	}
	if r.Ready() {
		t.Fatal("missing health client reported ready")
	}
}
