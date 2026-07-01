package attribution

import (
	"context"
	"errors"
	"testing"
)

type recordingSink struct {
	batches [][]Record
	err     error
}

func (s *recordingSink) InsertRequestLogs(_ context.Context, recs []Record) error {
	s.batches = append(s.batches, recs)
	return s.err
}

func TestMultiSinkFansOutToAll(t *testing.T) {
	a := &recordingSink{}
	b := &recordingSink{}
	m := NewMultiSink(a, b)

	recs := []Record{{RequestID: "r1"}, {RequestID: "r2"}}
	if err := m.InsertRequestLogs(context.Background(), recs); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	for name, s := range map[string]*recordingSink{"a": a, "b": b} {
		if len(s.batches) != 1 {
			t.Fatalf("sink %s got %d batches, want 1", name, len(s.batches))
		}
		if len(s.batches[0]) != 2 {
			t.Fatalf("sink %s got %d records, want 2", name, len(s.batches[0]))
		}
	}
}

func TestMultiSinkWritesAllDespiteError(t *testing.T) {
	errBoom := errors.New("boom")
	failing := &recordingSink{err: errBoom}
	ok := &recordingSink{}
	m := NewMultiSink(failing, ok)

	err := m.InsertRequestLogs(context.Background(), []Record{{RequestID: "r1"}})
	if !errors.Is(err, errBoom) {
		t.Fatalf("want joined error to contain boom, got %v", err)
	}
	// The healthy sink must still receive the batch even though the first failed.
	if len(ok.batches) != 1 {
		t.Fatalf("healthy sink got %d batches, want 1", len(ok.batches))
	}
}
