//go:build !linux

package reclaimd

import (
	"errors"
	"runtime"
)

// errNoFreeze is the answer on a kernel with no filesystem freeze this program
// knows how to drive. A disk with nothing mounted can still be rewritten
// there, since that path opens the disk exclusively and freezes nothing.
var errNoFreeze = errors.New("filesystem freeze is not supported on " + runtime.GOOS)

type noFreezer struct{}

func defaultFreezer() fsFreezer { return noFreezer{} }

func (noFreezer) Freeze(string) (fsHold, error) { return nil, errNoFreeze }

func thawIfFrozen(string) (bool, error) { return false, nil }
