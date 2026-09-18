// Package eventstore is the Runner's append-only journal of task events.
//
// It is the source of truth for task history, and it lives on the developer's
// VPS. An event is written here before it is streamed to Cloud, which is what
// lets a task survive a dropped connection without losing its trace: on
// reconnect Cloud reports the highest seq it holds per task, and the Runner
// replays everything after that.
//
// Nothing else on the VPS needs a database, so this is deliberately one SQLite
// file and no server.
package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/roostlabs/protocol"
	_ "modernc.org/sqlite" // cgo-free driver, so the Runner stays a single static binary
)

// Event is one journal entry. Seq is assigned on append and is monotonic within
// a task.
type Event struct {
	TaskID  string             `json:"taskId"`
	Seq     uint64             `json:"seq"`
	Kind    protocol.EventKind `json:"kind"`
	Payload json.RawMessage    `json:"payload,omitempty"`
	TS      int64              `json:"ts"` // unix milliseconds
}

// Store is an open journal.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
	task_id TEXT    NOT NULL,
	seq     INTEGER NOT NULL,
	kind    TEXT    NOT NULL,
	payload BLOB,
	ts      INTEGER NOT NULL,
	PRIMARY KEY (task_id, seq)
) STRICT;

CREATE INDEX IF NOT EXISTS events_ts ON events (ts);

CREATE TABLE IF NOT EXISTS tasks (
	task_id    TEXT    NOT NULL PRIMARY KEY,
	state      TEXT    NOT NULL,
	reason     TEXT    NOT NULL DEFAULT '',
	result     BLOB,
	updated_at INTEGER NOT NULL
) STRICT;
`

// Open opens the journal at path, creating the file and schema if needed.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("eventstore: %w", err)
		}
	}

	// WAL keeps a reader from blocking the writer; busy_timeout covers the
	// moment a checkpoint overlaps an append.
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("eventstore: open %s: %w", path, err)
	}

	// One connection: appends are serialized anyway, and a single writer avoids
	// SQLITE_BUSY entirely on a box running one sandbox at a time.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("eventstore: init schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("eventstore: close: %w", err)
	}
	return nil
}

// Append stores an event and returns the seq it was given.
//
// The read of MAX(seq) and the insert share one transaction, so two appends for
// the same task can never be handed the same seq.
func (s *Store) Append(ctx context.Context, taskID string, kind protocol.EventKind, payload json.RawMessage, ts int64) (uint64, error) {
	if taskID == "" {
		return 0, fmt.Errorf("eventstore: append %s: empty task id", kind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("eventstore: begin: %w", err)
	}
	defer tx.Rollback()

	var seq uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE task_id = ?`, taskID,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("eventstore: next seq for %s: %w", taskID, err)
	}

	var blob []byte
	if len(payload) > 0 {
		blob = []byte(payload)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (task_id, seq, kind, payload, ts) VALUES (?, ?, ?, ?, ?)`,
		taskID, seq, string(kind), blob, ts,
	); err != nil {
		return 0, fmt.Errorf("eventstore: insert %s for %s: %w", kind, taskID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("eventstore: commit: %w", err)
	}
	return seq, nil
}

// Since returns a task's events after seq, oldest first. This is the replay
// path: pass the LastSeq that Cloud reported in HelloOK.
//
// limit bounds one batch; zero means no bound.
func (s *Store) Since(ctx context.Context, taskID string, after uint64, limit int) ([]Event, error) {
	query := `SELECT task_id, seq, kind, payload, ts FROM events
	          WHERE task_id = ? AND seq > ? ORDER BY seq`
	args := []any{taskID, after}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("eventstore: replay %s: %w", taskID, err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var (
			e    Event
			kind string
			blob []byte
		)
		if err := rows.Scan(&e.TaskID, &e.Seq, &kind, &blob, &e.TS); err != nil {
			return nil, fmt.Errorf("eventstore: scan %s: %w", taskID, err)
		}
		e.Kind = protocol.EventKind(kind)
		if len(blob) > 0 {
			e.Payload = json.RawMessage(blob)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventstore: replay %s: %w", taskID, err)
	}
	return events, nil
}

// LastSeq reports the highest seq stored for a task, and zero when the task has
// no events.
func (s *Store) LastSeq(ctx context.Context, taskID string) (uint64, error) {
	var seq uint64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE task_id = ?`, taskID,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("eventstore: last seq for %s: %w", taskID, err)
	}
	return seq, nil
}

// Mark records a task's lifecycle state alongside its events.
//
// States are not events — they are sent up as task.state, not task.event — but
// history is read from this file, and a history that could say what a task
// did without saying how it ended would be no history.
func (s *Store) Mark(ctx context.Context, taskID string, st protocol.TaskState, ts int64) error {
	if taskID == "" {
		return errors.New("eventstore: mark: empty task id")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (task_id, state, reason, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (task_id) DO UPDATE SET
		   state = excluded.state, reason = excluded.reason, updated_at = excluded.updated_at`,
		taskID, string(st.State), st.Reason, ts,
	); err != nil {
		return fmt.Errorf("eventstore: mark %s %s: %w", taskID, st.State, err)
	}
	return nil
}

// Finish records a task's result. The state stays whatever Mark last set; a
// result arriving for a task never marked is stored against an empty state
// rather than refused, since the result is the part worth keeping.
func (s *Store) Finish(ctx context.Context, taskID string, res protocol.TaskResult, ts int64) error {
	if taskID == "" {
		return errors.New("eventstore: finish: empty task id")
	}
	blob, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("eventstore: encode result for %s: %w", taskID, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (task_id, state, result, updated_at) VALUES (?, '', ?, ?)
		 ON CONFLICT (task_id) DO UPDATE SET
		   result = excluded.result, updated_at = excluded.updated_at`,
		taskID, blob, ts,
	); err != nil {
		return fmt.Errorf("eventstore: finish %s: %w", taskID, err)
	}
	return nil
}

// History lists every task in the journal, most recently active first, with
// what the journal knows about each: its last state and result, the ticket
// its trace named, and how far its events go.
//
// limit bounds the list; zero means all of it. Tasks are read from the events
// table, so a task that has events but was never marked — one from before
// states were journalled — is still listed, with no state.
func (s *Store) History(ctx context.Context, limit int) ([]protocol.TaskHistoryEntry, error) {
	query := `
	SELECT e.task_id, MAX(e.seq), MIN(e.ts), MAX(MAX(e.ts), COALESCE(t.updated_at, 0)),
	       COALESCE(t.state, ''), COALESCE(t.reason, ''), t.result,
	       (SELECT payload FROM events WHERE task_id = e.task_id AND kind = ? ORDER BY seq LIMIT 1)
	FROM events e LEFT JOIN tasks t ON t.task_id = e.task_id
	GROUP BY e.task_id
	ORDER BY 4 DESC`
	args := []any{string(protocol.EventTicket)}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("eventstore: history: %w", err)
	}
	defer rows.Close()

	var out []protocol.TaskHistoryEntry
	for rows.Next() {
		var (
			entry  protocol.TaskHistoryEntry
			state  string
			result []byte
			ticket []byte
		)
		if err := rows.Scan(&entry.TaskID, &entry.LastSeq, &entry.StartedAt, &entry.UpdatedAt,
			&state, &entry.Reason, &result, &ticket); err != nil {
			return nil, fmt.Errorf("eventstore: scan history: %w", err)
		}
		entry.State = protocol.TaskStatus(state)
		if len(result) > 0 {
			var res protocol.TaskResult
			if err := json.Unmarshal(result, &res); err == nil {
				entry.Result = &res
			}
		}
		if len(ticket) > 0 {
			var tk protocol.TicketPayload
			if err := json.Unmarshal(ticket, &tk); err == nil && (tk.ID != "" || tk.Title != "") {
				entry.Ticket = &tk
			}
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventstore: history: %w", err)
	}
	return out, nil
}
