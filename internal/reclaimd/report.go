package reclaimd

import (
	"math"
	"time"
)

// Health grades and the rules that produce them.
//
// The grades come from a rule ladder. A weighted score would need coefficients
// nobody can defend, and it would answer "how bad" when the question is "what
// should I do". Each grade names the single rule that fired, and the UI prints
// that rule, so the verdict can be argued with.
const (
	GradeUnknown  = "unknown"
	GradeGood     = "good"
	GradeWatch    = "watch"
	GradeDegraded = "degraded"
	GradeFailing  = "failing"
)

const (
	RuleNoData          = "NO_COMPLETE_PASS"
	RuleRecentDropout   = "RECENT_DROPOUT"
	RuleRepeatDropouts  = "REPEATED_DROPOUTS"
	RuleNearHang        = "NEAR_HANG_PRESENT"
	RuleSlowOverBudget  = "SLOW_OVER_BUDGET"
	RuleSlowUnderBudget = "SLOW_PRESENT_UNDER_THRESHOLD"
	RuleBaselineDrift   = "BASELINE_DRIFT"
	RuleClean           = "LAST_PASS_CLEAN"
)

// Health is the verdict plus the rule and the numbers that produced it.
type Health struct {
	Grade  string         `json:"grade"`
	Rule   string         `json:"rule"`
	Params map[string]any `json:"params,omitempty"`
}

// Controller is one row of the web UI's decision desk: what the value is now,
// why it is that, and what the next round could turn it into.
type Controller struct {
	ID      string         `json:"id"`
	Value   any            `json:"value"`
	Unit    string         `json:"unit"`
	Reason  string         `json:"reason"`
	Params  map[string]any `json:"params,omitempty"`
	Formula string         `json:"formula,omitempty"`
	Bounds  map[string]any `json:"bounds,omitempty"`
	Clamped bool           `json:"clamped,omitempty"`
	WhatIf  map[string]any `json:"whatif,omitempty"`
}

// DiskView is the whole per-disk payload the browser renders.
type DiskView struct {
	Key         string       `json:"key"`
	Identity    DiskIdentity `json:"identity"`
	Present     bool         `json:"present"`
	KernelName  string       `json:"kernel_name,omitempty"`
	Enabled     bool         `json:"enabled"`
	Adopted     bool         `json:"adopted"`
	AdoptedAtTs int64        `json:"adopted_at_ts,omitempty"`
	Scanning    bool         `json:"scanning"`
	Health      Health       `json:"health"`
	NextScanTs  int64        `json:"next_scan_ts,omitempty"`
	LastScanTs  int64        `json:"last_scan_ts,omitempty"`
	SuppressTs  int64        `json:"suppress_until_ts,omitempty"`
	// ScanRequested is a Scan now the daemon has accepted and not yet started.
	// The daemon reports it because a page that only remembered it would offer
	// the button again after a reload, for a round that is already on its way.
	ScanRequested bool `json:"scan_requested"`
	// Stopping is a Stop the running round has been given and not yet acted on.
	// Scanning stays true until it has, and a reload in between has to show the
	// round winding down instead of offering Stop again.
	Stopping bool `json:"stopping"`
	// SpeedTesting is a speed test in flight, for the page to hold the button
	// down and the scheduler to leave the disk alone. The three plan fields
	// are what a test would do, so the page can say so before one has run.
	SpeedTesting   bool    `json:"speed_testing"`
	SpeedRegionsN  int     `json:"speed_regions_n,omitempty"`
	SpeedRegionMiB int     `json:"speed_region_mib,omitempty"`
	SpeedBudgetS   float64 `json:"speed_budget_s,omitempty"`
	// LastOutcome is what the previous round graded, which says whether a
	// cooldown in progress is the six-hour kind or the twenty-four-hour kind.
	// The difference is worth showing before anyone is offered the chance to
	// skip it.
	LastOutcome string         `json:"last_outcome,omitempty"`
	Controllers []Controller   `json:"controllers,omitempty"`
	Rounds      []RoundSummary `json:"rounds,omitempty"`
	Live        *LiveProgress  `json:"live,omitempty"`
	LastErr     string         `json:"last_error,omitempty"`

	TotalHealed  int   `json:"healed_total_n"`
	BytesWritten int64 `json:"bytes_written_by_tool"`
	// OldestDataS is how long ago the least recently read region was last
	// touched by this tool. The overlay's own file reads refresh data too but
	// are invisible from the raw device, so this is a lower bound on freshness.
	OldestDataS float64 `json:"oldest_data_s,omitempty"`
	// IntervalS lets the freshness map key its colours to this disk's own
	// current interval instead of absolute days, so the map keeps reading as
	// "are we behind?" no matter how the schedule has adapted.
	IntervalS float64 `json:"interval_s,omitempty"`
}

// assessHealth walks the ladder from worst to best and stops at the first rule
// that fires.
func assessHealth(rounds []RoundSummary, cfg Config) Health {
	// A cancelled pass, whether stopped from the page or cut off by a shutdown,
	// covered part of the disk at best, and judging by it put "clean end to
	// end" on a drive whose last real pass had dropped off the bus. It is left
	// out here for the reason the scheduler leaves it out of the interval.
	var judged []RoundSummary
	for _, r := range rounds {
		if r.Outcome != OutcomeCancelled {
			judged = append(judged, r)
		}
	}
	rounds = judged
	if len(rounds) == 0 {
		return Health{Grade: GradeUnknown, Rule: RuleNoData}
	}
	last := rounds[len(rounds)-1]

	if last.Dropouts > 0 {
		return Health{Grade: GradeFailing, Rule: RuleRecentDropout,
			Params: map[string]any{"drop_n": last.Dropouts, "seq_n": last.Seq}}
	}
	recent := rounds
	if len(recent) > 3 {
		recent = recent[len(recent)-3:]
	}
	drops := 0
	for _, r := range recent {
		drops += r.Dropouts
	}
	if drops > 2 {
		return Health{Grade: GradeFailing, Rule: RuleRepeatDropouts,
			Params: map[string]any{"drop_n": drops, "passes_n": len(recent)}}
	}
	if last.DangerBlocks > 0 {
		return Health{Grade: GradeDegraded, Rule: RuleNearHang,
			Params: map[string]any{"danger_n": last.DangerBlocks,
				"danger_threshold_ms": float64(last.DangerMicros) / 1000}}
	}
	if last.BlocksRead > 0 {
		frac := float64(last.SlowBlocks) / float64(last.BlocksRead)
		if frac > 0.001 {
			return Health{Grade: GradeDegraded, Rule: RuleSlowOverBudget,
				Params: map[string]any{"slow_n": last.SlowBlocks,
					"read_n": last.BlocksRead, "slow_pct": round2(frac)}}
		}
	}
	if last.SlowBlocks > 0 {
		return Health{Grade: GradeWatch, Rule: RuleSlowUnderBudget,
			Params: map[string]any{"slow_n": last.SlowBlocks,
				"danger_threshold_ms": float64(last.DangerMicros) / 1000}}
	}
	if drift, base, now := baselineDrift(rounds); drift > 0.20 {
		return Health{Grade: GradeWatch, Rule: RuleBaselineDrift,
			Params: map[string]any{"drift_pct": round2(drift),
				"best_ms": round2(base), "now_ms": round2(now)}}
	}
	return Health{Grade: GradeGood, Rule: RuleClean,
		Params: map[string]any{"seq_n": last.Seq}}
}

// baselineDrift compares the newest baseline against the best ever seen. A disk
// whose floor is rising is aging even when no single block trips a threshold.
// That is the slow signal the per-block checks cannot see.
func baselineDrift(rounds []RoundSummary) (drift, bestMs, nowMs float64) {
	best := math.MaxFloat64
	for _, r := range rounds {
		if r.BaselineMicros <= 0 {
			continue
		}
		ms := float64(r.BaselineMicros) / 1000
		if ms < best {
			best = ms
		}
	}
	last := rounds[len(rounds)-1]
	if best == math.MaxFloat64 || last.BaselineMicros <= 0 {
		return 0, 0, 0
	}
	nowMs = float64(last.BaselineMicros) / 1000
	return (nowMs - best) / best, best, nowMs
}

// controllersFor renders every adaptive parameter as a row the user can argue
// with: the value, the rule that set it, the formula, and what the next round
// would turn it into. The last is what makes the policy legible.
func controllersFor(sched Schedule, rounds []RoundSummary, cfg Config, id DiskIdentity,
	live *LiveProgress) []Controller {
	out := []Controller{{
		ID:      "scan_interval",
		Value:   round2(sched.Interval.Duration().Hours()),
		Unit:    "h",
		Reason:  orDefault(sched.LastReason, ReasonInit),
		Params:  sched.LastReasonParams,
		Formula: "clamp(prev * factor, 12h, 30d)",
		Bounds: map[string]any{
			"min_h": cfg.ScanIntervalMin.Duration().Hours(),
			"max_h": cfg.ScanIntervalMax.Duration().Hours(),
		},
		WhatIf: sched.WhatIf(cfg),
	}}

	// A read larger than the transfer limit is timed as several commands, so
	// this row exists to show that it is not, and to say so plainly when a
	// hand-set block_size has made it one.
	readSize := cfg.BlockSizeFor(id)
	readReason := "ONE_SCSI_COMMAND"
	if cfg.BlockSize > 0 {
		readReason = "SET_IN_CONFIG"
	}
	out = append(out, Controller{
		ID:      "read_size",
		Value:   float64(readSize) / 1024,
		Unit:    "KiB",
		Reason:  readReason,
		Formula: "largest power of two <= the kernel's per-command limit, capped at 1 MiB",
		Params: map[string]any{
			"max_sectors_kb": id.MaxSectorsKB,
			"split":          id.MaxSectorsKB > 0 && readSize > id.MaxSectorsKB*1024,
		},
	})

	if len(rounds) > 0 {
		last := rounds[len(rounds)-1]
		out = append(out, Controller{
			ID:      "thresholds",
			Value:   round2(float64(last.BaselineMicros) / 1000),
			Unit:    "ms",
			Reason:  "LEARNED_FROM_DISK",
			Formula: "slow = clamp(5 * p50, 50ms, 250ms); danger = clamp(50 * p50, 400ms, 1200ms)",
			Params: map[string]any{
				"base_ms":   round2(float64(last.BaselineMicros) / 1000),
				"slow_ms":   round2(float64(last.SlowMicros) / 1000),
				"danger_ms": round2(float64(last.DangerMicros) / 1000),
			},
		})
	}

	if live != nil {
		out = append(out, Controller{
			ID:      "duty_cycle",
			Value:   live.DriftN,
			Unit:    "x",
			Reason:  "DRIFT_" + live.Duty,
			Formula: "drift = rolling_p50 / round_p50; rest *= 1.30 above 1.25, *= 0.85 below 1.10",
			Params: map[string]any{
				"drift_n":     live.DriftN,
				"threshold_n": cfg.DutyDriftHigh,
				"lat_p50_ms":  live.LatP50Ms,
				"base_p50_ms": live.BaseP50Ms,
			},
		})
	}

	// The cooldown has two causes with two lengths, 6h after a near-hang and 24h
	// after a dropout, and this row used to report both as a dropout. Telling
	// somebody their drive fell off the bus when it did not is a bad mistake for
	// a diagnostic to make: it is a hardware event they will go looking for in
	// dmesg and not find.
	suppressed := time.Now().Before(sched.SuppressUntil)
	sup := Controller{ID: "cooldown", Unit: "h", Reason: "NOT_TRIGGERED", Value: 0}
	if suppressed {
		sup.Value = round2(time.Until(sched.SuppressUntil).Hours())
		sup.Reason = "SUPPRESSED_AFTER_NEAR_HANG"
		if sched.LastOutcome == OutcomeDropout {
			sup.Reason = "SUPPRESSED_AFTER_DROPOUT"
		}
		sup.Params = map[string]any{
			"until_ts": sched.SuppressUntil.Unix(),
			"outcome":  sched.LastOutcome,
		}
	}
	out = append(out, sup)
	out = append(out, rewriteController(id.Key, rounds, cfg))

	return out
}

// Rewrite reason codes, one per state the row can be in.
const (
	ReasonRewriteNoEvidence = "REWRITE_NO_EVIDENCE"
	ReasonRewriteReadsHeal  = "REWRITE_READS_HEAL"
	ReasonRewriteNeededOff  = "REWRITE_NEEDED_OFF"
	ReasonRewriteActive     = "REWRITE_ACTIVE"
)

// rewriteController is the row that says whether reading is enough for this
// drive, on what evidence, and what the daemon is doing about it if not. The
// value is the share of re-probed blocks that were still slow, which is the
// number the verdict turns on.
func rewriteController(key string, rounds []RoundSummary, cfg Config) Controller {
	ev := assessHealing(rounds, cfg.Rewrite)
	c := Controller{
		ID:      "rewrite",
		Value:   round2(ev.StillSlowFraction()),
		Unit:    "pct",
		Formula: "still_slow / (healed + still_slow) over the last 6 passes that re-probed anything",
		Params: map[string]any{
			"samples_n":      ev.Samples(),
			"healed_n":       ev.Healed,
			"still_slow_n":   ev.StillSlow,
			"still_slow_pct": round2(ev.StillSlowFraction()),
			"rounds_n":       ev.Rounds,
			"min_samples_n":  cfg.Rewrite.MinSamples,
			"min_rounds_n":   cfg.Rewrite.MinRounds,
			"threshold_pct":  cfg.Rewrite.StillSlowFraction,
		},
	}
	switch {
	case ev.Verdict == HealUnknown:
		c.Reason = ReasonRewriteNoEvidence
	case ev.Verdict == HealByRead:
		c.Reason = ReasonRewriteReadsHeal
	case !cfg.RewriteWanted(key):
		c.Reason = ReasonRewriteNeededOff
	default:
		c.Reason = ReasonRewriteActive
		// The most recent pass that wrote anything says what the writing did.
		for i := len(rounds) - 1; i >= 0; i-- {
			if rounds[i].Rewritten > 0 {
				c.Params["rewritten_n"] = rounds[i].Rewritten
				c.Params["rewrite_healed_n"] = rounds[i].RewriteHealed
				c.Params["seq_n"] = rounds[i].Seq
				break
			}
		}
		if _, ok := c.Params["rewritten_n"]; !ok {
			c.Params["rewritten_n"] = 0
			c.Params["rewrite_healed_n"] = 0
		}
	}
	return c
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
