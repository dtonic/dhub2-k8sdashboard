package clusterstate

import (
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// PendingGrace는 이 시간을 넘긴 Pending을 "진행 중"이 아니라 "문제"로 봅니다.
// 스케줄 대기 몇 초까지 빨간색으로 칠하면 화면이 늑대소년이 됩니다.
const PendingGrace = 5 * time.Minute

// waitingCritical은 재시도해도 스스로 낫지 않는 대기 사유입니다.
var waitingCritical = map[string]contract.IssueReason{
	"CrashLoopBackOff":           contract.IssueCrashLoopBackOff,
	"ImagePullBackOff":           contract.IssueImagePullBackOff,
	"ErrImagePull":               contract.IssueImagePullBackOff,
	"CreateContainerConfigError": contract.IssueProbeFailed,
}

// PodState는 Pod 하나를 정규화한 결과입니다.
type PodState struct {
	Severity  contract.Severity
	Issues    []contract.IssueReason
	Restarts  int
	ReadyText string
	// Reason은 목록 화면에 그대로 쓰는 한 줄 사유입니다.
	Reason string
	// Since는 이 상태가 시작된 시각입니다. forSeconds 계산에 씁니다.
	Since time.Time
}

// NormalizePod은 Pod의 status를 화면 어휘로 바꿉니다.
//
// 여기가 대시보드의 사실상 유일한 "판단" 지점입니다. 화면마다 조건을 다시 쓰면
// 같은 Pod가 Overview에서는 정상, Drill-down에서는 경고로 보이게 됩니다.
func NormalizePod(pod *corev1.Pod, now time.Time) PodState {
	st := PodState{Severity: contract.SeverityHealthy, Since: pod.CreationTimestamp.Time}

	ready, total := 0, len(pod.Spec.Containers)
	for _, cs := range pod.Status.ContainerStatuses {
		st.Restarts += int(cs.RestartCount)
		if cs.Ready {
			ready++
		}
		if w := cs.State.Waiting; w != nil {
			if issue, ok := waitingCritical[w.Reason]; ok {
				st.addIssue(issue)
				st.Reason = w.Reason
			}
		}
		if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
			st.addIssue(contract.IssueOOMKilled)
			if st.Reason == "" {
				st.Reason = "OOMKilled"
			}
		}
	}
	st.ReadyText = fmt.Sprintf("%d/%d", ready, total)

	if st.Restarts > 0 {
		st.addIssue(contract.IssueRestarting)
	}

	switch pod.Status.Phase {
	case corev1.PodFailed:
		st.Severity = contract.SeverityCritical
		if st.Reason == "" {
			st.Reason = firstNonEmpty(pod.Status.Reason, "Failed")
		}
	case corev1.PodPending:
		st.addIssue(contract.IssuePending)
		age := now.Sub(pod.CreationTimestamp.Time)
		if age > PendingGrace {
			st.Severity = contract.WorseOf(st.Severity, contract.SeverityWarning)
			st.Reason = firstNonEmpty(st.Reason, "Pending "+shortDuration(age))
		} else {
			st.Severity = contract.WorseOf(st.Severity, contract.SeverityProgressing)
			st.Reason = firstNonEmpty(st.Reason, "Pending")
		}
	case corev1.PodRunning:
		if ready < total {
			st.Severity = contract.WorseOf(st.Severity, contract.SeverityWarning)
			if !hasIssue(st.Issues, contract.IssueProbeFailed) && st.Reason == "" {
				st.addIssue(contract.IssueProbeFailed)
				st.Reason = "Readiness 실패"
			}
		}
	case corev1.PodUnknown:
		st.Severity = contract.WorseOf(st.Severity, contract.SeverityUnknown)
		st.Reason = firstNonEmpty(st.Reason, "Unknown")
	}

	// 대기 사유는 phase보다 강합니다. Running이어도 컨테이너가 CrashLoop이면 critical입니다.
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
	} else if st.Reason == "" && st.Restarts > 0 {
		st.Reason = fmt.Sprintf("재시작 %d회", st.Restarts)
	}
	sortIssues(st.Issues)
	return st
}

func (s *PodState) addIssue(i contract.IssueReason) {
	if !hasIssue(s.Issues, i) {
		s.Issues = append(s.Issues, i)
	}
}

// WorkloadState는 Workload 하나를 정규화한 결과입니다.
type WorkloadState struct {
	Replicas contract.ReplicaCounts
	Rollout  contract.RolloutStatus
	Severity contract.Severity
	Issues   []contract.IssueReason
}

// NormalizeDeployment는 Deployment의 replica·rollout 상태를 정규화합니다.
func NormalizeDeployment(d *appsv1.Deployment) WorkloadState {
	desired := 1
	if d.Spec.Replicas != nil {
		desired = int(*d.Spec.Replicas)
	}
	st := WorkloadState{Severity: contract.SeverityHealthy, Replicas: contract.ReplicaCounts{
		Desired:   desired,
		Ready:     int(d.Status.ReadyReplicas),
		Available: int(d.Status.AvailableReplicas),
		Updated:   int(d.Status.UpdatedReplicas),
	}}

	st.Rollout = contract.RolloutStatus{Status: "Complete"}
	if d.Spec.Paused {
		st.Rollout = contract.RolloutStatus{Status: "Paused", Message: "롤아웃이 일시정지되었습니다"}
	}
	for _, c := range d.Status.Conditions {
		if c.Type != appsv1.DeploymentProgressing {
			continue
		}
		switch {
		case c.Reason == "ProgressDeadlineExceeded":
			st.Rollout = contract.RolloutStatus{Status: "Stalled", Message: c.Message}
			st.Issues = append(st.Issues, contract.IssueRolloutStalled)
			st.Severity = contract.WorseOf(st.Severity, contract.SeverityCritical)
		case c.Status == corev1.ConditionTrue && c.Reason != "NewReplicaSetAvailable" && !d.Spec.Paused:
			st.Rollout = contract.RolloutStatus{Status: "Progressing", Message: c.Message}
		}
	}

	st.applyReplicaMismatch()
	return st
}

// NormalizeStatefulSet은 StatefulSet을 정규화합니다. StatefulSet에는 Progressing 조건이 없어
// updated/ready 비교로 롤아웃 진행 여부를 판단합니다.
func NormalizeStatefulSet(s *appsv1.StatefulSet) WorkloadState {
	desired := 1
	if s.Spec.Replicas != nil {
		desired = int(*s.Spec.Replicas)
	}
	st := WorkloadState{Severity: contract.SeverityHealthy, Replicas: contract.ReplicaCounts{
		Desired:   desired,
		Ready:     int(s.Status.ReadyReplicas),
		Available: int(s.Status.AvailableReplicas),
		Updated:   int(s.Status.UpdatedReplicas),
	}}
	st.Rollout = contract.RolloutStatus{Status: "Complete"}
	if s.Status.UpdateRevision != "" && s.Status.UpdateRevision != s.Status.CurrentRevision {
		st.Rollout = contract.RolloutStatus{Status: "Progressing", Message: "새 리비전 롤아웃 중"}
	}
	st.applyReplicaMismatch()
	return st
}

// NormalizeDaemonSet은 DaemonSet을 정규화합니다. desired는 노드 수입니다.
func NormalizeDaemonSet(d *appsv1.DaemonSet) WorkloadState {
	st := WorkloadState{Severity: contract.SeverityHealthy, Replicas: contract.ReplicaCounts{
		Desired:   int(d.Status.DesiredNumberScheduled),
		Ready:     int(d.Status.NumberReady),
		Available: int(d.Status.NumberAvailable),
		Updated:   int(d.Status.UpdatedNumberScheduled),
	}}
	st.Rollout = contract.RolloutStatus{Status: "Complete"}
	if d.Status.UpdatedNumberScheduled < d.Status.DesiredNumberScheduled {
		st.Rollout = contract.RolloutStatus{Status: "Progressing", Message: "노드 롤아웃 중"}
	}
	st.applyReplicaMismatch()
	return st
}

// NormalizeCronJob은 CronJob을 정규화합니다. replica 개념이 없으므로
// 활성 Job 수를 desired/ready로 보여주고, suspend는 Paused로 봅니다.
func NormalizeCronJob(c *batchv1.CronJob) WorkloadState {
	active := len(c.Status.Active)
	st := WorkloadState{Severity: contract.SeverityHealthy, Replicas: contract.ReplicaCounts{
		Desired: active, Ready: active, Available: active, Updated: active,
	}}
	st.Rollout = contract.RolloutStatus{Status: "Complete"}
	if c.Spec.Suspend != nil && *c.Spec.Suspend {
		st.Rollout = contract.RolloutStatus{Status: "Paused", Message: "일시정지된 스케줄입니다"}
	}
	return st
}

// applyReplicaMismatch는 desired와 ready가 다를 때만 문제로 표시합니다.
// desired가 0인 워크로드(의도적으로 내린 것)는 문제로 보지 않습니다.
func (s *WorkloadState) applyReplicaMismatch() {
	if s.Replicas.Desired == 0 {
		s.Severity = contract.WorseOf(s.Severity, contract.SeverityHealthy)
		return
	}
	if s.Replicas.Ready < s.Replicas.Desired {
		s.Issues = append(s.Issues, contract.IssueReplicaMismatch)
		if s.Replicas.Ready == 0 {
			s.Severity = contract.WorseOf(s.Severity, contract.SeverityCritical)
		} else {
			s.Severity = contract.WorseOf(s.Severity, contract.SeverityDegraded)
		}
	}
}

// NormalizeNode는 노드 하나가 어떤 상태인지 돌려줍니다.
func NormalizeNode(n *corev1.Node) (ready, pressure, unschedulable bool) {
	unschedulable = n.Spec.Unschedulable
	for _, c := range n.Status.Conditions {
		switch c.Type {
		case corev1.NodeReady:
			ready = c.Status == corev1.ConditionTrue
		case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
			if c.Status == corev1.ConditionTrue {
				pressure = true
			}
		}
	}
	return ready, pressure, unschedulable
}

// PodRequests는 Pod spec에서 request/limit 합계를 뽑습니다.
// 실제 사용량은 Kubernetes가 아니라 메트릭 데이터소스에서 옵니다.
func PodRequests(pod *corev1.Pod) contract.ResourceUsage {
	u := contract.ResourceUsage{}
	cpuLimit, memLimit := 0, 0
	cpuLimited, memLimited := true, true
	for _, c := range pod.Spec.Containers {
		u.CPURequestMilli += int(c.Resources.Requests.Cpu().MilliValue())
		u.MemoryRequestMib += int(c.Resources.Requests.Memory().Value() / (1 << 20))
		if q := c.Resources.Limits.Cpu(); q != nil && !q.IsZero() {
			cpuLimit += int(q.MilliValue())
		} else {
			cpuLimited = false
		}
		if q := c.Resources.Limits.Memory(); q != nil && !q.IsZero() {
			memLimit += int(q.Value() / (1 << 20))
		} else {
			memLimited = false
		}
	}
	if cpuLimited && len(pod.Spec.Containers) > 0 {
		u.CPULimitMilli = &cpuLimit
	}
	if memLimited && len(pod.Spec.Containers) > 0 {
		u.MemoryLimitMib = &memLimit
	}
	return u
}

// ContainerStatuses는 Pod의 컨테이너 상태를 화면 계약으로 바꿉니다.
func ContainerStatuses(pod *corev1.Pod) []contract.ContainerStatus {
	specByName := map[string]corev1.Container{}
	for _, c := range pod.Spec.Containers {
		specByName[c.Name] = c
	}
	out := make([]contract.ContainerStatus, 0, len(pod.Status.ContainerStatuses))
	for _, cs := range pod.Status.ContainerStatuses {
		c := contract.ContainerStatus{
			Name:     cs.Name,
			Image:    cs.Image,
			ImageID:  cs.ImageID,
			Ready:    cs.Ready,
			Restarts: int(cs.RestartCount),
			State:    "Waiting",
		}
		if cs.Started != nil {
			c.Started = *cs.Started
		}
		switch {
		case cs.State.Running != nil:
			c.State = "Running"
		case cs.State.Terminated != nil:
			c.State = "Terminated"
			c.Reason = cs.State.Terminated.Reason
			c.Message = cs.State.Terminated.Message
		case cs.State.Waiting != nil:
			c.Reason = cs.State.Waiting.Reason
			c.Message = cs.State.Waiting.Message
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			c.LastTerminated = &contract.ContainerTermination{
				Reason:     t.Reason,
				ExitCode:   int(t.ExitCode),
				FinishedAt: t.FinishedAt.UTC().Format(time.RFC3339),
			}
		}
		spec := specByName[cs.Name]
		c.Probes = contract.ProbeState{
			Liveness:  probeState(spec.LivenessProbe, c.State == "Running"),
			Readiness: probeState(spec.ReadinessProbe, cs.Ready),
		}
		out = append(out, c)
	}
	return out
}

// probeState는 probe가 정의되어 있는지와 현재 통과 여부를 합친 값입니다.
// probe가 없는데 "passing"으로 보이면 실제로는 확인되지 않은 상태가 정상처럼 보입니다.
func probeState(p *corev1.Probe, passing bool) string {
	if p == nil {
		return "none"
	}
	if passing {
		return "passing"
	}
	return "failing"
}

func hasIssue(list []contract.IssueReason, i contract.IssueReason) bool {
	for _, v := range list {
		if v == i {
			return true
		}
	}
	return false
}

func sortIssues(list []contract.IssueReason) {
	sort.Slice(list, func(a, b int) bool { return list[a] < list[b] })
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func shortDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%d일", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d시간", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%d분", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d초", int(d.Seconds()))
	}
}
