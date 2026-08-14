package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type reentrantObserver struct {
	client **Client
	once   sync.Once
	done   chan struct{}
}

func (*reentrantObserver) ObserveUpstream(context.Context, Upstream, Outcome, time.Duration) {}
func (o *reentrantObserver) SetCircuit(_ Upstream, _ CircuitState, _ uint64) {
	if o.client == nil || *o.client == nil {
		return
	}
	o.once.Do(func() {
		(*o.client).breakerMu.Lock()
		(*o.client).breakerMu.Unlock()
		close(o.done)
	})
}

func TestCircuitObserverRunsOutsideBreakerLock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "", http.StatusServiceUnavailable) }))
	defer srv.Close()
	var c *Client
	o := &reentrantObserver{client: &c, done: make(chan struct{})}
	var err error
	c, err = New(Config{BaseURL: srv.URL, What: "test", Timeout: 50 * time.Millisecond, CircuitThreshold: 1, Observer: o})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.GetJSON(context.Background(), "/", nil, nil)
	select {
	case <-o.done:
	case <-time.After(time.Second):
		t.Fatal("observer called while breaker lock held")
	}
}
