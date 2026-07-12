package remotefs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/microsoft"
)

// graphBaseURL is the Microsoft Graph v1.0 root. OneDrive items are
// addressed under a drive (either "/me/drive" or "/drives/{id}").
const graphBaseURL = "https://graph.microsoft.com/v1.0"

// OneDriveConfig is the JSON the user passes via fs.backend_config.
//
// Auth is OAuth2 against the Microsoft identity platform. The normal flow
// supplies AccessToken + RefreshToken + ClientID + ClientSecret (the token
// is refreshed as needed); AccessToken alone works for short-lived scratch
// use. Tenant defaults to "common".
//
// DriveID, when set, targets a specific drive (e.g. a SharePoint document
// library); otherwise the signed-in user's OneDrive ("/me/drive") is used.
// Prefix, when set, is a slash-separated subfolder path (created if absent)
// that becomes the effective root — useful for isolating runs or tenants.
type OneDriveConfig struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	Tenant       string `json:"tenant,omitempty"`
	DriveID      string `json:"drive_id,omitempty"`
	Prefix       string `json:"prefix,omitempty"`
}

// OneDrive is a [Store] backed by OneDrive via the Microsoft Graph API.
// Items are addressed by path (Graph's "root:/{path}" syntax), so unlike
// [GoogleDrive] no path→ID cache is needed. OneDrive has a native rename,
// so [Move] is a single PATCH.
//
// All Graph traffic — API calls and OAuth token refresh — travels through
// the same marked HTTP client as the other remote backends so it escapes
// the sandbox-pod's iptables REDIRECT; see [NewGoogleDrive] for why. Blob
// downloads use a short-lived pre-authenticated URL fetched over a separate
// non-OAuth client (rawClient) so the bearer token is never sent to the
// storage host.
type OneDrive struct {
	client    *http.Client // OAuth-authenticated, for Graph API calls
	rawClient *http.Client // no auth, for pre-signed download URLs
	driveRoot string       // "/me/drive" or "/drives/{id}"
	prefix    string       // without leading or trailing slash; empty = drive root

	graphBase  string // Graph API root; overridable in tests
	chunkBytes int    // resumable-upload chunk size; overridable in tests
	simpleMax  int64  // simple-upload size ceiling; overridable in tests
}

// NewOneDrive constructs a OneDrive client from cfg.
//
// outboundMark, when non-zero, sets SO_MARK on every TCP socket the client
// opens — same escape-hatch as [NewGoogleDrive]; see that doc for details.
//
// requestLog, when non-nil, receives one JSON line per outbound HTTP request.
func NewOneDrive(ctx context.Context, cfg OneDriveConfig, outboundMark int, requestLog io.Writer) (*OneDrive, error) {
	authClient, err := newOneDriveOAuthClient(ctx, cfg, outboundMark, requestLog)
	if err != nil {
		return nil, fmt.Errorf("onedrive: auth: %w", err)
	}
	raw := markedHTTPClient(outboundMark)
	if requestLog != nil {
		raw = &http.Client{Transport: newLoggingRoundTripper(raw.Transport, requestLog)}
	}

	driveRoot := "/me/drive"
	if cfg.DriveID != "" {
		driveRoot = "/drives/" + cfg.DriveID
	}
	od := &OneDrive{
		client:     authClient,
		rawClient:  raw,
		driveRoot:  driveRoot,
		prefix:     strings.Trim(cfg.Prefix, "/"),
		graphBase:  graphBaseURL,
		chunkBytes: uploadChunkBytes,
		simpleMax:  simpleUploadMaxBytes,
	}
	if od.prefix != "" {
		// Create the prefix subtree up front so the effective root exists.
		ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := od.ensureFolder(ctx2, "/"); err != nil {
			return nil, fmt.Errorf("onedrive: prefix %q: %w", cfg.Prefix, err)
		}
	}
	return od, nil
}

func newOneDriveOAuthClient(ctx context.Context, cfg OneDriveConfig, mark int, requestLog io.Writer) (*http.Client, error) {
	base := markedHTTPClient(mark)
	if requestLog != nil {
		base = &http.Client{Transport: newLoggingRoundTripper(base.Transport, requestLog)}
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, base)

	if cfg.AccessToken == "" {
		return nil, errors.New("access_token is required")
	}
	tenant := cfg.Tenant
	if tenant == "" {
		tenant = "common"
	}
	tok := &oauth2.Token{AccessToken: cfg.AccessToken, RefreshToken: cfg.RefreshToken}
	if cfg.RefreshToken != "" && cfg.ClientID != "" && cfg.ClientSecret != "" {
		// Past Expiry forces a refresh on the first request — we don't track
		// real Expiry through the env-var chain (same reasoning as gdrive).
		tok.Expiry = time.Now().Add(-time.Minute)
		oc := &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     microsoft.AzureADEndpoint(tenant),
		}
		cache := newTokenCache(ctx, oc, tok)
		return &http.Client{
			Transport: &retryOn401RoundTripper{
				base:       &oauth2.Transport{Source: cache, Base: base.Transport},
				invalidate: cache.Invalidate,
			},
		}, nil
	}
	return oauth2.NewClient(ctx, oauth2.StaticTokenSource(tok)), nil
}

// driveItem is the subset of a Graph DriveItem the store reads.
type driveItem struct {
	ID                   string  `json:"id"`
	Name                 string  `json:"name"`
	Size                 int64   `json:"size"`
	LastModifiedDateTime string  `json:"lastModifiedDateTime"`
	Folder               *folder `json:"folder"`
	File                 *struct {
		MimeType string `json:"mimeType"`
	} `json:"file"`
	DownloadURL string `json:"@microsoft.graph.downloadUrl"`
}

type folder struct {
	ChildCount int `json:"childCount"`
}

func (d *driveItem) isDir() bool { return d.Folder != nil }

func (d *driveItem) mtime() time.Time {
	t, _ := time.Parse(time.RFC3339, d.LastModifiedDateTime)
	return t
}

// effectivePath folds the configured prefix into a Store-canonical /path and
// returns it without a leading slash (empty for the effective root).
func (o *OneDrive) effectivePath(p string) string {
	p = strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(p, "/")), "/")
	if o.prefix == "" {
		return p
	}
	if p == "" {
		return o.prefix
	}
	return o.prefix + "/" + p
}

// escapePath percent-encodes each segment while preserving the slashes.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// itemURL builds the Graph URL addressing the item at Store-path p.
func (o *OneDrive) itemURL(p string) string {
	eff := o.effectivePath(p)
	if eff == "" {
		return o.graphBase + o.driveRoot + "/root"
	}
	return o.graphBase + o.driveRoot + "/root:/" + escapePath(eff)
}

// childrenURL builds the Graph URL listing the children of the folder at dir.
func (o *OneDrive) childrenURL(dir string) string {
	eff := o.effectivePath(dir)
	if eff == "" {
		return o.graphBase + o.driveRoot + "/root/children"
	}
	return o.graphBase + o.driveRoot + "/root:/" + escapePath(eff) + ":/children"
}

// contentURL builds the Graph URL for the content of the item at p.
func (o *OneDrive) contentURL(p string) string {
	return o.graphBase + o.driveRoot + "/root:/" + escapePath(o.effectivePath(p)) + ":/content"
}

// doJSON issues req on the OAuth client and, on 2xx, decodes the body into
// out (when non-nil). A 404 maps to [ErrNotExist]; other non-2xx statuses
// become an error carrying the response body.
func (o *OneDrive) doJSON(req *http.Request, out any) error {
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotExist
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("onedrive: %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (o *OneDrive) getItem(ctx context.Context, p string) (*driveItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.itemURL(p), nil)
	if err != nil {
		return nil, err
	}
	var item driveItem
	if err := o.doJSON(req, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

func (o *OneDrive) Stat(ctx context.Context, p string) (FileInfo, error) {
	item, err := o.getItem(ctx, p)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{
		Path:  path.Clean("/" + strings.TrimPrefix(p, "/")),
		Size:  item.Size,
		Mtime: item.mtime(),
		IsDir: item.isDir(),
	}, nil
}

// ListDir returns the immediate children of dir (paginated via @odata.nextLink).
func (o *OneDrive) ListDir(ctx context.Context, dir string) ([]FileInfo, error) {
	dirCanon := path.Clean("/" + strings.TrimPrefix(dir, "/"))
	var out []FileInfo
	next := o.childrenURL(dir)
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Value    []driveItem `json:"value"`
			NextLink string      `json:"@odata.nextLink"`
		}
		if err := o.doJSON(req, &page); err != nil {
			return nil, err
		}
		for i := range page.Value {
			item := &page.Value[i]
			out = append(out, FileInfo{
				Path:  path.Join(dirCanon, item.Name),
				Size:  item.Size,
				Mtime: item.mtime(),
				IsDir: item.isDir(),
			})
		}
		next = page.NextLink
	}
	return out, nil
}

// List walks the folder tree and returns the agent-visible paths of every
// regular file under prefix. Paginates internally.
func (o *OneDrive) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := o.ListDir(ctx, dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir {
				if err := walk(e.Path); err != nil {
					return err
				}
				continue
			}
			if prefix == "" || strings.HasPrefix(e.Path, prefix) {
				out = append(out, e.Path)
			}
		}
		return nil
	}
	if err := walk("/"); err != nil {
		return nil, err
	}
	return out, nil
}

func (o *OneDrive) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	// Fetch the item metadata including a short-lived pre-authenticated
	// download URL, then stream it over the non-OAuth client so the bearer
	// token is never sent to the storage host.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.itemURL(p)+"?select=id,@microsoft.graph.downloadUrl", nil)
	if err != nil {
		return nil, err
	}
	var item driveItem
	if err := o.doJSON(req, &item); err != nil {
		return nil, err // ErrNotExist mapped
	}
	if item.DownloadURL == "" {
		return nil, fmt.Errorf("onedrive: no download url for %s", p)
	}
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, item.DownloadURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.rawClient.Do(dlReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("onedrive: download %s: %s: %s", p, resp.Status, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

// simpleUploadMaxBytes is the size at or below which a single PUT to
// :/content is used. Larger content (or content whose size we can't
// determine cheaply) goes through a resumable upload session. Graph allows
// simple uploads up to 250 MiB, but switching earlier keeps a single failed
// PUT from having to re-send a large body.
const simpleUploadMaxBytes = 4 << 20 // 4 MiB

// uploadChunkBytes is the per-request chunk size for resumable uploads. It
// must be a multiple of 320 KiB per the Graph contract; 10 MiB is 32×320 KiB.
const uploadChunkBytes = 10 << 20 // 10 MiB

func (o *OneDrive) Put(ctx context.Context, p string, content io.Reader) error {
	if _, err := o.ensureFolder(ctx, path.Dir(p)); err != nil {
		return err
	}
	// Prefer a single PUT for small, size-known content. The oplog hands us a
	// seekable *os.File, so the size probe is cheap and non-consuming.
	if size, ok := readerSize(content); ok {
		if size <= o.simpleMax {
			return o.simplePut(ctx, p, content)
		}
		return o.uploadSession(ctx, p, content, size)
	}
	// Non-seekable reader: buffer to learn the size, then route accordingly.
	// This path is not expected in practice (the oplog buffer is a file).
	buf, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	if int64(len(buf)) <= o.simpleMax {
		return o.simplePut(ctx, p, bytes.NewReader(buf))
	}
	return o.uploadSession(ctx, p, bytes.NewReader(buf), int64(len(buf)))
}

// simplePut replaces the item at p in a single PUT to :/content.
func (o *OneDrive) simplePut(ctx context.Context, p string, content io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, o.contentURL(p), content)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	return o.doJSON(req, nil)
}

// uploadSession streams content of the given size to p via a resumable
// upload session — the required path for files over 250 MiB and a robust
// choice for anything large. Chunks are PUT to the session's pre-authenticated
// uploadUrl over the non-OAuth client, so the bearer token is never sent to
// the storage host.
func (o *OneDrive) uploadSession(ctx context.Context, p string, r io.Reader, size int64) error {
	createURL := o.graphBase + o.driveRoot + "/root:/" + escapePath(o.effectivePath(p)) + ":/createUploadSession"
	reqBody, err := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "replace"},
	})
	if err != nil {
		return err
	}
	createReq, err := http.NewRequestWithContext(ctx, http.MethodPost, createURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	createReq.Header.Set("Content-Type", "application/json")
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := o.doJSON(createReq, &session); err != nil {
		return fmt.Errorf("onedrive: create upload session: %w", err)
	}
	if session.UploadURL == "" {
		return fmt.Errorf("onedrive: upload session returned no uploadUrl")
	}

	buf := make([]byte, o.chunkBytes)
	var start int64
	for start < size {
		n := o.chunkBytes
		if rem := size - start; rem < int64(n) {
			n = int(rem)
		}
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			o.cancelUploadSession(ctx, session.UploadURL)
			return fmt.Errorf("onedrive: read chunk at %d: %w", start, err)
		}
		end := start + int64(n) - 1
		chunkReq, err := http.NewRequestWithContext(ctx, http.MethodPut, session.UploadURL, bytes.NewReader(buf[:n]))
		if err != nil {
			o.cancelUploadSession(ctx, session.UploadURL)
			return err
		}
		chunkReq.ContentLength = int64(n)
		chunkReq.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		// The uploadUrl is pre-authenticated; use the raw (non-OAuth) client.
		resp, err := o.rawClient.Do(chunkReq)
		if err != nil {
			o.cancelUploadSession(ctx, session.UploadURL)
			return err
		}
		final := end == size-1
		ok := (final && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated)) ||
			(!final && resp.StatusCode == http.StatusAccepted)
		if !ok {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			o.cancelUploadSession(ctx, session.UploadURL)
			return fmt.Errorf("onedrive: upload chunk %d-%d/%d: %s: %s", start, end, size, resp.Status, strings.TrimSpace(string(body)))
		}
		resp.Body.Close()
		start += int64(n)
	}
	return nil
}

// cancelUploadSession best-effort DELETEs an in-progress upload session so a
// failed upload doesn't leave a dangling session server-side.
func (o *OneDrive) cancelUploadSession(ctx context.Context, uploadURL string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, uploadURL, nil)
	if err != nil {
		return
	}
	if resp, err := o.rawClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// readerSize returns the number of unread bytes in r when it is an
// io.Seeker (the common case: an *os.File), restoring the original offset.
// The bool is false when the size can't be determined without consuming r.
func readerSize(r io.Reader) (int64, bool) {
	s, ok := r.(io.Seeker)
	if !ok {
		return 0, false
	}
	cur, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, false
	}
	end, err := s.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, false
	}
	if _, err := s.Seek(cur, io.SeekStart); err != nil {
		return 0, false
	}
	return end - cur, true
}

func (o *OneDrive) Delete(ctx context.Context, p string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, o.itemURL(p), nil)
	if err != nil {
		return err
	}
	if err := o.doJSON(req, nil); err != nil {
		if errors.Is(err, ErrNotExist) {
			return nil // idempotent
		}
		return err
	}
	return nil
}

// Move renames src to dst via a single PATCH (OneDrive has a native rename).
func (o *OneDrive) Move(ctx context.Context, src, dst string) error {
	parentID, err := o.ensureFolder(ctx, path.Dir(dst))
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"name":            path.Base(dst),
		"parentReference": map[string]string{"id": parentID},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, o.itemURL(src), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return o.doJSON(req, nil) // ErrNotExist for a missing source
}

// ensureFolder returns the item ID of the folder at p, creating it and any
// missing ancestors along the way.
func (o *OneDrive) ensureFolder(ctx context.Context, p string) (string, error) {
	p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	item, err := o.getItem(ctx, p)
	if err == nil {
		if !item.isDir() {
			return "", fmt.Errorf("onedrive: %s exists and is not a folder", p)
		}
		return item.ID, nil
	}
	if !errors.Is(err, ErrNotExist) {
		return "", err
	}
	if p == "/" {
		// The drive root always exists; a NotExist here means a real error.
		return "", fmt.Errorf("onedrive: drive root not found")
	}
	parentID, err := o.ensureFolder(ctx, path.Dir(p))
	if err != nil {
		return "", err
	}
	return o.createFolder(ctx, p, parentID, path.Base(p))
}

// createFolder POSTs a new folder under parentID. A concurrent creator
// racing us to the same name (409) is tolerated by re-reading the item.
func (o *OneDrive) createFolder(ctx context.Context, p, parentID, name string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"name":                              name,
		"folder":                            map[string]any{},
		"@microsoft.graph.conflictBehavior": "fail",
	})
	if err != nil {
		return "", err
	}
	reqURL := o.graphBase + o.driveRoot + "/items/" + url.PathEscape(parentID) + "/children"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var created driveItem
	if err := o.doJSON(req, &created); err != nil {
		// A 409 (name already taken) surfaces as a non-2xx error; fall back
		// to reading the existing folder.
		if item, gerr := o.getItem(ctx, p); gerr == nil && item.isDir() {
			return item.ID, nil
		}
		return "", fmt.Errorf("onedrive: create folder %s: %w", p, err)
	}
	return created.ID, nil
}

// ParseOneDriveConfig deserializes the JSON config sbxfuse receives via
// -remote-config.
func ParseOneDriveConfig(jsonBytes []byte) (OneDriveConfig, error) {
	var cfg OneDriveConfig
	if len(jsonBytes) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(jsonBytes, &cfg); err != nil {
		return cfg, fmt.Errorf("parse onedrive config: %w", err)
	}
	return cfg, nil
}
