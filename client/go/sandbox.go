package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
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
		apiURL: fmt.Sprintf("%s/sandbox/%s", base, ref.Key),
		http:   hc,
	}
}

// ProxyURL returns the base URL for reaching port inside the sandbox.
// Append a path to form a full URL, e.g. sandbox.ProxyURL(8080) + "/health".
func (s *Sandbox) ProxyURL(port int) string {
	return fmt.Sprintf("%s/v1/proxy/%d", s.apiURL, port)
}

// Ping keeps the sandbox alive by resetting its TTL countdown.
func (s *Sandbox) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL+"/v1/ping", nil)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL+"/v1/config", nil)
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

// ApplyConfig applies a desired SandboxConfig. The result's Applied field
// reports whether the change was committed or rolled back.
func (s *Sandbox) ApplyConfig(ctx context.Context, config SandboxConfig) (*ApplyResult, error) {
	body, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("ApplyConfig: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.apiURL+"/v1/config", bytes.NewReader(body))
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL+"/v1/ports", nil)
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
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL+"/v1/exec", bytes.NewReader(body))
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

	streamURL := fmt.Sprintf("%s/v1/exec-stream/%s", s.apiURL, execID)
	stdinURL := fmt.Sprintf("%s/v1/exec-stream/%s/stdin", s.apiURL, execID)

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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL+"/v1/directories", nil)
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

// DownloadFile returns the raw bytes of the file at path.
// path is the agent-visible absolute path (e.g. /workspace/data.csv).
func (s *Sandbox) DownloadFile(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL+"/v1/file", nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("path", path)
	req.URL.RawQuery = q.Encode()

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DownloadFile: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("DownloadFile: %w", readAPIError(resp))
	}
	return io.ReadAll(resp.Body)
}

// UploadFile writes content as filename under destination inside the sandbox.
// destination must match one of the configured fs[].mount paths exactly.
func (s *Sandbox) UploadFile(ctx context.Context, destination, filename string, content []byte) (*UploadResult, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if err := mw.WriteField("destination", destination); err != nil {
		return nil, fmt.Errorf("UploadFile: build form: %w", err)
	}

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("UploadFile: build form: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return nil, fmt.Errorf("UploadFile: build form: %w", err)
	}
	mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL+"/v1/file", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("UploadFile: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("UploadFile: %w", readAPIError(resp))
	}
	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("UploadFile: decode: %w", err)
	}
	return &result, nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL+"/v1/events", nil)
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
