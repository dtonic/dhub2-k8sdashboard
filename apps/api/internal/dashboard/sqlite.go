package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// sqliteSchema는 SQLite 백엔드의 스키마 버전 1입니다. PostgreSQL(migrations/001.sql)과
// 같은 테이블·인덱스를 SQLite 방언으로 옮긴 것입니다 — 계약은 동일하고 엔진만 다릅니다.
// 시각은 정렬 가능한 int64 UnixNano로 저장해 keyset 페이징의 사전순 == 시간순을 보장합니다.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS dashboard_drafts (
  id text PRIMARY KEY,
  owner_sub text NOT NULL,
  revision integer NOT NULL DEFAULT 1 CHECK (revision > 0),
  state text NOT NULL CHECK (state IN ('draft','submitted','approved')),
  schema_version integer NOT NULL,
  definition text NOT NULL,
  created_at integer NOT NULL,
  updated_at integer NOT NULL
);
CREATE INDEX IF NOT EXISTS dashboard_drafts_owner_page ON dashboard_drafts(owner_sub, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS dashboard_drafts_review_page ON dashboard_drafts(state, created_at DESC, id DESC);
INSERT OR IGNORE INTO dashboard_schema_version(version) VALUES (1);
`

const draftColumns = `id,owner_sub,revision,state,schema_version,definition,created_at,updated_at`

// SQLite는 dashboard draft를 단일 파일에 저장하는 Store 구현입니다. (ADR 0016)
// 단일 writer 전제로 MaxOpenConns(1)로 직렬화하며, 외부 DB 없이 PVC 볼륨에 영속합니다.
type SQLite struct {
	db        *sql.DB
	cursorKey []byte
}

// OpenSQLite는 path의 SQLite 파일을 열고 마이그레이션한 뒤 Store를 돌려줍니다.
// cursorKey는 서명 keyset cursor용으로 32바이트 이상이어야 합니다.
func OpenSQLite(ctx context.Context, path string, cursorKey []byte, timeout time.Duration) (*SQLite, error) {
	if len(cursorKey) < 32 {
		return nil, fmt.Errorf("dashboard cursor key must be at least 32 bytes")
	}
	if path == "" {
		return nil, fmt.Errorf("dashboard sqlite path is required")
	}
	// WAL + busy_timeout으로 재배포 직후 잠금 경합을 흡수합니다. foreign_keys는 관례상 켭니다.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open dashboard database: %w", err)
	}
	// 단일 writer — 직렬화로 "database is locked"를 원천 차단합니다.
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)
	octx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s := &SQLite{db: db, cursorKey: append([]byte(nil), cursorKey...)}
	if err = s.migrate(octx); err != nil {
		db.Close()
		return nil, err
	}
	if err = db.PingContext(octx); err != nil {
		db.Close()
		return nil, fmt.Errorf("dashboard database unavailable: %w", err)
	}
	return s, nil
}

func (s *SQLite) Close() error                    { return s.db.Close() }
func (s *SQLite) Ready(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *SQLite) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS dashboard_schema_version (version integer PRIMARY KEY)`); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM dashboard_schema_version`).Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return fmt.Errorf("dashboard database schema version %d is newer than supported 1", version)
	}
	if version < 1 {
		if _, err = tx.ExecContext(ctx, sqliteSchema); err != nil {
			return fmt.Errorf("dashboard migration 1: %w", err)
		}
	}
	return tx.Commit()
}

func (s *SQLite) Create(ctx context.Context, owner string, def Definition) (Draft, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Draft{}, err
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM dashboard_drafts WHERE owner_sub=?`, owner).Scan(&n); err != nil {
		return Draft{}, err
	}
	if n >= MaxDraftsPerOwner {
		return Draft{}, ErrLimit
	}
	b, _ := json.Marshal(def)
	id := uuid.NewString()
	now := time.Now().UTC()
	nano := now.UnixNano()
	if _, err = tx.ExecContext(ctx, `INSERT INTO dashboard_drafts(`+draftColumns+`) VALUES(?,?,1,'draft',?,?,?,?)`, id, owner, def.SchemaVersion, string(b), nano, nano); err != nil {
		return Draft{}, err
	}
	if err = tx.Commit(); err != nil {
		return Draft{}, err
	}
	return Draft{ID: id, Revision: 1, State: StateDraft, SchemaVersion: def.SchemaVersion, Definition: def, CreatedAt: now, UpdatedAt: now, Owner: owner}, nil
}

func (s *SQLite) List(ctx context.Context, owner string, admin bool, cursor string, limit int) (Page, error) {
	if limit < 1 || limit > MaxListPage {
		limit = MaxListPage
	}
	at, id, err := s.decodeCursor(cursor)
	if err != nil {
		return Page{}, err
	}
	// 첫 페이지는 최댓값 sentinel로 시작합니다 — 모든 행이 (created_at,id)보다 작습니다.
	atNano := int64(9223372036854775807)
	if !at.IsZero() {
		atNano = at.UnixNano()
	}
	if id == "" {
		id = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	adminInt := 0
	if admin {
		adminInt = 1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+draftColumns+` FROM dashboard_drafts WHERE (owner_sub=? OR (?=1 AND state IN ('submitted','approved'))) AND (created_at < ? OR (created_at = ? AND id < ?)) ORDER BY created_at DESC, id DESC LIMIT ?`, owner, adminInt, atNano, atNano, id, limit+1)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	page := Page{Items: []Draft{}}
	for rows.Next() {
		d, err := scanSQLite(rows)
		if err != nil {
			return Page{}, err
		}
		page.Items = append(page.Items, d)
	}
	if err = rows.Err(); err != nil {
		return Page{}, err
	}
	if len(page.Items) > limit {
		last := page.Items[limit-1]
		page.NextCursor = s.encodeCursor(last.CreatedAt, last.ID)
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func (s *SQLite) Get(ctx context.Context, id, owner string, admin bool) (Draft, error) {
	adminInt := 0
	if admin {
		adminInt = 1
	}
	return scanSQLite(s.db.QueryRowContext(ctx, `SELECT `+draftColumns+` FROM dashboard_drafts WHERE id=? AND (owner_sub=? OR (?=1 AND state IN ('submitted','approved')))`, id, owner, adminInt))
}

func (s *SQLite) Update(ctx context.Context, id, owner string, rev int64, def Definition) (Draft, error) {
	b, _ := json.Marshal(def)
	nano := time.Now().UTC().UnixNano()
	d, err := scanSQLite(s.db.QueryRowContext(ctx, `UPDATE dashboard_drafts SET definition=?,schema_version=?,revision=revision+1,updated_at=? WHERE id=? AND owner_sub=? AND revision=? AND state='draft' RETURNING `+draftColumns, string(b), def.SchemaVersion, nano, id, owner, rev))
	if errors.Is(err, ErrNotFound) {
		return Draft{}, s.casError(ctx, id, owner, rev, false)
	}
	return d, err
}

func (s *SQLite) Delete(ctx context.Context, id, owner string, rev int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM dashboard_drafts WHERE id=? AND owner_sub=? AND revision=? AND state IN ('draft','submitted')`, id, owner, rev)
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			return s.casError(ctx, id, owner, rev, false)
		}
	}
	return err
}

func (s *SQLite) Submit(ctx context.Context, id, owner string, rev int64) (Draft, error) {
	nano := time.Now().UTC().UnixNano()
	d, err := scanSQLite(s.db.QueryRowContext(ctx, `UPDATE dashboard_drafts SET state='submitted',revision=revision+1,updated_at=? WHERE id=? AND owner_sub=? AND revision=? AND state='draft' RETURNING `+draftColumns, nano, id, owner, rev))
	if errors.Is(err, ErrNotFound) {
		return Draft{}, s.casError(ctx, id, owner, rev, false)
	}
	return d, err
}

func (s *SQLite) Approve(ctx context.Context, id string, rev int64) (Draft, error) {
	nano := time.Now().UTC().UnixNano()
	d, err := scanSQLite(s.db.QueryRowContext(ctx, `UPDATE dashboard_drafts SET state='approved',revision=revision+1,updated_at=? WHERE id=? AND revision=? AND state='submitted' RETURNING `+draftColumns, nano, id, rev))
	if errors.Is(err, ErrNotFound) {
		return Draft{}, s.casError(ctx, id, "", rev, true)
	}
	return d, err
}

// casError는 mutation이 0행을 건드렸을 때 실제 사유(부재·불변·충돌·상태)를 되짚습니다. PostgreSQL과 동일합니다.
func (s *SQLite) casError(ctx context.Context, id, owner string, rev int64, admin bool) error {
	d, err := s.Get(ctx, id, owner, admin)
	if err != nil {
		return ErrNotFound
	}
	if d.State == StateApproved {
		return ErrImmutable
	}
	if d.Revision != rev {
		return ErrConflict
	}
	return ErrInvalidState
}

func scanSQLite(row scanner) (Draft, error) {
	var d Draft
	var def string
	var cNano, uNano int64
	if err := row.Scan(&d.ID, &d.Owner, &d.Revision, &d.State, &d.SchemaVersion, &def, &cNano, &uNano); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Draft{}, ErrNotFound
		}
		return Draft{}, err
	}
	if err := json.Unmarshal([]byte(def), &d.Definition); err != nil {
		return Draft{}, err
	}
	d.CreatedAt = time.Unix(0, cNano).UTC()
	d.UpdatedAt = time.Unix(0, uNano).UTC()
	return d, nil
}

// encodeCursor/decodeCursor는 PostgreSQL 백엔드와 동일한 서명 bounded keyset cursor입니다.
// 저장 엔진이 달라도 cursor 포맷은 같게 유지해 계약(ADR 0009)을 지킵니다.
func (s *SQLite) encodeCursor(t time.Time, id string) string {
	u, _ := uuid.Parse(id)
	b := make([]byte, 8+16+sha256.Size)
	binary.BigEndian.PutUint64(b, uint64(t.UnixNano()))
	copy(b[8:24], u[:])
	mac := hmac.New(sha256.New, s.cursorKey)
	mac.Write(b[:24])
	copy(b[24:], mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *SQLite) decodeCursor(str string) (time.Time, string, error) {
	if str == "" {
		return time.Time{}, "", nil
	}
	if len(str) > 128 {
		return time.Time{}, "", ErrInvalidCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(str)
	if err != nil || len(b) != 56 {
		return time.Time{}, "", ErrInvalidCursor
	}
	mac := hmac.New(sha256.New, s.cursorKey)
	mac.Write(b[:24])
	if !hmac.Equal(b[24:], mac.Sum(nil)) {
		return time.Time{}, "", ErrInvalidCursor
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(b[:8]))), uuid.UUID(b[8:24]).String(), nil
}
