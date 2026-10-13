package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	azblobblob "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	// Register blob URL schemes.
	_ "gocloud.dev/blob/azureblob"
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/gcsblob"
	_ "gocloud.dev/blob/s3blob"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
)

var supportedURLSchemes = []string{"s3", "gs", "azblob", "file"}

// BlobStore implements Store on a gocloud.dev blob.Bucket (S3, GCS, Azure Blob,
// local file). Credentials come from each provider's default chain.
//
// Conditional writes use the provider's native preconditions (S3 If-Match, GCS
// generation match, Azure If-Match), so they hold across replicas. fileblob has
// none, so file:// emulates them under a mutex, which only covers this process.
type BlobStore struct {
	bucket *blob.Bucket
	local  bool       // file:// — emulate conditional writes
	mu     sync.Mutex // guards read-compare-write when local
}

// OpenBlobStore opens the bucket at rawURL, e.g. s3://bucket?region=us-east-1.
func OpenBlobStore(ctx context.Context, rawURL string) (*BlobStore, error) {
	u, err := normalizeBlobURL(rawURL)
	if err != nil {
		return nil, err
	}
	bucket, err := blob.OpenBucket(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("snapshot: blob: open bucket: %w", err)
	}
	return &BlobStore{bucket: bucket, local: strings.HasPrefix(u, "file:")}, nil
}

// ValidateURL checks that rawURL parses and has a supported scheme.
func ValidateURL(rawURL string) error {
	_, err := parseBlobURL(rawURL)
	return err
}

func parseBlobURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("snapshot: blob: invalid URL: %w", err)
	}
	if !isSupportedURLScheme(u.Scheme) {
		return nil, fmt.Errorf("snapshot: blob: unsupported URL scheme %q: must be one of %s",
			u.Scheme, strings.Join(supportedURLSchemes, ", "))
	}
	return u, nil
}

// normalizeBlobURL ensures the prefix ends in "/" and applies file:// defaults:
// create the dir (0700), rename temp files in place, skip metadata sidecars.
func normalizeBlobURL(rawURL string) (string, error) {
	u, err := parseBlobURL(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if p := q.Get("prefix"); p != "" && !strings.HasSuffix(p, "/") {
		q.Set("prefix", p+"/")
	}
	if u.Scheme == "file" {
		setDefault(q, "create_dir", "true")
		setDefault(q, "dir_file_mode", strconv.Itoa(0o700))
		setDefault(q, "no_tmp_dir", "true")
		setDefault(q, "metadata", "skip")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func setDefault(q url.Values, key, value string) {
	if !q.Has(key) {
		q.Set(key, value)
	}
}

// RedactURL masks any userinfo password in rawURL for logging.
func RedactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "[invalid]"
	}
	return u.Redacted()
}

func isSupportedURLScheme(scheme string) bool {
	return slices.Contains(supportedURLSchemes, scheme)
}

func objectKey(keyID string) string {
	return keyID + ".json"
}

// Put writes the snapshot body for the given key ID unconditionally.
func (s *BlobStore) Put(ctx context.Context, keyID string, body []byte) error {
	if !sdkkey.IsValidID(keyID) {
		return fmt.Errorf("snapshot: blob: invalid key ID %q", keyID)
	}
	if err := s.bucket.WriteAll(ctx, objectKey(keyID), body, writerOptions()); err != nil {
		return fmt.Errorf("snapshot: blob: put %s: %w", keyID, err)
	}
	return nil
}

// PutIfMatch implements Store.
func (s *BlobStore) PutIfMatch(ctx context.Context, keyID string, body []byte, version string) error {
	if !sdkkey.IsValidID(keyID) {
		return fmt.Errorf("snapshot: blob: invalid key ID %q", keyID)
	}
	key := objectKey(keyID)
	opts := writerOptions()
	switch {
	case version == "":
		opts.IfNotExist = true
	case s.local:
		s.mu.Lock()
		defer s.mu.Unlock()
		attrs, err := s.bucket.Attributes(ctx, key)
		if err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			return fmt.Errorf("snapshot: blob: put %s: %w", keyID, err)
		}
		if err != nil || fileVersion(attrs.ModTime.UnixNano(), attrs.Size) != version {
			return fmt.Errorf("snapshot: blob: put %s: %w", keyID, ErrPreconditionFailed)
		}
	default:
		opts.BeforeWrite = ifMatch(version)
	}
	if err := s.bucket.WriteAll(ctx, key, body, opts); err != nil {
		if gcerrors.Code(err) == gcerrors.FailedPrecondition {
			return fmt.Errorf("snapshot: blob: put %s: %w", keyID, ErrPreconditionFailed)
		}
		return fmt.Errorf("snapshot: blob: put %s: %w", keyID, err)
	}
	return nil
}

func writerOptions() *blob.WriterOptions {
	return &blob.WriterOptions{ContentType: "application/json"}
}

// ifMatch sets the provider's write precondition to the given version.
func ifMatch(version string) func(func(any) bool) error {
	return func(as func(any) bool) error {
		var s3in *transfermanager.UploadObjectInput
		if as(&s3in) {
			s3in.IfMatch = aws.String(version)
			return nil
		}
		var gcsObj **storage.ObjectHandle
		if as(&gcsObj) {
			gen, err := strconv.ParseInt(version, 10, 64)
			if err != nil {
				return fmt.Errorf("snapshot: blob: invalid GCS generation %q: %w", version, err)
			}
			*gcsObj = (*gcsObj).If(storage.Conditions{GenerationMatch: gen})
			return nil
		}
		var azOpts *azblob.UploadStreamOptions
		if as(&azOpts) {
			etag := azcore.ETag(version)
			azOpts.AccessConditions = &azblob.AccessConditions{
				ModifiedAccessConditions: &azblobblob.ModifiedAccessConditions{IfMatch: &etag},
			}
			return nil
		}
		return errors.New("snapshot: blob: conditional writes not supported by this driver")
	}
}

// Get retrieves the snapshot body for the given key ID.
func (s *BlobStore) Get(ctx context.Context, keyID string) ([]byte, error) {
	body, _, err := s.GetVersioned(ctx, keyID)
	return body, err
}

// GetVersioned implements Store. The version is read from the same response
// as the body, so it always identifies the returned revision.
func (s *BlobStore) GetVersioned(ctx context.Context, keyID string) ([]byte, string, error) {
	if !sdkkey.IsValidID(keyID) {
		return nil, "", fmt.Errorf("snapshot: blob: invalid key ID %q", keyID)
	}
	if s.local {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	r, err := s.bucket.NewReader(ctx, objectKey(keyID), nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, "", fmt.Errorf("snapshot: blob: get %s: %w", keyID, ErrNotFound)
		}
		return nil, "", fmt.Errorf("snapshot: blob: get %s: %w", keyID, err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, "", fmt.Errorf("snapshot: blob: read %s: %w", keyID, err)
	}
	return body, readerVersion(r), nil
}

func readerVersion(r *blob.Reader) string {
	var s3out s3.GetObjectOutput
	if r.As(&s3out) {
		return aws.ToString(s3out.ETag)
	}
	var gcsr *storage.Reader
	if r.As(&gcsr) {
		return strconv.FormatInt(gcsr.Attrs.Generation, 10)
	}
	var az azblobblob.DownloadStreamResponse
	if r.As(&az) && az.ETag != nil {
		return string(*az.ETag)
	}
	return fileVersion(r.ModTime().UnixNano(), r.Size())
}

func fileVersion(modTimeNanos, size int64) string {
	return strconv.FormatInt(modTimeNanos, 16) + "-" + strconv.FormatInt(size, 16)
}

// List returns the key IDs of all snapshots directly under the bucket or prefix.
func (s *BlobStore) List(ctx context.Context) ([]string, error) {
	var keys []string
	iter := s.bucket.List(&blob.ListOptions{Delimiter: "/"})
	for {
		obj, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("snapshot: blob: list: %w", err)
		}
		if obj.IsDir {
			continue
		}
		if key := extractPublicID(obj.Key); key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// extractPublicID returns the key ID for "<keyID>.json", or "" otherwise.
func extractPublicID(objectKey string) string {
	name, ok := strings.CutSuffix(objectKey, ".json")
	if !ok || strings.ContainsRune(name, '/') || !sdkkey.IsValidID(name) {
		return ""
	}
	return name
}

// Delete removes the snapshot object for the given key ID.
// Returns nil when the object does not exist (idempotent).
func (s *BlobStore) Delete(ctx context.Context, keyID string) error {
	if !sdkkey.IsValidID(keyID) {
		return fmt.Errorf("snapshot: blob: invalid key ID %q", keyID)
	}
	err := s.bucket.Delete(ctx, objectKey(keyID))
	if err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		return fmt.Errorf("snapshot: blob: delete %s: %w", keyID, err)
	}
	return nil
}

// Close closes the bucket.
func (s *BlobStore) Close() error {
	return s.bucket.Close()
}

// ensure BlobStore implements Store at compile time.
var _ Store = (*BlobStore)(nil)
