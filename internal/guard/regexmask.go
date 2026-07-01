package guard

import (
	"context"
	"regexp"
)

// defaultMaskToken replaces matched sensitive substrings. It is safe to place
// inside a JSON string value, which is where these patterns match, so the
// rewritten body stays valid JSON.
const defaultMaskToken = "[REDACTED]"

// maskPattern is a named detector: a compiled regular expression whose matches
// are replaced with the mask token.
type maskPattern struct {
	name string
	re   *regexp.Regexp
}

// RegexMasker is a reference guard that masks common sensitive tokens (emails,
// secret API keys, credit-card and US Social Security numbers) in the request
// body. It is deliberately simple: it operates on the raw body bytes and is a
// starting point, not a substitute for a dedicated PII or secret scanner.
type RegexMasker struct {
	patterns []maskPattern
	token    string
	category string
}

// defaultMaskPatterns are conservative detectors that match inside JSON string
// values. They favor precision (few false positives) over exhaustive recall.
var defaultMaskPatterns = []maskPattern{
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	{"secret_key", regexp.MustCompile(`\b(?:sk|pk|rk)-[A-Za-z0-9]{16,}\b`)},
	{"aws_access_key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"credit_card", regexp.MustCompile(`\b\d{4}[ -]?\d{4}[ -]?\d{4}[ -]?\d{4}\b`)},
	{"ssn", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
}

// NewRegexMasker returns a masker with the default pattern set.
func NewRegexMasker() *RegexMasker {
	return &RegexMasker{
		patterns: defaultMaskPatterns,
		token:    defaultMaskToken,
		category: "pii",
	}
}

// Inspect masks any matched sensitive tokens in the body. When nothing matches
// it allows the request unchanged; otherwise it returns Mask with the rewritten
// body. It never blocks.
func (m *RegexMasker) Inspect(_ context.Context, req Request) Decision {
	masked := req.Body
	hit := false
	for _, p := range m.patterns {
		if p.re.Match(masked) {
			masked = p.re.ReplaceAll(masked, []byte(m.token))
			hit = true
		}
	}
	if !hit {
		return Decision{Action: Allow}
	}
	return Decision{
		Action:   Mask,
		Category: m.category,
		Reason:   "masked sensitive tokens in request",
		Rewrite:  masked,
	}
}

var _ Guard = (*RegexMasker)(nil)
