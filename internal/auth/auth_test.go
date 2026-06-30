package auth

import (
	"context"
	"strings"
	"testing"
)

func TestHashKeyDeterministic(t *testing.T) {
	h1 := HashKey("llmgw_abc")
	h2 := HashKey("llmgw_abc")
	if h1 != h2 {
		t.Fatal("HashKey not deterministic")
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(h1))
	}
	if HashKey("llmgw_abc") == HashKey("llmgw_abd") {
		t.Error("different inputs produced same hash")
	}
}

func TestGenerateKey(t *testing.T) {
	plaintext, hash := GenerateKey()
	if !strings.HasPrefix(plaintext, KeyPrefix) {
		t.Errorf("plaintext %q missing prefix %q", plaintext, KeyPrefix)
	}
	if hash != HashKey(plaintext) {
		t.Error("returned hash does not match HashKey(plaintext)")
	}
	other, _ := GenerateKey()
	if other == plaintext {
		t.Error("GenerateKey produced a duplicate key")
	}
}

func TestPrincipalContextRoundTrip(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Error("empty context should have no principal")
	}
	want := &Principal{KeyID: "k1", TeamID: "t1"}
	ctx := WithPrincipal(context.Background(), want)
	got, ok := FromContext(ctx)
	if !ok || got != want {
		t.Errorf("FromContext = %v, %v; want %v, true", got, ok, want)
	}
}
