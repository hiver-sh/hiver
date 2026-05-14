package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseCollector subscribes to GET /v1/events and accumulates every
// SandboxEvent into an in-memory slice until the connection closes
// (container exit) or stop() is called.
type sseCollector struct {
	mu     sync.Mutex
	events []map[string]any
	cancel context.CancelFunc
	done   chan struct{}
}

// startSSECollector spawns the collector goroutine. It retries the GET
// in a tight loop until the API server inside the pod is reachable,
// then streams events until the connection ends. The returned handle's
// stop() returns the collected events.
func startSSECollector(t *testing.T, baseURL string) *sseCollector {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := &sseCollector{cancel: cancel, done: make(chan struct{})}
	go c.run(ctx, t, baseURL)
	return c
}

func (c *sseCollector) run(ctx context.Context, t *testing.T, baseURL string) {
	defer close(c.done)
	// lastEventId=0 asks the broker to replay every event with id>0,
	// which is every event ever seen — covers the case where the API
	// server bound before we connected (events fire as soon as the
	// sidecars come up).
	url := baseURL + "/v1/events?lastEventId=0"
	for {
		if ctx.Err() != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// API server not up yet — retry. Short backoff so we
			// don't miss early events (sbxproxy denies fire in the
			// first few hundred ms).
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Logf("sse: unexpected status %d", resp.StatusCode)
			return
		}
		c.readFrames(resp.Body)
		resp.Body.Close()
		return
	}
}

// readFrames parses a minimal SSE stream: pulls `data:` payload lines
// and JSON-decodes each. `id:` is mirrored inside the payload so we
// don't need to track it separately. Multi-line `data:` frames aren't
// produced by sandboxd so we don't handle them.
func (c *sseCollector) readFrames(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\n")
			if payload, ok := strings.CutPrefix(line, "data: "); ok {
				var ev map[string]any
				if jsonErr := json.Unmarshal([]byte(payload), &ev); jsonErr == nil {
					c.mu.Lock()
					c.events = append(c.events, ev)
					c.mu.Unlock()
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *sseCollector) stop() []map[string]any {
	c.cancel()
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, len(c.events))
	copy(out, c.events)
	return out
}

// FetchOpts configures a one-shot [FetchEvents] call.
type FetchOpts struct {
	// LastEventID, if non-empty, is sent as the `?lastEventId=` query
	// param. The server replays every event whose id is strictly
	// greater.
	LastEventID string
	// LastEventIDHeader, if non-empty, is sent as the standard
	// `Last-Event-ID` request header. SSE clients (browsers'
	// EventSource) use this on reconnect. Server-side it takes
	// precedence over the query param when both are set.
	LastEventIDHeader string
	// IdleTimeout bounds how long FetchEvents waits between events
	// before declaring the read complete. The /v1/events stream is
	// open-ended (live tail), so a one-shot caller needs an idle
	// signal to know when the replay is drained. Defaults to 500 ms.
	IdleTimeout time.Duration
}

// FetchEvents opens a one-shot SSE subscription, accumulates frames
// until either `IdleTimeout` elapses without a new event or the server
// closes the connection, then returns the events. Closing the
// connection on the client side is handled via context cancel.
//
// Designed for resume-semantics tests: the server-side stream is
// live-tail, so this helper supplies the "stop after replay drained"
// behavior callers expect from a simple GET.
func FetchEvents(t *testing.T, baseURL string, opts FetchOpts) []map[string]any {
	t.Helper()
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 500 * time.Millisecond
	}
	url := baseURL + "/v1/events"
	if opts.LastEventID != "" {
		url += "?lastEventId=" + opts.LastEventID
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("FetchEvents: new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if opts.LastEventIDHeader != "" {
		req.Header.Set("Last-Event-ID", opts.LastEventIDHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("FetchEvents: GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("FetchEvents: %s -> status %d", url, resp.StatusCode)
	}

	eventCh := make(chan map[string]any, 256)
	go func() {
		defer close(eventCh)
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				line = strings.TrimRight(line, "\n")
				if payload, ok := strings.CutPrefix(line, "data: "); ok {
					var ev map[string]any
					if jsonErr := json.Unmarshal([]byte(payload), &ev); jsonErr == nil {
						eventCh <- ev
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	var out []map[string]any
	idle := time.NewTimer(opts.IdleTimeout)
	defer idle.Stop()
	for {
		select {
		case ev, ok := <-eventCh:
			if !ok {
				return out
			}
			out = append(out, ev)
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(opts.IdleTimeout)
		case <-idle.C:
			return out
		}
	}
}
