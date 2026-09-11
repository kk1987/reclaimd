// Command reclaimd keeps USB flash drives readable by periodically reading them
// end to end, which is what triggers the controller's own read-reclaim path and
// rewrites blocks whose charge has drifted toward the ECC margin.
//
// One binary with a subcommand per verb, so a deployment only ever has to copy
// a single file.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kk1987/reclaimd/internal/reclaimd"
)

// Set via -ldflags at build time.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

const usageText = `reclaimd keeps USB flash drives readable by reading them end to end.

usage: reclaimd <command> [flags]

commands:
  daemon    discover, adopt, schedule, scan, and serve the status page
  scan      one pass over one disk now; the outcome is the exit code
  list      every USB disk visible, and the key each is filed under
  export    dump all stored state as JSON
  refresh   rewrite a disk in place, behind four gates
  version   build metadata

reclaimd <command> -h lists the flags that command takes.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "help", "-h", "-help", "--help":
		fmt.Print(usageText)

	case "version":
		emitVersion()

	case "list":
		fs, common := newCommand(cmd)
		_ = fs.Parse(args)
		_, logger := common.load()
		if err := runList(logger); err != nil {
			logger.Error("list failed", "error", err)
			os.Exit(1)
		}

	case "daemon":
		fs, common := newCommand(cmd)
		common.listen = fs.String("listen", "", "override HTTP listen address")
		_ = fs.Parse(args)
		cfg, logger := common.load()
		asked, err := runDaemon(cfg, logger)
		if err != nil {
			logger.Error("daemon exited", "error", err)
			os.Exit(1)
		}
		if !asked {
			// systemd's Restart=on-failure only restarts a non-zero exit, so a
			// daemon that has finished for any recoverable reason must still
			// report failure. procd respawns regardless of the exit code.
			os.Exit(1)
		}

	case "scan":
		fs, common := newCommand(cmd)
		disk := fs.String("disk", "", "disk key or /dev node (required)")
		force := fs.Bool("i-mean-it", false, "scan inside a post-dropout suppression window")
		_ = fs.Parse(args)
		cfg, logger := common.load()
		if *disk == "" {
			logger.Error("-disk is required for scan")
			os.Exit(1)
		}
		os.Exit(runScan(cfg, logger, *disk, *force))

	case "export":
		fs, common := newCommand(cmd)
		_ = fs.Parse(args)
		cfg, logger := common.load()
		if err := runExport(cfg, logger); err != nil {
			logger.Error("export failed", "error", err)
			os.Exit(1)
		}

	case "refresh":
		fs, common := newCommand(cmd)
		disk := fs.String("disk", "", "disk key or /dev node (required)")
		confirm := fs.String("confirm", "", "must equal the target's serial number")
		mode := fs.String("mode", "rewrite", "rewrite | zero")
		rangeSpec := fs.String("range", "", "BYTE_START:BYTE_END, default whole device")
		iMeanIt := fs.Bool("i-mean-it", false, "required for -mode=zero")
		dryRun := fs.Bool("dry-run", false, "log the plan and change nothing")
		_ = fs.Parse(args)
		cfg, logger := common.load()
		if *disk == "" {
			logger.Error("-disk is required for refresh")
			os.Exit(1)
		}
		start, end, err := parseRange(*rangeSpec)
		if err != nil {
			logger.Error("bad -range", "error", err)
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = reclaimd.Refresh(ctx, cfg, reclaimd.DefaultPlatform(), logger,
			reclaimd.RefreshOpts{
				Key: *disk, Confirm: *confirm, Mode: *mode, IMeanIt: *iMeanIt,
				Start: start, End: end, DryRun: *dryRun,
			})
		stop()
		if errors.Is(err, context.Canceled) {
			// Stopped at a block boundary on the operator's signal. The log
			// already says where, and the -range that resumes from there.
			os.Exit(1)
		}
		if err != nil {
			logger.Error("refresh refused", "error", err)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
}

// commonFlags are the ones every command that reads config accepts. listen is
// filled in by daemon alone, which is the point of a flag set per command: list
// does not offer -confirm and refresh does not offer -listen.
type commonFlags struct {
	config   *string
	stateDir *string
	listen   *string
}

func newCommand(name string) (*flag.FlagSet, *commonFlags) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: reclaimd %s [flags]\n\nflags:\n", name)
		fs.PrintDefaults()
	}
	return fs, &commonFlags{
		config:   fs.String("config", "", "path to JSON config file"),
		stateDir: fs.String("state-dir", "", "override state directory"),
	}
}

// load reads the config, applies the command-line overrides on top of it, and
// builds the logger every command shares.
func (c *commonFlags) load() (reclaimd.Config, *slog.Logger) {
	cfg, err := reclaimd.LoadConfigFromFile(*c.config)
	if err != nil {
		// The logger does not exist yet and config failure is terminal, so this
		// is the one place that writes a bare line to stderr.
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	if *c.stateDir != "" {
		cfg.StateDir = *c.stateDir
	}
	if c.listen != nil && *c.listen != "" {
		cfg.ListenAddr = *c.listen
	}
	return cfg, slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(cfg.LogLevel),
	}))
}

// runScan performs one pass and reports the outcome through the exit code, so a
// cron entry or a shell script can act on it without parsing the log.
func runScan(cfg reclaimd.Config, logger *slog.Logger, disk string, force bool) int {
	store, err := reclaimd.OpenStore(cfg.StateDir, logger)
	if err != nil {
		logger.Error("open store", "error", err)
		return 1
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sup := reclaimd.NewSupervisor(cfg, store, logger, reclaimd.DefaultPlatform())
	sum, err := sup.ScanOnce(ctx, disk, force)
	if err != nil {
		logger.Error("scan failed", "error", err, "disk", disk)
		return 1
	}
	logger.Info("scan complete", "disk", disk, "outcome", sum.Outcome,
		"slow_n", sum.SlowBlocks, "danger_n", sum.DangerBlocks,
		"drop_n", sum.Dropouts, "healed_n", sum.Healed,
		"read_mib", sum.BytesRead>>20)

	switch sum.Outcome {
	case reclaimd.OutcomeClean:
		return 0
	case reclaimd.OutcomeDropout, reclaimd.OutcomeNearHang:
		return 3
	case reclaimd.OutcomeSlow, reclaimd.OutcomeMediaErrors:
		return 2
	default:
		return 0
	}
}

// runExport dumps everything the daemon knows without touching a device, so it
// can be diffed, archived, or read on a machine that is not the one holding the
// disk.
func runExport(cfg reclaimd.Config, logger *slog.Logger) error {
	store, err := reclaimd.OpenStore(cfg.StateDir, logger)
	if err != nil {
		return err
	}
	defer store.Close()

	keys, err := store.ListDisks()
	if err != nil {
		return err
	}
	out := map[string]any{"disks": map[string]any{}}
	for _, k := range keys {
		meta, _ := store.LoadMeta(k)
		sched, _ := store.LoadSchedule(k)
		rounds, _ := store.ListRounds(k, 0)
		events, _ := store.ListEvents(k, 500, 0)
		out["disks"].(map[string]any)[k] = map[string]any{
			"meta": meta, "schedule": sched, "rounds": rounds, "events": events,
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func parseRange(spec string) (int64, int64, error) {
	if spec == "" {
		return 0, 0, nil
	}
	a, b, ok := strings.Cut(spec, ":")
	if !ok {
		return 0, 0, fmt.Errorf("want START:END, got %q", spec)
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	end, err := strconv.ParseInt(b, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}

// runDaemon reports whether it stopped because it was asked to. Only a signal
// from the service manager counts: anything else means the daemon gave up on
// its own, and the caller turns that into a non-zero exit so the manager
// restarts it.
func runDaemon(cfg reclaimd.Config, logger *slog.Logger) (bool, error) {
	store, err := reclaimd.OpenStore(cfg.StateDir, logger)
	if err != nil {
		return false, err
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup := reclaimd.NewSupervisor(cfg, store, logger, reclaimd.DefaultPlatform())
	srv := reclaimd.NewServer(cfg, store, sup, logger)
	srv.Build = reclaimd.BuildInfo{Version: version, Commit: commit, Date: buildDate}

	if cfg.Rewrite.Enabled && os.Geteuid() != 0 {
		// Freezing a filesystem takes CAP_SYS_ADMIN and writing a block device
		// takes permission on the node, and the systemd unit grants neither.
		// The rewrite phase would find that out on its own, one refused round
		// at a time. Better to say so once, at the top of the log.
		logger.Warn("rewrite is enabled but the daemon is not root; live rewrites "+
			"need CAP_SYS_ADMIN for the freeze and write access to the device",
			"uid", os.Geteuid())
	}
	if cfg.ListenAddr != "" && !isLoopback(cfg.ListenAddr) && cfg.UIToken == "" {
		// An unauthenticated status page on a LAN interface has to be a
		// deliberate choice. The warning is there so it cannot happen
		// quietly by leaving a default alone.
		logger.Warn("listening off-loopback without a ui_token; the status page "+
			"is readable by anything on the network", "listen_addr", cfg.ListenAddr)
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Deliberately disabled: the SSE stream is a long-lived response, and
		// a write timeout would cut it off once per timeout period.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	stopPublisher := make(chan struct{})
	go srv.RunPublisher(stopPublisher)
	go reclaimd.RunWatchdog(ctx, sup.Heartbeat)
	go func() {
		logger.Info("http listening", "addr", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
			cancel()
		}
	}()

	supDone := make(chan struct{})
	go func() {
		defer close(supDone)
		if err := sup.Run(ctx); err != nil {
			logger.Error("supervisor error", "error", err)
		}
	}()

	_ = reclaimd.NotifyReady()
	reclaimd.NotifyStatus("watching for usb disks")

	asked := false
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)
	select {
	case <-done:
		asked = true
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	_ = reclaimd.NotifyStopping()
	cancel()
	close(stopPublisher)
	// This has to happen before Shutdown, which waits for connections to go
	// idle. An open event stream never does.
	srv.CloseStreams()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "error", err)
	}

	// A round can be mid-pread for up to the controller's watchdog interval, so
	// give the supervisor room to unwind before the unit's TimeoutStopSec.
	select {
	case <-supDone:
	case <-time.After(30 * time.Second):
		logger.Warn("supervisor did not stop in time")
	}
	return asked, nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func emitVersion() {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]string{
		"version":    version,
		"commit":     commit,
		"build_date": buildDate,
		"go_version": goVersion(),
	})
}

// runList prints every USB block device the daemon can see, along with the key
// it would file it under. This is the first thing to run on a new machine: if
// the key here does not match the key on the other machine, nothing downstream
// will line up.
func runList(logger *slog.Logger) error {
	disks, err := reclaimd.DefaultPlatform().Discover()
	if err != nil {
		return err
	}
	if len(disks) == 0 {
		logger.Info("no usb block devices found")
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	for _, d := range disks {
		if err := enc.Encode(d); err != nil {
			return err
		}
	}
	return nil
}
