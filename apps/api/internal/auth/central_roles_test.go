package auth

import (
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"testing"
)

func TestCentralRolesRequireQualification(t *testing.T) {
	_, s := ScopeForCentral(Claims{Roles: []string{"cluster.viewer", "namespace.viewer:legacy", "cluster.viewer:a", "namespace.viewer:b/payments"}}, []scope.Cluster{{ID: "a"}, {ID: "b"}})
	a, _ := s.Cluster("a")
	b, _ := s.Cluster("b")
	if !a.All || b.All || !b.AllowsNamespace("payments") || b.AllowsNamespace("legacy") {
		t.Fatal(s)
	}
}

func TestCentralRoleMatrix(t *testing.T) {
	configured := []scope.Cluster{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}
	cases := []struct {
		name  string
		roles []string
		want  int
		allA  bool
		nsB   []string
	}{{"legacy ignored", []string{"cluster.viewer", "namespace.viewer:legacy"}, 0, false, nil}, {"dedupe sort", []string{"namespace.viewer:b/z", "namespace.viewer:b/a", "namespace.viewer:b/a"}, 1, false, []string{"a", "z"}}, {"admin bounded", []string{"platform.admin", "cluster.viewer:unknown"}, 2, true, nil}, {"unknown fail closed", []string{"unknown.role", "cluster.viewer:unknown", "namespace.viewer:unknown/x"}, 0, false, nil}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, s := ScopeForCentral(Claims{Roles: tc.roles}, configured)
			if len(s.Clusters) != tc.want {
				t.Fatalf("%+v", s)
			}
			if a, ok := s.Cluster("a"); ok && a.All != tc.allA {
				t.Fatal(a)
			}
			if tc.nsB != nil {
				b, ok := s.Cluster("b")
				if !ok || len(b.Namespaces) != len(tc.nsB) {
					t.Fatal(b)
				}
				for i := range tc.nsB {
					if b.Namespaces[i] != tc.nsB[i] {
						t.Fatal(b)
					}
				}
			}
		})
	}
}
