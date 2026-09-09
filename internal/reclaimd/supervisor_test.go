package reclaimd

import (
	"errors"
	"testing"
	"time"
)

// "Scan now" during a round used to return ok and do nothing: considerDisk
// skips a disk that is already scanning, and persistRound overwrites the
// schedule when the round ends, so the request was discarded either way. The
// button looked like it worked, which is worse than a refusal.
func TestRequestScanRefusesWhileARoundIsRunning(t *testing.T) {
	sup := &Supervisor{disks: map[string]*diskState{
		"d": {Key: "d", Present: true, Scanning: true},
	}}
	if err := sup.RequestScan("d"); !errors.Is(err, ErrScanInProgress) {
		t.Fatalf("got %v, want ErrScanInProgress", err)
	}
}

// The suppression window still outranks impatience, and an idle disk still
// takes the request.
func TestRequestScanHonoursSuppressionThenSchedulesNow(t *testing.T) {
	now := time.Now()
	sup := &Supervisor{disks: map[string]*diskState{
		"sup":  {Key: "sup", Present: true, Schedule: Schedule{SuppressUntil: now.Add(time.Hour)}},
		"idle": {Key: "idle", Present: true, Schedule: Schedule{NextScanAt: now.Add(90 * time.Hour)}},
	}}

	if err := sup.RequestScan("sup"); !errors.Is(err, ErrScanSuppressed) {
		t.Fatalf("suppressed disk: got %v, want ErrScanSuppressed", err)
	}
	if err := sup.RequestScan("idle"); err != nil {
		t.Fatalf("idle disk: %v", err)
	}
	if got := sup.disks["idle"].Schedule.NextScanAt; got.After(now.Add(time.Minute)) {
		t.Errorf("next scan is still %v away; the request did not take", time.Until(got))
	}
}
