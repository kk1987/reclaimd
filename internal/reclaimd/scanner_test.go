package reclaimd

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeDisk is a scripted BlockReader. It is the only way the backoff logic gets
// tested at all: the failure it guards against is a router that needs its
// filesystem repaired, and that is not something to discover in production.
//
// It models the behaviour the tool depends on: reading a degraded block hands
// the controller its reclaim trigger, so the block is healthy on the next pass.
// That is what turns 110 dropouts into 10 and then 1.
type fakeDisk struct {
	size      int64
	blockSize int
	base      time.Duration

	degraded map[int64]time.Duration // block index -> latency while still bad
	drops    map[int64]bool          // block index -> hangs and drops the bus
	healed   map[int64]bool

	reported int // BlockSize() override; 0 means report blockSize honestly

	// sticky models the other kind of controller, the one that does not
	// reclaim a block on the strength of a slow read. A degraded block stays
	// degraded until something writes it, which is what the rewrite is for.
	sticky bool

	reads    int
	dropouts int
	// gone models the device leaving the bus: once it drops, every
	// subsequent read fails until the caller acknowledges by reattaching.
	gone bool
}

func newFakeDisk(blocks int64, blockSize int, base time.Duration) *fakeDisk {
	return &fakeDisk{
		size:      blocks * int64(blockSize),
		blockSize: blockSize,
		base:      base,
		degraded:  map[int64]time.Duration{},
		drops:     map[int64]bool{},
		healed:    map[int64]bool{},
	}
}

func (f *fakeDisk) Size() int64 { return f.size }

// BlockSize reports what the round will read with. reported exists so a test
// can hand back a size the fake cannot actually satisfy, which is the only way
// left to reach the misaligned path now that a round takes its geometry from
// the device instead of the config.
func (f *fakeDisk) BlockSize() int {
	if f.reported != 0 {
		return f.reported
	}
	return f.blockSize
}

func (f *fakeDisk) ReadBlock(off int64) (time.Duration, error) {
	if off%int64(f.blockSize) != 0 {
		return 0, ErrAlignment
	}
	if off < 0 || off >= f.size {
		return 0, ErrMediaError
	}
	f.reads++
	idx := off / int64(f.blockSize)

	if f.gone {
		return 0, ErrDeviceDisconnected
	}
	if f.drops[idx] && !f.healed[idx] {
		// The controller hangs for its watchdog interval and the device is
		// power-cycled off the bus. The reset itself is what reclaims the
		// block, so it reads normally afterwards.
		f.healed[idx] = true
		f.dropouts++
		f.gone = true
		return 1750 * time.Millisecond, ErrDeviceDisconnected
	}
	if d, ok := f.degraded[idx]; ok && !f.healed[idx] {
		if !f.sticky {
			f.healed[idx] = true
		}
		return d, nil
	}
	return f.base, nil
}

// reattach models the device coming back 5-6s later under a new name.
func (f *fakeDisk) reattach() { f.gone = false }

// cancellingDisk ends the round's context after a set number of reads, which is
// what pressing Stop partway through a pass looks like from inside it.
type cancellingDisk struct {
	*fakeDisk
	after  int
	cancel context.CancelFunc
}

func (c *cancellingDisk) ReadBlock(off int64) (time.Duration, error) {
	d, err := c.fakeDisk.ReadBlock(off)
	if c.reads == c.after {
		c.cancel()
	}
	return d, err
}

// seedSuperblockTails degrades the last block of every Nth segment, which is
// where 90.5% of the extreme-latency blocks landed in the forensics: the last
// 1 MiB of a 32 MiB superblock, the last wordline of an erase block.
func (f *fakeDisk) seedSuperblockTails(perSeg int, everyNth int, lat time.Duration) int {
	n := 0
	blocks := f.size / int64(f.blockSize)
	for seg := 0; int64(seg)*int64(perSeg) < blocks; seg++ {
		if seg%everyNth != 0 {
			continue
		}
		idx := int64(seg)*int64(perSeg) + int64(perSeg) - 1
		if idx < blocks {
			f.degraded[idx] = lat
			n++
		}
	}
	return n
}

// fastConfig removes the real cooldowns so a test round takes milliseconds.
// A real one is deliberately spread over hours.
func fastConfig(t *testing.T) Config {
	t.Helper()
	c := mustConfig(t)
	c.SlowCooldown = 0
	c.DangerCooldown = 0
	c.ReprobeDelay = 0
	c.ReprobeBudget = Duration(time.Minute)
	c.ReprobeSpacing = 0
	// No real device will ever reappear for a fake disk, so do not spend the
	// production reattach window discovering that.
	c.ReattachTimeout = Duration(50 * time.Millisecond)
	c.DutyFactorInit = 0
	c.DutyMinSleep = Duration(time.Hour) // never actually sleep
	c.MaxThroughputMBps = 0
	c.WarmupBlocks = 16
	c.WarmupDiscard = 0
	return c
}

func newTestScanner(t *testing.T, cfg Config) (*Scanner, *Store) {
	t.Helper()
	st, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewScanner(cfg, st, quietLogger(), DefaultRoots()), st
}

// TestConvergenceAcrossRounds checks that repeated rounds drive slow blocks
// and dropouts monotonically toward zero, the way the real stick went
// 1181 -> 258 -> 26 slow blocks and 110 -> 10 -> 1 dropouts.
func TestConvergenceAcrossRounds(t *testing.T) {
	cfg := fastConfig(t)
	perSeg := cfg.BlocksPerSegment()
	const blocks = 8192 // 8 GiB at 1 MiB blocks: 256 segments

	disk := newFakeDisk(blocks, cfg.BlockSize, 10*time.Millisecond)
	// Latency in the slow band. That matches the measured distribution, where
	// only 347 of the 1181 slow blocks were beyond 500ms, and it matters here
	// because a danger block trips the circuit breaker, which is a different
	// code path with its own test.
	seeded := disk.seedSuperblockTails(perSeg, 3, 120*time.Millisecond)
	if seeded == 0 {
		t.Fatal("test seeded no degraded blocks")
	}

	sc, store := newTestScanner(t, cfg)
	const key = "fake"
	sched := NewSchedule(cfg, time.Now())
	var owed segmentBitmap

	var slowPerRound []int
	for round := 0; round < 4; round++ {
		in := RoundInput{Key: key, Dev: disk, Schedule: sched, Deferred: owed}
		res, err := sc.Round(context.Background(), in)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		slowPerRound = append(slowPerRound, res.Summary.SlowBlocks+res.Summary.DangerBlocks)

		owed = res.Deferred
		sched = sched.Next(res.Summary, time.Now(), cfg)
		sched.Cursor = res.Cursor
		sched.RoundSeq = res.Summary.Seq
		sched.LearnedBaseline = Duration(BlendLearned(
			sched.LearnedBaseline.Duration(), res.Baseline.RoundP50))
		if err := store.AppendRound(key, res.Summary); err != nil {
			t.Fatal(err)
		}
	}

	t.Logf("slow+danger blocks per round: %v (seeded %d)", slowPerRound, seeded)
	if slowPerRound[0] == 0 {
		t.Fatal("round 1 found nothing; the fixture never exercised the backoff path")
	}
	for i := 1; i < len(slowPerRound); i++ {
		if slowPerRound[i] > slowPerRound[i-1] {
			t.Errorf("round %d found %d degraded blocks, up from %d -- reclaim is "+
				"supposed to make each pass cleaner than the last",
				i+1, slowPerRound[i], slowPerRound[i-1])
		}
	}
	if last := slowPerRound[len(slowPerRound)-1]; last != 0 {
		t.Errorf("still %d degraded blocks after 4 rounds; deferred segments are "+
			"not being drained", last)
	}
}

// Every degraded block must eventually be visited. Backing off is only safe
// because the debt is carried forward, so this is the property that keeps early
// backoff from quietly becoming "never scan the bad parts".
func TestDeferredSegmentsAreEventuallyDrained(t *testing.T) {
	cfg := fastConfig(t)
	perSeg := cfg.BlocksPerSegment()
	const blocks = 4096

	disk := newFakeDisk(blocks, cfg.BlockSize, 10*time.Millisecond)
	disk.seedSuperblockTails(perSeg, 2, 120*time.Millisecond)
	want := len(disk.degraded)

	sc, _ := newTestScanner(t, cfg)
	sched := NewSchedule(cfg, time.Now())
	var owed segmentBitmap

	for round := 0; round < 6; round++ {
		res, err := sc.Round(context.Background(),
			RoundInput{Key: "fake", Dev: disk, Schedule: sched, Deferred: owed})
		if err != nil {
			t.Fatal(err)
		}
		owed = res.Deferred
		sched = sched.Next(res.Summary, time.Now(), cfg)
		sched.Cursor = res.Cursor
		sched.RoundSeq = res.Summary.Seq
	}
	if got := len(disk.healed); got != want {
		t.Errorf("healed %d of %d degraded blocks; the rest were never revisited", got, want)
	}
}

// A round cut short by Stop or a shutdown used to leave ground behind: the
// cursor stepped past the segment it ended in, as it does past one that caused
// trouble, and owed segments it had not reached dropped out of the debt, since a
// round saves only what it deferred itself. Interrupted, it has to hand the next
// round both.
func TestAnInterruptedRoundResumesWhereItStopped(t *testing.T) {
	cfg := fastConfig(t)
	perSeg := cfg.BlocksPerSegment()
	segBytes := int64(perSeg) * int64(cfg.BlockSize)
	const blocks = 4096
	sc, _ := newTestScanner(t, cfg)

	run := func(owed segmentBitmap, stopAfter int) RoundResult {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		disk := &cancellingDisk{fakeDisk: newFakeDisk(blocks, cfg.BlockSize, 10*time.Millisecond),
			after: cfg.WarmupBlocks + stopAfter, cancel: cancel}
		sched := NewSchedule(cfg, time.Now())
		sched.Cursor = 40 * segBytes
		res, err := sc.Round(ctx, RoundInput{Key: "fake", Dev: disk, Schedule: sched, Deferred: owed})
		if err != nil {
			t.Fatal(err)
		}
		if res.Summary.Outcome != OutcomeCancelled {
			t.Fatalf("outcome %q, want %q", res.Summary.Outcome, OutcomeCancelled)
		}
		return res
	}

	// Stopped halfway into segment 41: the round finishes that segment and
	// notices at the start of 42, which it never read.
	if res := run(nil, perSeg+perSeg/2); res.Cursor != 42*segBytes {
		t.Errorf("sweeping: next round starts at segment %d, want 42", res.Cursor/segBytes)
	}

	// Stopped while draining the debt: the rotation has not moved, and the owed
	// segment it did not get to is still owed.
	owed := newSegmentBitmap(blocks / perSeg)
	owed.Set(10)
	owed.Set(20)
	res := run(owed, perSeg/2)
	if res.Cursor != 40*segBytes {
		t.Errorf("draining: next round starts at segment %d, want 40", res.Cursor/segBytes)
	}
	if !res.Deferred.Get(20) {
		t.Error("segment 20 was owed, never read, and is no longer owed")
	}
	if res.Deferred.Get(10) {
		t.Error("segment 10 was read in full and is still owed")
	}
}

// A dropout must persist its suppression window and stop the round. On a
// mounted overlay, carrying on after the backing store vanished is how a
// maintenance task turns into an outage.
func TestDropoutStopsRoundAndPersistsSuppression(t *testing.T) {
	cfg := fastConfig(t)
	const blocks = 4096
	disk := newFakeDisk(blocks, cfg.BlockSize, 10*time.Millisecond)
	disk.drops[2000] = true

	sc, store := newTestScanner(t, cfg)
	const key = "fake"
	sched := NewSchedule(cfg, time.Now())

	res, err := sc.Round(context.Background(),
		RoundInput{Key: key, Dev: disk, Schedule: sched})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Outcome != OutcomeDropout {
		t.Fatalf("outcome = %q, want %q", res.Summary.Outcome, OutcomeDropout)
	}
	if res.Summary.Dropouts != 1 {
		t.Errorf("dropouts = %d, want 1", res.Summary.Dropouts)
	}

	// The suppression must be on disk: losing power a second after a dropout
	// must not lose the window.
	prog, err := store.LoadProgress(key)
	if err != nil {
		t.Fatalf("progress was never persisted: %v", err)
	}
	if prog.SuppressUntil.IsZero() {
		t.Fatal("suppress_until not persisted on the dropout path")
	}
	if d := time.Until(prog.SuppressUntil); d < 23*time.Hour {
		t.Errorf("suppression is only %v, want ~24h", d)
	}
	if prog.LastOutcome != OutcomeDropout {
		t.Errorf("last_outcome = %q, want %q", prog.LastOutcome, OutcomeDropout)
	}

	// The resume point must be past the segment that caused it, so the next
	// round approaches it last, after a full idle window.
	if prog.Cursor <= 2000*int64(cfg.BlockSize) {
		t.Errorf("resume cursor %d does not clear the offending segment", prog.Cursor)
	}

	disk.reattach()
	sched = sched.Next(res.Summary, time.Now(), cfg)
	if ok, why := sched.Due(time.Now()); ok || why != CodeScanSuppressed {
		t.Errorf("a disk that just dropped should be suppressed, got due=%v why=%q", ok, why)
	}
}

// Three near-hangs is the point at which continuing stops being a calculated
// risk, because the next one may not come back.
func TestNearHangCircuitBreaker(t *testing.T) {
	cfg := fastConfig(t)
	perSeg := cfg.BlocksPerSegment()
	const blocks = 8192
	disk := newFakeDisk(blocks, cfg.BlockSize, 10*time.Millisecond)
	// Danger threshold lands at 500ms for a 10ms baseline.
	for i := 0; i < 6; i++ {
		disk.degraded[int64((i+1)*perSeg)-1] = 900 * time.Millisecond
	}

	sc, _ := newTestScanner(t, cfg)
	res, err := sc.Round(context.Background(), RoundInput{
		Key: "fake", Dev: disk, Schedule: NewSchedule(cfg, time.Now())})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Outcome != OutcomeNearHang {
		t.Fatalf("outcome = %q, want %q after repeated near-hangs",
			res.Summary.Outcome, OutcomeNearHang)
	}
	if res.Summary.DangerBlocks != cfg.MaxDangerPerRound {
		t.Errorf("danger blocks = %d, want the round to stop at %d",
			res.Summary.DangerBlocks, cfg.MaxDangerPerRound)
	}
}

// An alignment mistake is our bug and must surface as one. Reported as a media
// error, it would be blamed on the hardware for months.
func TestAlignmentErrorIsFatal(t *testing.T) {
	cfg := fastConfig(t)
	disk := newFakeDisk(1024, cfg.BlockSize, 10*time.Millisecond)
	sc, _ := newTestScanner(t, cfg)

	// A block size the fake cannot satisfy forces the misaligned path. It has
	// to come from the device: the round reads with the size the device was
	// opened at, so a config that disagrees can no longer be the cause.
	disk.reported = cfg.BlockSize + 4096

	_, err := sc.Round(context.Background(), RoundInput{
		Key: "fake", Dev: disk, Schedule: NewSchedule(cfg, time.Now())})
	if !errors.Is(err, ErrAlignment) {
		t.Fatalf("got %v, want ErrAlignment to propagate out of the round", err)
	}
}

func TestSegmentOrderDrainsDebtFirst(t *testing.T) {
	const segCount = 16
	owed := newSegmentBitmap(segCount)
	owed.Set(3)
	owed.Set(11)

	order := segmentOrder(segCount, owed, 8*32<<20, 1<<20, 32)
	if len(order) != segCount {
		t.Fatalf("order covers %d segments, want %d", len(order), segCount)
	}
	if order[0] != 3 || order[1] != 11 {
		t.Errorf("owed segments must come first, got %v", order[:4])
	}
	if order[2] != 8 {
		t.Errorf("rotation should resume at the cursor segment 8, got %d", order[2])
	}
	seen := map[int]bool{}
	for _, s := range order {
		if seen[s] {
			t.Fatalf("segment %d visited twice", s)
		}
		seen[s] = true
	}
}

// The cursor has to stay on a segment boundary, or the mod-32 structure
// analysis in the report silently shifts out of phase with the physical layout.
func TestNextCursorStaysSegmentAligned(t *testing.T) {
	const blockSize = int64(1 << 20)
	const perSeg = 32
	segBytes := int64(perSeg) * blockSize

	got := nextCursor(0, 12345*blockSize+7, OutcomeDropout, blockSize, perSeg, 61184)
	if got%segBytes != 0 {
		t.Errorf("cursor %d is not aligned to the %d-byte segment", got, segBytes)
	}
	if got <= 12345*blockSize {
		t.Errorf("cursor %d does not clear the offending segment", got)
	}
}

// The healing measurement must survive a round that ends early.
//
// A round that trips the danger circuit breaker is short by construction: the
// one this reproduces ran for 3m18s against a 10 minute re-probe delay. The
// re-probe used to step over every entry still inside its window and leave it
// "for next round", so healed and still-slow came back zero on exactly the
// disks the number exists to describe. The next round is an interval away and
// resumes from a cursor well past these offsets.
func TestShortRoundStillMeasuresHealing(t *testing.T) {
	cfg := fastConfig(t)
	// Non-zero, and longer than this round can possibly take.
	cfg.ReprobeDelay = Duration(40 * time.Millisecond)

	disk := newFakeDisk(4096, cfg.BlockSize, 10*time.Millisecond)
	// Three near-hangs is MaxDangerPerRound, so the sweep stops almost at once.
	for _, idx := range []int64{100, 200, 300} {
		disk.degraded[idx] = 700 * time.Millisecond
	}
	sc, _ := newTestScanner(t, cfg)

	res, err := sc.Round(context.Background(), RoundInput{
		Key: "fake", Dev: disk, Schedule: NewSchedule(cfg, time.Now())})
	if err != nil {
		t.Fatal(err)
	}

	if res.Summary.Outcome != OutcomeNearHang {
		t.Fatalf("outcome = %q, want %q", res.Summary.Outcome, OutcomeNearHang)
	}
	if res.Summary.Completed {
		t.Error("a round that stopped on the breaker reported a completed sweep")
	}
	if res.Summary.Healed == 0 {
		t.Errorf("healed = 0 after a short round; the re-probe skipped its own delay "+
			"(danger=%d, deferred=%d)", res.Summary.DangerBlocks, res.Summary.Deferred)
	}
	if res.Summary.StillSlow != 0 {
		t.Errorf("still slow = %d, want 0: the fake heals on the first read",
			res.Summary.StillSlow)
	}
}

// A pass that reaches the end of its segment list says so, which is what lets
// the scheduler tell "swept the disk and found slow blocks" apart from "quit
// after 8%".
func TestCleanRoundReportsCompleted(t *testing.T) {
	cfg := fastConfig(t)
	disk := newFakeDisk(512, cfg.BlockSize, 10*time.Millisecond)
	sc, _ := newTestScanner(t, cfg)

	res, err := sc.Round(context.Background(), RoundInput{
		Key: "fake", Dev: disk, Schedule: NewSchedule(cfg, time.Now())})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Outcome != OutcomeClean {
		t.Fatalf("outcome = %q, want %q", res.Summary.Outcome, OutcomeClean)
	}
	if !res.Summary.Completed {
		t.Error("a clean full sweep did not report itself completed")
	}
}
