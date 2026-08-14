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
	"fmt"
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

// TestLiveQuickwitCursorAdvancesWithoutDuplicates — 운영 인덱스의 실데이터 위에서
// 커서가 전진하고, 페이지 간 중복이 없고, 정렬이 내림차순인지 확인합니다.
// 읽기 전용입니다. 데이터가 없으면 그 사실만 기록하고 통과합니다.
func TestLiveQuickwitCursorAdvancesWithoutDuplicates(t *testing.T) {
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
      {"name": "span_id", "type": "text", "tokenizer": "raw"}
    ]
  },
  "search_settings": {"default_search_fields": ["message"]}
}`

// TestLiveQuickwitEndToEndPaging — 전용 인덱스에 timestamp 충돌 문서를 넣고
// 실서버에서 중복·누락 없는 전체 순회, Scope 필터, 서버 마스킹을 확인합니다.
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

	// 같은 초에 7건씩 몰리는 300건 — 커서 경계의 실제 난이도입니다.
	now := time.Now().Add(-time.Minute).Unix()
	var buf bytes.Buffer
	total := 300
	for i := 0; i < total; i++ {
		ns := "itest-a"
		if i%3 == 2 {
			ns = "itest-b"
		}
		doc := map[string]any{
			"timestamp": now - int64(i/7),
			"level":     []string{"INFO", "warn", "ERROR"}[i%3],
			"message":   fmt.Sprintf("request %04d authorization Bearer abcdef%016d done", i, i),
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

	s := liveSource(t, itestIndex)
	q := datasource.LogQuery{
		Target: datasource.Target{ClusterID: "itest", Namespace: "itest-a"},
		Window: datasource.Window{
			From: time.Unix(now-3600, 0), To: time.Unix(now+60, 0), Step: time.Minute,
		},
		PageSize: 40,
	}

	wantA := 0
	for i := 0; i < total; i++ {
		if i%3 != 2 {
			wantA++
		}
	}

	seen := map[string]int{}
	got := 0
	for page := 0; page < 50; page++ {
		res, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
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

	t.Logf("실서버 전체 순회 %d/%d · 중복 0 · Scope·마스킹·레벨 필터 확인", got, wantA)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
