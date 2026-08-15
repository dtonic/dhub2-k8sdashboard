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
	kind      string
	eventID   string
	container string
	cluster   string
	malformed bool
}

// fakeQuickwit은 ES 호환 검색을 최소한으로 구현합니다 — range·term·terms 필터,
// match(AND), timestamp desc 정렬, size. 정렬 동률은 id로 고정해 결정적입니다.
type fakeQuickwit struct {
	docs       []doc
	mu         sync.Mutex
	bodies     []map[string]any
	scrolls    map[string]fakeScroll
	nextScroll int
	fail       atomic.Int32
	hits       atomic.Int32
	scanSizes  []int
}

type fakeScroll struct {
	docs   []doc
	size   int
	offset int
}

func (f *fakeQuickwit) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.fail.Load() > 0 {
			f.fail.Add(-1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/api/v1/_elastic/_search/scroll" {
			var request struct {
				ScrollID string `json:"scroll_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "bad scroll", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			state, found := f.scrolls[request.ScrollID]
			delete(f.scrolls, request.ScrollID)
			f.mu.Unlock()
			if !found {
				http.Error(w, "expired", http.StatusInternalServerError)
				return
			}
			f.writeScrollPage(w, state)
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
		res := map[string]any{"hits": map[string]any{"total": map[string]any{"value": len(matched)}, "hits": hitsJSON(page)}}
		if r.URL.Query().Get("scroll") != "" {
			f.mu.Lock()
			f.scanSizes = append(f.scanSizes, len(page))
			if f.scrolls == nil {
				f.scrolls = map[string]fakeScroll{}
			}
			f.nextScroll++
			id := fmt.Sprintf("scroll-%d", f.nextScroll)
			f.scrolls[id] = fakeScroll{docs: matched, size: size, offset: len(page)}
			f.mu.Unlock()
			res["_scroll_id"] = id
		}
		if aggs, ok := body["aggs"].(map[string]any); ok {
			res["aggregations"] = f.aggregate(matched, aggs)
		}
		json.NewEncoder(w).Encode(res)
	})
}

func (f *fakeQuickwit) writeScrollPage(w http.ResponseWriter, state fakeScroll) {
	end := state.offset + state.size
	if end > len(state.docs) {
		end = len(state.docs)
	}
	page := state.docs[state.offset:end]
	f.mu.Lock()
	f.scanSizes = append(f.scanSizes, len(page))
	f.nextScroll++
	id := fmt.Sprintf("scroll-%d", f.nextScroll)
	if end < len(state.docs) {
		f.scrolls[id] = fakeScroll{docs: state.docs, size: state.size, offset: end}
	}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"_scroll_id": id,
		"hits":       map[string]any{"total": map[string]any{"value": len(state.docs)}, "hits": hitsJSON(page)},
	})
}

func hitsJSON(page []doc) []any {
	out := make([]any, 0, len(page))
	for _, d := range page {
		source := map[string]any{
			"namespace": d.ns, "pod_name": d.pod, "pod_uid": d.uid,
			"level": d.level, "message": d.msg, "workload_name": d.workload, "container": d.container,
			"workload_kind": d.kind,
			"event_id":      d.eventID,
		}
		if !d.malformed {
			source["timestamp"] = d.ts
		}
		out = append(out, map[string]any{
			"_id":     d.id,
			"_source": source,
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
			case "resource_attributes.k8s.cluster.name":
				return d.cluster
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
			case "workload_kind":
				return d.kind
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
			cluster: "seoul",
			level:   []string{"INFO", "warn", "ERROR"}[i%3],
			msg:     fmt.Sprintf("request %d handled", i), workload: wl,
			kind: map[bool]string{true: "Deployment", false: "StatefulSet"}[ns == "payments"], container: "app",
		})
	}
	return docs
}

type fakeCatalog []datasource.CatalogPod

func (f fakeCatalog) CatalogPods(_ string, namespace string, limit int) []datasource.CatalogPod {
	out := []datasource.CatalogPod{}
	for _, p := range f {
		if namespace == "" || p.Namespace == namespace {
			out = append(out, p)
		}
	}
	return out
}

type clusteredCatalog map[string][]datasource.CatalogPod

func (c clusteredCatalog) CatalogPods(clusterID, namespace string, limit int) []datasource.CatalogPod {
	var out []datasource.CatalogPod
	for _, pod := range c[clusterID] {
		if namespace == "" || pod.Namespace == namespace {
			out = append(out, pod)
		}
		if limit > 0 && len(out) == limit {
			break
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

// TestCursorPagingBeyond512TimestampCollisions는 한 timestamp의 경계 상태가
// 과거 512개 제한을 넘어도 모든 문서를 정확히 한 번 반환하는지 검증합니다.
func TestCursorPagingBeyond512TimestampCollisions(t *testing.T) {
	docs := makeDocs(700)
	for i := range docs {
		docs[i].ts = testEnd.UnixMilli()
	}
	f := &fakeQuickwit{docs: docs}
	s := newSource(t, f, func(c *Config) { c.MaxLines = 700 })

	q := baseQuery("")
	q.PageSize = 100
	seen := map[string]bool{}
	maxCursorBytes, maxUpstreamSize, pages := 0, 0, 0
	for {
		res, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, line := range res.Lines {
			if seen[line.ID] {
				t.Fatalf("중복: %s", line.ID)
			}
			seen[line.ID] = true
		}
		if len(res.Next) > maxCursorBytes {
			maxCursorBytes = len(res.Next)
		}
		body := f.bodies[len(f.bodies)-1]
		if size := int(body["size"].(float64)); size > maxUpstreamSize {
			maxUpstreamSize = size
		}
		if res.Next == "" {
			break
		}
		q.Cursor = res.Next
	}
	if len(seen) != 700 {
		t.Fatalf("누락: %d/700", len(seen))
	}
	if maxUpstreamSize > 700 {
		t.Fatalf("upstream size가 MaxLines를 넘었습니다: %d", maxUpstreamSize)
	}
	t.Logf("700 collision: pages=%d requests=%d max_cursor_bytes=%d max_upstream_size=%d", pages, f.hits.Load(), maxCursorBytes, maxUpstreamSize)
}

func TestMaxLinesStopsTraversalWithoutOverflow(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(800)}
	s := newSource(t, f, func(c *Config) { c.MaxLines = 550 })
	q := baseQuery("")
	q.PageSize = 100

	total := 0
	for {
		res, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		total += len(res.Lines)
		if res.Next == "" {
			if !res.Truncated {
				t.Fatal("cap 뒤 데이터가 있는데 truncated=false입니다")
			}
			break
		}
		q.Cursor = res.Next
	}
	if total != 500 {
		t.Fatalf("bounded scan 불일치: got %d want 500", total)
	}
	if got := int(f.bodies[0]["size"].(float64)); got != 100 {
		t.Fatalf("initial scroll size = %d, want 100", got)
	}
	scanned := 0
	for _, size := range f.scanSizes {
		if size > min(100, 550-scanned) {
			t.Fatalf("scan request exceeded remaining budget: size=%d scanned=%d", size, scanned)
		}
		scanned += size
	}
	if scanned != 500 {
		t.Fatalf("scanned = %d, want 500", scanned)
	}
	for _, body := range f.bodies {
		if size := int(body["size"].(float64)); size > 550 {
			t.Fatalf("upstream size가 MaxLines를 넘었습니다: %d", size)
		}
	}
}

func TestPrimeMaxLinesKeepsUpstreamCallsBounded(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(6000)}
	s := newSource(t, f, func(c *Config) { c.MaxLines = 5003 })
	q := baseQuery("")
	q.PageSize = 100

	returned := 0
	for {
		page, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		returned += len(page.Lines)
		if page.Next == "" {
			if !page.Truncated {
				t.Fatal("underfilled scan cap must be truncated")
			}
			break
		}
		q.Cursor = page.Next
	}
	scanned := 0
	for _, size := range f.scanSizes {
		scanned += size
	}
	if returned != 5000 || scanned != 5000 || f.hits.Load() != 50 {
		t.Fatalf("prime cap traversal: returned=%d scanned=%d calls=%d, want 5000/5000/50", returned, scanned, f.hits.Load())
	}
}

func TestMaxLinesCapsMalformedAndValidHitScanning(t *testing.T) {
	docs := make([]doc, 9)
	for i := range docs {
		docs[i] = doc{
			id: fmt.Sprintf("doc-%d", i), ts: testEnd.UnixMilli(), ns: "payments",
			pod: "payments-api-7f-aaa", uid: "uid-aaa", level: "INFO", msg: fmt.Sprintf("within-cap-%d", i),
		}
	}
	docs[0].malformed = true
	docs[2].malformed = true
	for i := 4; i < len(docs); i++ {
		docs[i].msg = fmt.Sprintf("after-cap-%d", i)
	}

	f := &fakeQuickwit{docs: docs}
	s := newSource(t, f, func(c *Config) { c.MaxLines = 6 })
	q := baseQuery("")
	q.PageSize = 4

	var lines []contract.LogLine
	for {
		page, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, page.Lines...)
		if page.Next == "" {
			if !page.Truncated {
				t.Fatal("scan cap reached with more hits but truncated=false")
			}
			break
		}
		q.Cursor = page.Next
	}
	if len(lines) != 2 {
		t.Fatalf("valid lines within scan cap = %d, want 2", len(lines))
	}
	for _, line := range lines {
		if strings.Contains(line.Message, "after-cap") {
			t.Fatalf("returned a valid line beyond scan cap: %q", line.Message)
		}
	}
	if got := fmt.Sprint(f.scanSizes); got != "[4]" {
		t.Fatalf("scan sizes = %s, want [4]", got)
	}
	if got := f.hits.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
}

func TestOversizedCursorIsRejectedBeforeUpstream(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(10)}
	s := newSource(t, f, func(c *Config) { c.MaxLines = 100 })
	q := baseQuery("")
	q.Cursor = strings.Repeat("A", maxEncodedCursorBytes+1)
	if _, err := s.Search(context.Background(), q); err == nil {
		t.Fatal("oversized cursor가 거절되지 않았습니다")
	}
	if got := f.hits.Load(); got != 0 {
		t.Fatalf("cursor 검증 전에 upstream이 호출됐습니다: %d회", got)
	}
}

func TestInvalidUpstreamScrollIDIsUnavailable(t *testing.T) {
	for name, scrollID := range map[string]string{
		"missing":   "",
		"oversized": strings.Repeat("s", maxScrollIDBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"_scroll_id": scrollID,
					"hits": map[string]any{
						"total": map[string]any{"value": 2},
						"hits":  hitsJSON([]doc{{ts: testEnd.UnixMilli(), ns: "payments", msg: "one"}}),
					},
				})
			}))
			t.Cleanup(srv.Close)
			s, err := New(Config{BaseURL: srv.URL}, catalog)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Search(context.Background(), baseQuery("payments"))
			if !errors.Is(err, datasource.ErrUnavailable) {
				t.Fatalf("invalid scroll id는 ErrUnavailable이어야 합니다: %v", err)
			}
		})
	}
}

func TestFinalPageDoesNotRequireScrollID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hits": map[string]any{
				"total": map[string]any{"value": 1},
				"hits":  hitsJSON([]doc{{ts: testEnd.UnixMilli(), ns: "payments", msg: "only"}}),
			},
		})
	}))
	t.Cleanup(srv.Close)
	s, err := New(Config{BaseURL: srv.URL}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(context.Background(), baseQuery("payments"))
	if err != nil || len(page.Lines) != 1 || page.Next != "" {
		t.Fatalf("final page: err=%v page=%+v", err, page)
	}
}

func TestCursorIsBoundToSourceAndCurrentScope(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(300)}
	s := newSource(t, f)
	q := baseQuery("")
	q.PageSize = 50
	first, err := s.Search(context.Background(), q)
	if err != nil || first.Next == "" {
		t.Fatalf("첫 scroll cursor: %v %+v", err, first)
	}
	before := f.hits.Load()

	q.Cursor = first.Next
	q.Target.Namespace = "payments"
	if _, err := s.Search(context.Background(), q); err == nil {
		t.Fatal("다른 Scope에서 cursor가 재사용됐습니다")
	}
	if f.hits.Load() != before {
		t.Fatal("Scope mismatch가 upstream 요청 전에 거절되지 않았습니다")
	}
	q.Target.Namespace = ""
	q.PageSize = 25
	if _, err := s.Search(context.Background(), q); err == nil {
		t.Fatal("다른 page size로 scroll page 일부를 버릴 수 있습니다")
	}

	other := newSource(t, f)
	q.PageSize = 50
	if _, err := other.Search(context.Background(), q); err == nil {
		t.Fatal("다른 Source HMAC key에서 cursor가 재사용됐습니다")
	}
}

func TestTraversalIDsAreUniqueForIdenticalDocuments(t *testing.T) {
	docs := make([]doc, 700)
	for i := range docs {
		docs[i] = doc{ts: testEnd.UnixMilli(), ns: "payments", pod: "same", uid: "same",
			level: "INFO", msg: "identical", workload: "same", kind: "Deployment", container: "app"}
	}
	f := &fakeQuickwit{docs: docs}
	s := newSource(t, f, func(c *Config) { c.MaxLines = 700 })
	q := baseQuery("payments")
	q.PageSize = 100
	seen := map[string]bool{}
	for {
		page, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range page.Lines {
			if seen[line.ID] {
				t.Fatalf("동일 문서 traversal ID 충돌: %s", line.ID)
			}
			seen[line.ID] = true
		}
		if page.Next == "" {
			break
		}
		q.Cursor = page.Next
	}
	if len(seen) != 700 {
		t.Fatalf("고유 ID %d/700", len(seen))
	}
}

func TestStoredEventIDIsPreferred(t *testing.T) {
	f := &fakeQuickwit{docs: []doc{{id: "", eventID: "event-42", ts: testEnd.UnixMilli(), ns: "payments", msg: "m"}}}
	s := newSource(t, f)
	page, err := s.Search(context.Background(), baseQuery("payments"))
	if err != nil || len(page.Lines) != 1 || page.Lines[0].ID != "event-42" {
		t.Fatalf("stored event_id가 사용되지 않았습니다: %v %+v", err, page.Lines)
	}
}

func BenchmarkTraversalIDs500(b *testing.B) {
	prefix := traversalIDPrefix(make([]byte, 32), "fixed-nonce")
	ids := make([]string, 500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for ordinal := range ids {
			ids[ordinal] = traversalID(prefix, ordinal)
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
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), `"format":"epoch_millis"`) {
			t.Fatal("Quickwit 0.8 range가 지원하지 않는 format 필드가 있습니다")
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

func TestCentralClusterFilterIsForcedForSearchHistogramFacetsAndCursor(t *testing.T) {
	f := &fakeQuickwit{docs: []doc{
		{id: "a-1", ts: testEnd.UnixMilli(), cluster: "a", ns: "same", pod: "same", uid: "same", level: "ERROR", msg: "from-a-1", workload: "api", kind: "Deployment", container: "app"},
		{id: "a-2", ts: testEnd.UnixMilli() - 1, cluster: "a", ns: "same", pod: "same", uid: "same", level: "INFO", msg: "from-a-2", workload: "api", kind: "Deployment", container: "app"},
		{id: "b-1", ts: testEnd.UnixMilli(), cluster: "b", ns: "same", pod: "same", uid: "same", level: "ERROR", msg: "from-b-1", workload: "api", kind: "Deployment", container: "app"},
		{id: "b-2", ts: testEnd.UnixMilli() - 1, cluster: "b", ns: "same", pod: "same", uid: "same", level: "INFO", msg: "from-b-2", workload: "api", kind: "Deployment", container: "app"},
	}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	source, err := New(Config{BaseURL: srv.URL, Index: "logs", ClusterScoped: true, MaxPageSize: 10, MaxLines: 10, Fields: FieldMap{Cluster: "resource_attributes.k8s.cluster.name"}}, clusteredCatalog{
		"a": {{Namespace: "same", Name: "same", UID: "uid-a"}},
		"b": {{Namespace: "same", Name: "same", UID: "uid-b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	q := datasource.LogQuery{Target: datasource.Target{ClusterID: "a", Namespace: "same"}, Window: datasource.Window{From: testEnd.Add(-time.Hour), To: testEnd, Step: time.Minute}, PageSize: 1}
	page, err := source.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 1 || !strings.Contains(page.Lines[0].Message, "from-a") || page.Next == "" {
		t.Fatalf("search leakage/page=%+v", page)
	}
	q.Cursor = page.Next
	q.Target.ClusterID = "b"
	if _, err = source.Search(context.Background(), q); err == nil {
		t.Fatal("A cursor reused for B")
	}
	q.Cursor, q.Target.ClusterID = "", "a"
	histogram, err := source.Histogram(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, bucket := range histogram {
		for _, value := range bucket.Counts {
			count += value
		}
	}
	if count != 2 {
		t.Fatalf("histogram cross-cluster count=%d", count)
	}
	facets, err := source.Facets(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(facets.Pods) != 1 || facets.Pods[0].Count != 2 || facets.Pods[0].UID != "uid-a" {
		t.Fatalf("facets leakage=%+v", facets)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, body := range f.bodies {
		raw, _ := json.Marshal(body)
		if !strings.Contains(string(raw), `"term":{"resource_attributes.k8s.cluster.name":{"value":"a"}}`) {
			t.Fatalf("cluster filter missing: %s", raw)
		}
	}
}

func TestCentralQuickwitClusterMappingFailsClosed(t *testing.T) {
	for _, field := range []string{"", "cluster", "resource_attributes.other"} {
		if _, err := New(Config{BaseURL: "http://quickwit", ClusterScoped: true, Fields: FieldMap{Cluster: field}}, catalog); err == nil {
			t.Fatalf("mapping %q accepted", field)
		}
	}
}

// TestIdentityScopeFiltersAreAlwaysInjected는 BFF가 확정한 workload와 Pod UID가
// 모두 bool.filter로 강제되는지 확인합니다. ClusterID는 단일-cluster MVP의 BFF
// 경계이며 Quickwit 인덱스에 존재하지 않는 cluster_id를 만들지 않습니다(ADR 0005).
func TestIdentityScopeFiltersAreAlwaysInjected(t *testing.T) {
	f := &fakeQuickwit{docs: makeDocs(90)}
	s := newSource(t, f)
	q := baseQuery("payments")
	q.Target.WorkloadKind = "Deployment"
	q.Target.WorkloadName = "payments-api"
	q.Target.PodUID = "uid-aaa"

	res, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) == 0 {
		t.Fatal("identity scope 결과가 비었습니다")
	}
	for _, line := range res.Lines {
		if line.Namespace != "payments" || line.WorkloadKind != "Deployment" ||
			line.WorkloadName != "payments-api" || line.PodUID != "uid-aaa" {
			t.Fatalf("identity scope 밖 로그가 나갔습니다: %+v", line)
		}
	}
	raw, _ := json.Marshal(f.bodies[len(f.bodies)-1])
	for _, want := range []string{
		`"term":{"namespace":{"value":"payments"}}`,
		`"term":{"workload_kind":{"value":"Deployment"}}`,
		`"term":{"workload_name":{"value":"payments-api"}}`,
		`"term":{"pod_uid":{"value":"uid-aaa"}}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("scope filter %s가 없습니다: %s", want, raw)
		}
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

func TestLineAtResolvesOTLPNestedFieldsWithExactKeyPrecedence(t *testing.T) {
	s := &Source{cfg: Config{Fields: FieldMap{
		Timestamp: "timestamp_nanos", Level: "severity_text", Message: "body.message",
		Namespace: "resource_attributes.k8s.namespace.name", PodName: "resource_attributes.k8s.pod.name",
		PodUID: "resource_attributes.k8s.pod.uid", Container: "resource_attributes.k8s.container.name",
		WorkloadKind: "resource_attributes.k8s.workload.kind", WorkloadName: "resource_attributes.k8s.workload.name",
		Node: "resource_attributes.k8s.node.name", EventID: "attributes.event_id",
	}}.withDefaults()}
	hit := esHit{Source: map[string]any{
		"timestamp_nanos": float64(1_720_000_000_000_000_000), "severity_text": "WARN",
		"body": map[string]any{"message": "nested body"},
		"resource_attributes": map[string]any{
			"k8s.namespace.name": "payments", "k8s.pod.name": "api-0", "k8s.pod.uid": "pod-1",
			"k8s.container.name": "api", "k8s.workload.kind": "StatefulSet",
			"k8s.workload.name": "api", "k8s.node.name": "node-a",
		},
		"attributes":   map[string]any{"event_id": "event-1"},
		"body.message": "legacy exact key",
	}}
	line, ok := s.lineAt(hit, "unused", 0)
	if !ok || line.Message != "legacy exact key" || line.Namespace != "payments" || line.PodUID != "pod-1" || line.ID != "event-1" {
		t.Fatalf("OTLP nested field mapping failed: ok=%v line=%+v", ok, line)
	}
}

func BenchmarkFieldResolution(b *testing.B) {
	legacy := map[string]any{"namespace": "payments"}
	nested := map[string]any{"resource_attributes": map[string]any{"k8s.namespace.name": "payments"}}
	worst := map[string]any{}
	current := worst
	segments := make([]string, maxFieldPathSegments)
	for i := range segments {
		segments[i] = fmt.Sprintf("s%d", i)
		if i == len(segments)-1 {
			current[segments[i]] = "value"
			continue
		}
		next := map[string]any{}
		current[segments[i]] = next
		current = next
	}
	b.Run("legacy-exact", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if field(legacy, "namespace") == nil {
				b.Fatal("missing")
			}
		}
	})
	b.Run("otel-nested", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if field(nested, "resource_attributes.k8s.namespace.name") == nil {
				b.Fatal("missing")
			}
		}
	})
	b.Run("otel-worst-bounded", func(b *testing.B) {
		path := strings.Join(segments, ".")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if field(worst, path) == nil {
				b.Fatal("missing")
			}
		}
	})
}

func TestFieldPathBoundsAndLongInputDoNotRecurse(t *testing.T) {
	long := strings.Repeat("segment.", maxFieldPathSegments+1) + "leaf"
	if field(map[string]any{}, long) != nil {
		t.Fatal("unexpected long path match")
	}
	_, err := New(Config{BaseURL: "http://quickwit.invalid", Fields: FieldMap{Message: long}}, fakeCatalog{})
	if err == nil {
		t.Fatal("unbounded field path was accepted")
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
	if size := int(body["size"].(float64)); size > 200 {
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

func TestTimeoutIsClassifiedAndRetriedOnce(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{}, 2)
	canceled := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		started <- struct{}{}
		w.WriteHeader(http.StatusServiceUnavailable)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		canceled <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	s, err := New(Config{BaseURL: srv.URL, Timeout: time.Second}, catalog)
	if err != nil {
		t.Fatal(err)
	}

	begin := time.Now()
	_, err = s.Search(context.Background(), baseQuery("payments"))
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("logical timeout exceeded broad bound: %v", elapsed)
	}
	if !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatalf("timeout은 ErrUnavailable이어야 합니다: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("attempt %d did not start", i+1)
		}
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatalf("attempt %d context was not canceled", i+1)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("timeout 재시도는 1회여야 합니다: %d회 호출", got)
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
