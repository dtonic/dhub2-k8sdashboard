package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type captureObserver struct {
	mu       sync.Mutex
	outcomes []Outcome
	states   []CircuitState
}

func (o *captureObserver) ObserveUpstream(_ context.Context, _ Upstream, v Outcome, _ time.Duration) {
	o.mu.Lock()
	o.outcomes = append(o.outcomes, v)
	o.mu.Unlock()
}
func (o *captureObserver) SetCircuit(_ Upstream, v CircuitState, _ uint64) {
	o.mu.Lock()
	o.states = append(o.states, v)
	o.mu.Unlock()
}

func TestObserverCountsLogicalRetriesOnceAndClassifiesOutcomes(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			http.Error(w, "", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	o := &captureObserver{}
	c, err := New(Config{BaseURL: srv.URL, What: "test", Timeout: time.Second, Observer: o, Upstream: UpstreamGreptime})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.GetJSON(context.Background(), "/", nil, &map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(o.outcomes) != 1 || o.outcomes[0] != OutcomeSuccess {
		t.Fatalf("calls=%d outcomes=%v", calls.Load(), o.outcomes)
	}

	check := func(name string, handler http.HandlerFunc, ctx context.Context, want Outcome) {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(handler)
			defer s.Close()
			obs := &captureObserver{}
			cl, _ := New(Config{BaseURL: s.URL, What: "test", Timeout: 20 * time.Millisecond, Observer: obs})
			_ = cl.GetJSON(ctx, "/", nil, &map[string]any{})
			if len(obs.outcomes) != 1 || obs.outcomes[0] != want {
				t.Fatalf("outcomes=%v", obs.outcomes)
			}
		})
	}
	check("bad_query", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "", http.StatusBadRequest) }, context.Background(), OutcomeBadQuery)
	check("timeout", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, context.Background(), OutcomeTimeout)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	check("canceled", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, canceled, OutcomeCanceled)
}

func TestObserverCircuitTransitionsAndOpenHasNoNetwork(t *testing.T) {
	now := time.Unix(0, 0)
	var fail atomic.Bool
	fail.Store(true)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, "", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	o := &captureObserver{}
	c, _ := New(Config{BaseURL: srv.URL, What: "test", Timeout: time.Second, CircuitThreshold: 1, CircuitCooldown: time.Second, Now: func() time.Time { return now }, Observer: o})
	_ = c.GetJSON(context.Background(), "/", nil, &map[string]any{})
	before := calls.Load()
	_ = c.GetJSON(context.Background(), "/", nil, &map[string]any{})
	if calls.Load() != before || o.outcomes[len(o.outcomes)-1] != OutcomeCircuitOpen {
		t.Fatal("open circuit reached network or wrong outcome")
	}
	now = now.Add(2 * time.Second)
	fail.Store(false)
	if err := c.GetJSON(context.Background(), "/", nil, &map[string]any{}); err != nil {
		t.Fatal(err)
	}
	want := []CircuitState{CircuitClosed, CircuitOpen, CircuitHalfOpen, CircuitClosed}
	if len(o.states) != len(want) {
		t.Fatalf("states=%v", o.states)
	}
	for i := range want {
		if o.states[i] != want[i] {
			t.Fatalf("states=%v", o.states)
		}
	}
}
