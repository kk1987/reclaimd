//go:build linux

package reclaimd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealFreezeCycle drives FIFREEZE and FITHAW against a real mount, which
// is the only way to know the ioctl numbers are right on this kernel. Getting
// FITHAW wrong is the one that matters: a freeze with no thaw is a machine
// whose every writer waits forever. It needs root and a mount to freeze:
//
//	truncate -s 64M /tmp/f.img && mkfs.ext4 -q /tmp/f.img
//	mount -o loop /tmp/f.img /mnt/x
//	RECLAIMD_TEST_FREEZE=/mnt/x go test -run TestRealFreezeCycle ./internal/reclaimd/
func TestRealFreezeCycle(t *testing.T) {
	point := os.Getenv("RECLAIMD_TEST_FREEZE")
	if point == "" {
		t.Skip("set RECLAIMD_TEST_FREEZE=<mountpoint> (as root) to freeze a real filesystem")
	}

	hold, err := linuxFreezer{}.Freeze(point)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// A write into the frozen filesystem must block until the thaw. The
	// goroutine reports when it got through.
	wrote := make(chan time.Time, 1)
	go func() {
		_ = os.WriteFile(filepath.Join(point, "reclaimd-freeze-test"), []byte("x"), 0o600)
		wrote <- time.Now()
	}()
	select {
	case at := <-wrote:
		_ = hold.Thaw()
		t.Fatalf("a write got through a frozen filesystem at %v", at)
	case <-time.After(300 * time.Millisecond):
	}

	thawedAt := time.Now()
	if err := hold.Thaw(); err != nil {
		t.Fatalf("thaw: %v", err)
	}
	select {
	case at := <-wrote:
		if at.Before(thawedAt) {
			t.Fatalf("the write completed at %v, before the thaw at %v", at, thawedAt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write is still blocked after the thaw")
	}
	_ = os.Remove(filepath.Join(point, "reclaimd-freeze-test"))

	// And the leftover path: a thaw on something not frozen says so without
	// error, and a thaw on something frozen thaws it.
	if thawed, err := thawIfFrozen(point); err != nil || thawed {
		t.Fatalf("thawIfFrozen on a thawed filesystem: thawed=%v err=%v", thawed, err)
	}
	if _, err := (linuxFreezer{}).Freeze(point); err != nil {
		t.Fatalf("second freeze: %v", err)
	}
	if thawed, err := thawIfFrozen(point); err != nil || !thawed {
		t.Fatalf("thawIfFrozen on a frozen filesystem: thawed=%v err=%v", thawed, err)
	}
}
