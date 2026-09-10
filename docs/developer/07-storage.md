# 07 存储：桶、文件与对象

> 面向后端开发者：桶/文件元数据、对象存储适配、分片上传、File Token 与安全输出。
> 源码：`internal/domain/storage/`、`internal/infra/storage/`、`internal/app/storage/`、`internal/api/serverhttp/file_handler.go`、`proto/server/v1/storage.proto`。

## 1 架构

```
HTTP multipart/file_handler ─┬→ app/storage.Storage ─┬→ bun tw_<project>.buckets/files（projectschema）
gRPC StorageService (gateway)─┘                        └→ domain/storage.ObjectStore (minio-go, S3/MinIO)
```

- 元数据与对象分离：`buckets`/`files` 是项目数据面 `tw_<project>` 的 bun 静态表（`internal/infra/bun/model/buckets.go:12`/`files.go:11`，`internal/infra/projectschema/`），无 `_id`/`_perms`/`_version`，不走 DocumentDB。
- `public.provider_resource_index` 为跨表资源索引（public 控制面）。
- 对象键 `objectKey = <projectID>/<bucketID>/<fileID>`（`internal/app/storage/storage.go:576`），所有对象落在同一 S3 bucket（`storage.s3.bucket`，未配时 `storage.DefaultBucketName="torchwood-files"`，见 `internal/domain/storage/object.go:50`）。
- `List` 前缀扫描用于桶删除与孤儿分片清理（`ObjectStore.List`）。

## 2 适配器端口

`internal/domain/storage/object.go:52` `ObjectStore`：

| 方法 | 语义 |
|---|---|
| `EnsureBucket(name)` | `BucketExists`→`MakeBucket(us-east-1)` 幂等 |
| `Put(bucket,key,r,size,ct)` | `PutObject`，缺省 `application/octet-stream` |
| `Get(bucket,key)` | `GetObject` + `Stat` 校验，`NoSuchKey→not found` |
| `Delete(bucket,key)` | `RemoveObject` |
| `Compose(bucket,dst,srcs)` | `ComposeObject` 按序合并（`MinComposePartSize=5MiB` 除末片、≤10000 源；目标 CT 固定 `octet-stream`） |
| `List(bucket,prefix)` | `ListObjects(Recursive:true)` 返回 `ObjectMeta{Key,LastModified}` |
| `Ping()` | `BucketExists` 探测 |

唯一实现 `internal/infra/storage/minio.go:22` `NewMinioObjectStore`：`minio.New(endpoint, StaticV4, Secure, Region)`，endpoint 含 `https://` 时自动 `useSSL=true`，bucket 未配回退 `torchwood-files`（S3 要求全小写）。

## 3 配置

`internal/pkg/config/config.proto` `Storage.S3`（`internal/pkg/config/bind.go` 映射）：

| 路径 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `storage.s3.endpoint` | `TORCHWOOD_STORAGE_S3_ENDPOINT` | — | 含 `http(s)://host:port` |
| `storage.s3.region` | `TORCHWOOD_STORAGE_S3_REGION` | `us-east-1` | `MakeBucket` region |
| `storage.s3.bucket` | `TORCHWOOD_STORAGE_S3_BUCKET` | `torchwood-files` | 单桶承载全项目 |
| `storage.s3.access_key_id` | `TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID` | — |  |
| `storage.s3.secret_access_key` | `TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY` | — |  |
| `storage.s3.use_ssl` | `TORCHWOOD_STORAGE_S3_USE_SSL` | `false` | https scheme 可覆盖 |

`storage.provider: local` 仅占位。

## 4 元数据与权限

`tw_<project>.buckets` 列：`id(PK)`/`name`/`permissions(JSONB,已废弃)`/`public`/`created_at`/`updated_at`。`buckets.permissions` 读路径忽略（`internal/app/storage/storage.go:50` 注释 A8），仅兼容旧数据。

`tw_<project>.files` 列：`id(PK)`/`bucket_id`/`name`/`mime_type`/`size`/`metadata(JSONB)`/`owner_user_id(nullable)`/`created_at`/`updated_at`。`OwnerUserID` 由 `principal` 派生（`storageEndUserID` 取首个 `user:<id>` 且含 `users` 角色）；API Key/Admin 为空（不归属）。

权限（`canAccessFile`/`isStoragePrivileged`）：

- `bucket.Public==true` → 均可（匿名 `GuestPrincipal` 需 `?project=` 或 File Token）。
- `System`/`PlatformAdmin`/`keys`/`owner|admin|member|viewer` → 特权放行。
- 否则 `EndUser` 仅当 `file.OwnerUserID==uid`。
- `ListFiles` 私有桶对 `EndUser` 仅返回自有文件（`storage.go:354`）；公有桶返回全部。

MIME 归一化（`normalizeMimeType`）：`""` 与 `text/html`、`application/xhtml+xml`、`application/javascript`、`text/javascript`、`application/xml`、`text/xml`（取 `;` 前 base）改判 `application/octet-stream`。

## 5 API

### 5.1 gRPC `StorageService`（`proto/server/v1/storage.proto:57`，`ACCESS_SERVER` 默认）

| RPC | HTTP | 说明 |
|---|---|---|
| `CreateBucket` | `POST /v1/server/storage/buckets` | `RequireServerPrincipal`，`permissions` 废弃仍落库 |
| `ListBuckets` | `GET /v1/server/storage/buckets` | `shared.v1.ListRequest` → `ListBucketsResponse{buckets,meta}`（AIP-158，`crud.DecodePageToken`，默认 25） |
| `GetBucket` | `GET /v1/server/storage/buckets/{id}` |  |
| `UpdateBucket` | `PATCH /v1/server/storage/buckets/{id}` | `optional name/public` |
| `DeleteBucket` | `DELETE /v1/server/storage/buckets/{id}` | 分页删文件对象+元数据，再前缀 `List+Delete` 清残留分片 |
| `CreateFile` | `POST /v1/server/storage/buckets/{bucket_id}/files` | gRPC body `bytes data`；`permissions` `deprecated` 已废弃 |
| `ListFiles` | `GET /v1/server/storage/buckets/{bucket_id}/files` | `page_size/page_token`（过滤仅限权限后内存分页；`queries` 携带即 `InvalidArgument` 显式拒绝） |
| `GetFile` | `GET /v1/server/storage/buckets/{bucket_id}/files/{file_id}` | 仅元数据 |
| `UpdateFile` | `PATCH .../files/{file_id}` | `optional name/mime_type` + `metadata` 整体替换 |
| `DeleteFile` | `DELETE .../files/{file_id}` | 删对象 + 行 |
| `CreateFileToken` | `POST .../files/{file_id}/tokens` | 显式 `method_auth`（member+admin_roles + storage.write scope，敏感方法不依赖服务级默认） |
| `GetStorageUsage` | `GET /v1/server/storage/usage` | `{buckets,files,total_size}`（`SUM(size)`，不走 DocumentDB 权限过滤） |

`StorageService` 共 12 个 RPC（桶 5 + 文件 5 + Token/Usage 2）；per-statement 超时见 §8。

### 5.2 HTTP 直传（`internal/api/serverhttp/file_handler.go`）

| 方法 | 路径 | 限制 | 鉴权 |
|---|---|---|---|
| `POST /v1/storage/buckets/{bucketId}/files` | multipart `file` 字段 | 100MiB+1MiB 缓冲，`ParseMultipartForm(32MiB)` | EndUser / `storage.write` scope / admin `member`+项目绑定；写 `logOp` 审计（`audit.Repository` 时 `Insert` 3s 超时） |
| `GET .../files/{fileId}/download` | `Content-Disposition: attachment` |  | 同 view |
| `GET .../files/{fileId}/view` | 安全 MIME 内联（见 §6） |  | 凭证→File Token→`public+?project=`→`GuestPrincipal`（`resolveReadContext` 优先级） |
| `GET .../files/{fileId}/preview` | 缩略图 |  |  |

### 5.3 分片上传（`internal/domain/storage/upload_session.go`）

常量：`DefaultChunkSize=MaxChunkSize=16MiB`、`UploadSessionTTL=24h`、`MinComposePartSize=5MiB`、`MaxComposePartCount=10000`、`MaxUploadSize≈156.25GB`。

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST /v1/storage/buckets/{bucketId}/uploads` | 建会话 `body{name,mime_type,size,metadata?}`（1MiB 上限） | 返回 `{upload_id,file_id,chunk_size,part_count,expires_at}` |
| `GET .../uploads/{uploadId}` | 断点续传 `{received:[1,3,...]}` | 需 `storage.read` |
| `POST .../uploads/{uploadId}/chunks/{partNumber}` | 上传分片 `multipart chunk`（16MiB+1MiB 上限，非末片必须==chunkSize） | 同号覆盖幂等 |
| `POST .../uploads/{uploadId}/complete` | 校验缺片→`Compose`→写 `files`→删分片→删会话 | `SETNX torchwood:upload:{id}:lock EX 300` 互斥，重复→`FailedPrecondition` |
| `DELETE .../uploads/{uploadId}` | 删会话+分片对象 | 204 |

Redis：`torchwood:upload:{id}` Hash + `:parts` Set，`Create/MarkChunk` 刷新 24h TTL（`EXISTS` 防孤儿）；分片对象键 `{project}/{bucket}/{file}/chunks/{part:03d}`。鉴权 `FileHandler.authorize` 对齐 gRPC（A9/C5）。Console `ChunkedUploader` 自动分片、进度条、`localStorage` 存 `uploadId` 续传。

## 6 安全输出与预览

- 加固：`X-Content-Type-Options: nosniff` + `CSP: default-src 'none'; sandbox`。
- 内联白名单 `inlineSafeMime`：`image/{png,jpeg,gif,webp,avif,svg+xml}`/`text/plain`/`application/pdf` + `video/*`/`audio/*`；SVG 强制附件，`/download` 恒附件；文件名 `safeFilename` 清控制字符，双编码 `filename`+`filename*=UTF-8''`。
- 缩略图（`disintegration/imaging`）：仅 `png/jpeg/gif/webp`，源>50MiB 拒绝；`?width=&height=` ≤4096，`imaging.Fit(Lanczos)`，webp 源转 JPEG；无参回源；`Cache-Control: public, max-age=86400`。

## 7 File Token（短 TTL + Header）

- 签发 `POST .../tokens`：`expires_in` 缺省 15min（`defaultFileTokenLifetime`），上限 1h（`maxFileTokenLifetime=3600`，`internal/app/storage/storage.go:451`）；需先 `canAccessFile`。
- 格式 `{expiresAt}.{projectID}.{bucketID}.{fileID}.{hex(hmac)}`（`signFileToken`），`key=fileTokenKey(master)` 由 `security.jwt.secret` 经 `jwtparser.DeriveKey(..., PurposeFileToken)` 域分离（`storage.go:532`）。
- 校验 `ParseFileToken`：段数/过期/`hmac.Equal`，失败 `Unauthenticated`；使用方可拼 `?token=` 到下载 URL，服务端比对路径一致性。
- 响应头 `X-Torchwood-File-Token` 亦可携带（见 `file_handler.go`），与 query 二选一。

## 8 超时与清理

- `internal/app/storage/cleanup.go:22` 每条 DB 语句 `context.WithTimeout(10s)`；`internal/api/serverhttp/audit.go:39` / `grpc/interceptor/audit.go:90` 审计插入 `3s`（`WithoutCancel`）。
- `DeleteBucket` 双重清理 + 前缀 `List` 兜底；`cmd/worker` `ChunkCleaner` 每小时 `List` 扫描 `.../chunks/` 且 `LastModified>48h` 的孤儿分片（24h TTL 的 2 倍余量）并删除。

## 9 补充：索引与可观测性

- `public.provider_resource_index`（`internal/infra/bun/model/provider_index.go`）为桶/文件与外部资产的统一索引，`tw_<project>` 元数据变更时同步写入，`List` 侧可经 `provider_index` 做跨项目检索（当前仅内部使用）。
- 指标：`internal/app/storage/cleanup.go` 与 `internal/infra/storage/minio.go` 的 `Put/Get/Delete/Compose` 均透传 `ctx`，上层以 `5s/10s` per-statement deadline 包装（`internal/infra/bun/bunrepo/*.go` 每方法 `context.WithTimeout(ctx,5s/10s)`），防单条慢查询卡住网关；对象存储侧无额外重试，`Compose` 失败直接透传（小片 5MiB 校验由服务端返回 `InvalidArgument`）。
- 日志：上传/下载/Token 校验均输出结构化 `access log`（`actor_id/kind/project_id/ip`，与 gRPC 审计同 `trusted_proxies` 规则），`logOp` 仅对创建/上传/删除计审计。

## 10 测试

- `internal/api/serverhttp/file_handler_integration_test.go` + `file_handler_uploads_test.go` 端到端（multipart、download/view/preview、Token、public 匿名、分片全流程/scope 校验）；
- `internal/app/storage/*_integration_test.go` 用例层（桶 CRUD、文件更新、usage 聚合、分片互斥/续传）；
- `internal/infra/storage/redis_upload_session_test.go`（`miniredis` 往返/TTL/锁）与 `minio_integration_test.go`（真实 `ComposeObject`，`TORCHWOOD_TEST_MINIO_ENDPOINT` 未设跳过）；
- 均用 `internal/pkg/testutil/db.go:SetupTestDB`（`TORCHWOOD_TEST_DATABASE_SOURCE`/`TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE`）。

## 11 已知边界

- 单 S3 bucket 多租户键隔离；`storage.provider: local` 占位未实现。
- 分片仅服务于 `file_handler` HTTP 路径，`StorageService.CreateFile` 的 gRPC `bytes data` 通道 ≤8MiB（网关 `MaxBytes`），大对象走 multipart/分片。
- `UpdateFile` 的 `metadata` 为整体替换（`map` 空值不修改语义未来 `optional` 化属破坏性变更）。

## 12 参考

- `internal/domain/storage/repository.go`：`BucketRepository`/`FileRepository`（`Count`/`SumSize`/`ListByBucket`）；`internal/domain/storage/object.go:52` 端口常量。
- `internal/infra/bun/model/buckets.go:12` / `files.go:11`：bun 表结构；`internal/infra/storage/redis_upload_session.go` 会话与锁；`internal/infra/storage/minio.go:22` 客户端构造。
- `proto/server/v1/storage.proto:57` 服务定义与 `shared.v1.ListRequest` 分页（`proto/shared/v1/common.proto:7`）。
- `AGENTS.md` §认证中间件、`docs/developer/05-authentication.md` 的 API Key scope 映射；`docs/implementation-storage-chunked-upload.md` 分片设计与偏离说明。
- 前置阅读 `06-databases.md`（三层与目录）、后续 `08-functions.md`（信号量对比）与 `09-api-guide.md`（新增存储 RPC）。
