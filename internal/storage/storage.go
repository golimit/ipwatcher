package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
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
}

// Change records an IP transition.
type Change struct {
	ID        int64
	ChangedAt time.Time
	OldIP     netip.Addr
	NewIP     netip.Addr
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
			error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_obs_time ON observations(observed_at)`,
		`CREATE INDEX IF NOT EXISTS idx_obs_ip ON observations(ip)`,
		`CREATE TABLE IF NOT EXISTS ip_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			changed_at TEXT NOT NULL,
			old_ip TEXT NOT NULL,
			new_ip TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chg_time ON ip_changes(changed_at)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("failed to initialize database: %w", err)
		}
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

// InsertObservation persists one detection attempt.
func (s *Store) InsertObservation(ctx context.Context, obs Observation) error {
	if obs.ObservedAt.IsZero() {
		obs.ObservedAt = time.Now().UTC()
	}
	obs.ObservedAt = obs.ObservedAt.UTC()

	var ip any
	if obs.IP.IsValid() {
		ip = obs.IP.String()
	}

	success := 0
	if obs.Success {
		success = 1
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO observations (observed_at, ip, provider, success, latency_ms, error)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		obs.ObservedAt.Format(time.RFC3339Nano),
		ip,
		obs.Provider,
		success,
		obs.LatencyMS,
		obs.Error,
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

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ip_changes (changed_at, old_ip, new_ip) VALUES (?, ?, ?)`,
		ch.ChangedAt.Format(time.RFC3339Nano),
		ch.OldIP.String(),
		ch.NewIP.String(),
	)
	if err != nil {
		return fmt.Errorf("failed to save IP change: %w", err)
	}
	return nil
}

// LatestSuccess returns the most recent successful observation, if any.
func (s *Store) LatestSuccess(ctx context.Context) (*Observation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, observed_at, ip, provider, success, latency_ms, COALESCE(error,'')
		 FROM observations
		 WHERE success = 1 AND ip IS NOT NULL
		 ORDER BY observed_at DESC, id DESC
		 LIMIT 1`)
	return scanObservation(row)
}

// LatestSuccessExcluding returns the latest successful observation that is
// strictly before `before`, used to compare against the current result.
func (s *Store) LatestSuccessExcluding(ctx context.Context, before time.Time, _ netip.Addr) (*Observation, error) {
	before = before.UTC()
	row := s.db.QueryRowContext(ctx,
		`SELECT id, observed_at, ip, provider, success, latency_ms, COALESCE(error,'')
		 FROM observations
		 WHERE success = 1 AND ip IS NOT NULL AND observed_at < ?
		 ORDER BY observed_at DESC, id DESC
		 LIMIT 1`,
		before.Format(time.RFC3339Nano),
	)
	return scanObservation(row)
}

// LatestObservation returns the most recent observation of any kind.
func (s *Store) LatestObservation(ctx context.Context) (*Observation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, observed_at, ip, provider, success, latency_ms, COALESCE(error,'')
		 FROM observations
		 ORDER BY observed_at DESC, id DESC
		 LIMIT 1`)
	return scanObservation(row)
}

// Changes returns change events newest-first, limited when limit > 0.
func (s *Store) Changes(ctx context.Context, limit int) ([]Change, error) {
	q := `SELECT id, changed_at, old_ip, new_ip FROM ip_changes ORDER BY changed_at DESC, id DESC`
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
		)
		if err := rows.Scan(&c.ID, &changedAt, &oldIP, &newIP); err != nil {
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
		c.OldIP = o
		c.NewIP = n
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed iterating IP changes: %w", err)
	}
	return out, nil
}

// UniqueSuccessIPs returns distinct successful IPs with first/last seen times.
func (s *Store) UniqueSuccessIPs(ctx context.Context) (map[netip.Addr][2]time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ip, MIN(observed_at), MAX(observed_at)
		 FROM observations
		 WHERE success = 1 AND ip IS NOT NULL AND ip != ''
		 GROUP BY ip`)
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
		out[addr] = [2]time.Time{first.UTC(), last.UTC()}
	}
	return out, rows.Err()
}

// SuccessIPSequence returns successful IPs ordered by time (for lifecycle).
func (s *Store) SuccessIPSequence(ctx context.Context) ([]Observation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, observed_at, ip, provider, success, latency_ms, COALESCE(error,'')
		 FROM observations
		 WHERE success = 1 AND ip IS NOT NULL AND ip != ''
		 ORDER BY observed_at ASC, id ASC`)
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
	CurrentIP       netip.Addr
	LastCheck       time.Time
	LastIPChange    time.Time
	CurrentDuration time.Duration
	HasData         bool
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

	cur, err := s.LatestSuccess(ctx)
	if err != nil {
		return st, err
	}
	if cur != nil {
		st.CurrentIP = cur.IP
	}

	changes, err := s.Changes(ctx, 1)
	if err != nil {
		return st, err
	}
	if len(changes) > 0 {
		st.LastIPChange = changes[0].ChangedAt
		st.CurrentDuration = time.Since(st.LastIPChange)
	} else if cur != nil {
		// No recorded change: duration since first observation of this IP.
		uniq, err := s.UniqueSuccessIPs(ctx)
		if err != nil {
			return st, err
		}
		if pair, ok := uniq[cur.IP]; ok {
			st.CurrentDuration = time.Since(pair[0])
		}
	}
	return st, nil
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
	)
	err := row.Scan(&o.ID, &observedAt, &ip, &provider, &success, &latency, &errText)
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
		o.IP = addr
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
