package eventstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/roostlabs/protocol"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "events.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAppendAssignsSeqPerTask(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	for i, want := range []uint64{1, 2, 3} {
		got, err := s.Append(ctx, "T-1", protocol.EventStage, nil, int64(i))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if got != want {
			t.Errorf("seq = %d, want %d", got, want)
		}
	}

	// A second task starts its own sequence rather than continuing the first.
	got, err := s.Append(ctx, "T-2", protocol.EventStage, nil, 10)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got != 1 {
		t.Errorf("seq for a fresh task = %d, want 1", got)
	}
}

func TestAppendRejectsEmptyTaskID(t *testing.T) {
	if _, err := open(t).Append(context.Background(), "", protocol.EventStage, nil, 1); err == nil {
		t.Error("Append accepted an empty task id, want error")
	}
}

func TestSinceReplaysAfterSeq(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	for i := range 5 {
		if _, err := s.Append(ctx, "T-1", protocol.EventCmdOutput,
			json.RawMessage(`{"chunk":"line"}`), int64(i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	// This is the reconnect path: Cloud says it holds up to seq 2.
	got, err := s.Since(ctx, "T-1", 2, 0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("replayed %d events, want 3", len(got))
	}
	for i, e := range got {
		if want := uint64(i + 3); e.Seq != want {
			t.Errorf("event %d has seq %d, want %d", i, e.Seq, want)
		}
		if e.Kind != protocol.EventCmdOutput {
			t.Errorf("event %d kind = %q", i, e.Kind)
		}
		if string(e.Payload) != `{"chunk":"line"}` {
			t.Errorf("event %d payload = %s", i, e.Payload)
		}
	}
}

func TestSinceRespectsLimit(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for i := range 10 {
		if _, err := s.Append(ctx, "T-1", protocol.EventStage, nil, int64(i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := s.Since(ctx, "T-1", 0, 4)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("got %d events, want the 4 asked for", len(got))
	}
}

func TestSinceOnUnknownTaskIsEmpty(t *testing.T) {
	got, err := open(t).Since(context.Background(), "T-nope", 0, 0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d events for an unknown task, want none", len(got))
	}
}

func TestLastSeq(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	got, err := s.LastSeq(ctx, "T-1")
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if got != 0 {
		t.Errorf("LastSeq on an empty task = %d, want 0", got)
	}

	for i := range 3 {
		if _, err := s.Append(ctx, "T-1", protocol.EventStage, nil, int64(i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if got, err = s.LastSeq(ctx, "T-1"); err != nil {
		t.Fatalf("LastSeq: %v", err)
	} else if got != 3 {
		t.Errorf("LastSeq = %d, want 3", got)
	}
}

func TestTasksOrderedByRecency(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if _, err := s.Append(ctx, "T-old", protocol.EventStage, nil, 100); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := s.Append(ctx, "T-new", protocol.EventStage, nil, 900); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Tasks(ctx)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(got) != 2 || got[0] != "T-new" || got[1] != "T-old" {
		t.Errorf("Tasks() = %v, want [T-new T-old]", got)
	}
}

// History has to outlive the process, or a restarted Runner would answer trace
// queries with nothing.
func TestJournalSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := first.Append(ctx, "T-1", protocol.EventPR,
		json.RawMessage(`{"url":"https://example.com/pull/1"}`), 1); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	got, err := second.Since(ctx, "T-1", 0, 0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 1 || got[0].Kind != protocol.EventPR {
		t.Fatalf("after reopen got %+v, want the pr event", got)
	}
}
