package reclaimd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DutyController converts observed latency drift into rest time.
//
// It is a thermostat whose thermometer is the disk's own read latency: a full
// speed sweep on the drive under test walks the mean 1 MiB read from 7.5ms up
// to 12ms as it heats. There is no temperature sensor to read, and this signal
// is free.
//
// Honest caveat, which the UI prints next to the gauge: the same signal rises
// when the sweep enters a degraded region, so "hot" and "struggling" cannot be
// told apart here. Both warrant slowing down, so conflating them is harmless --
// but it is a conflation, not a measurement of temperature.
//
// The control law is multiplicative because what it fights is multiplicative: a
// controller sliding into read-retry gets 50x slower, not 50ms slower. An
// additive step would need hundreds of blocks to catch up with something that
// happens in ten.
type DutyController struct {
	cfg     Config
	factor  float64
	debt    time.Duration
	state   string
	drift   float64
	restFor time.Duration

	// ref is this controller's OWN reference, and it is deliberately not the
	// frozen threshold baseline.
	//
	// That baseline is learned cold, on the first few hundred blocks, and
	// freezing it is right for deciding what counts as a slow block. It is
	// wrong here. Reading heats the drive, so its steady-state latency is
	// simply higher than its cold-start latency -- on the drive under test,
	// 10 ms against a 7.6 ms opening. A controller told to chase the cold
	// number can never reach it however long it rests, so it rests harder
	// forever: a measured full pass fell from 99 MiB/s to 16 MiB/s and stayed
	// there, turning a ten-minute scan into sixty-three.
	//
	// So the reference is taken once the loop has settled, and drift is
	// measured against the drive running warm rather than against the drive
	// standing still.
	ref      time.Duration
	evals    int
	ups      int
	lastRoll time.Duration
}

// settleEvals is how many evaluations pass before the duty reference is taken.
// At 64 blocks per evaluation this is a few hundred megabytes, by which point
// the throughput curve has flattened.
const settleEvals = 8

// maxFruitlessUps bounds the integral term. If resting this many times running
// has not brought the rolling latency down, resting is not the remedy: what is
// being measured is where the drive simply is slower, not a device getting hot.
// Without this the factor ratchets to its clamp and stays there.
const maxFruitlessUps = 6

// Duty states, reported to the UI as codes rather than sentences.
const (
	DutyRunning = "RUNNING"
	DutyResting = "RESTING"
)

func NewDutyController(cfg Config) *DutyController {
	return &DutyController{cfg: cfg, factor: cfg.DutyFactorInit, state: DutyRunning, drift: 1}
}

// Reference reports the latency drift is measured against, for the UI. Zero
// until the loop has settled.
func (c *DutyController) Reference() time.Duration { return c.ref }

func (c *DutyController) Factor() float64 { return c.factor }
func (c *DutyController) Drift() float64  { return c.drift }
func (c *DutyController) State() string   { return c.state }

// Evaluate adjusts the rest factor. It is called every DutyEvalEvery blocks
// rather than every block: evaluating continuously makes the loop hunt, because
// each correction changes the very timing that produced the measurement.
func (c *DutyController) Evaluate(rolling, baseline time.Duration) {
	if baseline <= 0 || rolling <= 0 {
		return
	}
	c.evals++

	// Hold the reference open until the throughput curve has flattened, then
	// take it. Falling back to the threshold baseline keeps the very first
	// evaluations meaningful.
	if c.ref == 0 {
		if c.evals < settleEvals {
			c.drift = float64(rolling) / float64(baseline)
			return
		}
		c.ref = rolling
	}

	c.drift = float64(rolling) / float64(c.ref)
	improved := c.lastRoll == 0 || rolling < c.lastRoll
	c.lastRoll = rolling

	switch {
	case c.drift > c.cfg.DutyDriftHigh:
		// Anti-windup. Resting is only worth doing while it is working; when
		// it is not, the drift is telling us about the drive's own variation
		// across its address space, which no amount of waiting will change.
		if c.ups >= maxFruitlessUps && !improved {
			break
		}
		c.factor *= c.cfg.DutyUp
		if improved {
			c.ups = 0
		} else {
			c.ups++
		}
	case c.drift < c.cfg.DutyDriftLow:
		c.factor *= c.cfg.DutyDown
		c.ups = 0
	default:
		// Deliberate dead band. Without it the controller oscillates around
		// the threshold instead of settling.
		c.ups = 0
	}
	if c.factor < 0 {
		c.factor = 0
	}
	if c.factor > c.cfg.DutyFactorMax {
		c.factor = c.cfg.DutyFactorMax
	}
}

// Pay accumulates rest debt and sleeps only once it is worth a syscall.
//
// Sub-millisecond sleeps do not survive the scheduler: asking for 400us costs
// more in wakeup overhead than it buys in rest, and gets rounded up anyway.
// Batching into 20ms chunks is what makes the requested duty cycle the one
// actually delivered.
func (c *DutyController) Pay(ctx context.Context, last time.Duration) error {
	c.debt += time.Duration(c.factor * float64(last))

	// A hard ceiling independent of the drift signal, so a live overlay keeps
	// headroom no matter what the controller concludes.
	if c.cfg.MaxThroughputMBps > 0 {
		floor := time.Duration(float64(c.cfg.BlockSize) /
			(c.cfg.MaxThroughputMBps * 1024 * 1024) * float64(time.Second))
		if floor > last && floor-last > c.debt {
			c.debt = floor - last
		}
	}

	if c.debt < c.cfg.DutyMinSleep.Duration() {
		c.state = DutyRunning
		return nil
	}
	d := c.debt
	if maxSleep := c.cfg.DutyMaxSleep.Duration(); d > maxSleep {
		d = maxSleep
	}
	c.debt = 0
	c.state = DutyResting
	c.restFor = d
	err := sleepCtx(ctx, d)
	c.state = DutyRunning
	return err
}

// RestRatio is the fraction of wall clock currently spent resting, used by the
// UI's ETA so that a drop in duty cycle does not silently invalidate it.
func (c *DutyController) RestRatio() float64 {
	return c.factor / (1 + c.factor)
}

// ---------------------------------------------------------------------------
// External I/O
// ---------------------------------------------------------------------------

// ExternalIOMonitor answers "is anybody else using this disk".
//
// It reads /proc/diskstats, a procfs read that never touches the bus -- so
// polling it, unlike opening the device, cannot defeat USB autosuspend.
type ExternalIOMonitor struct {
	cfg          Config
	roots        Roots
	major, minor int

	lastSectorsRead uint64
	lastWrites      uint64
	lastSample      time.Time
	selfSectors     uint64

	yieldStep int
	busySince time.Time
	foreignRd float64
	foreignWr float64
}

func NewExternalIOMonitor(cfg Config, roots Roots, p Presence) *ExternalIOMonitor {
	return &ExternalIOMonitor{cfg: cfg, roots: roots, major: p.Major, minor: p.Minor}
}

type diskStat struct {
	sectorsRead uint64
	writes      uint64
	inFlight    uint64
}

// readDiskstats locates the row by device NUMBER, not by name.
//
// After a re-enumeration the kernel name changes from sda to sdb, and a
// name-keyed lookup would quietly start describing a different disk -- or the
// same disk under a stale name, which is worse because it looks plausible.
func (m *ExternalIOMonitor) readDiskstats() (diskStat, error) {
	f, err := os.Open(filepath.Join(m.roots.Proc, "diskstats"))
	if err != nil {
		return diskStat{}, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 14 {
			continue
		}
		maj, err1 := strconv.Atoi(fields[0])
		min, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || maj != m.major || min != m.minor {
			continue
		}
		// Field indices are the post-5.5 layout: 5 sectors read, 7 writes
		// completed, 11 in-flight. Sectors are 512-byte units regardless of the
		// device's logical block size.
		sr, _ := strconv.ParseUint(fields[5], 10, 64)
		w, _ := strconv.ParseUint(fields[7], 10, 64)
		inf, _ := strconv.ParseUint(fields[11], 10, 64)
		return diskStat{sectorsRead: sr, writes: w, inFlight: inf}, nil
	}
	return diskStat{}, fmt.Errorf("no diskstats row for %d:%d", m.major, m.minor)
}

// RecordSelfRead tells the monitor how much of the traffic is ours.
//
// Corrections are done in SECTORS, not requests: the kernel merges requests --
// this disk already shows merges in field 4 -- but it never invents sectors, so
// sectors are exact where a request count is only approximate.
func (m *ExternalIOMonitor) RecordSelfRead(n int) {
	m.selfSectors += uint64(n) / sectorSize
}

// Sample returns estimated foreign read bytes/s and write IOPS.
//
// Writes need no correction at all: the daemon holds the device O_RDONLY, so
// every write in the counters belongs to somebody else. That makes writes both
// the cleanest signal available and, on an f2fs overlay, the one that actually
// fires.
func (m *ExternalIOMonitor) Sample() (readBps, writeIOPS float64, err error) {
	st, err := m.readDiskstats()
	if err != nil {
		return 0, 0, err
	}
	now := time.Now()
	if m.lastSample.IsZero() {
		m.lastSectorsRead, m.lastWrites, m.lastSample = st.sectorsRead, st.writes, now
		m.selfSectors = 0
		return 0, 0, nil
	}
	dt := now.Sub(m.lastSample).Seconds()
	if dt <= 0 {
		return m.foreignRd, m.foreignWr, nil
	}

	// Unsigned wraparound arithmetic. A counter that went backwards means it
	// was reset, which means the device re-enumerated -- a free cross-check on
	// the dropout path rather than a number to be trusted.
	dRead := st.sectorsRead - m.lastSectorsRead
	dWrite := st.writes - m.lastWrites
	if st.sectorsRead < m.lastSectorsRead || st.writes < m.lastWrites {
		dRead, dWrite = 0, 0
	}

	foreignSectors := uint64(0)
	if dRead > m.selfSectors {
		foreignSectors = dRead - m.selfSectors
	}
	m.foreignRd = float64(foreignSectors) * sectorSize / dt
	m.foreignWr = float64(dWrite) / dt

	m.lastSectorsRead, m.lastWrites, m.lastSample = st.sectorsRead, st.writes, now
	m.selfSectors = 0
	return m.foreignRd, m.foreignWr, nil
}

func (m *ExternalIOMonitor) busy(readBps, writeIOPS float64) bool {
	return writeIOPS > m.cfg.ForeignWriteIOPS ||
		readBps > m.cfg.ForeignReadMBps*1024*1024
}

// WaitIfBusy yields to whoever else is using the disk.
//
// "Stop on any foreign I/O" is not an option: on a router the overlay is
// written to continuously, and that policy would deadlock the scanner forever.
// These are rate thresholds, and the ladder backs off progressively so that a
// brief burst costs two seconds while sustained use costs the round.
func (m *ExternalIOMonitor) WaitIfBusy(ctx context.Context) error {
	rd, wr, err := m.Sample()
	if err != nil {
		return nil // diskstats unreadable: not a reason to stop scanning
	}
	if !m.busy(rd, wr) {
		m.yieldStep = 0
		m.busySince = time.Time{}
		return nil
	}
	if m.busySince.IsZero() {
		m.busySince = time.Now()
	}

	for m.busy(rd, wr) {
		if time.Since(m.busySince) > m.cfg.YieldGiveUp.Duration() {
			return ErrExternalBusy
		}
		wait := time.Duration(1<<m.yieldStep) * time.Second
		if wait > m.cfg.YieldMax.Duration() {
			wait = m.cfg.YieldMax.Duration()
		} else {
			m.yieldStep++
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
		rd, wr, err = m.Sample()
		if err != nil {
			return nil
		}
	}
	m.yieldStep = 0
	m.busySince = time.Time{}
	return nil
}
