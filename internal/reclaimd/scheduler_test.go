package reclaimd

import (
	"testing"
	"time"
)

// A round that stopped on its circuit breaker is not a full pass, and the
// interval is a full-pass cadence. The round that prompted this covered 8.6%
// of the disk and would have waited 84 hours to resume the other 91%.
func TestPartialPassResumesAtTheFloorNotTheInterval(t *testing.T) {
	cfg := mustConfig(t)
	now := time.Now()

	sched := NewSchedule(cfg, now)
	sched.Interval = Duration(168 * time.Hour)

	partial := RoundSummary{
		Outcome:     OutcomeNearHang,
		Completed:   false,
		BlocksRead:  41040,
		BlocksTotal: 480000,
	}
	got := sched.Next(partial, now, cfg)

	// The interval itself still halves: it is what a full pass would cost.
	if want := 84 * time.Hour; got.Interval.Duration() != want {
		t.Errorf("interval = %v, want %v", got.Interval.Duration(), want)
	}
	// But the next round comes back at the floor, not an interval away.
	wait := got.NextScanAt.Sub(now)
	if wait > cfg.ScanIntervalMin.Duration()+time.Minute {
		t.Errorf("next scan in %v; a partial pass must resume by the floor %v",
			wait, cfg.ScanIntervalMin.Duration())
	}
	// And never inside the suppression window the same round just set.
	if got.NextScanAt.Before(got.SuppressUntil) {
		t.Errorf("next scan %v is inside the suppression window ending %v",
			got.NextScanAt, got.SuppressUntil)
	}
	if got.LastReason != ReasonResumePartial {
		t.Errorf("reason = %q, want %q", got.LastReason, ReasonResumePartial)
	}
	if pct := got.LastReasonParams["covered_pct"]; pct != 8.6 {
		t.Errorf("covered_pct = %v, want 8.6", pct)
	}
}

// A pass that swept the whole disk keeps the ordinary cadence, however it
// graded: the point of the resume rule is unread ground, not bad news.
func TestCompletePassKeepsTheInterval(t *testing.T) {
	cfg := mustConfig(t)
	now := time.Now()

	sched := NewSchedule(cfg, now)
	sched.Interval = Duration(168 * time.Hour)

	full := RoundSummary{
		Outcome: OutcomeSlow, Completed: true,
		BlocksRead: 480000, BlocksTotal: 480000,
	}
	got := sched.Next(full, now, cfg)

	if wait := got.NextScanAt.Sub(now); wait < got.Interval.Duration() {
		t.Errorf("next scan in %v, want at least the interval %v",
			wait, got.Interval.Duration())
	}
	if got.LastReason == ReasonResumePartial {
		t.Error("a completed pass was scheduled as a resume")
	}
}

// The suppression window wins over the floor when it is longer: it is the
// safety mechanism, and resuming early would walk back into the fault.
func TestSuppressionWindowOutranksTheResumeFloor(t *testing.T) {
	cfg := mustConfig(t)
	cfg.ScanIntervalMin = Duration(time.Hour)
	now := time.Now()

	sched := NewSchedule(cfg, now)
	sched.Interval = Duration(168 * time.Hour)
	got := sched.Next(RoundSummary{Outcome: OutcomeNearHang, BlocksTotal: 100}, now, cfg)

	if got.NextScanAt.Before(got.SuppressUntil) {
		t.Errorf("resumed at %v, inside the suppression window ending %v",
			got.NextScanAt, got.SuppressUntil)
	}
}
