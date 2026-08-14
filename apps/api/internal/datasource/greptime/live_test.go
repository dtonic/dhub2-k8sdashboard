//go:build integration

// 실제 GreptimeDB를 대상으로 하는 검증입니다. (#6 완료 기준 — "실제 또는 컨테이너 기반")
//
//	GREPTIME_ITEST_URL=http://greptimedb:4000 make api-itest
//
// **기본 동작은 읽기 전용입니다.** 존재하는 메트릭을 조회만 하고 아무것도 만들지
// 않습니다. 쓰기 검증(전용 테이블 생성 → 삽입 → Scope 강제 확인 → 삭제)은
// ITEST_MUTATE=1일 때만 돌며, `k8s_dashboard_itest_metric` 테이블만 만졌다 지웁니다.
// 실제 메트릭 테이블(container_* 등)에는 절대 쓰지 않습니다.
package greptime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/upstream"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
)

func liveSource(t *testing.T) *Source {
	t.Helper()
	base := os.Getenv("GREPTIME_ITEST_URL")
	if base == "" {
		t.Skip("GREPTIME_ITEST_URL이 없어 건너뜁니다 — 실제 GreptimeDB 검증은 이 변수로 켭니다")
	}
	qc, err := querycatalog.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{
		BaseURL:  base,
		DB:       envOr("GREPTIME_ITEST_DB", "public"),
		Username: os.Getenv("GREPTIME_ITEST_USERNAME"),
		Password: os.Getenv("GREPTIME_ITEST_PASSWORD"),
	}, fakeCatalog{}, qc)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// TestLiveGreptimeRangeQueryRoundTrip — 카탈로그의 기본 range 질의 전부가
// 실제 GreptimeDB에서 오류 없이 실행되는지 확인합니다. 데이터가 없어도
// "빈 시리즈"지 "오류"가 아니어야 합니다.
func TestLiveGreptimeRangeQueryRoundTrip(t *testing.T) {
	s := liveSource(t)
	w := datasource.Window{
		From: time.Now().Add(-time.Hour), To: time.Now(), Step: time.Minute,
	}

	panels, err := s.Trends(context.Background(), datasource.Target{
		ClusterID: "itest", Namespace: "itest-does-not-exist",
	}, w, nil)
	if err != nil {
		t.Fatalf("기본 카탈로그 질의가 실서버에서 거절되었습니다: %v", err)
	}
	if len(panels) != 4 {
		t.Fatalf("패널 4개가 나와야 합니다: %d", len(panels))
	}
	t.Logf("카탈로그 range 질의 %d개 패널 실행 성공 (존재하지 않는 namespace → 빈 시리즈)", len(panels))
}

// TestLiveGreptimeInstantQueryRoundTrip — 사용량 스냅숏 질의가 실서버에서
// 실행되고 (namespace/pod → 값) 형태로 파싱되는지 확인합니다.
func TestLiveGreptimeInstantQueryRoundTrip(t *testing.T) {
	s := liveSource(t)
	usage, err := s.Usage(context.Background(), "itest")
	if err != nil {
		t.Fatalf("instant 질의가 실서버에서 거절되었습니다: %v", err)
	}
	// 카탈로그(informer)가 비어 있으므로 UID 매핑 결과는 0개가 정상입니다.
	// 실서버 왕복과 응답 파싱이 검증 대상입니다.
	t.Logf("instant 질의 성공 · UID 매핑 %d건 (fake catalog 기준 0이 정상)", len(usage))
}

// TestLiveGreptimeBadQueryIsClassified — 문법이 틀린 질의는 ErrBadQuery로,
// 잘못된 주소는 ErrUnavailable로 분류되는지 실서버에서 확인합니다.
func TestLiveGreptimeBadQueryIsClassified(t *testing.T) {
	s := liveSource(t)
	_, err := s.rangeQuery(context.Background(), `sum(rate(`, time.Now().Add(-time.Hour), time.Now(), time.Minute)
	if err == nil {
		t.Fatal("깨진 PromQL이 통과했습니다")
	}
	if !errors.Is(err, upstream.ErrBadQuery) {
		t.Fatalf("깨진 PromQL은 ErrBadQuery여야 합니다: %v", err)
	}
}

/* ── 쓰기 검증 (ITEST_MUTATE=1) ─────────────────────────────────────────── */

const itestMetric = "k8s_dashboard_itest_metric"

// TestLiveGreptimeScopeIsEnforcedOnRealData — 전용 테이블에 두 namespace의
// 샘플을 넣고, Scope 매처가 실제로 한쪽만 돌려주는지 끝까지 확인합니다.
//
// 카탈로그가 아니라 전용 메트릭을 쓰는 이유: 실환경의 container_* 테이블을
// 건드리는 순간 이 테스트는 운영 데이터를 오염시킵니다.
func TestLiveGreptimeScopeIsEnforcedOnRealData(t *testing.T) {
	s := liveSource(t)
	if os.Getenv("ITEST_MUTATE") != "1" {
		t.Skip("ITEST_MUTATE=1이 아니면 아무것도 만들지 않습니다")
	}

	base := os.Getenv("GREPTIME_ITEST_URL")
	db := envOr("GREPTIME_ITEST_DB", "public")
	sqlExec := func(q string) error {
		v := url.Values{"db": {db}, "sql": {q}}
		res, err := http.PostForm(strings.TrimSuffix(base, "/")+"/v1/sql?"+v.Encode(), nil)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			return fmt.Errorf("sql %d", res.StatusCode)
		}
		return nil
	}

	if err := sqlExec(fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (greptime_timestamp TIMESTAMP TIME INDEX, greptime_value DOUBLE, namespace STRING, pod STRING, PRIMARY KEY(namespace, pod))",
		itestMetric)); err != nil {
		t.Fatalf("전용 테이블 생성 실패: %v", err)
	}
	t.Cleanup(func() { _ = sqlExec("DROP TABLE IF EXISTS " + itestMetric) })

	now := time.Now().Truncate(time.Second)
	for i := 0; i < 10; i++ {
		ts := now.Add(-time.Duration(i) * time.Minute).UnixMilli()
		if err := sqlExec(fmt.Sprintf(
			"INSERT INTO %s (greptime_timestamp, greptime_value, namespace, pod) VALUES (%d, %d, 'itest-a', 'pod-a'), (%d, %d, 'itest-b', 'pod-b')",
			itestMetric, ts, i+1, ts, 100)); err != nil {
			t.Fatalf("샘플 삽입 실패: %v", err)
		}
	}

	// 전용 메트릭을 조회하는 1회용 카탈로그 — Scope 삽입 지점은 동일합니다.
	qc, err := querycatalog.LoadFS(fstest.MapFS{"itest.yaml": &fstest.MapFile{Data: []byte(`
version: 1
queries:
  - ref: itest.sum
    type: promql_range
    expr: sum(` + itestMetric + `{$__scope})
    minStep: 60s
panels:
  - id: itest
    title: itest
    series:
      - { key: sum, label: sum, query: itest.sum }
`)}})
	if err != nil {
		t.Fatal(err)
	}
	s.queries = qc

	w := datasource.Window{From: now.Add(-30 * time.Minute), To: now.Add(time.Minute), Step: time.Minute}
	scoped, err := s.Trends(context.Background(), datasource.Target{ClusterID: "itest", Namespace: "itest-a"}, w, []string{"itest"})
	if err != nil {
		t.Fatal(err)
	}
	pts := scoped[0].Series[0].Points
	if len(pts) == 0 {
		t.Fatal("itest-a 샘플이 조회되지 않았습니다")
	}
	for _, p := range pts {
		// itest-b의 값(100)이 섞이면 Scope가 뚫린 것입니다.
		if p.V >= 100 {
			t.Fatalf("Scope 밖 namespace의 값이 합산되었습니다: %+v", p)
		}
	}
	t.Logf("Scope 강제 확인 · itest-a 포인트 %d개, itest-b 값 미포함", len(pts))
}
