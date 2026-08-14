package dashboard

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPostgresConcurrencyMigrationAndPrivacy(t *testing.T) {
	url := os.Getenv("DASHBOARD_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("DASHBOARD_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	key := []byte("issue24-integration-cursor-key-000000000")
	var stores [2]*Postgres
	var openErr [2]error
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range stores {
		go func(i int) { defer wg.Done(); stores[i], openErr[i] = Open(ctx, url, key, 4, 5*time.Second) }(i)
	}
	wg.Wait()
	for _, err := range openErr {
		if err != nil {
			t.Fatal(err)
		}
	}
	defer stores[0].Close()
	defer stores[1].Close()
	p := stores[0]
	if _, err := p.pool.Exec(ctx, `DELETE FROM dashboard_drafts`); err != nil {
		t.Fatal(err)
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
	assertOneCASWinner(t, race)
	approved, _ := p.Create(ctx, "race-approve-delete", base)
	approved, _ = p.Submit(ctx, approved.ID, approved.Owner, approved.Revision)
	race = make(chan error, 2)
	go func() { _, e := p.Approve(ctx, approved.ID, approved.Revision); race <- e }()
	go func() { race <- stores[1].Delete(ctx, approved.ID, approved.Owner, approved.Revision) }()
	assertOneCASWinner(t, race)

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

	if _, err = p.List(ctx, "owner-a", false, "tampered", 10); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("invalid cursor err=%v", err)
	}
	if page, err = p.List(ctx, "owner-a", false, "", 1); err != nil || len(page.Items) != 1 {
		t.Fatalf("first page=%+v err=%v", page, err)
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
}

func assertOneCASWinner(t *testing.T, ch <-chan error) {
	t.Helper()
	success, loser := 0, 0
	for range 2 {
		e := <-ch
		if e == nil {
			success++
		} else if errors.Is(e, ErrConflict) || errors.Is(e, ErrNotFound) || errors.Is(e, ErrInvalidState) {
			loser++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || loser != 1 {
		t.Fatalf("CAS race success=%d loser=%d", success, loser)
	}
}
