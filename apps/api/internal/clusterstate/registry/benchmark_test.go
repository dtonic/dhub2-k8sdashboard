package registry

import (
	"fmt"
	"testing"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"google.golang.org/protobuf/proto"
)

func benchmarkRegistry(b *testing.B, n int) *Registry {
	b.Helper()
	r, e := New(DefaultLimits())
	if e != nil {
		b.Fatal(e)
	}
	_ = r.Connect(&v1.Hello{ClusterId: "a", ProtocolVersion: 1}, "a")
	c := r.get("a")
	c.live = &Snapshot{Epoch: 1, Resources: make(map[string]*v1.Resource, n), sizes: make(map[string]int, n)}
	for i := 0; i < n; i++ {
		p := pod(fmt.Sprintf("p-%d", i))
		k, size := key(p), proto.Size(p)
		c.live.Resources[k] = p
		c.live.sizes[k] = size
		c.live.Bytes += size
	}
	return r
}
func BenchmarkDeltaFlat(b *testing.B) {
	for _, n := range []int{1000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			r := benchmarkRegistry(b, n)
			d := v1.Delta{Epoch: 1, Resource: pod("hot")}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				d.Seq = uint64(i + 1)
				if _, nack := r.Delta("a", &d); nack != nil {
					b.Fatal(nack)
				}
			}
		})
	}
}
func BenchmarkIndexedScreenLookup(b *testing.B) {
	r := benchmarkRegistry(b, 100000)
	wanted := key(pod("p-99999"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, e := r.View("a", func(s *Snapshot) error { _ = s.Resources[wanted]; return nil })
		if e != nil {
			b.Fatal(e)
		}
	}
}
