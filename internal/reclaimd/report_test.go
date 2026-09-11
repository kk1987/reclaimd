package reclaimd

import (
	"testing"
	"time"
)

// The cooldown row reported every suppression as a dropout, including the far
// more common one that follows a near-hang. Telling somebody their drive fell
// off the USB bus when it did not is the worst thing a diagnostic can say: it
// is a hardware event they will go hunting for in dmesg and never find. The
// two also differ in length -- 6h against 24h -- so the number did not match
// the story either.
func TestCooldownNamesWhatActuallyHappened(t *testing.T) {
	cfg := mustConfig(t)
	id := DiskIdentity{MaxSectorsKB: 1024}
	now := time.Now()

	find := func(rows []Controller) Controller {
		t.Helper()
		for _, c := range rows {
			if c.ID == "cooldown" {
				return c
			}
		}
		t.Fatal("no cooldown row")
		return Controller{}
	}

	nearHang := Schedule{LastOutcome: OutcomeNearHang, SuppressUntil: now.Add(6 * time.Hour)}
	if got := find(controllersFor(nearHang, nil, cfg, id, nil)).Reason; got != "SUPPRESSED_AFTER_NEAR_HANG" {
		t.Errorf("after a near-hang the reason was %q", got)
	}

	dropout := Schedule{LastOutcome: OutcomeDropout, SuppressUntil: now.Add(24 * time.Hour)}
	if got := find(controllersFor(dropout, nil, cfg, id, nil)).Reason; got != "SUPPRESSED_AFTER_DROPOUT" {
		t.Errorf("after a dropout the reason was %q", got)
	}

	quiet := Schedule{LastOutcome: OutcomeClean}
	row := find(controllersFor(quiet, nil, cfg, id, nil))
	if row.Reason != "NOT_TRIGGERED" || row.Value != 0 {
		t.Errorf("with no cooldown running the row was %q / %v", row.Reason, row.Value)
	}
}

// A cancelled pass read part of the disk at best. Judged by it, the verdict on a
// drive whose last real pass had just dropped off the bus came out as healthy,
// clean end to end -- one press of Stop away.
func TestACancelledPassDoesNotReplaceTheVerdict(t *testing.T) {
	cfg := mustConfig(t)
	rounds := []RoundSummary{
		{Seq: 1, Outcome: OutcomeDropout, Dropouts: 1, BlocksRead: 900, BaselineMicros: 4000},
		{Seq: 2, Outcome: OutcomeCancelled, BlocksRead: 300, BaselineMicros: 4000},
	}
	if got := assessHealth(rounds, cfg); got.Rule != RuleRecentDropout {
		t.Errorf("after a dropout and a stopped pass the rule was %q, want %q",
			got.Rule, RuleRecentDropout)
	}
	if got := assessHealth(rounds[1:], cfg); got.Rule != RuleNoData {
		t.Errorf("with only a stopped pass the rule was %q, want %q", got.Rule, RuleNoData)
	}
}
