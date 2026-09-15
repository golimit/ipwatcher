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

// Lifecycle holds IP dwell-time statistics for one family.
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

// FamilyReport is distribution + lifecycle for one address family.
type FamilyReport struct {
	HasData     bool
	UniqueIPs   int
	ChangeCount int
	Prefixes    map[int][]PrefixStat
	Lifecycle   Lifecycle
}

// Report is the full analyze output.
type Report struct {
	ObservationDays float64
	ChangeCount     int
	UniqueIPs       int
	Prefixes        map[int][]PrefixStat // keyed by bits, IPv4 view (legacy)
	Lifecycle       Lifecycle            // IPv4-primary view (legacy)
	IPv4            FamilyReport
	IPv6            FamilyReport
	HasData         bool
	HasIPv6         bool
	Insufficient    bool // true when observation window < 30 days
}

var (
	v4PrefixBits = []int{24, 23, 22, 21, 20}
	v6PrefixBits = []int{64, 56, 48, 32}
)

// Analyze computes distribution and lifecycle stats from storage.
func Analyze(ctx context.Context, store *storage.Store) (Report, error) {
	var rep Report

	fr4, err := analyzeFamily(ctx, store, storage.Family4, v4PrefixBits)
	if err != nil {
		return rep, err
	}
	fr6, err := analyzeFamily(ctx, store, storage.Family6, v6PrefixBits)
	if err != nil {
		return rep, err
	}
	rep.IPv4 = fr4
	rep.IPv6 = fr6

	// Legacy top-level fields stay IPv4-primary so existing consumers keep working.
	rep.Prefixes = rep.IPv4.Prefixes
	rep.Lifecycle = rep.IPv4.Lifecycle
	rep.UniqueIPs = rep.IPv4.UniqueIPs
	rep.ChangeCount = rep.IPv4.ChangeCount
	rep.HasData = rep.IPv4.HasData || rep.IPv6.HasData
	rep.HasIPv6 = rep.IPv6.HasData

	// Observation window spans both families when present.
	uniqAll, err := store.UniqueSuccessIPs(ctx, 0)
	if err != nil {
		return rep, err
	}
	var first, last time.Time
	for _, pair := range uniqAll {
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
	rep.IPv4.Lifecycle.ObservationDays = rep.ObservationDays
	rep.IPv6.Lifecycle.ObservationDays = rep.ObservationDays
	rep.Lifecycle.ObservationDays = rep.ObservationDays
	if rep.ObservationDays >= 1 {
		rep.IPv4.Lifecycle.DailyChangeRate = float64(rep.IPv4.ChangeCount) / rep.ObservationDays
		rep.IPv6.Lifecycle.DailyChangeRate = float64(rep.IPv6.ChangeCount) / rep.ObservationDays
		rep.Lifecycle.DailyChangeRate = rep.IPv4.Lifecycle.DailyChangeRate
	}
	return rep, nil
}

func analyzeFamily(ctx context.Context, store *storage.Store, family byte, bits []int) (FamilyReport, error) {
	var fr FamilyReport

	uniq, err := store.UniqueSuccessIPs(ctx, family)
	if err != nil {
		return fr, err
	}
	if len(uniq) == 0 {
		return fr, nil
	}
	changes, err := store.Changes(ctx, 0)
	if err != nil {
		return fr, err
	}
	famChanges := 0
	for _, c := range changes {
		if c.Family == family {
			famChanges++
		}
	}
	seq, err := store.SuccessIPSequence(ctx, family)
	if err != nil {
		return fr, err
	}

	fr.HasData = true
	fr.UniqueIPs = len(uniq)
	fr.ChangeCount = famChanges
	fr.Prefixes = buildPrefixStats(uniq, bits)
	fr.Lifecycle = buildLifecycle(famChanges, len(uniq), seq)
	return fr, nil
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

func buildLifecycle(changeCount, uniqueIPs int, seq []storage.Observation) Lifecycle {
	lc := Lifecycle{
		ChangeCount: changeCount,
		UniqueIPs:   uniqueIPs,
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
