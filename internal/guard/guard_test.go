package guard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNopGuardAllows(t *testing.T) {
	d := NopGuard{}.Inspect(context.Background(), Request{Body: []byte(`{"x":"user@example.com"}`)})
	if d.Action != Allow {
		t.Errorf("NopGuard action = %v, want Allow", d.Action)
	}
}

func TestRegexMasker_MasksEachCategory(t *testing.T) {
	m := NewRegexMasker()
	tests := []struct {
		name    string
		content string
	}{
		{"email", "reach me at jane.doe@example.com please"},
		{"secret key", "key is sk-abcdef0123456789ABCDEF"},
		{"aws access key", "AKIAIOSFODNN7EXAMPLE in the log"},
		{"credit card", "card 4111 1111 1111 1111 expires soon"},
		{"ssn", "ssn 123-45-6789 on file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"prompt": tc.content})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			d := m.Inspect(context.Background(), Request{Body: body})
			if d.Action != Mask {
				t.Fatalf("action = %v, want Mask for %q", d.Action, tc.content)
			}
			if strings.Contains(string(d.Rewrite), sensitiveToken(tc.content)) {
				t.Errorf("rewrite still contains the sensitive token: %s", d.Rewrite)
			}
			if !strings.Contains(string(d.Rewrite), defaultMaskToken) {
				t.Errorf("rewrite missing mask token: %s", d.Rewrite)
			}
			// The rewritten body must remain valid JSON.
			var out map[string]string
			if err := json.Unmarshal(d.Rewrite, &out); err != nil {
				t.Errorf("rewritten body is not valid JSON: %v (%s)", err, d.Rewrite)
			}
			if d.Category != "pii" {
				t.Errorf("category = %q, want pii", d.Category)
			}
		})
	}
}

func TestRegexMasker_CleanBodyPassesThrough(t *testing.T) {
	m := NewRegexMasker()
	body := []byte(`{"model":"default","messages":[{"role":"user","content":"hello world"}]}`)
	d := m.Inspect(context.Background(), Request{Body: body})
	if d.Action != Allow {
		t.Errorf("action = %v, want Allow for clean body", d.Action)
	}
	if d.Rewrite != nil {
		t.Errorf("clean body should not be rewritten, got %s", d.Rewrite)
	}
}

func TestRegexMasker_MasksMultipleInOneBody(t *testing.T) {
	m := NewRegexMasker()
	body := []byte(`{"a":"user@example.com","b":"123-45-6789"}`)
	d := m.Inspect(context.Background(), Request{Body: body})
	if d.Action != Mask {
		t.Fatalf("action = %v, want Mask", d.Action)
	}
	s := string(d.Rewrite)
	if strings.Contains(s, "user@example.com") || strings.Contains(s, "123-45-6789") {
		t.Errorf("rewrite still contains a sensitive token: %s", s)
	}
}

// sensitiveToken extracts the specific substring expected to be masked from a
// test content string, for the "still contains" assertion.
func sensitiveToken(content string) string {
	for _, tok := range []string{
		"jane.doe@example.com",
		"sk-abcdef0123456789ABCDEF",
		"AKIAIOSFODNN7EXAMPLE",
		"4111 1111 1111 1111",
		"123-45-6789",
	} {
		if strings.Contains(content, tok) {
			return tok
		}
	}
	return content
}
