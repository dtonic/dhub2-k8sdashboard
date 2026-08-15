package alertmanager

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/upstream"
)

type observerStub struct {
	circuits int
}

func (*observerStub) ObserveUpstream(context.Context, upstream.Upstream, upstream.Outcome, time.Duration) {
}
func (o *observerStub) SetCircuit(upstream.Upstream, upstream.CircuitState, uint64) {
	o.circuits++
}
func (*observerStub) ObserveAlertSeverityFallback() {}

func encodedAlert(t *testing.T, fp, name, namespace, podUID, workloadUID string, offset time.Duration) json.RawMessage {
	t.Helper()
	start := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC).Add(offset)
	labels := map[string]string{"alertname": name, "k8s_cluster_name": "a"}
	if namespace != "" {
		labels["namespace"] = namespace
	}
	if podUID != "" {
		labels["pod_uid"], labels["pod"] = podUID, "same"
	}
	if workloadUID != "" {
		labels["workload_uid"] = workloadUID
	}
	b, err := json.Marshal(map[string]any{"annotations": map[string]string{}, "endsAt": "2026-08-15T01:00:00Z", "fingerprint": fp, "startsAt": start.Format(time.RFC3339Nano), "updatedAt": "2026-08-15T00:20:00Z", "status": map[string]any{"state": "active", "inhibitedBy": []string{}, "mutedBy": []string{}, "silencedBy": []string{}}, "labels": labels, "receivers": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requiredStatus(state string) *apiStatus {
	return &apiStatus{State: state, InhibitedBy: []string{}, MutedBy: []string{}, SilencedBy: []string{}}
}

type catalog struct {
	items []datasource.CatalogPod
	calls atomic.Int32
}

func (c *catalog) CatalogPods(cluster, namespace string, limit int) []datasource.CatalogPod {
	c.calls.Add(1)
	if cluster != "" && limit == maxCatalogPods+1 {
		return c.items
	}
	return nil
}

func fixture(t *testing.T, handler http.Handler, c datasource.PodCatalog) (*Source, *httptest.Server) {
	t.Helper()
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	cert := ts.Certificate()
	if cert == nil {
		t.Fatal("missing server certificate")
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	caFile := filepath.Join(dir, "ca.pem")
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(caFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("test-bearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Enabled: true, BaseURL: ts.URL + "/prom", PublicURL: ts.URL + "/ui", TokenFile: tokenFile, CAFile: caFile, ServerName: "127.0.0.1", ClusterLabel: "k8s_cluster_name", NamespaceLabel: "namespace", Timeout: time.Second, MaxBodyBytes: 64 << 10, MaxAlerts: 10, MaxConcurrent: 4, Now: func() time.Time { return time.Date(2026, 8, 15, 0, 30, 0, 0, time.UTC) }}
	src, err := New(cfg, c)
	if err != nil {
		t.Fatal(err)
	}
	return src, ts
}

func expiredIntermediateClientFiles(t *testing.T) (string, string) {
	t.Helper()
	now := time.Now()
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rootTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTpl, rootTpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(rootDER)
	intermediateKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	intermediateTpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "expired-intermediate"}, NotBefore: now.Add(-2 * time.Hour), NotAfter: now.Add(-time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	intermediateDER, err := x509.CreateCertificate(rand.Reader, intermediateTpl, root, &intermediateKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	intermediate, _ := x509.ParseCertificate(intermediateDER)
	leafKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	leafTpl := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "client"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, intermediate, &leafKey.PublicKey, intermediateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "client.pem"), filepath.Join(dir, "client-key.pem")
	certPEM := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: intermediateDER})...)
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestListInjectsScopeAndLinksCatalogOnce(t *testing.T) {
	var requests atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/prom/api/v2/alerts" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-bearer" {
			t.Error("missing bearer")
		}
		filters := r.URL.Query()["filter"]
		joined := strings.Join(filters, "|")
		if !strings.Contains(joined, `k8s_cluster_name="a"`) || !strings.Contains(joined, `namespace="ns"`) {
			t.Errorf("filters=%q", filters)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
{"annotations":{"summary":"pod A"},"endsAt":"2026-08-15T01:00:00Z","fingerprint":"fp-a","receivers":[{"name":"pager"}],"startsAt":"2026-08-15T00:00:00Z","updatedAt":"2026-08-15T00:01:00Z","status":{"inhibitedBy":[],"mutedBy":[],"silencedBy":[],"state":"active"},"labels":{"alertname":"PodDown","severity":"critical","k8s_cluster_name":"a","namespace":"ns","pod":"same","pod_uid":"uid-a","workload_uid":"work-a"}}
]`))
	})
	c := &catalog{items: []datasource.CatalogPod{{Namespace: "ns", Name: "same", UID: "uid-a", WorkloadKind: "Deployment", WorkloadName: "app-a", WorkloadUID: "work-a"}, {Namespace: "other", Name: "same", UID: "uid-b", WorkloadKind: "Deployment", WorkloadName: "app-b", WorkloadUID: "work-b"}}}
	src, _ := fixture(t, h, c)
	res, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a", Namespace: "ns"}})
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || c.calls.Load() != 1 {
		t.Fatalf("requests=%d catalog=%d", requests.Load(), c.calls.Load())
	}
	if len(res.Firing) != 1 || res.Firing[0].Entity == nil || res.Firing[0].Entity.PodUID != "uid-a" || res.Firing[0].Entity.WorkloadUID != "work-a" {
		t.Fatalf("result=%+v", res.Firing)
	}
	if res.Firing[0].SourceURL == "" || strings.Contains(res.Firing[0].SourceURL, "generator") {
		t.Fatalf("source=%q", res.Firing[0].SourceURL)
	}
	if res.HistoryErr != datasource.ErrAlertHistoryNotConfigured || len(res.Resolved) != 0 {
		t.Fatalf("history=%v resolved=%v", res.HistoryErr, res.Resolved)
	}
}

func TestClusterIdentitySeparatesSameNamespaceAndPodUID(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 30, 0, 0, time.UTC)
	s := &Source{cfg: Config{ClusterLabel: "k8s_cluster_name", NamespaceLabel: "namespace"}, catalog: &catalog{items: []datasource.CatalogPod{{Namespace: "ns", Name: "same", UID: "shared-uid"}}}, public: &url.URL{Scheme: "https", Host: "alerts.example"}, now: func() time.Time { return now }}
	makeAlert := func(cluster, fingerprint string) apiAlert {
		return apiAlert{Fingerprint: fingerprint, StartsAt: "2026-08-15T00:00:00Z", EndsAt: "2026-08-15T01:00:00Z", UpdatedAt: "2026-08-15T00:20:00Z", Status: requiredStatus("active"), Labels: map[string]string{"alertname": "SamePod", "k8s_cluster_name": cluster, "namespace": "ns", "pod": "same", "pod_uid": "shared-uid"}, Annotations: map[string]string{}, Receivers: []apiReceiver{}}
	}
	a, err := s.normalize(datasource.Target{ClusterID: "a", Namespace: "ns"}, []apiAlert{makeAlert("a", "same-fingerprint")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.normalize(datasource.Target{ClusterID: "b", Namespace: "ns"}, []apiAlert{makeAlert("b", "same-fingerprint")})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Firing) != 1 || len(b.Firing) != 1 || a.Firing[0].Entity == nil || b.Firing[0].Entity == nil || a.Firing[0].ID == b.Firing[0].ID || a.Firing[0].Entity.ClusterID != "a" || b.Firing[0].Entity.ClusterID != "b" {
		t.Fatalf("cluster A=%+v cluster B=%+v", a.Firing, b.Firing)
	}
}

func TestListFilterIgnoringUpstreamFailsClusterAndDropsNamespace(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong cluster", func(v map[string]any) { v["labels"].(map[string]any)["k8s_cluster_name"] = "b" }},
		{"wrong namespace", func(v map[string]any) { v["labels"].(map[string]any)["namespace"] = "secret" }},
		{"namespace-less scoped", func(v map[string]any) { delete(v["labels"].(map[string]any), "namespace") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(encodedAlert(t, "x", "Leak", "ns", "", "", 0), &value); err != nil {
				t.Fatal(err)
			}
			tc.mutate(value)
			src, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]any{value})
			}), &catalog{})
			res, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a", Namespace: "ns"}})
			if err == nil || len(res.Firing) != 0 {
				t.Fatalf("err=%v firing=%d", err, len(res.Firing))
			}
		})
	}
}

func TestListRejectsConflictingDuplicateAndUnsafeTransport(t *testing.T) {
	var first, second map[string]any
	if err := json.Unmarshal(encodedAlert(t, "same", "A", "", "", "", 0), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encodedAlert(t, "same", "A", "", "", "", 0), &second); err != nil {
		t.Fatal(err)
	}
	second["labels"].(map[string]any)["alertname"] = "B"
	src, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]any{first, second})
	}), &catalog{})
	if _, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}}); err == nil {
		t.Fatal("conflicting duplicate accepted")
	}

	base := Config{Enabled: true, PublicURL: "https://public.example", BaseURL: "https://am.example", TokenFile: "x", CAFile: "y", ServerName: "am.example", ClusterLabel: "k8s_cluster_name", NamespaceLabel: "namespace", Timeout: defaultTimeout, MaxBodyBytes: defaultMaxBody, MaxAlerts: defaultMaxAlerts, MaxConcurrent: defaultMaxConcurrent}
	mutations := []func(*Config){
		func(c *Config) { c.BaseURL = "http://am.example" }, func(c *Config) { c.BaseURL = "https://u:p@am.example" }, func(c *Config) { c.BaseURL = "https://bad_host" }, func(c *Config) { c.BaseURL = "https://am.example:0" }, func(c *Config) { c.BaseURL = "https://am.example:65536" }, func(c *Config) { c.BaseURL = "https://am.example/a/../b" }, func(c *Config) { c.PublicURL += "?token=x" }, func(c *Config) { c.NamespaceLabel = c.ClusterLabel }, func(c *Config) { c.ClientCertFile = "cert" }, func(c *Config) { c.Timeout = 31 * time.Second }, func(c *Config) { c.MaxAlerts = 10001 }, func(c *Config) { c.MaxConcurrent = 33 },
	}
	for i, mutate := range mutations {
		cfg := base
		mutate(&cfg)
		if Validate(cfg) == nil {
			t.Errorf("mutation %d accepted", i)
		}
	}
}

func TestValidateAcceptsPrivateCA(t *testing.T) {
	src, ts := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}), &catalog{})
	if _, err := x509.ParseCertificate(ts.Certificate().Raw); err != nil || src == nil {
		t.Fatal("fixture CA invalid")
	}
	cfg := src.cfg
	cfg.ClusterLabel, cfg.NamespaceLabel = "scope", "scope_"
	if err := Validate(cfg); err != nil {
		t.Fatalf("distinct near-name labels rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"minimums": func(c *Config) {
			c.Timeout, c.MaxBodyBytes, c.MaxAlerts, c.MaxConcurrent = 100*time.Millisecond, 64<<10, 1, 1
		},
		"maximums": func(c *Config) {
			c.Timeout, c.MaxBodyBytes, c.MaxAlerts, c.MaxConcurrent = 30*time.Second, 16<<20, 10000, 32
		},
	} {
		t.Run(name, func(t *testing.T) {
			bounded := cfg
			mutate(&bounded)
			if err := Validate(bounded); err != nil {
				t.Fatalf("valid bounds rejected: %v", err)
			}
		})
	}
}

func TestSharedConcurrencyBound(t *testing.T) {
	var calls, inFlight, peak atomic.Int32
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	src, _ := fixture(t, h, &catalog{})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}})
		}()
	}
	deadline := time.Now().Add(time.Second)
	for peak.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.List(ctx, datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("semaphore waiter cancellation=%v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("canceled waiter reached upstream: calls=%d", calls.Load())
	}
	close(release)
	wg.Wait()
	if peak.Load() != 4 {
		t.Fatalf("peak=%d, want 4", peak.Load())
	}
}

func TestSemaphoreQueueUsesTotalDeadlineWithoutBreakerFailure(t *testing.T) {
	var calls atomic.Int32
	src, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}), &catalog{})
	src.cfg.Timeout = 100 * time.Millisecond
	for i := 0; i < cap(src.sem); i++ {
		src.sem <- struct{}{}
	}
	started := time.Now()
	_, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}})
	elapsed := time.Since(started)
	for i := 0; i < cap(src.sem); i++ {
		<-src.sem
	}
	if !errors.Is(err, context.DeadlineExceeded) || elapsed < 90*time.Millisecond || elapsed > 300*time.Millisecond || calls.Load() != 0 {
		t.Fatalf("queue deadline elapsed=%s calls=%d err=%v", elapsed, calls.Load(), err)
	}
	src.mu.Lock()
	fails, open := src.fails, !src.openTil.IsZero()
	src.mu.Unlock()
	if fails != 0 || open {
		t.Fatalf("queue timeout changed breaker: fails=%d open=%v", fails, open)
	}
}

func TestTransportFailurePolicies(t *testing.T) {
	tests := []struct {
		name         string
		handler      http.Handler
		wantRequests int32
	}{
		{"unauthorized", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }), 1},
		{"retry 503 once", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }), 2},
		{"redirect denied", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.invalid", http.StatusFound)
		}), 1},
		{"wrong mime", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = w.Write([]byte(`[]`))
		}), 1},
		{"oversize", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[` + strings.Repeat(" ", 70<<10) + `]`))
		}), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			src, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); tc.handler.ServeHTTP(w, r) }), &catalog{})
			if _, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}}); err == nil {
				t.Fatal("failure accepted")
			}
			if calls.Load() != tc.wantRequests {
				t.Fatalf("requests=%d want=%d", calls.Load(), tc.wantRequests)
			}
		})
	}
}

func TestRequiredOpenAPIFieldsRejectMissingOrNull(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(map[string]any)
		ok     bool
	}{
		{"positive empty required collections", func(map[string]any) {}, true},
		{"annotations missing", func(v map[string]any) { delete(v, "annotations") }, false},
		{"annotations null", func(v map[string]any) { v["annotations"] = nil }, false},
		{"receivers missing", func(v map[string]any) { delete(v, "receivers") }, false},
		{"receivers null", func(v map[string]any) { v["receivers"] = nil }, false},
		{"status missing", func(v map[string]any) { delete(v, "status") }, false},
		{"status null", func(v map[string]any) { v["status"] = nil }, false},
		{"labels missing", func(v map[string]any) { delete(v, "labels") }, false},
		{"labels null", func(v map[string]any) { v["labels"] = nil }, false},
	}
	for _, field := range []string{"silencedBy", "inhibitedBy", "mutedBy"} {
		field := field
		mutations = append(mutations,
			struct {
				name   string
				mutate func(map[string]any)
				ok     bool
			}{field + " missing", func(v map[string]any) { delete(v["status"].(map[string]any), field) }, false},
			struct {
				name   string
				mutate func(map[string]any)
				ok     bool
			}{field + " null", func(v map[string]any) { v["status"].(map[string]any)[field] = nil }, false},
		)
	}
	mutations = append(mutations,
		struct {
			name   string
			mutate func(map[string]any)
			ok     bool
		}{"receiver null item", func(v map[string]any) { v["receivers"] = []any{nil} }, false},
		struct {
			name   string
			mutate func(map[string]any)
			ok     bool
		}{"receiver empty object", func(v map[string]any) { v["receivers"] = []any{map[string]any{}} }, false},
		struct {
			name   string
			mutate func(map[string]any)
			ok     bool
		}{"receiver name null", func(v map[string]any) { v["receivers"] = []any{map[string]any{"name": nil}} }, false},
		struct {
			name   string
			mutate func(map[string]any)
			ok     bool
		}{"receiver malformed name", func(v map[string]any) { v["receivers"] = []any{map[string]any{"name": 1}} }, false},
		struct {
			name   string
			mutate func(map[string]any)
			ok     bool
		}{"labels null value", func(v map[string]any) { v["labels"].(map[string]any)["extra"] = nil }, false},
		struct {
			name   string
			mutate func(map[string]any)
			ok     bool
		}{"labels malformed value", func(v map[string]any) { v["labels"].(map[string]any)["extra"] = 1 }, false},
		struct {
			name   string
			mutate func(map[string]any)
			ok     bool
		}{"annotations null value", func(v map[string]any) { v["annotations"].(map[string]any)["summary"] = nil }, false},
		struct {
			name   string
			mutate func(map[string]any)
			ok     bool
		}{"annotations malformed value", func(v map[string]any) { v["annotations"].(map[string]any)["summary"] = true }, false},
	)
	for _, field := range []string{"silencedBy", "inhibitedBy", "mutedBy"} {
		field := field
		mutations = append(mutations,
			struct {
				name   string
				mutate func(map[string]any)
				ok     bool
			}{field + " null item", func(v map[string]any) { v["status"].(map[string]any)[field] = []any{nil} }, false},
			struct {
				name   string
				mutate func(map[string]any)
				ok     bool
			}{field + " malformed item", func(v map[string]any) { v["status"].(map[string]any)[field] = []any{1} }, false},
		)
	}
	for _, field := range []string{"fingerprint", "startsAt", "endsAt", "updatedAt"} {
		field := field
		mutations = append(mutations, struct {
			name   string
			mutate func(map[string]any)
			ok     bool
		}{field + " null", func(v map[string]any) { v[field] = nil }, false})
	}
	mutations = append(mutations, struct {
		name   string
		mutate func(map[string]any)
		ok     bool
	}{"status state null", func(v map[string]any) { v["status"].(map[string]any)["state"] = nil }, false})
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(encodedAlert(t, "required", "A", "", "", "", 0), &value); err != nil {
				t.Fatal(err)
			}
			tc.mutate(value)
			src, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]any{value})
			}), &catalog{})
			res, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}})
			if tc.ok && (err != nil || len(res.Firing) != 1) {
				t.Fatalf("valid empty required fields rejected: alerts=%d err=%v", len(res.Firing), err)
			}
			if !tc.ok && err == nil {
				t.Fatal("missing/null required field accepted")
			}
		})
	}
}

func TestCredentialFilesAndClosePolicy(t *testing.T) {
	src, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}), &catalog{})
	base := src.cfg
	if _, err := New(base, nil); err == nil {
		t.Fatal("nil catalog accepted")
	}
	mutations := []struct {
		name   string
		change func(*Config)
	}{
		{"relative", func(c *Config) { c.TokenFile = "token" }},
		{"directory", func(c *Config) { c.TokenFile = filepath.Dir(c.TokenFile) }},
		{"oversize token", func(c *Config) {
			p := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(p, []byte(strings.Repeat("x", maxTokenFileBytes+1)), 0o600); err != nil {
				t.Fatal(err)
			}
			c.TokenFile = p
		}},
	}
	for name, value := range map[string]string{"inner whitespace": "abc def", "bearer prefix": "Bearer abc", "non-ascii": "토큰"} {
		name, value := name, value
		mutations = append(mutations, struct {
			name   string
			change func(*Config)
		}{name, func(c *Config) {
			p := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			c.TokenFile = p
		}})
	}
	certFile, keyFile := expiredIntermediateClientFiles(t)
	mutations = append(mutations, struct {
		name   string
		change func(*Config)
	}{"expired intermediate", func(c *Config) { c.ClientCertFile, c.ClientKeyFile = certFile, keyFile }})
	fifo := filepath.Join(t.TempDir(), "fifo")
	if syscall.Mkfifo(fifo, 0o600) == nil {
		mutations = append(mutations, struct {
			name   string
			change func(*Config)
		}{"fifo", func(c *Config) { c.TokenFile = fifo }})
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.change(&cfg)
			if Validate(cfg) == nil {
				t.Fatal("unsafe credential accepted")
			}
		})
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}}); err == nil {
		t.Fatal("closed source accepted request")
	}
}

func TestCircuitCountsMalformedButNotCancellationAndRecovers(t *testing.T) {
	var calls atomic.Int32
	var malformed atomic.Bool
	malformed.Store(true)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		cluster := "a"
		if malformed.Load() {
			cluster = "b"
		}
		a := encodedAlert(t, "fp", "A", "", "", "", 0)
		var value map[string]any
		_ = json.Unmarshal(a, &value)
		value["labels"].(map[string]any)["k8s_cluster_name"] = cluster
		_ = json.NewEncoder(w).Encode([]any{value})
	})
	src, _ := fixture(t, h, &catalog{})
	now := time.Date(2026, 8, 15, 0, 30, 0, 0, time.UTC)
	src.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}}); err == nil {
			t.Fatal("malformed accepted")
		}
	}
	if _, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}}); err == nil || calls.Load() != 3 {
		t.Fatalf("open circuit calls=%d err=%v", calls.Load(), err)
	}
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = src.List(ctx, datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}})
	}
	if calls.Load() != 3 {
		t.Fatalf("canceled calls reached upstream: %d", calls.Load())
	}
	now = now.Add(6 * time.Second)
	malformed.Store(false)
	res, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}})
	if err != nil || len(res.Firing) != 1 || calls.Load() != 4 {
		t.Fatalf("recovery calls=%d alerts=%d err=%v", calls.Load(), len(res.Firing), err)
	}
}

func TestInternalDeadlineCountsTowardCircuit(t *testing.T) {
	var calls atomic.Int32
	src, _ := fixture(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}), &catalog{})
	src.cfg.Timeout = 100 * time.Millisecond
	for i := 0; i < 3; i++ {
		if _, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}}); err == nil {
			t.Fatal("internal deadline accepted")
		}
	}
	if _, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}}); err == nil || calls.Load() != 3 {
		t.Fatalf("circuit did not open after internal deadlines: calls=%d err=%v", calls.Load(), err)
	}
}

func TestHalfOpenAllowsExactlyOneConcurrentProbe(t *testing.T) {
	var calls atomic.Int32
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	src, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n <= 3 {
			_, _ = w.Write([]byte(`{"invalid":"shape"}`))
			return
		}
		if n == 4 {
			close(probeStarted)
			<-releaseProbe
		}
		_, _ = w.Write([]byte(`[]`))
	}), &catalog{})
	now := time.Date(2026, 8, 15, 0, 30, 0, 0, time.UTC)
	src.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		_, _ = src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}})
	}
	now = now.Add(6 * time.Second)
	results := make(chan error, 20)
	start := make(chan struct{})
	for i := 0; i < cap(results); i++ {
		go func() {
			<-start
			_, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}})
			results <- err
		}()
	}
	close(start)
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("half-open probe did not start")
	}
	for i := 0; i < cap(results)-1; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, errCircuitOpen) {
				t.Fatalf("concurrent half-open result=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent half-open caller waited")
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("half-open upstream calls=%d", calls.Load())
	}
	close(releaseProbe)
	if err := <-results; err != nil {
		t.Fatalf("probe result=%v", err)
	}
}

func TestStaleSuccessCannotCloseNewCircuitGeneration(t *testing.T) {
	var calls atomic.Int32
	delayedStarted, releaseDelayed := make(chan struct{}), make(chan struct{})
	src, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case n == 1:
			close(delayedStarted)
			<-releaseDelayed
			_, _ = w.Write([]byte(`[]`))
		case n <= 4:
			_, _ = w.Write([]byte(`{"invalid":"shape"}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}), &catalog{})
	now := time.Date(2026, 8, 15, 0, 30, 0, 0, time.UTC)
	src.now = func() time.Time { return now }
	delayedResult := make(chan error, 1)
	go func() {
		_, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}})
		delayedResult <- err
	}()
	<-delayedStarted
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}})
		}()
	}
	wg.Wait()
	close(releaseDelayed)
	if err := <-delayedResult; err != nil {
		t.Fatalf("delayed success=%v", err)
	}
	if _, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}}); !errors.Is(err, errCircuitOpen) || calls.Load() != 4 {
		t.Fatalf("stale success closed circuit: calls=%d err=%v", calls.Load(), err)
	}
	now = now.Add(6 * time.Second)
	if _, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a"}}); err != nil || calls.Load() != 5 {
		t.Fatalf("half-open recovery calls=%d err=%v", calls.Load(), err)
	}
}

func TestGroupingIsDeterministicAndCollapsesRepresentatives(t *testing.T) {
	alerts := []json.RawMessage{encodedAlert(t, "one", "Down", "ns", "uid-a", "work-a", 0), encodedAlert(t, "two", "Down", "ns", "uid-a", "work-a", time.Second), encodedAlert(t, "three", "Down", "ns", "uid-a", "work-a", 2*time.Second)}
	var reverse atomic.Bool
	var single atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		values := append([]json.RawMessage(nil), alerts...)
		if single.Load() {
			values = values[:1]
		}
		if reverse.Load() {
			values[0], values[2] = values[2], values[0]
		}
		_ = json.NewEncoder(w).Encode(values)
	})
	src, _ := fixture(t, h, &catalog{items: []datasource.CatalogPod{{Namespace: "ns", Name: "same", UID: "uid-a", WorkloadName: "app", WorkloadKind: "Deployment", WorkloadUID: "work-a"}}})
	first, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a", Namespace: "ns"}})
	if err != nil {
		t.Fatal(err)
	}
	reverse.Store(true)
	second, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a", Namespace: "ns"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Firing) != 1 || first.Firing[0].GroupSize != 3 || first.Firing[0].ID != second.Firing[0].ID || strings.ContainsRune(first.Firing[0].GroupKey, '\x00') || len(first.Firing[0].GroupKey) > maxKeyBytes {
		t.Fatalf("first=%+v second=%+v", first.Firing, second.Firing)
	}
	single.Store(true)
	reverse.Store(false)
	third, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a", Namespace: "ns"}})
	if err != nil || len(third.Firing) != 1 || third.Firing[0].GroupSize != 1 || third.Firing[0].ID != first.Firing[0].ID {
		t.Fatalf("membership churn changed group identity: first=%+v third=%+v err=%v", first.Firing, third.Firing, err)
	}
}

func TestSourceLinksAndAnnotationsNeverLeakUpstreamValues(t *testing.T) {
	var serverURL string
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		a := encodedAlert(t, "secret-fingerprint", "PodDown", "ns", "uid-a", "work-a", 0)
		var value map[string]any
		_ = json.Unmarshal(a, &value)
		value["generatorURL"] = "https://evil.example/token-in-generator"
		value["annotations"] = map[string]string{"runbook_url": serverURL + "/ui/runbook?token=secret#fragment", "dashboard_url": "https://evil.example/leak", "summary": "password: supersecretvalue"}
		_ = json.NewEncoder(w).Encode([]any{value})
	})
	src, ts := fixture(t, h, &catalog{items: []datasource.CatalogPod{{Namespace: "ns", Name: "same", UID: "uid-a", WorkloadUID: "work-a", WorkloadName: "app", WorkloadKind: "Deployment"}}})
	serverURL = ts.URL
	res, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a", Namespace: "ns"}})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Firing[0]
	if strings.Contains(a.SourceURL, "secret-fingerprint") || !strings.Contains(a.SourceURL, "PodDown") || strings.Contains(a.SourceURL, "token-in-generator") {
		t.Fatalf("sourceURL=%q", a.SourceURL)
	}
	if got := a.Annotations["runbook_url"]; got != serverURL+"/ui/runbook" {
		t.Fatalf("runbook=%q", got)
	}
	if _, ok := a.Annotations["dashboard_url"]; ok {
		t.Fatal("cross-origin dashboard URL leaked")
	}
	if strings.Contains(a.Annotations["summary"], "supersecretvalue") {
		t.Fatalf("summary=%q", a.Annotations["summary"])
	}
	if a.Entity == nil || a.Entity.ContainerName != "" {
		t.Fatalf("untrusted container/entity=%+v", a.Entity)
	}
}

func TestScopeAndCatalogHardCapsFailClosed(t *testing.T) {
	var calls atomic.Int32
	src, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}), &catalog{})
	namespaces := make([]string, maxScopedNamespaces+1)
	for i := range namespaces {
		namespaces[i] = "ns"
	}
	if _, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a", Namespaces: namespaces}}); err == nil || calls.Load() != 0 {
		t.Fatalf("namespace cap calls=%d err=%v", calls.Load(), err)
	}
	over := make([]datasource.CatalogPod, maxCatalogPods+1)
	src.catalog = &catalog{items: over}
	if _, err := src.normalize(datasource.Target{ClusterID: "a"}, nil); err == nil {
		t.Fatal("catalog over cap accepted")
	}
}

func TestAmbiguousPodUIDAndUnknownGroupsStayUnlinkedAndDistinct(t *testing.T) {
	alerts := []json.RawMessage{encodedAlert(t, "one", "Down", "ns", "dup", "", 0), encodedAlert(t, "two", "Down", "ns", "missing", "", time.Second)}
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(alerts)
	})
	src, _ := fixture(t, h, &catalog{items: []datasource.CatalogPod{{Namespace: "ns", Name: "same", UID: "dup", WorkloadUID: "owner-a"}, {Namespace: "ns", Name: "same", UID: "dup", WorkloadUID: "owner-b"}}})
	res, err := src.List(context.Background(), datasource.AlertQuery{Target: datasource.Target{ClusterID: "a", Namespace: "ns"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Firing) != 2 || res.Firing[0].Entity != nil || res.Firing[1].Entity != nil || res.Firing[0].GroupKey == res.Firing[1].GroupKey {
		t.Fatalf("alerts=%+v", res.Firing)
	}
}

func TestRequestTimeOrderingAndSeverityCanonicalization(t *testing.T) {
	reference := time.Date(2026, 8, 15, 0, 30, 0, 0, time.UTC)
	s := &Source{cfg: Config{ClusterLabel: "k8s_cluster_name", NamespaceLabel: "namespace"}, catalog: &catalog{}, public: &url.URL{Scheme: "https", Host: "alerts.example"}, now: func() time.Time { return reference.Add(time.Hour) }}
	a := apiAlert{Fingerprint: "fp", StartsAt: "2026-08-15T00:00:00Z", EndsAt: "2026-08-15T00:30:00Z", UpdatedAt: "2026-08-15T00:20:00Z", Status: requiredStatus("suppressed"), Labels: map[string]string{"alertname": "A", "severity": "unknown", "k8s_cluster_name": "a"}, Annotations: map[string]string{}, Receivers: []apiReceiver{}}
	res, err := s.normalizeAt(datasource.Target{ClusterID: "a"}, []apiAlert{a}, reference)
	if err != nil || len(res.Firing) != 1 || res.Firing[0].Severity != "info" || res.Firing[0].Labels["severity"] != "info" {
		t.Fatalf("request-start normalization=%+v err=%v", res.Firing, err)
	}
	for _, updatedAt := range []string{"2026-08-14T23:59:59Z", "2026-08-15T00:30:01Z"} {
		bad := a
		bad.UpdatedAt = updatedAt
		if _, err := s.normalizeAt(datasource.Target{ClusterID: "a"}, []apiAlert{bad}, reference); err == nil {
			t.Fatalf("updatedAt ordering accepted: %s", updatedAt)
		}
	}
	a.Status.MutedBy = make([]string, maxStatusRefs+1)
	if _, err := s.normalizeAt(datasource.Target{ClusterID: "a"}, []apiAlert{a}, reference); !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("status refs cap=%v", err)
	}
}

func BenchmarkNormalize2000AlertsWith100kCatalog(b *testing.B) {
	pods := make([]datasource.CatalogPod, 100000)
	for i := range pods {
		pods[i] = datasource.CatalogPod{Namespace: "ns", Name: "pod", UID: "unused-" + strconv.Itoa(i)}
	}
	raw := make([]apiAlert, 2000)
	now := time.Date(2026, 8, 15, 0, 30, 0, 0, time.UTC)
	for i := range raw {
		raw[i] = apiAlert{Fingerprint: "fp-" + strconv.Itoa(i), StartsAt: "2026-08-15T00:00:00Z", EndsAt: "2026-08-15T01:00:00Z", UpdatedAt: "2026-08-15T00:20:00Z", Status: requiredStatus("active"), Labels: map[string]string{"alertname": "A", "k8s_cluster_name": "a", "namespace": "ns", "pod_uid": "unused-" + strconv.Itoa(i)}, Annotations: map[string]string{}, Receivers: []apiReceiver{}}
	}
	s := &Source{cfg: Config{ClusterLabel: "k8s_cluster_name", NamespaceLabel: "namespace"}, catalog: &catalog{items: pods}, public: &url.URL{Scheme: "https", Host: "alerts.example"}, now: func() time.Time { return now }}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.normalize(datasource.Target{ClusterID: "a", Namespace: "ns"}, raw); err != nil {
			b.Fatal(err)
		}
	}
}

type normalizePerformanceSample struct {
	Latency      time.Duration
	TotalAlloc   uint64
	CatalogCalls int32
}

func validateNormalizePerformance(got, limit normalizePerformanceSample) error {
	if got.Latency > limit.Latency {
		return errors.New("latency budget")
	}
	if got.TotalAlloc > limit.TotalAlloc {
		return errors.New("allocation budget")
	}
	if got.CatalogCalls > limit.CatalogCalls {
		return errors.New("catalog call budget")
	}
	return nil
}

func TestNormalizePerformanceBudgetsAndIndependentMutations(t *testing.T) {
	limit := normalizePerformanceSample{Latency: 100 * time.Millisecond, TotalAlloc: 32 << 20, CatalogCalls: 1}
	valid := normalizePerformanceSample{Latency: time.Millisecond, TotalAlloc: 1, CatalogCalls: 1}
	for name, mutate := range map[string]func(*normalizePerformanceSample){
		"latency":      func(s *normalizePerformanceSample) { s.Latency = limit.Latency + 1 },
		"allocation":   func(s *normalizePerformanceSample) { s.TotalAlloc = limit.TotalAlloc + 1 },
		"catalog-call": func(s *normalizePerformanceSample) { s.CatalogCalls = limit.CatalogCalls + 1 },
	} {
		t.Run(name+" mutation", func(t *testing.T) {
			x := valid
			mutate(&x)
			if validateNormalizePerformance(x, limit) == nil {
				t.Fatal("independent +1 mutation accepted")
			}
		})
	}
	pods := make([]datasource.CatalogPod, 100000)
	for i := range pods {
		pods[i] = datasource.CatalogPod{Namespace: "ns", Name: "pod", UID: "unused-" + strconv.Itoa(i)}
	}
	raw := make([]apiAlert, 2000)
	now := time.Date(2026, 8, 15, 0, 30, 0, 0, time.UTC)
	for i := range raw {
		raw[i] = apiAlert{Fingerprint: "fp-" + strconv.Itoa(i), StartsAt: "2026-08-15T00:00:00Z", EndsAt: "2026-08-15T01:00:00Z", UpdatedAt: "2026-08-15T00:20:00Z", Status: requiredStatus("active"), Labels: map[string]string{"alertname": "A", "k8s_cluster_name": "a", "namespace": "ns", "pod_uid": "unused-" + strconv.Itoa(i)}, Annotations: map[string]string{}, Receivers: []apiReceiver{}}
	}
	catalog := &catalog{items: pods}
	s := &Source{cfg: Config{ClusterLabel: "k8s_cluster_name", NamespaceLabel: "namespace"}, catalog: catalog, public: &url.URL{Scheme: "https", Host: "alerts.example"}, now: func() time.Time { return now }}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	if _, err := s.normalize(datasource.Target{ClusterID: "a", Namespace: "ns"}, raw); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	got := normalizePerformanceSample{Latency: elapsed, TotalAlloc: after.TotalAlloc - before.TotalAlloc, CatalogCalls: catalog.calls.Load()}
	if raceEnabled {
		got.Latency = 0
	}
	if err := validateNormalizePerformance(got, limit); err != nil {
		t.Fatalf("normalize performance=%+v: %v", got, err)
	}
	t.Logf("normalize performance latency=%s alloc=%d calls=%d", got.Latency, got.TotalAlloc, got.CatalogCalls)
}

func TestLifecycleHelpersAndObserver(t *testing.T) {
	s := &Source{}
	c := &catalog{}
	if err := s.BindCatalog(nil); err == nil {
		t.Fatal("nil catalog was accepted")
	}
	if err := s.BindCatalog(c); err != nil {
		t.Fatal(err)
	}
	if err := s.BindCatalog(c); err == nil {
		t.Fatal("duplicate catalog bind was accepted")
	}

	observer := &observerStub{}
	s.SetObserver(nil)
	s.SetObserver(observer)
	if observer.circuits != 1 {
		t.Fatalf("initial circuit observations=%d, want 1", observer.circuits)
	}
	if got := (httpError{code: http.StatusBadGateway}).Error(); got != "alertmanager request failed" {
		t.Fatalf("safe HTTP error=%q", got)
	}
	if got := namespaceRegex([]string{"team.b", "team-a"}); got != `^(?:team-a|team\.b)$` {
		t.Fatalf("namespace regex=%q", got)
	}
}

func TestAlertOutcome(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	deadline, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer deadlineCancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want upstream.Outcome
	}{
		{name: "success", ctx: context.Background(), want: upstream.OutcomeSuccess},
		{name: "caller canceled", ctx: canceled, err: errUnavailable, want: upstream.OutcomeCanceled},
		{name: "caller deadline", ctx: deadline, err: errUnavailable, want: upstream.OutcomeTimeout},
		{name: "circuit", ctx: context.Background(), err: errCircuitOpen, want: upstream.OutcomeCircuitOpen},
		{name: "internal timeout", ctx: context.Background(), err: context.DeadlineExceeded, want: upstream.OutcomeTimeout},
		{name: "unavailable", ctx: context.Background(), err: errUnavailable, want: upstream.OutcomeUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := alertOutcome(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("outcome=%v, want %v", got, tc.want)
			}
		})
	}
}
