package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// SenderRow is the stored configuration of one sender instance.
type SenderRow struct {
	Name      string
	Type      string
	Enabled   bool
	Config    json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

var ErrExists = errors.New("already exists")

func (s *Store) Senders(ctx context.Context) ([]SenderRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, type, enabled, config, created_at, updated_at FROM senders ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SenderRow
	for rows.Next() {
		r, err := scanSender(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanSender(sc scanner) (SenderRow, error) {
	var r SenderRow
	var cfg string
	var en int
	var c, u int64
	if err := sc.Scan(&r.Name, &r.Type, &en, &cfg, &c, &u); err != nil {
		return SenderRow{}, err
	}
	r.Enabled, r.Config, r.CreatedAt, r.UpdatedAt = en != 0, json.RawMessage(cfg), fromMS(c), fromMS(u)
	return r, nil
}

func (s *Store) Sender(ctx context.Context, name string) (SenderRow, error) {
	r, err := scanSender(s.db.QueryRowContext(ctx,
		`SELECT name, type, enabled, config, created_at, updated_at FROM senders WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return SenderRow{}, ErrNotFound
	}
	return r, err
}

func (s *Store) CreateSender(ctx context.Context, r SenderRow) error {
	t := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO senders(name, type, enabled, config, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, r.Name, r.Type, boolInt(r.Enabled), string(r.Config), t, t)
	if err != nil && isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

func (s *Store) UpdateSender(ctx context.Context, r SenderRow) error {
	res, err := s.db.ExecContext(ctx, `UPDATE senders SET enabled = ?, config = ?, updated_at = ? WHERE name = ?`,
		boolInt(r.Enabled), string(r.Config), now(), r.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSender removes the sender and drops it from every route.
func (s *Store) DeleteSender(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM senders WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	routes, err := routesTx(ctx, tx)
	if err != nil {
		return err
	}
	for kind, list := range routes {
		kept := list[:0]
		for _, n := range list {
			if n != name {
				kept = append(kept, n)
			}
		}
		if len(kept) != len(list) {
			if err := setRouteTx(ctx, tx, kind, kept); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Routes returns the per-kind default sender lists. Kinds without an entry go to every sender.
func (s *Store) Routes(ctx context.Context) (map[string][]string, error) {
	return routesTx(ctx, s.db)
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func routesTx(ctx context.Context, q querier) (map[string][]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT kind, senders FROM routes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var kind, list string
		if err := rows.Scan(&kind, &list); err != nil {
			return nil, err
		}
		var names []string
		if err := json.Unmarshal([]byte(list), &names); err != nil {
			return nil, err
		}
		if names == nil {
			names = []string{}
		}
		out[kind] = names
	}
	return out, rows.Err()
}

// SetRoute sets a kind's sender list; nil removes the route (back to "every sender").
func (s *Store) SetRoute(ctx context.Context, kind string, senders []string) error {
	return setRouteTx(ctx, s.db, kind, senders)
}

func setRouteTx(ctx context.Context, q querier, kind string, senders []string) error {
	if senders == nil {
		_, err := q.ExecContext(ctx, `DELETE FROM routes WHERE kind = ?`, kind)
		return err
	}
	b, _ := json.Marshal(senders)
	_, err := q.ExecContext(ctx, `INSERT INTO routes(kind, senders) VALUES (?, ?)
		ON CONFLICT(kind) DO UPDATE SET senders = excluded.senders`, kind, string(b))
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
