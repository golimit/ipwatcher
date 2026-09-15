package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewNilWhenEmpty(t *testing.T) {
	if New("", 0) != nil {
		t.Fatal("expected nil webhook for empty url")
	}
}

func TestSendPostsJSON(t *testing.T) {
	var got Event
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("bad json: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	hook := New(srv.URL, time.Second)
	ev := Event{
		Family:    "ipv4",
		OldIP:     "1.1.1.1",
		NewIP:     "2.2.2.2",
		ChangedAt: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
	}
	if err := hook.Send(context.Background(), ev); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if contentType != "application/json" {
		t.Fatalf("content-type = %q", contentType)
	}
	if got.NewIP != "2.2.2.2" || got.Family != "ipv4" {
		t.Fatalf("event = %+v", got)
	}
}

func TestSendHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	hook := New(srv.URL, time.Second)
	if err := hook.Send(context.Background(), Event{Family: "ipv4", NewIP: "1.1.1.1"}); err == nil {
		t.Fatal("expected error")
	}
}
