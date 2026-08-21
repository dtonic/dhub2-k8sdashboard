package auth

// Resource Explorer capability는 **정확히 platform.admin**에서만 나옵니다. (ADR 0018)

import (
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

func TestExploreCapabilityComesOnlyFromExactPlatformAdmin(t *testing.T) {
	cases := map[string]struct {
		roles []string
		want  bool
	}{
		"platform.admin":          {roles: []string{"platform.admin"}, want: true},
		"platform.admin with arg": {roles: []string{"platform.admin:prod"}, want: false},
		"cluster.viewer":          {roles: []string{"cluster.viewer"}, want: false},
		"namespace.viewer":        {roles: []string{"namespace.viewer:payments"}, want: false},
		"dashboard.editor":        {roles: []string{"dashboard.editor"}, want: false},
		"unknown role":            {roles: []string{"platform.administrator"}, want: false},
		"admin plus viewer":       {roles: []string{"cluster.viewer", "platform.admin"}, want: true},
		"no roles":                {roles: nil, want: false},
	}
	for name, tc := range cases {
		_, single := ScopeFor(Claims{Subject: "u", Roles: tc.roles}, "prod", "Prod")
		if single.CanExploreResources != tc.want {
			t.Errorf("%s: direct canExploreResources=%v want %v", name, single.CanExploreResources, tc.want)
		}
		_, central := ScopeForCentral(Claims{Subject: "u", Roles: tc.roles}, []scope.Cluster{{ID: "prod"}})
		if central.CanExploreResources != tc.want {
			t.Errorf("%s: central canExploreResources=%v want %v", name, central.CanExploreResources, tc.want)
		}
		// 기존 관리 capability 동작은 바뀌지 않아야 합니다.
		if single.CanManageWorkloads != tc.want || central.CanManageWorkloads != tc.want {
			t.Errorf("%s: canManageWorkloads 동작이 바뀌었습니다", name)
		}
	}
}

// TestExploreCapabilityTracksTheCachedTopologyFlag — 응답 캐시 키(cache.ScopeIdentity)는
// CanEditTopology를 담고 CanExploreResources는 담지 않습니다. 둘이 같은 admin 근거에서
// 나오는 한 안전하지만, 누군가 분리하면 권한이 다른 사용자끼리 /api/v1/scope 응답을
// 공유하게 됩니다. 그때 여기가 먼저 깨져야 합니다.
func TestExploreCapabilityTracksTheCachedTopologyFlag(t *testing.T) {
	for _, roles := range [][]string{
		{"platform.admin"},
		{"cluster.viewer"},
		{"namespace.viewer:payments"},
		{"dashboard.editor"},
		nil,
	} {
		_, single := ScopeFor(Claims{Subject: "u", Roles: roles}, "prod", "Prod")
		if single.CanExploreResources != single.CanEditTopology {
			t.Fatalf("%v: explore=%v topology=%v — 캐시 키가 두 권한을 구분하지 못합니다",
				roles, single.CanExploreResources, single.CanEditTopology)
		}
		_, central := ScopeForCentral(Claims{Subject: "u", Roles: roles}, []scope.Cluster{{ID: "prod"}})
		if central.CanExploreResources != central.CanEditTopology {
			t.Fatalf("%v(central): explore=%v topology=%v", roles, central.CanExploreResources, central.CanEditTopology)
		}
	}
}
