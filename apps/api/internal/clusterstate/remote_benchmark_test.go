package clusterstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
)

func largeProjectionFixture() map[string]*v1.Resource {
	out := make(map[string]*v1.Resource, 100_000)
	for i := 0; i < 2_000; i++ {
		id := fmt.Sprintf("w-%04d", i)
		out["w"+id] = &v1.Resource{Kind: v1.KindDeployment, Uid: id, Namespace: "ns", Name: id, Workload: &v1.WorkloadProjection{Desired: 49, Ready: 49, Available: 49, Updated: 49, RolloutStatus: "Complete", CreatedUnixMs: 1}}
	}
	for i := 0; i < 98_000; i++ {
		id := fmt.Sprintf("p-%05d", i)
		wid := fmt.Sprintf("w-%04d", i%2_000)
		out["p"+id] = &v1.Resource{Kind: v1.KindPod, Uid: id, Namespace: "ns", Name: id, Owners: []*v1.OwnerRef{{Uid: wid, Kind: v1.KindDeployment, Name: wid}}, Pod: &v1.PodProjection{Phase: "Running", CreatedUnixMs: 1, Containers: []*v1.ContainerStatus{{Name: "app", Ready: true, State: "Running", Image: "example/app@sha256:abc"}}}}
	}
	return out
}

func screenFixtureRequest(screen string) ScreenRequest {
	r := ScreenRequest{ClusterID: "a", Screen: screen, Namespaces: NamespaceFilter{All: true}, From: time.Unix(0, 0).UTC(), EventLimit: 50, UnhealthyLimit: 20}
	switch screen {
	case "namespace":
		r.Namespaces = NamespaceFilter{List: []string{"ns"}}
		r.RequestedNamespace = "ns"
	case "workload":
		r.Namespaces = NamespaceFilter{List: []string{"ns"}}
		r.RequestedNamespace = "ns"
		r.Kind = v1.KindDeployment
		r.Name = "w-0000"
	case "pod":
		r.Namespaces = NamespaceFilter{List: []string{"ns"}}
		r.RequestedNamespace = "ns"
		r.Name = "p-00000"
		r.EntityUID = "p-00000"
	}
	return r
}

func TestLargeScreenRepliesBoundedAndScoped(t *testing.T) {
	resources := largeProjectionFixture()
	limits := projectionPerformanceLimits()
	for _, screen := range []string{"overview", "namespace-list", "namespace", "workload", "pod", "topology", "logs"} {
		t.Run(screen, func(t *testing.T) {
			for sample := 0; sample < 3; sample++ {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				started := time.Now()
				p, err := ProjectScreen(screenFixtureRequest(screen), resources, time.Unix(2, 0))
				if err != nil {
					t.Fatal(err)
				}
				b, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				elapsed := time.Since(started)
				if raceEnabled {
					elapsed = 0
				}
				runtime.ReadMemStats(&after)
				if err := validatePerformanceSample(performanceSample{Latency: elapsed, ReplyBytes: len(b), TotalAlloc: after.TotalAlloc - before.TotalAlloc}, limits[screen]); err != nil {
					t.Fatalf("sample %d: %v", sample, err)
				}
				if err := validateProjectionJSON(b, screen); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	for screen := range limits {
		for sample := 0; sample < 3; sample++ {
			req := screenFixtureRequest(screen)
			allocs := testing.AllocsPerRun(1, func() {
				p, err := ProjectScreen(req, resources, time.Unix(2, 0))
				if err != nil {
					panic(err)
				}
				if _, err = json.Marshal(p); err != nil {
					panic(err)
				}
			})
			if err := validatePerformanceSample(performanceSample{AllocCount: allocs}, limits[screen]); err != nil {
				t.Fatalf("%s sample %d: %v (allocs=%.0f)", screen, sample, err, allocs)
			}
		}
	}
}

type performanceSample struct {
	Latency    time.Duration
	ReplyBytes int
	TotalAlloc uint64
	AllocCount float64
}

type performanceLimit performanceSample

func projectionPerformanceLimits() map[string]performanceLimit {
	return map[string]performanceLimit{
		"overview": {1500 * time.Millisecond, 4 << 20, 64 << 20, 50_000}, "namespace-list": {1500 * time.Millisecond, 4 << 20, 64 << 20, 50_000},
		"namespace": {1500 * time.Millisecond, 4 << 20, 64 << 20, 50_000}, "workload": {500 * time.Millisecond, 4 << 20, 16 << 20, 15_000},
		"pod": {250 * time.Millisecond, 1 << 20, 1 << 20, 1_000}, "topology": {1500 * time.Millisecond, 4 << 20, 64 << 20, 50_000}, "logs": {250 * time.Millisecond, 1 << 20, 1 << 20, 200},
	}
}

func validatePerformanceSample(got performanceSample, limit performanceLimit) error {
	if got.Latency > limit.Latency {
		return errors.New("latency budget")
	}
	if got.ReplyBytes > limit.ReplyBytes {
		return errors.New("reply byte budget")
	}
	if got.TotalAlloc > limit.TotalAlloc {
		return errors.New("allocation byte budget")
	}
	if got.AllocCount > limit.AllocCount {
		return errors.New("allocation count budget")
	}
	return nil
}

func TestPerformanceCheckerRejectsIndependentMutations(t *testing.T) {
	limit := performanceLimit{Latency: time.Second, ReplyBytes: 100, TotalAlloc: 1000, AllocCount: 100}
	valid := performanceSample{Latency: time.Millisecond, ReplyBytes: 10, TotalAlloc: 10, AllocCount: 10}
	if err := validatePerformanceSample(valid, limit); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*performanceSample){
		"latency": func(s *performanceSample) { s.Latency = limit.Latency + 1 },
		"reply":   func(s *performanceSample) { s.ReplyBytes = limit.ReplyBytes + 1 },
		"alloc":   func(s *performanceSample) { s.TotalAlloc = limit.TotalAlloc + 1 },
		"count":   func(s *performanceSample) { s.AllocCount = limit.AllocCount + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			x := valid
			mutate(&x)
			if validatePerformanceSample(x, limit) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}

func BenchmarkProjectScreen100k(b *testing.B) {
	resources := largeProjectionFixture()
	for _, screen := range []string{"overview", "namespace-list", "namespace", "workload", "pod", "topology", "logs"} {
		b.Run(screen, func(b *testing.B) {
			req := screenFixtureRequest(screen)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				p, err := ProjectScreen(req, resources, time.Unix(2, 0))
				if err != nil {
					b.Fatal(err)
				}
				encoded, _ := json.Marshal(p)
				b.ReportMetric(float64(len(encoded)), "reply-B")
			}
		})
	}
}
