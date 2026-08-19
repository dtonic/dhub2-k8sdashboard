package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Postgres struct {
	pool      *pgxpool.Pool
	cursorKey []byte
}

func Open(ctx context.Context, databaseURL string, cursorKey []byte, maxConns int32, timeout time.Duration, requireTLS ...bool) (*Postgres, error) {
	if len(cursorKey) < 32 {
		return nil, fmt.Errorf("dashboard cursor key must be at least 32 bytes")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid dashboard database configuration")
	}
	if len(requireTLS) > 0 && requireTLS[0] && !verifiedTLS(cfg) {
		return nil, fmt.Errorf("dashboard database requires verified TLS")
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open dashboard database: %w", err)
	}
	p := &Postgres{pool: pool, cursorKey: append([]byte(nil), cursorKey...)}
	if err = p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("dashboard database unavailable: %w", err)
	}
	return p, nil
}
func verifiedTLS(cfg *pgxpool.Config) bool {
	return cfg.ConnConfig.TLSConfig != nil && !cfg.ConnConfig.TLSConfig.InsecureSkipVerify && cfg.ConnConfig.TLSConfig.ServerName != ""
}
func (p *Postgres) Close() error                    { p.pool.Close(); return nil }
func (p *Postgres) Ready(ctx context.Context) error { return p.pool.Ping(ctx) }
func (p *Postgres) migrate(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(604390024)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS dashboard_schema_version (version integer PRIMARY KEY)`); err != nil {
		return err
	}
	var version int
	err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0) FROM dashboard_schema_version`).Scan(&version)
	if err != nil {
		return err
	}
	if version > 1 {
		return fmt.Errorf("dashboard database schema version %d is newer than supported 1", version)
	}
	if version < 1 {
		sql, err := migrations.ReadFile("migrations/001.sql")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("dashboard migration 1: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func (p *Postgres) Create(ctx context.Context, owner string, def Definition) (Draft, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Draft{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, owner); err != nil {
		return Draft{}, err
	}
	var n int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM dashboard_drafts WHERE owner_sub=$1`, owner).Scan(&n); err != nil {
		return Draft{}, err
	}
	if n >= MaxDraftsPerOwner {
		return Draft{}, ErrLimit
	}
	b, _ := json.Marshal(def)
	id := uuid.NewString()
	row := tx.QueryRow(ctx, `INSERT INTO dashboard_drafts(id,owner_sub,state,schema_version,definition) VALUES($1,$2,'draft',$3,$4) RETURNING id,owner_sub,revision,state,schema_version,definition,created_at,updated_at`, id, owner, def.SchemaVersion, b)
	d, err := scan(row)
	if err != nil {
		return Draft{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Draft{}, err
	}
	return d, nil
}
func (p *Postgres) List(ctx context.Context, owner string, admin bool, cursor string, limit int) (Page, error) {
	if limit < 1 || limit > MaxListPage {
		limit = MaxListPage
	}
	at, id, err := p.decodeCursor(cursor)
	if err != nil {
		return Page{}, err
	}
	if at.IsZero() {
		at = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		id = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := p.pool.Query(ctx, `SELECT id,owner_sub,revision,state,schema_version,definition,created_at,updated_at FROM dashboard_drafts WHERE (owner_sub=$2 OR ($1 AND state IN ('submitted','approved'))) AND (created_at,id)<($3,$4::uuid) ORDER BY created_at DESC,id DESC LIMIT $5`, admin, owner, at, id, limit+1)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	page := Page{Items: []Draft{}}
	for rows.Next() {
		d, err := scan(rows)
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
		page.NextCursor = p.encodeCursor(last.CreatedAt, last.ID)
		page.Items = page.Items[:limit]
	}
	return page, nil
}
func (p *Postgres) Get(ctx context.Context, id, owner string, admin bool) (Draft, error) {
	return scan(p.pool.QueryRow(ctx, `SELECT id,owner_sub,revision,state,schema_version,definition,created_at,updated_at FROM dashboard_drafts WHERE id=$1 AND (owner_sub=$2 OR ($3 AND state IN ('submitted','approved')))`, id, owner, admin))
}
func (p *Postgres) Update(ctx context.Context, id, owner string, rev int64, def Definition) (Draft, error) {
	b, _ := json.Marshal(def)
	d, err := scan(p.pool.QueryRow(ctx, `UPDATE dashboard_drafts SET definition=$4,schema_version=$5,revision=revision+1,updated_at=now() WHERE id=$1 AND owner_sub=$2 AND revision=$3 AND state='draft' RETURNING id,owner_sub,revision,state,schema_version,definition,created_at,updated_at`, id, owner, rev, b, def.SchemaVersion))
	if errors.Is(err, ErrNotFound) {
		return Draft{}, p.casError(ctx, id, owner, rev, false)
	}
	return d, err
}
func (p *Postgres) Delete(ctx context.Context, id, owner string, rev int64) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM dashboard_drafts WHERE id=$1 AND owner_sub=$2 AND revision=$3 AND state IN ('draft','submitted')`, id, owner, rev)
	if err == nil && tag.RowsAffected() == 0 {
		return p.casError(ctx, id, owner, rev, false)
	}
	return err
}
func (p *Postgres) Submit(ctx context.Context, id, owner string, rev int64) (Draft, error) {
	d, err := scan(p.pool.QueryRow(ctx, `UPDATE dashboard_drafts SET state='submitted',revision=revision+1,updated_at=now() WHERE id=$1 AND owner_sub=$2 AND revision=$3 AND state='draft' RETURNING id,owner_sub,revision,state,schema_version,definition,created_at,updated_at`, id, owner, rev))
	if errors.Is(err, ErrNotFound) {
		return Draft{}, p.casError(ctx, id, owner, rev, false)
	}
	return d, err
}
func (p *Postgres) Approve(ctx context.Context, id string, rev int64) (Draft, error) {
	d, err := scan(p.pool.QueryRow(ctx, `UPDATE dashboard_drafts SET state='approved',revision=revision+1,updated_at=now() WHERE id=$1 AND revision=$2 AND state='submitted' RETURNING id,owner_sub,revision,state,schema_version,definition,created_at,updated_at`, id, rev))
	if errors.Is(err, ErrNotFound) {
		return Draft{}, p.casError(ctx, id, "", rev, true)
	}
	return d, err
}
func (p *Postgres) casError(ctx context.Context, id, owner string, rev int64, admin bool) error {
	d, err := p.Get(ctx, id, owner, admin)
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

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Draft, error) {
	var d Draft
	var b []byte
	if err := row.Scan(&d.ID, &d.Owner, &d.Revision, &d.State, &d.SchemaVersion, &b, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Draft{}, ErrNotFound
		}
		return Draft{}, err
	}
	if err := json.Unmarshal(b, &d.Definition); err != nil {
		return Draft{}, err
	}
	return d, nil
}
func (p *Postgres) encodeCursor(t time.Time, id string) string {
	u, _ := uuid.Parse(id)
	b := make([]byte, 8+16+sha256.Size)
	binary.BigEndian.PutUint64(b, uint64(t.UnixNano()))
	copy(b[8:24], u[:])
	mac := hmac.New(sha256.New, p.cursorKey)
	mac.Write(b[:24])
	copy(b[24:], mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString(b)
}
func (p *Postgres) decodeCursor(s string) (time.Time, string, error) {
	if s == "" {
		return time.Time{}, "", nil
	}
	if len(s) > 128 {
		return time.Time{}, "", ErrInvalidCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 56 {
		return time.Time{}, "", ErrInvalidCursor
	}
	mac := hmac.New(sha256.New, p.cursorKey)
	mac.Write(b[:24])
	if !hmac.Equal(b[24:], mac.Sum(nil)) {
		return time.Time{}, "", ErrInvalidCursor
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(b[:8]))), uuid.UUID(b[8:24]).String(), nil
}
