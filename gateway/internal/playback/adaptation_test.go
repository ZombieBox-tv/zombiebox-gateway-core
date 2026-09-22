package playback

import (
	"testing"
	"time"
)

func TestAdaptationRequiresIndependentSamplesAndNeverUpshifts(t *testing.T) {
	var a Adaptation
	now := time.Now()
	if a.Observe(now, now, "LOW", "") {
		t.Fatal("one sample changed playback")
	}
	if a.Observe(now, now, "LOW", "") {
		t.Fatal("replay changed playback")
	}
	if !a.Observe(now.Add(time.Minute), now.Add(time.Minute), "LOW", "") {
		t.Fatal("sustained low link ignored")
	}
	if a.Observe(now.Add(2*time.Minute), now.Add(2*time.Minute), "STANDARD", "LOW") {
		t.Fatal("active quality upshift")
	}
	if a.Observe(now.Add(3*time.Minute), now.Add(3*time.Minute), "LOW", "LOW") {
		t.Fatal("redundant restart")
	}
}

func TestAdaptationRejectsStaleAndResetsOnRecovery(t *testing.T) {
	var a Adaptation
	now := time.Now()
	a.Observe(now, now, "LOW", "")
	a.Observe(now.Add(time.Minute), now.Add(time.Minute), "", "")
	if a.Observe(now.Add(2*time.Minute), now.Add(2*time.Minute), "LOW", "") {
		t.Fatal("recovered link retained low vote")
	}
	if a.Observe(now.Add(3*time.Minute), now.Add(9*time.Minute), "LOW", "") {
		t.Fatal("stale report adapted")
	}
}
