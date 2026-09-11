package reclaimd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// discoveryInterval is how often discovery runs. It is slow on purpose.
// Discovery never touches the bus, but there is no reason to spin either. A
// stick that just appeared can wait 30 seconds to be noticed, and it has 30
// minutes of probation ahead of it regardless. A round somebody asked for does
// not wait for it. See Supervisor.wake.
const discoveryInterval = 30 * time.Second

// diskState is the supervisor's per-disk view. Anything durable lives in the
// store. This is the part that is allowed to be forgotten on restart.
type diskState struct {
	Key      string
	Presence Presence
	Meta     Meta
	Schedule Schedule

	// PresentSince is not persisted, on purpose. After a daemon restart we
	// cannot know whether the stick stayed plugged in, and assuming it did
	// would let a stick that was swapped during the downtime skip probation.
	PresentSince time.Time
	Present      bool

	Scanning bool
	Live     *LiveProgress
	LastErr  string

	// stop cancels the running round, and is set for exactly as long as
	// Scanning is. StopRequested says it has been called: the round ends at its
	// next segment boundary or rest, and until then the page shows it winding
	// down and does not offer Stop a second time.
	stop          context.CancelFunc
	StopRequested bool

	// ScanRequested is somebody pressing Scan now. It carries the round past
	// the waits that exist for scans nobody asked for (probation and the grace
	// after boot), and is spent when the round starts or the disk goes.
	ScanRequested bool
}

// Supervisor owns discovery, adoption and scheduling.
type Supervisor struct {
	cfg      Config
	store    *Store
	logger   *slog.Logger
	platform Platform
	scanner  *Scanner

	mu    sync.RWMutex
	disks map[string]*diskState

	// wake runs a tick out of turn, for Scan now. Whoever pressed it is watching
	// the page, and leaving the round to the next discovery tick had them wait
	// up to discoveryInterval for it to begin. It runs a whole tick. considerDisk
	// alone would not do, because a round opens the device from the presence
	// discovery found, and that is only as fresh as the last discovery. One slot
	// is enough: a tick considers every disk, so a request that finds the slot
	// taken is served by the tick already queued.
	wake chan struct{}

	// OnLive is called with every progress snapshot. The HTTP layer coalesces
	// these. The supervisor just forwards them.
	OnLive func(LiveProgress)
	// OnChange is called when a disk's lifecycle state changes, so the UI can
	// refetch. That saves inventing a full state push for it.
	OnChange func(key, change string)

	heartbeat atomic64
}

func NewSupervisor(cfg Config, store *Store, logger *slog.Logger, pl Platform) *Supervisor {
	return &Supervisor{
		cfg:      cfg,
		store:    store,
		logger:   logger,
		platform: pl,
		scanner:  NewScanner(cfg, store, logger, pl),
		disks:    map[string]*diskState{},
		wake:     make(chan struct{}, 1),
	}
}

// Run drives discovery and scheduling until the context is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	s.loadKnownDisks()

	t := time.NewTicker(discoveryInterval)
	defer t.Stop()

	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.tick(ctx)
		case <-s.wake:
			s.tick(ctx)
		}
	}
}

// loadKnownDisks brings history back for disks that are not plugged in right
// now. Their pages should still render: "this stick is absent" is information.
func (s *Supervisor) loadKnownDisks() {
	keys, err := s.store.ListDisks()
	if err != nil {
		return
	}
	for _, k := range keys {
		meta, err := s.store.LoadMeta(k)
		if err != nil {
			continue
		}
		sched, err := s.store.LoadSchedule(k)
		if errors.Is(err, ErrNotFound) {
			sched = NewSchedule(s.cfg, time.Now())
		} else if err != nil {
			s.logger.Error("load schedule", "disk", k, "error", err)
			continue
		}
		s.disks[k] = &diskState{Key: k, Meta: meta, Schedule: sched}
	}
}

func (s *Supervisor) tick(ctx context.Context) {
	s.heartbeat.Store(time.Now().Unix())

	found, err := s.platform.Discover()
	if err != nil {
		s.logger.Error("discovery failed", "error", err)
		return
	}

	now := time.Now()
	seen := map[string]bool{}

	s.mu.Lock()
	for _, p := range found {
		if p.Ignored {
			continue
		}
		key := p.Identity.Key
		seen[key] = true
		st, ok := s.disks[key]
		if !ok {
			st = &diskState{Key: key}
			s.disks[key] = st
			s.logger.Info("new usb disk", "disk", key,
				"model", p.Identity.Model, "size_bytes", p.Identity.SizeBytes,
				"key_stable", p.Identity.KeyIsStable)
		}
		if !st.Present {
			st.PresentSince = now
			st.Present = true
		}
		st.Presence = p
		if st.Meta.Identity.Key == "" {
			st.Meta = Meta{Identity: p.Identity, FirstSeen: now, Enabled: true}
			_ = s.store.SaveMeta(key, st.Meta)
		}
	}
	var swept []string
	for key, st := range s.disks {
		if seen[key] {
			continue
		}
		st.Present = false
		st.ScanRequested = false

		// A stick pulled before its probation ended leaves nothing worth
		// keeping: never adopted means never scanned, so all that is on disk is
		// the identity record discovery wrote the moment it appeared. Sweeping
		// it is what stops disks/ growing an entry for every stick that was ever
		// in a port for ten seconds, and, for the ones whose key is not stable,
		// one entry per port they were ever in.
		//
		// Being excluded is a decision, so it survives the sweep. Forgetting it
		// would mean a stick that comes back finds no record of having been
		// switched off, and gets adopted half an hour later by the machinery
		// its owner had already said no to.
		if st.Meta.AdoptedAt.IsZero() && st.Meta.Enabled && !st.Scanning {
			delete(s.disks, key)
			if err := s.store.DeleteDisk(key); err != nil && !errors.Is(err, ErrNotFound) {
				s.logger.Error("forget unadopted disk", "disk", key, "error", err)
			}
			s.logger.Info("unadopted disk went away; forgotten", "disk", key)
			swept = append(swept, key)
		}
	}
	candidates := make([]*diskState, 0, len(s.disks))
	for _, st := range s.disks {
		candidates = append(candidates, st)
	}
	s.mu.Unlock()

	for _, key := range swept {
		s.notifyChange(key, "FORGOTTEN")
	}
	for _, st := range candidates {
		s.considerDisk(ctx, st, now)
	}
}

// considerDisk decides whether this disk should be scanned right now.
func (s *Supervisor) considerDisk(ctx context.Context, st *diskState, now time.Time) {
	s.mu.Lock()
	if st.Scanning || !st.Present {
		s.mu.Unlock()
		return
	}
	if st.Meta.AdoptedAt.IsZero() {
		// Probation keeps a stick plugged in to copy one file out of the
		// schedule. Asking for a round says this one is staying, so a request
		// adopts it on the spot.
		if !st.ScanRequested && now.Sub(st.PresentSince) < s.cfg.AdoptAfter.Duration() {
			s.mu.Unlock()
			return
		}
		st.Meta.AdoptedAt = now
		st.Meta.Identity = st.Presence.Identity
		if err := s.store.SaveMeta(st.Key, st.Meta); err != nil {
			s.logger.Error("save meta", "disk", st.Key, "error", err)
		}
		st.Schedule = NewSchedule(s.cfg, now)
		_ = s.store.SaveSchedule(st.Key, st.Schedule)
		_ = s.store.AppendEvent(st.Key, Event{Type: EventAdopted, Params: map[string]any{
			"probation_s": s.cfg.AdoptAfter.Duration().Seconds(),
		}})
		s.logger.Info("disk adopted", "disk", st.Key, "requested", st.ScanRequested)
		s.notifyChange(st.Key, "ADOPTED")
	}
	if !st.Meta.Enabled {
		s.mu.Unlock()
		return
	}

	// A machine with no RTC boots with a clock that makes every absolute
	// timestamp meaningless. Scanning on the strength of one is how a router
	// ends up either scanning continuously or never at all.
	if now.Before(SanityEpoch) {
		s.mu.Unlock()
		s.logger.Warn("clock not yet synced; holding off",
			"disk", st.Key, "code", CodeClockUnsynced, "now", now)
		return
	}
	if sched, rebased := st.Schedule.RebaseIfClockUnsynced(now); rebased {
		st.Schedule = sched
		_ = s.store.SaveSchedule(st.Key, sched)
		s.logger.Warn("schedule rebased after clock correction",
			"disk", st.Key, "code", CodeClockUnsynced, "next", sched.NextScanAt)
	}
	if up, err := s.platform.Uptime(); err == nil && up < s.cfg.MinUptime.Duration() &&
		!st.ScanRequested {
		s.mu.Unlock()
		return
	}
	if due, _ := st.Schedule.Due(now); !due {
		s.mu.Unlock()
		return
	}

	st.Scanning = true
	st.ScanRequested = false
	// A context of the round's own, so Stop can end it without ending the
	// daemon. It is made here, before runRound, so that it exists from the
	// moment Scanning says a round does.
	roundCtx, stop := context.WithCancel(ctx)
	st.stop = stop
	in := RoundInput{
		Key:      st.Key,
		Presence: st.Presence,
		Schedule: st.Schedule,
	}
	s.mu.Unlock()

	go s.runRound(ctx, roundCtx, st, in)
}

// runRound opens the device, runs one pass and persists everything. Opening and
// warming up happen under the daemon's ctx and only the pass under roundCtx, so a
// Stop that lands before the pass begins is still taken up by the pass, which is
// what knows how to end early and what to leave behind.
func (s *Supervisor) runRound(ctx, roundCtx context.Context, st *diskState, in RoundInput) {
	defer func() {
		s.mu.Lock()
		st.Scanning = false
		st.StopRequested = false
		stop := st.stop
		st.stop = nil
		st.Live = nil
		s.mu.Unlock()
		if stop != nil {
			stop()
		}
		s.notifyChange(st.Key, "SCAN_END")
	}()
	// Announced as the disk turns busy, before the device is even opened. The
	// page learns that a round is running from this event, and it has no reason
	// to wait out the open and the warm-up reads to be told.
	s.notifyChange(st.Key, "SCAN_START")

	dev, err := OpenDevice(in.Presence, s.cfg.BlockSizeFor(in.Presence.Identity), s.platform)
	if err != nil {
		s.logger.Error("open device", "disk", st.Key, "error", err)
		s.setErr(st, err)
		return
	}
	defer dev.Close()

	suspended, err := dev.WarmUp(ctx, s.cfg.WarmupDiscard)
	if err != nil {
		s.logger.Error("warm up", "disk", st.Key, "error", err)
		s.setErr(st, err)
		return
	}
	if suspended {
		s.logger.Info("device was autosuspended before warm-up", "disk", st.Key)
	}

	if bits, err := s.store.LoadDeferred(st.Key); err == nil {
		in.Deferred = bits
	}
	in.Dev = dev
	in.Ext = NewExternalIOMonitor(s.cfg, s.platform, in.Presence)
	in.OnProgress = func(p LiveProgress) {
		s.mu.Lock()
		cp := p
		st.Live = &cp
		s.mu.Unlock()
		if s.OnLive != nil {
			s.OnLive(p)
		}
	}

	s.logger.Info("scan started", "disk", st.Key, "cursor", in.Schedule.Cursor)

	res, err := s.scanner.Round(roundCtx, in)
	if err != nil {
		s.logger.Error("scan round failed", "disk", st.Key, "error", err)
		s.setErr(st, err)
		return
	}

	s.recordRound(st, res)
}

// recordRound writes down what a round found. A cancelled round that read
// nothing (interrupted before its first block, or ended by an error on it)
// found nothing, and writing it down anyway put an empty pass at the head of
// the history: a blank latency map, a blank row in the waterfall, a last scan
// of just now, and a cursor back at zero, since a round that never got past its
// baseline never set one. All such a round does is move the schedule off now.
func (s *Supervisor) recordRound(st *diskState, res RoundResult) {
	if res.Summary.Outcome != OutcomeCancelled || res.Summary.BlocksRead > 0 {
		s.persistRound(st, res)
		return
	}
	s.mu.Lock()
	st.Schedule = st.Schedule.Postpone(time.Now())
	sched := st.Schedule
	s.mu.Unlock()
	if err := s.store.SaveSchedule(st.Key, sched); err != nil {
		s.logger.Error("save schedule", "disk", st.Key, "error", err)
	}
	s.logger.Info("scan ended before reading anything", "disk", st.Key,
		"next_scan_at", sched.NextScanAt)
}

func (s *Supervisor) persistRound(st *diskState, res RoundResult) {
	key := st.Key
	now := time.Now()

	if err := s.store.SaveProfile(key, res.Latency, res.blocksPerSegment(s.cfg),
		s.cfg.KeepFullProfiles, s.cfg.KeepCoarseProfiles); err != nil {
		s.logger.Error("save profile", "disk", key, "error", err)
	}
	if err := s.store.SaveDeferred(key, res.Deferred); err != nil {
		s.logger.Error("save deferred", "disk", key, "error", err)
	}
	if err := s.store.AppendRound(key, res.Summary); err != nil {
		s.logger.Error("append round", "disk", key, "error", err)
	}
	s.updateFreshness(key, res)

	s.mu.Lock()
	prev := st.Schedule.Interval
	sched := st.Schedule.Next(res.Summary, now, s.cfg)
	sched.Cursor = res.Cursor
	sched.RoundSeq = res.Summary.Seq
	if res.Baseline.RoundP50 > 0 {
		sched.LearnedBaseline = Duration(BlendLearned(
			sched.LearnedBaseline.Duration(), res.Baseline.RoundP50))
	}
	// A dropout has already written its own suppression window, synchronously,
	// before anything else. Do not let the scheduler's own copy overwrite it
	// with a value computed later.
	if prog, err := s.store.LoadProgress(key); err == nil &&
		prog.SuppressUntil.After(sched.SuppressUntil) {
		sched.SuppressUntil = prog.SuppressUntil
	}
	st.Schedule = sched
	s.mu.Unlock()

	if err := s.store.SaveSchedule(key, sched); err != nil {
		s.logger.Error("save schedule", "disk", key, "error", err)
	}
	if prev != sched.Interval {
		_ = s.store.AppendEvent(key, Event{
			Type: EventInterval, Round: res.Summary.Seq,
			Params: map[string]any{
				"prev_h":  prev.Duration().Hours(),
				"value_h": sched.Interval.Duration().Hours(),
				"code":    sched.LastReason,
			},
		})
	}
	_ = s.store.AppendEvent(key, Event{
		Type: EventRound, Round: res.Summary.Seq,
		Params: map[string]any{
			"outcome":   res.Summary.Outcome,
			"slow_n":    res.Summary.SlowBlocks,
			"danger_n":  res.Summary.DangerBlocks,
			"drop_n":    res.Summary.Dropouts,
			"healed_n":  res.Summary.Healed,
			"read_mib":  res.Summary.BytesRead >> 20,
			"elapsed_s": res.Summary.EndedAt.Sub(res.Summary.StartedAt).Seconds(),
		},
	})

	// Fold this pass's own writes into the durable total. The SaveMeta below is
	// itself a write and lands in the next pass's tally, which is a rounding
	// error against a number meant to be read as "a few megabytes a year".
	s.mu.Lock()
	st.Meta.BytesWritten += s.store.TakeBytesWritten(key)
	meta := st.Meta
	s.mu.Unlock()
	if err := s.store.SaveMeta(key, meta); err != nil {
		s.logger.Error("save meta", "disk", key, "error", err)
	}

	s.logger.Info("scan finished", "disk", key,
		"outcome", res.Summary.Outcome,
		"slow_n", res.Summary.SlowBlocks,
		"danger_n", res.Summary.DangerBlocks,
		"drop_n", res.Summary.Dropouts,
		"healed_n", res.Summary.Healed,
		"next_scan_at", sched.NextScanAt)
}

// updateFreshness stamps every segment this pass actually read.
//
// Segments that were skipped keep their previous timestamp on purpose: the map
// exists to show what has not been refreshed lately, and quietly resetting a
// deferred segment would hide the thing worth seeing.
//
// The result is a lower bound on freshness. Reads performed by whatever
// filesystem lives on the disk refresh data too, and they are invisible from
// down here at the raw device.
func (s *Supervisor) updateFreshness(key string, res RoundResult) {
	perSeg := res.blocksPerSegment(s.cfg)
	if perSeg <= 0 || res.Latency == nil {
		return
	}
	segs := (len(res.Latency.Values) + perSeg - 1) / perSeg
	ages, err := s.store.LoadFreshness(key)
	if err != nil || len(ages) != segs {
		ages = make([]uint32, segs)
	}
	now := uint32(time.Now().Unix())
	for seg := 0; seg < segs; seg++ {
		lo := seg * perSeg
		hi := lo + perSeg
		if hi > len(res.Latency.Values) {
			hi = len(res.Latency.Values)
		}
		for _, v := range res.Latency.Values[lo:hi] {
			if _, ok := DecodeLatency(v); ok {
				ages[seg] = now
				break
			}
		}
	}
	if err := s.store.SaveFreshness(key, ages); err != nil {
		s.logger.Error("save freshness", "disk", key, "error", err)
	}
}

// ScanOnce runs a single pass synchronously and returns its summary. It is what
// the scan command drives, and it obeys every rule the daemon obeys,
// suppression included. A manual run is impatience, and that is no reason to
// re-enter a window opened by the disk taking a filesystem down.
func (s *Supervisor) ScanOnce(ctx context.Context, key string, force bool) (RoundSummary, error) {
	found, err := s.platform.Discover()
	if err != nil {
		return RoundSummary{}, err
	}
	var p Presence
	ok := false
	for _, d := range found {
		if d.Identity.Key == key || d.KernelName == key || d.Node == key {
			if d.Ignored {
				return RoundSummary{}, fmt.Errorf("%w: %s", ErrNotFound, d.IgnoredReason)
			}
			p, ok = d, true
			break
		}
	}
	if !ok {
		return RoundSummary{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	key = p.Identity.Key

	meta, err := s.store.LoadMeta(key)
	if errors.Is(err, ErrNotFound) {
		meta = Meta{Identity: p.Identity, FirstSeen: time.Now(), Enabled: true, AdoptedAt: time.Now()}
		if err := s.store.SaveMeta(key, meta); err != nil {
			return RoundSummary{}, err
		}
	} else if err != nil {
		return RoundSummary{}, err
	}

	sched, err := s.store.LoadSchedule(key)
	if errors.Is(err, ErrNotFound) {
		sched = NewSchedule(s.cfg, time.Now())
	} else if err != nil {
		return RoundSummary{}, err
	}
	if !force && time.Now().Before(sched.SuppressUntil) {
		return RoundSummary{}, ErrScanSuppressed
	}

	st := &diskState{Key: key, Presence: p, Meta: meta, Schedule: sched, Present: true}
	s.mu.Lock()
	s.disks[key] = st
	s.mu.Unlock()

	dev, err := OpenDevice(p, s.cfg.BlockSizeFor(p.Identity), s.platform)
	if err != nil {
		return RoundSummary{}, err
	}
	defer dev.Close()
	suspended, err := dev.WarmUp(ctx, s.cfg.WarmupDiscard)
	if err != nil {
		return RoundSummary{}, err
	}
	if suspended {
		s.logger.Info("device was autosuspended before warm-up", "disk", key)
	}

	in := RoundInput{Key: key, Dev: dev, Presence: p, Schedule: sched,
		Ext: NewExternalIOMonitor(s.cfg, s.platform, p)}
	if bits, err := s.store.LoadDeferred(key); err == nil {
		in.Deferred = bits
	}
	in.OnProgress = func(pr LiveProgress) {
		s.logger.Info("scan progress", "disk", key, "pos_mib", pr.PosMiB,
			"total_mib", pr.TotalMiB, "mib_s", pr.SpeedMiBs, "slow_n", pr.SlowN,
			"danger_n", pr.DangerN, "drift_n", pr.DriftN)
	}

	res, err := s.scanner.Round(ctx, in)
	if err != nil {
		return res.Summary, err
	}
	s.recordRound(st, res)
	return res.Summary, nil
}

func (s *Supervisor) setErr(st *diskState, err error) {
	s.mu.Lock()
	st.LastErr = err.Error()
	s.mu.Unlock()
}

func (s *Supervisor) notifyChange(key, change string) {
	if s.OnChange != nil {
		s.OnChange(key, change)
	}
}

// SetEnabled is the per-disk switch the web UI drives.
func (s *Supervisor) SetEnabled(key string, enabled bool) error {
	s.mu.Lock()
	st, ok := s.disks[key]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	st.Meta.Enabled = enabled
	meta := st.Meta
	s.mu.Unlock()

	if err := s.store.SaveMeta(key, meta); err != nil {
		return err
	}
	s.notifyChange(key, "ENABLED_CHANGED")
	return nil
}

// Forget drops a disk from the fleet and deletes everything stored under its
// key. It refuses mid-round because persistRound would write the history
// straight back a moment later.
//
// A disk that is still plugged in comes back on the next tick as a new arrival,
// with a fresh probation and no history. That is all forgetting can mean while
// the stick is in the port, and the UI says so before it asks.
func (s *Supervisor) Forget(key string) error {
	s.mu.Lock()
	st, ok := s.disks[key]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if st.Scanning {
		s.mu.Unlock()
		return ErrScanInProgress
	}
	// Deleting under the same lock that guards the map is what keeps a
	// discovery tick from slipping in between and re-creating what is on its
	// way out.
	err := s.store.DeleteDisk(key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.mu.Unlock()
		return err
	}
	delete(s.disks, key)
	s.mu.Unlock()

	s.logger.Info("disk forgotten", "disk", key)
	s.notifyChange(key, "FORGOTTEN")
	return nil
}

// RequestScan asks for a round now. Suppression still applies: asking is for
// impatience, and impatience does not get to override a safety window that
// exists because the disk just took a filesystem down with it. force is the
// one exception, and it is logged as such below. The waits before a round
// nobody asked for do not apply. See ScanRequested.
func (s *Supervisor) RequestScan(key string, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.disks[key]
	if !ok {
		return ErrNotFound
	}
	if st.Scanning {
		// Saying no is the honest answer. Setting NextScanAt here would look
		// like it worked and do nothing: considerDisk skips a disk that is
		// already scanning, and persistRound overwrites the schedule when the
		// round ends, so the request was discarded either way.
		return ErrScanInProgress
	}
	if left := time.Until(st.Schedule.SuppressUntil); left > 0 {
		// force is the same escape hatch `scan -i-mean-it` has always had, now
		// reachable from the UI as well as over ssh. It clears the cooldown and
		// nothing else, and it says so where it can be read back: overriding a
		// window that exists because the disk misbehaved is a decision worth
		// finding again later, next to whatever happened next.
		if !force {
			return ErrScanSuppressed
		}
		s.logger.Warn("cooldown overridden by request",
			"disk", key, "outcome", st.Schedule.LastOutcome,
			"remaining_h", left.Hours())
		_ = s.store.AppendEvent(key, Event{
			Type: EventOverride,
			Params: map[string]any{
				"outcome":     st.Schedule.LastOutcome,
				"remaining_h": round2(left.Hours()),
			},
		})
		st.Schedule.SuppressUntil = time.Time{}
		if err := s.store.SaveSchedule(key, st.Schedule); err != nil {
			s.logger.Error("save schedule after override", "disk", key, "error", err)
		}
	}
	st.Schedule.NextScanAt = time.Now()
	st.ScanRequested = true
	select {
	case s.wake <- struct{}{}:
	default: // a tick is already queued, and it will see this request
	}
	return nil
}

// StopScan ends the round running on a disk. The round notices at its next
// segment boundary, or at once if it is resting. A read already in the kernel
// finishes either way. The round is written down as cancelled, the outcome a
// shutdown gives it. That moves nothing the scheduler learns from, so the next
// attempt comes an hour later and carries on from where this one stopped.
// Stopping does not exclude the disk. Keeping it out of the schedule is a
// separate choice.
func (s *Supervisor) StopScan(key string) error {
	s.mu.Lock()
	st, ok := s.disks[key]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if !st.Scanning || st.stop == nil {
		s.mu.Unlock()
		return ErrNotScanning
	}
	if st.StopRequested {
		// A second press, from another tab or before the page caught up, counts
		// as the same stop.
		s.mu.Unlock()
		return nil
	}
	st.StopRequested = true
	st.stop()
	ev := Event{Type: EventStopped}
	if st.Live != nil {
		ev.Round = st.Live.RoundSeq
		ev.Offset = st.Live.PosMiB << 20
		ev.Params = map[string]any{"done_mib": st.Live.DoneMiB}
	}
	s.mu.Unlock()

	if err := s.store.AppendEvent(key, ev); err != nil {
		s.logger.Error("record stop", "disk", key, "error", err)
	}
	s.logger.Info("scan stop requested", "disk", key)
	s.notifyChange(key, "SCAN_STOPPING")
	return nil
}

// Heartbeat is what the systemd watchdog pings from.
//
// It comes from the supervisor loop and never from a scanner. A scanner blocked
// 1.8s inside pread is not hung. That is the expected case, and a watchdog that
// killed the process for it would fire precisely when things were working.
func (s *Supervisor) Heartbeat() time.Time { return time.Unix(s.heartbeat.Load(), 0) }
