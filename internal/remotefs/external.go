package remotefs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// ExternalConfig is the JSON sbxfuse receives via -remote-config for the
// "external" backend. Host is the base URL of an HTTP service that
// implements the contract in api/external_file_system.yaml.
type ExternalConfig struct {
	Host string `json:"host"`
}

// External is a [Store] backed by an HTTP host that speaks the external
// file system API (api/external_file_system.yaml). Each Store method maps
// to one request; the host is the source of truth for reads and the
// write-back target for mutations.
//
// Traffic travels over the same marked HTTP client as the Drive/GCS
// backends so requests escape the sandbox-pod's iptables REDIRECT — see
// [NewGoogleDrive] for why.
type External struct {
	base   *url.URL
	client *http.Client
}

// NewExternal constructs an External store from cfg.
//
// outboundMark, when non-zero, sets SO_MARK on every TCP socket the client
// opens — same escape-hatch as [NewGoogleDrive].
//
// requestLog, when non-nil, receives one JSON line per outbound HTTP request.
func NewExternal(_ context.Context, cfg ExternalConfig, outboundMark int, requestLog io.Writer) (*External, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("external: host is required")
	}
	base, err := url.Parse(strings.TrimRight(cfg.Host, "/"))
	if err != nil {
		return nil, fmt.Errorf("external: parse host %q: %w", cfg.Host, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("external: host must be an absolute URL, got %q", cfg.Host)
	}
	client := markedHTTPClient(outboundMark)
	if requestLog != nil {
		client = &http.Client{Transport: newLoggingRoundTripper(client.Transport, requestLog)}
	}
	return &External{base: base, client: client}, nil
}

// fileInfoJSON mirrors the FileInfo schema in external_file_system.yaml.
// Kept separate from [FileInfo] because the wire form is snake-cased and
// carries the mtime as an RFC 3339 string.
type fileInfoJSON struct {
	Path  string    `json:"path"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
	IsDir bool      `json:"is_dir"`
}

func (fi fileInfoJSON) toFileInfo() FileInfo {
	return FileInfo{Path: fi.Path, Size: fi.Size, Mtime: fi.Mtime, IsDir: fi.IsDir}
}

// canon normalizes p to a Store-canonical forward-slash path rooted at /.
func canon(p string) string {
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

// urlFor builds the absolute request URL for an endpoint, attaching query.
func (e *External) urlFor(endpoint string, query url.Values) string {
	u := *e.base
	u.Path = e.base.Path + endpoint
	u.RawQuery = query.Encode()
	return u.String()
}

func (e *External) List(ctx context.Context, prefix string) ([]string, error) {
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", canon(prefix))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.urlFor("/v1/list", q), nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := errFor(resp, "list"); err != nil {
		return nil, err
	}
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("external list: decode: %w", err)
	}
	return body.Paths, nil
}

func (e *External) ListDir(ctx context.Context, dir string) ([]FileInfo, error) {
	q := url.Values{"path": {canon(dir)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.urlFor("/v1/directory", q), nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := errFor(resp, "listdir"); err != nil {
		return nil, err
	}
	var body struct {
		Entries []fileInfoJSON `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("external listdir: decode: %w", err)
	}
	out := make([]FileInfo, len(body.Entries))
	for i, fi := range body.Entries {
		out[i] = fi.toFileInfo()
	}
	return out, nil
}

func (e *External) Stat(ctx context.Context, p string) (FileInfo, error) {
	q := url.Values{"path": {canon(p)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.urlFor("/v1/stat", q), nil)
	if err != nil {
		return FileInfo{}, err
	}
	resp, err := e.do(req)
	if err != nil {
		return FileInfo{}, err
	}
	defer resp.Body.Close()
	if err := errFor(resp, "stat"); err != nil {
		return FileInfo{}, err
	}
	var fi fileInfoJSON
	if err := json.NewDecoder(resp.Body).Decode(&fi); err != nil {
		return FileInfo{}, fmt.Errorf("external stat: decode: %w", err)
	}
	return fi.toFileInfo(), nil
}

func (e *External) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	q := url.Values{"path": {canon(p)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.urlFor("/v1/file", q), nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.do(req)
	if err != nil {
		return nil, err
	}
	if err := errFor(resp, "get"); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

func (e *External) Put(ctx context.Context, p string, content io.Reader) error {
	q := url.Values{"path": {canon(p)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, e.urlFor("/v1/file", q), content)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := e.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return errFor(resp, "put")
}

func (e *External) Delete(ctx context.Context, p string) error {
	q := url.Values{"path": {canon(p)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, e.urlFor("/v1/file", q), nil)
	if err != nil {
		return err
	}
	resp, err := e.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return errFor(resp, "delete")
}

func (e *External) Move(ctx context.Context, src, dst string) error {
	payload, err := json.Marshal(struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
	}{Src: canon(src), Dst: canon(dst)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.urlFor("/v1/move", nil), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return errFor(resp, "move")
}

// do issues req with the marked client.
func (e *External) do(req *http.Request) (*http.Response, error) {
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("external %s %s: %w", req.Method, req.URL.Path, err)
	}
	return resp, nil
}

// errFor maps a non-2xx response to an error, translating 404 to
// [ErrNotExist] so fusefs can tell "missing" from a transport failure.
// A 2xx response returns nil. The caller still owns resp.Body.
func errFor(resp *http.Response, op string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotExist
	}
	// Best-effort decode of the {"error": "..."} body for context.
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body)
	if body.Error != "" {
		return fmt.Errorf("external %s: %s (%s)", op, body.Error, resp.Status)
	}
	return fmt.Errorf("external %s: %s", op, resp.Status)
}

// ParseExternalConfig deserializes the JSON config sbxfuse receives via
// -remote-config for the external backend.
func ParseExternalConfig(jsonBytes []byte) (ExternalConfig, error) {
	var cfg ExternalConfig
	if len(jsonBytes) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(jsonBytes, &cfg); err != nil {
		return cfg, fmt.Errorf("parse external config: %w", err)
	}
	return cfg, nil
}
