package quota

// Manager handles streaming quota pre-allocation and confirmation.
// Since the quota unit is call count (not tokens), pre-allocation equals confirmation
// — no rollback is needed. This manager exists for the streaming flow.
type Manager struct {
	checker *Checker
}

// NewManager creates a new quota manager.
func NewManager(checker *Checker) *Manager {
	return &Manager{checker: checker}
}

// PreReserve atomically deducts the effective call count for a streaming request.
// Since call count is predictable (1 × multiplier), this is a one-step process.
// Returns true if the reservation succeeded (quota was sufficient).
func (m *Manager) PreReserve(userID int64, effectiveCalls int) (bool, error) {
	return m.checker.CheckAndDeduct(userID, effectiveCalls)
}

// ConfirmStream is a no-op in the call-count model — the quota was already deducted
// during pre-reservation. Token stats are logged separately via call_logs.
func (m *Manager) ConfirmStream(userID int64, totalTokens int) error {
	// In the call-count model, quota is already deducted during pre-reservation.
	// Token count is recorded in call_logs for statistics only.
	_ = userID
	_ = totalTokens
	return nil
}
