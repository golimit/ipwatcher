package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"io"
	"log/slog"

	"ipwatcher/internal/provider"
	"ipwatcher/internal/storage"
)

func TestTickOnceRecordsChange(t *testing.T) {
	var calls atomic.Int32
	var current atomic.Value
	current.Store("203.0.113.1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(current.Load().(string)))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "c.db")
	store, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fo := provider.NewFailover(provider.NewHTTPProvider(srv.URL, srv.Client()))
	col := New(Options{
		Interval:   time.Minute,
		Timeout:    2 * time.Second,
		Retries:    0,
		RetryDelay: 0,
	}, fo, store, log)

	ctx := context.Background()

	// First tick: record baseline, no change event.
	col.TickOnce(ctx)
	obs, err := store.LatestSuccess(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.IP.String() != "203.0.113.1" {
		t.Fatalf("first obs = %+v", obs)
	}
	changes, err := store.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected 0 changes after first tick, got %d", len(changes))
	}

	// Same IP: still no change.
	time.Sleep(5 * time.Millisecond)
	col.TickOnce(ctx)
	changes, _ = store.Changes(ctx, 0)
	if len(changes) != 0 {
		t.Fatalf("expected 0 changes for same IP, got %d", len(changes))
	}

	// Different IP: one change.
	current.Store("203.0.113.99")
	time.Sleep(5 * time.Millisecond)
	col.TickOnce(ctx)
	changes, _ = store.Changes(ctx, 0)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].OldIP.String() != "203.0.113.1" || changes[0].NewIP.String() != "203.0.113.99" {
		t.Fatalf("change = %+v", changes[0])
	}
}

func TestTickOnceFailureDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "f.db")
	store, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fo := provider.NewFailover(provider.NewHTTPProvider(srv.URL, srv.Client()))
	col := New(Options{
		Interval:   time.Minute,
		Timeout:    time.Second,
		Retries:    1,
		RetryDelay: time.Millisecond,
	}, fo, store, log)

	col.TickOnce(context.Background())

	latest, err := store.LatestObservation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil {
		t.Fatal("expected a failure observation to be written")
	}
	if latest.Success {
		t.Fatal("expected failure")
	}

	// Ensure CurrentIP still invalid.
	cur, err := store.LatestSuccess(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cur != nil {
		t.Fatalf("expected no success, got %v", cur.IP)
	}

	// Sanity: netip still works for us
	_ = netip.Addr{}
}
