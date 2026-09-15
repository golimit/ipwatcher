package provider

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Provider looks up the current public IP address for one family.
type Provider interface {
	Name() string
	Lookup(ctx context.Context) (netip.Addr, error)
}

// HTTPProvider queries a plain-text HTTPS endpoint that returns an IP.
type HTTPProvider struct {
	name   string
	url    string
	client *http.Client
	want6  bool
}

// NewHTTPProvider builds an IPv4 provider for a single HTTPS URL.
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

// NewHTTPProvider6 builds an IPv6 provider for a single HTTPS URL.
func NewHTTPProvider6(rawURL string, client *http.Client) *HTTPProvider {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &HTTPProvider{
		name:   hostOf(rawURL),
		url:    rawURL,
		client: client,
		want6:  true,
	}
}

func (p *HTTPProvider) Name() string { return p.name }

// Lookup fetches and validates a single IP response for the configured family.
func (p *HTTPProvider) Lookup(ctx context.Context) (netip.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("User-Agent", "ipwatcher/0.2")
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

	if p.want6 {
		return ParseIPv6(string(body))
	}
	return ParseIPv4(string(body))
}

// ParseIPv4 validates that s contains a single unicast IPv4 address.
func ParseIPv4(s string) (netip.Addr, error) {
	addr, err := parseIPToken(s)
	if err != nil {
		return netip.Addr{}, err
	}
	if !addr.Is4() {
		return netip.Addr{}, fmt.Errorf("expected IPv4, got %q", s)
	}
	return addr.Unmap(), nil
}

// ParseIPv6 validates that s contains a single unicast IPv6 address.
func ParseIPv6(s string) (netip.Addr, error) {
	addr, err := parseIPToken(s)
	if err != nil {
		return netip.Addr{}, err
	}
	if !addr.Is6() || addr.Is4In6() {
		return netip.Addr{}, fmt.Errorf("expected IPv6, got %q", s)
	}
	return addr, nil
}

func parseIPToken(s string) (netip.Addr, error) {
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
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("unspecified or invalid IP %q", trimmed)
	}
	return addr, nil
}

// Filter matches addresses against configured ignore prefixes.
type Filter interface {
	Contains(addr netip.Addr) bool
}

// Failover tries each provider in order until one succeeds.
type Failover struct {
	providers []Provider
	filter    Filter
}

// NewFailover builds a chain from ordered providers.
func NewFailover(providers ...Provider) *Failover {
	return &Failover{providers: providers}
}

// WithFilter skips providers that return a filtered address,
// so a bogus single-provider result can fall through to the next.
func (f *Failover) WithFilter(filter Filter) *Failover {
	f.filter = filter
	return f
}

// Providers returns the configured provider list (for check command).
func (f *Failover) Providers() []Provider { return f.providers }

// Lookup returns the first successful IP or the last error.
func (f *Failover) Lookup(ctx context.Context) (netip.Addr, Provider, error) {
	if len(f.providers) == 0 {
		return netip.Addr{}, nil, fmt.Errorf("no providers configured")
	}
	var lastErr error
	for _, p := range f.providers {
		addr, err := p.Lookup(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		if f.filter != nil && f.filter.Contains(addr) {
			lastErr = fmt.Errorf("ip %s is in ignore list", addr)
			continue
		}
		return addr, p, nil
	}
	return netip.Addr{}, nil, fmt.Errorf("all providers failed: %w", lastErr)
}

// DefaultClient returns the shared IPv4 HTTP client with the given timeout.
// Connections are forced to IPv4 (tcp4) so dual-stack hosts still observe
// their public IPv4 exit address.
func DefaultClient(timeout time.Duration) *http.Client {
	return familyClient(timeout, "tcp4")
}

// DefaultClient6 returns an IPv6-only HTTP client (tcp6 dial).
func DefaultClient6(timeout time.Duration) *http.Client {
	return familyClient(timeout, "tcp6")
}

func familyClient(timeout time.Duration, network string) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, _ string, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// BuildProviders constructs IPv4 HTTP providers from URLs.
func BuildProviders(urls []string, client *http.Client) []Provider {
	out := make([]Provider, 0, len(urls))
	for _, u := range urls {
		out = append(out, NewHTTPProvider(u, client))
	}
	return out
}

// BuildProviders6 constructs IPv6 HTTP providers from URLs.
func BuildProviders6(urls []string, client *http.Client) []Provider {
	out := make([]Provider, 0, len(urls))
	for _, u := range urls {
		out = append(out, NewHTTPProvider6(u, client))
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
