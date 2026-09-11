package reclaimd

import (
	"context"
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

// A stick that is pulled before its probation ends leaves nothing worth
// keeping, and keeping it anyway is how disks/ grows an entry for every stick
// that was ever in a port for ten seconds. A stick with no serial would get
// one entry per port it was ever in.
func TestTickForgetsAnUnadoptedDiskThatWasPulled(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-2", "090c", "1000", "0011223344556677", "4", "2")
	f.addDisk("sda", "usb4/4-2", "0:0:0:0", "8:0", 125304832, true)

	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sup := NewSupervisor(mustConfig(t), store, quietLogger(), f.roots())
	sup.tick(context.Background())

	const key = "usb-090c:1000-0011223344556677"
	if _, ok := sup.disks[key]; !ok {
		t.Fatalf("discovery did not pick the disk up; have %v", sup.disks)
	}
	if keys, _ := store.ListDisks(); len(keys) != 1 {
		t.Fatalf("state directory holds %v, want the one disk", keys)
	}
	if !sup.disks[key].Meta.AdoptedAt.IsZero() {
		t.Fatal("the disk was adopted immediately; there is no probation left to test")
	}

	f.removeDisk("sda")
	sup.tick(context.Background())

	if _, ok := sup.disks[key]; ok {
		t.Error("the disk is still in the fleet after being pulled during probation")
	}
	if keys, _ := store.ListDisks(); len(keys) != 0 {
		t.Errorf("state directory still holds %v", keys)
	}
}

// The sweep is for junk, and neither of these is junk: one has history behind
// it, the other carries a decision. Forgetting the excluded one would mean a
// stick that comes back finds no record of having been switched off, and gets
// adopted half an hour later by the machinery its owner said no to.
func TestTickKeepsAbsentDisksThatCarryHistoryOrADecision(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-2", "090c", "1000", "SERIAL", "4", "2")
	f.addDisk("sda", "usb4/4-2", "0:0:0:0", "8:0", 125304832, true)
	f.removeDisk("sda") // the tree exists; nothing is plugged into it

	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	adopted := Meta{Identity: DiskIdentity{Key: "adopted"}, Enabled: true, AdoptedAt: time.Now()}
	excluded := Meta{Identity: DiskIdentity{Key: "excluded"}, Enabled: false}
	for _, m := range []Meta{adopted, excluded} {
		if err := store.SaveMeta(m.Identity.Key, m); err != nil {
			t.Fatal(err)
		}
	}

	sup := NewSupervisor(mustConfig(t), store, quietLogger(), f.roots())
	sup.disks = map[string]*diskState{
		"adopted":  {Key: "adopted", Present: true, Meta: adopted},
		"excluded": {Key: "excluded", Present: true, Meta: excluded},
	}
	sup.tick(context.Background())

	for _, key := range []string{"adopted", "excluded"} {
		st, ok := sup.disks[key]
		if !ok {
			t.Fatalf("%s was swept", key)
		}
		if st.Present {
			t.Errorf("%s is absent but still reports present", key)
		}
		if _, err := store.LoadMeta(key); err != nil {
			t.Errorf("%s: state gone: %v", key, err)
		}
	}
}

// Forgetting is the one destructive thing the API can be asked for. Mid-round
// it is refused. It cannot simply be queued, because persistRound would write
// the history straight back when the round ended.
func TestForgetRefusesMidRoundThenDeletesEverything(t *testing.T) {
	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const key = "usb-090c:1000-SERIAL"
	if err := store.SaveMeta(key, Meta{Identity: DiskIdentity{Key: key}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRound(key, RoundSummary{Seq: 1, Outcome: OutcomeClean}); err != nil {
		t.Fatal(err)
	}

	sup := &Supervisor{store: store, logger: quietLogger(),
		disks: map[string]*diskState{key: {Key: key, Scanning: true}}}

	if err := sup.Forget(key); !errors.Is(err, ErrScanInProgress) {
		t.Fatalf("mid-round: got %v, want ErrScanInProgress", err)
	}
	if keys, _ := store.ListDisks(); len(keys) != 1 {
		t.Fatalf("the refusal deleted state anyway: %v", keys)
	}
	if err := sup.Forget("nosuchdisk"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown key: got %v, want ErrNotFound", err)
	}

	sup.disks[key].Scanning = false
	if err := sup.Forget(key); err != nil {
		t.Fatal(err)
	}
	if _, ok := sup.disks[key]; ok {
		t.Error("the disk is still in the fleet")
	}
	if keys, _ := store.ListDisks(); len(keys) != 0 {
		t.Errorf("state directory still holds %v", keys)
	}
	if rounds, _ := store.ListRounds(key, 10); len(rounds) != 0 {
		t.Errorf("round history survived: %v", rounds)
	}
}

// Scan now is somebody deciding this stick is worth a round, and the waits
// before an automatic one (probation, the grace after boot) exist for scans
// nobody asked for. The request used to move next_scan_at and then sit out
// both, so the page called the scan overdue while nothing ran.
func TestRequestScanSkipsTheWaitsForUnaskedScans(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-2", "090c", "1000", "0011223344556677", "4", "2")
	f.addDisk("sda", "usb4/4-2", "0:0:0:0", "8:0", 125304832, true)
	f.write("proc/uptime", "60.00 55.00\n") // a minute after boot

	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	sup := NewSupervisor(mustConfig(t), store, quietLogger(), f.roots())
	sup.tick(ctx)
	const key = "usb-090c:1000-0011223344556677"
	st := sup.disks[key]
	if st == nil || !st.Meta.AdoptedAt.IsZero() {
		t.Fatalf("want a disk on probation, got %+v", st)
	}

	if err := sup.RequestScan(key, false); err != nil {
		t.Fatal(err)
	}
	sup.considerDisk(ctx, st, time.Now())
	sup.mu.Lock()
	adopted, started := !st.Meta.AdoptedAt.IsZero(), !st.ScanRequested
	sup.mu.Unlock()
	if !adopted || !started {
		t.Fatalf("after Scan now: adopted=%v, round started=%v", adopted, started)
	}

	// The round cannot open the fake node, so it ends at once. Wait it out.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		sup.mu.Lock()
		busy := st.Scanning
		sup.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the round never ended")
		}
	}

	// Unasked, the same disk, due, still waits for the machine to finish booting.
	sup.mu.Lock()
	st.Schedule.NextScanAt = time.Now()
	sup.mu.Unlock()
	sup.considerDisk(ctx, st, time.Now())
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if st.Scanning {
		t.Error("a scan nobody asked for started a minute after boot")
	}
}

// Scan now used to take effect on the next discovery tick, so the round began
// anywhere up to 30 seconds after the press, and the page spent that time on a
// disk that was neither scanning nor, as far as it could tell, asked to scan.
// The request wakes the loop instead, so this gives it far less than a tick.
func TestRequestScanStartsTheRoundWithoutWaitingForATick(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-2", "090c", "1000", "0011223344556677", "4", "2")
	f.addDisk("sda", "usb4/4-2", "0:0:0:0", "8:0", 125304832, true)

	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sup := NewSupervisor(mustConfig(t), store, quietLogger(), f.roots())
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { _ = sup.Run(ctx); close(stopped) }()
	defer func() { cancel(); <-stopped }()

	const key = "usb-090c:1000-0011223344556677"
	waitFor := func(what string, cond func(*diskState) bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			sup.mu.Lock()
			st := sup.disks[key]
			ok := st != nil && cond(st)
			sup.mu.Unlock()
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: nothing within 5s", what)
			}
		}
	}
	waitFor("first discovery", func(*diskState) bool { return true })

	if err := sup.RequestScan(key, false); err != nil {
		t.Fatal(err)
	}
	// The fake node cannot be opened, so the round ends as soon as it begins.
	// Adopted with the request spent is the proof that it began. No longer
	// scanning is what lets the store close under it.
	waitFor("round after Scan now", func(st *diskState) bool {
		return !st.ScanRequested && !st.Meta.AdoptedAt.IsZero() && !st.Scanning
	})
}

// Stop ends the round and nothing else. It is refused where there is no round
// to end, a second press counts as the same stop, and the log says where the
// pass was when somebody ended it. Without that, all that is left is a
// cancelled pass nobody can tell from a shutdown.
func TestStopEndsTheRunningRoundOnce(t *testing.T) {
	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	roundCtx, stop := context.WithCancel(context.Background())
	defer stop()
	sup := &Supervisor{store: store, logger: quietLogger(), disks: map[string]*diskState{
		"idle": {Key: "idle", Present: true},
		"busy": {Key: "busy", Present: true, Scanning: true, stop: stop,
			Live: &LiveProgress{RoundSeq: 3, PosMiB: 512, DoneMiB: 480}},
	}}

	if err := sup.StopScan("idle"); !errors.Is(err, ErrNotScanning) {
		t.Errorf("idle disk: got %v, want ErrNotScanning", err)
	}
	if err := sup.StopScan("nosuchdisk"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown key: got %v, want ErrNotFound", err)
	}
	for i := 0; i < 2; i++ {
		if err := sup.StopScan("busy"); err != nil {
			t.Fatalf("press %d: %v", i+1, err)
		}
	}
	if roundCtx.Err() == nil {
		t.Error("the round's context is still live")
	}
	if !sup.disks["busy"].StopRequested {
		t.Error("the disk does not report that it is stopping")
	}

	events, err := store.ListEvents("busy", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stops []Event
	for _, e := range events {
		if e.Type == EventStopped {
			stops = append(stops, e)
		}
	}
	if len(stops) != 1 {
		t.Fatalf("%d stop events, want one", len(stops))
	}
	if e := stops[0]; e.Round != 3 || e.Offset != 512<<20 {
		t.Errorf("stop recorded at round %d, offset %d; want round 3 at 512 MiB", e.Round, e.Offset)
	}
}

// A round interrupted before its first block used to be written down like any
// other: an empty latency map became the latest one, the waterfall grew a blank
// row, and the cursor went back to zero, because a round that never got past
// its baseline never set one. Pressing Scan now and then Stop straight after
// was enough to wipe the map off the page.
func TestARoundThatReadNothingOnlyMovesTheSchedule(t *testing.T) {
	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now()
	before := Schedule{
		Interval: Duration(240 * time.Hour), NextScanAt: now, Cursor: 96 << 20,
		RoundSeq: 7, LastRoundAt: now.Add(-48 * time.Hour), LastOutcome: OutcomeClean,
	}
	st := &diskState{Key: "d", Present: true, Schedule: before}
	sup := &Supervisor{cfg: mustConfig(t), store: store, logger: quietLogger(),
		disks: map[string]*diskState{"d": st}}

	sup.recordRound(st, RoundResult{Summary: RoundSummary{Seq: 8, Outcome: OutcomeCancelled}})

	if rounds, _ := store.ListRounds("d", 10); len(rounds) != 0 {
		t.Errorf("history grew %v", rounds)
	}
	if _, err := store.LoadProfile("d", 0, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("a latency map was saved: %v", err)
	}
	got := st.Schedule
	if got.Cursor != before.Cursor || got.RoundSeq != before.RoundSeq ||
		!got.LastRoundAt.Equal(before.LastRoundAt) || got.LastOutcome != before.LastOutcome {
		t.Errorf("schedule changed beyond its start time: %+v", got)
	}
	if got.NextScanAt.Before(now.Add(59 * time.Minute)) {
		t.Errorf("next attempt in %v; the next tick would start it straight back up",
			got.NextScanAt.Sub(now))
	}
	if saved, err := store.LoadSchedule("d"); err != nil || !saved.NextScanAt.Equal(got.NextScanAt) {
		t.Errorf("stored schedule %+v (%v) does not match", saved, err)
	}
}
