package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"ipwatcher/internal/analyzer"
	"ipwatcher/internal/collector"
	"ipwatcher/internal/config"
	"ipwatcher/internal/logger"
	"ipwatcher/internal/provider"
	"ipwatcher/internal/storage"
)

// version can be overridden at build time via -ldflags "-X main.version=..."
var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd := os.Args[1]
	rest := os.Args[2:]

	switch cmd {
	case "run":
		os.Exit(cmdRun(rest))
	case "status":
		os.Exit(cmdStatus(rest))
	case "history":
		os.Exit(cmdHistory(rest))
	case "analyze":
		os.Exit(cmdAnalyze(rest))
	case "version", "-v", "--version":
		fmt.Printf("ipwatcher %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `ipwatcher %s — public IPv4 collector and subnet analyzer

Usage:
  ipwatcher run      Start long-running collector
  ipwatcher status   Show current IP and last change
  ipwatcher history  Show IP change history
  ipwatcher analyze  Show prefix distribution and lifecycle stats
  ipwatcher version  Print version

Global flags (after subcommand):
  -config string   Path to YAML config (default: ./config.yaml)

Environment overrides:
  IPWATCHER_INTERVAL, IPWATCHER_TIMEOUT, IPWATCHER_RETRIES,
  IPWATCHER_RETRY_DELAY, IPWATCHER_DB_PATH, IPWATCHER_LOG_LEVEL,
  IPWATCHER_PROVIDERS (comma-separated URLs)
`, version)
}

func loadConfig(args []string) (config.Config, error) {
	fs := flag.NewFlagSet("ipwatcher", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	return config.Load(*cfgPath)
}

func openStore(ctx context.Context, cfg config.Config) (*storage.Store, error) {
	return storage.Open(ctx, cfg.Database.Path)
}

func cmdRun(args []string) int {
	cfg, err := loadConfig(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	log, err := logger.New(cfg.Logging.Level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger error: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := openStore(ctx, cfg)
	if err != nil {
		log.Error("failed to initialize database", "error", err.Error())
		return 1
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			log.Error("failed to close database", "error", cerr.Error())
		}
	}()

	client := provider.DefaultClient(cfg.Collector.Timeout)
	provs := provider.BuildProviders(cfg.Providers, client)
	fo := provider.NewFailover(provs...)

	col := collector.New(collector.Options{
		Interval:   cfg.Collector.Interval,
		Timeout:    cfg.Collector.Timeout,
		Retries:    cfg.Collector.Retries,
		RetryDelay: cfg.Collector.RetryDelay,
	}, fo, store, log)

	log.Info("ipwatcher started",
		"interval", cfg.Collector.Interval.String(),
		"db", cfg.Database.Path,
		"providers", len(cfg.Providers),
	)
	if err := col.Run(ctx); err != nil {
		log.Error("collector exited with error", "error", err.Error())
		return 1
	}
	log.Info("ipwatcher stopped")
	return 0
}

func cmdStatus(args []string) int {
	cfg, err := loadConfig(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	ctx := context.Background()
	store, err := openStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize database: %v\n", err)
		return 1
	}
	defer store.Close()

	st, err := store.Status(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read status: %v\n", err)
		return 1
	}
	if !st.HasData {
		fmt.Println("No observations yet. Start `ipwatcher run` to begin collecting.")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	ip := "(none)"
	if st.CurrentIP.IsValid() {
		ip = st.CurrentIP.String()
	}
	fmt.Fprintf(w, "Current IP:\t%s\n", ip)
	fmt.Fprintf(w, "Last Check:\t%s\n", formatLocal(st.LastCheck))
	if !st.LastIPChange.IsZero() {
		fmt.Fprintf(w, "Last IP Change:\t%s\n", formatLocal(st.LastIPChange))
		fmt.Fprintf(w, "Current Duration:\t%s\n", analyzer.FormatDuration(st.CurrentDuration))
	} else {
		fmt.Fprintf(w, "Last IP Change:\t(none recorded)\n")
		if st.CurrentDuration > 0 {
			fmt.Fprintf(w, "Current Duration:\t%s\n", analyzer.FormatDuration(st.CurrentDuration))
		}
	}
	w.Flush()
	return 0
}

func cmdHistory(args []string) int {
	limit := 50
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	fs.IntVar(&limit, "limit", 50, "max rows to display (0 = all)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	ctx := context.Background()
	store, err := openStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize database: %v\n", err)
		return 1
	}
	defer store.Close()

	changes, err := store.Changes(ctx, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read history: %v\n", err)
		return 1
	}
	if len(changes) == 0 {
		fmt.Println("No IP changes recorded yet.")
		return 0
	}

	// Print oldest → newest for readability.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tOLD IP\tNEW IP")
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		fmt.Fprintf(w, "%s\t%s\t%s\n",
			formatLocalShort(c.ChangedAt),
			c.OldIP.String(),
			c.NewIP.String(),
		)
	}
	w.Flush()
	return 0
}

func cmdAnalyze(args []string) int {
	cfg, err := loadConfig(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	ctx := context.Background()
	store, err := openStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize database: %v\n", err)
		return 1
	}
	defer store.Close()

	rep, err := analyzer.Analyze(ctx, store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to analyze: %v\n", err)
		return 1
	}
	if !rep.HasData {
		fmt.Println("No successful observations yet. Start `ipwatcher run` to begin collecting.")
		return 0
	}

	fmt.Printf("Observation Period: %.1f days\n", rep.ObservationDays)
	fmt.Printf("IP Changes:         %d\n", rep.ChangeCount)
	fmt.Printf("Unique IPs:         %d\n", rep.UniqueIPs)
	fmt.Println()

	for _, bits := range []int{24, 23, 22, 21, 20} {
		stats := rep.Prefixes[bits]
		if len(stats) == 0 {
			continue
		}
		fmt.Printf("/%d Distribution (observed):\n\n", bits)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, s := range stats {
			fmt.Fprintf(w, "%s\t%.0f%%\t(%d IPs)\n", s.Prefix.String(), s.Pct, s.Count)
		}
		w.Flush()
		fmt.Println()
	}

	lc := rep.Lifecycle
	fmt.Println("IP Lifecycle:")
	fmt.Printf("  Change count:     %d\n", lc.ChangeCount)
	fmt.Printf("  Avg lifetime:     %s\n", analyzer.FormatDuration(lc.AvgLifetime))
	fmt.Printf("  Max lifetime:     %s\n", analyzer.FormatDuration(lc.MaxLifetime))
	fmt.Printf("  Min lifetime:     %s\n", analyzer.FormatDuration(lc.MinLifetime))
	if lc.ObservationDays >= 1 {
		fmt.Printf("  Daily change rate: %.2f / day\n", lc.DailyChangeRate)
	}
	fmt.Println()
	if rep.Insufficient {
		fmt.Println("Note: observation window is under 30 days. Treat candidate prefixes as indicative only.")
	}
	fmt.Println("These are observed/candidate prefixes from historical data — not a guarantee of ISP allocation.")
	return 0
}

func formatLocal(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05 -07:00")
}

func formatLocalShort(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}
