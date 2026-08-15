package transport

import (
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"google.golang.org/protobuf/proto"
)

func validScreenQuery() *v1.ScreenQuery {
	return &v1.ScreenQuery{
		ClusterId:      "cluster-a",
		Screen:         "overview",
		Scope:          &v1.NamespaceScope{All: true},
		EventLimit:     50,
		UnhealthyLimit: 20,
	}
}

func TestScreenRequestRejectsUntrustedShapeAndScope(t *testing.T) {
	tooManyNamespaces := make([]string, 257)
	for i := range tooManyNamespaces {
		tooManyNamespaces[i] = fmt.Sprintf("n-%03d", i)
	}
	tooManyScopeBytes := make([]string, 256)
	for i := range tooManyScopeBytes {
		tooManyScopeBytes[i] = fmt.Sprintf("n-%03d-%s", i, strings.Repeat("a", 55))
	}
	tests := []struct {
		name   string
		mutate func(*v1.ScreenQuery) *v1.ScreenQuery
	}{
		{name: "nil", mutate: func(*v1.ScreenQuery) *v1.ScreenQuery { return nil }},
		{name: "invalid cluster", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.ClusterId = "INVALID"; return q }},
		{name: "unknown screen", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.Screen = "admin"; return q }},
		{name: "nil scope", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.Scope = nil; return q }},
		{name: "zero event limit", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.EventLimit = 0; return q }},
		{name: "large event limit", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.EventLimit = 1001; return q }},
		{name: "zero unhealthy limit", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.UnhealthyLimit = 0; return q }},
		{name: "long entity uid", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.EntityUid = strings.Repeat("u", 254); return q }},
		{name: "long kind", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.Kind = strings.Repeat("k", 65); return q }},
		{name: "all with list", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.Scope.Namespaces = []string{"ns"}; return q }},
		{name: "restricted empty", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.Scope.All = false; return q }},
		{name: "too many namespaces", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery {
			q.Scope = &v1.NamespaceScope{Namespaces: tooManyNamespaces}
			return q
		}},
		{name: "unsorted namespaces", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery {
			q.Scope = &v1.NamespaceScope{Namespaces: []string{"z", "a"}}
			return q
		}},
		{name: "duplicate namespaces", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery {
			q.Scope = &v1.NamespaceScope{Namespaces: []string{"a", "a"}}
			return q
		}},
		{name: "invalid namespace", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery {
			q.Scope = &v1.NamespaceScope{Namespaces: []string{"Bad"}}
			return q
		}},
		{name: "scope byte cap", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery {
			q.Scope = &v1.NamespaceScope{Namespaces: tooManyScopeBytes}
			return q
		}},
		{name: "requested outside scope", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery {
			q.Scope = &v1.NamespaceScope{Namespaces: []string{"allowed"}}
			q.RequestedNamespace = "denied"
			return q
		}},
		{name: "namespace screen missing namespace", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.Screen = "namespace"; return q }},
		{name: "workload missing identity", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.Screen = "workload"; q.RequestedNamespace = "ns"; return q }},
		{name: "pod missing name", mutate: func(q *v1.ScreenQuery) *v1.ScreenQuery { q.Screen = "pod"; q.RequestedNamespace = "ns"; return q }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := tt.mutate(proto.Clone(validScreenQuery()).(*v1.ScreenQuery))
			if _, err := screenRequest(q); err == nil {
				t.Fatal("invalid query accepted")
			}
		})
	}
}

func TestScreenRequestPreservesAuthorizedBoundedRequest(t *testing.T) {
	q := validScreenQuery()
	q.Screen = "workload"
	q.Scope = &v1.NamespaceScope{Namespaces: []string{"ns-a", "ns-b"}}
	q.RequestedNamespace = "ns-b"
	q.EntityUid = "uid"
	q.Kind = "Deployment"
	q.Name = "api"
	q.FromUnixMs = 1234
	got, err := screenRequest(q)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClusterID != q.ClusterId || got.Screen != q.Screen || got.RequestedNamespace != q.RequestedNamespace || got.EntityUID != q.EntityUid || got.Kind != q.Kind || got.Name != q.Name || got.EventLimit != int(q.EventLimit) || got.UnhealthyLimit != int(q.UnhealthyLimit) || !got.From.Equal(time.UnixMilli(q.FromUnixMs).UTC()) || got.Namespaces.All || !got.Namespaces.Allows("ns-a") || got.Namespaces.Allows("ns-c") {
		t.Fatalf("request changed across transport boundary: %+v", got)
	}
}
