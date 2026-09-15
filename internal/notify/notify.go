package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Event is the JSON body posted on an IP change.
type Event struct {
	Family    string    `json:"family"` // "ipv4" or "ipv6"
	OldIP     string    `json:"old_ip"`
	NewIP     string    `json:"new_ip"`
	ChangedAt time.Time `json:"changed_at"`
}

// Webhook posts change events to a remote URL. Optional.
type Webhook struct {
	url    string
	client *http.Client
}

// New builds a webhook notifier. Returns nil when url is empty.
func New(url string, timeout time.Duration) *Webhook {
	if url == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Webhook{
		url:    url,
		client: &http.Client{Timeout: timeout},
	}
}

// URL exposes the configured endpoint (for check/debug).
func (w *Webhook) URL() string {
	if w == nil {
		return ""
	}
	return w.url
}

// Send posts the event. Never blocks the collector on failure — callers log.
func (w *Webhook) Send(ctx context.Context, ev Event) error {
	if w == nil || w.url == "" {
		return nil
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("failed to encode webhook payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ipwatcher/0.2")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
