# Contributing

Bug reports are the most useful thing you can send. The daemon's whole job is
reacting to how one particular USB controller misbehaves, and every controller
misbehaves differently — a report from a drive I have never seen is data I
cannot get any other way. Open an issue with `reclaimd version`, the platform,
and the `usb ... USB disconnect` lines from `dmesg` or `logread`.

## Working on the code

```sh
go test ./...      # everything runs without root and without a real disk
./build.sh         # cross-compiles both targets into out/
```

Run `gofmt -l .` and `go vet ./...` before pushing; CI runs both, plus the tests
under `-race`.

**No third-party dependencies.** `go.mod` has no `require` block and should stay
that way. The binary gets installed on routers with 8 MiB of flash, `CGO_ENABLED=0`
so it links against nothing at all, and the whole point is that it keeps working
after the machine has been left alone for a year. A dependency has to earn a lot
to be worth losing that.

Nothing about a disk's behaviour should be hardcoded either. Every threshold in
here is derived from the drive's own measured latency, and the UI shows the rule
that produced it. If you find yourself typing a millisecond count, that number
probably belongs to one drive and not to the next one.

## Commits

Subject line in the imperative, under 72 characters, saying what changes rather
than which files were touched. The body explains why, wrapped at 72. Keep a
change per commit.
