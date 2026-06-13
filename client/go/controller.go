package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// DefaultGatewayURL is the URL used when no gateway URL is provided.
const DefaultGatewayURL = "http://localhost:10000"

var sandboxKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const defaultTimeout = 30 * time.Second

// Client is the controller client. It provisions and manages sandboxes
// through the Hive gateway.
type Client struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the underlying HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// WithTimeout sets the default per-operation timeout applied to point
// requests (non-streaming). Pass 0 to disable. Defaults to 30s.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// NewClient creates a controller client pointed at gatewayURL.
func NewClient(gatewayURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(gatewayURL, "/"),
		http:    &http.Client{},
		timeout: defaultTimeout,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// GetOrCreateSandbox creates a sandbox, or fetches the existing one when key
// is already in use. The key acts as an idempotency key: calling again with
// the same key returns the same sandbox and leaves config unapplied. It blocks
// until the sandbox is ready (up to the client's configured timeout).
//
// As a convenience, an empty config.FS defaults to a /workspace local mount
// with rw access, and an empty config.Egress opens egress to all hosts.
func (c *Client) GetOrCreateSandbox(ctx context.Context, key string, config SandboxConfig) (*Sandbox, error) {
	if !sandboxKeyPattern.MatchString(key) {
		return nil, fmt.Errorf("GetOrCreateSandbox: key %q must match %s", key, sandboxKeyPattern)
	}

	if len(config.FS) == 0 {
		config.FS = []FileSystem{
			{Mount: "/workspace", Backend: "local", ACLs: []ACLRule{{Path: "/workspace/**", Access: "rw"}}},
		}
	}
	if len(config.Egress) == 0 {
		config.Egress = []EgressRule{{Access: "allow", Host: "*"}}
	}

	body, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("GetOrCreateSandbox: encode config: %w", err)
	}

	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	url := fmt.Sprintf("%s/controller/v1/sandboxes/%s", c.baseURL, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetOrCreateSandbox: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("GetOrCreateSandbox: %w", readAPIError(resp))
	}

	var ref SandboxRef
	if err := json.NewDecoder(resp.Body).Decode(&ref); err != nil {
		return nil, fmt.Errorf("GetOrCreateSandbox: decode response: %w", err)
	}

	sbx := newSandbox(ref, c.baseURL, c.http)

	if c.timeout > 0 {
		if err := waitUntilReachable(ctx, sbx, c.timeout); err != nil {
			return nil, err
		}
	}

	return sbx, nil
}

// ListSandboxes returns all currently provisioned sandboxes.
func (c *Client) ListSandboxes(ctx context.Context) ([]*Sandbox, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	url := fmt.Sprintf("%s/controller/v1/sandboxes", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ListSandboxes: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ListSandboxes: %w", readAPIError(resp))
	}

	var refs []SandboxRef
	if err := json.NewDecoder(resp.Body).Decode(&refs); err != nil {
		return nil, fmt.Errorf("ListSandboxes: decode response: %w", err)
	}

	sandboxes := make([]*Sandbox, len(refs))
	for i, ref := range refs {
		sandboxes[i] = newSandbox(ref, c.baseURL, c.http)
	}
	return sandboxes, nil
}

// Shutdown permanently stops and removes the sandbox identified by key. It is
// idempotent for already-stopped sandboxes; unknown keys return an error.
func (c *Client) Shutdown(ctx context.Context, key string) error {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	url := fmt.Sprintf("%s/controller/v1/shutdown/%s", c.baseURL, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Shutdown: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return fmt.Errorf("Shutdown: %w", readAPIError(resp))
}

// WatchEvents streams sandbox lifecycle events (start/stop/die/destroy) from
// the controller. The returned channel is closed when ctx is done or the
// server closes the stream. Any final error is sent on errc.
func (c *Client) WatchEvents(ctx context.Context) (<-chan SandboxLifecycleEvent, <-chan error) {
	events := make(chan SandboxLifecycleEvent)
	errc := make(chan error, 1)

	go func() {
		defer close(events)

		url := fmt.Sprintf("%s/controller/v1/sandboxes/events", c.baseURL)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			errc <- err
			return
		}
		req.Header.Set("Accept", "text/event-stream")

		resp, err := c.http.Do(req)
		if err != nil {
			errc <- fmt.Errorf("WatchEvents: %w", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errc <- fmt.Errorf("WatchEvents: %w", readAPIError(resp))
			return
		}

		for frame := range readSSE(resp.Body) {
			var evt SandboxLifecycleEvent
			if err := json.Unmarshal([]byte(frame.data), &evt); err != nil {
				continue
			}
			select {
			case events <- evt:
			case <-ctx.Done():
				return
			}
		}
	}()

	return events, errc
}

// withTimeout returns a child context that inherits the parent's deadline
// unless it already has one, in which case the parent is returned unchanged.
func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout == 0 {
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}

func readAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var apiErr APIError
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
		return &apiErr
	}
	return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func waitUntilReachable(ctx context.Context, s *Sandbox, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := s.Ping(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("GetOrCreateSandbox: sandbox %s did not become reachable within %s: %w",
		s.ID, timeout, lastErr)
}
