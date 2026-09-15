package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"ipwatcher/internal/analyzer"
	"ipwatcher/internal/collector"
	"ipwatcher/internal/config"
	"ipwatcher/internal/logger"
	"ipwatcher/internal/notify"
	"ipwatcher/internal/provider"
	"ipwatcher/internal/storage"
)

// version can be overridden at build time via -ldflags "-X main.version=..."
var version = "0.2.0"

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
	case "purge":
		os.Exit(cmdPurge(rest))
	case "ignore":
		os.Exit(cmdIgnore(rest))
	case "last":
		os.Exit(cmdLast(rest))
	case "check":
		os.Exit(cmdCheck(rest))
	case "export":
		os.Exit(cmdExport(rest))
	case "config":
		os.Exit(cmdConfigShow(rest))
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
	fmt.Fprintf(os.Stderr, `ipwatcher %s — public IP collector and subnet analyzer (IPv4 + optional IPv6)

Usage:
  ipwatcher run      Start long-running collector
  ipwatcher status   Show current IPv4/IPv6 and last changes
  ipwatcher last     Print current IPs (one-shot, script-friendly)
  ipwatcher history  Show IP change history
  ipwatcher analyze  Show prefix distribution and lifecycle stats
  ipwatcher purge    Remove observations/changes for bad IPs or CIDRs
  ipwatcher ignore   List/add/remove permanent ignore rules (stored in DB)
  ipwatcher check    Probe configured providers once
  ipwatcher export   Dump observations or changes as CSV/JSON
  ipwatcher config   Show effective runtime config
  ipwatcher version  Print version

Global flags (after subcommand):
  -config string   Path to YAML config (default: ./config.yaml)
  -json            JSON output (status/history/analyze/last)

ignore subcommands:
  ipwatcher ignore list
  ipwatcher ignore add <ip|cidr> [more...]
  ipwatcher ignore remove <ip|cidr> [more...]

purge flags:
  -ip string       IP(s) to delete, comma-separated
  -cidr string     CIDR(s) to delete, comma-separated
  -also-ignore     Also add the same targets to the permanent ignore list
  -dry-run         Show what would be deleted without writing

export flags:
  -table string    observations | changes (default observations)
  -format string   csv | json (default csv)
  -out string      output file (default stdout)
  -limit int       max rows (0 = all)

Environment overrides:
  IPWATCHER_INTERVAL, IPWATCHER_TIMEOUT, IPWATCHER_RETRIES,
  IPWATCHER_RETRY_DELAY, IPWATCHER_DB_PATH, IPWATCHER_LOG_LEVEL,
  IPWATCHER_PROVIDERS, IPWATCHER_PROVIDERS_V6,
  IPWATCHER_IGNORE_IPS (extra IP/CIDR, merged with DB rules),
  IPWATCHER_WEBHOOK_URL, IPWATCHER_WEBHOOK_TIMEOUT
`, version)
}

func loadConfig(args []string) (config.Config, []string, error) {
	fs := flag.NewFlagSet("ipwatcher", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, nil, err
	}
	cfg, err := config.Load(*cfgPath)
	return cfg, fs.Args(), err
}

func openStore(ctx context.Context, cfg config.Config) (*storage.Store, error) {
	return storage.Open(ctx, cfg.Database.Path)
}

// buildMergedIgnore combines DB-managed rules with config/env extras.
func buildMergedIgnore(ctx context.Context, store *storage.Store, cfg config.Config) (*config.IgnoreFilter, error) {
	dbRules, err := store.IgnoreRuleStrings(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]string, 0, len(dbRules)+len(cfg.IgnoreIPs))
	entries = append(entries, dbRules...)
	entries = append(entries, cfg.IgnoreIPs...)
	return config.ParseIgnoreFilter(entries)
}

func buildFilters(ctx context.Context, store *storage.Store, cfg config.Config) (*config.IgnoreFilter, *provider.Failover, *provider.Failover, error) {
	filter, err := buildMergedIgnore(ctx, store, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	client4 := provider.DefaultClient(cfg.Collector.Timeout)
	fo4 := provider.NewFailover(provider.BuildProviders(cfg.Providers, client4)...).WithFilter(filter)

	var fo6 *provider.Failover
	if len(cfg.Providers6) > 0 {
		client6 := provider.DefaultClient6(cfg.Collector.Timeout)
		fo6 = provider.NewFailover(provider.BuildProviders6(cfg.Providers6, client6)...).WithFilter(filter)
	}
	return filter, fo4, fo6, nil
}

func cmdRun(args []string) int {
	cfg, _, err := loadConfig(args)
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

	filter, fo4, fo6, err := buildFilters(ctx, store, cfg)
	if err != nil {
		log.Error("invalid ignore rules", "error", err.Error())
		return 1
	}
	hook := notify.New(cfg.Notify.WebhookURL, cfg.Notify.Timeout)

	col := collector.New(collector.Options{
		Interval:    cfg.Collector.Interval,
		Timeout:     cfg.Collector.Timeout,
		Retries:     cfg.Collector.Retries,
		RetryDelay:  cfg.Collector.RetryDelay,
		ExtraIgnore: cfg.IgnoreIPs,
	}, fo4, fo6, store, log, hook)

	log.Info("ipwatcher started",
		"interval", cfg.Collector.Interval.String(),
		"db", cfg.Database.Path,
		"providers", len(cfg.Providers),
		"providers_v6", len(cfg.Providers6),
		"ignore_ips", filter.Len(),
		"webhook", hook.URL() != "",
	)
	if err := col.Run(ctx); err != nil {
		log.Error("collector exited with error", "error", err.Error())
		return 1
	}
	log.Info("ipwatcher stopped")
	return 0
}

type statusJSON struct {
	CurrentIPv4    string `json:"current_ipv4,omitempty"`
	CurrentIPv6    string `json:"current_ipv6,omitempty"`
	LastCheck      string `json:"last_check,omitempty"`
	LastIPv4Change string `json:"last_ipv4_change,omitempty"`
	LastIPv6Change string `json:"last_ipv6_change,omitempty"`
	IPv4Duration   string `json:"ipv4_duration,omitempty"`
	IPv6Duration   string `json:"ipv6_duration,omitempty"`
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	asJSON := fs.Bool("json", false, "JSON output")
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
	defer func() { _ = store.Close() }()

	st, err := store.Status(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read status: %v\n", err)
		return 1
	}
	if !st.HasData {
		if *asJSON {
			_ = json.NewEncoder(os.Stdout).Encode(statusJSON{})
		} else {
			fmt.Println("No observations yet. Start `ipwatcher run` to begin collecting.")
		}
		return 0
	}

	if *asJSON {
		out := statusJSON{}
		if st.CurrentIPv4.IsValid() {
			out.CurrentIPv4 = st.CurrentIPv4.String()
		}
		if st.CurrentIPv6.IsValid() {
			out.CurrentIPv6 = st.CurrentIPv6.String()
		}
		if !st.LastCheck.IsZero() {
			out.LastCheck = st.LastCheck.Local().Format(time.RFC3339)
		}
		if !st.LastIPv4Change.IsZero() {
			out.LastIPv4Change = st.LastIPv4Change.Local().Format(time.RFC3339)
			out.IPv4Duration = analyzer.FormatDuration(st.CurrentIPv4Dur)
		}
		if !st.LastIPv6Change.IsZero() {
			out.LastIPv6Change = st.LastIPv6Change.Local().Format(time.RFC3339)
			out.IPv6Duration = analyzer.FormatDuration(st.CurrentIPv6Dur)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	ip4 := "(none)"
	if st.CurrentIPv4.IsValid() {
		ip4 = st.CurrentIPv4.String()
	}
	ip6 := "(not collected)"
	if st.CurrentIPv6.IsValid() {
		ip6 = st.CurrentIPv6.String()
	}
	fmt.Fprintf(w, "Current IPv4:\t%s\n", ip4)
	fmt.Fprintf(w, "Current IPv6:\t%s\n", ip6)
	fmt.Fprintf(w, "Last Check:\t%s\n", formatLocal(st.LastCheck))
	if !st.LastIPv4Change.IsZero() {
		fmt.Fprintf(w, "Last IPv4 Change:\t%s\n", formatLocal(st.LastIPv4Change))
		fmt.Fprintf(w, "IPv4 Duration:\t%s\n", analyzer.FormatDuration(st.CurrentIPv4Dur))
	} else {
		fmt.Fprintf(w, "Last IPv4 Change:\t(none recorded)\n")
		if st.CurrentIPv4Dur > 0 {
			fmt.Fprintf(w, "IPv4 Duration:\t%s\n", analyzer.FormatDuration(st.CurrentIPv4Dur))
		}
	}
	if st.HasIPv6 || !st.LastIPv6Change.IsZero() {
		if !st.LastIPv6Change.IsZero() {
			fmt.Fprintf(w, "Last IPv6 Change:\t%s\n", formatLocal(st.LastIPv6Change))
			fmt.Fprintf(w, "IPv6 Duration:\t%s\n", analyzer.FormatDuration(st.CurrentIPv6Dur))
		} else {
			fmt.Fprintf(w, "Last IPv6 Change:\t(none recorded)\n")
		}
	}
	w.Flush()
	return 0
}

func cmdLast(args []string) int {
	fs := flag.NewFlagSet("last", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	asJSON := fs.Bool("json", false, "JSON output")
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
	defer func() { _ = store.Close() }()

	v4, err := store.LatestSuccess(ctx, storage.Family4)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read last IPv4: %v\n", err)
		return 1
	}
	v6, err := store.LatestSuccess(ctx, storage.Family6)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read last IPv6: %v\n", err)
		return 1
	}

	if *asJSON {
		out := map[string]string{}
		if v4 != nil {
			out["ipv4"] = v4.IP.String()
		}
		if v6 != nil {
			out["ipv6"] = v6.IP.String()
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return 0
	}
	if v4 != nil {
		fmt.Printf("ipv4 %s\n", v4.IP)
	}
	if v6 != nil {
		fmt.Printf("ipv6 %s\n", v6.IP)
	}
	if v4 == nil && v6 == nil {
		fmt.Println("(no successful observations)")
		return 1
	}
	return 0
}

func cmdHistory(args []string) int {
	limit := 50
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	asJSON := fs.Bool("json", false, "JSON output")
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
	defer func() { _ = store.Close() }()

	changes, err := store.Changes(ctx, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read history: %v\n", err)
		return 1
	}

	if *asJSON {
		type row struct {
			Time   string `json:"time"`
			Family string `json:"family"`
			OldIP  string `json:"old_ip"`
			NewIP  string `json:"new_ip"`
		}
		out := make([]row, 0, len(changes))
		for i := len(changes) - 1; i >= 0; i-- {
			c := changes[i]
			fam := "ipv4"
			if c.Family == storage.Family6 {
				fam = "ipv6"
			}
			out = append(out, row{
				Time:   c.ChangedAt.Local().Format(time.RFC3339),
				Family: fam,
				OldIP:  c.OldIP.String(),
				NewIP:  c.NewIP.String(),
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return 0
	}

	if len(changes) == 0 {
		fmt.Println("No IP changes recorded yet.")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tFAMILY\tOLD IP\tNEW IP")
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		fam := "ipv4"
		if c.Family == storage.Family6 {
			fam = "ipv6"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			formatLocalShort(c.ChangedAt),
			fam,
			c.OldIP.String(),
			c.NewIP.String(),
		)
	}
	w.Flush()
	return 0
}

func cmdAnalyze(args []string) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	asJSON := fs.Bool("json", false, "JSON output")
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
	defer func() { _ = store.Close() }()

	rep, err := analyzer.Analyze(ctx, store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to analyze: %v\n", err)
		return 1
	}
	if !rep.HasData {
		if *asJSON {
			fmt.Println("{}")
		} else {
			fmt.Println("No successful observations yet. Start `ipwatcher run` to begin collecting.")
		}
		return 0
	}

	if *asJSON {
		type pstat struct {
			Prefix string  `json:"prefix"`
			Count  int     `json:"count"`
			Pct    float64 `json:"pct"`
		}
		type fam struct {
			UniqueIPs   int                `json:"unique_ips"`
			ChangeCount int                `json:"change_count"`
			Prefixes    map[string][]pstat `json:"prefixes"`
			AvgLifetime string             `json:"avg_lifetime,omitempty"`
			MinLifetime string             `json:"min_lifetime,omitempty"`
			MaxLifetime string             `json:"max_lifetime,omitempty"`
		}
		conv := func(fr analyzer.FamilyReport) fam {
			out := fam{UniqueIPs: fr.UniqueIPs, ChangeCount: fr.ChangeCount, Prefixes: map[string][]pstat{}}
			for bits, stats := range fr.Prefixes {
				key := fmt.Sprintf("/%d", bits)
				for _, s := range stats {
					out.Prefixes[key] = append(out.Prefixes[key], pstat{
						Prefix: s.Prefix.String(),
						Count:  s.Count,
						Pct:    s.Pct,
					})
				}
			}
			if fr.UniqueIPs > 0 {
				out.AvgLifetime = analyzer.FormatDuration(fr.Lifecycle.AvgLifetime)
				out.MinLifetime = analyzer.FormatDuration(fr.Lifecycle.MinLifetime)
				out.MaxLifetime = analyzer.FormatDuration(fr.Lifecycle.MaxLifetime)
			}
			return out
		}
		payload := map[string]any{
			"observation_days": rep.ObservationDays,
			"insufficient":     rep.Insufficient,
			"ipv4":             conv(rep.IPv4),
		}
		if rep.IPv6.HasData {
			payload["ipv6"] = conv(rep.IPv6)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return 0
	}

	fmt.Printf("Observation Period: %.1f days\n", rep.ObservationDays)
	printFamilyBlock("IPv4", rep.IPv4, []int{24, 23, 22, 21, 20})
	if rep.IPv6.HasData {
		printFamilyBlock("IPv6", rep.IPv6, []int{64, 56, 48, 32})
	}
	if rep.Insufficient {
		fmt.Println("Note: observation window is under 30 days. Treat candidate prefixes as indicative only.")
	}
	fmt.Println("These are observed/candidate prefixes from historical data — not a guarantee of ISP allocation.")
	return 0
}

func printFamilyBlock(label string, fr analyzer.FamilyReport, bitsList []int) {
	fmt.Printf("\n%s\n", label)
	fmt.Printf("  IP Changes:       %d\n", fr.ChangeCount)
	fmt.Printf("  Unique IPs:       %d\n", fr.UniqueIPs)
	for _, bits := range bitsList {
		stats := fr.Prefixes[bits]
		if len(stats) == 0 {
			continue
		}
		fmt.Printf("  /%d Distribution (observed):\n\n", bits)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, s := range stats {
			fmt.Fprintf(w, "    %s\t%.0f%%\t(%d IPs)\n", s.Prefix.String(), s.Pct, s.Count)
		}
		w.Flush()
		fmt.Println()
	}
	lc := fr.Lifecycle
	fmt.Println("  IP Lifecycle:")
	fmt.Printf("    Change count:     %d\n", lc.ChangeCount)
	fmt.Printf("    Avg lifetime:     %s\n", analyzer.FormatDuration(lc.AvgLifetime))
	fmt.Printf("    Max lifetime:     %s\n", analyzer.FormatDuration(lc.MaxLifetime))
	fmt.Printf("    Min lifetime:     %s\n", analyzer.FormatDuration(lc.MinLifetime))
	if lc.ObservationDays >= 1 {
		fmt.Printf("    Daily change rate: %.2f / day\n", lc.DailyChangeRate)
	}
}

func cmdPurge(args []string) int {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	ipList := fs.String("ip", "", "IP address(es) to delete, comma-separated")
	cidrList := fs.String("cidr", "", "CIDR prefix(es) to delete, comma-separated")
	alsoIgnore := fs.Bool("also-ignore", false, "also add targets to the permanent ignore list")
	dryRun := fs.Bool("dry-run", false, "show what would be deleted without writing")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ips := parseAddrList(*ipList)
	prefixes := parsePrefixList(*cidrList)
	if len(ips) == 0 && len(prefixes) == 0 {
		fmt.Fprintln(os.Stderr, "purge requires -ip and/or -cidr")
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
	defer func() { _ = store.Close() }()

	if *dryRun {
		fmt.Println("dry-run: no rows will be deleted")
		for _, ip := range ips {
			fmt.Printf("would purge IP %s\n", ip)
		}
		for _, p := range prefixes {
			fmt.Printf("would purge CIDR %s\n", p)
		}
		if *alsoIgnore {
			fmt.Println("would add the same targets to ignore_rules")
		}
		return 0
	}

	var totalObs, totalChg int64
	for _, ip := range ips {
		obs, chg, err := store.DeleteIP(ctx, ip)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to purge %s: %v\n", ip, err)
			return 1
		}
		totalObs += obs
		totalChg += chg
		fmt.Printf("purged %s: %d observation(s), %d change(s)\n", ip, obs, chg)
	}
	for _, p := range prefixes {
		obs, chg, err := store.DeletePrefix(ctx, p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to purge %s: %v\n", p, err)
			return 1
		}
		totalObs += obs
		totalChg += chg
		fmt.Printf("purged %s: %d observation(s), %d change(s)\n", p, obs, chg)
	}
	fmt.Printf("total: %d observation(s), %d change(s) removed\n", totalObs, totalChg)

	if *alsoIgnore {
		for _, ip := range ips {
			added, err := store.AddIgnoreRule(ctx, ip.String())
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to ignore %s: %v\n", ip, err)
				return 1
			}
			if added {
				fmt.Printf("ignored %s (permanent)\n", ip)
			} else {
				fmt.Printf("ignored %s (already present)\n", ip)
			}
		}
		for _, p := range prefixes {
			added, err := store.AddIgnoreRule(ctx, p.String())
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to ignore %s: %v\n", p, err)
				return 1
			}
			if added {
				fmt.Printf("ignored %s (permanent)\n", p)
			} else {
				fmt.Printf("ignored %s (already present)\n", p)
			}
		}
	}
	return 0
}

func cmdIgnore(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ipwatcher ignore list|add|remove ...")
		return 2
	}
	sub := args[0]
	rest := args[1:]

	flagArgs, targets := splitCLIArgs(rest, map[string]bool{"config": true, "json": false})

	fs := flag.NewFlagSet("ignore", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	asJSON := fs.Bool("json", false, "JSON output (list)")
	if err := fs.Parse(flagArgs); err != nil {
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
	defer func() { _ = store.Close() }()

	switch sub {
	case "list":
		rules, err := store.ListIgnoreRules(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to list ignore rules: %v\n", err)
			return 1
		}
		if *asJSON {
			type row struct {
				Rule      string `json:"rule"`
				CreatedAt string `json:"created_at"`
			}
			out := make([]row, 0, len(rules))
			for _, r := range rules {
				out = append(out, row{Rule: r.Rule, CreatedAt: r.CreatedAt.Local().Format(time.RFC3339)})
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
			return 0
		}
		if len(rules) == 0 {
			fmt.Println("No permanent ignore rules. Add with: ipwatcher ignore add <ip|cidr>")
			if len(cfg.IgnoreIPs) > 0 {
				fmt.Println("Config/env extras (not stored in DB):")
				for _, e := range cfg.IgnoreIPs {
					fmt.Printf("  %s\n", e)
				}
			}
			return 0
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "RULE\tCREATED")
		for _, r := range rules {
			fmt.Fprintf(w, "%s\t%s\n", r.Rule, r.CreatedAt.Local().Format("2006-01-02 15:04"))
		}
		w.Flush()
		if len(cfg.IgnoreIPs) > 0 {
			fmt.Println("\nConfig/env extras (merged at runtime, not in DB):")
			for _, e := range cfg.IgnoreIPs {
				fmt.Printf("  %s\n", e)
			}
		}
		return 0
	case "add":
		if len(targets) == 0 {
			fmt.Fprintln(os.Stderr, "ignore add requires at least one IP or CIDR")
			return 2
		}
		for _, t := range targets {
			if _, err := config.ParseIPOrPrefix(t); err != nil {
				fmt.Fprintf(os.Stderr, "invalid IP/CIDR %q: %v\n", t, err)
				return 2
			}
			added, err := store.AddIgnoreRule(ctx, t)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to add %s: %v\n", t, err)
				return 1
			}
			if added {
				fmt.Printf("added %s\n", t)
			} else {
				fmt.Printf("already present %s\n", t)
			}
		}
		fmt.Println("Collector picks this up on the next tick (no restart needed).")
		return 0
	case "remove", "rm", "del":
		if len(targets) == 0 {
			fmt.Fprintln(os.Stderr, "ignore remove requires at least one IP or CIDR")
			return 2
		}
		for _, t := range targets {
			removed, err := store.RemoveIgnoreRule(ctx, t)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to remove %s: %v\n", t, err)
				return 1
			}
			if removed {
				fmt.Printf("removed %s\n", t)
			} else {
				fmt.Printf("not found %s\n", t)
			}
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown ignore subcommand %q (want list|add|remove)\n", sub)
		return 2
	}
}

// splitCLIArgs separates flags (with optional values) from positional args
// so flags may appear before or after targets.
func splitCLIArgs(args []string, valueFlags map[string]bool) (flagArgs, posArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			posArgs = append(posArgs, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			posArgs = append(posArgs, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			continue
		}
		if valueFlags[name] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flagArgs = append(flagArgs, args[i+1])
			i++
		}
	}
	return flagArgs, posArgs
}

func cmdConfigShow(args []string) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// support: ipwatcher config show
	if sub := fs.Args(); len(sub) > 0 && sub[0] != "show" {
		fmt.Fprintf(os.Stderr, "usage: ipwatcher config [-json]\n")
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
	defer func() { _ = store.Close() }()
	dbRules, _ := store.IgnoreRuleStrings(ctx)
	filter, err := buildMergedIgnore(ctx, store, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ignore rules error: %v\n", err)
		return 1
	}

	if *asJSON {
		payload := map[string]any{
			"db_path":      cfg.Database.Path,
			"interval":     cfg.Collector.Interval.String(),
			"timeout":      cfg.Collector.Timeout.String(),
			"retries":      cfg.Collector.Retries,
			"retry_delay":  cfg.Collector.RetryDelay.String(),
			"log_level":    cfg.Logging.Level,
			"providers":    cfg.Providers,
			"providers_v6": cfg.Providers6,
			"ignore_db":    dbRules,
			"ignore_extra": cfg.IgnoreIPs,
			"ignore_total": filter.Len(),
			"webhook_set":  cfg.Notify.WebhookURL != "",
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Database:\t%s\n", cfg.Database.Path)
	fmt.Fprintf(w, "Interval:\t%s\n", cfg.Collector.Interval)
	fmt.Fprintf(w, "Timeout:\t%s\n", cfg.Collector.Timeout)
	fmt.Fprintf(w, "Retries:\t%d\n", cfg.Collector.Retries)
	fmt.Fprintf(w, "Log level:\t%s\n", cfg.Logging.Level)
	fmt.Fprintf(w, "Providers:\t%d\n", len(cfg.Providers))
	for _, p := range cfg.Providers {
		fmt.Fprintf(w, "  -\t%s\n", p)
	}
	fmt.Fprintf(w, "Providers v6:\t%d\n", len(cfg.Providers6))
	for _, p := range cfg.Providers6 {
		fmt.Fprintf(w, "  -\t%s\n", p)
	}
	fmt.Fprintf(w, "Ignore (DB):\t%d\n", len(dbRules))
	for _, r := range dbRules {
		fmt.Fprintf(w, "  -\t%s\n", r)
	}
	fmt.Fprintf(w, "Ignore (config/env):\t%d\n", len(cfg.IgnoreIPs))
	for _, r := range cfg.IgnoreIPs {
		fmt.Fprintf(w, "  -\t%s\n", r)
	}
	fmt.Fprintf(w, "Ignore (effective):\t%d\n", filter.Len())
	fmt.Fprintf(w, "Webhook:\t%s\n", map[bool]string{true: "configured", false: "disabled"}[cfg.Notify.WebhookURL != ""])
	w.Flush()
	return 0
}

func cmdCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
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
	defer func() { _ = store.Close() }()
	filter, _, _, err := buildFilters(ctx, store, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ignore rules error: %v\n", err)
		return 1
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "FAMILY\tPROVIDER\tRESULT\tDETAIL")

	failed := 0
	checkList := func(family string, urls []string, mk func(string) provider.Provider) {
		for _, u := range urls {
			p := mk(u)
			reqCtx, cancel := context.WithTimeout(ctx, cfg.Collector.Timeout)
			addr, err := p.Lookup(reqCtx)
			cancel()
			switch {
			case err != nil:
				failed++
				fmt.Fprintf(w, "%s\t%s\tFAIL\t%v\n", family, p.Name(), err)
			case filter.Contains(addr):
				fmt.Fprintf(w, "%s\t%s\tIGNORED\t%s matched ignore list\n", family, p.Name(), addr)
			default:
				fmt.Fprintf(w, "%s\t%s\tOK\t%s\n", family, p.Name(), addr)
			}
		}
	}

	client4 := provider.DefaultClient(cfg.Collector.Timeout)
	checkList("ipv4", cfg.Providers, func(u string) provider.Provider {
		return provider.NewHTTPProvider(u, client4)
	})
	if len(cfg.Providers6) > 0 {
		client6 := provider.DefaultClient6(cfg.Collector.Timeout)
		checkList("ipv6", cfg.Providers6, func(u string) provider.Provider {
			return provider.NewHTTPProvider6(u, client6)
		})
	}
	w.Flush()
	if failed > 0 {
		return 1
	}
	return 0
}

func cmdExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "path to YAML config file")
	table := fs.String("table", "observations", "observations | changes")
	format := fs.String("format", "csv", "csv | json")
	outPath := fs.String("out", "", "output file (default stdout)")
	limit := fs.Int("limit", 0, "max rows (0 = all)")
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
	defer func() { _ = store.Close() }()

	var w *os.File = os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create %s: %v\n", *outPath, err)
			return 1
		}
		defer f.Close()
		w = f
	}

	switch *table {
	case "observations":
		rows, err := store.Observations(ctx, *limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read observations: %v\n", err)
			return 1
		}
		// storage returns newest-first; export oldest-first for readability.
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
		if *format == "json" {
			type row struct {
				Time     string `json:"time"`
				IP       string `json:"ip,omitempty"`
				Family   string `json:"family"`
				Provider string `json:"provider"`
				Success  bool   `json:"success"`
				Latency  int64  `json:"latency_ms"`
				Error    string `json:"error,omitempty"`
			}
			out := make([]row, 0, len(rows))
			for _, o := range rows {
				fam := "ipv4"
				if o.Family == storage.Family6 {
					fam = "ipv6"
				}
				ip := ""
				if o.IP.IsValid() {
					ip = o.IP.String()
				}
				out = append(out, row{
					Time: o.ObservedAt.Format(time.RFC3339Nano), IP: ip, Family: fam,
					Provider: o.Provider, Success: o.Success, Latency: o.LatencyMS, Error: o.Error,
				})
			}
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
			return 0
		}
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"time", "ip", "family", "provider", "success", "latency_ms", "error"})
		for _, o := range rows {
			fam := "ipv4"
			if o.Family == storage.Family6 {
				fam = "ipv6"
			}
			ip := ""
			if o.IP.IsValid() {
				ip = o.IP.String()
			}
			_ = cw.Write([]string{
				o.ObservedAt.Format(time.RFC3339Nano), ip, fam, o.Provider,
				strconv.FormatBool(o.Success), strconv.FormatInt(o.LatencyMS, 10), o.Error,
			})
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			fmt.Fprintf(os.Stderr, "csv write failed: %v\n", err)
			return 1
		}
		return 0
	case "changes":
		rows, err := store.Changes(ctx, *limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read changes: %v\n", err)
			return 1
		}
		// oldest first for export readability
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
		if *format == "json" {
			type row struct {
				Time   string `json:"time"`
				Family string `json:"family"`
				OldIP  string `json:"old_ip"`
				NewIP  string `json:"new_ip"`
			}
			out := make([]row, 0, len(rows))
			for _, c := range rows {
				fam := "ipv4"
				if c.Family == storage.Family6 {
					fam = "ipv6"
				}
				out = append(out, row{
					Time: c.ChangedAt.Format(time.RFC3339Nano), Family: fam,
					OldIP: c.OldIP.String(), NewIP: c.NewIP.String(),
				})
			}
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
			return 0
		}
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"time", "family", "old_ip", "new_ip"})
		for _, c := range rows {
			fam := "ipv4"
			if c.Family == storage.Family6 {
				fam = "ipv6"
			}
			_ = cw.Write([]string{
				c.ChangedAt.Format(time.RFC3339Nano), fam, c.OldIP.String(), c.NewIP.String(),
			})
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			fmt.Fprintf(os.Stderr, "csv write failed: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown -table %q (want observations|changes)\n", *table)
		return 2
	}
}

func parseAddrList(s string) []netip.Addr {
	var out []netip.Addr
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping invalid IP %q\n", part)
			continue
		}
		out = append(out, addr.Unmap())
	}
	return out
}

func parsePrefixList(s string) []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := config.ParseIPOrPrefix(part)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping invalid CIDR %q\n", part)
			continue
		}
		out = append(out, p)
	}
	return out
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
