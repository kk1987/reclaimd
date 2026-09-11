package reclaimd

import (
	"context"
	"sort"
	"time"
)

// Window is a fixed-size ring with an exact median.
//
// An exact sliding median is used instead of a streaming estimator like P².
// P² exists to bound memory, and 512 samples is 4 KB, so there is nothing to
// save. What P² costs is accuracy on the shape of data this program sees:
// heavy-tailed, where rare 100x outliers drag an estimator off for a long time
// afterwards. Here the outliers are the signal, and they must move the
// threshold as little as possible.
type Window struct {
	ring    []time.Duration
	pos     int
	full    bool
	scratch []time.Duration
}

func NewWindow(n int) *Window {
	return &Window{ring: make([]time.Duration, n), scratch: make([]time.Duration, 0, n)}
}

func (w *Window) Observe(d time.Duration) {
	w.ring[w.pos] = d
	w.pos++
	if w.pos == len(w.ring) {
		w.pos = 0
		w.full = true
	}
}

func (w *Window) Len() int {
	if w.full {
		return len(w.ring)
	}
	return w.pos
}

// Median reuses its scratch buffer, so a scanner calling this every 64 blocks
// allocates nothing.
func (w *Window) Median() time.Duration {
	n := w.Len()
	if n == 0 {
		return 0
	}
	w.scratch = append(w.scratch[:0], w.ring[:n]...)
	sort.Slice(w.scratch, func(i, j int) bool { return w.scratch[i] < w.scratch[j] })
	return w.scratch[n/2]
}

// Baseline holds the two different notions of "normal" the scanner needs, and
// they must not be the same number.
//
// RoundP50 is frozen after warm-up. A threshold that tracked a degrading disk
// would raise itself out of range exactly when the disk started needing it,
// and the detector would go quiet at the moment of failure.
//
// Rolling is live and feeds only the duty-cycle controller, where tracking
// drift is the point.
type Baseline struct {
	RoundP50 time.Duration
	Rolling  *Window
	Slow     time.Duration
	Danger   time.Duration
}

// thresholds turns a learned p50 into the two decision lines.
//
// They are multipliers on the p50 because the right number of milliseconds
// differs per disk. Against a measured 10ms p50 they produce 50ms and 500ms,
// which is exactly where the forensics put the two interesting populations.
// The heuristic reproduces the hand-picked values, which is the best evidence
// available that it is calibrated correctly.
//
// The floors matter for fast disks, where 5x a 2ms p50 would be pure noise. The
// ceilings matter when warm-up lands on a rough patch: a 60ms p50 would push
// the slow line to 300ms and miss most of what it exists to catch.
func thresholds(p50 time.Duration, cfg Config) (slow, danger time.Duration) {
	slow = clampDur(time.Duration(float64(p50)*cfg.SlowFactor),
		cfg.SlowFloor.Duration(), cfg.SlowCeil.Duration())
	danger = clampDur(time.Duration(float64(p50)*cfg.DangerFactor),
		cfg.DangerFloor.Duration(), cfg.DangerCeil.Duration())
	return slow, danger
}

func clampDur(v, lo, hi time.Duration) time.Duration {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// LearnBaseline reads a warm-up sample and freezes the round's thresholds.
//
// The measured danger line lands near 500ms, and that number carries the
// justification for the backoff strategy. The controller's hang is a constant
// 1500-1800ms, but by the time 1500ms has been measured the round is already
// lost: the device reset follows and a mounted overlay is already gone. 500ms
// is the last point at which stopping is still a choice. It also costs
// nothing: of the 347 blocks over 500ms in the forensics, 345 read normally on
// the next pass, so a read that reached 500ms has already handed the
// controller its reclaim trigger.
func LearnBaseline(ctx context.Context, r BlockReader, cfg Config, start int64,
	learned time.Duration) (Baseline, error) {

	blocks := int64(cfg.BlockSize)
	total := r.Size() / blocks
	samples := make([]time.Duration, 0, cfg.WarmupBlocks)

	off := start
	for attempt := 0; attempt < 3 && len(samples) < cfg.WarmupBlocks; attempt++ {
		for i := 0; i < cfg.WarmupBlocks; i++ {
			if err := ctx.Err(); err != nil {
				return Baseline{}, err
			}
			idx := (off/blocks + int64(i)) % total
			d, err := r.ReadBlock(idx * blocks)
			if err != nil {
				// A warm-up that trips over a bad patch is informative, but it
				// is not a baseline. Let the caller deal with the error.
				return Baseline{}, err
			}
			samples = append(samples, d)
		}
		if coherent(samples) {
			break
		}
		// The sample straddled something unusual. Extend the warm-up instead
		// of freezing a threshold derived from a patch that is not
		// representative.
		off += int64(len(samples)) * blocks
		samples = samples[:0]
	}

	p50 := medianOf(samples)
	if learned > 0 {
		// Clamp against cross-round knowledge so that one pathological warm-up
		// cannot desensitise, or over-sensitise, an entire round.
		p50 = clampDur(p50, learned/2, learned*2)
	}

	b := Baseline{RoundP50: p50, Rolling: NewWindow(512)}
	b.Slow, b.Danger = thresholds(p50, cfg)
	return b, nil
}

// coherent rejects a warm-up whose middle mass is spread too wide to be a
// baseline. Trimming both ends first keeps a single spike from vetoing an
// otherwise fine sample.
func coherent(s []time.Duration) bool {
	if len(s) < 16 {
		return false
	}
	c := append([]time.Duration(nil), s...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	lo := c[len(c)/10]
	hi := c[len(c)*9/10]
	return lo > 0 && hi <= lo*3
}

func medianOf(s []time.Duration) time.Duration {
	if len(s) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), s...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// BlendLearned folds this round's baseline into the persisted one so that the
// first round after a reboot does not start from nothing.
func BlendLearned(old, round time.Duration) time.Duration {
	if old <= 0 {
		return round
	}
	return time.Duration(0.8*float64(old) + 0.2*float64(round))
}
