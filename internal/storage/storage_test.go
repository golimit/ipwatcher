package storage

import (
	"context"
	"database/sql"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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

	obs, err := s.LatestSuccess(ctx, Family4)
	if err != nil {
		t.Fatalf("LatestSuccess: %v", err)
	}
	if obs == nil {
		t.Fatal("expected observation")
	}
	if obs.IP.String() != "1.2.3.4" {
		t.Fatalf("ip = %v", obs.IP)
	}
	if obs.Family != Family4 {
		t.Fatalf("family = %q", obs.Family)
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

	cur, err := s.LatestSuccess(ctx, Family4)
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
	if changes[0].Family != Family4 {
		t.Fatalf("family = %q", changes[0].Family)
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

	uniq, err := s.UniqueSuccessIPs(ctx, Family4)
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

	prev, err := s.LatestSuccessExcluding(ctx, t3, Family4)
	if err != nil {
		t.Fatalf("LatestSuccessExcluding: %v", err)
	}
	if prev == nil || prev.IP.String() != "1.2.3.4" {
		t.Fatalf("prev = %+v", prev)
	}
}

func TestDeleteIP(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bad := netip.MustParseAddr("203.0.113.66")
	good := netip.MustParseAddr("198.51.100.10")
	base := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	for i, ip := range []netip.Addr{bad, good, good} {
		if err := s.InsertObservation(ctx, Observation{
			ObservedAt: base.Add(time.Duration(i) * time.Minute),
			IP:         ip,
			Success:    true,
			Provider:   "t",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertChange(ctx, Change{
		ChangedAt: base.Add(2 * time.Minute),
		OldIP:     bad,
		NewIP:     good,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChange(ctx, Change{
		ChangedAt: base.Add(5 * time.Minute),
		OldIP:     good,
		NewIP:     netip.MustParseAddr("198.51.100.200"),
	}); err != nil {
		t.Fatal(err)
	}

	obs, chg, err := s.DeleteIP(ctx, bad)
	if err != nil {
		t.Fatalf("DeleteIP: %v", err)
	}
	if obs != 1 {
		t.Fatalf("obsRemoved = %d, want 1", obs)
	}
	if chg != 1 {
		t.Fatalf("changesRemoved = %d, want 1", chg)
	}

	changes, err := s.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("remaining changes = %d, want 1", len(changes))
	}
	if changes[0].OldIP != good {
		t.Fatalf("remaining change = %+v", changes[0])
	}

	uniq, err := s.UniqueSuccessIPs(ctx, Family4)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := uniq[bad]; ok {
		t.Fatal("bad IP still present in unique set")
	}
	if len(uniq) != 1 {
		t.Fatalf("unique = %d, want 1", len(uniq))
	}
}

func TestDeletePrefix(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	for i, ip := range []string{"203.0.113.66", "203.0.113.80", "198.51.100.10"} {
		if err := s.InsertObservation(ctx, Observation{
			ObservedAt: base.Add(time.Duration(i) * time.Minute),
			IP:         netip.MustParseAddr(ip),
			Success:    true,
			Provider:   "t",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertChange(ctx, Change{
		ChangedAt: base.Add(3 * time.Minute),
		OldIP:     netip.MustParseAddr("203.0.113.66"),
		NewIP:     netip.MustParseAddr("198.51.100.10"),
	}); err != nil {
		t.Fatal(err)
	}

	obs, chg, err := s.DeletePrefix(ctx, netip.MustParsePrefix("203.0.113.0/24"))
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if obs != 2 {
		t.Fatalf("obsRemoved = %d, want 2", obs)
	}
	if chg < 1 {
		t.Fatalf("changesRemoved = %d, want >= 1", chg)
	}

	uniq, err := s.UniqueSuccessIPs(ctx, Family4)
	if err != nil {
		t.Fatal(err)
	}
	if len(uniq) != 1 {
		t.Fatalf("unique = %d, want 1", len(uniq))
	}
	if _, ok := uniq[netip.MustParseAddr("198.51.100.10")]; !ok {
		t.Fatal("good IP missing")
	}
}

func TestIPv6ObservationFamily(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.InsertObservation(ctx, Observation{
		ObservedAt: time.Now().UTC(),
		IP:         netip.MustParseAddr("2001:db8::1"),
		Success:    true,
		Provider:   "t",
	}); err != nil {
		t.Fatal(err)
	}
	v6, err := s.LatestSuccess(ctx, Family6)
	if err != nil {
		t.Fatal(err)
	}
	if v6 == nil || v6.Family != Family6 {
		t.Fatalf("v6 = %+v", v6)
	}
	v4, err := s.LatestSuccess(ctx, Family4)
	if err != nil {
		t.Fatal(err)
	}
	if v4 != nil {
		t.Fatalf("unexpected v4: %+v", v4)
	}
}

func TestStatusDualFamily(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	_ = s.InsertObservation(ctx, Observation{
		ObservedAt: base, IP: netip.MustParseAddr("1.2.3.4"), Success: true, Provider: "t",
	})
	_ = s.InsertObservation(ctx, Observation{
		ObservedAt: base.Add(time.Minute), IP: netip.MustParseAddr("2001:db8::1"), Success: true, Provider: "t",
	})
	_ = s.InsertChange(ctx, Change{
		ChangedAt: base.Add(2 * time.Minute),
		OldIP:     netip.MustParseAddr("1.2.3.4"),
		NewIP:     netip.MustParseAddr("1.2.3.5"),
	})
	_ = s.InsertObservation(ctx, Observation{
		ObservedAt: base.Add(2 * time.Minute), IP: netip.MustParseAddr("1.2.3.5"), Success: true, Provider: "t",
	})

	st, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentIPv4.String() != "1.2.3.5" {
		t.Fatalf("v4 = %v", st.CurrentIPv4)
	}
	if st.CurrentIPv6.String() != "2001:db8::1" {
		t.Fatalf("v6 = %v", st.CurrentIPv6)
	}
	if !st.HasIPv6 {
		t.Fatal("HasIPv6 should be true")
	}
	if st.LastIPv4Change.IsZero() {
		t.Fatal("expected last v4 change")
	}
}

func TestMigrateV01Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v01.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			observed_at TEXT NOT NULL, ip TEXT, provider TEXT,
			success INTEGER NOT NULL, latency_ms INTEGER, error TEXT
		)`,
		`CREATE TABLE ip_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			changed_at TEXT NOT NULL,
			old_ip TEXT NOT NULL, new_ip TEXT NOT NULL
		)`,
		`INSERT INTO observations (observed_at, ip, provider, success, latency_ms)
		 VALUES ('2026-09-15T10:00:00Z', '1.2.3.4', 't', 1, 10)`,
		`INSERT INTO observations (observed_at, ip, provider, success, latency_ms)
		 VALUES ('2026-09-15T10:01:00Z', '2001:db8::9', 't', 1, 10)`,
		`INSERT INTO ip_changes (changed_at, old_ip, new_ip)
		 VALUES ('2026-09-15T10:01:00Z', '1.2.3.4', '1.2.3.5')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open migrated: %v", err)
	}
	defer s.Close()

	v4, err := s.LatestSuccess(ctx, Family4)
	if err != nil {
		t.Fatal(err)
	}
	if v4 == nil || v4.IP.String() != "1.2.3.4" || v4.Family != Family4 {
		t.Fatalf("v4 = %+v", v4)
	}
	v6, err := s.LatestSuccess(ctx, Family6)
	if err != nil {
		t.Fatal(err)
	}
	if v6 == nil || v6.IP.String() != "2001:db8::9" || v6.Family != Family6 {
		t.Fatalf("v6 backfill failed: %+v", v6)
	}
	changes, err := s.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Family != Family4 {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestIgnoreRulesCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	added, err := s.AddIgnoreRule(ctx, "203.0.113.66")
	if err != nil || !added {
		t.Fatalf("add1: added=%v err=%v", added, err)
	}
	added, err = s.AddIgnoreRule(ctx, "203.0.113.66")
	if err != nil || added {
		t.Fatalf("dup should be ignored: added=%v err=%v", added, err)
	}
	if _, err := s.AddIgnoreRule(ctx, "203.0.113.0/24"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddIgnoreRule(ctx, "2001:db8::/32"); err != nil {
		t.Fatal(err)
	}

	rules, err := s.ListIgnoreRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(rules))
	}
	strs, err := s.IgnoreRuleStrings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(strs) != 3 {
		t.Fatalf("strings = %v", strs)
	}

	removed, err := s.RemoveIgnoreRule(ctx, "203.0.113.66")
	if err != nil || !removed {
		t.Fatalf("remove: removed=%v err=%v", removed, err)
	}
	removed, err = s.RemoveIgnoreRule(ctx, "203.0.113.66")
	if err != nil || removed {
		t.Fatalf("second remove should be false: %v %v", removed, err)
	}
	rules, _ = s.ListIgnoreRules(ctx)
	if len(rules) != 2 {
		t.Fatalf("after remove = %d", len(rules))
	}
}
