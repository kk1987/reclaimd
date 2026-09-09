package reclaimd

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"
)

// segmentBitmap records which segments a round did not read. A skipped segment
// is deferred, never abandoned: 1912 bits is 239 bytes for a 64 GB drive, so
// carrying the debt forward across rounds costs nothing at all.
type segmentBitmap []byte

func newSegmentBitmap(n int) segmentBitmap { return make(segmentBitmap, (n+7)/8) }

func (b segmentBitmap) Set(i int) {
	if i >= 0 && i/8 < len(b) {
		b[i/8] |= 1 << (i % 8)
	}
}

func (b segmentBitmap) Get(i int) bool {
	return i >= 0 && i/8 < len(b) && b[i/8]&(1<<(i%8)) != 0
}

func (b segmentBitmap) Count() int {
	n := 0
	for _, x := range b {
		for ; x != 0; x &= x - 1 {
			n++
		}
	}
	return n
}

// retryEntry is one block worth re-probing at the end of the round.
type retryEntry struct {
	Offset     int64
	Reason     string
	Latency    time.Duration
	EventID    uint64
	DeferUntil time.Time
}

// Scanner runs one round over one disk.
type Scanner struct {
	cfg    Config
	store  *Store
	logger *slog.Logger
	roots  Roots
}

func NewScanner(cfg Config, store *Store, logger *slog.Logger, roots Roots) *Scanner {
	return &Scanner{cfg: cfg, store: store, logger: logger, roots: roots}
}

// RoundInput carries everything a round needs that it cannot derive itself.
type RoundInput struct {
	Key      string
	Dev      BlockReader
	Presence Presence
	Schedule Schedule
	Deferred segmentBitmap
	Ext      *ExternalIOMonitor

	// OnProgress, when set, is called every ProgressEvery blocks and on every
	// notable event. It must not block: the caller coalesces and rate-limits
	// before anything reaches a browser.
	OnProgress func(LiveProgress)
}

// LiveProgress is a snapshot of a running round.
//
// Every numeric field carries a unit suffix (_mib, _ms, _n, _s) because the
// frontend uses that suffix to decide how to format it. The daemon emits
// numbers and codes; turning them into Chinese or English is the browser's job.
type LiveProgress struct {
	Disk      string  `json:"disk"`
	RoundSeq  uint64  `json:"seq"`
	PosMiB    int64   `json:"pos_mib"`
	TotalMiB  int64   `json:"total_mib"`
	DoneMiB   int64   `json:"done_mib"`
	SpeedMiBs float64 `json:"speed_mibs_n"`
	LatP50Ms  float64 `json:"lat_p50_ms"`
	BaseP50Ms float64 `json:"base_p50_ms"`
	DriftN    float64 `json:"drift_n"`
	Duty      string  `json:"duty"`
	ElapsedS  float64 `json:"elapsed_s"`
	ETAS      float64 `json:"eta_s"`
	SlowN     int     `json:"slow_n"`
	DangerN   int     `json:"danger_n"`
	DropN     int     `json:"drop_n"`
	DeferN    int     `json:"defer_n"`
	ReprobeN  int     `json:"reprobe_n"`
	SlowMs    float64 `json:"slow_threshold_ms"`
	DangerMs  float64 `json:"danger_threshold_ms"`
	Phase     string  `json:"phase"`
}

// Round phases, reported as codes.
const (
	PhaseWarmup  = "WARMUP"
	PhaseSweep   = "SWEEP"
	PhaseReprobe = "REPROBE"
	PhaseDone    = "DONE"
)

// RoundResult is what the supervisor persists and the UI renders.
type RoundResult struct {
	Summary  RoundSummary
	Latency  *LatencyMap
	Deferred segmentBitmap
	Cursor   int64
	Baseline Baseline
}

// blocksPerSegment reads the segment geometry back off the round that was
// actually run, rather than off the config. The config no longer knows the
// block size on its own -- that was resolved from the disk -- and the latency
// map carries the value the pass was measured with.
func (r RoundResult) blocksPerSegment(cfg Config) int {
	if r.Latency == nil || r.Latency.BlockSize <= 0 {
		return 0
	}
	return int(cfg.SegmentSize / int64(r.Latency.BlockSize))
}

// Round performs one pass with early backoff.
//
// The strategy rests on one finding: a slow read that COMPLETED has already
// handed the controller its read-reclaim trigger, so the refresh objective for
// that block is met. What remains after that is only risk -- and 84% of the
// dropouts in the forensics had a slow block within the preceding 10 MiB. So
// the correct move on seeing a slow block is to leave, not to press on. The
// backoff costs nothing and buys the whole margin.
func (s *Scanner) Round(ctx context.Context, in RoundInput) (RoundResult, error) {
	dev := in.Dev
	// The device is the single source of truth for the read size: it was
	// opened with the size resolved for this disk, so taking it from the
	// config again would let the two drift apart.
	cfg := s.cfg
	cfg.BlockSize = dev.BlockSize()
	blockSize := int64(cfg.BlockSize)
	blockCount := dev.Size() / blockSize
	perSeg := cfg.BlocksPerSegment()
	segCount := int((blockCount + int64(perSeg) - 1) / int64(perSeg))

	seq := in.Schedule.RoundSeq + 1
	started := time.Now()
	lat := NewLatencyMap(cfg.BlockSize, blockCount, seq, started)
	deferred := newSegmentBitmap(segCount)

	res := RoundResult{
		Latency:  lat,
		Deferred: deferred,
		Summary: RoundSummary{
			Seq:         seq,
			StartedAt:   started,
			BlocksTotal: int(blockCount),
			StartOffset: in.Schedule.Cursor,
		},
	}

	base, err := LearnBaseline(ctx, dev, cfg, in.Schedule.Cursor,
		in.Schedule.LearnedBaseline.Duration())
	if err != nil {
		return s.finish(res, started, s.outcomeFor(err), err)
	}
	res.Baseline = base
	res.Summary.BaselineMicros = base.RoundP50.Microseconds()
	res.Summary.SlowMicros = base.Slow.Microseconds()
	res.Summary.DangerMicros = base.Danger.Microseconds()

	duty := NewDutyController(cfg)
	var retries []retryEntry
	var blocksRead, slow, danger, media, dropouts int
	totalMiB := (blockCount * blockSize) >> 20

	emit := func(phase string, pos int64) {
		if in.OnProgress == nil {
			return
		}
		elapsed := time.Since(started).Seconds()
		doneMiB := int64(blocksRead) * blockSize >> 20
		var speed, eta float64
		if elapsed > 0 {
			speed = float64(doneMiB) / elapsed
		}
		if speed > 0 {
			// The ETA has to include the rest the duty controller is going to
			// take, or it becomes badly wrong the moment the controller backs
			// off -- which is exactly when somebody is watching.
			remaining := float64(totalMiB - doneMiB)
			eta = remaining/speed*(1+duty.RestRatio()) + 0
		}
		rolling := base.Rolling.Median()
		in.OnProgress(LiveProgress{
			Disk: in.Key, RoundSeq: seq,
			PosMiB: pos >> 20, TotalMiB: totalMiB, DoneMiB: doneMiB,
			SpeedMiBs: round2(speed),
			LatP50Ms:  msOf(rolling), BaseP50Ms: msOf(base.RoundP50),
			DriftN: round2(duty.Drift()), Duty: duty.State(),
			ElapsedS: round2(elapsed), ETAS: round2(eta),
			SlowN: slow, DangerN: danger, DropN: dropouts,
			DeferN: deferred.Count(), ReprobeN: len(retries),
			SlowMs: msOf(base.Slow), DangerMs: msOf(base.Danger),
			Phase: phase,
		})
	}
	emit(PhaseSweep, in.Schedule.Cursor)
	outcome := OutcomeClean
	cursor := in.Schedule.Cursor
	var fatal error

	order := segmentOrder(segCount, in.Deferred, in.Schedule.Cursor, blockSize, perSeg)

	// segIdx survives the loop so the round knows whether it finished the
	// ground it set out to cover. Every early exit here is a decision to stop,
	// and a round that stopped is not the same observation as one that swept
	// the disk -- the scheduler has to be able to tell them apart.
	segIdx := 0
sweep:
	for ; segIdx < len(order); segIdx++ {
		seg := order[segIdx]
		if err := ctx.Err(); err != nil {
			outcome, fatal = OutcomeCancelled, err
			break
		}
		if in.Ext != nil {
			if err := in.Ext.WaitIfBusy(ctx); err != nil {
				if errors.Is(err, ErrExternalBusy) {
					outcome = OutcomeExternal
					break
				}
				outcome, fatal = OutcomeCancelled, err
				break
			}
		}

		first := int64(seg) * int64(perSeg)
		last := min(first+int64(perSeg), blockCount)

		for idx := first; idx < last; idx++ {
			off := idx * blockSize
			d, err := dev.ReadBlock(off)

			switch {
			case err == nil:
				// fall through to threshold checks

			case errors.Is(err, ErrDeviceDisconnected):
				lat.Values[idx] = LatError
				dropouts++
				outcome = OutcomeDropout
				s.onDropout(ctx, in, seg, off, d, seq, perSeg, blockSize)
				break sweep

			case errors.Is(err, ErrAlignment):
				// Our own bug. Fail loudly rather than spending months
				// disguised as a mysteriously flaky stick.
				lat.Values[idx] = LatError
				outcome, fatal = OutcomeCancelled, err
				break sweep

			case errors.Is(err, ErrMediaError):
				lat.Values[idx] = LatError
				media++
				retries = append(retries, s.deferBlock(in.Key, seq, off, d,
					EventMedia, cfg.ReprobeDelay.Duration()))
				markSegments(deferred, seg, seg, segCount)
				continue sweep

			default:
				lat.Values[idx] = LatError
				outcome, fatal = OutcomeCancelled, err
				break sweep
			}

			lat.Values[idx] = EncodeLatency(d)
			blocksRead++
			cursor = off + blockSize
			if in.Ext != nil {
				in.Ext.RecordSelfRead(cfg.BlockSize)
			}

			if d >= base.Danger {
				danger++
				retries = append(retries, s.deferBlock(in.Key, seq, off, d,
					EventNearHang, cfg.ReprobeDelay.Duration()))
				markSegments(deferred, seg, seg+cfg.DangerSkipSegments, segCount)
				if outcome == OutcomeClean {
					outcome = OutcomeSlow
				}
				emit(PhaseSweep, off)
				if err := sleepCtx(ctx, cfg.DangerCooldown.Duration()); err != nil {
					outcome, fatal = OutcomeCancelled, err
					break sweep
				}
				if danger >= cfg.MaxDangerPerRound {
					// The disk is one read away from taking a mounted overlay
					// down. Stop with the same severity a real dropout gets.
					outcome = OutcomeNearHang
					break sweep
				}
				continue sweep
			}

			if d >= base.Slow {
				slow++
				retries = append(retries, s.deferBlock(in.Key, seq, off, d,
					EventSlow, cfg.ReprobeDelay.Duration()))
				markSegments(deferred, seg, seg+cfg.SlowSkipSegments, segCount)
				if outcome == OutcomeClean {
					outcome = OutcomeSlow
				}
				emit(PhaseSweep, off)
				if err := sleepCtx(ctx, cfg.SlowCooldown.Duration()); err != nil {
					outcome, fatal = OutcomeCancelled, err
					break sweep
				}
				continue sweep
			}

			base.Rolling.Observe(d)
			if blocksRead%cfg.DutyEvalEvery == 0 {
				duty.Evaluate(base.Rolling.Median(), base.RoundP50)
				emit(PhaseSweep, off)
			}
			if err := duty.Pay(ctx, d); err != nil {
				outcome, fatal = OutcomeCancelled, err
				break sweep
			}
		}

		// Circuit breaker: past 5% slow on a meaningful sample the marginal
		// value of continuing is small and the dropout risk is not.
		if blocksRead > 2000 && slow*20 > blocksRead {
			outcome = OutcomeSlow
			break
		}
	}

	healed, stillSlow := 0, 0
	emit(PhaseReprobe, cursor)
	if outcome != OutcomeDropout && outcome != OutcomeCancelled {
		healed, stillSlow = s.reprobe(ctx, in, retries, base, lat, blockSize)
	}

	if outcome == OutcomeClean && media > 0 {
		outcome = OutcomeMediaErrors
	}

	res.Summary.Completed = segIdx == len(order)
	res.Summary.BlocksRead = blocksRead
	res.Summary.SlowBlocks = slow
	res.Summary.DangerBlocks = danger
	res.Summary.MediaErrors = media
	res.Summary.Dropouts = dropouts
	res.Summary.Healed = healed
	res.Summary.StillSlow = stillSlow
	res.Summary.Deferred = deferred.Count()
	res.Summary.BytesRead = int64(blocksRead) * blockSize
	res.Cursor = nextCursor(in.Schedule.Cursor, cursor, outcome, blockSize, perSeg, blockCount)
	lat.Baseline = base.RoundP50
	emit(PhaseDone, cursor)

	return s.finish(res, started, outcome, fatal)
}

func (s *Scanner) finish(res RoundResult, started time.Time, outcome string, err error) (RoundResult, error) {
	res.Summary.EndedAt = time.Now()
	res.Summary.Outcome = outcome
	if res.Summary.StartedAt.IsZero() {
		res.Summary.StartedAt = started
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return res, nil // shutdown is not a failure
	}
	if errors.Is(err, ErrAlignment) {
		return res, err
	}
	return res, nil
}

func (s *Scanner) outcomeFor(err error) string {
	switch {
	case errors.Is(err, ErrDeviceDisconnected):
		return OutcomeDropout
	case errors.Is(err, ErrExternalBusy):
		return OutcomeExternal
	case errors.Is(err, context.Canceled):
		return OutcomeCancelled
	default:
		return OutcomeCancelled
	}
}

// onDropout runs the sequence whose ORDER is itself safety-critical.
//
// The suppression window is fsynced first -- before logging, before the event,
// before waiting for the device. Losing power one second from now must not lose
// the 24 hours that keep the next boot from walking straight back into the same
// fault on a disk that is carrying a mounted filesystem.
func (s *Scanner) onDropout(ctx context.Context, in RoundInput, seg int, off int64,
	d time.Duration, seq uint64, perSeg int, blockSize int64) {

	resume := alignDown(off, int64(perSeg)*blockSize) + int64(perSeg)*blockSize*int64(s.cfg.DangerSkipSegments+1)
	prog := Progress{
		Cursor:        resume,
		RoundSeq:      seq,
		SuppressUntil: time.Now().Add(s.cfg.SuppressAfterDropout.Duration()),
		LastOutcome:   OutcomeDropout,
	}
	if err := s.store.SaveProgress(in.Key, prog); err != nil {
		s.logger.Error("persist dropout suppression", "error", err, "disk", in.Key)
	}

	s.logger.Warn("device dropped off the bus",
		"disk", in.Key, "offset", off, "hang_ms", d.Milliseconds(),
		"code", CodeDeviceDisconnected)

	_ = s.store.AppendEvent(in.Key, Event{
		Type:   EventDropout,
		Round:  seq,
		Offset: off,
		Params: map[string]any{
			"hang_ms":    d.Milliseconds(),
			"offset_mib": off >> 20,
			"suppress_h": s.cfg.SuppressAfterDropout.Duration().Hours(),
			"resume_mib": resume >> 20,
		},
	})

	// Confirming the device came back is worth the wait even though the round
	// is over: on a mounted overlay, "did the backing store return" is the
	// question that actually matters.
	p, err := WaitForReattach(ctx, s.roots, in.Presence.Identity, int(blockSize),
		s.cfg.ReattachTimeout.Duration())
	if err != nil {
		s.logger.Error("device did not come back", "disk", in.Key, "error", err)
		return
	}
	s.logger.Info("device re-enumerated", "disk", in.Key, "kernel_name", p.KernelName)
}

func (s *Scanner) deferBlock(key string, seq uint64, off int64, d time.Duration,
	kind string, delay time.Duration) retryEntry {

	id := s.store.NextEventID()
	_ = s.store.AppendEvent(key, Event{
		ID:     id,
		Type:   kind,
		Round:  seq,
		Offset: off,
		Params: map[string]any{
			"latency_ms": d.Milliseconds(),
			"offset_mib": off >> 20,
		},
	})
	return retryEntry{Offset: off, Reason: kind, Latency: d, EventID: id,
		DeferUntil: time.Now().Add(delay)}
}

// reprobe revisits the blocks that caused a backoff.
//
// The delay before a re-probe is not arbitrary. dmesg reports "read cache:
// enabled" on this controller, so an immediate re-read would be served from its
// buffer and report a healed block that was never touched. Waiting clears the
// cache and, in the same stroke, gives the background reclaim time to finish --
// which is the thing being tested.
func (s *Scanner) reprobe(ctx context.Context, in RoundInput, entries []retryEntry,
	base Baseline, lat *LatencyMap, blockSize int64) (healed, stillSlow int) {

	if len(entries) == 0 {
		return 0, 0
	}
	if len(entries) > s.cfg.ReprobeMax {
		// Probe the ones that stopped the round, not a second sweep's worth.
		entries = entries[:s.cfg.ReprobeMax]
	}

	// Wait the delay out instead of stepping over it.
	//
	// This used to skip any entry still inside its window and leave it "for
	// next round", which quietly guaranteed the opposite of what it says: a
	// round that ends on the circuit breaker is short by construction -- the
	// one that prompted this fix ran for 3m18s against a 10 minute delay -- so
	// every entry was always still waiting, healed and still-slow were always
	// zero, and the next round is an interval away and resumes at a cursor
	// past these offsets anyway. The disks that trip the breaker fastest were
	// the ones the healing measurement never ran on.
	//
	// Waiting costs nothing that matters: it is an idle sleep on an open
	// read-only descriptor, and the round is already over in every sense but
	// the bookkeeping.
	wait := time.Duration(0)
	for _, e := range entries {
		if d := time.Until(e.DeferUntil); d > wait {
			wait = d
		}
	}
	if wait > s.cfg.ReprobeDelay.Duration() {
		wait = s.cfg.ReprobeDelay.Duration() // defensive: a clock jump, not a plan
	}
	if wait > 0 {
		if err := sleepCtx(ctx, wait); err != nil {
			return 0, 0
		}
	}

	// The budget covers the probing, not the waiting.
	deadline := time.Now().Add(s.cfg.ReprobeBudget.Duration())
	n := 0
	for _, e := range entries {
		if n >= s.cfg.ReprobeMax || time.Now().After(deadline) {
			break
		}
		n++

		worst := time.Duration(0)
		failed := false
		for k := int64(-2); k <= 2; k++ {
			off := e.Offset + k*blockSize
			if off < 0 || off >= in.Dev.Size() {
				continue
			}
			d, err := in.Dev.ReadBlock(off)
			if err != nil {
				failed = true
				break
			}
			if idx := off / blockSize; idx < int64(len(lat.Values)) {
				lat.Values[idx] = EncodeLatency(d)
			}
			if d > worst {
				worst = d
			}
		}
		if failed {
			// Do not push our luck on a disk that just failed a re-probe.
			return healed, stillSlow
		}

		switch {
		case worst >= base.Slow:
			stillSlow++
			_ = s.store.AppendEvent(in.Key, Event{
				Type: EventStillSlow, Round: lat.RoundSeq, Offset: e.Offset,
				HealsRef: e.EventID,
				Params: map[string]any{
					"latency_ms": worst.Milliseconds(),
					"offset_mib": e.Offset >> 20,
				},
			})
		default:
			healed++
			// This is the number the report leads with: it is the same
			// phenomenon as 345 of 347 extreme blocks reading normally on the
			// next pass, observed one block at a time.
			_ = s.store.AppendEvent(in.Key, Event{
				Type: EventHealed, Round: lat.RoundSeq, Offset: e.Offset,
				HealsRef: e.EventID,
				Params: map[string]any{
					"was_ms":     e.Latency.Milliseconds(),
					"now_ms":     worst.Milliseconds(),
					"offset_mib": e.Offset >> 20,
				},
			})
		}
		if err := sleepCtx(ctx, s.cfg.ReprobeSpacing.Duration()); err != nil {
			return healed, stillSlow
		}
	}
	return healed, stillSlow
}

// segmentOrder decides what to read and in what order: everything owed from
// last round first, then a full rotation starting from the cursor.
//
// Restarting at zero every round would be a real bug rather than a
// simplification: a segment at 40 GiB that reliably drops the device would mean
// everything past it never gets refreshed at all, which is precisely the
// failure this daemon exists to prevent.
func segmentOrder(segCount int, owed segmentBitmap, cursor, blockSize int64, perSeg int) []int {
	seen := make([]bool, segCount)
	order := make([]int, 0, segCount)

	for i := 0; i < segCount; i++ {
		if owed.Get(i) {
			order = append(order, i)
			seen[i] = true
		}
	}
	start := 0
	if segBytes := int64(perSeg) * blockSize; segBytes > 0 {
		start = int(cursor / segBytes)
	}
	if start < 0 || start >= segCount {
		start = 0
	}
	for k := 0; k < segCount; k++ {
		i := (start + k) % segCount
		if !seen[i] {
			order = append(order, i)
		}
	}
	return order
}

// nextCursor advances the rotating start point for the following round.
//
// After a dropout the cursor deliberately jumps PAST the segment that caused
// it, so the next round approaches it last -- by which time it has had a full
// suppression window of idle in which the controller can finish reclaiming it.
func nextCursor(prev, reached int64, outcome string, blockSize int64, perSeg int, blockCount int64) int64 {
	segBytes := int64(perSeg) * blockSize
	total := blockCount * blockSize
	if segBytes <= 0 || total <= 0 {
		return 0
	}
	var c int64
	switch outcome {
	case OutcomeClean, OutcomeMediaErrors:
		// A full rotation completed; start the next one where this one began.
		c = prev
	default:
		c = alignDown(reached, segBytes) + segBytes
	}
	if c >= total {
		c = 0
	}
	// The cursor must land on a segment boundary or the mod-32 structure
	// analysis in the report silently shifts out of phase.
	return alignDown(c, segBytes)
}

func markSegments(b segmentBitmap, from, to, segCount int) {
	if to >= segCount {
		to = segCount - 1
	}
	for i := from; i <= to; i++ {
		b.Set(i)
	}
}

func alignDown(v, a int64) int64 {
	if a <= 0 {
		return v
	}
	return v - v%a
}

func msOf(d time.Duration) float64 { return round2(float64(d) / float64(time.Millisecond)) }

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
