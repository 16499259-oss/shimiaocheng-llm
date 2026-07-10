// Package auth provides sub-key generation and authentication middleware.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	// SubKeyPrefix is the prefix for all generated sub-keys.
	SubKeyPrefix = "sk-"
	// SubKeyLength is the length of the hex-encoded sub-key (after prefix).
	SubKeyLength = 32
)

// GenerateSubKey generates a new unique sub-key with the given salt and user ID.
// The key is "sk-" + hex(sha256(salt + userID + timestamp + random))[:32].
func GenerateSubKey(salt string, userID int64) string {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		// Fallback: should never happen, but handle gracefully
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}

	input := fmt.Sprintf("%s:%d:%s", salt, userID, hex.EncodeToString(randomBytes))
	hash := sha256.Sum256([]byte(input))
	return SubKeyPrefix + hex.EncodeToString(hash[:])[:SubKeyLength]
}

// HashSubKey returns the SHA256 hex hash of a sub-key for storage/comparison.
func HashSubKey(subKey string) string {
	hash := sha256.Sum256([]byte(subKey))
	return hex.EncodeToString(hash[:])
}

// GenerateSessionToken generates a random session token for admin cookies.
func GenerateSessionToken() string {
	randomBytes := make([]byte, 64)
	if _, err := rand.Read(randomBytes); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(randomBytes)
}

// SubKeyPreview returns a truncated preview of a sub-key for display.
// Example: "sk-3f8a2..." from "sk-3f8a2b1c4d5e6f7a8b9c0d1e2f3a4b5c".
func SubKeyPreview(subKey string) string {
	if len(subKey) <= 9 {
		return subKey
	}
	return subKey[:8] + "..."
}
