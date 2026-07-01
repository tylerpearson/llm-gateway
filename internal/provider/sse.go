package provider

import "bytes"

// SSEPayloadScanner is an io.Writer that reassembles SSE lines from arbitrary
// chunk boundaries and invokes onPayload for each non-empty data payload,
// skipping the [DONE] sentinel. It owns the scaffolding shared by every
// provider's usage scanner: buffering partial writes, splitting on newlines,
// trimming CR line endings, and stripping the "data:" prefix. Provider
// specific code only needs to interpret the payload bytes.
type SSEPayloadScanner struct {
	pending   []byte
	onPayload func(payload []byte)
}

// NewSSEPayloadScanner returns a scanner that calls onPayload for each SSE
// data payload written to it.
func NewSSEPayloadScanner(onPayload func([]byte)) *SSEPayloadScanner {
	return &SSEPayloadScanner{onPayload: onPayload}
}

// Write feeds raw response bytes (arbitrary chunk boundaries) to the scanner.
func (s *SSEPayloadScanner) Write(p []byte) (int, error) {
	s.pending = append(s.pending, p...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		line := s.pending[:i]
		s.pending = s.pending[i+1:]
		s.processLine(line)
	}
	return len(p), nil
}

func (s *SSEPayloadScanner) processLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	payload := bytes.TrimSpace(line[len(prefix):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	s.onPayload(payload)
}
