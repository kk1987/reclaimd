//go:build linux

package reclaimd

import (
	"os"
	"syscall"
)

// DefaultPlatform is sysfs and procfs where Linux mounts them.
func DefaultPlatform() Platform { return DefaultRoots() }

const defaultStateDir = "/var/lib/reclaimd"

// scanOpenFlags asks for O_DIRECT, which is what gets a Linux read of a block
// device past the page cache and onto the device.
//
// O_EXCL is deliberately NOT passed. On a block device it means "fail if
// mounted or claimed", and running while the disk is a live overlay is the
// whole point.
const scanOpenFlags = os.O_RDONLY | syscall.O_DIRECT | syscall.O_CLOEXEC

// refreshOpenFlags passes O_EXCL, which on a block device means "fail if
// mounted or claimed". That upgrades the mount and swap check from one with a
// race window into a guarantee the kernel enforces, and closes the door on
// udisks2 mounting the disk between the check and the first write.
const refreshOpenFlags = os.O_RDWR | syscall.O_EXCL | syscall.O_DIRECT | syscall.O_CLOEXEC

// tmpfsMagic identifies a volatile filesystem. See OpenStore for why this
// matters more than it looks.
const tmpfsMagic = 0x01021994

func onTmpfs(dir string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(dir, &st) == nil && st.Type == tmpfsMagic
}

// kernelVersion is the kernel's name and release, from where procfs keeps them.
func kernelVersion() (name, release string) {
	return firstLine("/proc/sys/kernel/ostype"), firstLine("/proc/sys/kernel/osrelease")
}
