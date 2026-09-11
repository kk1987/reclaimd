package reclaimd

import (
	"context"
	"errors"
	"sort"
	"time"
)

// maxSpeedBudget is the hard ceiling on a speed test, whatever the config
// says. A test is a burst of full-speed reads on a disk that may be carrying
// a live filesystem, and a minute of that is as much as anyone should ask.
const maxSpeedBudget = time.Minute

// Speed test outcomes.
const (
	SpeedComplete  = "complete"
	SpeedDropout   = "dropout"
	SpeedCancelled = "cancelled"
)

// Why a region's sampling ended before its stretch was read out.
const (
	SpeedStopBudget   = "budget"    // its share of the time ran out
	SpeedStopNearHang = "near_hang" // a block took as long as a controller hang
	SpeedStopError    = "error"     // the device went away
)

// SpeedRegion is one sampled stretch of the disk, read flat out.
type SpeedRegion struct {
	Offset    int64   `json:"offset"`
	Bytes     int64   `json:"bytes"`
	Blocks    int     `json:"blocks_n"`
	ElapsedS  float64 `json:"elapsed_s"`
	MiBs      float64 `json:"mibs"`
	P50Ms     float64 `json:"p50_ms"`
	MaxMs     float64 `json:"max_ms"`
	Slow      int     `json:"slow_n"`   // blocks at or past the slow floor
	Errors    int     `json:"errors_n"` // unreadable blocks, stepped over
	StoppedBy string  `json:"stopped_by,omitempty"`
}

// SpeedResult is one speed test, stored as the disk's latest and summarised
// in an event.
type SpeedResult struct {
	Schema    int           `json:"schema"`
	At        time.Time     `json:"at"`
	BlockSize int           `json:"block_size"`
	RegionMiB int           `json:"region_mib"`
	Regions   []SpeedRegion `json:"regions"`

	Bytes         int64   `json:"bytes"`
	Blocks        int     `json:"blocks_n"`
	ElapsedS      float64 `json:"elapsed_s"`
	MiBs          float64 `json:"mibs"`
	MinMiBs       float64 `json:"min_mibs"`
	MaxMiBs       float64 `json:"max_mibs"`
	SlowestOffset int64   `json:"slowest_offset"`
	FastestOffset int64   `json:"fastest_offset"`
	P50Ms         float64 `json:"p50_ms"`
	P95Ms         float64 `json:"p95_ms"`
	MaxMs         float64 `json:"max_ms"`
	Slow          int     `json:"slow_n"`
	Errors        int     `json:"errors_n"`
	Outcome       string  `json:"outcome"`
}

// speedRegionOffsets spreads n stretches of regionBytes evenly from the start
// of the disk to its end, the first at zero and the last ending at the last
// block, so the sample covers both edges and the middle. Each start is aligned
// to a superblock when the stretch spans at least one, so a stretch reads
// whole erase blocks rather than the tail of one and the head of the next.
func speedRegionOffsets(size, regionBytes, blockSize, segmentSize int64, n int) []int64 {
	if n <= 0 || regionBytes <= 0 || size < regionBytes {
		return nil
	}
	align := blockSize
	if segmentSize > 0 && regionBytes >= segmentSize && segmentSize%blockSize == 0 {
		align = segmentSize
	}
	if n == 1 {
		return []int64{0}
	}
	stride := (size - regionBytes) / int64(n-1)
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		off := alignDown(int64(i)*stride, align)
		if len(out) > 0 && off <= out[len(out)-1] {
			continue // a tiny disk, where the stretches would overlap
		}
		out = append(out, off)
	}
	return out
}

// SpeedTest reads a few stretches of the disk as fast as it will go and
// reports what it got, per stretch and overall.
//
// The daemon's ordinary reads are deliberately slow: rate-limited, rested,
// backed off at the first slow block. That is right for maintenance and
// useless for answering "how fast does this drive read right now", which on
// these sticks is a question about the age of the data (a fresh write reads
// at the rated speed, an old one at a fraction of it). So this is the one
// read path with no duty cycle and no throughput ceiling. It stays short: each
// stretch has a share of the budget, and a block that takes as long as a
// controller hang ends that stretch, because the read after a near-hang is
// the one that drops the bus.
//
// Time is counted from the reads themselves rather than the wall clock, which
// is the same thing on hardware and lets the test run against a fake.
func SpeedTest(ctx context.Context, dev BlockReader, cfg Config) (SpeedResult, error) {
	blockSize := int64(dev.BlockSize())
	size := dev.Size()
	res := SpeedResult{
		Schema:    stateSchema,
		At:        time.Now(),
		BlockSize: int(blockSize),
		RegionMiB: cfg.SpeedRegionMiB,
		Outcome:   SpeedComplete,
	}

	n := cfg.SpeedRegions
	regionBytes := int64(cfg.SpeedRegionMiB) << 20
	if per := alignDown(size/int64(max(n, 1)), blockSize); regionBytes > per {
		regionBytes = per // a disk too small for the configured stretches
	}
	if regionBytes < blockSize {
		regionBytes = blockSize
	}
	offsets := speedRegionOffsets(size, regionBytes, blockSize, cfg.SegmentSize, n)
	if len(offsets) == 0 {
		return res, errors.New("disk too small for a speed test")
	}

	budget := cfg.SpeedBudget.Duration()
	if budget <= 0 || budget > maxSpeedBudget {
		budget = maxSpeedBudget
	}
	perRegion := budget / time.Duration(len(offsets))
	var spent time.Duration
	var all []time.Duration

	for _, start := range offsets {
		r := SpeedRegion{Offset: start}
		var lat []time.Duration
		var regionSpent time.Duration
		end := start + regionBytes
	region:
		for off := start; off < end; off += blockSize {
			if err := ctx.Err(); err != nil {
				res.Outcome = SpeedCancelled
				res.Regions = append(res.Regions, finishSpeedRegion(r, lat, regionSpent))
				return finishSpeed(res, all), err
			}
			if regionSpent >= perRegion || spent >= budget {
				r.StoppedBy = SpeedStopBudget
				break
			}
			d, err := dev.ReadBlock(off)
			spent += d
			regionSpent += d
			switch {
			case err == nil:
			case errors.Is(err, ErrMediaError):
				r.Errors++
				continue region
			case errors.Is(err, ErrDeviceDisconnected):
				r.StoppedBy = SpeedStopError
				res.Outcome = SpeedDropout
				res.Regions = append(res.Regions, finishSpeedRegion(r, lat, regionSpent))
				return finishSpeed(res, all), err
			default:
				r.StoppedBy = SpeedStopError
				res.Regions = append(res.Regions, finishSpeedRegion(r, lat, regionSpent))
				return finishSpeed(res, all), err
			}
			lat = append(lat, d)
			all = append(all, d)
			r.Blocks++
			r.Bytes += blockSize
			if d >= cfg.SlowFloor.Duration() {
				r.Slow++
			}
			if d >= cfg.DangerFloor.Duration() {
				// The reads after a near-hang are the ones that take the
				// device off the bus. This stretch has said what it has to say.
				r.StoppedBy = SpeedStopNearHang
				break
			}
		}
		res.Regions = append(res.Regions, finishSpeedRegion(r, lat, regionSpent))
	}
	return finishSpeed(res, all), nil
}

func finishSpeedRegion(r SpeedRegion, lat []time.Duration, spent time.Duration) SpeedRegion {
	r.ElapsedS = spent.Seconds()
	if len(lat) > 0 && spent > 0 {
		r.MiBs = round2(float64(r.Bytes) / (1 << 20) / spent.Seconds())
		p50, _, mx := latencyQuantiles(lat)
		r.P50Ms, r.MaxMs = msOf(p50), msOf(mx)
	}
	return r
}

func finishSpeed(res SpeedResult, all []time.Duration) SpeedResult {
	first := true
	for _, r := range res.Regions {
		res.Bytes += r.Bytes
		res.Blocks += r.Blocks
		res.ElapsedS += r.ElapsedS
		res.Slow += r.Slow
		res.Errors += r.Errors
		if r.Blocks == 0 {
			continue
		}
		if first || r.MiBs < res.MinMiBs {
			res.MinMiBs, res.SlowestOffset = r.MiBs, r.Offset
		}
		if first || r.MiBs > res.MaxMiBs {
			res.MaxMiBs, res.FastestOffset = r.MiBs, r.Offset
		}
		first = false
	}
	if res.ElapsedS > 0 {
		res.MiBs = round2(float64(res.Bytes) / (1 << 20) / res.ElapsedS)
	}
	if len(all) > 0 {
		p50, p95, mx := latencyQuantiles(all)
		res.P50Ms, res.P95Ms, res.MaxMs = msOf(p50), msOf(p95), msOf(mx)
	}
	return res
}

// latencyQuantiles sorts a copy and reads off p50, p95 and the maximum.
func latencyQuantiles(lat []time.Duration) (p50, p95, mx time.Duration) {
	s := append([]time.Duration(nil), lat...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(q float64) time.Duration {
		i := int(q * float64(len(s)-1))
		return s[i]
	}
	return at(0.5), at(0.95), s[len(s)-1]
}
