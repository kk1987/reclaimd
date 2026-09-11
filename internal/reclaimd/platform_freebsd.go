//go:build freebsd && (amd64 || arm64)

package reclaimd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

// DefaultPlatform is CAM, GEOM and sysctl on the running kernel.
func DefaultPlatform() Platform {
	return freeBSD{
		sysctl: sysctlRaw,
		cam:    camDisks,
		mounts: mountPoints,
		zpool:  zpoolDisks,
		uptime: monotonicUptime,
		usbSim: "umass-sim",
	}
}

const defaultStateDir = "/var/db/reclaimd"

// kernelVersion is the kernel's name and release, from kern.ostype and
// kern.osrelease.
func kernelVersion() (name, release string) {
	name, _ = syscall.Sysctl("kern.ostype")
	release, _ = syscall.Sysctl("kern.osrelease")
	return name, release
}

// scanOpenFlags has nothing to add. FreeBSD has had no block devices since
// 4.0: /dev/da0 is a character device with no buffer cache in front of it, so
// every read reaches the driver, and O_DIRECT would change nothing.
const scanOpenFlags = os.O_RDONLY | syscall.O_CLOEXEC

// refreshOpenFlags has no O_EXCL, because GEOM ignores it: g_dev_open() keeps
// it under #ifdef notyet. What refuses the open instead is GEOM's access rule,
// which denies a writer while anything else holds the disk exclusively. A
// read-write mount, swap on a partition and a ZFS vdev all do, handed down
// through the partition table. A read-only mount holds nothing exclusively, so
// against that one CheckNotInUse is the only gate.
const refreshOpenFlags = os.O_RDWR | syscall.O_CLOEXEC

func onTmpfs(dir string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(dir, &st) == nil && int8String(st.Fstypename[:]) == "tmpfs"
}

func int8String(s []int8) string {
	b := make([]byte, 0, len(s))
	for _, c := range s {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// sysctlRaw reads a sysctl by name, as bytes. syscall.Sysctl would do for a
// string, but it drops a trailing zero byte, which in a binary table is data.
func sysctlRaw(name string) ([]byte, error) {
	mib, err := sysctlMIB(name)
	if err != nil {
		return nil, fmt.Errorf("sysctl %s: %w", name, err)
	}
	for range 8 {
		var n uintptr
		if err := sysctl(mib, nil, &n); err != nil {
			return nil, fmt.Errorf("sysctl %s: %w", name, err)
		}
		if n == 0 {
			return nil, nil
		}
		// Headroom, because a table can grow between asking for its size and
		// reading it: a disk attaches, a provider appears.
		n += n / 4
		buf := make([]byte, n)
		err := sysctl(mib, &buf[0], &n)
		switch {
		case errors.Is(err, syscall.ENOMEM), errors.Is(err, syscall.EBUSY):
			// EBUSY is kern.devstat.all saying its generation moved mid-read.
			continue
		case err != nil:
			return nil, fmt.Errorf("sysctl %s: %w", name, err)
		}
		return buf[:n], nil
	}
	return nil, fmt.Errorf("sysctl %s: kept changing while being read", name)
}

// sysctlMIB turns a name into its OID through sysctl.name2oid, the {0, 3}
// node that sysctl(8) and syscall.Sysctl use too.
func sysctlMIB(name string) ([]int32, error) {
	oid := [2]int32{0, 3}
	var mib [24 + 2]int32 // CTL_MAXNAME, plus the slack syscall keeps
	n := uintptr(len(mib)) * 4
	b := []byte(name)
	_, _, e := syscall.Syscall6(syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&oid[0])), 2,
		uintptr(unsafe.Pointer(&mib[0])), uintptr(unsafe.Pointer(&n)),
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	if e != 0 {
		return nil, e
	}
	return mib[:n/4], nil
}

func sysctl(mib []int32, old *byte, oldlen *uintptr) error {
	_, _, e := syscall.Syscall6(syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(old)), uintptr(unsafe.Pointer(oldlen)), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// CAM's ioctl ABI at CAM_VERSION 0x1a, from cam_ccb.h and scsi_all.h on
// FreeBSD 15.1, the same on amd64 and arm64. The request number carries
// sizeof(union ccb), so a kernel with a different layout refuses the request
// with ENOTTY before it can read the buffer wrong.
const (
	camIOCommand = 0xc4e01a02 // CAMIOCOMMAND
	ccbSize      = 1248       // sizeof(union ccb)

	ccbFuncCode  = 80 // struct ccb_hdr
	ccbStatus    = 84
	ccbPathID    = 96
	ccbTargetID  = 100
	ccbTargetLUN = 104

	cdmStatus      = 200 // struct ccb_dev_match
	cdmNumMatches  = 224
	cdmMatchBufLen = 228
	cdmMatches     = 232

	cpiMaxIO = 448 // struct ccb_pathinq

	matchSize = 800 // sizeof(struct dev_match_result)
	matchBody = 8   // offsetof(struct dev_match_result, result)

	xptPathInq  = 0x004
	xptDevMatch = 0x409

	devMatchPeriph        = 0
	devMatchDevice        = 1
	devMatchBus           = 2
	devResultUnconfigured = 0x1

	camDevMatchLast   = 0
	camDevMatchMore   = 1
	camXPTPathID      = 0xffffffff
	camTargetWildcard = 0xffffffff
	camLUNWildcard    = 0xffffffff
	camStatusMask     = 0x3f
	camReqCmp         = 0x01

	inquiryRMB = 0x80 // SID_RMB, in scsi_inquiry_data.dev_qual2
)

// camDisks reads CAM's device table with XPT_DEV_MATCH, as camcontrol devlist
// does, and asks each disk's SIM for its transfer limit with XPT_PATH_INQ.
// Both answer from what the kernel already holds, and neither reaches a device.
// /dev/xpt0 belongs to root alone, and so, on FreeBSD, does discovery.
func camDisks() ([]camDisk, error) {
	// xptopen() refuses anything but read-write.
	xpt, err := os.OpenFile("/dev/xpt0", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer xpt.Close()

	// The kernel writes its results through a pointer inside the request, so
	// they go to memory outside the Go heap, where nothing can move it.
	results, err := syscall.Mmap(-1, 0, 128*matchSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, err
	}
	defer syscall.Munmap(results)

	le := binary.LittleEndian
	ccb := make([]byte, ccbSize)
	le.PutUint32(ccb[ccbFuncCode:], xptDevMatch)
	le.PutUint32(ccb[ccbPathID:], camXPTPathID)
	le.PutUint32(ccb[ccbTargetID:], camTargetWildcard)
	le.PutUint64(ccb[ccbTargetLUN:], camLUNWildcard)
	le.PutUint32(ccb[cdmMatchBufLen:], uint32(len(results)))
	le.PutUint64(ccb[cdmMatches:], uint64(uintptr(unsafe.Pointer(&results[0]))))

	type bus struct {
		sim      string
		unit, id uint32
	}
	type address struct {
		path, target uint32
		lun          uint64
	}
	buses := map[uint32]bus{}
	inquiries := map[address][]byte{}
	var disks []camDisk
	for {
		if err := camIoctl(xpt, ccb); err != nil {
			return nil, fmt.Errorf("XPT_DEV_MATCH: %w", err)
		}
		status := le.Uint32(ccb[cdmStatus:])
		if status != camDevMatchLast && status != camDevMatchMore {
			return nil, fmt.Errorf("XPT_DEV_MATCH: match status %d", status)
		}
		for i := range int(le.Uint32(ccb[cdmNumMatches:])) {
			r := results[i*matchSize : (i+1)*matchSize]
			body := r[matchBody:]
			switch le.Uint32(r) {
			case devMatchBus: // struct bus_match_result
				buses[le.Uint32(body)] = bus{sim: cString(body[4:20]),
					unit: le.Uint32(body[20:]), id: le.Uint32(body[24:])}
			case devMatchDevice: // struct device_match_result
				if le.Uint32(body[788:])&devResultUnconfigured != 0 {
					continue
				}
				a := address{le.Uint32(body), le.Uint32(body[4:]), le.Uint64(body[8:])}
				inquiries[a] = append([]byte(nil), body[20:20+36]...)
			case devMatchPeriph: // struct periph_match_result
				disks = append(disks, camDisk{
					Periph: cString(body[:16]),
					Unit:   int(le.Uint32(body[16:])),
					PathID: le.Uint32(body[20:]),
					Target: le.Uint32(body[24:]),
					LUN:    le.Uint64(body[32:]),
				})
			}
		}
		if status == camDevMatchLast {
			break
		}
	}

	limits := map[uint32]int{}
	out := disks[:0]
	for _, d := range disks {
		b, ok := buses[d.PathID]
		inq, ok2 := inquiries[address{d.PathID, d.Target, d.LUN}]
		if !ok || !ok2 || d.Periph == "pass" {
			continue
		}
		d.Sim, d.SimUnit, d.BusID = b.sim, int(b.unit), b.id
		d.Removable = inq[1]&inquiryRMB != 0
		d.Vendor = inquiryString(inq[8:16])
		d.Product = inquiryString(inq[16:32])
		d.Revision = inquiryString(inq[32:36])
		if _, ok := limits[d.PathID]; !ok {
			if limits[d.PathID], err = camMaxIO(xpt, d.PathID); err != nil {
				return nil, err
			}
		}
		d.MaxIO = limits[d.PathID]
		out = append(out, d)
	}
	return out, nil
}

func camMaxIO(xpt *os.File, path uint32) (int, error) {
	le := binary.LittleEndian
	ccb := make([]byte, ccbSize)
	le.PutUint32(ccb[ccbFuncCode:], xptPathInq)
	le.PutUint32(ccb[ccbPathID:], path)
	le.PutUint32(ccb[ccbTargetID:], camTargetWildcard)
	le.PutUint64(ccb[ccbTargetLUN:], camLUNWildcard)
	if err := camIoctl(xpt, ccb); err != nil {
		return 0, fmt.Errorf("XPT_PATH_INQ on scbus%d: %w", path, err)
	}
	return int(le.Uint32(ccb[cpiMaxIO:])), nil
}

func camIoctl(xpt *os.File, ccb []byte) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, xpt.Fd(), camIOCommand,
		uintptr(unsafe.Pointer(&ccb[0])))
	if e != 0 {
		return e
	}
	if s := binary.LittleEndian.Uint32(ccb[ccbStatus:]) & camStatusMask; s != camReqCmp {
		return fmt.Errorf("CAM status %#x", s)
	}
	return nil
}

// inquiryString trims an INQUIRY field, which is space-padded ASCII.
func inquiryString(b []byte) string {
	s := cString(b)
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}

const mntNoWait = 2 // MNT_NOWAIT, sys/mount.h

func mountPoints() ([]mountPoint, error) {
	n, err := syscall.Getfsstat(nil, mntNoWait)
	if err != nil {
		return nil, err
	}
	// Room for a mount that lands between the two calls.
	buf := make([]syscall.Statfs_t, n+8)
	if n, err = syscall.Getfsstat(buf, mntNoWait); err != nil {
		return nil, err
	}
	out := make([]mountPoint, 0, n)
	for _, st := range buf[:n] {
		out = append(out, mountPoint{
			From:   int8String(st.Mntfromname[:]),
			On:     int8String(st.Mntonname[:]),
			FSType: int8String(st.Fstypename[:]),
		})
	}
	return out, nil
}

// zpoolDisks asks zpool(8) which devices a pool is built on.
func zpoolDisks(pool string) ([]string, error) {
	out, err := exec.Command("/sbin/zpool", "list", "-vHP", pool).Output()
	if err != nil {
		return nil, fmt.Errorf("zpool list %s: %w", pool, err)
	}
	return zpoolVdevs(out), nil
}

const clockMonotonic = 4 // CLOCK_MONOTONIC, time.h

// monotonicUptime reads CLOCK_MONOTONIC, which FreeBSD serves from nanouptime:
// time since boot, which setting the wall clock does not move.
func monotonicUptime() (time.Duration, error) {
	var ts syscall.Timespec
	_, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockMonotonic,
		uintptr(unsafe.Pointer(&ts)), 0)
	if e != 0 {
		return 0, e
	}
	return time.Duration(ts.Nano()), nil
}
