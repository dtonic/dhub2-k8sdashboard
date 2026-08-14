package datasource_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

// TestTargetAllowsNamespace — 어댑터가 Scope 강제에 쓰는 판정의 전체 경우입니다.
func TestTargetAllowsNamespace(t *testing.T) {
	for name, tc := range map[string]struct {
		target datasource.Target
		ns     string
		want   bool
	}{
		"단일 ns 일치":     {datasource.Target{Namespace: "payments"}, "payments", true},
		"단일 ns 불일치":    {datasource.Target{Namespace: "payments"}, "media", false},
		"허용 목록 안":      {datasource.Target{Namespaces: []string{"a", "b"}}, "b", true},
		"허용 목록 밖":      {datasource.Target{Namespaces: []string{"a", "b"}}, "c", false},
		"제한 없음(전체 허용)": {datasource.Target{}, "anything", true},
		// 단일 ns가 있으면 목록보다 우선합니다 — 더 좁은 쪽이 이깁니다.
		"단일 ns가 목록보다 우선": {datasource.Target{Namespace: "a", Namespaces: []string{"a", "b"}}, "b", false},
	} {
		if got := tc.target.AllowsNamespace(tc.ns); got != tc.want {
			t.Fatalf("%s: got %v want %v", name, got, tc.want)
		}
	}
}

// TestUnavailableFailsEveryInterface — 설정되지 않은 데이터소스는 nil 검사
// 대신 항상 실패하는 구현입니다. 여덟 메서드 전부 오류여야 하고, 핸들러가
// 이를 섹션 degraded로 바꿉니다.
func TestUnavailableFailsEveryInterface(t *testing.T) {
	ctx := context.Background()
	u := datasource.Unavailable{}

	if _, err := u.Trends(ctx, datasource.Target{}, datasource.Window{}, nil); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatal("Trends가 표준 오류가 아닙니다")
	}
	if _, err := u.Usage(ctx, "c"); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatal("Usage가 표준 오류가 아닙니다")
	}
	if _, err := u.Search(ctx, datasource.LogQuery{}); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatal("Search가 표준 오류가 아닙니다")
	}
	if _, err := u.Histogram(ctx, datasource.LogQuery{}); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatal("Histogram이 표준 오류가 아닙니다")
	}
	if _, err := u.Facets(ctx, datasource.LogQuery{}); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatal("Facets가 표준 오류가 아닙니다")
	}
	if _, err := u.List(ctx, datasource.AlertQuery{}); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatal("List가 표준 오류가 아닙니다")
	}
	if _, err := u.Graph(ctx, datasource.Target{}, datasource.Window{}); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatal("Graph가 표준 오류가 아닙니다")
	}
	if _, err := u.EdgeSeries(ctx, "c", "e", datasource.Window{}); !errors.Is(err, datasource.ErrUnavailable) {
		t.Fatal("EdgeSeries가 표준 오류가 아닙니다")
	}

	// 사유가 있으면 그 사유가 오류 문자열이 됩니다 — degraded 사유로 보입니다.
	custom := datasource.Unavailable{Reason: "점검 중입니다"}
	if _, err := custom.Search(ctx, datasource.LogQuery{}); err == nil || err.Error() != "점검 중입니다" {
		t.Fatalf("사유가 전달되지 않았습니다: %v", err)
	}
}
