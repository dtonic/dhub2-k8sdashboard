package clusterstate

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/mask"
)

// SafeProjection returns only fields representable by the closed v1 schema.
func (s *Store) SafeProjection(max int) ([]*v1.Resource, error) {
	out := make([]*v1.Resource, 0, 1024)
	add := func(x *v1.Resource) error {
		if max > 0 && len(out) >= max {
			return fmt.Errorf("projection resource limit")
		}
		out = append(out, x)
		return nil
	}
	pods, e := s.listPods("")
	if e != nil {
		return nil, e
	}
	for _, p := range pods {
		cs := make([]*v1.ContainerStatus, 0, len(p.Status.ContainerStatuses))
		specs := map[string]corev1.Container{}
		for _, c := range p.Spec.Containers {
			specs[c.Name] = c
		}
		for _, c := range p.Status.ContainerStatuses {
			state, reason, message, last := "Waiting", "", "", ""
			if c.State.Waiting != nil {
				reason, message = c.State.Waiting.Reason, c.State.Waiting.Message
			} else if c.State.Running != nil {
				state = "Running"
			} else if c.State.Terminated != nil {
				state, reason, message = "Terminated", c.State.Terminated.Reason, c.State.Terminated.Message
			}
			message, _ = mask.Apply(message)
			lastExit, lastFinished := int32(0), int64(0)
			if c.LastTerminationState.Terminated != nil {
				last = c.LastTerminationState.Terminated.Reason
				lastExit = c.LastTerminationState.Terminated.ExitCode
				lastFinished = c.LastTerminationState.Terminated.FinishedAt.UnixMilli()
			}
			started := false
			if c.Started != nil {
				started = *c.Started
			}
			spec := specs[c.Name]
			cs = append(cs, &v1.ContainerStatus{Name: c.Name, Ready: c.Ready, Restarts: c.RestartCount, State: state, Reason: reason, MaskedMessage: message, LastTerminationReason: last, LastExitCode: lastExit, LastFinishedUnixMs: lastFinished, Image: spec.Image, ImageId: c.ImageID, Started: started, LivenessProbe: spec.LivenessProbe != nil, ReadinessProbe: spec.ReadinessProbe != nil})
		}
		started := int64(0)
		if p.Status.StartTime != nil {
			started = p.Status.StartTime.UnixMilli()
		}
		deleted := int64(0)
		if p.DeletionTimestamp != nil {
			deleted = p.DeletionTimestamp.UnixMilli()
		}
		u := PodRequests(p)
		pp := &v1.PodProjection{Phase: string(p.Status.Phase), Reason: p.Status.Reason, CreatedUnixMs: p.CreationTimestamp.UnixMilli(), Containers: cs, NodeName: p.Spec.NodeName, StartedUnixMs: started, DeletedUnixMs: deleted, CpuRequestMilli: int64(u.CPURequestMilli), MemoryRequestBytes: int64(u.MemoryRequestMib) << 20}
		if u.CPULimitMilli != nil {
			pp.HasCpuLimit = true
			pp.CpuLimitMilli = int64(*u.CPULimitMilli)
		}
		if u.MemoryLimitMib != nil {
			pp.HasMemoryLimit = true
			pp.MemoryLimitBytes = int64(*u.MemoryLimitMib) << 20
		}
		if e := add(&v1.Resource{Kind: v1.KindPod, Uid: string(p.UID), Namespace: p.Namespace, Name: p.Name, Owners: owners(p.OwnerReferences), Pod: pp}); e != nil {
			return nil, e
		}
	}
	nodes, e := s.nodes.List(labelsEverything)
	if e != nil {
		return nil, e
	}
	for _, n := range nodes {
		ready, pressure, unsched := NormalizeNode(n)
		if e := add(&v1.Resource{Kind: v1.KindNode, Uid: string(n.UID), Name: n.Name, Node: &v1.NodeProjection{Ready: ready, Pressure: pressure, Unschedulable: unsched}}); e != nil {
			return nil, e
		}
	}
	deps, e := s.listDeployments("")
	if e != nil {
		return nil, e
	}
	for _, x := range deps {
		st := NormalizeDeployment(x)
		if e := add(workloadProjection(v1.KindDeployment, string(x.UID), x.Namespace, x.Name, x.CreationTimestamp.UnixMilli(), x.OwnerReferences, st, images(x.Spec.Template.Spec.Containers))); e != nil {
			return nil, e
		}
	}
	sts, e := s.listStatefulSets("")
	if e != nil {
		return nil, e
	}
	for _, x := range sts {
		st := NormalizeStatefulSet(x)
		if e := add(workloadProjection(v1.KindStatefulSet, string(x.UID), x.Namespace, x.Name, x.CreationTimestamp.UnixMilli(), x.OwnerReferences, st, images(x.Spec.Template.Spec.Containers))); e != nil {
			return nil, e
		}
	}
	dss, e := s.listDaemonSets("")
	if e != nil {
		return nil, e
	}
	for _, x := range dss {
		st := NormalizeDaemonSet(x)
		if e := add(workloadProjection(v1.KindDaemonSet, string(x.UID), x.Namespace, x.Name, x.CreationTimestamp.UnixMilli(), x.OwnerReferences, st, images(x.Spec.Template.Spec.Containers))); e != nil {
			return nil, e
		}
	}
	cjs, e := s.listCronJobs("")
	if e != nil {
		return nil, e
	}
	for _, x := range cjs {
		st := NormalizeCronJob(x)
		if e := add(workloadProjection(v1.KindCronJob, string(x.UID), x.Namespace, x.Name, x.CreationTimestamp.UnixMilli(), x.OwnerReferences, st, images(x.Spec.JobTemplate.Spec.Template.Spec.Containers))); e != nil {
			return nil, e
		}
	}
	events, e := s.events.List(labelsEverything)
	if e != nil {
		return nil, e
	}
	for _, x := range events {
		msg, _ := mask.Apply(x.Message)
		if e := add(&v1.Resource{Kind: v1.KindEvent, Uid: string(x.UID), Namespace: x.Namespace, Name: x.Name, Event: &v1.EventProjection{InvolvedUid: string(x.InvolvedObject.UID), InvolvedKind: x.InvolvedObject.Kind, InvolvedName: x.InvolvedObject.Name, Reason: x.Reason, MaskedMessage: msg, Type: x.Type, LastSeenUnixMs: eventTime(x).UnixMilli(), Count: x.Count}}); e != nil {
			return nil, e
		}
	}
	for _, obj := range s.rsIndexer.List() {
		m, ok := obj.(*metav1.PartialObjectMetadata)
		if !ok {
			continue
		}
		refs := owners(m.OwnerReferences)
		if len(refs) > 0 {
			refs[0].Revision = m.Annotations[revisionAnnotation]
		}
		if e := add(&v1.Resource{Kind: v1.KindReplicaSet, Uid: string(m.UID), Namespace: m.Namespace, Name: m.Name, Owners: refs}); e != nil {
			return nil, e
		}
	}
	return out, nil
}
func owners(in []metav1.OwnerReference) []*v1.OwnerRef {
	out := make([]*v1.OwnerRef, 0, len(in))
	for _, x := range in {
		out = append(out, &v1.OwnerRef{Uid: string(x.UID), Kind: x.Kind, Name: x.Name})
	}
	return out
}
func workloadProjection(kind, uid, ns, name string, created int64, refs []metav1.OwnerReference, st WorkloadState, imgs []string) *v1.Resource {
	return &v1.Resource{Kind: kind, Uid: uid, Namespace: ns, Name: name, Owners: owners(refs), Workload: &v1.WorkloadProjection{Desired: int32(st.Replicas.Desired), Ready: int32(st.Replicas.Ready), Available: int32(st.Replicas.Available), Updated: int32(st.Replicas.Updated), RolloutStatus: st.Rollout.Status, RolloutReason: st.Rollout.Message, Images: imgs, CreatedUnixMs: created}}
}
