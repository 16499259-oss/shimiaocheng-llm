package quota

import (
	"database/sql"
	"strings"
	"time"

	"llm_api_gateway/internal/timeutil"
)

// TimeMultiplier represents a time-based multiplier rule.
type TimeMultiplier struct {
	ID         int64   `json:"id"`
	StartTime  string  `json:"start_time"`
	EndTime    string  `json:"end_time"`
	Multiplier float64 `json:"multiplier"`
	DaysOfWeek string  `json:"days_of_week"`
	Enabled    bool    `json:"enabled"`
	CreatedAt  string  `json:"created_at"`
}

// MultiplierEngine evaluates time-based multiplier rules.
type MultiplierEngine struct {
	db *sql.DB
}

// NewMultiplierEngine creates a new multiplier engine.
func NewMultiplierEngine(db *sql.DB) *MultiplierEngine {
	return &MultiplierEngine{db: db}
}

// GetEffectiveMultiplier returns the maximum matching multiplier for the given time.
// Returns 1.0 if no rules match (baseline: 1 call per request).
//
// The incoming time is normalized to Asia/Shanghai before any comparison, so
// multiplier windows are always evaluated in UTC+8 regardless of the host's
// local time zone (fixes a latent local-time-zone bug).
func (m *MultiplierEngine) GetEffectiveMultiplier(now time.Time) float64 {
	now = now.In(timeutil.ShanghaiTZ)

	rules, err := m.FindAllEnabled()
	if err != nil {
		return 1.0
	}

	maxMultiplier := 1.0
	for _, rule := range rules {
		// 1. Check day-of-week match (time-zone-safe via timeutil)
		if !timeutil.MatchDay(rule.DaysOfWeek, now) {
			continue
		}

		// 2. Check time range match (time-zone-safe via timeutil)
		inRange := timeutil.IsInRange(rule.StartTime, rule.EndTime, now)

		// 3. If matched, take the maximum multiplier
		if inRange && rule.Multiplier > maxMultiplier {
			maxMultiplier = rule.Multiplier
		}
	}

	return maxMultiplier
}

// FindAllEnabled returns all enabled time multiplier rules.
func (m *MultiplierEngine) FindAllEnabled() ([]TimeMultiplier, error) {
	rows, err := m.db.Query(
		`SELECT id, start_time, end_time, multiplier, days_of_week, enabled, created_at
		 FROM time_multipliers WHERE enabled = 1 ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []TimeMultiplier
	for rows.Next() {
		var r TimeMultiplier
		var enabled int
		err := rows.Scan(&r.ID, &r.StartTime, &r.EndTime, &r.Multiplier, &r.DaysOfWeek, &enabled, &r.CreatedAt)
		if err != nil {
			return nil, err
		}
		r.Enabled = enabled == 1
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// FindAll returns all time multiplier rules (including disabled).
func (m *MultiplierEngine) FindAll() ([]TimeMultiplier, error) {
	rows, err := m.db.Query(
		`SELECT id, start_time, end_time, multiplier, days_of_week, enabled, created_at
		 FROM time_multipliers ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []TimeMultiplier
	for rows.Next() {
		var r TimeMultiplier
		var enabled int
		err := rows.Scan(&r.ID, &r.StartTime, &r.EndTime, &r.Multiplier, &r.DaysOfWeek, &enabled, &r.CreatedAt)
		if err != nil {
			return nil, err
		}
		r.Enabled = enabled == 1
		rules = append(rules, r)
	}
	if rules == nil {
		rules = []TimeMultiplier{}
	}
	return rules, rows.Err()
}

// Create inserts a new time multiplier rule.
func (m *MultiplierEngine) Create(startTime, endTime string, multiplier float64, daysOfWeek string) (*TimeMultiplier, error) {
	now := time.Now().Format(time.RFC3339)
	result, err := m.db.Exec(
		`INSERT INTO time_multipliers (start_time, end_time, multiplier, days_of_week, enabled, created_at)
		 VALUES (?, ?, ?, ?, 1, ?)`,
		startTime, endTime, multiplier, daysOfWeek, now,
	)
	if err != nil {
		return nil, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &TimeMultiplier{
		ID:         id,
		StartTime:  startTime,
		EndTime:    endTime,
		Multiplier: multiplier,
		DaysOfWeek: daysOfWeek,
		Enabled:    true,
		CreatedAt:  now,
	}, nil
}

// Update modifies an existing time multiplier rule.
func (m *MultiplierEngine) Update(id int64, updates map[string]any) error {
	setClauses := []string{}
	args := []any{}

	if v, ok := updates["start_time"]; ok {
		setClauses = append(setClauses, "start_time = ?")
		args = append(args, v)
	}
	if v, ok := updates["end_time"]; ok {
		setClauses = append(setClauses, "end_time = ?")
		args = append(args, v)
	}
	if v, ok := updates["multiplier"]; ok {
		setClauses = append(setClauses, "multiplier = ?")
		args = append(args, v)
	}
	if v, ok := updates["days_of_week"]; ok {
		setClauses = append(setClauses, "days_of_week = ?")
		args = append(args, v)
	}
	if v, ok := updates["enabled"]; ok {
		setClauses = append(setClauses, "enabled = ?")
		args = append(args, v)
	}

	if len(setClauses) == 0 {
		return nil
	}

	query := "UPDATE time_multipliers SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
	args = append(args, id)

	_, err := m.db.Exec(query, args...)
	return err
}

// Delete removes a time multiplier rule.
func (m *MultiplierEngine) Delete(id int64) error {
	_, err := m.db.Exec(`DELETE FROM time_multipliers WHERE id = ?`, id)
	return err
}

// GetByID returns a single time multiplier rule by ID.
func (m *MultiplierEngine) GetByID(id int64) (*TimeMultiplier, error) {
	r := &TimeMultiplier{}
	var enabled int
	err := m.db.QueryRow(
		`SELECT id, start_time, end_time, multiplier, days_of_week, enabled, created_at
		 FROM time_multipliers WHERE id = ?`, id,
	).Scan(&r.ID, &r.StartTime, &r.EndTime, &r.Multiplier, &r.DaysOfWeek, &enabled, &r.CreatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Enabled = enabled == 1
	return r, nil
}

// matchDay / isInTimeRange were moved to the timeutil package so that all
// window/routing/time-multiplier decisions share one Asia/Shanghai-aware
// implementation. See internal/timeutil.
