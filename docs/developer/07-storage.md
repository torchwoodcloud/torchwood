# 07 存储：桶、文件与对象

面向后端开发者：桶 / 文件元数据模型、对象存储适配器、multipart 直传、分片上传、File Token 与安全输出。

> 源码锚点：`internal/domain/storage/`、`internal/infra/storage/`、`internal/app/storage/`、`internal/api/serverhttp/file_handler.go`、`proto/server/v1/storage.proto`。

## 1. 架构

```
HTTP multipart/file_handler ─┬→ app/storage.Storage ─┬→ bun tw_<project>.buckets/files（projectschema）
gRPC StorageService (gateway)─┘                        └→ domain/storage.ObjectStore（minio-go，S3/MinIO）
```

- **元数据与对象分离**：`buckets` / `files` 是项目数据面 `tw_<project>` 的 bun 静态表，无 `_id` / `_perms` / `_version`，不走 DocumentDB。
- `public.provider_resource_index` 是跨表资源索引（public 控制面），桶 / 文件元数据变更时同步写入。
- 对象键 `objectKey = <projectID>/<bucketID>/<fileID>`，所有对象落在同一个 S3 bucket（`storage.s3.bucket`，未配置时回退 `torchwood-files`——S3 要求全小写）。
- `ObjectStore.List` 前缀扫描用于桶删除与孤儿分片清理。

## 2. 适配器端口

`internal/domain/storage/object.go` 定义 `ObjectStore` 端口，唯一实现在 `internal/infra/storage/minio.go`（`minio.New(endpoint, StaticV4, Secure, Region)`；endpoint 含 `https://` 时自动启用 SSL）：

| 方法 | 语义 |
|---|---|
| `EnsureBucket(name)` | `BucketExists` → `MakeBucket(us-east-1)`，幂等 |
| `Put(bucket,key,r,size,ct)` | `PutObject`，缺省 content type `application/octet-stream` |
| `Get(bucket,key)` | `GetObject` + `Stat` 校验，`NoSuchKey` → not found |
| `Delete(bucket,key)` | `RemoveObject` |
| `Compose(bucket,dst,srcs)` | `ComposeObject` 按序合并（除末片外每片 ≥5MiB，≤10000 源；目标 content type 固定 `octet-stream`） |
| `List(bucket,prefix)` | `ListObjects(Recursive:true)`，返回 `ObjectMeta{Key,LastModified}` |
| `Ping()` | `BucketExists` 探测 |

## 3. 配置

| 路径 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `storage.s3.endpoint` | `TORCHWOOD_STORAGE_S3_ENDPOINT` | — | 含 `http(s)://host:port` |
| `storage.s3.region` | `TORCHWOOD_STORAGE_S3_REGION` | `us-east-1` | MakeBucket region |
| `storage.s3.bucket` | `TORCHWOOD_STORAGE_S3_BUCKET` | `torchwood-files` | 单桶承载全项目 |
| `storage.s3.access_key_id` | `TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID` | — | |
| `storage.s3.secret_access_key` | `TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY` | — | |
| `storage.s3.use_ssl` | `TORCHWOOD_STORAGE_S3_USE_SSL` | `false` | https scheme 可覆盖 |

`storage.provider: local` 仅占位，未实现。

## 4. 元数据与权限

`tw_<project>.buckets` 列：`id(PK)` / `name` / `permissions(JSONB,已废弃)` / `public` / `created_at` / `updated_at`。`buckets.permissions` 读路径已忽略，仅为兼容旧数据保留。

`tw_<project>.files` 列：`id(PK)` / `bucket_id` / `name` / `mime_type` / `size` / `metadata(JSONB)` / `owner_user_id(nullable)` / `created_at` / `updated_at`。`OwnerUserID` 由 principal 派生（取首个 `user:<id>` 且含 `users` 角色的角色）；API Key 与 Admin 主体不归属（留空）。

文件访问判定（`canAccessFile` / `isStoragePrivileged`）：

1. `bucket.Public==true` → 均可访问（匿名 `GuestPrincipal` 需 `?project=` 参数或 File Token）。
2. `System` / `PlatformAdmin` / `keys` / `owner|admin|member|viewer` 角色 → 特权放行。
3. 其余 `EndUser` 仅当 `file.OwnerUserID == uid` 时可访问。
4. `ListFiles` 在私有桶对 `EndUser` 仅返回自有文件；公有桶返回全部。

MIME 归一化（`normalizeMimeType`）：空值与 `text/html`、`application/xhtml+xml`、`application/javascript`、`text/javascript`、`application/xml`、`text/xml`（取 `;` 前的 base 类型）一律改判 `application/octet-stream`——防存储型 XSS。

## 5. API

### 5.1 gRPC StorageService（ACCESS_SERVER 默认）

| RPC | HTTP | 说明 |
|---|---|---|
| `CreateBucket` | `POST /v1/server/storage/buckets` | `RequireServerPrincipal`；`permissions` 已废弃但仍落库兼容 |
| `ListBuckets` | `GET /v1/server/storage/buckets` | `shared.v1.ListRequest` → AIP-158 分页（默认 25） |
| `GetBucket` | `GET /v1/server/storage/buckets/{id}` | |
| `UpdateBucket` | `PATCH /v1/server/storage/buckets/{id}` | `optional name` / `public` |
| `DeleteBucket` | `DELETE /v1/server/storage/buckets/{id}` | 分页删文件对象 + 元数据，再前缀 List + Delete 清残留分片 |
| `CreateFile` | `POST /v1/server/storage/buckets/{bucket_id}/files` | gRPC body `bytes data`；`permissions` 字段已 deprecated |
| `ListFiles` | `GET /v1/server/storage/buckets/{bucket_id}/files` | 过滤仅限权限后内存分页；`queries` 携带即 InvalidArgument 显式拒绝 |
| `GetFile` | `GET .../files/{file_id}` | 仅元数据 |
| `UpdateFile` | `PATCH .../files/{file_id}` | `optional name` / `mime_type` + `metadata` 整体替换 |
| `DeleteFile` | `DELETE .../files/{file_id}` | 删对象 + 行 |
| `CreateFileToken` | `POST .../files/{file_id}/tokens` | 显式 `method_auth`（member + admin_roles + storage.write scope——敏感方法不依赖服务级默认） |
| `GetStorageUsage` | `GET /v1/server/storage/usage` | `{buckets, files, total_size}`（`SUM(size)`，不走 DocumentDB 权限过滤） |

共 12 个 RPC（桶 5 + 文件 5 + Token / Usage 2）。

### 5.2 HTTP 直传（multipart）

`internal/api/serverhttp/file_handler.go`：

| 方法 | 路径 | 限制 | 鉴权 |
|---|---|---|---|
| `POST /v1/storage/buckets/{bucketId}/files` | multipart `file` 字段 | 100MiB + 1MiB 缓冲，`ParseMultipartForm(32MiB)` | EndUser / `storage.write` scope / admin `member` + 项目绑定；写审计 |
| `GET .../files/{fileId}/download` | `Content-Disposition: attachment` | | 同 view |
| `GET .../files/{fileId}/view` | 安全 MIME 内联（§6） | | 凭证 → File Token → `public + ?project=` → `GuestPrincipal`（`resolveReadContext` 优先级） |
| `GET .../files/{fileId}/preview` | 缩略图 | | 同 view |

### 5.3 分片上传

常量：`DefaultChunkSize = MaxChunkSize = 16MiB`、会话 TTL 24h、`MinComposePartSize = 5MiB`、`MaxComposePartCount = 10000`、单文件上限约 156.25GB。

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST /v1/storage/buckets/{bucketId}/uploads` | 建会话 `body{name,mime_type,size,metadata?}`（1MiB 上限） | 返回 `{upload_id, file_id, chunk_size, part_count, expires_at}` |
| `GET .../uploads/{uploadId}` | 断点续传查询，返回 `{received:[1,3,...]}` | 需 `storage.read` |
| `POST .../uploads/{uploadId}/chunks/{partNumber}` | 上传分片（multipart `chunk`，16MiB + 1MiB 上限；非末片必须等于 chunkSize） | 同号覆盖幂等 |
| `POST .../uploads/{uploadId}/complete` | 校验缺片 → Compose → 写 files → 删分片 → 删会话 | `SETNX torchwood:upload:{id}:lock EX 300` 互斥；重复 complete → FailedPrecondition |
| `DELETE .../uploads/{uploadId}` | 删会话 + 分片对象 | 204 |

会话状态存 Redis：`torchwood:upload:{id}` Hash + `:parts` Set，Create / MarkChunk 刷新 24h TTL（`EXISTS` 防孤儿）；分片对象键 `{project}/{bucket}/{file}/chunks/{part:03d}`。HTTP 鉴权与 gRPC 口径对齐。Console 的 `ChunkedUploader` 组件自动分片、显示进度条，并以 localStorage 存 `uploadId` 支持续传。

## 6. 安全输出与预览

- 响应加固：`X-Content-Type-Options: nosniff` + `CSP: default-src 'none'; sandbox`。
- **内联白名单**（`inlineSafeMime`）：`image/{png,jpeg,gif,webp,avif,svg+xml}`、`text/plain`、`application/pdf`、`video/*`、`audio/*`。SVG 强制附件；`/download` 恒为附件。文件名经 `safeFilename` 清理控制字符，双编码输出 `filename` + `filename*=UTF-8''`。
- **缩略图**（`disintegration/imaging`）：仅 `png/jpeg/gif/webp` 源；源 >50MiB 拒绝；`?width=&height=` ≤4096，`imaging.Fit(Lanczos)`；webp 源转 JPEG 输出；无参回源。`Cache-Control: public, max-age=86400`。

## 7. File Token（短 TTL 临时授权）

- **签发** `POST .../tokens`：`expires_in` 缺省 15min、上限 1h；签发前先过 `canAccessFile`。
- **格式** `{expiresAt}.{projectID}.{bucketID}.{fileID}.{hex(hmac)}`。HMAC 密钥由 `security.jwt.secret` 经 `jwtparser.DeriveKey(..., PurposeFileToken)` 域分离派生（与其他用途域不通用）。
- **校验** `ParseFileToken`：段数 / 过期 / `hmac.Equal`，失败 Unauthenticated。使用方可拼 `?token=` 到下载 URL，服务端比对 token 内路径与请求路径一致性。
- Token 也可经响应头 `X-Torchwood-File-Token` 携带，与 query 参数二选一。

## 8. 超时与清理

- app 层每条 DB 语句 `context.WithTimeout(10s)`；审计插入 3s（`WithoutCancel`）。
- `DeleteBucket` 双重清理 + 前缀 List 兜底；worker 的 `ChunkCleaner` 每小时扫描 `.../chunks/` 前缀下 `LastModified > 48h` 的孤儿分片（24h TTL 的 2 倍余量）并删除。
- bunrepo 侧统一 5s / 10s per-statement deadline，防单条慢查询卡住网关；对象存储侧无额外重试，Compose 失败直接透传（分片 5MiB 校验失败由服务端返回 InvalidArgument）。
- 上传 / 下载 / Token 校验均输出结构化 access log（`actor_id/kind/project_id/ip`，与 gRPC 审计同 trusted_proxies 规则）；`logOp` 仅对创建 / 上传 / 删除计审计。

## 9. 测试

- `internal/api/serverhttp/file_handler_integration_test.go` + `file_handler_uploads_test.go`：端到端（multipart、download / view / preview、Token、public 匿名、分片全流程与 scope 校验）。
- `internal/app/storage/*_integration_test.go`：用例层（桶 CRUD、文件更新、usage 聚合、分片互斥 / 续传）。
- `internal/infra/storage/redis_upload_session_test.go`（miniredis 往返 / TTL / 锁）与 `minio_integration_test.go`（真实 ComposeObject；`TORCHWOOD_TEST_MINIO_ENDPOINT` 未设跳过）。
- 均经 `internal/pkg/testutil/db.go:SetupTestDB` 建隔离库。

## 10. 已知边界

- 单 S3 bucket 多租户键隔离；`storage.provider: local` 占位未实现。
- 分片上传仅服务于 HTTP 路径；`StorageService.CreateFile` 的 gRPC `bytes data` 通道 ≤8MiB（网关 MaxBytes 限制），大对象走 multipart / 分片。
- `UpdateFile` 的 `metadata` 为整体替换（"map 空值不修改"语义若未来 optional 化属破坏性变更）。

## 相关文档

- `06-databases.md` — 三层 schema 与静态表定位
- `05-authentication.md` — API Key scope（`storage.write` / `storage:<bucket_id>` 实例限定）
- `docs/implementation-storage-chunked-upload.md` — 分片上传设计细节与偏离说明
