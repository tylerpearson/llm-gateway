// Package kafka is a Kafka sink for request attribution logs. It implements
// attribution.Sink by publishing one JSON message per record to a topic, so
// spend and usage events can flow into an existing logging pipeline (Kafka, or
// any Kafka-compatible bus) instead of, or alongside, ClickHouse. Downstream
// consumers land the stream wherever analytics live.
package kafka

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/segmentio/kafka-go"

	"github.com/tylerpearson/llm-gateway/internal/attribution"
)

// producer is the subset of kafka.Writer the sink uses. It is an interface so
// tests can substitute a fake without a live broker.
type producer interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Store publishes attribution records to a Kafka topic.
type Store struct {
	w producer
}

// Open returns a Store that writes to topic on the given comma-free broker list.
// Messages are keyed by virtual key ID so a single key's events land on one
// partition and stay ordered. The writer batches and retries internally; it is
// safe for concurrent use.
func Open(brokers []string, topic string) (*Store, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("kafka: no brokers configured")
	}
	if topic == "" {
		return nil, fmt.Errorf("kafka: no topic configured")
	}
	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.Hash{},
		AllowAutoTopicCreation: false,
	}
	return &Store{w: w}, nil
}

// Close flushes and closes the underlying writer.
func (s *Store) Close() error { return s.w.Close() }

// InsertRequestLogs publishes each record as a JSON message. The whole batch is
// sent in one WriteMessages call so the writer can group it efficiently.
func (s *Store) InsertRequestLogs(ctx context.Context, recs []attribution.Record) error {
	if len(recs) == 0 {
		return nil
	}
	msgs := make([]kafka.Message, 0, len(recs))
	for _, r := range recs {
		value, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("marshal record: %w", err)
		}
		msgs = append(msgs, kafka.Message{
			Key:   []byte(r.KeyID),
			Value: value,
		})
	}
	if err := s.w.WriteMessages(ctx, msgs...); err != nil {
		return fmt.Errorf("write messages: %w", err)
	}
	return nil
}

var _ attribution.Sink = (*Store)(nil)
