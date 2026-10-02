package reclaimd

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sharedDisk is a fakeDisk that somebody else is writing to, with the kernel
// counters to show for it.
//
// It models what a stick carrying a mounted filesystem does: one command at a
// time, so a read issued while a write is in flight comes back late by however
// long the write took, and the write shows up as completed in the counters by
// the time the read returns. The block that was read is as healthy as it was.
type sharedDisk struct {
	*fakeDisk
	// collide holds, per block, how long each successive read of it waits
	// behind a foreign write. A read past the end of the list waits for
	// nothing.
	collide map[int64][]time.Duration
	// always is a block every read of which collides.
	always map[int64]time.Duration

	stat    diskStat
	statErr error
}

func newSharedDisk(blocks int64, blockSize int, base time.Duration) *sharedDisk {
	return &sharedDisk{
		fakeDisk: newFakeDisk(blocks, blockSize, base),
		collide:  map[int64][]time.Duration{},
		always:   map[int64]time.Duration{},
	}
}

func (s *sharedDisk) ReadBlock(off int64) (time.Duration, error) {
	d, err := s.fakeDisk.ReadBlock(off)
	if err != nil {
		return d, err
	}
	s.stat.sectorsRead += uint64(s.blockSize) / sectorSize
	idx := off / int64(s.blockSize)
	if hold, ok := s.always[idx]; ok {
		s.stat.writes++
		return d + hold, nil
	}
	if q := s.collide[idx]; len(q) > 0 {
		s.collide[idx] = q[1:]
		s.stat.writes++
		return d + q[0], nil
	}
	return d, nil
}

// IOStats is the one part of a Platform a round's monitor asks for.
func (s *sharedDisk) IOStats(Presence) (diskStat, error) { return s.stat, s.statErr }

type statPlatform struct {
	Platform
	disk *sharedDisk
}

func (p statPlatform) IOStats(pr Presence) (diskStat, error) { return p.disk.IOStats(pr) }

// sharedConfig is fastConfig with the rate-based yield out of the way. A fake
// round runs in microseconds, so a single foreign write is thousands of writes
// a second, and the round would sit out the yield ladder for it.
func sharedConfig(t *testing.T) Config {
	t.Helper()
	cfg := fastConfig(t)
	cfg.ForeignWriteIOPS = 1e12
	cfg.ForeignReadMBps = 1e12
	return cfg
}

func sharedRound(t *testing.T, cfg Config, disk *sharedDisk) (RoundResult, *Store) {
	t.Helper()
	sc, store := newTestScanner(t, cfg)
	res, err := sc.Round(context.Background(), RoundInput{
		Key: "fake", Dev: disk, Schedule: NewSchedule(cfg, time.Now()),
		Ext: NewExternalIOMonitor(cfg, statPlatform{DefaultRoots(), disk}, Presence{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return res, store
}

// The case this exists for. A healthy stick under a mounted filesystem showed
// 117 slow blocks over 18 passes, at 117 different offsets, and every one was
// a read that had waited behind the filesystem's own write. The interval went
// to its floor and stayed there. Such a pass is a clean pass.
func TestContendedReadingsAreNotSlowBlocks(t *testing.T) {
	cfg := sharedConfig(t)
	const base = 10 * time.Millisecond
	disk := newSharedDisk(2048, cfg.BlockSize, base)
	// Three in the slow band and one past the danger line, as measured: the
	// waits ran from 50ms to a second.
	disk.collide[100] = []time.Duration{60 * time.Millisecond}
	disk.collide[700] = []time.Duration{300 * time.Millisecond}
	disk.collide[1500] = []time.Duration{55 * time.Millisecond}
	disk.collide[1900] = []time.Duration{700 * time.Millisecond}

	res, store := sharedRound(t, cfg, disk)
	sum := res.Summary

	if sum.Outcome != OutcomeClean || !sum.Completed {
		t.Fatalf("outcome = %q, completed = %v; want a clean full pass", sum.Outcome, sum.Completed)
	}
	if sum.SlowBlocks != 0 || sum.DangerBlocks != 0 {
		t.Errorf("slow = %d, danger = %d; a read held up by somebody else's write "+
			"is not a slow block", sum.SlowBlocks, sum.DangerBlocks)
	}
	if sum.Contended != 4 {
		t.Errorf("contended = %d, want 4", sum.Contended)
	}
	if sum.Deferred != 0 || sum.Healed != 0 {
		t.Errorf("deferred = %d, healed = %d; nothing was slow, so nothing is owed "+
			"and nothing healed", sum.Deferred, sum.Healed)
	}
	if sum.BlocksRead != 2048 {
		t.Errorf("read %d of 2048 blocks; a contended reading must not skip the "+
			"rest of its segment", sum.BlocksRead)
	}
	for _, idx := range []int64{100, 700, 1500, 1900} {
		if d, ok := DecodeLatency(res.Latency.Values[idx]); !ok || d >= res.Baseline.Slow {
			t.Errorf("block %d is on the map at %v; want the reading taken with the "+
				"disk quiet", idx, d)
		}
	}

	events, err := store.ListEvents("fake", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		switch e.Type {
		case EventContended:
			n++
			if e.Params["foreign_writes_n"] != float64(1) {
				t.Errorf("event at %d: foreign_writes_n = %v, want 1", e.Offset, e.Params["foreign_writes_n"])
			}
		case EventSlow, EventNearHang, EventHealed:
			t.Errorf("unexpected %s event at %d", e.Type, e.Offset)
		}
	}
	if n != 4 {
		t.Errorf("%d CONTENDED events, want 4", n)
	}

	// And the schedule reads it as the clean pass it was.
	sched := NewSchedule(cfg, time.Now())
	if next := sched.Next(sum, time.Now(), cfg); next.Interval <= sched.Interval {
		t.Errorf("interval went %v -> %v; a pass with only contended readings "+
			"should widen it", sched.Interval.Duration(), next.Interval.Duration())
	}
}

// With the monitor attached and nobody else on the disk, a slow reading is
// what it always was, and the block is not read a second time to check.
func TestSlowReadingOnAQuietDiskStands(t *testing.T) {
	cfg := sharedConfig(t)
	disk := newSharedDisk(1024, cfg.BlockSize, 10*time.Millisecond)
	disk.sticky = true
	disk.degraded[300] = 120 * time.Millisecond

	res, _ := sharedRound(t, cfg, disk)
	if res.Summary.SlowBlocks != 1 || res.Summary.Contended != 0 {
		t.Errorf("slow = %d, contended = %d; want 1 and 0",
			res.Summary.SlowBlocks, res.Summary.Contended)
	}
	if res.Summary.StillSlow != 1 {
		t.Errorf("still slow = %d, want 1: the block stays slow until written",
			res.Summary.StillSlow)
	}
}

// A block that really is slow, read while a write happened to be in flight.
// The first reading has an alibi and the second does not, so the block is
// found on this pass and not left for the next.
func TestSlowBlockUnderAForeignWriteIsStillFound(t *testing.T) {
	cfg := sharedConfig(t)
	disk := newSharedDisk(1024, cfg.BlockSize, 10*time.Millisecond)
	disk.sticky = true
	disk.degraded[300] = 120 * time.Millisecond
	disk.collide[300] = []time.Duration{80 * time.Millisecond}

	res, _ := sharedRound(t, cfg, disk)
	if res.Summary.SlowBlocks != 1 {
		t.Errorf("slow = %d, want 1: the re-read was slow with the disk to itself",
			res.Summary.SlowBlocks)
	}
	if res.Summary.Contended != 0 {
		t.Errorf("contended = %d, want 0: nothing was set aside", res.Summary.Contended)
	}
}

// When every reading of a block is slow and shared, there is no telling the
// queue from the block, and the cautious answer is the old one.
func TestContentionThatNeverLetsUpCountsAsSlow(t *testing.T) {
	cfg := sharedConfig(t)
	disk := newSharedDisk(1024, cfg.BlockSize, 10*time.Millisecond)
	disk.always[300] = 90 * time.Millisecond

	res, _ := sharedRound(t, cfg, disk)
	if res.Summary.SlowBlocks != 1 || res.Summary.Contended != 0 {
		t.Errorf("slow = %d, contended = %d; want 1 and 0",
			res.Summary.SlowBlocks, res.Summary.Contended)
	}
	// One reading in the sweep and one per retry, then the same again for the
	// re-probe, and no more: the retries are bounded. Every read of the block
	// is one foreign write in the counters.
	if want := 2 * (1 + cfg.ContentionRetries); int(disk.stat.writes) != want {
		t.Errorf("block 300 was read %d times, want %d", disk.stat.writes, want)
	}
}

// The re-probe's tally decides whether a drive gets written to, and a
// re-probe reading can wait behind a write as easily as a sweep's can. Here
// the slow block has healed, and one of the neighbours read alongside it
// collides: without the second look that comes out as still slow.
func TestContendedReprobeIsNotStillSlow(t *testing.T) {
	cfg := sharedConfig(t)
	disk := newSharedDisk(1024, cfg.BlockSize, 10*time.Millisecond)
	disk.degraded[300] = 120 * time.Millisecond
	// The sweep backs off at 300, so 301's first read is the re-probe's.
	disk.collide[301] = []time.Duration{200 * time.Millisecond}

	res, _ := sharedRound(t, cfg, disk)
	sum := res.Summary
	if sum.SlowBlocks != 1 || sum.Healed != 1 || sum.StillSlow != 0 {
		t.Errorf("slow = %d, healed = %d, still slow = %d; want 1, 1, 0",
			sum.SlowBlocks, sum.Healed, sum.StillSlow)
	}
	if sum.Contended != 1 {
		t.Errorf("contended = %d, want 1", sum.Contended)
	}
}

// A round that cannot read the counters has no way to know the disk was
// shared, and takes every reading as it comes.
func TestWithoutCountersEveryReadingStands(t *testing.T) {
	cfg := sharedConfig(t)
	disk := newSharedDisk(1024, cfg.BlockSize, 10*time.Millisecond)
	disk.collide[300] = []time.Duration{60 * time.Millisecond}
	disk.statErr = errors.New("no counters here")

	res, _ := sharedRound(t, cfg, disk)
	if res.Summary.SlowBlocks != 1 || res.Summary.Contended != 0 {
		t.Errorf("slow = %d, contended = %d; want 1 and 0: with the counters "+
			"unreadable there is no alibi to check", res.Summary.SlowBlocks, res.Summary.Contended)
	}
}

// The window around one read: our own sectors come out, anybody else's I/O
// stays in, and a write that finished during a rest before the read is not
// charged to it.
func TestWindowReportsOnlyForeignIO(t *testing.T) {
	cfg := mustConfig(t)
	disk := newSharedDisk(16, cfg.BlockSize, time.Millisecond)
	m := NewExternalIOMonitor(cfg, statPlatform{DefaultRoots(), disk}, Presence{})
	self := uint64(cfg.BlockSize) / sectorSize

	m.Begin()
	disk.stat.sectorsRead += self
	if f := m.End(cfg.BlockSize); f.Any() {
		t.Errorf("our own read came back as foreign: %+v", f)
	}

	m.Begin()
	disk.stat.sectorsRead += self + 8
	disk.stat.writes += 2
	if f := m.End(cfg.BlockSize); f.Writes != 2 || f.Sectors != 8 {
		t.Errorf("foreign = %+v, want 2 writes and 8 sectors", f)
	}

	// A write lands while the round rests. The next read opens its own
	// window, so the write is behind it.
	time.Sleep(windowReuse + 5*time.Millisecond)
	disk.stat.writes++
	m.Begin()
	disk.stat.sectorsRead += self
	if f := m.End(cfg.BlockSize); f.Any() {
		t.Errorf("a write from before the read was charged to it: %+v", f)
	}

	// Counters that went backwards mean the device re-enumerated, and the
	// difference means nothing.
	m.Begin()
	disk.stat = diskStat{}
	if f := m.End(cfg.BlockSize); f.Any() {
		t.Errorf("a counter reset came back as foreign I/O: %+v", f)
	}

	var none *ExternalIOMonitor
	none.Begin()
	if f := none.End(cfg.BlockSize); f.Any() {
		t.Errorf("a nil monitor reported %+v", f)
	}
}
