package attribution

import (
	"context"
	"errors"
)

// MultiSink fans a single batch out to several sinks, for example ClickHouse for
// direct queries and Kafka for an existing logging pipeline. Every sink is
// attempted even when an earlier one fails, and the combined error is returned
// so the Writer logs all failures rather than stopping at the first.
type MultiSink struct {
	sinks []Sink
}

// NewMultiSink returns a Sink that writes to each of sinks in order. It is the
// caller's job to pass at least one sink; an empty MultiSink is a no-op.
func NewMultiSink(sinks ...Sink) *MultiSink {
	return &MultiSink{sinks: sinks}
}

// InsertRequestLogs writes recs to every sink and joins any errors.
func (m *MultiSink) InsertRequestLogs(ctx context.Context, recs []Record) error {
	var errs []error
	for _, s := range m.sinks {
		if err := s.InsertRequestLogs(ctx, recs); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var _ Sink = (*MultiSink)(nil)
