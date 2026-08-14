package cache_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
)

func baseIdentity() cache.Identity {
	return cache.Identity{Dashboard: "overview", QueryRef: "metrics.cpu", Range: "1h", StepSeconds: 60,
		Scope:  cache.ScopeIdentity{Clusters: []cache.ClusterIdentity{{ID: "c", Namespaces: []string{"b", "a"}}}},
		Params: []cache.Param{{Name: "z", Values: []string{"2", "1"}}}}
}

type countingObserver struct{ requests, failures, loads atomic.Int32 }

func (o *countingObserver) ObserveCache(cache.Result) { o.requests.Add(1) }
func (o *countingObserver) ObserveCacheError(string)  { o.failures.Add(1) }
func (o *countingObserver) ObserveCacheLoad()         { o.loads.Add(1) }

func TestCacheMetricsOncePerRequestAndSingleRecoveryProbe(t *testing.T) {
	obs := &countingObserver{}
	now := time.Now()
	c := cache.New(cache.Config{DefaultTTL: time.Minute, RedisAddr: "127.0.0.1:1", RedisOpTimeout: 10 * time.Millisecond, RedisCooldown: time.Second, Observer: obs, Now: func() time.Time { return now }})
	defer c.Close()
	_, _ = c.Bytes(context.Background(), "first", time.Minute, func(context.Context) ([]byte, error) { return []byte("v"), nil })
	if obs.requests.Load() != 1 || obs.failures.Load() != 1 {
		t.Fatalf("initial requests=%d failures=%d", obs.requests.Load(), obs.failures.Load())
	}
	now = now.Add(2 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = c.Bytes(context.Background(), string(rune('a'+n)), time.Minute, func(context.Context) ([]byte, error) { return []byte("v"), nil })
		}(i)
	}
	wg.Wait()
	if obs.requests.Load() != 21 {
		t.Fatalf("request metric count=%d", obs.requests.Load())
	}
	if obs.failures.Load() != 2 {
		t.Fatalf("cooldown recovery probes=%d, want one additional failure", obs.failures.Load())
	}
}

func TestCacheFailureAndCancellationHaveOneOutcome(t *testing.T) {
	obs := &countingObserver{}
	c := cache.New(cache.Config{DefaultTTL: time.Minute, Observer: obs})
	_, err := c.Bytes(context.Background(), "error", time.Minute, func(context.Context) ([]byte, error) { return nil, errors.New("failed") })
	if err == nil {
		t.Fatal("expected error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Bytes(ctx, "cancel", time.Minute, func(context.Context) ([]byte, error) { return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if obs.requests.Load() != 2 || obs.loads.Load() != 1 {
		t.Fatalf("outcomes=%d loads=%d", obs.requests.Load(), obs.loads.Load())
	}
}

func TestBytesFollowerCancelsPromptly(t *testing.T) {
	c := cache.NewTTL(time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan []byte, 1)
	var calls atomic.Int32
	go func() {
		got, _ := c.Bytes(context.Background(), "k", time.Minute, func(context.Context) ([]byte, error) {
			calls.Add(1)
			close(started)
			<-release
			return []byte("v"), nil
		})
		leaderDone <- got
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	followerDone := make(chan error, 1)
	go func() {
		_, err := c.Bytes(ctx, "k", time.Minute, func(context.Context) ([]byte, error) {
			calls.Add(1)
			return []byte("duplicate"), nil
		})
		followerDone <- err
	}()
	time.Sleep(10 * time.Millisecond)
	start := time.Now()
	cancel()
	err := <-followerDone
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("follower cancellation was not prompt")
	}
	thirdDone := make(chan []byte, 1)
	go func() {
		got, _ := c.Bytes(context.Background(), "k", time.Minute, func(context.Context) ([]byte, error) {
			calls.Add(1)
			return []byte("duplicate"), nil
		})
		thirdDone <- got
	}()
	time.Sleep(10 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("third request started another producer: calls=%d", calls.Load())
	}
	close(release)
	if got := <-leaderDone; string(got) != "v" {
		t.Fatalf("leader result=%q", got)
	}
	if got := <-thirdDone; string(got) != "v" {
		t.Fatalf("third result=%q", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("producer calls=%d", calls.Load())
	}
}

func TestBytesSingleflightMetricsAndImmutableConcurrentReads(t *testing.T) {
	obs := &countingObserver{}
	c := cache.New(cache.Config{DefaultTTL: time.Minute, Observer: obs})
	var calls atomic.Int32
	release := make(chan struct{})
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := c.Bytes(context.Background(), "shared", time.Minute, func(context.Context) ([]byte, error) { calls.Add(1); <-release; return []byte("immutable"), nil })
			if err != nil || string(b) != "immutable" {
				t.Errorf("b=%q err=%v", b, err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 || obs.loads.Load() != 1 || obs.requests.Load() != n {
		t.Fatalf("calls=%d loads=%d outcomes=%d", calls.Load(), obs.loads.Load(), obs.requests.Load())
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _ := c.Bytes(context.Background(), "shared", time.Minute, func(context.Context) ([]byte, error) { return nil, nil })
			_ = b[len(b)-1]
		}()
	}
	wg.Wait()
}

func TestLocalByteBudgetEvictsLRU(t *testing.T) {
	c := cache.New(cache.Config{DefaultTTL: time.Minute, MaxEntries: 10, MaxValueBytes: 10, MaxLocalBytes: 3})
	for _, k := range []string{"a", "b"} {
		_, err := c.Bytes(context.Background(), k, time.Minute, func(context.Context) ([]byte, error) { return []byte("12"), nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != 1 {
		t.Fatalf("byte budget retained %d entries", c.Len())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c := cache.NewTTL(time.Second)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkLocalHit1B(b *testing.B)   { benchmarkLocalHit(b, []byte("x")) }
func BenchmarkLocalHit4MiB(b *testing.B) { benchmarkLocalHit(b, make([]byte, 4<<20)) }
func benchmarkLocalHit(b *testing.B, payload []byte) {
	c := cache.New(cache.Config{DefaultTTL: time.Minute, MaxValueBytes: len(payload) + 1, MaxLocalBytes: len(payload) + 1})
	_, _ = c.Bytes(context.Background(), "k", time.Minute, func(context.Context) ([]byte, error) { return payload, nil })
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Bytes(context.Background(), "k", time.Minute, func(context.Context) ([]byte, error) { return nil, nil })
	}
}

func BenchmarkSingleflightContention(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := cache.NewTTL(time.Minute)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for n := 0; n < 16; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _ = c.Bytes(context.Background(), "k", time.Minute, func(context.Context) ([]byte, error) { return []byte("x"), nil })
			}()
		}
		close(start)
		wg.Wait()
	}
}

func TestCanonicalIdentitySeparatesSecurityAndQuerySemantics(t *testing.T) {
	base := baseIdentity()
	want := base.Key()
	reordered := baseIdentity()
	reordered.Scope.Clusters[0].Namespaces = []string{"a", "b"}
	if reordered.Key() != want {
		t.Fatal("ordering-equivalent identity did not match")
	}
	tests := []struct {
		name   string
		mutate func(*cache.Identity)
	}{
		{"dashboard", func(i *cache.Identity) { i.Dashboard = "logs" }}, {"queryRef", func(i *cache.Identity) { i.QueryRef = "metrics.mem" }},
		{"namespace", func(i *cache.Identity) { i.Scope.Clusters[0].Namespaces = []string{"a"} }},
		{"range", func(i *cache.Identity) { i.Range = "1d" }}, {"step", func(i *cache.Identity) { i.StepSeconds = 300 }},
		{"params", func(i *cache.Identity) { i.Params[0].Values = []string{"3"} }},
		{"repeated-value-order", func(i *cache.Identity) { i.Params[0].Values = []string{"1", "2"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := baseIdentity()
			tt.mutate(&got)
			if got.Key() == want {
				t.Fatal("identity collision")
			}
		})
	}
}

func TestIdentityKeyDoesNotMutateCallerSlices(t *testing.T) {
	i := baseIdentity()
	_ = i.Key()
	if strings.Join(i.Scope.Clusters[0].Namespaces, ",") != "b,a" || strings.Join(i.Params[0].Values, ",") != "2,1" {
		t.Fatalf("identity mutated: %+v", i)
	}
}

func TestCanonicalIdentitySharesEqualAuthorizationAcrossSubjects(t *testing.T) {
	// Subject is intentionally absent; it is used only by the user request limiter.
	if baseIdentity().Key() != baseIdentity().Key() {
		t.Fatal("equal authorization scopes must share a key")
	}
}

func TestTTLPolicyHistoricalBoundaryAndMixedState(t *testing.T) {
	now := time.Unix(10_000, 0)
	p := cache.TTLPolicy{State: 5 * time.Second, Short: 20 * time.Second, Historical: time.Hour, HistoricalSafety: time.Minute}
	if got := p.For(cache.Historical, now.Add(-time.Minute), now); got != time.Hour {
		t.Fatalf("boundary=%v", got)
	}
	if got := p.For(cache.Historical, now.Add(-time.Minute+time.Nanosecond), now); got != 5*time.Second {
		t.Fatalf("unsafe historical=%v", got)
	}
	if got := p.For(cache.State, now.Add(-24*time.Hour), now); got != 5*time.Second {
		t.Fatalf("mixed current response got long TTL: %v", got)
	}
}

func TestLocalCapacityAndValueLimit(t *testing.T) {
	c := cache.New(cache.Config{DefaultTTL: time.Minute, MaxEntries: 2, MaxValueBytes: 2})
	for _, k := range []string{"a", "b", "c"} {
		_, err := c.Bytes(context.Background(), k, time.Minute, func(context.Context) ([]byte, error) { return []byte(k), nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != 2 {
		t.Fatalf("len=%d", c.Len())
	}
	_, err := c.Bytes(context.Background(), "large", time.Minute, func(context.Context) ([]byte, error) { return []byte("123"), nil })
	if !errors.Is(err, cache.ErrValueTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestRedisFailureCooldownBoundsLatency(t *testing.T) {
	c := cache.New(cache.Config{DefaultTTL: time.Minute, RedisAddr: "127.0.0.1:1", RedisOpTimeout: 20 * time.Millisecond, RedisCooldown: time.Second})
	defer c.Close()
	start := time.Now()
	for i := 0; i < 3; i++ {
		_, err := c.Bytes(context.Background(), string(rune('a'+i)), time.Minute, func(context.Context) ([]byte, error) { return []byte("ok"), nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("Redis cooldown did not bound fallback: %v", elapsed)
	}
}
