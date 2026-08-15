package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/registry"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/transport"
)

func TestPresentInvalidEnvironmentNeverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		check       func(string) error
	}{
		{"int", "bad", func(k string) error { return validateIntEnv(k, 1, 100) }},
		{"int-zero", "0", func(k string) error { return validateIntEnv(k, 1, 100) }},
		{"float", "NaN", func(k string) error { return validateFloatEnv(k, 1, 100) }},
		{"float-inf", "+Inf", func(k string) error { return validateFloatEnv(k, 1, 100) }},
		{"float-high", "100.1", func(k string) error { return validateFloatEnv(k, 1, 100) }},
		{"duration", "0s", func(k string) error { return validateDurationEnv(k, time.Hour) }},
		{"duration-high", "1h1ns", func(k string) error { return validateDurationEnv(k, time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ISSUE25_VALUE", tc.value)
			if tc.check("ISSUE25_VALUE") == nil {
				t.Fatalf("accepted %q", tc.value)
			}
		})
	}
}

func TestRunRejectsMalformedConfigurationBeforeListen(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"int", "CLUSTER_STATE_MAX_MESSAGE_BYTES", "bad"}, {"int-range", "CLUSTER_STATE_MAX_CLUSTERS", "65"},
		{"float", "CLUSTER_STATE_INGRESS_FRAME_RATE", "NaN"}, {"float-inf", "CLUSTER_STATE_INGRESS_BYTE_RATE", "+Inf"},
		{"frame-rate-high", "CLUSTER_STATE_INGRESS_FRAME_RATE", "100000.1"}, {"byte-rate-high", "CLUSTER_STATE_INGRESS_BYTE_RATE", "1073741825"},
		{"duration", "CLUSTER_STATE_STALE_TTL", "bad"}, {"stale-high", "CLUSTER_STATE_STALE_TTL", "24h1ns"},
		{"heartbeat-high", "CLUSTER_STATE_HEARTBEAT_TIMEOUT", "1h1ns"},
		{"same-address", "CLUSTER_STATE_QUERY_ADDR", ":9443"}, {"bad-agent-address", "CLUSTER_STATE_AGENT_ADDR", "bad"},
		{"bad-query-address", "CLUSTER_STATE_QUERY_ADDR", "bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if run() == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	if run() == nil {
		t.Fatal("missing configured clusters accepted")
	}
}

func TestRunAcceptsSafeBoundaryValuesUntilTLSPreflight(t *testing.T) {
	clusters := make([]string, registry.MaxConfiguredClusters)
	for i := range clusters {
		clusters[i] = fmt.Sprintf("c-%02d", i)
	}
	t.Setenv("CLUSTER_STATE_CLUSTERS", strings.Join(clusters, ","))
	t.Setenv("CLUSTER_STATE_MAX_CLUSTERS", strconv.Itoa(registry.MaxConfiguredClusters))
	t.Setenv("CLUSTER_STATE_STALE_TTL", "24h")
	t.Setenv("CLUSTER_STATE_HEARTBEAT_TIMEOUT", "1h")
	t.Setenv("CLUSTER_STATE_INGRESS_FRAME_RATE", "100000")
	t.Setenv("CLUSTER_STATE_INGRESS_BYTE_RATE", "1073741824")
	t.Setenv("CLUSTER_STATE_INGRESS_FRAME_BURST", "100000")
	t.Setenv("CLUSTER_STATE_INGRESS_BYTE_BURST", "1073741824")
	t.Setenv("CLUSTER_STATE_AGENT_ADDR", freeAddress(t))
	t.Setenv("CLUSTER_STATE_QUERY_ADDR", freeAddress(t))
	if err := run(); err == nil || err.Error() == "invalid cluster-state registry limits" || strings.Contains(err.Error(), " is invalid") {
		t.Fatalf("safe maxima rejected before expected TLS preflight: %v", err)
	}
}

func TestRunRejectsByteBurstBelowMessageBeforeListeners(t *testing.T) {
	agentAddr, queryAddr := freeAddress(t), freeAddress(t)
	t.Setenv("CLUSTER_STATE_CLUSTERS", "a")
	t.Setenv("CLUSTER_STATE_AGENT_ADDR", agentAddr)
	t.Setenv("CLUSTER_STATE_QUERY_ADDR", queryAddr)
	t.Setenv("CLUSTER_STATE_MAX_MESSAGE_BYTES", "4194304")
	t.Setenv("CLUSTER_STATE_INGRESS_BYTE_BURST", "4194303")
	if err := run(); err == nil || err.Error() != "invalid cluster-state registry limits" {
		t.Fatalf("cross-field limit was not rejected by registry admission: %v", err)
	}
	for _, addr := range []string{agentAddr, queryAddr} {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("listener was opened before limit validation: %v", err)
		}
		_ = listener.Close()
	}
}

func TestRunRejectsAllowlistBeyondConfiguredCapacityBeforeListeners(t *testing.T) {
	agentAddr, queryAddr := freeAddress(t), freeAddress(t)
	t.Setenv("CLUSTER_STATE_CLUSTERS", "a,b")
	t.Setenv("CLUSTER_STATE_MAX_CLUSTERS", "1")
	t.Setenv("CLUSTER_STATE_AGENT_ADDR", agentAddr)
	t.Setenv("CLUSTER_STATE_QUERY_ADDR", queryAddr)
	if err := run(); err == nil || err.Error() != "invalid cluster-state registry limits" {
		t.Fatalf("oversized allowlist was not rejected by registry admission: %v", err)
	}
	for _, addr := range []string{agentAddr, queryAddr} {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("listener was opened before allowlist validation: %v", err)
		}
		_ = listener.Close()
	}
}

func TestPruneLoopStopsWithContext(t *testing.T) {
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a"}
	limits.MaxClusters = 1
	limits.StaleTTL = time.Second
	limits.HeartbeatTimeout = time.Second
	r, e := registry.New(limits)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { pruneLoop(ctx, r, time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("prune loop did not stop")
	}
}

func TestServeOwnsBothListenersAndCleansPartialStartup(t *testing.T) {
	files := registryTLSFiles(t)
	limits := registry.DefaultLimits()
	limits.AllowedClusters = []string{"a"}
	reg, err := registry.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	svc := &transport.Service{Registry: reg, TrustDomain: files.TrustDomain, MaxMessageBytes: 4 << 20}

	agentAddr := freeAddress(t)
	queryAddr := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, svc, files, agentAddr, queryAddr, 4<<20) }()
	waitTCP(t, agentAddr)
	waitTCP(t, queryAddr)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not stop after cancellation")
	}
	for _, addr := range []string{agentAddr, queryAddr} {
		if conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
			_ = conn.Close()
			t.Fatalf("listener remained open: %s", addr)
		}
	}

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	agentAddr = freeAddress(t)
	if err := serve(context.Background(), svc, files, agentAddr, occupied.Addr().String(), 4<<20); err == nil {
		t.Fatal("occupied query listener accepted")
	}
	if conn, err := net.DialTimeout("tcp", agentAddr, 50*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("partial-start agent listener leaked")
	}
	if conn, err := net.DialTimeout("tcp", occupied.Addr().String(), time.Second); err != nil {
		t.Fatalf("serve closed listener it did not own: %v", err)
	} else {
		_ = conn.Close()
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

func waitTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 25*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener did not start: %s", addr)
}

func registryTLSFiles(t *testing.T) transport.TLSFiles {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "issue25-registry-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"registry"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, kind string, der []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return transport.TLSFiles{CertFile: write("server.pem", "CERTIFICATE", leafDER), KeyFile: write("server.key", "PRIVATE KEY", keyDER), CAFile: write("ca.pem", "CERTIFICATE", caDER), TrustDomain: "example.test"}
}
