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
	if err := sup.RequestScan("d", false); !errors.Is(err, ErrScanInProgress) {
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

	if err := sup.RequestScan("sup", false); !errors.Is(err, ErrScanSuppressed) {
		t.Fatalf("suppressed disk: got %v, want ErrScanSuppressed", err)
	}
	if err := sup.RequestScan("idle", false); err != nil {
		t.Fatalf("idle disk: %v", err)
	}
	if got := sup.disks["idle"].Schedule.NextScanAt; got.After(now.Add(time.Minute)) {
		t.Errorf("next scan is still %v away; the request did not take", time.Until(got))
	}
}

// The cooldown is refusable and overridable, and the override has to leave a
// trace: clearing a window that exists because the disk misbehaved is a
// decision somebody will want to find again next to whatever happened after.
func TestCooldownOverrideClearsTheWindowAndRecordsIt(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now()
	sup := &Supervisor{
		store: store, logger: quietLogger(),
		disks: map[string]*diskState{"d": {Key: "d", Present: true, Schedule: Schedule{
			LastOutcome:   OutcomeNearHang,
			SuppressUntil: now.Add(6 * time.Hour),
			NextScanAt:    now.Add(84 * time.Hour),
		}}},
	}

	if err := sup.RequestScan("d", false); !errors.Is(err, ErrScanSuppressed) {
		t.Fatalf("without the override: got %v, want ErrScanSuppressed", err)
	}
	if err := sup.RequestScan("d", true); err != nil {
		t.Fatalf("with the override: %v", err)
	}

	st := sup.disks["d"]
	if !st.Schedule.SuppressUntil.IsZero() {
		t.Errorf("window still ends at %v", st.Schedule.SuppressUntil)
	}
	if st.Schedule.NextScanAt.After(now.Add(time.Minute)) {
		t.Errorf("next scan is still %v away", time.Until(st.Schedule.NextScanAt))
	}

	events, err := store.ListEvents("d", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Type == EventOverride {
			found = true
			if e.Params["outcome"] != OutcomeNearHang {
				t.Errorf("event records outcome %v", e.Params["outcome"])
			}
		}
	}
	if !found {
		t.Error("the override left no event behind")
	}

	// And the disk is not scanned twice over just because somebody insisted.
	st.Scanning = true
	if err := sup.RequestScan("d", true); !errors.Is(err, ErrScanInProgress) {
		t.Errorf("override during a running round: got %v, want ErrScanInProgress", err)
	}
}
