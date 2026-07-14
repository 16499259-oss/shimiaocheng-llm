// Package models_test contains tests for the models package.
package models_test

import (
	"testing"

	"llm_api_gateway/internal/models"
)

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
