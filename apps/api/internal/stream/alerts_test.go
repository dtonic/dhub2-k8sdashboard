package stream

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

type staticAlerts struct{ result datasource.AlertResult }

func (s staticAlerts) List(context.Context, datasource.AlertQuery) (datasource.AlertResult, error) {
	return s.result, nil
}

// scriptedAlerts는 호출마다 정해진 결과를 돌려주는 가짜 알림 소스입니다.
type scriptedAlerts struct {
	results []func() (datasource.AlertResult, error)
	calls   int
}

func (s *scriptedAlerts) List(context.Context, datasource.AlertQuery) (datasource.AlertResult, error) {
	if s.calls >= len(s.results) {
		return datasource.AlertResult{}, errors.New("스크립트 소진")
	}
	r := s.results[s.calls]
	s.calls++
	return r()
}

func alert(id, ns string) contract.AlertInstance {
	return contract.AlertInstance{
		ID: id, Name: "HighErrorRate", Severity: "critical", Status: "firing",
		Labels: map[string]string{"namespace": ns},
		Entity: &contract.EntityRef{ClusterID: testCluster, Namespace: ns, PodUID: "pod-" + id},
	}
}

func fixed(firing, resolved []contract.AlertInstance) func() (datasource.AlertResult, error) {
	return func() (datasource.AlertResult, error) {
		return datasource.AlertResult{Firing: firing, Resolved: resolved}, nil
	}
}

func TestAlertPollerIgnoresIndependentHistoryError(t *testing.T) {
	a := contract.AlertInstance{ID: "current", Status: "firing"}
	src := &scriptedAlerts{results: []func() (datasource.AlertResult, error){func() (datasource.AlertResult, error) {
		return datasource.AlertResult{Firing: []contract.AlertInstance{a}, HistoryErr: datasource.ErrAlertHistoryNotConfigured}, nil
	}}}
	p, _, _ := newAlertFixture(t, src, 10)
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatalf("current snapshot must not fail with unavailable history: %v", err)
	}
}

func TestAlertPollOnceReadsTrustedNamespacesExactlyOnce(t *testing.T) {
	a := alert("a1", "payments")
	src := &scriptedAlerts{results: []func() (datasource.AlertResult, error){fixed([]contract.AlertInstance{a}, nil)}}
	calls := 0
	p := NewAlertPoller(AlertPollerConfig{ClusterID: testCluster, MaxAlerts: 10, TrustedNamespaces: func() map[string]string {
		calls++
		return map[string]string{"pod:pod-a1": "payments"}
	}}, src, newTestHub(t, Config{}, nil), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err := p.PollOnce(context.Background()); err != nil || calls != 1 {
		t.Fatalf("PollOnce trusted namespace calls=%d err=%v", calls, err)
	}
}

func BenchmarkAlertPollOnce2000With100kTrustedCatalog(b *testing.B) {
	alerts := make([]contract.AlertInstance, 2000)
	for i := range alerts {
		id := strconv.Itoa(i)
		alerts[i] = contract.AlertInstance{ID: "a-" + id, Status: "firing", Entity: &contract.EntityRef{ClusterID: testCluster, Namespace: "ns", PodUID: "pod-" + id}}
	}
	p := NewAlertPoller(AlertPollerConfig{ClusterID: testCluster, MaxAlerts: 2000, TrustedNamespaces: func() map[string]string {
		trusted := make(map[string]string, 100000)
		for i := 0; i < 100000; i++ {
			trusted["pod-"+strconv.Itoa(i)] = "ns"
		}
		return trusted
	}}, staticAlerts{result: datasource.AlertResult{Firing: alerts}}, nil, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.PollOnce(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

type pollPerformanceSample struct {
	Latency      time.Duration
	TotalAlloc   uint64
	CatalogCalls int
}

func validatePollPerformance(got, limit pollPerformanceSample) error {
	if got.Latency > limit.Latency {
		return errors.New("latency budget")
	}
	if got.TotalAlloc > limit.TotalAlloc {
		return errors.New("allocation budget")
	}
	if got.CatalogCalls > limit.CatalogCalls {
		return errors.New("catalog call budget")
	}
	return nil
}

func TestAlertPollOncePerformanceBudgetsAndIndependentMutations(t *testing.T) {
	limit := pollPerformanceSample{Latency: 100 * time.Millisecond, TotalAlloc: 32 << 20, CatalogCalls: 1}
	valid := pollPerformanceSample{Latency: time.Millisecond, TotalAlloc: 1, CatalogCalls: 1}
	for name, mutate := range map[string]func(*pollPerformanceSample){
		"latency":      func(s *pollPerformanceSample) { s.Latency = limit.Latency + 1 },
		"allocation":   func(s *pollPerformanceSample) { s.TotalAlloc = limit.TotalAlloc + 1 },
		"catalog-call": func(s *pollPerformanceSample) { s.CatalogCalls = limit.CatalogCalls + 1 },
	} {
		t.Run(name+" mutation", func(t *testing.T) {
			x := valid
			mutate(&x)
			if validatePollPerformance(x, limit) == nil {
				t.Fatal("independent +1 mutation accepted")
			}
		})
	}
	alerts := make([]contract.AlertInstance, 2000)
	for i := range alerts {
		id := strconv.Itoa(i)
		alerts[i] = contract.AlertInstance{ID: "a-" + id, Status: "firing", Entity: &contract.EntityRef{ClusterID: testCluster, Namespace: "ns", PodUID: "pod-" + id}}
	}
	calls := 0
	p := NewAlertPoller(AlertPollerConfig{ClusterID: testCluster, MaxAlerts: 2000, TrustedNamespaces: func() map[string]string {
		calls++
		trusted := make(map[string]string, 100000)
		for i := 0; i < 100000; i++ {
			trusted["pod-"+strconv.Itoa(i)] = "ns"
		}
		return trusted
	}}, staticAlerts{result: datasource.AlertResult{Firing: alerts}}, nil, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	got := pollPerformanceSample{Latency: elapsed, TotalAlloc: after.TotalAlloc - before.TotalAlloc, CatalogCalls: calls}
	if raceEnabled {
		got.Latency = 0
	}
	if err := validatePollPerformance(got, limit); err != nil {
		t.Fatalf("PollOnce performance=%+v: %v", got, err)
	}
	t.Logf("PollOnce performance latency=%s alloc=%d calls=%d", got.Latency, got.TotalAlloc, got.CatalogCalls)
}

func newAlertFixture(t *testing.T, src datasource.Alerts, maxAlerts int) (*AlertPoller, *Subscription, *Hub) {
	t.Helper()
	h := newTestHub(t, Config{}, nil)
	sub, err := h.Subscribe("watcher", allFilter(), "")
	if err != nil {
		t.Fatal(err)
	}
	p := NewAlertPoller(AlertPollerConfig{ClusterID: testCluster, MaxAlerts: maxAlerts,
		TrustedNamespaces: func() map[string]string {
			return map[string]string{"pod:pod-a1": "payments", "pod:pod-a2": "media", "pod:pod-a3": "media"}
		}}, src, h, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	return p, sub, h
}

func TestAlertInitialSnapshotEmitsNothing(t *testing.T) {
	src := &scriptedAlerts{results: []func() (datasource.AlertResult, error){
		fixed([]contract.AlertInstance{alert("a1", "payments"), alert("a2", "media")}, nil),
	}}
	p, sub, _ := newAlertFixture(t, src, 0)
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoEvent(t, sub.Events())
}

func TestAlertDiffEmitsAddedUpdatedDeleted(t *testing.T) {
	resolved := alert("a1", "payments")
	resolved.Status = "resolved"
	src := &scriptedAlerts{results: []func() (datasource.AlertResult, error){
		fixed([]contract.AlertInstance{alert("a1", "payments")}, nil),
		// a1은 resolved로 이동(updated), a3는 신규(added), 이전의 a1-firing만 있던
		// 스냅숏 기준으로 사라진 것은 없습니다.
		fixed([]contract.AlertInstance{alert("a3", "media")}, []contract.AlertInstance{resolved}),
		// a1·a3 모두 사라짐(deleted 2건).
		fixed(nil, nil),
	}}
	p, sub, _ := newAlertFixture(t, src, 0)

	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err) // 최초 — 무이벤트
	}
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[contract.StreamEventAction]int{}
	nsSeen := map[string]bool{}
	for i := 0; i < 2; i++ {
		ev := recv(t, sub.Events()).Envelope
		if ev.Kind != contract.StreamKindAlert {
			t.Fatalf("kind: %s", ev.Kind)
		}
		got[ev.Action]++
		nsSeen[ev.Namespace] = true
	}
	if got[contract.StreamActionAdded] != 1 || got[contract.StreamActionUpdated] != 1 {
		t.Fatalf("added/updated 기대, got %v", got)
	}
	if !nsSeen["payments"] || !nsSeen["media"] {
		t.Fatalf("namespace가 봉투에 없습니다: %v", nsSeen)
	}

	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ev := recv(t, sub.Events()).Envelope
		if ev.Action != contract.StreamActionDeleted {
			t.Fatalf("deleted 기대, got %s", ev.Action)
		}
	}
	assertNoEvent(t, sub.Events())
}

func TestAlertEnvelopeCarriesNoAnnotations(t *testing.T) {
	a := alert("a1", "payments")
	a.Annotations = map[string]string{"summary": "DB 커넥션 고갈 · 내부 주소 10.0.0.1"}
	src := &scriptedAlerts{results: []func() (datasource.AlertResult, error){
		fixed(nil, nil),
		fixed([]contract.AlertInstance{a}, nil),
	}}
	p, sub, _ := newAlertFixture(t, src, 0)
	_ = p.PollOnce(context.Background())
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev := recv(t, sub.Events()).Envelope
	if ev.Entity == nil || ev.Entity.Namespace != "payments" {
		t.Fatalf("entity가 없거나 틀렸습니다: %+v", ev.Entity)
	}
	// 봉투 어디에도 annotation 문자열이 실리면 안 됩니다 — 구조적으로 필드가 없습니다.
	if ev.ResourceVersion != "" {
		t.Fatalf("알림 봉투에 예상 밖 필드: %q", ev.ResourceVersion)
	}
}

func TestAlertVisibleFingerprintDetectsChangesAndIgnoresMapOrder(t *testing.T) {
	base := alert("a1", "payments")
	base.Labels = map[string]string{"a": "1", "b": "2"}
	base.Annotations = map[string]string{"summary": "same", "runbook": "same"}
	reordered := base
	reordered.Labels = map[string]string{"b": "2", "a": "1"}
	reordered.Annotations = map[string]string{"runbook": "same", "summary": "same"}
	severity := reordered
	severity.Severity = "warning"
	labels := severity
	labels.Labels = map[string]string{"a": "changed", "b": "2"}
	annotations := labels
	annotations.Annotations = map[string]string{"summary": "changed", "runbook": "same"}
	src := &scriptedAlerts{results: []func() (datasource.AlertResult, error){
		fixed([]contract.AlertInstance{base}, nil),
		fixed([]contract.AlertInstance{reordered}, nil),
		fixed([]contract.AlertInstance{severity}, nil),
		fixed([]contract.AlertInstance{labels}, nil),
		fixed([]contract.AlertInstance{annotations}, nil),
	}}
	p, sub, _ := newAlertFixture(t, src, 0)
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoEvent(t, sub.Events())
	for _, field := range []string{"severity", "labels", "annotations"} {
		if err := p.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if ev := recv(t, sub.Events()).Envelope; ev.Action != contract.StreamActionUpdated {
			t.Fatalf("%s change action=%s want updated", field, ev.Action)
		}
	}
}

func TestAlertSnapshotBounded(t *testing.T) {
	many := []contract.AlertInstance{alert("a1", "payments"), alert("a2", "media"), alert("a3", "batch")}
	src := &scriptedAlerts{results: []func() (datasource.AlertResult, error){
		fixed(many, nil),
		fixed(many, nil),
	}}
	p, sub, _ := newAlertFixture(t, src, 2)
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.snapshot) != 0 || p.primed {
		t.Fatalf("overflow snapshot must be retained, got size=%d primed=%t", len(p.snapshot), p.primed)
	}
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 같은 결과의 재조회는 diff 0건이어야 합니다 — 상한이 흔들리며 유령 변경을 만들면 안 됩니다.
	assertNoEvent(t, sub.Events())
}

func TestAlertNamespaceClaimFailsClosedWithoutTrustedUID(t *testing.T) {
	a := alert("unknown", "secret")
	a.Entity.PodUID = "forged"
	src := &scriptedAlerts{results: []func() (datasource.AlertResult, error){fixed(nil, nil), fixed([]contract.AlertInstance{a}, nil)}}
	p, sub, _ := newAlertFixture(t, src, 0)
	_ = p.PollOnce(context.Background())
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev := recv(t, sub.Events()).Envelope
	if ev.Namespace != "" || ev.Entity != nil {
		t.Fatalf("untrusted namespace/entity leaked: %+v", ev)
	}
}

func TestAlertTrustedIdentityMovementInvalidatesOldAndNewScopes(t *testing.T) {
	a := alert("a1", "claimed-is-ignored")
	src := &scriptedAlerts{results: []func() (datasource.AlertResult, error){
		fixed([]contract.AlertInstance{a}, nil),
		fixed([]contract.AlertInstance{a}, nil),
		fixed([]contract.AlertInstance{a}, nil),
	}}
	h := newTestHub(t, Config{}, nil)
	all, _ := h.Subscribe("all", allFilter(), "")
	payments, _ := h.Subscribe("payments", nsFilter("payments"), "")
	media, _ := h.Subscribe("media", nsFilter("media"), "")
	trusted := map[string]string{"pod:pod-a1": "payments"}
	p := NewAlertPoller(AlertPollerConfig{ClusterID: testCluster,
		TrustedNamespaces: func() map[string]string { return trusted }}, src, h, nil)

	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	trusted = map[string]string{"pod:pod-a1": "media"}
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := recv(t, all.Events()).Envelope
	cur := recv(t, all.Events()).Envelope
	if old.Action != contract.StreamActionDeleted || old.Namespace != "payments" ||
		cur.Action != contract.StreamActionUpdated || cur.Namespace != "media" {
		t.Fatalf("namespace move events: old=%+v current=%+v", old, cur)
	}
	if got := recv(t, payments.Events()).Envelope; got.Action != contract.StreamActionDeleted {
		t.Fatalf("old scope was not invalidated: %+v", got)
	}
	if got := recv(t, media.Events()).Envelope; got.Action != contract.StreamActionUpdated {
		t.Fatalf("new scope was not invalidated: %+v", got)
	}

	trusted = nil
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	old = recv(t, all.Events()).Envelope
	cur = recv(t, all.Events()).Envelope
	if old.Action != contract.StreamActionDeleted || old.Namespace != "media" ||
		cur.Action != contract.StreamActionUpdated || cur.Namespace != "" || cur.Entity != nil {
		t.Fatalf("verified to cluster-wide events: old=%+v current=%+v", old, cur)
	}
	if got := recv(t, media.Events()).Envelope; got.Action != contract.StreamActionDeleted {
		t.Fatalf("previous verified scope was not invalidated: %+v", got)
	}
}

func TestAlertUnavailableSourceBacksOffWithoutState(t *testing.T) {
	p, sub, _ := newAlertFixture(t, datasource.Unavailable{Reason: "미연결"}, 0)
	for i := 0; i < 3; i++ {
		if err := p.PollOnce(context.Background()); err == nil {
			t.Fatal("Unavailable 소스가 성공을 돌려줬습니다")
		}
	}
	if p.primed || len(p.snapshot) != 0 {
		t.Fatal("실패한 조회가 스냅숏 상태를 만들었습니다")
	}
	assertNoEvent(t, sub.Events())
}

func TestAlertErrorKeepsSnapshotForRecovery(t *testing.T) {
	src := &scriptedAlerts{results: []func() (datasource.AlertResult, error){
		fixed([]contract.AlertInstance{alert("a1", "payments")}, nil),
		func() (datasource.AlertResult, error) { return datasource.AlertResult{}, errors.New("일시 장애") },
		fixed(nil, nil), // 복구 — a1이 사라졌음이 이제 관측됩니다.
	}}
	p, sub, _ := newAlertFixture(t, src, 0)
	_ = p.PollOnce(context.Background())
	if err := p.PollOnce(context.Background()); err == nil {
		t.Fatal("장애 조회가 성공으로 처리되었습니다")
	}
	assertNoEvent(t, sub.Events()) // 장애는 유령 deleted를 만들면 안 됩니다.
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ev := recv(t, sub.Events()).Envelope; ev.Action != contract.StreamActionDeleted {
		t.Fatalf("복구 후 deleted 기대, got %s", ev.Action)
	}
}

type notifyingAlerts struct {
	calls chan time.Time
	err   error
}

func (s *notifyingAlerts) List(context.Context, datasource.AlertQuery) (datasource.AlertResult, error) {
	s.calls <- time.Now()
	return datasource.AlertResult{}, s.err
}

func TestAlertPollerRunPollsImmediately(t *testing.T) {
	src := &notifyingAlerts{calls: make(chan time.Time, 2)}
	h := newTestHub(t, Config{}, nil)
	p := NewAlertPoller(AlertPollerConfig{Interval: 5 * time.Second, MaxBackoff: 10 * time.Second}, src, h, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	select {
	case <-src.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("initial poll did not run immediately")
	}
}

func TestAlertPollerRunFailureBackoffDoesNotSpin(t *testing.T) {
	src := &notifyingAlerts{calls: make(chan time.Time, 3), err: errors.New("down")}
	h := newTestHub(t, Config{}, nil)
	p := NewAlertPoller(AlertPollerConfig{Interval: 500 * time.Millisecond, MaxBackoff: time.Second}, src, h, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	select {
	case <-src.calls: // immediate failure
	case <-time.After(2 * time.Second):
		t.Fatal("initial failure poll did not run immediately")
	}
	select {
	case <-src.calls:
		t.Fatal("failure path spun before bounded backoff")
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-src.calls: // first retry after one-second bounded backoff
	case <-time.After(2 * time.Second):
		t.Fatal("failure retry did not occur at bounded backoff")
	}
}
