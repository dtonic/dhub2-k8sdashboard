package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const authGuardIdentities = 1024
const authIdentityIdle = 10 * time.Minute

type authBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}
type authIdentity struct {
	bucket *authBucket
	last   time.Time
}
type authGuard struct {
	global     authBucket
	overflow   authBucket
	concurrent chan struct{}
	now        func() time.Time
	mu         sync.Mutex
	identities map[string]authIdentity
}

func newAuthGuard(now func() time.Time) *authGuard {
	return &authGuard{concurrent: make(chan struct{}, 16), now: now, identities: make(map[string]authIdentity, authGuardIdentities)}
}

func (g *authGuard) acquire(r *http.Request) (func(), bool) {
	if r.ContentLength != 0 || len(r.URL.RawQuery) > 4096 {
		return nil, false
	}
	now := g.now()
	if !takeAuthToken(&g.global, now, 50, 100) {
		return nil, false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	bucket := g.identityBucket(host, now)
	if !takeAuthToken(bucket, now, 10, 20) {
		return nil, false
	}
	select {
	case g.concurrent <- struct{}{}:
		return func() { <-g.concurrent }, true
	default:
		return nil, false
	}
}

func (g *authGuard) identityBucket(host string, now time.Time) *authBucket {
	g.mu.Lock()
	defer g.mu.Unlock()
	if entry, ok := g.identities[host]; ok {
		entry.last = now
		g.identities[host] = entry
		return entry.bucket
	}
	if len(g.identities) >= authGuardIdentities {
		for key, entry := range g.identities {
			if now.Sub(entry.last) >= authIdentityIdle {
				delete(g.identities, key)
				break
			}
		}
	}
	if len(g.identities) >= authGuardIdentities {
		return &g.overflow
	}
	b := &authBucket{}
	g.identities[host] = authIdentity{bucket: b, last: now}
	return b
}

func takeAuthToken(b *authBucket, now time.Time, rate, burst float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.tokens = burst
		b.last = now
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(burst, b.tokens+elapsed*rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
