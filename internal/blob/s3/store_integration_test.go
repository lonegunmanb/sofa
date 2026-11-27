//go:build integration

package s3_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/lonegunmanb/sofa/internal/blob"
	s3store "github.com/lonegunmanb/sofa/internal/blob/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ministackImage = "docker.io/ministackorg/ministack:latest"

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func startMinistackContainer(t *testing.T) (endpoint string, rawClient *awss3.Client) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := fmt.Sprintf("sofa-ministack-%d", time.Now().UnixNano())
	out, err := exec.CommandContext(ctx, "podman", "run", "-d", "--name", name, "-p", "127.0.0.1::4566", ministackImage).CombinedOutput()
	require.NoError(t, err, "start ministack container: %s", string(out))
	containerID := strings.TrimSpace(string(out))

	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rmCancel()
		_, _ = exec.CommandContext(rmCtx, "podman", "rm", "-f", containerID).CombinedOutput()
	})

	portOut, err := exec.CommandContext(ctx, "podman", "port", containerID, "4566/tcp").CombinedOutput()
	require.NoError(t, err, "query ministack mapped port: %s", string(portOut))
	hostPort := strings.TrimSpace(strings.Split(strings.TrimSpace(string(portOut)), "\n")[0])
	require.NotEmpty(t, hostPort)
	endpoint = "http://" + hostPort

	awsCfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	require.NoError(t, err)

	rawClient = awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = aws.String(endpoint)
	})

	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		_, lastErr = rawClient.ListBuckets(ctx, &awss3.ListBucketsInput{})
		if lastErr == nil {
			return endpoint, rawClient
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("ministack S3 endpoint %s did not become ready: %v", endpoint, lastErr)
	return "", nil
}

func TestS3StoreIntegrationMinistackMultipartAndIsolation(t *testing.T) {
	ctx := t.Context()
	endpoint, rawClient := startMinistackContainer(t)

	bucket := "sofa-integration-blobs"
	_, err := rawClient.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	storeA, err := s3store.NewStore(ctx, s3store.Config{
		Bucket:          bucket,
		Region:          "us-east-1",
		Endpoint:        endpoint,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		UsePathStyle:    true,
		KeyPrefix:       "tenant-a",
	})
	require.NoError(t, err)

	storeB, err := s3store.NewStore(ctx, s3store.Config{
		Bucket:          bucket,
		Region:          "us-east-1",
		Endpoint:        endpoint,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		UsePathStyle:    true,
		KeyPrefix:       "tenant-b",
	})
	require.NoError(t, err)

	// >12 MiB payload to trigger real multipart upload with default 8 MiB PartSizeBytes.
	const payloadSize = 13 * 1024 * 1024
	largePayload := bytes.Repeat([]byte("sofa-s3-multipart-stream-0123456"), payloadSize/32+1)[:payloadSize]
	largeDigest := sha256Hex(largePayload)

	// 1. Multipart Put with known expectedSHA256 -> Stat -> Get.
	knownKey := "runs/run-1/artifacts/known-13m.bin"
	metaKnown, err := storeA.Put(ctx, knownKey, bytes.NewReader(largePayload), largeDigest)
	require.NoError(t, err)
	assert.Equal(t, knownKey, metaKnown.Key)
	assert.Equal(t, largeDigest, metaKnown.SHA256)
	assert.Equal(t, int64(payloadSize), metaKnown.SizeBytes)

	statKnown, err := storeA.Stat(ctx, knownKey)
	require.NoError(t, err)
	assert.Equal(t, metaKnown, statKnown)

	rcKnown, gotMetaKnown, err := storeA.Get(ctx, knownKey, largeDigest)
	require.NoError(t, err)
	assert.Equal(t, metaKnown, gotMetaKnown)
	gotKnown, err := io.ReadAll(rcKnown)
	require.NoError(t, err)
	require.NoError(t, rcKnown.Close())
	assert.Equal(t, largePayload, gotKnown)

	// 2. Multipart Put with empty expectedSHA256 -> Stat confirms stored SHA256 -> Get verifies digest.
	unknownKey := "runs/run-1/artifacts/unknown-13m.bin"
	metaUnknown, err := storeA.Put(ctx, unknownKey, bytes.NewReader(largePayload), "")
	require.NoError(t, err)
	assert.Equal(t, largeDigest, metaUnknown.SHA256)
	assert.Equal(t, int64(payloadSize), metaUnknown.SizeBytes)

	statUnknown, err := storeA.Stat(ctx, unknownKey)
	require.NoError(t, err)
	assert.Equal(t, metaUnknown, statUnknown)

	rcUnknown, _, err := storeA.Get(ctx, unknownKey, "")
	require.NoError(t, err)
	gotUnknown, err := io.ReadAll(rcUnknown)
	require.NoError(t, err)
	require.NoError(t, rcUnknown.Close())
	assert.Equal(t, largePayload, gotUnknown)

	// 3. ErrDigestMismatch on Get with wrong expectedSHA256.
	_, _, err = storeA.Get(ctx, knownKey, strings.Repeat("0", 64))
	assert.ErrorIs(t, err, blob.ErrDigestMismatch)

	// 4. ErrDigestMismatch on Put with wrong expectedSHA256 leaves object absent.
	badKey := "runs/run-1/artifacts/bad-digest.bin"
	_, err = storeA.Put(ctx, badKey, bytes.NewReader([]byte("corrupt-check")), strings.Repeat("0", 64))
	assert.ErrorIs(t, err, blob.ErrDigestMismatch)
	_, err = storeA.Stat(ctx, badKey)
	assert.ErrorIs(t, err, blob.ErrNotFound)

	// 5. KeyPrefix isolation & sibling DeletePrefix boundary safety.
	siblingKey := "runs/run-10/keep.txt"
	_, err = storeA.Put(ctx, siblingKey, strings.NewReader("sibling-10"), "")
	require.NoError(t, err)

	tenantBKey := "runs/run-1/keep-b.txt"
	_, err = storeB.Put(ctx, tenantBKey, strings.NewReader("tenant-b"), "")
	require.NoError(t, err)

	require.NoError(t, storeA.DeletePrefix(ctx, "runs/run-1"))

	_, err = storeA.Stat(ctx, knownKey)
	assert.ErrorIs(t, err, blob.ErrNotFound)
	_, err = storeA.Stat(ctx, unknownKey)
	assert.ErrorIs(t, err, blob.ErrNotFound)

	// Sibling runs/run-10 and tenant-b must remain untouched.
	_, err = storeA.Stat(ctx, siblingKey)
	require.NoError(t, err)
	_, err = storeB.Stat(ctx, tenantBKey)
	require.NoError(t, err)

	// DeletePrefix("") on storeA clears tenant-a without affecting tenant-b.
	require.NoError(t, storeA.DeletePrefix(ctx, ""))
	_, err = storeA.Stat(ctx, siblingKey)
	assert.ErrorIs(t, err, blob.ErrNotFound)
	_, err = storeB.Stat(ctx, tenantBKey)
	require.NoError(t, err)

	// Idempotent Delete on tenant-b.
	require.NoError(t, storeB.Delete(ctx, tenantBKey))
	require.NoError(t, storeB.Delete(ctx, tenantBKey))
	_, err = storeB.Stat(ctx, tenantBKey)
	assert.ErrorIs(t, err, blob.ErrNotFound)
}
