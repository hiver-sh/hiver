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

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// gcsScopes are the OAuth scopes requested for GCS read/write access.
var gcsScopes = []string{storage.ScopeReadWrite}

// GoogleCloudStorageConfig is the JSON the user passes via fs.backend_config.
//
// Auth: if ServiceAccountJSON is set it is used directly (server-to-server,
// recommended for production). Otherwise Application Default Credentials are
// used — GOOGLE_APPLICATION_CREDENTIALS env var, gcloud user credentials, or
// the GCE/GKE metadata server, whichever is found first.
//
// Bucket is required. Prefix, when set, scopes every Store path to that
// sub-tree within the bucket (e.g. "workspace/session-42").
type GoogleCloudStorageConfig struct {
	Bucket             string `json:"bucket"`
	Prefix             string `json:"prefix,omitempty"`
	ServiceAccountJSON string `json:"service_account_json,omitempty"`
}

// GoogleCloudStorage is a [Store] backed by a GCS bucket. All object keys
// are formed by joining the optional Prefix with the caller-supplied path.
// GCS has no native rename; [Move] is implemented as copy-then-delete.
type GoogleCloudStorage struct {
	bucket *storage.BucketHandle
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
	client, err := storage.NewClient(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("gcs: storage.NewClient: %w", err)
	}
	return &GoogleCloudStorage{
		bucket: client.Bucket(cfg.Bucket),
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
	it := g.bucket.Objects(ctx, &storage.Query{Prefix: g.key(prefix)})
	var out []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(attrs.Name, "/") {
			continue // directory marker — not a real file
		}
		out = append(out, g.storePath(attrs.Name))
	}
	return out, nil
}

// ListDir returns the immediate children of dir. A "/" delimiter gives GCS
// single-level semantics; common prefixes (sub-directories) come back as
// IsDir FileInfo entries with zero size and mtime.
func (g *GoogleCloudStorage) ListDir(ctx context.Context, dir string) ([]FileInfo, error) {
	dirKey := g.key(dir)
	if dirKey != "" && !strings.HasSuffix(dirKey, "/") {
		dirKey += "/"
	}
	it := g.bucket.Objects(ctx, &storage.Query{Prefix: dirKey, Delimiter: "/"})
	var out []FileInfo
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		if attrs.Prefix != "" {
			out = append(out, FileInfo{Path: g.storePath(attrs.Prefix), IsDir: true})
			continue
		}
		if strings.HasSuffix(attrs.Name, "/") {
			continue // directory marker object
		}
		out = append(out, FileInfo{
			Path:  g.storePath(attrs.Name),
			Size:  attrs.Size,
			Mtime: attrs.Updated,
		})
	}
	return out, nil
}

// Stat returns metadata for the object at p. GCS has no native directory
// entries; if p is not a direct object, Stat checks for any object under p/
// and returns a synthetic IsDir FileInfo when one exists.
func (g *GoogleCloudStorage) Stat(ctx context.Context, p string) (FileInfo, error) {
	canon := path.Clean("/" + strings.TrimPrefix(p, "/"))
	attrs, err := g.bucket.Object(g.key(p)).Attrs(ctx)
	if err == nil {
		return FileInfo{Path: canon, Size: attrs.Size, Mtime: attrs.Updated}, nil
	}
	if !errors.Is(err, storage.ErrObjectNotExist) {
		return FileInfo{}, err
	}
	// Not a direct object — check for children to detect a directory prefix.
	prefix := g.key(p)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	it := g.bucket.Objects(ctx, &storage.Query{Prefix: prefix, Delimiter: "/"})
	_, next := it.Next()
	if errors.Is(next, iterator.Done) {
		return FileInfo{}, ErrNotExist
	}
	if next != nil {
		return FileInfo{}, next
	}
	return FileInfo{Path: canon, IsDir: true}, nil
}

func (g *GoogleCloudStorage) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	rc, err := g.bucket.Object(g.key(p)).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, ErrNotExist
	}
	return rc, err
}

func (g *GoogleCloudStorage) Put(ctx context.Context, p string, content io.Reader) error {
	wc := g.bucket.Object(g.key(p)).NewWriter(ctx)
	if _, err := io.Copy(wc, content); err != nil {
		_ = wc.Close()
		return err
	}
	return wc.Close()
}

func (g *GoogleCloudStorage) Delete(ctx context.Context, p string) error {
	err := g.bucket.Object(g.key(p)).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return err
}

// Move copies src to dst then deletes the source. GCS has no native rename.
func (g *GoogleCloudStorage) Move(ctx context.Context, src, dst string) error {
	srcObj := g.bucket.Object(g.key(src))
	dstObj := g.bucket.Object(g.key(dst))
	if _, err := dstObj.CopierFrom(srcObj).Run(ctx); err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return ErrNotExist
		}
		return fmt.Errorf("gcs move copy: %w", err)
	}
	if err := srcObj.Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("gcs move delete src: %w", err)
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
