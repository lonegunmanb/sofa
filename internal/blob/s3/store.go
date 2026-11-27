package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/lonegunmanb/sofa/internal/blob"
)

const (
	defaultRegion        = "us-east-1"
	defaultPartSizeBytes = int64(8 * 1024 * 1024)
	maxDeleteBatchSize   = 1000
	metadataSHA256Key    = "sofa-sha256"
)

// Config configures an S3 / MinIO / OSS compatible blob.Store.
type Config struct {
	Bucket          string
	Region          string // Defaults to "us-east-1"
	Endpoint        string // Optional custom endpoint for MinIO / S3-compatible servers
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	UsePathStyle    bool
	KeyPrefix       string // Optional bucket root prefix, e.g. "sofa"
	PartSizeBytes   int64  // Defaults to 8 MiB
}

type s3API interface {
	transfermanager.S3APIClient
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// Store implements blob.Store backed by an S3-compatible object store.
type Store struct {
	bucket        string
	keyPrefix     string
	partSizeBytes int64
	client        s3API
	uploader      *transfermanager.Client
}

// NewStore creates a streaming S3-compatible blob.Store from cfg.
func NewStore(ctx context.Context, cfg Config) (*Store, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	normCfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if (normCfg.AccessKeyID == "") != (normCfg.SecretAccessKey == "") {
		return nil, fmt.Errorf("s3 config: AccessKeyID and SecretAccessKey must both be set when using static credentials")
	}

	loadCtx := ctx
	if loadCtx == nil {
		loadCtx = context.Background()
	}
	loadOpts := []func(*config.LoadOptions) error{
		config.WithRegion(normCfg.Region),
	}
	if normCfg.AccessKeyID != "" && normCfg.SecretAccessKey != "" {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(normCfg.AccessKeyID, normCfg.SecretAccessKey, normCfg.SessionToken),
		))
	}

	awsCfg, err := config.LoadDefaultConfig(loadCtx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = normCfg.UsePathStyle
		if normCfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(normCfg.Endpoint)
		}
	})

	return newStoreWithClient(normCfg, client)
}

func newStoreWithClient(cfg Config, client s3API) (*Store, error) {
	if client == nil {
		return nil, fmt.Errorf("s3 client is nil")
	}
	normCfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}

	uploader := transfermanager.New(client, func(o *transfermanager.Options) {
		o.PartSizeBytes = normCfg.PartSizeBytes
		o.MultipartUploadThreshold = normCfg.PartSizeBytes
	})

	return &Store{
		bucket:        normCfg.Bucket,
		keyPrefix:     normCfg.KeyPrefix,
		partSizeBytes: normCfg.PartSizeBytes,
		client:        client,
		uploader:      uploader,
	}, nil
}

func normalizeConfig(cfg Config) (Config, error) {
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	if cfg.Bucket == "" {
		return Config{}, fmt.Errorf("s3 bucket is required")
	}
	cfg.Region = strings.TrimSpace(cfg.Region)
	if cfg.Region == "" {
		cfg.Region = defaultRegion
	}
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	if cfg.PartSizeBytes < 0 {
		return Config{}, fmt.Errorf("s3 part size must be non-negative")
	}
	if cfg.PartSizeBytes == 0 {
		cfg.PartSizeBytes = defaultPartSizeBytes
	}
	if cfg.KeyPrefix != "" {
		trimmed := strings.Trim(cfg.KeyPrefix, "/")
		if trimmed == "" {
			return Config{}, fmt.Errorf("%w: key prefix %q must not be '/'", blob.ErrInvalidKey, cfg.KeyPrefix)
		}
		if err := blob.ValidateKey(trimmed); err != nil {
			return Config{}, err
		}
		cfg.KeyPrefix = trimmed
	}
	return cfg, nil
}

// Put streams r into key, verifying or recording SHA-256 metadata in x-amz-meta-sofa-sha256.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, expectedSHA256 string) (blob.ObjectMeta, error) {
	if err := checkContext(ctx); err != nil {
		return blob.ObjectMeta{}, err
	}
	if err := blob.ValidateKey(key); err != nil {
		return blob.ObjectMeta{}, err
	}
	if r == nil {
		return blob.ObjectMeta{}, fmt.Errorf("blob reader is nil")
	}
	if expectedSHA256 != "" && !blob.IsValidSHA256Hex(expectedSHA256) {
		return blob.ObjectMeta{}, fmt.Errorf("%w: invalid expected sha256 %q", blob.ErrDigestMismatch, expectedSHA256)
	}

	if expectedSHA256 != "" {
		return s.putWithExpectedDigest(ctx, key, r, expectedSHA256)
	}
	return s.putWithTempDigest(ctx, key, r)
}

func (s *Store) putWithExpectedDigest(ctx context.Context, key string, r io.Reader, expectedSHA256 string) (blob.ObjectMeta, error) {
	hr := &hashingReader{
		ctx:    ctx,
		r:      r,
		hasher: sha256.New(),
	}
	fullKey := s.s3Key(key)

	_, err := s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(fullKey),
		Body:   hr,
		Metadata: map[string]string{
			metadataSHA256Key: expectedSHA256,
		},
	})
	if err != nil {
		return blob.ObjectMeta{}, fmt.Errorf("upload s3 object %q: %w", key, err)
	}

	actualSHA256, written := hr.digestAndSize()
	if actualSHA256 != expectedSHA256 {
		rollbackCtx := context.Background()
		if ctx != nil {
			rollbackCtx = context.WithoutCancel(ctx)
		}
		_, delErr := s.client.DeleteObject(rollbackCtx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(fullKey),
		})
		mismatchErr := fmt.Errorf("%w: expected %s, got %s", blob.ErrDigestMismatch, expectedSHA256, actualSHA256)
		if delErr != nil && !isNotFoundError(delErr) {
			return blob.ObjectMeta{}, errors.Join(mismatchErr, fmt.Errorf("rollback s3 object %q: %w", key, delErr))
		}
		return blob.ObjectMeta{}, mismatchErr
	}

	return blob.ObjectMeta{
		Key:       key,
		SHA256:    actualSHA256,
		SizeBytes: written,
	}, nil
}

func (s *Store) putWithTempDigest(ctx context.Context, key string, r io.Reader) (blob.ObjectMeta, error) {
	tmpFile, err := os.CreateTemp("", "sofa-s3-put-*")
	if err != nil {
		return blob.ObjectMeta{}, fmt.Errorf("create staging temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	if err := restrictTempFile(tmpPath, tmpFile); err != nil {
		return blob.ObjectMeta{}, fmt.Errorf("restrict staging temp file permissions: %w", err)
	}

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmpFile, hasher), &contextReader{ctx: ctx, r: r})
	if err != nil {
		return blob.ObjectMeta{}, fmt.Errorf("buffer stream for %q: %w", key, err)
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return blob.ObjectMeta{}, fmt.Errorf("rewind staging temp file for %q: %w", key, err)
	}

	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	fullKey := s.s3Key(key)

	_, err = s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(fullKey),
		Body:          tmpFile,
		ContentLength: aws.Int64(written),
		Metadata: map[string]string{
			metadataSHA256Key: actualSHA256,
		},
	})
	if err != nil {
		return blob.ObjectMeta{}, fmt.Errorf("upload s3 object %q: %w", key, err)
	}

	return blob.ObjectMeta{
		Key:       key,
		SHA256:    actualSHA256,
		SizeBytes: written,
	}, nil
}

// Get opens a streaming reader for key that verifies SHA-256 at io.EOF against stored metadata and expectedSHA256.
func (s *Store) Get(ctx context.Context, key string, expectedSHA256 string) (io.ReadCloser, blob.ObjectMeta, error) {
	if err := checkContext(ctx); err != nil {
		return nil, blob.ObjectMeta{}, err
	}
	if err := blob.ValidateKey(key); err != nil {
		return nil, blob.ObjectMeta{}, err
	}
	if expectedSHA256 != "" && !blob.IsValidSHA256Hex(expectedSHA256) {
		return nil, blob.ObjectMeta{}, fmt.Errorf("%w: invalid expected sha256 %q", blob.ErrDigestMismatch, expectedSHA256)
	}

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.s3Key(key)),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, blob.ObjectMeta{}, fmt.Errorf("%w: %s", blob.ErrNotFound, key)
		}
		return nil, blob.ObjectMeta{}, fmt.Errorf("get s3 object %q: %w", key, err)
	}

	storedSHA := extractStoredSHA256(out.Metadata)
	if !blob.IsValidSHA256Hex(storedSHA) {
		_ = out.Body.Close()
		return nil, blob.ObjectMeta{}, fmt.Errorf("%w: missing or invalid stored metadata sha256 %q", blob.ErrDigestMismatch, storedSHA)
	}
	if expectedSHA256 != "" && storedSHA != expectedSHA256 {
		_ = out.Body.Close()
		return nil, blob.ObjectMeta{}, fmt.Errorf("%w: metadata digest %s does not match expected %s", blob.ErrDigestMismatch, storedSHA, expectedSHA256)
	}

	var expectedSize int64
	var hasExpectedSize bool
	if out.ContentLength != nil && *out.ContentLength >= 0 {
		expectedSize = *out.ContentLength
		hasExpectedSize = true
	}

	meta := blob.ObjectMeta{
		Key:       key,
		SHA256:    storedSHA,
		SizeBytes: expectedSize,
	}

	return &verifyingReadCloser{
		body:            out.Body,
		hasher:          sha256.New(),
		storedSHA256:    storedSHA,
		expectedSHA256:  expectedSHA256,
		expectedSize:    expectedSize,
		hasExpectedSize: hasExpectedSize,
	}, meta, nil
}

// Stat retrieves object metadata without reading object bytes.
func (s *Store) Stat(ctx context.Context, key string) (blob.ObjectMeta, error) {
	if err := checkContext(ctx); err != nil {
		return blob.ObjectMeta{}, err
	}
	if err := blob.ValidateKey(key); err != nil {
		return blob.ObjectMeta{}, err
	}

	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.s3Key(key)),
	})
	if err != nil {
		if isNotFoundError(err) {
			return blob.ObjectMeta{}, fmt.Errorf("%w: %s", blob.ErrNotFound, key)
		}
		return blob.ObjectMeta{}, fmt.Errorf("stat s3 object %q: %w", key, err)
	}

	storedSHA := extractStoredSHA256(out.Metadata)
	if !blob.IsValidSHA256Hex(storedSHA) {
		return blob.ObjectMeta{}, fmt.Errorf("%w: missing or invalid stored metadata sha256 %q", blob.ErrDigestMismatch, storedSHA)
	}

	return blob.ObjectMeta{
		Key:       key,
		SHA256:    storedSHA,
		SizeBytes: aws.ToInt64(out.ContentLength),
	}, nil
}

// Delete removes key idempotently.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := blob.ValidateKey(key); err != nil {
		return err
	}

	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.s3Key(key)),
	})
	if err != nil && !isNotFoundError(err) {
		return fmt.Errorf("delete s3 object %q: %w", key, err)
	}
	return nil
}

// DeletePrefix deletes all objects matching prefix on a strict '/' boundary within KeyPrefix.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := blob.ValidatePrefix(prefix); err != nil {
		return err
	}

	listPrefix := s.s3Prefix(prefix)
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(listPrefix),
		MaxKeys: aws.Int32(maxDeleteBatchSize),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list s3 prefix %q: %w", prefix, err)
		}
		if len(page.Contents) == 0 {
			continue
		}

		objs := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, item := range page.Contents {
			if item.Key == nil || *item.Key == "" {
				continue
			}
			objs = append(objs, types.ObjectIdentifier{Key: item.Key})
		}

		for start := 0; start < len(objs); start += maxDeleteBatchSize {
			end := start + maxDeleteBatchSize
			if end > len(objs) {
				end = len(objs)
			}
			delOut, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(s.bucket),
				Delete: &types.Delete{
					Objects: objs[start:end],
					Quiet:   aws.Bool(true),
				},
			})
			if err != nil {
				return fmt.Errorf("delete s3 prefix %q: %w", prefix, err)
			}
			if delOut != nil && len(delOut.Errors) > 0 {
				var msgs []string
				for _, e := range delOut.Errors {
					msgs = append(msgs, fmt.Sprintf("%s (%s: %s)", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message)))
				}
				return fmt.Errorf("delete s3 prefix %q partial failures: %s", prefix, strings.Join(msgs, "; "))
			}
		}
	}

	return nil
}

func (s *Store) s3Key(key string) string {
	if s.keyPrefix == "" {
		return key
	}
	return s.keyPrefix + "/" + key
}

func (s *Store) s3Prefix(prefix string) string {
	if prefix == "" {
		if s.keyPrefix == "" {
			return ""
		}
		return s.keyPrefix + "/"
	}
	norm := strings.TrimSuffix(prefix, "/") + "/"
	if s.keyPrefix == "" {
		return norm
	}
	return s.keyPrefix + "/" + norm
}

func extractStoredSHA256(metadata map[string]string) string {
	for k, v := range metadata {
		if strings.EqualFold(k, metadataSHA256Key) {
			return v
		}
	}
	return ""
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := checkContext(c.ctx); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type hashingReader struct {
	mu     sync.Mutex
	ctx    context.Context
	r      io.Reader
	hasher hash.Hash
	n      int64
}

func (h *hashingReader) Read(p []byte) (int, error) {
	if err := checkContext(h.ctx); err != nil {
		return 0, err
	}
	n, err := h.r.Read(p)
	if n > 0 {
		h.mu.Lock()
		h.hasher.Write(p[:n])
		h.n += int64(n)
		h.mu.Unlock()
	}
	return n, err
}

func (h *hashingReader) digestAndSize() (string, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return hex.EncodeToString(h.hasher.Sum(nil)), h.n
}

type verifyingReadCloser struct {
	mu              sync.Mutex
	body            io.ReadCloser
	hasher          hash.Hash
	bytesRead       int64
	storedSHA256    string
	expectedSHA256  string
	expectedSize    int64
	hasExpectedSize bool
	verified        bool
	verifyErr       error
	closed          bool
}

func (v *verifyingReadCloser) Read(p []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.closed {
		return 0, os.ErrClosed
	}
	if v.verified {
		if v.verifyErr != nil {
			return 0, v.verifyErr
		}
		return 0, io.EOF
	}

	n, err := v.body.Read(p)
	if n > 0 {
		v.hasher.Write(p[:n])
		v.bytesRead += int64(n)
	}
	if errors.Is(err, io.EOF) {
		v.verified = true
		actual := hex.EncodeToString(v.hasher.Sum(nil))
		if v.hasExpectedSize && v.bytesRead != v.expectedSize {
			v.verifyErr = fmt.Errorf("%w: size mismatch (expected %d bytes, got %d)", blob.ErrDigestMismatch, v.expectedSize, v.bytesRead)
			return 0, v.verifyErr
		}
		if v.storedSHA256 != "" && actual != v.storedSHA256 {
			v.verifyErr = fmt.Errorf("%w: stored metadata digest mismatch (expected %s, got %s)", blob.ErrDigestMismatch, v.storedSHA256, actual)
			return 0, v.verifyErr
		}
		if v.expectedSHA256 != "" && actual != v.expectedSHA256 {
			v.verifyErr = fmt.Errorf("%w: expected %s, got %s", blob.ErrDigestMismatch, v.expectedSHA256, actual)
			return 0, v.verifyErr
		}
		return n, io.EOF
	}
	return n, err
}

func (v *verifyingReadCloser) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.closed {
		return nil
	}
	v.closed = true
	return v.body.Close()
}
