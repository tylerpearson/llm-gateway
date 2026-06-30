package attribution

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	mu    sync.Mutex
	total int
	calls int
	err   error
}

func (f *fakeSink) InsertRequestLogs(_ context.Context, recs []Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.total += len(recs)
	return nil
}

func (f *fakeSink) totalRecords() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestWriterFlushesAllOnClose records more than one batch worth and verifies
// every record reaches the sink across batched flushes and the final drain.
func TestWriterFlushesAllOnClose(t *testing.T) {
	sink := &fakeSink{}
	w := NewWriter(sink, testLogger(), Options{BufferSize: 1024, BatchSize: 100, Interval: time.Hour})

	const n = 250
	for i := 0; i < n; i++ {
		w.Record(Record{RequestID: "r", InputTokens: i})
	}
	w.Close()

	if got := sink.totalRecords(); got != n {
		t.Errorf("delivered %d records, want %d", got, n)
	}
	if sink.calls < 2 {
		t.Errorf("calls = %d, want at least 2 (batched + drain)", sink.calls)
	}
}

func TestWriterTimerFlush(t *testing.T) {
	sink := &fakeSink{}
	w := NewWriter(sink, testLogger(), Options{BufferSize: 16, BatchSize: 1000, Interval: 20 * time.Millisecond})
	defer w.Close()

	w.Record(Record{RequestID: "r"})
	// The record is below the batch threshold; only the interval ticker flushes.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.totalRecords() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("record not flushed by timer; delivered %d", sink.totalRecords())
}

func TestWriterSinkErrorDoesNotPanic(t *testing.T) {
	sink := &fakeSink{err: errors.New("clickhouse down")}
	w := NewWriter(sink, testLogger(), Options{BatchSize: 1, Interval: time.Hour})
	w.Record(Record{RequestID: "r"})
	w.Close() // must not panic despite the sink error
}
