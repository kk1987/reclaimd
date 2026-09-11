package reclaimd

import (
	"strings"
	"testing"
)

// A live frame goes out once and is then forgotten.
//
// Holding it was how a finished round kept arriving at the browser: the
// publisher sent every entry the map held on any tick that any disk had made
// dirty, so while a second disk was still sweeping, the finished round's last
// frame went out twice a second forever. The page reads a frame as proof that a
// scan is running, so it climbed back into the live panel and froze there,
// showing a position and an elapsed time that never moved again.
func TestPublisherSendsEachFrameOnce(t *testing.T) {
	s := &Server{pending: map[string]LiveProgress{}}

	s.onLive(LiveProgress{Disk: "a", PosMiB: 96})
	if got := s.drainPending(); len(got) != 1 || got[0].PosMiB != 96 {
		t.Fatalf("first drain = %+v, want one frame at 96 MiB", got)
	}
	if got := s.drainPending(); got != nil {
		t.Fatalf("second drain = %+v, want nothing", got)
	}

	// A disk that is still busy must not drag a quiet one's last frame along.
	s.onLive(LiveProgress{Disk: "a", PosMiB: 128})
	s.onLive(LiveProgress{Disk: "b", PosMiB: 4})
	if got := s.drainPending(); len(got) != 2 {
		t.Fatalf("drain = %d frames, want both disks", len(got))
	}
	s.onLive(LiveProgress{Disk: "b", PosMiB: 8})
	got := s.drainPending()
	if len(got) != 1 || got[0].Disk != "b" {
		t.Fatalf("drain = %+v, want only the disk that moved", got)
	}
}

// The end of a round takes the queued frame with it. Frames are coalesced to
// twice a second, so one produced in the last half-second is still in hand when
// the round ends. Delivered after SCAN_END, it would tell the page a scan is
// running, and nothing further ever arrives to correct that.
func TestScanEndDropsTheQueuedFrame(t *testing.T) {
	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sup := &Supervisor{store: store, logger: quietLogger()}
	s := NewServer(mustConfig(t), store, sup, quietLogger())

	sup.OnLive(LiveProgress{Disk: "a", PosMiB: 96})
	sup.OnLive(LiveProgress{Disk: "b", PosMiB: 12})
	sup.OnChange("a", "SCAN_END")

	got := s.drainPending()
	if len(got) != 1 || got[0].Disk != "b" {
		t.Fatalf("drain after SCAN_END = %+v, want only the disk still scanning", got)
	}
}

// The fleet list used to be handed out in Go map order, which is deliberately
// random: the cards changed places on every poll, and the disk the page selects
// on load, the first one in the list, was whichever the runtime yielded first.
// Present disks come first, then by key, and nothing else moves a card.
func TestFleetListComesBackInAStableOrder(t *testing.T) {
	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sup := &Supervisor{store: store, logger: quietLogger(), disks: map[string]*diskState{
		"usb-c": {Key: "usb-c", Present: true},
		"usb-a": {Key: "usb-a"},
		"usb-b": {Key: "usb-b", Present: true},
		"usb-d": {Key: "usb-d"},
	}}
	s := NewServer(mustConfig(t), store, sup, quietLogger())

	const want = "usb-b usb-c usb-a usb-d"
	for i := 0; i < 20; i++ {
		var keys []string
		for _, v := range s.views(false) {
			keys = append(keys, v.Key)
		}
		if got := strings.Join(keys, " "); got != want {
			t.Fatalf("call %d: order %q, want %q", i, got, want)
		}
	}
}

// A Scan now the daemon has taken but not yet started used to live only in the
// tab that sent it, so reloading in that gap brought the button back for a
// round already on its way. The list has to say so itself.
func TestFleetListReportsAScanRequestNotYetStarted(t *testing.T) {
	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sup := &Supervisor{store: store, logger: quietLogger(), disks: map[string]*diskState{
		"usb-a": {Key: "usb-a", Present: true},
	}}
	s := NewServer(mustConfig(t), store, sup, quietLogger())

	if v := s.views(false); len(v) != 1 || v[0].ScanRequested {
		t.Fatalf("before the request: %+v", v)
	}
	if err := sup.RequestScan("usb-a", false); err != nil {
		t.Fatal(err)
	}
	if v := s.views(false); len(v) != 1 || !v[0].ScanRequested {
		t.Fatalf("after the request: %+v, want scan_requested", v)
	}
}

// A Stop takes the round a moment to act on. A reload in that moment has to
// show the round winding down, and it must not offer Stop again.
func TestFleetListReportsARoundThatIsStopping(t *testing.T) {
	store, err := OpenStore(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sup := &Supervisor{store: store, logger: quietLogger(), disks: map[string]*diskState{
		"usb-a": {Key: "usb-a", Present: true, Scanning: true, stop: func() {}},
	}}
	s := NewServer(mustConfig(t), store, sup, quietLogger())

	if err := sup.StopScan("usb-a"); err != nil {
		t.Fatal(err)
	}
	if v := s.views(false); len(v) != 1 || !v[0].Scanning || !v[0].Stopping {
		t.Fatalf("after Stop: %+v, want scanning and stopping", v)
	}
}
