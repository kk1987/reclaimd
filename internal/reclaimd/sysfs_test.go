package reclaimd

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeTree builds a sysfs/procfs skeleton under t.TempDir(). Discovery is the
// one part of this program that cannot be tested by unplugging things, and the
// cases that matter most -- a multi-LUN reader, a batch of sticks sharing one
// hardcoded serial -- are ones we cannot reproduce on the bench at all.
type fakeTree struct {
	t    *testing.T
	root string
}

func newFakeTree(t *testing.T) *fakeTree {
	t.Helper()
	f := &fakeTree{t: t, root: t.TempDir()}
	f.write(filepath.Join("proc", "self", "mountinfo"),
		"25 1 259:3 / / rw,relatime shared:1 - btrfs /dev/nvme0n1p3 rw\n"+
			"31 25 259:1 / /boot/efi rw,relatime shared:2 - vfat /dev/nvme0n1p1 rw\n")
	return f
}

func (f *fakeTree) roots() Roots {
	return Roots{
		Sys:  filepath.Join(f.root, "sys"),
		Proc: filepath.Join(f.root, "proc"),
		Dev:  filepath.Join(f.root, "dev"),
	}
}

func (f *fakeTree) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// addUSBNode creates the USB device level: the directory that carries the
// serial and answers DEVTYPE=usb_device in its uevent.
func (f *fakeTree) addUSBNode(path, vid, pid, serial, busnum, devpath string) {
	base := filepath.Join("sys", "devices", "pci0000:00", path)
	f.write(filepath.Join(base, "idVendor"), vid+"\n")
	f.write(filepath.Join(base, "idProduct"), pid+"\n")
	if serial != "" {
		f.write(filepath.Join(base, "serial"), serial+"\n")
	}
	f.write(filepath.Join(base, "busnum"), busnum+"\n")
	f.write(filepath.Join(base, "devpath"), devpath+"\n")
	f.write(filepath.Join(base, "uevent"), "DEVTYPE=usb_device\nBUSNUM="+busnum+"\n")
}

// addDisk hangs a whole-disk block node off a USB node at the given LUN,
// reproducing the real depth: interface -> host -> target -> LUN.
func (f *fakeTree) addDisk(name, usbPath, lun, devno string, sizeSectors int64, removable bool) {
	scsi := filepath.Join("sys", "devices", "pci0000:00", usbPath,
		usbPath[len(usbPath)-3:]+":1.0", "host0", "target0:0:0", lun)
	f.write(filepath.Join(scsi, "vendor"), "Generic \n")
	f.write(filepath.Join(scsi, "model"), "Flash Disk \n")
	f.write(filepath.Join(scsi, "rev"), "1100\n")

	blk := filepath.Join("sys", "block", name)
	f.write(filepath.Join(blk, "size"), itoa(sizeSectors)+"\n")
	f.write(filepath.Join(blk, "dev"), devno+"\n")
	f.write(filepath.Join(blk, "queue", "logical_block_size"), "512\n")
	f.write(filepath.Join(blk, "queue", "max_sectors_kb"), "1024\n")
	if removable {
		f.write(filepath.Join(blk, "removable"), "1\n")
	} else {
		f.write(filepath.Join(blk, "removable"), "0\n")
	}
	if err := os.Symlink(filepath.Join(f.root, scsi),
		filepath.Join(f.root, blk, "device")); err != nil {
		f.t.Fatal(err)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

func byKernelName(disks []Presence, name string) *Presence {
	for i := range disks {
		if disks[i].KernelName == name {
			return &disks[i]
		}
	}
	return nil
}

func TestDiscoverStableSerialKey(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-2", "090c", "1000", "0011223344556677", "4", "2")
	f.addDisk("sda", "usb4/4-2", "0:0:0:0", "8:0", 125304832, true)

	disks, err := DiscoverUSBDisks(f.roots())
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 {
		t.Fatalf("want 1 disk, got %d", len(disks))
	}
	d := disks[0]
	if got, want := d.Identity.Key, "usb-090c:1000-0011223344556677"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if !d.Identity.KeyIsStable {
		t.Error("key should be marked stable")
	}
	if got, want := d.Identity.SizeBytes, int64(64156073984); got != want {
		t.Errorf("size = %d, want %d (sysfs size is always in 512B units)", got, want)
	}
	if d.Ignored {
		t.Errorf("disk should not be ignored, got reason %q", d.IgnoredReason)
	}
}

// A card reader exposes several LUNs behind ONE USB device node, so all its
// slots share a serial. That is not a collision and must not downgrade the key.
func TestDiscoverMultiLUNKeepsStableKeys(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-3", "058f", "6366", "058F63666433", "4", "3")
	f.addDisk("sdb", "usb4/4-3", "0:0:0:0", "8:16", 62333952, true)
	f.addDisk("sdc", "usb4/4-3", "0:0:0:1", "8:32", 31166976, true)

	disks, err := DiscoverUSBDisks(f.roots())
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 {
		t.Fatalf("want 2 disks, got %d", len(disks))
	}
	for _, d := range disks {
		if !d.Identity.KeyIsStable {
			t.Errorf("%s: multi-LUN should stay stable, got unstable key %q",
				d.KernelName, d.Identity.Key)
		}
	}
	a, b := byKernelName(disks, "sdb"), byKernelName(disks, "sdc")
	if a.Identity.Key == b.Identity.Key {
		t.Fatalf("LUNs must not share a key, both are %q", a.Identity.Key)
	}
	if want := "usb-058f:6366-058F63666433-lun0_0_0_0"; a.Identity.Key != want {
		t.Errorf("sdb key = %q, want %q", a.Identity.Key, want)
	}
}

// Some vendors ship a whole production run with one hardcoded serial. Both
// sides must be downgraded: an unstable key that forgets history across a port
// change is far cheaper than a stable key pointing at the wrong stick.
func TestDiscoverSerialCollisionDowngradesBoth(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-4", "abcd", "1234", "AAAA", "4", "4")
	f.addUSBNode("usb4/4-5", "abcd", "1234", "AAAA", "4", "5")
	f.addDisk("sdd", "usb4/4-4", "0:0:0:0", "8:48", 1000000, true)
	f.addDisk("sde", "usb4/4-5", "0:0:0:0", "8:64", 2000000, true)

	disks, err := DiscoverUSBDisks(f.roots())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		if d.Identity.KeyIsStable {
			t.Errorf("%s: colliding serial must downgrade, key %q still marked stable",
				d.KernelName, d.Identity.Key)
		}
		if got := d.Identity.Key[:5]; got != "path-" {
			t.Errorf("%s: want path-derived key, got %q", d.KernelName, d.Identity.Key)
		}
	}
	a, b := byKernelName(disks, "sdd"), byKernelName(disks, "sde")
	if a.Identity.Key == b.Identity.Key {
		t.Fatalf("downgraded keys still collide: %q", a.Identity.Key)
	}
}

func TestDiscoverMissingSerialDowngrades(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-6", "1234", "5678", "", "4", "6")
	f.addDisk("sdf", "usb4/4-6", "0:0:0:0", "8:80", 500000, true)

	disks, err := DiscoverUSBDisks(f.roots())
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 {
		t.Fatalf("want 1 disk, got %d", len(disks))
	}
	if disks[0].Identity.KeyIsStable {
		t.Error("serial-less device must not claim a stable key")
	}
}

func TestDiscoverIgnoresNonRemovable(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-7", "1111", "2222", "ENCLOSURE1", "4", "7")
	f.addDisk("sdg", "usb4/4-7", "0:0:0:0", "8:96", 1953525168, false)

	disks, err := DiscoverUSBDisks(f.roots())
	if err != nil {
		t.Fatal(err)
	}
	d := byKernelName(disks, "sdg")
	if d == nil || !d.Ignored || d.IgnoredReason != CodeNotRemovable {
		t.Fatalf("USB-attached fixed disk should be ignored as %s, got %+v",
			CodeNotRemovable, d)
	}
}

// Booting from USB is a real configuration, and scanning the disk carrying /
// would aim this tool at the machine running it.
func TestDiscoverIgnoresRootDevice(t *testing.T) {
	f := newFakeTree(t)
	f.write(filepath.Join("proc", "self", "mountinfo"),
		"25 1 8:113 / / rw,relatime shared:1 - ext4 /dev/sdh1 rw\n")
	f.addUSBNode("usb4/4-8", "3333", "4444", "BOOTSTICK", "4", "8")
	f.addDisk("sdh", "usb4/4-8", "0:0:0:0", "8:112", 30000000, true)
	// The root filesystem lives on a partition of that disk, not the disk itself.
	f.write(filepath.Join("sys", "block", "sdh", "sdh1", "dev"), "8:113\n")

	disks, err := DiscoverUSBDisks(f.roots())
	if err != nil {
		t.Fatal(err)
	}
	d := byKernelName(disks, "sdh")
	if d == nil || !d.Ignored || d.IgnoredReason != CodeRootDevice {
		t.Fatalf("disk backing / must be ignored as %s, got %+v", CodeRootDevice, d)
	}
}

// The stick renames itself across a re-enumeration; a stranger answering to a
// cloned serial must still be refused.
func TestFindByKeyRejectsSizeMismatch(t *testing.T) {
	f := newFakeTree(t)
	f.addUSBNode("usb4/4-2", "090c", "1000", "0011223344556677", "4", "2")
	f.addDisk("sdb", "usb4/4-2", "0:0:0:0", "8:16", 125304832, true)

	want := DiskIdentity{Key: "usb-090c:1000-0011223344556677", SizeBytes: 999}
	if _, err := FindByKey(f.roots(), want); err == nil {
		t.Fatal("want identity mismatch, got nil error")
	}

	want.SizeBytes = 64156073984
	p, err := FindByKey(f.roots(), want)
	if err != nil {
		t.Fatalf("same disk should be found after rename: %v", err)
	}
	if p.KernelName != "sdb" {
		t.Errorf("kernel name = %q, want sdb", p.KernelName)
	}
}
