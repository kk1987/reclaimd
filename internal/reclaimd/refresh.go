package reclaimd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"
)

// RefreshOpts configures the one command in this program that can write to a
// disk.
type RefreshOpts struct {
	Key     string
	Confirm string
	Mode    string // "rewrite" (default) or "zero"
	IMeanIt bool
	Start   int64
	End     int64
	DryRun  bool
	// MaxDropouts bounds how many times refresh will reattach and carry on.
	// Zero means the default. A refresh that gave up on the first dropout
	// would abandon the drive part-rewritten, which is the worst of both
	// states: neither refreshed nor left alone.
	MaxDropouts int
}

const (
	RefreshRewrite = "rewrite"
	RefreshZero    = "zero"
)

// Refresh rewrites a disk in place to force every block through a fresh
// program cycle.
//
// Reading is enough to trigger reclaim on the blocks the controller decides are
// marginal. Rewriting is the bigger hammer: it refreshes everything whether the
// controller agrees or not. That is why it is a separate command behind four
// gates. The daemon's own rewrite (rewrite.go) is the narrow version of this:
// only the blocks that stayed slow, only once the re-probe has shown reads do
// not heal them, and only with a config that says so.
func Refresh(ctx context.Context, cfg Config, pl Platform, logger *slog.Logger, opts RefreshOpts) error {
	if opts.Mode == "" {
		opts.Mode = RefreshRewrite
	}
	if opts.Mode == RefreshZero && !opts.IMeanIt {
		return fmt.Errorf("%w: -mode=zero destroys data outright and needs -i-mean-it",
			ErrConfirmMismatch)
	}

	disks, err := pl.Discover()
	if err != nil {
		return err
	}
	var p Presence
	found := false
	for _, d := range disks {
		if d.Identity.Key == opts.Key || d.Node == opts.Key || d.KernelName == opts.Key {
			p, found = d, true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrNotFound, opts.Key)
	}

	// Gate 1. A --yes flag would not do, because a confirmation you can paste
	// from the wrong terminal confirms nothing. The serial has to come from
	// having looked at the device that is about to be overwritten.
	if opts.Confirm != p.Identity.Serial || p.Identity.Serial == "" {
		// The serial is deliberately not echoed here. Printing it would let the
		// operator copy it straight out of the error message, which defeats the
		// point of the gate.
		return fmt.Errorf("%w: -confirm must be the serial number printed on the "+
			"target device (%s at %s)", ErrConfirmMismatch, p.Identity.Model, p.Node)
	}

	// Gate 2. Neither the whole disk nor any partition of it may be mounted,
	// used as swap or otherwise claimed.
	if err := pl.CheckNotInUse(p); err != nil {
		return err
	}

	if opts.End <= 0 || opts.End > p.Identity.SizeBytes {
		opts.End = p.Identity.SizeBytes
	}
	cfg = cfg.ForDisk(p.Identity)
	blockSize := int64(cfg.BlockSize)
	start := alignDown(opts.Start, blockSize)
	end := alignDown(opts.End, blockSize)
	if start >= end {
		return fmt.Errorf("empty range %d:%d", start, end)
	}

	logger.Warn("about to rewrite a disk",
		"disk", p.Identity.Key, "node", p.Node, "mode", opts.Mode,
		"start", start, "end", end, "dry_run", opts.DryRun)
	if opts.DryRun {
		return nil
	}

	// Gate 3. An open the kernel itself refuses while the disk is in use, which
	// upgrades gate 2 from a check with a race window into a guarantee. How
	// much of gate 2 each kernel enforces is documented at refreshOpenFlags.
	f, err := os.OpenFile(p.Node, refreshOpenFlags, 0)
	if err != nil {
		return fmt.Errorf("open %s exclusively (is it mounted?): %w", p.Node, err)
	}

	buf, err := alignedBuffer(cfg.BlockSize)
	if err != nil {
		f.Close()
		return err
	}
	defer syscall.Munmap(buf)
	if opts.Mode == RefreshZero {
		for i := range buf {
			buf[i] = 0
		}
	}

	maxDrops := opts.MaxDropouts
	if maxDrops == 0 {
		maxDrops = 8
	}

	// The reopener is what makes a dropout survivable: it waits for the same
	// identity to come back and hands the loop a fresh exclusive handle, under
	// whatever device name the kernel picked this time.
	reopen := func(ctx context.Context) (refreshTarget, error) {
		np, err := WaitForReattach(ctx, pl, p.Identity, cfg.BlockSize,
			cfg.ReattachTimeout.Duration())
		if err != nil {
			return nil, fmt.Errorf("device did not return after a refresh dropout: %w", err)
		}
		p = np
		nf, err := os.OpenFile(p.Node, refreshOpenFlags, 0)
		if err != nil {
			return nil, fmt.Errorf("reopen %s after dropout: %w", p.Node, err)
		}
		return nf, nil
	}

	t0 := time.Now()
	alive := func() bool { return pl.Alive(p) }
	st, err := rewriteRange(ctx, logger, f, reopen, alive, buf, start, end, blockSize,
		opts.Mode == RefreshRewrite, maxDrops)
	if err != nil {
		return err
	}
	logger.Info("refresh complete", "disk", p.Identity.Key,
		"written_mib", st.Written>>20, "skipped_blocks", st.Skipped,
		"dropouts_n", st.Dropouts, "elapsed_s", time.Since(t0).Seconds())
	return nil
}

// refreshTarget is the seam the rewrite loop is tested through.
//
// A dropout in the middle of a whole-drive rewrite cannot be staged against
// real hardware, and it is the one path where getting the recovery wrong leaves
// a drive half-rewritten, which is worse than either finishing or never
// starting. So the loop talks to an interface and the test supplies a device
// that fails where it likes.
type refreshTarget interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Close() error
}

// reopenFunc yields a fresh handle once the device is back on the bus.
type reopenFunc func(ctx context.Context) (refreshTarget, error)

type refreshStats struct {
	Written  int64
	Skipped  int64
	Dropouts int64
}

// rewriteRange walks the range once, reopening around dropouts. It owns the
// target's lifetime from here on and closes it on every path out.
func rewriteRange(ctx context.Context, logger *slog.Logger, tgt refreshTarget,
	reopen reopenFunc, alive func() bool, buf []byte, start, end, blockSize int64,
	rewrite bool, maxDrops int) (refreshStats, error) {

	var st refreshStats
	t0 := time.Now()

	// interrupted is the exit an operator chooses. It leaves the drive
	// part-rewritten, so the line says exactly where to pick up: off is the
	// first block not yet written back, whatever mix of writes, skips and
	// retries got here.
	interrupted := func(off int64) error {
		logger.Warn("refresh interrupted; the drive is part-rewritten",
			"offset", off, "resume_range", fmt.Sprintf("%d:%d", off, end))
		return ctx.Err()
	}

	for off := start; off < end; off += blockSize {
		if ctx.Err() != nil {
			tgt.Close()
			return st, interrupted(off)
		}
		if rewrite {
			if _, err := tgt.ReadAt(buf, off); err != nil {
				if isDisconnect(err, alive) {
					goto dropped
				}
				// Gate 4. Writing back a buffer we failed to fill would turn a
				// recoverable retention problem into permanent data loss, which
				// is the one outcome this whole program exists to prevent.
				logger.Warn("read failed; leaving this block untouched",
					"offset", off, "error", err)
				st.Skipped++
				continue
			}
		}
		if _, err := tgt.WriteAt(buf, off); err != nil {
			if isDisconnect(err, alive) {
				goto dropped
			}
			tgt.Close()
			return st, fmt.Errorf("write at %d: %w", off, err)
		}
		st.Written += blockSize

		if st.Written%(1<<30) == 0 {
			logger.Info("refresh progress", "written_gib", st.Written>>30,
				"mib_s", float64(st.Written>>20)/time.Since(t0).Seconds())
		}
		continue

	dropped:
		// Writing takes this controller off the bus just as reading does, and it
		// comes back on its own within a couple of seconds. A whole-drive
		// rewrite therefore has to pick up where it left off.
		st.Dropouts++
		tgt.Close()
		logger.Warn("device dropped off the bus during refresh",
			"offset", off, "dropouts_n", st.Dropouts, "code", CodeDeviceDisconnected)
		if st.Dropouts > int64(maxDrops) {
			return st, fmt.Errorf("%w: %d dropouts, giving up at offset %d",
				ErrDeviceDisconnected, st.Dropouts, off)
		}
		next, err := reopen(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Still the operator's choice, and the block that dropped the
				// bus is still the one to pick up from.
				return st, interrupted(off)
			}
			return st, err
		}
		tgt = next
		logger.Info("resuming refresh", "offset", off)
		// Retry the block that dropped us. The write never landed, so skipping
		// it would leave a hole in the refresh.
		off -= blockSize
	}

	if err := tgt.Sync(); err != nil {
		tgt.Close()
		return st, fmt.Errorf("sync: %w", err)
	}
	tgt.Close()
	return st, nil
}

// isDisconnect recognises the device leaving the bus mid-rewrite.
//
// ENODEV and ENXIO are unambiguous. EIO is not: it covers both a vanished
// device and a single sector that will not read, and the two need opposite
// responses. Treating every EIO as a dropout means one bad sector burns the
// whole dropout budget (close, reattach, retry the same block, fail again) and
// aborts a refresh that should simply have stepped over it. So EIO asks the
// platform, which answers without touching the bus. The scanner does the same.
func isDisconnect(err error, alive func() bool) bool {
	switch {
	case errors.Is(err, syscall.ENODEV), errors.Is(err, syscall.ENXIO):
		return true
	case errors.Is(err, syscall.EIO):
		return alive == nil || !alive()
	}
	return false
}
