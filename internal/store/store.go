// Package store persists messages and their deliveries in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"msg/internal/core"
)

var ErrNotFound = errors.New("not found")

const (
	StatusPending = "pending"
	StatusSent    = "sent"
	StatusFailed  = "failed"
)

// Message is the stored form of a submitted message.
type Message struct {
	ID         int64             `json:"id"`
	Kind       string            `json:"kind"`
	Source     string            `json:"source"`
	Data       json.RawMessage   `json:"data"`
	Envelope   core.Envelope     `json:"envelope"`
	CreatedAt  time.Time         `json:"created_at"`
	Deliveries map[string]string `json:"deliveries"` // sender -> status
}

type Delivery struct {
	ID            int64      `json:"id"`
	MessageID     int64      `json:"message_id"`
	Sender        string     `json:"sender"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"` // only while pending
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex // serialises inserts so concurrent duplicates are detected
}

var migrations = []string{
	`CREATE TABLE messages (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		kind       TEXT NOT NULL,
		source     TEXT NOT NULL,
		data       TEXT NOT NULL,
		envelope   TEXT NOT NULL,
		title      TEXT NOT NULL,  -- copies of envelope fields, for search
		body       TEXT NOT NULL,
		dedup_key  TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	);
	CREATE INDEX messages_kind ON messages(kind, id);
	CREATE INDEX messages_dedup ON messages(source, kind, dedup_key, created_at) WHERE dedup_key != '';
	CREATE TABLE deliveries (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id      INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		sender          TEXT NOT NULL,
		status          TEXT NOT NULL DEFAULT 'pending',
		attempts        INTEGER NOT NULL DEFAULT 0,
		last_error      TEXT NOT NULL DEFAULT '',
		next_attempt_at INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL,
		UNIQUE(message_id, sender)
	);
	CREATE INDEX deliveries_due ON deliveries(status, next_attempt_at);`,
	// v2: senders and routes are managed at runtime (web settings) instead of the config file.
	`CREATE TABLE senders (
		name       TEXT PRIMARY KEY,
		type       TEXT NOT NULL,
		enabled    INTEGER NOT NULL DEFAULT 1,
		config     TEXT NOT NULL,  -- JSON, includes secrets
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE TABLE routes (
		kind    TEXT PRIMARY KEY,
		senders TEXT NOT NULL  -- JSON array of sender names
	);`,
	// v3: API tokens created and revoked from the web UI. Only a hash is stored.
	`CREATE TABLE tokens (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		name         TEXT NOT NULL,
		hash         TEXT NOT NULL UNIQUE,  -- hex sha256 of the token
		prefix       TEXT NOT NULL,         -- first characters, for recognising it in lists
		scopes       TEXT NOT NULL,         -- JSON array
		kinds        TEXT,                  -- JSON array, NULL = all kinds
		created_at   INTEGER NOT NULL,
		created_by   TEXT NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0,
		revoked_at   INTEGER NOT NULL DEFAULT 0
	);
	CREATE UNIQUE INDEX tokens_active_name ON tokens(name) WHERE revoked_at = 0;`,
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// The database holds sender secrets: keep it (and its WAL files) private.
	if path != ":memory:" {
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			os.Chmod(p, 0o600)
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }
func now() int64               { return time.Now().UnixMilli() }

type NewMessage struct {
	Kind        string
	Source      string
	Data        []byte
	Envelope    core.Envelope
	DedupKey    string        // empty = no de-duplication
	DedupWindow time.Duration // how far back to look for a duplicate
}

// Insert stores a message and queues one delivery per sender, atomically.
// If a message with the same source, kind and dedup key was stored within
// the dedup window, nothing is written and that message is returned with dup=true.
func (s *Store) Insert(ctx context.Context, nm NewMessage, senders []string) (m Message, dup bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := now()

	if nm.DedupKey != "" && nm.DedupWindow > 0 {
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT id FROM messages
			WHERE source = ? AND kind = ? AND dedup_key = ? AND created_at >= ?
			ORDER BY id DESC LIMIT 1`, nm.Source, nm.Kind, nm.DedupKey, t-nm.DedupWindow.Milliseconds()).Scan(&id)
		if err == nil {
			m, err := s.Get(ctx, id)
			return m, true, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Message{}, false, err
		}
	}

	env, err := json.Marshal(nm.Envelope)
	if err != nil {
		return Message{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `INSERT INTO messages(kind, source, data, envelope, title, body, dedup_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nm.Kind, nm.Source, string(nm.Data), string(env), nm.Envelope.Title, nm.Envelope.Body, nm.DedupKey, t)
	if err != nil {
		return Message{}, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, false, err
	}
	deliveries := map[string]string{}
	for _, snd := range senders {
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(message_id, sender, next_attempt_at, updated_at)
			VALUES (?, ?, ?, ?)`, id, snd, t, t); err != nil {
			return Message{}, false, err
		}
		deliveries[snd] = StatusPending
	}
	if err := tx.Commit(); err != nil {
		return Message{}, false, err
	}
	return Message{
		ID: id, Kind: nm.Kind, Source: nm.Source, Data: nm.Data, Envelope: nm.Envelope,
		CreatedAt: fromMS(t), Deliveries: deliveries,
	}, false, nil
}

type Filter struct {
	Kinds  []string // nil = all kinds; non-nil empty = none
	Source string
	Query  string // substring of envelope title or body
	Before int64  // id < Before
	Since  int64  // id > Since
	Limit  int
}

const MaxLimit = 500

// List returns messages newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Message, error) {
	if f.Kinds != nil && len(f.Kinds) == 0 {
		return []Message{}, nil
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > MaxLimit {
		f.Limit = MaxLimit
	}
	var where []string
	var args []any
	if len(f.Kinds) > 0 {
		where = append(where, "kind IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Kinds)), ",")+")")
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if f.Source != "" {
		where = append(where, "source = ?")
		args = append(args, f.Source)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		where = append(where, `(title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	if f.Before > 0 {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	if f.Since > 0 {
		where = append(where, "id > ?")
		args = append(args, f.Since)
	}
	q := `SELECT id, kind, source, data, envelope, created_at FROM messages`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.attachDeliveries(ctx, out)
}

type scanner interface{ Scan(...any) error }

func scanMessage(r scanner) (Message, error) {
	var m Message
	var data, env string
	var created int64
	if err := r.Scan(&m.ID, &m.Kind, &m.Source, &data, &env, &created); err != nil {
		return Message{}, err
	}
	m.Data = json.RawMessage(data)
	m.CreatedAt = fromMS(created)
	if err := json.Unmarshal([]byte(env), &m.Envelope); err != nil {
		return Message{}, fmt.Errorf("message %d: bad envelope: %w", m.ID, err)
	}
	return m, nil
}

func (s *Store) attachDeliveries(ctx context.Context, ms []Message) error {
	if len(ms) == 0 {
		return nil
	}
	idx := map[int64]int{}
	args := make([]any, len(ms))
	for i := range ms {
		ms[i].Deliveries = map[string]string{}
		idx[ms[i].ID] = i
		args[i] = ms[i].ID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, sender, status FROM deliveries WHERE message_id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(ms)), ",")+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var snd, st string
		if err := rows.Scan(&id, &snd, &st); err != nil {
			return err
		}
		ms[idx[id]].Deliveries[snd] = st
	}
	return rows.Err()
}

func (s *Store) Get(ctx context.Context, id int64) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx,
		`SELECT id, kind, source, data, envelope, created_at FROM messages WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	one := []Message{m}
	if err := s.attachDeliveries(ctx, one); err != nil {
		return Message{}, err
	}
	return one[0], nil
}

// Delete removes a message; its deliveries go with it (ON DELETE CASCADE).
func (s *Store) Delete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Deliveries(ctx context.Context, messageID int64) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, message_id, sender, status, attempts, last_error, next_attempt_at, updated_at
		FROM deliveries WHERE message_id = ? ORDER BY id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		var next, upd int64
		if err := rows.Scan(&d.ID, &d.MessageID, &d.Sender, &d.Status, &d.Attempts, &d.LastError, &next, &upd); err != nil {
			return nil, err
		}
		d.UpdatedAt = fromMS(upd)
		if d.Status == StatusPending {
			t := fromMS(next)
			d.NextAttemptAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Redeliver queues the message again. With senders given, each of them is
// (re)queued, creating a delivery if there was none; otherwise every failed
// delivery of the message is retried. It returns how many deliveries were queued.
func (s *Store) Redeliver(ctx context.Context, messageID int64, senders []string) (int, error) {
	t := now()
	if len(senders) == 0 {
		res, err := s.db.ExecContext(ctx, `UPDATE deliveries SET status='pending', attempts=0, last_error='',
			next_attempt_at=?, updated_at=? WHERE message_id=? AND status='failed'`, t, t, messageID)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, snd := range senders {
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(message_id, sender, next_attempt_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(message_id, sender) DO UPDATE SET status='pending', attempts=0, last_error='',
				next_attempt_at=excluded.next_attempt_at, updated_at=excluded.updated_at`, messageID, snd, t, t); err != nil {
			return 0, err
		}
	}
	return len(senders), tx.Commit()
}

// ---- used by the dispatcher ----

type Due struct {
	ID        int64
	MessageID int64
	Sender    string
	Attempts  int
	Envelope  core.Envelope
}

// DueDeliveries returns pending deliveries whose next attempt time has passed.
func (s *Store) DueDeliveries(ctx context.Context, limit int) ([]Due, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id, d.message_id, d.sender, d.attempts, m.envelope
		FROM deliveries d JOIN messages m ON m.id = d.message_id
		WHERE d.status = 'pending' AND d.next_attempt_at <= ? ORDER BY d.id LIMIT ?`, now(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Due
	for rows.Next() {
		var d Due
		var env string
		if err := rows.Scan(&d.ID, &d.MessageID, &d.Sender, &d.Attempts, &env); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(env), &d.Envelope); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) MarkSent(ctx context.Context, id int64) error {
	t := now()
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET status='sent', attempts=attempts+1, last_error='', updated_at=? WHERE id=?`, t, id)
	return err
}

// MarkFailed records a failed attempt. A zero retryAt means give up.
func (s *Store) MarkFailed(ctx context.Context, id int64, errMsg string, retryAt time.Time) error {
	t := now()
	if retryAt.IsZero() {
		_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET status='failed', attempts=attempts+1, last_error=?, updated_at=? WHERE id=?`,
			errMsg, t, id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET attempts=attempts+1, last_error=?, next_attempt_at=?, updated_at=? WHERE id=?`,
		errMsg, ms(retryAt), t, id)
	return err
}
