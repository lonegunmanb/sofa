# Research: `aws-sdk-go-v2` & `LocalStore` Parity (`021-s3-blobstore`)

## 1. Upstream `aws-sdk-go-v2` Source Inspection (`GOMODCACHE`)

- **`feature/s3/manager` vs `feature/s3/transfermanager`**:
  - `feature/s3/manager@v1.23.1/go.mod` line 1 explicitly marks it deprecated: `// Deprecated: superceded by feature/s3/transfermanager. See https://github.com/aws/aws-sdk-go-v2/discussions/3306`.
  - `feature/s3/transfermanager@v0.4.1` provides `transfermanager.New(s3Client transfermanager.S3APIClient, optFns ...func(*transfermanager.Options)) *transfermanager.Client` and `Client.UploadObject(ctx, *transfermanager.UploadObjectInput, ...)`.
  - `transfermanager.Options` defaults `PartSizeBytes` to `8 MiB` (`defaultPartSizeBytes`) and `MultipartUploadThreshold` to `16 MiB` (`defaultMultipartUploadThreshold`). Setting BOTH `PartSizeBytes` and `MultipartUploadThreshold` to the configured `PartSizeBytes` (default `8 MiB`) ensures objects `>= PartSizeBytes` (such as >10–12 MiB integration payloads) execute real multipart uploads (`CreateMultipartUpload` / `UploadPart` / `CompleteMultipartUpload`) and allows Tier 1 tests to exercise multipart splitting with small `PartSizeBytes`.
- **Non-deprecated Endpoint Configuration (`service/s3@v1.114.5/options.go`)**:
  - `s3.Options.EndpointResolver` is deprecated; `s3.Options.BaseEndpoint` (`*string`) + `s3.Options.UsePathStyle` (`bool`) is the non-deprecated endpoint override mechanism.
- **Static & Default Credentials (`config@v1.33.11`, `credentials@v1.20.11`)**:
  - `config.LoadDefaultConfig(ctx, opts...)` supports `config.WithRegion(region)` and, when `AccessKeyID` and `SecretAccessKey` are provided, `config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(id, secret, token))`. When static credentials are omitted, standard AWS SDK default credential chain applies.
- **Error Mapping (`service/s3@v1.114.5/types/errors.go`, `smithy-go@v1.28.6`)**:
  - S3 missing object errors appear as `*types.NotFound` (commonly on `HeadObject`), `*types.NoSuchKey` (commonly on `GetObject`), `smithy.APIError` with `ErrorCode()` `"NoSuchKey"` / `"NotFound"` / `"404"`, or `*smithyhttp.ResponseError` with `HTTPStatusCode() == http.StatusNotFound`.
- **Pagination & Batch Delete (`service/s3@v1.114.5`)**:
  - `s3.NewListObjectsV2Paginator(client, input)` returns a `*s3.ListObjectsV2Paginator` (`HasMorePages()`, `NextPage(ctx)`).
  - `DeleteObjects` accepts up to 1000 `types.ObjectIdentifier` entries per request (`Quiet: aws.Bool(true)`) and returns `DeleteObjectsOutput.Errors []types.Error` for partial failures.

## 2. `LocalStore` Behavioral Parity (`internal/blob/{store.go,local.go,store_test.go}`)

- `ValidateKey(key)` and `ValidatePrefix(prefix)` enforce `[A-Za-z0-9._-]` per segment, rejecting empty keys, leading/trailing `/`, `//`, `\`, `.`, `..`, spaces, and control characters with `blob.ErrInvalidKey`.
- `IsValidSHA256Hex(digest)` enforces 64 lowercase hex characters; malformed non-empty `expectedSHA256` returns `blob.ErrDigestMismatch`.
- `Get` wraps the returned `io.ReadCloser` so EOF verification checks `actualSHA` against both stored metadata and optional `expectedSHA256`, returning `0, ErrDigestMismatch` if mismatched (even when `Read` receives `(n > 0, io.EOF)`), while early `Close()` before `EOF` returns `nil` without digest errors.
- `DeletePrefix` normalizes non-empty `prefix` with a trailing `'/'` so `"runs/run-1"` becomes `"runs/run-1/"` and never deletes sibling `"runs/run-10/..."`.
