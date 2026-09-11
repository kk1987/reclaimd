package reclaimd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestLatencyRoundTrip(t *testing.T) {
	m := NewLatencyMap(1<<20, 61184, 42, time.Unix(1757000000, 0))
	m.Baseline = 10 * time.Millisecond
	m.Values[0] = EncodeLatency(9 * time.Millisecond)
	m.Values[31] = EncodeLatency(1792 * time.Millisecond)
	m.Values[32] = LatError
	m.Values[33] = LatSkipped

	b, err := m.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if want := latHeaderLen + 61184*2; len(b) != want {
		t.Errorf("encoded size %d, want %d", len(b), want)
	}

	var got LatencyMap
	if err := got.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	if got.BlockSize != 1<<20 || got.BlockCount != 61184 || got.RoundSeq != 42 {
		t.Errorf("header mismatch: %+v", got)
	}
	if got.Baseline != 10*time.Millisecond {
		t.Errorf("baseline = %v, want 10ms", got.Baseline)
	}

	// 32us units exist to hold both ends of the measured distribution: a 9ms
	// normal read and a 1792ms controller hang.
	for _, tc := range []struct {
		idx  int
		want time.Duration
	}{{0, 9 * time.Millisecond}, {31, 1792 * time.Millisecond}} {
		d, ok := DecodeLatency(got.Values[tc.idx])
		if !ok {
			t.Fatalf("index %d decoded as a sentinel", tc.idx)
		}
		if diff := d - tc.want; diff > LatencyUnit || diff < -LatencyUnit {
			t.Errorf("index %d: %v, want %v (within one 32us quantum)", tc.idx, d, tc.want)
		}
	}
	if _, ok := DecodeLatency(got.Values[32]); ok {
		t.Error("LatError must not decode as a measurement")
	}
	if _, ok := DecodeLatency(got.Values[33]); ok {
		t.Error("LatSkipped must not decode as a measurement")
	}
}

// A single hang averaged with healthy neighbours disappears. Taking the max is
// what keeps the interesting sample alive through downsampling.
func TestCoarseTakesWorstNotMean(t *testing.T) {
	m := NewLatencyMap(1<<20, 64, 1, time.Now())
	for i := range m.Values {
		m.Values[i] = EncodeLatency(10 * time.Millisecond)
	}
	m.Values[31] = EncodeLatency(1792 * time.Millisecond)

	c := m.Coarse(32)
	if len(c) != 2 {
		t.Fatalf("want 2 segments, got %d", len(c))
	}
	d, ok := DecodeLatency(c[0])
	if !ok {
		t.Fatal("segment 0 decoded as sentinel")
	}
	if d < 1700*time.Millisecond {
		t.Errorf("segment 0 = %v; the 1792ms outlier was averaged away", d)
	}
}

// A segment where only some blocks were read should report the measurement.
// Reporting the skip would make partial coverage look like no coverage.
func TestCoarsePrefersMeasurementOverSkip(t *testing.T) {
	m := NewLatencyMap(1<<20, 32, 1, time.Now())
	m.Values[5] = EncodeLatency(12 * time.Millisecond)
	c := m.Coarse(32)
	if _, ok := DecodeLatency(c[0]); !ok {
		t.Fatal("segment with one real sample reported as skipped")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const key = "usb-090c:1000-0011223344556677"
	meta := Meta{Identity: DiskIdentity{Key: key, SizeBytes: 64156073984}, Enabled: true}
	if err := s.SaveMeta(key, meta); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity.Key != key || !got.Enabled || got.Schema != stateSchema {
		t.Errorf("meta round trip mismatch: %+v", got)
	}

	sc := NewSchedule(mustConfig(t), time.Now())
	if err := s.SaveSchedule(key, sc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSchedule(key); err != nil {
		t.Fatal(err)
	}

	p := Progress{Cursor: 32 << 20, RoundSeq: 7, SuppressUntil: time.Now().Add(24 * time.Hour)}
	if err := s.SaveProgress(key, p); err != nil {
		t.Fatal(err)
	}
	gp, err := s.LoadProgress(key)
	if err != nil {
		t.Fatal(err)
	}
	if gp.Cursor != p.Cursor || gp.RoundSeq != 7 {
		t.Errorf("progress round trip mismatch: %+v", gp)
	}
}

func TestStoreRejectsFutureSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const key = "k"
	if _, err := s.ensureDisk(key); err != nil {
		t.Fatal(err)
	}
	blob := fmt.Sprintf(`{"schema":%d,"enabled":true}`, stateSchema+1)
	if err := os.WriteFile(filepath.Join(s.diskDir(key), "meta.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadMeta(key); !errors.Is(err, ErrStateUnsupported) {
		t.Fatalf("got %v, want ErrStateUnsupported -- parsing a newer schema and "+
			"writing it back is how state gets destroyed rather than merely misread", err)
	}
}

func TestStoreLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := OpenStore(dir, quietLogger()); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second open returned %v, want ErrLockHeld", err)
	}
}

// ".lat" is a suffix of ".lat8", so a naive match would let the two retention
// tiers prune each other.
func TestPruneKeepsTiersSeparate(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const key = "k"
	for seq := uint64(1); seq <= 5; seq++ {
		m := NewLatencyMap(1<<20, 64, seq, time.Now())
		if err := s.SaveProfile(key, m, 32, 2, 4); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.diskDir(key), "history"))
	if err != nil {
		t.Fatal(err)
	}
	var full, coarse int
	for _, e := range entries {
		switch filepath.Ext(e.Name()) {
		case ".lat":
			full++
		case ".lat8":
			coarse++
		}
	}
	if full != 2 {
		t.Errorf("full profiles kept = %d, want 2", full)
	}
	if coarse != 4 {
		t.Errorf("coarse profiles kept = %d, want 4", coarse)
	}
}

// ---------------------------------------------------------------------------
// Durability
// ---------------------------------------------------------------------------

// TestWriteAtomicSurvivesKill spawns a child that writes the state file in a
// tight loop, SIGKILLs it at an arbitrary moment, and checks what is left.
//
// After a router loses power, the file on disk has to be either the previous
// complete version or the next complete version, never a truncated one. A
// zero-length state file is the classic symptom of skipping the
// parent-directory fsync.
func TestWriteAtomicSurvivesKill(t *testing.T) {
	if os.Getenv("RECLAIMD_ATOMIC_CHILD") != "" {
		atomicWriteChild()
		return
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "progress.json")

	for attempt := 0; attempt < 5; attempt++ {
		cmd := exec.Command(os.Args[0], "-test.run=TestWriteAtomicSurvivesKill")
		cmd.Env = append(os.Environ(), "RECLAIMD_ATOMIC_CHILD="+target)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(20+attempt*17) * time.Millisecond)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		b, err := os.ReadFile(target)
		if os.IsNotExist(err) {
			continue // killed before the first rename; nothing to check yet
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 {
			t.Fatalf("attempt %d: state file is zero length after kill", attempt)
		}
		var p Progress
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatalf("attempt %d: state file is not valid JSON after kill: %v\n%q",
				attempt, err, b)
		}
		if p.RoundSeq == 0 {
			t.Fatalf("attempt %d: decoded a structurally valid but empty record", attempt)
		}
	}

	// The temp file may survive a kill, but it must never be mistaken for the
	// real one. The rename is what publishes a write.
	if _, err := os.Stat(target + ".tmp"); err == nil {
		t.Log("a leftover .tmp is expected; it is never read back")
	}
}

func atomicWriteChild() {
	target := os.Getenv("RECLAIMD_ATOMIC_CHILD")
	for seq := uint64(1); ; seq++ {
		p := Progress{
			Schema:    stateSchema,
			Cursor:    int64(seq) << 20,
			RoundSeq:  seq,
			UpdatedAt: time.Now(),
		}
		b, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			os.Exit(1)
		}
		if err := writeAtomic(target, append(b, '\n'), 0o600); err != nil {
			os.Exit(1)
		}
	}
}

// mustConfig returns the defaults resolved against the drive from the
// forensics, whose max_sectors_kb is 1024. Block size is derived per disk in
// production, so a test that wants to talk about blocks has to say which disk
// it means. Every fake disk in this package models that one.
func mustConfig(t *testing.T) Config {
	t.Helper()
	c, err := LoadConfigFromFile("")
	if err != nil {
		t.Fatal(err)
	}
	return c.ForDisk(DiskIdentity{MaxSectorsKB: 1024})
}

// Deleting a disk takes its whole directory, and the key that names it comes
// from a URL. A wildcard matches one path segment, but an escaped slash inside
// it unescapes after routing, so what reaches the store is network text on its
// way to an os.RemoveAll.
func TestDeleteDiskWipesOneDiskAndRefusesATraversal(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const key = "usb-090c:1000-0011223344556677"
	if err := store.SaveMeta(key, Meta{Identity: DiskIdentity{Key: key}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSchedule(key, NewSchedule(mustConfig(t), time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(key, Event{Type: EventAdopted}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFreshness(key, []uint32{1, 2, 3}); err != nil {
		t.Fatal(err)
	}

	// A second disk is here to prove the delete is not a state-directory wipe.
	const other = "usb-abcd:1234-KEEPME"
	if err := store.SaveMeta(other, Meta{Identity: DiskIdentity{Key: other}, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"", ".", "..", "../..", "../" + other, ".hidden",
		"usb-090c:1000-0011223344556677/../" + other, "usb/090c"} {
		if err := store.DeleteDisk(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeleteDisk(%q) = %v, want ErrNotFound", bad, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "disks")); err != nil {
		t.Fatalf("the disks directory did not survive the traversal attempts: %v", err)
	}

	if err := store.DeleteDisk(key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "disks", key)); !os.IsNotExist(err) {
		t.Errorf("the disk directory is still there: %v", err)
	}
	if _, err := store.LoadMeta(key); !errors.Is(err, ErrNotFound) {
		t.Errorf("LoadMeta after delete = %v, want ErrNotFound", err)
	}
	keys, err := store.ListDisks()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != other {
		t.Errorf("ListDisks = %v, want just the other disk", keys)
	}

	// Deleting what is already gone is not an error worth a 500 upstream.
	if err := store.DeleteDisk(key); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}
