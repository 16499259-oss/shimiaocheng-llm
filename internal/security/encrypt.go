// Package security provides AES-256-GCM encryption/decryption for provider API keys
// and KEK (Key Encryption Key) derivation from environment variables.
//
// Encryption format: [12-byte random nonce][GCM ciphertext + 16-byte tag]
// The nonce is prepended to the ciphertext for storage as a single BLOB.
// Decryption extracts the nonce from the first 12 bytes, the rest is the ciphertext.
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

// DeriveKEK reads the GATEWAY_KEK_ENV environment variable and derives a 32-byte
// AES-256 key via SHA-256. Returns an error if the environment variable is not set.
func DeriveKEK() ([]byte, error) {
	raw := os.Getenv("GATEWAY_KEK_ENV")
	if raw == "" {
		return nil, fmt.Errorf("GATEWAY_KEK_ENV environment variable is not set")
	}
	h := sha256.Sum256([]byte(raw))
	return h[:], nil
}

// Encrypt encrypts a plaintext string using AES-256-GCM with the given 32-byte key.
// Returns the ciphertext as a byte slice with a 12-byte random nonce prepended.
// The format is: nonce(12) || encrypted_data || gcm_tag(16).
func Encrypt(plaintext string, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes for AES-256, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Generate a random 12-byte nonce (standard for GCM).
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Seal appends the encrypted data to the nonce, returning: nonce || ciphertext.
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return ciphertext, nil
}

// Decrypt decrypts a ciphertext (nonce-prefixed) using AES-256-GCM with the given
// 32-byte key. Returns the original plaintext string or an error.
func Decrypt(ciphertext []byte, key []byte) (string, error) {
	if len(key) != 32 {
		return "", fmt.Errorf("key must be 32 bytes for AES-256, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short: %d bytes, need at least %d", len(ciphertext), nonceSize)
	}

	nonce, cipherData := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, cipherData, nil)
	if err != nil {
		return "", fmt.Errorf("decryption failed: %w", err)
	}

	return string(plaintext), nil
}

// MaskKey masks an API key for frontend display, keeping the first 4 and last 4
// characters and replacing the middle with '*'. For keys shorter than 8 characters,
// the entire key is masked with a fixed number of asterisks.
func MaskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + stringsRepeat("*", len(key)-8) + key[len(key)-4:]
}

// stringsRepeat repeats a string n times. Avoids importing strings for a single use.
func stringsRepeat(s string, count int) string {
	if count <= 0 {
		return ""
	}
	result := make([]byte, 0, len(s)*count)
	for i := 0; i < count; i++ {
		result = append(result, s...)
	}
	return string(result)
}
