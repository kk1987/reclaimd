package reclaimd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeBSD stands in for a FreeBSD kernel: its sysctl tree, CAM's device table,
// the mount list and zpool. The mesh is written out with the nesting
// geom_dump.c produces, so the parser reads here what it reads on a real box.
type fakeBSD struct {
	sysctls    map[string]string
	disks      []camDisk
	mounts     []mountPoint
	geoms      []fakeGeom
	zpools     map[string][]string
	zpoolErr   error
	zpoolCalls int
}

type fakeGeom struct {
	class, name string
	uses        []fakeUse
	offers      []fakeProvider
}

type fakeUse struct{ provider, mode string }

type fakeProvider struct {
	name   string
	size   int64
	wither bool
}

// newFakeBSD starts from a stock install: ZFS on root, on an NVMe disk reached
// through its diskid label, and no USB storage.
func newFakeBSD() *fakeBSD {
	f := &fakeBSD{
		sysctls: map[string]string{"kern.maxphys": "\x00\x00\x10\x00\x00\x00\x00\x00"},
		disks:   []camDisk{{Periph: "nda", Unit: 0, Sim: "nvme", PathID: 0}},
		mounts:  []mountPoint{{From: "zroot/ROOT/default", On: "/", FSType: "zfs"}},
		zpools:  map[string][]string{"zroot": {"/dev/diskid/DISK-S67NNF1W179865p2"}},
	}
	f.geom("DISK", "nda0", nil, "nda0")
	f.geom("LABEL", "nda0", []fakeUse{{"nda0", "r1w1e3"}}, "diskid/DISK-S67NNF1W179865")
	f.geom("PART", "diskid/DISK-S67NNF1W179865", []fakeUse{{"diskid/DISK-S67NNF1W179865", "r1w1e2"}},
		"diskid/DISK-S67NNF1W179865p1", "diskid/DISK-S67NNF1W179865p2")
	f.geom("ZFS::VDEV", "zfs::vdev", []fakeUse{{"diskid/DISK-S67NNF1W179865p2", "r1w1e1"}})
	return f
}

func (f *fakeBSD) geom(class, name string, uses []fakeUse, offers ...string) {
	g := fakeGeom{class: class, name: name, uses: uses}
	for _, o := range offers {
		g.offers = append(g.offers, fakeProvider{name: o, size: 1 << 30})
	}
	f.geoms = append(f.geoms, g)
}

// addStick plugs in a USB stick: a umass node carrying the strings uhub(4)
// prints for it, a da periph on that node's SIM, and a DISK geom.
func (f *fakeBSD) addStick(umass, da int, serial string, size int64) {
	node := fmt.Sprintf("dev.umass.%d", umass)
	f.sysctls[node+".%pnpinfo"] = `vendor=0x090c product=0x1000 devclass=0x00 devsubclass=0x00 devproto=0x00 ` +
		`sernum="` + serial + `" release=0x1100 mode=host intclass=0x08 intsubclass=0x06 intprotocol=0x50` + "\x00"
	f.sysctls[node+".%location"] = fmt.Sprintf("bus=0 hubaddr=1 port=%d devaddr=%d interface=0 ugen=ugen0.%d\x00",
		umass+1, umass+2, umass+2)
	f.disks = append(f.disks, camDisk{Periph: "da", Unit: da, Sim: "umass-sim", SimUnit: umass,
		PathID: uint32(umass + 1), Vendor: "Generic", Product: "Flash Disk", Revision: "1100", Removable: true})
	f.disk(fmt.Sprintf("da%d", da), size)
}

// addLUN adds another slot to a card reader already plugged in.
func (f *fakeBSD) addLUN(umass, da int, lun uint64, size int64) {
	for _, d := range f.disks {
		if d.Sim == "umass-sim" && d.SimUnit == umass {
			d.Unit, d.LUN = da, lun
			f.disks = append(f.disks, d)
			f.disk(fmt.Sprintf("da%d", da), size)
			return
		}
	}
	panic("no reader on umass unit " + fmt.Sprint(umass))
}

func (f *fakeBSD) disk(name string, size int64) {
	f.geoms = append(f.geoms, fakeGeom{class: "DISK", name: name, offers: []fakeProvider{{name: name, size: size}}})
}

func (f *fakeBSD) platform() freeBSD {
	return freeBSD{
		sysctl: func(name string) ([]byte, error) {
			if name == "kern.geom.confxml" {
				return []byte(f.meshXML() + "\x00"), nil
			}
			if v, ok := f.sysctls[name]; ok {
				return []byte(v), nil
			}
			return nil, fmt.Errorf("sysctl %s: no such node", name)
		},
		cam:    func() ([]camDisk, error) { return f.disks, nil },
		mounts: func() ([]mountPoint, error) { return f.mounts, nil },
		zpool: func(pool string) ([]string, error) {
			f.zpoolCalls++
			return f.zpools[pool], f.zpoolErr
		},
		uptime: func() (time.Duration, error) { return time.Hour, nil },
		usbSim: "umass-sim",
	}
}

func (f *fakeBSD) meshXML() string {
	var b strings.Builder
	var classes []string
	byClass := map[string][]fakeGeom{}
	for _, g := range f.geoms {
		if byClass[g.class] == nil {
			classes = append(classes, g.class)
		}
		byClass[g.class] = append(byClass[g.class], g)
	}
	b.WriteString("<mesh>\n")
	for ci, class := range classes {
		fmt.Fprintf(&b, "  <class id=\"0xc%d\">\n    <name>%s</name>\n", ci, class)
		for gi, g := range byClass[class] {
			gid := fmt.Sprintf("0xg%d.%d", ci, gi)
			fmt.Fprintf(&b, "    <geom id=\"%s\">\n      <class ref=\"0xc%d\"/>\n      <name>%s</name>\n      <rank>1</rank>\n",
				gid, ci, g.name)
			for _, u := range g.uses {
				fmt.Fprintf(&b, "\t<consumer id=\"0xu\">\n\t  <geom ref=\"%s\"/>\n\t  <provider ref=\"0xp:%s\"/>\n"+
					"\t  <mode>%s</mode>\n\t</consumer>\n", gid, u.provider, u.mode)
			}
			for _, p := range g.offers {
				fmt.Fprintf(&b, "\t<provider id=\"0xp:%s\">\n\t  <geom ref=\"%s\"/>\n\t  <mode>r0w0e0</mode>\n"+
					"\t  <name>%s</name>\n\t  <mediasize>%d</mediasize>\n\t  <sectorsize>512</sectorsize>\n"+
					"\t  <stripesize>0</stripesize>\n\t  <stripeoffset>0</stripeoffset>\n", p.name, gid, p.name, p.size)
				if p.wither {
					b.WriteString("\t  <wither/>\n")
				} else {
					b.WriteString("\t  <config>\n\t    <ident>(null)</ident>\n\t    <descr>Generic Flash Disk</descr>\n\t  </config>\n")
				}
				b.WriteString("\t</provider>\n")
			}
			b.WriteString("    </geom>\n")
		}
		b.WriteString("  </class>\n")
	}
	b.WriteString("</mesh>\n")
	return b.String()
}

func discover(t *testing.T, f *fakeBSD) []Presence {
	t.Helper()
	disks, err := f.platform().Discover()
	if err != nil {
		t.Fatal(err)
	}
	return disks
}

// A stick carried between a FreeBSD machine and a Linux one has to land on the
// same history, so its key comes out of the same USB strings as it does in
// TestDiscoverStableSerialKey.
func TestFreeBSDKeysAStickAsLinuxDoes(t *testing.T) {
	f := newFakeBSD()
	f.addStick(0, 0, "0011223344556677", 64156073984)

	disks := discover(t, f)
	if len(disks) != 1 {
		t.Fatalf("want the stick alone, got %d disks", len(disks))
	}
	d := disks[0]
	if got, want := d.Identity.Key, "usb-090c:1000-0011223344556677"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if !d.Identity.KeyIsStable || d.Ignored {
		t.Errorf("stable=%v, ignored as %q", d.Identity.KeyIsStable, d.IgnoredReason)
	}
	if d.Node != "/dev/da0" || d.USBPath != "dev.umass.0" {
		t.Errorf("node %q, usb node %q", d.Node, d.USBPath)
	}
	if d.Identity.SizeBytes != 64156073984 || d.Identity.LogicalBlockSize != 512 {
		t.Errorf("size %d, sector %d", d.Identity.SizeBytes, d.Identity.LogicalBlockSize)
	}
	if d.Identity.Model != "Flash Disk" || d.Identity.BusNum != "0" || d.Identity.DevPath != "1.1" {
		t.Errorf("model %q, bus %q, port %q", d.Identity.Model, d.Identity.BusNum, d.Identity.DevPath)
	}
}

func TestFreeBSDSerialMayHoldASpace(t *testing.T) {
	pnp := `vendor=0x0781 product=0x5583 devclass=0x00 sernum="4C53 0001" release=0x0100 mode=host`
	if got := sernum(pnp); got != "4C53 0001" {
		t.Errorf("sernum = %q", got)
	}
}

// One read has to be one command, and FreeBSD computes the largest command da(4)
// will send without exporting it anywhere. These are daregister()'s cases.
func TestFreeBSDReadSizeFollowsDA(t *testing.T) {
	for _, tc := range []struct {
		name   string
		simMax int
		quirks string
		wantKB int
	}{
		{"USB 2: the SIM names no limit", 0, "0", 64},
		{"SuperSpeed: the SIM offers maxphys", 1 << 20, "0", 1024},
		{"never above maxphys", 4 << 20, "0", 1024},
		{"the 128KB quirk", 1 << 20, "0x200<128KB>", 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBSD()
			f.addStick(0, 0, "S", 1<<30)
			f.disks[len(f.disks)-1].MaxIO = tc.simMax
			f.sysctls["kern.cam.da.0.quirks"] = tc.quirks + "\x00"

			id := discover(t, f)[0].Identity
			if id.MaxSectorsKB != tc.wantKB {
				t.Errorf("limit %d KiB, want %d", id.MaxSectorsKB, tc.wantKB)
			}
			if got := (Config{}).BlockSizeFor(id); got != tc.wantKB<<10 {
				t.Errorf("read size %d, want %d", got, tc.wantKB<<10)
			}
		})
	}
}

func TestFreeBSDLeavesOtherDisksAlone(t *testing.T) {
	f := newFakeBSD()
	f.disks = append(f.disks, camDisk{Periph: "da", Unit: 7, Sim: "mpr", PathID: 9, Removable: true})
	f.disk("da7", 4<<40)
	f.addStick(0, 0, "ENCLOSURE", 2<<40)
	f.disks[len(f.disks)-1].Removable = false
	f.addStick(1, 1, "EMPTYSLOT", 0)

	disks := discover(t, f)
	if len(disks) != 2 {
		t.Fatalf("want the two USB disks, got %+v", disks)
	}
	if d := byKernelName(disks, "da0"); d == nil || d.IgnoredReason != CodeNotRemovable {
		t.Errorf("USB-attached fixed disk should be ignored as %s, got %+v", CodeNotRemovable, d)
	}
	if d := byKernelName(disks, "da1"); d == nil || d.IgnoredReason != CodeDeviceNotReady {
		t.Errorf("a reader with no card should be ignored as %s, got %+v", CodeDeviceNotReady, d)
	}
}

func TestFreeBSDMultiLUNKeepsStableKeys(t *testing.T) {
	f := newFakeBSD()
	f.addStick(3, 1, "058F63666433", 31914983424)
	f.addLUN(3, 2, 1, 15957491712)

	disks := discover(t, f)
	a, b := byKernelName(disks, "da1"), byKernelName(disks, "da2")
	if a == nil || b == nil {
		t.Fatalf("want both slots, got %+v", disks)
	}
	if !a.Identity.KeyIsStable || !b.Identity.KeyIsStable {
		t.Errorf("slots of one reader must keep stable keys: %q %q", a.Identity.Key, b.Identity.Key)
	}
	if a.Identity.Key == b.Identity.Key || !strings.Contains(b.Identity.Key, "-lun") {
		t.Errorf("slots must be told apart by LUN: %q %q", a.Identity.Key, b.Identity.Key)
	}
}

func TestFreeBSDSerialCollisionDowngradesBoth(t *testing.T) {
	f := newFakeBSD()
	f.addStick(4, 3, "AAAA", 1<<30)
	f.addStick(5, 4, "AAAA", 2<<30)

	disks := discover(t, f)
	for _, d := range disks {
		if d.Identity.KeyIsStable || !strings.HasPrefix(d.Identity.Key, "path-") {
			t.Errorf("%s: colliding serial must downgrade, got %q", d.KernelName, d.Identity.Key)
		}
	}
	if disks[0].Identity.Key == disks[1].Identity.Key {
		t.Fatalf("downgraded keys still collide: %q", disks[0].Identity.Key)
	}
}

// Booting from a stick is as real on FreeBSD as it is on Linux, and / there is
// usually a GPT label on a partition: two geoms up from the disk.
func TestFreeBSDIgnoresUFSRootOnAStick(t *testing.T) {
	f := newFakeBSD()
	f.mounts = []mountPoint{{From: "/dev/gpt/rootfs", On: "/", FSType: "ufs"}}
	f.addStick(0, 0, "BOOTSTICK", 30<<30)
	f.geom("PART", "da0", []fakeUse{{"da0", "r1w1e1"}}, "da0p1", "da0p2")
	f.geom("LABEL", "da0p2", []fakeUse{{"da0p2", "r1w1e1"}}, "gpt/rootfs")
	f.geom("VFS", "ffs.gpt/rootfs", []fakeUse{{"gpt/rootfs", "r1w1e1"}})

	if d := discover(t, f)[0]; d.IgnoredReason != CodeRootDevice {
		t.Fatalf("the stick carrying / must be ignored as %s, got %+v", CodeRootDevice, d)
	}
}

// twoPoolSticks is a machine booted from a ZFS pool on one stick, with a
// second pool on another stick mounted elsewhere.
func twoPoolSticks() *fakeBSD {
	f := newFakeBSD()
	f.mounts = []mountPoint{
		{From: "zboot/ROOT/default", On: "/", FSType: "zfs"},
		{From: "tank/media", On: "/tank/media", FSType: "zfs"},
	}
	f.zpools = map[string][]string{"zboot": {"/dev/da0p2"}, "tank": {"/dev/da1p1"}}
	f.addStick(0, 0, "BOOT", 16<<30)
	f.geom("PART", "da0", []fakeUse{{"da0", "r1w1e1"}}, "da0p1", "da0p2")
	f.addStick(1, 1, "TANK", 64<<30)
	f.geom("PART", "da1", []fakeUse{{"da1", "r1w1e1"}}, "da1p1")
	f.geom("ZFS::VDEV", "zfs::vdev", []fakeUse{{"da0p2", "r1w1e1"}, {"da1p1", "r1w1e1"}})
	return f
}

func TestFreeBSDIgnoresTheRootPoolStickOnly(t *testing.T) {
	f := twoPoolSticks()
	disks := discover(t, f)
	if d := byKernelName(disks, "da0"); d == nil || d.IgnoredReason != CodeRootDevice {
		t.Errorf("the root pool's stick must be ignored as %s, got %+v", CodeRootDevice, d)
	}
	if d := byKernelName(disks, "da1"); d == nil || d.Ignored {
		t.Errorf("a stick in another pool is fair game, got %+v", d)
	}
}

func TestFreeBSDProtectsEveryVdevStickWhenZpoolFails(t *testing.T) {
	f := twoPoolSticks()
	f.zpoolErr = errors.New("zpool: not found")
	for _, d := range discover(t, f) {
		if d.IgnoredReason != CodeRootDevice {
			t.Errorf("%s: with the pool unresolved every vdev stick must be held back, got %q",
				d.KernelName, d.IgnoredReason)
		}
	}
}

// A ZFS root is the default install, so asking zpool on every tick regardless
// would mean a process every thirty seconds on nearly every machine.
func TestFreeBSDAsksZpoolOnlyWhenAStickCarriesAVdev(t *testing.T) {
	f := newFakeBSD()
	f.addStick(0, 0, "PLAIN", 8<<30)
	if d := discover(t, f)[0]; d.Ignored {
		t.Fatalf("plain stick ignored as %s", d.IgnoredReason)
	}
	if f.zpoolCalls != 0 {
		t.Errorf("zpool was run %d times with no vdev on any stick", f.zpoolCalls)
	}
}

func TestFreeBSDCheckNotInUse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stack func(f *fakeBSD)
		want  string // empty for no refusal, else part of the message
	}{
		{"idle", func(f *fakeBSD) {
			f.geom("PART", "da0", []fakeUse{{"da0", "r0w0e0"}}, "da0p1")
		}, ""},
		{"open by a process, the daemon included", func(f *fakeBSD) {
			f.geom("PART", "da0", []fakeUse{{"da0", "r1w0e0"}}, "da0p1")
			f.geom("DEV", "da0", []fakeUse{{"da0", "r1w0e0"}})
			f.geom("DEV", "da0p1", []fakeUse{{"da0p1", "r1w0e0"}})
		}, ""},
		{"mounted read-only through a label", func(f *fakeBSD) {
			f.geom("PART", "da0", []fakeUse{{"da0", "r1w0e0"}}, "da0p1")
			f.geom("LABEL", "da0p1", []fakeUse{{"da0p1", "r1w0e0"}}, "msdosfs/STICK")
			f.geom("VFS", "msdosfs.msdosfs/STICK", []fakeUse{{"msdosfs/STICK", "r1w0e0"}})
			f.mounts = append(f.mounts, mountPoint{From: "/dev/msdosfs/STICK", On: "/media/stick", FSType: "msdosfs"})
		}, "mounted at /media/stick"},
		{"swap", func(f *fakeBSD) {
			f.geom("PART", "da0", []fakeUse{{"da0", "r1w1e1"}}, "da0p2")
			f.geom("SWAP", "swap", []fakeUse{{"da0p2", "r1w1e0"}})
		}, "swap"},
		{"geli", func(f *fakeBSD) {
			f.geom("PART", "da0", []fakeUse{{"da0", "r1w1e1"}}, "da0p1")
			f.geom("ELI", "da0p1.eli", []fakeUse{{"da0p1", "r1w1e1"}}, "da0p1.eli")
		}, "GEOM class ELI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBSD()
			f.addStick(0, 0, "S", 8<<30)
			tc.stack(f)
			pl := f.platform()

			err := pl.CheckNotInUse(discover(t, f)[0])
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (!errors.Is(err, ErrDeviceMounted) || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want ErrDeviceMounted saying %q", err, tc.want)
			}
		})
	}
}

// A stick that drops and comes straight back can reappear under the same
// umass unit with the same serial before the round has let go of the old
// disk. What gives it away is the old provider, which withers.
func TestFreeBSDAlive(t *testing.T) {
	f := newFakeBSD()
	f.addStick(0, 0, "0011223344556677", 8<<30)
	pl := f.platform()
	p := discover(t, f)[0]
	stick := &f.geoms[len(f.geoms)-1].offers[0]

	if !pl.Alive(p) {
		t.Fatal("a present stick reads as gone")
	}
	stick.wither = true
	if pl.Alive(p) {
		t.Error("a withering provider reads as alive")
	}
	stick.wither = false

	pnp := "dev.umass.0.%pnpinfo"
	f.sysctls[pnp] = strings.Replace(f.sysctls[pnp], "0011223344556677", "FFFFFFFFFFFFFFFF", 1)
	if pl.Alive(p) {
		t.Error("a different stick under the same umass unit reads as alive")
	}
	delete(f.sysctls, pnp)
	if pl.Alive(p) {
		t.Error("a detached umass node reads as alive")
	}
}

// GEOM keeps devstat rows of its own for providers, named in full. The disk
// driver's row is the one with a unit number.
func TestDevstatRowIsTheDriversRow(t *testing.T) {
	le := binary.LittleEndian
	b := make([]byte, 8+3*288)
	row := func(i int, dev string, unit int32, read, writes uint64, started, ended uint32) {
		r := b[8+i*288:]
		le.PutUint32(r[8:], started)
		le.PutUint32(r[12:], ended)
		copy(r[44:60], dev)
		le.PutUint32(r[60:], uint32(unit))
		le.PutUint64(r[72:], read)
		le.PutUint64(r[112:], writes)
	}
	row(0, "nda", 0, 1<<40, 5, 1, 1)
	row(1, "da0", -1, 999<<9, 999, 0, 0)
	row(2, "da", 0, 100*512, 7, 5, 3)

	st, err := devstatRow(b, "da0")
	if err != nil {
		t.Fatal(err)
	}
	if want := (diskStat{sectorsRead: 100, writes: 7, inFlight: 2}); st != want {
		t.Errorf("got %+v, want %+v", st, want)
	}
	if _, err := devstatRow(b, "da1"); err == nil {
		t.Error("a disk with no row was found anyway")
	}
}

func TestZpoolVdevsTakesTheLeaves(t *testing.T) {
	out := "zboot\t14.5G\t1.20G\t13.3G\t-\t-\t0%\t8%\t1.00x\tONLINE\t-\n" +
		"\tmirror-0\t14.5G\t1.20G\t13.3G\t-\t-\t0%\t8.2%\t-\tONLINE\n" +
		"\t/dev/da0p2\t-\t-\t-\t-\t-\t-\t-\t-\tONLINE\n" +
		"\t/dev/gpt/boot1\t-\t-\t-\t-\t-\t-\t-\t-\tONLINE\n"
	got := zpoolVdevs([]byte(out))
	if len(got) != 2 || got[0] != "/dev/da0p2" || got[1] != "/dev/gpt/boot1" {
		t.Errorf("got %q", got)
	}
}
