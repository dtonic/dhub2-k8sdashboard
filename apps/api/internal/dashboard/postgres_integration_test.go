package dashboard

import (
	"context"
	"errors"
	"fmt"
	urlpkg "net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresConcurrencyMigrationAndPrivacy(t *testing.T) {
	url := os.Getenv("DASHBOARD_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("DASHBOARD_POSTGRES_TEST_URL is not set")
	}
	parsedURL, err := urlpkg.Parse(url)
	if err != nil || parsedURL.Path != "/dashboard_ci" {
		t.Fatalf("integration test requires dedicated dashboard_ci database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	schema := "dashboard_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	adminPool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()
	if _, err = adminPool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, cleanupErr := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); cleanupErr != nil {
			t.Errorf("cleanup integration schema: %v", cleanupErr)
		}
	}()
	query := parsedURL.Query()
	query.Set("search_path", schema)
	parsedURL.RawQuery = query.Encode()
	url = parsedURL.String()
	key := []byte("issue24-integration-cursor-key-000000000")
	var stores [2]*Postgres
	var openErr [2]error
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range stores {
		go func(i int) { defer wg.Done(); stores[i], openErr[i] = Open(ctx, url, key, 4, 5*time.Second) }(i)
	}
	wg.Wait()
	defer func() {
		for _, store := range stores {
			if store != nil {
				store.Close()
			}
		}
	}()
	for _, err := range openErr {
		if err != nil {
			t.Fatal(err)
		}
	}
	p := stores[0]
	if err := p.Ready(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	base := validDefinition()
	d, err := p.Create(ctx, "owner-a", base)
	if err != nil {
		t.Fatal(err)
	}
	updates := []Definition{base, base}
	updates[0].Title = "writer-a"
	updates[1].Title = "writer-b"
	results := make(chan error, 2)
	for i := range updates {
		go func(i int) { _, e := stores[i].Update(ctx, d.ID, "owner-a", d.Revision, updates[i]); results <- e }(i)
	}
	success, conflict := 0, 0
	for range 2 {
		e := <-results
		if e == nil {
			success++
		} else if errors.Is(e, ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("update race success=%d conflict=%d", success, conflict)
	}

	traced, _ := p.Create(ctx, "race-delete-submit", base)
	race := make(chan error, 2)
	go func() { _, e := p.Submit(ctx, traced.ID, traced.Owner, traced.Revision); race <- e }()
	go func() { race <- stores[1].Delete(ctx, traced.ID, traced.Owner, traced.Revision) }()
	assertOneCASWinner(t, race, ErrConflict, ErrNotFound)
	approved, _ := p.Create(ctx, "race-approve-delete", base)
	approved, _ = p.Submit(ctx, approved.ID, approved.Owner, approved.Revision)
	race = make(chan error, 2)
	go func() { _, e := p.Approve(ctx, approved.ID, approved.Revision); race <- e }()
	go func() { race <- stores[1].Delete(ctx, approved.ID, approved.Owner, approved.Revision) }()
	assertOneCASWinner(t, race, ErrImmutable, ErrNotFound)

	private, _ := p.Create(ctx, "private-owner", base)
	page, err := p.List(ctx, "publisher", true, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.ID == private.ID {
			t.Fatal("publisher saw another owner's draft")
		}
	}
	if _, err = p.Get(ctx, private.ID, "publisher", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("publisher saw private draft: %v", err)
	}
	submitted, _ := p.Submit(ctx, private.ID, private.Owner, private.Revision)
	page, _ = p.List(ctx, "publisher", true, "", 50)
	found := false
	for _, item := range page.Items {
		found = found || item.ID == submitted.ID
	}
	if !found {
		t.Fatal("publisher cannot review submitted draft")
	}
	if _, err = p.Get(ctx, private.ID, "intruder", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner IDOR err=%v", err)
	}
	if _, err = p.Submit(ctx, submitted.ID, submitted.Owner, submitted.Revision); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("repeat submit err=%v", err)
	}
	immutable, err := p.Create(ctx, "immutable-owner", base)
	if err != nil {
		t.Fatal(err)
	}
	immutable, err = p.Submit(ctx, immutable.ID, immutable.Owner, immutable.Revision)
	if err != nil {
		t.Fatal(err)
	}
	immutable, err = p.Approve(ctx, immutable.ID, immutable.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Update(ctx, immutable.ID, immutable.Owner, immutable.Revision, base); !errors.Is(err, ErrImmutable) {
		t.Fatalf("approved update err=%v", err)
	}

	if _, err = p.List(ctx, "owner-a", false, "tampered", 10); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("invalid cursor err=%v", err)
	}
	if page, err = p.List(ctx, "owner-a", false, "", 1); err != nil || len(page.Items) != 1 {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	if _, err = p.Create(ctx, "owner-a", base); err != nil {
		t.Fatal(err)
	}
	page, err = p.List(ctx, "owner-a", false, "", 1)
	if err != nil || page.NextCursor == "" {
		t.Fatalf("cursor page=%+v err=%v", page, err)
	}
	next, err := p.List(ctx, "owner-a", false, page.NextCursor, 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatalf("next page=%+v err=%v", next, err)
	}
	for i := 0; i < MaxDraftsPerOwner; i++ {
		if _, err = p.Create(ctx, "capped-owner", base); err != nil {
			t.Fatalf("cap create %d: %v", i, err)
		}
	}
	if _, err = p.Create(ctx, "capped-owner", base); !errors.Is(err, ErrLimit) {
		t.Fatalf("cap err=%v", err)
	}
	concurrent := make(chan error, 40)
	for i := 0; i < 40; i++ {
		go func(i int) { _, e := stores[i%2].Create(ctx, "concurrent-capped-owner", base); concurrent <- e }(i)
	}
	capOK, capLimit := 0, 0
	for range 40 {
		e := <-concurrent
		if e == nil {
			capOK++
		} else if errors.Is(e, ErrLimit) {
			capLimit++
		} else {
			t.Fatal(e)
		}
	}
	if capOK != MaxDraftsPerOwner || capLimit != 8 {
		t.Fatalf("concurrent cap success=%d limit=%d", capOK, capLimit)
	}

	if _, err = p.pool.Exec(ctx, `INSERT INTO dashboard_schema_version(version) VALUES(2)`); err != nil {
		t.Fatal(err)
	}
	if future, err := Open(ctx, url, key, 2, time.Second); err == nil {
		future.Close()
		t.Fatal("future schema version accepted")
	}
	if _, err = p.pool.Exec(ctx, `DELETE FROM dashboard_schema_version WHERE version=2`); err != nil {
		t.Fatal(err)
	}

	canceled, cancelNow := context.WithCancel(ctx)
	cancelNow()
	if err = p.migrate(canceled); err == nil {
		t.Fatal("migration ignored canceled context")
	}
	if _, err = p.Create(canceled, "canceled-owner", base); err == nil {
		t.Fatal("create ignored canceled context")
	}
	if _, err = p.List(canceled, "canceled-owner", false, "", 1); err == nil {
		t.Fatal("list ignored canceled context")
	}
	if err = p.Delete(canceled, private.ID, private.Owner, private.Revision); err == nil {
		t.Fatal("delete ignored canceled context")
	}

	corrupt, err := p.Create(ctx, "corrupt-owner", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.pool.Exec(ctx, `UPDATE dashboard_drafts SET definition='"invalid"'::jsonb WHERE id=$1`, corrupt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = p.List(ctx, "corrupt-owner", false, "", 1); err == nil {
		t.Fatal("list accepted corrupt stored definition")
	}
}

func assertOneCASWinner(t *testing.T, ch <-chan error, allowed ...error) {
	t.Helper()
	success, loser := 0, 0
	for range 2 {
		e := <-ch
		if e == nil {
			success++
		} else {
			matched := false
			for _, sentinel := range allowed {
				matched = matched || errors.Is(e, sentinel)
			}
			if !matched {
				t.Fatal(fmt.Errorf("unexpected CAS loser: %w", e))
			}
			loser++
		}
	}
	if success != 1 || loser != 1 {
		t.Fatalf("CAS race success=%d loser=%d", success, loser)
	}
}
