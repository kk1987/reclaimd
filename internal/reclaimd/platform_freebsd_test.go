//go:build freebsd && (amd64 || arm64)

package reclaimd

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These run against the kernel they are built on. For the CAM and devstat
// layouts they are the only check there is: a wrong offset does not fail to
// compile, it reads garbage.

func kernDisks(t *testing.T) []string {
	t.Helper()
	b, err := sysctlRaw("kern.disks")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(cString(b))
}

func TestLiveMeshHasEveryDisk(t *testing.T) {
	m, err := DefaultPlatform().(freeBSD).mesh()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range kernDisks(t) {
		pp := m.providers[name]
		if pp == nil || pp.geom.class != "DISK" || pp.mediasize <= 0 || pp.sectorsize <= 0 {
			t.Errorf("%s: provider %+v", name, pp)
		}
	}
}

func TestLiveDevstatHasEveryDisk(t *testing.T) {
	pl := DefaultPlatform().(freeBSD)
	if v, err := pl.sysctlUint("kern.devstat.version"); err != nil || v != 6 {
		t.Fatalf("kern.devstat.version = %d (%v); the row layout is version 6's", v, err)
	}
	for _, name := range kernDisks(t) {
		if _, err := pl.IOStats(Presence{KernelName: name}); err != nil {
			t.Error(err)
		}
	}
}

func TestLiveMountsIncludeRoot(t *testing.T) {
	mps, err := mountPoints()
	if err != nil {
		t.Fatal(err)
	}
	for _, mp := range mps {
		if mp.On == "/" && mp.From != "" && mp.FSType != "" {
			return
		}
	}
	t.Fatalf("no / among %d mounts: %+v", len(mps), mps)
}

func TestLiveUptimeAgreesWithBoottime(t *testing.T) {
	up, err := monotonicUptime()
	if err != nil {
		t.Fatal(err)
	}
	b, err := sysctlRaw("kern.boottime") // struct timeval
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	boot := time.Unix(int64(le.Uint64(b)), int64(le.Uint64(b[8:]))*1000)
	if d := time.Since(boot) - up; d < -2*time.Second || d > 2*time.Second {
		t.Errorf("uptime %v, but kern.boottime says %v", up, time.Since(boot))
	}
}

// TestLiveCAMMatchesCamcontrol reads the device table both ways and compares:
// every periph camcontrol devlist names has to come back on the same bus,
// target and LUN, under the same SIM.
func TestLiveCAMMatchesCamcontrol(t *testing.T) {
	disks, err := camDisks()
	if errors.Is(err, os.ErrPermission) {
		t.Skip("reading the CAM device table takes root")
	}
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("camcontrol", "devlist", "-v").Output()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]camDisk{}
	for _, d := range disks {
		got[d.name()] = d
	}

	// scbus1 on umass-sim0 bus 0:
	// <Generic Flash Disk 8.07>          at scbus1 target 0 lun 0 (da0,pass1)
	sims := map[uint32]string{}
	var checked int
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		var bus uint32
		var sim string
		if n, _ := fmt.Sscanf(line, "scbus%d on %s bus", &bus, &sim); n == 2 {
			sims[bus] = sim
			continue
		}
		_, rest, ok := strings.Cut(line, " at scbus")
		if !ok {
			continue
		}
		var target uint32
		var lun uint64
		var periphs string
		if n, _ := fmt.Sscanf(rest, "%d target %d lun %x %s", &bus, &target, &lun, &periphs); n != 4 {
			continue
		}
		for _, name := range strings.Split(strings.Trim(periphs, "()"), ",") {
			if strings.HasPrefix(name, "pass") {
				continue
			}
			d, ok := got[name]
			if !ok {
				t.Errorf("%s: in camcontrol's list, not in ours", name)
				continue
			}
			if d.PathID != bus || d.Target != target || d.LUN != lun ||
				fmt.Sprintf("%s%d", d.Sim, d.SimUnit) != sims[bus] {
				t.Errorf("%s: ours says %s%d at scbus%d target %d lun %d; camcontrol says %s at scbus%d target %d lun %d",
					name, d.Sim, d.SimUnit, d.PathID, d.Target, d.LUN, sims[bus], bus, target, lun)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Skip("no CAM periphs besides pass on this machine")
	}
}

// TestLiveCTLStandsInForAStick runs discovery, the in-use check, the counters,
// real reads and a whole round against a CTL ramdisk exported through CAM, with
// a USB identity faked into sysctl: everything a stick would exercise short of
// the umass node itself. Set up as root with
//
//	kldload ctl
//	ctladm create -b ramdisk -s 536870912 -o removable=on
//	ctladm port -o on -p "$(ctladm portlist | awk '$3 == "camsim" { print $1 }')"
//
// and run as root with RECLAIMD_TEST_SIM=camsim.
func TestLiveCTLStandsInForAStick(t *testing.T) {
	sim := os.Getenv("RECLAIMD_TEST_SIM")
	if sim == "" {
		t.Skip("set RECLAIMD_TEST_SIM to a CAM SIM carrying a spare disk, such as CTL's")
	}
	pl := DefaultPlatform().(freeBSD)
	pl.usbSim = sim
	kernel := pl.sysctl
	node := "dev." + strings.TrimSuffix(sim, "-sim") + "."
	pl.sysctl = func(name string) ([]byte, error) {
		switch {
		case strings.HasPrefix(name, node) && strings.HasSuffix(name, ".%pnpinfo"):
			return []byte(`vendor=0x090c product=0x1000 sernum="CTLSTANDIN" release=0x0100`), nil
		case strings.HasPrefix(name, node) && strings.HasSuffix(name, ".%location"):
			return []byte("bus=9 hubaddr=1 port=1 devaddr=2 interface=0"), nil
		}
		return kernel(name)
	}

	disks, err := pl.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) == 0 {
		t.Fatalf("no disk on SIM %s", sim)
	}
	p := disks[0]
	if p.Ignored {
		t.Fatalf("%s ignored as %s", p.Node, p.IgnoredReason)
	}
	t.Logf("%s: %d bytes in %d-byte sectors, %d KiB per command, key %s", p.Node,
		p.Identity.SizeBytes, p.Identity.LogicalBlockSize, p.Identity.MaxSectorsKB, p.Identity.Key)

	if err := pl.CheckNotInUse(p); err != nil {
		t.Fatalf("an idle disk was refused: %v", err)
	}
	if !pl.Alive(p) {
		t.Fatal("a present disk reads as gone")
	}

	bs := (Config{}).BlockSizeFor(p.Identity)
	dev, err := OpenDevice(p, bs, pl)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	// The same block twice, counted twice: nothing between the read and the
	// driver answered the second one from a cache.
	before, err := pl.IOStats(p)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := dev.ReadBlock(0); err != nil {
			t.Fatal(err)
		}
	}
	after, err := pl.IOStats(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := after.sectorsRead-before.sectorsRead, uint64(2*bs/sectorSize); got < want {
		t.Errorf("two reads of one block moved the counters %d sectors, want %d", got, want)
	}

	if _, err := dev.ReadBlock(int64(bs) + 1); !errors.Is(err, ErrAlignment) {
		t.Errorf("unaligned read: got %v, want ErrAlignment", err)
	}

	// And one whole round, down the path the scan command takes.
	cfg, err := LoadConfigFromFile("")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sum, err := NewSupervisor(cfg, store, quietLogger(), pl).ScanOnce(context.Background(), p.Identity.Key, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("round: %s, %d MiB in %v", sum.Outcome, sum.BytesRead>>20, sum.EndedAt.Sub(sum.StartedAt))
	if sum.Outcome != OutcomeClean || !sum.Completed || sum.BytesRead < p.Identity.SizeBytes-int64(bs) {
		t.Errorf("a round over a ramdisk: %+v", sum)
	}
}
