// Package proxy contains tests for the proxy package's pure (DB-free) helpers.
package proxy

import (
	"encoding/json"
	"testing"
)

// TestRewriteBodyModelRoute verifies that rewriteBodyModel swaps the "model"
// field while preserving every other field (including already-normalized
// messages and the stream flag). This is the core of per-provider model rewrite.
func TestRewriteBodyModelRoute(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false,"temperature":0.7}`)

	out := rewriteBodyModel(body, "gpt-4o")

	var parsed struct {
		Model       string  `json:"model"`
		Stream      bool    `json:"stream"`
		Temperature float64 `json:"temperature"`
		Messages    []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("failed to unmarshal rewritten body: %v", err)
	}
	if parsed.Model != "gpt-4o" {
		t.Fatalf("expected model rewritten to gpt-4o, got %q", parsed.Model)
	}
	if parsed.Stream != false {
		t.Fatalf("expected stream flag preserved (false)")
	}
	if parsed.Temperature != 0.7 {
		t.Fatalf("expected temperature preserved (0.7), got %v", parsed.Temperature)
	}
	if len(parsed.Messages) != 1 || parsed.Messages[0].Content != "hi" {
		t.Fatalf("expected messages preserved, got %+v", parsed.Messages)
	}
}

// TestRewriteBodyModelRoute_InvalidBody verifies that an unparseable body is
// returned unchanged rather than panicking or producing garbage.
func TestRewriteBodyModelRoute_InvalidBody(t *testing.T) {
	body := []byte(`not-json`)
	out := rewriteBodyModel(body, "gpt-4o")
	if string(out) != string(body) {
		t.Fatalf("expected invalid body unchanged, got %q", out)
	}
}
