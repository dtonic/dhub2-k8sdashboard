package observability_test

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/observability"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/queryprotect"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
)

func TestMonitoringAssetsReferenceActualExpositionFamilies(t *testing.T) {
	var exposition bytes.Buffer
	platform := observability.New()
	if err := platform.WritePrometheus(&exposition); err != nil {
		t.Fatal(err)
	}
	protection := queryprotect.NewMetrics()
	for _, result := range []cache.Result{cache.ResultL1, cache.ResultL2, cache.ResultMiss, cache.ResultCoalesced, cache.ResultError, cache.ResultCanceled} {
		protection.ObserveCache(result)
	}
	protection.ObserveCacheError("redis")
	protection.ObserveCacheLoad()
	protection.Reject("user_rate", "overview")
	protection.Slow("overview")
	if err := protection.WritePrometheus(context.Background(), &exposition); err != nil {
		t.Fatal(err)
	}
	streams := stream.NewMetrics()
	if err := streams.WritePrometheus(&exposition); err != nil {
		t.Fatal(err)
	}
	familyRE := regexp.MustCompile(`(?m)^(dashboard_[a-z0-9_]+)(?:\{|\s)`)
	emitted := map[string]bool{}
	for _, match := range familyRE.FindAllStringSubmatch(exposition.String(), -1) {
		emitted[match[1]] = true
	}
	assets := ""
	for _, path := range []string{"../../../../deploy/helm/observability-dashboard/files/dashboard.json", "../../../../deploy/monitoring/alerts.yaml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		assets += string(raw)
	}
	for _, match := range regexp.MustCompile(`dashboard_[a-z0-9_]+`).FindAllString(assets, -1) {
		if !emitted[match] {
			t.Errorf("asset references non-emitted metric family %s", match)
		}
	}
}
