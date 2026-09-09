# reclaimd

Keeps USB flash drives readable by periodically reading them end to end.

## Why this exists

A USB flash drive left plugged in for months — hanging off a router, carrying a
NAS boot volume, sitting in a media box — starts dropping off the USB bus during
ordinary reads and re-enumerating seconds later under a new device name. That
looks like a dying controller, and on the drive traced here it was not:

- **NAND data-retention loss.** Data left unread long enough drifts toward the
  ECC margin. Reading it makes the controller work: a 1 MiB block that normally
  takes 10 ms takes 500 ms or more.
- At the extreme the controller **hangs for a constant 1500-1800 ms** and its
  internal watchdog power-cycles the whole device. The kernel sees a clean
  `USB disconnect` with no SCSI timeout, no reset and no protocol error.
- **Reading a degraded block hands the controller its read-reclaim trigger**, so
  it gets rewritten and is healthy afterwards. Three full passes over one such
  drive went **110 → 10 → 1 dropouts** and **1181 → 258 → 26 slow blocks**; of
  347 blocks over 500 ms in pass one, **345 read normally in pass two**.
- Degradation follows the physical layout, not the filesystem: **90.5% of the
  extreme blocks sit at `offset mod 32 MiB == 31`** — the last wordline of an
  erase block, the most fragile place on the die.
- After rewriting a region, reading it back was **flawless at full 135 MiB/s**.
- Writing is not immune either. A whole-drive rewrite dropped the device at
  12.6 GiB with the same clean `USB disconnect`, this time on a `op 0x1:(WRITE)`
  — so `refresh` has to reattach and resume rather than give up, or it leaves
  the drive part-rewritten, which is worse than either finishing or never
  starting.

So the hardware was fine and the data on it had gone stale. Reading the whole
drive on a schedule prevents that from ever accumulating. This daemon does that,
and shows its work.

## What it does

- Finds USB block devices through **sysfs only** (no `/dev/disk/by-id`, which
  OpenWrt does not populate) and identifies them by USB serial, so a rename from
  `sda` to `sdb` after a dropout rebinds automatically.
- Reads every block with `O_DIRECT` — a buffered read would come from the page
  cache and refresh nothing — and records the latency of each one.
- **Backs off at the first sign of trouble.** A slow read that completed has
  already triggered reclaim, so the refresh worked; carrying on is pure risk, and
  84% of the dropouts in the forensics had a slow block within the preceding
  10 MiB. Skipped segments are deferred, never dropped, and drained first next
  pass.
- Re-probes deferred blocks at the end of a pass, after long enough for the
  controller's read cache to clear, and counts the ones that **healed**.
- Serves a status page with live progress, the whole-drive latency map, the
  healing waterfall across passes (absolute, or diffed against the previous or
  the first pass), a freshness map of what has gone longest without a read, and
  a decision desk that explains every adaptive value it chose.
- Shows what it has worked out about the hardware: worst latency by offset
  within a superblock, which on the drive under test peaks on the last position
  of every erase block. Next to it sits the only output that asks for a human —
  segments that stayed slow for several passes and never healed.
- Publishes its own wear bill in the footer. Measured at roughly 9 MiB a year
  on a ten-day cadence.

**The daemon never writes to a disk.** It opens read-only, and the systemd unit
makes that a kernel-enforced property via `DeviceAllow=block-sd r` rather than a
promise the code makes about itself. Rewriting is a separate `refresh` command
behind four gates.

## Nothing to tune

Everything adapts, and every derived value is visible in the UI with the rule
that produced it:

| | how it decides |
|---|---|
| **Scan interval** | multiplicative: clean ×1.5, slow ×0.7, dropout ×0.5, clamped to 12 h–30 d |
| **Slow / danger thresholds** | 5× and 50× the disk's own learned p50. On the drive under test that lands on 50 ms and 500 ms — the same numbers picked by hand during the forensics |
| **Duty cycle** | rest scales with how far the rolling latency has drifted from the pass baseline. The read latency is the thermometer; no sensor needed |
| **Backoff distance** | one 32 MiB superblock past a slow block, seven past a near-hang — 3× the measured 10 MiB precursor window |

The UI also shows what the *next* pass would do (`still clean → 23.6 d /
slow blocks → 11.0 d / dropout → 7.9 d`), so the policy can be checked rather
than trusted.

## Build

```sh
./build.sh          # out/reclaimd-linux-amd64, out/reclaimd-linux-arm64
```

`CGO_ENABLED=0` throughout: with no libc linkage the musl/glibc split does not
exist, so one binary runs on both OpenWrt and Arch. No third-party dependencies
at all — `go.mod` has no `require` block.

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
everything, whether it agrees or not — useful for a drive that keeps finding
near-hangs in the same place. It is a separate command because it is destructive
if aimed wrongly, and it has four gates:

1. `-confirm` must be the serial **printed on the device**. Not a `--yes`: a
   confirmation you can paste from the wrong terminal is not a confirmation, and
   the error message deliberately does not tell you the serial.
2. The disk and all its partitions must be absent from `/proc/self/mountinfo`
   and `/proc/swaps`.
3. It opens `O_RDWR|O_EXCL`, which on a block device means "fail if mounted or
   claimed" — turning gate 2 from a check with a race window into something the
   kernel enforces.
4. The default `-mode=rewrite` reads each block and writes the same bytes back.
   Content-wise it is a no-op; to the NAND it is a full program cycle. **A block
   that fails to read is never written back** — doing so would turn a
   recoverable retention problem into permanent data loss. `-mode=zero` erases
   outright and needs `-i-mean-it` as well.

```sh
reclaimd refresh -disk=<key> -confirm=<serial> -dry-run
reclaimd refresh -disk=<key> -confirm=<serial> -range=9000000000:11000000000
```

### OpenWrt

```sh
install -m0755 out/reclaimd-linux-arm64 /usr/bin/reclaimd
install -m0755 deploy/reclaimd.init /etc/init.d/reclaimd
/etc/init.d/reclaimd enable && /etc/init.d/reclaimd start
logread -e reclaimd
```

State goes to `/root/reclaimd`, **not** `/var/lib`: on OpenWrt `/var` is a
symlink to `/tmp`, so state there is lost at every reboot along with the
post-dropout suppression window — and the only symptom would be a daemon that
never quite seems to work. The daemon warns if it finds itself on tmpfs.

### Arch / systemd

```sh
useradd --system --no-create-home --shell /usr/sbin/nologin reclaimd
install -m0755 out/reclaimd-linux-amd64 /usr/bin/reclaimd
install -m0644 deploy/reclaimd.service /etc/systemd/system/
systemctl enable --now reclaimd
```

## Before putting a drive into service

The first pass over a neglected drive is the dangerous one — that is the pass
that found 110 dropouts. If the drive is going to carry something that cannot
survive a dropout, such as a router's overlay filesystem, **do that first pass
while it is still just a stick in a laptop.**

Making the filesystem and copying the data on is itself a full-drive write, so a
freshly provisioned drive starts refreshed. From there the daemon only has to
keep it that way.

## Deliberately not done

The scanner does not drill down to locate a slow block precisely. Re-reading the
neighbourhood at a finer granularity would sharpen the map, and it would do so by
walking straight back into the thing being avoided. It backs off to the next
superblock boundary instead and lets the controller finish reclaiming in peace.
