package objstore

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config points at any S3-compatible endpoint. All values come from
// configuration; nothing is read from the environment or instance metadata.
type S3Config struct {
	Endpoint  string // e.g. http://sbx-s3.sbx-system:8333; empty for AWS
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool // required by most self-hosted stores
	// PresignEndpoint, if set, is used in presigned URLs instead of Endpoint
	// (for clients that reach the store under a different address).
	PresignEndpoint string
}

// S3 is a Store backed by an S3-compatible service.
type S3 struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// metaSHA256 is the user-metadata key holding a writer-supplied checksum.
const metaSHA256 = "sha256"

// NewS3 builds a client from explicit configuration only.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("objstore: bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := func(endpoint string) s3.Options {
		o := s3.Options{
			Region:       region,
			Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
			UsePathStyle: cfg.PathStyle,
			// Only send/verify checksums we ask for; many S3-compatible stores
			// reject the SDK's default CRC32 trailers.
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		}
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		return o
	}
	client := s3.New(opts(cfg.Endpoint))
	presignEndpoint := cfg.PresignEndpoint
	if presignEndpoint == "" {
		presignEndpoint = cfg.Endpoint
	}
	return &S3{
		client:  client,
		presign: s3.NewPresignClient(s3.New(opts(presignEndpoint))),
		bucket:  cfg.Bucket,
	}, nil
}

// EnsureBucket creates the bucket if it does not exist.
func (s *S3) EnsureBucket(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket}); err == nil {
		return nil
	}
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &s.bucket})
	var owned *types.BucketAlreadyOwnedByYou
	if errors.As(err, &owned) {
		return nil
	}
	return mapErr(err)
}

func (s *S3) putInput(key string, c Cond) (*s3.PutObjectInput, error) {
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: &key}
	if c.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	if c.IfMatch != "" {
		in.IfMatch = aws.String(c.IfMatch)
	}
	if c.SHA256 != "" {
		raw, err := hex.DecodeString(c.SHA256)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("objstore: invalid SHA256 %q", c.SHA256)
		}
		in.ChecksumSHA256 = aws.String(base64.StdEncoding.EncodeToString(raw))
		in.Metadata = map[string]string{metaSHA256: c.SHA256}
	}
	return in, nil
}

func (s *S3) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, c Cond) (Object, error) {
	in, err := s.putInput(key, c)
	if err != nil {
		return Object{}, err
	}
	in.Body, in.ContentLength = body, aws.Int64(size)
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return Object{}, mapErr(err)
	}
	return Object{Key: key, Size: size, ETag: aws.ToString(out.ETag), SHA256: c.SHA256}, nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, Object{}, mapErr(err)
	}
	return out.Body, Object{
		Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag),
		SHA256: out.Metadata[metaSHA256], Modified: aws.ToTime(out.LastModified),
	}, nil
}

func (s *S3) Head(ctx context.Context, key string) (Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return Object{}, mapErr(err)
	}
	return Object{
		Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag),
		SHA256: out.Metadata[metaSHA256], Modified: aws.ToTime(out.LastModified),
	}, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	if err = mapErr(err); errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, o := range page.Contents {
			out = append(out, Object{
				Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size),
				ETag: aws.ToString(o.ETag), Modified: aws.ToTime(o.LastModified),
			})
		}
	}
	return out, nil
}

func (s *S3) PresignPut(ctx context.Context, key string, ttl time.Duration, c Cond) (Request, error) {
	in, err := s.putInput(key, c)
	if err != nil {
		return Request{}, err
	}
	r, err := s.presign.PresignPutObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return Request{}, mapErr(err)
	}
	return Request{Method: r.Method, URL: r.URL, Header: r.SignedHeader, Expires: time.Now().Add(ttl)}, nil
}

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (Request, error) {
	r, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}, s3.WithPresignExpires(ttl))
	if err != nil {
		return Request{}, mapErr(err)
	}
	return Request{Method: r.Method, URL: r.URL, Header: r.SignedHeader, Expires: time.Now().Add(ttl)}, nil
}

// mapErr turns S3 error codes into ErrNotFound / ErrPrecondition.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "PreconditionFailed", "ConditionalRequestConflict":
			return fmt.Errorf("%w: %w", ErrPrecondition, err)
		case "BadDigest", "InvalidDigest", "XAmzContentSHA256Mismatch", "InvalidChecksum":
			return fmt.Errorf("%w: %w", ErrChecksum, err)
		case "NoSuchKey", "NotFound":
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		}
	}
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		switch re.HTTPStatusCode() {
		case http.StatusPreconditionFailed, http.StatusConflict:
			return fmt.Errorf("%w: %w", ErrPrecondition, err)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		}
	}
	return err
}
