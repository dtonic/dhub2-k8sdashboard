// Package cache provides bounded L1/L2 response caching and cold-miss collapse.
package cache

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

var ErrValueTooLarge = errors.New("cache value exceeds configured limit")

type Result string

const (
	ResultL1        Result = "l1_hit"
	ResultL2        Result = "redis_hit"
	ResultMiss      Result = "miss"
	ResultCoalesced Result = "coalesced"
	ResultError     Result = "miss_error"
	ResultCanceled  Result = "canceled"
)

type Observer interface{ ObserveCache(Result) }
type ErrorObserver interface{ ObserveCacheError(string) }
type LoadObserver interface{ ObserveCacheLoad() }

type entry struct {
	key       string
	val       any
	expiresAt time.Time
	element   *list.Element
	size      int
}
type bytesResult struct {
	value  []byte
	result Result
}

type Config struct {
	DefaultTTL     time.Duration
	MaxEntries     int
	MaxValueBytes  int
	MaxLocalBytes  int
	RedisAddr      string
	RedisOpTimeout time.Duration
	RedisCooldown  time.Duration
	LockTTL        time.Duration
	LockWait       time.Duration
	Observer       Observer
	Now            func() time.Time
}

type TTL struct {
	mu         sync.Mutex
	items      map[string]*entry
	order      *list.List
	group      singleflight.Group
	config     Config
	redis      *redis.Client
	redisMu    sync.Mutex
	redisOff   time.Time
	redisProbe bool
	closeOnce  sync.Once
	closeErr   error
	localBytes int
	// afterWaitMiss is an internal deterministic test seam for the Redis lock handoff window.
	afterWaitMiss func()
}

// Close releases the owned Redis pool. Local-only caches are a no-op.
func (c *TTL) Close() error {
	c.closeOnce.Do(func() {
		if c.redis != nil {
			c.closeErr = c.redis.Close()
		}
	})
	return c.closeErr
}

func NewTTL(ttl time.Duration) *TTL {
	return New(Config{DefaultTTL: ttl})
}

func New(cfg Config) *TTL {
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = 5 * time.Second
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 1024
	}
	if cfg.MaxValueBytes <= 0 {
		cfg.MaxValueBytes = 4 << 20
	}
	if cfg.MaxLocalBytes <= 0 {
		cfg.MaxLocalBytes = 64 << 20
	}
	if cfg.MaxLocalBytes < cfg.MaxValueBytes {
		cfg.MaxValueBytes = cfg.MaxLocalBytes
	}
	if cfg.RedisOpTimeout <= 0 {
		cfg.RedisOpTimeout = 75 * time.Millisecond
	}
	if cfg.RedisCooldown <= 0 {
		cfg.RedisCooldown = 2 * time.Second
	}
	if cfg.LockTTL <= 0 {
		cfg.LockTTL = 15 * time.Second
	}
	if cfg.LockWait <= 0 {
		cfg.LockWait = 12 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	c := &TTL{items: make(map[string]*entry), order: list.New(), config: cfg}
	if cfg.RedisAddr != "" {
		c.redis = redis.NewClient(&redis.Options{
			Addr: cfg.RedisAddr, MaxRetries: -1,
			DialTimeout: cfg.RedisOpTimeout, ReadTimeout: cfg.RedisOpTimeout, WriteTimeout: cfg.RedisOpTimeout,
		})
	}
	return c
}

// Do keeps source compatibility for process-local typed values.
func (c *TTL) Do(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	if v, ok := c.get(key); ok {
		return v, nil
	}
	ch := c.group.DoChan("typed:"+key, func() (any, error) {
		if v, ok := c.get(key); ok {
			return v, nil
		}
		v, err := fn(ctx)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.set(key, v, c.config.DefaultTTL)
		return v, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		return res.Val, res.Err
	}
}

// Bytes caches successful bounded serialized responses. Returned bytes are immutable:
// callers may read but must never modify them. This avoids a full response copy per hit.
func (c *TTL) Bytes(ctx context.Context, key string, ttl time.Duration, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	if ttl <= 0 {
		ttl = c.config.DefaultTTL
	}
	if v, ok := c.get(key); ok {
		if b, ok := v.([]byte); ok {
			c.observe(ResultL1)
			return b, nil
		}
	}
	ch := c.group.DoChan("bytes:"+key, func() (any, error) {
		if v, ok := c.get(key); ok {
			return bytesResult{v.([]byte), ResultL1}, nil
		}
		if b, remaining, ok := c.redisGet(ctx, key); ok {
			c.set(key, b, remaining)
			return bytesResult{b, ResultL2}, nil
		}
		b, result, err := c.produceBytes(ctx, key, ttl, fn)
		return bytesResult{b, result}, err
	})
	select {
	case <-ctx.Done():
		c.observe(ResultCanceled)
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			if errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded) {
				c.observe(ResultCanceled)
			} else {
				c.observe(ResultError)
			}
			return nil, res.Err
		}
		br := res.Val.(bytesResult)
		c.observe(br.result)
		return br.value, nil
	}
}

func (c *TTL) produceBytes(ctx context.Context, key string, ttl time.Duration, fn func(context.Context) ([]byte, error)) ([]byte, Result, error) {
	owner, locked := c.redisLock(ctx, key)
	waited := false
	if c.redis != nil && !locked && !c.redisCooling() {
		waited = true
		waitCtx, cancel := context.WithTimeout(ctx, c.config.LockWait)
		defer cancel()
		interval := 10 * time.Millisecond
		for !locked {
			t := time.NewTimer(interval)
			select {
			case <-waitCtx.Done():
				t.Stop()
				if ctx.Err() != nil {
					return nil, ResultCanceled, ctx.Err()
				}
				return nil, ResultError, waitCtx.Err()
			case <-t.C:
			}
			if b, remaining, ok := c.redisGet(ctx, key); ok {
				c.set(key, b, remaining)
				return b, ResultCoalesced, nil
			}
			if c.afterWaitMiss != nil {
				c.afterWaitMiss()
			}
			if c.redisCooling() {
				break
			}
			owner, locked = c.redisLock(ctx, key)
			if interval < 200*time.Millisecond {
				interval *= 2
				if interval > 200*time.Millisecond {
					interval = 200 * time.Millisecond
				}
			}
		}
	}
	if locked {
		defer c.redisUnlock(key, owner)
		// The previous owner can publish between our last GET and lock acquisition.
		// Recheck once after handoff so two instances never run the producer for one miss.
		if waited {
			if b, remaining, ok := c.redisGet(ctx, key); ok {
				c.set(key, b, remaining)
				return b, ResultCoalesced, nil
			}
		}
	}
	if o, ok := c.config.Observer.(LoadObserver); ok {
		o.ObserveCacheLoad()
	}
	b, err := fn(ctx)
	if err != nil {
		return nil, ResultError, err
	}
	if err := ctx.Err(); err != nil {
		return nil, ResultCanceled, err
	}
	if len(b) > c.config.MaxValueBytes {
		return nil, ResultError, ErrValueTooLarge
	}
	c.set(key, b, ttl)
	c.redisSet(ctx, key, b, ttl)
	return b, ResultMiss, nil
}

func (c *TTL) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if !c.config.Now().Before(e.expiresAt) {
		c.remove(e)
		return nil, false
	}
	c.order.MoveToFront(e.element)
	return e.val, true
}

func (c *TTL) set(key string, val any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := 1
	if b, ok := val.([]byte); ok {
		size = len(b)
	}
	if size > c.config.MaxLocalBytes {
		return
	}
	if e := c.items[key]; e != nil {
		c.localBytes += size - e.size
		e.val, e.expiresAt = val, c.config.Now().Add(ttl)
		e.size = size
		c.order.MoveToFront(e.element)
		for c.localBytes > c.config.MaxLocalBytes {
			c.remove(c.order.Back().Value.(*entry))
		}
		return
	}
	e := &entry{key: key, val: val, expiresAt: c.config.Now().Add(ttl), size: size}
	e.element = c.order.PushFront(e)
	c.items[key] = e
	c.localBytes += size
	for len(c.items) > c.config.MaxEntries || c.localBytes > c.config.MaxLocalBytes {
		c.remove(c.order.Back().Value.(*entry))
	}
}

func (c *TTL) remove(e *entry) {
	delete(c.items, e.key)
	c.order.Remove(e.element)
	c.localBytes -= e.size
}

func (c *TTL) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.config.Now()
	for e := c.order.Back(); e != nil; {
		prev := e.Prev()
		item := e.Value.(*entry)
		if !now.Before(item.expiresAt) {
			c.remove(item)
		}
		e = prev
	}
	return len(c.items)
}

func (c *TTL) redisAvailable() bool {
	if c.redis == nil {
		return false
	}
	c.redisMu.Lock()
	defer c.redisMu.Unlock()
	if c.redisOff.IsZero() {
		return true
	}
	if c.config.Now().Before(c.redisOff) || c.redisProbe {
		return false
	}
	c.redisProbe = true
	return true
}

func (c *TTL) redisCooling() bool {
	if c.redis == nil {
		return false
	}
	c.redisMu.Lock()
	defer c.redisMu.Unlock()
	return c.redisProbe || (!c.redisOff.IsZero() && c.config.Now().Before(c.redisOff))
}

func (c *TTL) redisContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.config.RedisOpTimeout)
}

func (c *TTL) redisFailure() {
	c.redisMu.Lock()
	c.redisOff = c.config.Now().Add(c.config.RedisCooldown)
	c.redisProbe = false
	c.redisMu.Unlock()
	if o, ok := c.config.Observer.(ErrorObserver); ok {
		o.ObserveCacheError("redis_fallback")
	}
}

func (c *TTL) redisSuccess() {
	c.redisMu.Lock()
	c.redisOff = time.Time{}
	c.redisProbe = false
	c.redisMu.Unlock()
}

func (c *TTL) redisGet(ctx context.Context, key string) ([]byte, time.Duration, bool) {
	if !c.redisAvailable() {
		return nil, 0, false
	}
	op, cancel := c.redisContext(ctx)
	defer cancel()
	pipe := c.redis.Pipeline()
	rangeCmd := pipe.GetRange(op, key, 0, int64(c.config.MaxValueBytes))
	ttlCmd := pipe.PTTL(op, key)
	_, err := pipe.Exec(op)
	if err != nil {
		c.redisFailure()
		return nil, 0, false
	}
	c.redisSuccess()
	remaining := ttlCmd.Val()
	if remaining <= 0 {
		return nil, 0, false
	}
	b, err := rangeCmd.Bytes()
	if err != nil {
		return nil, 0, false
	}
	if len(b) > c.config.MaxValueBytes {
		return nil, 0, false
	}
	return b, remaining, true
}

func (c *TTL) redisSet(ctx context.Context, key string, b []byte, ttl time.Duration) {
	if !c.redisAvailable() {
		return
	}
	op, cancel := c.redisContext(ctx)
	defer cancel()
	if err := c.redis.Set(op, key, b, ttl).Err(); err != nil {
		c.redisFailure()
	} else {
		c.redisSuccess()
	}
}

func (c *TTL) redisLock(ctx context.Context, key string) (string, bool) {
	if !c.redisAvailable() {
		return "", false
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", false
	}
	owner := hex.EncodeToString(token[:])
	op, cancel := c.redisContext(ctx)
	defer cancel()
	ok, err := c.redis.SetNX(op, key+":lock", owner, c.config.LockTTL).Result()
	if err != nil {
		c.redisFailure()
		return "", false
	}
	c.redisSuccess()
	return owner, ok
}

var unlockScript = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`)

func (c *TTL) redisUnlock(key, owner string) {
	if c.redis == nil {
		return
	}
	op, cancel := context.WithTimeout(context.Background(), c.config.RedisOpTimeout)
	defer cancel()
	if err := unlockScript.Run(op, c.redis, []string{key + ":lock"}, owner).Err(); err != nil {
		c.redisFailure()
	}
}

func (c *TTL) observe(r Result) {
	if c.config.Observer != nil {
		c.config.Observer.ObserveCache(r)
	}
}
func Typed[T any](ctx context.Context, c *TTL, key string, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	v, err := c.Do(ctx, key, func(ctx context.Context) (any, error) { return fn(ctx) })
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("cache type mismatch")
	}
	return t, nil
}
