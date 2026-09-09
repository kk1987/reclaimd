package reclaimd

import (
	"context"
	"errors"
	"syscall"
	"testing"
)

// fakeRefreshDev stands in for the block device. A dropout in the middle of a
// whole-drive rewrite cannot be staged against real hardware, and it is the one
// path where getting recovery wrong leaves a drive half-rewritten -- worse than
// either finishing or never starting.
type fakeRefreshDev struct {
	written   map[int64]int
	readCount map[int64]int
	dropWrite map[int64]int   // offset -> how many more times writing there drops the bus
	dropRead  map[int64]int   // same, for reads
	readErr   map[int64]error // a media error that leaves the device present
	alive     bool
	reopenErr error

	opens, closes, syncs int
}

func newFakeRefreshDev() *fakeRefreshDev {
	return &fakeRefreshDev{
		written: map[int64]int{}, readCount: map[int64]int{},
		dropWrite: map[int64]int{}, dropRead: map[int64]int{},
		readErr: map[int64]error{}, alive: true,
	}
}

// open yields a handle; state stays on the device so coverage can be checked
// across reopens, which is the whole point.
func (d *fakeRefreshDev) open() refreshTarget {
	d.opens++
	return &fakeHandle{dev: d}
}

func (d *fakeRefreshDev) reopen() reopenFunc {
	return func(ctx context.Context) (refreshTarget, error) {
		if d.reopenErr != nil {
			return nil, d.reopenErr
		}
		d.alive = true
		return d.open(), nil
	}
}

type fakeHandle struct {
	dev  *fakeRefreshDev
	dead bool
}

func (h *fakeHandle) ReadAt(p []byte, off int64) (int, error) {
	if h.dead {
		return 0, syscall.ENODEV
	}
	if n := h.dev.dropRead[off]; n > 0 {
		h.dev.dropRead[off] = n - 1
		h.dead, h.dev.alive = true, false
		return 0, syscall.ENODEV
	}
	if err := h.dev.readErr[off]; err != nil {
		return 0, err
	}
	h.dev.readCount[off]++
	return len(p), nil
}

func (h *fakeHandle) WriteAt(p []byte, off int64) (int, error) {
	if h.dead {
		return 0, syscall.ENODEV
	}
	if n := h.dev.dropWrite[off]; n > 0 {
		h.dev.dropWrite[off] = n - 1
		h.dead, h.dev.alive = true, false
		return 0, syscall.ENODEV
	}
	h.dev.written[off]++
	return len(p), nil
}

func (h *fakeHandle) Sync() error {
	if h.dead {
		return syscall.ENODEV
	}
	h.dev.syncs++
	return nil
}

func (h *fakeHandle) Close() error { h.dev.closes++; return nil }

const rBlk = int64(1 << 20)

func runRewrite(t *testing.T, d *fakeRefreshDev, blocks int64, rewrite bool,
	maxDrops int) (refreshStats, error) {
	t.Helper()
	return rewriteRange(context.Background(), quietLogger(), d.open(), d.reopen(),
		func() bool { return d.alive }, make([]byte, rBlk),
		0, blocks*rBlk, rBlk, rewrite, maxDrops)
}

// The property that matters: a dropout must not leave a hole. Every block in
// range has to end up written, including the one that took the bus down.
func TestRefreshLeavesNoHoleAfterDropouts(t *testing.T) {
	const blocks = 64
	d := newFakeRefreshDev()
	for _, b := range []int64{0, 10, 33, 63} { // first and last included on purpose
		d.dropWrite[b*rBlk] = 1
	}

	st, err := runRewrite(t, d, blocks, false, 8)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	if st.Dropouts != 4 {
		t.Errorf("dropouts = %d, want 4", st.Dropouts)
	}
	for b := int64(0); b < blocks; b++ {
		if d.written[b*rBlk] == 0 {
			t.Errorf("block %d was never written: the dropout retry skipped it", b)
		}
	}
	if want := blocks * rBlk; st.Written != want {
		t.Errorf("written = %d, want %d (each block counted once)", st.Written, want)
	}
	if d.opens != 5 { // one initial plus one per dropout
		t.Errorf("opens = %d, want 5", d.opens)
	}
	if d.syncs != 1 {
		t.Errorf("syncs = %d, want exactly 1 at the end", d.syncs)
	}
}

// The block that dropped the bus is retried, not stepped over. Skipping it
// would leave a hole in the very refresh being performed.
func TestRefreshRetriesTheOffendingBlock(t *testing.T) {
	d := newFakeRefreshDev()
	d.dropWrite[7*rBlk] = 2 // fails twice before letting the write land

	st, err := runRewrite(t, d, 16, false, 8)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	if st.Dropouts != 2 {
		t.Errorf("dropouts = %d, want 2", st.Dropouts)
	}
	if got := d.written[7*rBlk]; got != 1 {
		t.Errorf("offending block written %d times, want exactly 1", got)
	}
}

// A drive that has genuinely stopped working must not be retried forever.
func TestRefreshGivesUpPastTheDropoutBudget(t *testing.T) {
	d := newFakeRefreshDev()
	d.dropWrite[3*rBlk] = 99 // never recovers

	st, err := runRewrite(t, d, 16, false, 3)
	if !errors.Is(err, ErrDeviceDisconnected) {
		t.Fatalf("got %v, want ErrDeviceDisconnected once past the budget", err)
	}
	if st.Dropouts != 4 { // budget of 3 means the fourth is fatal
		t.Errorf("dropouts = %d, want 4", st.Dropouts)
	}
}

// A single unreadable sector is not a dropout. Treating every EIO as one meant
// one bad sector burned the whole dropout budget -- close, reattach, retry the
// same block, fail again -- and aborted a refresh that should have stepped over
// it. The device staying present in sysfs is what tells the two apart.
func TestRefreshSkipsMediaErrorWithoutWritingItBack(t *testing.T) {
	d := newFakeRefreshDev()
	d.readErr[5*rBlk] = syscall.EIO // device stays alive

	st, err := runRewrite(t, d, 16, true, 3)
	if err != nil {
		t.Fatalf("a readable-sector failure must not fail the whole job: %v", err)
	}
	if st.Dropouts != 0 {
		t.Errorf("dropouts = %d; a media error on a live device is not a dropout", st.Dropouts)
	}
	if st.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", st.Skipped)
	}
	// The guard that matters most in the whole file: never write back a buffer
	// we failed to fill, or a recoverable retention problem becomes permanent
	// data loss.
	if n := d.written[5*rBlk]; n != 0 {
		t.Errorf("block whose read failed was written %d times; it must never be "+
			"written back", n)
	}
	if want := int64(15 * rBlk); st.Written != want {
		t.Errorf("written = %d, want %d (all but the unreadable block)", st.Written, want)
	}
}

// The same errno with the device actually gone is a dropout, and has to be
// handled as one.
func TestRefreshTreatsEIOAsDropoutWhenDeviceIsGone(t *testing.T) {
	d := newFakeRefreshDev()
	d.dropRead[5*rBlk] = 1 // sets alive=false, mimicking the device leaving

	st, err := runRewrite(t, d, 16, true, 3)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	if st.Dropouts != 1 {
		t.Errorf("dropouts = %d, want 1", st.Dropouts)
	}
	if st.Skipped != 0 {
		t.Errorf("skipped = %d; a vanished device is not a skippable sector", st.Skipped)
	}
	if d.written[5*rBlk] == 0 {
		t.Error("block was never written after the device came back")
	}
}

func TestRefreshSurfacesReopenFailure(t *testing.T) {
	d := newFakeRefreshDev()
	d.dropWrite[2*rBlk] = 1
	d.reopenErr = ErrNotFound

	if _, err := runRewrite(t, d, 8, false, 8); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want the reopen failure to surface", err)
	}
}

// The shape of a run observed on real hardware: five dropouts scattered
// through a whole-drive rewrite, and the drive still ends up completely
// rewritten.
func TestRefreshCompletesWholeDriveDespiteFiveDropouts(t *testing.T) {
	const blocks = 512
	d := newFakeRefreshDev()
	for _, b := range []int64{21694, 25336, 35666, 38022, 38794} {
		d.dropWrite[(b%blocks)*rBlk] = 1
	}

	st, err := runRewrite(t, d, blocks, false, 8)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	if st.Skipped != 0 {
		t.Errorf("skipped = %d, want 0", st.Skipped)
	}
	if want := blocks * rBlk; st.Written != want {
		t.Errorf("written = %d, want the whole range %d", st.Written, want)
	}
	missing := 0
	for b := int64(0); b < blocks; b++ {
		if d.written[b*rBlk] == 0 {
			missing++
		}
	}
	if missing != 0 {
		t.Errorf("%d blocks left unwritten", missing)
	}
}
