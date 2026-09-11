package reclaimd

import "time"

// DiskIdentity is what makes a stick the same stick across a re-enumeration
// that renames sda to sdb.
//
// Serial comes from the USB device node (e.g. usb4/4-2), not from the SCSI LUN.
// The SCSI level only echoes the INQUIRY strings, which are byte-identical on
// every unit of a model and therefore useless for telling two sticks apart.
type DiskIdentity struct {
	Key       string `json:"key"`        // stable and filesystem-safe
	VendorID  string `json:"vendor_id"`  // "090c"
	ProductID string `json:"product_id"` // "1000"
	Serial    string `json:"serial"`     // "0011223344556677", may be empty
	Vendor    string `json:"vendor"`     // SCSI INQUIRY, space-trimmed
	Model     string `json:"model"`
	Revision  string `json:"revision"`
	SCSIAddr  string `json:"scsi_addr"` // "0:0:0:0", disambiguates multi-LUN readers
	BusNum    string `json:"bus_num"`
	DevPath   string `json:"usb_dev_path"` // USB topology path, e.g. "2"

	SizeBytes        int64 `json:"size_bytes"`
	LogicalBlockSize int   `json:"logical_block_size"`
	MaxSectorsKB     int   `json:"max_sectors_kb"`

	// KeyIsStable is false when the serial was missing or collided and the key
	// had to be derived from the USB topology path instead. A path key changes
	// when the stick moves to another port; that instability is deliberate,
	// because mistaking one stick for another is worse than losing history.
	KeyIsStable bool `json:"key_is_stable"`
}

// Presence is the live view of where a disk currently sits. Every field here is
// invalidated by a single disconnect, which is exactly why it is kept apart
// from DiskIdentity: the identity survives, the presence does not.
type Presence struct {
	Identity   DiskIdentity `json:"identity"`
	KernelName string       `json:"kernel_name"` // "sda"
	Node       string       `json:"node"`        // "/dev/sda", always built from KernelName
	Major      int          `json:"major"`
	Minor      int          `json:"minor"`
	SysPath    string       `json:"sys_path,omitempty"` // "/sys/block/sda", Linux only
	USBPath    string       `json:"usb_path"`           // "/sys/devices/.../usb4/4-2", or "dev.umass.3"
	SeenAt     time.Time    `json:"seen_at"`

	// Ignored is set for devices found but deliberately not managed, together
	// with the reason code. They are still reported so the UI can explain why
	// a disk is being left alone instead of silently omitting it.
	Ignored       bool   `json:"ignored,omitempty"`
	IgnoredReason string `json:"ignored_reason,omitempty"`
}

// Round outcomes. Worst outcome wins when a round produced several, so that a
// round which saw both slow blocks and a dropout halves the interval rather
// than merely shortening it.
const (
	OutcomeClean       = "clean"
	OutcomeSlow        = "slow"
	OutcomeNearHang    = "near_hang"
	OutcomeDropout     = "dropout"
	OutcomeCancelled   = "cancelled"
	OutcomeExternal    = "aborted_external"
	OutcomeMediaErrors = "media_errors"
)

// RoundSummary is one line of rounds.jsonl.
type RoundSummary struct {
	Seq       uint64    `json:"seq"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Outcome   string    `json:"outcome"`

	// Completed says the sweep reached the end of the ground it set out to
	// cover, as opposed to stopping early on a circuit breaker. The outcome
	// alone cannot say this: a pass that swept the whole disk and found slow
	// blocks and a pass that quit after 8% both come out as "slow".
	Completed bool `json:"completed"`

	BlocksRead     int   `json:"blocks_read_n"`
	BlocksTotal    int   `json:"blocks_total_n"`
	SlowBlocks     int   `json:"slow_blocks_n"`
	DangerBlocks   int   `json:"danger_blocks_n"`
	MediaErrors    int   `json:"media_errors_n"`
	Dropouts       int   `json:"dropouts_n"`
	Deferred       int   `json:"deferred_segments_n"`
	Healed         int   `json:"healed_n"`
	StillSlow      int   `json:"still_slow_n"`
	BaselineMicros int64 `json:"baseline_us"`
	SlowMicros     int64 `json:"slow_threshold_us"`
	DangerMicros   int64 `json:"danger_threshold_us"`
	StartOffset    int64 `json:"start_offset"`
	BytesRead      int64 `json:"bytes_read"`
}

// Event types written to events.jsonl.
const (
	EventDropout   = "DROPOUT"
	EventSlow      = "SLOW"
	EventNearHang  = "NEAR_HANG"
	EventHealed    = "HEALED"
	EventStillSlow = "STILL_SLOW"
	EventMedia     = "MEDIA_ERROR"
	EventDefer     = "DEFER"
	EventRound     = "ROUND"
	EventInterval  = "INTERVAL_CHANGE"
	EventAdopted   = "ADOPTED"
	EventResumed   = "PASS_RESUMED"
	EventOverride  = "COOLDOWN_OVERRIDDEN"
	EventStopped   = "PASS_STOPPED"
)

// Event is one line of events.jsonl. Params carries the structured values the
// browser formats; the suffix on each key (_ms, _ts, _mib, _n, _pct) tells the
// frontend how to render it without any extra metadata.
type Event struct {
	ID       uint64         `json:"id"`
	At       time.Time      `json:"ts"`
	Type     string         `json:"type"`
	Round    uint64         `json:"round,omitempty"`
	Offset   int64          `json:"offset,omitempty"`
	HealsRef uint64         `json:"heals_ref,omitempty"`
	Params   map[string]any `json:"params,omitempty"`
}
