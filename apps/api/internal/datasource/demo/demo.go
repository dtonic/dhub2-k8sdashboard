// Package demo는 GreptimeDB·Quickwit·Alertmanager 없이도 API를 띄우기 위한 어댑터입니다.
//
// 값은 **결정적**입니다. 같은 입력이면 항상 같은 출력이라 테스트와 화면 확인에 쓸 수 있습니다.
// Pod 이름·UID는 지어내지 않고 informer 캐시(PodCatalog)에서 빌려옵니다 —
// 지어내면 로그·토폴로지에서 Pod 상세로 가는 딥링크가 404가 됩니다. (CLAUDE.md)
package demo

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/mask"
)

// MaxLines는 한 조회에서 훑는 로그 줄 수 상한입니다. 넘으면 truncated로 알립니다.
const MaxLines = 5000

// DefaultPageSize는 커서 한 페이지 크기입니다.
const DefaultPageSize = 100

// Source는 네 어댑터를 모두 구현합니다.
type Source struct {
	Catalog datasource.PodCatalog
}

func New(c datasource.PodCatalog) *Source { return &Source{Catalog: c} }

/* ── 결정적 잡음 ────────────────────────────────────────────────────────── */

// noise는 키와 인덱스로 0~1 값을 만듭니다. 난수를 쓰지 않아 재실행해도 같습니다.
func noise(key string, i int) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
	return float64(h.Sum64()%10_000) / 10_000
}

func pick[T any](list []T, key string, i int) T {
	var zero T
	if len(list) == 0 {
		return zero
	}
	return list[int(noise(key, i)*float64(len(list)))%len(list)]
}

// pods는 Target의 namespace 범위를 존중해 Pod 신원을 빌려옵니다.
// 여러 namespace만 허용된 사용자(Namespace=="", Namespaces!=nil)의 범위 밖 Pod는
// 데모 데이터에도 나타나면 안 됩니다 — 데모라도 Scope 규칙은 같습니다. (README §10)
func (s *Source) pods(t datasource.Target) []datasource.CatalogPod {
	all := s.Catalog.CatalogPods(t.ClusterID, t.Namespace, 0)
	if t.Namespace != "" || len(t.Namespaces) == 0 {
		return all
	}
	out := make([]datasource.CatalogPod, 0, len(all))
	for _, p := range all {
		if t.AllowsNamespace(p.Namespace) {
			out = append(out, p)
		}
	}
	return out
}

/* ── Metrics ────────────────────────────────────────────────────────────── */

var panelTitles = map[string]struct {
	title  string
	unit   string
	series []string
}{
	"cpu":      {"CPU 사용률", "percent", []string{"used", "requested"}},
	"memory":   {"메모리 사용률", "percent", []string{"used", "requested"}},
	"network":  {"네트워크", "bytes_per_sec", []string{"rx", "tx"}},
	"restarts": {"컨테이너 재시작", "count", []string{"restarts"}},
}

var seriesLabel = map[string]string{
	"used": "사용", "requested": "Request", "rx": "수신", "tx": "송신", "restarts": "재시작",
}

func (s *Source) Trends(_ context.Context, t datasource.Target, w datasource.Window, panels []string) ([]contract.TrendPanel, error) {
	if len(panels) == 0 {
		panels = []string{"cpu", "memory", "network", "restarts"}
	}
	out := make([]contract.TrendPanel, 0, len(panels))
	for _, id := range panels {
		meta, ok := panelTitles[id]
		if !ok {
			continue
		}
		p := contract.TrendPanel{
			ID:          id,
			Title:       meta.title,
			StepSeconds: int(w.Step.Seconds()),
		}
		for _, key := range meta.series {
			p.Series = append(p.Series, contract.TrendSeries{
				Key:    key,
				Label:  seriesLabel[key],
				Unit:   meta.unit,
				Points: points(t.ClusterID+"/"+t.Namespace+"/"+id+"/"+key, w, id, key),
			})
		}
		out = append(out, p)
	}
	return out, nil
}

// timeBucket은 절대 시각 기반 버킷 번호입니다. 같은 시각은 항상 같은 값(결정성)을
// 유지하면서, 창이 미끄러지면 오른쪽에서 새 버킷이 들어와 갱신마다 곡선이 흐릅니다. (#27)
func timeBucket(ts time.Time, step time.Duration) int {
	if step < time.Second {
		step = time.Minute
	}
	return int(ts.Unix() / int64(step/time.Second))
}

func points(key string, w datasource.Window, panel, series string) []contract.TrendPoint {
	n := buckets(w)
	out := make([]contract.TrendPoint, 0, n)
	for i := 0; i < n; i++ {
		ts := w.From.Add(time.Duration(i) * w.Step)
		bi := timeBucket(ts, w.Step)
		wave := math.Sin(float64(bi)/float64(maxInt(n/6, 1))) * 0.5
		base := 0.5 + wave*0.3 + noise(key, bi)*0.2
		var v float64
		switch panel {
		case "cpu", "memory":
			v = clamp(base*100, 3, 99)
			if series == "requested" {
				v = clamp(base*70, 3, 99)
			}
		case "network":
			v = base * 400 * 1024 * 1024
			if series == "tx" {
				v *= 0.6
			}
		case "restarts":
			v = math.Floor(noise(key, bi) * 3)
		}
		out = append(out, contract.TrendPoint{T: ts.UnixMilli(), V: round2(v)})
	}
	return out
}

func (s *Source) Usage(_ context.Context, clusterID string) (map[string]contract.ContainerUsage, error) {
	out := map[string]contract.ContainerUsage{}
	for i, p := range s.Catalog.CatalogPods(clusterID, "", 0) {
		out[p.UID] = contract.ContainerUsage{
			CPUMilli:  40 + int(noise(clusterID+p.UID, i)*260),
			MemoryMib: 96 + int(noise(clusterID+p.UID+"m", i)*640),
		}
	}
	return out, nil
}

/* ── Logs ───────────────────────────────────────────────────────────────── */

var messages = []string{
	"GET /api/v1/orders 200 in %dms",
	"POST /api/v1/payments 500 upstream timeout after %dms",
	"connection pool exhausted (waiters=%d)",
	"user login succeeded for admin@example.com in %dms",
	"authorization header Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload rejected (%d)",
	"db query slow: SELECT * FROM ledger LIMIT %d",
	"cache miss ratio %d%% over last window",
	"retrying upstream 10.42.0.17 attempt %d",
	"config reloaded, api_key=sk-live-7f3ac91b22d4 (%d keys)", // gitleaks:allow
	"probe failed: readiness returned 503 (%d)",
}

var levels = []contract.LogLevel{
	contract.LevelInfo, contract.LevelInfo, contract.LevelInfo,
	contract.LevelDebug, contract.LevelWarn, contract.LevelError,
}

// cursorOf는 (index, timestamp)를 불투명 문자열로 인코딩합니다.
// offset이 아니라 **정렬 키**를 담기 때문에 새 로그가 들어와도 페이지가 밀리지 않습니다. (ADR 0003)
func cursorOf(i int, t int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(i) + "|" + strconv.FormatInt(t, 10)))
}

func parseCursor(c string) (int, bool) {
	if c == "" {
		return 0, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	parts := strings.SplitN(string(raw), "|", 2)
	i, err := strconv.Atoi(parts[0])
	if err != nil || i < 0 {
		return 0, false
	}
	return i, true
}

// line은 i번째 줄을 만듭니다. i는 최신순이며, 같은 i는 항상 같은 줄을 만듭니다.
func (s *Source) line(q datasource.LogQuery, pods []datasource.CatalogPod, i int) (contract.LogLine, bool) {
	if len(pods) == 0 {
		return contract.LogLine{}, false
	}
	span := q.Window.To.Sub(q.Window.From)
	if span <= 0 {
		return contract.LogLine{}, false
	}
	step := span / time.Duration(MaxLines)
	if step <= 0 {
		step = time.Millisecond
	}
	ts := q.Window.To.Add(-time.Duration(i) * step)
	if ts.Before(q.Window.From) {
		return contract.LogLine{}, false
	}

	pod := pods[i%len(pods)]
	key := q.Target.ClusterID + "/logs"
	level := pick(levels, key+"/lvl", i)
	raw := fmt.Sprintf(pick(messages, key+"/msg", i), 1+int(noise(key+"/n", i)*900))
	masked, spans := mask.Apply(raw)

	return contract.LogLine{
		ID:            fmt.Sprintf("%d-%s", ts.UnixMilli(), strconv.FormatInt(int64(i), 36)),
		T:             ts.UnixMilli(),
		Level:         level,
		Message:       masked,
		Masked:        spans,
		Namespace:     pod.Namespace,
		PodName:       pod.Name,
		PodUID:        pod.UID,
		ContainerName: "app",
		WorkloadKind:  pod.WorkloadKind,
		WorkloadName:  pod.WorkloadName,
		NodeName:      pod.Node,
		TraceID:       fmt.Sprintf("%016x", uint64(noise(key+"/trace", i)*(1<<62))),
	}, true
}

func (s *Source) matches(q datasource.LogQuery, l contract.LogLine) bool {
	if len(q.Levels) > 0 && !containsLevel(q.Levels, l.Level) {
		return false
	}
	if q.Target.PodUID != "" && l.PodUID != q.Target.PodUID {
		return false
	}
	if q.Target.WorkloadName != "" && l.WorkloadName != q.Target.WorkloadName {
		return false
	}
	if q.Container != "" && l.ContainerName != q.Container {
		return false
	}
	if q.Text != "" && !strings.Contains(strings.ToLower(l.Message), strings.ToLower(q.Text)) {
		return false
	}
	return true
}

func (s *Source) Search(_ context.Context, q datasource.LogQuery) (datasource.LogPage, error) {
	start, ok := parseCursor(q.Cursor)
	if !ok {
		return datasource.LogPage{}, fmt.Errorf("커서를 해석할 수 없습니다")
	}
	size := q.PageSize
	if size <= 0 {
		size = DefaultPageSize
	}
	pods := s.pods(q.Target)

	page := datasource.LogPage{Lines: make([]contract.LogLine, 0, size), MaxLines: MaxLines}
	i := start
	for ; i < MaxLines && len(page.Lines) < size; i++ {
		l, ok := s.line(q, pods, i)
		if !ok {
			break
		}
		if s.matches(q, l) {
			page.Lines = append(page.Lines, l)
		}
	}
	if i < MaxLines && len(page.Lines) == size {
		page.Next = cursorOf(i, page.Lines[len(page.Lines)-1].T)
	} else if i >= MaxLines {
		page.Truncated = true
	}
	return page, nil
}

func (s *Source) Histogram(_ context.Context, q datasource.LogQuery) ([]contract.LogHistogramBucket, error) {
	n := buckets(datasource.Window{From: q.Window.From, To: q.Window.To, Step: q.Window.Step})
	if n == 0 {
		return nil, nil
	}
	pods := s.pods(q.Target)
	out := make([]contract.LogHistogramBucket, n)
	for i := 0; i < n; i++ {
		out[i] = contract.LogHistogramBucket{
			T:      q.Window.From.Add(time.Duration(i) * q.Window.Step).UnixMilli(),
			Counts: map[contract.LogLevel]int{},
		}
	}
	for i := 0; i < MaxLines; i++ {
		l, ok := s.line(q, pods, i)
		if !ok {
			break
		}
		if !s.matches(q, l) {
			continue
		}
		idx := int(time.UnixMilli(l.T).Sub(q.Window.From) / q.Window.Step)
		if idx < 0 || idx >= n {
			continue
		}
		out[idx].Counts[l.Level]++
	}
	return out, nil
}

func (s *Source) Facets(_ context.Context, q datasource.LogQuery) (contract.LogFacets, error) {
	pods := s.pods(q.Target)
	f := contract.LogFacets{}
	byWorkload := map[string]*contract.LogFacetWorkload{}
	for _, p := range pods {
		f.Pods = append(f.Pods, contract.LogFacetPod{Name: p.Name, UID: p.UID, Count: 1 + len(p.Name)%40})
		if p.WorkloadName == "" {
			continue
		}
		if v, ok := byWorkload[p.WorkloadName]; ok {
			v.Count++
			continue
		}
		byWorkload[p.WorkloadName] = &contract.LogFacetWorkload{Name: p.WorkloadName, Kind: p.WorkloadKind, Count: 1}
	}
	for _, v := range byWorkload {
		f.Workloads = append(f.Workloads, *v)
	}
	sort.Slice(f.Workloads, func(a, b int) bool { return f.Workloads[a].Name < f.Workloads[b].Name })
	sort.Slice(f.Pods, func(a, b int) bool { return f.Pods[a].Name < f.Pods[b].Name })
	f.Containers = []contract.LogFacetContainer{{Name: "app", Count: len(pods)}}
	return f, nil
}

/* ── Alerts ─────────────────────────────────────────────────────────────── */

// GroupingRule은 화면에 노출되는 중복 묶음 기준입니다.
const GroupingRule = "alertname + namespace + workload"

var alertNames = []struct {
	name     string
	severity string
	summary  string
}{
	{"PodCrashLooping", "critical", "컨테이너가 반복해서 재시작합니다"},
	{"HighErrorRate", "critical", "5xx 비율이 임계치를 넘었습니다"},
	{"HighMemoryUsage", "warning", "메모리 사용률이 limit에 근접했습니다"},
	{"RolloutStuck", "warning", "롤아웃이 진행되지 않습니다"},
	{"CertificateExpiringSoon", "info", "인증서 만료가 다가옵니다"},
}

func (s *Source) List(_ context.Context, q datasource.AlertQuery) (datasource.AlertResult, error) {
	pods := s.pods(q.Target)
	res := datasource.AlertResult{GroupingRule: GroupingRule}
	if len(pods) == 0 {
		return res, nil
	}
	for i, p := range pods {
		if i >= 24 {
			break
		}
		spec := alertNames[i%len(alertNames)]
		startedAgo := time.Duration(5+int(noise("alert"+p.UID, i)*600)) * time.Minute
		start := q.Window.To.Add(-startedAgo)
		if start.Before(q.Window.From) {
			start = q.Window.From
		}
		a := contract.AlertInstance{
			ID:       fmt.Sprintf("%s-%s", strings.ToLower(spec.name), p.UID),
			Name:     spec.name,
			Severity: spec.severity,
			Status:   "firing",
			StartsAt: start.UTC().Format(time.RFC3339),
			Labels: map[string]string{
				"alertname": spec.name,
				"namespace": p.Namespace,
				"workload":  p.WorkloadName,
				"pod":       p.Name,
				"severity":  spec.severity,
			},
			Annotations: map[string]string{"summary": spec.summary},
			Entity: &contract.EntityRef{
				ClusterID:    q.Target.ClusterID,
				Namespace:    p.Namespace,
				WorkloadKind: p.WorkloadKind,
				WorkloadName: p.WorkloadName,
				PodName:      p.Name,
				PodUID:       p.UID,
			},
			EntityName: p.Name,
			Source:     "alertmanager",
			GroupSize:  1 + i%3,
			GroupKey:   fmt.Sprintf("%s/%s/%s", spec.name, p.Namespace, p.WorkloadName),
		}
		// 뒤쪽 절반은 이미 해소된 것으로 둡니다. 화면에서 두 탭이 같은 형식인지 확인할 수 있어야 합니다.
		if i%3 == 2 {
			a.Status = "resolved"
			end := start.Add(startedAgo / 2)
			a.EndsAt = end.UTC().Format(time.RFC3339)
			res.Resolved = append(res.Resolved, a)
			continue
		}
		res.Firing = append(res.Firing, a)
	}
	return res, nil
}

/* ── Topology ───────────────────────────────────────────────────────────── */

var protocols = []string{"HTTP", "gRPC", "TCP", "UDP"}

var httpRoutes = []string{"/api/v1/orders", "/api/v1/payments", "/api/v1/users", "/healthz"}
var grpcRoutes = []string{"payments.Charge", "auth.Verify", "ledger.Append"}
var tcpRoutes = []string{"tcp/5432", "tcp/6379", "tcp/9092"}
var udpRoutes = []string{"udp/53", "udp/8125"}

func routesFor(protocol string) []string {
	switch protocol {
	case "gRPC":
		return grpcRoutes
	case "TCP":
		return tcpRoutes
	case "UDP":
		return udpRoutes
	}
	return httpRoutes
}

func (s *Source) Graph(_ context.Context, t datasource.Target, w datasource.Window) (contract.TopologyGraph, error) {
	pods := s.pods(t)
	if len(pods) == 0 {
		return contract.TopologyGraph{}, nil
	}

	// 워크로드 단위로 접습니다. Pod를 전부 그리면 노드가 수백 개가 되어 읽을 수 없습니다.
	seen := map[string]datasource.CatalogPod{}
	order := make([]string, 0, len(pods))
	for _, p := range pods {
		k := p.Namespace + "/" + p.WorkloadName
		if p.WorkloadName == "" {
			k = p.Namespace + "/" + p.Name
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = p
		order = append(order, k)
	}
	sort.Strings(order)
	if len(order) > 12 {
		order = order[:12]
	}

	g := contract.TopologyGraph{}
	// 열 수를 고정하면 노드가 적을 때 한 열에 몰려 엣지가 하나도 생기지 않습니다.
	// 이웃한 노드가 서로 다른 열에 오도록 열을 먼저 채웁니다.
	const columns = 4
	for i, k := range order {
		p := seen[k]
		g.Nodes = append(g.Nodes, contract.TopologyNode{
			ID:        p.UID,
			Ref:       contract.EntityRef{ClusterID: t.ClusterID, Namespace: p.Namespace, WorkloadKind: p.WorkloadKind, WorkloadName: p.WorkloadName, PodName: p.Name, PodUID: p.UID},
			Name:      p.Name,
			Namespace: p.Namespace,
			Severity:  contract.SeverityHealthy,
			Column:    i % columns,
			Row:       i / columns,
		})
	}

	// A→B와 B→A를 **각각 별도의 엣지**로 만듭니다. 하나로 합치면 방향별 수치를 볼 수 없습니다.
	for i := 0; i+1 < len(g.Nodes); i++ {
		a, b := g.Nodes[i], g.Nodes[i+1]
		g.Edges = append(g.Edges, s.edge(t.ClusterID, a, b, w, i))
		g.Edges = append(g.Edges, s.edge(t.ClusterID, b, a, w, i+100))
	}
	// 한 칸 건너뛰는 호출 경로도 둡니다. 실제 그래프는 사슬 모양이 아닙니다.
	for i := 0; i+2 < len(g.Nodes); i += 2 {
		g.Edges = append(g.Edges, s.edge(t.ClusterID, g.Nodes[i], g.Nodes[i+2], w, i+200))
	}
	return g, nil
}

func (s *Source) edge(clusterID string, from, to contract.TopologyNode, w datasource.Window, i int) contract.TopologyEdge {
	proto := pick(protocols, clusterID+"/proto", i)
	total := 0
	errs := 0
	routes := make([]contract.TopologyRoute, 0, 4)
	for j, r := range routesFor(proto) {
		c := 200 + int(noise(clusterID+from.ID+to.ID+r, j)*40_000)
		e := int(float64(c) * noise(clusterID+r+"err", j) * 0.08)
		total += c
		errs += e
		routes = append(routes, contract.TopologyRoute{Protocol: proto, Route: r, Count: c, ErrorCount: e})
	}
	sort.Slice(routes, func(a, b int) bool { return routes[a].Count > routes[b].Count })

	rate := 0.0
	if total > 0 {
		rate = float64(errs) / float64(total)
	}
	sev := contract.SeverityHealthy
	switch {
	case rate > 0.05:
		sev = contract.SeverityCritical
	case rate > 0.02:
		sev = contract.SeverityDegraded
	case rate > 0.01:
		sev = contract.SeverityWarning
	}
	return contract.TopologyEdge{
		ID:         fmt.Sprintf("%s->%s", from.ID, to.ID),
		From:       from.ID,
		To:         to.ID,
		Severity:   sev,
		TotalCount: total,
		ErrorRate:  round4(rate),
		Protocols:  []string{proto},
		Routes:     routes,
	}
}

func (s *Source) EdgeSeries(_ context.Context, clusterID, edgeID string, w datasource.Window) ([]contract.TrendSeries, error) {
	n := buckets(w)
	total := make([]contract.TrendPoint, 0, n)
	errs := make([]contract.TrendPoint, 0, n)
	for i := 0; i < n; i++ {
		t := w.From.Add(time.Duration(i) * w.Step)
		bi := timeBucket(t, w.Step)
		v := 40 + noise(clusterID+edgeID, bi)*260
		total = append(total, contract.TrendPoint{T: t.UnixMilli(), V: round2(v)})
		errs = append(errs, contract.TrendPoint{T: t.UnixMilli(), V: round2(v * noise(clusterID+edgeID+"e", bi) * 0.06)})
	}
	return []contract.TrendSeries{
		{Key: "requests", Label: "요청", Unit: "count", Points: total},
		{Key: "errors", Label: "오류", Unit: "count", Points: errs},
	}, nil
}

/* ── 공통 ───────────────────────────────────────────────────────────────── */

func buckets(w datasource.Window) int {
	if w.Step <= 0 {
		return 0
	}
	n := int(w.To.Sub(w.From) / w.Step)
	if n < 1 {
		return 1
	}
	if n > 2000 {
		return 2000
	}
	return n
}

func containsLevel(list []contract.LogLevel, v contract.LogLevel) bool {
	for _, l := range list {
		if l == v {
			return true
		}
	}
	return false
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
func round2(v float64) float64        { return math.Round(v*100) / 100 }
func round4(v float64) float64        { return math.Round(v*10_000) / 10_000 }
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
