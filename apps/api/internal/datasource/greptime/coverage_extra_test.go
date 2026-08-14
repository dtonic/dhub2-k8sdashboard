package greptime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
)

// TestWorkloadTargetExpandsToPodSet — 워크로드 대상은 카탈로그의 Pod 이름
// 집합으로 풀립니다. Kind가 다르면 매칭되지 않습니다.
func TestWorkloadTargetExpandsToPodSet(t *testing.T) {
	f := newFakeGreptime(nil)
	s := newSource(t, f)

	_, err := s.Trends(context.Background(), datasource.Target{
		ClusterID: "seoul", Namespace: "payments",
		WorkloadKind: "Deployment", WorkloadName: "payments-api",
	}, window(time.Minute, time.Hour), []string{"restarts"})
	if err != nil {
		t.Fatal(err)
	}
	q := f.requests[0].Get("query")
	if !strings.Contains(q, `pod=~"^(payments-api-7f-aaa|payments-api-7f-bbb)$"`) {
		t.Fatalf("워크로드 Pod 집합 매처가 없습니다: %s", q)
	}

	// Kind 불일치 — Pod가 없으므로 질의 없이 빈 시리즈입니다.
	f2 := newFakeGreptime(nil)
	s2 := newSource(t, f2)
	panels, err := s2.Trends(context.Background(), datasource.Target{
		ClusterID: "seoul", Namespace: "payments",
		WorkloadKind: "StatefulSet", WorkloadName: "payments-api",
	}, window(time.Minute, time.Hour), []string{"restarts"})
	if err != nil {
		t.Fatal(err)
	}
	if f2.hits.Load() != 0 || len(panels[0].Series[0].Points) != 0 {
		t.Fatal("Kind가 다른 워크로드에 질의가 나갔습니다")
	}
}

// TestSampleParsingEdgeCases — Prometheus 응답 샘플 해석의 경계값입니다.
func TestSampleParsingEdgeCases(t *testing.T) {
	// 타임스탬프: float 초, 문자열 초, 해석 불가
	if ts, ok := sampleTime(float64(1700000000.5)); !ok || ts != 1700000000500 {
		t.Fatalf("float 초: %d %v", ts, ok)
	}
	if ts, ok := sampleTime("1700000000"); !ok || ts != 1700000000000 {
		t.Fatalf("문자열 초: %d %v", ts, ok)
	}
	if _, ok := sampleTime("nope"); ok {
		t.Fatal("깨진 타임스탬프가 통과했습니다")
	}
	if _, ok := sampleTime(nil); ok {
		t.Fatal("nil 타임스탬프가 통과했습니다")
	}

	// 값: 문자열·float 허용, 그 외 거절
	if v, ok := parseSample("1.5"); !ok || v != 1.5 {
		t.Fatal("문자열 값 해석 실패")
	}
	if v, ok := parseSample(float64(2)); !ok || v != 2 {
		t.Fatal("float 값 해석 실패")
	}
	if _, ok := parseSample("abc"); ok {
		t.Fatal("깨진 값이 통과했습니다")
	}
	if _, ok := parseSample(nil); ok {
		t.Fatal("nil 값이 통과했습니다")
	}

	// instant 샘플: NaN·nil은 버립니다
	if _, ok := sampleValue([2]any{float64(1), "NaN"}); ok {
		t.Fatal("NaN instant가 통과했습니다")
	}
	if _, ok := sampleValue([2]any{nil, nil}); ok {
		t.Fatal("빈 instant가 통과했습니다")
	}
	if v, ok := sampleValue([2]any{float64(1), "42"}); !ok || v != 42 {
		t.Fatal("정상 instant 해석 실패")
	}
}

// sqlSource는 고정 JSON을 돌려주는 서버에 붙은 Source를 만듭니다.
func sqlSource(t *testing.T, body string) *Source {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/sql") {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	qcat, err := querycatalog.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{BaseURL: srv.URL}, catalog, qcat)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSQLQuerierParsesRecords — 집계용 SQL 경로의 응답 정규화입니다.
func TestSQLQuerierParsesRecords(t *testing.T) {
	s := sqlSource(t, `{
	  "output": [{"records": {
	    "schema": {"column_schemas": [{"name": "namespace"}, {"name": "total"}]},
	    "rows": [["payments", 12], ["media", 3]]
	  }}]
	}`)
	res, err := s.SQL().Query(context.Background(), "SELECT namespace, count(*) FROM t GROUP BY namespace")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Columns) != 2 || res.Columns[0] != "namespace" || len(res.Rows) != 2 {
		t.Fatalf("SQL 결과: %+v", res)
	}

	// 빈 output도 오류가 아닙니다 — 결과 없는 질의입니다.
	s2 := sqlSource(t, `{"output": []}`)
	res, err = s2.SQL().Query(context.Background(), "SELECT 1")
	if err != nil || len(res.Columns) != 0 {
		t.Fatalf("빈 결과: %+v %v", res, err)
	}
}
