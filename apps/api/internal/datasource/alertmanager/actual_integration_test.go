//go:build integration

package alertmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

type actualCatalog map[string]datasource.CatalogPod

func (c actualCatalog) CatalogPods(cluster, namespace string, limit int) []datasource.CatalogPod {
	item, ok := c[cluster]
	if !ok || item.Namespace != namespace || limit != maxCatalogPods+1 {
		return nil
	}
	return []datasource.CatalogPod{item}
}

type proxyStats struct {
	Requests []struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Query  string `json:"query"`
	} `json:"requests"`
}

func actualEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Skip("Alertmanager integration fixture is not running")
	}
	return value
}

func readProxyStats(t *testing.T, path string) proxyStats {
	t.Helper()
	var result proxyStats
	for range 20 {
		body, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(body, &result) == nil {
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("proxy stats unavailable")
	return result
}

func actualSource(t *testing.T, prefix, tokenFile, caFile, serverName string, timeout time.Duration, bodyCap int64) *Source {
	return actualSourceWithClient(t, prefix, tokenFile, caFile, serverName, timeout, bodyCap,
		actualEnv(t, "ALERTMANAGER_ITEST_CLIENT_CERT_FILE"), actualEnv(t, "ALERTMANAGER_ITEST_CLIENT_KEY_FILE"))
}

func actualSourceWithClient(t *testing.T, prefix, tokenFile, caFile, serverName string, timeout time.Duration, bodyCap int64, clientCert, clientKey string) *Source {
	t.Helper()
	base := actualEnv(t, "ALERTMANAGER_ITEST_URL") + prefix
	catalog := actualCatalog{
		"cluster-a": {Namespace: "same-ns", Name: "same-pod", UID: "shared-pod-uid", WorkloadKind: "Deployment", WorkloadName: "app-a", WorkloadUID: "workload-a"},
		"cluster-b": {Namespace: "same-ns", Name: "same-pod", UID: "shared-pod-uid", WorkloadKind: "Deployment", WorkloadName: "app-b", WorkloadUID: "workload-b"},
	}
	source, err := New(Config{
		Enabled: true, BaseURL: base, PublicURL: base + "/ui", TokenFile: tokenFile, CAFile: caFile,
		ClientCertFile: clientCert, ClientKeyFile: clientKey,
		ServerName: serverName, ClusterLabel: "k8s_cluster_name", NamespaceLabel: "namespace",
		Timeout: timeout, MaxBodyBytes: bodyCap, MaxAlerts: 10, MaxConcurrent: 4,
	}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source
}

func seedActualAlertmanager(t *testing.T) {
	t.Helper()
	now := time.Now().UTC()
	alerts := make([]map[string]any, 0, 2)
	for _, cluster := range []string{"cluster-a", "cluster-b"} {
		suffix := strings.TrimPrefix(cluster, "cluster-")
		alerts = append(alerts, map[string]any{
			"labels": map[string]string{
				"alertname": "SamePodDown", "severity": "critical", "k8s_cluster_name": cluster,
				"namespace": "same-ns", "pod": "same-pod", "pod_uid": "shared-pod-uid", "workload_uid": "workload-" + suffix,
			},
			"annotations":  map[string]string{"summary": "same pod fixture"},
			"startsAt":     now.Add(-time.Minute).Format(time.RFC3339Nano),
			"endsAt":       now.Add(10 * time.Minute).Format(time.RFC3339Nano),
			"generatorURL": "https://generator.invalid/?token=must-not-leak",
		})
	}
	alerts = append(alerts, map[string]any{
		"labels": map[string]string{
			"alertname": "LivePodCrashLooping", "severity": "critical", "k8s_cluster_name": "prod-seoul",
			"namespace": "payments", "pod": "payments-api-7f-bbb", "pod_uid": "pod-payments-api-7f-bbb",
			"workload_uid": "dep-payments-api", "raw_secret_label": "raw-label-must-not-leak",
		},
		"annotations":  map[string]string{"summary": "Production adapter live firing fixture", "description": "Read-only Alertmanager drill-down"},
		"startsAt":     now.Add(-time.Minute).Format(time.RFC3339Nano),
		"endsAt":       now.Add(10 * time.Minute).Format(time.RFC3339Nano),
		"generatorURL": "https://generator.invalid/?token=must-not-leak",
	})
	body, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, actualEnv(t, "ALERTMANAGER_ITEST_ADMIN_URL")+"/api/v2/alerts", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatal(fmt.Errorf("fixture POST status %d", response.StatusCode))
	}
}

func TestActualAlertmanagerPrivateCABearerScopeAndFailures(t *testing.T) {
	tokenFile := actualEnv(t, "ALERTMANAGER_ITEST_TOKEN_FILE")
	caFile := actualEnv(t, "ALERTMANAGER_ITEST_CA_FILE")
	wrongCAFile := actualEnv(t, "ALERTMANAGER_ITEST_WRONG_CA_FILE")
	statsFile := actualEnv(t, "ALERTMANAGER_ITEST_STATS_FILE")
	seedActualAlertmanager(t)
	query := func(cluster string) datasource.AlertQuery {
		return datasource.AlertQuery{Target: datasource.Target{ClusterID: cluster, Namespace: "same-ns"}}
	}

	source := actualSource(t, "/am", tokenFile, caFile, "alertmanager.test", time.Second, 64<<10)
	before := len(readProxyStats(t, statsFile).Requests)
	a, err := source.List(context.Background(), query("cluster-a"))
	if err != nil {
		t.Fatal(err)
	}
	middle := readProxyStats(t, statsFile)
	if len(middle.Requests) != before+1 {
		t.Fatalf("cluster A requests=%d", len(middle.Requests)-before)
	}
	b, err := source.List(context.Background(), query("cluster-b"))
	if err != nil {
		t.Fatal(err)
	}
	after := readProxyStats(t, statsFile)
	if len(after.Requests) != before+2 {
		t.Fatalf("cluster B requests=%d", len(after.Requests)-len(middle.Requests))
	}
	if len(a.Firing) != 1 || len(b.Firing) != 1 || a.Firing[0].ID == b.Firing[0].ID {
		t.Fatalf("A=%+v B=%+v", a.Firing, b.Firing)
	}
	if a.Firing[0].Entity == nil || b.Firing[0].Entity == nil || a.Firing[0].Entity.PodUID != "shared-pod-uid" || b.Firing[0].Entity.PodUID != "shared-pod-uid" || a.Firing[0].Entity.ClusterID != "cluster-a" || b.Firing[0].Entity.ClusterID != "cluster-b" {
		t.Fatalf("A entity=%+v B entity=%+v", a.Firing[0].Entity, b.Firing[0].Entity)
	}
	if a.Firing[0].Entity.WorkloadUID != "workload-a" || b.Firing[0].Entity.WorkloadUID != "workload-b" || a.Firing[0].Entity.WorkloadName != "app-a" || b.Firing[0].Entity.WorkloadName != "app-b" {
		t.Fatalf("A workload=%+v B workload=%+v", a.Firing[0].Entity, b.Firing[0].Entity)
	}
	if a.HistoryErr != datasource.ErrAlertHistoryNotConfigured || b.HistoryErr != datasource.ErrAlertHistoryNotConfigured || len(a.Resolved)+len(b.Resolved) != 0 {
		t.Fatalf("history A=%v B=%v", a.HistoryErr, b.HistoryErr)
	}
	if strings.Contains(a.Firing[0].SourceURL, "generator") || strings.Contains(a.Firing[0].SourceURL, "must-not-leak") {
		t.Fatalf("unsafe source URL=%q", a.Firing[0].SourceURL)
	}
	for index, cluster := range []string{"cluster-a", "cluster-b"} {
		values, parseErr := url.ParseQuery(after.Requests[before+index].Query)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		filters := strings.Join(values["filter"], "|")
		if !strings.Contains(filters, `k8s_cluster_name="`+cluster+`"`) || !strings.Contains(filters, `namespace="same-ns"`) {
			t.Fatalf("cluster=%s filters=%q", cluster, values["filter"])
		}
	}

	t.Run("wrong bearer", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte("wrong-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := actualSource(t, "/am", path, caFile, "alertmanager.test", time.Second, 64<<10).List(context.Background(), query("cluster-a"))
		if err == nil {
			t.Fatal("wrong bearer was accepted")
		}
	})
	for _, tc := range []struct {
		name, cert, key string
	}{
		{"missing client certificate", "", ""},
		{"wrong client role", actualEnv(t, "ALERTMANAGER_ITEST_WRONG_CLIENT_CERT_FILE"), actualEnv(t, "ALERTMANAGER_ITEST_WRONG_CLIENT_KEY_FILE")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(readProxyStats(t, statsFile).Requests)
			result, err := actualSourceWithClient(t, "/am", tokenFile, caFile, "alertmanager.test", time.Second, 64<<10, tc.cert, tc.key).List(context.Background(), query("cluster-a"))
			if err == nil || len(result.Firing) != 0 {
				t.Fatalf("err=%v firing=%d", err, len(result.Firing))
			}
			if got := len(readProxyStats(t, statsFile).Requests); got != before {
				t.Fatalf("TLS-rejected request reached proxy: before=%d after=%d", before, got)
			}
		})
	}
	for _, tc := range []struct {
		name, prefix, ca, serverName string
		timeout                      time.Duration
		bodyCap                      int64
	}{
		{"wrong CA", "/am", wrongCAFile, "alertmanager.test", time.Second, 64 << 10},
		{"wrong SAN", "/am", caFile, "wrong-san.test", time.Second, 64 << 10},
		{"redirect", "/redirect", caFile, "alertmanager.test", time.Second, 64 << 10},
		{"filter ignored", "/ignore", caFile, "alertmanager.test", time.Second, 64 << 10},
		{"oversize", "/oversize", caFile, "alertmanager.test", time.Second, 64 << 10},
		{"timeout", "/timeout", caFile, "alertmanager.test", 100 * time.Millisecond, 64 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			result, err := actualSource(t, tc.prefix, tokenFile, tc.ca, tc.serverName, tc.timeout, tc.bodyCap).List(context.Background(), query("cluster-a"))
			if err == nil || len(result.Firing) != 0 {
				t.Fatalf("err=%v firing=%d", err, len(result.Firing))
			}
			if tc.name == "timeout" && time.Since(started) > 500*time.Millisecond {
				t.Fatalf("timeout took %s", time.Since(started))
			}
		})
	}

	t.Run("outage retry and circuit", func(t *testing.T) {
		outage := actualSource(t, "/outage", tokenFile, caFile, "alertmanager.test", time.Second, 64<<10)
		before := len(readProxyStats(t, statsFile).Requests)
		for attempt := 0; attempt < 3; attempt++ {
			if result, err := outage.List(context.Background(), query("cluster-a")); err == nil || len(result.Firing) != 0 {
				t.Fatalf("attempt=%d err=%v firing=%d", attempt, err, len(result.Firing))
			}
		}
		afterFailures := len(readProxyStats(t, statsFile).Requests)
		if afterFailures != before+6 {
			t.Fatalf("503 retry requests=%d", afterFailures-before)
		}
		if result, err := outage.List(context.Background(), query("cluster-a")); err == nil || len(result.Firing) != 0 {
			t.Fatalf("open circuit err=%v firing=%d", err, len(result.Firing))
		}
		if afterFastFail := len(readProxyStats(t, statsFile).Requests); afterFastFail != afterFailures {
			t.Fatalf("open circuit reached proxy: before=%d after=%d", afterFailures, afterFastFail)
		}
	})

	stats := readProxyStats(t, statsFile)
	expectedPaths := map[string]int{
		"/am/api/v2/alerts": 3, "/redirect/api/v2/alerts": 1, "/ignore/api/v2/alerts": 1,
		"/oversize/api/v2/alerts": 1, "/timeout/api/v2/alerts": 1, "/outage/api/v2/alerts": 6,
	}
	pathCounts := map[string]int{}
	for _, request := range stats.Requests {
		if request.Method != "GET" {
			t.Fatalf("production proxy method=%s", request.Method)
		}
		pathCounts[request.Path]++
	}
	if len(stats.Requests) != 13 || len(pathCounts) != len(expectedPaths) {
		t.Fatalf("request total=%d paths=%v", len(stats.Requests), pathCounts)
	}
	for path, expected := range expectedPaths {
		if pathCounts[path] != expected {
			t.Fatalf("path=%s requests=%d expected=%d", path, pathCounts[path], expected)
		}
	}
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), strings.TrimSpace(string(token))) {
		t.Fatal("bearer token leaked into proxy stats")
	}
}
