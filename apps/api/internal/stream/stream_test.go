package stream

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

func TestHubReturnsErrorWhenInstanceEntropyUnavailable(t *testing.T) {
	if _, err := newWithRandom(Config{}, nil, errorReader{}); err == nil {
		t.Fatal("entropy failure did not fail hub construction")
	}
}

func TestHubConfigCapsAreValidatedBeforeAllocation(t *testing.T) {
	tests := []Config{
		{RingSize: MaxRingSize + 1},
		{MaxConnections: MaxConnections + 1},
		{SubscriberBuffer: MaxSubscriberBuffer + 1},
		{MaxConnections: MaxConnections, SubscriberBuffer: 65},
	}
	for _, cfg := range tests {
		if _, err := newWithRandom(cfg, nil, errorReader{}); err == nil || strings.Contains(err.Error(), "entropy") {
			t.Fatalf("config was not rejected before entropy/allocation: cfg=%+v err=%v", cfg, err)
		}
	}
	if err := validateConfig(Config{RingSize: MaxRingSize, MaxConnections: MaxConnections, SubscriberBuffer: 64, MaxClusters: 1, MaxRetainedEvents: MaxRingSize}); err != nil {
		t.Fatalf("exact cap boundary rejected: %v", err)
	}
}

func TestHubRejectsInvalidAndDuplicateConfiguredClusters(t *testing.T) {
	for _, ids := range [][]string{{"A"}, {"bad/cluster"}, {"a", "a"}} {
		if _, err := New(Config{ClusterIDs: ids}, nil); err == nil {
			t.Fatalf("clusters %q were accepted", ids)
		}
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

const testCluster = "prod-seoul"

func newTestHub(t *testing.T, cfg Config, obs Observer) *Hub {
	t.Helper()
	h, err := New(cfg, obs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func podEnv(ns string) contract.EventEnvelope {
	return contract.EventEnvelope{
		Kind: contract.StreamKindPod, Action: contract.StreamActionUpdated,
		ClusterID: testCluster, Namespace: ns,
		Entity: &contract.EntityRef{ClusterID: testCluster, Namespace: ns},
	}
}

func allFilter() Filter { return Filter{ClusterID: testCluster, All: true} }

func nsFilter(ns ...string) Filter { return Filter{ClusterID: testCluster, Namespaces: ns} }

func testCursor(h *Hub, subject string, filter Filter, seq uint64) string {
	return h.eventID(h.cursorBinding(sha256.Sum256([]byte(subject)), filter), seq)
}

func TestPublishAssignsExactInstanceIDAndValidObservedAt(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 20, 30, 123, time.UTC)
	h := newTestHub(t, Config{Now: func() time.Time { return now }}, nil)
	if got := h.InstanceID(); len(got) != 24 {
		t.Fatalf("instance ID length=%d want 24", len(got))
	}
	sub, err := h.Subscribe("u", allFilter(), "")
	if err != nil {
		t.Fatal(err)
	}
	env := podEnv("payments")
	env.ObservedAt = "not-rfc3339"
	h.Publish(env)
	got := recv(t, sub.Events()).Envelope
	if _, err := time.Parse(time.RFC3339Nano, got.ObservedAt); err != nil {
		t.Fatalf("invalid observedAt %q: %v", got.ObservedAt, err)
	}
	if got.ObservedAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("observedAt=%q want %q", got.ObservedAt, now.Format(time.RFC3339Nano))
	}
}

// recv는 채널에서 하나를 기다립니다. 스트림 버그가 테스트를 영원히 세우지 않게 합니다.
func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("채널이 예상보다 먼저 닫혔습니다")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("이벤트 수신 타임아웃")
	}
	return Event{}
}

func assertNoEvent(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("받지 않아야 할 이벤트를 받았습니다: %+v", ev.Envelope)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

/* ── Scope 필터 ─────────────────────────────────────────────────────────── */

func TestScopeFilterIsolatesNamespaces(t *testing.T) {
	h := newTestHub(t, Config{}, nil)

	admin, err := h.Subscribe("admin", allFilter(), "")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := h.Subscribe("viewer", nsFilter("payments"), "")
	if err != nil {
		t.Fatal(err)
	}

	h.Publish(podEnv("payments"))
	h.Publish(podEnv("media"))
	// namespace 없는 클러스터 범위 신호 — All에게만 갑니다.
	h.Publish(contract.EventEnvelope{Kind: contract.StreamKindWorkload, Action: contract.StreamActionUpdated, ClusterID: testCluster})
	h.Publish(podEnv("payments")) // 마커 — viewer가 media를 건너뛰었음을 증명합니다.

	if got := recv(t, admin.Events()).Envelope.Namespace; got != "payments" {
		t.Fatalf("admin 1번째: got ns %q", got)
	}
	if got := recv(t, admin.Events()).Envelope.Namespace; got != "media" {
		t.Fatalf("admin 2번째: got ns %q", got)
	}
	if got := recv(t, admin.Events()).Envelope.Namespace; got != "" {
		t.Fatalf("admin 3번째: got ns %q, want 클러스터 범위", got)
	}

	if got := recv(t, viewer.Events()).Envelope.Namespace; got != "payments" {
		t.Fatalf("viewer 1번째: got ns %q", got)
	}
	// viewer의 다음 이벤트는 media·클러스터 범위를 건너뛴 마커여야 합니다.
	next := recv(t, viewer.Events()).Envelope
	if next.Namespace != "payments" {
		t.Fatalf("viewer에게 범위 밖 봉투가 갔습니다: %+v", next)
	}
}

func TestFilterRejectsOtherCluster(t *testing.T) {
	f := Filter{ClusterID: "other", All: true}
	if f.allows(podEnv("payments")) {
		t.Fatal("다른 클러스터의 봉투가 통과했습니다")
	}
}

func TestResetReachesNamespaceScopedSubscriber(t *testing.T) {
	f := nsFilter("payments")
	reset := contract.EventEnvelope{Kind: contract.StreamKindReset, Action: contract.StreamActionReset, ClusterID: testCluster}
	if !f.allows(reset) {
		t.Fatal("reset은 클러스터 접근 권한만 있으면 받아야 합니다")
	}
}

/* ── Last-Event-ID · 재생 · reset ───────────────────────────────────────── */

func lastID(ev Event) string { return ev.Envelope.ID }

func TestReplayExactOrderNoGapNoDuplicate(t *testing.T) {
	h := newTestHub(t, Config{}, nil)

	first, err := h.Subscribe("u", allFilter(), "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		h.Publish(podEnv("payments"))
	}
	var last Event
	for i := 0; i < 3; i++ {
		last = recv(t, first.Events())
	}
	first.Close()

	// 끊긴 사이에 발생한 이벤트들
	h.Publish(podEnv("media"))
	h.Publish(podEnv("payments"))

	second, err := h.Subscribe("u", allFilter(), lastID(last))
	if err != nil {
		t.Fatal(err)
	}
	replay := second.Replay()
	if len(replay) != 2 {
		t.Fatalf("재생 개수: got %d want 2", len(replay))
	}
	if replay[0].Envelope.Namespace != "media" || replay[1].Envelope.Namespace != "payments" {
		t.Fatalf("재생 순서가 틀렸습니다: %s, %s", replay[0].Envelope.Namespace, replay[1].Envelope.Namespace)
	}
	// 재생 직후의 라이브 이벤트는 중복 없이 이어져야 합니다.
	h.Publish(podEnv("payments"))
	live := recv(t, second.Events())
	if live.Envelope.ID == replay[1].Envelope.ID {
		t.Fatal("재생과 라이브가 중복되었습니다")
	}
	// ID 시퀀스 연속성 — 틈이 없어야 합니다.
	prev := replay[1].Envelope.ID
	if seqOf(t, live.Envelope.ID) != seqOf(t, prev)+1 {
		t.Fatalf("재생→라이브 사이에 틈: %s → %s", prev, live.Envelope.ID)
	}
}

func seqOf(t *testing.T, id string) uint64 {
	t.Helper()
	var inst string
	var seq uint64
	if _, err := fmt.Sscanf(id, "%16s-%d", &inst, &seq); err != nil {
		// Sscanf의 %16s는 '-'를 먹으므로 직접 자릅니다.
		for i := 0; i < len(id); i++ {
			if id[i] == '-' {
				n, err := strconv.ParseUint(id[i+1:], 10, 64)
				if err != nil {
					t.Fatalf("ID 파싱 실패: %s", id)
				}
				return n
			}
		}
		t.Fatalf("ID 형식이 아닙니다: %s", id)
	}
	return seq
}

func TestReplayWithCurrentIDIsEmpty(t *testing.T) {
	h := newTestHub(t, Config{}, nil)
	h.Publish(podEnv("payments"))
	sub, err := h.Subscribe("u", allFilter(), testCursor(h, "u", allFilter(), 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Replay()) != 0 {
		t.Fatalf("놓친 것이 없는데 재생이 있습니다: %d", len(sub.Replay()))
	}
}

func TestResetOnForeignStaleFutureID(t *testing.T) {
	h := newTestHub(t, Config{RingSize: 4}, nil)
	for i := 0; i < 10; i++ {
		h.Publish(podEnv("payments"))
	}

	cases := map[string]string{
		"foreign": "deadbeefdeadbeefdeadbeef.0000000000000000-3",
		"stale":   testCursor(h, "u-stale", allFilter(), 1),
		"future":  testCursor(h, "u-future", allFilter(), 999),
	}
	if cases["foreign"] == testCursor(h, "u-foreign", allFilter(), 3) {
		t.Skip("난수 인스턴스 충돌 — 재실행하세요")
	}
	for name, id := range cases {
		sub, err := h.Subscribe("u-"+name, allFilter(), id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		replay := sub.Replay()
		if len(replay) != 1 || replay[0].Envelope.Kind != contract.StreamKindReset {
			t.Fatalf("%s: reset 하나를 기대했으나 %d개 (%+v)", name, len(replay), replay)
		}
		if replay[0].Envelope.Action != contract.StreamActionReset {
			t.Fatalf("%s: action=%s", name, replay[0].Envelope.Action)
		}
		sub.Close()
	}
}

func TestClusterPartitionFloodCursorBindingAndDynamicValidation(t *testing.T) {
	cfg := Config{RingSize: 4, SubscriberBuffer: 8, ClusterIDs: []string{"a", "b"}, MaxClusters: 2, MaxRetainedEvents: 8}
	h := newTestHub(t, cfg, nil)
	bFilter := Filter{ClusterID: "b", All: true}
	b, err := h.Subscribe("viewer", bFilter, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		e := podEnv("ns")
		e.ClusterID, e.Entity.ClusterID = "b", "b"
		h.Publish(e)
	}
	bFirst := recv(t, b.Events())
	_ = recv(t, b.Events())
	b.Close()

	for i := 0; i < 100; i++ {
		e := podEnv("noisy")
		e.ClusterID, e.Entity.ClusterID = "a", "a"
		h.Publish(e)
	}
	reconnected, err := h.Subscribe("viewer", bFilter, bFirst.Envelope.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replay := reconnected.Replay(); len(replay) != 1 || replay[0].Envelope.ClusterID != "b" || replay[0].Envelope.Kind == contract.StreamKindReset {
		t.Fatalf("cluster A evicted B replay: %+v", replay)
	}
	reconnected.Close()

	bindings := []struct {
		subject string
		filter  Filter
	}{
		{"other", bFilter},
		{"viewer", Filter{ClusterID: "b", Namespaces: []string{"other"}}},
		{"viewer", Filter{ClusterID: "a", All: true}},
	}
	for _, tc := range bindings {
		if _, err := h.Subscribe(tc.subject, tc.filter, bFirst.Envelope.ID); err != ErrBadLastEventID {
			t.Fatalf("cursor binding subject=%q filter=%+v: %v", tc.subject, tc.filter, err)
		}
	}

	beforeConnections, beforeRetained := h.Stats()
	beforeSlots := h.allocatedSlots
	invalid := podEnv("ns")
	invalid.ClusterID = "invalid/cluster"
	h.Publish(invalid)
	unknown := podEnv("ns")
	unknown.ClusterID = "c"
	h.Publish(unknown)
	if _, err := h.Subscribe("viewer", Filter{ClusterID: "invalid/cluster", All: true}, ""); err == nil {
		t.Fatal("invalid dynamic subscription was accepted")
	}
	afterConnections, afterRetained := h.Stats()
	if beforeConnections != afterConnections || beforeRetained != afterRetained || beforeSlots != h.allocatedSlots {
		t.Fatalf("rejected cluster changed resources: conns %d/%d retained %d/%d slots %d/%d", beforeConnections, afterConnections, beforeRetained, afterRetained, beforeSlots, h.allocatedSlots)
	}
	if afterRetained > cfg.MaxRetainedEvents {
		t.Fatalf("global retention exceeded: %d > %d", afterRetained, cfg.MaxRetainedEvents)
	}

	dynamic := newTestHub(t, Config{RingSize: 4, MaxClusters: 2, MaxRetainedEvents: 8}, nil)
	dynamic.Publish(invalid)
	if _, err := dynamic.Subscribe("viewer", Filter{ClusterID: invalid.ClusterID, All: true}, ""); err == nil {
		t.Fatal("allowlist-empty hub accepted invalid cluster")
	}
	if connections, retained := dynamic.Stats(); connections != 0 || retained != 0 || dynamic.allocatedSlots != 0 {
		t.Fatalf("allowlist-empty rejection allocated state: connections=%d retained=%d slots=%d", connections, retained, dynamic.allocatedSlots)
	}
}

func TestForeignCursorOnEmptyClusterResetsAtZeroThenNextIsOne(t *testing.T) {
	h := newTestHub(t, Config{ClusterIDs: []string{"a"}, MaxClusters: 1}, nil)
	f := Filter{ClusterID: "a", All: true}
	sub, err := h.Subscribe("u", f, "deadbeefdeadbeefdeadbeef.0000000000000000-9")
	if err != nil {
		t.Fatal(err)
	}
	if got := sub.Replay(); len(got) != 1 || seqOf(t, got[0].Envelope.ID) != 0 || got[0].Envelope.Kind != contract.StreamKindReset {
		t.Fatalf("empty foreign replay=%+v", got)
	}
	e := podEnv("ns")
	e.ClusterID, e.Entity.ClusterID = "a", "a"
	h.Publish(e)
	if got := recv(t, sub.Events()); seqOf(t, got.Envelope.ID) != 1 {
		t.Fatalf("first live sequence=%d", seqOf(t, got.Envelope.ID))
	}
}

func TestSlowNoisyClusterSubscriberDoesNotDelayOtherCluster(t *testing.T) {
	h := newTestHub(t, Config{RingSize: 8, SubscriberBuffer: 1, ClusterIDs: []string{"a", "b"}, MaxClusters: 2, MaxRetainedEvents: 16}, nil)
	slow, err := h.Subscribe("slow", Filter{ClusterID: "a", All: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	fast, err := h.Subscribe("fast", Filter{ClusterID: "b", All: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		e := podEnv("ns")
		e.ClusterID, e.Entity.ClusterID = "a", "a"
		h.Publish(e)
	}
	start := time.Now()
	e := podEnv("ns")
	e.ClusterID, e.Entity.ClusterID = "b", "b"
	h.Publish(e)
	if got := recv(t, fast.Events()); got.Envelope.ClusterID != "b" {
		t.Fatalf("fast subscriber got %+v", got.Envelope)
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("cluster B delivery delayed %s", elapsed)
	}
	for range slow.Events() {
	}
}

func TestTwoReplicaRingsHaveEquivalentEventsAndIndependentCursors(t *testing.T) {
	now := time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC)
	cfg := Config{RingSize: 8, SubscriberBuffer: 8, ClusterIDs: []string{"a"}, MaxClusters: 1, Now: func() time.Time { return now }}
	h1, h2 := newTestHub(t, cfg, nil), newTestHub(t, cfg, nil)
	f := Filter{ClusterID: "a", All: true}
	s1, _ := h1.Subscribe("u", f, "")
	s2, _ := h2.Subscribe("u", f, "")
	frames := []*v1.WatchFrame{
		{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: now.UnixMilli()},
		{ClusterId: "a", Epoch: 1, Seq: 1, Type: v1.WatchFrameType_WATCH_DELTA, ObservedUnixMs: now.UnixMilli(), Change: &v1.CatalogChange{Epoch: 1, Seq: 1, Action: v1.CatalogAction_CATALOG_CREATED, Resource: &v1.CatalogResource{Kind: v1.KindPod, Uid: "p", Namespace: "ns", Name: "p"}}},
	}
	for _, frame := range frames {
		h1.PublishWatchFrame(frame)
		h2.PublishWatchFrame(frame)
	}
	for range frames {
		one, two := recv(t, s1.Events()), recv(t, s2.Events())
		if one.Envelope.ID == two.Envelope.ID {
			t.Fatal("replicas unexpectedly shared a cursor namespace")
		}
		one.Envelope.ID, two.Envelope.ID = "", ""
		if !reflect.DeepEqual(one.Envelope, two.Envelope) {
			t.Fatalf("replica event mismatch: %+v / %+v", one.Envelope, two.Envelope)
		}
	}
	foreign, err := h2.Subscribe("u", f, testCursor(h1, "u", f, 1))
	if err != nil || len(foreign.Replay()) != 1 || foreign.Replay()[0].Envelope.Kind != contract.StreamKindReset {
		t.Fatalf("foreign replica cursor did not reset: replay=%+v err=%v", foreign.Replay(), err)
	}
}

func TestMalformedLastEventIDFailsBeforeAllocation(t *testing.T) {
	h := newTestHub(t, Config{}, nil)
	long := make([]byte, contract.MaxStreamEventIDLen+1)
	for i := range long {
		long[i] = 'a'
	}
	bad := []string{
		"garbage", "-5", "a-", "ABCDEF0123456789-1", // 대문자 hex 거부
		"deadbeefdeadbeefdeadbeefdeadbeef-", "deadbeefdeadbeefdeadbeefdeadbeef-x", "deadbeef-1", // 길이 다른 인스턴스
		"deadbeefdeadbeefdeadbeefdeadbeef-111111111111111111111", // 시퀀스 21자리
		string(long),
	}
	for _, id := range bad {
		if _, err := h.Subscribe("u", allFilter(), id); err != ErrBadLastEventID {
			t.Fatalf("%q: got %v want ErrBadLastEventID", id, err)
		}
	}
	if conns, _ := h.Stats(); conns != 0 {
		t.Fatalf("거절된 구독이 자원을 남겼습니다: %d", conns)
	}
}

/* ── 상한 · backpressure ────────────────────────────────────────────────── */

func TestConnectionCaps(t *testing.T) {
	h := newTestHub(t, Config{MaxConnections: 2, MaxPerSubject: 1}, nil)

	a, err := h.Subscribe("kim", allFilter(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// 같은 주체 두 번째 → 주체 상한
	if _, err := h.Subscribe("kim", allFilter(), ""); err != ErrCapacity {
		t.Fatalf("주체 상한: got %v", err)
	}
	b, err := h.Subscribe("lee", allFilter(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// 전역 상한
	if _, err := h.Subscribe("park", allFilter(), ""); err != ErrCapacity {
		t.Fatalf("전역 상한: got %v", err)
	}
	// 하나 닫으면 다시 들어옵니다.
	b.Close()
	c, err := h.Subscribe("park", allFilter(), "")
	if err != nil {
		t.Fatalf("해제 후 재구독: %v", err)
	}
	c.Close()
}

func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	h := newTestHub(t, Config{SubscriberBuffer: 2}, nil)
	slow, err := h.Subscribe("slow", allFilter(), "")
	if err != nil {
		t.Fatal(err)
	}
	fast, err := h.Subscribe("fast", allFilter(), "")
	if err != nil {
		t.Fatal(err)
	}
	// slow는 소비하지 않습니다. 버퍼(2)를 넘는 발행은 논블로킹으로 slow를 끊어야 합니다.
	// fast는 발행 직후마다 소비해 최대 1개만 대기하게 둡니다 — 별도 드레인
	// 고루틴은 발행 루프를 못 따라가 fast까지 끊기는 경합이 있습니다.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			h.Publish(podEnv("payments"))
			select {
			case <-fast.Events():
			case <-time.After(time.Second):
				return // Publish가 막혔거나 fast가 끊겼습니다 — 아래 검증이 잡습니다.
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("느린 구독자가 Publish를 막았습니다")
	}

	// slow의 채널은 닫혀야 하고(버퍼에 남은 것 소비 후), 구독 수는 fast 하나여야 합니다.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-slow.Events():
			if !ok {
				goto closed
			}
		case <-deadline:
			t.Fatal("느린 구독자 채널이 닫히지 않았습니다")
		}
	}
closed:
	if conns, _ := h.Stats(); conns != 1 {
		t.Fatalf("구독 정리 실패: got %d want 1", conns)
	}
	fast.Close()
	if conns, _ := h.Stats(); conns != 0 {
		t.Fatalf("구독이 0으로 돌아오지 않았습니다: %d", conns)
	}
}

func TestCloseShutsDownSubscribersAndRejectsNew(t *testing.T) {
	h := newTestHub(t, Config{}, nil)
	sub, err := h.Subscribe("u", allFilter(), "")
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	if _, ok := <-sub.Events(); ok {
		t.Fatal("Close 후에도 채널이 열려 있습니다")
	}
	if _, err := h.Subscribe("u", allFilter(), ""); err != ErrClosed {
		t.Fatalf("Close 후 Subscribe error=%v want ErrClosed", err)
	}
	h.Publish(podEnv("payments")) // panic 없이 무시되어야 합니다.
	if conns, _ := h.Stats(); conns != 0 {
		t.Fatalf("Close 후 구독 수: %d", conns)
	}
	sub.Close() // 중복 해제도 안전해야 합니다.
	h.Close()   // 중복 Close도 안전해야 합니다.
}

// TestConcurrentPublishSubscribeClose — -race로 구독·발행·해제·Close의 경합을 봅니다.
func TestConcurrentPublishSubscribeClose(t *testing.T) {
	h := newTestHub(t, Config{RingSize: 64, SubscriberBuffer: 8, MaxConnections: 128, MaxPerSubject: 128}, NewMetrics())
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					h.Publish(podEnv("payments"))
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				sub, err := h.Subscribe(fmt.Sprintf("u%d", n), allFilter(), "")
				if err != nil {
					continue
				}
				for k := 0; k < 5; k++ {
					select {
					case <-sub.Events():
					case <-time.After(time.Millisecond):
					}
				}
				sub.Close()
			}
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	h.Close()
	if conns, _ := h.Stats(); conns != 0 {
		t.Fatalf("경합 후 구독 잔존: %d", conns)
	}
}

/* ── 벤치마크 ───────────────────────────────────────────────────────────── */

// BenchmarkPublish는 구독자 수별 발행 비용입니다. 구독자는 각자 소비하므로
// 측정값은 팬아웃(O(n)) 경로 자체입니다.
func BenchmarkPublish(b *testing.B) {
	for _, n := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("subs=%d", n), func(b *testing.B) {
			h, err := New(Config{RingSize: 1024, SubscriberBuffer: 256, MaxConnections: n + 1, MaxPerSubject: n + 1}, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer h.Close()
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				sub, err := h.Subscribe(fmt.Sprintf("u%d", i), allFilter(), "")
				if err != nil {
					b.Fatal(err)
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range sub.Events() {
					}
				}()
			}
			env := podEnv("payments")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h.Publish(env)
			}
			b.StopTimer()
			h.Close()
			wg.Wait()
		})
	}
}

// BenchmarkReplayFullRing은 최대 링 재생 비용입니다. 링이 구조적 상한임을
// 함께 확인합니다 — 발행 수와 무관하게 보존은 RingSize를 넘지 않습니다.
func BenchmarkReplayFullRing(b *testing.B) {
	const ring = 1024
	h, err := New(Config{RingSize: ring, SubscriberBuffer: ring, MaxConnections: 4, MaxPerSubject: 4}, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()
	env := podEnv("payments")
	for i := 0; i < ring*3; i++ { // 링 크기의 3배를 발행해도
		h.Publish(env)
	}
	if _, retained := h.Stats(); retained != ring {
		b.Fatalf("보존 이벤트가 링 크기를 넘었습니다: %d", retained)
	}
	first := testCursor(h, "u", allFilter(), uint64(ring*3-ring+1)) // 링의 가장 오래된 항목
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sub, err := h.Subscribe("u", allFilter(), first)
		if err != nil {
			b.Fatal(err)
		}
		if len(sub.Replay()) != ring-1 {
			b.Fatalf("재생 개수: %d", len(sub.Replay()))
		}
		sub.Close()
	}
}
