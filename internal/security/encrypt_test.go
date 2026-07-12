package security

import (
	"crypto/sha256"
	"os"
	"testing"
)

func TestDeriveKEK(t *testing.T) {
	os.Setenv("GATEWAY_KEK_ENV", "test-secret-key-123")
	defer os.Unsetenv("GATEWAY_KEK_ENV")

	kek, err := DeriveKEK()
	if err != nil {
		t.Fatalf("DeriveKEK failed: %v", err)
	}
	if len(kek) != 32 {
		t.Errorf("expected 32-byte key, got %d bytes", len(kek))
	}

	// Ensure deterministic: same input → same output.
	kek2, err := DeriveKEK()
	if err != nil {
		t.Fatalf("second DeriveKEK failed: %v", err)
	}
	if string(kek) != string(kek2) {
		t.Error("DeriveKEK should be deterministic (SHA-256)")
	}
}

func TestDeriveKEK_Missing(t *testing.T) {
	os.Unsetenv("GATEWAY_KEK_ENV")
	_, err := DeriveKEK()
	if err == nil {
		t.Error("expected error when GATEWAY_KEK_ENV is not set")
	}
}

// deriveKEKFromValue derives a 32-byte key from a raw string using SHA-256 (for testing).
func deriveKEKFromValue(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	kek := deriveKEKFromValue("my-strong-encryption-key-32b!")

	tests := []string{
		"sk-abc123def456",
		"",
		"a",
		"a very long API key that goes on and on sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		"🔑 unicode key with emoji",
	}

	for _, plaintext := range tests {
		ciphertext, err := Encrypt(plaintext, kek)
		if err != nil {
			t.Fatalf("Encrypt(%q) failed: %v", plaintext, err)
		}

		if len(ciphertext) < 12+16 { // nonce + at least tag
			t.Errorf("ciphertext too short for %q: %d bytes", plaintext, len(ciphertext))
		}

		decrypted, err := Decrypt(ciphertext, kek)
		if err != nil {
			t.Fatalf("Decrypt failed for %q: %v", plaintext, err)
		}

		if decrypted != plaintext {
			t.Errorf("round-trip mismatch: got %q, want %q", decrypted, plaintext)
		}
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	kek1 := deriveKEKFromValue("key-one-xxxxxxxxxxxxxxx!")
	kek2 := deriveKEKFromValue("key-two-xxxxxxxxxxxxxxx!")

	ciphertext, err := Encrypt("secret-api-key", kek1)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	_, err = Decrypt(ciphertext, kek2)
	if err == nil {
		t.Error("expected decryption failure with wrong key")
	}
}

func TestDecrypt_CorruptedData(t *testing.T) {
	kek := deriveKEKFromValue("test-key-xxxxxxxxxxxxxxxxx!")

	ciphertext, err := Encrypt("my-secret", kek)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	// Corrupt the ciphertext
	if len(ciphertext) > 13 {
		ciphertext[13] ^= 0xFF
	}

	_, err = Decrypt(ciphertext, kek)
	if err == nil {
		t.Error("expected decryption failure with corrupted ciphertext")
	}
}

func TestMaskKey(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"sk-abc123def456ghijklmn", "sk-a***************klmn"},
		{"short", "sh**rt"},
		{"1234", "12**34"},
		{"12345", "12**45"},
		{"12345678", "12****78"},
		{"123456789", "1234*6789"},
		{"abcdefghijklmnop", "abcd********mnop"},
		{"", "****"},
		{"a", "****"},
		{"ab", "****"},
		{"abc", "****"},
	}

	for _, tt := range tests {
		result := MaskKey(tt.input)
		if result != tt.expected {
			t.Errorf("MaskKey(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}
