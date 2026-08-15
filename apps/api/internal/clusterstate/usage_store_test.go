package clusterstate

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

func TestUsageStoreClusterIsolationAndLocalEnrichment(t *testing.T) {
	catalog, _ := NewRemoteCatalog([]string{"a", "b"}, 10)
	for _, id := range []string{"a", "b"} {
		applyCatalogSnapshot(t, catalog, id, 1, 0, &v1.CatalogResource{Kind: v1.KindPod, Uid: "same", Namespace: "ns", Name: "pod"})
	}
	usage, _ := NewUsageStore([]string{"a", "b"}, 2)
	_ = usage.Update("a", map[string]contract.ContainerUsage{"same": {CPUMilli: 10, MemoryMib: 20}})
	_ = usage.Update("b", map[string]contract.ContainerUsage{"same": {CPUMilli: 100, MemoryMib: 200}})
	projection := func(id string) *ScreenProjection {
		return &ScreenProjection{Request: ScreenRequest{ClusterID: id, Screen: "pod"}, ResolvedUID: "same", PodSummaryValue: &contract.PodSummary{UID: "same", Usage: contract.ResourceUsage{CPURequestMilli: 5, MemoryRequestMib: 10}}}
	}
	a, b := projection("a"), projection("b")
	EnrichUsage(a, catalog, usage)
	EnrichUsage(b, catalog, usage)
	if a.PodSummaryValue.Usage.CPUMilli != 10 || b.PodSummaryValue.Usage.CPUMilli != 100 || a.PodSummaryValue.Usage.CPUVsRequest != 2 || b.PodSummaryValue.Usage.MemoryMib != 200 {
		t.Fatalf("usage leaked: A=%+v B=%+v", a.PodSummaryValue.Usage, b.PodSummaryValue.Usage)
	}
	if err := usage.Update("a", map[string]contract.ContainerUsage{"x": {}, "y": {}, "z": {}}); err == nil {
		t.Fatal("oversized usage accepted")
	}
	if got, ok := usage.Lookup("a", "same"); !ok || got.CPUMilli != 10 {
		t.Fatalf("failed update replaced last known: %+v/%v", got, ok)
	}
	if err := catalog.Apply(&v1.WatchFrame{ClusterId: "a", Type: v1.WatchFrameType_WATCH_EXPIRED, ObservedUnixMs: 2000}); err != nil {
		t.Fatal(err)
	}
	expired := projection("a")
	EnrichUsage(expired, catalog, usage)
	if expired.PodSummaryValue.Usage.CPUMilli != 0 {
		t.Fatalf("expired catalog retained dynamic usage: %+v", expired.PodSummaryValue.Usage)
	}
}

func TestUsageStoreAggregateCapsLastGoodAndClusterReadIsolation(t *testing.T) {
	u, err := NewUsageStoreWithLimits([]string{"a", "b"}, UsageLimits{PerClusterEntries: 2, TotalEntries: 3, PerClusterBytes: 512, TotalBytes: 512, PeakEntries: 6, PeakBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err = u.Update("a", map[string]contract.ContainerUsage{"a1": {CPUMilli: 1}, "a2": {CPUMilli: 2}}); err != nil {
		t.Fatal(err)
	}
	if err = u.Update("b", map[string]contract.ContainerUsage{"b1": {CPUMilli: 3}}); err != nil {
		t.Fatal(err)
	}
	entries, bytes := u.Retained()
	if entries != 3 || bytes != 390 {
		t.Fatalf("retained=%d/%d", entries, bytes)
	}
	if err = u.Update("b", map[string]contract.ContainerUsage{"b1": {CPUMilli: 4}, "b2": {CPUMilli: 5}}); err == nil {
		t.Fatal("aggregate overflow accepted")
	}
	if got, ok := u.Lookup("b", "b1"); !ok || got.CPUMilli != 3 {
		t.Fatalf("failed update replaced B: %+v/%v", got, ok)
	}
	before := u.clusters["a"].value.Load()
	if err = u.Update("a", map[string]contract.ContainerUsage{"a2": {CPUMilli: 2}, "a1": {CPUMilli: 1}}); err != nil {
		t.Fatal(err)
	}
	if after := u.clusters["a"].value.Load(); after != before {
		t.Fatal("identical snapshot was reallocated")
	}
	if _, ok := u.Lookup("a", "unknown"); ok {
		t.Fatal("unknown UID accepted")
	}
}

func TestUsageStoreConcurrentAggregateReservation(t *testing.T) {
	u, err := NewUsageStoreWithLimits([]string{"a", "b"}, UsageLimits{PerClusterEntries: 60, TotalEntries: 100, PerClusterBytes: 16 << 10, TotalBytes: 32 << 10, PeakEntries: 120, PeakBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	values := func(prefix string) map[string]contract.ContainerUsage {
		out := make(map[string]contract.ContainerUsage, 60)
		for i := 0; i < 60; i++ {
			out[fmt.Sprintf("%s-%02d", prefix, i)] = contract.ContainerUsage{CPUMilli: i}
		}
		return out
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, input := range []struct {
		id     string
		values map[string]contract.ContainerUsage
	}{{"a", values("a")}, {"b", values("b")}} {
		input := input
		go func() {
			<-start
			results <- u.Update(input.id, input.values)
		}()
	}
	close(start)
	successes := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successes=%d", successes)
	}
	entries, _ := u.Retained()
	if entries != 60 || u.buildEntries != 0 || u.buildBytes != 0 || u.reservedEntries != 0 || u.reservedBytes != 0 {
		t.Fatalf("accounting entries=%d build=%d/%d reserved=%d/%d", entries, u.buildEntries, u.buildBytes, u.reservedEntries, u.reservedBytes)
	}
}

func TestUsageStore100kUpdateAndO1LookupBudgets(t *testing.T) {
	u, err := NewUsageStore([]string{"a", "b"}, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]contract.ContainerUsage, 100_000)
	for i := 0; i < 100_000; i++ {
		values[fmt.Sprintf("pod-%06d", i)] = contract.ContainerUsage{CPUMilli: i, MemoryMib: i * 2}
	}
	for sample := 0; sample < 3; sample++ {
		if sample > 0 {
			values["pod-000000"] = contract.ContainerUsage{CPUMilli: sample}
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		started := time.Now()
		if err = u.Update("a", values); err != nil {
			t.Fatal(err)
		}
		latency := time.Since(started)
		runtime.ReadMemStats(&after)
		allocated := after.TotalAlloc - before.TotalAlloc
		if latency > 1500*time.Millisecond || allocated > 32<<20 {
			t.Fatalf("sample=%d latency=%v allocated=%d", sample, latency, allocated)
		}
		t.Logf("sample=%d update=%v/%dB", sample, latency, allocated)
	}
	if allocations := testing.AllocsPerRun(1000, func() { _, _ = u.Lookup("a", "pod-099999") }); allocations != 0 {
		t.Fatalf("lookup allocations=%f", allocations)
	}

	// A replacement does not acquire B's read path; readers only load B's
	// immutable atomic snapshot.
	iso, err := NewUsageStoreWithLimits([]string{"a", "b"}, UsageLimits{PerClusterEntries: 100_000, TotalEntries: 100_001, PerClusterBytes: 32 << 20, TotalBytes: 32 << 20, PeakEntries: 200_001, PeakBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err = iso.Update("b", map[string]contract.ContainerUsage{"b": {CPUMilli: 7}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if updateErr := iso.Update("a", values); updateErr != nil {
			t.Errorf("A update: %v", updateErr)
		}
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for i := 0; i < 10_000; i++ {
		if value, ok := iso.Lookup("b", "b"); !ok || value.CPUMilli != 7 {
			t.Fatalf("B read changed: %+v/%v", value, ok)
		}
		if time.Now().After(deadline) {
			t.Fatal("B reads blocked by A update")
		}
	}
	wg.Wait()
}

func TestUsageEnrichment100kScreenBudgets(t *testing.T) {
	const podCount = 100_000
	catalog, err := NewRemoteCatalog([]string{"a"}, podCount)
	if err != nil {
		t.Fatal(err)
	}
	if err = catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_BEGIN, ObservedUnixMs: 1000}); err != nil {
		t.Fatal(err)
	}
	usageValues := make(map[string]contract.ContainerUsage, podCount)
	for start := 0; start < podCount; start += 1000 {
		resources := make([]*v1.CatalogResource, 1000)
		for i := range resources {
			index := start + i
			uid := fmt.Sprintf("pod-%06d", index)
			workloadUID := fmt.Sprintf("workload-%03d", index%100)
			resources[i] = &v1.CatalogResource{Kind: v1.KindPod, Uid: uid, Namespace: "ns", Name: uid, Owners: []*v1.CatalogOwner{{Kind: v1.KindDeployment, Uid: workloadUID, Name: workloadUID}}}
			usageValues[uid] = contract.ContainerUsage{CPUMilli: 1, MemoryMib: 2}
		}
		if err = catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_CHUNK, Resources: resources, ObservedUnixMs: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	if err = catalog.Apply(&v1.WatchFrame{ClusterId: "a", Epoch: 1, Type: v1.WatchFrameType_WATCH_SNAPSHOT_COMMIT, ObservedUnixMs: 1000}); err != nil {
		t.Fatal(err)
	}
	store, err := NewUsageStore([]string{"a"}, podCount)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update("a", usageValues); err != nil {
		t.Fatal(err)
	}

	makeProjection := func(screen string) *ScreenProjection {
		data := &ScreenProjection{Request: ScreenRequest{ClusterID: "a", Screen: screen, RequestedNamespace: "ns"}, ResolvedUID: "workload-000"}
		switch screen {
		case "namespace-list":
			data.Namespaces = []contract.NamespaceSummary{{Name: "ns"}}
		case "namespace":
			data.Namespace = &contract.NamespaceSummary{Name: "ns"}
			data.WorkloadsList = make([]contract.WorkloadSummary, 100)
			for i := range data.WorkloadsList {
				data.WorkloadsList[i].Ref.WorkloadUID = fmt.Sprintf("workload-%03d", i)
			}
		case "workload":
			data.WorkloadValue = &contract.WorkloadSummary{Ref: contract.EntityRef{WorkloadUID: "workload-000"}}
			data.PodsList = make([]contract.PodSummary, 1000)
			for i := range data.PodsList {
				data.PodsList[i].UID = fmt.Sprintf("pod-%06d", i*100)
			}
		}
		return data
	}
	for _, screen := range []string{"namespace-list", "namespace", "workload"} {
		for sample := 0; sample < 3; sample++ {
			data := makeProjection(screen)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			started := time.Now()
			EnrichUsage(data, catalog, store)
			latency := time.Since(started)
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			if latency > 500*time.Millisecond || allocated > 8<<20 {
				t.Fatalf("screen=%s sample=%d latency=%v allocated=%d", screen, sample, latency, allocated)
			}
			switch screen {
			case "namespace-list":
				if data.Namespaces[0].Usage.CPUMilli != podCount {
					t.Fatal("namespace-list usage mismatch")
				}
			case "namespace":
				if data.Namespace.Usage.CPUMilli != podCount || data.WorkloadsList[0].Usage.CPUMilli != 1000 {
					t.Fatal("namespace usage mismatch")
				}
			case "workload":
				if data.WorkloadValue.Usage.CPUMilli != 1000 || data.PodsList[0].Usage.CPUMilli != 1 {
					t.Fatal("workload usage mismatch")
				}
			}
			t.Logf("screen=%s sample=%d enrich=%v/%dB", screen, sample, latency, allocated)
		}
	}
}
