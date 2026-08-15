package clusterstate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

type fakeUsageMetrics struct {
	mu    sync.Mutex
	calls map[string]int
}

type failingUsageMetrics struct{}

func (failingUsageMetrics) Trends(context.Context, datasource.Target, datasource.Window, []string) ([]contract.TrendPanel, error) {
	return nil, errors.New("failed")
}
func (failingUsageMetrics) Usage(context.Context, string) (map[string]contract.ContainerUsage, error) {
	return nil, errors.New("failed")
}

type sequenceUsageMetrics struct {
	mu    sync.Mutex
	calls int
}

func (*sequenceUsageMetrics) Trends(context.Context, datasource.Target, datasource.Window, []string) ([]contract.TrendPanel, error) {
	return nil, errors.New("unused")
}
func (s *sequenceUsageMetrics) Usage(context.Context, string) (map[string]contract.ContainerUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 3 {
		return map[string]contract.ContainerUsage{"a": {CPUMilli: 1}}, nil
	}
	return nil, errors.New("failed")
}

func (*fakeUsageMetrics) Trends(context.Context, datasource.Target, datasource.Window, []string) ([]contract.TrendPanel, error) {
	return nil, errors.New("unused")
}

func (f *fakeUsageMetrics) Usage(ctx context.Context, clusterID string) (map[string]contract.ContainerUsage, error) {
	f.mu.Lock()
	f.calls[clusterID]++
	f.mu.Unlock()
	if clusterID == "a" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return map[string]contract.ContainerUsage{
		"same":    {CPUMilli: 200, MemoryMib: 300},
		"unknown": {CPUMilli: 999, MemoryMib: 999},
	}, nil
}

func (f *fakeUsageMetrics) count(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

func TestUsagePollerClusterIsolationFilteringExpiryAndCancellation(t *testing.T) {
	catalog, err := NewRemoteCatalog([]string{"a", "b"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		applyCatalogSnapshot(t, catalog, id, 1, 0, &v1.CatalogResource{Kind: v1.KindPod, Uid: "same", Namespace: "ns", Name: "pod"})
	}
	store, err := NewUsageStore([]string{"a", "b"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	metrics := &fakeUsageMetrics{calls: map[string]int{}}
	poller := &UsagePoller{Metrics: metrics, Catalog: catalog, Store: store, Interval: time.Hour, Timeout: 20 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond, Delay: func(time.Duration) time.Duration { return time.Millisecond }}
	invalid := *poller
	invalid.RetryMin, invalid.RetryMax = time.Minute, time.Second
	if err = invalid.Run(context.Background(), []string{"a"}); err == nil {
		t.Fatal("invalid retry range started workers")
	}
	invalid = *poller
	invalid.RetryMin, invalid.RetryMax = 31*time.Second, 0
	if err = invalid.Run(context.Background(), []string{"a"}); err == nil {
		t.Fatal("retry minimum above default maximum accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = poller.Run(ctx, []string{"a", "b"})
		close(done)
	}()
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		if value, ok := store.Lookup("b", "same"); ok && value.CPUMilli == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B immediate poll was blocked by A; calls A/B=%d/%d", metrics.count("a"), metrics.count("b"))
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := store.Lookup("b", "unknown"); ok {
		t.Fatal("unknown catalog UID admitted")
	}
	if err = catalog.Apply(&v1.WatchFrame{ClusterId: "b", Type: v1.WatchFrameType_WATCH_RESET}); err != nil {
		t.Fatal(err)
	}
	if value, ok := store.Lookup("b", "same"); !ok || value.CPUMilli != 200 {
		t.Fatal("stale-window reset discarded last-good usage")
	}
	if err = catalog.Apply(&v1.WatchFrame{ClusterId: "b", Type: v1.WatchFrameType_WATCH_EXPIRED, ObservedUnixMs: 2000}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(200 * time.Millisecond)
	for {
		if _, ok := store.Lookup("b", "same"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired catalog usage was not cleared")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll timeout/backoff goroutines did not drain")
	}
	if metrics.count("a") == 0 || metrics.count("b") == 0 {
		t.Fatalf("workers did not start independently: A/B=%d/%d", metrics.count("a"), metrics.count("b"))
	}
}

func TestUsagePollerExponentialBackoffResetsOnlyAfterSuccess(t *testing.T) {
	catalog, _ := NewRemoteCatalog([]string{"a"}, 1)
	applyCatalogSnapshot(t, catalog, "a", 1, 0, &v1.CatalogResource{Kind: v1.KindPod, Uid: "a", Namespace: "ns", Name: "a"})
	store, _ := NewUsageStore([]string{"a"}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var delays []time.Duration
	polls := 0
	poller := &UsagePoller{Metrics: &sequenceUsageMetrics{}, Catalog: catalog, Store: store, Interval: 10 * time.Millisecond, Timeout: time.Second, RetryMin: 2 * time.Millisecond, RetryMax: 8 * time.Millisecond}
	poller.Delay = func(delay time.Duration) time.Duration {
		mu.Lock()
		delays = append(delays, delay)
		mu.Unlock()
		return time.Millisecond
	}
	poller.AfterPoll = func(_ string, _ error) {
		polls++
		if polls == 4 {
			cancel()
		}
	}
	if err := poller.Run(ctx, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delays) < 4 || delays[0] != 2*time.Millisecond || delays[1] != 4*time.Millisecond || delays[2] != 10*time.Millisecond || delays[3] != 2*time.Millisecond {
		t.Fatalf("backoff=%v", delays)
	}
}
