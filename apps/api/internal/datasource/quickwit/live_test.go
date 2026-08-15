//go:build integration

// 실제 Quickwit을 대상으로 하는 검증입니다. (#7 완료 기준)
//
//	QUICKWIT_ITEST_URL=http://quickwit:7280 make api-itest
//
// **기본 동작은 읽기 전용입니다.** QUICKWIT_ITEST_INDEX(기본 k8s-logs)를 조회만
// 하며, 실데이터 위에서 커서 전진·중복 없음·정렬을 확인합니다.
//
// 쓰기 검증은 ITEST_MUTATE=1일 때만 돕니다 — 전용 인덱스
// `k8s-dashboard-itest`를 만들고, 같은 밀리초에 몰린 문서를 넣어
// 중복·누락 없는 커서 페이징과 Scope·마스킹을 끝까지 확인한 뒤 삭제합니다.
package quickwit

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

func liveBase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("QUICKWIT_ITEST_URL")
	if base == "" {
		t.Skip("QUICKWIT_ITEST_URL이 없어 건너뜁니다 — 실제 Quickwit 검증은 이 변수로 켭니다")
	}
	return strings.TrimSuffix(base, "/")
}

func liveSource(t *testing.T, index string) *Source {
	t.Helper()
	s, err := New(Config{BaseURL: liveBase(t), Index: index}, fakeCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLiveQuickwitOTLPSchemaCompatibility(t *testing.T) {
	if os.Getenv("QUICKWIT_OTEL_SCHEMA") != "1" {
		t.Skip("QUICKWIT_OTEL_SCHEMA=1 enables the dedicated OTLP schema fixture")
	}
	s, err := New(Config{BaseURL: liveBase(t), Index: "otel-logs-v0_7", Fields: FieldMap{
		Timestamp: "timestamp_nanos", Level: "severity_text", Message: "body.message",
		Cluster:   "resource_attributes.k8s.cluster.name",
		Namespace: "resource_attributes.k8s.namespace.name", PodName: "resource_attributes.k8s.pod.name",
		PodUID: "resource_attributes.k8s.pod.uid", Container: "resource_attributes.k8s.container.name",
		WorkloadKind: "resource_attributes.k8s.workload.kind", WorkloadName: "resource_attributes.k8s.workload.name",
		Node: "resource_attributes.k8s.node.name", EventID: "attributes.event_id",
	}, ClusterScoped: true}, fakeCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	q := datasource.LogQuery{Target: datasource.Target{ClusterID: "cluster-a", Namespace: "payments"},
		Window: datasource.Window{From: time.Now().Add(-5 * time.Minute), To: time.Now().Add(time.Minute), Step: time.Minute}, PageSize: 1}
	page, err := s.Search(context.Background(), q)
	if err != nil || len(page.Lines) != 1 || page.Next == "" {
		t.Fatalf("OTLP first page: lines=%d next=%q err=%v", len(page.Lines), page.Next, err)
	}
	first := page.Lines[0]
	for _, sensitive := range []string{"hunter2", "abcdefghijklmnop", "4111 1111", "user@example.com", "10.20.30.40"} {
		if strings.Contains(first.Message, sensitive) {
			t.Fatalf("OTLP sensitive value escaped masking: %q", first.Message)
		}
	}
	if first.ID != "event-cluster-a-2" || first.T == 0 || first.Message == "" || first.Namespace != "payments" || first.PodName != "checkout-same" ||
		first.PodUID != "pod-same" || first.ContainerName != "api" || first.WorkloadKind != "Deployment" || first.WorkloadName != "checkout" ||
		first.NodeName != "node-a" || first.TraceID != "0123456789abcdef0123456789abcdef" || first.SpanID != "0123456789abcdef" || first.Level != contract.LevelError {
		t.Fatalf("OTLP field/scope/masking mismatch: %+v", first)
	}
	crossCluster := q
	crossCluster.Target.ClusterID = "cluster-b"
	crossCluster.Cursor = page.Next
	if _, err = s.Search(context.Background(), crossCluster); err == nil {
		t.Fatal("cluster-a cursor was accepted for cluster-b")
	}
	q.Cursor = page.Next
	second, err := s.Search(context.Background(), q)
	if err != nil || len(second.Lines) != 1 || second.Lines[0].ID == first.ID {
		t.Fatalf("OTLP cursor duplicate: first=%+v second=%+v err=%v", first, second.Lines, err)
	}
	filtered := q
	filtered.Cursor = ""
	filtered.PageSize = 10
	filtered.Target.PodUID = "pod-same"
	filtered.Target.WorkloadKind = "Deployment"
	filtered.Target.WorkloadName = "checkout"
	filtered.Container = "api"
	filtered.Levels = []contract.LogLevel{contract.LevelError}
	filteredPage, err := s.Search(context.Background(), filtered)
	if err != nil || len(filteredPage.Lines) != 2 || filteredPage.Lines[0].PodUID != "pod-same" {
		t.Fatalf("OTLP pod/workload/container/level filter: %+v err=%v", filteredPage, err)
	}
	facets, err := s.Facets(context.Background(), datasource.LogQuery{Target: q.Target, Window: q.Window})
	if err != nil || len(facets.Workloads) != 1 || facets.Workloads[0].Name != "checkout" || facets.Workloads[0].Count != 2 || len(facets.Pods) != 1 || len(facets.Containers) != 1 {
		t.Fatalf("OTLP facets: %+v err=%v", facets, err)
	}
	hist, err := s.Histogram(context.Background(), datasource.LogQuery{Target: q.Target, Window: q.Window})
	if err != nil || len(hist) == 0 {
		t.Fatalf("OTLP histogram: %+v err=%v", hist, err)
	}
}

// TestLiveQuickwitCursorAdvancesWithoutDuplicates — 운영 인덱스의 실데이터 위에서
// 커서가 전진하고, 페이지 간 중복이 없고, 정렬이 내림차순인지 확인합니다.
// 읽기 전용입니다. 데이터가 없으면 그 사실만 기록하고 통과합니다.
func TestLiveQuickwitCursorAdvancesWithoutDuplicates(t *testing.T) {
	if os.Getenv("ITEST_MUTATE") == "1" {
		t.Skip("쓰기 검증이 전용 인덱스에서 더 강한 cursor 계약을 확인합니다")
	}
	s := liveSource(t, envOr("QUICKWIT_ITEST_INDEX", "k8s-logs"))

	q := datasource.LogQuery{
		Target: datasource.Target{ClusterID: "itest"},
		Window: datasource.Window{
			From: time.Now().Add(-24 * time.Hour), To: time.Now(), Step: time.Minute,
		},
		PageSize: 50,
	}

	seen := map[string]bool{}
	lastT := int64(1<<62 - 1)
	pages := 0
	for pages < 5 {
		res, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatalf("실서버 검색이 실패했습니다: %v", err)
		}
		for _, l := range res.Lines {
			if seen[l.ID] {
				t.Fatalf("페이지 간 중복: %s", l.ID)
			}
			seen[l.ID] = true
			if l.T > lastT {
				t.Fatalf("정렬이 내림차순이 아닙니다: %d > %d", l.T, lastT)
			}
			lastT = l.T
		}
		pages++
		if res.Next == "" {
			break
		}
		q.Cursor = res.Next
	}
	t.Logf("커서 %d페이지 · 고유 %d줄 · 중복 0", pages, len(seen))
}

/* ── 쓰기 검증 (ITEST_MUTATE=1) ─────────────────────────────────────────── */

const itestIndex = "k8s-dashboard-itest"

// itestIndexConfig는 기본 FieldMap과 같은 스키마입니다. 필터·집계 필드는
// fast(raw)여야 한다는 README의 요구가 실제로 성립하는지도 함께 검증됩니다.
const itestIndexConfig = `{
  "version": "0.8",
  "index_id": "` + itestIndex + `",
  "doc_mapping": {
    "timestamp_field": "timestamp",
    "field_mappings": [
      {"name": "timestamp", "type": "datetime", "input_formats": ["unix_timestamp"], "output_format": "unix_timestamp_millis", "fast": true},
      {"name": "level", "type": "text", "tokenizer": "raw", "fast": true},
      {"name": "message", "type": "text"},
      {"name": "namespace", "type": "text", "tokenizer": "raw", "fast": true},
      {"name": "pod_name", "type": "text", "tokenizer": "raw", "fast": true},
      {"name": "pod_uid", "type": "text", "tokenizer": "raw", "fast": true},
      {"name": "container", "type": "text", "tokenizer": "raw", "fast": true},
      {"name": "workload_kind", "type": "text", "tokenizer": "raw"},
      {"name": "workload_name", "type": "text", "tokenizer": "raw", "fast": true},
      {"name": "node", "type": "text", "tokenizer": "raw"},
      {"name": "trace_id", "type": "text", "tokenizer": "raw"},
      {"name": "span_id", "type": "text", "tokenizer": "raw"},
      {"name": "event_id", "type": "text", "tokenizer": "raw"}
    ]
  },
  "search_settings": {"default_search_fields": ["message"]}
}`

// TestLiveQuickwitEndToEndPaging — 전용 인덱스에 timestamp 충돌 문서를 넣고
// 실서버에서 512건을 넘는 동일 timestamp 충돌의 전체 순회, MaxLines 상한,
// Scope 필터, 서버 마스킹을 확인합니다.
func TestLiveQuickwitEndToEndPaging(t *testing.T) {
	base := liveBase(t)
	if os.Getenv("ITEST_MUTATE") != "1" {
		t.Skip("ITEST_MUTATE=1이 아니면 아무것도 만들지 않습니다")
	}

	// 전용 인덱스 생성 (있으면 지우고 새로)
	req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/indexes/"+itestIndex, nil)
	http.DefaultClient.Do(req) //nolint:errcheck // 없으면 404 — 무시
	res, err := http.Post(base+"/api/v1/indexes", "application/json", strings.NewReader(itestIndexConfig))
	if err != nil || res.StatusCode >= 300 {
		t.Fatalf("전용 인덱스 생성 실패: %v (status %v)", err, res)
	}
	res.Body.Close()
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/indexes/"+itestIndex, nil)
		http.DefaultClient.Do(req) //nolint:errcheck
	})

	// 같은 timestamp에 700건을 몰아 과거 경계 id 512개 제한을 실제로 넘깁니다.
	now := time.Now().Add(-time.Minute).Unix()
	var buf bytes.Buffer
	total := 720
	for i := 0; i < total; i++ {
		ns := "itest-a"
		if i >= 700 {
			ns = "itest-b"
		}
		doc := map[string]any{
			"timestamp": now,
			"level":     "warn",
			"message":   "identical authorization Bearer abcdef0000000000 done",
			"namespace": ns, "pod_name": "itest-pod", "pod_uid": "itest-uid",
			"container": "app", "workload_kind": "Deployment", "workload_name": "itest",
		}
		raw, _ := json.Marshal(doc)
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	res, err = http.Post(base+"/api/v1/"+itestIndex+"/ingest?commit=force", "application/x-ndjson", &buf)
	if err != nil || res.StatusCode >= 300 {
		t.Fatalf("문서 삽입 실패: %v (status %v)", err, res)
	}
	res.Body.Close()

	s, err := New(Config{BaseURL: base, Index: itestIndex, MaxLines: 800, MaxPageSize: 100}, fakeCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	q := datasource.LogQuery{
		Target: datasource.Target{
			ClusterID: "itest", Namespace: "itest-a", PodUID: "itest-uid",
			WorkloadKind: "Deployment", WorkloadName: "itest",
		},
		Window: datasource.Window{
			From: time.Unix(now-3600, 0), To: time.Unix(now+60, 0), Step: time.Minute,
		},
		PageSize: 73,
	}

	wantA := 700

	seen := map[string]int{}
	got := 0
	maxCursorBytes := 0
	pages := 0
	for page := 0; page < 20; page++ {
		res, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(res.Next) > maxCursorBytes {
			maxCursorBytes = len(res.Next)
		}
		for _, l := range res.Lines {
			seen[l.ID]++
			got++
			if l.Namespace != "itest-a" {
				t.Fatalf("Scope 밖 문서가 나갔습니다: %s", l.Namespace)
			}
			if strings.Contains(l.Message, "Bearer abcdef") {
				t.Fatalf("마스킹되지 않은 토큰이 나갔습니다: %s", l.Message)
			}
			if !strings.HasPrefix(l.ID, "scroll-") {
				t.Fatalf("event_id 없는 문서에 traversal ID가 쓰이지 않았습니다: %s", l.ID)
			}
		}
		if res.Next == "" {
			break
		}
		q.Cursor = res.Next
	}

	if got != wantA {
		t.Fatalf("누락 또는 초과: got %d want %d", got, wantA)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("중복: %s ×%d", id, n)
		}
	}

	// 같은 실제 데이터에서 cap을 넘지 않고 Next를 닫아야 합니다.
	capped, err := New(Config{BaseURL: base, Index: itestIndex, MaxLines: 550, MaxPageSize: 100}, fakeCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	q.Cursor = ""
	q.Levels = nil
	cappedTotal := 0
	for page := 0; page < 20; page++ {
		pageResult, err := capped.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		cappedTotal += len(pageResult.Lines)
		if pageResult.Next == "" {
			if !pageResult.Truncated {
				t.Fatal("MaxLines 뒤 실데이터가 있는데 truncated=false입니다")
			}
			break
		}
		q.Cursor = pageResult.Next
	}
	if cappedTotal != 511 {
		t.Fatalf("실서버 bounded MaxLines 불일치: got %d want 511", cappedTotal)
	}

	// 레벨 필터 — 소문자 warn 문서가 WARN 필터에 걸리는지 실서버에서 확인합니다.
	q.Cursor = ""
	q.Levels = []contract.LogLevel{contract.LevelWarn}
	resWarn, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(resWarn.Lines) == 0 {
		t.Fatal("레벨 필터가 실서버에서 동작하지 않습니다")
	}

	t.Logf("실서버 동일 timestamp 전체 순회 %d/%d · %d페이지 · cursor 최대 %dB · cap 550 이내 · 중복 0 · Scope·마스킹·레벨 필터 확인", got, wantA, pages, maxCursorBytes)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
