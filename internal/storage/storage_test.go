package storage

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestInsertAndReadObservation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	err := s.InsertObservation(ctx, Observation{
		ObservedAt: now,
		IP:         netip.MustParseAddr("1.2.3.4"),
		Provider:   "test",
		Success:    true,
		LatencyMS:  80,
	})
	if err != nil {
		t.Fatalf("InsertObservation: %v", err)
	}

	obs, err := s.LatestSuccess(ctx)
	if err != nil {
		t.Fatalf("LatestSuccess: %v", err)
	}
	if obs == nil {
		t.Fatal("expected observation")
	}
	if obs.IP.String() != "1.2.3.4" {
		t.Fatalf("ip = %v", obs.IP)
	}
	if !obs.ObservedAt.Equal(now) {
		t.Fatalf("observed_at = %v, want %v", obs.ObservedAt, now)
	}
}

func TestInsertFailureObservation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.InsertObservation(ctx, Observation{
		ObservedAt: time.Now().UTC(),
		Success:    false,
		Error:      "all providers failed",
	})
	if err != nil {
		t.Fatalf("InsertObservation: %v", err)
	}

	latest, err := s.LatestObservation(ctx)
	if err != nil {
		t.Fatalf("LatestObservation: %v", err)
	}
	if latest.Success {
		t.Fatal("expected failure observation")
	}
	if latest.Error != "all providers failed" {
		t.Fatalf("error = %q", latest.Error)
	}

	// LatestSuccess should still be empty.
	cur, err := s.LatestSuccess(ctx)
	if err != nil {
		t.Fatalf("LatestSuccess: %v", err)
	}
	if cur != nil {
		t.Fatalf("expected nil LatestSuccess, got %v", cur)
	}
}

func TestInsertChange(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 15, 10, 15, 0, 0, time.UTC)

	err := s.InsertChange(ctx, Change{
		ChangedAt: at,
		OldIP:     netip.MustParseAddr("1.2.3.4"),
		NewIP:     netip.MustParseAddr("1.2.4.25"),
	})
	if err != nil {
		t.Fatalf("InsertChange: %v", err)
	}

	changes, err := s.Changes(ctx, 0)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %d", len(changes))
	}
	if changes[0].OldIP.String() != "1.2.3.4" || changes[0].NewIP.String() != "1.2.4.25" {
		t.Fatalf("change = %+v", changes[0])
	}
}

func TestUniqueSuccessIPs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	ips := []string{"1.2.3.10", "1.2.3.10", "1.2.3.20", "1.2.4.10"}
	for i, ip := range ips {
		err := s.InsertObservation(ctx, Observation{
			ObservedAt: base.Add(time.Duration(i) * time.Hour),
			IP:         netip.MustParseAddr(ip),
			Provider:   "t",
			Success:    true,
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	uniq, err := s.UniqueSuccessIPs(ctx)
	if err != nil {
		t.Fatalf("UniqueSuccessIPs: %v", err)
	}
	if len(uniq) != 3 {
		t.Fatalf("unique = %d, want 3", len(uniq))
	}
}

func TestLatestSuccessExcluding(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	t1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 1, 10, 5, 0, 0, time.UTC)
	t3 := time.Date(2026, 9, 1, 10, 10, 0, 0, time.UTC)

	for _, item := range []struct {
		at time.Time
		ip string
	}{
		{t1, "1.2.3.4"},
		{t2, "1.2.3.4"},
		{t3, "1.2.3.18"},
	} {
		if err := s.InsertObservation(ctx, Observation{
			ObservedAt: item.at,
			IP:         netip.MustParseAddr(item.ip),
			Success:    true,
			Provider:   "t",
		}); err != nil {
			t.Fatal(err)
		}
	}

	prev, err := s.LatestSuccessExcluding(ctx, t3, netip.MustParseAddr("1.2.3.18"))
	if err != nil {
		t.Fatalf("LatestSuccessExcluding: %v", err)
	}
	if prev == nil || prev.IP.String() != "1.2.3.4" {
		t.Fatalf("prev = %+v", prev)
	}
}
