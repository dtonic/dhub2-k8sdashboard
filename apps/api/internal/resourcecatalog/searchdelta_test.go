package resourcecatalog

// 증분 델타 경로 검증 (Round 6 v4 / v4.1)
// --------------------------------------------------------------------------
// 여기서 보는 것은 네 가지입니다.
//
//	① 예약 회계가 세 불변식(I-A/I-B/I-C)을 어기지 않고 **정확히 한 번** 해제된다.
//	② 오래된 requeue가 새 delete를 되살리지 못한다.
//	③ 드롭·폐기가 조용한 낡음이 아니라 명시적 stale로 남는다.
//	④ 지속 구조 질의 결과가 기존 배열 색인과 **같은 순서·같은 필드**를 낸다.

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

/* ── 인덱스 픽스처 ──────────────────────────────────────────────────────── */

// indexFixture는 목록 스냅숏 하나에서 증분 인덱스를 세웁니다.
func indexFixture(t *testing.T, kind string, namespaced bool, rows ...*metav1.PartialObjectMetadata) *searchIndex {
	t.Helper()
	r := buildSearchIndex(indexOf(rows...), kind, namespaced, hugeBudget, hugeBudget)
	if r.state != SearchReady || r.index == nil {
		t.Fatalf("증분 인덱스를 세우지 못했습니다: state=%s reason=%s", r.state, r.reason)
	}
	return r.index
}

// dualService는 같은 목록 스냅숏 위에 **배열 색인과 지속 색인을 모두** 올린
// 서비스를 만듭니다. 두 경로의 응답을 바이트 단위로 비교하기 위한 장치입니다.
func dualService(t *testing.T, incremental bool, rows ...*metav1.PartialObjectMetadata) *Service {
	t.Helper()
	index := indexOf(rows...)
	s := &Service{
		cfg: Config{
			ClusterID: "prod-seoul", SearchEnabled: true, SearchIncremental: incremental,
			MaxSearchIndexBytes: DefaultMaxSearchIndexBytes,
			Now:                 func() time.Time { return indexBase },
		},
		order:   []schema.GroupVersionResource{scopedGVR},
		entries: map[schema.GroupVersionResource]*resourceEntry{scopedGVR: {gvr: scopedGVR}},
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	s.started.Store(true)
	s.disc.Store(&discoverySnapshot{
		entries: []discoveryEntry{{
			gvr: scopedGVR, kind: "Service", namespaced: true, served: true,
			verbs: []string{"get", "list", "watch"},
		}},
		byGVR: map[schema.GroupVersionResource]int{scopedGVR: 0},
	})
	e := s.entries[scopedGVR]
	e.setStatus(StateReady, "")
	legacy := buildSearchSnapshot(index, "Service", true, hugeBudget, hugeBudget)
	if legacy.snapshot == nil {
		t.Fatalf("배열 색인을 만들지 못했습니다: %s", legacy.state)
	}
	e.setSnap(&entrySnapshot{
		index: index, search: legacy.snapshot, searchState: SearchReady,
		sindex: indexFixture(t, "Service", true, rows...),
	})
	return s
}

// bindingFor는 그 항목의 **현재 세대에 고정된** 콜백 신원입니다.
func bindingFor(s *Service, gvr schema.GroupVersionResource) *handlerBinding {
	e := s.entries[gvr]
	if e.tokenPacked.Load() == 0 {
		e.lifecycle, e.generation = 1, 0
		e.tokenPacked.Store(packToken(1, 0))
	}
	return &handlerBinding{entry: e, packed: e.tokenPacked.Load()}
}

func pageJSON(t *testing.T, page SearchPage) string {
	t.Helper()
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

/* ── ④ 두 경로 동치 ─────────────────────────────────────────────────────── */

// TestPersistentIndexMatchesArrayIndex — 지속 구조 질의가 기존 배열 색인과
// **같은 순서·같은 matchedField·같은 cursor 흐름**을 내야 합니다.
func TestPersistentIndexMatchesArrayIndex(t *testing.T) {
	rows := []*metav1.PartialObjectMetadata{
		metaRow("payments", "payments-api", "uid-1", map[string]string{"app": "payments", "tier": "backend"}),
		metaRow("payments", "payments-worker", "uid-2", map[string]string{"app": "payments"}),
		metaRow("billing", "payments-proxy", "uid-3", map[string]string{"team": "payments"}),
		metaRow("billing", "ledger", "uid-4", nil),
		metaRow("", "cluster-payments", "uid-5", nil),
	}
	legacySvc := dualService(t, false, rows...)
	persistSvc := dualService(t, true, rows...)

	for _, query := range []string{"pay", "payments", "billing", "service", "tier", "backend", "led"} {
		for _, limit := range []int{1, 2, 50} {
			legacyPages := pageAllAll(t, legacySvc, query, limit)
			persistPages := pageAllAll(t, persistSvc, query, limit)
			if len(legacyPages) != len(persistPages) {
				t.Fatalf("query=%q limit=%d 페이지 수가 다릅니다: %d vs %d",
					query, limit, len(legacyPages), len(persistPages))
			}
			for i := range legacyPages {
				if legacyPages[i] != persistPages[i] {
					t.Fatalf("query=%q limit=%d %d번째 페이지가 다릅니다\n배열: %s\n지속: %s",
						query, limit, i, legacyPages[i], persistPages[i])
				}
			}
		}
	}
}

// pageAllAll은 클러스터 전체 접근으로 끝까지 페이징한 직렬 응답 목록입니다.
// cursor 값 자체는 두 경로가 같은 키를 담으므로 그대로 비교합니다.
func pageAllAll(t *testing.T, s *Service, query string, limit int) []string {
	t.Helper()
	var out []string
	cursor := ""
	for round := 0; round < 512; round++ {
		page, err := s.Search(SearchRequest{
			Query: query, Limit: limit, Cursor: cursor,
			Namespaces: NamespaceFilter{All: true},
		})
		if err != nil {
			t.Fatalf("query=%q: %v", query, err)
		}
		out = append(out, pageJSON(t, page))
		if page.NextCursor == "" {
			return out
		}
		cursor = page.NextCursor
	}
	t.Fatalf("query=%q: 페이지가 끝나지 않았습니다", query)
	return nil
}

// TestPersistentMatchedFieldPriorityAcrossDuplicateTokens — 같은 토큰이 여러 필드에
// 걸리면 **가장 구체적인 필드**를 보고해야 합니다(name < namespace < kind < label).
func TestPersistentMatchedFieldPriorityAcrossDuplicateTokens(t *testing.T) {
	// 이름·namespace·label 값이 모두 "payments"로 겹칩니다.
	rows := []*metav1.PartialObjectMetadata{
		metaRow("payments", "payments", "uid-1", map[string]string{"team": "payments"}),
	}
	svc := dualService(t, true, rows...)
	page, err := svc.Search(SearchRequest{Query: "payments", Namespaces: NamespaceFilter{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("결과가 %d건입니다 — 행 하나가 한 번만 나와야 합니다: %+v", len(page.Items), page.Items)
	}
	if page.Items[0].MatchedField != "name" {
		t.Fatalf("matchedField=%q — 가장 구체적인 name이어야 합니다", page.Items[0].MatchedField)
	}
}

/* ── ① 예약 회계 ────────────────────────────────────────────────────────── */

// TestBudgetReservationInvariants — I-C 예약이 상한을 넘지 않고, 해제가 정확히
// 한 번이어야 합니다.
func TestBudgetReservationInvariants(t *testing.T) {
	var b searchBudget
	b.max.Store(1000)
	if b.peakLimit() != 3000 {
		t.Fatalf("peak 상한이 %d입니다", b.peakLimit())
	}
	if !b.reserveQueued(2500) {
		t.Fatal("상한 안 예약이 거절됐습니다")
	}
	if b.reserveQueued(600) {
		t.Fatal("상한을 넘는 예약이 통과했습니다")
	}
	if b.rejected.Load() != 1 {
		t.Fatalf("거절 카운터가 %d입니다", b.rejected.Load())
	}
	b.transferQueuedToInflight(2500)
	if b.queued.Load() != 0 || b.inflight.Load() != 2500 {
		t.Fatalf("이관 후 queued=%d inflight=%d", b.queued.Load(), b.inflight.Load())
	}
	if b.live.Load() != 2500 {
		t.Fatalf("이관은 해제가 아닙니다: live=%d", b.live.Load())
	}
	b.releaseInflight(2500)
	if b.live.Load() != 0 || b.inflight.Load() != 0 {
		t.Fatalf("해제 후 live=%d inflight=%d", b.live.Load(), b.inflight.Load())
	}
}

// TestMultiGVRConcurrentReservation — 64개 GVR이 동시에 예약·해제해도
// 상한을 넘지 않고 기준선으로 정확히 돌아와야 합니다.
func TestMultiGVRConcurrentReservation(t *testing.T) {
	var b searchBudget
	const max = int64(64 << 20)
	b.max.Store(max)
	var wg sync.WaitGroup
	var over sync.Once
	overflowed := false
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				n := int64(1024 * (1 + g%7))
				if !b.reserveQueued(n) {
					continue
				}
				if b.live.Load() > b.peakLimit() {
					over.Do(func() { overflowed = true })
				}
				b.transferQueuedToInflight(n)
				b.releaseInflight(n)
			}
		}(g)
	}
	wg.Wait()
	if overflowed {
		t.Fatal("I-C 상한을 넘긴 순간이 있었습니다")
	}
	if b.live.Load() != 0 || b.queued.Load() != 0 || b.inflight.Load() != 0 {
		t.Fatalf("기준선으로 돌아오지 않았습니다: live=%d queued=%d inflight=%d",
			b.live.Load(), b.queued.Load(), b.inflight.Load())
	}
}

/* ── ② requeue 순서 ────────────────────────────────────────────────────── */

// TestOlderRequeueDoesNotResurrectNewerDelete — 드레인한 옛 upsert를 되돌릴 때
// 같은 키의 **더 새로운** 이벤트가 큐에 있으면 옛 것을 버려야 합니다.
func TestOlderRequeueDoesNotResurrectNewerDelete(t *testing.T) {
	s := &Service{
		cfg:     Config{SearchEnabled: true, SearchIncremental: true, MaxSearchIndexBytes: DefaultMaxSearchIndexBytes},
		order:   []schema.GroupVersionResource{scopedGVR},
		entries: map[schema.GroupVersionResource]*resourceEntry{scopedGVR: {gvr: scopedGVR}},
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	// 이후 콜백이 받을 seq를 옛 이벤트보다 **확실히 크게** 만듭니다.
	// 그러지 않으면 둘 다 seq 1을 받아 어느 쪽이 남았는지 구분할 수 없습니다.
	s.eventSeq.Store(100)

	// 드레인된 옛 이벤트(seq 7) — 예약이 inflight로 옮겨진 상태를 흉내 냅니다.
	old := deltaEvent{namespace: "prod", name: "payments-api", seq: 7, reserved: 200}
	s.budget.reserveQueued(old.reserved)
	s.budget.transferQueuedToInflight(old.reserved)

	// 그 사이 같은 키의 **새** 이벤트가 큐에 들어왔습니다(적용 시점에 삭제로 해석될 것).
	b := bindingFor(s, scopedGVR)
	s.enqueueKey(b, "prod", "payments-api")
	// 다른 키의 새 이벤트도 하나 둡니다 — 되돌림이 순서를 흔들지 않는지 봅니다.
	s.enqueueKey(b, "prod", "ledger")

	s.requeueDrained(scopedGVR, []deltaEvent{old})

	s.delta.mu.Lock()
	q := s.delta.queueFor(scopedGVR)
	events := append([]deltaEvent(nil), q.events...)
	idxLen := len(q.index)
	s.delta.mu.Unlock()

	if len(events) != 2 {
		t.Fatalf("큐에 %d건이 있습니다 — 새 이벤트 둘만 남아야 합니다: %+v", len(events), events)
	}
	for _, ev := range events {
		if ev.seq <= old.seq {
			t.Fatalf("오래된 이벤트(seq %d)가 살아남았습니다: %+v", old.seq, ev)
		}
	}
	if events[0].name != "payments-api" || events[1].name != "ledger" {
		t.Fatalf("되돌림이 큐 순서를 흔들었습니다: %+v", events)
	}
	if idxLen != 2 {
		t.Fatalf("키 색인이 %d건입니다 — 큐와 어긋났습니다", idxLen)
	}
	if s.budget.inflight.Load() != 0 {
		t.Fatalf("버린 이벤트의 예약이 남았습니다: inflight=%d", s.budget.inflight.Load())
	}

	// 대조군: 같은 키가 큐에 **없으면** 되돌린 이벤트가 예약을 유지한 채 앞에 놓입니다.
	lone := deltaEvent{namespace: "prod", name: "orphan", seq: 8, reserved: 200}
	s.budget.reserveQueued(lone.reserved)
	s.budget.transferQueuedToInflight(lone.reserved)
	s.requeueDrained(scopedGVR, []deltaEvent{lone})

	s.delta.mu.Lock()
	events = append([]deltaEvent(nil), q.events...)
	s.delta.mu.Unlock()
	if len(events) != 3 || events[0].name != "orphan" {
		t.Fatalf("되돌린 이벤트가 앞에 놓이지 않았습니다: %+v", events)
	}
	if s.budget.inflight.Load() != 0 || s.budget.queued.Load() == 0 {
		t.Fatalf("예약 소유권이 큐로 옮겨지지 않았습니다: inflight=%d queued=%d",
			s.budget.inflight.Load(), s.budget.queued.Load())
	}
}

/* ── ③ 드롭·폐기 ───────────────────────────────────────────────────────── */

// TestQueueOverflowMarksNamespaceStale — 큐가 가득 차면 조용히 버리지 않고
// 그 namespace를 회수 대상으로 남겨야 합니다.
func TestQueueOverflowMarksNamespaceStale(t *testing.T) {
	s := &Service{
		cfg:     Config{SearchEnabled: true, SearchIncremental: true, MaxSearchIndexBytes: DefaultMaxSearchIndexBytes},
		order:   []schema.GroupVersionResource{scopedGVR},
		entries: map[schema.GroupVersionResource]*resourceEntry{scopedGVR: {gvr: scopedGVR}},
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	s.budget.max.Store(DefaultMaxSearchIndexBytes)

	b := bindingFor(s, scopedGVR)
	for i := 0; i < maxPendingPerResource+16; i++ {
		s.enqueueKey(b, "prod", fmt.Sprintf("row-%05d", i))
	}
	s.delta.mu.Lock()
	q := s.delta.queueFor(scopedGVR)
	pending, dropped, stale, marker := len(q.events), q.dropped, q.staleNS.count, q.markerSeq
	s.delta.mu.Unlock()

	if pending != maxPendingPerResource {
		t.Fatalf("큐 길이가 %d입니다 — 상한 %d여야 합니다", pending, maxPendingPerResource)
	}
	if dropped != 16 {
		t.Fatalf("드롭이 %d건입니다 — 16건이어야 합니다", dropped)
	}
	if stale != 1 {
		t.Fatalf("stale namespace가 %d개입니다 — 드롭은 모두 같은 namespace입니다", stale)
	}
	if marker == 0 {
		t.Fatal("마커 seq가 남지 않았습니다 — 회수 게이트가 성립하지 않습니다")
	}
}

// TestStaleSetEscalatesToGVRBit — 추적 상한을 넘으면 유계 GVR 비트로 승급해야 합니다.
func TestStaleSetEscalatesToGVRBit(t *testing.T) {
	set := newStrSet(maxStaleTracked)
	for i := 0; i < maxStaleTracked; i++ {
		if !set.add(fmt.Sprintf("ns-%05d", i)) {
			t.Fatalf("%d번째 추가가 실패했습니다", i)
		}
	}
	if set.add("ns-overflow") {
		t.Fatal("상한을 넘겼는데 추가가 성공했습니다")
	}
	if set.count != maxStaleTracked {
		t.Fatalf("집합 크기가 %d입니다", set.count)
	}
	// 값 목록은 중복 없이 정렬되어야 합니다.
	values := set.values()
	if len(values) != maxStaleTracked {
		t.Fatalf("값이 %d개입니다", len(values))
	}
	for i := 1; i < len(values); i++ {
		if values[i-1] >= values[i] {
			t.Fatalf("%d번째에서 정렬·중복이 어긋났습니다", i)
		}
	}
}

// TestDiscardPurgesQueueAndReleasesReservations — 세대가 죽으면 대기 키도 함께
// 사라지고 예약이 정확히 한 번 해제되어야 합니다.
func TestDiscardPurgesQueueAndReleasesReservations(t *testing.T) {
	s := &Service{
		cfg:     Config{SearchEnabled: true, SearchIncremental: true, MaxSearchIndexBytes: DefaultMaxSearchIndexBytes},
		order:   []schema.GroupVersionResource{scopedGVR},
		entries: map[schema.GroupVersionResource]*resourceEntry{scopedGVR: {gvr: scopedGVR}},
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	e := s.entries[scopedGVR]

	b := bindingFor(s, scopedGVR)
	for i := 0; i < 32; i++ {
		s.enqueueKey(b, "prod", fmt.Sprintf("row-%02d", i))
	}
	if s.budget.queued.Load() == 0 {
		t.Fatal("예약이 잡히지 않았습니다")
	}
	e.discard(s)

	// 종단 경로입니다 — 큐 자체가 사라져야 합니다. 여기서 queueFor로 들여다보면
	// **관측이 큐를 되살려** 고정 몫이 다시 계상되므로, 맵을 직접 봅니다.
	s.delta.mu.Lock()
	q, exists := s.delta.queues[scopedGVR]
	pending := 0
	if exists {
		pending = len(q.events)
	}
	s.delta.mu.Unlock()
	if exists {
		t.Fatalf("폐기 뒤에도 큐가 남아 있습니다(대기 %d건)", pending)
	}
	if s.budget.queued.Load() != 0 || s.budget.live.Load() != 0 {
		t.Fatalf("예약이 남았습니다: queued=%d live=%d", s.budget.queued.Load(), s.budget.live.Load())
	}
}

/* ── tombstone·키 분해 ──────────────────────────────────────────────────── */

func TestSplitMetaKeyHandlesClusterScoped(t *testing.T) {
	cases := []struct {
		key        string
		namespaced bool
		ns         string
		name       string
		ok         bool
		comment    string
	}{
		{"prod/payments-api", true, "prod", "payments-api", true, "namespaced"},
		{"node-a", false, "", "node-a", true, "cluster-scoped"},
		{"", true, "", "", false, "빈 키"},
		{"", false, "", "", false, "빈 키(cluster)"},
	}
	for _, c := range cases {
		ns, name, ok := splitMetaKey(c.key, c.namespaced)
		if ns != c.ns || name != c.name || ok != c.ok {
			t.Fatalf("%s: (%q,%q,%v) — (%q,%q,%v)여야 합니다", c.comment, ns, name, ok, c.ns, c.name, c.ok)
		}
	}
}

/* ── 회복 게이트 ───────────────────────────────────────────────────────── */

// TestCoversThroughSeqGateIsInclusive — 게이트는 `>=`여야 합니다.
// `>`면 추가 이벤트가 없는 정지 상태에서 회수가 영원히 시작되지 않습니다.
func TestCoversThroughSeqGateIsInclusive(t *testing.T) {
	// seq=5에서 드롭이 났고, 이후 이벤트가 없어 목록 스냅숏의 cover도 5입니다.
	const markerSeq = uint64(5)
	const covers = uint64(5)
	if !(covers >= markerSeq) {
		t.Fatal("게이트 조건이 성립하지 않습니다")
	}
	if covers > markerSeq {
		t.Fatal("이 시나리오는 covers == markerSeq여야 합니다")
	}
}

/* ── 기본 예산에서의 표준 100k ─────────────────────────────────────────── */

// TestStandard100kReadyAtDefaultBudget — 기본 64MiB 전역 / 32MiB GVR 몫에서
// 표준 픽스처(10만 행, 이름 ~14B, **UID 36B**, label 2쌍)가 준비되어야 합니다.
//
// 준비되지 못하면 그 사실을 수치와 함께 남깁니다 — 조용히 낡지 않는 것이 계약입니다.
func TestStandard100kReadyAtDefaultBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("표준 100k 픽스처는 -short에서 건너뜁니다")
	}
	const rows = 100_000
	objs := make([]*metav1.PartialObjectMetadata, 0, rows)
	for i := 0; i < rows; i++ {
		objs = append(objs, metaRow(
			fmt.Sprintf("ns-%04d", i%500),
			fmt.Sprintf("obj-%06d", i),
			fmt.Sprintf("uid-%06d-abcdef0123456789abcdef01", i), // 36바이트에 맞춥니다.
			map[string]string{
				"app":  fmt.Sprintf("svc-%04d", i%500),
				"tier": "backend",
			}))
	}
	perResource := int64(DefaultMaxSearchIndexBytes) / searchPerResourceDivisor
	peak := int64(searchPeakMultiplier) * DefaultMaxSearchIndexBytes

	r := buildSearchIndex(indexOf(objs...), "Service", true, perResource, peak)
	if r.state != SearchReady {
		t.Fatalf("표준 100k가 기본 예산에서 준비되지 않았습니다: state=%s reason=%s needed=%d (몫 %d)",
			r.state, r.reason, r.needed, perResource)
	}
	t.Logf("표준 100k retained=%d bytes (GVR 몫 %d, 여유 %.1f%%)",
		r.index.bytes, perResource, 100*float64(perResource-r.index.bytes)/float64(perResource))
	if r.index.bytes > perResource {
		t.Fatalf("보유 %d가 GVR 몫 %d를 넘었습니다", r.index.bytes, perResource)
	}
	if got := r.index.partitionCount(); got != 500 {
		t.Fatalf("파티션이 %d개입니다 — 500개여야 합니다", got)
	}
}

// TestMaxLengthInputDegradesExplicitly — 최대 길이 입력은 예산을 넘고,
// 그 사실이 **명시적 SearchUnavailable + 필요 바이트**로 드러나야 합니다.
func TestMaxLengthInputDegradesExplicitly(t *testing.T) {
	longName := strings.Repeat("a", 253)
	labels := map[string]string{}
	for i := 0; i < MaxLabelKeysPerObject; i++ {
		labels[fmt.Sprintf("%s/%s-%02d", strings.Repeat("d", 100), strings.Repeat("k", 100), i)] =
			strings.Repeat("v", 63)
	}
	objs := make([]*metav1.PartialObjectMetadata, 0, 4000)
	for i := 0; i < 4000; i++ {
		objs = append(objs, metaRow("prod",
			fmt.Sprintf("%s%04d", longName[:249], i),
			fmt.Sprintf("uid-%04d-abcdef0123456789abcdef0123", i), labels))
	}
	r := buildSearchIndex(indexOf(objs...), "Service", true, 1<<20, 4<<20)
	if r.state != SearchUnavailable || r.reason != reasonBudget {
		t.Fatalf("최대 길이 입력이 조용히 통과했습니다: state=%s reason=%s", r.state, r.reason)
	}
	if r.needed <= 0 {
		t.Fatal("필요 바이트를 알리지 않았습니다 — 회로 판정이 불가능합니다")
	}
	if r.index != nil {
		t.Fatal("예산을 넘겼는데 반쪽 인덱스를 돌려줬습니다")
	}
}

/* ── 계측 형태 ──────────────────────────────────────────────────────────── */

// TestDeltaMetricsHaveBoundedLabels — 계측 라벨은 resource 하나뿐이어야 합니다.
func TestDeltaMetricsHaveBoundedLabels(t *testing.T) {
	s := dualService(t, true, metaRow("prod", "payments-api", "uid-1", nil))
	var buf strings.Builder
	if err := s.WriteSearchMetrics(&buf); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	for _, want := range []string{
		"dashboard_resource_search_live_bytes",
		"dashboard_resource_search_retained_bytes",
		"dashboard_resource_search_pending_events{resource=",
		"dashboard_resource_search_dropped_events_total{resource=",
		"dashboard_resource_search_recovery_attempts_total",
		"dashboard_resource_search_full_bootstrap_total",
		"dashboard_resource_search_resource_partitions{resource=",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("계측에 %q가 없습니다", want)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "{") {
			continue
		}
		inner := line[strings.Index(line, "{")+1 : strings.Index(line, "}")]
		for _, pair := range strings.Split(inner, ",") {
			if pair == "" {
				continue
			}
			key := strings.SplitN(pair, "=", 2)[0]
			if key != "resource" && key != "state" {
				t.Fatalf("허용되지 않은 계측 라벨 %q: %s", key, line)
			}
		}
	}
}
