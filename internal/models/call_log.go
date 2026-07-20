package models

import (
	"database/sql"
	"fmt"
	"time"

	"llm_api_gateway/internal/timeutil"
)

// CallLog represents a single API call record.
type CallLog struct {
	ID               int64   `json:"id"`
	UserID           int64   `json:"user_id"`
	Username         string  `json:"username"` // display name of the calling user; populated by the global admin query
	Model            string  `json:"model"`
	ProviderID       string  `json:"provider_id"` // upstream provider that served the call ("zhipu"/"openai"/...)
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	// RawTotalTokens is the UNMULTIPLIED raw token total (prompt_tokens +
	// completion_tokens), recorded at insert time. It is the canonical "raw
	// token" figure surfaced by the call-records summary ("未含倍率") and is
	// intentionally independent of TotalTokens (provider-reported, may include
	// extras) and of any multiplier. Never reverse-derived from multiplier_used.
	RawTotalTokens   int     `json:"raw_total_tokens"`
	EffectiveCalls   int     `json:"effective_calls"`
	MultiplierUsed   float64 `json:"multiplier_used"`
	StatusCode       int     `json:"status_code"`
	LatencyMs        int     `json:"latency_ms"`
	ErrorMsg         string  `json:"error_msg,omitempty"`
	CreatedAt        string  `json:"created_at"`
}

// CallLogFilter holds pagination and filter options for querying call logs.
// Zero-valued fields are treated as "no filter" (see buildCallLogWhere).
type CallLogFilter struct {
	UserID     int64
	ProviderID string // upstream slug ("" = all providers)
	Model      string // real model name ("" = all models)
	From       string // created_at >= from (SH-normalized RFC3339)
	To         string // created_at <= to (SH-normalized RFC3339)
	Page       int
	Limit      int
}

// CallLogPage holds paginated call log results.
type CallLogPage struct {
	Data       []CallLog  `json:"data"`
	Pagination Pagination `json:"pagination"`
}

// Pagination holds page metadata.
type Pagination struct {
	Page  int `json:"page"`
	Limit int `json:"limit"`
	Total int `json:"total"`
}

// InsertCallLog inserts a new call log record.
func InsertCallLog(db *sql.DB, log *CallLog) (int64, error) {
	// D10: store created_at in Asia/Shanghai (+08:00) RFC3339 so it can be
	// compared directly (as text) against the SH-normalized query boundaries
	// produced by NormalizeToShanghaiRFC3339. This is robust regardless of the
	// server's local time zone (we never assume TZ). The call-stats panel and
	// existing views only read the instant, so this does not regress display.
	now := time.Now().In(timeutil.ShanghaiTZ).Format(time.RFC3339)
	// Safety default: an empty provider_id would break analytics; fall back to
	// "zhipu" (also the DB column default) so the row is always well-formed.
	providerID := log.ProviderID
	if providerID == "" {
		providerID = "zhipu"
	}
	result, err := db.Exec(
		`INSERT INTO call_logs (user_id, model, provider_id, prompt_tokens, completion_tokens, total_tokens,
		 raw_total_tokens, effective_calls, multiplier_used, status_code, latency_ms, error_msg, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		log.UserID, log.Model, providerID, log.PromptTokens, log.CompletionTokens,
		log.TotalTokens, log.PromptTokens+log.CompletionTokens, log.EffectiveCalls, log.MultiplierUsed,
		log.StatusCode, log.LatencyMs, log.ErrorMsg, now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert call log: %w", err)
	}
	return result.LastInsertId()
}

// QueryCallLogs returns paginated call logs for a user.
func QueryCallLogs(db *sql.DB, filter CallLogFilter) (*CallLogPage, error) {
	if filter.Limit <= 0 {
		filter.Limit = 20
	}
	if filter.Page <= 0 {
		filter.Page = 1
	}
	offset := (filter.Page - 1) * filter.Limit

	// Build query conditions
	where := "user_id = ?"
	args := []any{filter.UserID}

	if filter.From != "" {
		where += " AND created_at >= ?"
		args = append(args, filter.From)
	}
	if filter.To != "" {
		where += " AND created_at <= ?"
		args = append(args, filter.To)
	}

	// Count total
	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM call_logs WHERE %s", where)
	if err := db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count call logs: %w", err)
	}

	// Query data
	dataQuery := fmt.Sprintf(
		`SELECT id, user_id, model, provider_id, prompt_tokens, completion_tokens, total_tokens,
		 effective_calls, multiplier_used, status_code, latency_ms,
		 COALESCE(error_msg, ''), created_at
		 FROM call_logs WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?`, where,
	)
	dataArgs := append(args, filter.Limit, offset)

	rows, err := db.Query(dataQuery, dataArgs...)
	if err != nil {
		return nil, fmt.Errorf("query call logs: %w", err)
	}
	defer rows.Close()

	var logs []CallLog
	for rows.Next() {
		var l CallLog
		err := rows.Scan(
			&l.ID, &l.UserID, &l.Model, &l.ProviderID, &l.PromptTokens, &l.CompletionTokens,
			&l.TotalTokens, &l.EffectiveCalls, &l.MultiplierUsed,
			&l.StatusCode, &l.LatencyMs, &l.ErrorMsg, &l.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan call log: %w", err)
		}
		logs = append(logs, l)
	}

	if logs == nil {
		logs = []CallLog{}
	}

	return &CallLogPage{
		Data: logs,
		Pagination: Pagination{
			Page:  filter.Page,
			Limit: filter.Limit,
			Total: total,
		},
	}, rows.Err()
}

// GetDashboardOverview returns aggregated statistics for the admin dashboard.
type DashboardOverview struct {
	TotalUsers       int `json:"total_users"`
	ActiveUsers      int `json:"active_users"`
	TotalCalls       int `json:"total_calls"`
	TotalCallsToday  int `json:"total_calls_today"`
	AvgLatencyMs     int `json:"avg_latency_ms"`
	TotalTokensToday int `json:"total_tokens_today"`
	ExpiringSoon     int `json:"expiring_soon"`
}

// GetDashboardOverview queries aggregated stats.
func GetDashboardOverview(db *sql.DB) (*DashboardOverview, error) {
	o := &DashboardOverview{}
	today := time.Now().Format("2006-01-02")

	// Total users
	db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&o.TotalUsers)

	// Active users
	db.QueryRow(`SELECT COUNT(*) FROM users WHERE status = 'active'`).Scan(&o.ActiveUsers)

	// Total calls
	db.QueryRow(`SELECT COUNT(*) FROM call_logs`).Scan(&o.TotalCalls)

	// Total calls today
	db.QueryRow(`SELECT COUNT(*) FROM call_logs WHERE created_at >= ?`, today).Scan(&o.TotalCallsToday)

	// Average latency
	var avgLatency sql.NullFloat64
	db.QueryRow(`SELECT AVG(latency_ms) FROM call_logs WHERE status_code = 200`).Scan(&avgLatency)
	if avgLatency.Valid {
		o.AvgLatencyMs = int(avgLatency.Float64)
	}

	// Total tokens today
	var totalTokens sql.NullInt64
	db.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0) FROM call_logs WHERE created_at >= ?`, today).Scan(&totalTokens)
	if totalTokens.Valid {
		o.TotalTokensToday = int(totalTokens.Int64)
	}

	// Expiring soon: active non-admin users whose expires_at is between now and now+7d.
	nowShanghai := time.Now().In(timeutil.ShanghaiTZ)
	nowStr := nowShanghai.Format(time.RFC3339)
	sevenDaysStr := nowShanghai.AddDate(0, 0, 7).Format(time.RFC3339)
	var expiringSoon int
	db.QueryRow(
		`SELECT COUNT(*) FROM users WHERE expires_at != '' AND expires_at >= ? AND expires_at < ? AND status = 'active'`,
		nowStr, sevenDaysStr,
	).Scan(&expiringSoon)
	o.ExpiringSoon = expiringSoon

	return o, nil
}
