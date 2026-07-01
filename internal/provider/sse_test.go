package provider

import "testing"

// TestSSEPayloadScanner_WholeWrite feeds a full multi-event SSE stream in one
// Write call and checks every non-empty, non-[DONE] payload is delivered.
func TestSSEPayloadScanner_WholeWrite(t *testing.T) {
	const stream = `event: message_start
data: {"a":1}

event: content_block_delta
data: {"b":2}

event: message_stop
data: {"c":3}

`
	var got []string
	s := NewSSEPayloadScanner(func(payload []byte) {
		got = append(got, string(payload))
	})
	if _, err := s.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}
	if len(got) != len(want) {
		t.Fatalf("payloads = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("payload %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSSEPayloadScanner_ChunkedWrites exercises partial-line buffering across
// Write calls by feeding the stream one byte at a time.
func TestSSEPayloadScanner_ChunkedWrites(t *testing.T) {
	const stream = "data: {\"a\":1}\n\ndata: {\"b\":2}\n\n"
	var got []string
	s := NewSSEPayloadScanner(func(payload []byte) {
		got = append(got, string(payload))
	})
	for i := 0; i < len(stream); i++ {
		if _, err := s.Write([]byte{stream[i]}); err != nil {
			t.Fatalf("Write byte %d: %v", i, err)
		}
	}
	want := []string{`{"a":1}`, `{"b":2}`}
	if len(got) != len(want) {
		t.Fatalf("payloads = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("payload %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSSEPayloadScanner_SplitAcrossWrites checks a single payload split
// midway across two Write calls is still reassembled correctly.
func TestSSEPayloadScanner_SplitAcrossWrites(t *testing.T) {
	const stream = "data: {\"prompt_tokens\":11,\"completion_tokens\":4}\n\n"
	var got []string
	s := NewSSEPayloadScanner(func(payload []byte) {
		got = append(got, string(payload))
	})
	mid := len(stream) / 2
	_, _ = s.Write([]byte(stream[:mid]))
	_, _ = s.Write([]byte(stream[mid:]))
	if len(got) != 1 || got[0] != `{"prompt_tokens":11,"completion_tokens":4}` {
		t.Errorf("payloads = %v", got)
	}
}

// TestSSEPayloadScanner_CRLF checks CRLF line endings are trimmed correctly.
func TestSSEPayloadScanner_CRLF(t *testing.T) {
	const stream = "data: {\"a\":1}\r\n\r\n"
	var got []string
	s := NewSSEPayloadScanner(func(payload []byte) {
		got = append(got, string(payload))
	})
	_, _ = s.Write([]byte(stream))
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Errorf("payloads = %v", got)
	}
}

// TestSSEPayloadScanner_IgnoresNonDataLines checks comment and event lines
// are ignored and never delivered as payloads.
func TestSSEPayloadScanner_IgnoresNonDataLines(t *testing.T) {
	const stream = "event: message_start\n: comment line\ndata: {\"a\":1}\n\n"
	var got []string
	s := NewSSEPayloadScanner(func(payload []byte) {
		got = append(got, string(payload))
	})
	_, _ = s.Write([]byte(stream))
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Errorf("payloads = %v", got)
	}
}

// TestSSEPayloadScanner_SkipsEmptyAndDone checks blank payloads and the
// [DONE] sentinel never reach onPayload.
func TestSSEPayloadScanner_SkipsEmptyAndDone(t *testing.T) {
	const stream = "data: \n\ndata: [DONE]\n\n"
	called := false
	s := NewSSEPayloadScanner(func(payload []byte) {
		called = true
	})
	_, _ = s.Write([]byte(stream))
	if called {
		t.Error("onPayload should not be called for empty or [DONE] payloads")
	}
}

// TestSSEPayloadScanner_MultipleEventsInOneChunk checks several events
// delivered in a single Write call are all reported.
func TestSSEPayloadScanner_MultipleEventsInOneChunk(t *testing.T) {
	const stream = "data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: {\"c\":3}\n\n"
	var got []string
	s := NewSSEPayloadScanner(func(payload []byte) {
		got = append(got, string(payload))
	})
	_, _ = s.Write([]byte(stream))
	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}
	if len(got) != len(want) {
		t.Fatalf("payloads = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("payload %d = %q, want %q", i, got[i], want[i])
		}
	}
}
