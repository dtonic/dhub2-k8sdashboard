package httpapi_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/queryprotect"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

type rejectingResolver struct{}

func (rejectingResolver) Resolve(*http.Request) (scope.Scope, error) {
	return scope.Scope{}, errors.New("sentinel@example.com")
}

func TestQueryRefTraceCacheHitAndRejectedPaths(t *testing.T) {
	var logs bytes.Buffer
	makeFixture := func(extra func(*httpapi.Deps)) fixture {
		return newFixture(t, func(d *httpapi.Deps) {
			d.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			d.Cache = cache.NewTTL(time.Minute)
			d.PlannedQueryRefs = []string{"metrics.z", "metrics.a", "metrics.a"}
			if extra != nil {
				extra(d)
			}
		})
	}
	f := makeFixture(nil)
	f.get(t, base+"/overview?range=1h", nil)
	f.get(t, base+"/overview?range=1h", nil)
	if strings.Count(logs.String(), "queryRefs=metrics.a,metrics.z") != 2 {
		t.Fatalf("planned refs missing on miss/cache hit:\n%s", logs.String())
	}
	logs.Reset()
	f.get(t, base+"/namespaces/media?range=1h", nil)
	if strings.Contains(logs.String(), "queryRefs") {
		t.Fatalf("403 recorded refs:\n%s", logs.String())
	}
	logs.Reset()
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, base+"/overview", nil))
	if strings.Contains(logs.String(), "queryRefs") {
		t.Fatalf("405 recorded refs")
	}
	logs.Reset()
	f.get(t, "/api/v1/scope", nil)
	if strings.Contains(logs.String(), "queryRefs") {
		t.Fatalf("non-query route created trace")
	}

	logs.Reset()
	unauthorized := makeFixture(func(d *httpapi.Deps) { d.Resolver = rejectingResolver{} })
	unauthorized.get(t, base+"/overview", nil)
	if strings.Contains(logs.String(), "queryRefs") || strings.Contains(logs.String(), "sentinel@example.com") {
		t.Fatalf("401 leaked trace/error")
	}

	logs.Reset()
	limited := makeFixture(func(d *httpapi.Deps) {
		cfg := queryprotect.DefaultConfig()
		cfg.UserBurst = 1
		cfg.DashboardBurst = 1
		cfg.UserRate = .0001
		cfg.DashboardRate = .0001
		d.Guard = queryprotect.New(cfg, nil)
	})
	limited.get(t, base+"/overview", nil)
	logs.Reset()
	limited.get(t, base+"/overview", nil)
	if strings.Contains(logs.String(), "queryRefs") {
		t.Fatalf("429 recorded refs")
	}
}
