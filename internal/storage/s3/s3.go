// Package s3 implements storage.Store with the AWS SDK for Go v2. It works with
// AWS S3 and S3-compatible services such as Cloudflare R2 and MinIO.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/amirkode/git-keepr/internal/storage"
)

// Options configures the client. Credentials come from the AWS default chain
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN, ...) unless overridden.
type Options struct {
	Bucket      string
	Region      string
	Endpoint    string // empty for AWS S3
	PathStyle   bool
	Credentials aws.CredentialsProvider // optional override (tests)
	HTTPClient  aws.HTTPClient          // optional override (tests)
	MaxAttempts int                     // 0 = SDK default
}

// Store is an S3-backed storage.Store.
type Store struct {
	bucket   string
	client   *awss3.Client
	uploader *manager.Uploader
	creds    aws.CredentialsProvider
}

var _ storage.Store = (*Store)(nil)

// New builds a client. It does not contact the service.
func New(ctx context.Context, o Options) (*Store, error) {
	if o.Bucket == "" || o.Region == "" {
		return nil, errors.New("s3: bucket and region are required")
	}
	opts := []func(*config.LoadOptions) error{config.WithRegion(o.Region)}
	if o.Credentials != nil {
		opts = append(opts, config.WithCredentialsProvider(o.Credentials))
	}
	if o.HTTPClient != nil {
		opts = append(opts, config.WithHTTPClient(o.HTTPClient))
	}
	if o.MaxAttempts > 0 {
		opts = append(opts, config.WithRetryMaxAttempts(o.MaxAttempts))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3: load AWS configuration: %w", err)
	}
	client := awss3.NewFromConfig(cfg, func(so *awss3.Options) {
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
		}
		so.UsePathStyle = o.PathStyle
		// Only send checksums when an operation requires them: widest compatibility with R2/MinIO.
		so.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		so.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &Store{bucket: o.Bucket, client: client, uploader: manager.NewUploader(client), creds: cfg.Credentials}, nil
}

// CheckCredentials resolves credentials from the default chain without calling S3.
func (s *Store) CheckCredentials(ctx context.Context) error {
	if s.creds == nil {
		return errors.New("s3: no credentials provider configured")
	}
	if _, err := s.creds.Retrieve(ctx); err != nil {
		// The SDK error lists every provider it probed (including EC2 metadata); keep the message actionable.
		return errors.New("s3 is enabled but no AWS credentials were found: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (and optionally AWS_SESSION_TOKEN) in the service environment, or disable storage.s3")
	}
	return nil
}

// Put uploads path to key via the SDK's managed uploader (multipart for large files).
// The object becomes visible only after the upload completes.
func (s *Store) Put(ctx context.Context, key, path string, meta map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := make(map[string]string, len(meta))
	for k, v := range meta {
		enc[k] = url.QueryEscape(v) // S3 user metadata must be ASCII
	}
	_, err = s.uploader.Upload(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        f,
		Metadata:    enc,
		ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return fmt.Errorf("s3 upload: %w", err)
	}
	return nil
}

// Head returns size and metadata, or storage.ErrNotFound.
func (s *Store) Head(ctx context.Context, key string) (storage.Object, error) {
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return storage.Object{}, storage.ErrNotFound
		}
		return storage.Object{}, fmt.Errorf("s3 head: %w", err)
	}
	meta := make(map[string]string, len(out.Metadata))
	for k, v := range out.Metadata {
		if dec, err := url.QueryUnescape(v); err == nil {
			v = dec
		}
		meta[k] = v
	}
	return storage.Object{Key: key, Size: aws.ToInt64(out.ContentLength), Metadata: meta}, nil
}

// Get streams the object into w.
func (s *Store) Get(ctx context.Context, key string, w io.Writer) error {
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return storage.ErrNotFound
		}
		return fmt.Errorf("s3 get: %w", err)
	}
	defer out.Body.Close()
	if _, err := io.Copy(w, out.Body); err != nil {
		return fmt.Errorf("s3 download: %w", err)
	}
	return nil
}

func isNotFound(err error) bool {
	var nf *types.NotFound
	var nsk *types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nsk) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}
