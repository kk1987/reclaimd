package reclaimd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// BlockReader is the seam the scanner is tested through.
//
// A fake returning a scripted latency profile -- a 500ms block at every
// offset%32MiB==31, a hard dropout at 40 GiB -- exercises every branch of the
// backoff logic without a physical stick. That is the only way this logic gets
// tested at all, and it is the highest-value test in the project: a bug here
// costs a router its filesystem.
type BlockReader interface {
	ReadBlock(off int64) (time.Duration, error)
	Size() int64
	BlockSize() int
}

// Device is an open, read-only, O_DIRECT handle on a whole disk.
type Device struct {
	file      *os.File
	buf       []byte
	presence  Presence
	blockSize int
	roots     Roots
}

// alignedBuffer returns page-aligned, off-heap memory for O_DIRECT.
//
// An anonymous mapping is page aligned by construction, which beats the usual
// over-allocate-and-reslice trick twice over: it cannot be invalidated by a
// future moving collector, and an accidental append cannot hand the kernel a
// heap address that no longer satisfies the alignment rule.
func alignedBuffer(n int) ([]byte, error) {
	b, err := syscall.Mmap(-1, 0, n,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		return nil, fmt.Errorf("mmap %d bytes for o_direct buffer: %w", n, err)
	}
	return b, nil
}

// alignment is what O_DIRECT demands of the buffer address, the file offset and
// the length -- all three, which is why everything here uses one value.
func alignment(p Presence) int {
	a := os.Getpagesize()
	if lbs := p.Identity.LogicalBlockSize; lbs > a {
		a = lbs
	}
	return a
}

// OpenDevice opens the whole-disk node read-only with O_DIRECT.
//
// O_DIRECT is not an optimisation here, it is the entire mechanism: a buffered
// read can be served from the page cache without touching NAND, which would
// make the daemon a very elaborate no-op. It also keeps us from evicting the
// overlay's own cache while we sweep 60 GiB past it.
//
// O_EXCL is deliberately NOT passed. On a block device it means "fail if
// mounted or claimed", and running while the disk is a live overlay is the
// whole point. The refresh command is the mirror image and always passes it.
func OpenDevice(p Presence, blockSize int, roots Roots) (*Device, error) {
	if blockSize%alignment(p) != 0 {
		return nil, fmt.Errorf("%w: block size %d is not a multiple of %d",
			ErrAlignment, blockSize, alignment(p))
	}
	f, err := os.OpenFile(p.Node, os.O_RDONLY|syscall.O_DIRECT|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p.Node, err)
	}
	buf, err := alignedBuffer(blockSize)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Device{file: f, buf: buf, presence: p, blockSize: blockSize, roots: roots}, nil
}

func (d *Device) Close() error {
	err := d.file.Close()
	if d.buf != nil {
		if e := syscall.Munmap(d.buf); err == nil {
			err = e
		}
		d.buf = nil
	}
	return err
}

func (d *Device) Size() int64        { return d.presence.Identity.SizeBytes }
func (d *Device) BlockSize() int     { return d.blockSize }
func (d *Device) Presence() Presence { return d.presence }

// BlockCount is the number of whole blocks. A trailing partial block is not
// counted: with O_DIRECT the remainder below one logical block cannot be read
// at all, and reading past the end returns 0 bytes (EOF) rather than an error,
// which would otherwise be misread as a failure.
func (d *Device) BlockCount() int64 { return d.Size() / int64(d.blockSize) }

// ReadBlock times one aligned read of exactly one block at off.
//
// os.File.ReadAt is a plain pread(2) here, and its fdMutex keeps the descriptor
// alive for the duration -- the thing a raw syscall.Pread on a stashed int fd
// gets wrong. time.Since uses the monotonic clock, so an NTP step mid-read
// cannot manufacture a fake 1500ms hang, which on a router that has just synced
// its clock for the first time is a real scenario rather than a hypothetical.
func (d *Device) ReadBlock(off int64) (time.Duration, error) {
	start := time.Now()
	n, err := d.file.ReadAt(d.buf, off)
	elapsed := time.Since(start)
	if err != nil {
		return elapsed, d.classify(err)
	}
	if n != len(d.buf) {
		return elapsed, fmt.Errorf("short read %d of %d at offset %d", n, len(d.buf), off)
	}
	return elapsed, nil
}

// classify turns a raw errno into one of the three outcomes the scanner can act
// on. The distinction that matters is "this block is bad" versus "the whole
// device left", because the second one means a live overlay just lost its
// backing store and the round has to stop immediately.
func (d *Device) classify(err error) error {
	switch {
	case errors.Is(err, syscall.EINVAL):
		// Alignment violation: always our bug, never the device's. Kept apart
		// from ErrMediaError so an O_DIRECT mistake cannot spend months
		// disguised as a mysteriously flaky stick.
		return fmt.Errorf("%w: %v", ErrAlignment, err)

	case errors.Is(err, syscall.ENODEV), errors.Is(err, syscall.ENXIO):
		return ErrDeviceDisconnected

	case errors.Is(err, syscall.EIO):
		// EIO is ambiguous: one unreadable sector and a vanished device look
		// identical at the syscall boundary. sysfs is the tiebreak, and reading
		// it does not touch the bus.
		if d.sysfsAlive() {
			return ErrMediaError
		}
		return ErrDeviceDisconnected
	}
	return err
}

// sysfsAlive reports whether the block node still exists, still reports the
// same capacity, and still says "running".
//
// A suspend/resume cycle ("root hub lost power or was reset") produces a
// transient EIO with all three of these still true. Treating that as a dropout
// would suppress scanning for 24 hours every time the laptop lid closes.
func (d *Device) sysfsAlive() bool {
	sys := d.presence.SysPath
	if readSysInt(filepath.Join(sys, "size"))*sectorSize != d.presence.Identity.SizeBytes {
		return false
	}
	if st := readSysString(filepath.Join(sys, "device", "state")); st != "" && st != "running" {
		return false
	}
	return true
}

// WarmUp burns throwaway reads before timing starts.
//
// A stick left at power/control=auto with a short autosuspend delay pays the
// USB resume cost on the first read after an idle gap. Recorded, that becomes
// a phantom slow block; worse, it lands during warm-up and poisons the very
// baseline it is supposed to establish.
func (d *Device) WarmUp(ctx context.Context, discard int) error {
	suspended := readSysString(filepath.Join(d.presence.USBPath, "power", "runtime_status"))
	for i := 0; i < discard; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := d.ReadBlock(int64(i) * int64(d.blockSize)); err != nil {
			return err
		}
	}
	if suspended == "suspended" {
		// Not an error, but worth a breadcrumb: it explains any first-round
		// baseline that looks slightly high compared with the stored one.
		return nil
	}
	return nil
}

// WaitForReattach re-enumerates sysfs until the same identity comes back.
//
// Nothing resumes scanning afterwards -- the round is over either way -- so
// this exists to record the recovery latency and, more importantly, to confirm
// that a mounted overlay's backing store actually returned.
//
// The deadline is generous because measured re-enumeration is 5-6s and the
// slack has to cover a SuperSpeed-to-HighSpeed renegotiation plus a second
// enumeration attempt.
func WaitForReattach(ctx context.Context, roots Roots, want DiskIdentity,
	blockSize int, deadline time.Duration) (Presence, error) {

	end := time.Now().Add(deadline)
	backoff := 500 * time.Millisecond
	for time.Now().Before(end) {
		if err := sleepCtx(ctx, backoff); err != nil {
			return Presence{}, err
		}
		if backoff < 5*time.Second {
			backoff = backoff * 3 / 2
		}

		p, err := FindByKey(roots, want)
		if errors.Is(err, ErrIdentityMismatch) {
			// Something else is wearing this key. Never touch it.
			return Presence{}, err
		}
		if err != nil {
			continue
		}
		if readSysString(filepath.Join(p.SysPath, "device", "state")) != "running" {
			continue
		}
		// The node can exist before the device answers commands, and on
		// devtmpfs there is a window where /dev/<name> is not there yet. An
		// ENOENT here means "keep waiting", not "gone for good".
		dev, err := OpenDevice(p, blockSize, roots)
		if err != nil {
			continue
		}
		_, err = dev.ReadBlock(0)
		dev.Close()
		if err != nil {
			continue
		}
		return p, nil
	}
	return Presence{}, ErrNotFound
}

// sleepCtx is the only way this program waits. A plain time.Sleep in a yield or
// cooldown path would add its full duration to shutdown, and some of them are
// 15 seconds long.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
