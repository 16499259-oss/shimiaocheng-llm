// Package models_test contains tests for the models package.
package models_test

import (
	"strings"
	"testing"

	"llm_api_gateway/internal/models"
)

// TestAggregateCallStats_ByModelMergeCaseInsensitive verifies the recent
// GROUP BY LOWER(model) behavior: call logs whose model name differs only in
// case (e.g. "GLM-5.2" vs "glm-5.2") must be merged into a single by_model
// entry, with tokens and call counts summed. A distinct model must stay separate.
func TestAggregateCallStats_ByModelMergeCaseInsensitive(t *testing.T) {
	database := newModelsTestDB(t)

	// call_logs has a FK on user_id, so create a user to reference first.
	owner, err := models.CreateUser(
		database.Conn,
		"stats-owner", "pw-hash", "sub-hash-owner", "sk-owner...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0,
	)
	if err != nil {
		t.Fatalf("CreateUser(owner) failed: %v", err)
	}

	// Two case-variant logs of the same model (glm-5.2 / GLM-5.2).
	insert := func(model string, p, c, tot, status int) {
		if _, err := models.InsertCallLog(database.Conn, &models.CallLog{
			UserID:           owner.ID,
			Model:            model,
			ProviderID:       "zhipu",
			PromptTokens:     p,
			CompletionTokens: c,
			TotalTokens:      tot,
			EffectiveCalls:   1,
			StatusCode:       status,
		}); err != nil {
			t.Fatalf("InsertCallLog(%q) failed: %v", model, err)
		}
	}
	insert("GLM-5.2", 100, 200, 300, 200) // success
	insert("glm-5.2", 50, 150, 200, 200)  // success (case variant -> merge)
	insert("gpt-4o", 10, 20, 30, 500)     // distinct model, error

	stats, err := models.AggregateCallStats(database.Conn, models.CallLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCallStats returned error: %v", err)
	}

	// Totals across all 3 rows.
	if stats.TotalCalls != 3 {
		t.Fatalf("expected TotalCalls == 3, got %d", stats.TotalCalls)
	}
	if stats.Success.SuccessCount != 2 || stats.Success.ErrorCount != 1 {
		t.Fatalf("expected 2 successes / 1 error, got %d/%d",
			stats.Success.SuccessCount, stats.Success.ErrorCount)
	}
	if stats.Tokens.Total != 300+200+30 {
		t.Fatalf("expected summed total tokens == 530, got %d", stats.Tokens.Total)
	}

	// by_model must have exactly 2 entries: merged "glm-5.2" (calls=2) first,
	// then "gpt-4o" (calls=1).
	if len(stats.ByModel) != 2 {
		t.Fatalf("expected 2 by_model entries (merge), got %d: %+v", len(stats.ByModel), stats.ByModel)
	}
	merged := stats.ByModel[0]
	if merged.Model != "glm-5.2" {
		t.Fatalf("expected merged model key 'glm-5.2' (LOWER), got %q", merged.Model)
	}
	if merged.Calls != 2 {
		t.Fatalf("expected merged calls == 2, got %d", merged.Calls)
	}
	if merged.Tokens.Total != 300+200 {
		t.Fatalf("expected merged total tokens == 500, got %d", merged.Tokens.Total)
	}
	if merged.Tokens.Prompt != 100+50 || merged.Tokens.Completion != 200+150 {
		t.Fatalf("merged token breakdown mismatch: prompt=%d completion=%d",
			merged.Tokens.Prompt, merged.Tokens.Completion)
	}
	if stats.ByModel[1].Model != "gpt-4o" || stats.ByModel[1].Calls != 1 {
		t.Fatalf("expected second entry gpt-4o calls=1, got %+v", stats.ByModel[1])
	}
}

// TestDistinctModels_NormalizedLower verifies that DistinctModels returns
// model names normalized to lower case, collapsing case variants into a single
// canonical entry (no duplicate upper/lower keys). This keeps the dropdown
// options consistent with the LOWER(model)-based filtering and by_model summary.
func TestDistinctModels_NormalizedLower(t *testing.T) {
	database := newModelsTestDB(t)

	owner, err := models.CreateUser(
		database.Conn,
		"distinct-owner", "pw-hash", "sub-hash-owner", "sk-owner...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0,
	)
	if err != nil {
		t.Fatalf("CreateUser(owner) failed: %v", err)
	}

	// Insert mixed-case models: GLM-5.2 + glm-5.2 are the same model (case
	// only), Astron-Code is a distinct model.
	type row struct {
		model string
	}
	inserts := []row{
		{"GLM-5.2"},
		{"glm-5.2"},
		{"Astron-Code"},
		{"astron-code"},
		{""}, // empty model must be excluded from the result
	}
	for _, r := range inserts {
		if _, err := models.InsertCallLog(database.Conn, &models.CallLog{
			UserID:           owner.ID,
			Model:            r.model,
			ProviderID:       "zhipu",
			PromptTokens:     1,
			CompletionTokens: 1,
			TotalTokens:      2,
			EffectiveCalls:   1,
			StatusCode:       200,
		}); err != nil {
			t.Fatalf("InsertCallLog(%q) failed: %v", r.model, err)
		}
	}

	got, err := models.DistinctModels(database.Conn)
	if err != nil {
		t.Fatalf("DistinctModels returned error: %v", err)
	}

	// Expect exactly two canonical (lower-cased) entries, no duplicates, and
	// no empty-string entry.
	want := map[string]bool{
		"glm-5.2":     true,
		"astron-code": true,
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d normalized models, got %d: %+v", len(want), len(got), got)
	}
	for _, m := range got {
		if !want[m] {
			t.Fatalf("unexpected model in DistinctModels result: %q (full result: %+v)", m, got)
		}
		if m != strings.ToLower(m) {
			t.Fatalf("DistinctModels returned a non-lowercased value: %q", m)
		}
	}
}

// TestQueryCallLogsGlobal_ModelFilterCaseInsensitive verifies that the model
// filter built by buildCallLogWhere is case-insensitive: a filter value
// "GLM-5.2" must match a stored row whose model is "glm-5.2".
func TestQueryCallLogsGlobal_ModelFilterCaseInsensitive(t *testing.T) {
	database := newModelsTestDB(t)

	owner, err := models.CreateUser(
		database.Conn,
		"filter-owner", "pw-hash", "sub-hash-filter", "sk-filter...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0,
	)
	if err != nil {
		t.Fatalf("CreateUser(owner) failed: %v", err)
	}

	// A stored row uses the lower-case model name.
	if _, err := models.InsertCallLog(database.Conn, &models.CallLog{
		UserID:           owner.ID,
		Model:            "glm-5.2",
		ProviderID:       "zhipu",
		PromptTokens:     10,
		CompletionTokens: 20,
		TotalTokens:      30,
		EffectiveCalls:   1,
		StatusCode:       200,
	}); err != nil {
		t.Fatalf("InsertCallLog(glm-5.2) failed: %v", err)
	}

	// Filter by the upper-case variant: must still hit the lower-case row.
	page, err := models.QueryCallLogsGlobal(database.Conn, models.CallLogFilter{
		Model: "GLM-5.2",
	})
	if err != nil {
		t.Fatalf("QueryCallLogsGlobal returned error: %v", err)
	}
	if page == nil || page.Data == nil {
		t.Fatalf("expected non-nil page/data, got %+v", page)
	}
	if page.Pagination.Total != 1 {
		t.Fatalf("expected 1 matching call for case-insensitive model filter, got %d", page.Pagination.Total)
	}
	if len(page.Data) != 1 || page.Data[0].Model != "glm-5.2" {
		t.Fatalf("expected matched row with model 'glm-5.2', got %+v", page.Data)
	}
}

// TestAggregateCallStats_EmptyMatch verifies the empty-data behavior required by
// PRD P0-6: when the filter matches zero rows, AggregateCallStats must return a
// 200-style result (TotalCalls == 0) without erroring on the two CASE-WHEN SUM
// columns (which coalesce to 0 instead of NULL).
func TestAggregateCallStats_EmptyMatch(t *testing.T) {
	database := newModelsTestDB(t)

	stats, err := models.AggregateCallStats(database.Conn, models.CallLogFilter{
		Model: "__nonexistent__",
	})
	if err != nil {
		t.Fatalf("AggregateCallStats(empty match) returned error: %v", err)
	}
	if stats == nil {
		t.Fatalf("expected non-nil *CallStats for empty match")
	}
	if stats.TotalCalls != 0 {
		t.Fatalf("expected TotalCalls == 0 for empty match, got %d", stats.TotalCalls)
	}
	if stats.Success.SuccessCount != 0 {
		t.Fatalf("expected SuccessCount == 0, got %d", stats.Success.SuccessCount)
	}
	if stats.Success.ErrorCount != 0 {
		t.Fatalf("expected ErrorCount == 0, got %d", stats.Success.ErrorCount)
	}
}
