# reclaimd

[![ci](https://img.shields.io/github/actions/workflow/status/kk1987/reclaimd/ci.yml?branch=main&label=ci)](https://github.com/kk1987/reclaimd/actions/workflows/ci.yml) [![release](https://img.shields.io/github/v/release/kk1987/reclaimd)](https://github.com/kk1987/reclaimd/releases/latest) [![go](https://img.shields.io/github/go-mod/go-version/kk1987/reclaimd)](go.mod) [![license](https://img.shields.io/github/license/kk1987/reclaimd)](LICENSE)

Keeps USB flash drives readable by periodically reading them end to end.

## Why this exists

A USB flash drive left plugged in for months (hanging off a router, carrying a
NAS boot volume, sitting in a media box) starts dropping off the USB bus during
ordinary reads and re-enumerating seconds later under a new device name. That
looks like a dying controller.

On the drive that prompted this tool it was not. That drive was a 64 GB USB 3.0
stick that had spent months as a router's overlay filesystem, and the findings
below come from reading it end to end several times over and timing every block.
The rest of this repository calls that investigation "the forensics" and the
stick "the drive under test". Where a comment cites a number from either, it is
that one drive's number and not a universal constant.

- The cause was NAND data-retention loss. Data left unread long enough drifts
  toward the ECC margin, and reading it makes the controller work: a 1 MiB
  block that normally takes 10 ms takes 500 ms or more.
- At the extreme the controller hangs for a constant 1500-1800 ms and its
  internal watchdog power-cycles the whole device. The kernel sees a clean
  `USB disconnect` with no SCSI timeout, no reset and no protocol error.
- Reading a degraded block hands the controller its read-reclaim trigger, so it
  gets rewritten and is healthy afterwards. Three full passes over the drive
  went 110 → 10 → 1 dropouts and 1181 → 258 → 26 slow blocks. Of the 347
  blocks over 500 ms in pass one, 345 read normally in pass two.
- Degradation follows the physical layout, not the filesystem: 90.5% of the
  extreme blocks sit at `offset mod 32 MiB == 31`, the last wordline of an
  erase block and the most fragile place on the die.
- After rewriting a region, reading it back was flawless at full 135 MiB/s.
- Writing is not immune either. A whole-drive rewrite dropped the device at
  12.6 GiB with the same clean `USB disconnect`, this time on a
  `op 0x1:(WRITE)`. So `refresh` has to reattach and resume. Giving up would
  leave the drive part-rewritten, which is worse than either finishing or never
  starting.

So the hardware was fine and the data on it had gone stale. Reading the whole
drive on a schedule prevents that from ever accumulating. This daemon does that,
and shows its work.

The mechanism carries across drives. The constants do not. A second stick, a
cheap 29 GB one behind `usb-storage` with a 120 KiB transfer limit instead of
1 MiB, told the same story in a different shape: stalls of 3.8 to 5.1 s that
never dropped the bus at all, and every one of them reading back at 4-5 ms once
it had been read once. The same read-reclaim on a controller with a longer
fuse, and none of the same numbers. That is why nothing here is a fixed
millisecond count or a fixed block size. Every threshold is derived from the
disk in front of it, and the UI shows the derivation.

## What it does

- Finds USB block devices through sysfs only (no `/dev/disk/by-id`, which
  OpenWrt does not populate), or through CAM, GEOM and sysctl on FreeBSD, and
  identifies them by USB serial, so a rename from `sda` to `sdb` after a
  dropout rebinds automatically.
- Reads every block unbuffered and records the latency of each one. A buffered
  read would come from the page cache and refresh nothing.
- Backs off at the first sign of trouble. A slow read that completed has
  already triggered reclaim, so the refresh worked. Carrying on is pure risk:
  84% of the dropouts in the forensics had a slow block within the preceding
  10 MiB. Skipped segments are deferred and drained first next pass, so nothing
  is dropped.
- Re-probes deferred blocks at the end of a pass, after waiting out the delay
  the controller's read cache needs to clear, and counts the ones that healed.
  A round that stops early ends sooner than that delay, so a re-probe that
  skipped whatever was not yet due would report nothing on precisely the disks
  worth measuring.
- Serves a status page with live progress, the whole-drive latency map, the
  healing waterfall across passes (absolute, or diffed against the previous or
  the first pass), a freshness map of what has gone longest without a read, and
  a decision desk that explains every adaptive value it chose.
- Shows what it has worked out about the hardware: worst latency by offset
  within a superblock, which on the drive under test peaks on the last position
  of every erase block. Next to it sits the only output that asks for a human:
  segments that stayed slow for several passes and never healed.
- Keeps its own records tidy. A stick pulled before its 30-minute probation
  ends is forgotten entirely, so plugging one in for a moment does not leave a
  state directory behind forever. A stick with no serial is filed under its USB
  port, and the same rule keeps that from leaving one directory per port.
  Anything it has adopted, or that you excluded by hand, is kept until you
  delete it from the status page.
- Publishes its own wear bill in the footer. Measured at roughly 9 MiB a year
  on a ten-day cadence.

The daemon never writes to a disk. It opens read-only, and the systemd unit's
`DeviceAllow=block-sd r` has the kernel enforce that, so it does not rest on a
promise the code makes about itself. Rewriting is a separate `refresh` command
behind four gates.

## Nothing to tune

Everything adapts, and every derived value is visible in the UI with the rule
that produced it:

| | how it decides |
|---|---|
| **Read size** | the largest power of two that still fits in one SCSI command, from the disk's own transfer limit (`max_sectors_kb` on Linux), capped at 1 MiB. The forensics drive takes 1 MiB; a USB 2.0 stick behind `usb-storage` reports a 120 KiB limit and gets 64 KiB |
| **Scan interval** | multiplicative: clean ×1.5, slow ×0.7, dropout ×0.5, clamped to 12 h–30 d |
| **When to resume** | the interval is a *full-pass* cadence. A round that stopped on its circuit breaker did not deliver one, so it comes back at the floor and carries on from the cursor rather than waiting a whole interval to read the part it never reached |
| **Slow / danger thresholds** | 5× and 50× the disk's own learned p50. On the drive under test that lands on 50 ms and 500 ms — the same numbers picked by hand during the forensics |
| **Duty cycle** | rest scales with how far the rolling latency has drifted from the drive's *settled* latency, taken once the pass has warmed up. The read latency is the thermometer; no sensor needed |
| **Backoff distance** | one 32 MiB superblock past a slow block, seven past a near-hang — 3× the measured 10 MiB precursor window |

The UI also shows what the next pass would do (`still clean → 23.6 d /
slow blocks → 11.0 d / dropout → 7.9 d`), so the policy can be checked.

## Build

Prebuilt binaries for Linux and FreeBSD on both architectures are attached to
every [release](https://github.com/kk1987/reclaimd/releases/latest), with a
`SHA256SUMS` next to them. To build it yourself:

```sh
./build.sh          # out/reclaimd-{linux,freebsd}-{amd64,arm64}
```

Or, for the host you are already on:

```sh
go install github.com/kk1987/reclaimd/cmd/reclaimd@latest
```

`CGO_ENABLED=0` throughout: with no libc linkage the musl/glibc split does not
exist, so one binary runs on both OpenWrt and Arch. There are no third-party
dependencies: `go.mod` has no `require` block.

## Run

```sh
reclaimd list                     # what it sees, and the key it files it under
reclaimd daemon                   # maintain + serve http://127.0.0.1:8099
reclaimd scan -disk=<key>         # one pass now; 0 clean, 2 slow, 3 near-hang
reclaimd version
```

| command | does | disk I/O |
|---|---|---|
| `daemon` | discovery, adoption, scheduling, scanning, status page | read-only |
| `scan` | one pass over one disk; outcome in the exit code | read-only |
| `list` | enumerate USB disks and show computed keys | none |
| `export` | dump all stored state as JSON | none |
| `refresh` | rewrite a disk in place, behind four gates | **read-write** |
| `version` | build metadata | none |

### Rewriting a drive

Reading refreshes the blocks the controller judges marginal. Rewriting refreshes
everything, whether the controller agrees or not, which is useful for a drive
that keeps finding near-hangs in the same place. It is a separate command
because it is destructive if aimed wrongly, and it has four gates:

1. `-confirm` must be the serial printed on the device. There is no `--yes`: a
   confirmation you can paste from the wrong terminal is not a confirmation, and
   the error message deliberately does not tell you the serial.
2. The disk and all its partitions must be absent from `/proc/self/mountinfo`
   and `/proc/swaps`, or on FreeBSD held by nothing in the GEOM graph.
3. It opens `O_RDWR|O_EXCL`, which on a block device means "fail if mounted or
   claimed". That turns gate 2 from a check with a race window into something
   the kernel enforces. FreeBSD ignores `O_EXCL` on a disk. There GEOM refuses
   the open while a read-write mount, swap on a partition or a ZFS pool holds
   the disk, and a read-only mount is left to gate 2.
4. The default `-mode=rewrite` reads each block and writes the same bytes back.
   The content does not change, but to the NAND it is a full program cycle. A
   block that fails to read is never written back, because that would turn a
   recoverable retention problem into permanent data loss. `-mode=zero` erases
   outright and needs `-i-mean-it` as well.

```sh
reclaimd refresh -disk=<key> -confirm=<serial> -dry-run
reclaimd refresh -disk=<key> -confirm=<serial> -range=9000000000:11000000000
```

Ctrl-C stops a rewrite at the next block boundary. In rewrite mode nothing is
lost: the drive is refreshed up to an offset, and the last line logged carries
the `-range` that picks up from there.

The status page assembles both of those for the disk being viewed, with the key
and the serial already filled in, next to the freshness map that is the reason
to run one. It only ever produces text to copy: there is no `refresh` over HTTP,
the daemon holds every device read-only, and the systemd unit's
`DeviceAllow=block-sd r` means the kernel would refuse it a write even if it
asked. Gate 1 is weaker when the serial is copied off the page instead of read
off the device. That is the trade the page makes: it still catches the wrong
drive, but not the wrong idea.

### OpenWrt

Copy the binary and the init script over. Busybox has no `install`, and stock
dropbear ships no sftp subsystem, so `scp` works only where
`openssh-sftp-server` was added. A pipe works everywhere:

```sh
cat out/reclaimd-linux-arm64 | ssh root@<router> 'cat > /tmp/reclaimd.bin'
cat deploy/reclaimd.init     | ssh root@<router> 'cat > /tmp/reclaimd.init'
ssh root@<router> '
  cp /tmp/reclaimd.bin /usr/bin/reclaimd.new && chmod 0755 /usr/bin/reclaimd.new
  mv /usr/bin/reclaimd.new /usr/bin/reclaimd
  cp /tmp/reclaimd.init /etc/init.d/reclaimd && chmod 0755 /etc/init.d/reclaimd
  /etc/init.d/reclaimd enable && /etc/init.d/reclaimd restart'
logread -e reclaimd
```

The binary lands next to the old one and is renamed over it because an upgrade
cannot write to `/usr/bin/reclaimd` while the daemon is running it: that is
`ETXTBSY`. A rename within one directory swaps the entry and leaves the running
process on the old inode, so the restart is the only downtime. `restart` also
starts a daemon that was not running, so the same lines serve a first install.

There is nothing to configure. The daemon finds the disk, derives its read size
from it and schedules itself. `/etc/reclaimd/config.json` is read if it exists,
and it is not expected to.

To reach the status page from the LAN, put it behind the web server the router
is already running:

```sh
cat deploy/reclaimd.locations | ssh root@<router> 'cat > /etc/nginx/conf.d/reclaimd.locations'
ssh root@<router> 'nginx -t -c /etc/nginx/uci.conf && /etc/init.d/nginx reload'
```

On OpenWrt that file is included inside the default `_lan` server, so the page
appears at `https://<router>/reclaimd/`. The daemon keeps its loopback default,
with no `ui_token` to manage and no restart. The alternative is binding
`listen_addr` off loopback, and then `ui_token` is not optional.

Each location in that file also pulls in nginx's `restrict_locally`, so it can
be included by hand into a vhost that faces the internet, one with a real
certificate, and the page stays internal while the rest of the vhost does not:

```nginx
server {
    server_name example.com;          # public, real certificate
    include conf.d/reclaimd.locations;
    location / { proxy_pass http://127.0.0.1:8080; }
}
```

Verify which address the proxy sees before trusting that, because a gateway
doing NAT reflection decides it: on the router this was written for, a LAN
client reaching the public name is SNATed to the gateway's own LAN address and
allowed, while a request arriving from the internet keeps its real source and
gets 403.

State goes to `/root/reclaimd`, not `/var/lib`: on OpenWrt `/var` is a symlink
to `/tmp`, so state there is lost at every reboot along with the post-dropout
suppression window, and the only symptom would be a daemon that never quite
seems to work. The daemon warns if it finds itself on tmpfs.

### Arch / systemd

```sh
useradd --system --no-create-home --shell /usr/sbin/nologin reclaimd
install -m0755 out/reclaimd-linux-amd64 /usr/bin/reclaimd
install -m0644 deploy/reclaimd.service /etc/systemd/system/
systemctl enable --now reclaimd
```

### FreeBSD

```sh
install -m0755 out/reclaimd-freebsd-amd64 /usr/local/bin/reclaimd
install -m0755 deploy/reclaimd.rc /usr/local/etc/rc.d/reclaimd
sysrc reclaimd_enable=YES
service reclaimd start
```

It runs as root. Discovery reads CAM's device table through `/dev/xpt0`, which
nothing else may open, so here, as on OpenWrt, read-only is a promise the code
keeps, with nothing in the kernel enforcing it. State goes to
`/var/db/reclaimd` and the log to syslog, tagged `reclaimd`.

A stick is keyed by the same USB vendor, product and serial as on Linux, so it
keeps its history when it moves between the two. The read size follows the
transfer limit da(4) works out for the disk, which FreeBSD does not export:
1 MiB at SuperSpeed, 64 KiB behind a USB 2 port.

Other BSDs are not supported. Everything that asks the kernel about a disk sits
behind one interface, `Platform` in `internal/reclaimd/platform.go`, and the
rest of the program already compiles for NetBSD, OpenBSD and DragonFly. But
NetBSD and OpenBSD have neither CAM nor GEOM, so a port there would be a new
implementation of that interface, not a variation on the FreeBSD one.

## Before putting a drive into service

The first pass over a neglected drive is the dangerous one: that is the pass
that found 110 dropouts. If the drive is going to carry something that cannot
survive a dropout, such as a router's overlay filesystem, do that first pass
while it is still just a stick in a laptop.

Making the filesystem and copying the data on is itself a full-drive write, so a
freshly provisioned drive starts refreshed. From there the daemon only has to
keep it that way.

## Deliberately not done

The scanner does not drill down to locate a slow block precisely. Re-reading the
neighbourhood at a finer granularity would sharpen the map, and it would do so
by walking straight back into the thing being avoided. It backs off to the next
superblock boundary instead and lets the controller finish reclaiming in peace.

## Contributing

Bug reports from drives I have never seen are the most useful thing here. See
[CONTRIBUTING.md](CONTRIBUTING.md) for what to include, and
[SECURITY.md](SECURITY.md) for anything that should not be filed in public.

## License

[MIT](LICENSE).
