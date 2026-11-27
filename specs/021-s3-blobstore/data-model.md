# Data Model: `internal/blob/s3` (`021-s3-blobstore`)

## 1. Configuration (`Config`)

| Field | Type | Default / Behavior |
| :--- | :--- | :--- |
| `Bucket` | `string` | Required; non-empty target S3 bucket name |
| `Region` | `string` | Defaults to `"us-east-1"` when empty |
| `Endpoint` | `string` | Optional custom endpoint URL (e.g., MinIO, ministack, R2, OSS) via `s3.Options.BaseEndpoint` |
| `AccessKeyID` | `string` | Optional static credential; paired with `SecretAccessKey` |
| `SecretAccessKey` | `string` | Optional static credential; paired with `AccessKeyID` |
| `SessionToken` | `string` | Optional static STS session token |
| `UsePathStyle` | `bool` | Enables path-style bucket addressing (`s3.Options.UsePathStyle`) |
| `KeyPrefix` | `string` | Optional bucket root prefix (e.g. `"sofa"` -> objects under `"sofa/<key>"`) |
| `PartSizeBytes` | `int64` | Multipart part size & threshold; defaults to `8 * 1024 * 1024` (8 MiB); negative rejected |

## 2. Internal S3 Boundary (`s3API`)

Embeds `transfermanager.S3APIClient` (`PutObject`, `UploadPart`, `CreateMultipartUpload`, `CompleteMultipartUpload`, `AbortMultipartUpload`, `GetObject`, `HeadObject`, `ListObjectsV2`) and adds:
- `DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)`
- `DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)`

## 3. S3 Object Metadata Mapping

- **Physical S3 Key**: `<normalized_KeyPrefix>/<logical_key>` if `KeyPrefix != ""`, otherwise `<logical_key>`.
- **Logical Key (`ObjectMeta.Key`)**: Always the unprefixed caller-supplied key.
- **User Metadata Header**: `Metadata["sofa-sha256"]` (S3 wire header `x-amz-meta-sofa-sha256`), storing 64-char lowercase hex SHA-256.
- **Size (`ObjectMeta.SizeBytes`)**: Verified byte count on `Put`; `ContentLength` on `Stat` and `Get`.
