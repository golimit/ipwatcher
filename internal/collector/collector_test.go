package collector

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ipwatcher/internal/config"
	"ipwatcher/internal/notify"
	"ipwatcher/internal/provider"
	"ipwatcher/internal/storage"
)

func openStore(t *testing.T) *storage.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.db")
	store, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newCol(store *storage.Store, fo4 *provider.Failover, fo6 *provider.Failover) *Collector {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(Options{
		Interval:   time.Minute,
		Timeout:    2 * time.Second,
		Retries:    0,
		RetryDelay: 0,
	}, fo4, fo6, store, log, nil)
}

func TestTickOnceRecordsChange(t *testing.T) {
	var calls atomic.Int32
	var current atomic.Value
	current.Store("203.0.113.1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(current.Load().(string)))
	}))
	defer srv.Close()

	store := openStore(t)
	fo := provider.NewFailover(provider.NewHTTPProvider(srv.URL, srv.Client()))
	col := newCol(store, fo, nil)

	ctx := context.Background()

	// First tick: record baseline, no change event.
	col.TickOnce(ctx)
	obs, err := store.LatestSuccess(ctx, storage.Family4)
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
	if changes[0].Family != storage.Family4 {
		t.Fatalf("family = %q, want 4", changes[0].Family)
	}
}

func TestTickOnceFailureDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fo := provider.NewFailover(provider.NewHTTPProvider(srv.URL, srv.Client()))
	col := New(Options{
		Interval:   time.Minute,
		Timeout:    time.Second,
		Retries:    1,
		RetryDelay: time.Millisecond,
	}, fo, nil, store, log, nil)

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

	cur, err := store.LatestSuccess(context.Background(), storage.Family4)
	if err != nil {
		t.Fatal(err)
	}
	if cur != nil {
		t.Fatalf("expected no success, got %v", cur.IP)
	}
	_ = netip.Addr{}
}

func TestTickOnceSkipsIgnoredIP(t *testing.T) {
	var current atomic.Value
	current.Store("154.3.34.66")
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(current.Load().(string)))
	}))
	defer srvBad.Close()

	srvGood := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("120.229.60.138"))
	}))
	defer srvGood.Close()

	filter, err := config.ParseIgnoreFilter([]string{"154.3.34.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	fo := provider.NewFailover(
		provider.NewHTTPProvider(srvBad.URL, srvBad.Client()),
		provider.NewHTTPProvider(srvGood.URL, srvGood.Client()),
	).WithFilter(filter)
	col := newCol(store, fo, nil)
	col.TickOnce(context.Background())

	obs, err := store.LatestSuccess(context.Background(), storage.Family4)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.IP.String() != "120.229.60.138" {
		t.Fatalf("obs = %+v, want 120.229.60.138", obs)
	}
}

func TestTickOnceIPv6BestEffort(t *testing.T) {
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("203.0.113.1"))
	}))
	defer srv4.Close()
	srv6 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("2001:db8::10"))
	}))
	defer srv6.Close()
	srv6fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv6fail.Close()

	store := openStore(t)
	fo4 := provider.NewFailover(provider.NewHTTPProvider(srv4.URL, srv4.Client()))
	fo6 := provider.NewFailover(provider.NewHTTPProvider6(srv6.URL, srv6.Client()))
	col := newCol(store, fo4, fo6)
	ctx := context.Background()
	col.TickOnce(ctx)

	v6, err := store.LatestSuccess(ctx, storage.Family6)
	if err != nil {
		t.Fatal(err)
	}
	if v6 == nil || v6.IP.String() != "2001:db8::10" {
		t.Fatalf("v6 = %+v", v6)
	}

	// IPv6 failure must not write a failure observation.
	fo6bad := provider.NewFailover(provider.NewHTTPProvider6(srv6fail.URL, srv6fail.Client()))
	col2 := newCol(store, fo4, fo6bad)
	before, _ := store.LatestObservation(ctx)
	col2.TickOnce(ctx)
	after, _ := store.LatestObservation(ctx)
	if after != nil && before != nil && after.ID == before.ID {
		// no new rows — good
	} else if after != nil && !after.Success && after.Family == storage.Family6 {
		t.Fatal("ipv6 failure should not write failure observation")
	}
}

func TestWebhookFailureStillRecordsChange(t *testing.T) {
	var current atomic.Value
	current.Store("203.0.113.1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(current.Load().(string)))
	}))
	defer srv.Close()

	badHook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badHook.Close()

	store := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fo := provider.NewFailover(provider.NewHTTPProvider(srv.URL, srv.Client()))
	hook := notify.New(badHook.URL, time.Second)
	col := New(Options{
		Interval: time.Minute, Timeout: time.Second, Retries: 0, RetryDelay: 0,
	}, fo, nil, store, log, hook)

	ctx := context.Background()
	col.TickOnce(ctx)
	current.Store("203.0.113.2")
	time.Sleep(5 * time.Millisecond)
	col.TickOnce(ctx)

	changes, err := store.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1 despite webhook failure", len(changes))
	}
	if changes[0].NewIP.String() != "203.0.113.2" {
		t.Fatalf("change = %+v", changes[0])
	}
}
