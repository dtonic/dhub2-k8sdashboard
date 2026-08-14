package httpapi_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

// capturingSource는 핸들러가 어댑터에 넘긴 Target을 기록합니다.
// 값 검증이 아니라 **Scope가 어댑터까지 전달되는지**를 봅니다.
type capturingSource struct {
	*demo.Source
	lastLogTarget datasource.Target
}

func (c *capturingSource) Search(ctx context.Context, q datasource.LogQuery) (datasource.LogPage, error) {
	c.lastLogTarget = q.Target
	return c.Source.Search(ctx, q)
}

// TestDatasourceTargetCarriesAllowedNamespaces는 namespace 제한 사용자의 요청이
// 어댑터 Target에 허용 목록으로 실리는지 확인합니다.
//
// f.Single()만 넘기던 시절에는 "여러 namespace 허용" 사용자가 어댑터에게
// 전체 허용처럼 보였습니다. 실데이터소스(Quickwit/GreptimeDB)는 이 목록을
// 질의에 강제 삽입하므로, 목록이 비면 그대로 데이터 유출이 됩니다.
func TestDatasourceTargetCarriesAllowedNamespaces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, _ := testcluster.NewStore(t, ctx)
	src := &capturingSource{Source: demo.New(store)}

	deps := httpapi.Deps{
		Store:    store,
		Metrics:  demo.New(store),
		Logs:     src,
		Alerts:   demo.New(store),
		Topology: demo.New(store),
		Resolver: scope.Static{S: scope.Scope{Clusters: []scope.Cluster{{
			ID: testcluster.ClusterID, Name: "Seoul Production",
			Namespaces: []string{"media", "payments"},
		}}}},
		Cache: cache.NewTTL(time.Nanosecond),
		Now:   func() time.Time { return testcluster.Now },
	}
	f := fixture{srv: httpapi.NewServer(deps)}

	// namespace를 지정하지 않아도 허용 목록이 Target에 실려야 합니다.
	f.get(t, base+"/logs?range=1h", nil)
	got := src.lastLogTarget
	if got.Namespace != "" {
		t.Fatalf("두 namespace가 허용된 사용자의 Namespace는 비어야 합니다: %q", got.Namespace)
	}
	if want := []string{"media", "payments"}; !reflect.DeepEqual(got.Namespaces, want) {
		t.Fatalf("허용 목록이 어댑터까지 전달되지 않았습니다: got %v want %v", got.Namespaces, want)
	}

	// 단일 namespace로 좁히면 Namespace가 채워집니다.
	f.get(t, base+"/logs?ns=payments&range=1h", nil)
	got = src.lastLogTarget
	if got.Namespace != "payments" {
		t.Fatalf("단일 namespace 요청의 Namespace: got %q want payments", got.Namespace)
	}
	if !got.AllowsNamespace("payments") || got.AllowsNamespace("media") {
		t.Fatalf("AllowsNamespace가 단일 namespace 범위를 지키지 않습니다: %+v", got)
	}
}

// TestDemoAdapterRespectsAllowedNamespaces는 데모 어댑터도 허용 목록 밖의
// Pod를 만들지 않는지 확인합니다. 데모라도 Scope 규칙은 같습니다.
func TestDemoAdapterRespectsAllowedNamespaces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, _ := testcluster.NewStore(t, ctx)
	d := demo.New(store)

	page, err := d.Search(ctx, datasource.LogQuery{
		Target: datasource.Target{
			ClusterID:  testcluster.ClusterID,
			Namespaces: []string{"payments"},
		},
		Window: datasource.Window{
			From: testcluster.Now.Add(-time.Hour),
			To:   testcluster.Now,
			Step: time.Minute,
		},
		PageSize: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) == 0 {
		t.Fatal("데모 로그가 비어 있습니다")
	}
	for _, l := range page.Lines {
		if l.Namespace != "payments" {
			t.Fatalf("허용 목록 밖 namespace의 로그가 나갔습니다: %s", l.Namespace)
		}
	}
}
