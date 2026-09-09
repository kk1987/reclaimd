package reclaimd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
// marginal; rewriting is the bigger hammer, and it refreshes everything whether
// the controller agrees or not. That is why it is a separate command behind
// four gates rather than something the daemon may decide to do.
func Refresh(ctx context.Context, cfg Config, roots Roots, logger *slog.Logger, opts RefreshOpts) error {
	if opts.Mode == "" {
		opts.Mode = RefreshRewrite
	}
	if opts.Mode == RefreshZero && !opts.IMeanIt {
		return fmt.Errorf("%w: -mode=zero destroys data outright and needs -i-mean-it",
			ErrConfirmMismatch)
	}

	disks, err := DiscoverUSBDisks(roots)
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

	// Gate 1. Not a --yes flag: a confirmation you can paste from the wrong
	// terminal is not a confirmation. The serial has to come from having looked
	// at the device that is about to be overwritten.
	if opts.Confirm != p.Identity.Serial || p.Identity.Serial == "" {
		// The serial is deliberately NOT echoed here. Printing it would let the
		// operator copy it straight out of the error message, which defeats the
		// only thing this gate is for: making them look at the device that is
		// about to be overwritten.
		return fmt.Errorf("%w: -confirm must be the serial number printed on the "+
			"target device (%s at %s)", ErrConfirmMismatch, p.Identity.Model, p.Node)
	}

	// Gate 2. The whole disk and every partition of it must be absent from both
	// the mount table and the swap table.
	if err := assertNotInUse(roots, p); err != nil {
		return err
	}

	if opts.End <= 0 || opts.End > p.Identity.SizeBytes {
		opts.End = p.Identity.SizeBytes
	}
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

	// Gate 3. O_EXCL on a block device means "fail if mounted or claimed", which
	// upgrades gate 2 from a check with a race window into a guarantee the
	// kernel enforces. It also closes the door on udisks2 mounting the disk
	// between the check and the first write.
	f, err := os.OpenFile(p.Node,
		os.O_RDWR|syscall.O_EXCL|syscall.O_DIRECT|syscall.O_CLOEXEC, 0)
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

	var written, skipped, drops int64
	t0 := time.Now()
	for off := start; off < end; off += blockSize {
		if err := ctx.Err(); err != nil {
			f.Close()
			return err
		}
		if opts.Mode == RefreshRewrite {
			if _, err := f.ReadAt(buf, off); err != nil {
				if isDisconnect(err) {
					goto dropped
				}
				// Gate 4, and the most important line in this file. Writing back
				// a buffer we failed to fill would turn a recoverable retention
				// problem into permanent data loss -- the single outcome this
				// whole program exists to prevent.
				logger.Warn("read failed; leaving this block untouched",
					"offset", off, "error", err)
				skipped++
				continue
			}
		}
		if _, err := f.WriteAt(buf, off); err != nil {
			if isDisconnect(err) {
				goto dropped
			}
			f.Close()
			return fmt.Errorf("write at %d: %w", off, err)
		}
		written += blockSize

		if written%(1<<30) == 0 {
			logger.Info("refresh progress", "written_gib", written>>30,
				"mib_s", float64(written>>20)/time.Since(t0).Seconds())
		}
		continue

	dropped:
		// Writing can take this controller off the bus just as reading can. The
		// device comes back on its own within a couple of seconds, so a whole
		// -drive rewrite has to be able to pick up where it left off; giving up
		// here would leave the drive part-rewritten, which is worse than either
		// finishing or never starting.
		drops++
		f.Close()
		logger.Warn("device dropped off the bus during refresh",
			"offset", off, "dropouts_n", drops, "code", CodeDeviceDisconnected)
		if drops > int64(maxDrops) {
			return fmt.Errorf("%w: %d dropouts, giving up at offset %d",
				ErrDeviceDisconnected, drops, off)
		}
		np, rerr := WaitForReattach(ctx, roots, p.Identity, cfg.BlockSize,
			cfg.ReattachTimeout.Duration())
		if rerr != nil {
			return fmt.Errorf("device did not return after a refresh dropout: %w", rerr)
		}
		p = np
		f, err = os.OpenFile(p.Node,
			os.O_RDWR|syscall.O_EXCL|syscall.O_DIRECT|syscall.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("reopen %s after dropout: %w", p.Node, err)
		}
		logger.Info("resuming refresh", "node", p.Node, "offset", off)
		// Retry the block that dropped us rather than skipping it: the write
		// never landed, so skipping would leave a hole in the refresh.
		off -= blockSize
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync: %w", err)
	}
	f.Close()
	logger.Info("refresh complete", "disk", p.Identity.Key,
		"written_mib", written>>20, "skipped_blocks", skipped,
		"dropouts_n", drops, "elapsed_s", time.Since(t0).Seconds())
	return nil
}

// isDisconnect recognises the device leaving the bus mid-write. ENODEV and
// ENXIO are unambiguous; EIO is not, but during a whole-drive rewrite the
// difference does not change what we do -- reattach and carry on either way.
func isDisconnect(err error) bool {
	return errors.Is(err, syscall.ENODEV) ||
		errors.Is(err, syscall.ENXIO) ||
		errors.Is(err, syscall.EIO)
}

// assertNotInUse checks the mount and swap tables for the disk and every
// partition of it.
func assertNotInUse(roots Roots, p Presence) error {
	devnos := map[devno]bool{{p.Major, p.Minor}: true}
	if entries, err := os.ReadDir(p.SysPath); err == nil {
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), p.KernelName) {
				continue
			}
			if maj, min, err := parseDevno(
				readSysString(filepath.Join(p.SysPath, e.Name(), "dev"))); err == nil {
				devnos[devno{maj, min}] = true
			}
		}
	}

	f, err := os.Open(filepath.Join(roots.Proc, "self", "mountinfo"))
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		maj, min, err := parseDevno(fields[2])
		if err != nil {
			continue
		}
		if devnos[devno{maj, min}] {
			return fmt.Errorf("%w: %s is mounted at %s", ErrDeviceMounted, p.Node, fields[4])
		}
	}

	sf, err := os.Open(filepath.Join(roots.Proc, "swaps"))
	if err == nil {
		defer sf.Close()
		ssc := bufio.NewScanner(sf)
		for ssc.Scan() {
			line := ssc.Text()
			if strings.HasPrefix(line, p.Node) {
				return fmt.Errorf("%w: %s is in use as swap", ErrDeviceMounted, p.Node)
			}
		}
	}
	return nil
}
