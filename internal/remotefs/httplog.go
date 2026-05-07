package remotefs

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// loggingRoundTripper records every outbound HTTP request the wrapped
// transport handles as one JSON line on out. Used by [NewGoogleDrive]
// to surface real upstream traffic to Drive (Files.Get, Files.List,
// Files.Update, Files.Create, Files.Delete) and to the OAuth token
// endpoint, so the cache + dedup behavior in fusefs can be measured
// against actual API call counts.
//
// Errors at the transport layer (DNS, dial, TLS) are logged with an
// empty status; the caller still sees the original error returned.
type loggingRoundTripper struct {
	base http.RoundTripper

	mu  sync.Mutex
	out io.Writer
}

func newLoggingRoundTripper(base http.RoundTripper, out io.Writer) *loggingRoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &loggingRoundTripper{
		base: base,
		out:  out,
	}
}

type httpLogEvent struct {
	At         time.Time `json:"at"`
	Method     string    `json:"method"`
	URL        string    `json:"url"`
	Status     int       `json:"status,omitempty"`
	DurationMs int64     `json:"duration_ms"`
	Err        string    `json:"err,omitempty"`
}

func (l *loggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := l.base.RoundTrip(req)
	ev := httpLogEvent{
		At:         start,
		Method:     req.Method,
		URL:        req.URL.String(),
		DurationMs: time.Since(start).Milliseconds(),
	}
	if resp != nil {
		ev.Status = resp.StatusCode
	}
	if err != nil {
		ev.Err = err.Error()
	}
	line, _ := json.Marshal(ev)
	line = append(line, '\n')
	l.mu.Lock()
	_, _ = l.out.Write(line)
	l.mu.Unlock()
	return resp, err
}
