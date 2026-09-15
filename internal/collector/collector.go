package collector

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"ipwatcher/internal/provider"
	"ipwatcher/internal/storage"
)

// Options controls a Collector run loop.
type Options struct {
	Interval   time.Duration
	Timeout    time.Duration
	Retries    int
	RetryDelay time.Duration
}

// Collector runs the periodic public-IP detection loop.
type Collector struct {
	opts     Options
	failover *provider.Failover
	store    *storage.Store
	log      *slog.Logger
}

// New builds a Collector.
func New(opts Options, failover *provider.Failover, store *storage.Store, log *slog.Logger) *Collector {
	if log == nil {
		log = slog.Default()
	}
	return &Collector{
		opts:     opts,
		failover: failover,
		store:    store,
		log:      log,
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
	addr, prov, latency, err := c.lookupWithRetry(ctx)

	obs := storage.Observation{
		ObservedAt: time.Now().UTC(),
		Provider:   "",
		LatencyMS:  latency,
	}

	// Writes must finish even if the process is stopping (SIGINT/SIGTERM).
	writeCtx := context.WithoutCancel(ctx)

	if err != nil {
		obs.Success = false
		obs.Error = err.Error()
		if werr := c.store.InsertObservation(writeCtx, obs); werr != nil {
			c.log.Error("database write failed", "error", werr.Error())
		}
		c.log.Warn("public ip query failed", "error", err.Error())
		return
	}

	obs.Success = true
	obs.IP = addr
	obs.Provider = prov.Name()

	if werr := c.store.InsertObservation(writeCtx, obs); werr != nil {
		c.log.Error("database write failed", "error", werr.Error())
		return
	}

	c.log.Info("public ip checked",
		"ip", addr.String(),
		"provider", prov.Name(),
		"latency", fmt.Sprintf("%dms", latency),
	)

	if err := c.detectChange(writeCtx, addr, obs.ObservedAt); err != nil {
		c.log.Error("failed to record IP change", "error", err.Error())
	}
}

func (c *Collector) lookupWithRetry(ctx context.Context) (netip.Addr, provider.Provider, int64, error) {
	attempts := c.opts.Retries + 1
	var lastErr error
	var latencyMS int64
	for i := 0; i < attempts; i++ {
		if i > 0 && c.opts.RetryDelay > 0 {
			select {
			case <-ctx.Done():
				return netip.Addr{}, nil, 0, ctx.Err()
			case <-time.After(c.opts.RetryDelay):
			}
		}

		reqCtx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
		start := time.Now()
		addr, prov, err := c.failover.Lookup(reqCtx)
		latencyMS = time.Since(start).Milliseconds()
		cancel()
		if err == nil {
			return addr, prov, latencyMS, nil
		}
		lastErr = err
		c.log.Warn("public ip query failed", "attempt", i+1, "error", err.Error())
	}
	return netip.Addr{}, nil, latencyMS, lastErr
}

func (c *Collector) detectChange(ctx context.Context, newIP netip.Addr, at time.Time) error {
	prev, err := c.store.LatestSuccessExcluding(ctx, at, newIP)
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
	}
	if err := c.store.InsertChange(ctx, ch); err != nil {
		return err
	}
	c.log.Info("public ip changed",
		"old", prev.IP.String(),
		"new", newIP.String(),
	)
	return nil
}
