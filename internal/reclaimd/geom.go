package reclaimd

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// freeBSD is the FreeBSD platform. Where Linux lays a disk out as files under
// /sys, FreeBSD answers the same questions from three tables: CAM's says which
// da(4) disk sits on which SCSI bus, the umass(4) node's sysctls say which USB
// device drives that bus, and the GEOM mesh says how big the disk is and what
// is stacked on top of it. Reading them sends nothing to a device.
//
// Every kernel query is a field, so the joining -- the part that decides which
// stick is which -- is tested on any machine, and platform_freebsd.go holds
// only the queries themselves.
type freeBSD struct {
	sysctl func(name string) ([]byte, error)
	cam    func() ([]camDisk, error)
	mounts func() ([]mountPoint, error)
	zpool  func(pool string) ([]string, error)
	uptime func() (time.Duration, error)

	// usbSim is the name umass(4) gives its CAM SIM. The SIM is registered
	// under the unit number of the umass device itself -- umass.c passes
	// sc_unit to cam_sim_alloc -- so umass-sim3 is dev.umass.3. It is a field
	// so that a test can stand a CTL disk in for a stick.
	usbSim string
}

// camDisk is one periph from CAM's device table, with what CAM holds about
// the bus and the device under it.
type camDisk struct {
	Periph    string // "da"
	Unit      int
	Sim       string // "umass-sim"
	SimUnit   int
	PathID    uint32 // the scbus number
	BusID     uint32
	Target    uint32
	LUN       uint64
	Vendor    string // SCSI INQUIRY, space-trimmed
	Product   string
	Revision  string
	Removable bool
	// MaxIO is the SIM's own transfer limit from XPT_PATH_INQ. Zero means the
	// SIM states none.
	MaxIO int
}

func (d camDisk) name() string { return d.Periph + strconv.Itoa(d.Unit) }

type mountPoint struct{ From, On, FSType string }

// Discover joins the disks CAM has on the umass SIM with their USB identity
// and their GEOM provider.
func (f freeBSD) Discover() ([]Presence, error) {
	all, err := f.cam()
	if errors.Is(err, os.ErrPermission) {
		return nil, fmt.Errorf("read the CAM device table: %w (on FreeBSD that takes root)", err)
	}
	if err != nil {
		return nil, fmt.Errorf("read the CAM device table: %w", err)
	}
	mesh, err := f.mesh()
	if err != nil {
		return nil, err
	}
	maxphys, err := f.sysctlUint("kern.maxphys")
	if err != nil {
		return nil, err
	}

	var disks []camDisk
	var names []string
	for _, d := range all {
		if d.Periph == "da" && d.Sim == f.usbSim {
			disks = append(disks, d)
			names = append(names, d.name())
		}
	}
	protected, err := f.protectedDisks(mesh, names)
	if err != nil {
		// As on Linux: not knowing what backs / must not quietly switch off
		// the guard that keeps us off the system disk.
		return nil, fmt.Errorf("resolve protected devices: %w", err)
	}

	now := time.Now()
	driver := strings.TrimSuffix(f.usbSim, "-sim")
	var found []Presence
	for _, d := range disks {
		node := fmt.Sprintf("dev.%s.%d", driver, d.SimUnit)
		usb, err := f.usbDevice(node)
		if err != nil {
			// Detached between CAM's table and its sysctls. The next tick
			// will not list it either.
			continue
		}
		name := d.name()
		id := DiskIdentity{
			VendorID:     usb.vendor,
			ProductID:    usb.product,
			Serial:       usb.serial,
			Vendor:       d.Vendor,
			Model:        d.Product,
			Revision:     d.Revision,
			SCSIAddr:     fmt.Sprintf("%d:%d:%d:%d", d.PathID, d.BusID, d.Target, d.LUN),
			BusNum:       usb.bus,
			DevPath:      usb.port,
			MaxSectorsKB: f.maxIO(d, int(maxphys)) / 1024,
		}
		pp := mesh.providers[name]
		if pp != nil {
			id.SizeBytes, id.LogicalBlockSize = pp.mediasize, pp.sectorsize
		}

		p := Presence{Identity: id, KernelName: name, Node: "/dev/" + name, USBPath: node, SeenAt: now}
		switch {
		case pp == nil || pp.withering || id.SizeBytes <= 0:
			p.Ignored, p.IgnoredReason = true, CodeDeviceNotReady
		case !d.Removable:
			// USB-attached but fixed media, left alone as on Linux: this tool
			// is about flash retention.
			p.Ignored, p.IgnoredReason = true, CodeNotRemovable
		case protected[name]:
			p.Ignored, p.IgnoredReason = true, CodeRootDevice
		}
		found = append(found, p)
	}

	assignKeys(found)
	return found, nil
}

// dfltphys is DFLTPHYS from sys/param.h.
const dfltphys = 64 << 10

// maxIO is the largest read da(4) hands its SIM as one command. FreeBSD keeps
// that in the disk's d_maxsize and exports it nowhere, so it is worked out here
// the way daregister() in scsi_da.c works it out: the SIM's maxio, DFLTPHYS
// when the SIM gives none, never more than kern.maxphys, and at most 128 KiB
// under the quirk of that name. umass(4) gives a maxio only at SuperSpeed, so a
// stick in a USB 2 port reads 64 KiB at a time.
func (f freeBSD) maxIO(d camDisk, maxphys int) int {
	n := d.MaxIO
	switch {
	case n == 0:
		n = dfltphys
	case n > maxphys:
		n = maxphys
	}
	q, err := f.sysctlString(fmt.Sprintf("kern.cam.%s.%d.quirks", d.Periph, d.Unit))
	if err == nil && hasFlag(q, "128KB") {
		n = min(n, 128<<10)
	}
	return n
}

// hasFlag reads a kernel "%b" bitmask such as "0x204<NO_PREVENT,128KB>".
func hasFlag(s, flag string) bool {
	_, list, ok := strings.Cut(s, "<")
	if !ok {
		return false
	}
	list, _, _ = strings.Cut(list, ">")
	for _, name := range strings.Split(list, ",") {
		if name == flag {
			return true
		}
	}
	return false
}

type usbDevice struct{ vendor, product, serial, bus, port string }

// usbDevice reads a USB device's identity from the two strings uhub(4) prints
// for each of its children (usb_hub.c):
//
//	%pnpinfo   vendor=0x090c product=0x1000 ... sernum="0011223344556677" release=0x1100 ...
//	%location  bus=0 hubaddr=1 port=2 devaddr=3 interface=0 ugen=ugen0.3
//
// The vendor and product come out as the same four lowercase hex digits sysfs
// shows, and the serial is the same string, so a stick carried between a
// FreeBSD machine and a Linux one is filed under the same key on both.
func (f freeBSD) usbDevice(node string) (usbDevice, error) {
	pnp, err := f.sysctlString(node + ".%pnpinfo")
	if err != nil {
		return usbDevice{}, err
	}
	loc, err := f.sysctlString(node + ".%location")
	if err != nil {
		return usbDevice{}, err
	}
	p, l := keyValues(pnp), keyValues(loc)
	u := usbDevice{
		vendor:  strings.TrimPrefix(p["vendor"], "0x"),
		product: strings.TrimPrefix(p["product"], "0x"),
		serial:  sernum(pnp),
		bus:     l["bus"],
		port:    l["hubaddr"] + "." + l["port"],
	}
	if u.vendor == "" || u.product == "" {
		return usbDevice{}, fmt.Errorf("%s: no USB identity in %q", node, pnp)
	}
	return u, nil
}

func keyValues(s string) map[string]string {
	out := map[string]string{}
	for _, field := range strings.Fields(s) {
		if k, v, ok := strings.Cut(field, "="); ok {
			out[k] = v
		}
	}
	return out
}

// sernum is the one quoted value in %pnpinfo, and the only one that can hold a
// space, so it is cut out by its delimiters rather than split on whitespace.
func sernum(pnp string) string {
	_, rest, ok := strings.Cut(pnp, `sernum="`)
	if !ok {
		return ""
	}
	serial, _, ok := strings.Cut(rest, `" release=`)
	if !ok {
		return ""
	}
	return strings.TrimSpace(serial)
}

// protectedDisks names the disks under the guarded mount points, however deep:
// through a partition, a label, a mirror or an encryption layer.
//
// A ZFS mount names a dataset rather than a device, and which devices a pool is
// built on is kept in its own labels, not anywhere GEOM shows. So zpool(8) is
// asked -- but only when one of the USB disks carries a ZFS vdev at all.
// Otherwise the answer cannot matter, and on a ZFS root, the default install,
// asking would mean a zpool process on every discovery tick.
func (f freeBSD) protectedDisks(m *geomMesh, candidates []string) (map[string]bool, error) {
	mps, err := f.mounts()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	pools := map[string]bool{}
	for _, mp := range mps {
		if !guardedMounts[mp.On] {
			continue
		}
		if mp.FSType == "zfs" {
			pool, _, _ := strings.Cut(mp.From, "/")
			pools[pool] = true
		} else if dev, ok := strings.CutPrefix(mp.From, "/dev/"); ok {
			m.disksUnder(dev, out)
		}
	}

	var carriers []string
	for _, name := range candidates {
		if pp := m.providers[name]; pp != nil && pp.carries("ZFS::VDEV") {
			carriers = append(carriers, name)
		}
	}
	if len(pools) == 0 || len(carriers) == 0 {
		return out, nil
	}
	for pool := range pools {
		vdevs, err := f.zpool(pool)
		if err != nil {
			// Not knowing which disks hold the pool must not mean protecting
			// none of them.
			for _, name := range carriers {
				out[name] = true
			}
			continue
		}
		for _, v := range vdevs {
			m.disksUnder(strings.TrimPrefix(v, "/dev/"), out)
		}
	}
	return out, nil
}

// zpoolVdevs reads `zpool list -vHP`: the pool on the first line, then one
// tab-separated line per vdev, the leaves named by their full /dev path.
func zpoolVdevs(out []byte) []string {
	var devs []string
	for _, line := range strings.Split(string(out), "\n") {
		for _, field := range strings.Split(line, "\t") {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			if strings.HasPrefix(field, "/dev/") {
				devs = append(devs, field)
			}
			break
		}
	}
	return devs
}

// Alive asks whether the umass node still answers with the same identity and
// the disk's GEOM provider is still there, at the same size and not withering.
// A disk that has left the bus fails the second test even when the same stick
// is already back under the same umass unit: its old provider withers while
// anything still holds it open.
func (f freeBSD) Alive(p Presence) bool {
	usb, err := f.usbDevice(p.USBPath)
	if err != nil || usb.vendor != p.Identity.VendorID ||
		usb.product != p.Identity.ProductID || usb.serial != p.Identity.Serial {
		return false
	}
	m, err := f.mesh()
	if err != nil {
		return false
	}
	pp := m.providers[p.KernelName]
	return pp != nil && !pp.withering && pp.mediasize == p.Identity.SizeBytes
}

// geomLookThrough are the classes that republish what is under them and hold
// it open only while something above them does: a partition table, a label.
// DEV is a process with the node open -- the daemon itself while it scans --
// which is no more a claim here than an open without O_EXCL is on Linux.
var geomLookThrough = map[string]bool{"PART": true, "LABEL": true, "DEV": true}

// CheckNotInUse walks the mesh upward from the disk looking for anything that
// holds it, or a partition of it, for its own purposes.
func (f freeBSD) CheckNotInUse(p Presence) error {
	m, err := f.mesh()
	if err != nil {
		return err
	}
	pp := m.providers[p.KernelName]
	if pp == nil {
		return fmt.Errorf("%w: %s has no GEOM provider", ErrNotFound, p.Node)
	}
	h := pp.holder()
	if h == nil {
		return nil
	}
	dev := "/dev/" + h.provider.name
	switch h.geom.class {
	case "VFS":
		if mps, err := f.mounts(); err == nil {
			for _, mp := range mps {
				if mp.From == dev {
					return fmt.Errorf("%w: %s is mounted at %s", ErrDeviceMounted, dev, mp.On)
				}
			}
		}
		return fmt.Errorf("%w: %s is mounted", ErrDeviceMounted, dev)
	case "SWAP":
		return fmt.Errorf("%w: %s is in use as swap", ErrDeviceMounted, dev)
	case "ZFS::VDEV":
		return fmt.Errorf("%w: %s is part of a ZFS pool", ErrDeviceMounted, dev)
	}
	return fmt.Errorf("%w: %s is held open by GEOM class %s", ErrDeviceMounted, dev, h.geom.class)
}

// IOStats reads the disk's row from kern.devstat.all, the table iostat(8)
// reads.
func (f freeBSD) IOStats(p Presence) (diskStat, error) {
	b, err := f.sysctl("kern.devstat.all")
	if err != nil {
		return diskStat{}, err
	}
	return devstatRow(b, p.KernelName)
}

// devstatRow picks one disk out of kern.devstat.all: a long generation number,
// then one struct devstat per device (sys/devicestat.h, DEVSTAT_VERSION 6).
// The offsets are for LP64 little-endian, which amd64 and arm64 both are.
//
// A disk driver's own row is named by driver and unit, "da" and 0. GEOM adds
// rows of its own for providers, named in full with unit -1, and those are
// skipped.
func devstatRow(b []byte, name string) (diskStat, error) {
	const (
		rowSize    = 288
		startCount = 8
		endCount   = 12
		deviceName = 44
		unitNumber = 60
		bytesRead  = 64 + 8*1 // bytes[DEVSTAT_READ]
		writeOps   = 96 + 8*2 // operations[DEVSTAT_WRITE]
	)
	le := binary.LittleEndian
	for off := 8; off+rowSize <= len(b); off += rowSize {
		row := b[off : off+rowSize]
		unit := int32(le.Uint32(row[unitNumber:]))
		if unit < 0 || cString(row[deviceName:deviceName+16])+strconv.Itoa(int(unit)) != name {
			continue
		}
		return diskStat{
			sectorsRead: le.Uint64(row[bytesRead:]) / sectorSize,
			writes:      le.Uint64(row[writeOps:]),
			inFlight:    uint64(le.Uint32(row[startCount:]) - le.Uint32(row[endCount:])),
		}, nil
	}
	return diskStat{}, fmt.Errorf("no devstat row for %s", name)
}

func (f freeBSD) Uptime() (time.Duration, error) { return f.uptime() }

func (f freeBSD) mesh() (*geomMesh, error) {
	b, err := f.sysctl("kern.geom.confxml")
	if err != nil {
		return nil, err
	}
	return parseGeomMesh(bytes.TrimRight(b, "\x00"))
}

func (f freeBSD) sysctlString(name string) (string, error) {
	b, err := f.sysctl(name)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimRight(b, "\x00")), nil
}

func (f freeBSD) sysctlUint(name string) (uint64, error) {
	b, err := f.sysctl(name)
	if err != nil {
		return 0, err
	}
	switch len(b) {
	case 4:
		return uint64(binary.LittleEndian.Uint32(b)), nil
	case 8:
		return binary.LittleEndian.Uint64(b), nil
	}
	return 0, fmt.Errorf("sysctl %s: %d bytes is not an integer", name, len(b))
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// geomMesh is kern.geom.confxml -- the whole GEOM graph as geom_dump.c writes
// it -- indexed by provider name.
type geomMesh struct {
	providers map[string]*gProvider
}

type gGeom struct {
	class, name string
	consumers   []*gConsumer // what it is built on
	providers   []*gProvider // what it offers up
}

type gProvider struct {
	name       string
	mediasize  int64
	sectorsize int
	withering  bool
	geom       *gGeom
	consumers  []*gConsumer // attached to it from above
}

type gConsumer struct {
	geom     *gGeom
	provider *gProvider
	mode     string // "r1w1e0": open counts for read, write and exclusive
}

func parseGeomMesh(b []byte) (*geomMesh, error) {
	var doc struct {
		Classes []struct {
			Name  string `xml:"name"`
			Geoms []struct {
				Name      string `xml:"name"`
				Consumers []struct {
					Provider struct {
						Ref string `xml:"ref,attr"`
					} `xml:"provider"`
					Mode string `xml:"mode"`
				} `xml:"consumer"`
				Providers []struct {
					ID         string    `xml:"id,attr"`
					Name       string    `xml:"name"`
					Aliases    []string  `xml:"alias"`
					Mediasize  int64     `xml:"mediasize"`
					Sectorsize int       `xml:"sectorsize"`
					Wither     *struct{} `xml:"wither"`
				} `xml:"provider"`
			} `xml:"geom"`
		} `xml:"class"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse kern.geom.confxml: %w", err)
	}

	m := &geomMesh{providers: map[string]*gProvider{}}
	byID := map[string]*gProvider{}
	type link struct {
		consumer *gConsumer
		ref      string
	}
	var links []link
	for _, c := range doc.Classes {
		for _, g := range c.Geoms {
			geom := &gGeom{class: c.Name, name: g.Name}
			for _, x := range g.Providers {
				pp := &gProvider{name: x.Name, mediasize: x.Mediasize, sectorsize: x.Sectorsize,
					withering: x.Wither != nil, geom: geom}
				geom.providers = append(geom.providers, pp)
				byID[x.ID] = pp
				for _, n := range append([]string{x.Name}, x.Aliases...) {
					// A disk's own provider wins any clash over its name.
					if old := m.providers[n]; old == nil || c.Name == "DISK" {
						m.providers[n] = pp
					}
				}
			}
			for _, x := range g.Consumers {
				cp := &gConsumer{geom: geom, mode: x.Mode}
				geom.consumers = append(geom.consumers, cp)
				links = append(links, link{cp, x.Provider.Ref})
			}
		}
	}
	for _, l := range links {
		if pp := byID[l.ref]; pp != nil {
			l.consumer.provider = pp
			pp.consumers = append(pp.consumers, l.consumer)
		}
	}
	return m, nil
}

// disksUnder adds the DISK geoms a provider is ultimately built on.
func (m *geomMesh) disksUnder(name string, out map[string]bool) {
	if pp := m.providers[name]; pp != nil {
		pp.geom.disksUnder(out)
	}
}

func (g *gGeom) disksUnder(out map[string]bool) {
	if g.class == "DISK" {
		out[g.name] = true
		return
	}
	for _, c := range g.consumers {
		if c.provider != nil {
			c.provider.geom.disksUnder(out)
		}
	}
}

// holder finds the first thing above a provider that holds it open for its
// own purposes: a mounted filesystem, swap, a ZFS vdev, a geli or gmirror
// layer.
func (pp *gProvider) holder() *gConsumer {
	for _, c := range pp.consumers {
		if !geomLookThrough[c.geom.class] && c.mode != "r0w0e0" {
			return c
		}
		for _, up := range c.geom.providers {
			if h := up.holder(); h != nil {
				return h
			}
		}
	}
	return nil
}

// carries reports whether a geom of the given class sits anywhere above.
func (pp *gProvider) carries(class string) bool {
	for _, c := range pp.consumers {
		if c.geom.class == class {
			return true
		}
		for _, up := range c.geom.providers {
			if up.carries(class) {
				return true
			}
		}
	}
	return false
}
