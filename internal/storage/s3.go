package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// On feature/s3/manager: the SDK marks it superseded by feature/s3/transfermanager,
// but that replacement is still pre-1.0 (v0.4.x) with an unstable API. Uploading
// is this tool's durability boundary, so we stay on the stable, still-maintained
// manager package and revisit when transfermanager reaches v1. The SA1019
// suppressions below exist only for that reason.

const (
	// uploadPartSize bounds each HTTP request. Small parts keep any single
	// request short enough to survive proxies and CDN gateways that time out
	// long uploads; 16MiB still allows objects up to ~160GB (10,000 parts).
	uploadPartSize = 16 * 1024 * 1024

	// uploadConcurrency caps in-flight parts. Peak upload memory is roughly
	// uploadPartSize * uploadConcurrency.
	uploadConcurrency = 4

	// maxRetryAttempts is raised above the SDK default of 3 because
	// S3-compatible gateways in front of the real store return transient
	// 502/503/504 far more often than AWS S3 does.
	maxRetryAttempts = 6
)

type S3 struct {
	client *s3.Client
	//lint:ignore SA1019 stable API; replacement is pre-1.0. See note above.
	uploader    *manager.Uploader
	bucket      string
	endpoint    string
	maxAttempts int
}

// Option adjusts an S3 provider. Used by tests to keep retry backoff short.
type Option func(*s3Settings)

type s3Settings struct {
	maxAttempts int
}

// WithMaxRetryAttempts overrides how many times a transient failure is retried.
func WithMaxRetryAttempts(n int) Option {
	return func(o *s3Settings) { o.maxAttempts = n }
}

// normalizeEndpoint makes a bare host usable as a v2 BaseEndpoint.
//
// aws-sdk-go v1 accepted "s3.example.com" and assumed https. v2 parses the
// value as a URL and fails without a scheme, so an endpoint that worked before
// the migration must keep working now.
func normalizeEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	if !strings.Contains(endpoint, "://") {
		return "https://" + endpoint
	}
	return endpoint
}

func NewS3(bucket, region, endpoint, accessKey, secretKey string, opts ...Option) (*S3, error) {
	settings := s3Settings{maxAttempts: maxRetryAttempts}
	for _, opt := range opts {
		opt(&settings)
	}

	endpoint = normalizeEndpoint(endpoint)
	if endpoint != "" {
		if _, err := url.Parse(endpoint); err != nil {
			return nil, fmt.Errorf("invalid s3 endpoint %q: %w", endpoint, err)
		}
	}

	awsCfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = settings.maxAttempts
			})
		},
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})

	//lint:ignore SA1019 stable API; replacement is pre-1.0. See note above.
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = uploadPartSize
		u.Concurrency = uploadConcurrency
	})

	return &S3{
		client:      client,
		uploader:    uploader,
		bucket:      bucket,
		endpoint:    endpoint,
		maxAttempts: settings.maxAttempts,
	}, nil
}

// describeError turns an SDK error into something an operator can act on.
//
// A gateway in front of an S3-compatible store answers outages with an HTML
// page, which the SDK cannot parse as an S3 <Error>. Left alone that surfaces
// as a SerializationError with a hex dump of the HTML and no indication that
// the real problem was an HTTP 502.
func (s *S3) describeError(op string, err error) error {
	if err == nil {
		return nil
	}

	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		status := respErr.HTTPStatusCode()
		if status >= 500 {
			return fmt.Errorf("s3 %s failed: endpoint %s returned HTTP %d. "+
				"This is a gateway/upstream failure at the storage provider, not a credentials problem; "+
				"it was retried %d times: %w",
				op, s.endpointLabel(), status, s.maxAttempts, err)
		}
		return fmt.Errorf("s3 %s failed: endpoint %s returned HTTP %d: %w",
			op, s.endpointLabel(), status, err)
	}

	return fmt.Errorf("s3 %s failed (endpoint %s): %w", op, s.endpointLabel(), err)
}

func (s *S3) endpointLabel() string {
	if s.endpoint == "" {
		return "AWS S3"
	}
	return s.endpoint
}

// Store streams data to S3 as a multipart upload. Unlike PutObject this needs
// no seekable body, is not capped at 5GB, and splits the transfer into
// individually retryable parts. A read error aborts the upload, so no partial
// object is left in the bucket.
func (s *S3) Store(ctx context.Context, filename string, data io.Reader) error {
	//lint:ignore SA1019 stable API; replacement is pre-1.0. See note above.
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(filename),
		Body:   data,
	})
	return s.describeError("upload of "+filename, err)
}

func (s *S3) List(ctx context.Context) ([]Object, error) {
	var objects []Object
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, s.describeError("list", err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, Object{
				Name:    aws.ToString(obj.Key),
				Size:    aws.ToInt64(obj.Size),
				ModTime: aws.ToTime(obj.LastModified),
			})
		}
	}
	return objects, nil
}

func (s *S3) Delete(ctx context.Context, filename string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(filename),
	})
	var notFound *types.NoSuchKey
	if errors.As(err, &notFound) {
		return nil
	}
	return s.describeError("delete of "+filename, err)
}

// interface guard
var _ Provider = (*S3)(nil)
