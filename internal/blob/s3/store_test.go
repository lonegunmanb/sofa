package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ blob.Store = (*Store)(nil)

func sha256Of(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type storedObject struct {
	data     []byte
	metadata map[string]string
}

type trackedReadCloser struct {
	r      io.Reader
	closed bool
	mu     sync.Mutex
}

func (t *trackedReadCloser) Read(p []byte) (int, error) {
	return t.r.Read(p)
}

func (t *trackedReadCloser) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

func (t *trackedReadCloser) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

type fakeS3Client struct {
	mu                 sync.Mutex
	objects            map[string]*storedObject
	uploads            map[string]map[int32][]byte
	uploadMeta         map[string]map[string]string
	nextUploadID       int
	calls              []string
	deleteCalls        []string
	deleteCtxErrOnCall error
	deleteBatchSizes   []int
	deleteQuietFlags   []bool
	listPageLimit      int
	getErr             error
	headErr            error
	deleteErr          error
	onPutObject        func()
	deleteObjectsErrs  []types.Error
	corruptNextGetBody func(data []byte) []byte
	lastGetBody        *trackedReadCloser
	lastGetCtxErr      error
	singleEOFChunkRead bool
}

func newFakeS3Client() *fakeS3Client {
	return &fakeS3Client{
		objects:       make(map[string]*storedObject),
		uploads:       make(map[string]map[int32][]byte),
		uploadMeta:    make(map[string]map[string]string),
		listPageLimit: 1000,
	}
}

func (f *fakeS3Client) recordCall(op string) {
	f.calls = append(f.calls, op)
}

func cloneMeta(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}

func (f *fakeS3Client) PutObject(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	data, err := io.ReadAll(params.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.recordCall("PutObject")
	f.objects[aws.ToString(params.Key)] = &storedObject{
		data:     data,
		metadata: cloneMeta(params.Metadata),
	}
	cb := f.onPutObject
	f.mu.Unlock()
	if cb != nil {
		cb()
	}
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3Client) CreateMultipartUpload(_ context.Context, params *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCall("CreateMultipartUpload")
	f.nextUploadID++
	id := fmt.Sprintf("mpu-%d", f.nextUploadID)
	f.uploads[id] = make(map[int32][]byte)
	f.uploadMeta[id] = cloneMeta(params.Metadata)
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String(id)}, nil
}

func (f *fakeS3Client) UploadPart(_ context.Context, params *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	data, err := io.ReadAll(params.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCall("UploadPart")
	parts, ok := f.uploads[aws.ToString(params.UploadId)]
	if !ok {
		return nil, &types.NoSuchUpload{}
	}
	partNum := aws.ToInt32(params.PartNumber)
	parts[partNum] = data
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprintf("\"etag-%d\"", partNum))}, nil
}

func (f *fakeS3Client) CompleteMultipartUpload(_ context.Context, params *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCall("CompleteMultipartUpload")
	id := aws.ToString(params.UploadId)
	parts, ok := f.uploads[id]
	if !ok {
		return nil, &types.NoSuchUpload{}
	}
	var buf bytes.Buffer
	if params.MultipartUpload != nil {
		for _, p := range params.MultipartUpload.Parts {
			buf.Write(parts[aws.ToInt32(p.PartNumber)])
		}
	}
	key := aws.ToString(params.Key)
	f.objects[key] = &storedObject{
		data:     buf.Bytes(),
		metadata: f.uploadMeta[id],
	}
	delete(f.uploads, id)
	delete(f.uploadMeta, id)
	return &s3.CompleteMultipartUploadOutput{Key: aws.String(key)}, nil
}

func (f *fakeS3Client) AbortMultipartUpload(_ context.Context, params *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCall("AbortMultipartUpload")
	id := aws.ToString(params.UploadId)
	delete(f.uploads, id)
	delete(f.uploadMeta, id)
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (f *fakeS3Client) GetObject(_ context.Context, params *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCall("GetObject")
	if f.getErr != nil {
		return nil, f.getErr
	}
	obj, ok := f.objects[aws.ToString(params.Key)]
	if !ok {
		return nil, &types.NoSuchKey{Message: aws.String("not found")}
	}
	payload := append([]byte(nil), obj.data...)
	origLen := int64(len(payload))
	if f.corruptNextGetBody != nil {
		payload = f.corruptNextGetBody(payload)
		f.corruptNextGetBody = nil
	}
	var reader io.Reader = bytes.NewReader(payload)
	if f.singleEOFChunkRead {
		reader = &eofWithDataReader{data: payload}
	}
	rc := &trackedReadCloser{r: reader}
	f.lastGetBody = rc
	metaCopy := make(map[string]string, len(obj.metadata))
	for k, v := range obj.metadata {
		metaCopy[k] = v
	}
	return &s3.GetObjectOutput{
		Body:          rc,
		ContentLength: aws.Int64(origLen),
		Metadata:      metaCopy,
	}, nil
}

func (f *fakeS3Client) HeadObject(_ context.Context, params *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCall("HeadObject")
	if f.headErr != nil {
		return nil, f.headErr
	}
	obj, ok := f.objects[aws.ToString(params.Key)]
	if !ok {
		return nil, &types.NotFound{Message: aws.String("not found")}
	}
	metaCopy := make(map[string]string, len(obj.metadata))
	for k, v := range obj.metadata {
		metaCopy[k] = v
	}
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(obj.data))),
		Metadata:      metaCopy,
	}, nil
}

func (f *fakeS3Client) DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCall("DeleteObject")
	f.lastGetCtxErr = ctx.Err()
	if ctx.Err() != nil {
		f.deleteCtxErrOnCall = ctx.Err()
		return nil, ctx.Err()
	}
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	key := aws.ToString(params.Key)
	f.deleteCalls = append(f.deleteCalls, key)
	delete(f.objects, key)
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3Client) ListObjectsV2(_ context.Context, params *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCall("ListObjectsV2")
	prefix := aws.ToString(params.Prefix)
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	start := 0
	if params.ContinuationToken != nil {
		for idx, k := range keys {
			if k > *params.ContinuationToken {
				start = idx
				break
			}
			start = len(keys)
		}
	}
	limit := f.listPageLimit
	if params.MaxKeys != nil && int(*params.MaxKeys) < limit && *params.MaxKeys > 0 {
		limit = int(*params.MaxKeys)
	}
	if limit <= 0 {
		limit = 1000
	}
	end := start + limit
	isTruncated := false
	var nextToken *string
	if end < len(keys) {
		isTruncated = true
		tok := keys[end-1]
		nextToken = &tok
	} else {
		end = len(keys)
	}
	contents := make([]types.Object, 0, end-start)
	for _, k := range keys[start:end] {
		contents = append(contents, types.Object{
			Key:  aws.String(k),
			Size: aws.Int64(int64(len(f.objects[k].data))),
		})
	}
	return &s3.ListObjectsV2Output{
		Contents:              contents,
		IsTruncated:           aws.Bool(isTruncated),
		NextContinuationToken: nextToken,
	}, nil
}

func (f *fakeS3Client) DeleteObjects(_ context.Context, params *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCall("DeleteObjects")
	if params.Delete != nil {
		f.deleteBatchSizes = append(f.deleteBatchSizes, len(params.Delete.Objects))
		f.deleteQuietFlags = append(f.deleteQuietFlags, aws.ToBool(params.Delete.Quiet))
		for _, obj := range params.Delete.Objects {
			delete(f.objects, aws.ToString(obj.Key))
		}
	}
	if len(f.deleteObjectsErrs) > 0 {
		errs := append([]types.Error(nil), f.deleteObjectsErrs...)
		f.deleteObjectsErrs = nil
		return &s3.DeleteObjectsOutput{Errors: errs}, nil
	}
	return &s3.DeleteObjectsOutput{}, nil
}

// eofWithDataReader returns all bytes together with io.EOF on the first Read call.
type eofWithDataReader struct {
	data []byte
	done bool
}

func (e *eofWithDataReader) Read(p []byte) (int, error) {
	if e.done {
		return 0, io.EOF
	}
	n := copy(p, e.data)
	e.done = true
	return n, io.EOF
}

func TestNewStoreValidationAndDefaults(t *testing.T) {
	ctx := context.Background()

	_, err := NewStore(ctx, Config{})
	require.Error(t, err)

	_, err = NewStore(ctx, Config{Bucket: "sofa", PartSizeBytes: -1})
	require.Error(t, err)

	_, err = NewStore(ctx, Config{Bucket: "sofa", KeyPrefix: "../evil"})
	require.ErrorIs(t, err, blob.ErrInvalidKey)

	_, err = NewStore(ctx, Config{Bucket: "sofa", KeyPrefix: "bad\\prefix"})
	require.ErrorIs(t, err, blob.ErrInvalidKey)

	store, err := NewStore(ctx, Config{
		Bucket:          "sofa-bucket",
		Endpoint:        "http://127.0.0.1:4566",
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		UsePathStyle:    true,
		KeyPrefix:       "/tenant-a/",
	})
	require.NoError(t, err)
	assert.Equal(t, "sofa-bucket", store.bucket)
	assert.Equal(t, "tenant-a", store.keyPrefix)
	assert.Equal(t, int64(defaultPartSizeBytes), store.partSizeBytes)
}

func TestStoreKeyPrefixAndDigestValidationBeforeS3Calls(t *testing.T) {
	ctx := context.Background()
	fake := newFakeS3Client()
	store, err := newStoreWithClient(Config{Bucket: "bkt"}, fake)
	require.NoError(t, err)

	invalidKeys := []string{"", "/", "/runs/a", "runs/a/", "runs//a", `runs\a`, ".", "..", "runs/../a", "runs/a b", "runs/a\x00b"}
	for _, k := range invalidKeys {
		_, err := store.Put(ctx, k, strings.NewReader("hi"), "")
		assert.ErrorIs(t, err, blob.ErrInvalidKey, "Put key %q", k)
		_, _, err = store.Get(ctx, k, "")
		assert.ErrorIs(t, err, blob.ErrInvalidKey, "Get key %q", k)
		_, err = store.Stat(ctx, k)
		assert.ErrorIs(t, err, blob.ErrInvalidKey, "Stat key %q", k)
		err = store.Delete(ctx, k)
		assert.ErrorIs(t, err, blob.ErrInvalidKey, "Delete key %q", k)
	}

	_, err = store.Put(ctx, "runs/a.txt", nil, "")
	require.Error(t, err)

	invalidPrefixes := []string{"/", "../evil", `a\b`, "a//b", "a b"}
	for _, p := range invalidPrefixes {
		err := store.DeletePrefix(ctx, p)
		assert.ErrorIs(t, err, blob.ErrInvalidKey, "DeletePrefix %q", p)
	}

	validPayload := []byte("hello")
	badDigests := []string{"not-hex", strings.ToUpper(sha256Of(validPayload)), strings.Repeat("a", 63)}
	for _, d := range badDigests {
		_, err := store.Put(ctx, "runs/a.txt", bytes.NewReader(validPayload), d)
		assert.ErrorIs(t, err, blob.ErrDigestMismatch, "Put digest %q", d)
		_, _, err = store.Get(ctx, "runs/a.txt", d)
		assert.ErrorIs(t, err, blob.ErrDigestMismatch, "Get digest %q", d)
	}

	assert.Empty(t, fake.calls, "validation errors must not invoke any S3 API")
}

func TestStore404AndNotFoundMapping(t *testing.T) {
	ctx := context.Background()
	fake := newFakeS3Client()
	store, err := newStoreWithClient(Config{Bucket: "bkt"}, fake)
	require.NoError(t, err)

	// Default fake returns types.NoSuchKey on Get and types.NotFound on HeadObject.
	_, _, err = store.Get(ctx, "missing/obj.bin", "")
	assert.ErrorIs(t, err, blob.ErrNotFound)

	_, err = store.Stat(ctx, "missing/obj.bin")
	assert.ErrorIs(t, err, blob.ErrNotFound)

	// HTTP 404 ResponseError must also map to ErrNotFound.
	http404 := &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusNotFound}},
		Err:      errors.New("404 not found"),
	}
	fake.getErr = http404
	fake.headErr = http404
	_, _, err = store.Get(ctx, "missing/http404.bin", "")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	_, err = store.Stat(ctx, "missing/http404.bin")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	fake.getErr = nil
	fake.headErr = nil

	// Delete is idempotent on missing keys and HTTP 404.
	require.NoError(t, store.Delete(ctx, "missing/obj.bin"))
	fake.deleteErr = http404
	require.NoError(t, store.Delete(ctx, "missing/obj.bin"))
	fake.deleteErr = &types.NoSuchKey{}
	require.NoError(t, store.Delete(ctx, "missing/obj.bin"))
}

func TestStorePutWithExpectedSHA256AndRollbackOnMismatch(t *testing.T) {
	payload := []byte("important build artifact payload")
	wrongDigest := strings.Repeat("0", 64)

	fake := newFakeS3Client()
	store, err := newStoreWithClient(Config{Bucket: "bkt", KeyPrefix: "prod"}, fake)
	require.NoError(t, err)

	// Cancel ctx as PutObject finishes to prove rollback DeleteObject uses context.WithoutCancel(ctx).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.onPutObject = cancel

	_, err = store.Put(ctx, "runs/r1/art.zip", bytes.NewReader(payload), wrongDigest)
	require.ErrorIs(t, err, blob.ErrDigestMismatch)
	assert.Nil(t, fake.deleteCtxErrOnCall, "rollback DeleteObject must survive canceled caller ctx")
	assert.Contains(t, fake.deleteCalls, "prod/runs/r1/art.zip")
	assert.Empty(t, fake.objects, "dirty object must be deleted on digest mismatch")
}

func TestStorePutMultipartAndUnknownDigestTempFileCleanup(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)
	t.Setenv("TMP", tmpDir)
	t.Setenv("TEMP", tmpDir)

	fake := newFakeS3Client()
	store, err := newStoreWithClient(Config{
		Bucket:        "bkt",
		KeyPrefix:     "sofa",
		PartSizeBytes: 64,
	}, fake)
	require.NoError(t, err)

	// 200 bytes > 64-byte PartSizeBytes must trigger multipart upload.
	largePayload := bytes.Repeat([]byte("multipart-chunk-"), 13)[:200]
	largeDigest := sha256Of(largePayload)

	metaKnown, err := store.Put(ctx, "runs/r1/large-known.bin", bytes.NewReader(largePayload), largeDigest)
	require.NoError(t, err)
	assert.Equal(t, "runs/r1/large-known.bin", metaKnown.Key)
	assert.Equal(t, largeDigest, metaKnown.SHA256)
	assert.Equal(t, int64(len(largePayload)), metaKnown.SizeBytes)
	assert.Contains(t, fake.calls, "CreateMultipartUpload")
	assert.Contains(t, fake.calls, "UploadPart")
	assert.Contains(t, fake.calls, "CompleteMultipartUpload")

	// Unknown digest (expectedSHA256 == "") computes SHA256 via temp file, uploads with sofa-sha256 metadata, and removes temp file.
	fake.calls = nil
	metaUnknown, err := store.Put(ctx, "runs/r1/large-unknown.bin", bytes.NewReader(largePayload), "")
	require.NoError(t, err)
	assert.Equal(t, metaKnown.SHA256, metaUnknown.SHA256)
	assert.Equal(t, metaKnown.SizeBytes, metaUnknown.SizeBytes)
	assert.Contains(t, fake.calls, "CreateMultipartUpload")

	stored := fake.objects["sofa/runs/r1/large-unknown.bin"]
	require.NotNil(t, stored)
	assert.Equal(t, largeDigest, stored.metadata["sofa-sha256"])

	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sofa-s3-put-") {
			t.Fatalf("leftover temp file %s found in %s", filepath.Join(tmpDir, e.Name()), tmpDir)
		}
	}
}

func TestStoreGetStatAndTamperDetection(t *testing.T) {
	ctx := context.Background()
	fake := newFakeS3Client()
	store, err := newStoreWithClient(Config{Bucket: "bkt", KeyPrefix: "tenant"}, fake)
	require.NoError(t, err)

	payload := []byte("deterministic artifact bytes for verification")
	digest := sha256Of(payload)
	key := "runs/r1/art.zip"

	putMeta, err := store.Put(ctx, key, bytes.NewReader(payload), digest)
	require.NoError(t, err)

	st, err := store.Stat(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, putMeta, st)

	// Upfront metadata mismatch closes body immediately before returning ErrDigestMismatch.
	wrongDigest := strings.Repeat("f", 64)
	_, _, err = store.Get(ctx, key, wrongDigest)
	require.ErrorIs(t, err, blob.ErrDigestMismatch)
	require.NotNil(t, fake.lastGetBody)
	assert.True(t, fake.lastGetBody.isClosed(), "Get must close body immediately on upfront metadata mismatch")

	// Case-insensitive metadata key support.
	fake.objects["tenant/"+key].metadata = map[string]string{"Sofa-Sha256": digest}
	rc, gotMeta, err := store.Get(ctx, key, digest)
	require.NoError(t, err)
	assert.Equal(t, putMeta, gotMeta)
	gotBytes, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, payload, gotBytes)

	// Early Close before EOF releases handle cleanly without ErrDigestMismatch.
	rcEarly, _, err := store.Get(ctx, key, digest)
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = rcEarly.Read(buf)
	require.NoError(t, err)
	require.NoError(t, rcEarly.Close())
	assert.True(t, fake.lastGetBody.isClosed())

	// Reading (n > 0, io.EOF) in single Read succeeds when digest matches.
	fake.singleEOFChunkRead = true
	rcSingleEOF, _, err := store.Get(ctx, key, digest)
	require.NoError(t, err)
	gotSingle, err := io.ReadAll(rcSingleEOF)
	require.NoError(t, err)
	require.NoError(t, rcSingleEOF.Close())
	assert.Equal(t, payload, gotSingle)
	fake.singleEOFChunkRead = false

	// Mid-stream corruption caught at EOF even with empty expectedSHA256.
	fake.corruptNextGetBody = func(b []byte) []byte {
		out := append([]byte(nil), b...)
		out[5] ^= 0xff
		return out
	}
	rcTampered, _, err := store.Get(ctx, key, "")
	require.NoError(t, err)
	_, err = io.ReadAll(rcTampered)
	require.ErrorIs(t, err, blob.ErrDigestMismatch)
	require.NoError(t, rcTampered.Close())

	// Mid-stream corruption also caught when expectedSHA256 is given.
	fake.corruptNextGetBody = func(b []byte) []byte {
		out := append([]byte(nil), b...)
		out[0] ^= 0x01
		return out
	}
	rcTamperedExpected, _, err := store.Get(ctx, key, digest)
	require.NoError(t, err)
	_, err = io.ReadAll(rcTamperedExpected)
	require.ErrorIs(t, err, blob.ErrDigestMismatch)
	require.NoError(t, rcTamperedExpected.Close())

	// Missing or invalid sofa-sha256 metadata must be rejected by both Stat and Get (closing Get body).
	for _, badMeta := range []map[string]string{
		nil,
		{"sofa-sha256": ""},
		{"sofa-sha256": "corrupt-hex"},
		{"sofa-sha256": strings.ToUpper(digest)},
	} {
		fake.objects["tenant/"+key].metadata = badMeta

		_, err = store.Stat(ctx, key)
		require.ErrorIs(t, err, blob.ErrDigestMismatch, "Stat with metadata %v", badMeta)

		fake.lastGetBody = nil
		_, _, err = store.Get(ctx, key, "")
		require.ErrorIs(t, err, blob.ErrDigestMismatch, "Get(expected=\"\") with metadata %v", badMeta)
		require.NotNil(t, fake.lastGetBody)
		assert.True(t, fake.lastGetBody.isClosed(), "Get must close body on missing/invalid metadata %v", badMeta)

		fake.lastGetBody = nil
		_, _, err = store.Get(ctx, key, digest)
		require.ErrorIs(t, err, blob.ErrDigestMismatch, "Get(expected=digest) with metadata %v", badMeta)
		require.NotNil(t, fake.lastGetBody)
		assert.True(t, fake.lastGetBody.isClosed(), "Get must close body on missing/invalid metadata %v", badMeta)
	}
}

func TestStoreDeletePrefixPaginationAndBoundaryIsolation(t *testing.T) {
	ctx := context.Background()
	fake := newFakeS3Client()
	// Force pagination even if listPageLimit is 400 so 1250 keys span 4 pages AND trigger >1000 batching tests.
	fake.listPageLimit = 600
	store, err := newStoreWithClient(Config{Bucket: "bkt", KeyPrefix: "tenant-a"}, fake)
	require.NoError(t, err)

	for i := 0; i < 1250; i++ {
		key := fmt.Sprintf("tenant-a/runs/run-1/obj-%04d.bin", i)
		fake.objects[key] = &storedObject{data: []byte("x"), metadata: map[string]string{"sofa-sha256": sha256Of([]byte("x"))}}
	}
	// Sibling prefixes and external KeyPrefix objects that MUST survive.
	keepKeys := []string{
		"tenant-a/runs/run-10/keep.bin",
		"tenant-a/runs/run-2/keep.bin",
		"tenant-a2/runs/run-1/keep.bin",
	}
	for _, k := range keepKeys {
		fake.objects[k] = &storedObject{data: []byte("keep"), metadata: map[string]string{"sofa-sha256": sha256Of([]byte("keep"))}}
	}

	// DeletePrefix without trailing slash MUST normalize to slash boundary ("runs/run-1/").
	require.NoError(t, store.DeletePrefix(ctx, "runs/run-1"))

	for i := 0; i < 1250; i++ {
		key := fmt.Sprintf("tenant-a/runs/run-1/obj-%04d.bin", i)
		_, exists := fake.objects[key]
		assert.False(t, exists, "expected %s to be deleted", key)
	}
	for _, k := range keepKeys {
		_, exists := fake.objects[k]
		assert.True(t, exists, "sibling/other-tenant key %s must remain intact", k)
	}
	for _, sz := range fake.deleteBatchSizes {
		assert.LessOrEqual(t, sz, 1000)
	}
	for _, q := range fake.deleteQuietFlags {
		assert.True(t, q, "DeleteObjects must enable Quiet: true")
	}

	// Empty prefix match is idempotent nil.
	fake.deleteBatchSizes = nil
	require.NoError(t, store.DeletePrefix(ctx, "runs/nonexistent"))
	assert.Empty(t, fake.deleteBatchSizes)

	// DeletePrefix("") with KeyPrefix deletes only within tenant-a/, leaving tenant-a2/ intact.
	require.NoError(t, store.DeletePrefix(ctx, ""))
	_, err = store.Stat(ctx, "runs/run-10/keep.bin")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	_, err = store.Stat(ctx, "runs/run-2/keep.bin")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	_, stillExists := fake.objects["tenant-a2/runs/run-1/keep.bin"]
	assert.True(t, stillExists, "tenant-a2 object must never be deleted by tenant-a DeletePrefix(\"\")")

	// DeleteObjectsOutput.Errors must be surfaced as an error.
	fake.objects["tenant-a/runs/err/a.bin"] = &storedObject{data: []byte("a")}
	fake.deleteObjectsErrs = []types.Error{{
		Key:     aws.String("tenant-a/runs/err/a.bin"),
		Code:    aws.String("AccessDenied"),
		Message: aws.String("denied"),
	}}
	err = store.DeletePrefix(ctx, "runs/err")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AccessDenied")
}
