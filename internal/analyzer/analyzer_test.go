package analyzer

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"ipwatcher/internal/storage"
)

func seedStore(t *testing.T, ips []string, interval time.Duration) *storage.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.db")
	s, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()
	var prev string
	for i, ip := range ips {
		at := base.Add(time.Duration(i) * interval)
		if err := s.InsertObservation(ctx, storage.Observation{
			ObservedAt: at,
			IP:         netip.MustParseAddr(ip),
			Provider:   "t",
			Success:    true,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if prev != "" && prev != ip {
			if err := s.InsertChange(ctx, storage.Change{
				ChangedAt: at,
				OldIP:     netip.MustParseAddr(prev),
				NewIP:     netip.MustParseAddr(ip),
			}); err != nil {
				t.Fatalf("change: %v", err)
			}
		}
		prev = ip
	}
	return s
}

func TestAnalyzePrefixDistribution(t *testing.T) {
	// Plan §24 sample:
	// 1.2.3.10, 1.2.3.20, 1.2.3.30, 1.2.4.10
	// /24: 1.2.3.0/24 → 3, 1.2.4.0/24 → 1
	// /23: 1.2.2.0/23 covers 1.2.2–1.2.3 → 3; 1.2.4.0/23 covers 1.2.4–1.2.5 → 1
	// /22: 1.2.0.0/22 covers 1.2.0–1.2.3 → 3; 1.2.4.0/22 covers 1.2.4–1.2.7 → 1
	s := seedStore(t, []string{"1.2.3.10", "1.2.3.20", "1.2.3.30", "1.2.4.10"}, time.Hour)

	rep, err := Analyze(context.Background(), s)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !rep.HasData {
		t.Fatal("expected data")
	}
	if rep.UniqueIPs != 4 {
		t.Fatalf("UniqueIPs = %d, want 4", rep.UniqueIPs)
	}
	if rep.ChangeCount != 3 {
		t.Fatalf("ChangeCount = %d, want 3", rep.ChangeCount)
	}

	// /24
	s24 := prefixCount(rep.Prefixes[24])
	if s24["1.2.3.0/24"] != 3 {
		t.Fatalf("/24 1.2.3.0/24 = %d, want 3; got %v", s24["1.2.3.0/24"], rep.Prefixes[24])
	}
	if s24["1.2.4.0/24"] != 1 {
		t.Fatalf("/24 1.2.4.0/24 = %d, want 1", s24["1.2.4.0/24"])
	}

	// /23: 1.2.2.0/23 contains 1.2.2.x and 1.2.3.x → 3; 1.2.4.0/23 → 1
	s23 := prefixCount(rep.Prefixes[23])
	if s23["1.2.2.0/23"] != 3 {
		t.Fatalf("/23 1.2.2.0/23 = %d, want 3; got %v", s23["1.2.2.0/23"], rep.Prefixes[23])
	}
	if s23["1.2.4.0/23"] != 1 {
		t.Fatalf("/23 1.2.4.0/23 = %d, want 1", s23["1.2.4.0/23"])
	}

	// /22: 1.2.0.0/22 → 3; 1.2.4.0/22 → 1
	s22 := prefixCount(rep.Prefixes[22])
	if s22["1.2.0.0/22"] != 3 {
		t.Fatalf("/22 1.2.0.0/22 = %d, want 3; got %v", s22["1.2.0.0/22"], rep.Prefixes[22])
	}
	if s22["1.2.4.0/22"] != 1 {
		t.Fatalf("/22 1.2.4.0/22 = %d, want 1", s22["1.2.4.0/22"])
	}
}

func TestAnalyzeEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	s, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rep, err := Analyze(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasData {
		t.Fatal("expected no data")
	}
}

func TestAnalyzeLifecycle(t *testing.T) {
	// 4 observations, 1h apart, IPs: A A B C
	// completed runs only: A 0h→2h = 2h; B 2h→3h = 1h; open C excluded
	s := seedStore(t, []string{"10.0.0.1", "10.0.0.1", "10.0.0.2", "10.0.0.3"}, time.Hour)
	rep, err := Analyze(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Lifecycle.UniqueIPs != 3 {
		t.Fatalf("unique = %d", rep.Lifecycle.UniqueIPs)
	}
	if rep.Lifecycle.ChangeCount != 2 {
		t.Fatalf("changes = %d", rep.Lifecycle.ChangeCount)
	}
	if rep.Lifecycle.AvgLifetime != time.Hour+30*time.Minute {
		t.Fatalf("avg lifetime = %v, want 1.5h", rep.Lifecycle.AvgLifetime)
	}
	if rep.Lifecycle.MaxLifetime != 2*time.Hour {
		t.Fatalf("max = %v, want 2h", rep.Lifecycle.MaxLifetime)
	}
	if rep.Lifecycle.MinLifetime != time.Hour {
		t.Fatalf("min = %v, want 1h", rep.Lifecycle.MinLifetime)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5.0m"},
		{3 * time.Hour, "3.0h"},
		{48 * time.Hour, "2.0d"},
	}
	for _, tc := range cases {
		if got := FormatDuration(tc.d); got != tc.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func prefixCount(stats []PrefixStat) map[string]int {
	m := make(map[string]int)
	for _, s := range stats {
		m[s.Prefix.String()] = s.Count
	}
	return m
}
