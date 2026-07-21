package models

import (
	"database/sql"
	"math"
	"sort"
	"strings"
	"time"

	"llm_api_gateway/internal/timeutil"
)

// CallStats is the aggregated summary for a filtered set of call logs.
// The summary is always computed over the FULL filtered set (ignoring
// pagination), so it stays consistent with the list view's filtering.
type CallStats struct {
	TotalCalls int            `json:"total_calls"`
	Tokens     TokenBreakdown `json:"tokens"`
	// RawTokens is the UNMULTIPLIED token breakdown for the same filtered set,
	// shown side-by-side with Tokens (multiplier-inflated) so admins can compare
	// raw consumption vs billed consumption. Every value is the verbatim per-row
	// raw figure (prompt_tokens / completion_tokens / raw_total_tokens), summed
	// WITHOUT applying any multiplier — the raw_total_tokens column is written at
	// call-log insert time, so this is never reverse-derived from multiplier_used.
	RawTokens      TokenBreakdown   `json:"raw_tokens"`
	EffectiveCalls int              `json:"effective_calls"`
	Success        SuccessStats     `json:"success"`
	ByModel        []ModelBreakdown `json:"by_model"`
	ByUser         []UserBreakdown  `json:"by_user"`
}

// TokenBreakdown holds prompt/completion/total token sums.
//
// IMPORTANT (口径一致性 / caliber consistency): these sums are the
// MULTIPLIER-INFLATED token consumption — every row's
// (prompt_tokens + completion_tokens) is scaled by that row's own
// multiplier_used and ceiled, exactly like the user-panel Token display
// (internal/handler/quota.sumMultipliedTokens) and the billed quota counters.
// The raw, unmultiplied audit values stored in call_logs are intentionally NOT
// summed here; see multipliedTokenBreakdown for the formula.
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

// UserBreakdown holds per-user call count and token usage within the
// filtered result set, rendered as the "按用户明细" tab in the admin UI.
type UserBreakdown struct {
	UserID         int            `json:"user_id"`
	Username       string         `json:"username"`
	Calls          int            `json:"calls"`
	EffectiveCalls int            `json:"effective_calls"`
	Tokens         TokenBreakdown `json:"tokens"`
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
		COALESCE(error_msg, ''), created_at,
		COALESCE((SELECT username FROM users WHERE users.id = call_logs.user_id), '') AS username
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
			&l.StatusCode, &l.LatencyMs, &l.ErrorMsg, &l.CreatedAt, &l.Username,
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
// ignoring page/limit.
//
// CALIBER CONSISTENCY (口径一致性): the TokenBreakdown fields (Prompt /
// Completion / Total) are the MULTIPLIER-INFLATED consumption. Each call-log
// row stores its own multiplier_used (the rate in effect at call time, default
// 1.0 for pre-multiplier history), and we apply the exact same per-row formula
// used by the quota/billing path and the user-panel Token display:
//
//	billed = ceil((prompt_tokens + completion_tokens) * multiplier_used)
//
// so the admin summary agrees with the billed quota counters and the user
// panel's "累计/今日 Token". The raw call_logs token columns are the audit
// (unmultiplied) values and are intentionally NOT summed here.
//
// SQLite (modernc.org/sqlite) does not guarantee a CEIL math function, so — as
// in quota.sumMultipliedTokens — we fetch the per-row components + multiplier
// and apply the ceil formula in Go, then bucket the multiplied sums for the
// per-model and per-user breakdowns.
func AggregateCallStats(db *sql.DB, filter CallLogFilter) (*CallStats, error) {
	where, args := buildCallLogWhere(filter)

	s := CallStats{}

	// --- (1) Main aggregate: token sums + success/error counts over the full
	// filtered set. Fetches per-row (prompt, completion, multiplier) so the
	// multiplied token totals can be computed in Go. ---
	mainQuery := `SELECT prompt_tokens, completion_tokens, raw_total_tokens, multiplier_used, effective_calls,
		CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END,
		CASE WHEN status_code >= 400 THEN 1 ELSE 0 END
		FROM call_logs WHERE ` + where

	mainRows, err := db.Query(mainQuery, args...)
	if err != nil {
		return nil, err
	}
	var total int64
	for mainRows.Next() {
		var p, c, raw, eff int
		var m float64
		var ok, er int
		if err := mainRows.Scan(&p, &c, &raw, &m, &eff, &ok, &er); err != nil {
			mainRows.Close()
			return nil, err
		}
		total++
		pb, cb, tb := multipliedTokenBreakdown(p, c, m)
		s.Tokens.Prompt += pb
		s.Tokens.Completion += cb
		s.Tokens.Total += tb
		// Raw (unmultiplied) breakdown — verbatim per-row raw values, no multiplier.
		s.RawTokens.Prompt += p
		s.RawTokens.Completion += c
		s.RawTokens.Total += raw
		s.EffectiveCalls += eff
		s.Success.SuccessCount += ok
		s.Success.ErrorCount += er
	}
	if err := mainRows.Err(); err != nil {
		mainRows.Close()
		return nil, err
	}
	mainRows.Close()

	s.TotalCalls = int(total)
	if total > 0 {
		s.Success.SuccessRate = float64(s.Success.SuccessCount) / float64(total) * 100.0
	}

	// --- (2) Per-model breakdown: same WHERE, GROUP BY LOWER(model). Multiply
	// each row's tokens by its own multiplier_used and bucket under the
	// case-normalized model key. Ordered by call count DESC so the most-used
	// models appear first in the frontend table. ---
	where2, args2 := buildCallLogWhere(filter)
	bmQuery := `SELECT LOWER(model) AS model, prompt_tokens, completion_tokens, multiplier_used
		FROM call_logs WHERE ` + where2

	bmRows, err := db.Query(bmQuery, args2...)
	if err != nil {
		return nil, err
	}
	bmMap := map[string]*ModelBreakdown{}
	for bmRows.Next() {
		var model string
		var p, c int
		var m float64
		if err := bmRows.Scan(&model, &p, &c, &m); err != nil {
			bmRows.Close()
			return nil, err
		}
		entry, ok := bmMap[model]
		if !ok {
			entry = &ModelBreakdown{Model: model}
			bmMap[model] = entry
		}
		entry.Calls++
		pb, cb, tb := multipliedTokenBreakdown(p, c, m)
		entry.Tokens.Prompt += pb
		entry.Tokens.Completion += cb
		entry.Tokens.Total += tb
	}
	if err := bmRows.Err(); err != nil {
		bmRows.Close()
		return nil, err
	}
	bmRows.Close()

	s.ByModel = []ModelBreakdown{} // ensure [] not null in JSON
	for _, entry := range bmMap {
		s.ByModel = append(s.ByModel, *entry)
	}
	// Stable sort by Calls DESC keeps output deterministic for equal counts.
	sort.SliceStable(s.ByModel, func(i, j int) bool {
		return s.ByModel[i].Calls > s.ByModel[j].Calls
	})

	// --- (3) Per-user breakdown: same WHERE, LEFT JOIN users for the display
	// name. Token sums are multiplier-inflated per row; effective_calls is the
	// call-count already inflated at log time (int(ceil(multiplier))) and is
	// summed verbatim. Ordered by call count DESC so the heaviest users lead. ---
	where3, args3 := buildCallLogWhere(filter)
	buQuery := `SELECT call_logs.user_id,
		COALESCE(users.username, '') AS username,
		prompt_tokens, completion_tokens, multiplier_used, effective_calls
		FROM call_logs
		LEFT JOIN users ON users.id = call_logs.user_id
		WHERE ` + where3

	buRows, err := db.Query(buQuery, args3...)
	if err != nil {
		return nil, err
	}
	buMap := map[int]*UserBreakdown{}
	for buRows.Next() {
		var userID int
		var username string
		var p, c, eff int
		var m float64
		if err := buRows.Scan(&userID, &username, &p, &c, &m, &eff); err != nil {
			buRows.Close()
			return nil, err
		}
		entry, ok := buMap[userID]
		if !ok {
			entry = &UserBreakdown{UserID: userID, Username: username}
			buMap[userID] = entry
		}
		entry.Calls++
		pb, cb, tb := multipliedTokenBreakdown(p, c, m)
		entry.Tokens.Prompt += pb
		entry.Tokens.Completion += cb
		entry.Tokens.Total += tb
		entry.EffectiveCalls += eff
	}
	if err := buRows.Err(); err != nil {
		buRows.Close()
		return nil, err
	}
	buRows.Close()

	s.ByUser = []UserBreakdown{} // ensure [] not null in JSON
	for _, entry := range buMap {
		s.ByUser = append(s.ByUser, *entry)
	}
	sort.SliceStable(s.ByUser, func(i, j int) bool {
		return s.ByUser[i].Calls > s.ByUser[j].Calls
	})

	return &s, nil
}

// multipliedTokenBreakdown applies the multiplier-inflated token formula used
// by the quota/billing path (see internal/handler/quota.sumMultipliedTokens) so
// the admin call-stats summary stays consistent with the user-panel Token
// display and the billed quota counters ("口径一致性").
//
//   - promptBilled     = ceil(prompt * m)
//   - totalBilled      = ceil((prompt + completion) * m)   // matches the billed value
//   - completionBilled = totalBilled - promptBilled         // keeps prompt + completion == total
//
// multiplier defaults to 1.0 when it is 0 (pre-multiplier history or rows that
// did not set a multiplier), reproducing the raw audit value exactly and
// avoiding a 0 token count for those rows.
func multipliedTokenBreakdown(prompt, completion int, multiplier float64) (promptBilled, completionBilled, totalBilled int) {
	if multiplier == 0 {
		multiplier = 1.0
	}
	promptBilled = int(math.Ceil(float64(prompt) * multiplier))
	totalBilled = int(math.Ceil(float64(prompt+completion) * multiplier))
	// completionBilled is derived so that, within every group, the per-row
	// (and therefore the summed) prompt + completion always equals total.
	// (totalBilled >= promptBilled because (prompt+completion) >= prompt and
	// multiplier >= 0, so completionBilled is never negative.)
	completionBilled = totalBilled - promptBilled
	return promptBilled, completionBilled, totalBilled
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
		// Qualify with the call_logs table alias so the predicate stays
		// unambiguous when the WHERE clause is reused by the by_user query,
		// which LEFT JOINs users (a table that also has a created_at column).
		// A bare "created_at" would otherwise trigger SQLite's
		// "ambiguous column name: created_at" error and fail the whole
		// AggregateCallStats call (HTTP 500 on /api/calls/stats). The prefix
		// is harmless for the non-JOIN call sites.
		conds = append(conds, "call_logs.created_at >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "call_logs.created_at <= ?")
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
