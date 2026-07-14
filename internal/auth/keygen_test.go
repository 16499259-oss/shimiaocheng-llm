// Package auth_test contains tests for sub-key generation and hashing.
package auth_test

import (
	"strings"
	"testing"

	"llm_api_gateway/internal/auth"
)

// TestGenerateSubKey_Format verifies every generated sub-key has the required
// "sk-" prefix and the expected length ("sk-" + 32 hex chars = 35 bytes).
func TestGenerateSubKey_Format(t *testing.T) {
	key := auth.GenerateSubKey("test-salt", 42)
	if !strings.HasPrefix(key, auth.SubKeyPrefix) {
		t.Fatalf("expected prefix %q, got %q", auth.SubKeyPrefix, key)
	}
	// "sk-" (3) + 32 hex chars.
	if len(key) != 3+auth.SubKeyLength {
		t.Fatalf("expected length %d, got %d (%q)", 3+auth.SubKeyLength, len(key), key)
	}
}

// TestHashSubKey_Deterministic verifies the storage hash is stable for the same
// input and differs for different inputs (so lookups match and collisions are
// impossible to confuse).
func TestHashSubKey_Deterministic(t *testing.T) {
	a := auth.HashSubKey("sk-some-real-key-1234567890abcdef")
	b := auth.HashSubKey("sk-some-real-key-1234567890abcdef")
	if a != b {
		t.Fatalf("HashSubKey must be deterministic: %q != %q", a, b)
	}
	c := auth.HashSubKey("sk-a-different-key-1234567890abcdef")
	if a == c {
		t.Fatalf("different sub-keys must hash differently")
	}
	if len(a) != 64 {
		t.Fatalf("expected 64-char SHA256 hex, got %d chars (%q)", len(a), a)
	}
}

// TestGenerateSubKey_UniqueAcrossInputs verifies that distinct context
// combinations produce distinct keys. GenerateSubKey mixes a random 32-byte
// source in, so every call yields a fresh key; collisions across different
// (salt, userID) inputs are cryptographically negligible.
func TestGenerateSubKey_UniqueAcrossInputs(t *testing.T) {
	k1 := auth.GenerateSubKey("salt", 1)
	k2 := auth.GenerateSubKey("salt", 2)
	k3 := auth.GenerateSubKey("other-salt", 1)

	if k1 == k2 {
		t.Fatalf("keys for different userIDs must differ")
	}
	if k1 == k3 {
		t.Fatalf("keys for different salts must differ")
	}
	// A regenerated key for the same inputs must also differ (random nonce),
	// which is the desired behavior for "regenerate key" (old key is revoked).
	k4 := auth.GenerateSubKey("salt", 1)
	if k1 == k4 {
		t.Fatalf("regenerated key must differ from previous (random source)")
	}
}

// TestSubKeyPreview verifies the display truncation: long keys become
// "sk-xxxx..." and short keys are returned unchanged.
func TestSubKeyPreview(t *testing.T) {
	long := "sk-3f8a2b1c4d5e6f7a8b9c0d1e2f3a4b5c"
	got := auth.SubKeyPreview(long)
	// SubKeyPreview returns the first 8 chars + "..." (matches the doc comment).
	if got != "sk-3f8a2..." {
		t.Fatalf("expected 'sk-3f8a2...', got %q", got)
	}

	short := "sk-ab"
	if auth.SubKeyPreview(short) != short {
		t.Fatalf("short key should be returned unchanged, got %q", auth.SubKeyPreview(short))
	}
}
