package scope_test

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

// TestParseNamespaces — "*"·빈 값은 전체, 목록은 정렬·공백 제거입니다.
func TestParseNamespaces(t *testing.T) {
	for _, tc := range []struct {
		in   string
		list []string
		all  bool
	}{
		{"*", nil, true},
		{"", nil, true},
		{"all", nil, true},
		{"payments", []string{"payments"}, false},
		{" media , payments ,, ", []string{"media", "payments"}, false},
		{"z,a", []string{"a", "z"}, false},
	} {
		list, all := scope.ParseNamespaces(tc.in)
		if all != tc.all || !reflect.DeepEqual(list, tc.list) {
			t.Fatalf("%q: got (%v,%v) want (%v,%v)", tc.in, list, all, tc.list, tc.all)
		}
	}
}

// TestClusterAccessRules — 허용 판정과 접근 가능 여부의 경계값입니다.
func TestClusterAccessRules(t *testing.T) {
	all := scope.Cluster{ID: "seoul", All: true}
	if !all.AllowsNamespace("anything") || !all.Accessible() {
		t.Fatal("전체 허용 클러스터의 판정이 틀렸습니다")
	}
	limited := scope.Cluster{ID: "seoul", Namespaces: []string{"payments"}}
	if !limited.AllowsNamespace("payments") || limited.AllowsNamespace("media") {
		t.Fatal("목록 기반 허용 판정이 틀렸습니다")
	}
	if !limited.Accessible() {
		t.Fatal("namespace가 있으면 접근 가능해야 합니다")
	}
	empty := scope.Cluster{ID: "seoul"}
	if empty.Accessible() || empty.AllowsNamespace("payments") {
		t.Fatal("빈 클러스터는 아무것도 볼 수 없어야 합니다")
	}
}

// TestNamespacesJSON — 계약의 `string[] | "all"` 표현입니다. 전체 허용은
// 문자열 "all"이고, 목록은 정렬된 사본입니다(원본 오염 없음).
func TestNamespacesJSON(t *testing.T) {
	if v := (scope.Cluster{All: true}).NamespacesJSON(); v != "all" {
		t.Fatalf(`전체 허용은 "all"이어야 합니다: %v`, v)
	}
	c := scope.Cluster{Namespaces: []string{"z", "a"}}
	v, ok := c.NamespacesJSON().([]string)
	if !ok || !reflect.DeepEqual(v, []string{"a", "z"}) {
		t.Fatalf("정렬된 목록이어야 합니다: %v", v)
	}
	if c.Namespaces[0] != "z" {
		t.Fatal("원본이 정렬로 오염되었습니다")
	}
	if empty, ok := (scope.Cluster{}).NamespacesJSON().([]string); !ok || empty == nil || len(empty) != 0 {
		t.Fatalf("빈 제한 범위는 JSON 배열이어야 합니다: %#v", empty)
	}
}

// TestScopeClusterLookupAndContext — 클러스터 조회, 컨텍스트 왕복,
// 없는 경우의 빈 Scope를 확인합니다.
func TestScopeClusterLookupAndContext(t *testing.T) {
	s := scope.Scope{Clusters: []scope.Cluster{{ID: "seoul"}, {ID: "tokyo"}}}
	if c, ok := s.Cluster("tokyo"); !ok || c.ID != "tokyo" {
		t.Fatal("클러스터 조회 실패")
	}
	if _, ok := s.Cluster("osaka"); ok {
		t.Fatal("없는 클러스터가 조회되었습니다")
	}

	ctx := scope.With(context.Background(), s)
	if got := scope.From(ctx); len(got.Clusters) != 2 {
		t.Fatal("컨텍스트 왕복이 깨졌습니다")
	}
	// Scope가 없는 컨텍스트는 **빈 Scope**입니다 — 아무것도 볼 수 없는 것이 기본입니다.
	if got := scope.From(context.Background()); len(got.Clusters) != 0 {
		t.Fatal("빈 컨텍스트는 빈 Scope여야 합니다")
	}
}

// TestStaticResolver — 정적 해석기는 요청과 무관하게 같은 Scope를 돌려줍니다.
func TestStaticResolver(t *testing.T) {
	s := scope.Scope{Clusters: []scope.Cluster{{ID: "seoul", All: true}}}
	got, err := scope.Static{S: s}.Resolve(httptest.NewRequest("GET", "/", nil))
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("정적 Scope가 그대로 나와야 합니다: %v %v", got, err)
	}
}
