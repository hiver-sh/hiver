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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

// AzureBlobConfig is the JSON the user passes via fs.backend_config.
//
// Container is required (the Azure equivalent of a bucket). Auth is one
// of, in precedence order: ConnectionString (self-contained), SASToken
// (with Account/Endpoint), or Account + AccountKey (shared key). Prefix,
// when set, scopes every Store path to that sub-tree within the container
// (e.g. "workspace/session-42"). Endpoint overrides the default
// "https://{account}.blob.core.windows.net" service URL — set it for the
// Azurite emulator or a custom domain.
type AzureBlobConfig struct {
	Account          string `json:"account,omitempty"`
	Container        string `json:"container"`
	Prefix           string `json:"prefix,omitempty"`
	AccountKey       string `json:"account_key,omitempty"`
	ConnectionString string `json:"connection_string,omitempty"`
	SASToken         string `json:"sas_token,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
}

// AzureBlob is a [Store] backed by an Azure Blob Storage container. It runs
// the Azure SDK over the same marked HTTP client as [GoogleCloudStorage] and
// [S3] so all traffic escapes the sandbox-pod's iptables REDIRECT — see
// [NewGoogleDrive] for why. Azure has no native rename; [Move] is composed
// from Get + Put + Delete.
type AzureBlob struct {
	client *container.Client
	prefix string // without leading or trailing slash; empty = container root
}

// NewAzureBlob constructs an Azure Blob client from cfg.
//
// outboundMark, when non-zero, sets SO_MARK on every TCP socket the client
// opens — same escape-hatch as [NewGoogleDrive]; see that doc for details.
//
// requestLog, when non-nil, receives one JSON line per outbound HTTP request.
func NewAzureBlob(_ context.Context, cfg AzureBlobConfig, outboundMark int, requestLog io.Writer) (*AzureBlob, error) {
	if cfg.Container == "" {
		return nil, fmt.Errorf("azblob: container is required")
	}

	httpClient := markedHTTPClient(outboundMark)
	if requestLog != nil {
		httpClient = &http.Client{Transport: newLoggingRoundTripper(httpClient.Transport, requestLog)}
	}
	opts := &container.ClientOptions{}
	opts.Transport = httpClient

	// serviceURL is the blob endpoint; containerURL appends the container name.
	serviceURL := strings.TrimRight(cfg.Endpoint, "/")
	if serviceURL == "" {
		if cfg.Account == "" {
			return nil, errors.New("azblob: account is required when endpoint is not set")
		}
		serviceURL = fmt.Sprintf("https://%s.blob.core.windows.net", cfg.Account)
	}
	containerURL := serviceURL + "/" + cfg.Container

	var (
		c   *container.Client
		err error
	)
	switch {
	case cfg.ConnectionString != "":
		c, err = container.NewClientFromConnectionString(cfg.ConnectionString, cfg.Container, opts)
	case cfg.SASToken != "":
		// A SAS token authorizes the URL itself, so no credential is attached.
		c, err = container.NewClientWithNoCredential(containerURL+"?"+strings.TrimPrefix(cfg.SASToken, "?"), opts)
	case cfg.AccountKey != "":
		if cfg.Account == "" {
			return nil, errors.New("azblob: account is required with account_key")
		}
		cred, cerr := azblob.NewSharedKeyCredential(cfg.Account, cfg.AccountKey)
		if cerr != nil {
			return nil, fmt.Errorf("azblob: shared key credential: %w", cerr)
		}
		c, err = container.NewClientWithSharedKeyCredential(containerURL, cred, opts)
	default:
		return nil, errors.New("azblob: one of connection_string, sas_token, or account_key is required")
	}
	if err != nil {
		return nil, fmt.Errorf("azblob: new client: %w", err)
	}

	return &AzureBlob{
		client: c,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

// key converts a Store-canonical /path to a blob name, prepending the
// configured prefix when one is set.
func (a *AzureBlob) key(p string) string {
	p = strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(p, "/")), "/")
	if a.prefix == "" {
		return p
	}
	return a.prefix + "/" + p
}

// storePath is the inverse of key: strips the configured prefix and returns a
// Store-canonical /path. Trailing "/" is stripped (directory markers).
func (a *AzureBlob) storePath(k string) string {
	if a.prefix != "" {
		k = strings.TrimPrefix(k, a.prefix+"/")
	}
	return "/" + strings.TrimSuffix(k, "/")
}

// List returns every non-directory blob whose name falls under prefix.
// Directory marker blobs (name ending in "/") are omitted.
func (a *AzureBlob) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	pager := a.client.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix: to.Ptr(a.key(prefix)),
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range page.Segment.BlobItems {
			name := deref(b.Name)
			if !strings.HasSuffix(name, "/") {
				out = append(out, a.storePath(name))
			}
		}
	}
	return out, nil
}

// ListDir returns the immediate children of dir. A "/" delimiter gives
// single-level semantics; blob prefixes (sub-directories) come back as
// IsDir FileInfo entries with zero size and mtime.
func (a *AzureBlob) ListDir(ctx context.Context, dir string) ([]FileInfo, error) {
	dirKey := a.key(dir)
	if dirKey != "" && !strings.HasSuffix(dirKey, "/") {
		dirKey += "/"
	}
	var out []FileInfo
	pager := a.client.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{
		Prefix: to.Ptr(dirKey),
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, bp := range page.Segment.BlobPrefixes {
			out = append(out, FileInfo{Path: a.storePath(deref(bp.Name)), IsDir: true})
		}
		for _, b := range page.Segment.BlobItems {
			name := deref(b.Name)
			if strings.HasSuffix(name, "/") {
				continue // directory marker
			}
			fi := FileInfo{Path: a.storePath(name)}
			if b.Properties != nil {
				if b.Properties.ContentLength != nil {
					fi.Size = *b.Properties.ContentLength
				}
				if b.Properties.LastModified != nil {
					fi.Mtime = *b.Properties.LastModified
				}
			}
			out = append(out, fi)
		}
	}
	return out, nil
}

// Stat returns metadata for the blob at p. Azure has no native directory
// entries; if p is not a direct blob, Stat checks for any blob under p/ and
// returns a synthetic IsDir FileInfo when one exists.
func (a *AzureBlob) Stat(ctx context.Context, p string) (FileInfo, error) {
	canon := path.Clean("/" + strings.TrimPrefix(p, "/"))
	props, err := a.client.NewBlobClient(a.key(p)).GetProperties(ctx, nil)
	if err == nil {
		fi := FileInfo{Path: canon}
		if props.ContentLength != nil {
			fi.Size = *props.ContentLength
		}
		if props.LastModified != nil {
			fi.Mtime = *props.LastModified
		}
		return fi, nil
	}
	if !isAzNotFound(err) {
		return FileInfo{}, err
	}
	// Not a direct blob — check for children to detect a directory prefix.
	prefix := a.key(p)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	pager := a.client.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{
		Prefix:     to.Ptr(prefix),
		MaxResults: to.Ptr(int32(1)),
	})
	if pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return FileInfo{}, err
		}
		if len(page.Segment.BlobItems) > 0 || len(page.Segment.BlobPrefixes) > 0 {
			return FileInfo{Path: canon, IsDir: true}, nil
		}
	}
	return FileInfo{}, ErrNotExist
}

func (a *AzureBlob) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	resp, err := a.client.NewBlobClient(a.key(p)).DownloadStream(ctx, nil)
	if err != nil {
		if isAzNotFound(err) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	return resp.Body, nil
}

func (a *AzureBlob) Put(ctx context.Context, p string, content io.Reader) error {
	_, err := a.client.NewBlockBlobClient(a.key(p)).UploadStream(ctx, content, nil)
	return err
}

func (a *AzureBlob) Delete(ctx context.Context, p string) error {
	_, err := a.client.NewBlobClient(a.key(p)).Delete(ctx, nil)
	if isAzNotFound(err) {
		return nil // idempotent
	}
	return err
}

// Move renames src to dst. Azure server-side copy requires an authorized
// source URL (a SAS or public blob), so — as the [Store] contract allows for
// backends without a native rename — it is composed from Get + Put + Delete.
func (a *AzureBlob) Move(ctx context.Context, src, dst string) error {
	rc, err := a.Get(ctx, src)
	if err != nil {
		return err // ErrNotExist for a missing source
	}
	defer rc.Close()
	if err := a.Put(ctx, dst, rc); err != nil {
		return fmt.Errorf("azblob move put: %w", err)
	}
	if err := a.Delete(ctx, src); err != nil {
		return fmt.Errorf("azblob move delete src: %w", err)
	}
	return nil
}

// isAzNotFound reports whether err is an Azure "blob does not exist" error.
func isAzNotFound(err error) bool {
	return bloberror.HasCode(err, bloberror.BlobNotFound)
}

// deref returns the value behind a *string, or "" when nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ParseAzureBlobConfig deserializes the JSON config sbxfuse receives via
// -remote-config.
func ParseAzureBlobConfig(jsonBytes []byte) (AzureBlobConfig, error) {
	var cfg AzureBlobConfig
	if len(jsonBytes) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(jsonBytes, &cfg); err != nil {
		return cfg, fmt.Errorf("parse azblob config: %w", err)
	}
	return cfg, nil
}
