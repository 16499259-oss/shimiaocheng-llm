// Package proxy contains tests for the proxy package's pure (DB-free) helpers.
package proxy

import (
	"encoding/json"
	"testing"
)

// TestNormalizeContentArrays_ArrayToText verifies that an array-of-parts content
// field is converted into a plain string so the upstream receives simple text.
func TestNormalizeContentArrays_ArrayToText(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	out := normalizeContentArrays(body)

	var parsed struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("failed to unmarshal normalized body: %v", err)
	}
	if len(parsed.Messages) != 1 {
		t.Fatalf("expected exactly 1 message, got %d", len(parsed.Messages))
	}

	// The content must now be a JSON string, not an array.
	var asString string
	if err := json.Unmarshal(parsed.Messages[0].Content, &asString); err != nil {
		t.Fatalf("content should be a string after normalize, got raw: %s", parsed.Messages[0].Content)
	}
	if asString != "hello" {
		t.Fatalf("expected content string %q, got %q", "hello", asString)
	}

	// Ensure it is NOT still an array.
	var asArray []any
	if err := json.Unmarshal(parsed.Messages[0].Content, &asArray); err == nil {
		t.Fatalf("content should no longer be an array, got: %v", asArray)
	}
}

// TestNormalizeContentArrays_StringUnchanged verifies that a plain string content
// is left untouched (it must remain a string, not be wrapped into an array).
func TestNormalizeContentArrays_StringUnchanged(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	out := normalizeContentArrays(body)

	var parsed struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("failed to unmarshal normalized body: %v", err)
	}
	if len(parsed.Messages) != 1 {
		t.Fatalf("expected exactly 1 message, got %d", len(parsed.Messages))
	}

	var asString string
	if err := json.Unmarshal(parsed.Messages[0].Content, &asString); err != nil {
		t.Fatalf("content should remain a string, got raw: %s", parsed.Messages[0].Content)
	}
	if asString != "hi" {
		t.Fatalf("expected content %q, got %q", "hi", asString)
	}
}

// TestChatMessage_ContentText_Array verifies ContentText joins the text parts of a
// multimodal (array) content field with newlines.
func TestChatMessage_ContentText_Array(t *testing.T) {
	msg := ChatMessage{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"text","text":"hello"},{"type":"text","text":"world"}]`),
	}

	got := msg.ContentText()
	want := "hello\nworld"
	if got != want {
		t.Fatalf("ContentText() = %q, want %q", got, want)
	}
}

// TestChatMessage_ContentText_String verifies ContentText returns the raw text for a
// plain string content field.
func TestChatMessage_ContentText_String(t *testing.T) {
	msg := ChatMessage{
		Role:    "user",
		Content: json.RawMessage(`"hi"`),
	}

	got := msg.ContentText()
	want := "hi"
	if got != want {
		t.Fatalf("ContentText() = %q, want %q", got, want)
	}
}
