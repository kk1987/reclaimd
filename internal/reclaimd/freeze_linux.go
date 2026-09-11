//go:build linux

package reclaimd

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// FIFREEZE and FITHAW, _IOWR('X', 119, int) and _IOWR('X', 120, int) in the
// asm-generic ioctl encoding, which is the one every architecture this program
// is built for uses. fsfreeze(8) issues the same two.
const (
	fiFreeze = 0xC0045877
	fiThaw   = 0xC0045878
)

// linuxFreezer freezes a filesystem the way fsfreeze(8) and LVM snapshots do.
//
// FIFREEZE flushes everything dirty, commits the journal or checkpoint, and
// then blocks every new write at the VFS until FITHAW. Writers sleep rather
// than fail. f2fs's own background GC checks for the freeze before each pass
// and skips it, so from the freeze until the thaw the device sees no I/O from
// that filesystem at all, which is the guarantee the rewrite needs.
//
// It needs CAP_SYS_ADMIN, which on the router the daemon has because it runs
// as root. Under the systemd unit it does not, and the freeze fails with
// EPERM, which the rewrite reports and gives up on.
type linuxFreezer struct{}

func defaultFreezer() fsFreezer { return linuxFreezer{} }

func (linuxFreezer) Freeze(point string) (fsHold, error) {
	f, err := os.OpenFile(point, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := fsIoctl(f, fiFreeze); err != nil {
		f.Close()
		return nil, fmt.Errorf("freeze %s: %w", point, err)
	}
	return &linuxHold{f: f, point: point}, nil
}

type linuxHold struct {
	f     *os.File
	point string
}

// Thaw undoes the freeze and gives the descriptor back. It is safe to call
// once; the guard in rewrite.go is what makes sure it is called exactly once.
func (h *linuxHold) Thaw() error {
	err := fsIoctl(h.f, fiThaw)
	h.f.Close()
	if err != nil {
		return fmt.Errorf("thaw %s: %w", h.point, err)
	}
	return nil
}

// thawIfFrozen issues FITHAW to a mount point, for a freeze a previous run of
// this daemon may have left behind. EINVAL means it was not frozen, which is
// the usual answer and not an error.
func thawIfFrozen(point string) (thawed bool, err error) {
	f, err := os.OpenFile(point, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = fsIoctl(f, fiThaw)
	if errors.Is(err, syscall.EINVAL) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("thaw %s: %w", point, err)
	}
	return true, nil
}

func fsIoctl(f *os.File, req uintptr) error {
	var arg int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(&arg)))
	if errno != 0 {
		return errno
	}
	return nil
}
