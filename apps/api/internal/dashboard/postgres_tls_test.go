package dashboard

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
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
