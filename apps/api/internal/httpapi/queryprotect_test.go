package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/queryprotect"
)

type deadlineMetrics struct{ datasource.Unavailable }

func (deadlineMetrics) Trends(ctx context.Context, _ datasource.Target, _ datasource.Window, _ []string) ([]contract.TrendPanel, error) {
	<-ctx.Done()
	return nil, nil
}

type heldMetrics struct {
	datasource.Unavailable
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (h *heldMetrics) Trends(ctx context.Context, _ datasource.Target, _ datasource.Window, _ []string) ([]contract.TrendPanel, error) {
	h.calls.Add(1)
	select {
	case h.entered <- struct{}{}:
	default:
	}
	select {
	case <-h.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestHTTPGuardsRejectBeforeDatasource(t *testing.T) {
	t.Run("user-rate-across-dashboards", func(t *testing.T) {
		cfg := queryprotect.DefaultConfig()
		cfg.UserRate = 0.001
		cfg.UserBurst = 1
		f := newFixture(t, func(d *httpapi.Deps) { d.Guard = queryprotect.New(cfg, nil) })
		if rec := f.get(t, base+"/overview?range=1h", nil); rec.Code != http.StatusOK {
			t.Fatal(rec.Code)
		}
		rec := f.get(t, base+"/alerts?range=1h", nil)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("status=%d retry=%q", rec.Code, rec.Header().Get("Retry-After"))
		}
		if f.counts.alerts.Load() != 1 {
			t.Fatalf("alerts datasource calls=%d, want only overview's aggregate call", f.counts.alerts.Load())
		}
	})
	t.Run("dashboard-concurrency-across-users", func(t *testing.T) {
		held := &heldMetrics{entered: make(chan struct{}, 1), release: make(chan struct{})}
		cfg := queryprotect.DefaultConfig()
		cfg.DashboardConcurrent = 1
		f := newFixture(t, func(d *httpapi.Deps) {
			d.Metrics = held
			d.Resolver = subjectHeaderResolver{}
			d.Guard = queryprotect.New(cfg, nil)
		})
		done := make(chan struct{})
		go func() {
			r := httptest.NewRequest(http.MethodGet, base+"/overview?range=1h", nil)
			r.Header.Set("X-Test-Subject", "a")
			f.srv.ServeHTTP(httptest.NewRecorder(), r)
			close(done)
		}()
		<-held.entered
		r := httptest.NewRequest(http.MethodGet, base+"/overview?range=1h", nil)
		r.Header.Set("X-Test-Subject", "b")
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, r)
		if rec.Code != http.StatusTooManyRequests || held.calls.Load() != 1 {
			t.Fatalf("status=%d calls=%d", rec.Code, held.calls.Load())
		}
		close(held.release)
		<-done
	})
	t.Run("user-concurrency-across-dashboards", func(t *testing.T) {
		held := &heldMetrics{entered: make(chan struct{}, 1), release: make(chan struct{})}
		cfg := queryprotect.DefaultConfig()
		cfg.UserConcurrent = 1
		f := newFixture(t, func(d *httpapi.Deps) {
			d.Metrics = held
			d.Resolver = subjectHeaderResolver{}
			d.Guard = queryprotect.New(cfg, nil)
		})
		done := make(chan struct{})
		go func() {
			r := httptest.NewRequest(http.MethodGet, base+"/overview?range=1h", nil)
			r.Header.Set("X-Test-Subject", "a")
			f.srv.ServeHTTP(httptest.NewRecorder(), r)
			close(done)
		}()
		<-held.entered
		r := httptest.NewRequest(http.MethodGet, base+"/alerts?range=1h", nil)
		r.Header.Set("X-Test-Subject", "a")
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, r)
		if rec.Code != http.StatusTooManyRequests || f.counts.alerts.Load() != 0 {
			t.Fatalf("status=%d alertCalls=%d", rec.Code, f.counts.alerts.Load())
		}
		close(held.release)
		<-done
	})
}

func TestOversizedSerializedResultIs502AndNeverCached(t *testing.T) {
	c := cache.New(cache.Config{DefaultTTL: time.Minute, MaxValueBytes: 64})
	f := newFixture(t, func(d *httpapi.Deps) { d.Cache = c })
	for i := 0; i < 2; i++ {
		rec := f.get(t, base+"/overview?range=1h", nil)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d status=%d body=%s", i, rec.Code, rec.Body.String())
		}
		var apiErr contract.APIError
		if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil || apiErr.Code != "result_too_large" {
			t.Fatalf("partial/non-error body: %s", rec.Body.String())
		}
		if c.Len() != 0 {
			t.Fatalf("oversized response cached: len=%d", c.Len())
		}
	}
	if f.counts.trends.Load() != 2 {
		t.Fatalf("producer was not rerun: %d", f.counts.trends.Load())
	}
}

func TestServerOwnedQueryDeadlineReturns504AndIsNotCached(t *testing.T) {
	var source deadlineMetrics
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Metrics = source
		cfg := queryprotect.DefaultConfig()
		cfg.QueryTimeout = 20 * time.Millisecond
		d.Guard = queryprotect.New(cfg, nil)
	})
	rec := f.get(t, base+"/overview?range=1h", nil)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "query_timeout") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if rec := f.get(t, base+"/overview?range=1h", nil); rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("deadline result was cached: %d", rec.Code)
	}
}

func TestParentDeadlineDoesNotBecomeSynthetic504(t *testing.T) {
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Metrics = deadlineMetrics{}
		cfg := queryprotect.DefaultConfig()
		cfg.QueryTimeout = time.Second
		d.Guard = queryprotect.New(cfg, nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, base+"/overview?range=1h", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Body.Len() != 0 {
		t.Fatalf("parent deadline produced synthetic response: %s", rec.Body.String())
	}
}

func TestMetricsExpositionIsOperationalAndDoesNotLeakQuery(t *testing.T) {
	f := newFixture(t)
	_ = f.get(t, base+"/overview?range=bogus&q=top-secret", nil)
	rec := f.get(t, "/metrics", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("status=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if strings.Contains(rec.Body.String(), "top-secret") || strings.Contains(rec.Body.String(), "q=") {
		t.Fatalf("secret/high-cardinality metrics: %s", rec.Body.String())
	}
}
