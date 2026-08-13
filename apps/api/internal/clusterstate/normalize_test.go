package clusterstate_test

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

var now = testcluster.Now

func TestPodSeverityVocabulary(t *testing.T) {
	// 정규화가 화면마다 달라지면 같은 Pod가 Overview에서는 정상,
	// Drill-down에서는 경고로 보입니다. 어휘는 여기 한 곳에서만 정합니다.
	cases := []struct {
		name  string
		pod   *corev1.Pod
		want  contract.Severity
		issue contract.IssueReason
	}{
		{"정상 Running", running(true, 0, ""), contract.SeverityHealthy, ""},
		{"CrashLoopBackOff", running(false, 7, "CrashLoopBackOff"), contract.SeverityCritical, contract.IssueCrashLoopBackOff},
		{"ImagePullBackOff", running(false, 0, "ImagePullBackOff"), contract.SeverityDegraded, contract.IssueImagePullBackOff},
		{"Ready 실패", running(false, 0, ""), contract.SeverityWarning, contract.IssueProbeFailed},
		{"재시작만 있음", running(true, 3, ""), contract.SeverityWarning, contract.IssueRestarting},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := clusterstate.NormalizePod(c.pod, now)
			if st.Severity != c.want {
				t.Errorf("severity=%s, want %s", st.Severity, c.want)
			}
			if c.issue == "" {
				return
			}
			found := false
			for _, i := range st.Issues {
				if i == c.issue {
					found = true
				}
			}
			if !found {
				t.Errorf("issue %s가 없습니다: %v", c.issue, st.Issues)
			}
		})
	}
}

func TestShortPendingIsProgressingNotWarning(t *testing.T) {
	// 스케줄 대기 몇 초까지 경고로 칠하면 화면이 늑대소년이 됩니다.
	fresh := pending(now.Add(-30 * time.Second))
	if got := clusterstate.NormalizePod(fresh, now).Severity; got != contract.SeverityProgressing {
		t.Errorf("갓 생성된 Pending severity=%s, want progressing", got)
	}
	stuck := pending(now.Add(-30 * time.Minute))
	if got := clusterstate.NormalizePod(stuck, now).Severity; got != contract.SeverityWarning {
		t.Errorf("오래된 Pending severity=%s, want warning", got)
	}
}

func TestHealthyPodHasNoReason(t *testing.T) {
	st := clusterstate.NormalizePod(running(true, 0, ""), now)
	if st.Reason != "" {
		t.Errorf("정상 Pod에 사유가 붙었습니다: %q", st.Reason)
	}
	if st.ReadyText != "1/1" {
		t.Errorf("ready=%q, want 1/1", st.ReadyText)
	}
}

func TestReplicaMismatchSeverityDependsOnRemainingReplicas(t *testing.T) {
	// 3개 중 2개가 살아 있는 것과 전부 죽은 것은 대응 긴급도가 다릅니다.
	partial := clusterstate.NormalizeDeployment(deploy(3, 2, nil))
	if partial.Severity != contract.SeverityDegraded {
		t.Errorf("2/3 severity=%s, want degraded", partial.Severity)
	}
	none := clusterstate.NormalizeDeployment(deploy(3, 0, nil))
	if none.Severity != contract.SeverityCritical {
		t.Errorf("0/3 severity=%s, want critical", none.Severity)
	}
	// 의도적으로 0으로 내린 워크로드는 문제가 아닙니다.
	scaledDown := clusterstate.NormalizeDeployment(deploy(0, 0, nil))
	if scaledDown.Severity != contract.SeverityHealthy || len(scaledDown.Issues) != 0 {
		t.Errorf("replicas=0을 문제로 봤습니다: %+v", scaledDown)
	}
}

func TestProgressDeadlineExceededIsStalled(t *testing.T) {
	d := deploy(3, 3, []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
		Reason: "ProgressDeadlineExceeded", Message: "deadline exceeded",
	}})
	st := clusterstate.NormalizeDeployment(d)
	if st.Rollout.Status != "Stalled" {
		t.Errorf("rollout=%s, want Stalled", st.Rollout.Status)
	}
	if st.Severity != contract.SeverityCritical {
		t.Errorf("severity=%s, want critical", st.Severity)
	}
}

func TestLimitSumIsAbsentWhenAnyContainerHasNoLimit(t *testing.T) {
	// "limit 없음"을 0으로 접으면 화면에서 무한 과사용으로 보입니다.
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "a", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
		}},
		{Name: "b", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
		}},
	}}}
	u := clusterstate.PodRequests(pod)
	if u.CPURequestMilli != 150 {
		t.Errorf("cpu request=%d, want 150", u.CPURequestMilli)
	}
	if u.CPULimitMilli != nil {
		t.Errorf("limit 없는 컨테이너가 있는데 합계가 나왔습니다: %d", *u.CPULimitMilli)
	}
}

func TestUsageRatiosAreComputedOnTheServer(t *testing.T) {
	limit := 500
	u := contract.ResourceUsage{CPUMilli: 250, CPURequestMilli: 200, CPULimitMilli: &limit}
	u.Normalize()
	if u.CPUVsRequest != 1.25 {
		t.Errorf("cpuVsRequest=%v, want 1.25", u.CPUVsRequest)
	}
	if u.CPUVsLimit == nil || *u.CPUVsLimit != 0.5 {
		t.Errorf("cpuVsLimit=%v, want 0.5", u.CPUVsLimit)
	}
	if u.MemoryVsLimit != nil {
		t.Errorf("limit 없는 메모리 비율이 나왔습니다: %v", *u.MemoryVsLimit)
	}
}

func TestProbeStateDistinguishesMissingFromPassing(t *testing.T) {
	// probe가 없는데 passing으로 보이면, 확인되지 않은 상태가 정상처럼 보입니다.
	pod := running(true, 0, "")
	pod.Spec.Containers[0].LivenessProbe = nil
	cs := clusterstate.ContainerStatuses(pod)
	if len(cs) != 1 {
		t.Fatalf("컨테이너 수=%d", len(cs))
	}
	if cs[0].Probes.Liveness != "none" {
		t.Errorf("liveness=%s, want none", cs[0].Probes.Liveness)
	}
	if cs[0].Probes.Readiness != "passing" {
		t.Errorf("readiness=%s, want passing", cs[0].Probes.Readiness)
	}
}

func TestNodeConditionsAreNormalized(t *testing.T) {
	n := &corev1.Node{
		Spec: corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue},
		}},
	}
	ready, pressure, unschedulable := clusterstate.NormalizeNode(n)
	if !ready || !pressure || !unschedulable {
		t.Errorf("ready=%v pressure=%v unschedulable=%v", ready, pressure, unschedulable)
	}
}

/* ── 헬퍼 ───────────────────────────────────────────────────────────────── */

func running(ready bool, restarts int32, waiting string) *corev1.Pod {
	started := ready
	cs := corev1.ContainerStatus{
		Name: "app", Ready: ready, Started: &started, RestartCount: restarts,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
	if waiting != "" {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waiting}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "payments",
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app", ReadinessProbe: &corev1.Probe{}, LivenessProbe: &corev1.Probe{},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

func pending(created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "payments", CreationTimestamp: metav1.NewTime(created),
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func deploy(desired, ready int32, conds []appsv1.DeploymentCondition) *appsv1.Deployment {
	return &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{Replicas: &desired},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: ready, AvailableReplicas: ready, UpdatedReplicas: ready, Conditions: conds,
		},
	}
}
