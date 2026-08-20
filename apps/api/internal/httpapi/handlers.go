package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/timerange"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/topologylayout"
)

// eventLimit은 화면에 싣는 이벤트 수 상한입니다. 전부 내려보내면 응답이 수 MB가 됩니다.
const eventLimit = 50

// unhealthyLimit은 Top N 목록 크기입니다.
const unhealthyLimit = 20

/* ── Scope ──────────────────────────────────────────────────────────────── */

func (s *Server) handleScope(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.ScopeResponse, error) {
		sc := scope.From(ctx)
		out := contract.ScopeResponse{
			Clusters:           make([]contract.ScopeCluster, 0, len(sc.Clusters)),
			CanManageWorkloads: sc.CanManageWorkloads && s.deps.KubeClient != nil,
		}
		for _, c := range sc.Clusters {
			cluster := contract.ScopeCluster{
				ID:         c.ID,
				Name:       c.Name,
				Namespaces: c.NamespacesJSON(),
				Accessible: c.Accessible(),
			}
			// 전체(all) scope는 계약이 이름을 열거하지 않아 셀렉터가 채울 실데이터가
			// 없었습니다 — informer 캐시에서 이름을 보충합니다. (#1)
			if c.All {
				cluster.AvailableNamespaces = s.availableNamespaces(c.ID)
			}
			out.Clusters = append(out.Clusters, cluster)
		}
		return out, nil
	})
}

// availableNamespaces enumerates selector options from the configured local
// informer/watch catalog. Direct mode falls back to Store for compatibility;
// central mode injects its bounded RemoteCatalog. Neither path calls Kubernetes
// or the cluster-state registry while handling this request. (#1)
func (s *Server) availableNamespaces(clusterID string) []string {
	catalog := s.deps.ScopeNamespaces
	if catalog == nil {
		catalog, _ = s.deps.Store.(clusterstate.NamespaceCatalog)
	}
	if catalog == nil {
		return nil
	}
	return catalog.NamespaceNames(clusterID)
}

/* ── Cluster Overview ───────────────────────────────────────────────────── */

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.ClusterOverviewResponse, error) {
		var out contract.ClusterOverviewResponse
		store := providerFrom(ctx, s.deps.Store)
		c, f, win, err := s.resolve(ctx, r, r.URL.Query().Get("namespace"))
		if err != nil {
			return out, err
		}
		target := dsTarget(c, f)

		out = contract.ClusterOverviewResponse{
			ClusterID:    c.ID,
			ClusterName:  c.Name,
			AppliedScope: contract.AppliedScope{ClusterID: c.ID, Namespaces: c.NamespacesJSON()},
			Range:        win.Contract(),
			GeneratedAt:  s.nowRFC3339(),
		}

		// 노드는 클러스터 스코프 리소스입니다. namespace로 제한된 사용자에게는
		// 0을 보여주는 대신 "권한 없음"으로 구분해 알려줍니다.
		if c.All {
			v1, err1 := store.NodeHealth()
			out.Nodes = kubeSection(store, v1, err1)
		} else {
			out.Nodes = contract.Forbidden[contract.NodeHealth]("노드 상태는 클러스터 범위 권한이 필요합니다.")
		}

		v2, err2 := store.PodHealth(f)
		out.Pods = kubeSection(store, v2, err2)
		v3, err3 := store.WorkloadHealth(f)
		out.Workloads = kubeSection(store, v3, err3)
		v4, err4 := store.Unhealthy(f, unhealthyLimit)
		out.Unhealthy = kubeSection(store, v4, err4)
		v5, err5 := store.Events(f, win.From, eventLimit)
		out.Events = kubeSection(store, v5, err5)

		panels, err := s.deps.Metrics.Trends(ctx, target, dsWindow(win), nil)
		out.Trends = dsSection(panels, err, contract.SourceGreptimeDB, "GreptimeDB")

		alerts, err := s.deps.Alerts.List(ctx, datasource.AlertQuery{Target: target, Window: dsWindow(win)})
		out.Alerts = dsSection(alertSummary(alerts), err, contract.SourceAlertmanager, "Alertmanager")

		graph, err := s.deps.Topology.Graph(ctx, target, dsWindow(win))
		out.Topology = dsSection(topologySummary(graph), err, contract.SourceGreptimeDB, "GreptimeDB")

		return out, nil
	})
}

func alertSummary(res datasource.AlertResult) contract.AlertSummary {
	sum := contract.AlertSummary{Top: []contract.AlertTop{}}
	for _, a := range res.Firing {
		switch a.Severity {
		case "critical":
			sum.BySeverity.Critical++
		case "warning":
			sum.BySeverity.Warning++
		default:
			sum.BySeverity.Info++
		}
	}
	firing := append([]contract.AlertInstance(nil), res.Firing...)
	sort.SliceStable(firing, func(a, b int) bool {
		return alertRank(firing[a].Severity) > alertRank(firing[b].Severity)
	})
	for i, a := range firing {
		if i >= 5 {
			break
		}
		sum.Top = append(sum.Top, contract.AlertTop{
			ID: a.ID, Name: a.Name, Severity: a.Severity,
			Namespace: a.Labels["namespace"], ActiveSince: a.StartsAt,
		})
	}
	return sum
}

func alertRank(s string) int {
	switch s {
	case "critical":
		return 3
	case "warning":
		return 2
	}
	return 1
}

func topologySummary(g contract.TopologyGraph) contract.TopologySummary {
	sum := contract.TopologySummary{Pods: len(g.Nodes), Edges: len(g.Edges), ProblemEdges: []contract.TopologyEdgeSummary{}}
	nameOf := map[string]string{}
	for _, n := range g.Nodes {
		nameOf[n.ID] = n.Name
	}
	for _, e := range g.Edges {
		if e.Severity == contract.SeverityHealthy {
			continue
		}
		proto := "HTTP"
		if len(e.Protocols) > 0 {
			proto = e.Protocols[0]
		}
		sum.ProblemEdges = append(sum.ProblemEdges, contract.TopologyEdgeSummary{
			From:              nameOf[e.From],
			To:                nameOf[e.To],
			Protocol:          proto,
			RequestsPerSecond: 0,
			ErrorRate:         e.ErrorRate,
			Severity:          e.Severity,
		})
		if len(sum.ProblemEdges) >= 5 {
			break
		}
	}
	return sum
}

/* ── Nodes ──────────────────────────────────────────────────────────────── */

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.NodeListResponse, error) {
		var out contract.NodeListResponse
		store := providerFrom(ctx, s.deps.Store)
		c, _, _, err := s.resolve(ctx, r, "")
		if err != nil {
			return out, err
		}
		out = contract.NodeListResponse{ClusterID: c.ID, GeneratedAt: s.nowRFC3339()}
		// 노드는 클러스터 스코프 리소스입니다 — namespace로 제한된 사용자에게는
		// 빈 목록 대신 "권한 없음"으로 구분해 알립니다. (Overview NodeHealth와 같은 규칙)
		if !c.All {
			out.Nodes = contract.Forbidden[[]contract.NodeSummary]("노드 목록은 클러스터 범위 권한이 필요합니다.")
			return out, nil
		}
		v, errN := store.NodeSummaries()
		out.Nodes = kubeSection(store, v, errN)
		return out, nil
	})
}

/* ── Namespace ──────────────────────────────────────────────────────────── */

func (s *Server) handleNamespaceList(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.NamespaceListResponse, error) {
		var out contract.NamespaceListResponse
		store := providerFrom(ctx, s.deps.Store)
		c, f, win, err := s.resolve(ctx, r, "")
		if err != nil {
			return out, err
		}
		out = contract.NamespaceListResponse{
			ClusterID:   c.ID,
			Range:       win.Contract(),
			GeneratedAt: s.nowRFC3339(),
		}
		v6, err6 := store.NamespaceSummaries(f)
		out.Namespaces = kubeSection(store, v6, err6)
		return out, nil
	})
}

func (s *Server) handleNamespaceDetail(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.NamespaceDetailResponse, error) {
		var out contract.NamespaceDetailResponse
		store := providerFrom(ctx, s.deps.Store)
		ns := r.PathValue("namespace")
		c, f, win, err := s.resolve(ctx, r, ns)
		if err != nil {
			return out, err
		}
		target := datasource.Target{ClusterID: c.ID, Namespace: ns}

		out = contract.NamespaceDetailResponse{
			ClusterID:   c.ID,
			Namespace:   ns,
			Range:       win.Contract(),
			GeneratedAt: s.nowRFC3339(),
		}

		summary, found, err := store.NamespaceSummary(ns)
		switch {
		case err != nil:
			out.Summary = contract.Degraded[contract.NamespaceSummary](contract.SourceKubernetes, "클러스터 상태를 읽지 못했습니다", nil)
		case !found:
			out.Summary = contract.Empty[contract.NamespaceSummary]()
		default:
			out.Summary = kubeSection(store, summary, nil)
		}

		v7, err7 := store.Workloads(f)
		out.Workloads = kubeSection(store, v7, err7)
		v8, err8 := store.Events(f, win.From, eventLimit)
		out.Events = kubeSection(store, v8, err8)

		panels, err := s.deps.Metrics.Trends(ctx, target, dsWindow(win), nil)
		out.Trends = dsSection(panels, err, contract.SourceGreptimeDB, "GreptimeDB")
		return out, nil
	})
}

/* ── Workload ───────────────────────────────────────────────────────────── */

func (s *Server) handleWorkloadDetail(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.WorkloadDetailResponse, error) {
		var out contract.WorkloadDetailResponse
		store := providerFrom(ctx, s.deps.Store)
		ns := r.URL.Query().Get("ns")
		kind, name := r.PathValue("kind"), r.PathValue("name")
		c, _, win, err := s.resolve(ctx, r, ns)
		if err != nil {
			return out, err
		}

		out = contract.WorkloadDetailResponse{
			ClusterID:   c.ID,
			Namespace:   ns,
			Range:       win.Contract(),
			GeneratedAt: s.nowRFC3339(),
		}

		wl, found, err := store.Workload(ns, kind, name)
		if err != nil || !found {
			out.Workload = contract.Empty[contract.WorkloadSummary]()
			out.OwnerChain = contract.Empty[[]contract.OwnerRef]()
			out.Pods = contract.Empty[[]contract.PodSummary]()
			out.Events = contract.Empty[[]contract.ClusterEvent]()
			out.Trends = contract.Empty[[]contract.TrendPanel]()
			return out, nil
		}

		out.Workload = kubeSection(store, wl, nil)
		out.OwnerChain = kubeSection(store, store.WorkloadOwnerChain(ns, kind, name, wl.Ref.WorkloadUID), nil)
		v9, err9 := store.PodsForWorkload(ns, kind, name, wl.Ref.WorkloadUID)
		out.Pods = kubeSection(store, v9, err9)
		v10, err10 := store.EventsForUID(wl.Ref.WorkloadUID, win.From, eventLimit)
		out.Events = kubeSection(store, v10, err10)

		target := datasource.Target{ClusterID: c.ID, Namespace: ns, WorkloadKind: kind, WorkloadName: name}
		panels, err := s.deps.Metrics.Trends(ctx, target, dsWindow(win), nil)
		out.Trends = dsSection(panels, err, contract.SourceGreptimeDB, "GreptimeDB")
		return out, nil
	})
}

/* ── Pod ────────────────────────────────────────────────────────────────── */

func (s *Server) handlePodDetail(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.PodDetailResponse, error) {
		var out contract.PodDetailResponse
		store := providerFrom(ctx, s.deps.Store)
		q := r.URL.Query()
		ns, uid, name := q.Get("ns"), q.Get("uid"), r.PathValue("name")
		c, _, win, err := s.resolve(ctx, r, ns)
		if err != nil {
			return out, err
		}

		out = contract.PodDetailResponse{
			ClusterID:   c.ID,
			Namespace:   ns,
			Range:       win.Contract(),
			GeneratedAt: s.nowRFC3339(),
		}

		pod, found, err := store.Pod(ns, name, uid)
		if err != nil || !found {
			out.Pod = contract.Empty[contract.PodSummary]()
			out.OwnerChain = contract.Empty[[]contract.OwnerRef]()
			out.Containers = contract.Empty[[]contract.ContainerStatus]()
			out.Events = contract.Empty[[]contract.ClusterEvent]()
			out.Trends = contract.Empty[[]contract.TrendPanel]()
			return out, nil
		}

		summary := store.PodSummary(pod)
		out.Pod = kubeSection(store, summary, nil)
		out.OwnerChain = kubeSection(store, store.PodOwnerChain(pod), nil)
		out.Containers = kubeSection(store, clusterstate.ContainerStatuses(pod), nil)
		v11, err11 := store.EventsForUID(summary.UID, win.From, eventLimit)
		out.Events = kubeSection(store, v11, err11)

		target := datasource.Target{ClusterID: c.ID, Namespace: ns, PodUID: summary.UID}
		panels, err := s.deps.Metrics.Trends(ctx, target, dsWindow(win), nil)
		out.Trends = dsSection(panels, err, contract.SourceGreptimeDB, "GreptimeDB")
		out.SecretRefs = secretNamesFromPod(pod) // 값 아님 — 이름만. (#33)
		return out, nil
	})
}

/* ── Logs ───────────────────────────────────────────────────────────────── */

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.LogSearchResponse, error) {
		var out contract.LogSearchResponse
		store := providerFrom(ctx, s.deps.Store)
		q := r.URL.Query()
		c, f, win, err := s.resolve(ctx, r, q.Get("ns"))
		if err != nil {
			return out, err
		}

		lq := datasource.LogQuery{
			Target: func() datasource.Target {
				t := dsTarget(c, f)
				t.WorkloadName = q.Get("workload")
				t.PodUID = q.Get("podUid")
				return t
			}(),
			Window:    dsWindow(win),
			Levels:    parseLevels(q.Get("levels")),
			Container: q.Get("container"),
			Text:      q.Get("q"),
			Cursor:    q.Get("cursor"),
		}

		page, searchErr := s.deps.Logs.Search(ctx, lq)
		out.Lines = dsSection(page.Lines, searchErr, contract.SourceQuickwit, "Quickwit")
		out.Cursor = contract.LogCursor{PageSize: len(page.Lines)}
		if page.Next != "" {
			next := page.Next
			out.Cursor.Next = &next
		}

		hist, err := s.deps.Logs.Histogram(ctx, lq)
		out.Histogram = dsSection(hist, err, contract.SourceQuickwit, "Quickwit")

		facets, err := s.deps.Logs.Facets(ctx, lq)
		out.Facets = dsSection(facets, err, contract.SourceQuickwit, "Quickwit")

		v12, err12 := store.Events(f, win.From, eventLimit)
		out.Events = kubeSection(store, v12, err12)

		var nsPtr *string
		if v := f.Single(); v != "" {
			nsPtr = &v
		}
		out.Applied = contract.LogApplied{
			ClusterID: c.ID,
			Namespace: nsPtr,
			From:      win.From.UTC().Format(time.RFC3339),
			To:        win.To.UTC().Format(time.RFC3339),
			Truncated: page.Truncated,
			MaxLines:  page.MaxLines,
		}
		out.GeneratedAt = s.nowRFC3339()
		return out, nil
	})
}

func parseLevels(v string) []contract.LogLevel {
	if v == "" {
		return nil
	}
	out := make([]contract.LogLevel, 0, 4)
	for _, p := range strings.Split(v, ",") {
		p = strings.ToUpper(strings.TrimSpace(p))
		switch contract.LogLevel(p) {
		case contract.LevelError, contract.LevelWarn, contract.LevelInfo, contract.LevelDebug:
			out = append(out, contract.LogLevel(p))
		}
	}
	return out
}

/* ── Topology ───────────────────────────────────────────────────────────── */

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.TopologyResponse, error) {
		var out contract.TopologyResponse
		store := providerFrom(ctx, s.deps.Store)
		c, f, win, err := s.resolve(ctx, r, r.URL.Query().Get("ns"))
		if err != nil {
			return out, err
		}
		var nsPtr *string
		if v := f.Single(); v != "" {
			nsPtr = &v
		}
		out = contract.TopologyResponse{
			ClusterID:   c.ID,
			Namespace:   nsPtr,
			Range:       win.Contract(),
			GeneratedAt: s.nowRFC3339(),
		}
		v13, err13 := store.TopologyPods(f, unhealthyLimit)
		out.Pods = kubeSection(store, v13, err13)

		graph, err := s.deps.Topology.Graph(ctx, dsTarget(c, f), dsWindow(win))
		out.Graph = dsSection(graph, err, contract.SourceGreptimeDB, "GreptimeDB")

		// 공유 배치는 표시 편의 데이터입니다 — 조회 실패는 화면을 degrade하지 않고
		// 기본 배치(null)로 내려갑니다. (#28)
		out.CanEditLayout = scope.From(ctx).CanEditTopology && s.deps.TopologyLayout != nil
		if s.deps.TopologyLayout != nil {
			if l, lerr := s.deps.TopologyLayout.Get(ctx, c.ID); lerr == nil {
				out.Layout = l
			}
		}
		return out, nil
	})
}

// handleTopologyLayoutPut은 공유 배치 저장입니다. platform.admin(또는 AUTH_MODE=none)만
// 허용하며, Scope 검증은 요청 파라미터가 아니라 서버가 해석한 Scope로만 합니다. (#28)
func (s *Server) handleTopologyLayoutPut(w http.ResponseWriter, r *http.Request) {
	sc := scope.From(r.Context())
	clusterID := r.PathValue("clusterId")
	var target *scope.Cluster
	for i := range sc.Clusters {
		if sc.Clusters[i].ID == clusterID {
			target = &sc.Clusters[i]
			break
		}
	}
	if target == nil || !target.Accessible() {
		writeError(w, r, http.StatusForbidden, "cluster_access_denied", "이 클러스터에 대한 권한이 없습니다.")
		return
	}
	if !sc.CanEditTopology {
		writeError(w, r, http.StatusForbidden, "forbidden", "토폴로지 배치를 저장할 권한이 없습니다.")
		return
	}
	if s.deps.TopologyLayout == nil {
		writeError(w, r, http.StatusServiceUnavailable, "layout_store_unavailable", "배치 저장소를 사용할 수 없습니다.")
		return
	}
	var body struct {
		Positions []contract.TopologyNodePosition `json:"positions"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_body", "요청 본문이 올바르지 않습니다.")
		return
	}
	layout, err := s.deps.TopologyLayout.Put(r.Context(), clusterID, body.Positions)
	if err != nil {
		if errors.Is(err, topologylayout.ErrInvalid) {
			writeError(w, r, http.StatusBadRequest, "invalid_layout", "배치 값이 허용 범위를 벗어났습니다.")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "layout_store_error", "배치를 저장하지 못했습니다.")
		return
	}
	writeJSON(w, http.StatusOK, layout)
}

func (s *Server) handleEdgeSeries(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.TopologyEdgeSeriesResponse, error) {
		var out contract.TopologyEdgeSeriesResponse
		c, _, win, err := s.resolve(ctx, r, r.URL.Query().Get("ns"))
		if err != nil {
			return out, err
		}
		edgeID := r.PathValue("edgeId")
		out = contract.TopologyEdgeSeriesResponse{
			EdgeID:      edgeID,
			Range:       win.Contract(),
			GeneratedAt: s.nowRFC3339(),
		}
		series, err := s.deps.Topology.EdgeSeries(ctx, c.ID, edgeID, dsWindow(win))
		out.Series = dsSection(series, err, contract.SourceGreptimeDB, "GreptimeDB")
		return out, nil
	})
}

/* ── Alerts ─────────────────────────────────────────────────────────────── */

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	serve(s, w, r, func(ctx context.Context) (contract.AlertListResponse, error) {
		var out contract.AlertListResponse
		c, f, win, err := s.resolve(ctx, r, r.URL.Query().Get("ns"))
		if err != nil {
			return out, err
		}
		out = contract.AlertListResponse{
			ClusterID:   c.ID,
			Range:       win.Contract(),
			GeneratedAt: s.nowRFC3339(),
		}

		res, err := s.deps.Alerts.List(ctx, datasource.AlertQuery{
			Target: dsTarget(c, f),
			Window: dsWindow(win),
		})
		out.Firing = dsSection(res.Firing, err, contract.SourceAlertmanager, "Alertmanager")
		historyErr := err
		if historyErr == nil {
			historyErr = res.HistoryErr
		}
		out.Resolved = alertHistorySection(res.Resolved, historyErr)
		out.Counts = alertHistorySection(alertCounts(res), historyErr)
		out.GroupingRule = res.GroupingRule
		return out, nil
	})
}

func alertHistorySection[T any](value T, err error) contract.Section[T] {
	if errors.Is(err, datasource.ErrAlertHistoryNotConfigured) {
		return contract.Degraded[T](contract.SourceAlertmanager, "history_not_configured", nil)
	}
	return dsSection(value, err, contract.SourceAlertmanager, "Alertmanager")
}

func alertCounts(res datasource.AlertResult) map[string]contract.AlertCount {
	counts := map[string]contract.AlertCount{
		"critical": {}, "warning": {}, "info": {},
	}
	for _, a := range res.Firing {
		c := counts[a.Severity]
		c.Firing++
		counts[a.Severity] = c
	}
	for _, a := range res.Resolved {
		c := counts[a.Severity]
		c.Resolved++
		counts[a.Severity] = c
	}
	return counts
}

/* ── 공통 ───────────────────────────────────────────────────────────────── */

// resolve는 모든 핸들러가 똑같이 하는 세 가지를 한 번에 처리합니다.
// 권한 확인 → namespace 교차 → 시간 범위 확정.
func (s *Server) resolve(ctx context.Context, r *http.Request, namespace string) (
	scope.Cluster, clusterstate.NamespaceFilter, timerange.Window, error,
) {
	c, err := s.authorize(ctx, r.PathValue("clusterId"))
	if err != nil {
		return scope.Cluster{}, clusterstate.NamespaceFilter{}, timerange.Window{}, err
	}
	f, err := namespaceFilter(c, namespace)
	if err != nil {
		return scope.Cluster{}, clusterstate.NamespaceFilter{}, timerange.Window{}, err
	}
	q := r.URL.Query()
	win, err := timerange.Parse(q.Get("range"), q.Get("from"), q.Get("to"), s.deps.Now())
	if err != nil {
		return scope.Cluster{}, clusterstate.NamespaceFilter{}, timerange.Window{},
			errBadRequest{"invalid_range", "조회할 수 없는 시간 범위입니다. Custom 범위는 최대 30일입니다."}
	}
	return c, f, win, nil
}

func (s *Server) nowRFC3339() string { return s.deps.Now().UTC().Format(time.RFC3339) }

func dsWindow(w timerange.Window) datasource.Window {
	return datasource.Window{From: w.From, To: w.To, Step: w.Step}
}

// dsTarget은 Scope가 확정한 namespace 범위를 데이터소스 Target에 싣습니다.
//
// f.Single()만 넘기면 "여러 namespace만 허용된 사용자"가 어댑터에게는
// 전체 허용처럼 보입니다. 허용 목록을 함께 실어야 어댑터가 질의에 강제할 수 있습니다.
func dsTarget(c scope.Cluster, f clusterstate.NamespaceFilter) datasource.Target {
	t := datasource.Target{ClusterID: c.ID, Namespace: f.Single()}
	if !f.All {
		t.Namespaces = append([]string(nil), f.List...)
	}
	return t
}
