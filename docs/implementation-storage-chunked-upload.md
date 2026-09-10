# Torchwood Storage 分片上传实现方案

> 状态：**已实现**（2026-08-10 验收通过：upload session 全链路 + 断点续传 + 服务端
> ComposeObject 合并；minio 真实集成测试 3/3 PASS；4 项实现偏差经裁决全部接受。
> 遗留问题修复 2026-08-10 验收通过：孤儿分片后台清理（worker ChunkCleaner 1h tick/
> 48h 阈值 + DeleteBucket 前缀清尾）、received_count 原子准确化（SCARD）、
> minio List 端口与集成测试；5 项修复偏差经裁决全部接受）
> 目标读者：维护者与后续扩展
> 关联：`docs/roadmap.md` §2.5（Storage）、`AGENTS.md`（开发约定，必读）
> 参考：`docs/implementation-settings-page.md`（上一轮同类方案：先审查、后实现、再汇报）
> 修订记录：2026-08-09 v2（独立评审修订：use-case 签名补 projectID/ownerUserID、
> ComposeObject ContentType 无效声明、complete 互斥锁、分片大小严格校验、part_count 上限、
> MaxBytesReader 缓冲、getUpload scope 修正、CI minio service 等）

---

## 1. 目标与验收标准

实现大文件分片上传（断点续传 + 服务端合并），突破现有单次 multipart 100MiB 上限。

**验收标准**：

1. 创建上传会话 → 逐片上传（每片 ≤ 16MiB）→ 查询已收分片（断点续传）→ 完成合并：
   合并后文件**文档**元数据（size/mime/metadata）正确，`GET .../download` 下载内容与
   原始文件完全一致。**mime 以文档为准**（ComposeObject 无法设置目标对象
   Content-Type，见 §3.2 阻断修正——对象自身 mime 恒为 octet-stream，下载/内联
   响应头不受影响）。
2. 分片上传会话 24h 过期（Redis TTL，上传中每片刷新）；取消上传清理全部暂存分片
   对象与会话。
3. 鉴权与会话绑定：仅会话所属项目的主体可上传/完成/取消（use-case 二次校验）；
   API key scope：写操作 `storage.write`，**getUpload 因 GET 分支自动要求
   `storage.read`**（file_handler.go:429-431 按方法分支）。
4. 校验：单分片 ≤ 16MiB（**非最后一片必须 == chunkSize，最后一片 1..chunkSize**，
   保证 sum(parts)==size）；分片号 1..part_count；part_count ≤ 10000
   （ComposeObject 上限，即 size ≤ 156.25GB）；重复上传同号幂等（覆盖）。
5. Console 大文件（> 16MiB）自动分片上传：切片、顺序上传、进度显示、失败重试
   （uploadId 存 localStorage 实现跨页面续传）。
6. `go test ./...`、`task lint`、`task build` 全绿；新增 minio 真实集成测试
   （本地/CI 提供 MinIO 时运行，否则跳过）。

**对 roadmap 端点的偏离（显式声明）**：roadmap §2.5 的 `POST /v1/storage/buckets/{id}/files/{id}/chunks`
（先建文件、再传分片）改为 **upload session 端点**（`uploads/{uploadId}/chunks/{part}`）：
分片上传需要会话生命周期（start/query/complete/abort），"先建文件文档"的语义会在中途
失败时留下孤儿文件记录；完成时才创建文件文档更干净。

---

## 2. 现状盘点（调研结论）

| 项 | 现状 |
|---|---|
| ObjectStore 端口 | `internal/domain/storage/object.go:41-52`：EnsureBucket/Put/Get/Delete/Ping，无分片方法 |
| minio-go | v7.2.0（go.mod:17）；**无公开低层多部分 API**（v6 的 NewMultipartUpload 等 v7 移除）；`PutObject` 自动分片；**`ComposeObject` 公开可用**（服务端拷贝拼接，除最后一片外每片 ≥5MiB，源数 ≤10000） |
| 上传路径 | `file_handler.go:105-110` 单次 multipart（字段 `file`，`maxUploadBytes=100MiB` :113）；鉴权 `authorize`（:419-445）+ scope 按方法分支（:426-434） |
| use-case | `CreateFile`（storage.go:171-233）：handler 传 projectID/ownerUserID 经 Command 字段（**databases.Principal 无 ProjectID/UserID**，:7-16） |
| 会话存储 | 无；Redis 已接线（clients.NewRedis，miniredis 先例多） |
| 残留 | 全仓库无 chunk/upload-session 代码（全新实现） |
| Console | `uploadFile` 单文件 multipart（api/storage.ts:81-95）；BucketDetailPage 单 Input（pages.tsx:354-364）；无 Progress 组件 |
| CI | backend job 只有 postgres service（.github/workflows/ci.yml:16-33），**无 minio**；`TORCHWOOD_RUN_DOCKER_TESTS=1` 已设 |

---

## 3. 分层实现规格

### 3.1 领域端口

**`internal/domain/storage/object.go`** 追加：

```go
// Compose 将 srcKeys 按序服务端合并为 dstKey（映射 minio-go ComposeObject；
// 约束：除最后一个源外每个源 ≥ 5MiB、源数 ≤ 10000、目标对象 Content-Type
// 无法设置（多源路径忽略，对象 mime 恒为 octet-stream，以文档 mime 为准））。
Compose(ctx context.Context, bucket, dstKey string, srcKeys []string) error
```

**`internal/domain/storage/upload_session.go`**（新建）：

```go
package storage

type UploadSession struct {
    ID          string
    ProjectID   string
    BucketID    string
    FileID      string // 预生成，complete 时创建文件文档
    Name        string
    MimeType    string // 已归一化（normalizeMimeType）
    Size        int64
    Metadata    map[string]string
    Permissions []string
    ChunkSize   int64
    PartCount   int
    Received    map[int]bool // 已收分片（续传查询）
    CreatedAt   time.Time
    ExpiresAt   time.Time
}

// UploadSessionStore 持久化上传会话（Redis 实现，TTL 24h）。
type UploadSessionStore interface {
    Create(ctx context.Context, s *UploadSession) error
    Get(ctx context.Context, uploadID string) (*UploadSession, error)
    // MarkChunk 原子标记分片已收（幂等）。
    MarkChunk(ctx context.Context, uploadID string, partNumber int) error
    Delete(ctx context.Context, uploadID string) error
    // LockComplete 尝试获取 complete 互斥锁（SETNX，5min TTL）；已持有返回 false。
    LockComplete(ctx context.Context, uploadID string) (bool, error)
    // UnlockComplete 释放 complete 锁。
    UnlockComplete(ctx context.Context, uploadID string) error
}
```

**常量**：

```go
const (
    DefaultChunkSize     = 16 << 20 // 16 MiB
    UploadSessionTTL     = 24 * time.Hour
    MaxChunkSize         = 16 << 20 // 单分片上限
    MinComposePartSize   = 5 << 20  // ComposeObject：除末片外每片 ≥ 5MiB
    MaxComposePartCount  = 10000    // ComposeObject 源数上限
    MaxUploadSize        = int64(MaxComposePartCount) * DefaultChunkSize // ≈156.25GB
)
```

### 3.2 infra 适配器

**`internal/infra/storage/minio.go`** 追加 `Compose`（**无 contentType 参数**——评审
修正 B2：多源路径忽略 ContentType，传了也是无效操作）：

```go
func (m *minioObjectStore) Compose(ctx context.Context, bucket, dstKey string, srcKeys []string) error {
    srcs := make([]minio.CopySrcOptions, len(srcKeys))
    for i, k := range srcKeys {
        srcs[i] = minio.CopySrcOptions{Bucket: bucket, Object: k}
    }
    _, err := m.client.ComposeObject(ctx, minio.CopyDestOptions{Bucket: bucket, Object: dstKey}, srcs...)
    return err
}
```

- 5MiB/10000 约束由服务端校验兜底（§3.3）；ComposeObject 失败（小片）返回错误，
  complete 接口透传。
- 注意：ComposeObject 内部 Stat 阶段不携带 ctx（api-compose-object.go:438），
  不要依赖 ctx 取消中断该阶段（可接受）。
- 分片对象 key：`objectKey(projectID, bucketID, fileID) + "/chunks/{partNumber:03d}"`。

**`internal/pkg/testutil/memstore.go`** 追加 `Compose`（按序拼接字节写 dstKey，无
5MiB/10000 约束——测试语义）。

**`internal/infra/storage/redis_upload_session.go`**（新建）：

- Redis 结构：`torchwood:upload:{uploadID}` Hash（全部元数据字段，metadata/
  permissions JSON 序列化）+ Set `torchwood:upload:{uploadID}:parts`（已收分片）；
  TTL 24h，**Create 与 MarkChunk 都刷新 EXPIRE**。
- `MarkChunk`：pipeline `SADD parts partNumber` + `EXPIRE`；**EXPIRE 前 EXISTS
  检查**（评审 M5：会话已删时 SADD 会重建无 TTL 的孤儿 key）。
- `LockComplete`：`SET {uploadID}:lock 1 EX 300 NX`；`UnlockComplete`：DEL。
- `Get`：HGETALL + SMEMBERS 还原；不存在 → `nil, nil`。
- 测试：miniredis（参照 `internal/infra/queue/redis_queue_test.go`）。
- provider：`internal/infra/storage/provides.go` 注册
  `NewRedisUploadSessionStore(rdb)` 直接返回接口（**明确选直接返回接口，不加
  wire.Bind**——FunctionRepo 先例）。

### 3.3 use-case（新建 `internal/app/storage/uploads.go`）

**方法签名（评审修正 B1——databases.Principal 无 ProjectID/UserID，必须由 handler
传入）**：

```go
type CreateUploadCommand struct {
    ProjectID, BucketID, Name, MimeType string
    Size        int64
    Metadata    map[string]string
    Permissions []string
    OwnerUserID string // handler 传 principal.UserID
}

func (s *Storage) CreateUploadSession(ctx context.Context, cmd CreateUploadCommand, principal databases.Principal) (*storage.UploadSession, error)
func (s *Storage) GetUploadSession(ctx context.Context, projectID, uploadID string, principal databases.Principal) (*storage.UploadSession, error)
func (s *Storage) UploadChunk(ctx context.Context, projectID, uploadID string, partNumber int, content io.Reader, size int64, principal databases.Principal) error
func (s *Storage) CompleteUpload(ctx context.Context, projectID, uploadID string, ownerUserID string, principal databases.Principal) (*storage.File, error)
func (s *Storage) AbortUpload(ctx context.Context, projectID, uploadID string, principal databases.Principal) error
```

**CreateUploadSession**：
1. 校验 bucket_id/name/size>0（`InvalidArgument`）；bucket 存在校验（GetDocument，
   参照 CreateFile :188-220）。
2. **上限校验**：`size > MaxUploadSize` → `InvalidArgument` "file too large"
   （评审 I3，part_count ≤ 10000）。
3. `chunkSize = DefaultChunkSize`；`partCount = ceil(size/chunkSize)`。
4. **mime 归一化**（评审 I4）：`mimeType = normalizeMimeType(cmd.MimeType)` 存入会话
   （防存储型 XSS 绕过）。
5. 生成 `uploadID` 与预生成 `fileID`（idgen.UUID）。
6. `s.uploads.Create(ctx, session)`。
7. 返回会话（含 part_count/chunk_size/expires_at）。

**UploadChunk**：
1. 会话存在且未过期（TTL 自动；GET 后校验 expires_at）→ 不存在 `NotFound`
   "upload session not found or expired"。
2. **项目归属**：`session.ProjectID != projectID` → `PermissionDenied`（纵深防御）。
3. `partNumber < 1 || partNumber > partCount` → `InvalidArgument`。
4. **分片大小严格校验（评审 I2）**：
   - `size > MaxChunkSize` → `InvalidArgument` "chunk exceeds maximum size"；
   - `partNumber < partCount && size != chunkSize` → `InvalidArgument`
     "chunk size must equal chunk_size for non-final parts"（保证
     sum(parts)==size 可推导）；
   - `partNumber == partCount && (size < 1 || size > chunkSize)` → `InvalidArgument`
     （最后一片 1..chunkSize；注意 `size < MinComposePartSize` 的**最后一片**合法——
     5MiB 约束仅对非末片）。
5. `s.store.Put(ctx, bucket, chunkKey, content, size, "application/octet-stream")`
   （同号覆盖 = 幂等）。
6. `s.uploads.MarkChunk(ctx, uploadID, partNumber)`。

**CompleteUpload**：
1. 会话存在 + 项目归属校验。
2. **complete 互斥锁（评审 B3）**：`LockComplete` 失败 → `FailedPrecondition`
   "upload is already being completed"；`defer UnlockComplete`。
3. 校验全部片已收：`len(received) == partCount` 且每号在 1..partCount → 缺片
   `FailedPrecondition` "missing chunks: [列表]"（**续传关键**；锁在缺片时释放，
   会话保留可继续传）。
4. `s.store.Compose(ctx, bucket, objectKey(projectID, bucketID, fileID),
   chunkKeys...)`（按 1..partCount 序）。
5. 建文件文档（size=session.Size，mime=session.MimeType 已归一化）：
   复用 `filePermissions(fileID, ownerUserID, session.Permissions)` +
   `CreateDocument` 写 files 集合。
6. **回滚（评审 B3 修正）**：文档创建失败（含 `ErrDuplicateKey` 并发场景）时——
   **先二次确认会话仍存在（`Get` 非 nil）再删最终对象**；锁保护下并发 complete
   已被互斥，二次确认是双保险。
7. 删暂存分片对象（逐片 `store.Delete`，失败仅记日志）+ `s.uploads.Delete` +
   `UnlockComplete`（在会话删除之后）。
8. 返回 `*storage.File`。

**AbortUpload**：项目校验 → 删会话 → 清理全部暂存分片对象（按 partCount 循环
Delete，幂等）。

**主键/密钥**：`chunkKey(projectID, bucketID, fileID, part)` =
`fmt.Sprintf("%s/chunks/%03d", objectKey(projectID, bucketID, fileID), part)`。

### 3.4 HTTP handler（`internal/api/serverhttp/file_handler.go` 扩展）

`Register` 追加 5 个路由：

```go
_ = mux.HandlePath("POST", "/v1/storage/buckets/{bucketId}/uploads", h.createUpload)          // JSON body
_ = mux.HandlePath("GET", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}", h.getUpload)   // 续传查询
_ = mux.HandlePath("POST", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}/chunks/{partNumber}", h.uploadChunk) // multipart 字段 chunk
_ = mux.HandlePath("POST", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}/complete", h.completeUpload)
_ = mux.HandlePath("DELETE", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}", h.abortUpload)
```

- **createUpload**：JSON body `{name, mime_type, size, metadata?, permissions?}`，
  `MaxBytesReader(1<<20)`（防超大 metadata）→ `CreateUploadSession` →
  201 `{upload_id, file_id, chunk_size, part_count, expires_at}`。
- **getUpload**：返回 `{upload_id, part_count, received: [1,3,...], chunk_size}`。
- **uploadChunk**：**`MaxBytesReader(w, r.Body, MaxChunkSize+1<<20)`**（评审 I1：
  multipart 边界/头部开销，16MiB 整片 + 缓冲；use-case 仍按 size 严格拒绝）+
  `ParseMultipartForm(1<<20)` → 字段 `chunk` → `UploadChunk` → 200
  `{part_number, received_count}`。
- **completeUpload**：`CompleteUpload` → 200 文件 JSON（与 `createFile` 响应同构）。
- **abortUpload**：`AbortUpload` → 204。
- 分片号解析：`strconv.Atoi`，失败 → `InvalidArgument`。
- 鉴权后校验 `bucketId` 与会话 `bucket_id` 匹配（纵深防御）。
- 日志：**uploadChunk 成功不记 logOp（防 64 片 64 条噪音），失败仍记**（评审 M6）；
  create/complete/abort 保持现有 logOp 模式。
- 顺手修正：现有 `maxUploadBytes`（:113/:136）同有"整 100MiB 被拒"缺陷，本方案
  对 upload 路径一并改为 `maxUploadBytes + 1<<20`（或另行处理，见实现时决定）。

### 3.5 Console

**`console/src/api/storage.ts`** 追加：

```ts
createUploadSession(bucketId, {name, mime_type, size, metadata?}): Promise<UploadSession>
getUploadSession(bucketId, uploadId): Promise<{upload_id, part_count, received: number[], chunk_size}>
uploadChunk(bucketId, uploadId, partNumber, blob: Blob): Promise<{part_number, received_count}>
completeUpload(bucketId, uploadId): Promise<FileItem>
abortUpload(bucketId, uploadId): Promise<void>
const CHUNK_SIZE = 16 * 1024 * 1024; // 与后端 DefaultChunkSize 一致
```

**`console/src/routes/storage/pages.tsx`** 的 BucketDetailPage 上传改造：

- 新组件 `ChunkedUploader`：`file.size > CHUNK_SIZE` 走分片流程：
  1. **uploadId 存 localStorage**（评审 M7：键含 bucketId+fileName+size，create 时
     写入、complete/abort 时清除——页面刷新后可续传）；
  2. 已有 uploadId → `getUploadSession` 拿 `received`，跳过已收分片；
  3. `for part in 1..part_count`（跳过 received）：`file.slice((part-1)*chunkSize,
     part*chunkSize)` → `uploadChunk(part, blob)`，更新进度（`part/part_count`）；
  4. 失败：中断，提示"已上传 X/N 片，可重试续传"（保留 uploadId）；
  5. 全部完成 → `completeUpload` → 刷新文件列表 + 清除 localStorage。
- 进度 UI：新增 `console/src/components/ui/progress.tsx`（简单 div 宽度百分比）。
- ≤16MiB 文件保持现有 `uploadFile` 单次上传。
- 文件 > MaxUploadSize（156.25GB）前端拒绝（评审 I3 同步）。

### 3.6 鉴权与 scope

- 全部走 serverhttp 自定义 handler：**createUpload/uploadChunk/complete/abort 复用
  `interceptor.StorageServiceCreateFile`（storage.write）；getUpload 因 GET 分支
  自动要求 `StorageServiceGetFile`（storage.read）**（评审 I7 修正——file_handler.go
  :426-434 按方法分支）。
- 项目归属：handler `projectID(r, principal)` 取项目，经 Command 参数传入 use-case
  二次校验。

---

## 4. 实现顺序（建议）

| 步骤 | 内容 | 验证 |
|------|------|------|
| 1 | 领域端口：ObjectStore.Compose + UploadSession/Store + 常量；memstore.Compose | `go build ./...` |
| 2 | infra：minio Compose、redis_upload_session + miniredis 测试；**NewStorage 签名加 uploads 参数 + `task wire-all` + `go build ./...`**（评审 I8） | infra 单测 |
| 3 | use-case：uploads.go 五方法 + 集成测试 | `go test ./internal/app/storage/...` |
| 4 | HTTP handler：5 路由 + 集成测试 | `go test ./internal/api/serverhttp/...` |
| 5 | **CI：backend job 加 minio service + minio 真实集成测试**（评审 I6，见 §5） | CI 绿 |
| 6 | Console：api + ChunkedUploader + progress 组件 | `task console-build` |
| 7 | 全量验证 + roadmap/developer 文档更新 | 见 §5 |

每步完成跑 `gofmt -l .`（必须空）+ `go vet ./...`。

---

## 5. 测试与验证

- **session store**（`internal/infra/storage/redis_upload_session_test.go`，miniredis）：
  Create/Get 往返（metadata/permissions 还原）、MarkChunk 幂等 + **TTL 刷新**、
  `mr.FastForward(25h)` 过期、Delete、**LockComplete 互斥（第二次 false）与释放**。
- **use-case 集成**（`internal/app/storage/uploads_integration_test.go`，MemObjectStore +
  真实 docDB）：
  - 完整流程：12MiB 文件 2 片（各 6MiB，满足 ≥5MiB 测试语义）→ complete →
    GetFile 内容一致 + 文档 mime 归一化（传 `text/html` → 会话存
    `application/octet-stream`）；
  - 缺片 complete → FailedPrecondition（含缺失片列表）；补片后 complete 成功
    （**续传路径**）；
  - 分片号越界/超大 chunk/**非末片 size != chunkSize** → InvalidArgument；
  - size > MaxUploadSize → InvalidArgument；
  - 会话过期/不存在 → NotFound；AbortUpload 后 Get nil；
  - 同号重复上传幂等；
  - **complete 互斥**：先 LockComplete 再调 CompleteUpload → FailedPrecondition。
- **HTTP 集成**（`internal/api/serverhttp/file_handler_integration_test.go` 追加）：
  - 全流程（API key）：create → 2 片 → get（received 正确）→ complete →
    download 内容一致；
  - 无 storage scope 的 key：create → 403；**只读 scope（storage.read）key：
    getUpload 200、uploadChunk 403**（评审 I7 断言）；
  - 超大 chunk（>16MiB+缓冲）→ 拒绝；缺失分片 complete → **400**
    （grpc-gateway 将 FailedPrecondition 映射为 400，非 412——评审 M1）；
  - 无凭证 → 401。
- **minio 真实集成测试**（评审 I6）：`internal/infra/storage/minio_integration_test.go`：
  `TORCHWOOD_TEST_MINIO_ENDPOINT` 未设 → `t.Skip`；设了则跑：真实 ComposeObject
  合并 2×6MiB 分片 → GetObject 校验内容与字节数、5MiB 约束（1MiB 非末片 →
  报错）、10000 上限不可测（跳过）。**CI backend job 增加 minio service**
  （`minio/minio:latest`，端口 9000，健康检查 `curl /minio/health/live`）+
  `TORCHWOOD_TEST_MINIO_ENDPOINT: http://localhost:9000`。
- **全量验证**：`go test ./...`（.env 提供 TORCHWOOD_TEST_DATABASE_SOURCE）、
  `task lint`、`task build`。

---

## 6. 范围外（明确不做）

- S3 原生多部分直传（预签名 URL / aws-sdk-go-v2）。
- 分片并发上传（MVP 顺序；ComposeObject 依赖按序合并）。
- 上传进度事件推送（P2 Realtime）。
- 服务端强制每片 ≥5MiB 的上传阶段校验（大小校验已保证非末片 == 16MiB ≥ 5MiB；
  手工小片由 ComposeObject 报错兜底）。
- **孤儿分片对象清理**：complete/abort 过程中删除失败或与上传并发产生的孤儿分片
  对象、以及 DeleteBucket 后残留的会话分片（评审 I5 声明——MVP 由未来后台清理
  任务覆盖，P2）。
- 分片大小/会话 TTL 配置化（MVP 常量）。

---

## 7. 关键坑（实现时必须注意）

1. **minio-go v7 无低层多部分 API**：合并只能用 `ComposeObject`；不要尝试
   NewMultipartUpload（v7 不存在，编译失败）。
2. **ComposeObject 无法设置目标 Content-Type**（多源路径忽略）——**验收标准 1 的
   mime 以文档为准**，不要试图传 contentType 参数（无效）。
3. **use-case 签名必须带 projectID/ownerUserID 参数**（databases.Principal 无
   ProjectID/UserID，handler 解析传入）——按 §3.3 签名实现，不要自造
   "projectIDFromPrincipal"。
4. **complete 互斥锁**：LockComplete（SETNX 5min）防并发 complete 数据丢失；
   回滚删对象前二次确认会话存在。
5. **分片大小严格校验**：非末片 == chunkSize（否则 sum(parts) != size 文档失真）。
6. **MaxBytesReader 缓冲**：`MaxChunkSize + 1<<20`（multipart 开销）；`maxUploadBytes`
   同病顺手修。
7. **part_count ≤ 10000**（size ≤ 156.25GB）——CreateUploadSession 前置拒绝。
8. **mime 归一化**：会话 mime 在 CreateUploadSession 时 normalize（安全一致）。
9. **会话 Redis TTL**：Create/MarkChunk 都刷新 EXPIRE；MarkChunk 前 EXISTS 检查
   （防孤儿 key）。
10. **分片 key 命名**：`{objectKey}/chunks/{part:03d}`——与最终 key 不冲突。
11. **complete 时序**：Lock → 校验缺片 → Compose → 建文档 → 删分片 → 删会话 →
    Unlock；任一步失败按 §3.3 回滚规则（Compose 失败不删任何东西可重试；
    建文档失败二次确认后删最终对象）。
12. **FailedPrecondition HTTP 映射为 400**（非 412）——测试断言写 400。
13. **uploadChunk 成功不记 logOp**（防噪音），失败记。
14. **NewStorage 签名变化**：`task wire-all` 重生成 wire_gen（含 NewFileHandler
    调用链）。
15. **getUpload scope 是 storage.read**（GET 分支）——测试按此断言。
16. **CI minio service**：加进 backend job；minio 集成测试
    `TORCHWOOD_TEST_MINIO_ENDPOINT` 未设跳过（本地 compose 有 minio 可直接跑）。
