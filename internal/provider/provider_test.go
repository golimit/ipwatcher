package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestParseIPv4(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"1.2.3.4", "1.2.3.4", false},
		{"  8.8.8.8\n", "8.8.8.8", false},
		{"1.2.3.4\r\n", "1.2.3.4", false},
		{"", "", true},
		{"not-an-ip", "", true},
		{"::1", "", true},                    // IPv6 rejected
		{"2001:db8::1", "", true},            // IPv6 rejected
		{"0.0.0.0", "", true},                // unspecified
		{"1.2.3.4\nextra", "1.2.3.4", false}, // first line only
	}
	for _, tc := range cases {
		got, err := ParseIPv4(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseIPv4(%q) expected error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseIPv4(%q) error: %v", tc.in, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("ParseIPv4(%q) = %v, want %s", tc.in, got, tc.want)
		}
	}
}

func TestHTTPProviderSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("203.0.113.10\n"))
	}))
	defer srv.Close()

	p := NewHTTPProvider(srv.URL, srv.Client())
	addr, err := p.Lookup(context.Background())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if addr != netip.MustParseAddr("203.0.113.10") {
		t.Fatalf("addr = %v", addr)
	}
}

func TestHTTPProviderHTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewHTTPProvider(srv.URL, srv.Client())
	_, err := p.Lookup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected HTTP 500 error, got %v", err)
	}
}

func TestHTTPProviderInvalidBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	p := NewHTTPProvider(srv.URL, srv.Client())
	if _, err := p.Lookup(context.Background()); err == nil {
		t.Fatal("expected invalid IP error")
	}
}

func TestHTTPProviderEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("   \n"))
	}))
	defer srv.Close()

	p := NewHTTPProvider(srv.URL, srv.Client())
	if _, err := p.Lookup(context.Background()); err == nil {
		t.Fatal("expected empty response error")
	}
}

func TestHTTPProviderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("1.2.3.4"))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 50 * time.Millisecond}
	p := NewHTTPProvider(srv.URL, client)
	if _, err := p.Lookup(context.Background()); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestFailoverSkipsFailingProviders(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("198.51.100.7"))
	}))
	defer good.Close()

	fo := NewFailover(
		NewHTTPProvider(bad.URL, bad.Client()),
		NewHTTPProvider(good.URL, good.Client()),
	)
	addr, prov, err := fo.Lookup(context.Background())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if addr.String() != "198.51.100.7" {
		t.Fatalf("addr = %v", addr)
	}
	if prov.Name() == "" {
		t.Fatal("expected provider name")
	}
}

func TestFailoverAllFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	fo := NewFailover(NewHTTPProvider(bad.URL, bad.Client()))
	if _, _, err := fo.Lookup(context.Background()); err == nil {
		t.Fatal("expected all-fail error")
	}
}
