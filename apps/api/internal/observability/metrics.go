// Package observability provides bounded, dependency-free platform metrics.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/upstream"
)

var routes = [...]string{
	"healthz", "readyz", "version", "metrics", "scope", "overview", "namespaces", "namespace",
	"workload", "pod", "logs", "topology", "edge_series", "alerts", "stream", "unmatched",
}
var statusClasses = [...]string{"2xx", "3xx", "4xx", "5xx", "other", "canceled"}
var upstreams = [...]string{"other", "greptime", "quickwit"}
var outcomes = [...]string{"success", "timeout", "canceled", "bad_query", "unavailable", "circuit_open"}
var durationBuckets = [...]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
var byteBuckets = [...]float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304}

type histogram struct {
	buckets [13]atomic.Uint64 // non-cumulative finite buckets plus +Inf
	sum     atomic.Uint64
}

func (h *histogram) observe(v float64, sum uint64, bounds []float64) {
	idx := len(bounds)
	for i, b := range bounds {
		if v <= b {
			idx = i
			break
		}
	}
	h.buckets[idx].Add(1)
	h.sum.Add(sum)
}

type Metrics struct {
	httpRequests [16][6]atomic.Uint64
	httpDuration [16]histogram
	httpBytes    [16]histogram
	inflight     atomic.Int64
	upRequests   [3][6]atomic.Uint64
	upDuration   [3]histogram
	circuit      [3]atomic.Uint64 // generation<<2 | state
	informer     atomic.Int64
	logger       *slog.Logger
	slowUpstream time.Duration
}

func New() *Metrics { return &Metrics{} }
func (m *Metrics) ConfigureLogging(logger *slog.Logger, slow time.Duration) {
	m.logger, m.slowUpstream = logger, slow
}

func routeIndex(route string) int {
	for i, v := range routes {
		if v == route {
			return i
		}
	}
	return len(routes) - 1
}
func upstreamIndex(v upstream.Upstream) int {
	if int(v) < 0 || int(v) >= len(upstreams) {
		return 0
	}
	return int(v)
}
func outcomeIndex(v upstream.Outcome) int {
	if int(v) < 0 || int(v) >= len(outcomes) {
		return int(upstream.OutcomeUnavailable)
	}
	return int(v)
}
func statusIndex(code int, canceled bool) int {
	if canceled && code == 0 {
		return 5
	}
	if code >= 200 && code < 600 {
		return code/100 - 2
	}
	return 4
}

func (m *Metrics) HTTPStarted() { m.inflight.Add(1) }
func (m *Metrics) HTTPFinished(route string, status, bytes int, d time.Duration, stream, canceled bool) {
	m.inflight.Add(-1)
	i := routeIndex(route)
	m.httpRequests[i][statusIndex(status, canceled)].Add(1)
	if d < 0 {
		d = 0
	}
	if !stream {
		m.httpDuration[i].observe(d.Seconds(), uint64(d.Microseconds()), durationBuckets[:])
		if bytes < 0 {
			bytes = 0
		}
		m.httpBytes[i].observe(float64(bytes), uint64(bytes), byteBuckets[:])
	}
}

// ObserveUpstream records one logical request, including all retries.
func (m *Metrics) ObserveUpstream(ctx context.Context, upstream upstream.Upstream, outcome upstream.Outcome, d time.Duration) {
	if d < 0 {
		d = 0
	}
	i, o := upstreamIndex(upstream), outcomeIndex(outcome)
	m.upRequests[i][o].Add(1)
	m.upDuration[i].observe(d.Seconds(), uint64(d.Microseconds()), durationBuckets[:])
	if m.logger != nil && m.slowUpstream > 0 && d >= m.slowUpstream {
		args := []any{"upstream", upstream.String(), "outcome", outcome.String(), "durationMs", d.Milliseconds()}
		if trace := TraceFrom(ctx); trace != nil {
			refs, overflow := trace.Summary()
			args = append(args, "requestId", trace.RequestID(), "queryRefs", refs, "queryRefsOverflow", overflow)
		}
		m.logger.Warn("slow_upstream", args...)
	}
}
func (m *Metrics) SetCircuit(up upstream.Upstream, state upstream.CircuitState, generation uint64) {
	if state < upstream.CircuitClosed || state > upstream.CircuitHalfOpen {
		state = upstream.CircuitClosed
	}
	v := &m.circuit[upstreamIndex(up)]
	next := generation<<2 | uint64(state)
	for {
		old := v.Load()
		if old>>2 > generation {
			return
		}
		if v.CompareAndSwap(old, next) {
			return
		}
	}
}
func (m *Metrics) SetInformerSynced(ok bool) {
	if ok {
		m.informer.Store(1)
	} else {
		m.informer.Store(0)
	}
}

func (m *Metrics) WritePrometheus(w io.Writer) error {
	write := func(f string, a ...any) error { _, err := fmt.Fprintf(w, f, a...); return err }
	if err := write("# HELP dashboard_http_requests_total Completed HTTP requests.\n# TYPE dashboard_http_requests_total counter\n"); err != nil {
		return err
	}
	for i, route := range routes {
		for j, class := range statusClasses {
			if err := write("dashboard_http_requests_total{route=%q,status_class=%q} %d\n", route, class, m.httpRequests[i][j].Load()); err != nil {
				return err
			}
		}
	}
	if err := write("# HELP dashboard_http_inflight_requests Current HTTP requests.\n# TYPE dashboard_http_inflight_requests gauge\ndashboard_http_inflight_requests %d\n", m.inflight.Load()); err != nil {
		return err
	}
	if err := write("# HELP dashboard_http_request_duration_seconds HTTP request latency excluding SSE.\n# TYPE dashboard_http_request_duration_seconds histogram\n"); err != nil {
		return err
	}
	for i, route := range routes {
		if err := writeHistogram(w, "dashboard_http_request_duration_seconds", "route", route, &m.httpDuration[i], durationBuckets[:], 1e6); err != nil {
			return err
		}
	}
	if err := write("# HELP dashboard_http_response_bytes HTTP response bytes excluding SSE.\n# TYPE dashboard_http_response_bytes histogram\n"); err != nil {
		return err
	}
	for i, route := range routes {
		if err := writeHistogram(w, "dashboard_http_response_bytes", "route", route, &m.httpBytes[i], byteBuckets[:], 1); err != nil {
			return err
		}
	}
	if err := write("# HELP dashboard_upstream_requests_total Logical upstream requests.\n# TYPE dashboard_upstream_requests_total counter\n"); err != nil {
		return err
	}
	for i, up := range upstreams {
		for j, out := range outcomes {
			if err := write("dashboard_upstream_requests_total{outcome=%q,upstream=%q} %d\n", out, up, m.upRequests[i][j].Load()); err != nil {
				return err
			}
		}
	}
	if err := write("# HELP dashboard_upstream_request_duration_seconds Logical upstream latency including retries.\n# TYPE dashboard_upstream_request_duration_seconds histogram\n"); err != nil {
		return err
	}
	for i, up := range upstreams {
		if err := writeHistogram(w, "dashboard_upstream_request_duration_seconds", "upstream", up, &m.upDuration[i], durationBuckets[:], 1e6); err != nil {
			return err
		}
	}
	if err := write("# HELP dashboard_upstream_circuit_state Circuit state: 0 closed, 1 open, 2 half-open.\n# TYPE dashboard_upstream_circuit_state gauge\n"); err != nil {
		return err
	}
	for i, up := range upstreams {
		if err := write("dashboard_upstream_circuit_state{upstream=%q} %d\n", up, m.circuit[i].Load()&3); err != nil {
			return err
		}
	}
	return write("# HELP dashboard_informer_synced Whether informer caches are synced.\n# TYPE dashboard_informer_synced gauge\ndashboard_informer_synced %d\n", m.informer.Load())
}

func writeHistogram(w io.Writer, name, label, value string, h *histogram, bounds []float64, sumScale float64) error {
	cumulative := uint64(0)
	for i, b := range bounds {
		cumulative += h.buckets[i].Load()
		if _, err := fmt.Fprintf(w, "%s_bucket{%s=%q,le=%q} %d\n", name, label, value, fmt.Sprintf("%g", b), cumulative); err != nil {
			return err
		}
	}
	cumulative += h.buckets[len(bounds)].Load()
	if _, err := fmt.Fprintf(w, "%s_bucket{%s=%q,le=%q} %d\n", name, label, value, "+Inf", cumulative); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s_sum{%s=%q} %g\n", name, label, value, float64(h.sum.Load())/sumScale); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s_count{%s=%q} %d\n", name, label, value, cumulative)
	return err
}
