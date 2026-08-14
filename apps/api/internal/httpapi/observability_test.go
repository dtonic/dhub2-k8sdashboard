package httpapi_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/observability"
)

type panicOnceAuditHandler struct {
	panicked    atomic.Bool
	auditStatus atomic.Int64
}

func (*panicOnceAuditHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *panicOnceAuditHandler) Handle(_ context.Context, record slog.Record) error {
	if h.panicked.CompareAndSwap(false, true) {
		panic("panic after response write")
	}
	if record.Message == "audit" {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "status" {
				h.auditStatus.Store(attr.Value.Int64())
			}
			return true
		})
	}
	return nil
}

func (h *panicOnceAuditHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *panicOnceAuditHandler) WithGroup(string) slog.Handler      { return h }

func TestHTTPMetricsUseFixedRoutesExactBytesAndExcludeScrape(t *testing.T) {
	m := observability.New()
	f := newFixture(t, func(d *httpapi.Deps) { d.Observability = m })
	health := f.get(t, "/healthz", nil)
	f.get(t, "/api/v1/dashboard-capabilities", nil)
	f.get(t, "/raw/secret@example.com?q=token&cursor=opaque", nil)
	scrape := f.get(t, "/metrics", nil).Body.String()
	if !strings.Contains(scrape, `dashboard_http_requests_total{route="healthz",status_class="2xx"} 1`) ||
		!strings.Contains(scrape, `dashboard_http_requests_total{route="dashboard",status_class="2xx"} 1`) ||
		!strings.Contains(scrape, `dashboard_http_requests_total{route="unmatched",status_class="4xx"} 1`) {
		t.Fatalf("missing fixed status series:\n%s", scrape)
	}
	wantBytes := `dashboard_http_response_bytes_sum{route="healthz"} ` + strconv.Itoa(health.Body.Len())
	if !strings.Contains(scrape, wantBytes) {
		t.Fatalf("response bytes not exact: want %s", wantBytes)
	}
	for _, forbidden := range []string{"secret@example.com", "token", "opaque", "/raw/"} {
		if strings.Contains(scrape, forbidden) {
			t.Fatalf("metrics leaked %q", forbidden)
		}
	}
	second := httptest.NewRecorder()
	f.srv.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(second.Body.String(), `dashboard_http_requests_total{route="metrics",status_class="2xx"} 1`) {
		t.Fatal("metrics scrape self-amplified HTTP SLI")
	}
}

func TestPanicBeforeWriteReturns500AndRecordsActualResponse(t *testing.T) {
	m := observability.New()
	var logs bytes.Buffer
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Observability = m
		d.Metrics = panicMetrics{Metrics: d.Metrics}
		d.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	})

	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"/overview?range=1h", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content-type=%q", got)
	}
	if !strings.Contains(rec.Body.String(), `"code":"internal"`) {
		t.Fatalf("missing internal API error: %s", rec.Body.String())
	}
	metrics := prometheusText(t, m)
	if !strings.Contains(metrics, `dashboard_http_requests_total{route="overview",status_class="5xx"} 1`) ||
		!strings.Contains(metrics, `dashboard_http_response_bytes_sum{route="overview"} `+strconv.Itoa(rec.Body.Len())) {
		t.Fatalf("500 response metrics do not match actual response:\n%s", metrics)
	}
	if !strings.Contains(logs.String(), `"msg":"audit"`) || !strings.Contains(logs.String(), `"status":500`) {
		t.Fatalf("audit did not preserve status 500: %s", logs.String())
	}
}

func TestPanicAfterWritePreserves200BodyAndMetrics(t *testing.T) {
	m := observability.New()
	handler := &panicOnceAuditHandler{}
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Observability = m
		d.Logger = slog.New(handler)
	})

	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"/overview?range=1h", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content-type=%q", got)
	}
	if strings.Contains(rec.Body.String(), `"code":"internal"`) {
		t.Fatalf("panic recovery appended an API error after the response: %s", rec.Body.String())
	}
	metrics := prometheusText(t, m)
	if !strings.Contains(metrics, `dashboard_http_requests_total{route="overview",status_class="2xx"} 1`) ||
		strings.Contains(metrics, `dashboard_http_requests_total{route="overview",status_class="5xx"} 1`) ||
		!strings.Contains(metrics, `dashboard_http_response_bytes_sum{route="overview"} `+strconv.Itoa(rec.Body.Len())) {
		t.Fatalf("200 response metrics do not match actual response:\n%s", metrics)
	}
	if got := handler.auditStatus.Load(); got != http.StatusOK {
		t.Fatalf("audit status=%d, want 200", got)
	}
}

func prometheusText(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	var out bytes.Buffer
	if err := m.WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}
