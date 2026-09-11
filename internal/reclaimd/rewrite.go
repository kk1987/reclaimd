package reclaimd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Does reading refresh this drive?
// ---------------------------------------------------------------------------

// Healing verdicts, reported to the UI as codes.
const (
	HealUnknown   = "UNKNOWN"       // not enough re-probe evidence yet
	HealByRead    = "READS_HEAL"    // the controller reclaims on its own
	HealByRewrite = "NEEDS_REWRITE" // it does not, so the host has to
)

// healingWindow is how many measured rounds the verdict is drawn from. Whether
// a controller reclaims on read is a property of its firmware, so the window
// only has to be long enough to average out one odd round.
const healingWindow = 6

// HealEvidence is the re-probe tally behind a verdict.
type HealEvidence struct {
	Verdict   string `json:"verdict"`
	Rounds    int    `json:"rounds_n"`
	Healed    int    `json:"healed_n"`
	StillSlow int    `json:"still_slow_n"`
}

// Samples is how many re-probed blocks the verdict rests on.
func (e HealEvidence) Samples() int { return e.Healed + e.StillSlow }

// StillSlowFraction is the share of them that were still slow.
func (e HealEvidence) StillSlowFraction() float64 {
	if e.Samples() == 0 {
		return 0
	}
	return float64(e.StillSlow) / float64(e.Samples())
}

// assessHealing reads the verdict off the re-probe results.
//
// A block that was slow, was read, and ten minutes later reads normally was
// reclaimed by the controller on the strength of that read. A block that is
// still slow was not. The re-probe measures exactly this, one block at a time,
// and the tally over a few rounds says which kind of controller the drive has.
// Blocks the filesystem overwrote in between are left out, since they read
// fast for a reason that says nothing about the controller.
func assessHealing(rounds []RoundSummary, cfg RewriteConfig) HealEvidence {
	var ev HealEvidence
	for i := len(rounds) - 1; i >= 0 && ev.Rounds < healingWindow; i-- {
		r := rounds[i]
		if r.Healed+r.StillSlow == 0 {
			continue
		}
		ev.Rounds++
		ev.Healed += r.Healed
		ev.StillSlow += r.StillSlow
	}
	switch {
	case ev.Rounds < cfg.MinRounds || ev.Samples() < cfg.MinSamples:
		ev.Verdict = HealUnknown
	case ev.StillSlowFraction() >= cfg.StillSlowFraction:
		ev.Verdict = HealByRewrite
	default:
		ev.Verdict = HealByRead
	}
	return ev
}

// ---------------------------------------------------------------------------
// The freeze
// ---------------------------------------------------------------------------

// fsFreezer is the seam the rewrite is tested through: what freezes a mounted
// filesystem and hands back the means to thaw it.
type fsFreezer interface {
	Freeze(point string) (fsHold, error)
}

// fsHold is one frozen filesystem. Thaw is called exactly once.
type fsHold interface {
	Thaw() error
}

// errFreezeLost means the watchdog thawed the filesystem before the batch was
// done with it. Nothing more may be written until it is frozen again.
var errFreezeLost = errors.New("freeze watchdog fired")

// freezeGuard is one frozen filesystem with a watchdog on it.
//
// A batch holds the guard's lock for the whole of one block's read and
// write-back, so the watchdog can never thaw between the two. If the watchdog
// fires, it waits for the block in flight, thaws, and every block after that
// is refused. The timer starts before the freeze is even asked for, because
// the freeze itself flushes dirty data and can take a while, and the bound is
// on how long the machine's writers wait, not on how long the batch runs.
type freezeGuard struct {
	point  string
	logger *slog.Logger
	timer  *time.Timer

	mu       sync.Mutex
	hold     fsHold
	released bool
	tripped  bool
	frozenAt time.Time
	held     time.Duration
}

func freezeWithWatchdog(fr fsFreezer, point string, maxHold time.Duration,
	logger *slog.Logger) (*freezeGuard, error) {

	g := &freezeGuard{point: point, logger: logger, frozenAt: time.Now()}
	g.timer = time.AfterFunc(maxHold, g.watchdog)

	hold, err := fr.Freeze(point)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.timer.Stop()
		return nil, err
	}
	g.hold = hold
	if g.tripped {
		// The watchdog fired while the freeze was still going in. The
		// filesystem is frozen now, so it is thawed here and at once, and the
		// caller gets told it never had it.
		g.thawLocked()
		return nil, errFreezeLost
	}
	return g, nil
}

func (g *freezeGuard) watchdog() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tripped = true
	if g.hold == nil || g.released {
		return
	}
	g.logger.Error("freeze watchdog fired; thawing the filesystem from the timer",
		"mount", g.point, "held_ms", time.Since(g.frozenAt).Milliseconds(),
		"code", CodeRewriteWatchdog)
	g.thawLocked()
}

// withHeld runs one block's read and write-back while nothing else can thaw.
func (g *freezeGuard) withHeld(fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tripped || g.released {
		return errFreezeLost
	}
	return fn()
}

// Release thaws, and reports how long the filesystem was frozen and whether
// the watchdog got there first.
func (g *freezeGuard) Release() (held time.Duration, tripped bool, err error) {
	g.timer.Stop()
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.released {
		err = g.thawLocked()
	}
	return g.held, g.tripped, err
}

func (g *freezeGuard) thawLocked() error {
	g.released = true
	g.held = time.Since(g.frozenAt)
	return g.hold.Thaw()
}

// ---------------------------------------------------------------------------
// The rewrite
// ---------------------------------------------------------------------------

// Rewrite modes, reported in the event.
const (
	RewriteModeLive      = "live"      // mounted f2fs, frozen around each batch
	RewriteModeExclusive = "exclusive" // nothing mounted, opened O_EXCL
)

// Rewriter writes slow blocks back in place. It is the daemon's only writer,
// and it runs only after assessHealing has said reads are not enough and the
// config has said yes.
type Rewriter struct {
	cfg      Config
	store    *Store
	logger   *slog.Logger
	platform Platform
	freezer  fsFreezer

	// open and sleep are seams for the tests. A rewrite that stops halfway
	// with the filesystem frozen is not something to discover on hardware.
	open  func(p Presence, exclusive bool) (refreshTarget, error)
	sleep func(context.Context, time.Duration) error
}

func newRewriter(cfg Config, store *Store, logger *slog.Logger, pl Platform) *Rewriter {
	return &Rewriter{
		cfg: cfg, store: store, logger: logger, platform: pl,
		freezer: defaultFreezer(),
		open:    openForRewrite,
		sleep:   sleepCtx,
	}
}

// openForRewrite opens the whole-disk node for writing. With nothing mounted
// it takes O_EXCL, the guarantee refresh relies on. Under a mounted filesystem
// O_EXCL cannot be had, and the freeze is what stands in for it.
func openForRewrite(p Presence, exclusive bool) (refreshTarget, error) {
	flags := refreshOpenFlags
	if !exclusive {
		flags &^= syscall.O_EXCL
	}
	return os.OpenFile(p.Node, flags, 0)
}

// rewriteRequest is what one round hands the rewriter.
type rewriteRequest struct {
	Key      string
	Presence Presence
	Dev      BlockReader // the round's read-only handle, for the check afterwards
	Ext      *ExternalIOMonitor
	Lat      *LatencyMap
	Slow     time.Duration

	// OnProgress is called as blocks are written back, with the offset and
	// the running count.
	OnProgress func(off int64, rewritten int)
}

// RewriteResult is what the phase did and why it stopped.
type RewriteResult struct {
	Mode        string
	Candidates  int // slow blocks this round found
	Truncated   int // of those, left for next round by the per-round cap
	Rewritten   int
	Written     int64
	SkippedRead int // blocks whose read failed, so nothing was written back
	Batches     int
	FreezeMax   time.Duration
	FreezeTotal time.Duration
	Healed      int // rewritten blocks that then read at normal speed
	StillSlow   int
	Verified    bool // the check afterwards ran to completion

	// Code says why the phase stopped before its candidates ran out, or why
	// it never started. Empty means it finished.
	Code string
	Err  error
	// Disconnected is the device leaving the bus, which the round has to
	// treat exactly as it would a dropout during the sweep.
	Disconnected bool
	DropOffset   int64
}

// slowBlocks lists the offsets the round measured at or past the slow line,
// in disk order.
func slowBlocks(lat *LatencyMap, slow time.Duration) []int64 {
	var out []int64
	bs := int64(lat.BlockSize)
	for i, v := range lat.Values {
		d, ok := DecodeLatency(v)
		if ok && d >= slow {
			out = append(out, int64(i)*bs)
		}
	}
	return out
}

// Run rewrites this round's slow blocks. It returns rather than fails: every
// way it can stop is written into the result for the round to record.
func (rw *Rewriter) Run(ctx context.Context, req rewriteRequest) RewriteResult {
	var res RewriteResult
	blockSize := int64(req.Lat.BlockSize)
	offsets := slowBlocks(req.Lat, req.Slow)
	res.Candidates = len(offsets)
	if cap := rw.cfg.Rewrite.MaxPerRoundMiB << 20 / blockSize; cap > 0 && int64(len(offsets)) > cap {
		res.Truncated = len(offsets) - int(cap)
		offsets = offsets[:cap]
	}
	if len(offsets) == 0 {
		return res
	}

	mounts, err := rw.platform.Mounts(req.Presence)
	if err != nil {
		res.Code, res.Err = CodeRewriteOpenFailed, fmt.Errorf("list mounts: %w", err)
		return res
	}
	for _, m := range mounts {
		if m.FSType != "f2fs" {
			// ext4 keeps its superblock and group descriptors pinned in the
			// block device's page cache, and a raw write underneath them
			// leaves an error on that cache that ext4 later reads as a
			// failed metadata writeback, at which point it can go read-only.
			// f2fs keeps its metadata in its own inodes and has no such
			// collision. Nothing else has been checked, so nothing else is
			// allowed.
			res.Code = CodeRewriteUnsupportedFS
			res.Err = fmt.Errorf("%s is mounted at %s as %s; only f2fs can be rewritten live",
				m.Source, m.Point, m.FSType)
			return res
		}
	}
	res.Mode = RewriteModeLive
	if len(mounts) == 0 {
		res.Mode = RewriteModeExclusive
	}

	tgt, err := rw.open(req.Presence, res.Mode == RewriteModeExclusive)
	if err != nil {
		res.Code, res.Err = CodeRewriteOpenFailed, err
		return res
	}
	defer tgt.Close()
	buf, err := alignedBuffer(int(blockSize))
	if err != nil {
		res.Code, res.Err = CodeRewriteOpenFailed, err
		return res
	}
	defer syscall.Munmap(buf)

	alive := func() bool { return rw.platform.Alive(req.Presence) }
	var done []int64
	hold := rw.cfg.Rewrite.BatchHold.Duration()

	for i := 0; i < len(offsets); {
		if err := ctx.Err(); err != nil {
			res.Code, res.Err = CodeRoundAborted, err
			break
		}
		if req.Ext != nil {
			if err := req.Ext.WaitIfBusy(ctx); err != nil {
				if errors.Is(err, ErrExternalBusy) {
					res.Code = CodeExternalIOBusy
				} else {
					res.Code = CodeRoundAborted
				}
				res.Err = err
				break
			}
		}

		guards, err := rw.freezeAll(req.Key, mounts)
		if err != nil {
			res.Code, res.Err = CodeRewriteFreezeFailed, err
			break
		}

		// Nothing in this loop logs or touches the store: on the router the
		// state directory lives on the filesystem that is frozen right now,
		// and a write to it would wait for a thaw this goroutine is holding.
		start := time.Now()
		var batchErr error
		for n := 0; i < len(offsets); n++ {
			off := offsets[i]
			// At least one block per freeze, or a hold shorter than a read
			// would freeze and thaw forever without writing anything.
			if res.Mode == RewriteModeLive && n > 0 && time.Since(start) >= hold {
				break
			}
			if err := ctx.Err(); err != nil {
				batchErr = err
				break
			}
			err := held(guards, func() error {
				if _, err := tgt.ReadAt(buf, off); err != nil {
					return readFailed{err}
				}
				_, err := tgt.WriteAt(buf, off)
				return err
			})
			var rf readFailed
			switch {
			case err == nil:
				i++
				res.Rewritten++
				res.Written += blockSize
				done = append(done, off)
				if req.Ext != nil {
					req.Ext.RecordSelfRead(int(blockSize))
					req.Ext.RecordSelfWrite()
				}
			case errors.As(err, &rf) && !isDisconnect(rf.err, alive):
				// Gate 4, as in refresh: a buffer that did not fill is never
				// written back. Leaving the block alone keeps whatever the
				// controller can still recover from it.
				i++
				res.SkippedRead++
			default:
				batchErr = err
			}
			if batchErr != nil {
				break
			}
		}
		res.Batches++
		heldFor, tripped, thawErr := rw.thawAll(req.Key, guards, len(mounts) > 0)
		res.FreezeTotal += heldFor
		res.FreezeMax = max(res.FreezeMax, heldFor)

		switch {
		case tripped || errors.Is(batchErr, errFreezeLost):
			res.Code, res.Err = CodeRewriteWatchdog, errFreezeLost
		case thawErr != nil:
			res.Code, res.Err = CodeRewriteFreezeFailed, thawErr
		case errors.Is(batchErr, context.Canceled), errors.Is(batchErr, context.DeadlineExceeded):
			res.Code, res.Err = CodeRoundAborted, batchErr
		case batchErr != nil:
			res.Err = batchErr
			var rf readFailed
			if errors.As(batchErr, &rf) {
				res.Err = rf.err
			}
			if isDisconnect(res.Err, alive) {
				res.Code, res.Disconnected = CodeDeviceDisconnected, true
				res.DropOffset = offsets[i]
			} else {
				res.Code = CodeRewriteWriteFailed
			}
		}
		if res.Code != "" {
			break
		}
		if req.OnProgress != nil {
			req.OnProgress(offsets[i-1], res.Rewritten)
		}
		// The flush happens after the thaw. The bytes are the ones that were
		// there, so nothing depends on the order, and the thaw is what every
		// waiting writer on the machine is queued behind.
		if err := tgt.Sync(); err != nil {
			if isDisconnect(err, alive) {
				res.Code, res.Err, res.Disconnected = CodeDeviceDisconnected, err, true
				res.DropOffset = offsets[i-1]
			} else {
				res.Code, res.Err = CodeRewriteWriteFailed, err
			}
			break
		}
		if i < len(offsets) {
			if err := rw.sleep(ctx, rw.cfg.Rewrite.BatchGap.Duration()); err != nil {
				res.Code, res.Err = CodeRoundAborted, err
				break
			}
		}
	}

	if len(done) > 0 && !res.Disconnected && ctx.Err() == nil {
		rw.verify(ctx, req, done, &res)
	}
	return res
}

// readFailed marks a read error apart from a write error inside a batch,
// since the two get opposite treatment: a failed read skips the block and a
// failed write ends the phase.
type readFailed struct{ err error }

func (r readFailed) Error() string { return r.err.Error() }
func (r readFailed) Unwrap() error { return r.err }

// held runs fn under every guard. With no mounts there are no guards, and the
// exclusive open is what keeps everything else off the disk.
func held(guards []*freezeGuard, fn func() error) error {
	if len(guards) == 0 {
		return fn()
	}
	return guards[0].withHeld(func() error { return held(guards[1:], fn) })
}

// freezeAll freezes every filesystem on the disk, marker first. If any one of
// them refuses, the ones already frozen are thawed before the error goes back.
func (rw *Rewriter) freezeAll(key string, mounts []Mount) ([]*freezeGuard, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	points := make([]string, 0, len(mounts))
	for _, m := range mounts {
		points = append(points, m.Point)
	}
	if err := rw.store.SaveFreezeMarker(FreezeMarker{Disk: key, Mounts: points}); err != nil {
		return nil, fmt.Errorf("write freeze marker: %w", err)
	}
	var guards []*freezeGuard
	for _, pt := range points {
		g, err := freezeWithWatchdog(rw.freezer, pt, rw.cfg.Rewrite.FreezeMax.Duration(), rw.logger)
		if err != nil {
			// The marker went down before the first freeze, so it comes up
			// with the rest whether or not anything was actually frozen.
			rw.thawAll(key, guards, true)
			return nil, err
		}
		guards = append(guards, g)
	}
	return guards, nil
}

// thawAll releases every guard and then the marker, and reports the longest
// hold among them, which is what the machine's writers waited. marked says a
// marker was written for this batch, which is so for every batch on a mounted
// disk, including one whose freeze failed.
func (rw *Rewriter) thawAll(key string, guards []*freezeGuard, marked bool) (held time.Duration, tripped bool, err error) {
	for _, g := range guards {
		h, t, e := g.Release()
		held = max(held, h)
		tripped = tripped || t
		if e != nil && err == nil {
			err = e
		}
	}
	if marked {
		if e := rw.store.ClearFreezeMarker(); e != nil && err == nil {
			err = fmt.Errorf("clear freeze marker: %w", e)
		}
	}
	return held, tripped, err
}

// verify reads the rewritten blocks back after the same delay the re-probe
// waits, and for the same reason: the controller's cache would otherwise
// answer, and answer fast, for a block that is still slow on the NAND. This is
// where "rewriting heals this drive" gets measured rather than assumed, and
// the round's map is updated with what it finds.
func (rw *Rewriter) verify(ctx context.Context, req rewriteRequest, done []int64, res *RewriteResult) {
	if err := rw.sleep(ctx, rw.cfg.ReprobeDelay.Duration()); err != nil {
		return
	}
	blockSize := int64(req.Lat.BlockSize)
	deadline := time.Now().Add(rw.cfg.ReprobeBudget.Duration())
	for n, off := range done {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return
		}
		d, err := req.Dev.ReadBlock(off)
		if err != nil {
			return // do not push a disk that just failed a read
		}
		if req.Ext != nil {
			req.Ext.RecordSelfRead(int(blockSize))
		}
		if idx := off / blockSize; idx < int64(len(req.Lat.Values)) {
			req.Lat.Values[idx] = EncodeLatency(d)
		}
		if d >= req.Slow {
			res.StillSlow++
		} else {
			res.Healed++
		}
		if n+1 < len(done) {
			if err := rw.sleep(ctx, rw.cfg.ReprobeSpacing.Duration()); err != nil {
				return
			}
		}
	}
	res.Verified = true
}
