package analyzer

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"ipwatcher/internal/storage"
)

// PrefixStat is one observed network prefix and its coverage.
type PrefixStat struct {
	Prefix netip.Prefix
	Count  int
	Pct    float64
}

// Lifecycle holds IP dwell-time statistics.
type Lifecycle struct {
	ObservationDays float64
	ChangeCount     int
	UniqueIPs       int
	AvgLifetime     time.Duration
	MinLifetime     time.Duration
	MaxLifetime     time.Duration
	// DailyChangeRate is changes per day (0 when observation window < 1 day).
	DailyChangeRate float64
}

// Report is the full analyze output.
type Report struct {
	ObservationDays float64
	ChangeCount     int
	UniqueIPs       int
	Prefixes        map[int][]PrefixStat // keyed by bits: 24,23,22,21,20
	Lifecycle       Lifecycle
	HasData         bool
	Insufficient    bool // true when observation window < 30 days
}

var defaultPrefixBits = []int{24, 23, 22, 21, 20}

// Analyze computes distribution and lifecycle stats from storage.
func Analyze(ctx context.Context, store *storage.Store) (Report, error) {
	var rep Report

	uniq, err := store.UniqueSuccessIPs(ctx)
	if err != nil {
		return rep, err
	}
	changes, err := store.Changes(ctx, 0)
	if err != nil {
		return rep, err
	}
	seq, err := store.SuccessIPSequence(ctx)
	if err != nil {
		return rep, err
	}

	if len(uniq) == 0 && len(changes) == 0 && len(seq) == 0 {
		return rep, nil
	}
	rep.HasData = true
	rep.UniqueIPs = len(uniq)
	rep.ChangeCount = len(changes)

	var first, last time.Time
	for _, pair := range uniq {
		if first.IsZero() || pair[0].Before(first) {
			first = pair[0]
		}
		if pair[1].After(last) {
			last = pair[1]
		}
	}
	if !first.IsZero() {
		rep.ObservationDays = last.Sub(first).Hours() / 24
		if rep.ObservationDays < 0 {
			rep.ObservationDays = 0
		}
		rep.Insufficient = rep.ObservationDays < 30
	}

	rep.Prefixes = buildPrefixStats(uniq, defaultPrefixBits)
	rep.Lifecycle = buildLifecycle(rep, seq)
	return rep, nil
}

func buildPrefixStats(uniq map[netip.Addr][2]time.Time, bits []int) map[int][]PrefixStat {
	out := make(map[int][]PrefixStat, len(bits))
	for _, b := range bits {
		counts := make(map[netip.Prefix]int)
		for addr := range uniq {
			p, err := addr.Prefix(b)
			if err != nil {
				continue
			}
			p = netip.PrefixFrom(p.Addr(), b)
			counts[p]++
		}

		total := 0
		for _, c := range counts {
			total += c
		}

		stats := make([]PrefixStat, 0, len(counts))
		for p, c := range counts {
			pct := 0.0
			if total > 0 {
				pct = float64(c) / float64(total) * 100
			}
			stats = append(stats, PrefixStat{Prefix: p, Count: c, Pct: pct})
		}
		sort.Slice(stats, func(i, j int) bool {
			if stats[i].Pct == stats[j].Pct {
				return stats[i].Prefix.String() < stats[j].Prefix.String()
			}
			return stats[i].Pct > stats[j].Pct
		})
		out[b] = stats
	}
	return out
}

func buildLifecycle(rep Report, seq []storage.Observation) Lifecycle {
	lc := Lifecycle{
		ObservationDays: rep.ObservationDays,
		ChangeCount:     rep.ChangeCount,
		UniqueIPs:       rep.UniqueIPs,
	}
	if rep.ObservationDays >= 1 {
		lc.DailyChangeRate = float64(rep.ChangeCount) / rep.ObservationDays
	}
	if len(seq) == 0 {
		return lc
	}

	var durations []time.Duration
	runStart := seq[0].ObservedAt
	runIP := seq[0].IP
	for i := 1; i < len(seq); i++ {
		if seq[i].IP != runIP {
			d := seq[i].ObservedAt.Sub(runStart)
			if d >= 0 {
				durations = append(durations, d)
			}
			runStart = seq[i].ObservedAt
			runIP = seq[i].IP
		}
	}
	// Do not treat the still-open last IP run as a completed lifetime.

	if len(durations) == 0 {
		return lc
	}
	var sum time.Duration
	minD := durations[0]
	maxD := durations[0]
	for _, d := range durations {
		sum += d
		if d < minD {
			minD = d
		}
		if d > maxD {
			maxD = d
		}
	}
	lc.AvgLifetime = sum / time.Duration(len(durations))
	lc.MinLifetime = minD
	lc.MaxLifetime = maxD
	return lc
}

// FormatDuration renders a duration in a human-friendly compact form.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%.1fd", d.Hours()/24)
}
