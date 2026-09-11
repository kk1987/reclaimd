package reclaimd

import "errors"

// ---------------------------------------------------------------------------
// Error codes
//
// These are the vocabulary the HTTP API speaks. The daemon never emits a
// natural-language sentence: it emits one of these codes plus structured
// parameters, and the browser renders it into Chinese or English. Keeping the
// boundary here is what makes the UI translatable without a translation layer
// in Go.
// ---------------------------------------------------------------------------

const (
	CodeDeviceNotFound     = "DEVICE_NOT_FOUND"
	CodeDeviceDisconnected = "DEVICE_DISCONNECTED"
	CodeDeviceNotReady     = "DEVICE_NOT_READY"
	CodeDeviceMounted      = "DEVICE_MOUNTED"
	CodeIdentityMismatch   = "IDENTITY_MISMATCH"
	CodeAmbiguousIdentity  = "AMBIGUOUS_IDENTITY"
	CodeAlignmentError     = "ALIGNMENT_ERROR"
	CodeMediaError         = "MEDIA_ERROR"
	CodeRoundAborted       = "ROUND_ABORTED"
	CodeScanSuppressed     = "SCAN_SUPPRESSED"
	CodeScanInProgress     = "SCAN_IN_PROGRESS"
	CodeNotScanning        = "NOT_SCANNING"
	CodeExternalIOBusy     = "EXTERNAL_IO_BUSY"
	CodeStateCorrupt       = "STATE_CORRUPT"
	CodeStateUnsupported   = "STATE_SCHEMA_UNSUPPORTED"
	CodeStateVolatile      = "STATE_VOLATILE"
	CodeClockUnsynced      = "CLOCK_UNSYNCED"
	CodeConfirmMismatch    = "CONFIRMATION_MISMATCH"
	CodeLockHeld           = "LOCK_HELD"
	CodeRootDevice         = "IGNORED_ROOT_DEVICE"
	CodeNotRemovable       = "IGNORED_NOT_REMOVABLE"
	CodeNoSerial           = "IGNORED_NO_SERIAL"
	CodeInternalError      = "INTERNAL_ERROR"
)

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

var (
	// ErrDeviceDisconnected means the block device left the bus. On a stick
	// that is carrying a mounted overlay this is the expensive failure: the
	// filesystem lost its backing store and the round must stop immediately.
	ErrDeviceDisconnected = errors.New("device disconnected")

	// ErrIdentityMismatch means something came back under the same key but is
	// not the same hardware. Refusing to scan is the only safe response.
	ErrIdentityMismatch = errors.New("device identity mismatch")

	// ErrAlignment is always our own bug, never the device's. It is kept
	// separate from ErrMediaError precisely so an O_DIRECT mistake cannot hide
	// for months disguised as a mysterious flaky stick.
	ErrAlignment = errors.New("o_direct alignment violation")

	// ErrMediaError is a read that failed while the device stayed present.
	ErrMediaError = errors.New("media read error")

	ErrRoundAborted   = errors.New("scan round aborted")
	ErrScanSuppressed = errors.New("scan suppressed")
	ErrScanInProgress = errors.New("scan already running")
	ErrNotScanning    = errors.New("no scan running")
	ErrDeviceMounted  = errors.New("device is mounted")
	ErrNotFound       = errors.New("disk not found")
)

// ErrStateCorrupt and ErrStateUnsupported separate "this file is damaged" from
// "this file is from a newer version". The second one must never be parsed
// optimistically: reading a future schema with today's code and then writing it
// back is how state gets destroyed rather than merely misread.
var (
	ErrStateCorrupt     = errors.New("state file corrupt")
	ErrStateUnsupported = errors.New("state schema unsupported")
)

// ErrLockHeld means another reclaimd owns the state directory.
var ErrLockHeld = errors.New("state directory locked by another process")

// ErrExternalBusy means somebody else used the disk for the whole round. It is
// neutral news: nothing is wrong with the disk, and nothing was learned about
// it either, so the scheduler must not move the interval in either direction.
var ErrExternalBusy = errors.New("external i/o busy")

// ErrConfirmMismatch means the operator did not supply the target's serial.
var ErrConfirmMismatch = errors.New("confirmation does not match target")
