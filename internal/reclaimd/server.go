package reclaimd

import (
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// progressHz is how often live frames reach the browser. The scanner produces
// far more than this. Coalescing to a fixed rate is what keeps a router from
// spending its CPU on a status page nobody is looking at.
const progressHz = 2

type Server struct {
	cfg    Config
	store  *Store
	sup    *Supervisor
	logger *slog.Logger
	hub    *Hub
	start  time.Time

	// Build is what the binary was built from, for the page to show. The
	// command fills it in after NewServer.
	Build BuildInfo

	mu sync.Mutex
	// pending holds the frames that arrived since the last tick, and only
	// those. An entry left behind here goes out again on every later tick that
	// any other disk makes busy, which is how a round that ended half an hour
	// ago keeps arriving at the browser.
	pending map[string]LiveProgress
}

func NewServer(cfg Config, store *Store, sup *Supervisor, logger *slog.Logger) *Server {
	s := &Server{
		cfg: cfg, store: store, sup: sup, logger: logger,
		hub: NewHub(), start: time.Now(), pending: map[string]LiveProgress{},
	}
	sup.OnLive = s.onLive
	sup.OnChange = func(key, change string) {
		if change == "SCAN_END" {
			// A frame queued in the last half-second would otherwise land
			// after this event and put the page back into a round that has
			// already finished, showing numbers that never move again.
			s.dropPending(key)
		}
		s.hub.Publish("state", map[string]any{
			"disk": key, "change": change,
			"refetch": []string{"disk", "rounds", "events"},
		})
	}
	return s
}

func (s *Server) onLive(p LiveProgress) {
	s.mu.Lock()
	s.pending[p.Disk] = p
	s.mu.Unlock()
}

// drainPending takes the frames that arrived since the last call, leaving the
// map empty. Emptying it is the point. See the field's comment for why.
func (s *Server) drainPending() []LiveProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	out := make([]LiveProgress, 0, len(s.pending))
	for key, p := range s.pending {
		out = append(out, p)
		delete(s.pending, key)
	}
	return out
}

// dropPending forgets a disk's queued frame, for when its round has ended.
func (s *Server) dropPending(key string) {
	s.mu.Lock()
	delete(s.pending, key)
	s.mu.Unlock()
}

// RunPublisher is the single shared ticker that turns a firehose of scanner
// callbacks into at most progressHz frames per second, and sends nothing when
// nothing changed.
func (s *Server) RunPublisher(stop <-chan struct{}) {
	t := time.NewTicker(time.Second / progressHz)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			for _, p := range s.drainPending() {
				s.hub.Publish("progress", p)
			}
		}
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/system", s.handleSystem)
	mux.HandleFunc("GET /api/v1/disks", s.handleDisks)
	mux.HandleFunc("GET /api/v1/disks/{key}", s.handleDisk)
	mux.HandleFunc("DELETE /api/v1/disks/{key}", s.handleForget)
	mux.HandleFunc("GET /api/v1/disks/{key}/rounds", s.handleRounds)
	mux.HandleFunc("GET /api/v1/disks/{key}/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/disks/{key}/profile", s.handleProfile)
	mux.HandleFunc("GET /api/v1/disks/{key}/freshness", s.handleFreshness)
	mux.HandleFunc("POST /api/v1/disks/{key}/enabled", s.handleEnabled)
	mux.HandleFunc("POST /api/v1/disks/{key}/scan", s.handleScan)
	mux.HandleFunc("POST /api/v1/disks/{key}/stop", s.handleStop)
	mux.HandleFunc("GET /api/v1/disks/{key}/speed", s.handleSpeed)
	mux.HandleFunc("POST /api/v1/disks/{key}/speed", s.handleSpeedTest)
	mux.HandleFunc("GET /api/v1/stream", s.handleStream)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		s.logger.Error("embedded web assets missing", "error", err)
	} else {
		mux.Handle("GET /", http.FileServerFS(sub))
	}
	return s.withGuards(mux)
}

// withGuards applies the two protections that matter for a status page that may
// be exposed on a LAN: a bearer token when one is configured, and a same-origin
// requirement on anything that mutates.
func (s *Server) withGuards(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.UIToken != "" {
			tok := r.Header.Get("Authorization")
			tok = strings.TrimPrefix(tok, "Bearer ")
			if tok == "" {
				tok = r.URL.Query().Get("token")
			}
			if tok != s.cfg.UIToken {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "token required")
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// No cookies are used, so there is no ambient authority to steal. A
			// cross-site write is still refused outright, so that nothing
			// depends on that reasoning holding forever.
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" &&
				site != "same-origin" && site != "none" {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "cross-site request")
				return
			}
		}
		if r.Method == http.MethodPost {
			// A JSON content type is what a cross-origin form cannot produce.
			// DELETE carries no body and is unreachable from a form at all, so
			// the check belongs to POST alone.
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, "VALIDATION_ERROR",
					"expected application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": msg}})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"uptime_s":  round2(time.Since(s.start).Seconds()),
		"heartbeat": s.sup.Heartbeat().Unix(),
	})
}

func (s *Server) handleDisks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"disks": s.views(false)})
}

// diskKey pulls the {key} wildcard out of the path and refuses anything not
// shaped like a key this program produces. A wildcard matches one path segment,
// but an escaped slash inside it survives routing and unescapes afterwards, so
// what arrives here is text from the network that ends up in a filesystem path.
func diskKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.PathValue("key")
	if !validDiskKey(key) {
		writeError(w, http.StatusNotFound, CodeDeviceNotFound, "no such disk")
		return "", false
	}
	return key, true
}

func (s *Server) handleDisk(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	for _, v := range s.views(true) {
		if v.Key == key {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
	writeError(w, http.StatusNotFound, CodeDeviceNotFound, "no such disk")
}

// views assembles the per-disk payload. Detail adds the controller rows and the
// round history, which the fleet list does not need.
func (s *Server) views(detail bool) []DiskView {
	s.sup.mu.RLock()
	states := make([]*diskState, 0, len(s.sup.disks))
	for _, st := range s.sup.disks {
		states = append(states, st)
	}
	s.sup.mu.RUnlock()

	out := make([]DiskView, 0, len(states))
	for _, st := range states {
		s.sup.mu.RLock()
		v := DiskView{
			Key:      st.Key,
			Identity: st.Meta.Identity,
			Present:  st.Present,
			Enabled:  st.Meta.Enabled,
			Adopted:  !st.Meta.AdoptedAt.IsZero(),
			Scanning: st.Scanning,
			LastErr:  st.LastErr,
		}
		if st.Present {
			v.KernelName = st.Presence.KernelName
			if v.Identity.Key == "" {
				v.Identity = st.Presence.Identity
			}
		}
		if !st.Meta.AdoptedAt.IsZero() {
			v.AdoptedAtTs = st.Meta.AdoptedAt.Unix()
		}
		if !st.Schedule.NextScanAt.IsZero() {
			v.NextScanTs = st.Schedule.NextScanAt.Unix()
		}
		if !st.Schedule.LastRoundAt.IsZero() {
			v.LastScanTs = st.Schedule.LastRoundAt.Unix()
		}
		if !st.Schedule.SuppressUntil.IsZero() {
			v.SuppressTs = st.Schedule.SuppressUntil.Unix()
		}
		v.LastOutcome = st.Schedule.LastOutcome
		v.ScanRequested = st.ScanRequested
		v.Stopping = st.StopRequested
		v.SpeedTesting = st.Testing
		v.BytesWritten = st.Meta.BytesWritten
		if st.Live != nil {
			live := *st.Live
			v.Live = &live
		}
		sched := st.Schedule
		s.sup.mu.RUnlock()

		rounds, _ := s.store.ListRounds(st.Key, 60)
		v.Health = assessHealth(rounds, s.cfg)
		if ages, err := s.store.LoadFreshness(st.Key); err == nil && len(ages) > 0 {
			oldest := uint32(0)
			now := uint32(time.Now().Unix())
			first := true
			for _, a := range ages {
				if a == 0 {
					continue // never read, counted separately by the UI
				}
				if first || a < oldest {
					oldest, first = a, false
				}
			}
			if !first {
				v.OldestDataS = float64(now - oldest)
			}
		}
		v.IntervalS = sched.Interval.Duration().Seconds()
		for _, r := range rounds {
			v.TotalHealed += r.Healed
		}
		if detail {
			v.Controllers = controllersFor(sched, rounds, s.cfg, v.Identity, v.Live)
			v.Rounds = rounds
			v.SpeedRegionsN = s.cfg.SpeedRegions
			v.SpeedRegionMiB = s.cfg.SpeedRegionMiB
			v.SpeedBudgetS = s.cfg.SpeedBudget.Duration().Seconds()
		}
		out = append(out, v)
	}

	// Map iteration order is not stable. Without this the fleet list arrives
	// shuffled on every poll: the cards swap places under the pointer, and the
	// disk the page selects on load is whichever one the runtime felt like
	// yielding first. Absent disks sink to the bottom, which is the only
	// reordering a reader should ever see and one they caused by pulling the
	// stick.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Present != out[j].Present {
			return out[i].Present
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func (s *Server) handleRounds(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	limit := intParam(r, "limit", 200)
	rounds, err := s.store.ListRounds(key, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rounds": rounds})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	limit := intParam(r, "limit", 200)
	before := uint64(intParam(r, "before_id", 0))
	events, err := s.store.ListEvents(key, limit, before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// handleProfile serves a latency map as raw little-endian uint16.
//
// Completed rounds never change, so they are served immutable and a browser
// fetches each one exactly once for the lifetime of the page. That is what
// makes the stacked multi-pass view affordable on a router: twelve passes cost
// twelve requests in total, however many times the view is redrawn.
func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	coarse := r.URL.Query().Get("res") == "coarse"
	var seq uint64
	if v := r.URL.Query().Get("round"); v != "" && v != "latest" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "bad round")
			return
		}
		seq = n
	}
	m, err := s.store.LoadProfile(key, seq, coarse)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeDeviceNotFound, "no profile")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	b, err := m.MarshalBinary()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if seq > 0 {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// handleFreshness serves one uint32 of Unix seconds per segment.
//
// Unlike a completed pass this changes every round, so it is never cached.
func (s *Server) handleFreshness(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	ages, err := s.store.LoadFreshness(key)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeDeviceNotFound, "no freshness data")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	b := make([]byte, len(ages)*4)
	for i, v := range ages {
		binary.LittleEndian.PutUint32(b[i*4:], v)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) handleEnabled(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if err := s.sup.SetEnabled(key, body.Enabled); err != nil {
		writeError(w, http.StatusNotFound, CodeDeviceNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": body.Enabled})
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	// Named after the CLI flag. Something tidier like "force" would lose the
	// awkwardness, which is the point in both places: this clears a window that
	// the last round opened because the disk misbehaved.
	var body struct {
		IMeanIt bool `json:"i_mean_it"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil &&
		!errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	err := s.sup.RequestScan(key, body.IMeanIt)
	switch {
	case errors.Is(err, ErrScanInProgress):
		writeError(w, http.StatusConflict, CodeScanInProgress,
			"a round is already running on this disk")
	case errors.Is(err, ErrSpeedTestRunning):
		writeError(w, http.StatusConflict, CodeSpeedTestRunning,
			"a speed test is running on this disk")
	case errors.Is(err, ErrScanSuppressed):
		// This refusal is deliberate. The suppression window exists because the
		// disk just took a filesystem down with it, and impatience is no reason
		// to go back in early.
		writeError(w, http.StatusConflict, CodeScanSuppressed,
			"disk is in the cooldown window the last round opened")
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, CodeDeviceNotFound, "no such disk")
	case err != nil:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleStop ends the round running on a disk. It answers as soon as the round
// has been told. The round itself ends at the next segment boundary or rest,
// and the page hears about that from the SCAN_END that follows.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	err := s.sup.StopScan(key)
	switch {
	case errors.Is(err, ErrNotScanning):
		writeError(w, http.StatusConflict, CodeNotScanning, "no round is running on this disk")
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, CodeDeviceNotFound, "no such disk")
	case err != nil:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleForget deletes one disk's stored state. The page asks the operator
// first. The daemon does not ask again, but it does refuse mid-round.
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	err := s.sup.Forget(key)
	switch {
	case errors.Is(err, ErrScanInProgress):
		writeError(w, http.StatusConflict, CodeScanInProgress,
			"a round is running on this disk")
	case errors.Is(err, ErrSpeedTestRunning):
		writeError(w, http.StatusConflict, CodeSpeedTestRunning,
			"a speed test is running on this disk")
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, CodeDeviceNotFound, "no such disk")
	case err != nil:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSpeed serves the disk's latest speed test.
func (s *Server) handleSpeed(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	res, err := s.store.LoadSpeed(key)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeDeviceNotFound, "no speed test yet")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleSpeedTest starts a speed test. It answers once the test is under
// way, and the page hears SPEED_END when the result is there to fetch.
func (s *Server) handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	key, ok := diskKey(w, r)
	if !ok {
		return
	}
	var body struct {
		IMeanIt bool `json:"i_mean_it"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil &&
		!errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	err := s.sup.RequestSpeedTest(key, body.IMeanIt)
	switch {
	case errors.Is(err, ErrScanInProgress):
		writeError(w, http.StatusConflict, CodeScanInProgress,
			"a round is running on this disk")
	case errors.Is(err, ErrSpeedTestRunning):
		writeError(w, http.StatusConflict, CodeSpeedTestRunning,
			"a speed test is already running on this disk")
	case errors.Is(err, ErrScanSuppressed):
		writeError(w, http.StatusConflict, CodeScanSuppressed,
			"disk is in the cooldown window the last round opened")
	case errors.Is(err, ErrNotPresent):
		writeError(w, http.StatusConflict, CodeDeviceNotReady, "disk is not plugged in")
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, CodeDeviceNotFound, "no such disk")
	case err != nil:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
	}
}

// CloseStreams ends every open event stream. Call it before shutting the HTTP
// server down. Hub.Close says why the order matters.
func (s *Server) CloseStreams() { s.hub.Close() }

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	s.hub.ServeSSE(w, r, map[string]any{
		"v":         1,
		"server_ts": time.Now().Unix(),
		"disks":     s.views(false),
	})
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
