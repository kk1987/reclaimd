package reclaimd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Roots is the Linux platform: sysfs and procfs, read as plain files. It lets
// the whole discovery layer be pointed at a fake tree under testdata/. Device
// discovery is the one piece of this program that cannot be exercised by
// unplugging things in CI, so it has to be injectable.
type Roots struct {
	Sys  string
	Proc string
	Dev  string
}

func DefaultRoots() Roots { return Roots{Sys: "/sys", Proc: "/proc", Dev: "/dev"} }

func (r Roots) Discover() ([]Presence, error) { return DiscoverUSBDisks(r) }

// sectorSize is the unit of /sys/block/<x>/size. It is always 512, whatever the
// device's logical_block_size. Multiplying by logical_block_size instead would
// be an 8x capacity overrun waiting for the first 4Kn enclosure.
const sectorSize = 512

func readSysString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readSysInt(path string) int64 {
	s := readSysString(path)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// resolveUSBParent walks up from a resolved <block>/device path until it finds
// the USB device node, the directory carrying idVendor/idProduct/serial.
//
// For a plain USB stick that is four levels up (LUN -> target -> host ->
// interface -> device), but card readers and hubs change the depth, so the
// loop is bounded only by reaching the sysfs devices root. Hardcoding four
// levels works right up until the day somebody plugs in a hub.
func resolveUSBParent(devicePath, sysRoot string) string {
	stop := filepath.Join(sysRoot, "devices")
	p := devicePath
	for i := 0; i < 24; i++ {
		if p == stop || p == "/" || p == "." {
			return ""
		}
		if readSysString(filepath.Join(p, "idVendor")) != "" &&
			readSysString(filepath.Join(p, "serial")) != "" {
			return p
		}
		// A device node without a serial is still the right level to stop at,
		// so detect it through uevent. Otherwise a serial-less stick would be
		// walked past, up into the host controller.
		if isUSBDeviceNode(p) {
			return p
		}
		p = filepath.Dir(p)
	}
	return ""
}

func isUSBDeviceNode(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "uevent"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "DEVTYPE=usb_device" {
			return true
		}
	}
	return false
}

// DiscoverUSBDisks enumerates every whole-disk block device whose parent chain
// passes through a USB device node.
//
// It never opens a block device. That matters wherever USB devices are left at
// power/control=auto, as a desktop udev rule may well do: opening the node
// would wake the stick out of autosuspend on every poll. Reading sysfs does not
// touch the bus at all.
func DiscoverUSBDisks(r Roots) ([]Presence, error) {
	blockDir := filepath.Join(r.Sys, "block")
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", blockDir, err)
	}

	protected, err := protectedDevnos(r)
	if err != nil {
		// Failing to read mountinfo must not silently disable the guard that
		// keeps us off the system disk, so this is fatal.
		return nil, fmt.Errorf("resolve protected devices: %w", err)
	}

	now := time.Now()
	var found []Presence
	for _, e := range entries {
		name := e.Name()
		sysPath := filepath.Join(blockDir, name)
		deviceLink := filepath.Join(sysPath, "device")

		// No device symlink means loop, zram, dm-, md and friends.
		dev, err := filepath.EvalSymlinks(deviceLink)
		if err != nil {
			continue
		}
		usb := resolveUSBParent(dev, r.Sys)
		if usb == "" {
			continue
		}

		major, minor, err := parseDevno(readSysString(filepath.Join(sysPath, "dev")))
		if err != nil {
			continue
		}

		id := DiskIdentity{
			VendorID:         readSysString(filepath.Join(usb, "idVendor")),
			ProductID:        readSysString(filepath.Join(usb, "idProduct")),
			Serial:           readSysString(filepath.Join(usb, "serial")),
			Vendor:           readSysString(filepath.Join(dev, "vendor")),
			Model:            readSysString(filepath.Join(dev, "model")),
			Revision:         readSysString(filepath.Join(dev, "rev")),
			SCSIAddr:         filepath.Base(dev),
			BusNum:           readSysString(filepath.Join(usb, "busnum")),
			DevPath:          readSysString(filepath.Join(usb, "devpath")),
			SizeBytes:        readSysInt(filepath.Join(sysPath, "size")) * sectorSize,
			LogicalBlockSize: int(readSysInt(filepath.Join(sysPath, "queue", "logical_block_size"))),
			MaxSectorsKB:     int(readSysInt(filepath.Join(sysPath, "queue", "max_sectors_kb"))),
		}

		p := Presence{
			Identity:   id,
			KernelName: name,
			Node:       filepath.Join(r.Dev, name),
			Major:      major,
			Minor:      minor,
			SysPath:    sysPath,
			USBPath:    usb,
			SeenAt:     now,
		}

		switch {
		case id.SizeBytes <= 0:
			p.Ignored, p.IgnoredReason = true, CodeDeviceNotReady
		case readSysInt(filepath.Join(sysPath, "removable")) != 1:
			// USB-attached but fixed media (an enclosure with a spinning disk,
			// say). Out of scope: this tool is about flash retention.
			p.Ignored, p.IgnoredReason = true, CodeNotRemovable
		case backsProtected(sysPath, name, major, minor, protected):
			// Booting from USB is a real configuration. Scanning the disk that
			// carries / would be aiming this tool at the machine running it.
			p.Ignored, p.IgnoredReason = true, CodeRootDevice
		}

		found = append(found, p)
	}

	assignKeys(found)
	return found, nil
}

func parseDevno(s string) (int, int, error) {
	maj, min, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("malformed devno %q", s)
	}
	a, err := strconv.Atoi(maj)
	if err != nil {
		return 0, 0, err
	}
	b, err := strconv.Atoi(min)
	if err != nil {
		return 0, 0, err
	}
	return a, b, nil
}

type devno struct{ major, minor int }

// protectedDevnos returns the device numbers backing mount points we must never
// scan. It compares device numbers because mountinfo field 3 is authoritative
// and immune to the naming churn that renames sda to sdb underneath us. A path
// comparison would not be.
func protectedDevnos(r Roots) (map[devno]bool, error) {
	f, err := os.Open(filepath.Join(r.Proc, "self", "mountinfo"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[devno]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		if !guardedMounts[fields[4]] {
			continue
		}
		maj, min, err := parseDevno(fields[2])
		if err != nil {
			continue
		}
		out[devno{maj, min}] = true
	}
	return out, sc.Err()
}

// backsProtected reports whether this whole disk, or any partition of it, backs
// a protected mount point.
func backsProtected(sysPath, name string, major, minor int, protected map[devno]bool) bool {
	if protected[devno{major, minor}] {
		return true
	}
	entries, err := os.ReadDir(sysPath)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), name) {
			continue
		}
		maj, min, err := parseDevno(readSysString(filepath.Join(sysPath, e.Name(), "dev")))
		if err != nil {
			continue
		}
		if protected[devno{maj, min}] {
			return true
		}
	}
	return false
}

// Alive reports whether the block node still exists, still reports the same
// capacity, and still says "running".
//
// A suspend/resume cycle ("root hub lost power or was reset") produces a
// transient EIO with all three of these still true. Treating that as a dropout
// would suppress scanning for 24 hours every time the laptop lid closes.
func (r Roots) Alive(p Presence) bool {
	if readSysInt(filepath.Join(p.SysPath, "size"))*sectorSize != p.Identity.SizeBytes {
		return false
	}
	if st := readSysString(filepath.Join(p.SysPath, "device", "state")); st != "" && st != "running" {
		return false
	}
	return true
}

// CheckNotInUse checks the mount and swap tables for the disk and every
// partition of it.
func (r Roots) CheckNotInUse(p Presence) error {
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

	f, err := os.Open(filepath.Join(r.Proc, "self", "mountinfo"))
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

	sf, err := os.Open(filepath.Join(r.Proc, "swaps"))
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

// IOStats reads /proc/diskstats, a procfs read that never touches the bus.
//
// It locates the row by device number. After a re-enumeration the kernel name
// changes from sda to sdb, and a name-keyed lookup would quietly start
// describing a different disk, or the same disk under a stale name, which is
// worse because it looks plausible.
func (r Roots) IOStats(p Presence) (diskStat, error) {
	f, err := os.Open(filepath.Join(r.Proc, "diskstats"))
	if err != nil {
		return diskStat{}, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 14 {
			continue
		}
		maj, err1 := strconv.Atoi(fields[0])
		min, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || maj != p.Major || min != p.Minor {
			continue
		}
		// Field indices are the post-5.5 layout: 5 sectors read, 7 writes
		// completed, 11 in-flight. Sectors are 512-byte units regardless of the
		// device's logical block size.
		sr, _ := strconv.ParseUint(fields[5], 10, 64)
		w, _ := strconv.ParseUint(fields[7], 10, 64)
		inf, _ := strconv.ParseUint(fields[11], 10, 64)
		return diskStat{sectorsRead: sr, writes: w, inFlight: inf}, nil
	}
	return diskStat{}, fmt.Errorf("no diskstats row for %d:%d", p.Major, p.Minor)
}

// Uptime reads /proc/uptime. Comparing wall clocks would not do, because on a
// router the wall clock at boot is fiction.
func (r Roots) Uptime() (time.Duration, error) {
	b, err := os.ReadFile(r.Proc + "/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("malformed /proc/uptime")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}
