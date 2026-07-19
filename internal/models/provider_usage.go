package models

import (
	"database/sql"
	"fmt"
	"time"

	"llm_api_gateway/internal/timeutil"
)

// LowBalanceRatio is the default low-balance threshold: when used/limit >= this
// ratio (i.e. remaining < 10%), the provider is flagged as "low balance".
// Global and shared across token and call-count dimensions.
const LowBalanceRatio = 0.9

// RollingWindowStart returns the start of the rolling 30-day window as an
// RFC3339 timestamp in Asia/Shanghai. It is now-30*24h, formatted in the same
// timezone/layout as call_logs.created_at so the two can be compared directly
// as text (created_at >= windowStart).
func RollingWindowStart() string {
	return time.Now().Add(-30 * 24 * time.Hour).In(timeutil.ShanghaiTZ).Format(time.RFC3339)
}

// ProviderMonthlyUsage is the raw aggregated usage (within the rolling window)
// for a single provider. Slug identifies the provider.
type ProviderMonthlyUsage struct {
	Slug      string `json:"slug"`
	TokenUsed int64  `json:"token_used"`
	CallUsed  int64  `json:"call_used"`
}

// AggregateProviderUsage aggregates token and call usage for every provider
// within the rolling window in a single GROUP BY query.
//
// Providers with no calls in the window are simply absent from the returned
// map; callers should treat a missing entry as zero usage.
func AggregateProviderUsage(db *sql.DB, windowStart string) (map[string]*ProviderMonthlyUsage, error) {
	rows, err := db.Query(
		`SELECT provider_id,
		        COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS token_used,
		        COALESCE(SUM(effective_calls), 0)                    AS call_used
		 FROM call_logs
		 WHERE created_at >= ?
		 GROUP BY provider_id`,
		windowStart,
	)
	if err != nil {
		return nil, fmt.Errorf("aggregate provider usage: %w", err)
	}
	defer rows.Close()

	result := map[string]*ProviderMonthlyUsage{}
	for rows.Next() {
		var slug string
		var u ProviderMonthlyUsage
		if err := rows.Scan(&slug, &u.TokenUsed, &u.CallUsed); err != nil {
			return nil, fmt.Errorf("scan provider usage: %w", err)
		}
		u.Slug = slug
		result[slug] = &u
	}
	if result == nil {
		result = map[string]*ProviderMonthlyUsage{}
	}
	return result, rows.Err()
}

// GetProviderUsage returns the token and call usage for a single provider
// within the rolling window. Used by the account-creation form hint.
func GetProviderUsage(db *sql.DB, slug, windowStart string) (*ProviderMonthlyUsage, error) {
	var u ProviderMonthlyUsage
	u.Slug = slug
	err := db.QueryRow(
		`SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0),
		        COALESCE(SUM(effective_calls), 0)
		 FROM call_logs
		 WHERE provider_id = ? AND created_at >= ?`,
		slug, windowStart,
	).Scan(&u.TokenUsed, &u.CallUsed)
	if err != nil {
		return nil, fmt.Errorf("get provider usage %s: %w", slug, err)
	}
	return &u, nil
}

// ProviderUsageView is the fully computed, frontend-ready view for a single
// provider: it already folds in remaining/infinite/low-balance decisions so the
// frontend only renders (never recomputes thresholds).
type ProviderUsageView struct {
	Slug              string `json:"slug"`
	Name              string `json:"name"`
	MonthlyTokenLimit int64  `json:"monthly_token_limit"` // 0 = unlimited
	MonthlyCallLimit  int64  `json:"monthly_call_limit"`  // 0 = unlimited
	TokenUsed         int64  `json:"token_used"`
	TokenRemaining    int64  `json:"token_remaining"` // -1 when unlimited; may be negative if over limit
	TokenUnlimited    bool   `json:"token_unlimited"`
	CallUsed          int64  `json:"call_used"`
	CallRemaining     int64  `json:"call_remaining"` // -1 when unlimited; may be negative if over limit
	CallUnlimited     bool   `json:"call_unlimited"`
	WindowStart       string `json:"window_start"` // Asia/Shanghai RFC3339
	TokenLow          bool   `json:"token_low"`    // remaining < 10% -> flag red
	CallLow           bool   `json:"call_low"`
}

// IsLowBalance is the single source of truth for low-balance detection.
// An unlimited provider (limit <= 0) is never low. Otherwise low when
// used/limit >= LowBalanceRatio.
func IsLowBalance(used, limit int64) bool {
	if limit <= 0 {
		return false
	}
	return float64(used)/float64(limit) >= LowBalanceRatio
}

// BuildProviderUsageView synthesizes a ProviderUsageView from a provider record
// and its raw usage. A nil/empty usage is treated as zero usage.
func BuildProviderUsageView(p ProviderRecord, used *ProviderMonthlyUsage, windowStart string) ProviderUsageView {
	view := ProviderUsageView{
		Slug:              p.Slug,
		Name:              p.Name,
		MonthlyTokenLimit: p.MonthlyTokenLimit,
		MonthlyCallLimit:  p.MonthlyCallLimit,
		WindowStart:       windowStart,
	}

	tokenUsed := int64(0)
	callUsed := int64(0)
	if used != nil {
		tokenUsed = used.TokenUsed
		callUsed = used.CallUsed
	}
	view.TokenUsed = tokenUsed
	view.CallUsed = callUsed

	if p.MonthlyTokenLimit <= 0 {
		view.TokenUnlimited = true
		view.TokenRemaining = -1
	} else {
		view.TokenRemaining = p.MonthlyTokenLimit - tokenUsed
	}
	if p.MonthlyCallLimit <= 0 {
		view.CallUnlimited = true
		view.CallRemaining = -1
	} else {
		view.CallRemaining = p.MonthlyCallLimit - callUsed
	}

	view.TokenLow = IsLowBalance(tokenUsed, p.MonthlyTokenLimit)
	view.CallLow = IsLowBalance(callUsed, p.MonthlyCallLimit)
	return view
}
