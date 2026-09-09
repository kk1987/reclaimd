package reclaimd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// discoveryInterval is how often sysfs is enumerated. It is slow on purpose:
// discovery reads sysfs only, which never touches the bus, but there is no
// reason to spin either -- a stick that just appeared can wait 30 seconds to be
// noticed, and it has 30 minutes of probation ahead of it regardless.
const discoveryInterval = 30 * time.Second

// diskState is the supervisor's per-disk view. Anything durable lives in the
// store; this is the part that is allowed to be forgotten on restart.
type diskState struct {
	Key      string
	Presence Presence
	Meta     Meta
	Schedule Schedule

	// PresentSince is deliberately NOT persisted. After a daemon restart we
	// cannot know whether the stick stayed plugged in, and assuming it did
	// would let a stick that was swapped during the downtime skip probation.
	PresentSince time.Time
	Present      bool

	Scanning bool
	Live     *LiveProgress
	LastErr  string
}

// Supervisor owns discovery, adoption and scheduling.
type Supervisor struct {
	cfg     Config
	store   *Store
	logger  *slog.Logger
	roots   Roots
	scanner *Scanner

	mu    sync.RWMutex
	disks map[string]*diskState

	// OnLive is called with every progress snapshot. The HTTP layer coalesces
	// these; the supervisor just forwards them.
	OnLive func(LiveProgress)
	// OnChange is called when a disk's lifecycle state changes, so the UI can
	// refetch rather than have a full state push invented for it.
	OnChange func(key, change string)

	heartbeat atomic64
}

func NewSupervisor(cfg Config, store *Store, logger *slog.Logger, roots Roots) *Supervisor {
	return &Supervisor{
		cfg:     cfg,
		store:   store,
		logger:  logger,
		roots:   roots,
		scanner: NewScanner(cfg, store, logger, roots),
		disks:   map[string]*diskState{},
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

	found, err := DiscoverUSBDisks(s.roots)
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
	for key, st := range s.disks {
		if !seen[key] {
			st.Present = false
		}
	}
	candidates := make([]*diskState, 0, len(s.disks))
	for _, st := range s.disks {
		candidates = append(candidates, st)
	}
	s.mu.Unlock()

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
		if now.Sub(st.PresentSince) < s.cfg.AdoptAfter.Duration() {
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
		s.logger.Info("disk adopted", "disk", st.Key)
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
	if up, err := uptime(s.roots); err == nil && up < s.cfg.MinUptime.Duration() {
		s.mu.Unlock()
		return
	}
	if due, _ := st.Schedule.Due(now); !due {
		s.mu.Unlock()
		return
	}

	st.Scanning = true
	in := RoundInput{
		Key:      st.Key,
		Presence: st.Presence,
		Schedule: st.Schedule,
	}
	s.mu.Unlock()

	go s.runRound(ctx, st, in)
}

// runRound opens the device, runs one pass and persists everything.
func (s *Supervisor) runRound(ctx context.Context, st *diskState, in RoundInput) {
	defer func() {
		s.mu.Lock()
		st.Scanning = false
		st.Live = nil
		s.mu.Unlock()
		s.notifyChange(st.Key, "SCAN_END")
	}()

	dev, err := OpenDevice(in.Presence, s.cfg.BlockSize, s.roots)
	if err != nil {
		s.logger.Error("open device", "disk", st.Key, "error", err)
		s.setErr(st, err)
		return
	}
	defer dev.Close()

	if err := dev.WarmUp(ctx, s.cfg.WarmupDiscard); err != nil {
		s.logger.Error("warm up", "disk", st.Key, "error", err)
		s.setErr(st, err)
		return
	}

	if bits, err := s.store.LoadDeferred(st.Key); err == nil {
		in.Deferred = bits
	}
	in.Dev = dev
	in.Ext = NewExternalIOMonitor(s.cfg, s.roots, in.Presence)
	in.OnProgress = func(p LiveProgress) {
		s.mu.Lock()
		cp := p
		st.Live = &cp
		s.mu.Unlock()
		if s.OnLive != nil {
			s.OnLive(p)
		}
	}

	s.notifyChange(st.Key, "SCAN_START")
	s.logger.Info("scan started", "disk", st.Key, "cursor", in.Schedule.Cursor)

	res, err := s.scanner.Round(ctx, in)
	if err != nil {
		s.logger.Error("scan round failed", "disk", st.Key, "error", err)
		s.setErr(st, err)
		return
	}

	s.persistRound(st, res)
}

func (s *Supervisor) persistRound(st *diskState, res RoundResult) {
	key := st.Key
	now := time.Now()

	if err := s.store.SaveProfile(key, res.Latency, s.cfg.BlocksPerSegment(),
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
	sched := st.Schedule.Next(res.Summary.Outcome, now, s.cfg)
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
// exists to show what has NOT been refreshed lately, and quietly resetting a
// deferred segment would hide exactly the thing worth seeing.
//
// The result is a lower bound on freshness, never an upper one. Reads performed
// by whatever filesystem lives on the disk refresh data too, and they are
// invisible from down here at the raw device.
func (s *Supervisor) updateFreshness(key string, res RoundResult) {
	perSeg := s.cfg.BlocksPerSegment()
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
// the scan command drives, and it obeys every rule the daemon obeys --
// suppression included, because a manual run is impatience, not a reason to
// re-enter a window opened by the disk taking a filesystem down.
func (s *Supervisor) ScanOnce(ctx context.Context, key string, force bool) (RoundSummary, error) {
	found, err := DiscoverUSBDisks(s.roots)
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

	dev, err := OpenDevice(p, s.cfg.BlockSize, s.roots)
	if err != nil {
		return RoundSummary{}, err
	}
	defer dev.Close()
	if err := dev.WarmUp(ctx, s.cfg.WarmupDiscard); err != nil {
		return RoundSummary{}, err
	}

	in := RoundInput{Key: key, Dev: dev, Presence: p, Schedule: sched,
		Ext: NewExternalIOMonitor(s.cfg, s.roots, p)}
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
	s.persistRound(st, res)
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

// RequestScan asks for a round now. Suppression still applies: the escape hatch
// is for impatience, not for overriding a safety window that exists because the
// disk just took a filesystem down with it.
func (s *Supervisor) RequestScan(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.disks[key]
	if !ok {
		return ErrNotFound
	}
	if time.Now().Before(st.Schedule.SuppressUntil) {
		return ErrScanSuppressed
	}
	st.Schedule.NextScanAt = time.Now()
	return nil
}

// Heartbeat is what the systemd watchdog pings from.
//
// It comes from the supervisor loop, never from a scanner. A scanner blocked
// 1.8s inside pread is the expected case, not a hang, and a watchdog that
// killed the process for it would fire precisely when things were working.
func (s *Supervisor) Heartbeat() time.Time { return time.Unix(s.heartbeat.Load(), 0) }

// uptime reads /proc/uptime rather than comparing wall clocks, because on a
// router the wall clock at boot is fiction.
func uptime(r Roots) (time.Duration, error) {
	b, err := os.ReadFile(r.Proc + "/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("malformed /proc/uptime")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}
