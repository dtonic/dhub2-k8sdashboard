// Package cache는 화면 응답을 짧게 재사용하고 **같은 키의 동시 요청을 하나로 합칩니다**.
//
// 대시보드는 여러 사람이 같은 화면을 같은 순간에 봅니다. 캐시만 있으면
// 캐시가 만료된 순간 N개의 요청이 동시에 upstream으로 나갑니다(cache stampede).
// singleflight로 그 순간에도 upstream 호출은 1회입니다.
package cache

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type entry struct {
	val       any
	expiresAt time.Time
}

// TTL은 만료 시간이 있는 키-값 저장소입니다.
type TTL struct {
	mu    sync.RWMutex
	items map[string]entry
	group singleflight.Group
	ttl   time.Duration
	// now는 테스트에서 시간을 고정하기 위한 훅입니다.
	now func() time.Time
}

func NewTTL(ttl time.Duration) *TTL {
	return &TTL{items: make(map[string]entry), ttl: ttl, now: time.Now}
}

// Do는 키가 유효하면 캐시 값을, 아니면 fn을 **한 번만** 실행해 결과를 돌려줍니다.
func (c *TTL) Do(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	if v, ok := c.get(key); ok {
		return v, nil
	}
	v, err, _ := c.group.Do(key, func() (any, error) {
		// 대기 중에 다른 호출이 채웠을 수 있습니다.
		if v, ok := c.get(key); ok {
			return v, nil
		}
		v, err := fn(ctx)
		if err != nil {
			return nil, err
		}
		c.set(key, v)
		return v, nil
	})
	return v, err
}

func (c *TTL) get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[key]
	if !ok || c.now().After(e.expiresAt) {
		return nil, false
	}
	return e.val, true
}

func (c *TTL) set(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry{val: v, expiresAt: c.now().Add(c.ttl)}
}

// Len은 만료되지 않은 항목 수입니다. 테스트와 진단에 씁니다.
func (c *TTL) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, e := range c.items {
		if !c.now().After(e.expiresAt) {
			n++
		}
	}
	return n
}

// Typed는 제네릭 래퍼입니다. 핸들러에서 형변환을 반복하지 않게 합니다.
func Typed[T any](ctx context.Context, c *TTL, key string, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	v, err := c.Do(ctx, key, func(ctx context.Context) (any, error) { return fn(ctx) })
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, nil
	}
	return t, nil
}
