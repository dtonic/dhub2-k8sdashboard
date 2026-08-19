package dashboard

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

const sqliteTestKey = "sqlite-test-cursor-key-0000000000000000"

func newSQLite(t *testing.T) *SQLite {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "drafts.db")
	s, err := OpenSQLite(ctx, path, []byte(sqliteTestKey), 5*time.Second)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sampleDef(id string) Definition {
	return Definition{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Title:         "샘플 대시보드",
		Variables:     []Variable{{ID: "scope", Label: "Scope", Kind: "scope"}},
		Widgets:       []Widget{{ID: "w1", Title: "Nodes Ready", Type: "Stat", Binding: "nodes.ready", Layout: Layout{X: 0, Y: 0, W: 3, H: 2}}},
	}
}

func TestSQLiteLifecycle(t *testing.T) {
	s := newSQLite(t)
	ctx := context.Background()

	d, err := s.Create(ctx, "alice", sampleDef("custom-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d.Revision != 1 || d.State != StateDraft || d.Owner != "alice" {
		t.Fatalf("unexpected draft: %+v", d)
	}

	// 소유자는 조회 가능, 다른 사용자는 제출 전 보이지 않는다.
	if _, err := s.Get(ctx, d.ID, "alice", false); err != nil {
		t.Fatalf("owner get: %v", err)
	}
	if _, err := s.Get(ctx, d.ID, "bob", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("admin should not see draft state: %v", err)
	}

	// revision CAS: 잘못된 revision은 충돌.
	def2 := sampleDef("custom-1")
	def2.Title = "수정본"
	if _, err := s.Update(ctx, d.ID, "alice", 99, def2); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	updated, err := s.Update(ctx, d.ID, "alice", 1, def2)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != 2 || updated.Definition.Title != "수정본" {
		t.Fatalf("unexpected update: %+v", updated)
	}

	// submit → admin 가시성 → approve → immutable.
	sub, err := s.Submit(ctx, d.ID, "alice", 2)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if sub.State != StateSubmitted {
		t.Fatalf("state: %v", sub.State)
	}
	if _, err := s.Get(ctx, d.ID, "bob", true); err != nil {
		t.Fatalf("admin should see submitted: %v", err)
	}
	appr, err := s.Approve(ctx, d.ID, sub.Revision)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if appr.State != StateApproved {
		t.Fatalf("state: %v", appr.State)
	}
	if _, err := s.Update(ctx, d.ID, "alice", appr.Revision, def2); !errors.Is(err, ErrImmutable) {
		t.Fatalf("approved must be immutable, got %v", err)
	}
	if err := s.Delete(ctx, d.ID, "alice", appr.Revision); !errors.Is(err, ErrImmutable) {
		t.Fatalf("approved delete must be immutable, got %v", err)
	}
}

func TestSQLiteLimit(t *testing.T) {
	s := newSQLite(t)
	ctx := context.Background()
	for i := 0; i < MaxDraftsPerOwner; i++ {
		if _, err := s.Create(ctx, "alice", sampleDef(fmt.Sprintf("custom-%d", i))); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := s.Create(ctx, "alice", sampleDef("overflow")); !errors.Is(err, ErrLimit) {
		t.Fatalf("expected limit, got %v", err)
	}
	// 다른 소유자는 자기 한도를 따로 가진다.
	if _, err := s.Create(ctx, "bob", sampleDef("bob-1")); err != nil {
		t.Fatalf("bob create: %v", err)
	}
}

func TestSQLiteKeysetPaging(t *testing.T) {
	s := newSQLite(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.Create(ctx, "alice", sampleDef(fmt.Sprintf("custom-%d", i))); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := s.List(ctx, "alice", false, cursor, 2)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, d := range page.Items {
			if seen[d.ID] {
				t.Fatalf("duplicate item across pages: %s", d.ID)
			}
			seen[d.ID] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("expected 5 unique drafts, got %d", len(seen))
	}
}

func TestSQLiteCursorTamperRejected(t *testing.T) {
	s := newSQLite(t)
	if _, err := s.List(context.Background(), "alice", false, "not-a-valid-cursor", 10); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("expected invalid cursor, got %v", err)
	}
}

func TestSQLitePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "drafts.db")
	s1, err := OpenSQLite(ctx, path, []byte(sqliteTestKey), 5*time.Second)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	created, err := s1.Create(ctx, "alice", sampleDef("persist-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s1.Close()

	// 재배포/재시작을 흉내내 같은 파일을 다시 연다 — draft가 보존되어야 한다.
	s2, err := OpenSQLite(ctx, path, []byte(sqliteTestKey), 5*time.Second)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer s2.Close()
	got, err := s2.Get(ctx, created.ID, "alice", false)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.Definition.ID != "persist-1" {
		t.Fatalf("lost draft after reopen: %+v", got)
	}
}
