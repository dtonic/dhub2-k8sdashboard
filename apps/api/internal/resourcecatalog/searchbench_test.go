package resourcecatalog

// 증분 경로 벤치마크 (Round 6 v4.1 §7)
// --------------------------------------------------------------------------
// 보고 항목은 ns/op·B/op·allocs/op에 더해 **정규화된 카운터**입니다.
//
//	directory_copies/op   손댄 posting 리프 entry 수 / 합쳐진 키 수
//	visited_rows/op       해석한 행 레코드 수 / 합쳐진 키 수
//	postings_changed/op   바뀐 posting 수 / 합쳐진 키 수
//	store_list/op         0이어야 합니다 (정상 델타 flush)
//	full_build/op         0이어야 합니다 (정상 델타 flush)
//
// 배치 크기가 달라져도 같은 임계값으로 볼 수 있도록 **합쳐진 키 수로 정규화**합니다.

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

// newDeltaBenchHarness는 벤치용 서비스 한 벌입니다(테스트 하네스와 같은 모양).
func newDeltaBenchHarness(b *testing.B, rows []*metav1.PartialObjectMetadata) *deltaHarness {
	b.Helper()
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "services"}
	s := &Service{
		cfg: Config{
			ClusterID: "prod-seoul", SearchEnabled: true, SearchIncremental: true,
			MaxSearchIndexBytes: DefaultMaxSearchIndexBytes,
			Now:                 func() time.Time { return indexBase },
		},
		order:   []schema.GroupVersionResource{gvr},
		entries: map[schema.GroupVersionResource]*resourceEntry{gvr: {gvr: gvr}},
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	s.started.Store(true)
	s.disc.Store(&discoverySnapshot{
		entries: []discoveryEntry{{
			gvr: gvr, kind: "Service", namespaced: true, served: true,
			verbs: []string{"get", "list", "watch"},
		}},
		byGVR: map[schema.GroupVersionResource]int{gvr: 0},
	})
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, r := range rows {
		if err := store.Add(r); err != nil {
			b.Fatal(err)
		}
	}
	e := s.entries[gvr]
	e.setStatus(StateReady, "")
	e.lifecycle, e.generation = 1, 0
	e.tokenPacked.Store(packToken(1, 0))
	e.informer = &fakeInformer{store: store, synced: true}
	e.bootstrapped.Store(true)

	index := indexOf(rows...)
	r := buildSearchIndex(index, "Service", true, hugeBudget, hugeBudget)
	if r.state != SearchReady {
		b.Fatalf("부트스트랩 실패: %s", r.state)
	}
	snap := &entrySnapshot{
		index: index, sindex: r.index, searchState: SearchReady,
		indexVer: 1, searchVer: 1,
	}
	e.setSnap(snap)
	s.snapMu.Lock()
	s.publishLeaseLocked(e, snap)
	s.snapMu.Unlock()
	return &deltaHarness{
		svc: s, entry: e, store: store, gvr: gvr,
		binding: &handlerBinding{entry: e, packed: packToken(1, 0)},
	}
}

// benchRows는 분포를 지정해 100k 목록 스냅숏을 만듭니다.
//
//	namespaces == 0  이면 cluster-scoped(namespace="") 한 파티션입니다.
func benchRows(total, namespaces, labelPairs int) []*metav1.PartialObjectMetadata {
	rows := make([]*metav1.PartialObjectMetadata, 0, total)
	for i := 0; i < total; i++ {
		ns := ""
		if namespaces > 0 {
			ns = fmt.Sprintf("ns-%04d", i%namespaces)
		}
		labels := make(map[string]string, labelPairs)
		for k := 0; k < labelPairs; k++ {
			labels[fmt.Sprintf("lk%02d", k)] = fmt.Sprintf("lv%02d-%06d", k, i)
		}
		rows = append(rows, metaRow(ns, fmt.Sprintf("obj-%06d", i), fmt.Sprintf("uid-%06d-abcdef0123456789abcdef01", i), labels))
	}
	return rows
}

// benchIndex는 벤치 기준이 되는 증분 인덱스입니다.
func benchIndex(b *testing.B, rows []*metav1.PartialObjectMetadata) *searchIndex {
	b.Helper()
	r := buildSearchIndex(indexOf(rows...), "Service", true, hugeBudget, hugeBudget)
	if r.state != SearchReady || r.index == nil {
		b.Fatalf("인덱스를 세우지 못했습니다: %s %s", r.state, r.reason)
	}
	return r.index
}

// benchOps는 batch개 키를 갱신하는 연산 묶음입니다.
func benchOps(rows []*metav1.PartialObjectMetadata, batch, round int) map[string][]partOp {
	byNS := make(map[string][]partOp, 8)
	for i := 0; i < batch; i++ {
		row := rows[(round*batch+i*7)%len(rows)]
		labels := make([]string, 0, len(row.Labels)*2)
		for k, v := range row.Labels {
			labels = append(labels, normalizeToken(k), normalizeToken(v+"-x"))
		}
		byNS[row.Namespace] = append(byNS[row.Namespace], partOp{
			name: row.Name,
			input: &rowInput{
				name: row.Name, uid: string(row.UID), labels: labels,
			},
		})
	}
	return byNS
}

func runDeltaBench(b *testing.B, rows []*metav1.PartialObjectMetadata, batch int) {
	idx := benchIndex(b, rows)
	b.ReportAllocs()
	b.ResetTimer()

	var dirCopies, visited, changed, keys int64
	for i := 0; i < b.N; i++ {
		ops := benchOps(rows, batch, i)
		var st applyStats
		for _, list := range ops {
			keys += int64(len(list))
		}
		_ = idx.applyOps(indexBase, ops, &st)
		dirCopies += st.directoryCopies
		visited += st.visitedRows
		changed += st.postingsChanged
	}
	b.StopTimer()
	if keys == 0 {
		keys = 1
	}
	b.ReportMetric(float64(dirCopies)/float64(keys), "dircopies/key")
	b.ReportMetric(float64(visited)/float64(keys), "visited/key")
	b.ReportMetric(float64(changed)/float64(keys), "postings/key")
	// store_list/full_build는 여기서 상수 0으로 적지 않습니다 — 이 벤치는 인덱스
	// 수준이라 서비스 카운터를 지나지 않기 때문입니다. 그 값은
	// BenchmarkServiceFlushCounters가 **실제 카운터**로 잽니다.
}

// BenchmarkServiceFlushCounters는 서비스 경로로 flush를 돌리며 **실제** 카운터를
// 읽습니다. 정상 델타는 Store.List·전체 빌드가 0이어야 합니다.
func BenchmarkServiceFlushCounters(b *testing.B) {
	rows := benchRows(2_000, 20, 2)
	h := newDeltaBenchHarness(b, rows)
	b.ReportAllocs()
	b.ResetTimer()

	baseList := h.svc.delta.storeListCalls.Load()
	baseBoot := h.svc.delta.fullBootstraps.Load()
	baseFull := h.svc.delta.fullRecoveries.Load()
	for i := 0; i < b.N; i++ {
		row := rows[i%len(rows)]
		h.svc.enqueueKey(h.binding, row.Namespace, row.Name)
		h.svc.flushSearchDeltas(context.Background(), h.gvr, maxBatchPerResource)
	}
	b.StopTimer()
	b.ReportMetric(float64(h.svc.delta.storeListCalls.Load()-baseList)/float64(b.N), "store_list/op")
	b.ReportMetric(float64(h.svc.delta.fullBootstraps.Load()-baseBoot)/float64(b.N), "full_build/op")
	b.ReportMetric(float64(h.svc.delta.fullRecoveries.Load()-baseFull)/float64(b.N), "full_recovery/op")
	if got := h.svc.delta.storeListCalls.Load() - baseList; got != 0 {
		b.Fatalf("정상 델타가 Store.List를 %d회 불렀습니다", got)
	}
	if got := h.svc.delta.fullBootstraps.Load() - baseBoot; got != 0 {
		b.Fatalf("정상 델타가 전체 빌드를 %d회 했습니다", got)
	}
}

/* ── 분산 분포 (500 namespace × 200행) ───────────────────────────────────── */

func BenchmarkResourceSearchDeltaBatch1Distributed(b *testing.B) {
	runDeltaBench(b, benchRows(100_000, 500, 2), 1)
}

func BenchmarkResourceSearchDeltaBatch100Distributed(b *testing.B) {
	runDeltaBench(b, benchRows(100_000, 500, 2), 100)
}

func BenchmarkResourceSearchDeltaBatch1000Distributed(b *testing.B) {
	runDeltaBench(b, benchRows(100_000, 500, 2), 1000)
}

/* ── 단일 hot namespace (1 × 100k) ──────────────────────────────────────── */

func BenchmarkResourceSearchDeltaBatch1HotSingleNS(b *testing.B) {
	runDeltaBench(b, benchRows(100_000, 1, 2), 1)
}

func BenchmarkResourceSearchDeltaBatch100HotSingleNS(b *testing.B) {
	runDeltaBench(b, benchRows(100_000, 1, 2), 100)
}

func BenchmarkResourceSearchDeltaBatch1000HotSingleNS(b *testing.B) {
	runDeltaBench(b, benchRows(100_000, 1, 2), 1000)
}

/* ── cluster-scoped hot 파티션 (namespace="") ───────────────────────────── */

func BenchmarkResourceSearchDeltaBatch1HotClusterScoped(b *testing.B) {
	runDeltaBench(b, benchRows(100_000, 0, 2), 1)
}

func BenchmarkResourceSearchDeltaBatch100HotClusterScoped(b *testing.B) {
	runDeltaBench(b, benchRows(100_000, 0, 2), 100)
}

func BenchmarkResourceSearchDeltaBatch1000HotClusterScoped(b *testing.B) {
	runDeltaBench(b, benchRows(100_000, 0, 2), 1000)
}

/* ── 포화된 hot namespace (label 상한 초과) ─────────────────────────────── */

func BenchmarkResourceSearchDeltaBatch1SaturatedHotNS(b *testing.B) {
	// 16 키/값 × 8,300행 = 2^18을 넘겨 fold가 포화 상태로 돕니다.
	runDeltaBench(b, benchRows(8_300, 1, MaxLabelKeysPerObject), 1)
}

func BenchmarkResourceSearchDeltaBatch100SaturatedHotNS(b *testing.B) {
	runDeltaBench(b, benchRows(8_300, 1, MaxLabelKeysPerObject), 100)
}

/* ── 부트스트랩 ─────────────────────────────────────────────────────────── */

func BenchmarkResourceSearchPersistentBootstrap100k(b *testing.B) {
	rows := benchRows(100_000, 500, 2)
	index := indexOf(rows...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := buildSearchIndex(index, "Service", true, hugeBudget, hugeBudget)
		if r.state != SearchReady {
			b.Fatalf("state=%s", r.state)
		}
		b.ReportMetric(float64(r.index.bytes), "retained_bytes")
	}
}

/* ── 질의 ───────────────────────────────────────────────────────────────── */

func benchQuery(b *testing.B, query string, namespaces int) {
	rows := benchRows(100_000, namespaces, 2)
	idx := benchIndex(b, rows)
	part := idx.dir.find(rows[0].Namespace)
	if part == nil {
		b.Fatal("파티션을 찾지 못했습니다")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := openPartStream(part, query, searchCursorKey{}, false, "core/v1/services")
		st := &partStream{part: part, cursor: c, query: query, namespaced: true}
		count := 0
		for st.load() && count < MaxSearchPageSize {
			count++
			if !st.next() {
				break
			}
		}
	}
}

// BenchmarkResourceSearchPersistentNarrowQuery100k는 좁은 접두사(결과 소수)입니다.
func BenchmarkResourceSearchPersistentNarrowQuery100k(b *testing.B) {
	benchQuery(b, "obj-000001", 500)
}

// BenchmarkResourceSearchPersistentWideQuery100k는 넓은 접두사(구간이 큼)입니다.
func BenchmarkResourceSearchPersistentWideQuery100k(b *testing.B) {
	benchQuery(b, "obj", 500)
}

/* ── 16,384 파티션 진단 ─────────────────────────────────────────────────── */

// BenchmarkAllDiagnostics16384Partitions는 루트 집계 비용과 **요청 전체 할당**을
// 함께 잽니다. 진단이 파티션 수에 불변인지 확인하는 것이 목적입니다.
func BenchmarkAllDiagnostics16384Partitions(b *testing.B) {
	const partitions = 16_384
	rows := make([]*metav1.PartialObjectMetadata, 0, partitions)
	for i := 0; i < partitions; i++ {
		rows = append(rows, metaRow(fmt.Sprintf("ns-%05d", i),
			fmt.Sprintf("obj-%05d", i), fmt.Sprintf("uid-%05d", i), nil))
	}
	idx := benchIndex(b, rows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		agg := idx.rootAgg()
		if agg.nsCount != partitions {
			b.Fatalf("파티션 수가 %d입니다", agg.nsCount)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(idx.bytes), "retained_bytes")
}

/* ── 포화 스케줄러 ──────────────────────────────────────────────────────── */

// BenchmarkSaturatedDeltaTick은 **64개 GVR이 전부 포화**인 상태에서 deltaTick
// 한 번의 실제 소요 시간을 잽니다.
//
// 100ms 합치기 창 계약의 근거가 되는 수치입니다 — tick 하나가 100ms를 넘기면
// 다음 창이 밀리고, 밀린 창이 다시 큐를 키웁니다. 정상 델타 경로이므로
// Store.List·전체 빌드·전체 회수는 **0이어야** 합니다.
func BenchmarkSaturatedDeltaTick(b *testing.B) {
	h := newSaturatedScheduler(b, 64)
	baseList := h.svc.delta.storeListCalls.Load()
	baseBoot := h.svc.delta.fullBootstraps.Load()
	baseFull := h.svc.delta.fullRecoveries.Load()

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		h.saturate(b) // 매 반복 앞에서 다시 채웁니다(계측 밖).
		b.StartTimer()
		h.svc.deltaTick(ctx)
	}
	b.StopTimer()

	if got := h.svc.delta.storeListCalls.Load() - baseList; got != 0 {
		b.Fatalf("델타 tick이 Store.List를 %d회 불렀습니다", got)
	}
	if got := h.svc.delta.fullBootstraps.Load() - baseBoot; got != 0 {
		b.Fatalf("델타 tick이 전체 빌드를 %d회 했습니다", got)
	}
	if got := h.svc.delta.fullRecoveries.Load() - baseFull; got != 0 {
		b.Fatalf("델타 tick이 전체 회수를 %d회 했습니다", got)
	}
	// tick 하나의 벽시계 예산(100ms) 대비 여유를 그대로 보고합니다.
	perTick := float64(b.Elapsed().Nanoseconds()) / float64(max(b.N, 1))
	b.ReportMetric(perTick/1e6, "tick_ms")
	b.ReportMetric(perTick/float64(DeltaTickInterval.Nanoseconds()), "tick/window")
}
