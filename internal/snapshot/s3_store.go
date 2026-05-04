package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Store implements Store against any S3-compatible object storage service
// (AWS S3, MinIO, Ceph RGW, GCS S3-compat, etc.).
//
// Credentials are loaded from the AWS SDK default chain:
// environment variables, shared config, EC2/ECS/EKS instance roles (IRSA).
// These are object-storage credentials, not SDK keys.
type S3Store struct {
	client *s3.Client
	bucket string
	prefix string
}

// S3Config holds the parameters for constructing an S3Store.
type S3Config struct {
	// Endpoint is the base URL of the S3-compatible service. Leave empty to use
	// the AWS S3 regional endpoint derived from Region.
	Endpoint string
	// Bucket is required.
	Bucket string
	// Region is required for AWS S3; may be a placeholder for self-hosted stores.
	Region string
	// Prefix is an optional key prefix applied to all object paths. May be empty.
	Prefix string

	// AccessKeyID and SecretAccessKey are optional explicit credentials.
	// When empty, the AWS SDK default credential chain is used (env vars, IRSA, etc.).
	AccessKeyID     string
	SecretAccessKey string
}

// NewS3Store constructs an S3Store from the provided config.
func NewS3Store(ctx context.Context, cfg S3Config) (*S3Store, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}

	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("snapshot: s3: load AWS config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			// Use path-style addressing for self-hosted S3-compatible services.
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, clientOpts...)
	return &S3Store{
		client: client,
		bucket: cfg.Bucket,
		prefix: cfg.Prefix,
	}, nil
}

func (s *S3Store) objectKey(keyHash string) string {
	if s.prefix != "" {
		return s.prefix + "/" + keyHash + ".json"
	}
	return keyHash + ".json"
}

// Put writes the snapshot body to the bucket under the key hash.
func (s *S3Store) Put(ctx context.Context, keyHash string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.objectKey(keyHash)),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("snapshot: s3: put %s: %w", keyHash, err)
	}
	return nil
}

// Get retrieves the snapshot body for the given key hash.
func (s *S3Store) Get(ctx context.Context, keyHash string) ([]byte, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(keyHash)),
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot: s3: get %s: %w", keyHash, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("snapshot: s3: read %s: %w", keyHash, err)
	}
	return data, nil
}

// List returns the key hashes of all snapshot objects in the bucket/prefix.
func (s *S3Store) List(ctx context.Context) ([]string, error) {
	prefix := s.prefix
	if prefix != "" {
		prefix += "/"
	}

	var keys []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot: s3: list: %w", err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			key := extractKeyHash(*obj.Key, prefix)
			if key != "" {
				keys = append(keys, key)
			}
		}
	}
	return keys, nil
}

// extractKeyHash derives the key hash from an object key by stripping the
// prefix and the ".json" suffix.
func extractKeyHash(objectKey, prefix string) string {
	name := strings.TrimPrefix(objectKey, prefix)
	name = strings.TrimSuffix(name, ".json")
	// Guard against nested slashes or non-leaf objects.
	if strings.ContainsRune(name, '/') || name == "" {
		return ""
	}
	return name
}

// Delete removes the snapshot object for the given key hash.
// Returns nil when the object does not exist (idempotent).
func (s *S3Store) Delete(ctx context.Context, keyHash string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.objectKey(keyHash)),
	})
	if err != nil {
		// S3 DeleteObject returns NoSuchKey only in certain configurations; treat it as success.
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil
		}
		return fmt.Errorf("snapshot: s3: delete %s: %w", keyHash, err)
	}
	return nil
}

// ensure S3Store implements Store at compile time.
var _ Store = (*S3Store)(nil)

// ensure ListObjectsV2Output.Contents is typed as []types.Object.
var _ = types.Object{}
