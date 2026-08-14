package quickwit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

/* ── 픽스처: 가짜 Quickwit ──────────────────────────────────────────────── */

type doc struct {
	id        string
	ts        int64 // millis
	ns        string
	pod       string
	uid       string
	level     string
	msg       string
	workload  string
	container string
}

// fakeQuickwit은 ES 호환 검색을 최소한으로 구현합니다 — range·term·terms 필터,
// match(AND), timestamp desc 정렬, size. 정렬 동률은 id로 고정해 결정적입니다.
type fakeQuickwit struct {
	docs   []doc
	mu     sync.Mutex
	bodies []map[string]any
	fail   atomic.Int32
	hits   atomic.Int32
}

func (f *fakeQuickwit) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.fail.Load() > 0 {
			f.fail.Add(-1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v1/_elastic/") || !strings.HasSuffix(r.URL.Path, "/_search") {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("본문 해석 실패: %v", err)
			http.Error(w, "bad body", 400)
			return
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()

		matched := f.filter(body)
		sort.Slice(matched, func(a, b int) bool {
			if matched[a].ts != matched[b].ts {
				return matched[a].ts > matched[b].ts
			}
			return matched[a].id < matched[b].id
		})

		size := 10
		if v, ok := body["size"].(float64); ok {
			size = int(v)
		}
		page := matched
		if len(page) > size {
			page = page[:size]
		}

		res := map[string]any{
			"hits": map[string]any{
				"total": map[string]any{"value": len(matched)},
				"hits":  hitsJSON(page),
			},
		}
		if aggs, ok := body["aggs"].(map[string]any); ok {
			res["aggregations"] = f.aggregate(matched, aggs)
		}
		json.NewEncoder(w).Encode(res)
	})
}

func hitsJSON(page []doc) []any {
	out := make([]any, 0, len(page))
	for _, d := range page {
		out = append(out, map[string]any{
			"_id": d.id,
			"_source": map[string]any{
				"timestamp": d.ts, "namespace": d.ns, "pod_name": d.pod, "pod_uid": d.uid,
				"level": d.level, "message": d.msg, "workload_name": d.workload, "container": d.container,
			},
		})
	}
	return out
}

func (f *fakeQuickwit) filter(body map[string]any) []doc {
	b, _ := body["query"].(map[string]any)
	boolQ, _ := b["bool"].(map[string]any)
	filters, _ := boolQ["filter"].([]any)
	musts, _ := boolQ["must"].([]any)

	keep := func(d doc) bool {
		field := func(name string) string {
			switch name {
			case "namespace":
				return d.ns
			case "pod_name":
				return d.pod
			case "pod_uid":
				return d.uid
			case "level":
				return d.level
			case "workload_name":
				return d.workload
			case "container":
				return d.container
			}
			return ""
		}
		for _, raw := range filters {
			node := raw.(map[string]any)
			if rng, ok := node["range"].(map[string]any); ok {
				spec := rng["timestamp"].(map[string]any)
				if g, ok := spec["gte"].(float64); ok && d.ts < int64(g) {
					return false
				}
				if l, ok := spec["lte"].(float64); ok && d.ts > int64(l) {
					return false
				}
			}
			if tm, ok := node["term"].(map[string]any); ok {
				for name, spec := range tm {
					want := spec.(map[string]any)["value"].(string)
					if field(name) != want {
						return false
					}
				}
			}
			if tms, ok := node["terms"].(map[string]any); ok {
				for name, spec := range tms {
					found := false
					for _, v := range spec.([]any) {
						if field(name) == v.(string) {
							found = true
							break
						}
					}
					if !found {
						return false
					}
				}
			}
		}
		for _, raw := range musts {
			node := raw.(map[string]any)
			if m, ok := node["match"].(map[string]any); ok {
				for _, spec := range m {
					text := spec.(map[string]any)["query"].(string)
					for _, word := range strings.Fields(strings.ToLower(text)) {
						if !strings.Contains(strings.ToLower(d.msg), word) {
							return false
						}
					}
				}
			}
		}
		return true
	}

	var out []doc
	for _, d := range f.docs {
		if keep(d) {
			out = append(out, d)
		}
	}
	return out
}

func (f *fakeQuickwit) aggregate(matched []doc, aggs map[string]any) map[string]any {
	out := map[string]any{}
	termCounts := func(get func(doc) string) []any {
		counts := map[string]int{}
		for _, d := range matched {
			counts[get(d)]++
		}
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buckets := []any{}
		for _, k := range keys {
			buckets = append(buckets, map[string]any{"key": k, "doc_count": counts[k]})
		}
		return buckets
	}
	if spec, ok := aggs["over_time"].(map[string]any); ok {
		dh := spec["date_histogram"].(map[string]any)
		var interval int64
		fmt.Sscanf(dh["fixed_interval"].(string), "%dms", &interval)
		byBucket := map[int64]map[string]int{}
		for _, d := range matched {
			key := d.ts / interval * interval
			if byBucket[key] == nil {
				byBucket[key] = map[string]int{}
			}
			byBucket[key][d.level]++
		}
		keys := make([]int64, 0, len(byBucket))
		for k := range byBucket {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })
		buckets := []any{}
		for _, k := range keys {
			var lvl []any
			for name, c := range byBucket[k] {
				lvl = append(lvl, map[string]any{"key": name, "doc_count": c})
			}
			buckets = append(buckets, map[string]any{
				"key": float64(k), "levels": map[string]any{"buckets": lvl},
			})
		}
		out["over_time"] = map[string]any{"buckets": buckets}
	}
	if _, ok := aggs["workloads"]; ok {
		out["workloads"] = map[string]any{"buckets": termCounts(func(d doc) string { return d.workload })}
		out["pods"] = map[string]any{"buckets": termCounts(func(d doc) string { return d.pod })}
		out["containers"] = map[string]any{"buckets": termCounts(func(d doc) string { return d.container })}
	}
	return out
}

/* ── 픽스처: 문서와 카탈로그 ────────────────────────────────────────────── */

var testEnd = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

// makeDocs는 timestamp 충돌이 심한 문서 집합을 만듭니다.
// 같은 밀리초에 여러 줄이 몰리는 것이 커서 페이징의 실제 난이도입니다. (ADR 0003)
func makeDocs(n int) []doc {
	docs := make([]doc, 0, n)
	for i := 0; i < n; i++ {
		ns, pod, uid, wl := "payments", "payments-api-7f-aaa", "uid-aaa", "payments-api"
		if i%3 == 2 {
			ns, pod, uid, wl = "media", "media-worker-0", "uid-ccc", "media-worker"
		}
		// 7줄씩 같은 밀리초를 공유합니다.
		ts := testEnd.UnixMilli() - int64(i/7)*250
		docs = append(docs, doc{
			id: fmt.Sprintf("doc-%04d", i), ts: ts, ns: ns, pod: pod, uid: uid,
			level: []string{"INFO", "warn", "ERROR"}[i%3],
			msg:   fmt.Sprintf("request %d handled", i), workload: wl, container: "app",
		})
	}
	return docs
}

type fakeCatalog []datasource.CatalogPod

func (f fakeCatalog) CatalogPods(namespace string, limit int) []datasource.CatalogPod {
	out := []datasource.CatalogPod{}
	for _, p := range f {
		if namespace == "" || p.Namespace == namespace {
			out = append(out, p)
		}
	}
	return out
}

var catalog = fakeCatalog{
	{Namespace: "payments", Name: "payments-api-7f-aaa", UID: "uid-aaa", WorkloadKind: "Deployment", WorkloadName: "payments-api"},
	{Namespace: "media", Name: "media-worker-0", UID: "uid-ccc", WorkloadKind: "StatefulSet", WorkloadName: "media-worker"},
}

func newSource(t *testing.T, f *fakeQuickwit, mutate ...func(*Config)) *Source {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	cfg := Config{BaseURL: srv.URL, Index: "k8s-logs"}
	for _, m := range mutate {
		m(&cfg)
	}
	s, err := New(cfg, catalog)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func baseQuery(ns string) datasource.LogQuery {
	return datasource.LogQuery{
		Target: datasource.Target{ClusterID: "seoul", Namespace: ns},
		Window: datasource.Window{
			From: testEnd.Add(-time.Hour), To: testEnd, Step: time.Minute,
		},
	}
}

/* ── 테스트 ─────────────────────────────────────────────────────────────── */

// TestCursorPagingHasNoDuplicatesOrGaps — 커서로 끝까지 넘겼을 때 전체 문서가
// 정확히 한 번씩 나와야 합니다. 같은 밀리초에 몰린 문서가 경계에 걸려도 그렇습니다.
func TestCursorPagingHasNoDuplicatesOrGaps(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(900)}
	s := newSource(t, f)

	q := baseQuery("") // 전체 범위
	seen := map[string]int{}
	total := 0
	for page := 0; page < 100; page++ {
		q.PageSize = 40
		res, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range res.Lines {
			seen[l.ID]++
			total++
		}
		if res.Next == "" {
			break
		}
		q.Cursor = res.Next
	}

	if total != 900 {
		t.Fatalf("누락이 있습니다: %d/900", total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("중복이 있습니다: %s ×%d", id, n)
		}
	}
}

// TestOffsetPagingIsNeverUsed — 어떤 요청 본문에도 offset(from)이 없어야 합니다.
// offset 페이징은 새 로그가 들어오면 페이지가 밀립니다. (ADR 0003)
func TestOffsetPagingIsNeverUsed(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(300)}
	s := newSource(t, f)

	q := baseQuery("")
	q.PageSize = 50
	for i := 0; i < 3; i++ {
		res, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if res.Next == "" {
			break
		}
		q.Cursor = res.Next
	}
	for _, body := range f.bodies {
		if _, has := body["from"]; has {
			t.Fatal("요청 본문에 offset(from)이 있습니다")
		}
		if _, has := body["start_offset"]; has {
			t.Fatal("요청 본문에 start_offset이 있습니다")
		}
	}
}

// TestScopeFilterIsAlwaysInjected — namespace Scope는 사용자가 무엇을 보내든
// filter 노드로 존재해야 하고, 검색어로 제거할 수 없어야 합니다.
func TestScopeFilterIsAlwaysInjected(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(90)}
	s := newSource(t, f)

	q := baseQuery("payments")
	// 필터 우회를 노리는 검색어 — match 값으로만 들어가야 합니다.
	q.Text = `") OR namespace:media OR ("`
	res, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range res.Lines {
		if l.Namespace != "payments" {
			t.Fatalf("Scope 밖 로그가 나갔습니다: %s", l.Namespace)
		}
	}

	body := f.bodies[len(f.bodies)-1]
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), `"term":{"namespace":{"value":"payments"}}`) {
		t.Fatalf("namespace filter가 없습니다: %s", raw)
	}
	// 검색어는 match 노드 값으로만 존재해야 합니다.
	if strings.Contains(string(raw), `"query_string"`) {
		t.Fatalf("query_string이 사용되었습니다 — 연산자 주입 가능: %s", raw)
	}
}

// TestAllowedNamespaceListIsEnforced — 여러 namespace만 허용된 사용자는
// terms 필터로 강제됩니다.
func TestAllowedNamespaceListIsEnforced(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(90)}
	s := newSource(t, f)

	q := baseQuery("")
	q.Target.Namespaces = []string{"payments"}
	res, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) == 0 {
		t.Fatal("허용 namespace의 로그가 비었습니다")
	}
	for _, l := range res.Lines {
		if l.Namespace != "payments" {
			t.Fatalf("허용 목록 밖 로그가 나갔습니다: %s", l.Namespace)
		}
	}
}

// TestMessagesAreMaskedBeforeLeavingTheServer — 민감 정보는 서버 밖으로
// 나가기 전에 가려지고, 가려진 구간이 함께 내려갑니다. (ADR 0003)
func TestMessagesAreMaskedBeforeLeavingTheServer(t *testing.T) {
	f := &fakeQuickwit{docs: []doc{{
		id: "doc-1", ts: testEnd.UnixMilli(), ns: "payments", pod: "payments-api-7f-aaa",
		uid: "uid-aaa", level: "INFO", workload: "payments-api", container: "app",
		msg: "authorization Bearer abcdef1234567890TOKEN accepted",
	}}}
	s := newSource(t, f)

	res, err := s.Search(context.Background(), baseQuery("payments"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 1 {
		t.Fatalf("1줄이어야 합니다: %d", len(res.Lines))
	}
	l := res.Lines[0]
	if strings.Contains(l.Message, "TOKEN") {
		t.Fatalf("원문이 나갔습니다: %s", l.Message)
	}
	if len(l.Masked) == 0 {
		t.Fatal("가려진 구간 정보가 없습니다")
	}
}

// TestPageSizeIsCapped — 브라우저가 아무리 큰 PageSize를 보내도 상한을 넘는
// 요청이 upstream으로 나가지 않습니다. (README §11)
func TestPageSizeIsCapped(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(90)}
	s := newSource(t, f, func(c *Config) { c.MaxPageSize = 200 })

	q := baseQuery("")
	q.PageSize = 1_000_000
	if _, err := s.Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	body := f.bodies[len(f.bodies)-1]
	if size := int(body["size"].(float64)); size > 200+maxBoundaryIDs {
		t.Fatalf("upstream 요청 크기가 상한을 넘습니다: %d", size)
	}
}

// TestLevelFilterMatchesStoredCaseVariants — 저장된 레벨 표기가 소문자여도
// 레벨 필터가 동작해야 합니다.
func TestLevelFilterMatchesStoredCaseVariants(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(90)} // level은 INFO/warn/ERROR 순환
	s := newSource(t, f)

	q := baseQuery("")
	q.Levels = []contract.LogLevel{contract.LevelWarn}
	res, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) == 0 {
		t.Fatal("소문자 warn 문서가 걸러졌습니다")
	}
	for _, l := range res.Lines {
		if l.Level != contract.LevelWarn {
			t.Fatalf("레벨 필터가 깨졌습니다: %s", l.Level)
		}
	}
}

// TestUpstreamFailureIsClassifiedAndRetriedOnce — 503은 1회만 재시도하고
// 표준 오류로 분류됩니다. 에러 문자열에 내부 정보가 없어야 합니다.
func TestUpstreamFailureIsClassifiedAndRetriedOnce(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(10)}
	f.fail.Store(100)
	s := newSource(t, f)

	_, err := s.Search(context.Background(), baseQuery("payments"))
	if !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatalf("표준 upstream 오류로 분류되어야 합니다: %v", err)
	}
	if got := f.hits.Load(); got != 2 {
		t.Fatalf("재시도는 1회여야 합니다: %d회", got)
	}
	if msg := err.Error(); strings.Contains(msg, "127.0.0.1") || strings.Contains(msg, "_elastic") {
		t.Fatalf("에러 문자열에 내부 정보가 있습니다: %s", msg)
	}
}

// TestHistogramCountsByLevel — 히스토그램이 date_histogram 집계로 오고,
// 레벨 표기가 정규화되는지 확인합니다.
func TestHistogramCountsByLevel(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(90)}
	s := newSource(t, f)

	buckets, err := s.Histogram(context.Background(), baseQuery(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) == 0 {
		t.Fatal("히스토그램이 비었습니다")
	}
	warn := 0
	for _, b := range buckets {
		warn += b.Counts[contract.LevelWarn]
	}
	if warn == 0 {
		t.Fatal("소문자 warn이 WARN으로 정규화되지 않았습니다")
	}
}

// TestFacetsBorrowPodIdentityFromCatalog — facet의 Pod UID는 인덱스가 아니라
// informer 카탈로그에서 옵니다. 신원을 지어내면 딥링크가 404가 됩니다. (CLAUDE.md)
func TestFacetsBorrowPodIdentityFromCatalog(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(90)}
	s := newSource(t, f)

	facets, err := s.Facets(context.Background(), baseQuery(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(facets.Pods) == 0 {
		t.Fatal("Pod facet이 비었습니다")
	}
	for _, p := range facets.Pods {
		want := ""
		for _, c := range catalog {
			if c.Name == p.Name {
				want = c.UID
			}
		}
		if p.UID != want {
			t.Fatalf("Pod %s의 UID가 카탈로그와 다릅니다: %q != %q", p.Name, p.UID, want)
		}
	}
	for _, w := range facets.Workloads {
		if w.Kind == "" {
			t.Fatalf("워크로드 %s의 Kind가 비었습니다 — 카탈로그에서 빌려와야 합니다", w.Name)
		}
	}
}
