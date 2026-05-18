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
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	storagev1 "google.golang.org/api/storage/v1"
)

// gcsScopes are the OAuth scopes requested for GCS read/write access.
var gcsScopes = []string{storagev1.DevstorageReadWriteScope}

// GoogleCloudStorageConfig is the JSON the user passes via fs.backend_config.
//
// Auth: ServiceAccountJSON is used directly (server-to-server). Bucket is
// required. Prefix, when set, scopes every Store path to that sub-tree within
// the bucket (e.g. "workspace/session-42").
type GoogleCloudStorageConfig struct {
	Bucket             string `json:"bucket"`
	Prefix             string `json:"prefix,omitempty"`
	ServiceAccountJSON string `json:"service_account_json,omitempty"`
}

// GoogleCloudStorage is a [Store] backed by a GCS bucket. Uses the same
// google.golang.org/api transport layer as [GoogleDrive] so all traffic —
// API calls and token refresh — travels through the marked HTTP client and
// never touches [http.DefaultClient] or the GCE metadata server.
// GCS has no native rename; [Move] is implemented as copy-then-delete.
type GoogleCloudStorage struct {
	svc    *storagev1.ObjectsService
	bucket string
	prefix string // without leading or trailing slash; empty = bucket root
}

// NewGoogleCloudStorage constructs a GCS client from cfg.
//
// outboundMark, when non-zero, sets SO_MARK on every TCP socket the client
// opens — same escape-hatch as [NewGoogleDrive]; see that doc for details.
//
// requestLog, when non-nil, receives one JSON line per outbound HTTP request.
func NewGoogleCloudStorage(ctx context.Context, cfg GoogleCloudStorageConfig, outboundMark int, requestLog io.Writer) (*GoogleCloudStorage, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("gcs: bucket is required")
	}
	httpClient, err := newGCSAuthClient(ctx, cfg, outboundMark, requestLog)
	if err != nil {
		return nil, fmt.Errorf("gcs: auth: %w", err)
	}
	svc, err := storagev1.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("gcs: storagev1.NewService: %w", err)
	}
	return &GoogleCloudStorage{
		svc:    svc.Objects,
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

// newGCSAuthClient builds an authenticated HTTP client for GCS. The marked
// base transport is injected via context so token-refresh requests also travel
// over the marked socket (same approach as newOAuthClient for Google Drive).
func newGCSAuthClient(ctx context.Context, cfg GoogleCloudStorageConfig, mark int, requestLog io.Writer) (*http.Client, error) {
	base := markedHTTPClient(mark)
	if requestLog != nil {
		base = &http.Client{Transport: newLoggingRoundTripper(base.Transport, requestLog)}
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, base)

	if cfg.ServiceAccountJSON == "" {
		return nil, errors.New("service_account_json is required")
	}
	jcfg, err := google.JWTConfigFromJSON([]byte(cfg.ServiceAccountJSON), gcsScopes...)
	if err != nil {
		return nil, fmt.Errorf("service account: %w", err)
	}
	return jcfg.Client(ctx), nil
}

// key converts a Store-canonical /path to a GCS object key, prepending the
// configured prefix when one is set.
func (g *GoogleCloudStorage) key(p string) string {
	p = strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(p, "/")), "/")
	if g.prefix == "" {
		return p
	}
	return g.prefix + "/" + p
}

// storePath is the inverse of key: strips the configured prefix and returns a
// Store-canonical /path. Trailing "/" is stripped (directory markers).
func (g *GoogleCloudStorage) storePath(k string) string {
	if g.prefix != "" {
		k = strings.TrimPrefix(k, g.prefix+"/")
	}
	return "/" + strings.TrimSuffix(k, "/")
}

// List returns every non-directory object whose key falls under prefix.
// Directory marker objects (name ending in "/") are omitted.
func (g *GoogleCloudStorage) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	err := g.svc.List(g.bucket).Prefix(g.key(prefix)).Context(ctx).
		Pages(ctx, func(page *storagev1.Objects) error {
			for _, obj := range page.Items {
				if !strings.HasSuffix(obj.Name, "/") {
					out = append(out, g.storePath(obj.Name))
				}
			}
			return nil
		})
	return out, err
}

// ListDir returns the immediate children of dir. A "/" delimiter gives GCS
// single-level semantics; common prefixes (sub-directories) come back as
// IsDir FileInfo entries with zero size and mtime.
func (g *GoogleCloudStorage) ListDir(ctx context.Context, dir string) ([]FileInfo, error) {
	dirKey := g.key(dir)
	if dirKey != "" && !strings.HasSuffix(dirKey, "/") {
		dirKey += "/"
	}
	var out []FileInfo
	err := g.svc.List(g.bucket).Prefix(dirKey).Delimiter("/").Context(ctx).
		Pages(ctx, func(page *storagev1.Objects) error {
			for _, p := range page.Prefixes {
				out = append(out, FileInfo{Path: g.storePath(p), IsDir: true})
			}
			for _, obj := range page.Items {
				if strings.HasSuffix(obj.Name, "/") {
					continue // directory marker
				}
				mtime, _ := time.Parse(time.RFC3339, obj.Updated)
				out = append(out, FileInfo{
					Path:  g.storePath(obj.Name),
					Size:  int64(obj.Size),
					Mtime: mtime,
				})
			}
			return nil
		})
	return out, err
}

// Stat returns metadata for the object at p. GCS has no native directory
// entries; if p is not a direct object, Stat checks for any object under p/
// and returns a synthetic IsDir FileInfo when one exists.
func (g *GoogleCloudStorage) Stat(ctx context.Context, p string) (FileInfo, error) {
	canon := path.Clean("/" + strings.TrimPrefix(p, "/"))
	obj, err := g.svc.Get(g.bucket, g.key(p)).Context(ctx).Do()
	if err == nil {
		mtime, _ := time.Parse(time.RFC3339, obj.Updated)
		return FileInfo{Path: canon, Size: int64(obj.Size), Mtime: mtime}, nil
	}
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		return FileInfo{}, err
	}
	// Not a direct object — check for children to detect a directory prefix.
	prefix := g.key(p)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	page, err := g.svc.List(g.bucket).Prefix(prefix).Delimiter("/").MaxResults(1).Context(ctx).Do()
	if err != nil {
		return FileInfo{}, err
	}
	if len(page.Items) == 0 && len(page.Prefixes) == 0 {
		return FileInfo{}, ErrNotExist
	}
	return FileInfo{Path: canon, IsDir: true}, nil
}

func (g *GoogleCloudStorage) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	resp, err := g.svc.Get(g.bucket, g.key(p)).Context(ctx).Download()
	if err != nil {
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Code == http.StatusNotFound {
			return nil, ErrNotExist
		}
		return nil, err
	}
	return resp.Body, nil
}

func (g *GoogleCloudStorage) Put(ctx context.Context, p string, content io.Reader) error {
	_, err := g.svc.Insert(g.bucket, &storagev1.Object{Name: g.key(p)}).
		Context(ctx).
		Media(content).
		Do()
	return err
}

func (g *GoogleCloudStorage) Delete(ctx context.Context, p string) error {
	err := g.svc.Delete(g.bucket, g.key(p)).Context(ctx).Do()
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code == http.StatusNotFound {
		return nil
	}
	return err
}

// Move copies src to dst then deletes the source. GCS has no native rename.
func (g *GoogleCloudStorage) Move(ctx context.Context, src, dst string) error {
	_, err := g.svc.Copy(g.bucket, g.key(src), g.bucket, g.key(dst), &storagev1.Object{}).
		Context(ctx).Do()
	if err != nil {
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Code == http.StatusNotFound {
			return ErrNotExist
		}
		return fmt.Errorf("gcs move copy: %w", err)
	}
	if err := g.svc.Delete(g.bucket, g.key(src)).Context(ctx).Do(); err != nil {
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
			return fmt.Errorf("gcs move delete src: %w", err)
		}
	}
	return nil
}

// ParseGoogleCloudStorageConfig deserializes the JSON config sbxfuse receives
// via -remote-config.
func ParseGoogleCloudStorageConfig(jsonBytes []byte) (GoogleCloudStorageConfig, error) {
	var cfg GoogleCloudStorageConfig
	if len(jsonBytes) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(jsonBytes, &cfg); err != nil {
		return cfg, fmt.Errorf("parse gcs config: %w", err)
	}
	return cfg, nil
}
