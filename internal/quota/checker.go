// Package quota provides quota checking, deduction, and management.
package quota

import (
	"database/sql"
	"math"
	"time"

	"llm_api_gateway/internal/models"
)

// Checker handles quota verification and atomic deduction.
type Checker struct {
	db         *sql.DB
	multEng    *MultiplierEngine
	resetHours int
}

// NewChecker creates a new quota checker.
func NewChecker(db *sql.DB, multEng *MultiplierEngine, resetHours int) *Checker {
	return &Checker{
		db:         db,
		multEng:    multEng,
		resetHours: resetHours,
	}
}

// DB returns the underlying database connection (for call logging).
func (c *Checker) DB() *sql.DB {
	return c.db
}

// CheckAndDeduct verifies quota availability and atomically deducts effective calls.
// Returns (true, nil) if the deduction succeeded, (false, nil) if quota is insufficient.
func (c *Checker) CheckAndDeduct(userID int64, effectiveCalls int) (bool, error) {
	return models.AtomicDeductQuota(c.db, userID, effectiveCalls)
}

// GetEffectiveCalls computes the effective call count based on the current time multiplier.
func (c *Checker) GetEffectiveCalls() int {
	multiplier := c.multEng.GetEffectiveMultiplier(time.Now())
	return int(math.Ceil(1.0 * multiplier))
}

// GetCurrentMultiplier returns the current effective multiplier.
func (c *Checker) GetCurrentMultiplier() float64 {
	return c.multEng.GetEffectiveMultiplier(time.Now())
}

// CheckAvailability checks if the user has enough remaining quota without deducting.
// Returns (canUse, remaining5h, remainingTotal, error).
func (c *Checker) CheckAvailability(userID int64, effectiveCalls int) (bool, int, int, error) {
	quota, err := models.GetQuota(c.db, userID)
	if err != nil {
		return false, 0, 0, err
	}
	if quota == nil {
		return false, 0, 0, nil
	}

	remaining5h := quota.Quota5hLimit - quota.Quota5hUsed
	remainingTotal := quota.QuotaTotalLimit - quota.QuotaTotalUsed

	// A count limit of 0 means "unlimited" (call-count not restricted), unified
	// with the Token cap since 2026-07-21. Skip the per-dimension check when the
	// cap is 0 so a 0 cap opens the gate unconditionally (matching
	// AtomicDeductQuota's "(limit = 0 OR used + calls <= limit)" condition).
	countOk := (quota.Quota5hLimit == 0 || remaining5h >= effectiveCalls) &&
		(quota.QuotaTotalLimit == 0 || remainingTotal >= effectiveCalls)
	return countOk, remaining5h, remainingTotal, nil
}
