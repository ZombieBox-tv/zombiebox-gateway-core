package playback

import "time"

// Adaptation requires two distinct recent samples agreeing on a lower ceiling.
// It never upgrades an active session or overrides a user-selected backend.
type Adaptation struct {
	Measured  time.Time
	Candidate string
	Matches   int
}

func (a *Adaptation) Observe(measured, now time.Time, candidate, current string) bool {
	if measured.IsZero() || now.Sub(measured) > 5*time.Minute || !measured.After(a.Measured) {
		return false
	}
	if measured.Sub(a.Measured) > 3*time.Minute {
		a.Matches = 0
	}
	a.Measured = measured
	rank := map[string]int{"": 0, "STANDARD": 1, "LOW": 2}
	if candidate == "" || rank[candidate] <= rank[current] {
		a.Candidate, a.Matches = "", 0
		return false
	}
	if a.Candidate != candidate {
		a.Candidate, a.Matches = candidate, 0
	}
	a.Matches++
	if a.Matches < 2 {
		return false
	}
	a.Matches = 0
	return true
}
