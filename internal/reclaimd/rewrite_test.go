package reclaimd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Evidence
// ---------------------------------------------------------------------------

func TestAssessHealingVerdicts(t *testing.T) {
	cfg := RewriteConfig{MinRounds: 2, MinSamples: 8, StillSlowFraction: 0.5}
	r := func(healed, still int) RoundSummary { return RoundSummary{Healed: healed, StillSlow: still} }

	cases := []struct {
		name   string
		rounds []RoundSummary
		want   string
	}{
		{"nothing measured", []RoundSummary{r(0, 0), r(0, 0)}, HealUnknown},
		{"one round is not enough", []RoundSummary{r(0, 20)}, HealUnknown},
		{"two rounds but too few blocks", []RoundSummary{r(1, 2), r(0, 3)}, HealUnknown},
		// The Samsung on the router: everything it re-probes has healed.
		{"reads heal", []RoundSummary{r(3, 0), r(2, 0), r(4, 0)}, HealByRead},
		// The Lexar on the laptop: 11 healed, 39 still slow over four rounds.
		{"reads do not heal", []RoundSummary{r(5, 5), r(4, 6), r(1, 13), r(1, 15)}, HealByRewrite},
		{"rounds that measured nothing do not count", []RoundSummary{r(0, 10), r(0, 0), r(0, 0), r(0, 10)}, HealByRewrite},
		{"exactly at the threshold rewrites", []RoundSummary{r(4, 4), r(4, 4)}, HealByRewrite},
		{"just under does not", []RoundSummary{r(5, 4), r(4, 4)}, HealByRead},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := assessHealing(c.rounds, cfg); got.Verdict != c.want {
				t.Errorf("got %s (%+v), want %s", got.Verdict, got, c.want)
			}
		})
	}
}

// The window is the newest rounds that measured anything, so a drive whose
// old history was one kind of controller and whose recent history is the
// other follows the recent one.
func TestAssessHealingUsesTheNewestMeasuredRounds(t *testing.T) {
	cfg := RewriteConfig{MinRounds: 2, MinSamples: 8, StillSlowFraction: 0.5}
	var rounds []RoundSummary
	for i := 0; i < 10; i++ {
		rounds = append(rounds, RoundSummary{Healed: 0, StillSlow: 10})
	}
	for i := 0; i < healingWindow; i++ {
		rounds = append(rounds, RoundSummary{Healed: 10, StillSlow: 0})
	}
	ev := assessHealing(rounds, cfg)
	if ev.Verdict != HealByRead || ev.Rounds != healingWindow || ev.StillSlow != 0 {
		t.Fatalf("got %+v, want reads-heal over the last %d rounds", ev, healingWindow)
	}
}

// ---------------------------------------------------------------------------
// Fakes for the rewrite loop
// ---------------------------------------------------------------------------

// memTarget is the disk being rewritten: a byte slice with a write log.
type memTarget struct {
	mu         sync.Mutex
	data       []byte
	writes     []int64
	readFail   map[int64]error
	writeFail  map[int64]error
	writeDelay time.Duration
	syncs      int
	closed     bool
	exclusive  bool
	onWrite    func(off int64)
}

func newMemTarget(blocks int, blockSize int) *memTarget {
	m := &memTarget{data: make([]byte, blocks*blockSize),
		readFail: map[int64]error{}, writeFail: map[int64]error{}}
	for i := range m.data {
		m.data[i] = byte(i*7 + i/blockSize) // anything but zeros
	}
	return m
}

func (m *memTarget) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.readFail[off]; err != nil {
		return 0, err
	}
	return copy(p, m.data[off:]), nil
}

func (m *memTarget) WriteAt(p []byte, off int64) (int, error) {
	time.Sleep(m.writeDelay)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.writeFail[off]; err != nil {
		return 0, err
	}
	n := copy(m.data[off:], p)
	m.writes = append(m.writes, off)
	if m.onWrite != nil {
		m.onWrite(off)
	}
	return n, nil
}

func (m *memTarget) Sync() error  { m.mu.Lock(); defer m.mu.Unlock(); m.syncs++; return nil }
func (m *memTarget) Close() error { m.mu.Lock(); defer m.mu.Unlock(); m.closed = true; return nil }

// fakeFreezer logs every freeze and thaw in order.
type fakeFreezer struct {
	mu       sync.Mutex
	log      []string
	fail     error
	onFreeze func()
}

func (f *fakeFreezer) Freeze(point string) (fsHold, error) {
	if f.onFreeze != nil {
		f.onFreeze()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	f.log = append(f.log, "freeze "+point)
	return &fakeHold{f: f, point: point}, nil
}

func (f *fakeFreezer) entries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

type fakeHold struct {
	f     *fakeFreezer
	point string
}

func (h *fakeHold) Thaw() error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.log = append(h.f.log, "thaw "+h.point)
	return nil
}

// latencyDisk is the read-only side: a latency per block that the target's
// write hook can change, which is how "rewriting heals" is modelled.
type latencyDisk struct {
	mu        sync.Mutex
	size      int64
	blockSize int
	base      time.Duration
	slow      map[int64]time.Duration
}

func (d *latencyDisk) Size() int64    { return d.size }
func (d *latencyDisk) BlockSize() int { return d.blockSize }
func (d *latencyDisk) ReadBlock(off int64) (time.Duration, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v, ok := d.slow[off]; ok {
		return v, nil
	}
	return d.base, nil
}

func (d *latencyDisk) heal(off int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.slow, off)
}

// rewriteRig is one test's worth of fakes wired into a Rewriter.
type rewriteRig struct {
	rw      *Rewriter
	store   *Store
	tgt     *memTarget
	fr      *fakeFreezer
	disk    *latencyDisk
	lat     *LatencyMap
	pres    Presence
	sleeps  []time.Duration
	openErr error
}

const (
	rigBlocks    = 64
	rigBlockSize = 4096
)

// newRewriteRig builds a 64-block disk on a fake sysfs and procfs tree. The
// mountinfo argument is what the kernel would say is mounted from it.
func newRewriteRig(t *testing.T, cfg Config, mountinfo string) *rewriteRig {
	t.Helper()
	return newRewriteRigN(t, cfg, mountinfo, rigBlocks)
}

func newRewriteRigN(t *testing.T, cfg Config, mountinfo string, blocks int) *rewriteRig {
	t.Helper()
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-2", "090c", "1000", "0011223344556677", "4", "2")
	f.addDisk("sda", "usb4/4-2", "0:0:0:0", "8:0", int64(blocks)*rigBlockSize/sectorSize, true)
	f.write(filepath.Join("sys", "block", "sda", "sda1", "dev"), "8:1\n")
	f.write(filepath.Join("proc", "self", "mountinfo"),
		"25 1 259:3 / / rw,relatime shared:1 - btrfs /dev/nvme0n1p3 rw\n"+mountinfo)
	disks, err := DiscoverUSBDisks(f.roots())
	if err != nil {
		t.Fatal(err)
	}
	p := byKernelName(disks, "sda")
	if p == nil {
		t.Fatal("fake disk not discovered")
	}

	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	rig := &rewriteRig{
		store: store,
		tgt:   newMemTarget(blocks, rigBlockSize),
		fr:    &fakeFreezer{},
		disk: &latencyDisk{size: int64(blocks) * rigBlockSize, blockSize: rigBlockSize,
			base: 8 * time.Millisecond, slow: map[int64]time.Duration{}},
		lat:  NewLatencyMap(rigBlockSize, int64(blocks), 1, time.Now()),
		pres: *p,
	}
	// Writing a block is what makes it read normally again.
	rig.tgt.onWrite = rig.disk.heal

	rig.rw = newRewriter(cfg, store, quietLogger(), f.roots())
	rig.rw.freezer = rig.fr
	rig.rw.open = func(p Presence, exclusive bool) (refreshTarget, error) {
		if rig.openErr != nil {
			return nil, rig.openErr
		}
		rig.tgt.exclusive = exclusive
		return rig.tgt, nil
	}
	rig.rw.sleep = func(ctx context.Context, d time.Duration) error {
		rig.sleeps = append(rig.sleeps, d)
		return ctx.Err()
	}
	return rig
}

// markSlow makes these block indexes read slow, on the disk and in the map
// the round would have built.
func (r *rewriteRig) markSlow(idx ...int) []int64 {
	var offs []int64
	for _, i := range idx {
		off := int64(i) * rigBlockSize
		r.disk.slow[off] = 90 * time.Millisecond
		r.lat.Values[i] = EncodeLatency(90 * time.Millisecond)
		offs = append(offs, off)
	}
	for i := range r.lat.Values {
		if r.lat.Values[i] == LatSkipped {
			r.lat.Values[i] = EncodeLatency(8 * time.Millisecond)
		}
	}
	return offs
}

func (r *rewriteRig) run(t *testing.T) RewriteResult {
	t.Helper()
	return r.rw.Run(context.Background(), rewriteRequest{
		Key: r.pres.Identity.Key, Presence: r.pres, Dev: r.disk,
		Lat: r.lat, Slow: 40 * time.Millisecond,
	})
}

func rewriteTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := fastConfig(t)
	cfg.Rewrite.Enabled = true
	cfg.Rewrite.BatchHold = Duration(time.Nanosecond) // one block per freeze
	cfg.Rewrite.FreezeMax = Duration(10 * time.Second)
	return cfg
}

const f2fsOverlay = "21 23 8:1 / /overlay rw,relatime - f2fs /dev/sda1 rw,background_gc=on\n" +
	"28 21 8:1 /docker /overlay/docker rw,relatime shared:1 - f2fs /dev/sda1 rw\n"

// ---------------------------------------------------------------------------
// The rewrite loop
// ---------------------------------------------------------------------------

// The whole point, in one test: only the slow blocks are written, with the
// bytes that were already there, each batch inside a freeze of the one
// filesystem on the disk, and afterwards the blocks read normally.
func TestRewriteWritesSlowBlocksBackUnderAFreeze(t *testing.T) {
	rig := newRewriteRig(t, rewriteTestConfig(t), f2fsOverlay)
	want := rig.markSlow(3, 17, 31, 40, 63)
	before := bytes.Clone(rig.tgt.data)

	res := rig.run(t)

	if res.Code != "" || res.Err != nil {
		t.Fatalf("stopped with %s: %v", res.Code, res.Err)
	}
	if res.Mode != RewriteModeLive {
		t.Errorf("mode %s, want live", res.Mode)
	}
	if res.Candidates != 5 || res.Rewritten != 5 || res.Written != 5*rigBlockSize {
		t.Errorf("candidates=%d rewritten=%d written=%d", res.Candidates, res.Rewritten, res.Written)
	}
	if got := rig.tgt.writes; len(got) != len(want) {
		t.Fatalf("wrote %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("wrote %v, want %v", got, want)
			}
		}
	}
	if !bytes.Equal(rig.tgt.data, before) {
		t.Error("the disk's content changed; a write-back must be byte-identical")
	}

	// One freeze and one thaw per batch, and the bind mount does not get a
	// second freeze: it is the same filesystem.
	log := rig.fr.entries()
	if len(log) != 2*res.Batches || res.Batches != 5 {
		t.Fatalf("batches=%d, freezer log %v", res.Batches, log)
	}
	for i := 0; i < len(log); i += 2 {
		if log[i] != "freeze /overlay" || log[i+1] != "thaw /overlay" {
			t.Fatalf("freezer log out of order: %v", log)
		}
	}
	if rig.tgt.syncs != 5 {
		t.Errorf("synced %d times, want once per batch", rig.tgt.syncs)
	}
	if rig.tgt.exclusive {
		t.Error("opened O_EXCL under a mounted filesystem; that cannot succeed on a real kernel")
	}
	if !rig.tgt.closed {
		t.Error("target left open")
	}
	if _, err := rig.store.LoadFreezeMarker(); !errors.Is(err, ErrNotFound) {
		t.Errorf("freeze marker still present after a clean run: %v", err)
	}

	// The check afterwards found them all healed and updated the map.
	if !res.Verified || res.Healed != 5 || res.StillSlow != 0 {
		t.Errorf("verified=%v healed=%d still=%d", res.Verified, res.Healed, res.StillSlow)
	}
	for _, off := range want {
		d, ok := DecodeLatency(rig.lat.Values[off/rigBlockSize])
		if !ok || d >= 40*time.Millisecond {
			t.Errorf("map still says %v at %d after the rewrite", d, off)
		}
	}
	// A rest between batches, and none after the last one.
	gaps := 0
	for _, d := range rig.sleeps {
		if d == rig.rw.cfg.Rewrite.BatchGap.Duration() {
			gaps++
		}
	}
	if gaps != 4 {
		t.Errorf("slept %v, want four batch gaps", rig.sleeps)
	}
}

// The marker is what a restart uses to thaw a filesystem this process died
// holding, so it has to be on disk before the freeze goes in, and gone after
// the thaw.
func TestRewriteWritesTheMarkerBeforeFreezing(t *testing.T) {
	rig := newRewriteRig(t, rewriteTestConfig(t), f2fsOverlay)
	rig.markSlow(5)
	var seen bool
	var mounts []string
	rig.fr.onFreeze = func() {
		m, err := rig.store.LoadFreezeMarker()
		seen = err == nil
		mounts = m.Mounts
	}
	if res := rig.run(t); res.Code != "" {
		t.Fatalf("stopped with %s: %v", res.Code, res.Err)
	}
	if !seen || len(mounts) != 1 || mounts[0] != "/overlay" {
		t.Errorf("marker at freeze time: present=%v mounts=%v", seen, mounts)
	}
	if _, err := rig.store.LoadFreezeMarker(); !errors.Is(err, ErrNotFound) {
		t.Errorf("marker after the thaw: %v", err)
	}
}

func TestRewriteRefusesAnythingButF2fs(t *testing.T) {
	rig := newRewriteRig(t, rewriteTestConfig(t),
		"21 23 8:1 / /mnt/stick rw,relatime - ext4 /dev/sda1 rw\n")
	rig.markSlow(1, 2)
	res := rig.run(t)
	if res.Code != CodeRewriteUnsupportedFS {
		t.Fatalf("code %q, want %s (%v)", res.Code, CodeRewriteUnsupportedFS, res.Err)
	}
	if len(rig.tgt.writes) != 0 || len(rig.fr.entries()) != 0 || rig.tgt.closed {
		t.Errorf("wrote %v, froze %v, opened=%v: an unsupported filesystem must not be touched",
			rig.tgt.writes, rig.fr.entries(), rig.tgt.closed)
	}
}

// With nothing mounted the exclusive open is the guarantee, and no freeze is
// wanted or possible.
func TestRewriteOpensExclusivelyWhenNothingIsMounted(t *testing.T) {
	rig := newRewriteRig(t, rewriteTestConfig(t), "")
	want := rig.markSlow(8, 9)
	res := rig.run(t)
	if res.Code != "" {
		t.Fatalf("stopped with %s: %v", res.Code, res.Err)
	}
	if res.Mode != RewriteModeExclusive || !rig.tgt.exclusive {
		t.Errorf("mode=%s exclusive=%v", res.Mode, rig.tgt.exclusive)
	}
	if len(rig.fr.entries()) != 0 {
		t.Errorf("froze %v with nothing mounted", rig.fr.entries())
	}
	if len(rig.tgt.writes) != len(want) {
		t.Errorf("wrote %v, want %v", rig.tgt.writes, want)
	}
}

// Gate 4 again: a block that will not read is left exactly as it is.
func TestRewriteSkipsABlockThatFailsToRead(t *testing.T) {
	rig := newRewriteRig(t, rewriteTestConfig(t), f2fsOverlay)
	offs := rig.markSlow(10, 20, 30)
	rig.tgt.readFail[offs[1]] = ErrMediaError
	res := rig.run(t)
	if res.Code != "" {
		t.Fatalf("stopped with %s: %v", res.Code, res.Err)
	}
	if res.Rewritten != 2 || res.SkippedRead != 1 {
		t.Errorf("rewritten=%d skipped=%d", res.Rewritten, res.SkippedRead)
	}
	for _, w := range rig.tgt.writes {
		if w == offs[1] {
			t.Fatal("the unreadable block was written back")
		}
	}
}

// A write error ends the phase. It is not skipped over, because a disk that
// refuses a write is not one to keep writing to.
func TestRewriteStopsOnAWriteError(t *testing.T) {
	rig := newRewriteRig(t, rewriteTestConfig(t), f2fsOverlay)
	offs := rig.markSlow(10, 20, 30)
	rig.tgt.writeFail[offs[1]] = errors.New("write failed")
	res := rig.run(t)
	if res.Code != CodeRewriteWriteFailed {
		t.Fatalf("code %q, want %s", res.Code, CodeRewriteWriteFailed)
	}
	if res.Rewritten != 1 {
		t.Errorf("rewritten=%d, want 1", res.Rewritten)
	}
	log := rig.fr.entries()
	if len(log) != 4 || log[3] != "thaw /overlay" {
		t.Errorf("the filesystem was not thawed after the failure: %v", log)
	}
	if _, err := rig.store.LoadFreezeMarker(); !errors.Is(err, ErrNotFound) {
		t.Errorf("marker left behind: %v", err)
	}
}

// The watchdog thaws from its timer and the batch stops writing. The thaw
// happens once: the batch must not thaw again over the top of it.
func TestRewriteStopsWhenTheWatchdogFires(t *testing.T) {
	cfg := rewriteTestConfig(t)
	cfg.Rewrite.BatchHold = Duration(time.Hour)
	cfg.Rewrite.FreezeMax = Duration(20 * time.Millisecond)
	rig := newRewriteRig(t, cfg, f2fsOverlay)
	rig.markSlow(1, 2, 3, 4, 5)
	rig.tgt.writeDelay = 30 * time.Millisecond

	res := rig.run(t)

	if res.Code != CodeRewriteWatchdog {
		t.Fatalf("code %q, want %s (%v)", res.Code, CodeRewriteWatchdog, res.Err)
	}
	if res.Rewritten == 0 || res.Rewritten >= 5 {
		t.Errorf("rewritten=%d, want the block in flight and nothing after it", res.Rewritten)
	}
	if log := rig.fr.entries(); len(log) != 2 {
		t.Errorf("freezer log %v, want one freeze and exactly one thaw", log)
	}
	if _, err := rig.store.LoadFreezeMarker(); !errors.Is(err, ErrNotFound) {
		t.Errorf("marker left behind: %v", err)
	}
}

// The per-round cap is in MiB, so at 4 KiB blocks 1 MiB is 256 blocks: on a
// 300-block disk that is all slow, 44 wait for the next round.
func TestRewriteCapsOneRound(t *testing.T) {
	cfg := rewriteTestConfig(t)
	cfg.Rewrite.MaxPerRoundMiB = 1
	cfg.Rewrite.BatchHold = Duration(time.Hour)
	rig := newRewriteRigN(t, cfg, f2fsOverlay, 300)
	var idx []int
	for i := 0; i < 300; i++ {
		idx = append(idx, i)
	}
	rig.markSlow(idx...)
	res := rig.run(t)
	if res.Code != "" {
		t.Fatalf("stopped with %s: %v", res.Code, res.Err)
	}
	if res.Candidates != 300 || res.Rewritten != 256 || res.Truncated != 44 {
		t.Errorf("candidates=%d rewritten=%d truncated=%d", res.Candidates, res.Rewritten, res.Truncated)
	}
	if last := rig.tgt.writes[len(rig.tgt.writes)-1]; last != 255*rigBlockSize {
		t.Errorf("last write at %d; the cap should take the first blocks in disk order", last)
	}
}

func TestRewriteWithNothingSlowDoesNothing(t *testing.T) {
	rig := newRewriteRig(t, rewriteTestConfig(t), f2fsOverlay)
	rig.markSlow()
	res := rig.run(t)
	if res.Candidates != 0 || res.Rewritten != 0 || rig.tgt.closed || len(rig.fr.entries()) != 0 {
		t.Errorf("did something with nothing to do: %+v, opened=%v, froze=%v",
			res, rig.tgt.closed, rig.fr.entries())
	}
}

func TestRewriteReportsAFreezeThatFails(t *testing.T) {
	rig := newRewriteRig(t, rewriteTestConfig(t), f2fsOverlay)
	rig.markSlow(1)
	rig.fr.fail = errors.New("operation not permitted")
	res := rig.run(t)
	if res.Code != CodeRewriteFreezeFailed || res.Rewritten != 0 {
		t.Errorf("code=%s rewritten=%d err=%v", res.Code, res.Rewritten, res.Err)
	}
	if _, err := rig.store.LoadFreezeMarker(); !errors.Is(err, ErrNotFound) {
		t.Errorf("marker left behind after a failed freeze: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The round
// ---------------------------------------------------------------------------

// scannerRig runs a whole round against a fake disk whose controller does not
// reclaim on read, so that the rewrite is the only thing that can heal it.
type scannerRig struct {
	sc    *Scanner
	store *Store
	disk  *fakeDisk
	tgt   *memTarget
	fr    *fakeFreezer
	pres  Presence
	cfg   Config
}

func newScannerRig(t *testing.T, cfg Config, mountinfo string, priorRounds int) *scannerRig {
	t.Helper()
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-2", "090c", "1000", "0011223344556677", "4", "2")
	blocks := int64(512)
	f.addDisk("sda", "usb4/4-2", "0:0:0:0", "8:0", blocks*rigBlockSize/sectorSize, true)
	f.write(filepath.Join("sys", "block", "sda", "sda1", "dev"), "8:1\n")
	f.write(filepath.Join("proc", "self", "mountinfo"),
		"25 1 259:3 / / rw,relatime shared:1 - btrfs /dev/nvme0n1p3 rw\n"+mountinfo)
	disks, err := DiscoverUSBDisks(f.roots())
	if err != nil {
		t.Fatal(err)
	}
	p := byKernelName(disks, "sda")

	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	// The evidence from earlier rounds: everything re-probed stayed slow.
	for i := 0; i < priorRounds; i++ {
		if err := store.AppendRound(p.Identity.Key, RoundSummary{Seq: uint64(i + 1),
			Outcome: OutcomeSlow, StillSlow: 6}); err != nil {
			t.Fatal(err)
		}
	}

	cfg.BlockSize = rigBlockSize
	cfg.SegmentSize = 32 * rigBlockSize
	rig := &scannerRig{store: store, pres: *p, cfg: cfg,
		disk: newFakeDisk(blocks, rigBlockSize, 8*time.Millisecond),
		tgt:  newMemTarget(int(blocks), rigBlockSize),
		fr:   &fakeFreezer{}}
	rig.disk.sticky = true
	rig.tgt.onWrite = func(off int64) { rig.disk.healed[off/rigBlockSize] = true }

	rig.sc = NewScanner(cfg, store, quietLogger(), f.roots())
	rig.sc.rewriter.freezer = rig.fr
	rig.sc.rewriter.open = func(Presence, bool) (refreshTarget, error) { return rig.tgt, nil }
	rig.sc.rewriter.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return rig
}

func (r *scannerRig) round(t *testing.T, seq uint64) RoundResult {
	t.Helper()
	res, err := r.sc.Round(context.Background(), RoundInput{
		Key: r.pres.Identity.Key, Dev: r.disk, Presence: r.pres,
		Schedule: Schedule{RoundSeq: seq - 1, Interval: Duration(24 * time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRoundRewritesWhenReadsDoNotHeal(t *testing.T) {
	cfg := rewriteTestConfig(t)
	rig := newScannerRig(t, cfg, f2fsOverlay, 2)
	n := rig.disk.seedSuperblockTails(32, 4, 90*time.Millisecond)

	res := rig.round(t, 3)

	if res.Summary.SlowBlocks != n {
		t.Fatalf("found %d slow blocks, seeded %d", res.Summary.SlowBlocks, n)
	}
	// Reads did not heal them, so the re-probe said so...
	if res.Summary.Healed != 0 || res.Summary.StillSlow != n {
		t.Errorf("re-probe healed=%d still=%d, want 0 and %d", res.Summary.Healed, res.Summary.StillSlow, n)
	}
	// ...and the rewrite did.
	if res.Summary.Rewritten != n || res.Summary.RewriteHealed != n {
		t.Errorf("rewritten=%d rewriteHealed=%d, want %d and %d",
			res.Summary.Rewritten, res.Summary.RewriteHealed, n, n)
	}
	if len(rig.tgt.writes) != n {
		t.Errorf("%d writes, want %d", len(rig.tgt.writes), n)
	}
	for idx := range rig.disk.degraded {
		if !rig.disk.healed[idx] {
			t.Errorf("block %d still degraded after the round", idx)
		}
	}
	events, _ := rig.store.ListEvents(rig.pres.Identity.Key, 0, 0)
	var rewrite *Event
	for i := range events {
		if events[i].Type == EventRewrite {
			rewrite = &events[i]
		}
	}
	if rewrite == nil {
		t.Fatal("no REWRITE event")
	}
	// Numbers come back from the JSON log as float64.
	if rewrite.Params["mode"] != RewriteModeLive || rewrite.Params["rewritten_n"] != float64(n) {
		t.Errorf("REWRITE event params %v", rewrite.Params)
	}
	if rewrite.Params["healed_n"] != float64(n) {
		t.Errorf("REWRITE event healed_n=%v, want %d", rewrite.Params["healed_n"], n)
	}
}

// Off by default means off: the same drive, the same evidence, and nothing
// is written.
func TestRoundDoesNotRewriteUnlessAsked(t *testing.T) {
	cfg := rewriteTestConfig(t)
	cfg.Rewrite.Enabled = false
	rig := newScannerRig(t, cfg, f2fsOverlay, 2)
	rig.disk.seedSuperblockTails(32, 4, 90*time.Millisecond)

	res := rig.round(t, 3)

	if res.Summary.Rewritten != 0 || len(rig.tgt.writes) != 0 || len(rig.fr.entries()) != 0 {
		t.Errorf("rewritten=%d writes=%v freezes=%v with rewrite disabled",
			res.Summary.Rewritten, rig.tgt.writes, rig.fr.entries())
	}
}

// Enabled but unproven is still off. The first rounds on a drive only gather
// the evidence.
func TestRoundDoesNotRewriteWithoutEvidence(t *testing.T) {
	cfg := rewriteTestConfig(t)
	rig := newScannerRig(t, cfg, f2fsOverlay, 0)
	rig.disk.seedSuperblockTails(32, 4, 90*time.Millisecond)

	res := rig.round(t, 1)

	if res.Summary.StillSlow == 0 {
		t.Fatal("the re-probe measured nothing, so the test proves nothing")
	}
	if res.Summary.Rewritten != 0 || len(rig.tgt.writes) != 0 {
		t.Errorf("rewrote %d blocks on one round of evidence", res.Summary.Rewritten)
	}
}

// The disks list narrows the config to the keys named.
func TestRoundRewritesOnlyTheDisksNamed(t *testing.T) {
	cfg := rewriteTestConfig(t)
	cfg.Rewrite.Disks = []string{"usb-ffff:ffff-somebodyelse"}
	rig := newScannerRig(t, cfg, f2fsOverlay, 2)
	rig.disk.seedSuperblockTails(32, 4, 90*time.Millisecond)
	if res := rig.round(t, 3); res.Summary.Rewritten != 0 {
		t.Errorf("rewrote %d blocks on a disk the config did not name", res.Summary.Rewritten)
	}

	cfg.Rewrite.Disks = []string{"usb-090c:1000-0011223344556677"}
	rig = newScannerRig(t, cfg, f2fsOverlay, 2)
	rig.disk.seedSuperblockTails(32, 4, 90*time.Millisecond)
	if res := rig.round(t, 3); res.Summary.Rewritten == 0 {
		t.Error("did not rewrite the disk the config named")
	}
}

// ---------------------------------------------------------------------------
// Mounts
// ---------------------------------------------------------------------------

func TestMountsListsOneEntryPerFilesystem(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-2", "090c", "1000", "0011223344556677", "4", "2")
	f.addDisk("sda", "usb4/4-2", "0:0:0:0", "8:0", 125304832, true)
	f.write(filepath.Join("sys", "block", "sda", "sda1", "dev"), "8:1\n")
	f.write(filepath.Join("sys", "block", "sda", "sda2", "dev"), "8:2\n")
	f.write(filepath.Join("proc", "self", "mountinfo"),
		"25 1 259:3 / / rw,relatime shared:1 - btrfs /dev/nvme0n1p3 rw\n"+
			f2fsOverlay+
			"40 25 8:2 / /mnt/my\\040stick rw,relatime shared:3 - ext4 /dev/sda2 rw\n"+
			"41 25 8:16 / /mnt/other rw,relatime - vfat /dev/sdb rw\n")
	disks, err := DiscoverUSBDisks(f.roots())
	if err != nil {
		t.Fatal(err)
	}
	p := byKernelName(disks, "sda")

	got, err := f.roots().Mounts(*p)
	if err != nil {
		t.Fatal(err)
	}
	want := []Mount{
		{Point: "/overlay", FSType: "f2fs", Source: "/dev/sda1"},
		{Point: "/mnt/my stick", FSType: "ext4", Source: "/dev/sda2"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mount %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := f.roots().CheckNotInUse(*p); !errors.Is(err, ErrDeviceMounted) {
		t.Errorf("CheckNotInUse: got %v, want ErrDeviceMounted", err)
	}
}
