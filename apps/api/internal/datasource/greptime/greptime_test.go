package greptime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

/* ── 픽스처 ─────────────────────────────────────────────────────────────── */

// fakeCatalog는 informer 캐시를 대신합니다. 신원 규칙은 실제와 같습니다 —
// 어댑터는 여기서 빌린 이름만 질의에 쓸 수 있습니다.
type fakeCatalog []datasource.CatalogPod

func (f fakeCatalog) CatalogPods(namespace string, limit int) []datasource.CatalogPod {
	out := []datasource.CatalogPod{}
	for _, p := range f {
		if namespace != "" && p.Namespace != namespace {
			continue
		}
		out = append(out, p)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

var catalog = fakeCatalog{
	{Namespace: "payments", Name: "payments-api-7f-aaa", UID: "uid-aaa", WorkloadKind: "Deployment", WorkloadName: "payments-api"},
	{Namespace: "payments", Name: "payments-api-7f-bbb", UID: "uid-bbb", WorkloadKind: "Deployment", WorkloadName: "payments-api"},
	{Namespace: "media", Name: "media-worker-0", UID: "uid-ccc", WorkloadKind: "StatefulSet", WorkloadName: "media-worker"},
}

// fakeGreptime은 Prometheus 호환 API를 흉내 냅니다. 받은 질의를 기록하고
// 등록된 응답을 돌려줍니다.
type fakeGreptime struct {
	mu       chan struct{}
	requests []url.Values
	// respond는 query 문자열을 보고 matrix 값을 돌려줍니다.
	respond func(query string) [][2]any
	fail    atomic.Int32 // 남은 실패 횟수 (503)
	hits    atomic.Int32
}

func newFakeGreptime(respond func(string) [][2]any) *fakeGreptime {
	return &fakeGreptime{mu: make(chan struct{}, 1), respond: respond}
}

func (f *fakeGreptime) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.fail.Load() > 0 {
			f.fail.Add(-1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/prometheus/api/v1/") {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		f.mu <- struct{}{}
		f.requests = append(f.requests, q)
		<-f.mu

		values := [][2]any{}
		if f.respond != nil {
			values = f.respond(q.Get("query"))
		}
		resultType, key := "matrix", "values"
		if strings.HasSuffix(r.URL.Path, "/query") {
			resultType, key = "vector", "value"
		}
		var results []map[string]any
		if resultType == "vector" {
			for _, v := range values {
				results = append(results, map[string]any{
					"metric": map[string]string{"namespace": "payments", "pod": "payments-api-7f-aaa"},
					key:      v,
				})
			}
		} else {
			results = []map[string]any{{"metric": map[string]string{}, key: values}}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": resultType, "result": results},
		})
	})
}

func newSource(t *testing.T, f *fakeGreptime, mutate ...func(*Config)) *Source {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	cfg := Config{BaseURL: srv.URL, DB: "metrics"}
	for _, m := range mutate {
		m(&cfg)
	}
	s, err := New(cfg, catalog)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func window(step time.Duration, span time.Duration) datasource.Window {
	to := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	return datasource.Window{From: to.Add(-span), To: to, Step: step}
}

/* ── 테스트 ─────────────────────────────────────────────────────────────── */

// TestRangeQueryCarriesWindowAndScope는 질의가 서버 확정 값(범위·Step·Scope)을
// 그대로 싣는지 확인합니다. 프런트가 준 값이 아니라 서버가 만든 매처여야 합니다.
func TestRangeQueryCarriesWindowAndScope(t *testing.T) {
	f := newFakeGreptime(nil)
	s := newSource(t, f)
	w := window(time.Minute, time.Hour)

	_, err := s.Trends(context.Background(), datasource.Target{
		ClusterID: "seoul", Namespace: "payments",
	}, w, []string{"cpu"})
	if err != nil {
		t.Fatal(err)
	}

	if len(f.requests) != 2 { // used + requested
		t.Fatalf("cpu 패널은 시리즈당 1건, 총 2건이어야 합니다: %d", len(f.requests))
	}
	for _, q := range f.requests {
		if q.Get("start") == "" || q.Get("end") == "" || q.Get("step") != "60" {
			t.Fatalf("범위·Step이 서버 확정 값과 다릅니다: %v", q)
		}
		if !strings.Contains(q.Get("query"), `namespace="payments"`) {
			t.Fatalf("namespace Scope가 질의에 강제되지 않았습니다: %s", q.Get("query"))
		}
		if q.Get("db") != "metrics" {
			t.Fatalf("db 파라미터가 없습니다: %v", q)
		}
	}
}

// TestPodTargetResolvesNameFromCatalog는 Pod UID가 카탈로그의 이름으로
// 변환되는지 확인합니다. 메트릭 라벨은 이름이지만 화면 신원은 UID입니다.
func TestPodTargetResolvesNameFromCatalog(t *testing.T) {
	f := newFakeGreptime(nil)
	s := newSource(t, f)

	_, err := s.Trends(context.Background(), datasource.Target{
		ClusterID: "seoul", Namespace: "payments", PodUID: "uid-bbb",
	}, window(time.Minute, time.Hour), []string{"memory"})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range f.requests {
		if !strings.Contains(q.Get("query"), `pod="payments-api-7f-bbb"`) {
			t.Fatalf("UID가 카탈로그 이름으로 변환되지 않았습니다: %s", q.Get("query"))
		}
	}
}

// TestUnknownPodReturnsEmptySeriesWithoutQuerying — 카탈로그에 없는 Pod는
// "데이터 없음"이지 장애가 아닙니다. 질의 없이 빈 시리즈를 돌려줍니다.
func TestUnknownPodReturnsEmptySeriesWithoutQuerying(t *testing.T) {
	f := newFakeGreptime(nil)
	s := newSource(t, f)

	panels, err := s.Trends(context.Background(), datasource.Target{
		ClusterID: "seoul", Namespace: "payments", PodUID: "uid-ghost",
	}, window(time.Minute, time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 0 {
		t.Fatalf("카탈로그에 없는 Pod인데 질의가 나갔습니다: %d회", f.hits.Load())
	}
	if len(panels) != 4 {
		t.Fatalf("패널 골격은 유지되어야 합니다: %d", len(panels))
	}
	for _, p := range panels {
		for _, ser := range p.Series {
			if len(ser.Points) != 0 {
				t.Fatalf("빈 시리즈여야 합니다: %s/%s", p.ID, ser.Key)
			}
		}
	}
}

// TestMultiNamespaceScopeBecomesAnchoredRegex는 허용 목록이 앵커된 정규식
// 매처로 강제되는지, 이름의 정규식 문자가 이스케이프되는지 확인합니다.
func TestMultiNamespaceScopeBecomesAnchoredRegex(t *testing.T) {
	f := newFakeGreptime(nil)
	s := newSource(t, f)

	_, err := s.Trends(context.Background(), datasource.Target{
		ClusterID: "seoul", Namespaces: []string{"media", "pay.ments"},
	}, window(time.Minute, time.Hour), []string{"network"})
	if err != nil {
		t.Fatal(err)
	}
	want := `namespace=~"^(media|pay\\.ments)$"`
	for _, q := range f.requests {
		if !strings.Contains(q.Get("query"), want) {
			t.Fatalf("허용 목록 매처가 없습니다:\n got %s\nwant 포함 %s", q.Get("query"), want)
		}
	}
}

// TestPointsAreSortedFiniteAndDeduped는 upstream 응답의 NaN·Inf·역순·중복
// 샘플이 화면에 도달하지 않는지 확인합니다. (#6 작업 범위)
func TestPointsAreSortedFiniteAndDeduped(t *testing.T) {
	f := newFakeGreptime(func(string) [][2]any {
		return [][2]any{
			{float64(3000), "3"},
			{float64(1000), "NaN"},
			{float64(2000), "+Inf"},
			{float64(1000), "1"},
			{float64(3000), "3.5"}, // 같은 타임스탬프 — 마지막 값만 남습니다
		}
	})
	s := newSource(t, f)

	panels, err := s.Trends(context.Background(), datasource.Target{
		ClusterID: "seoul", Namespace: "payments",
	}, window(time.Minute, time.Hour), []string{"restarts"})
	if err != nil {
		t.Fatal(err)
	}
	pts := panels[0].Series[0].Points
	if len(pts) != 2 {
		t.Fatalf("NaN·Inf 제거 후 2포인트여야 합니다: %+v", pts)
	}
	if pts[0].T != 1000*1000 || pts[1].T != 3000*1000 {
		t.Fatalf("정렬이 깨졌습니다: %+v", pts)
	}
	if pts[1].V != 3.5 {
		t.Fatalf("같은 타임스탬프는 마지막 값이어야 합니다: %+v", pts)
	}
}

// TestStepWidensToRespectMaxDataPoints — 장기 조회에서도 포인트 상한을
// 지킵니다. Step은 넓어질 수만 있고, 원래 Step의 배수를 유지합니다.
func TestStepWidensToRespectMaxDataPoints(t *testing.T) {
	f := newFakeGreptime(nil)
	s := newSource(t, f, func(c *Config) { c.MaxDataPoints = 100 })
	w := window(time.Minute, 24*time.Hour) // 1440포인트 → 상한 100 초과

	panels, err := s.Trends(context.Background(), datasource.Target{
		ClusterID: "seoul", Namespace: "payments",
	}, w, []string{"restarts"})
	if err != nil {
		t.Fatal(err)
	}
	q := f.requests[0]
	stepSec := q.Get("step")
	if stepSec == "60" {
		t.Fatal("Step이 넓어지지 않았습니다")
	}
	var sec int
	fmt.Sscanf(stepSec, "%d", &sec)
	if sec%60 != 0 {
		t.Fatalf("넓힌 Step은 원래 Step의 배수여야 합니다: %d", sec)
	}
	if got := int((24 * time.Hour).Seconds()) / sec; got > 100 {
		t.Fatalf("포인트 수가 상한을 넘습니다: %d", got)
	}
	if panels[0].StepSeconds != sec {
		t.Fatalf("응답 StepSeconds가 실제 질의 Step과 다릅니다: %d != %d", panels[0].StepSeconds, sec)
	}
}

// TestUpstreamFailureIsClassifiedAndRetriedOnce — 503은 1회만 재시도하고,
// 계속 실패하면 표준 upstream 오류(ErrUnavailable)로 분류됩니다.
func TestUpstreamFailureIsClassifiedAndRetriedOnce(t *testing.T) {
	f := newFakeGreptime(nil)
	f.fail.Store(100)
	s := newSource(t, f)

	_, err := s.Trends(context.Background(), datasource.Target{
		ClusterID: "seoul", Namespace: "payments",
	}, window(time.Minute, time.Hour), []string{"restarts"})
	if !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatalf("표준 upstream 오류로 분류되어야 합니다: %v", err)
	}
	if got := f.hits.Load(); got != 2 { // 원 요청 1 + 재시도 1
		t.Fatalf("재시도는 1회여야 합니다: %d회 호출", got)
	}
	// degraded 사유로 노출될 수 있으므로 에러 문자열에 내부 정보가 없어야 합니다.
	if s := err.Error(); strings.Contains(s, "127.0.0.1") || strings.Contains(s, "query") {
		t.Fatalf("에러 문자열에 내부 정보가 있습니다: %s", s)
	}
}

// TestTransientFailureRecoversAfterRetry — 일시 오류 1번은 사용자에게
// 보이지 않아야 합니다.
func TestTransientFailureRecoversAfterRetry(t *testing.T) {
	f := newFakeGreptime(nil)
	f.fail.Store(1)
	s := newSource(t, f)

	_, err := s.Trends(context.Background(), datasource.Target{
		ClusterID: "seoul", Namespace: "payments",
	}, window(time.Minute, time.Hour), []string{"restarts"})
	if err != nil {
		t.Fatalf("재시도로 회복해야 합니다: %v", err)
	}
}

// TestUsageKeysByPodUID는 사용량 스냅숏이 이름이 아니라 UID로 키가 되는지,
// 카탈로그에 없는 Pod의 잔여 시계열은 버려지는지 확인합니다. (README §5)
func TestUsageKeysByPodUID(t *testing.T) {
	f := newFakeGreptime(func(query string) [][2]any {
		return [][2]any{{float64(1000), "250"}}
	})
	s := newSource(t, f)

	usage, err := s.Usage(context.Background(), "seoul")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := usage["uid-aaa"]; !ok {
		t.Fatalf("UID 키가 없습니다: %v", usage)
	}
	for key := range usage {
		if strings.Contains(key, "/") {
			t.Fatalf("이름 기반 키가 남아 있습니다: %s", key)
		}
	}
}
