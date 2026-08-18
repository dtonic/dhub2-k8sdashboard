package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/topologylayout"
)

func layoutURL() string {
	return "/api/v1/clusters/" + testcluster.ClusterID + "/topology/layout"
}

func putLayout(t *testing.T, f fixture, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

// TestTopologyLayoutRequiresEditor — 편집 권한이 없으면 저장은 403입니다.
// Scope는 서버가 해석한 값만 쓰고, 요청 파라미터로는 권한이 넓어지지 않습니다.
func TestTopologyLayoutRequiresEditor(t *testing.T) {
	f := newFixture(t, func(d *httpapi.Deps) {
		d.TopologyLayout = topologylayout.New(topologylayout.Config{})
	})
	rec := putLayout(t, f, layoutURL(), `{"positions":[{"id":"a","x":1,"y":2}]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT = %d, want 403", rec.Code)
	}
}

// TestTopologyLayoutClusterScopeEnforced — Scope 밖 클러스터에는 저장할 수 없습니다.
func TestTopologyLayoutClusterScopeEnforced(t *testing.T) {
	f := newFixture(t, func(d *httpapi.Deps) {
		d.TopologyLayout = topologylayout.New(topologylayout.Config{})
		d.Resolver = scope.Static{S: scope.Scope{CanEditTopology: true, Clusters: []scope.Cluster{{
			ID: testcluster.ClusterID, Name: "Seoul Production", All: true,
		}}}}
	})
	rec := putLayout(t, f, "/api/v1/clusters/other-cluster/topology/layout", `{"positions":[]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("scope 밖 클러스터 PUT = %d, want 403", rec.Code)
	}
}

// TestTopologyLayoutPutAndSharedRead — admin이 저장한 배치는 topology 응답에 실려
// 모든 사용자에게 공유되고, 편집 권한 없는 사용자에게는 canEditLayout=false입니다.
func TestTopologyLayoutPutAndSharedRead(t *testing.T) {
	shared := topologylayout.New(topologylayout.Config{})
	admin := newFixture(t, func(d *httpapi.Deps) {
		d.TopologyLayout = shared
		d.Resolver = scope.Static{S: scope.Scope{Subject: "admin", CanEditTopology: true, Clusters: []scope.Cluster{{
			ID: testcluster.ClusterID, Name: "Seoul Production", All: true,
		}}}}
	})

	if rec := putLayout(t, admin, layoutURL(), `{"positions":[{"id":"pod-a","x":120,"y":40}]}`); rec.Code != http.StatusOK {
		t.Fatalf("admin PUT = %d %s", rec.Code, rec.Body.String())
	}
	if rec := putLayout(t, admin, layoutURL(), `{"positions":[{"id":"a","x":1e12,"y":0}]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("범위 밖 좌표 PUT = %d, want 400", rec.Code)
	}
	if rec := putLayout(t, admin, layoutURL(), `not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("잘못된 본문 PUT = %d, want 400", rec.Code)
	}

	// admin 조회 — layout과 canEditLayout=true가 함께 실립니다.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters/"+testcluster.ClusterID+"/topology?range=1h", nil)
	rec := httptest.NewRecorder()
	admin.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("topology GET = %d", rec.Code)
	}
	var adminOut contract.TopologyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &adminOut); err != nil {
		t.Fatal(err)
	}
	if !adminOut.CanEditLayout || adminOut.Layout == nil || len(adminOut.Layout.Positions) != 1 || adminOut.Layout.Positions[0].ID != "pod-a" {
		t.Fatalf("admin 응답 layout = %+v canEdit=%v", adminOut.Layout, adminOut.CanEditLayout)
	}

	// 같은 저장소를 보는 viewer — 배치는 공유되고 편집 권한만 false입니다.
	viewer := newFixture(t, func(d *httpapi.Deps) {
		d.TopologyLayout = shared
		d.Resolver = scope.Static{S: scope.Scope{Subject: "viewer", Clusters: []scope.Cluster{{
			ID: testcluster.ClusterID, Name: "Seoul Production", All: true,
		}}}}
	})
	rec = httptest.NewRecorder()
	viewer.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/clusters/"+testcluster.ClusterID+"/topology?range=1h", nil))
	var viewerOut contract.TopologyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &viewerOut); err != nil {
		t.Fatal(err)
	}
	if viewerOut.CanEditLayout {
		t.Fatal("viewer에게 canEditLayout=true가 내려갔습니다")
	}
	if viewerOut.Layout == nil || len(viewerOut.Layout.Positions) != 1 {
		t.Fatalf("viewer가 공유 배치를 받지 못했습니다: %+v", viewerOut.Layout)
	}
}
