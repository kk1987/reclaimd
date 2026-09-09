package reclaimd

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
	"unsafe"
)

func TestAlignedBufferIsPageAligned(t *testing.T) {
	const size = 1 << 20
	b, err := alignedBuffer(size)
	if err != nil {
		t.Fatal(err)
	}
	defer munmapForTest(b)

	if len(b) != size {
		t.Fatalf("len = %d, want %d", len(b), size)
	}
	addr := uintptr(unsafe.Pointer(&b[0]))
	if page := uintptr(os.Getpagesize()); addr%page != 0 {
		t.Errorf("buffer at %#x is not %d-aligned; o_direct will reject it", addr, page)
	}
}

// realDevice gates the tests that need the physical stick. They are the only
// way to confirm the O_DIRECT constant is right for the architecture, which is
// the single most consequential portability trap in this program: on arm64
// O_DIRECT and O_DIRECTORY have exactly each other's values, so a wrong
// constant fails with ENOTDIR and looks like something else entirely.
func realDevice(t *testing.T) Presence {
	t.Helper()
	key := os.Getenv("RECLAIMD_TEST_DISK")
	if key == "" {
		t.Skip("set RECLAIMD_TEST_DISK=<key> to run against real hardware")
	}
	disks, err := DiscoverUSBDisks(DefaultRoots())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		if d.Identity.Key == key {
			return d
		}
	}
	t.Fatalf("disk %q not present", key)
	return Presence{}
}

func TestRealDeviceReadLatency(t *testing.T) {
	p := realDevice(t)
	dev, err := OpenDevice(p, 1<<20, DefaultRoots())
	if err != nil {
		t.Fatalf("open (a wrong O_DIRECT constant shows up here as ENOTDIR): %v", err)
	}
	defer dev.Close()

	if err := dev.WarmUp(context.Background(), 8); err != nil {
		t.Fatalf("warm up: %v", err)
	}
	var total time.Duration
	const n = 32
	for i := 0; i < n; i++ {
		d, err := dev.ReadBlock(int64(i+64) * (1 << 20))
		if err != nil {
			t.Fatalf("read block %d: %v", i, err)
		}
		total += d
	}
	avg := total / n
	t.Logf("mean 1 MiB read: %v (%.1f MiB/s)", avg, float64(time.Second)/float64(avg))
	if avg > 200*time.Millisecond {
		t.Errorf("mean latency %v is implausibly high for a healthy sequential read", avg)
	}
}

// An unaligned offset must surface as ErrAlignment, never as ErrMediaError.
// Getting this wrong is how an O_DIRECT bug spends months being blamed on the
// hardware.
func TestRealDeviceUnalignedOffsetIsAlignmentError(t *testing.T) {
	p := realDevice(t)
	dev, err := OpenDevice(p, 1<<20, DefaultRoots())
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	_, err = dev.ReadBlock(1<<20 + 1)
	if err == nil {
		t.Fatal("unaligned read unexpectedly succeeded")
	}
	if !errors.Is(err, ErrAlignment) {
		t.Fatalf("got %v, want ErrAlignment", err)
	}
	if errors.Is(err, ErrMediaError) {
		t.Fatal("alignment bug was misclassified as a media error")
	}
}

func TestRealDeviceBlockCount(t *testing.T) {
	p := realDevice(t)
	dev, err := OpenDevice(p, 1<<20, DefaultRoots())
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	t.Logf("size=%d bytes, %d whole 1 MiB blocks, max_sectors_kb=%d",
		dev.Size(), dev.BlockCount(), p.Identity.MaxSectorsKB)

	// One read must map to one SCSI command, or the latency measured is an
	// average of several and the whole detection scheme is blunted.
	if p.Identity.MaxSectorsKB > 0 && dev.BlockSize() > p.Identity.MaxSectorsKB*1024 {
		t.Errorf("block size %d exceeds max_sectors_kb %d; reads will be split",
			dev.BlockSize(), p.Identity.MaxSectorsKB)
	}
}
