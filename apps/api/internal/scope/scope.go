// Package scope는 **서버가 강제하는 조회 범위**입니다.
//
// 요청에 실려오는 cluster/namespace는 힌트일 뿐 신뢰하지 않습니다.
// URL을 직접 고쳐도 범위 밖 데이터가 한 줄도 나가면 안 됩니다. (README §10)
package scope

import (
	"context"
	"net/http"
	"sort"
	"strings"
)

// Scope는 한 요청에 적용되는 접근 범위입니다.
type Scope struct {
	// Subject는 감사 로그에 남길 사용자 표기입니다. 인증이 없는 정적 Scope에서는
	// 비어 있습니다. 캐시 키에는 넣지 않습니다 — 같은 Scope의 사용자끼리는
	// 응답을 공유하는 것이 singleflight의 목적입니다.
	Subject string
	// Dashboard 권한은 인식된 OIDC 역할에서만 파생합니다.
	CanEditDashboard    bool
	CanPublishDashboard bool
	// CanEditTopology는 Pod Topology의 공유 배치를 저장할 수 있는 권한입니다.
	// OIDC에서는 platform.admin에서만 파생하고, AUTH_MODE=none(개발·데모)은 허용합니다. (#28)
	CanEditTopology bool
	// CanManageWorkloads는 Deployment/Secret 조회·수정·재배포 권한입니다.
	// platform.admin에서만 파생하고 AUTH_MODE=none은 허용합니다. (ADR 0014, #32)
	CanManageWorkloads bool
	// CanExploreResources는 Resource Explorer(조회 전용) 권한입니다.
	// CanManageWorkloads와 같은 근거(정확히 platform.admin, AUTH_MODE=none)에서
	// 파생하지만 별개 값입니다 — 관리 권한을 넓히지 않고 탐색만 여는 배포를 위해서입니다. (ADR 0018)
	CanExploreResources bool
	// Clusters는 접근 가능한 클러스터입니다.
	Clusters []Cluster
}

type Cluster struct {
	ID   string
	Name string
	// Namespaces가 비어 있고 All이 true면 전체 접근입니다.
	Namespaces []string
	All        bool
}

// AllowsNamespace는 namespace 단위 허용 여부입니다.
func (c Cluster) AllowsNamespace(ns string) bool {
	if c.All {
		return true
	}
	for _, n := range c.Namespaces {
		if n == ns {
			return true
		}
	}
	return false
}

// Accessible은 이 클러스터에서 볼 수 있는 것이 하나라도 있는지입니다.
func (c Cluster) Accessible() bool { return c.All || len(c.Namespaces) > 0 }

// NamespacesJSON은 계약의 `string[] | "all"`을 만듭니다.
func (c Cluster) NamespacesJSON() any {
	if c.All {
		return "all"
	}
	out := make([]string, len(c.Namespaces))
	copy(out, c.Namespaces)
	sort.Strings(out)
	return out
}

// Cluster는 ID로 클러스터를 찾습니다.
func (s Scope) Cluster(id string) (Cluster, bool) {
	for _, c := range s.Clusters {
		if c.ID == id {
			return c, true
		}
	}
	return Cluster{}, false
}

// Resolver는 요청 → Scope 변환입니다.
//
// 지금은 설정에서 읽는 정적 구현만 있습니다. OIDC 토큰의 group을 읽거나
// SubjectAccessReview로 확인하는 구현을 나중에 여기에 끼웁니다.
// 핸들러는 Resolver 뒤를 보지 않으므로 교체해도 화면 코드는 바뀌지 않습니다.
type Resolver interface {
	Resolve(r *http.Request) (Scope, error)
}

// Static은 프로세스 전체에 같은 Scope를 적용합니다.
type Static struct{ S Scope }

func (s Static) Resolve(*http.Request) (Scope, error) { return s.S, nil }

// ParseNamespaces는 "a,b,c" 또는 "*"(전체)를 해석합니다.
func ParseNamespaces(v string) (list []string, all bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "*" || v == "all" {
		return nil, true
	}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			list = append(list, p)
		}
	}
	sort.Strings(list)
	return list, false
}

type ctxKey struct{}

// With는 미들웨어가 해석한 Scope를 컨텍스트에 싣습니다.
func With(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// From은 컨텍스트에서 Scope를 꺼냅니다. 없으면 아무것도 볼 수 없는 빈 Scope입니다.
func From(ctx context.Context) Scope {
	if s, ok := ctx.Value(ctxKey{}).(Scope); ok {
		return s
	}
	return Scope{}
}
