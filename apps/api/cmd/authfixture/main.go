//go:build e2efixture

// Command authfixture runs the real browser-session handlers behind a TLS
// same-origin fixture. It is excluded from production builds and images.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/alertmanager"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/e2efixture"
)

func main() {
	dist := flag.String("dist", "../web/dist", "production Web distribution")
	redisAddr := flag.String("redis", "", "owned acceptance Redis address")
	key := flag.String("key", "", "base64url 32-byte fixture session key")
	readyFile := flag.String("ready-file", "", "write the TLS origin after readiness")
	publicOrigin := flag.String("public-origin", "", "fixed external nginx HTTPS origin")
	certFile := flag.String("cert-file", "", "write fixture TLS certificate")
	keyFile := flag.String("key-file", "", "write fixture TLS private key")
	backendAddr := flag.String("backend-addr", "", "test-only private interface bind for an owned reverse proxy")
	alertmanagerURL := flag.String("alertmanager-url", "", "private Alertmanager API base URL")
	alertmanagerPublicURL := flag.String("alertmanager-public-url", "", "public Alertmanager base URL")
	alertmanagerTokenFile := flag.String("alertmanager-token-file", "", "bearer token file")
	alertmanagerCAFile := flag.String("alertmanager-ca-file", "", "private CA file")
	alertmanagerClientCertFile := flag.String("alertmanager-client-cert-file", "", "client certificate file")
	alertmanagerClientKeyFile := flag.String("alertmanager-client-key-file", "", "client key file")
	alertmanagerServerName := flag.String("alertmanager-server-name", "", "verified TLS server name")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	f, err := e2efixture.StartAuthSession(ctx, e2efixture.AuthSessionConfig{DistDir: *dist, RedisAddr: *redisAddr, EncryptionKey: *key, PublicOrigin: *publicOrigin, CertFile: *certFile, KeyFile: *keyFile, BackendAddr: *backendAddr,
		Alertmanager: alertmanager.Config{Enabled: *alertmanagerURL != "", BaseURL: *alertmanagerURL, PublicURL: *alertmanagerPublicURL,
			TokenFile: *alertmanagerTokenFile, CAFile: *alertmanagerCAFile, ClientCertFile: *alertmanagerClientCertFile,
			ClientKeyFile: *alertmanagerClientKeyFile, ServerName: *alertmanagerServerName, ClusterLabel: "k8s_cluster_name",
			NamespaceLabel: "namespace", Timeout: time.Second, MaxBodyBytes: 4 << 20, MaxAlerts: 2000, MaxConcurrent: 4}}, logger)
	if err != nil {
		logger.Error("auth fixture failed", "err", err)
		os.Exit(1)
	}
	defer f.Close()
	if *readyFile != "" {
		ready, _ := json.Marshal(map[string]any{"fixtureURL": f.URL, "issuer": f.Issuer, "apiURLs": f.APIURLs})
		if err := os.WriteFile(*readyFile, ready, 0o600); err != nil {
			logger.Error("ready file failed", "err", err)
			os.Exit(1)
		}
	}
	fmt.Println("AUTH_FIXTURE_URL=" + f.URL)
	<-ctx.Done()
}
