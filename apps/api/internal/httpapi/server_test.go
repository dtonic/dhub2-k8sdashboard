package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

/* ── 픽스처 ─────────────────────────────────────────────────────────────── */

type fixture struct {
	srv    *httpapi.Server
	counts *countingSource
}

// newFixture는 payments만 볼 수 있는 사용자로 서버를 세웁니다.
// media는 존재하지만 Scope 밖입니다 — 권한 강제를 실제로 확인하기 위해서입니다.
func newFixture(t *testing.T, opts ...func(*httpapi.Deps)) fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, _ := testcluster.NewStore(t, ctx)
	src := &countingSource{inner: demo.New(store)}

	deps := httpapi.Deps{
		Store:    store,
		Metrics:  src,
		Logs:     src,
		Alerts:   src,
		Topology: src,
		Resolver: scope.Static{S: scope.Scope{Clusters: []scope.Cluster{{
			ID: testcluster.ClusterID, Name: "Seoul Production", Namespaces: []string{"payments"},
		}}}},
		// 테스트에서는 캐시가 결과를 가리지 않도록 TTL을 0에 가깝게 둡니다.
		Cache: cache.NewTTL(time.Nanosecond),
		Now:   func() time.Time { return testcluster.Now },
	}
	for _, o := range opts {
		o(&deps)
	}
	return fixture{srv: httpapi.NewServer(deps), counts: src}
}

func withClusterWideScope(d *httpapi.Deps) {
	d.Resolver = scope.Static{S: scope.Scope{Clusters: []scope.Cluster{{
		ID: testcluster.ClusterID, Name: "Seoul Production", All: true,
	}}}}
}

func withBrokenDatasources(d *httpapi.Deps) {
	u := datasource.Unavailable{Reason: "테스트용 장애"}
	d.Metrics, d.Logs, d.Alerts, d.Topology = u, u, u, u
}

func (f fixture) get(t *testing.T, path string, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: 응답을 해석하지 못했습니다: %v\n%s", path, err, rec.Body.String())
		}
	}
	return rec
}

const base = "/api/v1/clusters/" + testcluster.ClusterID

/* ── 권한 ───────────────────────────────────────────────────────────────── */

func TestUnknownClusterIsRejectedWithoutData(t *testing.T) {
	f := newFixture(t)
	rec := f.get(t, "/api/v1/clusters/prod-frankfurt/overview?range=1h", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	var e contract.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != "forbidden" {
		t.Errorf("code=%q, want forbidden", e.Code)
	}
	// 부분 데이터도 나가면 안 됩니다.
	if strings.Contains(rec.Body.String(), "payments") {
		t.Errorf("권한 없는 응답에 데이터가 섞였습니다: %s", rec.Body.String())
	}
}

func TestNamespaceOutsideScopeLeaksNothing(t *testing.T) {
	// URL을 직접 고쳐도 범위 밖 데이터가 한 줄도 나가면 안 됩니다. (README §10)
	f := newFixture(t)
	for _, path := range []string{
		base + "/namespaces/media?range=1h",
		base + "/logs?range=1h&ns=media",
		base + "/topology?range=1h&ns=media",
		base + "/alerts?range=7d&ns=media",
	} {
		rec := f.get(t, path, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status=%d, want 403", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "media-api") {
			t.Errorf("%s: 범위 밖 데이터가 노출되었습니다", path)
		}
	}
}

func TestScopedListsExcludeOtherNamespaces(t *testing.T) {
	f := newFixture(t)
	var res contract.NamespaceListResponse
	f.get(t, base+"/namespaces?range=1h", &res)
	if res.Namespaces.Data == nil {
		t.Fatalf("namespaces 섹션이 비었습니다: %+v", res.Namespaces)
	}
	for _, ns := range *res.Namespaces.Data {
		if ns.Name != "payments" {
			t.Errorf("Scope 밖 namespace가 목록에 있습니다: %s", ns.Name)
		}
	}
}

func TestNodeHealthNeedsClusterWideScope(t *testing.T) {
	// namespace로 제한된 사용자에게 노드 수를 0으로 보여주면 사실이 아닌 값이 됩니다.
	scoped := newFixture(t)
	var a contract.ClusterOverviewResponse
	scoped.get(t, base+"/overview?range=1h", &a)
	if a.Nodes.Status != contract.StatusForbidden {
		t.Errorf("nodes.status=%s, want forbidden", a.Nodes.Status)
	}
	if a.Nodes.Data != nil {
		t.Error("forbidden 섹션에 데이터가 실렸습니다")
	}

	wide := newFixture(t, withClusterWideScope)
	var b contract.ClusterOverviewResponse
	wide.get(t, base+"/overview?range=1h", &b)
	if b.Nodes.Status != contract.StatusOK || b.Nodes.Data == nil {
		t.Fatalf("nodes 섹션=%+v", b.Nodes)
	}
	if b.Nodes.Data.Total != 2 || b.Nodes.Data.NotReady != 1 {
		t.Errorf("노드 집계=%+v", *b.Nodes.Data)
	}
}

/* ── 화면 단위 응답 ─────────────────────────────────────────────────────── */

func TestOverviewIsOneRequestWithoutPerWidgetFanout(t *testing.T) {
	// 화면 하나 = 요청 하나(ADR 0002). 그리고 그 요청 안에서도
	// 데이터소스 호출이 패널 수만큼 늘어나면 안 됩니다.
	f := newFixture(t)
	var res contract.ClusterOverviewResponse
	rec := f.get(t, base+"/overview?range=1h", &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if f.counts.trends.Load() != 1 {
		t.Errorf("Trends 호출=%d회, want 1회", f.counts.trends.Load())
	}
	if f.counts.alerts.Load() != 1 {
		t.Errorf("Alerts 호출=%d회, want 1회", f.counts.alerts.Load())
	}
	if f.counts.graph.Load() != 1 {
		t.Errorf("Topology 호출=%d회, want 1회", f.counts.graph.Load())
	}

	if res.Range.StepSeconds != 60 {
		t.Errorf("step=%d, want 60", res.Range.StepSeconds)
	}
	if res.Pods.Data == nil || res.Pods.Data.CrashLoopBackOff != 1 {
		t.Errorf("pods 섹션=%+v", res.Pods)
	}
	if res.Unhealthy.Data == nil || len(*res.Unhealthy.Data) == 0 {
		t.Error("이상 엔티티가 비었습니다")
	}
}

func TestUnhealthyEntitiesCarryLinkableIdentity(t *testing.T) {
	// 이상 목록에서 1클릭으로 상세로 가려면 ref에 ns와 UID가 있어야 합니다.
	f := newFixture(t)
	var res contract.ClusterOverviewResponse
	f.get(t, base+"/overview?range=1h", &res)

	for _, u := range *res.Unhealthy.Data {
		if u.Ref.Namespace == "" {
			t.Errorf("namespace 없는 ref: %+v", u)
		}
		if u.Kind == "Pod" && u.Ref.PodUID == "" {
			t.Errorf("Pod인데 UID가 없습니다: %+v", u)
		}
	}
}

func TestWorkloadDetailShowsGenerations(t *testing.T) {
	f := newFixture(t)
	var res contract.WorkloadDetailResponse
	f.get(t, base+"/workloads/Deployment/payments-api?ns=payments&range=1h", &res)

	if res.Workload.Data == nil {
		t.Fatalf("workload 섹션=%+v", res.Workload)
	}
	if res.OwnerChain.Data == nil || len(*res.OwnerChain.Data) != 2 {
		t.Fatalf("ownerChain=%+v", res.OwnerChain)
	}
	chain := *res.OwnerChain.Data
	if !chain[0].Current || chain[1].Current {
		t.Errorf("현재 세대 표시가 잘못되었습니다: %+v", chain)
	}
	if res.Pods.Data == nil || len(*res.Pods.Data) != 3 {
		t.Fatalf("pods=%+v", res.Pods)
	}
}

func TestMissingEntityIsEmptyNotError(t *testing.T) {
	// 없는 Pod는 "장애"가 아니라 "결과 없음"입니다. 화면이 다르게 그려야 합니다.
	f := newFixture(t)
	var res contract.PodDetailResponse
	rec := f.get(t, base+"/pods/does-not-exist?ns=payments&range=1h", &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if res.Pod.Status != contract.StatusEmpty {
		t.Errorf("pod.status=%s, want empty", res.Pod.Status)
	}
}

func TestPodDetailIsIdentifiedByUID(t *testing.T) {
	f := newFixture(t)
	var res contract.PodDetailResponse
	f.get(t, base+"/pods/payments-api-7f-bbb?ns=payments&uid="+testcluster.UIDPodCrashLoop+"&range=1h", &res)
	if res.Pod.Data == nil {
		t.Fatalf("pod 섹션=%+v", res.Pod)
	}
	if res.Pod.Data.UID != testcluster.UIDPodCrashLoop {
		t.Errorf("uid=%s", res.Pod.Data.UID)
	}
	if res.Pod.Data.Severity != contract.SeverityCritical {
		t.Errorf("severity=%s, want critical", res.Pod.Data.Severity)
	}
	if res.Containers.Data == nil || (*res.Containers.Data)[0].Reason != "CrashLoopBackOff" {
		t.Errorf("containers=%+v", res.Containers)
	}
	if res.Events.Data == nil || len(*res.Events.Data) != 1 {
		t.Errorf("이 Pod의 이벤트만 와야 합니다: %+v", res.Events)
	}
}

/* ── 세 가지 상태 구분 ──────────────────────────────────────────────────── */

func TestDatasourceOutageDegradesSectionsNotThePage(t *testing.T) {
	// 한 데이터소스가 죽어도 Kubernetes에서 오는 값은 그대로 보여야 합니다.
	f := newFixture(t, withBrokenDatasources)
	var res contract.ClusterOverviewResponse
	rec := f.get(t, base+"/overview?range=1h", &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 — 부분 장애가 화면 전체를 죽였습니다", rec.Code)
	}
	if res.Trends.Status != contract.StatusDegraded || res.Trends.Source != contract.SourceGreptimeDB {
		t.Errorf("trends=%+v, want degraded/greptimedb", res.Trends)
	}
	if res.Alerts.Status != contract.StatusDegraded || res.Alerts.Source != contract.SourceAlertmanager {
		t.Errorf("alerts=%+v", res.Alerts)
	}
	if res.Pods.Status != contract.StatusOK {
		t.Errorf("pods=%+v — Kubernetes 섹션까지 같이 죽었습니다", res.Pods)
	}
	// 사유에 내부 주소나 질의가 섞이면 안 됩니다.
	if strings.Contains(res.Trends.Reason, "테스트용 장애") {
		t.Errorf("내부 에러 원문이 노출되었습니다: %q", res.Trends.Reason)
	}
}

func TestThreeStatesAreDistinguishable(t *testing.T) {
	f := newFixture(t)
	var res contract.ClusterOverviewResponse
	f.get(t, base+"/overview?range=1h", &res)

	seen := map[contract.SectionStatus]bool{
		res.Nodes.Status: true, res.Pods.Status: true,
	}
	if !seen[contract.StatusForbidden] {
		t.Error("forbidden 상태가 하나도 없습니다")
	}
	if !seen[contract.StatusOK] {
		t.Error("ok 상태가 하나도 없습니다")
	}

	// 결과 0건은 ok가 아니라 empty로 내려가야 화면이 다르게 그립니다.
	var pod contract.PodDetailResponse
	f.get(t, base+"/pods/none?ns=payments&range=1h", &pod)
	if pod.Containers.Status != contract.StatusEmpty {
		t.Errorf("containers=%s, want empty", pod.Containers.Status)
	}
}

/* ── 시간 범위 ──────────────────────────────────────────────────────────── */

func TestCustomRangeOverThirtyDaysIsRejected(t *testing.T) {
	f := newFixture(t)
	from := testcluster.Now.Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	to := testcluster.Now.Format(time.RFC3339)
	rec := f.get(t, base+"/overview?from="+from+"&to="+to, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	var e contract.APIError
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != "invalid_range" {
		t.Errorf("code=%q, want invalid_range", e.Code)
	}
}

/* ── 로그 ───────────────────────────────────────────────────────────────── */

func TestLogCursorPagingHasNoDuplicatesOrGaps(t *testing.T) {
	// offset을 쓰면 로그가 들어오는 동안 페이지 경계가 밀려 중복·누락이 생깁니다. (ADR 0003)
	f := newFixture(t)
	seen := map[string]bool{}
	total, dup := 0, 0
	cursor := ""

	for page := 0; page < 8; page++ {
		var res contract.LogSearchResponse
		path := base + "/logs?range=1h"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		if rec := f.get(t, path, &res); rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		if res.Lines.Data == nil {
			t.Fatalf("lines 섹션=%+v", res.Lines)
		}
		for _, l := range *res.Lines.Data {
			total++
			if seen[l.ID] {
				dup++
			}
			seen[l.ID] = true
		}
		if res.Cursor.Next == nil {
			break
		}
		cursor = *res.Cursor.Next
	}

	if dup != 0 {
		t.Errorf("중복 %d줄", dup)
	}
	if len(seen) != total {
		t.Errorf("고유 %d줄 != 전체 %d줄", len(seen), total)
	}
	if total < 400 {
		t.Errorf("가져온 줄 수=%d, want 400줄 이상", total)
	}
}

func TestLogMessagesAreMaskedBeforeLeavingTheServer(t *testing.T) {
	f := newFixture(t)
	var res contract.LogSearchResponse
	f.get(t, base+"/logs?range=1h", &res)

	raw := f.get(t, base+"/logs?range=1h", nil).Body.String()
	for _, forbidden := range []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", "sk-live-7f3ac91b22d4", "admin@example.com"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("원문이 응답에 남았습니다: %s", forbidden)
		}
	}
	if strings.Contains(raw, `"raw"`) || strings.Contains(raw, `"original"`) {
		t.Error("원문 필드가 따로 실려 나갔습니다")
	}

	masked := 0
	for _, l := range *res.Lines.Data {
		if len(l.Masked) == 0 {
			continue
		}
		masked++
		if !strings.Contains(l.Message, "•") {
			t.Errorf("span은 있는데 가림 문자가 없습니다: %q", l.Message)
		}
	}
	if masked == 0 {
		t.Fatal("마스킹된 줄이 하나도 없습니다")
	}
}

func TestLogLinesPointAtRealPods(t *testing.T) {
	// 로그 한 줄에서 Pod 상세로 가는 딥링크가 404가 되면 안 됩니다.
	f := newFixture(t)
	var logs contract.LogSearchResponse
	f.get(t, base+"/logs?range=1h", &logs)

	for _, l := range *logs.Lines.Data {
		var pod contract.PodDetailResponse
		f.get(t, base+"/pods/"+l.PodName+"?ns="+l.Namespace+"&uid="+l.PodUID+"&range=1h", &pod)
		if pod.Pod.Status != contract.StatusOK {
			t.Fatalf("로그의 Pod를 찾을 수 없습니다: %s/%s (%s) → %s", l.Namespace, l.PodName, l.PodUID, pod.Pod.Status)
		}
	}
}

func TestLogFilterNarrowsResults(t *testing.T) {
	f := newFixture(t)
	var res contract.LogSearchResponse
	f.get(t, base+"/logs?range=1h&levels=ERROR", &res)
	if res.Lines.Data == nil {
		t.Fatalf("lines=%+v", res.Lines)
	}
	for _, l := range *res.Lines.Data {
		if l.Level != contract.LevelError {
			t.Fatalf("필터가 적용되지 않았습니다: %s", l.Level)
		}
	}
}

/* ── Topology · Alerts ──────────────────────────────────────────────────── */

func TestTopologyEdgesAreDirectional(t *testing.T) {
	// A→B와 B→A가 하나로 합쳐지면 방향별 수치를 볼 수 없습니다.
	f := newFixture(t, withClusterWideScope)
	var res contract.TopologyResponse
	f.get(t, base+"/topology?range=1h", &res)
	if res.Graph.Data == nil {
		t.Fatalf("graph=%+v", res.Graph)
	}
	pairs := map[string]bool{}
	for _, e := range res.Graph.Data.Edges {
		if e.From == e.To {
			t.Errorf("자기 자신으로 향하는 엣지: %+v", e)
		}
		pairs[e.From+"->"+e.To] = true
	}
	found := false
	for k := range pairs {
		parts := strings.SplitN(k, "->", 2)
		if pairs[parts[1]+"->"+parts[0]] {
			found = true
		}
	}
	if !found {
		t.Error("양방향 엣지 쌍이 하나도 없습니다")
	}
	if res.Pods.Data == nil || res.Pods.Data.Total == 0 {
		t.Errorf("pods 헤더=%+v", res.Pods)
	}
	if res.Pods.Data.Unhealthy != len(res.Pods.Data.UnhealthyList) {
		t.Errorf("비정상 수와 목록 길이가 다릅니다: %d vs %d",
			res.Pods.Data.Unhealthy, len(res.Pods.Data.UnhealthyList))
	}
}

func TestEdgeSeriesUsesTheSameWindow(t *testing.T) {
	f := newFixture(t)
	var res contract.TopologyEdgeSeriesResponse
	f.get(t, base+"/topology/edges/a-%3Eb/series?range=1h", &res)
	if res.Series.Data == nil || len(*res.Series.Data) == 0 {
		t.Fatalf("series=%+v", res.Series)
	}
	if res.Range.StepSeconds != 60 {
		t.Errorf("step=%d, want 60 (분 단위 누적)", res.Range.StepSeconds)
	}
}

func TestAlertsExposeGroupingRule(t *testing.T) {
	// 왜 묶였는지 화면에 그대로 보여야 사용자가 숫자를 믿습니다.
	f := newFixture(t)
	var res contract.AlertListResponse
	f.get(t, base+"/alerts?range=7d", &res)
	if res.GroupingRule == "" {
		t.Error("grouping 기준이 비었습니다")
	}
	if res.Counts.Data == nil {
		t.Fatalf("counts=%+v", res.Counts)
	}
	firing := 0
	if res.Firing.Data != nil {
		firing = len(*res.Firing.Data)
	}
	sum := 0
	for _, c := range *res.Counts.Data {
		sum += c.Firing
	}
	if sum != firing {
		t.Errorf("counts 합=%d, firing 수=%d", sum, firing)
	}
}

func TestAlertBackendOutageKeepsThePageAlive(t *testing.T) {
	f := newFixture(t, withBrokenDatasources)
	var res contract.AlertListResponse
	rec := f.get(t, base+"/alerts?range=7d", &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if res.Firing.Status != contract.StatusDegraded {
		t.Errorf("firing=%+v, want degraded", res.Firing)
	}
}

/* ── 캐시 ───────────────────────────────────────────────────────────────── */

func TestCacheKeyIncludesScope(t *testing.T) {
	// Scope가 키에 없으면 권한이 다른 사용자끼리 캐시를 나눠 갖습니다.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, _ := testcluster.NewStore(t, ctx)
	src := demo.New(store)

	srv := httpapi.NewServer(httpapi.Deps{
		Store: store, Metrics: src, Logs: src, Alerts: src, Topology: src,
		Resolver: headerResolver{},
		Cache:    cache.NewTTL(time.Minute),
		Now:      func() time.Time { return testcluster.Now },
	})

	call := func(header string) contract.ClusterOverviewResponse {
		r := httptest.NewRequest(http.MethodGet, base+"/overview?range=1h", nil)
		r.Header.Set("X-Test-Scope", header)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		var out contract.ClusterOverviewResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", header, err)
		}
		return out
	}

	wide := call("all")
	narrow := call("payments")
	if wide.Nodes.Status == narrow.Nodes.Status {
		t.Fatalf("Scope가 달라도 같은 캐시 값을 받았습니다: %s", wide.Nodes.Status)
	}
}

func TestCacheSharesEqualAuthorizationScopeAcrossSubjects(t *testing.T) {
	f := newFixture(t, func(d *httpapi.Deps) { d.Resolver = subjectHeaderResolver{}; d.Cache = cache.NewTTL(time.Minute) })
	for _, subject := range []string{"subject-a", "subject-b"} {
		r := httptest.NewRequest(http.MethodGet, base+"/overview?range=1h", nil)
		r.Header.Set("X-Test-Subject", subject)
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d", subject, rec.Code)
		}
	}
	if got := f.counts.trends.Load(); got != 1 {
		t.Fatalf("equal authorization scopes did not share cache: trends=%d", got)
	}
}

type subjectHeaderResolver struct{}

func (subjectHeaderResolver) Resolve(r *http.Request) (scope.Scope, error) {
	return scope.Scope{Subject: r.Header.Get("X-Test-Subject"), Clusters: []scope.Cluster{{ID: testcluster.ClusterID, Name: "Seoul Production", Namespaces: []string{"payments"}}}}, nil
}

// headerResolver는 헤더로 Scope를 바꿉니다. 실제 토큰 연동 자리를 흉내 냅니다.
type headerResolver struct{}

func (headerResolver) Resolve(r *http.Request) (scope.Scope, error) {
	c := scope.Cluster{ID: testcluster.ClusterID, Name: "Seoul Production"}
	if r.Header.Get("X-Test-Scope") == "all" {
		c.All = true
	} else {
		c.Namespaces = []string{"payments"}
	}
	return scope.Scope{Clusters: []scope.Cluster{c}}, nil
}

/* ── 데이터소스 호출 횟수 세기 ──────────────────────────────────────────── */

type countingSource struct {
	inner  *demo.Source
	trends atomic.Int32
	alerts atomic.Int32
	graph  atomic.Int32
}

func (c *countingSource) Trends(ctx context.Context, t datasource.Target, w datasource.Window, p []string) ([]contract.TrendPanel, error) {
	c.trends.Add(1)
	return c.inner.Trends(ctx, t, w, p)
}

func (c *countingSource) Usage(ctx context.Context, id string) (map[string]contract.ContainerUsage, error) {
	return c.inner.Usage(ctx, id)
}

func (c *countingSource) Search(ctx context.Context, q datasource.LogQuery) (datasource.LogPage, error) {
	return c.inner.Search(ctx, q)
}

func (c *countingSource) Histogram(ctx context.Context, q datasource.LogQuery) ([]contract.LogHistogramBucket, error) {
	return c.inner.Histogram(ctx, q)
}

func (c *countingSource) Facets(ctx context.Context, q datasource.LogQuery) (contract.LogFacets, error) {
	return c.inner.Facets(ctx, q)
}

func (c *countingSource) List(ctx context.Context, q datasource.AlertQuery) (datasource.AlertResult, error) {
	c.alerts.Add(1)
	return c.inner.List(ctx, q)
}

func (c *countingSource) Graph(ctx context.Context, t datasource.Target, w datasource.Window) (contract.TopologyGraph, error) {
	c.graph.Add(1)
	return c.inner.Graph(ctx, t, w)
}

func (c *countingSource) EdgeSeries(ctx context.Context, id, edge string, w datasource.Window) ([]contract.TrendSeries, error) {
	return c.inner.EdgeSeries(ctx, id, edge, w)
}
