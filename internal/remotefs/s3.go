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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// S3Config is the JSON the user passes via fs.backend_config.
//
// Auth: static credentials (AccessKeyID + SecretAccessKey, plus an
// optional SessionToken for temporary creds) are used directly. Bucket
// and Region are required for AWS; Endpoint targets S3-compatible
// services (MinIO, Cloudflare R2, Backblaze B2, …) and UsePathStyle
// switches from virtual-hosted to path-style addressing, which those
// services usually need. Prefix, when set, scopes every Store path to
// that sub-tree within the bucket (e.g. "workspace/session-42").
type S3Config struct {
	Bucket          string `json:"bucket"`
	Region          string `json:"region,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	UsePathStyle    bool   `json:"use_path_style,omitempty"`
}

// S3 is a [Store] backed by an Amazon S3 (or S3-compatible) bucket. It
// runs the AWS SDK over the same marked HTTP client as [GoogleCloudStorage]
// so all traffic escapes the sandbox-pod's iptables REDIRECT — see
// [NewGoogleDrive] for why. Like GCS, S3 has no native rename; [Move] is
// implemented as copy-then-delete.
type S3 struct {
	client *s3.Client
	bucket string
	prefix string // without leading or trailing slash; empty = bucket root
}

// NewS3 constructs an S3 client from cfg.
//
// outboundMark, when non-zero, sets SO_MARK on every TCP socket the client
// opens — same escape-hatch as [NewGoogleDrive]; see that doc for details.
//
// requestLog, when non-nil, receives one JSON line per outbound HTTP request.
func NewS3(_ context.Context, cfg S3Config, outboundMark int, requestLog io.Writer) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket is required")
	}

	httpClient := markedHTTPClient(outboundMark)
	if requestLog != nil {
		httpClient = &http.Client{Transport: newLoggingRoundTripper(httpClient.Transport, requestLog)}
	}

	awsCfg := aws.Config{
		Region:     cfg.Region,
		HTTPClient: httpClient,
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})

	return &S3{
		client: client,
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

// key converts a Store-canonical /path to an S3 object key, prepending the
// configured prefix when one is set.
func (s *S3) key(p string) string {
	p = strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(p, "/")), "/")
	if s.prefix == "" {
		return p
	}
	return s.prefix + "/" + p
}

// storePath is the inverse of key: strips the configured prefix and returns a
// Store-canonical /path. Trailing "/" is stripped (directory markers).
func (s *S3) storePath(k string) string {
	if s.prefix != "" {
		k = strings.TrimPrefix(k, s.prefix+"/")
	}
	return "/" + strings.TrimSuffix(k, "/")
}

// List returns every non-directory object whose key falls under prefix.
// Directory marker objects (key ending in "/") are omitted.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.key(prefix)),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			if !strings.HasSuffix(aws.ToString(obj.Key), "/") {
				out = append(out, s.storePath(aws.ToString(obj.Key)))
			}
		}
	}
	return out, nil
}

// ListDir returns the immediate children of dir. A "/" delimiter gives
// single-level semantics; common prefixes (sub-directories) come back as
// IsDir FileInfo entries with zero size and mtime.
func (s *S3) ListDir(ctx context.Context, dir string) ([]FileInfo, error) {
	dirKey := s.key(dir)
	if dirKey != "" && !strings.HasSuffix(dirKey, "/") {
		dirKey += "/"
	}
	var out []FileInfo
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Prefix:    aws.String(dirKey),
		Delimiter: aws.String("/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, cp := range page.CommonPrefixes {
			out = append(out, FileInfo{Path: s.storePath(aws.ToString(cp.Prefix)), IsDir: true})
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if strings.HasSuffix(key, "/") {
				continue // directory marker
			}
			var mtime = aws.ToTime(obj.LastModified)
			out = append(out, FileInfo{
				Path:  s.storePath(key),
				Size:  aws.ToInt64(obj.Size),
				Mtime: mtime,
			})
		}
	}
	return out, nil
}

// Stat returns metadata for the object at p. S3 has no native directory
// entries; if p is not a direct object, Stat checks for any object under p/
// and returns a synthetic IsDir FileInfo when one exists.
func (s *S3) Stat(ctx context.Context, p string) (FileInfo, error) {
	canon := path.Clean("/" + strings.TrimPrefix(p, "/"))
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(p)),
	})
	if err == nil {
		return FileInfo{Path: canon, Size: aws.ToInt64(head.ContentLength), Mtime: aws.ToTime(head.LastModified)}, nil
	}
	if !isS3NotFound(err) {
		return FileInfo{}, err
	}
	// Not a direct object — check for children to detect a directory prefix.
	prefix := s.key(p)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int32(1),
	})
	if err != nil {
		return FileInfo{}, err
	}
	if len(page.Contents) == 0 && len(page.CommonPrefixes) == 0 {
		return FileInfo{}, ErrNotExist
	}
	return FileInfo{Path: canon, IsDir: true}, nil
}

func (s *S3) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(p)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	return resp.Body, nil
}

func (s *S3) Put(ctx context.Context, p string, content io.Reader) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(p)),
		Body:   content,
	})
	return err
}

func (s *S3) Delete(ctx context.Context, p string) error {
	// S3 DeleteObject is already idempotent — deleting a missing key
	// returns success — so no NotFound special-casing is needed.
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(p)),
	})
	return err
}

// Move copies src to dst then deletes the source. S3 has no native rename.
func (s *S3) Move(ctx context.Context, src, dst string) error {
	// CopySource is "<bucket>/<key>", URL-path-escaped by the SDK.
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(s.key(dst)),
		CopySource: aws.String(s.bucket + "/" + s.key(src)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return ErrNotExist
		}
		return fmt.Errorf("s3 move copy: %w", err)
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(src)),
	}); err != nil {
		return fmt.Errorf("s3 move delete src: %w", err)
	}
	return nil
}

// isS3NotFound reports whether err is an S3 "object does not exist" error.
// GetObject surfaces NoSuchKey; HeadObject has no typed body and surfaces a
// generic NotFound; some S3-compatible services only set the HTTP status, so
// fall back to a 404 status-code check.
func isS3NotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}

// ParseS3Config deserializes the JSON config sbxfuse receives via
// -remote-config.
func ParseS3Config(jsonBytes []byte) (S3Config, error) {
	var cfg S3Config
	if len(jsonBytes) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(jsonBytes, &cfg); err != nil {
		return cfg, fmt.Errorf("parse s3 config: %w", err)
	}
	return cfg, nil
}
