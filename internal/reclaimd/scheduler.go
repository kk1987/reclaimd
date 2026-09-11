package reclaimd

import (
	"math"
	"math/rand"
	"time"
)

// SanityEpoch guards machines with no RTC.
//
// The MX4300 boots with a wall clock somewhere in 1970 until NTP lands, and an
// absolute next_scan_at compared against that clock either fires instantly and
// forever, or never fires at all. Neither failure announces itself. The daemon
// simply behaves wrongly until somebody goes looking.
var SanityEpoch = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// Schedule is everything the daemon needs to decide when to scan a disk next,
// plus the cross-round knowledge that makes round one after a reboot sensible.
type Schedule struct {
	Schema           int       `json:"schema"`
	Interval         Duration  `json:"interval"`
	NextScanAt       time.Time `json:"next_scan_at"`
	SuppressUntil    time.Time `json:"suppress_until,omitempty"`
	LastOutcome      string    `json:"last_outcome,omitempty"`
	LastRoundAt      time.Time `json:"last_round_at,omitempty"`
	LearnedBaseline  Duration  `json:"learned_baseline,omitempty"`
	Cursor           int64     `json:"cursor"`
	ConsecutiveClean int       `json:"consecutive_clean"`
	RoundSeq         uint64    `json:"round_seq"`

	// LastReason is the code the UI renders to explain the current interval.
	// The daemon never produces a sentence. The browser turns this plus
	// LastReasonParams into one, in whichever language is selected.
	LastReason       string         `json:"last_reason,omitempty"`
	LastReasonParams map[string]any `json:"last_reason_params,omitempty"`
}

// Reason codes for interval changes.
const (
	ReasonInit          = "INIT"
	ReasonCleanBackoff  = "SCAN_CLEAN_BACKOFF"
	ReasonSlowTighten   = "SCAN_SLOW_TIGHTEN"
	ReasonDangerTighten = "SCAN_DANGER_TIGHTEN"
	ReasonDropoutHalve  = "SCAN_DROPOUT_HALVE"
	ReasonNeutral       = "SCAN_NEUTRAL_RETRY"
	ReasonResumePartial = "SCAN_RESUME_PARTIAL"
	ReasonClockRebased  = "CLOCK_REBASED"
)

// NewSchedule starts a freshly adopted disk.
//
// The first pass is scheduled immediately instead of one interval out. A disk
// with no history has no baseline, no map and no verdict, so everything this
// tool said about it would be "unknown" for a week. Adoption already waited
// out its probation, so there is nothing left to be cautious about.
func NewSchedule(cfg Config, now time.Time) Schedule {
	return Schedule{
		Schema:     stateSchema,
		Interval:   cfg.ScanIntervalInit,
		NextScanAt: now,
		LastReason: ReasonInit,
		LastReasonParams: map[string]any{
			"value_h": hours(cfg.ScanIntervalInit.Duration()),
		},
	}
}

// Next applies the multiplicative policy after a round.
//
// The policy is multiplicative because what it tracks is a rate of decay: a
// disk that needs attention twice as often needs the interval halved, not
// shortened by a day. The clamp floor keeps a sick disk from being scanned
// into the ground. The ceiling keeps a healthy one from aging past the
// retention window this tool exists to defend.
func (s Schedule) Next(sum RoundSummary, now time.Time, cfg Config) Schedule {
	outcome := sum.Outcome
	prev := s.Interval.Duration()
	next := prev
	reason := ReasonNeutral
	params := map[string]any{"prev_h": hours(prev)}

	switch outcome {
	case OutcomeClean:
		next = scale(prev, cfg.FactorClean)
		s.ConsecutiveClean++
		reason = ReasonCleanBackoff
		params["clean_passes_n"] = s.ConsecutiveClean
		params["factor_n"] = cfg.FactorClean

	case OutcomeSlow, OutcomeMediaErrors:
		next = scale(prev, cfg.FactorSlow)
		s.ConsecutiveClean = 0
		reason = ReasonSlowTighten
		params["factor_n"] = cfg.FactorSlow

	case OutcomeNearHang:
		next = scale(prev, cfg.FactorDanger)
		s.ConsecutiveClean = 0
		s.SuppressUntil = now.Add(cfg.SuppressAfterNearHang.Duration())
		reason = ReasonDangerTighten
		params["factor_n"] = cfg.FactorDanger
		params["suppress_h"] = hours(cfg.SuppressAfterNearHang.Duration())

	case OutcomeDropout:
		next = scale(prev, cfg.FactorDanger)
		s.ConsecutiveClean = 0
		s.SuppressUntil = now.Add(cfg.SuppressAfterDropout.Duration())
		reason = ReasonDropoutHalve
		params["factor_n"] = cfg.FactorDanger
		params["suppress_h"] = hours(cfg.SuppressAfterDropout.Duration())

	case OutcomeCancelled:
		// Interrupted, by a shutdown or from the page: we learned nothing about
		// the disk, so the interval must not move in either direction.
		s.LastOutcome = outcome
		s.LastRoundAt = now
		s.NextScanAt = now.Add(retryAfterCancel)
		return s

	case OutcomeExternal:
		// Somebody else was using the disk all round. Also neutral: nothing is
		// wrong, and nothing was measured.
		s.LastOutcome = outcome
		s.LastRoundAt = now
		s.NextScanAt = now.Add(2 * time.Hour)
		return s
	}

	clamped := false
	if lo := cfg.ScanIntervalMin.Duration(); next < lo {
		next, clamped = lo, true
	}
	if hi := cfg.ScanIntervalMax.Duration(); next > hi {
		next, clamped = hi, true
	}

	s.Interval = Duration(next)
	s.LastOutcome = outcome
	s.LastRoundAt = now
	s.LastReason = reason
	params["value_h"] = hours(next)
	params["clamped"] = clamped
	s.LastReasonParams = params

	// Jitter keeps a fleet from synchronising, but the reason it matters on one
	// machine is different: without it a reboot loop would retrigger a scan at
	// exactly the same moment every time.
	s.NextScanAt = now.Add(next + time.Duration(rand.Int63n(int64(next/10)+1)))

	// The interval is a full-pass cadence: how long a whole disk may go
	// unread. A round that stopped on its circuit breaker did not deliver a
	// full pass (the one that prompted this covered 8.6% and left a cursor
	// mid-disk), and waiting a full interval to resume applies a whole-disk
	// answer to a fraction of a disk. Come back at the floor instead, which is
	// the documented answer to "how often is too often for a sick disk", and
	// never before the suppression window that the same round just set.
	if !sum.Completed && outcome != OutcomeClean && outcome != OutcomeMediaErrors {
		resume := now.Add(cfg.ScanIntervalMin.Duration())
		if resume.Before(s.SuppressUntil) {
			resume = s.SuppressUntil
		}
		if resume.Before(s.NextScanAt) {
			s.NextScanAt = resume
			s.LastReason = ReasonResumePartial
			params["resume_h"] = hours(resume.Sub(now))
			params["covered_pct"] = coveredPct(sum)
			s.LastReasonParams = params
		}
	}
	return s
}

// coveredPct is how much of the disk the round actually read, for the line the
// UI shows next to a resume. Rounded to one place: it is only an explanation,
// and nothing depends on the exact value.
func coveredPct(sum RoundSummary) float64 {
	if sum.BlocksTotal <= 0 {
		return 0
	}
	return math.Round(float64(sum.BlocksRead)/float64(sum.BlocksTotal)*1000) / 10
}

// retryAfterCancel is how soon a round that was interrupted is tried again.
const retryAfterCancel = time.Hour

// Postpone moves the next attempt off now without recording a round. It is for
// a round that ended before reading a block, which has no outcome to record and
// no cursor to move, but would otherwise be due again on the very next tick.
func (s Schedule) Postpone(now time.Time) Schedule {
	s.NextScanAt = now.Add(retryAfterCancel)
	return s
}

// WhatIf is what the UI shows so that the policy can be argued with: the three
// intervals the next round could produce, given today's value.
func (s Schedule) WhatIf(cfg Config) map[string]any {
	clampf := func(d time.Duration) float64 {
		if d < cfg.ScanIntervalMin.Duration() {
			d = cfg.ScanIntervalMin.Duration()
		}
		if d > cfg.ScanIntervalMax.Duration() {
			d = cfg.ScanIntervalMax.Duration()
		}
		return hours(d)
	}
	prev := s.Interval.Duration()
	return map[string]any{
		"clean_h":   clampf(scale(prev, cfg.FactorClean)),
		"slow_h":    clampf(scale(prev, cfg.FactorSlow)),
		"dropout_h": clampf(scale(prev, cfg.FactorDanger)),
	}
}

// Due reports whether a round should start now, and why not when it should not.
func (s Schedule) Due(now time.Time) (bool, string) {
	if now.Before(s.SuppressUntil) {
		return false, CodeScanSuppressed
	}
	if now.Before(s.NextScanAt) {
		return false, ""
	}
	return true, ""
}

// RebaseIfClockUnsynced repairs a schedule written against a bogus clock.
//
// Two symptoms are handled: an absolute timestamp far in the future (written
// before NTP, when "now" was 1970) and a LastRoundAt in the future (the clock
// went backwards). Both are fixed the same way: discard the absolute value
// and re-derive it from the interval, which is the part that is still valid.
func (s Schedule) RebaseIfClockUnsynced(now time.Time) (Schedule, bool) {
	if now.Before(SanityEpoch) {
		return s, false // caller must wait for NTP, nothing sane to compute yet
	}
	needsRebase := s.NextScanAt.After(now.Add(s.Interval.Duration()+24*time.Hour)) ||
		s.LastRoundAt.After(now)
	if !needsRebase {
		return s, false
	}
	s.NextScanAt = now.Add(s.Interval.Duration())
	if s.LastRoundAt.After(now) {
		s.LastRoundAt = time.Time{}
	}
	if s.SuppressUntil.After(now.Add(48 * time.Hour)) {
		s.SuppressUntil = now.Add(24 * time.Hour)
	}
	s.LastReason = ReasonClockRebased
	s.LastReasonParams = map[string]any{"value_h": hours(s.Interval.Duration())}
	return s, true
}

func scale(d time.Duration, f float64) time.Duration {
	return time.Duration(math.Round(float64(d) * f))
}

func hours(d time.Duration) float64 {
	return math.Round(d.Hours()*100) / 100
}
