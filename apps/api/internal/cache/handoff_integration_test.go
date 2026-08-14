//go:build integration

package cache

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type resultChannel chan Result

func (c resultChannel) ObserveCache(result Result) { c <- result }

func waitForHandoffSignal(ctx context.Context, signal <-chan struct{}) bool {
	select {
	case <-signal:
		return true
	case <-ctx.Done():
		return false
	}
}

func TestHandoffBarrierHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForHandoffSignal(ctx, make(chan struct{})) {
		t.Fatal("blocked barrier reported a signal after cancellation")
	}
}

func TestRedisLockHandoffRechecksPublishedValue(t *testing.T) {
	addr := os.Getenv("REDIS_ITEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_ITEST_ADDR is not set")
	}

	for iteration := 0; iteration < 20; iteration++ {
		t.Run(fmt.Sprintf("iteration-%02d", iteration), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			key := fmt.Sprintf("handoff:%d:%d", time.Now().UnixNano(), iteration)
			results := make(resultChannel, 1)
			cfg := Config{
				DefaultTTL: time.Second, RedisAddr: addr, RedisOpTimeout: 200 * time.Millisecond,
				LockTTL: 4 * time.Second, LockWait: 4 * time.Second,
			}
			owner := New(cfg)
			waiterCfg := cfg
			waiterCfg.Observer = results
			waiter := New(waiterCfg)

			producerStarted := make(chan struct{})
			releaseProducer := make(chan struct{})
			ownerDone := make(chan struct{})
			waiterMissed := make(chan struct{})
			allowLockRetry := make(chan struct{})
			var hookOnce sync.Once
			var releaseOnce sync.Once
			var retryOnce sync.Once
			var workers sync.WaitGroup
			t.Cleanup(func() {
				cancel()
				releaseOnce.Do(func() { close(releaseProducer) })
				retryOnce.Do(func() { close(allowLockRetry) })
				drained := make(chan struct{})
				go func() { workers.Wait(); close(drained) }()
				select {
				case <-drained:
				case <-time.After(time.Second):
					t.Error("handoff workers did not drain after cancellation")
				}
				_ = owner.Close()
				_ = waiter.Close()
			})
			waiter.afterWaitMiss = func() {
				hookOnce.Do(func() { close(waiterMissed) })
				waitForHandoffSignal(ctx, allowLockRetry)
			}

			var producerCalls atomic.Int32
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer close(ownerDone)
				_, _ = owner.Bytes(ctx, key, time.Second, func(context.Context) ([]byte, error) {
					producerCalls.Add(1)
					close(producerStarted)
					if !waitForHandoffSignal(ctx, releaseProducer) {
						return nil, ctx.Err()
					}
					return []byte("owner-value"), nil
				})
			}()
			if !waitForHandoffSignal(ctx, producerStarted) {
				t.Fatalf("iteration %d: producer did not start: %v", iteration, ctx.Err())
			}

			type response struct {
				value []byte
				err   error
			}
			waiterDone := make(chan response, 1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				value, err := waiter.Bytes(ctx, key, time.Second, func(context.Context) ([]byte, error) {
					producerCalls.Add(1)
					return []byte("duplicate"), nil
				})
				waiterDone <- response{value: value, err: err}
			}()
			select {
			case <-waiterMissed:
			case <-ctx.Done():
				t.Fatalf("iteration %d: waiter did not reach the lock handoff: %v", iteration, ctx.Err())
			}
			releaseOnce.Do(func() { close(releaseProducer) })
			if !waitForHandoffSignal(ctx, ownerDone) { // published value and deferred unlock are complete
				t.Fatalf("iteration %d: owner did not publish: %v", iteration, ctx.Err())
			}
			retryOnce.Do(func() { close(allowLockRetry) })

			var got response
			select {
			case got = <-waiterDone:
			case <-ctx.Done():
				t.Fatalf("iteration %d: waiter did not complete: %v", iteration, ctx.Err())
			}
			if got.err != nil || string(got.value) != "owner-value" {
				t.Fatalf("iteration %d: value=%q err=%v", iteration, got.value, got.err)
			}
			if calls := producerCalls.Load(); calls != 1 {
				t.Fatalf("iteration %d: producer calls=%d, want 1", iteration, calls)
			}
			var result Result
			select {
			case result = <-results:
			case <-ctx.Done():
				t.Fatalf("iteration %d: observer did not receive a result: %v", iteration, ctx.Err())
			}
			if result != ResultCoalesced {
				t.Fatalf("iteration %d: result=%s, want %s", iteration, result, ResultCoalesced)
			}
			_ = owner.Close()
			_ = waiter.Close()
			cancel()
		})
	}
}
