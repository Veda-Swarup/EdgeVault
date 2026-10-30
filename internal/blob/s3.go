package blob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Client wraps the AWS S3 SDK and implements the Blob interface.
// When Endpoint is set, the client points at MinIO instead of AWS S3.
type S3Client struct {
	client *s3.Client
}

// S3Config holds the parameters needed to construct an S3Client.
type S3Config struct {
	Region         string
	Endpoint       string // empty → use real AWS; non-empty → MinIO / localstack
	ForcePathStyle bool
	// Static credentials for MinIO; in production use IRSA (no keys needed).
	AccessKeyID     string
	SecretAccessKey string
}

// NewS3Client creates an S3Client.
func NewS3Client(ctx context.Context, cfg S3Config) (*S3Client, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}

	// If static credentials are provided (MinIO in dev), use them.
	// In production IRSA provides credentials via the EKS service account.
	if cfg.AccessKeyID != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("blob: load aws config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		// MinIO requires path-style addressing (http://host/bucket/key).
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = cfg.ForcePathStyle
		})
	}

	return &S3Client{client: s3.NewFromConfig(awsCfg, clientOpts...)}, nil
}

// Put uploads data to S3.  size must match the actual content length so S3
// can validate the transfer without buffering everything in memory.
func (c *S3Client) Put(ctx context.Context, bucket, key string, data io.Reader, size int64) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          data,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return fmt.Errorf("blob: put %s/%s: %w", bucket, key, err)
	}
	return nil
}

func (c *S3Client) PutProtected(ctx context.Context, bucket, key string, data io.Reader, size int64, retainUntil time.Time) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: data, ContentLength: aws.Int64(size), ObjectLockMode: s3types.ObjectLockModeGovernance, ObjectLockRetainUntilDate: &retainUntil})
	if err != nil {
		return fmt.Errorf("blob: protected put %s/%s: %w", bucket, key, err)
	}
	return nil
}

// Get downloads an object.  The caller must call Close() on the returned reader.
func (c *S3Client) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("blob: get %s/%s: %w", bucket, key, err)
	}
	return out.Body, nil
}

// Exists performs a cheap HeadObject to check if a key is present.
func (c *S3Client) Exists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// The SDK wraps 404 as a *s3.NoSuchKey or *smithy HTTPResponseError.
		// Checking the string is fragile; use a type assertion in Phase 3.
		// For Phase 1 return false on any error and let the caller re-upload.
		return false, nil
	}
	return true, nil
}

func (c *S3Client) Delete(ctx context.Context, bucket, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("blob: delete %s/%s: %w", bucket, key, err)
	}
	return nil
}

// --- In-memory fake used in unit tests ---

// MemBlob is an in-memory Blob implementation for tests.
// It is not thread-safe; tests that need concurrency should add a mutex.
type MemBlob struct {
	mu          sync.RWMutex
	objects     map[string][]byte // key = bucket+"/"+key
	lockedUntil map[string]time.Time
}

func NewMemBlob() *MemBlob {
	return &MemBlob{objects: make(map[string][]byte), lockedUntil: make(map[string]time.Time)}
}

func (m *MemBlob) PutProtected(ctx context.Context, bucket, key string, data io.Reader, size int64, retainUntil time.Time) error {
	if err := m.Put(ctx, bucket, key, data, size); err != nil {
		return err
	}
	m.mu.Lock()
	m.lockedUntil[bucket+"/"+key] = retainUntil
	m.mu.Unlock()
	return nil
}

func (m *MemBlob) LockedObjects() int { m.mu.RLock(); defer m.mu.RUnlock(); return len(m.lockedUntil) }

func (m *MemBlob) Put(_ context.Context, bucket, key string, data io.Reader, _ int64) error {
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.objects[bucket+"/"+key] = b
	m.mu.Unlock()
	return nil
}

func (m *MemBlob) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	m.mu.RLock()
	b, ok := m.objects[bucket+"/"+key]
	b = append([]byte(nil), b...)
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("blob: not found: %s/%s", bucket, key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *MemBlob) Exists(_ context.Context, bucket, key string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.objects[bucket+"/"+key]
	return ok, nil
}

func (m *MemBlob) Delete(_ context.Context, bucket, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	objectKey := bucket + "/" + key
	if until, ok := m.lockedUntil[objectKey]; ok && time.Now().Before(until) {
		return fmt.Errorf("blob: object is retained until %s", until)
	}
	delete(m.objects, objectKey)
	delete(m.lockedUntil, objectKey)
	return nil
}

// Len returns the number of stored objects (useful in tests).
func (m *MemBlob) Len() int { m.mu.RLock(); defer m.mu.RUnlock(); return len(m.objects) }
