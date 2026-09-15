package collector

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"ipwatcher/internal/config"
	"ipwatcher/internal/notify"
	"ipwatcher/internal/provider"
	"ipwatcher/internal/storage"
)

// Options controls a Collector run loop.
type Options struct {
	Interval   time.Duration
	Timeout    time.Duration
	Retries    int
	RetryDelay time.Duration
	// ExtraIgnore merges config/env ignore entries with DB-managed rules.
	ExtraIgnore []string
}

// Collector runs the periodic public-IP detection loop.
// IPv4 is required each tick; IPv6 is best-effort when fo6 is set.
type Collector struct {
	opts   Options
	fo4    *provider.Failover
	fo6    *provider.Failover // nil disables IPv6 collection
	store  *storage.Store
	log    *slog.Logger
	notify *notify.Webhook
}

// New builds a Collector for IPv4. fo6 may be nil.
func New(opts Options, fo4, fo6 *provider.Failover, store *storage.Store, log *slog.Logger, hook *notify.Webhook) *Collector {
	if log == nil {
		log = slog.Default()
	}
	return &Collector{
		opts:   opts,
		fo4:    fo4,
		fo6:    fo6,
		store:  store,
		log:    log,
		notify: hook,
	}
}

// Run ticks until ctx is cancelled. Returns nil on graceful stop.
func (c *Collector) Run(ctx context.Context) error {
	// Run immediately on start so the tool is useful right away.
	c.tickOnce(ctx)

	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.log.Info("collector stopping")
			return nil
		case <-ticker.C:
			c.tickOnce(ctx)
		}
	}
}

// TickOnce exposes a single detection cycle (used by tests and Run).
func (c *Collector) TickOnce(ctx context.Context) {
	c.tickOnce(ctx)
}

func (c *Collector) tickOnce(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	// Writes must finish even if the process is stopping (SIGINT/SIGTERM).
	writeCtx := context.WithoutCancel(ctx)

	c.refreshIgnoreFilter(writeCtx)

	c.collectFamily(writeCtx, storage.Family4, c.fo4, true)
	if c.fo6 != nil {
		c.collectFamily(writeCtx, storage.Family6, c.fo6, false)
	}
}

// refreshIgnoreFilter reloads DB-managed rules (+ config extras) each tick so
// `ipwatcher ignore add` applies without restarting the collector.
func (c *Collector) refreshIgnoreFilter(ctx context.Context) {
	if c.fo4 == nil {
		return
	}
	dbRules, err := c.store.IgnoreRuleStrings(ctx)
	if err != nil {
		c.log.Warn("failed to load ignore rules from database", "error", err.Error())
		dbRules = nil
	}
	entries := make([]string, 0, len(dbRules)+len(c.opts.ExtraIgnore))
	entries = append(entries, dbRules...)
	entries = append(entries, c.opts.ExtraIgnore...)
	filter, err := config.ParseIgnoreFilter(entries)
	if err != nil {
		c.log.Warn("invalid ignore rule, skipping refresh", "error", err.Error())
		return
	}
	c.fo4.WithFilter(filter)
	if c.fo6 != nil {
		c.fo6.WithFilter(filter)
	}
}

func (c *Collector) collectFamily(ctx context.Context, family byte, fo *provider.Failover, required bool) {
	label := "ipv4"
	if family == storage.Family6 {
		label = "ipv6"
	}

	obs := storage.Observation{
		ObservedAt: time.Now().UTC(),
		Provider:   "",
		Family:     family,
	}

	attempts := 1
	if required {
		attempts = c.opts.Retries + 1
	}

	var (
		addr    netip.Addr
		prov    provider.Provider
		latency int64
		lastErr error
	)

	for i := 0; i < attempts; i++ {
		if i > 0 && c.opts.RetryDelay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.opts.RetryDelay):
			}
		}
		reqCtx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
		start := time.Now()
		var err error
		addr, prov, err = fo.Lookup(reqCtx)
		latency = time.Since(start).Milliseconds()
		cancel()
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		c.log.Warn("public ip query failed", "family", label, "attempt", i+1, "error", err.Error())
	}

	obs.LatencyMS = latency

	if lastErr != nil {
		if required {
			obs.Success = false
			obs.Error = lastErr.Error()
			if werr := c.store.InsertObservation(ctx, obs); werr != nil {
				c.log.Error("database write failed", "error", werr.Error())
			}
			c.log.Warn("public ip query failed", "family", label, "error", lastErr.Error())
		} else {
			// IPv6 is best-effort: silence expected failures (no AAAA / NAT64 / etc).
			c.log.Debug("ipv6 lookup skipped", "error", lastErr.Error())
		}
		return
	}

	obs.Success = true
	obs.IP = addr
	obs.Provider = prov.Name()

	if werr := c.store.InsertObservation(ctx, obs); werr != nil {
		c.log.Error("database write failed", "error", werr.Error())
		return
	}

	c.log.Info("public ip checked",
		"family", label,
		"ip", addr.String(),
		"provider", prov.Name(),
		"latency", fmt.Sprintf("%dms", latency),
	)

	if err := c.detectChange(ctx, family, label, addr, obs.ObservedAt); err != nil {
		c.log.Error("failed to record IP change", "family", label, "error", err.Error())
	}
}

func (c *Collector) detectChange(ctx context.Context, family byte, label string, newIP netip.Addr, at time.Time) error {
	prev, err := c.store.LatestSuccessExcluding(ctx, at, family)
	if err != nil {
		return err
	}
	if prev == nil || !prev.IP.IsValid() {
		// First observation or nothing to compare — not a change event.
		return nil
	}
	if prev.IP == newIP {
		return nil
	}

	ch := storage.Change{
		ChangedAt: at,
		OldIP:     prev.IP,
		NewIP:     newIP,
		Family:    family,
	}
	if err := c.store.InsertChange(ctx, ch); err != nil {
		return err
	}
	c.log.Info("public ip changed",
		"family", label,
		"old", prev.IP.String(),
		"new", newIP.String(),
	)

	if c.notify != nil {
		ev := notify.Event{
			Family:    label,
			OldIP:     prev.IP.String(),
			NewIP:     newIP.String(),
			ChangedAt: at,
		}
		if nerr := c.notify.Send(ctx, ev); nerr != nil {
			c.log.Warn("webhook notify failed", "error", nerr.Error())
		}
	}
	return nil
}
