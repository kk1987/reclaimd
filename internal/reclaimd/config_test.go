package reclaimd

import "testing"

// A USB 2.0 stick behind usb-storage reports max_sectors_kb=120. Reading it in
// 1 MiB blocks would time nine SCSI commands as one, lifting the disk's p50
// ninefold and taking the slow threshold with it.
func TestBlockSizeFollowsTheTransferLimit(t *testing.T) {
	cfg, err := LoadConfigFromFile("")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name         string
		maxSectorsKB int
		want         int
	}{
		{"forensics drive", 1024, 1 << 20},
		{"usb-storage stick", 120, 64 << 10},
		{"exact power of two", 256, 256 << 10},
		{"above the cap", 4096, MaxBlockSize},
		{"unknown limit falls back", 0, MaxBlockSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cfg.BlockSizeFor(DiskIdentity{MaxSectorsKB: tc.maxSectorsKB})
			if got != tc.want {
				t.Fatalf("max_sectors_kb=%d: got %d, want %d", tc.maxSectorsKB, got, tc.want)
			}
			if tc.maxSectorsKB > 0 && got > tc.maxSectorsKB*1024 {
				t.Fatalf("block size %d exceeds the transfer limit %d KiB; reads will be split",
					got, tc.maxSectorsKB)
			}
			if got&(got-1) != 0 {
				t.Fatalf("block size %d is not a power of two; the latency map cannot encode it", got)
			}
			if resolved := cfg.ForDisk(DiskIdentity{MaxSectorsKB: tc.maxSectorsKB}); resolved.BlocksPerSegment()*got != int(resolved.SegmentSize) {
				t.Fatalf("segment %d does not divide evenly into %d-byte blocks", resolved.SegmentSize, got)
			}
		})
	}
}

// An explicit setting still wins. The derivation is only a default.
func TestExplicitBlockSizeOverridesTheDerivation(t *testing.T) {
	cfg, err := LoadConfigFromFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.BlockSize = 8192
	if got := cfg.BlockSizeFor(DiskIdentity{MaxSectorsKB: 1024}); got != 8192 {
		t.Fatalf("got %d, want the configured 8192", got)
	}
}

// A block size the latency map cannot encode must be rejected at load. Finding
// out after a full pass has been measured with it is too late.
func TestValidateRejectsNonPowerOfTwoBlockSize(t *testing.T) {
	cfg, err := LoadConfigFromFile("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.BlockSize = 122880 // 120 KiB: a multiple of 4096, but not a power of two
	if err := cfg.validate(); err == nil {
		t.Fatal("validate accepted a block size the latency map cannot encode")
	}
}
