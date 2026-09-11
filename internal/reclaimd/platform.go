package reclaimd

import (
	"fmt"
	"strings"
	"time"
)

// Platform is the part of this program that depends on the kernel underneath:
// where USB disks are found, how a vanished disk is told apart from a bad
// sector, what else is using a disk, and the counters and clock the scheduler
// reads. Scanning, backoff, scheduling and the store sit above it and are the
// same program everywhere.
//
// Roots is the Linux implementation and freeBSD the FreeBSD one. A kernel
// without one still builds, and says so the first time it is asked for a disk.
type Platform interface {
	// Discover lists every whole disk behind USB, including the ones
	// deliberately left alone, with keys assigned. It must not open a device:
	// on a stick left to autosuspend, a poll that touched the bus would wake
	// it every time.
	Discover() ([]Presence, error)

	// Alive reports whether a disk is still attached as the same hardware. It
	// is the tiebreak for EIO, which looks the same for one unreadable sector
	// and for a device that has left the bus, so it must not touch the bus
	// either.
	Alive(p Presence) bool

	// CheckNotInUse refuses a disk that is mounted, used as swap or otherwise
	// claimed, whole or through any partition of it.
	CheckNotInUse(p Presence) error

	// IOStats reads the kernel's running I/O counters for one disk.
	IOStats(p Presence) (diskStat, error)

	// Uptime is time since boot, on a clock that setting the wall clock does
	// not move.
	Uptime() (time.Duration, error)
}

// guardedMounts are the mount points whose disks discovery never offers up.
var guardedMounts = map[string]bool{"/": true, "/boot": true, "/boot/efi": true, "/usr": true}

// assignKeys fills in Key for every discovered disk, downgrading to an unstable
// path-derived key when a serial is missing or shared.
//
// Some vendors ship an entire production run with one hardcoded serial. Two
// sticks answering to the same key would silently share history and, worse,
// could be mistaken for each other after a re-enumeration. Downgrading both
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
			// Downgrade both sides of a collision. A path key changes when the
			// stick moves to another port, and that instability is the point:
			// losing history is far cheaper than attributing one stick's
			// history, and its dropout suppression, to another.
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

// FindByKey re-resolves a key to wherever the disk lives right now.
//
// This is the function that makes a dropout survivable. After a re-enumeration
// the old path may well name a different disk, so nothing may be reused from
// before: the whole presence is rebuilt from a fresh scan, and the capacity is
// re-checked as a last line of defence against a stranger answering to a
// cloned serial.
func FindByKey(pl Platform, want DiskIdentity) (Presence, error) {
	disks, err := pl.Discover()
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
