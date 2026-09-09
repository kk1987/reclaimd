package reclaimd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Roots lets the whole discovery layer be pointed at a fake tree under
// testdata/. Device discovery is the one piece of this program that cannot be
// exercised by unplugging things in CI, so it has to be injectable.
type Roots struct {
	Sys  string
	Proc string
	Dev  string
}

func DefaultRoots() Roots { return Roots{Sys: "/sys", Proc: "/proc", Dev: "/dev"} }

// sectorSize is the unit of /sys/block/<x>/size. It is ALWAYS 512 regardless of
// the device's logical_block_size -- multiplying by logical_block_size instead
// is an 8x capacity overrun waiting for the first 4Kn enclosure.
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
// the USB device node -- the directory carrying idVendor/idProduct/serial.
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
		// A device node without a serial is still the right level to stop at;
		// detect it via uevent so that serial-less sticks are found too rather
		// than walking past them into the host controller.
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
// would wake the stick out of autosuspend on every poll, whereas reading sysfs
// does not touch the bus at all.
func DiscoverUSBDisks(r Roots) ([]Presence, error) {
	blockDir := filepath.Join(r.Sys, "block")
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", blockDir, err)
	}

	protected, err := protectedDevnos(r)
	if err != nil {
		// Failing to read mountinfo must not silently disable the guard that
		// keeps us off the system disk, so this is fatal rather than ignored.
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

// assignKeys fills in Key for every discovered disk, downgrading to an unstable
// path-derived key when a serial is missing or shared.
//
// Some vendors ship an entire production run with one hardcoded serial. Two
// sticks answering to the same key would silently share history and, worse,
// could be mistaken for each other after a re-enumeration. Downgrading BOTH
// sides of a collision is deliberate: an unstable key that forgets history
// across a port change is much cheaper than a stable key pointing at the wrong
// hardware.
func assignKeys(disks []Presence) {
	// Two disks sharing a serial mean very different things depending on
	// whether they hang off the same USB device node. One node with several
	// LUNs is a card reader: legitimate, and the LUN address separates them.
	// Several nodes answering the same serial is a vendor that hardcoded one
	// serial across a production run, and there is nothing left to tell those
	// sticks apart by.
	nodesPerSerial := map[string]map[string]bool{}
	lunsPerNode := map[string]int{}
	for i := range disks {
		sk := serialKey(disks[i].Identity)
		nk := usbNodeKey(disks[i].Identity)
		if nodesPerSerial[sk] == nil {
			nodesPerSerial[sk] = map[string]bool{}
		}
		nodesPerSerial[sk][nk] = true
		lunsPerNode[nk]++
	}

	for i := range disks {
		id := &disks[i].Identity
		sk := serialKey(*id)
		switch {
		case id.Serial == "" || len(nodesPerSerial[sk]) > 1:
			// Downgrade BOTH sides of a collision. A path key changes when the
			// stick moves to another port, and that instability is the point:
			// losing history is far cheaper than attributing one stick's
			// history -- and its dropout suppression -- to another.
			id.Key = fmt.Sprintf("path-%s:%s-b%s-p%s-%d",
				id.VendorID, id.ProductID, id.BusNum, id.DevPath, id.SizeBytes)
			id.KeyIsStable = false
		case lunsPerNode[usbNodeKey(*id)] > 1:
			id.Key = sk + "-lun" + sanitizeKey(id.SCSIAddr)
			id.KeyIsStable = true
		default:
			id.Key = sk
			id.KeyIsStable = true
		}
	}
}

func serialKey(id DiskIdentity) string {
	return fmt.Sprintf("usb-%s:%s-%s", id.VendorID, id.ProductID, sanitizeKey(id.Serial))
}

func usbNodeKey(id DiskIdentity) string {
	return fmt.Sprintf("%s:%s-b%s-p%s", id.VendorID, id.ProductID, id.BusNum, id.DevPath)
}

// sanitizeKey keeps keys usable as directory names. Serials are vendor-supplied
// and have been seen to contain spaces and slashes.
func sanitizeKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
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
// scan. Comparing device NUMBERS rather than device paths is what makes this
// reliable: mountinfo field 3 is authoritative and immune to the naming churn
// that renames sda to sdb underneath us.
func protectedDevnos(r Roots) (map[devno]bool, error) {
	f, err := os.Open(filepath.Join(r.Proc, "self", "mountinfo"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	guard := map[string]bool{"/": true, "/boot": true, "/boot/efi": true, "/usr": true}
	out := map[devno]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		if !guard[fields[4]] {
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

// FindByKey re-resolves a key to wherever the disk lives right now.
//
// This is the function that makes a dropout survivable. After a re-enumeration
// the old path may well name a DIFFERENT disk, so nothing may be reused from
// before: the whole presence is rebuilt from a fresh scan, and the capacity is
// re-checked as a last line of defence against a stranger answering to a
// cloned serial.
func FindByKey(r Roots, want DiskIdentity) (Presence, error) {
	disks, err := DiscoverUSBDisks(r)
	if err != nil {
		return Presence{}, err
	}
	for _, p := range disks {
		if p.Identity.Key != want.Key {
			continue
		}
		if want.SizeBytes != 0 && p.Identity.SizeBytes != want.SizeBytes {
			return Presence{}, fmt.Errorf("%w: key %s now reports %d bytes, expected %d",
				ErrIdentityMismatch, want.Key, p.Identity.SizeBytes, want.SizeBytes)
		}
		return p, nil
	}
	return Presence{}, ErrNotFound
}
