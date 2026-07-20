package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// seedMultipliedCallLogs inserts two call_logs rows for userID:
//   - a "today" row with multiplier 3.0 (10+5 tokens) -> ceil(15*3) = 45
//   - an "8 days ago" row with multiplier 1.0 (10+5 tokens) -> 15
//
// created_at is written as a local RFC3339 timestamp so it aligns with the
// handler's own `today` filter (`time.Now().Format("2006-01-02")` + textual
// `created_at >= today` comparison): the today row is guaranteed to sort >= the
// date string, the old row is guaranteed to sort below it.
func seedMultipliedCallLogs(t *testing.T, conn *sql.DB, userID int64) {
	t.Helper()
	today := time.Now().Format(time.RFC3339)
	old := time.Now().AddDate(0, 0, -8).Format(time.RFC3339)

	rows := []struct {
		p, c    int
		m       float64
		created string
	}{
		{p: 10, c: 5, m: 3.0, created: today}, // today, x3 -> 45
		{p: 10, c: 5, m: 1.0, created: old},   // 8 days ago -> 15
	}
	for _, r := range rows {
		if _, err := conn.Exec(
			`INSERT INTO call_logs (user_id, prompt_tokens, completion_tokens, total_tokens, multiplier_used, created_at, status_code)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			userID, r.p, r.c, r.p+r.c, r.m, r.created, 200,
		); err != nil {
			t.Fatalf("seed call_logs: %v", err)
		}
	}
}

// TestSumMultipliedTokensDirectly verifies the recompute helper itself: it must
// multiply each row's (prompt+completion) by its own multiplier_used and ceil,
// and must honour the "since" filter for the today counter (audit L3: the raw
// total_tokens column is NOT what is summed). This is the priority coverage.
func TestSumMultipliedTokensDirectly(t *testing.T) {
	database := openQuotaHandlerTestDB(t)

	subKey := auth.GenerateSubKey("mt", 1)
	u, err := models.CreateUser(database.Conn, "mt_direct", "pw",
		auth.HashSubKey(subKey), auth.SubKeyPreview(subKey),
		"user", "active", "", "auto", "", 1000, 1000, nil, 0, models.DefaultMaxConcurrency)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedMultipliedCallLogs(t, database.Conn, u.ID)

	// Full cumulative sum: 45 (today, x3) + 15 (old, x1) = 60.
	all, err := sumMultipliedTokens(database.Conn, u.ID, "")
	if err != nil {
		t.Fatalf("sumMultipliedTokens(all): %v", err)
	}
	if all != 60 {
		t.Fatalf("expected total multiplied tokens == 60, got %d", all)
	}

	// Today-only sum (handler's own date string): only the x3 row -> 45.
	today := time.Now().Format("2006-01-02")
	todays, err := sumMultipliedTokens(database.Conn, u.ID, today)
	if err != nil {
		t.Fatalf("sumMultipliedTokens(today): %v", err)
	}
	if todays != 45 {
		t.Fatalf("expected today multiplied tokens == 45, got %d", todays)
	}
}

// TestQuotaHandler_TotalTokensIncludesMultiplier drives the live /v1/quota
// endpoint and asserts the JSON contract: total_tokens / total_tokens_today are
// now the multiplier-inflated values (60 / 45), while the field names are
// unchanged. The call-records detail page keeps the raw total_tokens separately
// and is intentionally not affected here.
func TestQuotaHandler_TotalTokensIncludesMultiplier(t *testing.T) {
	database := openQuotaHandlerTestDB(t)

	subKey := auth.GenerateSubKey("mt", 2)
	u, err := models.CreateUser(database.Conn, "mt_ep", "pw",
		auth.HashSubKey(subKey), auth.SubKeyPreview(subKey),
		"user", "active", "", "auto", "", 1000, 1000, nil, 0, models.DefaultMaxConcurrency)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedMultipliedCallLogs(t, database.Conn, u.ID)

	multEng := quota.NewMultiplierEngine(database.Conn)
	h := &QuotaHandler{DB: database.Conn, MultEng: multEng, ResetInterval: 5}
	authMW := auth.NewMiddleware(database.Conn)
	wrapped := authMW.SubKeyAuth(h)

	req := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	var status models.QuotaStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode quota status: %v", err)
	}
	if status.TotalTokens != 60 {
		t.Fatalf("expected total_tokens == 60, got %d", status.TotalTokens)
	}
	if status.TotalTokensToday != 45 {
		t.Fatalf("expected total_tokens_today == 45, got %d", status.TotalTokensToday)
	}

	// Field names must remain unchanged (only the value semantics changed).
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, ok := raw["total_tokens"]; !ok {
		t.Fatalf("response JSON missing total_tokens key")
	}
	if _, ok := raw["total_tokens_today"]; !ok {
		t.Fatalf("response JSON missing total_tokens_today key")
	}
}

// TestSumMultipliedTokens_CeilFractionalMultiplier is an independent boundary
// case proving the per-row math.Ceil semantics with a FRACTIONAL multiplier:
// a non-integer product must be rounded UP, not truncated or floored, and the
// value must NOT equal a naive (prompt+completion)*multiplier without ceil, nor
// the raw total_tokens column. This isolates the "multiply then ceil" behaviour
// from the integer-multiplier cases above.
//
// Two rows, both today:
//   - multiplier 2.5, (10+5)=15 -> ceil(15*2.5) = ceil(37.5) = 38
//   - multiplier 1.0, (10+5)=15 -> ceil(15*1.0) = 15
//
// Expected full cumulative sum = 38 + 15 = 53.
func TestSumMultipliedTokens_CeilFractionalMultiplier(t *testing.T) {
	database := openQuotaHandlerTestDB(t)

	subKey := auth.GenerateSubKey("mtc", 3)
	u, err := models.CreateUser(database.Conn, "mtc_ceil", "pw",
		auth.HashSubKey(subKey), auth.SubKeyPreview(subKey),
		"user", "active", "", "auto", "", 1000, 1000, nil, 0, models.DefaultMaxConcurrency)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	todayRFC := time.Now().Format(time.RFC3339)
	type seed struct {
		p, c int
		m    float64
	}
	rows := []seed{
		{p: 10, c: 5, m: 2.5}, // ceil(15*2.5)=ceil(37.5)=38
		{p: 10, c: 5, m: 1.0}, // ceil(15*1.0)=15
	}
	for _, r := range rows {
		if _, err := database.Conn.Exec(
			`INSERT INTO call_logs (user_id, prompt_tokens, completion_tokens, total_tokens, multiplier_used, created_at, status_code)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			u.ID, r.p, r.c, r.p+r.c, r.m, todayRFC, 200,
		); err != nil {
			t.Fatalf("seed call_logs: %v", err)
		}
	}

	// The helper itself: full cumulative sum must be 53.
	all, err := sumMultipliedTokens(database.Conn, u.ID, "")
	if err != nil {
		t.Fatalf("sumMultipliedTokens(all): %v", err)
	}
	if all != 53 {
		t.Fatalf("expected ceiled fractional sum == 53, got %d", all)
	}

	// Negative control: a naive float sum without ceil would be 37.5+15 = 52.5
	// (-> 52 when truncated), not 53. Assert we are NOT 52 to make the ceil
	// direction explicit (defends against a future regression to truncation).
	if all == 52 {
		t.Fatalf("sum looks truncated (no ceil): got 52, expected 53 (ceil of 37.5 = 38)")
	}

	// Drive the live /v1/quota endpoint. Both rows are "today", so the today
	// counter must also be 53.
	multEng := quota.NewMultiplierEngine(database.Conn)
	h := &QuotaHandler{DB: database.Conn, MultEng: multEng, ResetInterval: 5}
	authMW := auth.NewMiddleware(database.Conn)
	wrapped := authMW.SubKeyAuth(h)

	req := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var status models.QuotaStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode quota status: %v", err)
	}
	if status.TotalTokens != 53 {
		t.Fatalf("endpoint: expected total_tokens == 53, got %d", status.TotalTokens)
	}

	todayStr := time.Now().Format("2006-01-02")
	todays, err := sumMultipliedTokens(database.Conn, u.ID, todayStr)
	if err != nil {
		t.Fatalf("sumMultipliedTokens(today): %v", err)
	}
	if todays != 53 {
		t.Fatalf("expected today multiplied tokens == 53, got %d", todays)
	}
	if status.TotalTokensToday != 53 {
		t.Fatalf("endpoint: expected total_tokens_today == 53, got %d", status.TotalTokensToday)
	}
}
