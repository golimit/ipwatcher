package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Family identifies the IP protocol version of a row.
const (
	Family4 = byte('4')
	Family6 = byte('6')
)

// Store wraps the SQLite database used by ipwatcher.
type Store struct {
	db *sql.DB
}

// Observation is one detection attempt (success or failure).
type Observation struct {
	ID         int64
	ObservedAt time.Time
	IP         netip.Addr // zero if failed or empty
	Provider   string
	Success    bool
	LatencyMS  int64
	Error      string
	Family     byte
}

// Change records an IP transition within one family.
type Change struct {
	ID        int64
	ChangedAt time.Time
	OldIP     netip.Addr
	NewIP     netip.Addr
	Family    byte
}

// Open creates parent directories if needed and migrates schema.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	// SQLite is single-writer; keep one connection to avoid lock errors.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			observed_at TEXT NOT NULL,
			ip TEXT,
			provider TEXT,
			success INTEGER NOT NULL,
			latency_ms INTEGER,
			error TEXT,
			family TEXT NOT NULL DEFAULT '4'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_obs_time ON observations(observed_at)`,
		`CREATE INDEX IF NOT EXISTS idx_obs_ip ON observations(ip)`,
		`CREATE TABLE IF NOT EXISTS ip_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			changed_at TEXT NOT NULL,
			old_ip TEXT NOT NULL,
			new_ip TEXT NOT NULL,
			family TEXT NOT NULL DEFAULT '4'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chg_time ON ip_changes(changed_at)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("failed to initialize database: %w", err)
		}
	}
	// Upgrade v0.1 databases that lack the family column (before family indexes).
	for _, table := range []string{"observations", "ip_changes"} {
		if err := s.ensureColumn(ctx, table, "family", `TEXT NOT NULL DEFAULT '4'`); err != nil {
			return err
		}
	}
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_obs_family ON observations(family)`,
		`CREATE INDEX IF NOT EXISTS idx_chg_family ON ip_changes(family)`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("failed to create family index: %w", err)
		}
	}
	// Backfill family from stored address text for pre-upgrade rows.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE observations SET family = '6' WHERE family = '4' AND ip IS NOT NULL AND instr(ip, ':') > 0`); err != nil {
		return fmt.Errorf("failed to backfill observation family: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE ip_changes SET family = '6' WHERE family = '4' AND (instr(old_ip, ':') > 0 OR instr(new_ip, ':') > 0)`); err != nil {
		return fmt.Errorf("failed to backfill change family: %w", err)
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, decl string) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return fmt.Errorf("failed to inspect %s: %w", table, err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("failed to scan table info: %w", err)
		}
		if strings.EqualFold(name, column) {
			found = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	if err != nil {
		return fmt.Errorf("failed to add %s.%s: %w", table, column, err)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func familyOf(addr netip.Addr) byte {
	if addr.Is4() || addr.Is4In6() {
		return Family4
	}
	return Family6
}

// InsertObservation persists one detection attempt.
func (s *Store) InsertObservation(ctx context.Context, obs Observation) error {
	if obs.ObservedAt.IsZero() {
		obs.ObservedAt = time.Now().UTC()
	}
	obs.ObservedAt = obs.ObservedAt.UTC()

	var ip any
	if obs.IP.IsValid() {
		ip = obs.IP.Unmap().String()
	}

	family := obs.Family
	if family == 0 && obs.IP.IsValid() {
		family = familyOf(obs.IP)
	}
	if family == 0 {
		family = Family4
	}

	success := 0
	if obs.Success {
		success = 1
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO observations (observed_at, ip, provider, success, latency_ms, error, family)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		obs.ObservedAt.Format(time.RFC3339Nano),
		ip,
		obs.Provider,
		success,
		obs.LatencyMS,
		obs.Error,
		string(family),
	)
	if err != nil {
		return fmt.Errorf("failed to save IP observation: %w", err)
	}
	return nil
}

// InsertChange records an IP transition.
func (s *Store) InsertChange(ctx context.Context, ch Change) error {
	if ch.ChangedAt.IsZero() {
		ch.ChangedAt = time.Now().UTC()
	}
	ch.ChangedAt = ch.ChangedAt.UTC()

	family := ch.Family
	if family == 0 {
		family = familyOf(ch.NewIP)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ip_changes (changed_at, old_ip, new_ip, family) VALUES (?, ?, ?, ?)`,
		ch.ChangedAt.Format(time.RFC3339Nano),
		ch.OldIP.Unmap().String(),
		ch.NewIP.Unmap().String(),
		string(family),
	)
	if err != nil {
		return fmt.Errorf("failed to save IP change: %w", err)
	}
	return nil
}

// DeleteIP removes every observation and change event involving ip.
func (s *Store) DeleteIP(ctx context.Context, ip netip.Addr) (obsRemoved, changesRemoved int64, err error) {
	if !ip.IsValid() {
		return 0, 0, fmt.Errorf("ip is invalid")
	}
	sIP := ip.Unmap().String()

	r1, err := s.db.ExecContext(ctx,
		`DELETE FROM observations WHERE ip = ?`, sIP)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to delete observations: %w", err)
	}
	obsRemoved, _ = r1.RowsAffected()

	r2, err := s.db.ExecContext(ctx,
		`DELETE FROM ip_changes WHERE old_ip = ? OR new_ip = ?`, sIP, sIP)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to delete IP changes: %w", err)
	}
	changesRemoved, _ = r2.RowsAffected()
	return obsRemoved, changesRemoved, nil
}

// DeletePrefix removes observations whose IP falls in prefix, and any change
// event that references one of those IPs (or an IP already inside prefix).
func (s *Store) DeletePrefix(ctx context.Context, prefix netip.Prefix) (obsRemoved, changesRemoved int64, err error) {
	if !prefix.IsValid() {
		return 0, 0, fmt.Errorf("prefix is invalid")
	}
	prefix = prefix.Masked()

	// Collect matching observation IPs, then delete rows.
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT ip FROM observations WHERE ip IS NOT NULL AND ip != ''`)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to scan observation IPs: %w", err)
	}
	var hits []string
	for rows.Next() {
		var sIP string
		if err := rows.Scan(&sIP); err != nil {
			rows.Close()
			return 0, 0, err
		}
		addr, err := netip.ParseAddr(sIP)
		if err != nil {
			continue
		}
		if prefix.Contains(addr.Unmap()) {
			hits = append(hits, addr.Unmap().String())
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()

	for _, sIP := range hits {
		o, c, err := s.DeleteIP(ctx, netip.MustParseAddr(sIP))
		if err != nil {
			return obsRemoved, changesRemoved, err
		}
		obsRemoved += o
		changesRemoved += c
	}

	// Also drop change rows whose IPs are inside the prefix even if no
	// matching observation remains (e.g. already purged observations).
	crows, err := s.db.QueryContext(ctx,
		`SELECT id, old_ip, new_ip FROM ip_changes`)
	if err != nil {
		return obsRemoved, changesRemoved, fmt.Errorf("failed to scan changes: %w", err)
	}
	type chgRow struct {
		id           int64
		oldIP, newIP string
	}
	var drop []int64
	for crows.Next() {
		var r chgRow
		if err := crows.Scan(&r.id, &r.oldIP, &r.newIP); err != nil {
			crows.Close()
			return obsRemoved, changesRemoved, err
		}
		o, err1 := netip.ParseAddr(r.oldIP)
		n, err2 := netip.ParseAddr(r.newIP)
		if err1 == nil && prefix.Contains(o.Unmap()) {
			drop = append(drop, r.id)
			continue
		}
		if err2 == nil && prefix.Contains(n.Unmap()) {
			drop = append(drop, r.id)
		}
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return obsRemoved, changesRemoved, err
	}
	crows.Close()

	for _, id := range drop {
		// Skip if already deleted via DeleteIP above (count separately).
		res, err := s.db.ExecContext(ctx, `DELETE FROM ip_changes WHERE id = ?`, id)
		if err != nil {
			return obsRemoved, changesRemoved, fmt.Errorf("failed to delete change %d: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changesRemoved += n
		}
	}
	return obsRemoved, changesRemoved, nil
}

// LatestSuccess returns the most recent successful observation for a family.
// Pass family 0 to accept any family (v4 preferred for compatibility).
func (s *Store) LatestSuccess(ctx context.Context, family byte) (*Observation, error) {
	q := `SELECT id, observed_at, ip, provider, success, latency_ms, COALESCE(error,''), family
		 FROM observations
		 WHERE success = 1 AND ip IS NOT NULL`
	args := []any{}
	if family != 0 {
		q += ` AND family = ?`
		args = append(args, string(family))
	}
	q += ` ORDER BY observed_at DESC, id DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, args...)
	return scanObservation(row)
}

// LatestSuccessExcluding returns the latest successful observation of the same
// family that is strictly before `before`.
func (s *Store) LatestSuccessExcluding(ctx context.Context, before time.Time, family byte) (*Observation, error) {
	before = before.UTC()
	row := s.db.QueryRowContext(ctx,
		`SELECT id, observed_at, ip, provider, success, latency_ms, COALESCE(error,''), family
		 FROM observations
		 WHERE success = 1 AND ip IS NOT NULL AND observed_at < ? AND family = ?
		 ORDER BY observed_at DESC, id DESC
		 LIMIT 1`,
		before.Format(time.RFC3339Nano),
		string(family),
	)
	return scanObservation(row)
}

// LatestObservation returns the most recent observation of any kind.
func (s *Store) LatestObservation(ctx context.Context) (*Observation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, observed_at, ip, provider, success, latency_ms, COALESCE(error,''), family
		 FROM observations
		 ORDER BY observed_at DESC, id DESC
		 LIMIT 1`)
	return scanObservation(row)
}

// Changes returns change events newest-first, limited when limit > 0.
func (s *Store) Changes(ctx context.Context, limit int) ([]Change, error) {
	q := `SELECT id, changed_at, old_ip, new_ip, family FROM ip_changes ORDER BY changed_at DESC, id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query IP changes: %w", err)
	}
	defer rows.Close()

	var out []Change
	for rows.Next() {
		var (
			c         Change
			changedAt string
			oldIP     string
			newIP     string
			family    string
		)
		if err := rows.Scan(&c.ID, &changedAt, &oldIP, &newIP, &family); err != nil {
			return nil, fmt.Errorf("failed to scan IP change: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, changedAt)
		if err != nil {
			return nil, fmt.Errorf("invalid change timestamp %q: %w", changedAt, err)
		}
		c.ChangedAt = t.UTC()
		o, err := netip.ParseAddr(oldIP)
		if err != nil {
			return nil, fmt.Errorf("invalid old_ip %q: %w", oldIP, err)
		}
		n, err := netip.ParseAddr(newIP)
		if err != nil {
			return nil, fmt.Errorf("invalid new_ip %q: %w", newIP, err)
		}
		c.OldIP = o.Unmap()
		c.NewIP = n.Unmap()
		c.Family = Family4
		if family == string(Family6) {
			c.Family = Family6
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed iterating IP changes: %w", err)
	}
	return out, nil
}

// Observations returns raw observation rows newest-first (for export).
func (s *Store) Observations(ctx context.Context, limit int) ([]Observation, error) {
	q := `SELECT id, observed_at, ip, provider, success, latency_ms, COALESCE(error,''), family
		 FROM observations ORDER BY observed_at DESC, id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query observations: %w", err)
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		o, err := scanObservationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UniqueSuccessIPs returns distinct successful IPs with first/last seen times.
// Pass family 0 for all families.
func (s *Store) UniqueSuccessIPs(ctx context.Context, family byte) (map[netip.Addr][2]time.Time, error) {
	q := `SELECT ip, MIN(observed_at), MAX(observed_at)
		 FROM observations
		 WHERE success = 1 AND ip IS NOT NULL AND ip != ''`
	args := []any{}
	if family != 0 {
		q += ` AND family = ?`
		args = append(args, string(family))
	}
	q += ` GROUP BY ip`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query unique IPs: %w", err)
	}
	defer rows.Close()

	out := make(map[netip.Addr][2]time.Time)
	for rows.Next() {
		var (
			ipStr  string
			firstS string
			lastS  string
		)
		if err := rows.Scan(&ipStr, &firstS, &lastS); err != nil {
			return nil, fmt.Errorf("failed to scan unique IP: %w", err)
		}
		addr, err := netip.ParseAddr(ipStr)
		if err != nil {
			return nil, fmt.Errorf("invalid stored IP %q: %w", ipStr, err)
		}
		first, err := time.Parse(time.RFC3339Nano, firstS)
		if err != nil {
			return nil, fmt.Errorf("invalid first-seen timestamp: %w", err)
		}
		last, err := time.Parse(time.RFC3339Nano, lastS)
		if err != nil {
			return nil, fmt.Errorf("invalid last-seen timestamp: %w", err)
		}
		out[addr.Unmap()] = [2]time.Time{first.UTC(), last.UTC()}
	}
	return out, rows.Err()
}

// SuccessIPSequence returns successful IPs ordered by time (for lifecycle).
func (s *Store) SuccessIPSequence(ctx context.Context, family byte) ([]Observation, error) {
	q := `SELECT id, observed_at, ip, provider, success, latency_ms, COALESCE(error,''), family
		 FROM observations
		 WHERE success = 1 AND ip IS NOT NULL AND ip != ''`
	args := []any{}
	if family != 0 {
		q += ` AND family = ?`
		args = append(args, string(family))
	}
	q += ` ORDER BY observed_at ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query observation sequence: %w", err)
	}
	defer rows.Close()

	var out []Observation
	for rows.Next() {
		o, err := scanObservationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Status aggregates current state for `ipwatcher status`.
type Status struct {
	CurrentIPv4    netip.Addr
	CurrentIPv6    netip.Addr
	LastCheck      time.Time
	LastIPv4Change time.Time
	LastIPv6Change time.Time
	CurrentIPv4Dur time.Duration
	CurrentIPv6Dur time.Duration
	HasData        bool
	HasIPv6        bool
}

// Status reads the latest state from the database.
func (s *Store) Status(ctx context.Context) (Status, error) {
	var st Status

	latest, err := s.LatestObservation(ctx)
	if err != nil {
		return st, err
	}
	if latest != nil {
		st.HasData = true
		st.LastCheck = latest.ObservedAt
	}

	cur4, err := s.LatestSuccess(ctx, Family4)
	if err != nil {
		return st, err
	}
	if cur4 != nil {
		st.CurrentIPv4 = cur4.IP
		st.HasData = true
	}

	cur6, err := s.LatestSuccess(ctx, Family6)
	if err != nil {
		return st, err
	}
	if cur6 != nil {
		st.CurrentIPv6 = cur6.IP
		st.HasIPv6 = true
		st.HasData = true
	}

	changes, err := s.Changes(ctx, 0)
	if err != nil {
		return st, err
	}
	for _, c := range changes {
		if c.Family == Family6 {
			if st.LastIPv6Change.IsZero() {
				st.LastIPv6Change = c.ChangedAt
			}
		} else if st.LastIPv4Change.IsZero() {
			st.LastIPv4Change = c.ChangedAt
		}
	}

	st.CurrentIPv4Dur = s.durationFor(ctx, cur4, st.LastIPv4Change)
	st.CurrentIPv6Dur = s.durationFor(ctx, cur6, st.LastIPv6Change)
	return st, nil
}

func (s *Store) durationFor(ctx context.Context, cur *Observation, lastChange time.Time) time.Duration {
	if !lastChange.IsZero() {
		return time.Since(lastChange)
	}
	if cur == nil {
		return 0
	}
	uniq, err := s.UniqueSuccessIPs(ctx, familyOf(cur.IP))
	if err != nil {
		return 0
	}
	if pair, ok := uniq[cur.IP]; ok {
		return time.Since(pair[0])
	}
	return 0
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanObservation(row rowScanner) (*Observation, error) {
	var (
		o          Observation
		observedAt string
		ip         sql.NullString
		success    int
		latency    sql.NullInt64
		errText    string
		provider   string
		family     sql.NullString
	)
	err := row.Scan(&o.ID, &observedAt, &ip, &provider, &success, &latency, &errText, &family)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan observation: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, observedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid observation timestamp %q: %w", observedAt, err)
	}
	o.ObservedAt = t.UTC()
	if ip.Valid && ip.String != "" {
		addr, err := netip.ParseAddr(ip.String)
		if err != nil {
			return nil, fmt.Errorf("invalid stored IP %q: %w", ip.String, err)
		}
		o.IP = addr.Unmap()
		o.Family = familyOf(o.IP)
	} else if family.Valid && family.String != "" {
		o.Family = family.String[0]
	}
	o.Provider = provider
	o.Success = success != 0
	o.LatencyMS = latency.Int64
	o.Error = errText
	return &o, nil
}

func scanObservationRow(rows *sql.Rows) (Observation, error) {
	o, err := scanObservation(rows)
	if err != nil {
		return Observation{}, err
	}
	if o == nil {
		return Observation{}, fmt.Errorf("unexpected empty observation row")
	}
	return *o, nil
}
