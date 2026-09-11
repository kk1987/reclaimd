package reclaimd

import (
	"context"
	"time"
)

// DutyController converts observed latency drift into rest time.
//
// It is a thermostat whose thermometer is the disk's own read latency: a full
// speed sweep on the drive under test walks the mean 1 MiB read from 7.5ms up
// to 12ms as it heats. There is no temperature sensor to read, and this signal
// is free.
//
// One caveat, which the UI prints next to the gauge: the same signal rises when
// the sweep enters a degraded region, so "hot" and "struggling" cannot be told
// apart here. Both warrant slowing down, so conflating them is harmless, but
// the number is not a temperature.
//
// The control law is multiplicative because what it fights is multiplicative: a
// controller sliding into read-retry gets 50x slower, not 50ms slower. An
// additive step would need hundreds of blocks to catch up with something that
// happens in ten.
type DutyController struct {
	cfg      Config
	factor   float64
	debt     time.Duration
	paceDebt time.Duration // owed to the throughput ceiling (see Pay)
	state    string
	drift    float64
	restFor  time.Duration

	// ref is this controller's own reference. It is deliberately not the
	// frozen threshold baseline.
	//
	// That baseline is learned cold, on the first few hundred blocks, and
	// freezing it is right for deciding what counts as a slow block. It is
	// wrong here. Reading heats the drive, so its steady-state latency is
	// higher than its cold-start latency: on the drive under test, 10 ms
	// against a 7.6 ms opening. A controller told to chase the cold number can
	// never reach it however long it rests, so it rests harder forever. A
	// measured full pass fell from 99 MiB/s to 16 MiB/s and stayed there,
	// turning a ten-minute scan into sixty-three.
	//
	// So the reference is taken once the loop has settled, and drift is
	// measured against the drive running warm.
	ref      time.Duration
	evals    int
	ups      int
	lastRoll time.Duration

	// sleep is sleepCtx. It is a field so a test can add up the rest without
	// actually sleeping.
	sleep func(context.Context, time.Duration) error
}

// settleEvals is how many evaluations pass before the duty reference is taken.
// At 64 blocks per evaluation this is a few hundred megabytes, by which point
// the throughput curve has flattened.
const settleEvals = 8

// maxFruitlessUps bounds the integral term. If resting this many times running
// has not brought the rolling latency down, resting is not the remedy: the
// sweep is in a region where the drive is simply slower, and heat is not the
// cause. Without this the factor ratchets to its clamp and stays there.
const maxFruitlessUps = 6

// Duty states, reported to the UI as codes.
const (
	DutyRunning = "RUNNING"
	DutyResting = "RESTING"
)

func NewDutyController(cfg Config) *DutyController {
	return &DutyController{cfg: cfg, factor: cfg.DutyFactorInit, state: DutyRunning, drift: 1,
		sleep: sleepCtx}
}

// Reference reports the latency drift is measured against, for the UI. Zero
// until the loop has settled.
func (c *DutyController) Reference() time.Duration { return c.ref }

func (c *DutyController) Factor() float64 { return c.factor }
func (c *DutyController) Drift() float64  { return c.drift }
func (c *DutyController) State() string   { return c.state }

// Evaluate adjusts the rest factor. It is called every DutyEvalEvery blocks.
// Evaluating on every block makes the loop hunt, because each correction
// changes the timing that produced the measurement.
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
		// Anti-windup. Resting is only worth doing while it is working. When
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
// Batching into 20ms chunks makes the delivered duty cycle match the requested
// one.
//
// The throughput ceiling keeps a debt of its own because the two are settled
// differently: the controller's rest is capped at DutyMaxSleep per sleep, and
// the ceiling's is paid in full, because a pace longer than that cap would
// otherwise leak. One sleep covers both, so it lasts as long as the larger of
// the two.
func (c *DutyController) Pay(ctx context.Context, last time.Duration) error {
	c.debt += time.Duration(c.factor * float64(last))

	// A hard ceiling independent of the drift signal, so a live overlay keeps
	// headroom no matter what the controller concludes. What each block falls
	// short of the pace is added up, since a single shortfall under
	// DutyMinSleep (at the default 60 MB/s and 1 MiB, every one of them) would
	// never come due on its own. A read slower than the pace pays down what
	// earlier ones owed but banks nothing, so a slow stretch cannot buy a
	// burst after it.
	if c.cfg.MaxThroughputMBps > 0 {
		pace := time.Duration(float64(c.cfg.BlockSize) /
			(c.cfg.MaxThroughputMBps * 1024 * 1024) * float64(time.Second))
		c.paceDebt = max(0, c.paceDebt+pace-last)
	}

	if max(c.debt, c.paceDebt) < c.cfg.DutyMinSleep.Duration() {
		c.state = DutyRunning
		return nil
	}
	d := max(min(c.debt, c.cfg.DutyMaxSleep.Duration()), c.paceDebt)
	c.debt, c.paceDebt = 0, 0
	c.state = DutyResting
	c.restFor = d
	err := c.sleep(ctx, d)
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
// It reads the kernel's I/O counters, which never touches the bus, so polling
// them cannot defeat USB autosuspend the way opening the device would.
type ExternalIOMonitor struct {
	cfg      Config
	platform Platform
	presence Presence

	lastSectorsRead uint64
	lastWrites      uint64
	lastSample      time.Time
	selfSectors     uint64
	selfWrites      uint64

	yieldStep int
	busySince time.Time
	foreignRd float64
	foreignWr float64
}

func NewExternalIOMonitor(cfg Config, pl Platform, p Presence) *ExternalIOMonitor {
	return &ExternalIOMonitor{cfg: cfg, platform: pl, presence: p}
}

type diskStat struct {
	sectorsRead uint64
	writes      uint64
	inFlight    uint64
}

// RecordSelfRead tells the monitor how much of the traffic is ours.
//
// The correction is done in sectors. The kernel merges requests (this disk
// already shows merges in field 4) but never invents sectors, so a sector
// count is exact where a request count is only approximate.
func (m *ExternalIOMonitor) RecordSelfRead(n int) {
	m.selfSectors += uint64(n) / sectorSize
}

// RecordSelfWrite counts one block written back by the rewrite phase. A block
// is at most one command's worth, so it lands in the kernel's counter as one
// request, which is the unit writes are measured in here.
func (m *ExternalIOMonitor) RecordSelfWrite() { m.selfWrites++ }

// Sample returns estimated foreign read bytes/s and write IOPS.
//
// Outside the rewrite phase writes need no correction: the daemon holds the
// device O_RDONLY, so every write in the counters belongs to somebody else.
// That makes writes the cleanest signal available and, on an f2fs overlay,
// the one that actually fires. During a rewrite the phase's own write-backs
// are taken out, or the monitor would yield to the daemon itself.
func (m *ExternalIOMonitor) Sample() (readBps, writeIOPS float64, err error) {
	st, err := m.platform.IOStats(m.presence)
	if err != nil {
		return 0, 0, err
	}
	now := time.Now()
	if m.lastSample.IsZero() {
		m.lastSectorsRead, m.lastWrites, m.lastSample = st.sectorsRead, st.writes, now
		m.selfSectors, m.selfWrites = 0, 0
		return 0, 0, nil
	}
	dt := now.Sub(m.lastSample).Seconds()
	if dt <= 0 {
		return m.foreignRd, m.foreignWr, nil
	}

	// Unsigned wraparound arithmetic. A counter that went backwards means it
	// was reset, which means the device re-enumerated. That is a free
	// cross-check on the dropout path, and the delta itself cannot be trusted.
	dRead := st.sectorsRead - m.lastSectorsRead
	dWrite := st.writes - m.lastWrites
	if st.sectorsRead < m.lastSectorsRead || st.writes < m.lastWrites {
		dRead, dWrite = 0, 0
	}

	foreignSectors := uint64(0)
	if dRead > m.selfSectors {
		foreignSectors = dRead - m.selfSectors
	}
	foreignWrites := uint64(0)
	if dWrite > m.selfWrites {
		foreignWrites = dWrite - m.selfWrites
	}
	m.foreignRd = float64(foreignSectors) * sectorSize / dt
	m.foreignWr = float64(foreignWrites) / dt

	m.lastSectorsRead, m.lastWrites, m.lastSample = st.sectorsRead, st.writes, now
	m.selfSectors, m.selfWrites = 0, 0
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
// These are rate thresholds, and the ladder backs off progressively, so a
// brief burst costs two seconds and sustained use costs the round.
func (m *ExternalIOMonitor) WaitIfBusy(ctx context.Context) error {
	rd, wr, err := m.Sample()
	if err != nil {
		return nil // counters unreadable: not a reason to stop scanning
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
