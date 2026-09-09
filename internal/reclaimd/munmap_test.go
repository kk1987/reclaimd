package reclaimd

import "syscall"

func munmapForTest(b []byte) { _ = syscall.Munmap(b) }
