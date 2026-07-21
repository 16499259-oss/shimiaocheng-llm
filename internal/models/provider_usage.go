package models

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"llm_api_gateway/internal/timeutil"
)

// RollingWindowStart returns the start of the rolling 30-day window as an
// RFC3339 timestamp in Asia/Shanghai. It is now-30*24h, formatted in the same
// timezone/layout as call_logs.created_at so the two can be compared directly
// as text (created_at >= windowStart).
//
// Deprecated: Use CurrentCycleWindow for fixed 30-day cycle windows (V3).
// Kept for backward compatibility with external callers.
func RollingWindowStart() string {
	return time.Now().Add(-30 * 24 * time.Hour).In(timeutil.ShanghaiTZ).Format(time.RFC3339)
}

// CurrentCycleWindow computes the current 30-day fixed cycle [start, end)
// anchored on cycleStartDate. Both returned strings are "2006-01-02" DATE
// values in Asia/Shanghai.
//
//	N = FLOOR(DATEDIFF(NOW(), cycleStart) / 30)
//	start = cycleStart + N*30d
//	end   = cycleStart + (N+1)*30d
//
// If cycleStartDate is empty or unparseable, it falls back to today's date.
func CurrentCycleWindow(cycleStartDate string) (start, end string) {
	now := time.Now().In(timeutil.ShanghaiTZ)

	cycleStart, err := time.ParseInLocation("2006-01-02", cycleStartDate, timeutil.ShanghaiTZ)
	if err != nil {
		// Fallback: anchor on today.
		cycleStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, timeutil.ShanghaiTZ)
	}

	days := int(now.Sub(cycleStart).Hours() / 24)
	n := days / 30
	if n < 0 {
		n = 0
	}

	startTime := cycleStart.AddDate(0, 0, n*30)
	endTime := cycleStart.AddDate(0, 0, (n+1)*30)

	return startTime.Format("2006-01-02"), endTime.Format("2006-01-02")
}

// ProviderMonthlyUsage is the raw aggregated usage (within the rolling window)
// for a single provider. Slug identifies the provider.
type ProviderMonthlyUsage struct {
	Slug      string `json:"slug"`
	TokenUsed int64  `json:"token_used"`
	CallUsed  int64  `json:"call_used"`
}

// AggregateProviderUsage aggregates token and call usage for every provider
// within the given window in a single GROUP BY query.
//
// windowStart should be an RFC3339 timestamp (e.g. "2026-01-01T00:00:00+08:00")
// for comparison against call_logs.created_at.
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
// within the given window. Used by the account-creation form hint.
// windowStart should be an RFC3339 timestamp.
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

// ── Allocation aggregation (V3) ──

// ProviderAllocation is the aggregated allocated quota for a single provider,
// computed by cross-table JOIN of users and quotas filtered by fixed_provider.
type ProviderAllocation struct {
	AllocatedTokens    int64 `json:"allocated_tokens"`
	AllocatedCalls     int64 `json:"allocated_calls"`
	UnlimitedUserCount int64 `json:"unlimited_user_count"` // Token-dimension unlimited users
}

// GetProviderAllocation cross-table aggregates allocated quota for the given
// provider. It SUMs quota_token_total_limit (>0 only) and quota_total_limit
// (>0 only) from all active, non-expired users whose fixed_provider matches.
//
// 0-semantics (unified since 2026-07-21: both dimensions treat 0 as
// unlimited, matching the self-service panel which hides a row whose cap is 0):
//   - Token: quota_token_total_limit = 0 → unlimited (excluded from
//     allocated_tokens SUM, counted in unlimited_user_count).
//   - Call:  quota_total_limit = 0 → unlimited (excluded from
//     allocated_calls SUM, NOT counted in unlimited_user_count). An unlimited
//     call-count user is metered only by Token usage, so it correctly does not
//     occupy any call-count allocation on the upstream.
func GetProviderAllocation(db *sql.DB, providerSlug string) (*ProviderAllocation, error) {
	var a ProviderAllocation
	err := db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN q.quota_token_total_limit > 0
			                  THEN q.quota_token_total_limit END), 0),
			COALESCE(SUM(CASE WHEN q.quota_total_limit > 0
			                  THEN q.quota_total_limit END), 0),
			COUNT(CASE WHEN q.quota_token_total_limit = 0
			           THEN 1 END)
		FROM users u
		JOIN quotas q ON u.id = q.user_id
		WHERE u.fixed_provider = ?
		  AND u.status = 'active'
		  AND (u.expires_at = '' OR u.expires_at > datetime('now'))
	`, providerSlug).Scan(&a.AllocatedTokens, &a.AllocatedCalls, &a.UnlimitedUserCount)
	if err != nil {
		return nil, fmt.Errorf("get provider allocation %s: %w", providerSlug, err)
	}
	return &a, nil
}

// ── Allocation detail (per-user breakdown) ──

// ProviderAllocationUser is a single row of the per-user allocation breakdown
// for a provider: who is pinned to it, what quota they carry, and how much
// they have consumed within the current 30-day cycle window.
type ProviderAllocationUser struct {
	Username             string `json:"username"`
	Status               string `json:"status"`
	CreatedAt            string `json:"created_at"`
	ExpiresAt            string `json:"expires_at"`
	QuotaTokenTotalLimit int64  `json:"quota_token_total_limit"` // 0 = unlimited (token dim)
	QuotaTotalLimit      int64  `json:"quota_total_limit"`       // 0 = unlimited (call dim, since 2026-07-21)
	TokenUsed            int64  `json:"token_used"`              // within cycle window
	CallUsed             int64  `json:"call_used"`               // within cycle window
}

// GetProviderAllocationDetails returns the per-user allocation breakdown for a
// provider: every active, non-expired user whose fixed_provider matches, with
// their monthly quota limits and cycle-window usage.
//
// Token usage is the BILLED (multiplier-scaled) sum — ceil((prompt+completion)
// * multiplier_used) per call_log row — exactly matching the quota_token_total_used
// "Token 月总量" the user sees in their own panel, so the per-user "已用" reflects
// actual consumption (含倍率). Call usage is effective_calls (no multiplier). The
// cycle window is aligned to the same one used by GetProviderUsage.
//
// windowStart must be an RFC3339 timestamp (e.g. "2026-07-01T00:00:00+08:00")
// for direct text comparison against call_logs.created_at.
func GetProviderAllocationDetails(db *sql.DB, providerSlug, windowStart string) ([]ProviderAllocationUser, error) {
	rows, err := db.Query(
		`SELECT
			u.username,
			u.status,
			u.created_at,
			u.expires_at,
			COALESCE(q.quota_token_total_limit, 0),
			COALESCE(q.quota_total_limit, 0),
			COALESCE(SUM(CASE WHEN c.created_at >= ? THEN CAST((c.prompt_tokens + c.completion_tokens) * c.multiplier_used + 0.999999 AS INTEGER) END), 0) AS token_used,
			COALESCE(SUM(CASE WHEN c.created_at >= ? THEN c.effective_calls END), 0) AS call_used
		 FROM users u
		 JOIN quotas q ON u.id = q.user_id
		 LEFT JOIN call_logs c ON c.user_id = u.id
		 WHERE u.fixed_provider = ?
		   AND u.status = 'active'
		   AND (u.expires_at = '' OR u.expires_at > datetime('now'))
		 GROUP BY u.id
		 ORDER BY token_used DESC, u.username ASC`,
		windowStart, windowStart, providerSlug,
	)
	if err != nil {
		return nil, fmt.Errorf("get provider allocation details %s: %w", providerSlug, err)
	}
	defer rows.Close()

	out := make([]ProviderAllocationUser, 0)
	for rows.Next() {
		var r ProviderAllocationUser
		if err := rows.Scan(
			&r.Username, &r.Status, &r.CreatedAt, &r.ExpiresAt,
			&r.QuotaTokenTotalLimit, &r.QuotaTotalLimit, &r.TokenUsed, &r.CallUsed,
		); err != nil {
			return nil, fmt.Errorf("scan allocation detail: %w", err)
		}
		out = append(out, r)
	}
	if out == nil {
		out = []ProviderAllocationUser{}
	}
	return out, rows.Err()
}

// ── Auto user allocation (shared pool attribution) ──

// AutoProviderLoad is the per-provider attribution of auto (route_mode='auto')
// users' quotas and actual usage within the cycle window. It lets the provider
// "已分配" column reflect the real pressure auto traffic puts on each upstream,
// without double-counting (an auto user's quota is counted once globally and
// split across providers by actual usage share).
type AutoProviderLoad struct {
	ProviderSlug    string `json:"provider_slug"`
	TokenQuotaShare int64  `json:"token_quota_share"` // attributed auto token quota (finite users, by usage share)
	CallQuotaShare  int64  `json:"call_quota_share"`  // attributed auto call quota
	AutoTokenUsage  int64  `json:"auto_token_usage"`  // actual billed tokens from auto users in window
	AutoCallUsage   int64  `json:"auto_call_usage"`   // actual calls from auto users in window
}

// AutoAllocationResult is the global breakdown of auto (route_mode='auto') users.
type AutoAllocationResult struct {
	ByProvider         []AutoProviderLoad `json:"by_provider"`
	PoolTokenTotal     int64              `json:"pool_token_total"` // Σ finite auto token quotas (shared pool)
	PoolCallTotal      int64              `json:"pool_call_total"`  // Σ finite auto call quotas
	UnlimitedUserCount int64              `json:"unlimited_user_count"`
}

// GetAutoUserAllocationByProvider aggregates auto (route_mode='auto') users'
// quotas into a global shared pool and attributes each user's quota to the
// providers they actually used (by their cycle-window token usage share), so
// the per-provider "已分配" can include a realistic auto-load estimate.
//
// Attribution rule (no double-counting):
//   - For an auto user with cycle-window billed usage T>0 on providers, their
//     finite token/call quota is split across those providers by share
//     (billed_on_p / T). Shares sum to 1, so the user's quota is counted exactly
//     once across all providers.
//   - Auto users with zero cycle-window usage are NOT attributed to any single
//     provider — their quotas stay in the global shared pool (PoolTokenTotal /
//     PoolCallTotal) until they generate traffic.
//   - Unlimited auto users (quota_token_total_limit=0) have no finite token quota
//     to attribute, but their actual window usage is still counted in
//     AutoTokenUsage/AutoCallUsage (real pressure on the upstream).
//
// Billed token = ceil((prompt+completion)*multiplier_used) per call_log row —
// the same multiplier-aware口径 as quota_token_total_used / the allocation
// detail (PR #42), so the auto attribution is consistent with the user's own
// "Token 月总量" and the per-user "已用" shown in the detail modal.
//
// windowStart must be an RFC3339 timestamp comparable to call_logs.created_at.
func GetAutoUserAllocationByProvider(db *sql.DB, windowStart string) (*AutoAllocationResult, error) {
	// 1. All active, non-expired auto users and their finite quotas.
	type autoUser struct {
		id         int64
		tokenLimit int64 // 0 = unlimited (token dim)
		callLimit  int64 // 0 = locked/invalid (call dim)
	}
	users := make([]autoUser, 0)
	var poolToken, poolCall, unlimitedCnt int64

	rows, err := db.Query(`
		SELECT u.id, q.quota_token_total_limit, q.quota_total_limit
		FROM users u
		JOIN quotas q ON u.id = q.user_id
		WHERE u.route_mode = 'auto'
		  AND u.status = 'active'
		  AND (u.expires_at = '' OR u.expires_at > datetime('now'))
	`)
	if err != nil {
		return nil, fmt.Errorf("list auto users: %w", err)
	}
	for rows.Next() {
		var au autoUser
		if err := rows.Scan(&au.id, &au.tokenLimit, &au.callLimit); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan auto user: %w", err)
		}
		if au.tokenLimit == 0 {
			unlimitedCnt++
		} else {
			poolToken += int64(au.tokenLimit)
		}
		// Call dim: 0 = invalid/locked (PR #14); a locked user commits no
		// provider call capacity, so only >0 counts toward the shared pool.
		if au.callLimit > 0 {
			poolCall += int64(au.callLimit)
		}
		users = append(users, au)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate auto users: %w", err)
	}

	res := &AutoAllocationResult{
		ByProvider:         []AutoProviderLoad{},
		PoolTokenTotal:     poolToken,
		PoolCallTotal:      poolCall,
		UnlimitedUserCount: unlimitedCnt,
	}
	if len(users) == 0 {
		return res, nil
	}

	// 2. Per (auto user, provider) cycle-window billed tokens + calls.
	type uP struct {
		billed int64
		calls  int64
	}
	usage := map[int64]map[string]*uP{} // userID -> provider -> usage
	urows, err := db.Query(`
		SELECT c.user_id, c.provider_id,
		       COALESCE(SUM(CAST((c.prompt_tokens + c.completion_tokens) * c.multiplier_used + 0.999999 AS INTEGER)), 0) AS billed,
		       COALESCE(SUM(c.effective_calls), 0) AS calls
		FROM call_logs c
		JOIN users u ON u.id = c.user_id
		WHERE u.route_mode = 'auto'
		  AND u.status = 'active'
		  AND (u.expires_at = '' OR u.expires_at > datetime('now'))
		  AND c.created_at >= ?
		GROUP BY c.user_id, c.provider_id
	`, windowStart)
	if err != nil {
		return nil, fmt.Errorf("auto usage: %w", err)
	}
	for urows.Next() {
		var uid int64
		var slug string
		var up uP
		if err := urows.Scan(&uid, &slug, &up.billed, &up.calls); err != nil {
			urows.Close()
			return nil, fmt.Errorf("scan auto usage: %w", err)
		}
		if usage[uid] == nil {
			usage[uid] = map[string]*uP{}
		}
		usage[uid][slug] = &up
	}
	urows.Close()
	if err := urows.Err(); err != nil {
		return nil, fmt.Errorf("iterate auto usage: %w", err)
	}

	// 3. Distribute each user's finite quota by usage share.
	bySlug := map[string]*AutoProviderLoad{}
	getLoad := func(slug string) *AutoProviderLoad {
		if l, ok := bySlug[slug]; ok {
			return l
		}
		l := &AutoProviderLoad{ProviderSlug: slug}
		bySlug[slug] = l
		return l
	}
	for _, au := range users {
		ups := usage[au.id]
		if ups == nil {
			continue // zero usage → stays in shared pool
		}
		var totalBilled int64
		for _, v := range ups {
			totalBilled += v.billed
		}
		if totalBilled <= 0 {
			continue // no billed usage → stays in shared pool
		}
		for slug, v := range ups {
			share := float64(v.billed) / float64(totalBilled)
			l := getLoad(slug)
			l.AutoTokenUsage += v.billed
			l.AutoCallUsage += v.calls
			if au.tokenLimit > 0 {
				l.TokenQuotaShare += int64(math.Round(float64(au.tokenLimit) * share))
			}
			if au.callLimit > 0 {
				l.CallQuotaShare += int64(math.Round(float64(au.callLimit) * share))
			}
		}
	}

	for _, l := range bySlug {
		res.ByProvider = append(res.ByProvider, *l)
	}
	sort.Slice(res.ByProvider, func(i, j int) bool {
		return res.ByProvider[i].ProviderSlug < res.ByProvider[j].ProviderSlug
	})
	return res, nil
}

// ── Provider usage view (V3: dual-column allocation) ──

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
	WindowStart       string `json:"window_start"` // cycle start DATE string (V3)
	TokenLow          bool   `json:"token_low"`    // remaining < threshold -> flag red
	CallLow           bool   `json:"call_low"`
	// ── V3: Allocation (dual-column) ──
	AllocatedTokens    int64 `json:"allocated_tokens"`
	AllocatedCalls     int64 `json:"allocated_calls"`
	UnlimitedUserCount int64 `json:"unlimited_user_count"`
	AllocationLow      bool  `json:"allocation_low"` // allocated exceeds threshold
	// ── V3.5: Auto (route_mode='auto') shared-pool attribution ──
	// These make "已分配" reflect the real pressure auto traffic puts on each
	// upstream, without double-counting (see GetAutoUserAllocationByProvider).
	AutoAllocatedTokens int64  `json:"auto_allocated_tokens"` // attributed auto token quota (by usage share)
	AutoAllocatedCalls  int64  `json:"auto_allocated_calls"`  // attributed auto call quota
	AutoTokenUsage      int64  `json:"auto_token_usage"`      // actual billed tokens from auto users in window
	AutoCallUsage       int64  `json:"auto_call_usage"`       // actual calls from auto users in window
	CycleStart          string `json:"cycle_start"`           // current cycle start DATE
	CycleEnd            string `json:"cycle_end"`             // current cycle end DATE (exclusive)
	CycleDaysRemaining  int    `json:"cycle_days_remaining"`  // days left in cycle
}

// IsLowBalance is the single source of truth for low-balance detection.
// remainingRatio is the "remaining threshold" (e.g. 0.10 = "flag red when
// < 10% remains"). An unlimited provider (limit <= 0) is never low. Otherwise
// low when used/limit >= (1 - remainingRatio), i.e. when the remaining
// fraction drops below remainingRatio. (Over-limit usage is still flagged low,
// but only for display — never used to block requests.)
func IsLowBalance(used, limit int64, remainingRatio float64) bool {
	if limit <= 0 {
		return false
	}
	usedRatio := float64(used) / float64(limit)
	return usedRatio >= (1 - remainingRatio)
}

// BuildProviderUsageView synthesizes a ProviderUsageView from a provider record
// and its raw usage. A nil/empty usage is treated as zero usage.
//
// alloc is the cross-table allocation aggregate (may be nil if unavailable).
// globalTokenRemainingRatio / globalCallRemainingRatio are the global default
// thresholds (remaining ratio) from config.ProviderQuota. A per-provider
// override (MonthlyTokenLowRatio / MonthlyCallLowRatio > 0) takes precedence
// over the global default; 0 means "inherit the global default".
func BuildProviderUsageView(p ProviderRecord, used *ProviderMonthlyUsage, alloc *ProviderAllocation, globalTokenRemainingRatio, globalCallRemainingRatio float64) ProviderUsageView {
	cycleStart, cycleEnd := CurrentCycleWindow(p.CycleStartDate)

	view := ProviderUsageView{
		Slug:              p.Slug,
		Name:              p.Name,
		MonthlyTokenLimit: p.MonthlyTokenLimit,
		MonthlyCallLimit:  p.MonthlyCallLimit,
		WindowStart:       cycleStart,
		CycleStart:        cycleStart,
		CycleEnd:          cycleEnd,
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

	// Resolve effective threshold: per-provider override wins over global default.
	tokenRatio := globalTokenRemainingRatio
	if p.MonthlyTokenLowRatio > 0 {
		tokenRatio = p.MonthlyTokenLowRatio
	}
	callRatio := globalCallRemainingRatio
	if p.MonthlyCallLowRatio > 0 {
		callRatio = p.MonthlyCallLowRatio
	}

	view.TokenLow = IsLowBalance(tokenUsed, p.MonthlyTokenLimit, tokenRatio)
	view.CallLow = IsLowBalance(callUsed, p.MonthlyCallLimit, callRatio)

	// ── V3: Allocation fields ──
	if alloc != nil {
		view.AllocatedTokens = alloc.AllocatedTokens
		view.AllocatedCalls = alloc.AllocatedCalls
		view.UnlimitedUserCount = alloc.UnlimitedUserCount
		// AllocationLow: uses same IsLowBalance logic, just with allocated instead of used.
		view.AllocationLow = IsLowBalance(alloc.AllocatedTokens, p.MonthlyTokenLimit, tokenRatio) ||
			IsLowBalance(alloc.AllocatedCalls, p.MonthlyCallLimit, callRatio)
	}

	// ── Cycle days remaining ──
	endTime, err := time.ParseInLocation("2006-01-02", cycleEnd, timeutil.ShanghaiTZ)
	if err == nil {
		now := time.Now().In(timeutil.ShanghaiTZ)
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, timeutil.ShanghaiTZ)
		remaining := int(endTime.Sub(today).Hours() / 24)
		if remaining < 0 {
			remaining = 0
		}
		view.CycleDaysRemaining = remaining
	}

	return view
}
