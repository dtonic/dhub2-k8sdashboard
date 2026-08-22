package resourcecatalog

// Round 8 P1 회귀
// --------------------------------------------------------------------------
// 소유권·상한(항목 1), cursor 전역 순서(2), 게시 결과 구분(3), 회수 회로 지속(4),
// 포화 스케줄러(5)를 각각 못박습니다.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

/* ── 다중 GVR 하네스 ────────────────────────────────────────────────────── */

// multiHarness는 여러 GVR을 한 서비스에 올린 하네스입니다.
type multiHarness struct {
	svc   *Service
	order []schema.GroupVersionResource
}

func newMultiHarness(t *testing.T, rowsByGVR map[schema.GroupVersionResource][]*metav1.PartialObjectMetadata) *multiHarness {
	t.Helper()
	order := make([]schema.GroupVersionResource, 0, len(rowsByGVR))
	for gvr := range rowsByGVR {
		order = append(order, gvr)
	}
	// allowlist와 같은 규칙(FormatGVR 사전순)으로 정렬합니다.
	for i := 1; i < len(order); i++ {
		for j := i; j > 0 && FormatGVR(order[j]) < FormatGVR(order[j-1]); j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}
	s := &Service{
		cfg: Config{
			ClusterID: "prod-seoul", SearchEnabled: true, SearchIncremental: true,
			MaxSearchIndexBytes: DefaultMaxSearchIndexBytes,
			Now:                 func() time.Time { return indexBase },
		},
		order:   order,
		entries: make(map[schema.GroupVersionResource]*resourceEntry, len(order)),
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	s.started.Store(true)

	disc := &discoverySnapshot{byGVR: map[schema.GroupVersionResource]int{}}
	for _, gvr := range order {
		disc.byGVR[gvr] = len(disc.entries)
		disc.entries = append(disc.entries, discoveryEntry{
			gvr: gvr, kind: "Thing", namespaced: true, served: true,
			verbs: []string{"get", "list", "watch"},
		})
		e := &resourceEntry{gvr: gvr}
		e.setStatus(StateReady, "")
		e.lifecycle, e.generation = 1, 0
		e.tokenPacked.Store(packToken(1, 0))
		e.informer = &fakeInformer{store: cache.NewStore(cache.MetaNamespaceKeyFunc), synced: true}
		e.bootstrapped.Store(true)
		s.entries[gvr] = e
	}
	s.disc.Store(disc)

	for _, gvr := range order {
		rows := rowsByGVR[gvr]
		index := indexOf(rows...)
		r := buildSearchIndex(index, "Thing", true, hugeBudget, hugeBudget)
		if r.state != SearchReady {
			t.Fatalf("%s 부트스트랩 실패: %s", FormatGVR(gvr), r.state)
		}
		e := s.entries[gvr]
		snap := &entrySnapshot{
			index: index, sindex: r.index, searchState: SearchReady,
			indexVer: 1, searchVer: 1,
		}
		e.setSnap(snap)
		s.snapMu.Lock()
		admitted := s.publishLeaseLocked(e, snap)
		s.snapMu.Unlock()
		if !admitted {
			t.Fatalf("%s 세대가 예산 승인을 받지 못했습니다", FormatGVR(gvr))
		}
	}
	return &multiHarness{svc: s, order: order}
}

/* ── 항목 2: cursor 전역 순서 ───────────────────────────────────────────── */

// TestPersistentCursorOrdersByGVRAcrossThreeResources — token·namespace·name이
// **완전히 같은** 세 GVR에서, 전역 순서 (token, namespace, name, gvr, uid)의
// gvr 성분이 실제로 순서를 가르는지 봅니다.
//
// UID 순서를 GVR 순서와 **반대로** 두어, uid로 정렬하면 결과가 뒤집히게 만듭니다.
// limit=1 페이징이 한 번에 받은 결과와 정확히 같아야 합니다(중복·누락 없음).
func TestPersistentCursorOrdersByGVRAcrossThreeResources(t *testing.T) {
	gvrA := schema.GroupVersionResource{Group: "aaa", Version: "v1", Resource: "things"}
	gvrB := schema.GroupVersionResource{Group: "bbb", Version: "v1", Resource: "things"}
	gvrC := schema.GroupVersionResource{Group: "ccc", Version: "v1", Resource: "things"}

	// 이름·namespace가 모두 같고 UID만 다릅니다. UID는 GVR 순서의 역순입니다.
	const ns, name = "prod", "shared-name"
	h := newMultiHarness(t, map[schema.GroupVersionResource][]*metav1.PartialObjectMetadata{
		gvrA: {metaRow(ns, name, "uid-zzz", nil)},
		gvrB: {metaRow(ns, name, "uid-mmm", nil)},
		gvrC: {metaRow(ns, name, "uid-aaa", nil)},
	})

	oneShot := searchAllKeys(t, h.svc, "shared", 10)
	if len(oneShot) != 3 {
		t.Fatalf("한 번에 받은 결과가 %d건입니다: %v", len(oneShot), oneShot)
	}
	// gvr 사전순(aaa < bbb < ccc)이어야 합니다 — uid 순서(aaa<mmm<zzz)와 반대입니다.
	want := []string{
		"aaa/v1/things|uid-zzz",
		"bbb/v1/things|uid-mmm",
		"ccc/v1/things|uid-aaa",
	}
	for i := range want {
		if oneShot[i] != want[i] {
			t.Fatalf("전역 순서가 gvr을 따르지 않습니다:\n got  %v\n want %v", oneShot, want)
		}
	}

	paged := searchAllKeys(t, h.svc, "shared", 1)
	if len(paged) != len(oneShot) {
		t.Fatalf("페이징 결과가 %d건입니다 — %d건이어야 합니다\n%v\n%v",
			len(paged), len(oneShot), paged, oneShot)
	}
	for i := range paged {
		if paged[i] != oneShot[i] {
			t.Fatalf("limit=1 페이징이 한 번에 받은 결과와 다릅니다:\n %v\n %v", paged, oneShot)
		}
	}

	// 페이징 도중 세대가 바뀌어도 중복이 생기면 안 됩니다.
	first, err := h.svc.Search(SearchRequest{Query: "shared", Limit: 1, Namespaces: allNS()})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("이어보기 cursor가 없습니다")
	}
	// 무관한 GVR에 새 세대를 게시합니다(같은 내용, 새 인덱스 객체).
	eC := h.svc.entries[gvrC]
	rebuilt := buildSearchIndex(indexOf(metaRow(ns, name, "uid-aaa", nil)), "Thing", true, hugeBudget, hugeBudget)
	if rebuilt.state != SearchReady {
		t.Fatalf("재빌드 실패: %s", rebuilt.state)
	}
	curC := eC.load()
	nextC := &entrySnapshot{
		index: curC.index, sindex: rebuilt.index, searchState: SearchReady,
		indexVer: curC.indexVer + 1, searchVer: curC.searchVer + 1,
	}
	h.svc.snapMu.Lock()
	h.svc.publishLeaseLocked(eC, nextC)
	eC.setSnap(nextC)
	h.svc.snapMu.Unlock()

	rest := []string{keyOfItem(first.Items[0])}
	cursor := first.NextCursor
	for round := 0; round < 8 && cursor != ""; round++ {
		page, err := h.svc.Search(SearchRequest{
			Query: "shared", Limit: 1, Cursor: cursor, Namespaces: allNS(),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			rest = append(rest, keyOfItem(it))
		}
		cursor = page.NextCursor
	}
	if len(rest) != 3 {
		t.Fatalf("세대 교체 중 페이징이 %d건입니다: %v", len(rest), rest)
	}
	seen := map[string]bool{}
	for _, k := range rest {
		if seen[k] {
			t.Fatalf("세대 교체 중 중복이 나왔습니다: %v", rest)
		}
		seen[k] = true
	}
}

func keyOfItem(it SearchItem) string {
	group := it.Group
	if group == "" {
		group = "core"
	}
	return group + "/" + it.Version + "/" + it.Resource + "|" + it.UID
}

// searchAllKeys는 끝까지 페이징한 (gvr|uid) 목록입니다.
func searchAllKeys(t *testing.T, s *Service, query string, limit int) []string {
	t.Helper()
	var out []string
	cursor := ""
	for round := 0; round < 64; round++ {
		page, err := s.Search(SearchRequest{
			Query: query, Limit: limit, Cursor: cursor, Namespaces: allNS(),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			out = append(out, keyOfItem(it))
		}
		if page.NextCursor == "" {
			return out
		}
		cursor = page.NextCursor
	}
	t.Fatal("페이지가 끝나지 않았습니다")
	return nil
}

/* ── 항목 1: 소유권 ─────────────────────────────────────────────────────── */

// TestRestrictedScopeSearchTakesNoLease — 범위 제한 요청은 검색 인덱스를 훑지
// 않으므로 세대를 **빌리지도, 들여다보지도** 않아야 합니다.
func TestRestrictedScopeSearchTakesNoLease(t *testing.T) {
	h := newDeltaHarness(t,
		metaRow("allowed", "payments-api", "uid-1", nil),
		metaRow("hidden", "secret", "uid-2", nil),
	)
	lease := h.entry.leasePtr.Load()
	if lease == nil {
		t.Fatal("세대가 설치되지 않았습니다")
	}
	before := lease.refs.Load()

	// 범위 제한 요청 도중에도 refs가 움직이면 안 됩니다.
	page, err := h.svc.Search(SearchRequest{
		Query: "payments", Namespaces: NamespaceFilter{List: []string{"allowed"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "payments-api" {
		t.Fatalf("범위 제한 결과가 어긋났습니다: %+v", page.Items)
	}
	if got := lease.refs.Load(); got != before {
		t.Fatalf("범위 제한 요청이 세대를 빌렸습니다: refs %d → %d", before, got)
	}

	// 대조군: 클러스터 전체 접근은 빌렸다가 놓습니다(끝나면 다시 같은 값).
	if _, err := h.svc.Search(SearchRequest{Query: "payments", Namespaces: allNS()}); err != nil {
		t.Fatal(err)
	}
	if got := lease.refs.Load(); got != before {
		t.Fatalf("전체 접근이 세대를 놓지 않았습니다: refs %d → %d", before, got)
	}
}

// TestListOnlyPublishReusesSearchLease — 목록만 갱신된 게시는 같은 검색 객체를
// 다시 계상하거나 세대를 은퇴시키면 안 됩니다.
func TestListOnlyPublishReusesSearchLease(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	before := h.entry.leasePtr.Load()
	beforeBytes := h.svc.searchBytes.Load()
	if before == nil || beforeBytes <= 0 {
		t.Fatalf("초기 상태가 어긋났습니다: lease=%v bytes=%d", before, beforeBytes)
	}

	// 목록만 바뀐 게시를 흉내 냅니다(검색 절반을 그대로 물려받습니다).
	cur := h.entry.load()
	_, token, _ := h.entry.beginBuild(h.svc)
	newIndex := indexOf(
		metaRow("prod", "payments-api", "uid-1", nil),
		metaRow("prod", "ledger", "uid-2", nil),
	)
	outcome := h.entry.publishList(h.svc, token, newIndex, searchBuildResult{},
		listPublishExtra{keepSearch: true, setCovers: true, covers: 7})
	if outcome != publishedListOnly {
		t.Fatalf("목록 전용 게시 결과가 %v입니다", outcome)
	}

	after := h.entry.leasePtr.Load()
	if after != before {
		t.Fatal("목록만 갱신했는데 검색 세대가 교체되었습니다")
	}
	if got := h.svc.searchBytes.Load(); got != beforeBytes {
		t.Fatalf("같은 검색 객체를 다시 계상했습니다: %d → %d", beforeBytes, got)
	}
	if before.retired.Load() {
		t.Fatal("목록만 갱신했는데 세대가 은퇴했습니다")
	}
	// 목록은 실제로 갱신되어야 합니다.
	if idx := h.entry.baselineIndex(); idx == nil || len(idx.rows) != 2 {
		t.Fatalf("목록이 갱신되지 않았습니다: %v", idx)
	}
	if cur.index == h.entry.load().index {
		t.Fatal("목록 스냅숏이 바뀌지 않았습니다")
	}
}

// TestSamePayloadStateTransitionsReachLaterAcquirers — 검색 payload 포인터가
// **동일한 채로** 상태·사유만 바뀌는 게시에서, 이후 대여자는 새 상태를 보고
// 이미 빌려 간 요청 뷰는 자기가 집은 시점을 그대로 유지해야 합니다.
//
// 세대(lease)를 재사용하면서 그 세대가 대표하는 스냅숏을 갈아 끼우지 않으면,
// syncing→unavailable→ready 전이가 대여자에게 영원히 보이지 않습니다.
func TestSamePayloadStateTransitionsReachLaterAcquirers(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	e := h.entry
	cur := e.load()
	payload := cur.sindex // 이 포인터는 끝까지 바뀌지 않습니다.
	if payload == nil {
		t.Fatal("검색 payload가 없습니다")
	}
	lease0 := e.leasePtr.Load()
	bytes0 := h.svc.searchBytes.Load()

	// 요청 하나가 **먼저** 빌려 갑니다. 이 뷰는 이후 전이에 영향받지 않아야 합니다.
	early, earlySnap := e.acquireSearch(h.svc)
	if early == nil || earlySnap == nil {
		t.Fatal("세대를 빌리지 못했습니다")
	}
	defer h.svc.releaseSearch(early)
	earlyState, earlyReason := earlySnap.searchState, earlySnap.searchReason

	steps := []struct {
		state  SearchState
		reason string
	}{
		{SearchSyncing, ""},
		{SearchUnavailable, reasonBudget},
		{SearchReady, ""},
	}
	for i, step := range steps {
		next := &entrySnapshot{
			index:        indexOf(metaRow("prod", "payments-api", "uid-1", nil)),
			sindex:       payload, // **같은 객체**입니다.
			search:       cur.search,
			searchState:  step.state,
			searchReason: step.reason,
			indexVer:     cur.indexVer + uint64(i) + 1,
			searchVer:    cur.searchVer,
		}
		h.svc.snapMu.Lock()
		ok := h.svc.publishLeaseLocked(e, next)
		h.svc.snapMu.Unlock()
		if !ok {
			t.Fatalf("%d단계 게시가 거절되었습니다", i)
		}
		e.setSnap(next)

		// 세대는 그대로여야 합니다(재계상·은퇴 없음).
		if got := e.leasePtr.Load(); got != lease0 {
			t.Fatalf("%d단계에서 세대가 교체되었습니다", i)
		}
		if lease0.retired.Load() {
			t.Fatalf("%d단계에서 세대가 은퇴했습니다", i)
		}
		if got := h.svc.searchBytes.Load(); got != bytes0 {
			t.Fatalf("%d단계에서 같은 payload를 다시 계상했습니다: %d → %d", i, bytes0, got)
		}

		// **이후 대여자**는 새 상태를 봅니다.
		later, laterSnap := e.acquireSearch(h.svc)
		if laterSnap == nil {
			t.Fatalf("%d단계: 이후 대여자가 스냅숏을 받지 못했습니다", i)
		}
		if laterSnap != next {
			t.Fatalf("%d단계: 이후 대여자가 옛 스냅숏을 봅니다", i)
		}
		if laterSnap.searchState != step.state || laterSnap.searchReason != step.reason {
			t.Fatalf("%d단계: 이후 대여자가 state=%q reason=%q를 봅니다 — %q/%q여야 합니다",
				i, laterSnap.searchState, laterSnap.searchReason, step.state, step.reason)
		}
		h.svc.releaseSearch(later)

		// **먼저 빌려 간 뷰**는 그대로입니다.
		if earlySnap.searchState != earlyState || earlySnap.searchReason != earlyReason {
			t.Fatalf("%d단계: 이미 빌린 요청 뷰가 바뀌었습니다: %q/%q",
				i, earlySnap.searchState, earlySnap.searchReason)
		}
	}
	assertNoNegativeAccounting(t, h.svc, "same-payload-transition")
}

// TestNearCapOwnershipAdmissionHoldsInvariants — 상한 가까이에서 옛 독자가 세대를
// 붙잡은 채 같은 크기의 새 세대를 반복 게시하고, 동시에 큐가 차오르고, 계측·flush가
// 함께 돌아도 I-A/I-C를 넘지 않고 마지막에는 수렴해야 합니다.
func TestNearCapOwnershipAdmissionHoldsInvariants(t *testing.T) {
	// 큐 고정 구조(약 40KiB)가 세대 하나보다 크면 상한을 세대 기준으로 좁힐 수
	// 없습니다. 고정 몫이 **세대에 비해 작아지도록** 충분히 큰 인덱스를 씁니다.
	rows := make([]*metav1.PartialObjectMetadata, 0, 400)
	for i := 0; i < 400; i++ {
		rows = append(rows, metaRow("prod", fmt.Sprintf("row-%04d", i),
			fmt.Sprintf("uid-%04d", i), map[string]string{"app": "payments"}))
	}
	h := newDeltaHarness(t, rows...)
	genBytes := h.svc.searchBytes.Load()
	if genBytes <= 0 {
		t.Fatalf("세대 크기가 %d입니다", genBytes)
	}

	// 큐를 **먼저 만들어** 고정 몫을 원장에 싣습니다. 상한은 그 뒤에 고릅니다 —
	// 큐가 정당하게 소유하는 몫을 빼고 상한을 정하면, 프로덕션이 아니라
	// 픽스처가 I-C를 깨뜨립니다.
	h.svc.delta.mu.Lock()
	fixed := h.svc.delta.queueFor(h.gvr).fixed
	h.svc.delta.mu.Unlock()
	if fixed <= 0 {
		t.Fatalf("큐 고정 몫이 %d입니다 — 계상되지 않았습니다", fixed)
	}

	// 세대 세 개 남짓만 들어가도록 좁히되, I-C(=3*max)가 **고정 몫 + 세대 세 개 +
	// 배치 하나의 임시분**을 덮을 수 있는 하한은 지킵니다.
	//	fixed + retained(<=max) + transient <= 3*max  ⇔  max >= (fixed + transient)/2
	tightCap := genBytes * 3
	// 안전 여유는 이 픽스처가 실제로 만들 수 있는 배치(한 번에 몇 건)를 넉넉히
	// 덮는 값이면 됩니다. 그보다 크게 잡으면 상한이 헐거워져 near-cap이 아니게 됩니다.
	if floor := (fixed+deltaTransientBytes(8)+genBytes)/2 + 1; tightCap < floor {
		tightCap = floor
	}
	h.svc.budget.max.Store(tightCap)
	// GVR 몫(cfg/2)이 정확히 I-A와 같은 지점에서 걸리도록 둡니다. I-B를 끄는 것이
	// 아니라, GVR이 하나뿐인 픽스처에서 두 불변식이 같은 값으로 강제되게 하는 것입니다.
	h.svc.cfg.MaxSearchIndexBytes = tightCap * 2

	// 옛 세대를 붙잡는 독자.
	pinned := h.svc.acquireView()
	defer h.svc.releaseView(pinned)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	var violated sync.Once
	violation := ""

	check := func(where string) {
		if got := h.svc.budget.retained.Load(); got > h.svc.budget.limit() {
			violated.Do(func() { violation = fmt.Sprintf("%s: I-A 위반 retained=%d > %d", where, got, h.svc.budget.limit()) })
		}
		if got := h.svc.budget.live.Load(); got > h.svc.budget.peakLimit() {
			violated.Do(func() { violation = fmt.Sprintf("%s: I-C 위반 live=%d > %d", where, got, h.svc.budget.peakLimit()) })
		}
	}

	// 같은 크기의 새 세대를 반복 게시합니다.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(writerDone)
		for i := 0; i < 40; i++ {
			r := buildSearchIndex(indexOf(rows...), "Service", true, hugeBudget, hugeBudget)
			if r.state != SearchReady {
				continue
			}
			cur := h.entry.load()
			next := &entrySnapshot{
				index: cur.index, sindex: r.index, searchState: SearchReady,
				indexVer: cur.indexVer + 1, searchVer: cur.searchVer + 1,
			}
			h.svc.snapMu.Lock()
			if h.svc.publishLeaseLocked(h.entry, next) {
				h.entry.setSnap(next)
			}
			h.svc.snapMu.Unlock()
			check("publish")
		}
	}()

	// 동시에 큐를 채우고 flush를 돌립니다.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.svc.enqueueKey(h.binding, "prod", fmt.Sprintf("row-%04d", i%64))
			h.svc.flushSearchDeltas(context.Background(), h.gvr, maxBatchPerResource)
			check("flush")
		}
	}()

	// 계측·검색 독자도 함께 돕니다.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			v := h.svc.acquireView()
			h.svc.releaseView(v)
			check("reader")
		}
	}()

	<-writerDone
	close(stop)
	wg.Wait()

	if violation != "" {
		t.Fatal(violation)
	}
	// 붙잡은 독자를 놓으면 최종 상태로 수렴해야 합니다.
	h.svc.releaseView(pinned)
	live := h.entry.leasePtr.Load()
	if live == nil {
		t.Fatal("게시본이 없습니다")
	}
	if got := h.svc.searchBytes.Load(); got != live.bytes {
		t.Fatalf("수렴하지 않았습니다: searchBytes=%d, 게시본=%d", got, live.bytes)
	}
	if got := h.svc.budget.retained.Load(); got != live.bytes {
		t.Fatalf("retained가 %d입니다 — %d여야 합니다", got, live.bytes)
	}
	assertNoNegativeAccounting(t, h.svc, "near-cap")
	if got := h.svc.budget.inflight.Load(); got != 0 {
		t.Fatalf("inflight가 %d 남았습니다", got)
	}
}

// TestPerGVRCapCountsEveryPinnedGeneration — I-B는 **그 GVR이 소유한 모든 세대**를
// 셉니다. 전역 64MiB / GVR 몫 32MiB에서 10MiB 세대를 넷 게시하고 넷 다 붙잡으면,
// 네 번째는 거절되어야 합니다(30 + 10 > 32).
//
// 직전 세대 하나만 세던 시절에는 넷 다 통과했습니다 — 느린 독자가 은퇴 세대를
// 붙잡고 있는 GVR이 자기 몫의 몇 배를 차지할 수 있었다는 뜻입니다.
func TestPerGVRCapCountsEveryPinnedGeneration(t *testing.T) {
	const genBytes = 10 << 20
	s := lifetimeService() // 전역 64MiB, GVR 몫 32MiB
	if got, want := s.budget.limit(), int64(64<<20); got != want {
		t.Fatalf("전역 상한이 %d입니다 — %d여야 합니다", got, want)
	}
	if got, want := s.perResourceSearchBudget(), int64(32<<20); got != want {
		t.Fatalf("GVR 몫이 %d입니다 — %d여야 합니다", got, want)
	}
	e := &resourceEntry{gvr: scopedGVR}

	pinned := make([]*searchLease, 0, 4)
	admittedCount := 0
	for i := 0; i < 4; i++ {
		// 세대마다 **서로 다른** 인덱스 객체입니다(같은 payload 재사용이 아닙니다).
		snap := &entrySnapshot{sindex: &searchIndex{bytes: genBytes}, searchVer: uint64(i + 1)}
		s.snapMu.Lock()
		ok := s.publishLeaseLocked(e, snap)
		s.snapMu.Unlock()
		switch {
		case i < 3 && !ok:
			t.Fatalf("%d번째 세대가 거절되었습니다 — GVR 몫 안입니다", i+1)
		case i == 3 && ok:
			t.Fatalf("네 번째 세대가 승인되었습니다 — I-B(%d)를 넘습니다", s.perResourceSearchBudget())
		}
		if !ok {
			continue
		}
		admittedCount++
		l, _ := e.acquireSearch(s)
		if l == nil {
			t.Fatalf("%d번째 세대를 빌리지 못했습니다", i+1)
		}
		pinned = append(pinned, l)

		if got := e.ownerRetained.Load(); got > s.perResourceSearchBudget() {
			t.Fatalf("I-B 위반: ownerRetained=%d > %d", got, s.perResourceSearchBudget())
		}
		if got := s.budget.retained.Load(); got > s.budget.limit() {
			t.Fatalf("I-A 위반: retained=%d > %d", got, s.budget.limit())
		}
		if got := s.budget.live.Load(); got > s.budget.peakLimit() {
			t.Fatalf("I-C 위반: live=%d > %d", got, s.budget.peakLimit())
		}
	}
	if admittedCount != 3 {
		t.Fatalf("승인된 세대가 %d개입니다 — 3개여야 합니다", admittedCount)
	}
	if got, want := e.ownerRetained.Load(), int64(3*genBytes); got != want {
		t.Fatalf("소유자 원장이 %d입니다 — %d여야 합니다", got, want)
	}

	// 붙잡은 것을 놓으면 게시본 하나로 수렴합니다.
	for _, l := range pinned {
		s.releaseSearch(l)
	}
	if got, want := e.ownerRetained.Load(), int64(genBytes); got != want {
		t.Fatalf("해제 뒤 소유자 원장이 %d입니다 — %d여야 합니다", got, want)
	}
	if got, want := s.budget.retained.Load(), int64(genBytes); got != want {
		t.Fatalf("해제 뒤 retained가 %d입니다 — %d여야 합니다", got, want)
	}
	assertNoNegativeAccounting(t, s, "per-gvr-cap")

	// 자리가 생겼으므로 네 번째 세대가 이제 통과해야 합니다.
	snap := &entrySnapshot{sindex: &searchIndex{bytes: genBytes}, searchVer: 4}
	s.snapMu.Lock()
	ok := s.publishLeaseLocked(e, snap)
	s.snapMu.Unlock()
	if !ok {
		t.Fatal("독자가 모두 놓았는데도 새 세대가 거절되었습니다")
	}
	if got, want := e.ownerRetained.Load(), int64(genBytes); got != want {
		t.Fatalf("교체 뒤 소유자 원장이 %d입니다 — %d여야 합니다", got, want)
	}
}

// TestStopReleasesQueueFixedExactlyOnce — 리소스가 멈추면 큐 구조의 고정 몫까지
// **정확히 한 번** 되돌리고 원장이 0으로 수렴해야 합니다.
//
// 살아 있는 큐가 고정 몫을 소유하는 것은 누수가 아닙니다. 누수가 되는 지점은
// 리소스가 멈췄는데도 그 몫이 남는 경우이고, 이 테스트가 그 종단 경로를 못박습니다.
func TestStopReleasesQueueFixedExactlyOnce(t *testing.T) {
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))

	// 큐를 실제로 쓰게 만듭니다(고정 몫 + 이벤트 몫).
	h.upsert(t, metaRow("prod", "brand-new", "uid-2", nil))
	h.svc.requestResync(h.gvr, "prod", 7) // 회로도 하나 만들어 둡니다.
	h.svc.delta.mu.Lock()
	h.svc.delta.circuitFor(recoveryTarget{gvr: h.gvr, namespace: "prod"})
	fixed := h.svc.delta.queueFor(h.gvr).fixed
	circuits := len(h.svc.delta.circuits)
	h.svc.delta.mu.Unlock()
	if fixed <= 0 || circuits == 0 {
		t.Fatalf("고정 몫=%d 회로=%d — 계상 대상이 만들어지지 않았습니다", fixed, circuits)
	}
	if got := h.svc.budget.queued.Load(); got <= fixed {
		t.Fatalf("이벤트 몫이 계상되지 않았습니다: queued=%d, 고정=%d", got, fixed)
	}

	// 종단: 멈춥니다.
	h.entry.discard(h.svc)

	h.svc.delta.mu.Lock()
	_, queueLeft := h.svc.delta.queues[h.gvr]
	circuitsLeft := len(h.svc.delta.circuits)
	h.svc.delta.mu.Unlock()
	if queueLeft {
		t.Fatal("멈춘 리소스의 큐가 남아 있습니다")
	}
	if circuitsLeft != 0 {
		t.Fatalf("멈춘 리소스의 회로가 %d개 남았습니다", circuitsLeft)
	}
	if got := h.svc.budget.queued.Load(); got != 0 {
		t.Fatalf("멈춘 뒤 queued가 %d입니다", got)
	}
	if got := h.svc.budget.live.Load(); got != 0 {
		t.Fatalf("멈춘 뒤 live가 %d입니다", got)
	}
	assertNoNegativeAccounting(t, h.svc, "stop")

	// 두 번 멈춰도 이중 해제가 없어야 합니다.
	h.entry.discard(h.svc)
	if got := h.svc.budget.queued.Load(); got != 0 {
		t.Fatalf("두 번째 폐기 뒤 queued가 %d입니다", got)
	}
	assertNoNegativeAccounting(t, h.svc, "stop-twice")
}

// TestDiscardDuringWorkerKeepsRecoveryReservation — 작업자가 지역 사본을 들고
// 있는 동안에는 회수 예약이 살아 있어야 하고, 작업자가 빠질 때 정확히 한 번 풀려야 합니다.
func TestDiscardDuringWorkerKeepsRecoveryReservation(t *testing.T) {
	rows := make([]*metav1.PartialObjectMetadata, 0, 200)
	for i := 0; i < 200; i++ {
		rows = append(rows, metaRow("prod", fmt.Sprintf("row-%04d", i), fmt.Sprintf("uid-%04d", i), nil))
	}
	h := newDeltaHarness(t, rows...)
	ticket := pinTicket(t, h, "prod")

	h.svc.delta.mu.Lock()
	reserved := ticket.reserved
	// 작업자가 지역 사본을 든 상태를 흉내 냅니다.
	ticket.workers++
	h.svc.delta.mu.Unlock()
	if reserved <= 0 {
		t.Fatalf("회수 예약이 %d입니다", reserved)
	}
	if got := h.svc.budget.recovery.Load(); got != reserved {
		t.Fatalf("회수 예약이 %d입니다 — %d여야 합니다", got, reserved)
	}

	// 폐기: 티켓은 죽지만 예약은 **아직** 살아 있어야 합니다.
	h.entry.discard(h.svc)
	if got := h.svc.budget.recovery.Load(); got != reserved {
		t.Fatalf("작업자가 들고 있는데 예약이 풀렸습니다: %d", got)
	}

	// 작업자가 빠지면 정확히 한 번 풀립니다.
	h.svc.delta.mu.Lock()
	h.svc.workerDoneLocked(ticket)
	h.svc.delta.mu.Unlock()
	if got := h.svc.budget.recovery.Load(); got != 0 {
		t.Fatalf("작업자가 빠졌는데 예약이 %d 남았습니다", got)
	}
	assertNoNegativeAccounting(t, h.svc, "worker-drop")
}

/* ── 항목 3: 게시 결과 구분 ─────────────────────────────────────────────── */

// TestBudgetRejectedBootstrapStaysRetryableWithoutSpin — **진짜 GVR 두 개**로,
// 경쟁 GVR이 세대를 붙잡고 있는 동안 대상 GVR의 부트스트랩이 거절되고,
// 경쟁이 놓으면 프로덕션 경로(rebuildIndexes → buildSearchIndexFor)가 스스로
// 다시 시도해 설치까지 가야 합니다.
//
// 예산 값을 직접 Store하지 않습니다 — 실제 lease 두 개가 상한을 나눠 갖게 하고,
// 회로가 "설정 상한"이 아니라 **가용 용량**을 보는지까지 확인합니다.
func TestBudgetRejectedBootstrapStaysRetryableWithoutSpin(t *testing.T) {
	hogGVR := schema.GroupVersionResource{Group: "hog.example.com", Version: "v1", Resource: "things"}
	targetGVR := schema.GroupVersionResource{Group: "target.example.com", Version: "v1", Resource: "things"}

	// 두 GVR 모두 실제 informer·Store를 답니다.
	hogRows := make([]*metav1.PartialObjectMetadata, 0, 256)
	for i := 0; i < 256; i++ {
		hogRows = append(hogRows, metaRow("prod", fmt.Sprintf("hog-%04d", i), fmt.Sprintf("uid-hog-%04d", i), nil))
	}
	targetRows := []*metav1.PartialObjectMetadata{
		metaRow("prod", "payments-api", "uid-target-1", nil),
		metaRow("prod", "payments-worker", "uid-target-2", nil),
	}
	h := newMultiHarness(t, map[schema.GroupVersionResource][]*metav1.PartialObjectMetadata{
		hogGVR:    hogRows,
		targetGVR: targetRows,
	})
	s := h.svc
	for _, gvr := range h.order {
		store := cache.NewStore(cache.MetaNamespaceKeyFunc)
		rows := hogRows
		if gvr == targetGVR {
			rows = targetRows
		}
		for _, r := range rows {
			if err := store.Add(r); err != nil {
				t.Fatal(err)
			}
		}
		s.entries[gvr].informer = &fakeInformer{store: store, synced: true}
	}

	hog, target := s.entries[hogGVR], s.entries[targetGVR]

	// 대상 GVR을 **부트스트랩 전** 상태로 되돌립니다(세대 반납 포함).
	s.snapMu.Lock()
	s.installLeaseLocked(target, nil)
	target.setSnap(&entrySnapshot{index: indexOf(targetRows...)})
	s.snapMu.Unlock()
	target.bootstrapped.Store(false)
	target.dirty.Store(true)

	// 경쟁 GVR이 남은 자리를 전부 차지하도록 상한을 조입니다.
	// **cfg와 budget.max를 함께** 옮깁니다 — 한쪽만 바꾸면 I-B가 무력화됩니다.
	hogBytes := hog.ownerRetained.Load()
	if hogBytes <= 0 {
		t.Fatalf("경쟁 GVR이 아무 바이트도 붙잡고 있지 않습니다")
	}
	setSearchBudget(s, hogBytes+1024)

	// 프로덕션 경로로 돌립니다. 대상은 설치되지 않아야 합니다.
	s.rebuildIndexes(false)
	if target.bootstrapped.Load() {
		t.Fatal("예산이 없는데 부트스트랩 완료로 표시되었습니다")
	}
	got := target.load()
	if got.sindex != nil {
		t.Fatal("거절됐는데 색인이 설치되었습니다")
	}
	if got.searchState != SearchUnavailable || got.searchReason == "" {
		t.Fatalf("명시적 unavailable이 아닙니다: state=%q reason=%q", got.searchState, got.searchReason)
	}
	if got.index == nil {
		t.Fatal("목록 절반까지 게시되지 않았습니다 — 검색 예산이 목록을 죽였습니다")
	}
	s.delta.mu.Lock()
	pendingAfterReject := len(s.delta.queueFor(targetGVR).events)
	s.delta.mu.Unlock()
	if pendingAfterReject != 0 {
		t.Fatalf("비울 수 없는 큐가 %d건 남았습니다", pendingAfterReject)
	}

	// 입력도 가용 용량도 그대로면 **전체 빌드를 반복하지 않아야** 합니다.
	target.dirty.Store(true)
	baseBoot := s.delta.fullBootstraps.Load()
	for i := 0; i < 5; i++ {
		target.dirty.Store(true)
		s.rebuildIndexes(false)
	}
	if got := s.delta.fullBootstraps.Load() - baseBoot; got != 0 {
		t.Fatalf("입력도 용량도 그대로인데 전체 빌드를 %d회 반복했습니다 — 회전입니다", got)
	}
	if target.bootstrapped.Load() {
		t.Fatal("설치되지 않았는데 부트스트랩 완료로 표시되었습니다")
	}

	// 경쟁 GVR이 세대를 놓습니다. **설정 상한은 그대로**입니다 — 바뀐 것은 가용 용량뿐입니다.
	beforeMax := s.budget.limit()
	s.snapMu.Lock()
	s.installLeaseLocked(hog, nil)
	s.snapMu.Unlock()
	if s.budget.limit() != beforeMax {
		t.Fatal("테스트가 설정 상한을 건드렸습니다 — 가용 용량만 달라져야 합니다")
	}
	if got := hog.ownerRetained.Load(); got != 0 {
		t.Fatalf("경쟁 GVR이 놓았는데 소유자 원장에 %d가 남았습니다", got)
	}

	target.dirty.Store(true)
	s.rebuildIndexes(false)
	if got := s.delta.fullBootstraps.Load() - baseBoot; got != 1 {
		t.Fatalf("용량이 풀린 뒤 전체 빌드가 %d회입니다 — 정확히 1회여야 합니다", got)
	}
	if !target.bootstrapped.Load() {
		t.Fatal("용량이 풀렸는데 부트스트랩이 완료되지 않았습니다")
	}
	got = target.load()
	if got.sindex == nil || got.searchState != SearchReady {
		t.Fatalf("설치 뒤 상태가 어긋났습니다: state=%q sindex=%v", got.searchState, got.sindex != nil)
	}
	if st := target.currentStatus(); st.state != StateReady {
		t.Fatalf("설치 뒤 상태가 %s입니다", st.state)
	}
	assertNoNegativeAccounting(t, s, "bootstrap-retry")
}

// setSearchBudget은 cfg와 원장 상한을 **함께** 옮깁니다.
//
// 한쪽만 바꾸면 I-B(GVR별 상한 = cfg/2)와 I-A(원장 상한)가 서로 다른 값을 보고,
// 테스트가 의도치 않게 I-B를 꺼 버립니다.
func setSearchBudget(s *Service, max int64) {
	s.cfg.MaxSearchIndexBytes = max
	s.budget.max.Store(max)
}

/* ── 항목 4: 회수 회로 지속 ─────────────────────────────────────────────── */

// TestRecoveryCircuitSurvivesTicketDisposal — 티켓이 버려져도 백오프와 "같은
// 입력이면 재시도 없음"이 남아야 합니다. 가짜 시계로 폭풍이 없음을 봅니다.
func TestRecoveryCircuitSurvivesTicketDisposal(t *testing.T) {
	now := indexBase
	h := newDeltaHarness(t, metaRow("prod", "payments-api", "uid-1", nil))
	h.svc.cfg.Now = func() time.Time { return now }

	target := recoveryTarget{gvr: h.gvr, namespace: "prod"}
	h.svc.requestResync(h.gvr, "prod", 10)

	// 같은 입력·같은 예산에서 반복 실패시킵니다.
	for i := 0; i < 4; i++ {
		h.svc.delta.mu.Lock()
		q := h.svc.delta.queueFor(h.gvr)
		ticket := &recoveryTicket{gvr: h.gvr, namespace: "prod", epoch: q.staleEpoch, markerSeq: 10}
		h.svc.delta.ticket = ticket
		h.svc.delta.mu.Unlock()
		h.svc.finishRecovery(ticket, recoveryBudgetRejected)

		h.svc.delta.mu.Lock()
		c, _ := h.svc.delta.circuitFor(target)
		attempts, backoff, open := c.attempts, c.backoff, c.open
		h.svc.delta.mu.Unlock()
		if attempts != i+1 {
			t.Fatalf("%d번째: 시도 횟수가 %d입니다 — 티켓이 버려지면서 회로가 사라졌습니다", i, attempts)
		}
		if !open {
			t.Fatalf("%d번째: 예산 거절인데 회로가 열리지 않았습니다", i)
		}
		if backoff < recoveryBackoffMin {
			t.Fatalf("%d번째: 백오프가 %v입니다", i, backoff)
		}
	}

	// 쿨다운이 지나도 입력·예산이 그대로면 다시 잡지 않아야 합니다(폭풍 없음).
	now = now.Add(recoveryBackoffMax * 4)
	h.svc.delta.mu.Lock()
	h.svc.delta.cooldownUntil = time.Time{}
	picked := h.svc.pickRecoveryLocked(now)
	h.svc.delta.mu.Unlock()
	if picked != nil {
		t.Fatal("입력도 예산도 그대로인데 회수를 다시 잡았습니다 — 폭풍이 됩니다")
	}

	// 새 마커(입력 변화)가 생기면 다시 잡아야 합니다.
	h.svc.requestResync(h.gvr, "prod", 99)
	h.svc.delta.mu.Lock()
	picked = h.svc.pickRecoveryLocked(now)
	h.svc.delta.mu.Unlock()
	if picked == nil {
		t.Fatal("입력이 바뀌었는데 회수를 잡지 않았습니다")
	}

	// 예산 상향도 회로를 닫아야 합니다.
	h.svc.delta.mu.Lock()
	c, _ := h.svc.delta.circuitFor(target)
	c.open, c.lastMarker, c.lastMax = true, 99, h.svc.budget.limit()
	c.notBefore = time.Time{}
	h.svc.delta.ticket = nil
	h.svc.delta.mu.Unlock()
	h.svc.budget.max.Store(h.svc.budget.limit() * 2)
	h.svc.delta.mu.Lock()
	picked = h.svc.pickRecoveryLocked(now)
	h.svc.delta.mu.Unlock()
	if picked == nil {
		t.Fatal("예산이 늘었는데 회수를 잡지 않았습니다")
	}
}

// TestCircuitsAreTargetSpecificBoundedAndNonEvicting — 회로는 **대상별**이고,
// 상한이 있으며, 아직 낡은 대상의 열린 회로를 조용히 버리지 않아야 합니다.
//
// 티켓을 직접 주입하지 않습니다 — requestResync/circuitFor/pickRecoveryLocked라는
// 프로덕션 경로만 씁니다.
func TestCircuitsAreTargetSpecificBoundedAndNonEvicting(t *testing.T) {
	const (
		gvrs         = 4
		nsPerGVR     = 600 // 4 × 600 = 2400 > maxCircuits(2048)
		keepNS       = "keep-ns"
		unrelatedNS  = "other-ns"
		relievedRows = 1
	)
	now := indexBase
	rowsByGVR := make(map[schema.GroupVersionResource][]*metav1.PartialObjectMetadata, gvrs)
	for i := 0; i < gvrs; i++ {
		gvr := schema.GroupVersionResource{
			Group: fmt.Sprintf("c%02d.example.com", i), Version: "v1", Resource: "things",
		}
		rowsByGVR[gvr] = []*metav1.PartialObjectMetadata{
			metaRow(keepNS, "row-a", fmt.Sprintf("uid-%02d-a", i), nil),
		}
	}
	h := newMultiHarness(t, rowsByGVR)
	s := h.svc
	s.cfg.Now = func() time.Time { return now }
	gvrA := h.order[0]

	/* ① 대상별 지문: 옆 namespace의 마커가 남의 회로를 닫으면 안 됩니다. */
	s.requestResync(gvrA, keepNS, 10)
	keepTarget := recoveryTarget{gvr: gvrA, namespace: keepNS}
	// 필요했던 바이트를 **지금 용량으로는 감당할 수 없게** 둡니다.
	// 그래야 회로가 "용량이 충분해졌다"로 스스로 닫히지 않고, 이 절이 보려는
	// 마커 대상성만 남습니다.
	unreachable := s.budget.peakLimit() * 4
	s.delta.mu.Lock()
	q := s.delta.queueFor(gvrA)
	keep, _ := s.delta.circuitFor(keepTarget)
	keep.fail(now, q.markerFor(keepTarget), 100, unreachable, s.budget.limit(), s.availableFor(gvrA), true)
	keepNotBefore, keepMarker := keep.notBefore, keep.lastMarker
	s.delta.mu.Unlock()
	if keepMarker != 10 {
		t.Fatalf("대상 지문이 %d입니다 — 그 namespace의 마커(10)여야 합니다", keepMarker)
	}

	// **무관한** namespace가 낡습니다. keep의 회로는 그대로여야 합니다.
	s.requestResync(gvrA, unrelatedNS, 11)
	s.delta.mu.Lock()
	q = s.delta.queueFor(gvrA)
	keepC, _ := s.delta.circuitFor(keepTarget)
	stillClosed := !keepC.allows(
		now.Add(recoveryBackoffMax*4), q.markerFor(keepTarget), s.budget.limit(), s.availableFor(gvrA))
	s.delta.mu.Unlock()
	if !stillClosed {
		t.Fatal("무관한 namespace의 마커가 남의 회로를 닫았습니다 — 재시도 폭풍이 됩니다")
	}

	// **그 대상 자신의** 입력이 바뀌면 열립니다.
	s.requestResync(gvrA, keepNS, 12)
	s.delta.mu.Lock()
	q = s.delta.queueFor(gvrA)
	openedC, _ := s.delta.circuitFor(keepTarget)
	opened := openedC.allows(
		now.Add(recoveryBackoffMax*4), q.markerFor(keepTarget), s.budget.limit(), s.availableFor(gvrA))
	s.delta.mu.Unlock()
	if !opened {
		t.Fatal("대상 자신의 입력이 바뀌었는데 회로가 닫힌 채입니다")
	}

	/* ② 입력 축소가 **선택자에 걸리기 전에** 관찰되어야 합니다. */
	s.delta.mu.Lock()
	c, _ := s.delta.circuitFor(keepTarget)
	// 행 수가 100이던 시점의 실패로 회로를 엽니다. 백오프는 지웁니다 —
	// 시간이 아니라 **입력 축소**만 보게 하려는 것입니다.
	c.fail(now, s.delta.queueFor(gvrA).markerFor(keepTarget), 100, unreachable,
		s.budget.limit(), s.availableFor(gvrA), true)
	c.notBefore = time.Time{}
	span := s.targetRowSpan(keepTarget)
	s.delta.cooldownUntil = time.Time{}
	// 커서를 0에 두어 선택자가 order[0](=gvrA)의 staleNS 첫 항목(=keep-ns)을
	// 결정적으로 보게 합니다.
	s.delta.recoveryRR = 0
	// 프로덕션 선택자를 그대로 돌립니다. keep 회로는 선택자에 걸리기 전에
	// noteRows를 지나며 닫혀 있어야 합니다.
	s.pickRecoveryLocked(now)
	afterC, _ := s.delta.circuitFor(keepTarget)
	afterOpen := afterC.open
	s.delta.ticket = nil
	s.delta.mu.Unlock()
	if span != relievedRows {
		t.Fatalf("namespace 구간이 %d행입니다 — %d행이어야 합니다(유계 메타데이터)", span, relievedRows)
	}
	if afterOpen {
		t.Fatal("원본 행 수가 100→1로 줄었는데 회로가 열린 채입니다 — 축소가 관찰되지 않았습니다")
	}

	/* ③ 상한: 회로가 넘치면 임의 축출이 아니라 GVR 전체로 승급합니다. */
	s.delta.mu.Lock()
	// keep 회로를 다시 열고 백오프를 먼 미래로 밀어 둡니다.
	keep, _ = s.delta.circuitFor(keepTarget)
	keep.open, keep.notBefore = true, keepNotBefore.Add(time.Hour)
	wantNotBefore := keep.notBefore
	// **뒤에서부터** 채웁니다. 상한을 넘기는 순간의 GVR이 gvrA가 되도록 하여
	// keep 회로가 실제로 승급 대상이 되게 만듭니다.
	for i := len(h.order) - 1; i >= 0; i-- {
		for n := 0; n < nsPerGVR; n++ {
			s.delta.circuitFor(recoveryTarget{
				gvr: h.order[i], namespace: fmt.Sprintf("ns-%02d-%04d", i, n),
			})
			if got := len(s.delta.circuits); got > maxCircuits {
				s.delta.mu.Unlock()
				t.Fatalf("회로가 %d개입니다 — 상한 %d를 넘겼습니다", got, maxCircuits)
			}
		}
	}
	circuits := len(s.delta.circuits)
	direct, hasDirect := s.delta.circuits[keepTarget]
	whole, hasWhole := s.delta.circuits[recoveryTarget{gvr: gvrA, whole: true}]
	s.delta.mu.Unlock()

	if circuits > maxCircuits {
		t.Fatalf("회로가 %d개 남았습니다 — 상한 %d", circuits, maxCircuits)
	}
	// 아직 낡은 대상의 열린 회로는 **사라지지 않습니다** — 그대로 남거나
	// GVR 전체 회로가 그 상태를 물려받습니다. 어느 쪽도 아니면 축출된 것입니다.
	switch {
	case hasDirect:
		if !direct.open || direct.notBefore.Before(wantNotBefore) {
			t.Fatalf("열린 회로의 상태가 약해졌습니다: open=%v notBefore=%v", direct.open, direct.notBefore)
		}
	case hasWhole:
		if !whole.open {
			t.Fatal("승급된 GVR 회로가 열림 상태를 물려받지 않았습니다 — 폭풍이 됩니다")
		}
		if whole.notBefore.Before(wantNotBefore) {
			t.Fatalf("승급된 회로의 백오프가 %v로 앞당겨졌습니다 — %v 이후여야 합니다",
				whole.notBefore, wantNotBefore)
		}
	default:
		t.Fatal("아직 낡은 대상의 열린 회로가 흔적 없이 사라졌습니다")
	}

	/* ④ 굶음 없음: 회수 선택이 GVR을 한 바퀴 돌아야 합니다. */
	for i, gvr := range h.order {
		s.requestResync(gvr, fmt.Sprintf("ns-star-%02d", i), uint64(200+i))
	}
	seen := make(map[schema.GroupVersionResource]bool, len(h.order))
	for i := 0; i < len(h.order)*4; i++ {
		s.delta.mu.Lock()
		s.delta.cooldownUntil = time.Time{}
		s.delta.ticket = nil
		got := s.pickRecoveryLocked(now)
		s.delta.mu.Unlock()
		if got != nil {
			seen[got.gvr] = true
		}
	}
	for _, gvr := range h.order {
		if !seen[gvr] {
			t.Fatalf("%s가 한 바퀴 동안 한 번도 선택되지 않았습니다 — 굶습니다", FormatGVR(gvr))
		}
	}
}

/* ── 항목 5: 포화 스케줄러 ──────────────────────────────────────────────── */

// saturatedScheduler는 **진짜 스냅숏·informer를 올린** 64 GVR 서비스입니다.
//
// 이전 하네스는 스냅숏이 없어 flush가 즉시 0을 돌려주었고, 그래서 "한 tick에
// 전부 시도됨"이라는 쉬운 경로만 증명했습니다. 여기서는 GVR마다 실제 색인과
// Store를 올려 flush가 진짜 작업을 하게 만듭니다 — tick 예산이 실제로 소진되고,
// 그때도 커서가 굶는 GVR을 남기지 않아야 합니다.
type saturatedScheduler struct {
	svc      *Service
	order    []schema.GroupVersionResource
	bindings map[schema.GroupVersionResource]*handlerBinding
	stores   map[schema.GroupVersionResource]cache.Store
	// next는 GVR별 이름 카운터입니다. 이름이 단조로워야 enqueue가 합쳐지지 않고
	// 큐가 실제로 자랍니다.
	next map[schema.GroupVersionResource]int
}

func newSaturatedScheduler(tb testing.TB, resources int) *saturatedScheduler {
	tb.Helper()
	order := make([]schema.GroupVersionResource, 0, resources)
	entries := make(map[schema.GroupVersionResource]*resourceEntry, resources)
	stores := make(map[schema.GroupVersionResource]cache.Store, resources)
	disc := &discoverySnapshot{byGVR: map[schema.GroupVersionResource]int{}}

	for i := 0; i < resources; i++ {
		gvr := schema.GroupVersionResource{
			Group: fmt.Sprintf("g%02d.example.com", i), Version: "v1", Resource: "things",
		}
		order = append(order, gvr)
		disc.byGVR[gvr] = len(disc.entries)
		disc.entries = append(disc.entries, discoveryEntry{
			gvr: gvr, kind: "Thing", namespaced: true, served: true,
			verbs: []string{"get", "list", "watch"},
		})
		e := &resourceEntry{gvr: gvr}
		e.setStatus(StateReady, "")
		e.lifecycle, e.generation = 1, 0
		e.tokenPacked.Store(packToken(1, 0))
		e.bootstrapped.Store(true)
		entries[gvr] = e
	}

	s := &Service{
		cfg: Config{
			ClusterID: "prod-seoul", SearchEnabled: true, SearchIncremental: true,
			MaxSearchIndexBytes: DefaultMaxSearchIndexBytes,
			Now:                 func() time.Time { return indexBase },
		},
		order: order, entries: entries,
	}
	s.delta = newDeltaState()
	s.delta.budget = &s.budget
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	s.started.Store(true)
	s.disc.Store(disc)

	bindings := make(map[schema.GroupVersionResource]*handlerBinding, resources)
	for _, gvr := range order {
		e := entries[gvr]
		store := cache.NewStore(cache.MetaNamespaceKeyFunc)
		rows := make([]*metav1.PartialObjectMetadata, 0, 64)
		for i := 0; i < 64; i++ {
			row := metaRow("prod", fmt.Sprintf("row-%04d", i), fmt.Sprintf("uid-%s-%04d", gvr.Group, i), nil)
			if err := store.Add(row); err != nil {
				tb.Fatal(err)
			}
			rows = append(rows, row)
		}
		stores[gvr] = store
		e.informer = &fakeInformer{store: store, synced: true}

		index := indexOf(rows...)
		r := buildSearchIndex(index, "Thing", true, hugeBudget, hugeBudget)
		if r.state != SearchReady {
			tb.Fatalf("%s 부트스트랩 실패: %s", FormatGVR(gvr), r.state)
		}
		snap := &entrySnapshot{
			index: index, sindex: r.index, searchState: SearchReady,
			indexVer: 1, searchVer: 1,
		}
		e.setSnap(snap)
		s.snapMu.Lock()
		admitted := s.publishLeaseLocked(e, snap)
		s.snapMu.Unlock()
		if !admitted {
			tb.Fatalf("%s 세대가 예산 승인을 받지 못했습니다", FormatGVR(gvr))
		}
		bindings[gvr] = &handlerBinding{entry: e, packed: packToken(1, 0), namespaced: true}
	}
	return &saturatedScheduler{
		svc: s, order: order, bindings: bindings, stores: stores,
		next: make(map[schema.GroupVersionResource]int, resources),
	}
}

// saturate는 GVR마다 큐를 2*maxBatchPerResource건까지 **채웁니다**(멱등).
//
// 한 tick이 GVR 하나에서 최대 maxBatchPerResource건만 가져가므로, 이 상태를
// 유지하면 매 tick이 언제나 포화입니다. 이미 들어 있는 만큼만 보충하므로
// 반복해서 불러도 큐 상한(maxPendingPerResource)을 넘겨 드롭을 만들지 않습니다.
func (h *saturatedScheduler) saturate(tb testing.TB) {
	tb.Helper()
	const (
		target = maxBatchPerResource * 2
		// nameSpace는 이름 재사용 주기입니다. 큐에 동시에 살아 있는 키(<= target)보다
		// 훨씬 크므로, 재사용된 이름이 **아직 큐에 있는 키와 겹치지 않습니다.**
		// 겹치면 enqueue가 합쳐져 큐가 자라지 않고, 포화가 만들어지지 않습니다.
		nameSpace = target * 4
	)
	for _, gvr := range h.order {
		store := h.stores[gvr]
		b := h.bindings[gvr]
		for have := h.pending(gvr); have < target; have++ {
			n := h.next[gvr]
			h.next[gvr] = n + 1
			name := fmt.Sprintf("row-%06d", n%nameSpace)
			if err := store.Add(metaRow("prod", name,
				fmt.Sprintf("uid-%s-%06d", gvr.Group, n%nameSpace), nil)); err != nil {
				tb.Fatal(err)
			}
			h.svc.enqueueKey(b, "prod", name)
		}
	}
}

func (h *saturatedScheduler) pending(gvr schema.GroupVersionResource) int {
	h.svc.delta.mu.Lock()
	defer h.svc.delta.mu.Unlock()
	return len(h.svc.delta.queueFor(gvr).events)
}

// TestSaturatedSchedulerServesEveryResource — 진짜 스냅숏·informer를 올린 64 GVR이
// 전부 포화일 때, **실제로 줄어든 GVR만** 커서가 지나가고 8 tick 안에 모두 한 번씩
// 서비스되어야 합니다.
//
// 100ms 합치기 창 계약은 그대로입니다 — tick 하나가 쓰는 최대 작업량
// (maxBatchEvents, GVR당 maxBatchPerResource)과 커서 전진 규칙만 봅니다.
func TestSaturatedSchedulerServesEveryResource(t *testing.T) {
	const resources = 64
	perTick := maxBatchEvents / maxBatchPerResource // tick 하나가 실제로 비우는 GVR 수
	maxTicks := (resources + perTick - 1) / perTick // 한 바퀴에 필요한 tick 수 = 8
	h := newSaturatedScheduler(t, resources)
	h.saturate(t)

	before := make(map[schema.GroupVersionResource]int, resources)
	for _, gvr := range h.order {
		before[gvr] = h.pending(gvr)
		if before[gvr] != maxBatchPerResource*2 {
			t.Fatalf("%s 큐가 %d건입니다 — 2*maxBatchPerResource여야 합니다", FormatGVR(gvr), before[gvr])
		}
	}

	baseList := h.svc.delta.storeListCalls.Load()
	baseBoot := h.svc.delta.fullBootstraps.Load()
	baseFull := h.svc.delta.fullRecoveries.Load()

	servedAt := make(map[schema.GroupVersionResource]int, resources)
	for tick := 0; tick < maxTicks; tick++ {
		h.svc.delta.mu.Lock()
		start := h.svc.delta.rr % resources
		h.svc.delta.mu.Unlock()

		h.svc.deltaTick(context.Background())

		// 이 tick에 **실제로 줄어든** GVR만 서비스된 것입니다.
		advanced := 0
		for _, gvr := range h.order {
			now := h.pending(gvr)
			if now < before[gvr] {
				if _, seen := servedAt[gvr]; !seen {
					servedAt[gvr] = tick
				}
				advanced++
				before[gvr] = now
			}
		}
		if advanced == 0 {
			t.Fatalf("%d번째 tick이 아무 GVR도 처리하지 못했습니다", tick)
		}
		if advanced > perTick {
			t.Fatalf("%d번째 tick이 %d개 GVR을 처리했습니다 — tick 예산 상한은 %d개입니다",
				tick, advanced, perTick)
		}
		// 커서는 처리한 만큼만 앞으로 갑니다. 처리하지 않은 GVR을 건너뛰면 굶습니다.
		h.svc.delta.mu.Lock()
		end := h.svc.delta.rr % resources
		h.svc.delta.mu.Unlock()
		moved := end - start
		if moved <= 0 {
			moved += resources
		}
		if moved != advanced {
			t.Fatalf("%d번째 tick: 커서가 %d칸 갔는데 처리한 GVR은 %d개입니다", tick, moved, advanced)
		}
	}

	for _, gvr := range h.order {
		at, ok := servedAt[gvr]
		if !ok {
			t.Fatalf("%s가 %d tick 안에 한 번도 서비스되지 않았습니다", FormatGVR(gvr), maxTicks)
		}
		if at >= maxTicks {
			t.Fatalf("%s가 %d번째 tick에야 서비스되었습니다 — 상한 %d", FormatGVR(gvr), at, maxTicks)
		}
	}

	// 델타 경로는 목록 재조회·전체 빌드·전체 회수를 쓰지 않습니다.
	if got := h.svc.delta.storeListCalls.Load() - baseList; got != 0 {
		t.Fatalf("델타 tick이 Store.List를 %d회 불렀습니다", got)
	}
	if got := h.svc.delta.fullBootstraps.Load() - baseBoot; got != 0 {
		t.Fatalf("델타 tick이 전체 빌드를 %d회 했습니다", got)
	}
	if got := h.svc.delta.fullRecoveries.Load() - baseFull; got != 0 {
		t.Fatalf("델타 tick이 전체 회수를 %d회 했습니다", got)
	}
	assertNoNegativeAccounting(t, h.svc, "saturated-scheduler")
}
