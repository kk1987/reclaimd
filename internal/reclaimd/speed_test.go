package reclaimd

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSpeedRegionOffsetsSpanTheDisk(t *testing.T) {
	const blockSize = 4096
	size := int64(4096 * blockSize) // 16 MiB
	region := int64(1 << 20)
	offs := speedRegionOffsets(size, region, blockSize, 32<<20, 8)
	if len(offs) != 8 {
		t.Fatalf("got %d stretches, want 8: %v", len(offs), offs)
	}
	if offs[0] != 0 {
		t.Errorf("first stretch starts at %d, want the start of the disk", offs[0])
	}
	for i, off := range offs {
		if off%blockSize != 0 {
			t.Errorf("stretch %d at %d is not block aligned", i, off)
		}
		if off+region > size {
			t.Errorf("stretch %d at %d runs past the end of the disk", i, off)
		}
		if i > 0 && off <= offs[i-1] {
			t.Errorf("stretch %d at %d does not follow %d", i, off, offs[i-1])
		}
	}
	// The last one ends within a stretch of the end, so both edges are read.
	if last := offs[len(offs)-1]; last+region < size-region {
		t.Errorf("last stretch at %d leaves %d bytes of tail unsampled", last, size-last-region)
	}
}

// A stretch that spans a superblock starts on one, so it reads whole erase
// blocks and not the tail of one plus the head of the next.
func TestSpeedRegionOffsetsAlignToSuperblocks(t *testing.T) {
	const blockSize, seg = 1 << 20, 32 << 20
	offs := speedRegionOffsets(1<<30, 64<<20, blockSize, seg, 8)
	if len(offs) != 8 {
		t.Fatalf("got %v", offs)
	}
	for _, off := range offs {
		if off%seg != 0 {
			t.Errorf("stretch at %d is not on a %d boundary", off, seg)
		}
	}
	if got := speedRegionOffsets(1<<30, 64<<20, blockSize, seg, 1); len(got) != 1 || got[0] != 0 {
		t.Errorf("one stretch: got %v, want [0]", got)
	}
	if got := speedRegionOffsets(32<<20, 64<<20, blockSize, seg, 4); got != nil {
		t.Errorf("a disk smaller than one stretch: got %v, want nothing", got)
	}
}

func speedTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := fastConfig(t)
	cfg.SpeedRegions = 8
	cfg.SpeedRegionMiB = 1
	cfg.SpeedBudget = Duration(20 * time.Second)
	return cfg
}

func TestSpeedTestReadsEveryStretchFlatOut(t *testing.T) {
	disk := newFakeDisk(4096, 4096, time.Millisecond)
	res, err := SpeedTest(context.Background(), disk, speedTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != SpeedComplete || len(res.Regions) != 8 {
		t.Fatalf("outcome=%s regions=%d", res.Outcome, len(res.Regions))
	}
	for i, r := range res.Regions {
		if r.Blocks != 256 || r.Bytes != 1<<20 || r.StoppedBy != "" {
			t.Errorf("stretch %d: blocks=%d bytes=%d stopped=%q", i, r.Blocks, r.Bytes, r.StoppedBy)
		}
		// 1 MiB in 256 ms.
		if r.MiBs < 3.9 || r.MiBs > 3.91 {
			t.Errorf("stretch %d: %v MiB/s, want 3.9", i, r.MiBs)
		}
	}
	if res.Blocks != 2048 || res.Bytes != 8<<20 || res.MiBs < 3.9 || res.MiBs > 3.91 {
		t.Errorf("overall blocks=%d bytes=%d mibs=%v", res.Blocks, res.Bytes, res.MiBs)
	}
	if res.P50Ms != 1 || res.MaxMs != 1 || res.Slow != 0 {
		t.Errorf("p50=%v max=%v slow=%d", res.P50Ms, res.MaxMs, res.Slow)
	}
	if disk.reads != 2048 {
		t.Errorf("%d reads on the fake, want exactly the sample", disk.reads)
	}
}

// A slow drive answers in the same time as a fast one: each stretch stops
// when its share of the budget is spent, and says so.
func TestSpeedTestKeepsToItsBudget(t *testing.T) {
	disk := newFakeDisk(4096, 4096, 20*time.Millisecond) // 5 s per stretch, 2.5 s allowed
	cfg := speedTestConfig(t)
	res, err := SpeedTest(context.Background(), disk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != SpeedComplete || len(res.Regions) != 8 {
		t.Fatalf("outcome=%s regions=%d", res.Outcome, len(res.Regions))
	}
	for i, r := range res.Regions {
		if r.StoppedBy != SpeedStopBudget {
			t.Errorf("stretch %d ended by %q, want the budget", i, r.StoppedBy)
		}
		if r.Blocks < 124 || r.Blocks > 126 {
			t.Errorf("stretch %d read %d blocks, want about 125 (2.5 s at 20 ms)", i, r.Blocks)
		}
	}
	if res.ElapsedS > cfg.SpeedBudget.Duration().Seconds()+0.2 {
		t.Errorf("the test took %.1f s against a %v budget", res.ElapsedS, cfg.SpeedBudget.Duration())
	}
}

// A block that takes as long as a controller hang ends its stretch. The reads
// after a near-hang are the ones that drop the bus, and a speed test has no
// business finding that out.
func TestSpeedTestStopsAStretchAtANearHang(t *testing.T) {
	disk := newFakeDisk(4096, 4096, time.Millisecond)
	disk.sticky = true
	cfg := speedTestConfig(t)
	offs := speedRegionOffsets(disk.Size(), 1<<20, 4096, cfg.SegmentSize, 8)
	hang := offs[3]/4096 + 9 // the tenth block of the fourth stretch
	disk.degraded[hang] = 900 * time.Millisecond

	res, err := SpeedTest(context.Background(), disk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Regions[3]
	if r.StoppedBy != SpeedStopNearHang || r.Blocks != 10 || r.Slow != 1 || r.MaxMs != 900 {
		t.Errorf("stretch 3: %+v", r)
	}
	for i, o := range res.Regions {
		if i != 3 && (o.Blocks != 256 || o.StoppedBy != "") {
			t.Errorf("stretch %d was cut short too: %+v", i, o)
		}
	}
	if res.Slow != 1 || res.MaxMs != 900 || res.SlowestOffset != r.Offset {
		t.Errorf("overall slow=%d max=%v slowest=%d", res.Slow, res.MaxMs, res.SlowestOffset)
	}
}

func TestSpeedTestReportsADropout(t *testing.T) {
	disk := newFakeDisk(4096, 4096, time.Millisecond)
	cfg := speedTestConfig(t)
	offs := speedRegionOffsets(disk.Size(), 1<<20, 4096, cfg.SegmentSize, 8)
	disk.drops[offs[1]/4096+3] = true

	res, err := SpeedTest(context.Background(), disk, cfg)
	if !errors.Is(err, ErrDeviceDisconnected) {
		t.Fatalf("err=%v, want ErrDeviceDisconnected", err)
	}
	if res.Outcome != SpeedDropout || len(res.Regions) != 2 {
		t.Fatalf("outcome=%s regions=%d", res.Outcome, len(res.Regions))
	}
	if r := res.Regions[1]; r.StoppedBy != SpeedStopError || r.Blocks != 3 {
		t.Errorf("stretch 1: %+v", r)
	}
	// What was read before the dropout is still reported.
	if res.Regions[0].Blocks != 256 || res.Blocks != 259 {
		t.Errorf("blocks: first=%d total=%d", res.Regions[0].Blocks, res.Blocks)
	}
}

// One unreadable block is stepped over and counted. It does not end the
// stretch, and it does not go into the speed, since nothing was read.
func TestSpeedTestStepsOverAnUnreadableBlock(t *testing.T) {
	disk := newFakeDisk(4096, 4096, time.Millisecond)
	cfg := speedTestConfig(t)
	offs := speedRegionOffsets(disk.Size(), 1<<20, 4096, cfg.SegmentSize, 8)
	disk.media[offs[0]/4096+5] = true

	res, err := SpeedTest(context.Background(), disk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Regions[0]
	if r.Errors != 1 || r.Blocks != 255 || r.Bytes != 255*4096 || r.StoppedBy != "" {
		t.Errorf("stretch 0: %+v", r)
	}
	if res.Errors != 1 || res.Blocks != 2047 {
		t.Errorf("overall errors=%d blocks=%d", res.Errors, res.Blocks)
	}
}

func TestSpeedResultRoundTrip(t *testing.T) {
	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.LoadSpeed("d"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before any test: %v, want ErrNotFound", err)
	}
	want := SpeedResult{At: time.Now().Truncate(time.Second), BlockSize: 1 << 20, RegionMiB: 64,
		Regions: []SpeedRegion{{Offset: 0, Bytes: 64 << 20, Blocks: 64, MiBs: 110.5, P50Ms: 9}},
		Bytes:   64 << 20, Blocks: 64, MiBs: 110.5, MinMiBs: 110.5, MaxMiBs: 110.5, Outcome: SpeedComplete}
	if err := store.SaveSpeed("d", want); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadSpeed("d")
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != stateSchema || got.MiBs != want.MiBs || len(got.Regions) != 1 ||
		got.Regions[0].MiBs != 110.5 || !got.At.Equal(want.At) {
		t.Errorf("got %+v", got)
	}
}

// The speed test holds the disk the way a round does, and the same refusals
// apply in both directions.
func TestRequestSpeedTestRefusals(t *testing.T) {
	now := time.Now()
	sup := &Supervisor{disks: map[string]*diskState{
		"scanning": {Key: "scanning", Present: true, Scanning: true},
		"testing":  {Key: "testing", Present: true, Testing: true},
		"absent":   {Key: "absent", Present: false},
		"cooling":  {Key: "cooling", Present: true, Schedule: Schedule{SuppressUntil: now.Add(time.Hour)}},
	}}
	cases := map[string]error{
		"scanning": ErrScanInProgress,
		"testing":  ErrSpeedTestRunning,
		"absent":   ErrNotPresent,
		"cooling":  ErrScanSuppressed,
		"missing":  ErrNotFound,
	}
	for key, want := range cases {
		if err := sup.RequestSpeedTest(key, false); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", key, err, want)
		}
	}
	// And a round does not start on a disk that is being tested.
	if err := sup.RequestScan("testing", false); !errors.Is(err, ErrSpeedTestRunning) {
		t.Errorf("scan during a speed test: got %v, want ErrSpeedTestRunning", err)
	}
	if err := sup.Forget("testing"); !errors.Is(err, ErrSpeedTestRunning) {
		t.Errorf("forget during a speed test: got %v, want ErrSpeedTestRunning", err)
	}
}
