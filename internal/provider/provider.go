package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Provider looks up the current public IPv4 address.
type Provider interface {
	Name() string
	Lookup(ctx context.Context) (netip.Addr, error)
}

// HTTPProvider queries a plain-text HTTPS endpoint that returns an IPv4.
type HTTPProvider struct {
	name   string
	url    string
	client *http.Client
}

// NewHTTPProvider builds a provider for a single HTTPS URL.
func NewHTTPProvider(rawURL string, client *http.Client) *HTTPProvider {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &HTTPProvider{
		name:   hostOf(rawURL),
		url:    rawURL,
		client: client,
	}
}

func (p *HTTPProvider) Name() string { return p.name }

// Lookup fetches and validates a single IPv4 response.
func (p *HTTPProvider) Lookup(ctx context.Context) (netip.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("User-Agent", "ipwatcher/0.1")
	req.Header.Set("Accept", "text/plain")

	resp, err := p.client.Do(req)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to query public IP: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to read provider response: %w", err)
	}

	return ParseIPv4(string(body))
}

// ParseIPv4 validates that s contains a single unicast IPv4 address.
func ParseIPv4(s string) (netip.Addr, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return netip.Addr{}, fmt.Errorf("empty IP response")
	}
	// Take first line in case a provider appends newline text.
	if i := strings.IndexAny(trimmed, "\r\n"); i >= 0 {
		trimmed = strings.TrimSpace(trimmed[:i])
	}
	addr, err := netip.ParseAddr(trimmed)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid IP %q: %w", trimmed, err)
	}
	if !addr.Is4() {
		return netip.Addr{}, fmt.Errorf("expected IPv4, got %q", trimmed)
	}
	if !addr.IsValid() || addr.IsUnspecified() {
		return netip.Addr{}, fmt.Errorf("unspecified or invalid IP %q", trimmed)
	}
	return addr.Unmap(), nil
}

// Failover tries each provider in order until one succeeds.
type Failover struct {
	providers []Provider
}

// NewFailover builds a chain from ordered providers.
func NewFailover(providers ...Provider) *Failover {
	return &Failover{providers: providers}
}

// Lookup returns the first successful IPv4 or the last error.
func (f *Failover) Lookup(ctx context.Context) (netip.Addr, Provider, error) {
	if len(f.providers) == 0 {
		return netip.Addr{}, nil, fmt.Errorf("no providers configured")
	}
	var lastErr error
	for _, p := range f.providers {
		addr, err := p.Lookup(ctx)
		if err == nil {
			return addr, p, nil
		}
		lastErr = err
	}
	return netip.Addr{}, nil, fmt.Errorf("all providers failed: %w", lastErr)
}

// DefaultClient returns the shared HTTP client with the given timeout.
func DefaultClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// BuildProviders constructs HTTP providers from URLs.
func BuildProviders(urls []string, client *http.Client) []Provider {
	out := make([]Provider, 0, len(urls))
	for _, u := range urls {
		out = append(out, NewHTTPProvider(u, client))
	}
	return out
}

func hostOf(rawURL string) string {
	u := strings.TrimPrefix(rawURL, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexAny(u, "/?"); i >= 0 {
		u = u[:i]
	}
	if u == "" {
		return rawURL
	}
	return u
}
