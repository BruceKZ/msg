package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// TokenRow is a stored API token. The token itself is never stored, only its hash.
type TokenRow struct {
	ID         int64
	Name       string
	Hash       string
	Prefix     string
	Scopes     []string
	Kinds      []string // nil = all kinds
	CreatedAt  time.Time
	CreatedBy  string
	LastUsedAt time.Time // zero = never
	RevokedAt  time.Time // zero = active
}

func (s *Store) CreateToken(ctx context.Context, t TokenRow) (int64, error) {
	scopes, _ := json.Marshal(t.Scopes)
	var kinds any
	if t.Kinds != nil {
		b, _ := json.Marshal(t.Kinds)
		kinds = string(b)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO tokens(name, hash, prefix, scopes, kinds, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, t.Name, t.Hash, t.Prefix, string(scopes), kinds, now(), t.CreatedBy)
	if isUniqueViolation(err) {
		return 0, ErrExists
	}
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const tokenCols = `id, name, hash, prefix, scopes, kinds, created_at, created_by, last_used_at, revoked_at`

func scanToken(sc scanner) (TokenRow, error) {
	var t TokenRow
	var scopes string
	var kinds sql.NullString
	var c, lu, rv int64
	if err := sc.Scan(&t.ID, &t.Name, &t.Hash, &t.Prefix, &scopes, &kinds, &c, &t.CreatedBy, &lu, &rv); err != nil {
		return TokenRow{}, err
	}
	if err := json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
		return TokenRow{}, err
	}
	if kinds.Valid {
		if err := json.Unmarshal([]byte(kinds.String), &t.Kinds); err != nil {
			return TokenRow{}, err
		}
		if t.Kinds == nil {
			t.Kinds = []string{}
		}
	}
	t.CreatedAt = fromMS(c)
	if lu > 0 {
		t.LastUsedAt = fromMS(lu)
	}
	if rv > 0 {
		t.RevokedAt = fromMS(rv)
	}
	return t, nil
}

// ActiveToken finds a non-revoked token by hash.
func (s *Store) ActiveToken(ctx context.Context, hash string) (TokenRow, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE hash = ? AND revoked_at = 0`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return TokenRow{}, ErrNotFound
	}
	return t, err
}

// Tokens lists tokens, newest first; revoked ones only if includeRevoked.
func (s *Store) Tokens(ctx context.Context, includeRevoked bool) ([]TokenRow, error) {
	q := `SELECT ` + tokenCols + ` FROM tokens`
	if !includeRevoked {
		q += ` WHERE revoked_at = 0`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenRow{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken marks an active token revoked.
func (s *Store) RevokeToken(ctx context.Context, id int64) (TokenRow, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at = 0`, now(), id)
	if err != nil {
		return TokenRow{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return TokenRow{}, ErrNotFound
	}
	return scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE id = ?`, id))
}

// TouchToken records use, at most about once a minute per token to avoid a write per request.
func (s *Store) TouchToken(ctx context.Context, id int64) {
	t := now()
	s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ? AND last_used_at < ?`, t, id, t-60_000)
}
