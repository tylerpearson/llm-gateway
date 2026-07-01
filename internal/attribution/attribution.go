// Package attribution turns a completed request into a cost attributed log
// record and writes it to a sink (ClickHouse in production) off the request hot
// path. Records are buffered and batched so a slow analytics store never blocks
// or fails a user request.
package attribution

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Record is one request's attribution row: who spent what on which model.
type Record struct {
	Timestamp        time.Time
	RequestID        string
	KeyID            string
	TeamID           string
	RequestedModel   string
	ServedModel      string
	Provider         string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          float64
	LatencyMS        int64
	CacheHit         bool
	Status           int

	// UserAgent is the client's User-Agent header, the tool that made the call
	// (for example Claude Code or a CLI). It lets spend be sliced by client tool.
	UserAgent string
	// EndUser is the end customer the call was made on behalf of, taken from the
	// request rather than the virtual key. It is empty when the caller supplies
	// no end user.
	EndUser string
	// Tags are arbitrary spend tags captured from the request, each either a bare
	// label or a name:value pair from a configured header. They are deduplicated
	// and sorted so equal inputs produce equal rows.
	Tags []string
}

// Recorder accepts records for asynchronous persistence. Record must not block.
type Recorder interface {
	Record(r Record)
}

// Sink is the durable backend a Writer flushes batches to.
type Sink interface {
	InsertRequestLogs(ctx context.Context, recs []Record) error
}

// Writer is an asynchronous, batching Recorder. It collects records from a
// buffered channel and flushes them to the Sink in batches or on a timer.
type Writer struct {
	ch        chan Record
	sink      Sink
	log       *slog.Logger
	batchSize int
	interval  time.Duration
	wg        sync.WaitGroup

	dropped atomic.Int64
}

// Options configure a Writer. Zero values select sensible defaults.
type Options struct {
	BufferSize int
	BatchSize  int
	Interval   time.Duration
}

// NewWriter starts a Writer flushing to sink. Call Close to drain and stop.
func NewWriter(sink Sink, log *slog.Logger, opts Options) *Writer {
	if opts.BufferSize <= 0 {
		opts.BufferSize = 1024
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 100
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	w := &Writer{
		ch:        make(chan Record, opts.BufferSize),
		sink:      sink,
		log:       log,
		batchSize: opts.BatchSize,
		interval:  opts.Interval,
	}
	w.wg.Add(1)
	go w.loop()
	return w
}

// Record enqueues r without blocking. If the buffer is full the record is
// dropped and counted; losing analytics rows is preferable to stalling a user
// request.
func (w *Writer) Record(r Record) {
	select {
	case w.ch <- r:
	default:
		n := w.dropped.Add(1)
		if n == 1 || n%1000 == 0 {
			w.log.Warn("attribution buffer full, dropping record", slog.Int64("dropped_total", n))
		}
	}
}

func (w *Writer) loop() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	batch := make([]Record, 0, w.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := w.sink.InsertRequestLogs(ctx, batch); err != nil {
			w.log.Error("attribution flush failed", slog.Int("records", len(batch)), slog.Any("error", err))
		}
		batch = batch[:0]
	}

	for {
		select {
		case r, ok := <-w.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, r)
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Close stops accepting records, flushes the remaining batch, and waits for the
// background goroutine to exit.
func (w *Writer) Close() {
	close(w.ch)
	w.wg.Wait()
}

var _ Recorder = (*Writer)(nil)
