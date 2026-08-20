package clusterstate_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

func TestDirectAndRemoteProviderParity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	direct, _ := testcluster.NewStore(t, ctx)
	usageValues := map[string]contract.ContainerUsage{}
	for i, pod := range direct.CatalogPods(testcluster.ClusterID, "", 0) {
		usageValues[pod.UID] = contract.ContainerUsage{CPUMilli: 10 + i, MemoryMib: 20 + i}
	}
	direct.SetUsage(func(uid string) (contract.ContainerUsage, bool) {
		value, ok := usageValues[uid]
		return value, ok
	})
	resources, err := direct.SafeProjection(100_000, true)
	if err != nil {
		t.Fatal(err)
	}
	state := make(map[string]*v1.Resource, len(resources))
	for _, x := range resources {
		state[x.Kind+"\x00"+x.Uid] = x
	}
	all := clusterstate.NamespaceFilter{All: true}
	from := testcluster.Now.Add(-24 * time.Hour)
	catalog, err := clusterstate.NewRemoteCatalog([]string{testcluster.ClusterID}, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if err = catalog.Apply(&v1.WatchFrame{ClusterId: testcluster.ClusterID, Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: testcluster.Now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	var catalogResources []*v1.CatalogResource
	for _, pod := range direct.CatalogPods(testcluster.ClusterID, "", 0) {
		x := &v1.CatalogResource{Kind: v1.KindPod, Uid: pod.UID, Namespace: pod.Namespace, Name: pod.Name, NodeName: pod.Node}
		if pod.WorkloadUID != "" {
			x.Owners = []*v1.CatalogOwner{{Kind: pod.WorkloadKind, Uid: pod.WorkloadUID, Name: pod.WorkloadName}}
		}
		catalogResources = append(catalogResources, x)
	}
	if err = catalog.Apply(&v1.WatchFrame{ClusterId: testcluster.ClusterID, Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, Resources: catalogResources, ObservedUnixMs: testcluster.Now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err = catalog.Apply(&v1.WatchFrame{ClusterId: testcluster.ClusterID, Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: testcluster.Now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	usageStore, err := clusterstate.NewUsageStore([]string{testcluster.ClusterID}, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if err = usageStore.Update(testcluster.ClusterID, usageValues); err != nil {
		t.Fatal(err)
	}
	project := func(screen, ns, kind, name, uid string, f clusterstate.NamespaceFilter) *clusterstate.RemoteProvider {
		req := clusterstate.ScreenRequest{ClusterID: testcluster.ClusterID, Screen: screen, Namespaces: f, RequestedNamespace: ns, Kind: kind, Name: name, EntityUID: uid, From: from, EventLimit: 50, UnhealthyLimit: 20}
		data, err := clusterstate.ProjectScreen(req, state, testcluster.Now)
		if err != nil {
			t.Fatal(err)
		}
		clusterstate.EnrichUsage(data, catalog, usageStore)
		return &clusterstate.RemoteProvider{Data: data}
	}
	equal := func(name string, a, b any) {
		t.Helper()
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s mismatch\ndirect=%#v\nremote=%#v", name, a, b)
		}
	}
	overview := project("overview", "", "", "", "", all)
	a, _ := direct.NodeHealth()
	b, _ := overview.NodeHealth()
	equal("nodes", a, b)
	pa, _ := direct.PodHealth(all)
	pb, _ := overview.PodHealth(all)
	equal("pod health", pa, pb)
	wa, _ := direct.WorkloadHealth(all)
	wb, _ := overview.WorkloadHealth(all)
	equal("workload health", wa, wb)
	ua, _ := direct.Unhealthy(all, 20)
	ub, _ := overview.Unhealthy(all, 20)
	equal("unhealthy", ua, ub)
	ea, _ := direct.Events(all, from, 50)
	eb, _ := overview.Events(all, from, 50)
	equal("events", ea, eb)
	denied := clusterstate.NamespaceFilter{List: []string{"payments"}}
	if _, err := overview.PodHealth(denied); err == nil {
		t.Fatal("pod health accepted a mismatched scope")
	}
	if _, err := overview.WorkloadHealth(denied); err == nil {
		t.Fatal("workload health accepted a mismatched scope")
	}
	if _, err := overview.Unhealthy(all, 21); err == nil {
		t.Fatal("unhealthy accepted a mismatched limit")
	}
	if _, err := overview.Events(all, from.Add(time.Second), 50); err == nil {
		t.Fatal("events accepted a mismatched time range")
	}
	if _, err := overview.TopologyPods(all, 20); err == nil {
		t.Fatal("overview exposed topology-only data")
	}
	if (&clusterstate.RemoteProvider{Data: overview.Data, Stale: true}).HasSynced() {
		t.Fatal("stale remote provider reported synced")
	}
	if (&clusterstate.RemoteProvider{}).HasSynced() || (*clusterstate.RemoteProvider)(nil).HasSynced() || !(*clusterstate.RemoteProvider)(nil).ObservedAt().IsZero() {
		t.Fatal("nil remote provider reported state")
	}

	nsFilter := clusterstate.NamespaceFilter{List: []string{"payments"}}
	nsl := project("namespace-list", "", "", "", "", all)
	na, _ := direct.NamespaceSummaries(all)
	nb, _ := nsl.NamespaceSummaries(all)
	equal("namespaces", na, nb)
	if _, err := nsl.NamespaceSummaries(denied); err == nil {
		t.Fatal("namespace list accepted a mismatched scope")
	}
	nsd := project("namespace", "payments", "", "", "", nsFilter)
	nsa, fa, _ := direct.NamespaceSummary("payments")
	nsb, fb, _ := nsd.NamespaceSummary("payments")
	equal("namespace found", fa, fb)
	equal("namespace", nsa, nsb)
	wla, _ := direct.Workloads(nsFilter)
	wlb, _ := nsd.Workloads(nsFilter)
	equal("namespace workloads", wla, wlb)
	if _, err := nsd.Workloads(all); err == nil {
		t.Fatal("namespace workloads accepted a mismatched scope")
	}
	nea, _ := direct.Events(nsFilter, from, 50)
	neb, _ := nsd.Events(nsFilter, from, 50)
	equal("namespace events", nea, neb)
	logs := project("logs", "payments", "", "", "", nsFilter)
	leb, _ := logs.Events(nsFilter, from, 50)
	equal("logs events", nea, leb)

	work := project("workload", "payments", "Deployment", "payments-api", "", nsFilter)
	wda, wfa, _ := direct.Workload("payments", "Deployment", "payments-api")
	wdb, wfb, _ := work.Workload("payments", "Deployment", "payments-api")
	equal("workload found", wfa, wfb)
	equal("workload", wda, wdb)
	pda, _ := direct.PodsForWorkload("payments", "Deployment", "payments-api", wda.Ref.WorkloadUID)
	pdb, _ := work.PodsForWorkload("payments", "Deployment", "payments-api", wdb.Ref.WorkloadUID)
	equal("workload pods", pda, pdb)
	ewa, _ := direct.EventsForUID(wda.Ref.WorkloadUID, from, 50)
	ewb, _ := work.EventsForUID(wdb.Ref.WorkloadUID, from, 50)
	equal("workload events", ewa, ewb)
	equal("workload owners", direct.WorkloadOwnerChain("payments", "Deployment", "payments-api", wda.Ref.WorkloadUID), work.WorkloadOwnerChain("payments", "Deployment", "payments-api", wdb.Ref.WorkloadUID))
	if _, err := work.PodsForWorkload("other", "Deployment", "payments-api", wdb.Ref.WorkloadUID); err == nil {
		t.Fatal("wrong workload namespace accepted")
	}
	if _, found, _ := work.Workload("payments", "Deployment", "wrong"); found {
		t.Fatal("wrong workload name accepted")
	}
	if _, err := work.PodsForWorkload("payments", "Deployment", "wrong", wdb.Ref.WorkloadUID); err == nil {
		t.Fatal("wrong pod-list workload name accepted")
	}
	if _, err := work.PodsForWorkload("payments", "Deployment", "payments-api", "wrong"); err == nil {
		t.Fatal("wrong pod-list workload UID accepted")
	}
	if _, err := work.EventsForUID("wrong", from, 50); err == nil {
		t.Fatal("workload events accepted wrong UID")
	}
	if _, err := work.EventsForUID(wdb.Ref.WorkloadUID, from, 49); err == nil {
		t.Fatal("workload events accepted wrong limit")
	}
	if got := work.WorkloadOwnerChain("payments", "StatefulSet", "payments-api", wdb.Ref.WorkloadUID); got != nil {
		t.Fatal("wrong workload kind accepted")
	}
	if got := work.WorkloadOwnerChain("payments", "Deployment", "payments-api", "wrong"); got != nil {
		t.Fatal("wrong workload owner UID accepted")
	}

	pod := project("pod", "payments", "", "payments-api-7f-bbb", testcluster.UIDPodCrashLoop, nsFilter)
	p1, fa, _ := direct.Pod("payments", "payments-api-7f-bbb", testcluster.UIDPodCrashLoop)
	p2, fb, _ := pod.Pod("payments", "payments-api-7f-bbb", testcluster.UIDPodCrashLoop)
	equal("pod found", fa, fb)
	equal("pod summary", direct.PodSummary(p1), pod.PodSummary(p2))
	equal("pod owners", direct.PodOwnerChain(p1), pod.PodOwnerChain(p2))
	equal("containers", clusterstate.ContainerStatuses(p1), clusterstate.ContainerStatuses(p2))
	if _, found, _ := pod.Pod("payments", "payments-api-7f-bbb", "wrong"); found {
		t.Fatal("wrong pod UID accepted")
	}
	if _, found, _ := pod.Pod("other", "payments-api-7f-bbb", testcluster.UIDPodCrashLoop); found {
		t.Fatal("wrong pod namespace accepted")
	}
	if _, found, _ := pod.Pod("payments", "wrong", testcluster.UIDPodCrashLoop); found {
		t.Fatal("wrong pod name accepted")
	}
	if got := pod.PodSummary(nil); got.UID != "" {
		t.Fatal("nil input pod produced a summary")
	}
	if got := pod.PodOwnerChain(nil); got != nil {
		t.Fatal("nil input pod produced owners")
	}
	wrongPod := p2.DeepCopy()
	wrongPod.Name = "wrong"
	if got := pod.PodSummary(wrongPod); got.UID != "" {
		t.Fatal("wrong input pod accepted")
	}
	if got := pod.PodOwnerChain(wrongPod); got != nil {
		t.Fatal("wrong owner input pod accepted")
	}
	podByName := project("pod", "payments", "", "payments-api-7f-bbb", "", nsFilter)
	p3, found, _ := podByName.Pod("payments", "payments-api-7f-bbb", "")
	if !found {
		t.Fatal("UID-omitted name lookup failed")
	}
	equal("UID-omitted pod", direct.PodSummary(p1), podByName.PodSummary(p3))
	notFound := project("pod", "payments", "", "missing", "", nsFilter)
	if _, found, _ := notFound.Pod("payments", "missing", ""); found {
		t.Fatal("missing pod found")
	}

	top := project("topology", "", "", "", "", all)
	ta, _ := direct.TopologyPods(all, 20)
	tb, _ := top.TopologyPods(all, 20)
	equal("topology", ta, tb)
	if _, err := top.TopologyPods(denied, 20); err == nil {
		t.Fatal("topology accepted a mismatched scope")
	}
	if _, err := top.TopologyPods(all, 21); err == nil {
		t.Fatal("topology accepted a mismatched limit")
	}
}
