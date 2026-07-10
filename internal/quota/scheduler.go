package quota

import (
	"database/sql"
	"log"
	"sync"
	"time"

	"llm_api_gateway/internal/models"
)

// Scheduler handles periodic 5h window quota reset.
type Scheduler struct {
	db         *sql.DB
	resetHours int
	mu         sync.Mutex
	stopCh     chan struct{}
}

// NewScheduler creates a new quota reset scheduler.
func NewScheduler(db *sql.DB, resetHours int) *Scheduler {
	return &Scheduler{
		db:         db,
		resetHours: resetHours,
		stopCh:     make(chan struct{}),
	}
}

// Start begins the periodic quota reset check.
// It runs a compensation check immediately, then checks every 30 seconds.
func (s *Scheduler) Start() {
	// Immediate compensation on startup
	s.compensate()

	// Periodic check every 30 seconds
	ticker := time.NewTicker(30 * time.Second)
	go func() {
		for {
			select {
			case <-ticker.C:
				s.tryReset()
			case <-s.stopCh:
				ticker.Stop()
				return
			}
		}
	}()

	log.Printf("Quota scheduler started (reset interval: %dh, check every 30s)", s.resetHours)
}

// Stop halts the scheduler.
func (s *Scheduler) Stop() {
	close(s.stopCh)
	log.Println("Quota scheduler stopped")
}

// tryReset checks if we're at a window boundary and resets all 5h quotas.
func (s *Scheduler) tryReset() {
	now := time.Now()
	hours := now.Hour()
	minute := now.Minute()

	// Check if we're at a window boundary (hour % resetHours == 0 and minute == 0)
	if hours%s.resetHours != 0 || minute != 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := models.Reset5hQuota(s.db, s.resetHours); err != nil {
		log.Printf("ERROR: quota reset failed: %v", err)
	} else {
		log.Printf("Quota 5h reset completed at %s", now.Format(time.RFC3339))
	}
}

// compensate resets quotas for users whose window_start is before the current window.
// This handles the case where the server was down during a window transition.
func (s *Scheduler) compensate() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := models.CompensateQuotaReset(s.db, s.resetHours); err != nil {
		log.Printf("ERROR: quota compensation failed: %v", err)
	} else {
		log.Println("Quota compensation check completed")
	}
}
