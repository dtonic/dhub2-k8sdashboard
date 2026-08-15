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
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	f, err := e2efixture.StartAuthSession(ctx, e2efixture.AuthSessionConfig{DistDir: *dist, RedisAddr: *redisAddr, EncryptionKey: *key, PublicOrigin: *publicOrigin, CertFile: *certFile, KeyFile: *keyFile}, logger)
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
