//go:build integration

package cache_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
)

func TestRedisCrossInstanceCollapseAndTTL(t *testing.T) {
	addr := os.Getenv("REDIS_ITEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_ITEST_ADDR is not set")
	}
	cfg := cache.Config{DefaultTTL: time.Second, RedisAddr: addr, RedisOpTimeout: 100 * time.Millisecond, LockTTL: 4 * time.Second, LockWait: 4 * time.Second}
	a, b := cache.New(cfg), cache.New(cfg)
	defer a.Close()
	defer b.Close()
	key := cache.Identity{Dashboard: "itest", QueryRef: time.Now().String()}.Key()
	var calls atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range []*cache.TTL{a, b} {
		wg.Add(1)
		go func(c *cache.TTL) {
			defer wg.Done()
			<-start
			got, err := c.Bytes(context.Background(), key, 300*time.Millisecond, func(context.Context) ([]byte, error) {
				calls.Add(1)
				time.Sleep(2200 * time.Millisecond)
				return []byte("value"), nil
			})
			if err != nil || string(got) != "value" {
				t.Errorf("got=%q err=%v", got, err)
			}
		}(c)
	}
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("producer calls=%d", calls.Load())
	}
	time.Sleep(350 * time.Millisecond)
	_, err := b.Bytes(context.Background(), key, 300*time.Millisecond, func(context.Context) ([]byte, error) { calls.Add(1); return []byte("new"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("TTL did not expire: calls=%d", calls.Load())
	}
}

func TestRedisHitSeedsL1OnlyForRemainingTTL(t *testing.T) {
	addr := os.Getenv("REDIS_ITEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_ITEST_ADDR is not set")
	}
	now := time.Now()
	cfg := cache.Config{DefaultTTL: time.Second, RedisAddr: addr, RedisOpTimeout: 100 * time.Millisecond, Now: func() time.Time { return now }}
	writer, reader := cache.New(cfg), cache.New(cfg)
	defer writer.Close()
	defer reader.Close()
	key := cache.Identity{Dashboard: "remaining-ttl", QueryRef: time.Now().String()}.Key()
	_, err := writer.Bytes(context.Background(), key, 600*time.Millisecond, func(context.Context) ([]byte, error) { return []byte("remote"), nil })
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	var calls atomic.Int32
	got, err := reader.Bytes(context.Background(), key, 600*time.Millisecond, func(context.Context) ([]byte, error) { calls.Add(1); return []byte("producer"), nil })
	if err != nil || string(got) != "remote" || calls.Load() != 0 {
		t.Fatalf("Redis hit got=%q err=%v calls=%d", got, err, calls.Load())
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	defer rdb.Close()
	if err := rdb.Del(context.Background(), key).Err(); err != nil {
		t.Fatal(err)
	}
	got, err = reader.Bytes(context.Background(), key, 600*time.Millisecond, func(context.Context) ([]byte, error) { calls.Add(1); return []byte("producer"), nil })
	if err != nil || string(got) != "remote" || calls.Load() != 0 {
		t.Fatalf("repeat did not use L1: got=%q calls=%d err=%v", got, calls.Load(), err)
	}
	now = now.Add(400 * time.Millisecond)
	got, err = reader.Bytes(context.Background(), key, 600*time.Millisecond, func(context.Context) ([]byte, error) { calls.Add(1); return []byte("producer"), nil })
	if err != nil || string(got) != "producer" || calls.Load() != 1 {
		t.Fatalf("L1 exceeded Redis remaining TTL: got=%q calls=%d err=%v", got, calls.Load(), err)
	}
}

func TestRedisOwnerErrorHandsOffToOneWaiter(t *testing.T) {
	addr := os.Getenv("REDIS_ITEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_ITEST_ADDR is not set")
	}
	cfg := cache.Config{DefaultTTL: time.Second, RedisAddr: addr, RedisOpTimeout: 100 * time.Millisecond, LockTTL: 2 * time.Second, LockWait: time.Second}
	a, b := cache.New(cfg), cache.New(cfg)
	defer a.Close()
	defer b.Close()
	key := cache.Identity{Dashboard: "handoff", QueryRef: time.Now().String()}.Key()
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	var calls, concurrent, maxConcurrent atomic.Int32
	producer := func(fail bool) func(context.Context) ([]byte, error) {
		return func(context.Context) ([]byte, error) {
			calls.Add(1)
			n := concurrent.Add(1)
			defer concurrent.Add(-1)
			for old := maxConcurrent.Load(); n > old && !maxConcurrent.CompareAndSwap(old, n); old = maxConcurrent.Load() {
			}
			if fail {
				close(entered)
				<-release
				return nil, errors.New("owner failed")
			}
			return []byte("recovered"), nil
		}
	}
	go func() { _, err := a.Bytes(context.Background(), key, time.Second, producer(true)); firstDone <- err }()
	<-entered
	secondDone := make(chan error, 1)
	go func() {
		got, err := b.Bytes(context.Background(), key, time.Second, producer(false))
		if err == nil && string(got) != "recovered" {
			err = errors.New("wrong recovered value")
		}
		secondDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-firstDone; err == nil {
		t.Fatal("first owner unexpectedly succeeded")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || maxConcurrent.Load() != 1 {
		t.Fatalf("calls=%d maxConcurrent=%d", calls.Load(), maxConcurrent.Load())
	}
}

func BenchmarkRedisHit(b *testing.B) {
	addr := os.Getenv("REDIS_ITEST_ADDR")
	if addr == "" {
		b.Skip("REDIS_ITEST_ADDR is not set")
	}
	cfg := cache.Config{DefaultTTL: time.Minute, RedisAddr: addr, RedisOpTimeout: 100 * time.Millisecond}
	reader := cache.New(cfg)
	defer reader.Close()
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	defer rdb.Close()
	prefix := cache.Identity{Dashboard: "bench", QueryRef: time.Now().String()}.Key()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		key := prefix + strconv.Itoa(i)
		if err := rdb.Set(context.Background(), key, "v", time.Minute).Err(); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		_, _ = reader.Bytes(context.Background(), key, time.Minute, func(context.Context) ([]byte, error) { return nil, nil })
	}
}
