//go:build e2efixture

package e2efixture

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

func fixtureWindow() datasource.Window {
	return datasource.Window{
		From: testcluster.Now.Add(-time.Hour),
		To:   testcluster.Now,
		Step: time.Minute,
	}
}

func buildSource(t *testing.T) *Source {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, _, err := testcluster.Build(ctx, testcluster.ScenarioObjects()...)
	if err != nil {
		t.Fatalf("store 생성 실패: %v", err)
	}
	return NewSource(store, Scenarios())
}

// corpus는 릴리스 기준(#22)의 네 시나리오를 **전부** 담아야 하고, ID는 유일해야 합니다.
func TestScenarioCorpusIsExhaustive(t *testing.T) {
	want := []string{ScenarioCPUSpike, ScenarioCrashLoop, ScenarioErrorLog, ScenarioImagePull}
	got := make([]string, 0, 4)
	uids := map[string]bool{}
	alertIDs := map[string]bool{}
	for _, sc := range Scenarios() {
		got = append(got, sc.ID)
		if uids[sc.PodUID] {
			t.Errorf("루트 Pod UID 중복: %s", sc.PodUID)
		}
		uids[sc.PodUID] = true
		aid := "e2e-alert-" + sc.ID
		if alertIDs[aid] {
			t.Errorf("알림 ID 중복: %s", aid)
		}
		alertIDs[aid] = true
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("시나리오 corpus가 릴리스 기준과 다릅니다: got %v want %v", got, want)
	}
}

func TestUnknownScenarioFailsFast(t *testing.T) {
	if _, err := scenariosFor([]string{"crashloop", "bogus"}); err == nil {
		t.Fatal("알 수 없는 시나리오가 통과했습니다 — 기동은 fail-fast여야 합니다")
	}
}

// 각 시나리오의 루트 Pod UID가 카탈로그·이벤트·로그·알림에서 **같은 사실**을 가리켜야 하고,
// 모든 신호가 조회 구간 안에 있어야 합니다.
func TestScenarioSignalsAreCoherent(t *testing.T) {
	src := buildSource(t)
	w := fixtureWindow()
	ctx := context.Background()

	catalog := map[string]datasource.CatalogPod{}
	for _, p := range src.Catalog.CatalogPods(testcluster.ClusterID, "", 0) {
		catalog[p.UID] = p
	}

	store := src.Catalog.(interface {
		EventsForUID(uid string, since time.Time, limit int) ([]contract.ClusterEvent, error)
	})

	for _, sc := range Scenarios() {
		t.Run(sc.ID, func(t *testing.T) {
			// 1. Kubernetes: 루트 Pod가 informer 카탈로그에 존재합니다.
			p, ok := catalog[sc.PodUID]
			if !ok {
				t.Fatalf("루트 Pod %s(%s)가 카탈로그에 없습니다", sc.PodName, sc.PodUID)
			}
			if p.Name != sc.PodName || p.Namespace != sc.Namespace {
				t.Fatalf("카탈로그 신원 불일치: got %s/%s want %s/%s", p.Namespace, p.Name, sc.Namespace, sc.PodName)
			}

			// 2. Event: 루트 UID에 시나리오 Warning 이벤트가 구간 안에 있습니다.
			events, err := store.EventsForUID(sc.PodUID, w.From, 50)
			if err != nil || len(events) == 0 {
				t.Fatalf("루트 UID의 이벤트가 없습니다 (err=%v)", err)
			}
			found := false
			for _, e := range events {
				if e.Reason == sc.EventReason {
					found = true
				}
			}
			if !found {
				t.Fatalf("이벤트 Reason %q이 없습니다: %+v", sc.EventReason, events)
			}

			// 3. Log: 루트 Pod로 좁힌 검색에 구간 안의 ERROR 로그가 있습니다.
			page, err := src.Search(ctx, datasource.LogQuery{
				Target: datasource.Target{ClusterID: testcluster.ClusterID, Namespace: sc.Namespace, PodUID: sc.PodUID},
				Window: w,
				Levels: []contract.LogLevel{contract.LevelError},
			})
			if err != nil {
				t.Fatalf("로그 검색 실패: %v", err)
			}
			logHit := false
			for _, l := range page.Lines {
				if l.PodUID != sc.PodUID {
					t.Fatalf("PodUID 필터가 깨졌습니다: %s", l.PodUID)
				}
				if l.T < w.From.UnixMilli() || l.T > w.To.UnixMilli() {
					t.Fatalf("로그 시각이 구간 밖입니다: %d", l.T)
				}
				if strings.HasPrefix(l.ID, "e2e-"+sc.ID+"-") && l.Level == contract.LevelError {
					logHit = true
				}
			}
			if !logHit {
				t.Fatal("시나리오 ERROR 로그가 없습니다")
			}

			// 4. Alert: 루트 Pod UID를 가리키는 발화 알림이 구간 안에 있습니다.
			res, err := src.List(ctx, datasource.AlertQuery{
				Target: datasource.Target{ClusterID: testcluster.ClusterID, Namespace: sc.Namespace},
				Window: w,
			})
			if err != nil {
				t.Fatalf("알림 조회 실패: %v", err)
			}
			alertHit := false
			for _, a := range res.Firing {
				if a.ID != "e2e-alert-"+sc.ID {
					continue
				}
				alertHit = true
				if a.Entity == nil || a.Entity.PodUID != sc.PodUID {
					t.Fatalf("알림 Entity가 루트 Pod UID를 가리키지 않습니다: %+v", a.Entity)
				}
				start, err := time.Parse(time.RFC3339, a.StartsAt)
				if err != nil || start.Before(w.From) || start.After(w.To) {
					t.Fatalf("알림 시작 시각이 구간 밖입니다: %s (err=%v)", a.StartsAt, err)
				}
			}
			if !alertHit {
				t.Fatalf("시나리오 알림 e2e-alert-%s가 없습니다", sc.ID)
			}
		})
	}
}

// CPU spike는 메트릭에서 보여야 합니다 — 스파이크 구간과 baseline이 뚜렷이 갈립니다.
func TestCPUSpikeVisibleInTrends(t *testing.T) {
	src := buildSource(t)
	w := fixtureWindow()
	panels, err := src.Trends(context.Background(), datasource.Target{
		ClusterID: testcluster.ClusterID, Namespace: "payments", PodUID: testcluster.UIDPodBatchSync,
	}, w, []string{"cpu"})
	if err != nil || len(panels) == 0 {
		t.Fatalf("Trends 실패: %v", err)
	}
	spikeFrom := w.To.Add(-15 * time.Minute).UnixMilli()
	var base, spike []float64
	for _, s := range panels[0].Series {
		if s.Key != "used" {
			continue
		}
		for _, p := range s.Points {
			if p.T < spikeFrom {
				base = append(base, p.V)
			} else {
				spike = append(spike, p.V)
			}
		}
	}
	if len(base) == 0 || len(spike) == 0 {
		t.Fatal("포인트가 비어 있습니다")
	}
	for _, v := range base {
		if v >= 50 {
			t.Fatalf("baseline이 너무 높습니다: %v", v)
		}
	}
	for _, v := range spike {
		if v < 90 {
			t.Fatalf("스파이크가 보이지 않습니다: %v", v)
		}
	}
}

// Scope 밖 namespace의 시나리오 신호는 한 줄도 나가면 안 됩니다. (README §10)
func TestScenarioSignalsRespectScope(t *testing.T) {
	src := buildSource(t)
	w := fixtureWindow()
	ctx := context.Background()
	// media만 허용된 사용자 — payments 시나리오(crashloop·cpuspike)는 보이지 않습니다.
	target := datasource.Target{ClusterID: testcluster.ClusterID, Namespaces: []string{"media"}}

	page, err := src.Search(ctx, datasource.LogQuery{Target: target, Window: w})
	if err != nil {
		t.Fatalf("로그 검색 실패: %v", err)
	}
	for _, l := range page.Lines {
		if l.Namespace != "media" {
			t.Fatalf("scope 밖 로그가 노출되었습니다: %s/%s", l.Namespace, l.PodName)
		}
	}

	res, err := src.List(ctx, datasource.AlertQuery{Target: target, Window: w})
	if err != nil {
		t.Fatalf("알림 조회 실패: %v", err)
	}
	for _, a := range res.Firing {
		if ns := a.Labels["namespace"]; ns != "" && ns != "media" {
			t.Fatalf("scope 밖 알림이 노출되었습니다: %s", a.ID)
		}
	}
}

// 주입이 있어도 페이지 계약은 그대로여야 합니다: 응답은 PageSize를 넘지 않고,
// 커서로 이어지는 후속 페이지에 같은 줄이 다시 나오지 않습니다. (ADR 0003)
func TestSearchKeepsPageBoundsAndCursor(t *testing.T) {
	src := buildSource(t)
	w := fixtureWindow()
	ctx := context.Background()
	q := datasource.LogQuery{
		Target:   datasource.Target{ClusterID: testcluster.ClusterID},
		Window:   w,
		PageSize: 5,
	}
	seen := map[string]bool{}
	cursor := ""
	var previousT int64
	for page := 0; page < 1100; page++ {
		q.Cursor = cursor
		res, err := src.Search(ctx, q)
		if err != nil {
			t.Fatalf("검색 실패: %v", err)
		}
		if len(res.Lines) > q.PageSize {
			t.Fatalf("페이지 %d가 PageSize를 넘었습니다: %d > %d", page, len(res.Lines), q.PageSize)
		}
		if res.MaxLines <= 0 {
			t.Fatalf("MaxLines가 전달되지 않았습니다: %+v", res.MaxLines)
		}
		for _, l := range res.Lines {
			if previousT != 0 && l.T > previousT {
				t.Fatalf("페이지 경계에서 timestamp 내림차순 계약 위반: %d > %d", l.T, previousT)
			}
			if seen[l.ID] {
				t.Fatalf("페이지 %d에서 줄이 중복되었습니다: %s", page, l.ID)
			}
			seen[l.ID] = true
			previousT = l.T
		}
		if res.Next == "" {
			break
		}
		if len(res.Next) > maxFixtureCursorLength {
			t.Fatalf("cursor 크기 상한 초과: %d > %d", len(res.Next), maxFixtureCursorLength)
		}
		cursor = res.Next
	}
	if len(seen) == 0 {
		t.Fatal("로그가 비어 있습니다")
	}
	for _, sc := range Scenarios() {
		for i := 0; i < scenarioLogCount; i++ {
			id := "e2e-" + sc.ID + "-" + strconv.Itoa(i)
			if !seen[id] {
				t.Fatalf("cursor traversal에서 scenario 로그가 누락되었습니다: %s", id)
			}
		}
	}
	if len(seen) > fixtureMaxLines {
		t.Fatalf("MaxLines 초과: %d > %d", len(seen), fixtureMaxLines)
	}
	if len(seen) != fixtureMaxLines {
		t.Fatalf("cap까지 결정적으로 순회하지 못했습니다: %d != %d", len(seen), fixtureMaxLines)
	}
}

func TestSearchRejectsInvalidFixtureCursor(t *testing.T) {
	for _, cursor := range []string{
		"not-a-fixture-cursor",
		"e2e." + strings.Repeat("a", maxFixtureCursorLength),
		"e2e.eyJvIjoxLCJ1bmtub3duIjp0cnVlfQ",
	} {
		_, err := buildSource(t).Search(context.Background(), datasource.LogQuery{
			Target: datasource.Target{ClusterID: testcluster.ClusterID}, Window: fixtureWindow(), Cursor: cursor,
		})
		if err == nil {
			t.Fatalf("invalid cursor가 수락되었습니다: %q", cursor)
		}
	}
}

func TestHistogramAndFacetsIncludeEveryInjectedLine(t *testing.T) {
	src := buildSource(t)
	q := datasource.LogQuery{
		Target: datasource.Target{ClusterID: testcluster.ClusterID, Namespace: "payments", PodUID: testcluster.UIDPodCrashLoop},
		Window: fixtureWindow(), Levels: []contract.LogLevel{contract.LevelError},
	}
	baseHistogram, err := src.Source.Histogram(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	histogram, err := src.Histogram(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	baseErrors, gotErrors := 0, 0
	for _, bucket := range baseHistogram {
		baseErrors += bucket.Counts[contract.LevelError]
	}
	for _, bucket := range histogram {
		gotErrors += bucket.Counts[contract.LevelError]
	}
	if gotErrors-baseErrors != scenarioLogCount {
		t.Fatalf("histogram injected count=%d want=%d", gotErrors-baseErrors, scenarioLogCount)
	}
	baseFacets, _ := src.Source.Facets(context.Background(), q)
	facets, err := src.Facets(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	basePod, gotPod := 0, 0
	for _, p := range baseFacets.Pods {
		if p.UID == testcluster.UIDPodCrashLoop {
			basePod = p.Count
		}
	}
	for _, p := range facets.Pods {
		if p.UID == testcluster.UIDPodCrashLoop {
			gotPod = p.Count
		}
	}
	if gotPod-basePod != scenarioLogCount {
		t.Fatalf("facet injected count=%d want=%d", gotPod-basePod, scenarioLogCount)
	}
}

// 로그 본문에 마스킹되지 않은 Secret·토큰·Raw 질의가 나가면 안 됩니다.
func TestLogsContainNoRawSecrets(t *testing.T) {
	src := buildSource(t)
	page, err := src.Search(context.Background(), datasource.LogQuery{
		Target:   datasource.Target{ClusterID: testcluster.ClusterID},
		Window:   fixtureWindow(),
		PageSize: 500,
	})
	if err != nil {
		t.Fatalf("로그 검색 실패: %v", err)
	}
	if len(page.Lines) == 0 {
		t.Fatal("로그가 비어 있습니다")
	}
	banned := []string{"sk-live-7f3ac91b22d4", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"}
	for _, l := range page.Lines {
		for _, b := range banned {
			if strings.Contains(l.Message, b) {
				t.Fatalf("마스킹되지 않은 Secret이 로그에 남았습니다: %s", l.ID)
			}
		}
	}
}
