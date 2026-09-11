package reclaimd

import "sync/atomic"

// atomic64 is a named wrapper around atomic.Int64 so the heartbeat's intent
// reads clearly at its use sites.
type atomic64 struct{ v atomic.Int64 }

func (a *atomic64) Store(v int64) { a.v.Store(v) }
func (a *atomic64) Load() int64   { return a.v.Load() }
