package reclaimd

import (
	"bufio"
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

// tmpfsMagic identifies a volatile filesystem. See OpenStore for why this
// matters more than it looks.
const tmpfsMagic = 0x01021994

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
// from Schedule so that the write on the dropout path stays under 200 bytes:
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

// Store owns the state directory. One instance per daemon; the flock makes that
// a guarantee rather than a convention.
type Store struct {
	root   string
	logger *slog.Logger
	lock   *os.File

	mu      sync.Mutex
	eventID uint64
}

// OpenStore prepares the state directory and takes the process lock.
//
// The tmpfs check is not paranoia. On OpenWrt /var is a symlink to /tmp, so the
// compiled-in default of /var/lib/reclaimd lands on a tmpfs that evaporates at
// every reboot. The schedule and, far worse, the 24-hour dropout suppression
// would silently reset forever, and the only symptom would be a daemon that
// never quite seems to do its job.
func OpenStore(root string, logger *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "disks"), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", root, err)
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err == nil && st.Type == tmpfsMagic {
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

	s := &Store{root: root, logger: logger, lock: lf}
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

func (s *Store) ensureDisk(key string) (string, error) {
	d := s.diskDir(key)
	if err := os.MkdirAll(filepath.Join(d, "history"), 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// writeAtomic writes via a temp file in the same directory, fsyncs it, renames
// it, and then fsyncs the PARENT DIRECTORY.
//
// That last step is the one everybody omits and the one that matters. rename is
// atomic with respect to concurrent readers, but the directory entry is not
// durable until the directory itself hits the medium. Skipping it is the
// standard way to find a zero-length state file after a power cut -- and on a
// router, losing power without warning is the normal case, not the exception.
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

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'), 0o600)
}

// readJSON returns ErrNotFound for a missing file so callers can treat "never
// written" as a normal first-run condition rather than an error.
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
	return writeJSONAtomic(filepath.Join(s.diskDir(key), "meta.json"), m)
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
	return writeJSONAtomic(filepath.Join(s.diskDir(key), "schedule.json"), sc)
}

func (s *Store) LoadProgress(key string) (Progress, error) {
	var p Progress
	path := filepath.Join(s.diskDir(key), "progress.json")
	if err := readJSON(path, &p); err != nil {
		return p, err
	}
	return p, checkSchema(path, p.Schema)
}

// SaveProgress is the safety-critical write of the whole program. On the
// dropout path it runs BEFORE logging and before anything else, so that losing
// power one second later cannot lose the suppression window that keeps us from
// walking straight back into the fault.
func (s *Store) SaveProgress(key string, p Progress) error {
	if _, err := s.ensureDisk(key); err != nil {
		return err
	}
	p.Schema = stateSchema
	p.UpdatedAt = time.Now()
	return writeJSONAtomic(filepath.Join(s.diskDir(key), "progress.json"), p)
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
// monotonic across the daemon's lifetime -- the UI pairs healed events back to
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
// during a scan would multiply this tool's wear on the very disk it protects.
func (s *Store) SaveProfile(key string, m *LatencyMap, blocksPerSegment, keepFull, keepCoarse int) error {
	dir, err := s.ensureDisk(key)
	if err != nil {
		return err
	}
	full, err := m.MarshalBinary()
	if err != nil {
		return err
	}
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

// ListDisks returns every key the store knows about, including ones not
// currently plugged in -- their history is still worth showing.
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
