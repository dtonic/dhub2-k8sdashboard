package clusterstate

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

func TestEventTimestampIdentityAndSeverityFallbacks(t *testing.T) {
	base := time.Unix(1_000, 0).UTC()
	for name, tc := range map[string]struct {
		event *corev1.Event
		want  time.Time
	}{
		"last timestamp":  {&corev1.Event{LastTimestamp: metav1.NewTime(base.Add(4 * time.Minute)), EventTime: metav1.NewMicroTime(base.Add(3 * time.Minute)), FirstTimestamp: metav1.NewTime(base.Add(2 * time.Minute)), ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)}}, base.Add(4 * time.Minute)},
		"event time":      {&corev1.Event{EventTime: metav1.NewMicroTime(base.Add(3 * time.Minute)), FirstTimestamp: metav1.NewTime(base.Add(2 * time.Minute)), ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)}}, base.Add(3 * time.Minute)},
		"first timestamp": {&corev1.Event{FirstTimestamp: metav1.NewTime(base.Add(2 * time.Minute)), ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)}}, base.Add(2 * time.Minute)},
		"creation":        {&corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)}}, base},
	} {
		t.Run(name, func(t *testing.T) {
			if got := eventTime(tc.event); !got.Equal(tc.want) {
				t.Fatalf("eventTime=%v want=%v", got, tc.want)
			}
		})
	}

	s := &Store{opts: Options{ClusterID: "cluster-a"}}
	podEvent := &corev1.Event{ObjectMeta: metav1.ObjectMeta{UID: types.UID("event-pod"), Namespace: "ns"}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "pod", UID: types.UID("pod-uid")}}
	pod := s.toClusterEvent(podEvent, base)
	if pod.Type != "Normal" || pod.Count != 1 || pod.Involved.ClusterID != "cluster-a" || pod.Involved.PodUID != "pod-uid" || pod.Involved.WorkloadUID != "" {
		t.Fatalf("pod event=%+v", pod)
	}
	workloadEvent := &corev1.Event{ObjectMeta: metav1.ObjectMeta{UID: types.UID("event-workload"), Namespace: "ns"}, Type: "Warning", Count: 3, InvolvedObject: corev1.ObjectReference{Kind: "Deployment", Name: "api", UID: types.UID("workload-uid")}}
	workload := s.toClusterEvent(workloadEvent, base)
	if workload.Type != "Warning" || workload.Count != 3 || workload.Involved.WorkloadKind != "Deployment" || workload.Involved.WorkloadUID != "workload-uid" || workload.Involved.PodUID != "" {
		t.Fatalf("workload event=%+v", workload)
	}

	ordered := []contract.Severity{contract.SeverityHealthy, contract.SeverityUnknown, contract.SeverityProgressing, contract.SeverityWarning, contract.SeverityDegraded, contract.SeverityCritical}
	for i, severity := range ordered {
		if rank := severityRank(severity); rank != i {
			t.Fatalf("severity=%s rank=%d want=%d", severity, rank, i)
		}
	}
}

func TestStatusFormattingAndDirectOwnerFallbacks(t *testing.T) {
	durations := []string{shortDuration(5 * time.Second), shortDuration(5 * time.Minute), shortDuration(5 * time.Hour), shortDuration(5 * 24 * time.Hour)}
	seen := map[string]bool{}
	for _, formatted := range durations {
		if formatted == "" || seen[formatted] {
			t.Fatalf("duration formatting is empty or ambiguous: %v", durations)
		}
		seen[formatted] = true
	}
	if got := firstNonEmpty("", "fallback", "ignored"); got != "fallback" {
		t.Fatalf("first non-empty=%q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("all-empty fallback=%q", got)
	}
	controller := true
	notController := false
	refs := []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "fallback", Controller: &notController}, {Kind: "Deployment", Name: "controller", Controller: &controller}}
	if owner := directOwner(refs); owner == nil || owner.Name != "controller" {
		t.Fatalf("controller owner=%+v", owner)
	}
	if owner := directOwner(refs[:1]); owner == nil || owner.Name != "fallback" {
		t.Fatalf("fallback owner=%+v", owner)
	}
	if owner := directOwner(nil); owner != nil {
		t.Fatalf("empty owner=%+v", owner)
	}
	stalled := contract.WorkloadSummary{Issues: []contract.IssueReason{contract.IssueRolloutStalled}}
	healthy := contract.WorkloadSummary{Replicas: contract.ReplicaCounts{Ready: 2, Desired: 3}}
	if workloadReason(stalled) == workloadReason(healthy) || workloadReason(healthy) == "" {
		t.Fatal("stalled and replica status reasons were not distinguished")
	}
}
