package models

import (
	"database/sql"
	"strings"
	"time"

	"llm_api_gateway/internal/timeutil"
)

// CallStats is the aggregated summary for a filtered set of call logs.
// The summary is always computed over the FULL filtered set (ignoring
// pagination), so it stays consistent with the list view's filtering.
type CallStats struct {
	TotalCalls     int              `json:"total_calls"`
	Tokens         TokenBreakdown   `json:"tokens"`
	EffectiveCalls int              `json:"effective_calls"`
	Success        SuccessStats     `json:"success"`
	ByModel        []ModelBreakdown `json:"by_model"`
}

// TokenBreakdown holds prompt/completion/total token sums.
type TokenBreakdown struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// SuccessStats holds success/error counts and the success rate (percent).
// A request is "successful" when 200 <= status_code < 300; "error" when
// status_code >= 400. success_rate = okSuccess / total * 100.
type SuccessStats struct {
	SuccessCount int     `json:"success_count"`
	ErrorCount   int     `json:"error_count"`
	SuccessRate  float64 `json:"success_rate"`
}

// ModelBreakdown holds per-model call count and token usage within the
// filtered result set. It is returned as part of CallStats.ByModel so
// the frontend can render a model-level breakdown below the summary cards.
type ModelBreakdown struct {
	Model  string         `json:"model"`
	Calls  int            `json:"calls"`
	Tokens TokenBreakdown `json:"tokens"`
}

// QueryCallLogsGlobal returns a paginated, global (multi-user) list of call
// logs, honoring every filter field. Zero-valued filter fields are skipped,
// so an empty filter means "all users / all time / all providers / all models".
// Rows are ordered by id DESC so the newest calls appear first.
func QueryCallLogsGlobal(db *sql.DB, filter CallLogFilter) (*CallLogPage, error) {
	if filter.Limit <= 0 {
		filter.Limit = 20
	}
	if filter.Page <= 0 {
		filter.Page = 1
	}
	offset := (filter.Page - 1) * filter.Limit

	where, args := buildCallLogWhere(filter)

	var total int
	countQuery := `SELECT COUNT(*) FROM call_logs WHERE ` + where
	if err := db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, err
	}

	dataQuery := `SELECT id, user_id, model, provider_id, prompt_tokens, completion_tokens, total_tokens,
		effective_calls, multiplier_used, status_code, latency_ms,
		COALESCE(error_msg, ''), created_at
		FROM call_logs WHERE ` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	dataArgs := append(args, filter.Limit, offset)

	rows, err := db.Query(dataQuery, dataArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	logs := make([]CallLog, 0, filter.Limit)
	for rows.Next() {
		var l CallLog
		if err := rows.Scan(
			&l.ID, &l.UserID, &l.Model, &l.ProviderID, &l.PromptTokens, &l.CompletionTokens,
			&l.TotalTokens, &l.EffectiveCalls, &l.MultiplierUsed,
			&l.StatusCode, &l.LatencyMs, &l.ErrorMsg, &l.CreatedAt,
		); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &CallLogPage{
		Data: logs,
		Pagination: Pagination{
			Page:  filter.Page,
			Limit: filter.Limit,
			Total: total,
		},
	}, nil
}

// AggregateCallStats computes the summary metrics for the filtered set,
// ignoring page/limit. It runs a single aggregate SQL over COUNT/SUM with
// CASE WHEN expressions so it stays fast even on large tables.
func AggregateCallStats(db *sql.DB, filter CallLogFilter) (*CallStats, error) {
	where, args := buildCallLogWhere(filter)

	q := `SELECT
		COUNT(*),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(effective_calls), 0),
		COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0)
	FROM call_logs WHERE ` + where

	var s CallStats
	var total, okSuccess, errCount int64
	if err := db.QueryRow(q, args...).Scan(
		&total,
		&s.Tokens.Prompt, &s.Tokens.Completion, &s.Tokens.Total,
		&s.EffectiveCalls, &okSuccess, &errCount,
	); err != nil {
		return nil, err
	}

	s.TotalCalls = int(total)
	s.Success.SuccessCount = int(okSuccess)
	s.Success.ErrorCount = int(errCount)
	if total > 0 {
		s.Success.SuccessRate = float64(okSuccess) / float64(total) * 100.0
	}

	// Per-model breakdown: reuse the same WHERE clause so the filter is
	// consistent with the aggregate above. Ordered by call count DESC so
	// the most-used models appear first in the frontend table.
	where2, args2 := buildCallLogWhere(filter)
	bmQuery := `SELECT LOWER(model) AS model, COUNT(*),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(total_tokens), 0)
	FROM call_logs WHERE ` + where2 + ` GROUP BY LOWER(model) ORDER BY COUNT(*) DESC`

	bmRows, err := db.Query(bmQuery, args2...)
	if err != nil {
		return nil, err
	}
	defer bmRows.Close()

	s.ByModel = []ModelBreakdown{} // ensure [] not null in JSON
	for bmRows.Next() {
		var bm ModelBreakdown
		if err := bmRows.Scan(
			&bm.Model,
			&bm.Calls,
			&bm.Tokens.Prompt,
			&bm.Tokens.Completion,
			&bm.Tokens.Total,
		); err != nil {
			return nil, err
		}
		s.ByModel = append(s.ByModel, bm)
	}
	if err := bmRows.Err(); err != nil {
		return nil, err
	}

	return &s, nil
}

// DistinctModels returns the sorted, distinct (non-empty) model names that
// appear in call_logs. It is used to populate the model filter dropdown.
//
// Model names are normalized via LOWER(model) so the dropdown options match
// the case-insensitive filtering in buildCallLogWhere (LOWER(model) = LOWER(?))
// and the case-insensitive aggregation in AggregateCallStats (GROUP BY
// LOWER(model)). This guarantees the dropdown, the list/model filter, and the
// by_model summary all use the exact same canonical (lowercased) key.
//
// The query has no user input, so it is inherently injection-safe.
func DistinctModels(db *sql.DB) ([]string, error) {
	// Normalize to LOWER(model) and exclude empty strings directly in SQL so
	// the returned set is already de-duplicated and canonicalized.
	rows, err := db.Query(`SELECT DISTINCT LOWER(model) FROM call_logs WHERE model != '' ORDER BY LOWER(model)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	models := []string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		if m != "" {
			models = append(models, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return models, nil
}

// buildCallLogWhere builds the WHERE clause for call-log queries.
//
// SECURITY: column names are hard-coded literals; every user-supplied value is
// bound via a "?" placeholder. No user input is ever concatenated into the SQL
// text. Zero-valued filter fields are skipped so an empty filter means "all".
func buildCallLogWhere(f CallLogFilter) (string, []any) {
	conds := []string{}
	args := []any{}
	if f.UserID != 0 {
		conds = append(conds, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.ProviderID != "" {
		conds = append(conds, "provider_id = ?")
		args = append(args, f.ProviderID)
	}
	if f.Model != "" {
		// Case-insensitive model match so it agrees with DistinctModels
		// (LOWER(model)) and AggregateCallStats (GROUP BY LOWER(model)).
		// The value is passed as-is and normalized by the DB via LOWER(?),
		// so "GLM-5.2" matches a stored "glm-5.2" row and vice versa.
		conds = append(conds, "LOWER(model) = LOWER(?)")
		args = append(args, f.Model)
	}
	if f.From != "" {
		conds = append(conds, "created_at >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "created_at <= ?")
		args = append(args, f.To)
	}
	if len(conds) == 0 {
		// 1=1 keeps the surrounding "WHERE " syntactically valid for "all".
		return "1=1", args
	}
	return strings.Join(conds, " AND "), args
}

// NormalizeToShanghaiRFC3339 normalizes an arbitrary RFC3339 or plain-date
// string into an Asia/Shanghai (+08:00) RFC3339 string, so it can be compared
// directly (as text) against the stored created_at column — which is likewise
// written in +08:00 RFC3339 (see InsertCallLog). Parsing failures yield "" so
// the corresponding boundary is simply omitted (treated as "no bound").
//
// A bare date "2006-01-02" is interpreted as midnight of that calendar day in
// Asia/Shanghai (not UTC), matching the "from = start-of-day SH" semantics
// used by the call-stats panel.
//
// This helper is exported so the admin handler can normalize query boundaries
// at the API edge before they reach the models layer.
func NormalizeToShanghaiRFC3339(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(timeutil.ShanghaiTZ).Format(time.RFC3339)
	}
	if d, err := time.Parse("2006-01-02", s); err == nil {
		// Midnight of that calendar day, interpreted in Asia/Shanghai.
		local := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, timeutil.ShanghaiTZ)
		return local.Format(time.RFC3339)
	}
	return ""
}
