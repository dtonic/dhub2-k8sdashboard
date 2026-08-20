package clusterstate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

var labelsEverything = labels.Everything()

// revisionAnnotation은 Deployment가 ReplicaSet에 남기는 세대 번호입니다.
// "현재 세대 / 이전 세대" 표시는 이 값 하나로 결정됩니다.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// UsageFunc는 Pod UID로 현재 사용량을 찾습니다. 사용량은 Kubernetes가 아니라
// 메트릭 데이터소스에서 옵니다. 없으면 request/limit만 채워집니다.
type UsageFunc func(podUID string) (contract.ContainerUsage, bool)

type usageProvider struct{ lookup UsageFunc }

// SetUsage는 사용량 조회원을 붙입니다. 붙이지 않으면 사용량은 0입니다.
func (s *Store) SetUsage(fn UsageFunc) {
	if fn == nil {
		s.usage.Store(nil)
		return
	}
	s.usage.Store(&usageProvider{lookup: fn})
}

// NamespaceFilter는 이 요청이 볼 수 있는 namespace입니다.
// **Scope에서 만들어진 값만 들어와야 합니다.** 요청 파라미터를 그대로 넣으면 안 됩니다.
type NamespaceFilter struct {
	All  bool
	List []string
}

func (f NamespaceFilter) Allows(ns string) bool {
	if f.All {
		return true
	}
	for _, n := range f.List {
		if n == ns {
			return true
		}
	}
	return false
}

// Single은 단일 namespace로 좁혀졌으면 그 이름을 돌려줍니다.
// lister를 namespace 단위로 호출해 전체 순회를 피하는 데 씁니다.
func (f NamespaceFilter) Single() string {
	if !f.All && len(f.List) == 1 {
		return f.List[0]
	}
	return ""
}

/* ── Overview 집계 ──────────────────────────────────────────────────────── */

// NodeHealth는 노드 상태 집계입니다. 노드는 클러스터 스코프이므로
// namespace로 제한된 사용자에게는 핸들러가 forbidden으로 내려보냅니다.
func (s *Store) NodeHealth() (contract.NodeHealth, error) {
	nodes, err := s.nodes.List(labelsEverything)
	if err != nil {
		return contract.NodeHealth{}, err
	}
	h := contract.NodeHealth{Total: len(nodes)}
	for _, n := range nodes {
		ready, pressure, unschedulable := NormalizeNode(n)
		if ready {
			h.Ready++
		} else {
			h.NotReady++
		}
		if pressure {
			h.Pressure++
		}
		if unschedulable {
			h.Unschedulable++
		}
	}
	return h, nil
}

// NodeSummaries는 Nodes 화면용 노드 목록입니다. 노드는 클러스터 스코프이므로
// 핸들러가 클러스터 전체 권한을 요구합니다(Overview의 NodeHealth와 같은 규칙).
// requested/limits와 pods 목록은 스케줄러 관점(종료되지 않은 Pod 전체)입니다.
func (s *Store) NodeSummaries() ([]contract.NodeSummary, error) {
	nodes, err := s.nodes.List(labelsEverything)
	if err != nil {
		return nil, err
	}
	pods, err := s.listPods("")
	if err != nil {
		return nil, err
	}
	now := s.now()

	byNode := map[string][]*corev1.Pod{}
	for _, p := range pods {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], p)
	}

	out := make([]contract.NodeSummary, 0, len(nodes))
	for _, n := range nodes {
		ready, pressure, unschedulable := NormalizeNode(n)
		sum := contract.NodeSummary{
			Name:           n.Name,
			Roles:          nodeRoles(n),
			Ready:          ready,
			Unschedulable:  unschedulable,
			Pressure:       pressure,
			Severity:       nodeSeverity(ready, pressure, unschedulable),
			KubeletVersion: n.Status.NodeInfo.KubeletVersion,
			OSImage:        n.Status.NodeInfo.OSImage,
			InternalIP:     nodeInternalIP(n),
			AgeSeconds:     int(now.Sub(n.CreationTimestamp.Time).Seconds()),
			Capacity:       nodeCapacity(n.Status.Capacity),
			Allocatable:    nodeCapacity(n.Status.Allocatable),
			Pods:           []contract.NodePodSummary{},
		}
		for _, p := range byNode[n.Name] {
			u := PodRequests(p)
			sum.Requested.CPUMilli += u.CPURequestMilli
			sum.Requested.MemoryMib += u.MemoryRequestMib
			// 노드 합계는 kubectl describe node처럼 컨테이너별로 존재하는 limit만 더합니다.
			for _, c := range p.Spec.Containers {
				sum.Limits.CPUMilli += int(c.Resources.Limits.Cpu().MilliValue())
				sum.Limits.MemoryMib += int(c.Resources.Limits.Memory().Value() / (1 << 20))
			}
			st := NormalizePod(p, now)
			sum.Pods = append(sum.Pods, contract.NodePodSummary{
				UID:              string(p.UID),
				Name:             p.Name,
				Namespace:        p.Namespace,
				Phase:            string(p.Status.Phase),
				Severity:         st.Severity,
				Restarts:         st.Restarts,
				CPURequestMilli:  u.CPURequestMilli,
				MemoryRequestMib: u.MemoryRequestMib,
			})
		}
		sum.PodsTotal = len(sum.Pods)
		sort.Slice(sum.Pods, func(a, b int) bool {
			ra, rb := severityRank(sum.Pods[a].Severity), severityRank(sum.Pods[b].Severity)
			if ra != rb {
				return ra > rb
			}
			if sum.Pods[a].Namespace != sum.Pods[b].Namespace {
				return sum.Pods[a].Namespace < sum.Pods[b].Namespace
			}
			return sum.Pods[a].Name < sum.Pods[b].Name
		})
		out = append(out, sum)
	}
	sort.Slice(out, func(a, b int) bool {
		ra, rb := severityRank(out[a].Severity), severityRank(out[b].Severity)
		if ra != rb {
			return ra > rb
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

// nodeSeverity는 노드 상태를 화면 심각도로 접습니다. NotReady가 최우선입니다.
func nodeSeverity(ready, pressure, unschedulable bool) contract.Severity {
	switch {
	case !ready:
		return contract.SeverityCritical
	case pressure:
		return contract.SeverityDegraded
	case unschedulable:
		return contract.SeverityWarning
	default:
		return contract.SeverityHealthy
	}
}

// nodeRoles는 node-role.kubernetes.io/<role> 라벨에서 역할 이름을 뽑습니다.
func nodeRoles(n *corev1.Node) []string {
	out := []string{}
	for k := range n.Labels {
		if role, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && role != "" {
			out = append(out, role)
		}
	}
	sort.Strings(out)
	return out
}

func nodeInternalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

func nodeCapacity(rl corev1.ResourceList) contract.NodeCapacity {
	return contract.NodeCapacity{
		CPUMilli:  int(rl.Cpu().MilliValue()),
		MemoryMib: int(rl.Memory().Value() / (1 << 20)),
		Pods:      int(rl.Pods().Value()),
	}
}

// PodHealth는 Pod 상태 집계입니다.
func (s *Store) PodHealth(f NamespaceFilter) (contract.PodHealth, error) {
	pods, err := s.scopedPods(f)
	if err != nil {
		return contract.PodHealth{}, err
	}
	now := s.now()
	h := contract.PodHealth{Total: len(pods)}
	for _, p := range pods {
		st := NormalizePod(p, now)
		h.Restarts += st.Restarts
		switch p.Status.Phase {
		case corev1.PodRunning:
			h.Running++
		case corev1.PodPending:
			h.Pending++
		case corev1.PodFailed:
			h.Failed++
		}
		if hasIssue(st.Issues, contract.IssueCrashLoopBackOff) {
			h.CrashLoopBackOff++
		}
		if hasIssue(st.Issues, contract.IssueImagePullBackOff) {
			h.ImagePullBackOff++
		}
	}
	return h, nil
}

// WorkloadHealth는 Workload 상태 집계입니다.
func (s *Store) WorkloadHealth(f NamespaceFilter) (contract.WorkloadHealth, error) {
	ws, err := s.Workloads(f)
	if err != nil {
		return contract.WorkloadHealth{}, err
	}
	h := contract.WorkloadHealth{Total: len(ws)}
	for _, w := range ws {
		if w.Severity == contract.SeverityHealthy {
			h.Available++
		}
		if hasIssue(w.Issues, contract.IssueReplicaMismatch) {
			h.ReplicaMismatch++
		}
		if hasIssue(w.Issues, contract.IssueRolloutStalled) {
			h.RolloutStalled++
		}
	}
	return h, nil
}

// Unhealthy는 이상 엔티티 목록입니다. 심각도 → 지속 시간 순으로 정렬합니다.
// 사용자는 "가장 나쁜 것"과 "가장 오래된 것"을 먼저 봐야 합니다.
func (s *Store) Unhealthy(f NamespaceFilter, limit int) ([]contract.UnhealthyEntity, error) {
	pods, err := s.scopedPods(f)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]contract.UnhealthyEntity, 0, 16)
	for _, p := range pods {
		st := NormalizePod(p, now)
		if st.Severity == contract.SeverityHealthy {
			continue
		}
		kind, name, uid := s.workloadOfPod(p)
		out = append(out, contract.UnhealthyEntity{
			Ref:        s.podRef(p, kind, name, uid),
			Name:       p.Name,
			Kind:       "Pod",
			Namespace:  p.Namespace,
			Severity:   st.Severity,
			Reason:     firstNonEmpty(st.Reason, "이상"),
			Restarts:   st.Restarts,
			ForSeconds: int(now.Sub(st.Since).Seconds()),
		})
	}

	ws, err := s.Workloads(f)
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		if w.Severity == contract.SeverityHealthy || len(w.Issues) == 0 {
			continue
		}
		// Pod 단위로 이미 드러난 문제는 중복해서 보여주지 않습니다.
		if !hasIssue(w.Issues, contract.IssueReplicaMismatch) && !hasIssue(w.Issues, contract.IssueRolloutStalled) {
			continue
		}
		out = append(out, contract.UnhealthyEntity{
			Ref:        w.Ref,
			Name:       w.Name,
			Kind:       "Workload",
			Namespace:  w.Namespace,
			Severity:   w.Severity,
			Reason:     workloadReason(w),
			Restarts:   w.Restarts,
			ForSeconds: w.AgeSeconds,
		})
	}

	sort.SliceStable(out, func(a, b int) bool {
		sa, sb := severityRank(out[a].Severity), severityRank(out[b].Severity)
		if sa != sb {
			return sa > sb
		}
		return out[a].ForSeconds > out[b].ForSeconds
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func workloadReason(w contract.WorkloadSummary) string {
	if hasIssue(w.Issues, contract.IssueRolloutStalled) {
		return "Rollout 지연"
	}
	return fmt.Sprintf("Replica %d/%d", w.Replicas.Ready, w.Replicas.Desired)
}

func severityRank(s contract.Severity) int {
	switch s {
	case contract.SeverityCritical:
		return 5
	case contract.SeverityDegraded:
		return 4
	case contract.SeverityWarning:
		return 3
	case contract.SeverityProgressing:
		return 2
	case contract.SeverityUnknown:
		return 1
	}
	return 0
}

/* ── Events ─────────────────────────────────────────────────────────────── */

// Events는 범위 안의 이벤트를 최신순으로 돌려줍니다.
func (s *Store) Events(f NamespaceFilter, since time.Time, limit int) ([]contract.ClusterEvent, error) {
	var (
		evs []*corev1.Event
		err error
	)
	if ns := f.Single(); ns != "" {
		evs, err = s.events.Events(ns).List(labelsEverything)
	} else {
		evs, err = s.events.List(labelsEverything)
	}
	if err != nil {
		return nil, err
	}
	out := make([]contract.ClusterEvent, 0, limit)
	for _, e := range evs {
		if !f.Allows(e.Namespace) {
			continue
		}
		last := eventTime(e)
		if last.Before(since) {
			continue
		}
		out = append(out, s.toClusterEvent(e, last))
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].LastSeen != out[b].LastSeen {
			return out[a].LastSeen > out[b].LastSeen
		}
		return out[a].ID < out[b].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// EventsForUID는 특정 객체에 붙은 이벤트만 인덱스로 찾아옵니다.
func (s *Store) EventsForUID(uid string, since time.Time, limit int) ([]contract.ClusterEvent, error) {
	objs, err := s.eventIndexer.ByIndex(IndexEventByInvolved, uid)
	if err != nil {
		return nil, err
	}
	out := make([]contract.ClusterEvent, 0, len(objs))
	for _, o := range objs {
		e, ok := o.(*corev1.Event)
		if !ok {
			continue
		}
		last := eventTime(e)
		if last.Before(since) {
			continue
		}
		out = append(out, s.toClusterEvent(e, last))
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].LastSeen != out[b].LastSeen {
			return out[a].LastSeen > out[b].LastSeen
		}
		return out[a].ID < out[b].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) toClusterEvent(e *corev1.Event, last time.Time) contract.ClusterEvent {
	ref := contract.EntityRef{ClusterID: s.opts.ClusterID, Namespace: e.Namespace}
	switch e.InvolvedObject.Kind {
	case "Pod":
		ref.PodName = e.InvolvedObject.Name
		ref.PodUID = string(e.InvolvedObject.UID)
	default:
		ref.WorkloadKind = e.InvolvedObject.Kind
		ref.WorkloadName = e.InvolvedObject.Name
		ref.WorkloadUID = string(e.InvolvedObject.UID)
	}
	count := int(e.Count)
	if count == 0 {
		count = 1
	}
	return contract.ClusterEvent{
		ID:           string(e.UID),
		Type:         firstNonEmpty(e.Type, "Normal"),
		Reason:       e.Reason,
		Message:      e.Message,
		Involved:     ref,
		InvolvedName: e.InvolvedObject.Name,
		Namespace:    e.Namespace,
		Count:        count,
		LastSeen:     last.UTC().Format(time.RFC3339),
	}
}

// eventTime은 구/신 Event API 필드를 모두 고려한 마지막 발생 시각입니다.
func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	if !e.FirstTimestamp.IsZero() {
		return e.FirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

/* ── Namespace ──────────────────────────────────────────────────────────── */

// NamespaceSummaries는 Scope 안의 namespace 요약입니다.
func (s *Store) NamespaceSummaries(f NamespaceFilter) ([]contract.NamespaceSummary, error) {
	namespaces, err := s.namespaces.List(labelsEverything)
	if err != nil {
		return nil, err
	}
	pods, err := s.scopedPods(f)
	if err != nil {
		return nil, err
	}
	workloads, err := s.Workloads(f)
	if err != nil {
		return nil, err
	}

	now := s.now()
	byNS := map[string]*contract.NamespaceSummary{}
	get := func(ns string) *contract.NamespaceSummary {
		if v, ok := byNS[ns]; ok {
			return v
		}
		v := &contract.NamespaceSummary{Name: ns, Severity: contract.SeverityHealthy}
		byNS[ns] = v
		return v
	}
	for _, namespace := range namespaces {
		if f.Allows(namespace.Name) {
			get(namespace.Name)
		}
	}

	for _, p := range pods {
		v := get(p.Namespace)
		st := NormalizePod(p, now)
		v.Pods.Total++
		v.Pods.Restarts += st.Restarts
		switch p.Status.Phase {
		case corev1.PodRunning:
			v.Pods.Running++
		case corev1.PodPending:
			v.Pods.Pending++
		case corev1.PodFailed:
			v.Pods.Failed++
		}
		v.Severity = contract.WorseOf(v.Severity, st.Severity)
		v.Issues = mergeIssues(v.Issues, st.Issues)
		v.Usage.Add(s.podUsage(p))
	}

	for _, w := range workloads {
		v := get(w.Namespace)
		v.Workloads.Total++
		if w.Severity != contract.SeverityHealthy {
			v.Workloads.Unhealthy++
		}
		v.Severity = contract.WorseOf(v.Severity, w.Severity)
		v.Issues = mergeIssues(v.Issues, w.Issues)
	}

	out := make([]contract.NamespaceSummary, 0, len(byNS))
	for _, v := range byNS {
		v.Usage.Normalize()
		if v.Issues == nil {
			// 계약은 배열입니다 — nil이 JSON null로 나가면 UI의 필터/렌더가 깨집니다.
			v.Issues = []contract.IssueReason{}
		}
		sortIssues(v.Issues)
		out = append(out, *v)
	}
	sort.Slice(out, func(a, b int) bool {
		ra, rb := severityRank(out[a].Severity), severityRank(out[b].Severity)
		if ra != rb {
			return ra > rb
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

// NamespaceSummary는 단일 namespace 요약입니다.
func (s *Store) NamespaceSummary(ns string) (contract.NamespaceSummary, bool, error) {
	list, err := s.NamespaceSummaries(NamespaceFilter{List: []string{ns}})
	if err != nil {
		return contract.NamespaceSummary{}, false, err
	}
	if len(list) == 0 {
		return contract.NamespaceSummary{}, false, nil
	}
	return list[0], true, nil
}

/* ── Workload ───────────────────────────────────────────────────────────── */

// Workloads는 Scope 안의 모든 워크로드를 정규화해 돌려줍니다.
func (s *Store) Workloads(f NamespaceFilter) ([]contract.WorkloadSummary, error) {
	ns := f.Single()
	now := s.now()
	out := make([]contract.WorkloadSummary, 0, 32)

	deps, err := s.listDeployments(ns)
	if err != nil {
		return nil, err
	}
	for _, d := range deps {
		if !f.Allows(d.Namespace) {
			continue
		}
		w := s.baseWorkload(d.ObjectMeta, "Deployment", images(d.Spec.Template.Spec.Containers), now)
		s.applyState(&w, NormalizeDeployment(d))
		out = append(out, w)
	}

	stss, err := s.listStatefulSets(ns)
	if err != nil {
		return nil, err
	}
	for _, x := range stss {
		if !f.Allows(x.Namespace) {
			continue
		}
		w := s.baseWorkload(x.ObjectMeta, "StatefulSet", images(x.Spec.Template.Spec.Containers), now)
		s.applyState(&w, NormalizeStatefulSet(x))
		out = append(out, w)
	}

	dss, err := s.listDaemonSets(ns)
	if err != nil {
		return nil, err
	}
	for _, x := range dss {
		if !f.Allows(x.Namespace) {
			continue
		}
		w := s.baseWorkload(x.ObjectMeta, "DaemonSet", images(x.Spec.Template.Spec.Containers), now)
		s.applyState(&w, NormalizeDaemonSet(x))
		out = append(out, w)
	}

	cjs, err := s.listCronJobs(ns)
	if err != nil {
		return nil, err
	}
	for _, x := range cjs {
		if !f.Allows(x.Namespace) {
			continue
		}
		w := s.baseWorkload(x.ObjectMeta, "CronJob", images(x.Spec.JobTemplate.Spec.Template.Spec.Containers), now)
		s.applyState(&w, NormalizeCronJob(x))
		out = append(out, w)
	}

	sort.Slice(out, func(a, b int) bool {
		ra, rb := severityRank(out[a].Severity), severityRank(out[b].Severity)
		if ra != rb {
			return ra > rb
		}
		if out[a].Namespace != out[b].Namespace {
			return out[a].Namespace < out[b].Namespace
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

// Workload는 단일 워크로드입니다.
func (s *Store) Workload(ns, kind, name string) (contract.WorkloadSummary, bool, error) {
	list, err := s.Workloads(NamespaceFilter{List: []string{ns}})
	if err != nil {
		return contract.WorkloadSummary{}, false, err
	}
	for _, w := range list {
		if w.Kind == kind && w.Name == name {
			return w, true, nil
		}
	}
	return contract.WorkloadSummary{}, false, nil
}

func (s *Store) baseWorkload(m metav1.ObjectMeta, kind string, imgs []string, now time.Time) contract.WorkloadSummary {
	return contract.WorkloadSummary{
		Ref: contract.EntityRef{
			ClusterID:    s.opts.ClusterID,
			Namespace:    m.Namespace,
			WorkloadKind: kind,
			WorkloadName: m.Name,
			WorkloadUID:  string(m.UID),
		},
		Name:       m.Name,
		Kind:       kind,
		Namespace:  m.Namespace,
		Severity:   contract.SeverityHealthy,
		Images:     imgs,
		Issues:     []contract.IssueReason{},
		AgeSeconds: int(now.Sub(m.CreationTimestamp.Time).Seconds()),
	}
}

// applyState는 정규화 결과와 Pod 단위 사실(재시작, 사용량)을 워크로드에 합칩니다.
func (s *Store) applyState(w *contract.WorkloadSummary, st WorkloadState) {
	w.Replicas = st.Replicas
	w.Rollout = st.Rollout
	w.Severity = contract.WorseOf(w.Severity, st.Severity)
	w.Issues = mergeIssues(w.Issues, st.Issues)

	now := s.now()
	for _, p := range s.podsOfWorkload(w.Namespace, w.Kind, w.Name, w.Ref.WorkloadUID) {
		ps := NormalizePod(p, now)
		w.Restarts += ps.Restarts
		w.Severity = contract.WorseOf(w.Severity, ps.Severity)
		w.Issues = mergeIssues(w.Issues, ps.Issues)
		w.Usage.Add(s.podUsage(p))
	}
	w.Usage.Normalize()
	sortIssues(w.Issues)
}

/* ── Pod ────────────────────────────────────────────────────────────────── */

// PodsForWorkload는 워크로드에 속한 Pod 목록입니다.
func (s *Store) PodsForWorkload(ns, kind, name, uid string) ([]contract.PodSummary, error) {
	pods := s.podsOfWorkload(ns, kind, name, uid)
	now := s.now()
	out := make([]contract.PodSummary, 0, len(pods))
	for _, p := range pods {
		out = append(out, s.podSummary(p, kind, name, uid, now))
	}
	sort.Slice(out, func(a, b int) bool {
		ra, rb := severityRank(out[a].Severity), severityRank(out[b].Severity)
		if ra != rb {
			return ra > rb
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

// Pod는 이름과 **UID**로 Pod를 찾습니다.
//
// UID가 오면 이름이 같아도 다른 인스턴스는 걸러냅니다. 재생성된 Pod를
// 같은 것으로 취급하면 "재시작 0회인데 로그에 크래시가 있는" 화면이 나옵니다. (README §5)
func (s *Store) Pod(ns, name, uid string) (*corev1.Pod, bool, error) {
	if uid != "" {
		pods, err := s.pods.Pods(ns).List(labelsEverything)
		if err != nil {
			return nil, false, err
		}
		for _, p := range pods {
			if string(p.UID) == uid {
				return p, true, nil
			}
		}
		return nil, false, nil
	}
	p, err := s.pods.Pods(ns).Get(name)
	if err != nil {
		return nil, false, nil
	}
	return p, true, nil
}

// PodSummary는 Pod 하나를 화면 계약으로 바꿉니다.
func (s *Store) PodSummary(p *corev1.Pod) contract.PodSummary {
	kind, name, uid := s.workloadOfPod(p)
	return s.podSummary(p, kind, name, uid, s.now())
}

func (s *Store) podSummary(p *corev1.Pod, workloadKind, workloadName, workloadUID string, now time.Time) contract.PodSummary {
	st := NormalizePod(p, now)
	usage := s.podUsage(p)
	usage.Normalize()

	ps := contract.PodSummary{
		Ref:       s.podRef(p, workloadKind, workloadName, workloadUID),
		Name:      p.Name,
		UID:       string(p.UID),
		Namespace: p.Namespace,
		Phase:     string(p.Status.Phase),
		Severity:  st.Severity,
		Ready:     st.ReadyText,
		Restarts:  st.Restarts,
		Node:      p.Spec.NodeName,
		Issues:    st.Issues,
		Usage:     usage,
		StartedAt: p.CreationTimestamp.UTC().Format(time.RFC3339),
	}
	if ps.Issues == nil {
		ps.Issues = []contract.IssueReason{}
	}
	if p.Status.StartTime != nil {
		ps.StartedAt = p.Status.StartTime.UTC().Format(time.RFC3339)
	}
	if p.DeletionTimestamp != nil {
		ps.FinishedAt = p.DeletionTimestamp.UTC().Format(time.RFC3339)
	}
	if o := directOwner(p.OwnerReferences); o != nil {
		ps.Owner = &contract.OwnerRef{Kind: o.Kind, Name: o.Name, UID: string(o.UID)}
	}
	return ps
}

// PodOwnerChain은 Pod → ReplicaSet → Deployment 순서의 상위 체인입니다.
func (s *Store) PodOwnerChain(p *corev1.Pod) []contract.OwnerRef {
	out := []contract.OwnerRef{}
	o := directOwner(p.OwnerReferences)
	if o == nil {
		return out
	}
	out = append(out, contract.OwnerRef{
		Kind: o.Kind, Name: o.Name, UID: string(o.UID), Current: true,
		Pods: len(s.podsByOwnerUID(string(o.UID))),
	})
	if o.Kind != "ReplicaSet" {
		return out
	}
	rs, ok := s.replicaSetMeta(p.Namespace, o.Name)
	if !ok {
		return out
	}
	out[0].Revision = rs.Annotations[revisionAnnotation]
	if top := directOwner(rs.OwnerReferences); top != nil {
		out = append(out, contract.OwnerRef{Kind: top.Kind, Name: top.Name, UID: string(top.UID), Current: true})
	}
	return out
}

// WorkloadOwnerChain은 Deployment → ReplicaSet 체인입니다.
//
// 롤아웃 중에는 ReplicaSet이 여러 개 공존합니다. 이때 **어느 것이 현재 세대인지**를
// revision 애노테이션으로 정해 화면에 그대로 표시합니다. (이슈 #15 완료 기준)
// ReplicaSet은 metadata-only informer에서 오므로 spec/status를 받지 않습니다.
func (s *Store) WorkloadOwnerChain(ns, kind, name, uid string) []contract.OwnerRef {
	out := []contract.OwnerRef{}
	if kind != "Deployment" || uid == "" {
		return out
	}
	objs, err := s.rsIndexer.ByIndex(IndexReplicaSetByOwner, uid)
	if err != nil {
		return out
	}
	type rsInfo struct {
		meta *metav1.PartialObjectMetadata
		rev  int
	}
	list := make([]rsInfo, 0, len(objs))
	for _, o := range objs {
		m, ok := o.(*metav1.PartialObjectMetadata)
		if !ok {
			continue
		}
		rev, _ := strconv.Atoi(m.Annotations[revisionAnnotation])
		list = append(list, rsInfo{meta: m, rev: rev})
	}
	sort.Slice(list, func(a, b int) bool { return list[a].rev > list[b].rev })

	for i, r := range list {
		pods := s.podsByOwnerUID(string(r.meta.UID))
		// Pod가 하나도 없는 옛 세대는 화면을 채우기만 하고 정보가 없습니다.
		if i > 0 && len(pods) == 0 {
			continue
		}
		out = append(out, contract.OwnerRef{
			Kind:     "ReplicaSet",
			Name:     r.meta.Name,
			UID:      string(r.meta.UID),
			Current:  i == 0,
			Pods:     len(pods),
			Revision: r.meta.Annotations[revisionAnnotation],
		})
	}
	return out
}

/* ── Topology 보조 ──────────────────────────────────────────────────────── */

// TopologyPods는 토폴로지 헤더에 쓰는 Pod 수와 비정상 목록입니다.
func (s *Store) TopologyPods(f NamespaceFilter, limit int) (contract.TopologyPods, error) {
	pods, err := s.scopedPods(f)
	if err != nil {
		return contract.TopologyPods{}, err
	}
	unhealthy, err := s.Unhealthy(f, limit)
	if err != nil {
		return contract.TopologyPods{}, err
	}
	onlyPods := make([]contract.UnhealthyEntity, 0, len(unhealthy))
	for _, u := range unhealthy {
		if u.Kind == "Pod" {
			onlyPods = append(onlyPods, u)
		}
	}
	now := s.now()
	bad := 0
	for _, p := range pods {
		if NormalizePod(p, now).Severity != contract.SeverityHealthy {
			bad++
		}
	}
	return contract.TopologyPods{
		Total:         len(pods),
		Healthy:       len(pods) - bad,
		Unhealthy:     bad,
		UnhealthyList: onlyPods,
	}, nil
}

/* ── 내부 헬퍼 ──────────────────────────────────────────────────────────── */

func (s *Store) scopedPods(f NamespaceFilter) ([]*corev1.Pod, error) {
	pods, err := s.listPods(f.Single())
	if err != nil {
		return nil, err
	}
	if f.All {
		return pods, nil
	}
	out := make([]*corev1.Pod, 0, len(pods))
	for _, p := range pods {
		if f.Allows(p.Namespace) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Store) podsByOwnerUID(uid string) []*corev1.Pod {
	objs, err := s.podIndexer.ByIndex(IndexPodByOwner, uid)
	if err != nil {
		return nil
	}
	out := make([]*corev1.Pod, 0, len(objs))
	for _, o := range objs {
		if p, ok := o.(*corev1.Pod); ok {
			out = append(out, p)
		}
	}
	return out
}

// podsOfWorkload는 워크로드에 속한 Pod를 인덱스로 찾습니다.
// Deployment는 ReplicaSet을 한 단계 거칩니다.
func (s *Store) podsOfWorkload(ns, kind, name, uid string) []*corev1.Pod {
	if uid == "" {
		return nil
	}
	if kind != "Deployment" {
		return s.podsByOwnerUID(uid)
	}
	objs, err := s.rsIndexer.ByIndex(IndexReplicaSetByOwner, uid)
	if err != nil {
		return nil
	}
	out := make([]*corev1.Pod, 0, 8)
	for _, o := range objs {
		m, ok := o.(*metav1.PartialObjectMetadata)
		if !ok {
			continue
		}
		out = append(out, s.podsByOwnerUID(string(m.UID))...)
	}
	return out
}

func (s *Store) replicaSetMeta(ns, name string) (*metav1.PartialObjectMetadata, bool) {
	obj, exists, err := s.rsIndexer.GetByKey(ns + "/" + name)
	if err != nil || !exists {
		return nil, false
	}
	m, ok := obj.(*metav1.PartialObjectMetadata)
	return m, ok
}

// workloadOfPod는 Pod가 어느 워크로드에 속하는지 owner 체인에서 찾습니다.
// ReplicaSet은 Deployment의 구현 세부사항이므로 화면에는 Deployment로 올려서 보여줍니다.
// UID도 함께 돌려줘 Pod EntityRef가 이름이 아니라 Workload UID로 상관될 수 있게 합니다. (이슈 #4)
func (s *Store) workloadOfPod(p *corev1.Pod) (kind, name, uid string) {
	o := directOwner(p.OwnerReferences)
	if o == nil {
		return "", "", ""
	}
	if o.Kind != "ReplicaSet" {
		return o.Kind, o.Name, string(o.UID)
	}
	rs, ok := s.replicaSetMeta(p.Namespace, o.Name)
	if !ok {
		return "ReplicaSet", o.Name, string(o.UID)
	}
	if top := directOwner(rs.OwnerReferences); top != nil {
		return top.Kind, top.Name, string(top.UID)
	}
	return "ReplicaSet", o.Name, string(o.UID)
}

func (s *Store) podRef(p *corev1.Pod, workloadKind, workloadName, workloadUID string) contract.EntityRef {
	return contract.EntityRef{
		ClusterID:    s.opts.ClusterID,
		Namespace:    p.Namespace,
		WorkloadKind: workloadKind,
		WorkloadName: workloadName,
		WorkloadUID:  workloadUID,
		PodName:      p.Name,
		PodUID:       string(p.UID),
	}
}

func (s *Store) podUsage(p *corev1.Pod) contract.ResourceUsage {
	u := PodRequests(p)
	if provider := s.usage.Load(); provider != nil {
		if v, ok := provider.lookup(string(p.UID)); ok {
			u.CPUMilli = v.CPUMilli
			u.MemoryMib = v.MemoryMib
		}
	}
	return u
}

// directOwner는 controller ownerReference를 고릅니다. 없으면 첫 번째를 씁니다.
func directOwner(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}

func images(cs []corev1.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Image)
	}
	return out
}

func mergeIssues(dst, src []contract.IssueReason) []contract.IssueReason {
	for _, i := range src {
		if !hasIssue(dst, i) {
			dst = append(dst, i)
		}
	}
	return dst
}

// ShortName은 화면 라벨용으로 긴 이름을 줄입니다.
func ShortName(name string, max int) string {
	if len(name) <= max {
		return name
	}
	return strings.TrimRight(name[:max-1], "-") + "…"
}
