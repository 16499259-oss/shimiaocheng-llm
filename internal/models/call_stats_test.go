// Package models_test contains tests for the models package.
package models_test

import (
	"strings"
	"testing"
	"time"

	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/timeutil"
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
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
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
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
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
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
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

// TestQueryCallLogsGlobal_UsernamePopulated verifies that the global admin
// call-log list (QueryCallLogsGlobal) returns the calling user's display name
// per row, so an admin can tell which user made each call. Uses a correlated
// subquery on users.id = call_logs.user_id (no JOIN, to avoid ambiguous
// column names with buildCallLogWhere's unqualified created_at).
func TestQueryCallLogsGlobal_UsernamePopulated(t *testing.T) {
	database := newModelsTestDB(t)

	alice, err := models.CreateUser(
		database.Conn,
		"alice", "pw-hash", "sub-hash-alice", "sk-alice...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(alice) failed: %v", err)
	}
	bob, err := models.CreateUser(
		database.Conn,
		"bob", "pw-hash", "sub-hash-bob", "sk-bob...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(bob) failed: %v", err)
	}

	insert := func(userID int64, model string) {
		if _, err := models.InsertCallLog(database.Conn, &models.CallLog{
			UserID:           userID,
			Model:            model,
			ProviderID:       "zhipu",
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
			EffectiveCalls:   1,
			StatusCode:       200,
		}); err != nil {
			t.Fatalf("InsertCallLog(%d,%q) failed: %v", userID, model, err)
		}
	}
	insert(alice.ID, "glm-5.2") // alice's call
	insert(bob.ID, "gpt-4o")    // bob's call

	page, err := models.QueryCallLogsGlobal(database.Conn, models.CallLogFilter{})
	if err != nil {
		t.Fatalf("QueryCallLogsGlobal returned error: %v", err)
	}
	if page == nil || page.Data == nil {
		t.Fatalf("expected non-nil page/data, got %+v", page)
	}
	if page.Pagination.Total != 2 {
		t.Fatalf("expected 2 total calls, got %d", page.Pagination.Total)
	}
	if len(page.Data) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(page.Data), page.Data)
	}

	// Rows are ordered by id DESC, so the newest (bob's) is first.
	bobRow := page.Data[0]
	aliceRow := page.Data[1]
	if bobRow.UserID != bob.ID || bobRow.Username != "bob" {
		t.Fatalf("expected first row (newest) to be bob, got id=%d username=%q", bobRow.UserID, bobRow.Username)
	}
	if aliceRow.UserID != alice.ID || aliceRow.Username != "alice" {
		t.Fatalf("expected second row to be alice, got id=%d username=%q", aliceRow.UserID, aliceRow.Username)
	}
}

// TestAggregateCallStats_ByUser verifies the per-user breakdown added for the
// "按用户明细" admin tab: call_logs are grouped by user_id (LEFT JOIN users for
// the display name), token columns are summed, and rows are ordered by call
// count DESC.
//
// NOTE on the orphan (user_id = 0) case mentioned in the spec: call_logs has a
// FOREIGN KEY (user_id) REFERENCES users(id), so a true orphan row cannot be
// inserted through the normal path. The COALESCE(users.username, ”) in the
// aggregate query is therefore a defensive safeguard (covers e.g. a user row
// dropped without CASCADE). This test validates the real-user path with two
// users and asserts the sums/order are correct.
func TestAggregateCallStats_ByUser(t *testing.T) {
	database := newModelsTestDB(t)

	alice, err := models.CreateUser(
		database.Conn,
		"byuser-alice", "pw-hash", "sub-alice", "sk-alice...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(alice) failed: %v", err)
	}
	bob, err := models.CreateUser(
		database.Conn,
		"byuser-bob", "pw-hash", "sub-bob", "sk-bob...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(bob) failed: %v", err)
	}

	// alice: 2 calls (glm-5.2 + gpt-4o) -> leads by count.
	// bob:   1 call  (glm-5.2).
	insert := func(userID int64, model string, p, c, tot, status, eff int) {
		if _, err := models.InsertCallLog(database.Conn, &models.CallLog{
			UserID:           userID,
			Model:            model,
			ProviderID:       "zhipu",
			PromptTokens:     p,
			CompletionTokens: c,
			TotalTokens:      tot,
			EffectiveCalls:   eff,
			StatusCode:       status,
		}); err != nil {
			t.Fatalf("InsertCallLog(%d,%q) failed: %v", userID, model, err)
		}
	}
	insert(alice.ID, "glm-5.2", 100, 200, 300, 200, 1)
	insert(alice.ID, "gpt-4o", 10, 20, 30, 200, 1)
	insert(bob.ID, "glm-5.2", 40, 60, 100, 200, 1)

	stats, err := models.AggregateCallStats(database.Conn, models.CallLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCallStats returned error: %v", err)
	}

	if len(stats.ByUser) != 2 {
		t.Fatalf("expected 2 by_user entries, got %d: %+v", len(stats.ByUser), stats.ByUser)
	}

	// Ordered by Calls DESC: alice (2) must come before bob (1).
	if stats.ByUser[0].Calls < stats.ByUser[1].Calls {
		t.Fatalf("expected by_user sorted by Calls DESC, got %d then %d",
			stats.ByUser[0].Calls, stats.ByUser[1].Calls)
	}

	// Locate each user by username (ids are not guaranteed to be 1/2 in a temp DB).
	find := func(name string) models.UserBreakdown {
		for _, u := range stats.ByUser {
			if u.Username == name {
				return u
			}
		}
		t.Fatalf("by_user entry for %q not found: %+v", name, stats.ByUser)
		return models.UserBreakdown{}
	}
	a := find("byuser-alice")
	b := find("byuser-bob")

	if a.Calls != 2 {
		t.Fatalf("alice expected Calls==2, got %d", a.Calls)
	}
	if a.Tokens.Prompt != 100+10 {
		t.Fatalf("alice prompt mismatch: got %d want %d", a.Tokens.Prompt, 100+10)
	}
	if a.Tokens.Completion != 200+20 {
		t.Fatalf("alice completion mismatch: got %d want %d", a.Tokens.Completion, 200+20)
	}
	if a.Tokens.Total != 300+30 {
		t.Fatalf("alice total mismatch: got %d want %d", a.Tokens.Total, 300+30)
	}
	if a.EffectiveCalls != 2 {
		t.Fatalf("alice expected EffectiveCalls==2, got %d", a.EffectiveCalls)
	}

	if b.Calls != 1 {
		t.Fatalf("bob expected Calls==1, got %d", b.Calls)
	}
	if b.Tokens.Prompt != 40 || b.Tokens.Completion != 60 || b.Tokens.Total != 100 {
		t.Fatalf("bob token mismatch: %+v", b.Tokens)
	}
	if b.EffectiveCalls != 1 {
		t.Fatalf("bob expected EffectiveCalls==1, got %d", b.EffectiveCalls)
	}
}

// TestAggregateCallStats_ByUserWithTimeFilter is the regression test for the
// production 500 on GET /api/calls/stats.
//
// Root cause: buildCallLogWhere emitted a *bare* "created_at" predicate. That
// is fine for the main aggregate / by_model queries (no JOIN), but the by_user
// query does `LEFT JOIN users ON users.id = call_logs.user_id`, and the users
// table also has a created_at column. With a time-range filter (the frontend
// defaults to 7d and always sends from/to), the bare column became ambiguous
// and SQLite raised "ambiguous column name: created_at", failing the whole
// AggregateCallStats call -> HTTP 500 -> both breakdown tabs render empty.
//
// Now that buildCallLogWhere prefixes created_at with call_logs., the by_user
// query with a time filter must succeed. This test forces the JOIN+time-filter
// path and asserts no error plus correct filtering.
func TestAggregateCallStats_ByUserWithTimeFilter(t *testing.T) {
	database := newModelsTestDB(t)

	// Three users; one of them (carol) will have only out-of-window calls so
	// we can prove she is excluded from ByUser.
	alice, err := models.CreateUser(
		database.Conn,
		"tbf-alice", "pw-hash", "sub-alice", "sk-alice...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(alice) failed: %v", err)
	}
	bob, err := models.CreateUser(
		database.Conn,
		"tbf-bob", "pw-hash", "sub-bob", "sk-bob...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(bob) failed: %v", err)
	}
	carol, err := models.CreateUser(
		database.Conn,
		"tbf-carol", "pw-hash", "sub-carol", "sk-carol...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(carol) failed: %v", err)
	}

	// Insert a call log then pin its created_at to a specific instant so we can
	// deterministically drive the time window. InsertCallLog stamps created_at
	// with time.Now() internally, so we override it via a direct UPDATE in the
	// same Asia/Shanghai RFC3339 format the column stores/compares.
	insertAt := func(userID int64, model string, at time.Time) {
		id, err := models.InsertCallLog(database.Conn, &models.CallLog{
			UserID:           userID,
			Model:            model,
			ProviderID:       "zhipu",
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
			EffectiveCalls:   1,
			StatusCode:       200,
		})
		if err != nil {
			t.Fatalf("InsertCallLog failed: %v", err)
		}
		ts := at.In(timeutil.ShanghaiTZ).Format(time.RFC3339)
		if _, err := database.Conn.Exec(
			"UPDATE call_logs SET created_at = ? WHERE id = ?", ts, id,
		); err != nil {
			t.Fatalf("override created_at for id=%d failed: %v", id, err)
		}
	}

	now := time.Now().In(timeutil.ShanghaiTZ)
	// 7-day window boundaries, expressed as Asia/Shanghai RFC3339 strings —
	// exactly what the admin handler produces via NormalizeToShanghaiRFC3339.
	from := now.AddDate(0, 0, -7).Format(time.RFC3339)
	to := now.Format(time.RFC3339)

	// alice: one IN-window call and one 10-days-ago call (out of window).
	insertAt(alice.ID, "glm-5.2", now.Add(-1*time.Hour))
	insertAt(alice.ID, "glm-5.2", now.Add(-10*24*time.Hour))
	// bob: one IN-window call.
	insertAt(bob.ID, "gpt-4o", now.Add(-2*time.Hour))
	// carol: one 30-days-ago call (entirely out of window -> excluded).
	insertAt(carol.ID, "glm-5.2", now.Add(-30*24*time.Hour))

	// The filter is exactly the (from, to) tuple the frontend sends; the bug
	// only manifested when From/To were non-empty (LEFT JOIN users path).
	stats, err := models.AggregateCallStats(database.Conn, models.CallLogFilter{
		From: from,
		To:   to,
	})
	if err != nil {
		t.Fatalf("AggregateCallStats(with From/To) returned error: %v", err)
	}
	if stats == nil {
		t.Fatalf("expected non-nil *CallStats")
	}

	// Only alice's 1 in-window call and bob's 1 in-window call count = 2.
	if stats.TotalCalls != 2 {
		t.Fatalf("expected TotalCalls == 2 within window, got %d", stats.TotalCalls)
	}

	// ByModel must be non-empty (we have glm-5.2 + gpt-4o in window).
	if len(stats.ByModel) == 0 {
		t.Fatalf("expected non-empty by_model breakdown, got %+v", stats.ByModel)
	}

	// ByUser must contain exactly alice and bob (each 1 in-window call) and
	// must NOT contain carol (all her calls are out of window).
	if len(stats.ByUser) != 2 {
		t.Fatalf("expected 2 by_user entries (alice+bob), got %d: %+v",
			len(stats.ByUser), stats.ByUser)
	}
	names := map[string]bool{}
	callsByUser := map[string]int{}
	for _, u := range stats.ByUser {
		names[u.Username] = true
		callsByUser[u.Username] = u.Calls
	}
	if !names["tbf-alice"] || !names["tbf-bob"] {
		t.Fatalf("expected alice and bob in by_user, got %+v", stats.ByUser)
	}
	if names["tbf-carol"] {
		t.Fatalf("carol must be excluded (all calls out of window), got %+v", stats.ByUser)
	}
	if callsByUser["tbf-alice"] != 1 || callsByUser["tbf-bob"] != 1 {
		t.Fatalf("expected alice=1, bob=1 in-window calls, got %+v", callsByUser)
	}
}

// TestAggregateCallStats_RawTokens verifies that CallStats.RawTokens is the
// UNMULTIPLIED token breakdown (summed verbatim from each row's raw
// prompt/completion/raw_total_tokens), independent of the multiplier-inflated
// Tokens field. Rows use explicit multipliers != 1 so the two breakdowns must
// differ, proving RawTokens is NOT derived by dividing the multiplied value.
func TestAggregateCallStats_RawTokens(t *testing.T) {
	database := newModelsTestDB(t)

	owner, err := models.CreateUser(
		database.Conn,
		"raw-stats-owner", "pw-hash", "sub-hash-raw", "sk-raw...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(owner) failed: %v", err)
	}

	// Each row carries an explicit multiplier so the billed Tokens diverge from
	// the raw Tokens. raw_total_tokens is written at insert as prompt+completion.
	insert := func(p, c, status int, m float64) {
		if _, err := models.InsertCallLog(database.Conn, &models.CallLog{
			UserID:           owner.ID,
			Model:            "glm-5.2",
			ProviderID:       "zhipu",
			PromptTokens:     p,
			CompletionTokens: c,
			TotalTokens:      p + c,
			MultiplierUsed:   m,
			EffectiveCalls:   1,
			StatusCode:       status,
		}); err != nil {
			t.Fatalf("InsertCallLog failed: %v", err)
		}
	}
	insert(100, 200, 200, 2.0) // raw 300, billed prompt=200 completion=400 total=600
	insert(50, 150, 200, 3.0)  // raw 200, billed prompt=150 completion=450 total=600
	insert(10, 20, 500, 0.5)   // raw 30,  billed prompt=5  completion=15  total=30

	stats, err := models.AggregateCallStats(database.Conn, models.CallLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCallStats returned error: %v", err)
	}

	// Raw breakdown: verbatim per-row sums, no multiplier applied.
	if stats.RawTokens.Prompt != 100+50+10 {
		t.Fatalf("RawTokens.Prompt expected %d, got %d", 160, stats.RawTokens.Prompt)
	}
	if stats.RawTokens.Completion != 200+150+20 {
		t.Fatalf("RawTokens.Completion expected %d, got %d", 370, stats.RawTokens.Completion)
	}
	if stats.RawTokens.Total != 300+200+30 {
		t.Fatalf("RawTokens.Total expected %d, got %d", 530, stats.RawTokens.Total)
	}

	// The persisted raw_total_tokens (prompt+completion per row) must equal the
	// raw total computed here; assert it independently via a direct SUM.
	var persistedRaw int
	if err := database.Conn.QueryRow(
		`SELECT COALESCE(SUM(raw_total_tokens), 0) FROM call_logs`,
	).Scan(&persistedRaw); err != nil {
		t.Fatalf("sum raw_total_tokens: %v", err)
	}
	if persistedRaw != 530 {
		t.Fatalf("persisted SUM(raw_total_tokens) expected 530, got %d", persistedRaw)
	}

	// Negative control: the multiplier-inflated Tokens.Total must NOT equal the
	// raw total (proving RawTokens is a distinct, unmultiplied figure).
	if stats.Tokens.Total == stats.RawTokens.Total {
		t.Fatalf("BUG: multiplied Tokens.Total (%d) equals raw RawTokens.Total (%d); multiplier not applied",
			stats.Tokens.Total, stats.RawTokens.Total)
	}
	// Sanity: multiplied total = 600 + 600 + 15 = 1215
	// (row3: ceil((10+20)*0.5) = ceil(15.0) = 15).
	if stats.Tokens.Total != 1215 {
		t.Fatalf("expected multiplied Tokens.Total == 1215, got %d", stats.Tokens.Total)
	}
}
