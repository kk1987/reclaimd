package reclaimd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Duration wraps time.Duration for JSON unmarshaling (accepts "10s", "12h").
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch val := v.(type) {
	case float64:
		*d = Duration(time.Duration(val) * time.Second)
	case string:
		dur, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", val, err)
		}
		*d = Duration(dur)
	default:
		return fmt.Errorf("invalid duration type %T", v)
	}
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Config holds every knob in the daemon.
//
// The design intent is that none of these ever need to be set: the scanner
// learns its own thresholds from the disk in front of it, and the scheduler
// derives its own interval from what it finds. They are exposed anyway, with
// the reasoning written down, because a number you cannot see is a number you
// cannot argue with -- and the web UI shows the derived values next to these
// defaults for exactly that reason.
type Config struct {
	// ---- deployment (the only settings that genuinely need a human) ----

	// ListenAddr defaults to loopback. Binding anywhere else requires UIToken,
	// because an unauthenticated status page on a router's LAN interface must
	// not be something you get by accident.
	ListenAddr string `json:"listen_addr"`
	UIToken    string `json:"ui_token"`

	// StateDir must be on persistent storage. On OpenWrt /var is a symlink to
	// /tmp, so the default below is a tmpfs there and the init script has to
	// override it -- see the volatile-state warning in store.go.
	StateDir string `json:"state_dir"`

	LogLevel string `json:"log_level"`

	// ---- adoption ----

	// AdoptAfter keeps transient sticks out of the schedule. Something plugged
	// in to copy a file is gone long before this elapses; something left in a
	// router is not.
	AdoptAfter Duration `json:"adopt_after"`

	// MinUptime stops the daemon scanning while the machine is still booting,
	// when the stick has only just enumerated and everything else is competing
	// for it. Measured on the kernel's uptime clock, never the wall clock.
	MinUptime Duration `json:"min_uptime"`

	// ---- read geometry ----

	// BlockSize is zero by default, meaning it is derived per disk from that
	// disk's max_sectors_kb -- see BlockSizeFor. One read must be exactly one
	// SCSI command, or the latency recorded is the sum of several and the
	// spikes this tool exists to detect get measured against a baseline that
	// has grown by the same factor. The drive in the forensics reports 1 MiB;
	// a USB 2.0 stick behind usb-storage reports 120 KiB, and hardcoding
	// either one is wrong on the other.
	BlockSize int `json:"block_size"`

	// WarmupDiscard blocks are read and thrown away before timing starts. A
	// stick left at power/control=auto with a short autosuspend delay pays a
	// USB resume cost on the first read after an idle gap, which would
	// otherwise be recorded as a slow block -- and poison the baseline it is
	// meant to establish.
	WarmupDiscard int `json:"warmup_discard_n"`
	WarmupBlocks  int `json:"warmup_blocks_n"`

	// ---- thresholds (multipliers on the disk's own learned p50) ----

	// A fixed millisecond threshold would be wrong for every disk but one.
	// Measured against a 10ms p50, these produce 50ms and 500ms -- which is
	// exactly where the forensics put the two interesting populations.
	SlowFactor   float64  `json:"slow_factor"`
	DangerFactor float64  `json:"danger_factor"`
	SlowFloor    Duration `json:"slow_floor"`
	SlowCeil     Duration `json:"slow_ceil"`
	DangerFloor  Duration `json:"danger_floor"`
	DangerCeil   Duration `json:"danger_ceil"`

	// ---- backoff ----

	// SegmentSize is the NAND superblock inferred from the forensics: 90.5% of
	// extreme-latency blocks land on offset mod 32 MiB == 31, the last wordline
	// of an erase block. Backing off in units of the physical layout is what
	// makes a single skip enough.
	SegmentSize int64 `json:"segment_size"`

	// SlowSkipSegments is 1, giving at least 32 MiB of clearance past a slow
	// block. The precursor window measured in the forensics was 10 MiB, so this
	// is roughly 3x safety margin -- and because the extreme blocks sit at the
	// END of a segment anyway, the skip usually costs nothing.
	SlowSkipSegments   int `json:"slow_skip_segments_n"`
	DangerSkipSegments int `json:"danger_skip_segments_n"`

	SlowCooldown   Duration `json:"slow_cooldown"`
	DangerCooldown Duration `json:"danger_cooldown"`

	// ReattachTimeout bounds how long a dropout waits for the stick to come
	// back. Nothing resumes scanning afterwards -- the round is over either
	// way -- so this is about recording the recovery, and on a mounted overlay
	// about answering the only question that matters: did the backing store
	// return at all. Measured re-enumeration is 5-6s; the rest is slack for a
	// SuperSpeed-to-HighSpeed renegotiation plus a second attempt.
	ReattachTimeout Duration `json:"reattach_timeout"`

	// MaxDangerPerRound ends the round early. Three near-hangs means the disk
	// is one read away from taking a mounted overlay down with it.
	MaxDangerPerRound int `json:"max_danger_per_round_n"`

	// ReprobeDelay must clear the controller's read cache. dmesg reports
	// "read cache: enabled" for the drive under test, so an immediate re-read
	// would be served from the buffer and report a healed block that was never
	// touched. The delay doubles as time for the controller's background
	// reclaim to run.
	ReprobeDelay  Duration `json:"reprobe_delay"`
	ReprobeBudget Duration `json:"reprobe_budget"`

	// ReprobeMax caps the re-probe at a measurement rather than a second
	// sweep. Each entry costs five reads -- the block and two either side --
	// and these are issued at the end of a round that just chose to back off,
	// so the cap wants to be small enough that the check is never itself the
	// thing that pushes the disk over.
	ReprobeMax int `json:"reprobe_max_n"`

	// ReprobeSpacing keeps the re-probe phase from becoming a burst of its own.
	// The blocks being revisited are the ones that just misbehaved, so they are
	// the last place to issue back-to-back reads.
	ReprobeSpacing Duration `json:"reprobe_spacing"`

	// ---- duty cycle ----

	// The control law is multiplicative because what it fights is
	// multiplicative: a controller sliding into read-retry gets 50x slower, not
	// 50ms slower. The dead band between the two thresholds is deliberate --
	// without it the controller hunts.
	DutyDriftHigh  float64  `json:"duty_drift_high"`
	DutyDriftLow   float64  `json:"duty_drift_low"`
	DutyUp         float64  `json:"duty_up"`
	DutyDown       float64  `json:"duty_down"`
	DutyFactorInit float64  `json:"duty_factor_init"`
	DutyFactorMax  float64  `json:"duty_factor_max"`
	DutyEvalEvery  int      `json:"duty_eval_every_n"`
	DutyMinSleep   Duration `json:"duty_min_sleep"`
	DutyMaxSleep   Duration `json:"duty_max_sleep"`

	// MaxThroughputMBps is a hard ceiling independent of the controller, so a
	// live overlay always keeps headroom no matter what the drift signal says.
	MaxThroughputMBps float64 `json:"max_throughput_mbps"`

	// ---- external I/O yielding ----

	// "Stop on any foreign I/O" would deadlock on a router, where the overlay
	// is written to continuously. These are rate thresholds, not presence tests.
	ForeignWriteIOPS float64  `json:"foreign_write_iops"`
	ForeignReadMBps  float64  `json:"foreign_read_mbps"`
	YieldMax         Duration `json:"yield_max"`
	YieldGiveUp      Duration `json:"yield_give_up"`

	// ---- scheduling ----

	ScanIntervalInit Duration `json:"scan_interval_init"`
	ScanIntervalMin  Duration `json:"scan_interval_min"`
	ScanIntervalMax  Duration `json:"scan_interval_max"`
	FactorClean      float64  `json:"factor_clean"`
	FactorSlow       float64  `json:"factor_slow"`
	FactorDanger     float64  `json:"factor_danger"`

	// SuppressAfterDropout is long because a dropout on a mounted overlay is
	// expensive and the controller needs uninterrupted idle time to finish
	// reclaiming whatever we just tripped over.
	SuppressAfterDropout  Duration `json:"suppress_after_dropout"`
	SuppressAfterNearHang Duration `json:"suppress_after_near_hang"`

	// ---- retention ----

	KeepFullProfiles   int `json:"keep_full_profiles_n"`
	KeepCoarseProfiles int `json:"keep_coarse_profiles_n"`
}

func (c *Config) withDefaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = "127.0.0.1:8099"
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.AdoptAfter == 0 {
		c.AdoptAfter = Duration(30 * time.Minute)
	}
	if c.MinUptime == 0 {
		c.MinUptime = Duration(15 * time.Minute)
	}
	// BlockSize deliberately keeps its zero: it is resolved per disk, once the
	// disk is in front of us. See BlockSizeFor.
	if c.WarmupDiscard == 0 {
		c.WarmupDiscard = 8
	}
	if c.WarmupBlocks == 0 {
		c.WarmupBlocks = 256
	}
	if c.SlowFactor == 0 {
		c.SlowFactor = 5
	}
	if c.DangerFactor == 0 {
		c.DangerFactor = 50
	}
	if c.SlowFloor == 0 {
		c.SlowFloor = Duration(50 * time.Millisecond)
	}
	if c.SlowCeil == 0 {
		c.SlowCeil = Duration(250 * time.Millisecond)
	}
	if c.DangerFloor == 0 {
		c.DangerFloor = Duration(400 * time.Millisecond)
	}
	if c.DangerCeil == 0 {
		c.DangerCeil = Duration(1200 * time.Millisecond)
	}
	if c.SegmentSize == 0 {
		c.SegmentSize = 32 << 20
	}
	if c.SlowSkipSegments == 0 {
		c.SlowSkipSegments = 1
	}
	if c.DangerSkipSegments == 0 {
		c.DangerSkipSegments = 7
	}
	if c.SlowCooldown == 0 {
		c.SlowCooldown = Duration(1500 * time.Millisecond)
	}
	if c.DangerCooldown == 0 {
		c.DangerCooldown = Duration(5 * time.Second)
	}
	if c.MaxDangerPerRound == 0 {
		c.MaxDangerPerRound = 3
	}
	if c.ReattachTimeout == 0 {
		c.ReattachTimeout = Duration(120 * time.Second)
	}
	if c.ReprobeDelay == 0 {
		c.ReprobeDelay = Duration(10 * time.Minute)
	}
	if c.ReprobeBudget == 0 {
		c.ReprobeBudget = Duration(5 * time.Minute)
	}
	if c.ReprobeMax == 0 {
		c.ReprobeMax = 16
	}
	if c.ReprobeSpacing == 0 {
		c.ReprobeSpacing = Duration(500 * time.Millisecond)
	}
	if c.DutyDriftHigh == 0 {
		c.DutyDriftHigh = 1.25
	}
	if c.DutyDriftLow == 0 {
		c.DutyDriftLow = 1.10
	}
	if c.DutyUp == 0 {
		c.DutyUp = 1.30
	}
	if c.DutyDown == 0 {
		c.DutyDown = 0.85
	}
	if c.DutyFactorInit == 0 {
		c.DutyFactorInit = 0.25
	}
	if c.DutyFactorMax == 0 {
		c.DutyFactorMax = 8
	}
	if c.DutyEvalEvery == 0 {
		c.DutyEvalEvery = 64
	}
	if c.DutyMinSleep == 0 {
		c.DutyMinSleep = Duration(20 * time.Millisecond)
	}
	if c.DutyMaxSleep == 0 {
		c.DutyMaxSleep = Duration(200 * time.Millisecond)
	}
	if c.MaxThroughputMBps == 0 {
		c.MaxThroughputMBps = 60
	}
	if c.ForeignWriteIOPS == 0 {
		c.ForeignWriteIOPS = 8
	}
	if c.ForeignReadMBps == 0 {
		c.ForeignReadMBps = 2
	}
	if c.YieldMax == 0 {
		c.YieldMax = Duration(15 * time.Second)
	}
	if c.YieldGiveUp == 0 {
		c.YieldGiveUp = Duration(10 * time.Minute)
	}
	if c.ScanIntervalInit == 0 {
		c.ScanIntervalInit = Duration(7 * 24 * time.Hour)
	}
	if c.ScanIntervalMin == 0 {
		c.ScanIntervalMin = Duration(12 * time.Hour)
	}
	if c.ScanIntervalMax == 0 {
		c.ScanIntervalMax = Duration(30 * 24 * time.Hour)
	}
	if c.FactorClean == 0 {
		c.FactorClean = 1.5
	}
	if c.FactorSlow == 0 {
		c.FactorSlow = 0.7
	}
	if c.FactorDanger == 0 {
		c.FactorDanger = 0.5
	}
	if c.SuppressAfterDropout == 0 {
		c.SuppressAfterDropout = Duration(24 * time.Hour)
	}
	if c.SuppressAfterNearHang == 0 {
		c.SuppressAfterNearHang = Duration(6 * time.Hour)
	}
	if c.KeepFullProfiles == 0 {
		c.KeepFullProfiles = 12
	}
	if c.KeepCoarseProfiles == 0 {
		c.KeepCoarseProfiles = 104
	}
}

func (c *Config) withEnvOverrides() {
	envStr("RECLAIMD_LISTEN_ADDR", &c.ListenAddr)
	envStr("RECLAIMD_STATE_DIR", &c.StateDir)
	envStr("RECLAIMD_LOG_LEVEL", &c.LogLevel)
	envStr("RECLAIMD_UI_TOKEN", &c.UIToken)
	envInt("RECLAIMD_BLOCK_SIZE", &c.BlockSize)
	envFloat("RECLAIMD_MAX_THROUGHPUT_MBPS", &c.MaxThroughputMBps)
	envDur("RECLAIMD_ADOPT_AFTER", &c.AdoptAfter)
	envDur("RECLAIMD_SCAN_INTERVAL_INIT", &c.ScanIntervalInit)
}

func envStr(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func envInt(key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func envFloat(key string, dst *float64) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			*dst = n
		}
	}
}

func envDur(key string, dst *Duration) {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*dst = Duration(d)
		}
	}
}

// LoadConfigFromFile reads an optional JSON config, then applies defaults and
// environment overrides. An empty path is not an error: the whole point is that
// the daemon runs correctly with no configuration at all.
func LoadConfigFromFile(path string) (Config, error) {
	var c Config
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	c.withDefaults()
	c.withEnvOverrides()
	return c, c.validate()
}

func (c Config) validate() error {
	// Zero means derive. A set value has to be a power of two, not merely a
	// multiple of 4096: the latency map encodes the block size as a shift, so
	// anything else is rejected only at the end of the first round -- after a
	// full pass has already been spent measuring with it.
	if c.BlockSize != 0 {
		if c.BlockSize < 4096 || c.BlockSize > MaxBlockSize || c.BlockSize&(c.BlockSize-1) != 0 {
			return fmt.Errorf("block_size must be a power of two between 4096 and %d, got %d",
				MaxBlockSize, c.BlockSize)
		}
		if c.SegmentSize <= 0 || c.SegmentSize%int64(c.BlockSize) != 0 {
			return fmt.Errorf("segment_size must be a positive multiple of block_size")
		}
	} else if c.SegmentSize <= 0 || c.SegmentSize%MaxBlockSize != 0 {
		// With block_size derived, the value is not known yet. Requiring the
		// segment to be a multiple of the largest size it can take makes it
		// divisible by every size it can take.
		return fmt.Errorf("segment_size must be a positive multiple of %d", MaxBlockSize)
	}
	if c.ScanIntervalMin >= c.ScanIntervalMax {
		return fmt.Errorf("scan_interval_min must be below scan_interval_max")
	}
	return nil
}

// MaxBlockSize caps the derived read size. It is the point past which a larger
// read buys nothing: it is already several times any transfer limit seen in the
// wild, and every doubling beyond it halves the resolution of the latency map
// for no gain in measurement fidelity.
const MaxBlockSize = 1 << 20

// BlockSizeFor is the read size to use on one particular disk.
//
// The kernel splits any read larger than max_sectors_kb into several SCSI
// commands, so a block above that limit is timed as the sum of a handful of
// device operations. That does not hide a spike -- the sum still contains it --
// but it raises the disk's own p50 by the same factor, and the thresholds are
// multiples of that p50. On a stick whose limit is 120 KiB, a 1 MiB block puts
// the slow line at ~150ms instead of the floor of 50ms, and the 50ms population
// that the whole tool was written to catch disappears under the threshold.
//
// So: the largest power of two that still fits in one command. Power of two
// because the latency map stores the size as a shift, and the next size down
// costs at most a factor of two in throughput per command -- cheap next to
// measuring the wrong thing.
func (c Config) BlockSizeFor(id DiskIdentity) int {
	if c.BlockSize > 0 {
		return c.BlockSize
	}
	// An unknown limit is not a reason to read small; sysfs virtually always
	// has it, and the old fixed 1 MiB is the right guess when it does not.
	if id.MaxSectorsKB <= 0 {
		return MaxBlockSize
	}
	size := 4096
	for size*2 <= id.MaxSectorsKB*1024 && size*2 <= MaxBlockSize {
		size *= 2
	}
	return size
}

// ForDisk resolves everything that depends on the disk in front of us, so the
// rest of a round can go on reading plain config fields.
func (c Config) ForDisk(id DiskIdentity) Config {
	c.BlockSize = c.BlockSizeFor(id)
	return c
}

// BlocksPerSegment is used everywhere the scanner reasons about the physical
// layout; deriving it once keeps the mod-32 structure analysis honest. It is
// only meaningful on a config that has been through ForDisk.
func (c Config) BlocksPerSegment() int {
	if c.BlockSize <= 0 {
		return 0
	}
	return int(c.SegmentSize / int64(c.BlockSize))
}
