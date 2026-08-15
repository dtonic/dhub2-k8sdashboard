package httpapi

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAuthGuardFixedCardinalityRateAndConcurrency(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := newAuthGuard(func() time.Time { return now })
	releases := make([]func(), 0, 16)
	admitted := 0
	for i := 0; i < 10_000; i++ {
		r := httptest.NewRequest("GET", "https://dashboard.example/api/v1/auth/login?returnTo=%2F", nil)
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i)
		release, ok := g.acquire(r)
		if ok {
			admitted++
			releases = append(releases, release)
		}
	}
	if admitted != 16 {
		t.Fatalf("concurrency admission=%d want=16", admitted)
	}
	if len(g.identities) > authGuardIdentities {
		t.Fatalf("identities=%d", len(g.identities))
	}
	for _, release := range releases {
		release()
	}
}

func TestAuthGuardRejectsOversizeBeforeHandler(t *testing.T) {
	g := newAuthGuard(time.Now)
	r := httptest.NewRequest("POST", "https://dashboard.example/api/v1/auth/refresh", nil)
	r.ContentLength = -1
	if release, ok := g.acquire(r); ok {
		release()
		t.Fatal("oversize auth request admitted")
	}
}
