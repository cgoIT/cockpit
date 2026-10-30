package store

import (
	"testing"
	"time"

	"cockpit/internal/jobs"
)

// Ein laufender Auftrag darf nicht als verwaist gelten, solange sein Zeitlimit noch läuft.
func TestJobStaleExceedsLongestTimeout(t *testing.T) {
	if jobStale <= jobs.MaxTimeout+time.Minute {
		t.Errorf("jobStale %v muss deutlich über %v liegen", jobStale, jobs.MaxTimeout)
	}
}
