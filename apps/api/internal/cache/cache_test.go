package cache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
)

func TestConcurrentMissesCallUpstreamOnce(t *testing.T) {
	// 자동 갱신을 켠 사용자가 여러 명이면 캐시가 만료되는 순간 요청이 동시에 몰립니다.
	// 그때도 upstream 호출은 1회여야 합니다.
	c := cache.NewTTL(time.Minute)
	var calls atomic.Int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cache.Typed(context.Background(), c, "overview", func(context.Context) (int, error) {
				calls.Add(1)
				<-release
				return 42, nil
			})
		}()
	}
	// 모든 고루틴이 캐시를 지나칠 시간을 준 뒤 upstream을 풀어줍니다.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream 호출=%d회, want 1회", got)
	}
}

func TestValueIsReusedWithinTTL(t *testing.T) {
	c := cache.NewTTL(time.Minute)
	var calls int
	fn := func(context.Context) (string, error) {
		calls++
		return "v", nil
	}
	for i := 0; i < 3; i++ {
		v, err := cache.Typed(context.Background(), c, "k", fn)
		if err != nil || v != "v" {
			t.Fatalf("v=%q err=%v", v, err)
		}
	}
	if calls != 1 {
		t.Fatalf("호출=%d회, want 1회", calls)
	}
}

func TestDifferentKeysDoNotShareValues(t *testing.T) {
	// 캐시 키에 Scope가 빠지면 권한이 다른 사용자끼리 값을 나눠 갖게 됩니다.
	c := cache.NewTTL(time.Minute)
	a, _ := cache.Typed(context.Background(), c, "overview|scope=a", func(context.Context) (string, error) { return "A", nil })
	b, _ := cache.Typed(context.Background(), c, "overview|scope=b", func(context.Context) (string, error) { return "B", nil })
	if a == b {
		t.Fatalf("서로 다른 Scope가 같은 값을 받았습니다: %q", a)
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	c := cache.NewTTL(time.Minute)
	var calls int
	fn := func(context.Context) (int, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("upstream 장애")
		}
		return 7, nil
	}
	if _, err := cache.Typed(context.Background(), c, "k", fn); err == nil {
		t.Fatal("첫 호출이 에러를 돌려주지 않았습니다")
	}
	v, err := cache.Typed(context.Background(), c, "k", fn)
	if err != nil || v != 7 {
		t.Fatalf("장애가 캐시되었습니다: v=%d err=%v", v, err)
	}
}

func TestExpiredEntryIsRefetched(t *testing.T) {
	c := cache.NewTTL(20 * time.Millisecond)
	var calls int
	fn := func(context.Context) (int, error) {
		calls++
		return calls, nil
	}
	if _, err := cache.Typed(context.Background(), c, "k", fn); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	v, err := cache.Typed(context.Background(), c, "k", fn)
	if err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("만료 후 값=%d, want 2", v)
	}
	if c.Len() != 1 {
		t.Errorf("항목 수=%d, want 1", c.Len())
	}
}
