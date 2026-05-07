package remotefs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// driveFolderMime is what Drive returns for File.MimeType on folders.
const driveFolderMime = "application/vnd.google-apps.folder"

// GoogleDriveConfig is the JSON the user passes via fs.backend_config.
//
// Auth flavor is whichever set of fields is populated, in this order:
//
//  1. ServiceAccountJSON    — server-to-server creds; no user, no
//     refresh dance, recommended for production.
//  2. AccessToken + Refresh + ClientID + ClientSecret — refreshable
//     user credentials, the normal interactive
//     flow.
//  3. AccessToken alone     — short-lived (≈1h), no refresh; convenient
//     for OAuth-Playground-style scratch work.
//
// FolderID, when set, scopes every operation to that Drive folder (the
// store treats it as the root). Leaving it blank uses the user's "My
// Drive" root, which is rarely what you want for a sandbox workspace.
type GoogleDriveConfig struct {
	AccessToken        string `json:"access_token,omitempty"`
	RefreshToken       string `json:"refresh_token,omitempty"`
	ClientID           string `json:"client_id,omitempty"`
	ClientSecret       string `json:"client_secret,omitempty"`
	ServiceAccountJSON string `json:"service_account_json,omitempty"`
	FolderID           string `json:"folder_id,omitempty"`
}

// driveScopes is what we ask Google for when refreshing user tokens.
// drive.FileScope (drive.file) is the narrowest — only files the
// sandbox creates or opens. For scoped folder access, drive.scope is
// usually what users grant.
var driveScopes = []string{drive.DriveScope}

// GoogleDrive is a [Store] backed by the real Google Drive API. Path
// → fileID resolution is cached lazily; the cache is invalidated on
// Delete/Move so stale IDs don't leak across mutations.
//
// All paths from the caller are forward-slash, rooted at /. They map
// to a folder hierarchy on Drive rooted at FolderID — intermediate
// folders are auto-created on Put.
type GoogleDrive struct {
	svc    *drive.Service
	rootID string

	cacheMu  sync.Mutex
	pathToID map[string]string
}

// NewGoogleDrive constructs a Drive client from cfg.
//
// outboundMark, when non-zero, sets SO_MARK on every TCP socket the
// returned client opens — both for OAuth token refresh and for the
// Drive API calls themselves. sandboxd uses this so sbxfuse's
// platform traffic to Google escapes the iptables OUTPUT REDIRECT
// it installed in the sandbox-pod's netns. Pass 0 when no escape is
// needed (e.g. unit tests with no iptables).
//
// requestLog, when non-nil, receives one JSON line per outbound HTTP
// request (Drive API + OAuth token endpoint), useful for measuring
// API call volume and the impact of the fusefs stat cache.
func NewGoogleDrive(ctx context.Context, cfg GoogleDriveConfig, outboundMark int, requestLog io.Writer) (*GoogleDrive, error) {
	httpClient, err := newOAuthClient(ctx, cfg, outboundMark, requestLog)
	if err != nil {
		return nil, fmt.Errorf("gdrive: auth: %w", err)
	}
	svc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("gdrive: drive.NewService: %w", err)
	}
	root := cfg.FolderID
	if root == "" {
		root = "root" // Drive's alias for the user's My Drive root
	}
	return &GoogleDrive{
		svc:      svc,
		rootID:   root,
		pathToID: map[string]string{"/": root},
	}, nil
}

func newOAuthClient(ctx context.Context, cfg GoogleDriveConfig, mark int, requestLog io.Writer) (*http.Client, error) {
	// All OAuth and API traffic goes through the marked transport so
	// the kernel's first iptables nat-OUTPUT rule (RETURN on -m mark
	// match) skips the REDIRECT. We pass our base client to the
	// oauth2 package via context — its token-refresh requests pick it
	// up the same way the Drive client does.
	base := markedHTTPClient(mark)
	if requestLog != nil {
		// Wrap the marked transport so every outbound request — OAuth
		// token refresh and Drive API alike — gets a JSON-line log entry.
		base = &http.Client{Transport: newLoggingRoundTripper(base.Transport, requestLog)}
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, base)

	if cfg.ServiceAccountJSON != "" {
		jcfg, err := google.JWTConfigFromJSON([]byte(cfg.ServiceAccountJSON), driveScopes...)
		if err != nil {
			return nil, fmt.Errorf("service account: %w", err)
		}
		return jcfg.Client(ctx), nil
	}
	if cfg.AccessToken == "" {
		return nil, errors.New("either access_token or service_account_json is required")
	}
	tok := &oauth2.Token{AccessToken: cfg.AccessToken, RefreshToken: cfg.RefreshToken}
	if cfg.RefreshToken != "" && cfg.ClientID != "" && cfg.ClientSecret != "" {
		// Past Expiry on the seed forces a refresh on the very first
		// request — we don't track real Expiry through the env-var
		// chain, so we can't trust the saved access token's lifetime.
		tok.Expiry = time.Now().Add(-time.Minute)
		oc := &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     google.Endpoint,
			Scopes:       driveScopes,
		}
		// Build the chain ourselves so we own the token cache:
		//   retryOn401  (catches 401, invalidates cache, retries once)
		//     └── oauth2.Transport (injects Authorization header)
		//           └── base.Transport  (marked + maybe-logging TCP)
		// On a 401 we Invalidate the cache; the retry's call to
		// Source.Token() then misses, refreshes, and the second
		// attempt goes out with a fresh access token.
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

// resolve walks the cached path → fileID map, calling Drive's Files.List
// to fill gaps. Returns the fileID at p, or [ErrNotExist] if any
// component is missing. The cache is best-effort — entries can stale
// out under concurrent edits, in which case the caller's API call
// returns 404 and we invalidate.
func (g *GoogleDrive) resolve(ctx context.Context, p string) (string, error) {
	p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	g.cacheMu.Lock()
	if id, ok := g.pathToID[p]; ok {
		g.cacheMu.Unlock()
		return id, nil
	}
	g.cacheMu.Unlock()

	parent := path.Dir(p)
	parentID, err := g.resolve(ctx, parent)
	if err != nil {
		return "", err
	}
	name := path.Base(p)
	id, err := g.findChild(ctx, parentID, name)
	if err != nil {
		return "", err
	}
	g.cacheMu.Lock()
	g.pathToID[p] = id
	g.cacheMu.Unlock()
	return id, nil
}

func (g *GoogleDrive) findChild(ctx context.Context, parentID, name string) (string, error) {
	q := fmt.Sprintf("'%s' in parents and name = %q and trashed = false", parentID, name)
	resp, err := g.svc.Files.List().
		Context(ctx).
		Q(q).
		Fields("files(id,name,mimeType)").
		Do()
	if err != nil {
		return "", err
	}
	if len(resp.Files) == 0 {
		return "", ErrNotExist
	}
	return resp.Files[0].Id, nil
}

// ensureFolder returns the folder ID at p, creating any missing
// intermediate folders along the way. Used by Put/Move when the
// destination's parent doesn't exist yet.
func (g *GoogleDrive) ensureFolder(ctx context.Context, p string) (string, error) {
	p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	if id, err := g.resolve(ctx, p); err == nil {
		return id, nil
	} else if !errors.Is(err, ErrNotExist) {
		return "", err
	}
	parentID, err := g.ensureFolder(ctx, path.Dir(p))
	if err != nil {
		return "", err
	}
	f, err := g.svc.Files.Create(&drive.File{
		Name:     path.Base(p),
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{parentID},
	}).Context(ctx).Fields("id").Do()
	if err != nil {
		return "", fmt.Errorf("create folder %s: %w", p, err)
	}
	g.cacheMu.Lock()
	g.pathToID[p] = f.Id
	g.cacheMu.Unlock()
	return f.Id, nil
}

func (g *GoogleDrive) invalidate(p string) {
	g.cacheMu.Lock()
	delete(g.pathToID, p)
	g.cacheMu.Unlock()
}

// List walks the folder tree and returns the agent-visible paths of
// every regular file under prefix. Paginates internally.
func (g *GoogleDrive) List(ctx context.Context, prefix string) ([]string, error) {
	startID, err := g.resolve(ctx, "/")
	if err != nil {
		return nil, err
	}
	var out []string
	var walk func(folderID, folderPath string) error
	walk = func(folderID, folderPath string) error {
		pageToken := ""
		for {
			resp, err := g.svc.Files.List().
				Context(ctx).
				Q(fmt.Sprintf("'%s' in parents and trashed = false", folderID)).
				Fields("nextPageToken, files(id,name,mimeType)").
				PageToken(pageToken).
				Do()
			if err != nil {
				return err
			}
			for _, f := range resp.Files {
				childPath := path.Join(folderPath, f.Name)
				if f.MimeType == "application/vnd.google-apps.folder" {
					if err := walk(f.Id, childPath); err != nil {
						return err
					}
					continue
				}
				if prefix == "" || strings.HasPrefix(childPath, prefix) {
					out = append(out, childPath)
				}
			}
			if resp.NextPageToken == "" {
				break
			}
			pageToken = resp.NextPageToken
		}
		return nil
	}
	if err := walk(startID, "/"); err != nil {
		return nil, err
	}
	return out, nil
}

// Stat returns metadata for the file or folder at p. One Drive API call
// (files.get with size/mimeType/modifiedTime) — cheap enough that fusefs
// can call it on every Lookup/Attr to keep the agent's view fresh.
func (g *GoogleDrive) Stat(ctx context.Context, p string) (FileInfo, error) {
	id, err := g.resolve(ctx, p)
	if err != nil {
		return FileInfo{}, err
	}
	f, err := g.svc.Files.Get(id).
		Context(ctx).
		Fields("id,name,mimeType,size,modifiedTime").
		Do()
	if err != nil {
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Code == http.StatusNotFound {
			g.invalidate(path.Clean("/" + strings.TrimPrefix(p, "/")))
			return FileInfo{}, ErrNotExist
		}
		return FileInfo{}, err
	}
	mtime, _ := time.Parse(time.RFC3339, f.ModifiedTime)
	return FileInfo{
		Path:  path.Clean("/" + strings.TrimPrefix(p, "/")),
		Size:  f.Size,
		Mtime: mtime,
		IsDir: f.MimeType == driveFolderMime,
	}, nil
}

// ListDir returns the immediate children of dir (one Drive page request,
// not a recursive walk). Used for FUSE ReadDirAll.
func (g *GoogleDrive) ListDir(ctx context.Context, dir string) ([]FileInfo, error) {
	folderID, err := g.resolve(ctx, dir)
	if err != nil {
		return nil, err
	}
	dirCanon := path.Clean("/" + strings.TrimPrefix(dir, "/"))
	var out []FileInfo
	pageToken := ""
	for {
		resp, err := g.svc.Files.List().
			Context(ctx).
			Q(fmt.Sprintf("'%s' in parents and trashed = false", folderID)).
			Fields("nextPageToken, files(id,name,mimeType,size,modifiedTime)").
			PageToken(pageToken).
			Do()
		if err != nil {
			return nil, err
		}
		for _, f := range resp.Files {
			childPath := path.Join(dirCanon, f.Name)
			mtime, _ := time.Parse(time.RFC3339, f.ModifiedTime)
			out = append(out, FileInfo{
				Path:  childPath,
				Size:  f.Size,
				Mtime: mtime,
				IsDir: f.MimeType == driveFolderMime,
			})
			// Warm the path → ID cache so a follow-up Stat/Get on this
			// child doesn't re-issue a findChild call.
			g.cacheMu.Lock()
			g.pathToID[childPath] = f.Id
			g.cacheMu.Unlock()
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return out, nil
}

func (g *GoogleDrive) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	id, err := g.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	resp, err := g.svc.Files.Get(id).Context(ctx).Download()
	if err != nil {
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Code == http.StatusNotFound {
			return nil, ErrNotExist
		}
		return nil, err
	}
	return resp.Body, nil
}

func (g *GoogleDrive) Put(ctx context.Context, p string, content io.Reader) error {
	parentID, err := g.ensureFolder(ctx, path.Dir(p))
	if err != nil {
		return err
	}
	name := path.Base(p)
	if id, err := g.findChild(ctx, parentID, name); err == nil {
		// Existing file → update content via Files.Update.
		_, err := g.svc.Files.Update(id, &drive.File{}).
			Context(ctx).
			Media(content).
			Fields("id").
			Do()
		return err
	} else if !errors.Is(err, ErrNotExist) {
		return err
	}
	f, err := g.svc.Files.Create(&drive.File{
		Name:    name,
		Parents: []string{parentID},
	}).Context(ctx).Media(content).Fields("id").Do()
	if err != nil {
		return err
	}
	g.cacheMu.Lock()
	g.pathToID[path.Clean("/"+strings.TrimPrefix(p, "/"))] = f.Id
	g.cacheMu.Unlock()
	return nil
}

func (g *GoogleDrive) Delete(ctx context.Context, p string) error {
	id, err := g.resolve(ctx, p)
	if errors.Is(err, ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := g.svc.Files.Delete(id).Context(ctx).Do(); err != nil {
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Code == http.StatusNotFound {
			g.invalidate(path.Clean("/" + strings.TrimPrefix(p, "/")))
			return nil
		}
		return err
	}
	g.invalidate(path.Clean("/" + strings.TrimPrefix(p, "/")))
	return nil
}

func (g *GoogleDrive) Move(ctx context.Context, src, dst string) error {
	id, err := g.resolve(ctx, src)
	if err != nil {
		return err
	}
	srcMeta, err := g.svc.Files.Get(id).Context(ctx).Fields("parents").Do()
	if err != nil {
		return err
	}
	dstParentID, err := g.ensureFolder(ctx, path.Dir(dst))
	if err != nil {
		return err
	}
	upd := g.svc.Files.Update(id, &drive.File{Name: path.Base(dst)}).
		Context(ctx).
		Fields("id")
	if len(srcMeta.Parents) > 0 {
		upd = upd.RemoveParents(strings.Join(srcMeta.Parents, ","))
	}
	upd = upd.AddParents(dstParentID)
	if _, err := upd.Do(); err != nil {
		return err
	}
	g.invalidate(path.Clean("/" + strings.TrimPrefix(src, "/")))
	g.cacheMu.Lock()
	g.pathToID[path.Clean("/"+strings.TrimPrefix(dst, "/"))] = id
	g.cacheMu.Unlock()
	return nil
}

// ParseGoogleDriveConfig is a small convenience that lets sbxfuse
// pass through the spec's backend_config map verbatim.
func ParseGoogleDriveConfig(jsonBytes []byte) (GoogleDriveConfig, error) {
	var cfg GoogleDriveConfig
	if len(jsonBytes) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(jsonBytes, &cfg); err != nil {
		return cfg, fmt.Errorf("parse google-drive config: %w", err)
	}
	return cfg, nil
}
