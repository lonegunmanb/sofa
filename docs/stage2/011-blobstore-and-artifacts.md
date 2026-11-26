# 分片 011：`BlobStore` 流式存储层与跨机器不可变 `Artifact` 打包/还原

## 1. 目标与边界

构建支撑跨机器非 Git 数据流转（`.sofaplan` 归档包、不可变 `Artifact` 产物包、检查点半成品 `/.sofa/artifacts` 快照以及原始会话事件日志）的存储与打包底座：
1. **通用流式 `BlobStore` 接口与本地原子 `LocalStore`（`internal/blob`）**：
   - 定义面向流（`io.Reader` / `io.ReadCloser`）的 `Store` 接口，内置写入与读取双向 `SHA-256` 强核验及按前缀批量清理（`DeletePrefix`）；
   - 实现 `<state_dir>/blobs/` 本地文件系统 `LocalStore`（阶段三仅需再加一个实现同一接口的 `S3Store` 即可无缝切换分布式对象存储）。
2. **不可变 `Artifact` 校验、打包发布与跨机器只读还原（`internal/artifact`）**：
   - 对宿主机临时 `artifactsDir`（对应容器 `/.sofa/artifacts/`）执行严格存在性、类型（`file`/`directory`）、`non_empty` 与防符号链接校验；
   - 将每个产物打包为确定性 ZIP 流上传 `BlobStore`，签发全局唯一 `art_<uuid>` 不可变记录；
   - 在消费机器上从 `BlobStore` 拉取（带本机按 `SHA-256` 缓存避免同机重复下载）、核验摘要并安全解压到宿主机 `importsDir`（对应容器 `/.sofa/imports/`）；
   - 提供检查点期半成品 `artifactsDir` 整目录快照打包与跨机恢复解压辅助函数。

**前置依赖**：分片 `010`。

---

## 2. 涉及包与文件清单

- `internal/blob/store.go`：`Store` 接口、`ObjectMeta`、键名合法性校验与标准哨兵错误（`ErrNotFound`、`ErrDigestMismatch`、`ErrInvalidKey`）
- `internal/blob/local.go`（及 `permissions_windows.go` / `permissions_unix.go` 或复用通用 `0600` 辅助）：`LocalStore` 原子写盘与并发安全读写
- `internal/blob/store_test.go`：`LocalStore` 单测
- `internal/artifact/validate.go`：对齐 `r42/internal/artifact/validator.go` 的 `Validate` 与结构化 `Issue` 返回
- `internal/artifact/service.go`：`Service`、`PublishedArtifact`、`Publish`、`StageImports`、`SnapshotStagingDir`、`RestoreStagingDir`
- `internal/artifact/artifact_test.go`：单机与模拟跨机器（双独立本地目录共用同一 `BlobStore`）全链路单测

（`go.mod` 按需引入 `github.com/google/uuid` 生成 `art_<uuid>`，或使用标准库 `crypto/rand` 生成符合 UUIDv4 格式的 ID。）

---

## 3. 保姆级实现细则

### 3.1 `internal/blob` 接口与 `LocalStore` 规范

```go
package blob

type ObjectMeta struct {
    Key       string `json:"key"`
    SHA256    string `json:"sha256"`
    SizeBytes int64  `json:"size_bytes"`
}

type Store interface {
    // Put 流式写入对象。若 expectedSHA256 非空（64 位小写 hex），边写边算哈希，不匹配则丢弃临时文件并返回 ErrDigestMismatch。
    Put(ctx context.Context, key string, r io.Reader, expectedSHA256 string) (ObjectMeta, error)
    // Get 打开对象读取流。若 expectedSHA256 非空，包装返回的 ReadCloser 在读到 EOF 时核验完整 SHA-256（篡改则 Read 返回 ErrDigestMismatch）。
    Get(ctx context.Context, key string, expectedSHA256 string) (io.ReadCloser, ObjectMeta, error)
    Stat(ctx context.Context, key string) (ObjectMeta, error)
    Delete(ctx context.Context, key string) error          // 不存在时幂等返回 nil
    DeletePrefix(ctx context.Context, prefix string) error // 清理如 "runs/<run_id>/" 下的全部对象
}
```

- **Key 安全约束（`ValidateKey`）**：
  - 仅允许以 `/` 分隔的相对路径段，每段仅允许 `[A-Za-z0-9._-]`，严禁空段、前导/尾随 `/`、`\`、`.`、`..` 或控制字符（防路径穿越且直接兼容未来 S3 Object Key）。
- **`LocalStore` 落盘布局**：
  - 根目录 `rootDir`（权限 `0700`）：数据文件存为 `<rootDir>/objects/<key>`，同级 `<rootDir>/objects/<key>.meta.json` 记录 `{"sha256":"...","size_bytes":123}`；
  - `Put` 必须先写 `<rootDir>/.tmp/upload-*.tmp` → 计算 `sha256` 与字节数 → 关闭文件句柄（Windows 重命名铁律！）→ 权限收紧为 `0600` / Windows 当前用户专属 DACL → `os.Rename` 原子替换数据文件与 `.meta.json`。

### 3.2 `internal/artifact` 数据模型与核心流程

```go
package artifact

type PublishedArtifact struct {
    ID            string `json:"id"`             // 格式: "art_<uuid>"
    Name          string `json:"name"`           // HCL artifact 块名
    Type          string `json:"type"`           // "file" | "directory"
    RelPath       string `json:"rel_path"`       // 如 "docs/plan.md"
    ContainerPath string `json:"container_path"` // 如 "/.sofa/artifacts/docs/plan.md"
    BlobKey       string `json:"blob_key"`       // 如 "runs/<run_id>/artifacts/art_<uuid>.zip"
    SHA256        string `json:"sha256"`
    SizeBytes     int64  `json:"size_bytes"`
}

type ImportSource struct {
    Artifact PublishedArtifact
}

type ImportRequest struct {
    Name    string         // HCL import_artifact 块名
    RelPath string         // effectiveImportRelPath，如 "design" 或 "inputs/plan.md"
    Sources []ImportSource // 1..N 个已发布上游产物
}
```

1. **产出校验（`Validate(artifactsDir string, decls []workerspec.Artifact)`）**：
   - 使用 `os.OpenRoot(artifactsDir)` 限定访问边界，严禁任何符号链接（`fs.ModeSymlink` 直接报错）；
   - 按序检查每个 `decl`：
     - 文件不存在：若 `decl.Required == true` 记录结构化 `Issue{Code: "artifact_missing", ...}`，若 `Required == false` 则跳过（不发布该可选产物）；
     - 类型不匹配（声明 `file` 实际是目录，或声明 `directory` 实际是普通文件）：无论 `Required` 与否均记录 `artifact_type_mismatch`；
     - `decl.NonEmpty == true`：`file` 要求 `Size() > 0`；`directory` 要求递归至少包含 1 个普通文件（且至少 1 个普通文件或非空内容，与 `r42` 对齐：至少含 1 个常规文件且总字节或常规文件存在），否则记录 `artifact_empty`。
   - 返回 `(validDecls []workerspec.Artifact, issues []Issue, err error)`：存在 `issues` 时供上层反馈给 Work Agent 自修，无 `issues` 时才允许 `Publish`。
2. **不可变打包上传（`Publish(ctx, runID, artifactsDir, validDecls)`）**：
   - 对每个有效产物创建确定性 ZIP：
     - 若 `Type == "file"`：ZIP 内固定写入单条目 `"payload"`（存储该文件内容，修改时间固定为 `time.Unix(0, 0).UTC()`）；
     - 若 `Type == "directory"`：按字典序遍历该子目录，写入 `"tree/<rel>"`（拒绝符号链接与非常规文件，单文件与总大小复用安全上限）；
   - 调用 `store.Put(ctx, fmt.Sprintf("runs/%s/artifacts/%s.zip", runID, id), zipReader, zipSHA256)` 返回 `PublishedArtifact`。
3. **消费端跨机还原（`StageImports(ctx, importsDir string, requests []ImportRequest)`）**：
   - 若配置了本机只读缓存目录 `cacheDir`：先查 `<cacheDir>/<sha256>.zip` 是否存在且实际哈希等于 `src.Artifact.SHA256`；未命中则从 `store.Get(ctx, blobKey, sha256)` 拉取原子写入缓存；
   - 解压规则（使用 `os.OpenRoot(importsDir)` 防 Zip-Slip，解压上限校验与 `009` 一致）：
     - **当 `len(req.Sources) == 1`**：
       - 若源 `Type == "file"`：将 `"payload"` 直接写到 `<importsDir>/<req.RelPath>`；
       - 若源 `Type == "directory"`：将 `"tree/**"` 解压到目录 `<importsDir>/<req.RelPath>/**`；
     - **当 `len(req.Sources) > 1`**（如从上游 `for_each` 收集多个产物）：
       - 统一将 `<importsDir>/<req.RelPath>` 作为目录，第 `i` 个源解压到 `<importsDir>/<req.RelPath>/<fmt.Sprintf("%03d_%s", i, src.Artifact.Name)>` 下，彻底杜绝多源同名覆盖！
4. **检查点半成品快照（`SnapshotStagingDir` / `RestoreStagingDir`）**：
   - `SnapshotStagingDir(ctx, blobKey, artifactsDir)`：若 `artifactsDir` 为空返回 `(nil, nil)`；非空则整树按字典序打成确定性 ZIP 上传 `BlobStore` 返回 `*ObjectMeta`；
   - `RestoreStagingDir(ctx, meta, destArtifactsDir)`：从 `BlobStore` 按 `meta.Key` + `meta.SHA256` 拉取并完整还原回新机器的 `destArtifactsDir`。

---

## 4. 必测 TDD Red → Green 用例清单

1. **`LocalStore` 完整性与清理**：
   - `Put` 传入错误 `expectedSHA256` 被拒绝且不留垃圾文件；落盘后人为篡改底层文件内容，`Get(..., expectedSHA256)` 读完时精确返回 `ErrDigestMismatch`；
   - 非法 Key（含 `../evil`、`a\b`、空串）返回 `ErrInvalidKey`；
   - `DeletePrefix("runs/run-1/")` 清空 `run-1` 下全部对象但完整保留 `runs/run-2/`。
2. **`Validate` 全矩阵**：
   - 覆盖缺失必填文件（返回 `artifact_missing` Issue）、缺失可选文件（无 Issue 且跳过发布）、类型错配（`artifact_type_mismatch`）、`non_empty = true` 遇 0 字节文件或空目录（`artifact_empty`）、以及目录内含符号链接报错。
3. **模拟跨机器 A → `BlobStore` → 机器 B 往返**：
   - 在临时目录 `machineA` 生成单文件产物与多级目录产物 → `Publish` 上传到共享 `LocalStore` → **彻底删除 `machineA` 目录** → 在全新临时目录 `machineB` 调用 `StageImports`（覆盖单源 `file`、单源 `directory`、多源 `len(Sources)==2` 三种情况），验证还原文件字节 100% 一致，且第二次 `StageImports` 命中 `cacheDir`（即使删除 `BlobStore` 原对象或统计读取次数也可验证缓存生效与防篡改重拉）。
4. **检查点半成品快照恢复**：
   - 在 `machineA` 的 `artifactsDir` 写入未定稿半成品 → `SnapshotStagingDir` → 删除 `machineA` → 在 `machineC` 调用 `RestoreStagingDir` 完整恢复目录结构与文件内容。
