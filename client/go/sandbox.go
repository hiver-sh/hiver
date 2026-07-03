package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sandbox is a handle to a provisioned sandbox. Obtain one via Client.GetOrCreateSandbox.
type Sandbox struct {
	// ID is the server-assigned unique identifier (UUID).
	ID string
	// Key is the caller-chosen key the sandbox was provisioned under.
	Key string

	apiURL string
	http   *http.Client
}

// ExecProcess is a handle to a running streaming exec. Obtain one via Sandbox.ExecStream.
type ExecProcess struct {
	// ID is the client-assigned exec identifier.
	ID string
	// Output receives stdout and stderr frames until the process exits.
	Output <-chan ExecOutput

	writeFn  func(ctx context.Context, data string) error
	exitCode <-chan int
	exitErr  <-chan error
}

// WriteStdin sends data to the process's stdin.
func (p *ExecProcess) WriteStdin(ctx context.Context, data string) error {
	return p.writeFn(ctx, data)
}

// Wait blocks until the process exits and returns its exit code.
func (p *ExecProcess) Wait() (int, error) {
	select {
	case code := <-p.exitCode:
		return code, nil
	case err := <-p.exitErr:
		return -1, err
	}
}

func newSandbox(ref SandboxRef, gatewayURL string, hc *http.Client) *Sandbox {
	base := strings.TrimRight(gatewayURL, "/")
	return &Sandbox{
		ID:     ref.ID,
		Key:    ref.Key,
		apiURL: fmt.Sprintf("%s/sandbox/%s", base, ref.ID),
		http:   hc,
	}
}

// keyed builds a per-sandbox API URL: <gateway>/sandbox/<id>/v1/<key><suffix>.
// suffix begins with "/" (e.g. "/config"), or is "" to address the sandbox
// resource itself (create/delete). Pod-level routes (/v1/ping) don't use this.
func (s *Sandbox) keyed(suffix string) string {
	return fmt.Sprintf("%s/v1/%s%s", s.apiURL, s.Key, suffix)
}

// fileURL addresses a file operation for the agent-visible absolute path,
// carried as trailing URL segments after /file (e.g. /file/workspace/data.csv).
// Each segment is escaped while the "/" separators are preserved, so a path with
// arbitrarily many segments round-trips intact.
func (s *Sandbox) fileURL(path string) string {
	var b strings.Builder
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			continue
		}
		b.WriteByte('/')
		b.WriteString(url.PathEscape(seg))
	}
	return s.keyed("/file" + b.String())
}

// Shutdown tears the sandbox down via DELETE /v1/<key>, cancelling its lifecycle.
func (s *Sandbox) Shutdown(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.keyed(""), nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("Shutdown: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return fmt.Errorf("Shutdown: %w", readAPIError(resp))
	}
	return nil
}

// ProxyURL returns the base URL for reaching port inside the sandbox.
// Append a path to form a full URL, e.g. sandbox.ProxyURL(8080) + "/health".
func (s *Sandbox) ProxyURL(port int) string {
	return s.keyed(fmt.Sprintf("/proxy/%d", port))
}

// Ping keeps the sandbox alive by resetting its TTL countdown.
func (s *Sandbox) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.keyed("/ping"), nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("Ping: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return fmt.Errorf("Ping: %w", readAPIError(resp))
	}
	return nil
}

// GetConfig reads the current SandboxConfig.
func (s *Sandbox) GetConfig(ctx context.Context) (*SandboxConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.keyed("/config"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetConfig: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("GetConfig: %w", readAPIError(resp))
	}
	var cfg SandboxConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("GetConfig: decode: %w", err)
	}
	return &cfg, nil
}

// GetInfo reads internal runtime information about the sandbox — currently the
// isolation mechanism in use, which sandboxd selects automatically from the
// image rather than from config.
func (s *Sandbox) GetInfo(ctx context.Context) (*SandboxInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.keyed("/info"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetInfo: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("GetInfo: %w", readAPIError(resp))
	}
	var info SandboxInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("GetInfo: decode: %w", err)
	}
	return &info, nil
}

// Snapshot captures a snapshot of the running sandbox now, without stopping it.
// The request selects which parts to capture: VM (full microVM state, keyed for
// a later resume; a no-op on container isolation) and/or Files (the writable
// filesystem). Each part is reported independently in the result.
func (s *Sandbox) Snapshot(ctx context.Context, req Snapshot) (*SnapshotResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("Snapshot: encode: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.keyed("/snapshot"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("Snapshot: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("Snapshot: %w", readAPIError(resp))
	}
	var result SnapshotResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("Snapshot: decode: %w", err)
	}
	return &result, nil
}

// ApplyConfig applies a desired SandboxConfig. The result's Applied field
// reports whether the change was committed or rolled back.
func (s *Sandbox) ApplyConfig(ctx context.Context, config SandboxConfig) (*ApplyResult, error) {
	body, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("ApplyConfig: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.keyed("/config"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ApplyConfig: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("ApplyConfig: %w", readAPIError(resp))
	}
	var result ApplyResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ApplyConfig: decode: %w", err)
	}
	return &result, nil
}

// GetPorts returns the TCP ports the sandbox currently exposes.
// Each port is reachable via ProxyURL.
func (s *Sandbox) GetPorts(ctx context.Context) ([]int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.keyed("/ports"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetPorts: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("GetPorts: %w", readAPIError(resp))
	}
	var ports []int
	if err := json.NewDecoder(resp.Body).Decode(&ports); err != nil {
		return nil, fmt.Errorf("GetPorts: decode: %w", err)
	}
	return ports, nil
}

// Exec runs a command inside the sandbox and returns the buffered result once
// the process exits. Use ExecStream when you need output incrementally.
func (s *Sandbox) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("Exec: encode: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.keyed("/exec"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("Exec: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("Exec: %w", readAPIError(resp))
	}
	var result ExecResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("Exec: decode: %w", err)
	}
	return &result, nil
}

// ExecStream starts a streaming exec and returns an ExecProcess handle.
// Drain Output to receive stdout/stderr frames, call WriteStdin to write
// to the process, and call Wait to block until exit. Pass an empty command
// in req to attach to the sandbox entrypoint's TTY (requires tty:true).
func (s *Sandbox) ExecStream(ctx context.Context, req ExecStreamRequest) (*ExecProcess, error) {
	execID := uuid.New().String()

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("ExecStream: encode: %w", err)
	}

	streamURL := s.keyed("/exec-stream/"+execID)
	stdinURL := s.keyed("/exec-stream/"+execID+"/stdin")

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, streamURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")

	resp, err := s.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("ExecStream: %w", err)
	}
	if !isSuccess(resp.StatusCode) {
		defer resp.Body.Close()
		return nil, fmt.Errorf("ExecStream: %w", readAPIError(resp))
	}

	outputCh := make(chan ExecOutput, 32)
	exitCodeCh := make(chan int, 1)
	exitErrCh := make(chan error, 1)

	go func() {
		defer resp.Body.Close()
		defer close(outputCh)
		for frame := range readSSE(resp.Body) {
			var event struct {
				Type string `json:"type"`
				Text string `json:"text"`
				Code int    `json:"code"`
			}
			if err := json.Unmarshal([]byte(frame.data), &event); err != nil {
				continue
			}
			switch event.Type {
			case "stdout":
				outputCh <- ExecOutput{Stdout: event.Text}
			case "stderr":
				outputCh <- ExecOutput{Stderr: event.Text}
			case "exit":
				exitCodeCh <- event.Code
				return
			}
		}
		exitErrCh <- fmt.Errorf("ExecStream: stream closed without exit frame")
	}()

	hc := s.http
	writeFn := func(ctx context.Context, data string) error {
		body, _ := json.Marshal(map[string]string{"data": data})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, stdinURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("WriteStdin: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("WriteStdin: %w", readAPIError(resp))
		}
		return nil
	}

	return &ExecProcess{
		ID:       execID,
		Output:   outputCh,
		writeFn:  writeFn,
		exitCode: exitCodeCh,
		exitErr:  exitErrCh,
	}, nil
}

// ListDirectory returns the immediate children of path inside the sandbox.
// path is the agent-visible absolute path (e.g. /workspace).
func (s *Sandbox) ListDirectory(ctx context.Context, path string) ([]DirEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.keyed("/directories"), nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("path", path)
	req.URL.RawQuery = q.Encode()

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ListDirectory: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("ListDirectory: %w", readAPIError(resp))
	}
	var result struct {
		Entries []DirEntry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ListDirectory: decode: %w", err)
	}
	return result.Entries, nil
}

// ReadFile returns the raw bytes of the file at path.
// path is the agent-visible absolute path (e.g. /workspace/data.csv).
func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.fileURL(path), nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ReadFile: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("ReadFile: %w", readAPIError(resp))
	}
	return io.ReadAll(resp.Body)
}

// WriteFile writes content to path inside the sandbox.
// path is the agent-visible absolute path (e.g. /workspace/data.csv) and must
// resolve beneath one of the configured fs[].mount paths.
func (s *Sandbox) WriteFile(ctx context.Context, path string, content []byte) (*UploadResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.fileURL(path), bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("WriteFile: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("WriteFile: %w", readAPIError(resp))
	}
	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("WriteFile: decode: %w", err)
	}
	return &result, nil
}

// DeleteFile removes the file or empty directory at path inside the sandbox.
// path is the agent-visible absolute path (e.g. /workspace/data.csv).
func (s *Sandbox) DeleteFile(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.fileURL(path), nil)
	if err != nil {
		return err
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("DeleteFile: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return fmt.Errorf("DeleteFile: %w", readAPIError(resp))
	}
	return nil
}

// WatchEvents streams the sandbox's activity events (egress, filesystem, exec,
// stdio, resource usage) as they happen. It auto-resumes across transient
// disconnects up to 3 times with exponential backoff. Pass lastEventID = -1 to
// start from the next new event, or a non-negative value to resume after a
// specific event.
//
// The returned channel is closed when ctx is done or retries are exhausted.
// Any terminal error is sent on errc.
func (s *Sandbox) WatchEvents(ctx context.Context, lastEventID int) (<-chan SandboxEvent, <-chan error) {
	events := make(chan SandboxEvent)
	errc := make(chan error, 1)

	go func() {
		defer close(events)
		const maxRetries = 3
		backoff := 200 * time.Millisecond
		retries := 0

		for {
			if ctx.Err() != nil {
				return
			}
			if retries > maxRetries {
				errc <- fmt.Errorf("WatchEvents: max reconnect retries exceeded")
				return
			}

			lastID, err := s.streamEvents(ctx, lastEventID, events)
			lastEventID = lastID
			if err == nil || ctx.Err() != nil {
				return
			}

			retries++
			select {
			case <-time.After(backoff):
				backoff = min(backoff*2, 30*time.Second)
			case <-ctx.Done():
				return
			}
		}
	}()

	return events, errc
}

func (s *Sandbox) streamEvents(ctx context.Context, lastEventID int, out chan<- SandboxEvent) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.keyed("/events"), nil)
	if err != nil {
		return lastEventID, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID >= 0 {
		q := req.URL.Query()
		q.Set("lastEventId", strconv.Itoa(lastEventID))
		req.URL.RawQuery = q.Encode()
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return lastEventID, err
	}
	defer resp.Body.Close()

	if !isSuccess(resp.StatusCode) {
		return lastEventID, fmt.Errorf("status %d", resp.StatusCode)
	}

	for frame := range readSSE(resp.Body) {
		var evt SandboxEvent
		if err := json.Unmarshal([]byte(frame.data), &evt); err != nil {
			continue
		}
		lastEventID = evt.ID
		select {
		case out <- evt:
		case <-ctx.Done():
			return lastEventID, nil
		}
	}
	return lastEventID, nil
}

func isSuccess(code int) bool {
	return code >= 200 && code < 300
}
