package reclaimd

import (
	"testing"
	"time"
)

// A drive whose steady-state latency simply sits above its cold opening
// baseline must not be throttled into the ground.
//
// This is what a real full pass did: baseline learned cold at 7.58 ms, the rest
// of the drive reading at about 10 ms, drift pinned at 1.33 and never dropping
// under the 1.10 that would relax the controller. The factor climbed on every
// evaluation until it hit its clamp, and a scan that should have taken ten
// minutes took sixty-three at 16 MiB/s against a 99 MiB/s line rate.
func TestDutyDoesNotWindUpOnSteadyStateOffset(t *testing.T) {
	cfg := mustConfig(t)
	c := NewDutyController(cfg)

	const baseline = 7580 * time.Microsecond // learned cold
	const steady = 10100 * time.Microsecond  // what the drive actually does warm

	for i := 0; i < 400; i++ {
		c.Evaluate(steady, baseline)
	}
	if c.Factor() >= cfg.DutyFactorMax {
		t.Fatalf("rest factor reached its clamp (%.2f); a constant offset between "+
			"the cold baseline and the steady state must not ratchet forever",
			c.Factor())
	}
	// Duty cycle = 1/(1+factor). Anything past ~4 means resting more than 80%
	// of the time for a drive that is behaving perfectly well.
	if c.Factor() > 4 {
		t.Errorf("rest factor %.2f throttles a healthy drive to %.0f%% duty",
			c.Factor(), 100/(1+c.Factor()))
	}
	if c.Reference() == 0 {
		t.Error("duty reference was never taken; it should settle to the warm value")
	}
	if c.Reference() != steady {
		t.Errorf("reference = %v, want the settled %v rather than the cold baseline",
			c.Reference(), steady)
	}
}

// The controller still has to react to a real excursion -- a drive heating up,
// or a sweep entering a degraded region -- which is the entire reason it exists.
func TestDutyStillBacksOffOnRealDrift(t *testing.T) {
	cfg := mustConfig(t)
	c := NewDutyController(cfg)

	const steady = 10 * time.Millisecond
	for i := 0; i < settleEvals+2; i++ {
		c.Evaluate(steady, steady)
	}
	before := c.Factor()

	// Latency climbs and keeps climbing, the way it does on a heating device.
	lat := steady
	for i := 0; i < 20; i++ {
		lat += 400 * time.Microsecond
		c.Evaluate(lat, steady)
	}
	if c.Factor() <= before {
		t.Fatalf("factor %.2f did not rise above %.2f while latency was climbing",
			c.Factor(), before)
	}
	if c.Drift() <= cfg.DutyDriftHigh {
		t.Errorf("drift %.2f should have exceeded the %.2f threshold",
			c.Drift(), cfg.DutyDriftHigh)
	}
}

// Once the excursion passes, the controller has to give the throughput back.
func TestDutyRelaxesWhenLatencyRecovers(t *testing.T) {
	cfg := mustConfig(t)
	c := NewDutyController(cfg)

	const steady = 10 * time.Millisecond
	for i := 0; i < settleEvals+2; i++ {
		c.Evaluate(steady, steady)
	}
	for i := 0; i < 10; i++ {
		c.Evaluate(15*time.Millisecond, steady)
	}
	hot := c.Factor()
	for i := 0; i < 30; i++ {
		c.Evaluate(9*time.Millisecond, steady)
	}
	if c.Factor() >= hot {
		t.Errorf("factor stayed at %.2f after latency recovered (was %.2f while hot)",
			c.Factor(), hot)
	}
}
