package main

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// --- users (signed in through the IdP) -----------------------------------------

type User struct {
	Sub          string
	Email        string
	Name         string
	PlanCache    string
	PlanCachedAt *time.Time
	CreatedAt    time.Time
	LastSeen     time.Time
}

// UpsertUser records a sign-in: creates the row on first sight, refreshes
// email/name/last_seen afterwards.
func (s *Store) UpsertUser(ctx context.Context, sub, email, name string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO users (sub, email, name, created_at, last_seen) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(sub) DO UPDATE SET email = excluded.email, name = excluded.name, last_seen = excluded.last_seen`,
		sub, email, name, now, now)
	return err
}

func (s *Store) GetUser(ctx context.Context, sub string) (*User, error) {
	var u User
	var created, seen int64
	var cachedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT sub, email, name, plan_cache, plan_cached_at, created_at, last_seen FROM users WHERE sub = ?`, sub).
		Scan(&u.Sub, &u.Email, &u.Name, &u.PlanCache, &cachedAt, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, u.LastSeen = time.Unix(created, 0).UTC(), time.Unix(seen, 0).UTC()
	u.PlanCachedAt = fromUnix(cachedAt)
	return &u, nil
}

// SetUserPlanCache remembers the last plan the billing service reported, so the
// account page can still show something if billing is briefly unreachable.
func (s *Store) SetUserPlanCache(ctx context.Context, sub, plan string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET plan_cache = ?, plan_cached_at = ? WHERE sub = ?`,
		plan, time.Now().Unix(), sub)
	return err
}

// CountOwned counts live links + pastes a user owns (the per-plan item cap).
func (s *Store) CountOwned(ctx context.Context, sub string) (int64, error) {
	now := time.Now().Unix()
	var links, pastes int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM links WHERE owner_sub = ? AND (expires_at IS NULL OR expires_at > ?)`, sub, now).Scan(&links); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pastes WHERE owner_sub = ? AND (expires_at IS NULL OR expires_at > ?)`, sub, now).Scan(&pastes); err != nil {
		return 0, err
	}
	return links + pastes, nil
}

// --- per-user API tokens -------------------------------------------------------------

type UserToken struct {
	ID        string     `json:"id"`
	Sub       string     `json:"-"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

// CreateUserToken stores the sha256 hash of a token; the plaintext is shown once by the caller.
func (s *Store) CreateUserToken(ctx context.Context, id, sub, hash, name string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_tokens (id, sub, hash, name, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, sub, hash, name, time.Now().Unix())
	if isSQLiteUnique(err) {
		return errExists
	}
	return err
}

func (s *Store) ListUserTokens(ctx context.Context, sub string) ([]UserToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, sub, name, created_at, last_used FROM user_tokens WHERE sub = ? ORDER BY created_at DESC`, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserToken{}
	for rows.Next() {
		var t UserToken
		var created int64
		var last sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Sub, &t.Name, &created, &last); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsed = time.Unix(created, 0).UTC(), fromUnix(last)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteUserToken(ctx context.Context, sub, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_tokens WHERE id = ? AND sub = ?`, id, sub)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

// LookupUserToken resolves a token hash to its owner (sub) and token id.
func (s *Store) LookupUserToken(ctx context.Context, hash string) (sub, id string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT sub, id FROM user_tokens WHERE hash = ?`, hash).Scan(&sub, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errNotFound
	}
	return sub, id, err
}

func (s *Store) TouchUserToken(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_tokens SET last_used = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}
