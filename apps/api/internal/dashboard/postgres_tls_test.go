package dashboard

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVerifiedTLSRequiresHostnameVerification(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		want bool
	}{{"postgres://u:p@db.example.invalid/d?sslmode=disable", false}, {"postgres://u:p@db.example.invalid/d?sslmode=require", false}, {"postgres://u:p@db.example.invalid/d?sslmode=verify-full", true}} {
		cfg, err := pgxpool.ParseConfig(tc.dsn)
		if err != nil {
			t.Fatal(err)
		}
		if got := verifiedTLS(cfg); got != tc.want {
			t.Fatalf("%s verified=%v want=%v", tc.dsn, got, tc.want)
		}
	}
}

func TestOpenRejectsInvalidConfigurationBeforeDatabaseUse(t *testing.T) {
	ctx := context.Background()
	key := []byte("unit-test-cursor-key-0000000000000")
	for name, call := range map[string]func() error{
		"short cursor key": func() error {
			_, err := Open(ctx, "postgres://localhost/db", []byte("short"), 1, time.Second)
			return err
		},
		"invalid DSN": func() error { _, err := Open(ctx, "://", key, 1, time.Second); return err },
		"unverified TLS": func() error {
			_, err := Open(ctx, "postgres://u:p@db.example.invalid/d?sslmode=disable", key, 1, time.Second, true)
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	_, err := Open(ctx, "postgres://u:p@127.0.0.1:1/d?sslmode=disable&connect_timeout=1", key, 1, 200*time.Millisecond)
	if err == nil {
		t.Fatalf("unavailable database error=%v", err)
	}
}
