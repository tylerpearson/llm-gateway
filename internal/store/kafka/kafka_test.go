package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"github.com/tylerpearson/llm-gateway/internal/attribution"
)

type fakeProducer struct {
	written []kafkago.Message
	err     error
}

func (f *fakeProducer) WriteMessages(_ context.Context, msgs ...kafkago.Message) error {
	if f.err != nil {
		return f.err
	}
	f.written = append(f.written, msgs...)
	return nil
}

func (f *fakeProducer) Close() error { return nil }

func TestInsertRequestLogsPublishesKeyedJSON(t *testing.T) {
	fp := &fakeProducer{}
	s := &Store{w: fp}

	recs := []attribution.Record{
		{RequestID: "r1", KeyID: "key-a", CostUSD: 0.5, ServedModel: "claude-sonnet-5"},
		{RequestID: "r2", KeyID: "key-b", InputTokens: 10, Timestamp: time.Unix(0, 0).UTC()},
	}
	if err := s.InsertRequestLogs(context.Background(), recs); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}
	if len(fp.written) != 2 {
		t.Fatalf("wrote %d messages, want 2", len(fp.written))
	}

	if got := string(fp.written[0].Key); got != "key-a" {
		t.Errorf("message 0 key = %q, want key-a", got)
	}
	var decoded attribution.Record
	if err := json.Unmarshal(fp.written[0].Value, &decoded); err != nil {
		t.Fatalf("unmarshal value: %v", err)
	}
	if decoded.RequestID != "r1" || decoded.ServedModel != "claude-sonnet-5" {
		t.Errorf("decoded record = %+v, want r1/claude-sonnet-5", decoded)
	}
}

func TestInsertRequestLogsEmptyBatchIsNoop(t *testing.T) {
	fp := &fakeProducer{}
	s := &Store{w: fp}
	if err := s.InsertRequestLogs(context.Background(), nil); err != nil {
		t.Fatalf("InsertRequestLogs(nil): %v", err)
	}
	if len(fp.written) != 0 {
		t.Fatalf("wrote %d messages for empty batch, want 0", len(fp.written))
	}
}

func TestInsertRequestLogsPropagatesWriteError(t *testing.T) {
	errBoom := errors.New("broker down")
	s := &Store{w: &fakeProducer{err: errBoom}}
	err := s.InsertRequestLogs(context.Background(), []attribution.Record{{RequestID: "r1"}})
	if !errors.Is(err, errBoom) {
		t.Fatalf("want error to wrap broker down, got %v", err)
	}
}

func TestOpenValidatesConfig(t *testing.T) {
	if _, err := Open(nil, "topic"); err == nil {
		t.Error("Open with no brokers should error")
	}
	if _, err := Open([]string{"localhost:9092"}, ""); err == nil {
		t.Error("Open with no topic should error")
	}
}
