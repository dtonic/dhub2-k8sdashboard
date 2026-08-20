package clusterstate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// ScreenProjection is the bounded registry-owned result of one screen query.
// It contains no informer object and cannot trigger another RPC.
type ScreenProjection struct {
	Request         ScreenRequest               `json:"request"`
	ResolvedUID     string                      `json:"resolvedUid,omitempty"`
	Nodes           *contract.NodeHealth        `json:"nodes,omitempty"`
	PodsHealth      *contract.PodHealth         `json:"podsHealth,omitempty"`
	WorkloadsHealth *contract.WorkloadHealth    `json:"workloadsHealth,omitempty"`
	Unhealthy       []contract.UnhealthyEntity  `json:"unhealthy,omitempty"`
	EventsList      []contract.ClusterEvent     `json:"events,omitempty"`
	Namespaces      []contract.NamespaceSummary `json:"namespaces,omitempty"`
	Namespace       *contract.NamespaceSummary  `json:"namespace,omitempty"`
	WorkloadsList   []contract.WorkloadSummary  `json:"workloads,omitempty"`
	WorkloadValue   *contract.WorkloadSummary   `json:"workload,omitempty"`
	PodsList        []contract.PodSummary       `json:"workloadPods,omitempty"`
	PodValue        *corev1.Pod                 `json:"pod,omitempty"`
	PodSummaryValue *contract.PodSummary        `json:"podSummary,omitempty"`
	PodOwners       []contract.OwnerRef         `json:"podOwners,omitempty"`
	WorkloadOwners  []contract.OwnerRef         `json:"workloadOwners,omitempty"`
	Topology        *contract.TopologyPods      `json:"topology,omitempty"`
}

type projectedPod struct {
	uid, namespace, name string
	owners               []*v1.OwnerRef
	pod                  *v1.PodProjection
	s                    PodState
}

func (p projectedPod) resource() *v1.Resource {
	return &v1.Resource{Kind: v1.KindPod, Uid: p.uid, Namespace: p.namespace, Name: p.name, Owners: p.owners, Pod: p.pod}
}

// ProjectScreen computes the complete Kubernetes portion of one HTTP screen
// while the registry holds the cluster read lock. The returned value owns all
// of its data and does not retain resource-map references.
func ProjectScreen(req ScreenRequest, resources map[string]*v1.Resource, now time.Time) (*ScreenProjection, error) {
	return ProjectScreenContext(context.Background(), req, resources, now)
}

func ProjectScreenContext(ctx context.Context, req ScreenRequest, resources map[string]*v1.Resource, now time.Time) (*ScreenProjection, error) {
	if req.EventLimit < 1 || req.EventLimit > 1000 || req.UnhealthyLimit < 1 || req.UnhealthyLimit > 1000 {
		return nil, fmt.Errorf("invalid screen limit")
	}
	out := &ScreenProjection{Request: req, Nodes: &contract.NodeHealth{}, PodsHealth: &contract.PodHealth{}, WorkloadsHealth: &contract.WorkloadHealth{}, Unhealthy: []contract.UnhealthyEntity{}, EventsList: []contract.ClusterEvent{}, Namespaces: []contract.NamespaceSummary{}, WorkloadsList: []contract.WorkloadSummary{}, PodsList: []contract.PodSummary{}, PodOwners: []contract.OwnerRef{}, WorkloadOwners: []contract.OwnerRef{}}
	needNodes := req.Screen == "overview"
	needPods := req.Screen != "logs"
	needWorkloads := req.Screen == "overview" || req.Screen == "namespace-list" || req.Screen == "namespace" || req.Screen == "workload"
	needOwners := req.Screen == "overview" || req.Screen == "namespace-list" || req.Screen == "namespace" || req.Screen == "workload" || req.Screen == "pod" || req.Screen == "topology"
	needEvents := req.Screen == "overview" || req.Screen == "namespace" || req.Screen == "workload" || req.Screen == "pod" || req.Screen == "logs"
	needNamespaces := req.Screen == "namespace-list" || req.Screen == "namespace"
	rs := map[string]*v1.Resource{}
	workloads := map[string]*v1.Resource{}
	targetUID := req.EntityUID
	targetOwnerUID := ""
	podCount := 0
	allScopedPods := req.Screen == "overview" || req.Screen == "namespace-list" || req.Screen == "namespace" || req.Screen == "topology"
	i := 0
	for _, x := range resources {
		i++
		if i&511 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if x == nil {
			continue
		}
		if needOwners && x.Kind == v1.KindReplicaSet {
			rs[x.Uid] = &v1.Resource{Kind: x.Kind, Uid: x.Uid, Namespace: x.Namespace, Name: x.Name, Owners: x.Owners}
		}
		if needWorkloads && x.Workload != nil {
			workloads[x.Uid] = &v1.Resource{Kind: x.Kind, Uid: x.Uid, Namespace: x.Namespace, Name: x.Name, Owners: x.Owners, Workload: x.Workload}
			if req.Screen == "workload" && x.Namespace == req.RequestedNamespace && x.Kind == req.Kind && x.Name == req.Name {
				targetUID = x.Uid
			}
		}
		if needPods && req.Screen == "pod" && x.Pod != nil && x.Namespace == req.RequestedNamespace && x.Name == req.Name && (req.EntityUID == "" || x.Uid == req.EntityUID) {
			targetUID = x.Uid
			if len(x.Owners) > 0 {
				targetOwnerUID = x.Owners[0].Uid
			}
		}
		if allScopedPods && x.Pod != nil && req.Namespaces.Allows(x.Namespace) {
			podCount++
		}
	}
	if req.Screen == "workload" {
		podCount = 128
	}
	if req.Screen == "pod" {
		podCount = 64
	}
	pods := make([]projectedPod, 0, podCount)
	byNS := map[string]*contract.NamespaceSummary{}
	i = 0
	for _, x := range resources {
		i++
		if i&511 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if x == nil {
			continue
		}
		switch {
		case needNodes && x.Node != nil:
			out.Nodes.Total++
			if x.Node.Ready {
				out.Nodes.Ready++
			} else {
				out.Nodes.NotReady++
			}
			if x.Node.Pressure {
				out.Nodes.Pressure++
			}
			if x.Node.Unschedulable {
				out.Nodes.Unschedulable++
			}
		case needPods && x.Pod != nil && req.Namespaces.Allows(x.Namespace) && includeScreenPod(req.Screen, x.Uid, x.Owners, targetUID, targetOwnerUID, rs):
			st := projectedPodState(x.Pod, now)
			pods = append(pods, projectedPod{uid: x.Uid, namespace: x.Namespace, name: x.Name, owners: x.Owners, pod: x.Pod, s: st})
			out.PodsHealth.Total++
			out.PodsHealth.Restarts += st.Restarts
			switch x.Pod.Phase {
			case "Running":
				out.PodsHealth.Running++
			case "Pending":
				out.PodsHealth.Pending++
			case "Failed":
				out.PodsHealth.Failed++
			}
			if hasIssue(st.Issues, contract.IssueCrashLoopBackOff) {
				out.PodsHealth.CrashLoopBackOff++
			}
			if hasIssue(st.Issues, contract.IssueImagePullBackOff) {
				out.PodsHealth.ImagePullBackOff++
			}
			if needNamespaces {
				n := namespaceAggregate(byNS, x.Namespace)
				n.Pods.Total++
				n.Pods.Restarts += st.Restarts
				switch x.Pod.Phase {
				case "Running":
					n.Pods.Running++
				case "Pending":
					n.Pods.Pending++
				case "Failed":
					n.Pods.Failed++
				}
				n.Severity = contract.WorseOf(n.Severity, st.Severity)
				n.Issues = mergeIssues(n.Issues, st.Issues)
				n.Usage.Add(projectedUsage(x.Pod))
			}
		case needEvents && x.Event != nil && req.Namespaces.Allows(x.Namespace):
			if x.Event.LastSeenUnixMs >= req.From.UnixMilli() && (targetUID == "" || x.Event.InvolvedUid == targetUID) {
				out.EventsList = append(out.EventsList, projectedEvent(req.ClusterID, x))
			}
		}
	}
	podsByWorkload := make(map[string][]int, len(workloads))
	for i, pod := range pods {
		if i&511 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		_, _, uid := projectedWorkloadOfPod(pod, rs)
		if uid != "" {
			podsByWorkload[uid] = append(podsByWorkload[uid], i)
		}
	}
	i = 0
	for _, x := range workloads {
		i++
		if i&511 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !req.Namespaces.Allows(x.Namespace) {
			continue
		}
		ownedPods := podsByWorkload[x.Uid]
		w := projectedWorkload(req.ClusterID, x, pods, ownedPods, rs, now)
		out.WorkloadsList = append(out.WorkloadsList, w)
		out.WorkloadsHealth.Total++
		if w.Severity == contract.SeverityHealthy {
			out.WorkloadsHealth.Available++
		}
		if hasIssue(w.Issues, contract.IssueReplicaMismatch) {
			out.WorkloadsHealth.ReplicaMismatch++
		}
		if hasIssue(w.Issues, contract.IssueRolloutStalled) {
			out.WorkloadsHealth.RolloutStalled++
		}
		if needNamespaces {
			n := namespaceAggregate(byNS, x.Namespace)
			n.Workloads.Total++
			if w.Severity != contract.SeverityHealthy {
				n.Workloads.Unhealthy++
			}
			n.Severity = contract.WorseOf(n.Severity, w.Severity)
			n.Issues = mergeIssues(n.Issues, w.Issues)
		}
		if x.Namespace == req.RequestedNamespace && x.Kind == req.Kind && x.Name == req.Name {
			copy := w
			out.WorkloadValue = &copy
			out.ResolvedUID = x.Uid
			out.PodsList = projectedPodsFor(x, pods, ownedPods, rs, req.ClusterID, now)
			out.WorkloadOwners = projectedWorkloadOwners(x, rs, pods)
		}
	}
	for i, p := range pods {
		if i&511 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if p.s.Severity != contract.SeverityHealthy {
			ps := projectedPodSummary(req.ClusterID, p, rs)
			out.Unhealthy = append(out.Unhealthy, contract.UnhealthyEntity{Ref: ps.Ref, Name: p.name, Kind: "Pod", Namespace: p.namespace, Severity: p.s.Severity, Reason: firstNonEmpty(p.s.Reason, "Unknown"), Restarts: p.s.Restarts, ForSeconds: max(0, int(now.Sub(time.UnixMilli(p.pod.CreatedUnixMs)).Seconds()))})
		}
		if p.namespace == req.RequestedNamespace && p.name == req.Name && (req.EntityUID == "" || p.uid == req.EntityUID) {
			ps := projectedPodSummary(req.ClusterID, p, rs)
			resource := p.resource()
			pod := safePod(resource)
			out.PodValue = pod
			copy := ps
			out.PodSummaryValue = &copy
			out.ResolvedUID = p.uid
			out.PodOwners = projectedPodOwners(resource, rs, pods)
		}
	}
	for _, w := range out.WorkloadsList {
		if w.Severity != contract.SeverityHealthy && (hasIssue(w.Issues, contract.IssueReplicaMismatch) || hasIssue(w.Issues, contract.IssueRolloutStalled)) {
			out.Unhealthy = append(out.Unhealthy, contract.UnhealthyEntity{Ref: w.Ref, Name: w.Name, Kind: "Workload", Namespace: w.Namespace, Severity: w.Severity, Reason: workloadReason(w), Restarts: w.Restarts, ForSeconds: w.AgeSeconds})
		}
	}
	sort.Slice(out.EventsList, func(i, j int) bool {
		if out.EventsList[i].LastSeen != out.EventsList[j].LastSeen {
			return out.EventsList[i].LastSeen > out.EventsList[j].LastSeen
		}
		return out.EventsList[i].ID < out.EventsList[j].ID
	})
	trimEvents := req.EventLimit
	if len(out.EventsList) > trimEvents {
		out.EventsList = out.EventsList[:trimEvents]
	}
	sort.SliceStable(out.Unhealthy, func(i, j int) bool {
		a, b := severityRank(out.Unhealthy[i].Severity), severityRank(out.Unhealthy[j].Severity)
		if a != b {
			return a > b
		}
		if out.Unhealthy[i].ForSeconds != out.Unhealthy[j].ForSeconds {
			return out.Unhealthy[i].ForSeconds > out.Unhealthy[j].ForSeconds
		}
		x, y := out.Unhealthy[i], out.Unhealthy[j]
		if x.Namespace != y.Namespace {
			return x.Namespace < y.Namespace
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		return x.Ref.PodUID+x.Ref.WorkloadUID < y.Ref.PodUID+y.Ref.WorkloadUID
	})
	if len(out.Unhealthy) > req.UnhealthyLimit {
		out.Unhealthy = out.Unhealthy[:req.UnhealthyLimit]
	}
	for _, n := range byNS {
		n.Usage.Normalize()
		if n.Issues == nil {
			// 계약은 배열입니다 — direct 경로와 같은 보장(JSON null 금지)입니다.
			n.Issues = []contract.IssueReason{}
		}
		sortIssues(n.Issues)
		out.Namespaces = append(out.Namespaces, *n)
	}
	sort.Slice(out.Namespaces, func(i, j int) bool {
		a, b := severityRank(out.Namespaces[i].Severity), severityRank(out.Namespaces[j].Severity)
		if a != b {
			return a > b
		}
		return out.Namespaces[i].Name < out.Namespaces[j].Name
	})
	for i := range out.Namespaces {
		if out.Namespaces[i].Name == req.RequestedNamespace {
			copy := out.Namespaces[i]
			out.Namespace = &copy
		}
	}
	sort.Slice(out.WorkloadsList, func(i, j int) bool {
		a, b := severityRank(out.WorkloadsList[i].Severity), severityRank(out.WorkloadsList[j].Severity)
		if a != b {
			return a > b
		}
		if out.WorkloadsList[i].Namespace != out.WorkloadsList[j].Namespace {
			return out.WorkloadsList[i].Namespace < out.WorkloadsList[j].Namespace
		}
		if out.WorkloadsList[i].Name != out.WorkloadsList[j].Name {
			return out.WorkloadsList[i].Name < out.WorkloadsList[j].Name
		}
		if out.WorkloadsList[i].Kind != out.WorkloadsList[j].Kind {
			return out.WorkloadsList[i].Kind < out.WorkloadsList[j].Kind
		}
		return out.WorkloadsList[i].Ref.WorkloadUID < out.WorkloadsList[j].Ref.WorkloadUID
	})
	badPods := make([]contract.UnhealthyEntity, 0, len(out.Unhealthy))
	for _, x := range out.Unhealthy {
		if x.Kind == "Pod" {
			badPods = append(badPods, x)
		}
	}
	badCount := 0
	for _, p := range pods {
		if p.s.Severity != contract.SeverityHealthy {
			badCount++
		}
	}
	topology := contract.TopologyPods{Total: len(pods), Healthy: len(pods) - badCount, Unhealthy: badCount, UnhealthyList: badPods}
	out.Topology = &topology
	pruneScreenProjection(out)
	return out, nil
}

func pruneScreenProjection(p *ScreenProjection) {
	switch p.Request.Screen {
	case "overview":
		p.Namespaces = nil
		p.Namespace = nil
		p.WorkloadsList = nil
		p.WorkloadValue = nil
		p.PodsList = nil
		p.PodValue = nil
		p.PodSummaryValue = nil
		p.PodOwners = nil
		p.WorkloadOwners = nil
		p.Topology = nil
	case "namespace-list":
		p.Nodes = nil
		p.PodsHealth = nil
		p.WorkloadsHealth = nil
		p.Unhealthy = nil
		p.EventsList = nil
		p.Namespace = nil
		p.WorkloadsList = nil
		p.WorkloadValue = nil
		p.PodsList = nil
		p.PodValue = nil
		p.PodSummaryValue = nil
		p.PodOwners = nil
		p.WorkloadOwners = nil
		p.Topology = nil
	case "namespace":
		p.Nodes = nil
		p.PodsHealth = nil
		p.WorkloadsHealth = nil
		p.Unhealthy = nil
		p.Namespaces = nil
		p.WorkloadValue = nil
		p.PodsList = nil
		p.PodValue = nil
		p.PodSummaryValue = nil
		p.PodOwners = nil
		p.WorkloadOwners = nil
		p.Topology = nil
	case "workload":
		p.Nodes = nil
		p.PodsHealth = nil
		p.WorkloadsHealth = nil
		p.Unhealthy = nil
		p.Namespaces = nil
		p.Namespace = nil
		p.WorkloadsList = nil
		p.PodValue = nil
		p.PodSummaryValue = nil
		p.PodOwners = nil
		p.Topology = nil
	case "pod":
		p.Nodes = nil
		p.PodsHealth = nil
		p.WorkloadsHealth = nil
		p.Unhealthy = nil
		p.Namespaces = nil
		p.Namespace = nil
		p.WorkloadsList = nil
		p.WorkloadValue = nil
		p.PodsList = nil
		p.WorkloadOwners = nil
		p.Topology = nil
	case "topology":
		p.Nodes = nil
		p.PodsHealth = nil
		p.WorkloadsHealth = nil
		p.Unhealthy = nil
		p.EventsList = nil
		p.Namespaces = nil
		p.Namespace = nil
		p.WorkloadsList = nil
		p.WorkloadValue = nil
		p.PodsList = nil
		p.PodValue = nil
		p.PodSummaryValue = nil
		p.PodOwners = nil
		p.WorkloadOwners = nil
	case "logs":
		p.Nodes = nil
		p.PodsHealth = nil
		p.WorkloadsHealth = nil
		p.Unhealthy = nil
		p.Namespaces = nil
		p.Namespace = nil
		p.WorkloadsList = nil
		p.WorkloadValue = nil
		p.PodsList = nil
		p.PodValue = nil
		p.PodSummaryValue = nil
		p.PodOwners = nil
		p.WorkloadOwners = nil
		p.Topology = nil
	}
}

func (p ScreenProjection) MarshalJSON() ([]byte, error) {
	m := map[string]any{"request": p.Request}
	switch p.Request.Screen {
	case "overview":
		m["nodes"], m["podsHealth"], m["workloadsHealth"], m["unhealthy"], m["events"] = p.Nodes, p.PodsHealth, p.WorkloadsHealth, p.Unhealthy, p.EventsList
	case "namespace-list":
		m["namespaces"] = p.Namespaces
	case "namespace":
		m["namespace"], m["workloads"], m["events"] = p.Namespace, p.WorkloadsList, p.EventsList
	case "workload":
		m["resolvedUid"], m["workload"], m["workloadOwners"], m["workloadPods"], m["events"] = p.ResolvedUID, p.WorkloadValue, p.WorkloadOwners, p.PodsList, p.EventsList
	case "pod":
		m["resolvedUid"], m["pod"], m["podSummary"], m["podOwners"], m["events"] = p.ResolvedUID, p.PodValue, p.PodSummaryValue, p.PodOwners, p.EventsList
	case "topology":
		m["topology"] = p.Topology
	case "logs":
		m["events"] = p.EventsList
	default:
		return nil, fmt.Errorf("unknown screen")
	}
	return json.Marshal(m)
}

func namespaceAggregate(m map[string]*contract.NamespaceSummary, ns string) *contract.NamespaceSummary {
	if x := m[ns]; x != nil {
		return x
	}
	x := &contract.NamespaceSummary{Name: ns, Severity: contract.SeverityHealthy}
	m[ns] = x
	return x
}
func projectedPodState(pod *v1.PodProjection, now time.Time) PodState {
	st := PodState{Severity: contract.SeverityHealthy, Since: time.UnixMilli(pod.CreatedUnixMs)}
	ready, total := 0, len(pod.Containers)
	for _, c := range pod.Containers {
		st.Restarts += int(c.Restarts)
		if c.Ready {
			ready++
		}
		if issue, ok := waitingCritical[c.Reason]; ok {
			st.addIssue(issue)
			st.Reason = c.Reason
		}
		if c.LastTerminationReason == "OOMKilled" {
			st.addIssue(contract.IssueOOMKilled)
			if st.Reason == "" {
				st.Reason = "OOMKilled"
			}
		}
	}
	if st.Restarts > 0 {
		st.addIssue(contract.IssueRestarting)
	}
	switch pod.Phase {
	case "Failed":
		st.Severity = contract.SeverityCritical
		st.Reason = firstNonEmpty(st.Reason, firstNonEmpty(pod.Reason, "Failed"))
	case "Pending":
		st.addIssue(contract.IssuePending)
		age := now.Sub(st.Since)
		if age > PendingGrace {
			st.Severity = contract.WorseOf(st.Severity, contract.SeverityWarning)
			st.Reason = firstNonEmpty(st.Reason, "Pending "+shortDuration(age))
		} else {
			st.Severity = contract.WorseOf(st.Severity, contract.SeverityProgressing)
			st.Reason = firstNonEmpty(st.Reason, "Pending")
		}
	case "Running":
		if ready < total {
			st.Severity = contract.WorseOf(st.Severity, contract.SeverityWarning)
			if !hasIssue(st.Issues, contract.IssueProbeFailed) && st.Reason == "" {
				st.addIssue(contract.IssueProbeFailed)
				st.Reason = "Readiness failed"
			}
		}
	case "Unknown":
		st.Severity = contract.WorseOf(st.Severity, contract.SeverityUnknown)
		st.Reason = firstNonEmpty(st.Reason, "Unknown")
	}
	for _, i := range st.Issues {
		switch i {
		case contract.IssueCrashLoopBackOff, contract.IssueOOMKilled:
			st.Severity = contract.WorseOf(st.Severity, contract.SeverityCritical)
		case contract.IssueImagePullBackOff:
			st.Severity = contract.WorseOf(st.Severity, contract.SeverityDegraded)
		case contract.IssueRestarting:
			st.Severity = contract.WorseOf(st.Severity, contract.SeverityWarning)
		}
	}
	if st.Severity == contract.SeverityHealthy {
		st.Reason = ""
	} else {
		st.ReadyText = fmt.Sprintf("%d/%d", ready, total)
	}
	sortIssues(st.Issues)
	return st
}
func safePod(x *v1.Resource) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: x.Name, Namespace: x.Namespace, UID: types.UID(x.Uid), CreationTimestamp: metav1.NewTime(time.UnixMilli(x.Pod.CreatedUnixMs))}, Spec: corev1.PodSpec{NodeName: x.Pod.NodeName}, Status: corev1.PodStatus{Phase: corev1.PodPhase(x.Pod.Phase), Reason: x.Pod.Reason}}
	for _, o := range x.Owners {
		p.OwnerReferences = append(p.OwnerReferences, metav1.OwnerReference{UID: types.UID(o.Uid), Kind: o.Kind, Name: o.Name})
	}
	if x.Pod.StartedUnixMs > 0 {
		t := metav1.NewTime(time.UnixMilli(x.Pod.StartedUnixMs))
		p.Status.StartTime = &t
	}
	for _, c := range x.Pod.Containers {
		spec := corev1.Container{Name: c.Name, Image: c.Image}
		if c.LivenessProbe {
			spec.LivenessProbe = &corev1.Probe{}
		}
		if c.ReadinessProbe {
			spec.ReadinessProbe = &corev1.Probe{}
		}
		p.Spec.Containers = append(p.Spec.Containers, spec)
		started := c.Started
		cs := corev1.ContainerStatus{Name: c.Name, Image: c.Image, ImageID: c.ImageId, Ready: c.Ready, RestartCount: c.Restarts, Started: &started}
		switch c.State {
		case "Running":
			cs.State.Running = &corev1.ContainerStateRunning{}
		case "Terminated":
			cs.State.Terminated = &corev1.ContainerStateTerminated{Reason: c.Reason, Message: c.MaskedMessage}
		default:
			cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: c.Reason, Message: c.MaskedMessage}
		}
		if c.LastTerminationReason != "" {
			cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: c.LastTerminationReason, ExitCode: c.LastExitCode, FinishedAt: metav1.NewTime(time.UnixMilli(c.LastFinishedUnixMs))}
		}
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, cs)
	}
	if len(p.Spec.Containers) > 0 {
		c := &p.Spec.Containers[0]
		c.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(x.Pod.CpuRequestMilli, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(x.Pod.MemoryRequestBytes, resource.BinarySI)}
		c.Resources.Limits = corev1.ResourceList{}
		if x.Pod.HasCpuLimit {
			c.Resources.Limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(x.Pod.CpuLimitMilli, resource.DecimalSI)
		}
		if x.Pod.HasMemoryLimit {
			c.Resources.Limits[corev1.ResourceMemory] = *resource.NewQuantity(x.Pod.MemoryLimitBytes, resource.BinarySI)
		}
	}
	if x.Pod.DeletedUnixMs > 0 {
		t := metav1.NewTime(time.UnixMilli(x.Pod.DeletedUnixMs))
		p.DeletionTimestamp = &t
	}
	return p
}
func projectedEvent(cluster string, x *v1.Resource) contract.ClusterEvent {
	e := x.Event
	ref := contract.EntityRef{ClusterID: cluster, Namespace: x.Namespace}
	if e.InvolvedKind == "Pod" {
		ref.PodUID, ref.PodName = e.InvolvedUid, e.InvolvedName
	} else {
		ref.WorkloadUID, ref.WorkloadKind, ref.WorkloadName = e.InvolvedUid, e.InvolvedKind, e.InvolvedName
	}
	count := int(e.Count)
	if count == 0 {
		count = 1
	}
	return contract.ClusterEvent{ID: x.Uid, Type: firstNonEmpty(e.Type, "Normal"), Reason: e.Reason, Message: e.MaskedMessage, Involved: ref, InvolvedName: e.InvolvedName, Namespace: x.Namespace, Count: count, LastSeen: time.UnixMilli(e.LastSeenUnixMs).UTC().Format(time.RFC3339)}
}
func projectedWorkload(cluster string, x *v1.Resource, pods []projectedPod, indices []int, rs map[string]*v1.Resource, now time.Time) contract.WorkloadSummary {
	p := x.Workload
	w := contract.WorkloadSummary{Ref: contract.EntityRef{ClusterID: cluster, Namespace: x.Namespace, WorkloadKind: x.Kind, WorkloadName: x.Name, WorkloadUID: x.Uid}, Name: x.Name, Kind: x.Kind, Namespace: x.Namespace, Severity: contract.SeverityHealthy, Replicas: contract.ReplicaCounts{Desired: int(p.Desired), Ready: int(p.Ready), Available: int(p.Available), Updated: int(p.Updated)}, Rollout: contract.RolloutStatus{Status: p.RolloutStatus, Message: p.RolloutReason}, Images: append([]string(nil), p.Images...), Issues: []contract.IssueReason{}, AgeSeconds: max(0, int(now.Sub(time.UnixMilli(p.CreatedUnixMs)).Seconds()))}
	if p.RolloutStatus == "Stalled" {
		w.Issues = append(w.Issues, contract.IssueRolloutStalled)
		w.Severity = contract.SeverityCritical
	}
	if p.Desired > 0 && p.Ready < p.Desired {
		w.Issues = mergeIssues(w.Issues, []contract.IssueReason{contract.IssueReplicaMismatch})
		if p.Ready == 0 {
			w.Severity = contract.WorseOf(w.Severity, contract.SeverityCritical)
		} else {
			w.Severity = contract.WorseOf(w.Severity, contract.SeverityDegraded)
		}
	}
	for _, index := range indices {
		pod := pods[index]
		kind, _, uid := projectedWorkloadOfPod(pod, rs)
		if uid == x.Uid && kind == x.Kind {
			w.Restarts += pod.s.Restarts
			w.Severity = contract.WorseOf(w.Severity, pod.s.Severity)
			w.Issues = mergeIssues(w.Issues, pod.s.Issues)
			w.Usage.Add(projectedUsage(pod.pod))
		}
	}
	w.Usage.Normalize()
	sortIssues(w.Issues)
	return w
}
func projectedWorkloadOfPod(p projectedPod, rs map[string]*v1.Resource) (string, string, string) {
	return projectedWorkloadOfOwners(p.owners, rs)
}
func includeScreenPod(screen, uid string, owners []*v1.OwnerRef, targetUID, targetOwnerUID string, rs map[string]*v1.Resource) bool {
	switch screen {
	case "workload":
		_, _, workloadUID := projectedWorkloadOfOwners(owners, rs)
		return workloadUID == targetUID
	case "pod":
		if targetOwnerUID != "" {
			return len(owners) > 0 && owners[0].Uid == targetOwnerUID
		}
		return uid == targetUID
	default:
		return true
	}
}
func projectedWorkloadOfOwners(owners []*v1.OwnerRef, rs map[string]*v1.Resource) (string, string, string) {
	if len(owners) == 0 {
		return "", "", ""
	}
	o := owners[0]
	if o.Kind != "ReplicaSet" {
		return o.Kind, o.Name, o.Uid
	}
	r := rs[o.Uid]
	if r != nil && len(r.Owners) > 0 {
		top := r.Owners[0]
		return top.Kind, top.Name, top.Uid
	}
	return o.Kind, o.Name, o.Uid
}
func projectedPodSummary(cluster string, x projectedPod, rs map[string]*v1.Resource) contract.PodSummary {
	k, n, u := projectedWorkloadOfOwners(x.owners, rs)
	usage := projectedUsage(x.pod)
	usage.Normalize()
	st := x.s
	if st.ReadyText == "" {
		ready := 0
		for _, c := range x.pod.Containers {
			if c.Ready {
				ready++
			}
		}
		st.ReadyText = fmt.Sprintf("%d/%d", ready, len(x.pod.Containers))
	}
	p := contract.PodSummary{Ref: contract.EntityRef{ClusterID: cluster, Namespace: x.namespace, WorkloadKind: k, WorkloadName: n, WorkloadUID: u, PodName: x.name, PodUID: x.uid}, Name: x.name, UID: x.uid, Namespace: x.namespace, Phase: x.pod.Phase, Severity: st.Severity, Ready: st.ReadyText, Restarts: st.Restarts, Node: x.pod.NodeName, Issues: append([]contract.IssueReason(nil), st.Issues...), Usage: usage, StartedAt: time.UnixMilli(x.pod.CreatedUnixMs).UTC().Format(time.RFC3339)}
	if p.Issues == nil {
		p.Issues = []contract.IssueReason{}
	}
	if x.pod.StartedUnixMs > 0 {
		p.StartedAt = time.UnixMilli(x.pod.StartedUnixMs).UTC().Format(time.RFC3339)
	}
	if x.pod.DeletedUnixMs > 0 {
		p.FinishedAt = time.UnixMilli(x.pod.DeletedUnixMs).UTC().Format(time.RFC3339)
	}
	if len(x.owners) > 0 {
		o := x.owners[0]
		p.Owner = &contract.OwnerRef{Kind: o.Kind, Name: o.Name, UID: o.Uid}
	}
	return p
}

func projectedUsage(p *v1.PodProjection) contract.ResourceUsage {
	u := contract.ResourceUsage{CPURequestMilli: int(p.CpuRequestMilli), MemoryRequestMib: int(p.MemoryRequestBytes / (1 << 20))}
	if p.HasCpuLimit {
		v := int(p.CpuLimitMilli)
		u.CPULimitMilli = &v
	}
	if p.HasMemoryLimit {
		v := int(p.MemoryLimitBytes / (1 << 20))
		u.MemoryLimitMib = &v
	}
	return u
}
func projectedPodsFor(w *v1.Resource, pods []projectedPod, indices []int, rs map[string]*v1.Resource, cluster string, now time.Time) []contract.PodSummary {
	out := make([]contract.PodSummary, 0, len(indices))
	for _, index := range indices {
		p := pods[index]
		k, _, u := projectedWorkloadOfPod(p, rs)
		if k == w.Kind && u == w.Uid {
			out = append(out, projectedPodSummary(cluster, p, rs))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := severityRank(out[i].Severity), severityRank(out[j].Severity)
		if a != b {
			return a > b
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].UID < out[j].UID
	})
	return out
}
func projectedPodOwners(p *v1.Resource, rs map[string]*v1.Resource, pods []projectedPod) []contract.OwnerRef {
	out := []contract.OwnerRef{}
	if len(p.Owners) == 0 {
		return out
	}
	o := p.Owners[0]
	count := 0
	for _, x := range pods {
		if len(x.owners) > 0 && x.owners[0].Uid == o.Uid {
			count++
		}
	}
	out = append(out, contract.OwnerRef{Kind: o.Kind, Name: o.Name, UID: o.Uid, Current: true, Pods: count, Revision: o.Revision})
	if r := rs[o.Uid]; r != nil && len(r.Owners) > 0 {
		top := r.Owners[0]
		out[0].Revision = top.Revision
		out = append(out, contract.OwnerRef{Kind: top.Kind, Name: top.Name, UID: top.Uid, Current: true})
	}
	return out
}
func projectedWorkloadOwners(w *v1.Resource, rs map[string]*v1.Resource, pods []projectedPod) []contract.OwnerRef {
	if w.Kind != "Deployment" {
		return []contract.OwnerRef{}
	}
	out := []contract.OwnerRef{}
	for _, r := range rs {
		if len(r.Owners) == 0 || r.Owners[0].Uid != w.Uid {
			continue
		}
		count := 0
		for _, p := range pods {
			if len(p.owners) > 0 && p.owners[0].Uid == r.Uid {
				count++
			}
		}
		out = append(out, contract.OwnerRef{Kind: "ReplicaSet", Name: r.Name, UID: r.Uid, Pods: count, Revision: r.Owners[0].Revision})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Revision != out[j].Revision {
			return out[i].Revision > out[j].Revision
		}
		return out[i].UID < out[j].UID
	})
	if len(out) > 0 {
		out[0].Current = true
	}
	return out
}

type RemoteProvider struct {
	Data     *ScreenProjection
	Stale    bool
	Observed time.Time
}

func (p *RemoteProvider) HasSynced() bool { return p != nil && p.Data != nil && !p.Stale }
func (p *RemoteProvider) ObservedAt() time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.Observed
}
func (p *RemoteProvider) NodeHealth() (contract.NodeHealth, error) {
	if p.Data.Nodes == nil {
		return contract.NodeHealth{}, fmt.Errorf("screen field unavailable")
	}
	return *p.Data.Nodes, nil
}
func (p *RemoteProvider) NodeSummaries() ([]contract.NodeSummary, error) {
	// central 화면 projection에는 노드 상세가 실리지 않습니다 — 섹션 degraded로 알립니다.
	return nil, fmt.Errorf("screen field unavailable")
}
func (p *RemoteProvider) PodHealth(f NamespaceFilter) (contract.PodHealth, error) {
	if !sameFilter(f, p.Data.Request.Namespaces) {
		return contract.PodHealth{}, fmt.Errorf("screen scope mismatch")
	}
	if p.Data.PodsHealth == nil {
		return contract.PodHealth{}, fmt.Errorf("screen field unavailable")
	}
	return *p.Data.PodsHealth, nil
}
func (p *RemoteProvider) WorkloadHealth(f NamespaceFilter) (contract.WorkloadHealth, error) {
	if !sameFilter(f, p.Data.Request.Namespaces) {
		return contract.WorkloadHealth{}, fmt.Errorf("screen scope mismatch")
	}
	if p.Data.WorkloadsHealth == nil {
		return contract.WorkloadHealth{}, fmt.Errorf("screen field unavailable")
	}
	return *p.Data.WorkloadsHealth, nil
}
func (p *RemoteProvider) Unhealthy(f NamespaceFilter, n int) ([]contract.UnhealthyEntity, error) {
	if !sameFilter(f, p.Data.Request.Namespaces) || n != p.Data.Request.UnhealthyLimit {
		return nil, fmt.Errorf("screen argument mismatch")
	}
	return p.Data.Unhealthy, nil
}
func (p *RemoteProvider) Events(f NamespaceFilter, t time.Time, n int) ([]contract.ClusterEvent, error) {
	if !sameFilter(f, p.Data.Request.Namespaces) || !t.Equal(p.Data.Request.From) || n != p.Data.Request.EventLimit {
		return nil, fmt.Errorf("screen argument mismatch")
	}
	return p.Data.EventsList, nil
}
func (p *RemoteProvider) EventsForUID(uid string, t time.Time, n int) ([]contract.ClusterEvent, error) {
	wantUID := p.Data.Request.EntityUID
	if wantUID == "" && p.Data.WorkloadValue != nil {
		wantUID = p.Data.WorkloadValue.Ref.WorkloadUID
	}
	if wantUID == "" && p.Data.PodSummaryValue != nil {
		wantUID = p.Data.PodSummaryValue.UID
	}
	if uid != wantUID || !t.Equal(p.Data.Request.From) || n != p.Data.Request.EventLimit {
		return nil, fmt.Errorf("screen argument mismatch")
	}
	return p.Data.EventsList, nil
}
func (p *RemoteProvider) NamespaceSummaries(f NamespaceFilter) ([]contract.NamespaceSummary, error) {
	if !sameFilter(f, p.Data.Request.Namespaces) {
		return nil, fmt.Errorf("screen scope mismatch")
	}
	return p.Data.Namespaces, nil
}
func (p *RemoteProvider) NamespaceSummary(ns string) (contract.NamespaceSummary, bool, error) {
	if ns != p.Data.Request.RequestedNamespace || p.Data.Namespace == nil {
		return contract.NamespaceSummary{}, false, nil
	}
	return *p.Data.Namespace, true, nil
}
func (p *RemoteProvider) Workloads(f NamespaceFilter) ([]contract.WorkloadSummary, error) {
	if !sameFilter(f, p.Data.Request.Namespaces) {
		return nil, fmt.Errorf("screen scope mismatch")
	}
	return p.Data.WorkloadsList, nil
}
func (p *RemoteProvider) Workload(ns, k, n string) (contract.WorkloadSummary, bool, error) {
	if ns != p.Data.Request.RequestedNamespace || k != p.Data.Request.Kind || n != p.Data.Request.Name || p.Data.WorkloadValue == nil {
		return contract.WorkloadSummary{}, false, nil
	}
	return *p.Data.WorkloadValue, true, nil
}
func (p *RemoteProvider) PodsForWorkload(ns, k, n, uid string) ([]contract.PodSummary, error) {
	if p.Data.WorkloadValue == nil || ns != p.Data.Request.RequestedNamespace || k != p.Data.Request.Kind || n != p.Data.Request.Name || uid != p.Data.ResolvedUID {
		return nil, fmt.Errorf("screen entity mismatch")
	}
	return p.Data.PodsList, nil
}
func (p *RemoteProvider) Pod(ns, n, uid string) (*corev1.Pod, bool, error) {
	uidMatches := uid == p.Data.Request.EntityUID && (uid == "" || uid == p.Data.ResolvedUID)
	if ns != p.Data.Request.RequestedNamespace || n != p.Data.Request.Name || !uidMatches || p.Data.PodValue == nil {
		return nil, false, nil
	}
	return p.Data.PodValue.DeepCopy(), true, nil
}
func (p *RemoteProvider) PodSummary(pod *corev1.Pod) contract.PodSummary {
	if p.Data.PodSummaryValue == nil || pod == nil || pod.Namespace != p.Data.Request.RequestedNamespace || pod.Name != p.Data.Request.Name || string(pod.UID) != p.Data.ResolvedUID {
		return contract.PodSummary{}
	}
	return *p.Data.PodSummaryValue
}
func (p *RemoteProvider) PodOwnerChain(pod *corev1.Pod) []contract.OwnerRef {
	if pod == nil || pod.Namespace != p.Data.Request.RequestedNamespace || pod.Name != p.Data.Request.Name || string(pod.UID) != p.Data.ResolvedUID {
		return nil
	}
	return p.Data.PodOwners
}
func (p *RemoteProvider) WorkloadOwnerChain(ns, k, n, uid string) []contract.OwnerRef {
	if ns != p.Data.Request.RequestedNamespace || k != p.Data.Request.Kind || n != p.Data.Request.Name || uid != p.Data.ResolvedUID {
		return nil
	}
	return p.Data.WorkloadOwners
}
func (p *RemoteProvider) TopologyPods(f NamespaceFilter, n int) (contract.TopologyPods, error) {
	if !sameFilter(f, p.Data.Request.Namespaces) || n != p.Data.Request.UnhealthyLimit {
		return contract.TopologyPods{}, fmt.Errorf("screen argument mismatch")
	}
	if p.Data.Topology == nil {
		return contract.TopologyPods{}, fmt.Errorf("screen field unavailable")
	}
	return *p.Data.Topology, nil
}
func sameFilter(a, b NamespaceFilter) bool {
	if a.All != b.All || len(a.List) != len(b.List) {
		return false
	}
	for i := range a.List {
		if a.List[i] != b.List[i] {
			return false
		}
	}
	return true
}

var _ Provider = (*RemoteProvider)(nil)
