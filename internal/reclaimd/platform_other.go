//go:build !linux && !(freebsd && (amd64 || arm64))

package reclaimd

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
)

// DefaultPlatform on a kernel this program has no discovery for. Everything
// else builds, so supporting a kernel means writing its Platform and nothing
// more; until then the first question about a disk gets a plain no.
func DefaultPlatform() Platform { return unsupported{} }

var errUnsupported = fmt.Errorf("no USB disk discovery for %s/%s", runtime.GOOS, runtime.GOARCH)

type unsupported struct{}

func (unsupported) Discover() ([]Presence, error)      { return nil, errUnsupported }
func (unsupported) Alive(Presence) bool                { return false }
func (unsupported) CheckNotInUse(Presence) error       { return errUnsupported }
func (unsupported) IOStats(Presence) (diskStat, error) { return diskStat{}, errUnsupported }
func (unsupported) Uptime() (time.Duration, error)     { return 0, errUnsupported }

const defaultStateDir = "/var/db/reclaimd"

// Without discovery nothing is ever opened, so these only have to compile.
const (
	scanOpenFlags    = os.O_RDONLY | syscall.O_CLOEXEC
	refreshOpenFlags = os.O_RDWR | syscall.O_CLOEXEC
)

func onTmpfs(string) bool { return false }

// kernelVersion names the kernel as the Go runtime knows it; its release would
// take a platform file of its own.
func kernelVersion() (name, release string) { return runtime.GOOS, "" }
