package reclaimd

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// notify sends one sd_notify datagram.
//
// The protocol is a single unconnected AF_UNIX datagram to $NOTIFY_SOCKET; a
// leading '@' means an abstract socket, which Go spells with a leading NUL.
// Fifteen lines of stdlib buys Type=notify and WatchdogSec, and a watchdog is
// worth having on a daemon whose whole job is poking hardware known to hang.
func notify(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err
}

// NotifyReady tells systemd the daemon is up.
func NotifyReady() error { return notify("READY=1") }

// NotifyStopping tells systemd a clean shutdown is in progress, so a slow stop
// while a 1.8s pread unwinds is not mistaken for a hang.
func NotifyStopping() error { return notify("STOPPING=1") }

// RunWatchdog pings systemd from the supervisor's heartbeat.
//
// Deliberately not from a scanner: a scanner blocked 1.8 seconds inside pread
// is the expected case on ailing flash, not a hang, and a watchdog that killed
// the process for it would fire at precisely the moment things were working as
// designed.
func RunWatchdog(ctx context.Context, beat func() time.Time) {
	usecStr := os.Getenv("WATCHDOG_USEC")
	if usecStr == "" {
		return
	}
	usec, err := strconv.ParseInt(usecStr, 10, 64)
	if err != nil || usec <= 0 {
		return
	}
	interval := time.Duration(usec) * time.Microsecond / 2
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Since(beat()) > 3*interval {
				continue // supervisor is wedged; let systemd notice
			}
			_ = notify("WATCHDOG=1")
		}
	}
}

// NotifyStatus publishes a one-line status visible in `systemctl status`.
func NotifyStatus(format string, args ...any) {
	_ = notify("STATUS=" + fmt.Sprintf(format, args...))
}
