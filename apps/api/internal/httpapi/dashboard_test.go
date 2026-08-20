package httpapi_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/auth"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/dashboard"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

type dashboardStub struct {
	item      dashboard.Draft
	updateErr error
	readyErr  error
	listErr   error
	get       func(string, bool) (dashboard.Draft, error)
}

func (d *dashboardStub) Ready(context.Context) error { return d.readyErr }
func (d *dashboardStub) Create(_ context.Context, owner string, def dashboard.Definition) (dashboard.Draft, error) {
	d.item = dashboard.Draft{ID: "11111111-1111-4111-8111-111111111111", Owner: owner, Revision: 1, State: dashboard.StateDraft, SchemaVersion: 1, Definition: def}
	return d.item, nil
}
func (d *dashboardStub) List(context.Context, string, bool, string, int) (dashboard.Page, error) {
	if d.listErr != nil {
		return dashboard.Page{}, d.listErr
	}
	return dashboard.Page{Items: []dashboard.Draft{d.item}}, nil
}
func (d *dashboardStub) Get(_ context.Context, _ string, owner string, admin bool) (dashboard.Draft, error) {
	if d.get != nil {
		return d.get(owner, admin)
	}
	if d.item.ID == "" {
		return dashboard.Draft{}, dashboard.ErrNotFound
	}
	return d.item, nil
}
func (d *dashboardStub) Update(_ context.Context, _ string, _ string, _ int64, def dashboard.Definition) (dashboard.Draft, error) {
	if d.updateErr != nil {
		return dashboard.Draft{}, d.updateErr
	}
	d.item.Definition = def
	d.item.Revision++
	return d.item, nil
}
func (d *dashboardStub) Delete(context.Context, string, string, int64) error { return nil }
func (d *dashboardStub) Submit(context.Context, string, string, int64) (dashboard.Draft, error) {
	d.item.State = dashboard.StateSubmitted
	d.item.Revision++
	return d.item, nil
}
func (d *dashboardStub) Approve(context.Context, string, int64) (dashboard.Draft, error) {
	return d.item, nil
}

func TestDashboardPreconditionValidationAndExport(t *testing.T) {
	stub := &dashboardStub{}
	f := newFixture(t, func(d *httpapi.Deps) {
		d.DashboardStore = stub
		d.Resolver = scope.Static{S: scope.Scope{Subject: "editor", CanEditDashboard: true, Clusters: []scope.Cluster{{ID: "kind-local", All: true}}}}
	})
	body := `{"definition":{"schemaVersion":1,"id":"custom","title":"Custom","variables":[],"widgets":[{"id":"ready","title":"Ready","type":"Stat","binding":"nodes.ready","layout":{"x":0,"y":0,"w":3,"h":2}}]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/dashboard-drafts", strings.NewReader(body))
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type status=%d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/dashboard-drafts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"owned":true`) {
		t.Fatalf("create did not mark owned: %s", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPut, "/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing if-match=%d", rec.Code)
	}
	stub.updateErr = dashboard.ErrConflict
	req = httptest.NewRequest(http.MethodPut, "/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"revision-1"`)
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict=%d", rec.Code)
	}
	stub.item.State = dashboard.StateApproved
	req = httptest.NewRequest(http.MethodGet, "/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111/export", nil)
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("ETag") == "" || rec.Body.Bytes()[rec.Body.Len()-1] != '\n' {
		t.Fatalf("export=%d etag=%q", rec.Code, rec.Header().Get("ETag"))
	}
	etag := rec.Header().Get("ETag")
	req = httptest.NewRequest(http.MethodGet, "/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111/export", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != 304 || rec.Header().Get("ETag") != etag {
		t.Fatalf("304=%d etag=%q", rec.Code, rec.Header().Get("ETag"))
	}
	_, _ = io.Copy(io.Discard, rec.Body)
}

func TestDashboardClonePermissions(t *testing.T) {
	def := dashboard.Definition{SchemaVersion: 1, ID: "custom", Title: "Custom", Variables: []dashboard.Variable{}, Widgets: []dashboard.Widget{{ID: "ready", Title: "Ready", Type: "Stat", Binding: "nodes.ready", Layout: dashboard.Layout{X: 0, Y: 0, W: 1, H: 1}}}}
	id := "22222222-2222-4222-8222-222222222222"
	run := func(t *testing.T, sc scope.Scope, source dashboard.Draft, want int) {
		t.Helper()
		stub := &dashboardStub{item: source}
		stub.get = func(owner string, admin bool) (dashboard.Draft, error) {
			if source.Owner == owner {
				return source, nil
			}
			if admin {
				return source, nil
			}
			return dashboard.Draft{}, dashboard.ErrNotFound
		}
		f := newFixture(t, func(d *httpapi.Deps) { d.DashboardStore = stub; d.Resolver = scope.Static{S: sc} })
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/dashboard-drafts/"+id+"/clone", nil))
		if rec.Code != want {
			t.Fatalf("clone=%d want=%d body=%s", rec.Code, want, rec.Body.String())
		}
	}
	editor := scope.Scope{Subject: "editor", CanEditDashboard: true}
	run(t, editor, dashboard.Draft{ID: id, Owner: "editor", State: dashboard.StateDraft, Definition: def}, 200)
	run(t, editor, dashboard.Draft{ID: id, Owner: "other", State: dashboard.StateSubmitted, Definition: def}, 404)
	run(t, editor, dashboard.Draft{ID: id, Owner: "other", State: dashboard.StateApproved, Definition: def}, 200)
	run(t, scope.Scope{Subject: "publisher", CanPublishDashboard: true}, dashboard.Draft{ID: id, Owner: "other", State: dashboard.StateApproved, Definition: def}, 403)
}

// TestDashboardDeleteApproveImport — delete/approve/import 본문 경로 (#5).
func TestDashboardDeleteApproveImport(t *testing.T) {
	def := dashboard.Definition{SchemaVersion: 1, ID: "custom", Title: "Custom", Variables: []dashboard.Variable{}, Widgets: []dashboard.Widget{{ID: "ready", Title: "Ready", Type: "Stat", Binding: "nodes.ready", Layout: dashboard.Layout{X: 0, Y: 0, W: 3, H: 2}}}}
	id := "33333333-3333-4333-8333-333333333333"
	canonical := `{"schemaVersion":1,"id":"custom","title":"Custom","variables":[],"widgets":[{"id":"ready","title":"Ready","type":"Stat","binding":"nodes.ready","layout":{"x":0,"y":0,"w":3,"h":2}}]}`

	t.Run("delete는 If-Match를 요구하고 성공 시 204", func(t *testing.T) {
		stub := &dashboardStub{item: dashboard.Draft{ID: id, Owner: "editor", Revision: 1, State: dashboard.StateDraft, SchemaVersion: 1, Definition: def}}
		f := newFixture(t, func(d *httpapi.Deps) {
			d.DashboardStore = stub
			d.Resolver = scope.Static{S: scope.Scope{Subject: "editor", CanEditDashboard: true}}
		})
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/dashboard-drafts/"+id, nil))
		if rec.Code != http.StatusPreconditionRequired {
			t.Fatalf("delete without if-match = %d, want 428", rec.Code)
		}
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/dashboard-drafts/"+id, nil)
		req.Header.Set("If-Match", `"revision-1"`)
		rec = httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/dashboard-drafts/not-a-uuid", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("delete invalid id = %d, want 404", rec.Code)
		}
	})

	t.Run("approve는 publisher 권한과 If-Match로 200", func(t *testing.T) {
		stub := &dashboardStub{item: dashboard.Draft{ID: id, Owner: "other", Revision: 3, State: dashboard.StateSubmitted, SchemaVersion: 1, Definition: def}}
		f := newFixture(t, func(d *httpapi.Deps) {
			d.DashboardStore = stub
			d.Resolver = scope.Static{S: scope.Scope{Subject: "publisher", CanPublishDashboard: true}}
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/dashboard-drafts/"+id+"/approve", nil)
		req.Header.Set("If-Match", `"revision-3"`)
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Header().Get("ETag") == "" {
			t.Fatalf("approve = %d etag=%q %s", rec.Code, rec.Header().Get("ETag"), rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"owned":false`) {
			t.Fatalf("남의 draft 승인 응답이 owned여서는 안 됩니다: %s", rec.Body.String())
		}
	})

	t.Run("approve는 편집 권한만으로는 403", func(t *testing.T) {
		stub := &dashboardStub{item: dashboard.Draft{ID: id, Owner: "editor", Revision: 3, State: dashboard.StateSubmitted, SchemaVersion: 1, Definition: def}}
		f := newFixture(t, func(d *httpapi.Deps) {
			d.DashboardStore = stub
			d.Resolver = scope.Static{S: scope.Scope{Subject: "editor", CanEditDashboard: true}}
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/dashboard-drafts/"+id+"/approve", nil)
		req.Header.Set("If-Match", `"revision-3"`)
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("editor approve = %d, want 403", rec.Code)
		}
	})

	t.Run("import는 canonical 정의를 재검증해 draft로 만든다", func(t *testing.T) {
		stub := &dashboardStub{}
		f := newFixture(t, func(d *httpapi.Deps) {
			d.DashboardStore = stub
			d.Resolver = scope.Static{S: scope.Scope{Subject: "editor", CanEditDashboard: true}}
		})
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/dashboard-drafts/import", strings.NewReader(canonical)))
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("import without content-type = %d, want 415", rec.Code)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/dashboard-drafts/import", strings.NewReader(`{"broken`))
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("import broken json = %d, want 400", rec.Code)
		}
		req = httptest.NewRequest(http.MethodPost, "/api/v1/dashboard-drafts/import", strings.NewReader(canonical))
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated || rec.Header().Get("ETag") == "" {
			t.Fatalf("import = %d etag=%q %s", rec.Code, rec.Header().Get("ETag"), rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"owned":true`) {
			t.Fatalf("import 결과가 요청자 소유가 아닙니다: %s", rec.Body.String())
		}
	})
}

func TestDashboardMutationResponsesRemainOwned(t *testing.T) {
	stub := &dashboardStub{}
	f := newFixture(t, func(d *httpapi.Deps) {
		d.DashboardStore = stub
		d.Resolver = scope.Static{S: scope.Scope{Subject: "editor", CanEditDashboard: true}}
	})
	body := `{"definition":{"schemaVersion":1,"id":"custom","title":"Custom","variables":[],"widgets":[{"id":"ready","title":"Ready","type":"Stat","binding":"nodes.ready","layout":{"x":0,"y":0,"w":3,"h":2}}]}}`
	request := func(method, path, etag string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if method == http.MethodPost && strings.HasSuffix(path, "/submit") {
			req.Body = http.NoBody
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
		if etag != "" {
			req.Header.Set("If-Match", etag)
		}
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		return rec
	}
	created := request(http.MethodPost, "/api/v1/dashboard-drafts", "")
	updated := request(http.MethodPut, "/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111", `"revision-1"`)
	submitted := request(http.MethodPost, "/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111/submit", `"revision-2"`)
	for name, rec := range map[string]*httptest.ResponseRecorder{"create": created, "update": updated, "submit": submitted} {
		if rec.Code < 200 || rec.Code >= 300 || !strings.Contains(rec.Body.String(), `"owned":true`) {
			t.Fatalf("%s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestDashboardBoundaryErrors(t *testing.T) {
	valid := `{"definition":{"schemaVersion":1,"id":"custom","title":"Custom","variables":[],"widgets":[{"id":"ready","title":"Ready","type":"Stat","binding":"nodes.ready","layout":{"x":0,"y":0,"w":3,"h":2}}]}}`
	tests := []struct {
		name, method, path, contentType, body string
		store                                 *dashboardStub
		want                                  int
	}{
		{"invalid uuid", http.MethodGet, "/api/v1/dashboard-drafts/not-a-uuid", "", "", &dashboardStub{}, 404},
		{"invalid limit", http.MethodGet, "/api/v1/dashboard-drafts?limit=51", "", "", &dashboardStub{}, 400},
		{"invalid cursor", http.MethodGet, "/api/v1/dashboard-drafts?cursor=tampered", "", "", &dashboardStub{listErr: dashboard.ErrInvalidCursor}, 400},
		{"content type", http.MethodPost, "/api/v1/dashboard-drafts", "text/plain", valid, &dashboardStub{}, 415},
		{"trailing token", http.MethodPost, "/api/v1/dashboard-drafts", "application/json", valid + `{}`, &dashboardStub{}, 400},
		{"null description", http.MethodPost, "/api/v1/dashboard-drafts", "application/json", strings.Replace(valid, `"title":"Custom"`, `"title":"Custom","description":null`, 1), &dashboardStub{}, 400},
		{"empty description", http.MethodPost, "/api/v1/dashboard-drafts", "application/json", strings.Replace(valid, `"title":"Custom"`, `"title":"Custom","description":""`, 1), &dashboardStub{}, 400},
		{"null query refs", http.MethodPost, "/api/v1/dashboard-drafts", "application/json", strings.Replace(valid, `"type":"Stat","binding":"nodes.ready"`, `"type":"TimeSeries","binding":"trends","queryRefs":null`, 1), &dashboardStub{}, 400},
		{"null options", http.MethodPost, "/api/v1/dashboard-drafts", "application/json", strings.Replace(valid, `"type":"Stat","binding":"nodes.ready"`, `"type":"Table","binding":"unhealthy","options":null`, 1), &dashboardStub{}, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(d *httpapi.Deps) {
				d.DashboardStore = tc.store
				d.Resolver = scope.Static{S: scope.Scope{Subject: "editor", CanEditDashboard: true}}
			})
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := httptest.NewRecorder()
			f.srv.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestDashboardUnavailableIsFailClosed(t *testing.T) {
	f := newFixture(t, func(d *httpapi.Deps) {
		d.DashboardStore = nil
		d.Resolver = scope.Static{S: scope.Scope{Subject: "editor", CanEditDashboard: true}}
	})
	for _, tc := range []struct {
		path string
		want int
	}{{"/api/v1/dashboard-capabilities", 200}, {"/api/v1/dashboard-drafts", 503}, {"/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111", 503}} {
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Fatalf("%s status=%d want=%d body=%s", tc.path, rec.Code, tc.want, rec.Body.String())
		}
		if strings.HasSuffix(tc.path, "capabilities") && !strings.Contains(rec.Body.String(), `"enabled":false`) {
			t.Fatalf("capabilities did not fail closed: %s", rec.Body.String())
		}
	}
}

func TestRegisteredPathsReturnDeterministicMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	id := "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		method, path, allow string
	}{
		{http.MethodPost, "/api/v1/clusters/kind-local/overview", "GET"},
		{http.MethodGet, "/api/v1/dashboard-drafts/" + id + "/submit", "POST"},
		{http.MethodDelete, "/api/v1/dashboard-drafts/" + id + "/approve", "POST"},
		{http.MethodPatch, "/api/v1/dashboard-drafts/" + id, "GET, PUT, DELETE"},
		{http.MethodHead, "/api/v1/dashboard-drafts/" + id, "GET, PUT, DELETE"},
		{http.MethodHead, "/api/v1/dashboard-drafts", "GET, POST"},
	} {
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != tc.allow || !strings.Contains(rec.Body.String(), `"code":"method_not_allowed"`) {
			t.Fatalf("%s %s status=%d allow=%q body=%s", tc.method, tc.path, rec.Code, rec.Header().Get("Allow"), rec.Body.String())
		}
	}
}

func TestDashboardRolesOverRealOIDC(t *testing.T) {
	now := time.Now
	idp, err := auth.StartMockIDP("", "dashboard-test", now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idp.Close() })
	resolver, err := auth.NewResolver(context.Background(), auth.Config{IssuerURL: idp.Issuer, Audience: "dashboard-test", ClusterID: "kind-local", Now: now}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	def := dashboard.Definition{SchemaVersion: 1, ID: "custom", Title: "Custom", Variables: []dashboard.Variable{}, Widgets: []dashboard.Widget{{ID: "ready", Title: "Ready", Type: "Stat", Binding: "nodes.ready", Layout: dashboard.Layout{X: 0, Y: 0, W: 1, H: 1}}}}
	stub := &dashboardStub{item: dashboard.Draft{ID: "11111111-1111-4111-8111-111111111111", Owner: "editor", Revision: 1, State: dashboard.StateSubmitted, Definition: def}}
	stub.get = func(owner string, admin bool) (dashboard.Draft, error) {
		if owner == stub.item.Owner || admin {
			return stub.item, nil
		}
		return dashboard.Draft{}, dashboard.ErrNotFound
	}
	f := newFixture(t, func(d *httpapi.Deps) { d.DashboardStore = stub; d.Resolver = resolver })
	tokens := map[string]string{}
	for name, roles := range map[string][]string{"editor": {"dashboard.editor"}, "publisher": {"platform.admin"}, "viewer": {"cluster.viewer"}} {
		tokens[name], err = idp.Token(name, roles, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := func(token, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		return rec
	}
	for _, tc := range []struct {
		role, flags string
	}{{"editor", `"canEdit":true,"canPublish":false`}, {"publisher", `"canEdit":false,"canPublish":true`}, {"viewer", `"canEdit":false,"canPublish":false`}} {
		rec := request(tokens[tc.role], "/api/v1/dashboard-capabilities")
		if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(tc.flags)) {
			t.Fatalf("%s capabilities=%d %s", tc.role, rec.Code, rec.Body.String())
		}
	}
	if rec := request(tokens["viewer"], "/api/v1/dashboard-drafts"); rec.Code != 403 {
		t.Fatalf("viewer collection=%d", rec.Code)
	}
	if rec := request(tokens["editor"], "/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111"); rec.Code != 200 {
		t.Fatalf("owner item=%d", rec.Code)
	}
	stub.item.Owner = "other"
	if rec := request(tokens["editor"], "/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111"); rec.Code != 404 {
		t.Fatalf("editor IDOR=%d", rec.Code)
	}
	if rec := request(tokens["publisher"], "/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111"); rec.Code != 200 {
		t.Fatalf("publisher review=%d", rec.Code)
	}
	stub.item.Owner = "viewer"
	stub.item.State = dashboard.StateApproved
	if rec := request(tokens["viewer"], "/api/v1/dashboard-drafts/11111111-1111-4111-8111-111111111111/export"); rec.Code != 403 {
		t.Fatalf("viewer owner export=%d", rec.Code)
	}
}
