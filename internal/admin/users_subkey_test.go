package admin

import (
	"testing"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
)

// P2-8/P2-9 regression: a sub-key created by CreateUser must be bound to the
// REAL user.ID (not the old placeholder 0) and must authenticate as that user.
//
// The auth middleware resolves a sub-key via
//   GetUserBySubKeyHash(DB, HashSubKey(providedKey))
// (see internal/auth/middleware.go). So the critical invariant is:
//   HashSubKey(returned sub_key) == stored SubKeyHash
// and that lookup resolves back to the same user.ID. GenerateSubKey mixes a
// random nonce, so we NEVER compare the plaintext key to a freshly generated
// one — only the stored-hash invariant and the end-to-end resolution.

func TestAdminCreateUser_SubKeyAuthenticatesAsCreatedUser(t *testing.T) {
	h := newAdminTestHandler(t)

	id, resp := adminCreateUser(t, h, "sk-auth", nil)

	subKey, ok := resp["sub_key"].(string)
	if !ok || subKey == "" {
		t.Fatalf("CreateUser response missing sub_key: %v", resp)
	}
	if len(subKey) < 3 || subKey[:3] != auth.SubKeyPrefix {
		t.Fatalf("returned sub_key has wrong format: %q", subKey)
	}

	// Stored hash must equal the hash of the returned plaintext key. This is
	// exactly what the auth middleware computes to look the user up, so it
	// proves the generated sub-key is usable for login.
	u, err := models.GetUserByID(h.DB, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.SubKeyHash != auth.HashSubKey(subKey) {
		t.Fatalf("stored sub_key_hash %q != hash of returned sub_key (auth would fail)", u.SubKeyHash)
	}
	if u.SubKeyPreview == "" {
		t.Fatalf("SubKeyPreview must be populated after CreateUser")
	}
	if u.SubKeyPreview != auth.SubKeyPreview(subKey) {
		t.Fatalf("SubKeyPreview %q != preview of returned sub_key %q", u.SubKeyPreview, auth.SubKeyPreview(subKey))
	}

	// End-to-end: the middleware's lookup by the returned key resolves to the
	// same user.ID — i.e. sub_key_hash is correctly associated with user.ID.
	resolved, err := models.GetUserBySubKeyHash(h.DB, auth.HashSubKey(subKey))
	if err != nil {
		t.Fatalf("GetUserBySubKeyHash: %v", err)
	}
	if resolved == nil {
		t.Fatalf("sub_key did not resolve to any user (auth would 401)")
	}
	if resolved.ID != id {
		t.Fatalf("sub_key resolved to user %d, want %d (sub_key_hash bound to wrong user)", resolved.ID, id)
	}
}

// P2-8/P2-9 regression: two distinct users must get distinct, correctly-bound
// sub-keys — A's key must NOT authenticate as B (proves per-user binding, the
// exact defect class the placeholder-0 generation had).
func TestAdminCreateUser_SubKeysAreUniqueAndPerUser(t *testing.T) {
	h := newAdminTestHandler(t)

	idA, respA := adminCreateUser(t, h, "sk-A", nil)
	idB, respB := adminCreateUser(t, h, "sk-B", nil)
	if idA == idB {
		t.Fatalf("test setup error: two users share an id")
	}
	keyA, _ := respA["sub_key"].(string)
	keyB, _ := respB["sub_key"].(string)
	if keyA == "" || keyB == "" {
		t.Fatalf("both sub_keys must be non-empty: %q / %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("two users must not share a sub_key")
	}

	resolvedA, err := models.GetUserBySubKeyHash(h.DB, auth.HashSubKey(keyA))
	if err != nil {
		t.Fatalf("GetUserBySubKeyHash(A): %v", err)
	}
	resolvedB, err := models.GetUserBySubKeyHash(h.DB, auth.HashSubKey(keyB))
	if err != nil {
		t.Fatalf("GetUserBySubKeyHash(B): %v", err)
	}
	if resolvedA == nil || resolvedA.ID != idA {
		t.Fatalf("A's sub_key did not resolve to A (idA=%d, got=%v)", idA, resolvedA)
	}
	if resolvedB == nil || resolvedB.ID != idB {
		t.Fatalf("B's sub_key did not resolve to B (idB=%d, got=%v)", idB, resolvedB)
	}
}
