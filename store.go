package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	errNotFound = errors.New("not found")
	errExists   = errors.New("already exists")
)

type Link struct {
	Slug      string     `json:"slug"`
	URL       string     `json:"url"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Hits      int64      `json:"hits"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

type Paste struct {
	ID        string     `json:"id"`
	Title     string     `json:"title,omitempty"`
	Lang      string     `json:"lang,omitempty"`
	Content   []byte     `json:"-"`
	Size      int64      `json:"size"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Anon      bool       `json:"anon"`         // created without a token (public pastes mode)
	IP        string     `json:"ip,omitempty"` // creator IP, anonymous pastes only; never shown on the public view
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS links (
  slug       TEXT PRIMARY KEY,
  url        TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER,
  hits       INTEGER NOT NULL DEFAULT 0,
  last_used  INTEGER
);
CREATE TABLE IF NOT EXISTS pastes (
  id         TEXT PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  lang       TEXT NOT NULL DEFAULT '',
  content    BLOB NOT NULL,
  size       INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER
);
CREATE INDEX IF NOT EXISTS links_expires ON links(expires_at);
CREATE INDEX IF NOT EXISTS pastes_expires ON pastes(expires_at);
`

func openStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate adds columns that older databases lack. Each step is idempotent:
// it looks at PRAGMA table_info first, so re-running on a current DB is a no-op.
func migrate(db *sql.DB) error {
	have, err := columns(db, "pastes")
	if err != nil {
		return err
	}
	for col, ddl := range map[string]string{
		"anon": `ALTER TABLE pastes ADD COLUMN anon INTEGER NOT NULL DEFAULT 0`,
		"ip":   `ALTER TABLE pastes ADD COLUMN ip TEXT`,
	} {
		if !have[col] {
			if _, err := db.Exec(ddl); err != nil {
				return fmt.Errorf("%s: %w", col, err)
			}
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS pastes_anon_created ON pastes(anon, created_at)`)
	return err
}

func columns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// --- helpers for nullable unix timestamps -----------------------------------

func toUnix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func fromUnix(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

func isSQLiteUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}

// --- links ------------------------------------------------------------------

func (s *Store) CreateLink(ctx context.Context, l *Link) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO links (slug, url, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		l.Slug, l.URL, l.CreatedAt.Unix(), toUnix(l.ExpiresAt))
	if isSQLiteUnique(err) {
		return errExists
	}
	return err
}

// GetLink returns errNotFound for unknown and expired slugs alike.
func (s *Store) GetLink(ctx context.Context, slug string) (*Link, error) {
	var l Link
	var created int64
	var exp, last sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT slug, url, created_at, expires_at, hits, last_used FROM links WHERE slug = ?`, slug).
		Scan(&l.Slug, &l.URL, &created, &exp, &l.Hits, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	l.CreatedAt = time.Unix(created, 0).UTC()
	l.ExpiresAt, l.LastUsed = fromUnix(exp), fromUnix(last)
	if l.ExpiresAt != nil && !l.ExpiresAt.After(time.Now()) {
		return nil, errNotFound
	}
	return &l, nil
}

func (s *Store) TouchLink(ctx context.Context, slug string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE links SET hits = hits + 1, last_used = ? WHERE slug = ?`, time.Now().Unix(), slug)
	return err
}

func (s *Store) DeleteLink(ctx context.Context, slug string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE slug = ?`, slug)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

func (s *Store) ListLinks(ctx context.Context) ([]Link, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT slug, url, created_at, expires_at, hits, last_used FROM links
		 WHERE expires_at IS NULL OR expires_at > ? ORDER BY created_at DESC LIMIT 1000`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		var l Link
		var created int64
		var exp, last sql.NullInt64
		if err := rows.Scan(&l.Slug, &l.URL, &created, &exp, &l.Hits, &last); err != nil {
			return nil, err
		}
		l.CreatedAt = time.Unix(created, 0).UTC()
		l.ExpiresAt, l.LastUsed = fromUnix(exp), fromUnix(last)
		out = append(out, l)
	}
	return out, rows.Err()
}

// --- pastes -----------------------------------------------------------------

func (s *Store) CreatePaste(ctx context.Context, p *Paste) error {
	anon := 0
	if p.Anon {
		anon = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pastes (id, title, lang, content, size, created_at, expires_at, anon, ip) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Title, p.Lang, p.Content, p.Size, p.CreatedAt.Unix(), toUnix(p.ExpiresAt), anon, nullStr(p.IP))
	if isSQLiteUnique(err) {
		return errExists
	}
	return err
}

func (s *Store) GetPaste(ctx context.Context, id string) (*Paste, error) {
	var p Paste
	var created int64
	var exp sql.NullInt64
	var anon int
	var ip sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, lang, content, size, created_at, expires_at, anon, ip FROM pastes WHERE id = ?`, id).
		Scan(&p.ID, &p.Title, &p.Lang, &p.Content, &p.Size, &created, &exp, &anon, &ip)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	p.ExpiresAt = fromUnix(exp)
	p.Anon, p.IP = anon == 1, ip.String
	if p.ExpiresAt != nil && !p.ExpiresAt.After(time.Now()) {
		return nil, errNotFound
	}
	return &p, nil
}

func (s *Store) DeletePaste(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pastes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

func (s *Store) ListPastes(ctx context.Context) ([]Paste, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, lang, size, created_at, expires_at, anon, ip FROM pastes
		 WHERE expires_at IS NULL OR expires_at > ? ORDER BY created_at DESC LIMIT 1000`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Paste{}
	for rows.Next() {
		var p Paste
		var created int64
		var exp sql.NullInt64
		var anon int
		var ip sql.NullString
		if err := rows.Scan(&p.ID, &p.Title, &p.Lang, &p.Size, &created, &exp, &anon, &ip); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(created, 0).UTC()
		p.ExpiresAt = fromUnix(exp)
		p.Anon, p.IP = anon == 1, ip.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountAnonSince counts anonymous pastes created at or after t (the daily cap).
func (s *Store) CountAnonSince(ctx context.Context, t time.Time) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pastes WHERE anon = 1 AND created_at >= ?`, t.Unix()).Scan(&n)
	return n, err
}

// PurgeAnon deletes every anonymous paste (the abuse kill switch). Returns rows removed.
func (s *Store) PurgeAnon(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pastes WHERE anon = 1`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Sweep deletes everything past its expiry. Returns rows removed.
func (s *Store) Sweep(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	var total int64
	for _, q := range []string{
		`DELETE FROM links WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		`DELETE FROM pastes WHERE expires_at IS NOT NULL AND expires_at <= ?`,
	} {
		res, err := s.db.ExecContext(ctx, q, now)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// janitor runs Sweep on a ticker until ctx is done.
func (s *Store) janitor(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Sweep(ctx); err != nil {
				log.Printf("janitor: %v", err)
			} else if n > 0 {
				log.Printf("janitor: removed %d expired rows", n)
			}
		}
	}
}
