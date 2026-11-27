# 分片 021：S3 / MinIO 兼容流式 `BlobStore` 实现（`internal/blob/s3`）

## 1. 目标与边界

在阶段二 `internal/blob.Store` 流式接口之上，实现面向多机分布式部署的 S3 兼容对象存储后端（支持 AWS S3、MinIO、阿里云 OSS、Cloudflare R2 等）：
1. **100% 契约同构**：完整实现 `blob.Store` 的 `Put`、`Get`、`Stat`、`Delete`、`DeletePrefix`，严格复用 `blob.ValidateKey`、`blob.ValidatePrefix`、`blob.ErrNotFound`、`blob.ErrDigestMismatch`、`blob.ErrInvalidKey`。
2. **流式上传下载与端到端 SHA-256 强校验**：
   - 大文件通过 `feature/s3/manager`（或 `transfermanager`）流式分片上传，内存中不整包缓冲；
   - 对象真实 SHA-256 摘要统一写入 S3 User Metadata（`x-amz-meta-sofa-sha256`），读取时在 `EOF` 处核验字节流真实哈希与元数据 / `expectedSHA256` 完全一致，防静默数据损坏。
3. **安全前缀隔离**：支持可选 `KeyPrefix`（如多集群共用同一 Bucket 时隔离在 `sofa-prod/` 下），所有操作仅作用于该前缀内。

**前置依赖**：分片 `011`（`internal/blob`）。

---

## 2. 涉及包与文件清单

- `internal/blob/s3/store.go`：`Config`、`Store` 结构体与 `blob.Store` 五个方法完整实现
- `internal/blob/s3/store_test.go`：注入窄 `s3API` 接口的 Tier 1 纯内存流式校验、错误映射与分页批量删除单测
- `internal/blob/s3/store_integration_test.go`（`//go:build integration`）：基于本地 Podman S3 兼容容器（`ministack:latest`）的真实分片上传、流式校验与前缀清理 Tier 2 集成测试

---

## 3. 保姆级实现细则

### 3.1 配置与构造签名（`internal/blob/s3/store.go`）
```go
package s3

type Config struct {
    Bucket          string
    Region          string // 默认 "us-east-1"
    Endpoint        string // 可选：自定义 MinIO / 私有 S3 Endpoint
    AccessKeyID     string
    SecretAccessKey string
    SessionToken    string
    UsePathStyle    bool   // MinIO / 本地容器默认开启
    KeyPrefix       string // 可选桶内根前缀，如 "sofa"
    PartSizeBytes   int64  // 默认 8 MiB
}

func NewStore(ctx context.Context, cfg Config) (*Store, error)
```

### 3.2 关键行为与防坑约束
1. **上传原子性与摘要校验（`Put`）**：
   - 先校验 `blob.ValidateKey(key)`；若 `expectedSHA256 != ""` 则校验 `blob.IsValidSHA256Hex(expectedSHA256)`（非法格式直接返回 `blob.ErrDigestMismatch`）。
   - 流式读取 `r` 计算真实 SHA-256：
     - 若调用方传入了已知 `expectedSHA256`：边流式上传边算哈希；若流结束发现真实哈希与 `expectedSHA256` 不符，立即删除刚写入的脏对象并返回 `blob.ErrDigestMismatch`（写入时同步附上 `Metadata: map[string]string{"sofa-sha256": expectedSHA256}`）；
     - 若 `expectedSHA256 == ""`（未知长度/哈希流）：先流式写入本地受限临时文件算出 `sha256` 与 `size`，再携带 `Metadata: {"sofa-sha256": actualHex}` 流式上传并在 `defer` 中删除临时文件，确保 S3 对象上的 `x-amz-meta-sofa-sha256` 永远存在且准确！
2. **流式校验读取（`Get`）与元数据查询（`Stat`）**：
   - S3 返回 `NoSuchKey` / HTTP `404` 时统一映射为 `blob.ErrNotFound`；
   - 从响应 `Metadata["sofa-sha256"]` 提取存储哈希；`Get` 返回包装后的 `io.ReadCloser`，在读到 `io.EOF` 时同时核对 `actualSHA == storedSHA` 且（若非空）`actualSHA == expectedSHA256`，不一致返回 `blob.ErrDigestMismatch`。
3. **分页批量清理（`DeletePrefix`）**：
   - 校验 `blob.ValidatePrefix(prefix)`；将非空 `prefix` 规范化为带尾部 `'/'`（与 `LocalStore` 语义一致，防止删 `"runs/r1"` 误伤 `"runs/r10/..."`！）；
   - 使用 `ListObjectsV2Paginator` 循环列出每页最多 1000 个对象并调用 `DeleteObjects`（`Quiet: true`）批量删除，空前缀匹配零对象时幂等返回 `nil`。

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 单测（`store_test.go`）**：
   - 合法/非法 Key 与 Prefix 校验、`404` → `blob.ErrNotFound` 映射；
   - `Put` 指定错误 `expectedSHA256` 时触发回滚删除并返回 `blob.ErrDigestMismatch`；
   - `Get` 流在中途被篡改字节时，读到 `EOF` 精确返回 `blob.ErrDigestMismatch`；
   - `DeletePrefix("runs/run-1")` 超过 1000 个对象时跨页删净，且绝不误删兄弟前缀 `"runs/run-10/..."`。
2. **Tier 2 真实 S3 容器测试（`store_integration_test.go`，`//go:build integration`）**：
   - 使用 Podman 启动本地 `docker.io/ministackorg/ministack:latest`（Path-Style S3 服务，`defer` 销毁容器），创建测试 Bucket，跑通 >10 MiB 多分片大对象流式 `Put` → `Stat` → `Get` SHA-256 校验 → `DeletePrefix` 清空验证。
