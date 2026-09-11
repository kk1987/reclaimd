package reclaimd

import (
	"encoding/binary"
	"fmt"
	"time"
)

// LatencyUnit is the quantum of a stored latency sample.
//
// 32us was fitted to the measured distribution, which is why it is not a round
// number. One uint16 then covers 0..2.097s, which brackets both the 10ms normal
// read and the 1500-1800ms controller hang with room to spare, while still
// resolving 0.3% at the 10ms baseline. Milliseconds would throw away everything
// below 1ms of variation. Microseconds would top out at 65ms and clip every
// event this tool exists to record.
const LatencyUnit = 32 * time.Microsecond

// Sentinels occupy the top of the range. "Skipped" has to stay distinct from
// "fast": a round that backed off a lot is mostly skipped blocks, and drawing
// those in the fast colour would make a deteriorating disk look steadily
// healthier.
const (
	LatSkipped uint16 = 0xFFFF // never read: backed off, deferred, round ended
	LatError   uint16 = 0xFFFE // read returned an error
	LatMax     uint16 = 0xFFFD // clipped, >= 2.097s
)

func EncodeLatency(d time.Duration) uint16 {
	if d < 0 {
		return 0
	}
	v := d / LatencyUnit
	if v >= time.Duration(LatMax) {
		return LatMax
	}
	return uint16(v)
}

// DecodeLatency returns the sample and whether it is a real measurement.
func DecodeLatency(v uint16) (time.Duration, bool) {
	switch v {
	case LatSkipped, LatError:
		return 0, false
	default:
		return time.Duration(v) * LatencyUnit, true
	}
}

const (
	latMagic     = "RCLM"
	latVersion   = 1
	latHeaderLen = 64
)

// LatencyMap is one round's full-resolution profile: one uint16 per block.
// For a 64 GB drive at 1 MiB blocks that is 61184 samples, 122,432 bytes.
type LatencyMap struct {
	BlockSize  int
	BlockCount uint32
	RoundSeq   uint64
	StartedAt  time.Time
	Baseline   time.Duration
	Values     []uint16
}

func NewLatencyMap(blockSize int, blockCount int64, seq uint64, started time.Time) *LatencyMap {
	v := make([]uint16, blockCount)
	for i := range v {
		v[i] = LatSkipped
	}
	return &LatencyMap{
		BlockSize:  blockSize,
		BlockCount: uint32(blockCount),
		RoundSeq:   seq,
		StartedAt:  started,
		Values:     v,
	}
}

func (m *LatencyMap) MarshalBinary() ([]byte, error) {
	shift := 0
	for s := m.BlockSize; s > 1; s >>= 1 {
		shift++
	}
	if 1<<shift != m.BlockSize {
		return nil, fmt.Errorf("block size %d is not a power of two", m.BlockSize)
	}
	out := make([]byte, latHeaderLen+len(m.Values)*2)
	copy(out[0:4], latMagic)
	binary.LittleEndian.PutUint16(out[4:6], latVersion)
	out[6] = byte(shift)
	binary.LittleEndian.PutUint32(out[8:12], m.BlockCount)
	binary.LittleEndian.PutUint64(out[12:20], m.RoundSeq)
	binary.LittleEndian.PutUint64(out[20:28], uint64(m.StartedAt.Unix()))
	binary.LittleEndian.PutUint32(out[28:32], uint32(m.Baseline/time.Microsecond))
	for i, v := range m.Values {
		binary.LittleEndian.PutUint16(out[latHeaderLen+i*2:], v)
	}
	return out, nil
}

func (m *LatencyMap) UnmarshalBinary(b []byte) error {
	if len(b) < latHeaderLen {
		return fmt.Errorf("%w: latency map truncated at %d bytes", ErrStateCorrupt, len(b))
	}
	if string(b[0:4]) != latMagic {
		return fmt.Errorf("%w: bad magic %q", ErrStateCorrupt, b[0:4])
	}
	if v := binary.LittleEndian.Uint16(b[4:6]); v != latVersion {
		return fmt.Errorf("%w: latency map version %d", ErrStateUnsupported, v)
	}
	m.BlockSize = 1 << b[6]
	m.BlockCount = binary.LittleEndian.Uint32(b[8:12])
	m.RoundSeq = binary.LittleEndian.Uint64(b[12:20])
	m.StartedAt = time.Unix(int64(binary.LittleEndian.Uint64(b[20:28])), 0)
	m.Baseline = time.Duration(binary.LittleEndian.Uint32(b[28:32])) * time.Microsecond

	want := latHeaderLen + int(m.BlockCount)*2
	if len(b) < want {
		return fmt.Errorf("%w: want %d bytes for %d blocks, got %d",
			ErrStateCorrupt, want, m.BlockCount, len(b))
	}
	m.Values = make([]uint16, m.BlockCount)
	for i := range m.Values {
		m.Values[i] = binary.LittleEndian.Uint16(b[latHeaderLen+i*2:])
	}
	return nil
}

// Coarse downsamples to one record per segment for long-term history, taking
// the worst sample in each bin.
//
// A mean would be the wrong choice. A segment holds blocksPerSegment reads, 32
// at 1 MiB blocks and 512 at 64 KiB, and a mean over that many ordinary ones
// drags a single near-hang down to somewhere near the baseline. The outlier is
// the thing worth storing, so the outlier is what survives.
func (m *LatencyMap) Coarse(blocksPerSegment int) []uint16 {
	if blocksPerSegment <= 0 {
		return nil
	}
	n := (len(m.Values) + blocksPerSegment - 1) / blocksPerSegment
	out := make([]uint16, n)
	for i := range out {
		lo := i * blocksPerSegment
		hi := min(lo+blocksPerSegment, len(m.Values))
		worst := LatSkipped
		for _, v := range m.Values[lo:hi] {
			switch {
			case v == LatSkipped:
				// Skipped never wins: a segment with any real measurement
				// should report that measurement.
			case worst == LatSkipped, v > worst:
				worst = v
			}
		}
		out[i] = worst
	}
	return out
}
