package observability

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/upstream"
)

func TestHistogramBoundsAreExactAndCumulative(t *testing.T) {
	m := New()
	m.HTTPStarted()
	m.HTTPFinished("overview", 200, 10, 12*time.Second, false, false)
	var b bytes.Buffer
	if err := m.WritePrometheus(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, le := range []string{`le="10"`, `le="30"`} {
		if !strings.Contains(out, le) {
			t.Fatalf("missing exact boundary %s", le)
		}
	}
	for _, want := range []string{
		`dashboard_http_request_duration_seconds_bucket{route="overview",le="+Inf"} 1`,
		`dashboard_http_request_duration_seconds_sum{route="overview"} 12`,
		`dashboard_http_request_duration_seconds_count{route="overview"} 1`,
		`dashboard_http_response_bytes_sum{route="overview"} 10`,
		`dashboard_http_response_bytes_count{route="overview"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing histogram invariant %q", want)
		}
	}
	var previous uint64
	for i := range durationBuckets {
		var got uint64
		needle := fmt.Sprintf("dashboard_http_request_duration_seconds_bucket{route=%q,le=%q} %%d", "overview", fmt.Sprintf("%g", durationBuckets[i]))
		if _, err := fmt.Sscanf(findLine(out, strings.Split(needle, " %d")[0]), needle, &got); err != nil {
			t.Fatal(err)
		}
		if got < previous {
			t.Fatal("histogram buckets are not cumulative")
		}
		previous = got
	}
}

func TestDynamicInputsCannotCreateSeries(t *testing.T) {
	m := New()
	var before bytes.Buffer
	_ = m.WritePrometheus(&before)
	for i := 0; i < 10000; i++ {
		m.HTTPStarted()
		m.HTTPFinished(fmt.Sprintf("/raw/%d?q=secret", i), 404, i, time.Millisecond, false, false)
		m.ObserveUpstream(context.Background(), upstream.Upstream(200+i), upstream.Outcome(200+i), time.Millisecond)
	}
	var after bytes.Buffer
	_ = m.WritePrometheus(&after)
	if lineCount(before.String()) != lineCount(after.String()) {
		t.Fatal("dynamic input changed series count")
	}
	if strings.Contains(after.String(), "secret") || strings.Contains(after.String(), "token-") {
		t.Fatal("dynamic input leaked")
	}
}

func TestInvalidEnumsAndStaleCircuitUpdateAreBounded(t *testing.T) {
	m := New()
	invalid := -1
	m.ObserveUpstream(context.Background(), upstream.Upstream(invalid), upstream.Outcome(invalid), time.Millisecond)
	m.SetCircuit(upstream.UpstreamGreptime, upstream.CircuitOpen, 2)
	m.SetCircuit(upstream.UpstreamGreptime, upstream.CircuitClosed, 1)
	m.SetCircuit(upstream.Upstream(invalid), upstream.CircuitState(invalid), 3)
	var b bytes.Buffer
	if err := m.WritePrometheus(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, `dashboard_upstream_circuit_state{upstream="greptime"} 1`) {
		t.Fatal("stale circuit update won")
	}
}

func TestTraceRefsAreSortedDeduplicatedAndBounded(t *testing.T) {
	ctx, trace := WithTrace(context.Background())
	for i := 40; i >= 0; i-- {
		RecordQueryRef(ctx, fmt.Sprintf("metrics.ref.%02d", i))
		RecordQueryRef(ctx, fmt.Sprintf("metrics.ref.%02d", i))
	}
	refs, overflow := trace.Summary()
	if !overflow || len(strings.Split(refs, ",")) != maxQueryRefs {
		t.Fatalf("refs=%d overflow=%v", len(strings.Split(refs, ",")), overflow)
	}
	if refs != strings.Join(sortedCopy(strings.Split(refs, ",")), ",") {
		t.Fatal("refs not sorted")
	}
}

func sortedCopy(v []string) []string { sort.Strings(v); return v }

func BenchmarkHTTPObserve(b *testing.B) {
	m := New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.HTTPStarted()
		m.HTTPFinished("overview", 200, 4096, 25*time.Millisecond, false, false)
	}
}

func BenchmarkPrometheusExport(b *testing.B) {
	m := New()
	m.HTTPStarted()
	m.HTTPFinished("overview", 200, 4096, 25*time.Millisecond, false, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := m.WritePrometheus(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

func findLine(s, prefix string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
func lineCount(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if line != "" && line[0] != '#' {
			n++
		}
	}
	return n
}
