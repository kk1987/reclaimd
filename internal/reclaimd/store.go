package reclaimd

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const stateSchema = 1

// Meta is the per-disk identity record: what this disk is, and whether we are
// managing it.
type Meta struct {
	Schema       int          `json:"schema"`
	Identity     DiskIdentity `json:"identity"`
	FirstSeen    time.Time    `json:"first_seen"`
	AdoptedAt    time.Time    `json:"adopted_at,omitempty"`
	Enabled      bool         `json:"enabled"`
	BytesWritten int64        `json:"bytes_written_by_tool"`
}

// Progress is the small, frequently-fsynced record. It is deliberately separate
// from Schedule so that the write on the dropout path stays around 200 bytes:
// that write has to survive a power cut in the next second, and a small file is
// a faster and more atomic thing to force to the medium.
type Progress struct {
	Schema        int       `json:"schema"`
	Cursor        int64     `json:"cursor"`
	RoundSeq      uint64    `json:"round_seq"`
	SuppressUntil time.Time `json:"suppress_until,omitempty"`
	LastOutcome   string    `json:"last_outcome,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Store owns the state directory. There is one instance per daemon, and the
// flock enforces that.
type Store struct {
	root   string
	logger *slog.Logger
	lock   *os.File

	mu      sync.Mutex
	eventID uint64
	// written accumulates what this process has written per disk since the last
	// TakeBytesWritten. A tool whose whole justification is wear should be able
	// to state its own wear bill, and the footer of the status page prints it.
	written map[string]int64
}

// OpenStore prepares the state directory and takes the process lock.
//
// The tmpfs check is there for OpenWrt, where /var is a symlink to /tmp, so the
// compiled-in default of /var/lib/reclaimd lands on a tmpfs that evaporates at
// every reboot. The schedule would reset at every boot, and so would the
// 24-hour dropout suppression, which is worse. Neither reset would report
// anything, and the only symptom would be a daemon that never quite seems to do
// its job.
func OpenStore(root string, logger *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "disks"), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", root, err)
	}

	if onTmpfs(root) {
		logger.Warn("state directory is on tmpfs; schedule and dropout suppression "+
			"will not survive a reboot -- pass -state-dir to somewhere persistent",
			"code", CodeStateVolatile, "dir", root)
	}

	lockPath := filepath.Join(root, "lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		return nil, fmt.Errorf("%w: another reclaimd holds %s", ErrLockHeld, lockPath)
	}

	s := &Store{root: root, logger: logger, lock: lf, written: map[string]int64{}}
	s.eventID = s.highestEventID()
	return s, nil
}

func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	if e := s.lock.Close(); err == nil {
		err = e
	}
	s.lock = nil
	return err
}

func (s *Store) diskDir(key string) string { return filepath.Join(s.root, "disks", key) }

func (s *Store) account(key string, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.written[key] += int64(n)
	s.mu.Unlock()
}

// TakeBytesWritten returns and clears the accumulated byte count, so the caller
// can fold it into the durable per-disk total exactly once.
func (s *Store) TakeBytesWritten(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.written[key]
	s.written[key] = 0
	return n
}

func (s *Store) ensureDisk(key string) (string, error) {
	d := s.diskDir(key)
	if err := os.MkdirAll(filepath.Join(d, "history"), 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// validDiskKey says whether a key is shaped like one this package produced.
//
// Keys reach the store from HTTP path values. A wildcard matches a single path
// segment, but an escaped slash inside it survives routing and unescapes
// afterwards, so what arrives is text from the network on its way to a
// filesystem path, and DeleteDisk turns that path into an rm -rf. Everything we
// generate comes out of sanitizeKey plus the ':' separating vendor from
// product, and never starts with a dot, which rules out ".." without a special
// case for it.
func validDiskKey(key string) bool {
	if key == "" || len(key) > 255 || strings.HasPrefix(key, ".") {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '.', r == '-', r == ':':
		default:
			return false
		}
	}
	return true
}

// DeleteDisk removes everything stored under one key: identity, schedule,
// progress, deferred bits, rounds, events, freshness and the whole latency
// history. It is the only code in the program that deletes a disk's state, and
// there is no undo.
func (s *Store) DeleteDisk(key string) error {
	if !validDiskKey(key) {
		return ErrNotFound
	}
	dir := s.diskDir(key)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return ErrNotFound
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}

	// The wear tally counts bytes written to a disk that still has a record.
	// Left behind, it would be folded into whatever next lands on this key.
	s.mu.Lock()
	delete(s.written, key)
	s.mu.Unlock()

	// Same reason writeAtomic fsyncs a parent directory: on a router the power
	// goes without warning, and an unsynced removal can come back at the next
	// boot as a directory with half its contents.
	if d, err := os.Open(filepath.Join(s.root, "disks")); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// writeAtomic writes via a temp file in the same directory, fsyncs it, renames
// it, and then fsyncs the parent directory.
//
// The last step is the one that usually gets left out, and it matters. rename
// is atomic with respect to concurrent readers, but the directory entry is not
// durable until the directory itself hits the medium. Skipping the sync is the
// usual way to find a zero-length state file after a power cut, and on a
// router, losing power without warning is the normal case.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) writeJSONAccounted(key, path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	s.account(key, len(b)+1)
	return writeAtomic(path, append(b, '\n'), 0o600)
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'), 0o600)
}

// readJSON returns ErrNotFound for a missing file so callers can treat "never
// written" as a normal first-run condition.
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrStateCorrupt, path, err)
	}
	return nil
}

func checkSchema(path string, got int) error {
	if got > stateSchema {
		return fmt.Errorf("%w: %s is schema %d, this build understands %d",
			ErrStateUnsupported, path, got, stateSchema)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Meta
// ---------------------------------------------------------------------------

func (s *Store) LoadMeta(key string) (Meta, error) {
	var m Meta
	p := filepath.Join(s.diskDir(key), "meta.json")
	if err := readJSON(p, &m); err != nil {
		return m, err
	}
	return m, checkSchema(p, m.Schema)
}

func (s *Store) SaveMeta(key string, m Meta) error {
	if _, err := s.ensureDisk(key); err != nil {
		return err
	}
	m.Schema = stateSchema
	return s.writeJSONAccounted(key, filepath.Join(s.diskDir(key), "meta.json"), m)
}

// ---------------------------------------------------------------------------
// Schedule and progress
// ---------------------------------------------------------------------------

func (s *Store) LoadSchedule(key string) (Schedule, error) {
	var sc Schedule
	p := filepath.Join(s.diskDir(key), "schedule.json")
	if err := readJSON(p, &sc); err != nil {
		return sc, err
	}
	return sc, checkSchema(p, sc.Schema)
}

func (s *Store) SaveSchedule(key string, sc Schedule) error {
	if _, err := s.ensureDisk(key); err != nil {
		return err
	}
	sc.Schema = stateSchema
	return s.writeJSONAccounted(key, filepath.Join(s.diskDir(key), "schedule.json"), sc)
}

func (s *Store) LoadProgress(key string) (Progress, error) {
	var p Progress
	path := filepath.Join(s.diskDir(key), "progress.json")
	if err := readJSON(path, &p); err != nil {
		return p, err
	}
	return p, checkSchema(path, p.Schema)
}

// SaveProgress is the safety-critical write in the program. On the dropout path
// it runs before logging and before anything else, so that losing power one
// second later cannot lose the suppression window that keeps us from walking
// straight back into the fault.
func (s *Store) SaveProgress(key string, p Progress) error {
	if _, err := s.ensureDisk(key); err != nil {
		return err
	}
	p.Schema = stateSchema
	p.UpdatedAt = time.Now()
	return s.writeJSONAccounted(key, filepath.Join(s.diskDir(key), "progress.json"), p)
}

// ---------------------------------------------------------------------------
// Deferred segment bitmap
// ---------------------------------------------------------------------------

// SaveDeferred persists the set of segments this round did not read. It is a
// bitmap because the debt is per-segment and unbounded: 1912 bits is 239 bytes
// for a 64 GB drive, so carrying the whole thing forward costs nothing.
func (s *Store) SaveDeferred(key string, bits []byte) error {
	if _, err := s.ensureDisk(key); err != nil {
		return err
	}
	s.account(key, len(bits))
	return writeAtomic(filepath.Join(s.diskDir(key), "deferred.bits"), bits, 0o600)
}

func (s *Store) LoadDeferred(key string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(s.diskDir(key), "deferred.bits"))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	return b, err
}

// ---------------------------------------------------------------------------
// Rounds and events (append-only jsonl)
// ---------------------------------------------------------------------------

// appendLine does a single O_APPEND write of one line. It is not fsynced:
// losing the tail of a log after a power cut is survivable, and fsyncing every
// event would multiply this tool's own write load on the disk it is protecting.
func appendLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (s *Store) AppendRound(key string, r RoundSummary) error {
	if _, err := s.ensureDisk(key); err != nil {
		return err
	}
	s.account(key, 220) // one summary line, near enough for a wear estimate
	return appendLine(filepath.Join(s.diskDir(key), "rounds.jsonl"), r)
}

func (s *Store) ListRounds(key string, limit int) ([]RoundSummary, error) {
	f, err := os.Open(filepath.Join(s.diskDir(key), "rounds.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []RoundSummary
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var r RoundSummary
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue // a torn last line after a power cut must not kill the API
		}
		out = append(out, r)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, sc.Err()
}

func (s *Store) NextEventID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventID++
	return s.eventID
}

func (s *Store) AppendEvent(key string, e Event) error {
	if _, err := s.ensureDisk(key); err != nil {
		return err
	}
	if e.ID == 0 {
		e.ID = s.NextEventID()
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	s.account(key, 200)
	return appendLine(filepath.Join(s.diskDir(key), "events.jsonl"), e)
}

func (s *Store) ListEvents(key string, limit int, beforeID uint64) ([]Event, error) {
	f, err := os.Open(filepath.Join(s.diskDir(key), "events.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if beforeID > 0 && e.ID >= beforeID {
			continue
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, sc.Err()
}

// highestEventID recovers the counter after a restart so that event ids stay
// monotonic across the daemon's lifetime. The UI pairs healed events back to
// the defer that caused them by id, and reused ids would corrupt those links.
func (s *Store) highestEventID() uint64 {
	var maxID uint64
	dirs, err := os.ReadDir(filepath.Join(s.root, "disks"))
	if err != nil {
		return 0
	}
	for _, d := range dirs {
		events, err := s.ListEvents(d.Name(), 0, 0)
		if err != nil {
			continue
		}
		for _, e := range events {
			if e.ID > maxID {
				maxID = e.ID
			}
		}
	}
	return maxID
}

// ---------------------------------------------------------------------------
// Latency profiles
// ---------------------------------------------------------------------------

// SaveProfile writes the full-resolution map plus its coarse companion, then
// prunes. Both are written once, at the end of a round: writing incrementally
// during a scan would multiply this tool's wear on the disk it is protecting.
func (s *Store) SaveProfile(key string, m *LatencyMap, blocksPerSegment, keepFull, keepCoarse int) error {
	dir, err := s.ensureDisk(key)
	if err != nil {
		return err
	}
	full, err := m.MarshalBinary()
	if err != nil {
		return err
	}
	s.account(key, len(full)*2) // history copy plus latest.lat
	name := fmt.Sprintf("%06d.lat", m.RoundSeq)
	if err := writeAtomic(filepath.Join(dir, "history", name), full, 0o600); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "latest.lat"), full, 0o600); err != nil {
		return err
	}

	coarse := m.Coarse(blocksPerSegment)
	cm := &LatencyMap{
		BlockSize:  m.BlockSize * blocksPerSegment,
		BlockCount: uint32(len(coarse)),
		RoundSeq:   m.RoundSeq,
		StartedAt:  m.StartedAt,
		Baseline:   m.Baseline,
		Values:     coarse,
	}
	cb, err := cm.MarshalBinary()
	if err != nil {
		return err
	}
	s.account(key, len(cb))
	cname := fmt.Sprintf("%06d.lat8", m.RoundSeq)
	if err := writeAtomic(filepath.Join(dir, "history", cname), cb, 0o600); err != nil {
		return err
	}

	s.prune(filepath.Join(dir, "history"), ".lat", keepFull)
	s.prune(filepath.Join(dir, "history"), ".lat8", keepCoarse)
	return nil
}

func (s *Store) LoadProfile(key string, seq uint64, coarse bool) (*LatencyMap, error) {
	dir := s.diskDir(key)
	var path string
	switch {
	case seq == 0 && !coarse:
		path = filepath.Join(dir, "latest.lat")
	case coarse:
		path = filepath.Join(dir, "history", fmt.Sprintf("%06d.lat8", seq))
	default:
		path = filepath.Join(dir, "history", fmt.Sprintf("%06d.lat", seq))
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m := &LatencyMap{}
	if err := m.UnmarshalBinary(b); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Store) prune(dir, suffix string, keep int) {
	if keep <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		// ".lat" is a suffix of ".lat8", so an exact extension match is needed
		// to keep the two retention tiers from pruning each other.
		if filepath.Ext(n) == suffix {
			names = append(names, n)
		}
	}
	if len(names) <= keep {
		return
	}
	sort.Slice(names, func(i, j int) bool { return seqOf(names[i]) < seqOf(names[j]) })
	for _, n := range names[:len(names)-keep] {
		_ = os.Remove(filepath.Join(dir, n))
	}
}

func seqOf(name string) uint64 {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	v, _ := strconv.ParseUint(base, 10, 64)
	return v
}

// SaveFreshness records, per segment, when this tool last read it successfully.
//
// It is one uint32 of Unix seconds per segment: 1912 segments for the reference
// stick, so 7.5 KiB rewritten once per pass. Segments the pass skipped keep
// their old timestamp, which is the point: the map is meant to show what has
// not been refreshed lately.
func (s *Store) SaveFreshness(key string, ages []uint32) error {
	if _, err := s.ensureDisk(key); err != nil {
		return err
	}
	b := make([]byte, len(ages)*4)
	for i, v := range ages {
		binary.LittleEndian.PutUint32(b[i*4:], v)
	}
	s.account(key, len(b))
	return writeAtomic(filepath.Join(s.diskDir(key), "freshness.bin"), b, 0o600)
}

func (s *Store) LoadFreshness(key string) ([]uint32, error) {
	b, err := os.ReadFile(filepath.Join(s.diskDir(key), "freshness.bin"))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return out, nil
}

// FreezeMarker says which filesystems the daemon is about to freeze, or was
// freezing when it last died.
//
// A freeze outlives the process that made it. If the daemon is killed between
// FIFREEZE and FITHAW, every write to that filesystem on the machine waits
// forever, and on a router that filesystem is the root overlay. So the marker
// is written and fsynced before the freeze and removed after the thaw, and the
// next start thaws whatever it names. It is written before rather than after
// because after the freeze nothing can be written to the disk the marker most
// likely lives on.
type FreezeMarker struct {
	Schema int       `json:"schema"`
	Disk   string    `json:"disk"`
	Mounts []string  `json:"mounts"`
	At     time.Time `json:"at"`
}

func (s *Store) freezeMarkerPath() string { return filepath.Join(s.root, "frozen.json") }

func (s *Store) SaveFreezeMarker(m FreezeMarker) error {
	m.Schema = stateSchema
	if m.At.IsZero() {
		m.At = time.Now()
	}
	return writeJSONAtomic(s.freezeMarkerPath(), m)
}

func (s *Store) LoadFreezeMarker() (FreezeMarker, error) {
	var m FreezeMarker
	if err := readJSON(s.freezeMarkerPath(), &m); err != nil {
		return m, err
	}
	return m, checkSchema(s.freezeMarkerPath(), m.Schema)
}

// ClearFreezeMarker removes the marker and syncs the directory, so that a
// power cut right after a clean thaw cannot bring the marker back and have
// the next boot thaw a filesystem nobody froze.
func (s *Store) ClearFreezeMarker() error {
	if err := os.Remove(s.freezeMarkerPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	d, err := os.Open(s.root)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ListDisks returns every key the store knows about, including ones not
// currently plugged in, since their history is still worth showing.
func (s *Store) ListDisks() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "disks"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
