// 역할 → Scope 계산입니다. (#10)
//
// 역할은 토큰의 역할 클레임(기본 "roles")에 문자열로 실립니다. 형식:
//
//	platform.admin                          모든 클러스터 · 모든 namespace
//	cluster.viewer                          이 프로세스의 클러스터 전체
//	cluster.viewer:<clusterID>              해당 클러스터 전체 (id가 일치할 때만)
//	namespace.viewer:<ns>                   이 클러스터의 특정 namespace
//	namespace.viewer:<clusterID>/<ns>       해당 클러스터의 특정 namespace
//	dashboard.editor                        (예약) 대시보드 편집 — MVP는 조회 전용이라
//	                                        Scope에는 영향이 없고 Principal에만 남습니다
//
// 모르는 역할은 **무시**합니다. 거절하면 IdP에 다른 앱의 역할이 섞여 있을 때
// 로그인 자체가 막힙니다. 무시된 역할은 Scope를 넓히지 못하므로 안전합니다.
//
// 역할이 하나도 매칭되지 않으면 **빈 Scope**입니다 — 인증은 됐지만 볼 수 있는
// 것이 없는 상태이고, 화면 요청은 403으로 끝납니다. (401과 다릅니다)
package auth

import (
	"sort"
	"strings"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

// 역할 이름 — 이슈 #10에서 확정한 네 가지입니다.
const (
	RolePlatformAdmin   = "platform.admin"
	RoleClusterViewer   = "cluster.viewer"
	RoleNamespaceViewer = "namespace.viewer"
	RoleDashboardEditor = "dashboard.editor"
)

// Principal은 인증이 끝난 사용자입니다. 감사 로그와 편집 권한 판단에 씁니다.
type Principal struct {
	Subject  string
	Email    string
	Username string
	Roles    []string
	// CanEdit는 dashboard.editor 보유 여부입니다. MVP 화면은 조회 전용이라
	// 아직 쓰는 곳이 없지만, 편집 기능이 생길 때 이 값 하나로 판단합니다.
	CanEdit bool
}

// Name은 UI 등에 쓸 표시 이름입니다. 보안·감사 식별자는 Scope의 sub를 사용합니다.
func (p Principal) Name() string {
	switch {
	case p.Username != "":
		return p.Username
	case p.Email != "":
		return p.Email
	default:
		return p.Subject
	}
}

// ScopeFor는 역할 목록에서 이 프로세스(clusterID·clusterName)의 Scope를 계산합니다.
func ScopeFor(claims Claims, clusterID, clusterName string) (Principal, scope.Scope) {
	p := Principal{
		Subject:  claims.Subject,
		Email:    claims.Email,
		Username: claims.Username,
		Roles:    claims.Roles,
	}
	if clusterName == "" {
		clusterName = clusterID
	}

	all := false
	nsSet := map[string]bool{}

	for _, role := range claims.Roles {
		name, arg, hasArg := strings.Cut(role, ":")
		switch name {
		case RolePlatformAdmin:
			if !hasArg {
				all = true
			}
		case RoleDashboardEditor:
			if !hasArg {
				p.CanEdit = true
			}
		case RoleClusterViewer:
			// 인자가 없으면 이 클러스터, 있으면 id가 일치할 때만 적용합니다.
			if !hasArg || (arg != "" && arg == clusterID) {
				all = true
			}
		case RoleNamespaceViewer:
			if arg == "" {
				continue // namespace 없는 viewer는 아무것도 열지 않습니다.
			}
			cid, ns, hasCluster := strings.Cut(arg, "/")
			if !hasCluster {
				nsSet[cid] = true // "namespace.viewer:payments" — 이 클러스터의 ns
				continue
			}
			if cid == clusterID && ns != "" {
				nsSet[ns] = true
			}
		}
	}

	c := scope.Cluster{ID: clusterID, Name: clusterName}
	if all {
		c.All = true
	} else {
		for ns := range nsSet {
			c.Namespaces = append(c.Namespaces, ns)
		}
		sort.Strings(c.Namespaces)
	}
	// 보안 주체와 감사 식별자는 변경 가능한 표시용 클레임이 아니라 OIDC sub입니다.
	return p, scope.Scope{Subject: p.Subject, Clusters: []scope.Cluster{c}}
}
