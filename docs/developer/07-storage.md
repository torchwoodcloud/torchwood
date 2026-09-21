# 07 存储：桶、文件与对象

面向后端开发者：桶 / 文件元数据模型、对象存储适配器、multipart 直传、分片上传、File Token、安全输出，以及认证 / 限速 / 审计路径。

> 源码锚点：`internal/domain/storage/`（端口）、`internal/infra/storage/`（MinIO / Redis 适配）、`internal/app/storage/`（用例）、`internal/api/serverhttp/file_handler.go`（HTTP 直传面）、`internal/api/servergrpc/storage.go`（gRPC handler）、`proto/server/v1/storage.proto`（API 契约）。

## 1. 架构

```
HTTP multipart/file_handler ─┬→ app/storage.Storage ─┬→ bun tw_<project>.buckets/files（项目数据面静态表）
gRPC StorageService (gateway)─┘                        └→ domain/storage.ObjectStore（minio-go → S3/MinIO）
                                                        └→ domain/storage.UploadSessionStore（Redis）
```

- **元数据与对象分离**：`buckets` / `files` 是项目数据面 `tw_<project>` 的 bun 静态表（迁移 `000008` 建 `sys_*`、`000009` cut 改名为最终名），无 `_id` / `_acl` / `_version`，不走 DocumentDB 与 RLS。
- **对象键**：`objectKey = <projectID>/<bucketID>/<fileID>`；分片暂存于 `objectKey/chunks/{part:03d}`。所有项目共享同一个 S3 bucket（`storage.s3.bucket`，未配置时回退常量 `torchwood-files`——S3 桶名要求全小写，`internal/domain/storage/object.go:51`）。
- **前缀扫描**服务于三类清理：`DeleteBucket` 的残留清尾、worker 孤儿分片回收、项目删除后的 `{projectID}/` 前缀异步清空（`internal/app/server/projects.go:242-269`，经 `Purger` 端口，每桶 60s 预算 + 失败重试一次）。
- `public.provider_resource_index` 与存储面无关（仅支付 webhook 寻址使用），桶 / 文件元数据变更不写该表。

## 2. 适配器端口

`internal/domain/storage/object.go` 定义 `ObjectStore` 端口，唯一生产实现在 `internal/infra/storage/minio.go`（装配固定 MinIO/S3，见 `internal/infra/storage/provides.go`；`minio.New(endpoint, StaticV4, Secure, Region)`，endpoint 含 `https://` scheme 时强制 SSL）：

| 方法 | 语义 |
|---|---|
| `EnsureBucket(name)` | `BucketExists` → `MakeBucket(us-east-1)`，幂等；并发建桶竞态（`BucketAlreadyOwnedByYou`/`BucketAlreadyExists`）视为成功 |
| `Put(bucket,key,r,size,ct)` | `PutObject`，缺省 content type `application/octet-stream` |
| `Get(bucket,key)` | `GetObject` + `Stat` 校验；`NoSuchKey`/`NotFound` 按结构化错误码归一为 `ErrObjectNotFound` 哨兵（`errors.Is` 可区分 miss 与暂态故障） |
| `Delete(bucket,key)` | `RemoveObject` |
| `Compose(bucket,dst,srcs)` | `ComposeObject` 按序合并；除末片外每片 ≥5MiB、源数 ≤10000 由 app 层校验兜底；多源路径目标 content type 恒为 `octet-stream`（以文档 mime 为准） |
| `List(bucket,prefix)` | `ListObjects(Recursive:true)`，返回 `ObjectMeta{Key,LastModified}` |
| `Ping()` | `BucketExists` 探测 |

可选扩展接口 `PrefixStreamer.StreamPrefix`（`object.go:82-86`）流式枚举前缀，minio 实现已提供；`Purger.PurgePrefix`（`domain/storage/purger.go`）由 `NewObjectPurger` 从同一 ObjectStore 派生（共享 client），优先走流式路径，单对象删除失败累计跳过并在末尾汇总（明细至多 10 条）。

## 3. 配置

| 路径 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `storage.s3.endpoint` | `TORCHWOOD_STORAGE_S3_ENDPOINT` | — | 含 `http(s)://host:port` |
| `storage.s3.region` | `TORCHWOOD_STORAGE_S3_REGION` | `us-east-1` | minio client region（MakeBucket region 硬编码 `us-east-1`） |
| `storage.s3.bucket` | `TORCHWOOD_STORAGE_S3_BUCKET` | `torchwood-files` | 单桶承载全项目（config 模板示例值 `torchwood-storage`） |
| `storage.s3.access_key_id` | `TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID` | — | |
| `storage.s3.secret_access_key` | `TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY` | — | |
| `storage.s3.use_ssl` | `TORCHWOOD_STORAGE_S3_USE_SSL` | `false` | endpoint 的 https scheme 可覆盖为 true |

`storage.provider` 与 `storage.local.path`（`internal/pkg/config/config.proto:133-148`）是无消费键：不存在任何读取它们的生产代码，运行时装配固定为 MinIO/S3 适配器，无论取值如何。

## 4. 元数据与权限

### 4.1 表结构（项目 schema 静态表）

`buckets`（迁移 `000008_system_tables.up.sql:109`）：`id TEXT PK` / `name VARCHAR(256) NOT NULL` / `permissions JSONB NOT NULL DEFAULT '[]'` / `public BOOLEAN NOT NULL DEFAULT FALSE` / `created_at` / `updated_at`，name 普通索引。`permissions` 是兼容遗留：`CreateBucket` 仍接受并落库、proto 响应仍回显，但读路径与鉴权完全不消费（A8 决策，`internal/app/storage/storage.go:79`）。

`files`（同文件 `:121`）：`id TEXT PK` / `bucket_id TEXT NOT NULL → buckets(id) ON DELETE CASCADE` / `name VARCHAR(256)` / `mime_type VARCHAR(128) DEFAULT ''` / `size BIGINT CHECK (size >= 0)` / `metadata JSONB DEFAULT '{}'` / `owner_user_id TEXT → users(id) ON DELETE SET NULL`（真实外键） / `created_at` / `updated_at`；索引 bucket_id、owner（部分索引）、name GIN 全文。

### 4.2 归属与访问判定

- `owner_user_id` 仅 EndUser 主体可填（`shared.Principal.StorageOwnerID`，`internal/domain/shared/principal.go:114-119`）：取派生角色中 `users` + `user:<id>` 的 id；API key / admin / system 创建的文件不归属（NULL）。
- 文件访问判定 `canAccessFile`（`internal/app/storage/storage.go:693-705`）：`bucket.Public==true` → 放行所有主体（含匿名 Guest）；特权主体放行；其余 EndUser 仅当 `file.OwnerUserID == 自己的 UserID`。特权 = `BypassesDocumentACL`（system / platform admin）∨ `keys` 角色 ∨ admin 会话角色 `owner|admin|member|viewer`（`isStoragePrivileged`，`:658-672`）。
- 列表隔离在 SQL 侧下推：私有桶 + 非特权 EndUser → 查询追加 `owner_user_id = ?`；非 EndUser 无特权 → 直接 `PermissionDenied`（`:416-423`，`bunrepo/files_repo.go:103-141`）。
- **写路径无文件级 ACL**：`CreateFile` 只校验桶存在，不做 `canAccessFile`（文件尚不存在）；EndUser 对本项目的任意桶可直接上传，归属写在文件行上，读 / 改 / 删 / 列表才走 owner 判定。分片上传的会话操作另有会话属主校验（§5.3）。

### 4.3 MIME 归一化

`normalizeMimeType`（`internal/app/storage/storage.go:633-644`）在**写入时**生效：取 `;` 前的 base 类型，空值与 `text/html`、`application/xhtml+xml`、`application/javascript`、`text/javascript`、`application/xml`、`text/xml`、**`image/svg+xml`** 一律改判 `application/octet-stream`——防存储型 XSS；SVG 落库后 mime 已不存在，`/view` 的 `inlineSafeMime` 白名单仍保留 SVG 强制附件的兜底（纵深防御，`file_handler.go:600-608`）。

## 5. API

### 5.1 gRPC StorageService（ACCESS_SERVER，12 个 RPC：桶 5 + 文件 5 + Token/Usage 2）

| RPC | HTTP | 说明 |
|---|---|---|
| `CreateBucket` | `POST /v1/server/storage/buckets` | app 层 `RequireServerPrincipal`；`name` protovalidate required |
| `ListBuckets` | `GET /v1/server/storage/buckets` | `shared.v1.ListRequest`；`queries` 携带即 InvalidArgument（`servergrpc/list_guard.go`）；SQL 侧 LIMIT/OFFSET 分页 |
| `GetBucket` | `GET .../buckets/{id}` | |
| `UpdateBucket` | `PATCH .../buckets/{id}` | `optional name` / `public`；空补丁 InvalidArgument |
| `DeleteBucket` | `DELETE .../buckets/{id}` | 按 1000/页删文件对象 + 元数据行，再前缀 List+Delete 清残留（含孤儿分片） |
| `CreateFile` | `POST .../buckets/{bucket_id}/files` | gRPC `bytes data` 通道；`permissions` 字段已 reserved（字段号 6，`storage.proto:203-205`） |
| `ListFiles` | `GET .../files` | 同样拒绝 `queries`；owner 过滤与分页 SQL 下推 |
| `GetFile` | `GET .../files/{file_id}` | 仅元数据：走 `GetFileMeta`，不开对象内容流（`servergrpc/storage.go:183-185`） |
| `UpdateFile` | `PATCH .../files/{file_id}` | `optional name` / `mime_type` + `metadata` 整体替换（nil=不修改，含空 map） |
| `DeleteFile` | `DELETE .../files/{file_id}` | 对象删除失败保留 DB 行并报错；对象不存在视为幂等成功 |
| `CreateFileToken` | `POST .../files/{file_id}/tokens` | 显式 `method_auth`（敏感方法不依赖服务级默认） |
| `GetStorageUsage` | `GET /v1/server/storage/usage` | `{buckets, files, total_size}`（`COUNT` + `SUM(size)`，无权限过滤） |

- **鉴权注解**（`storage.proto`）：写方法（CreateBucket / UpdateBucket / DeleteBucket / CreateFile / DeleteFile / UpdateFile / CreateFileToken）= `admin_roles: [MEMBER, ADMIN, OWNER]` + `api_key_scope: storage:write`；读方法（List/Get/Usage）= `api_key_scope: storage:read`，不限 admin 角色。
- **protovalidate**（commit 915471b 起生效，链尾 `ValidateInterceptor` 统一求值）：`CreateBucketRequest.name` required；`CreateFileRequest.bucket_id` / `name` required（`mime_type` 刻意不设 pattern——归一化在服务端）；`CreateFileTokenRequest.expires_in` 为 `1..3600` 的 optional int64。
- **分页**：page_token 为 offset（`crud.EncodePageToken` 编解码）；默认页 25、上限 clamp 100；列表排序 `created_at DESC, id DESC`。
- **大文件边界**：gRPC server `MaxRecvMsgSize(8MiB)`（`cmd/server/internal/runtime/grpc.go:129`），`CreateFile` 的 bytes 通道仅适合小对象；大对象走 §5.2 / §5.3。

### 5.2 HTTP 直传（multipart，挂载于 gateway mux `:9080`）

`internal/api/serverhttp/file_handler.go` 经 `Register(mux)` 挂在 grpc-gateway ServeMux 上（`cmd/server/internal/runtime/grpc_gateway.go:112`），**不经过 gRPC 拦截器链**——认证 / 授权由 handler 自带的 `httpAuth` 完成（复用与 gRPC 同一个 `Validator.Authenticate` 与 `PolicySet`）：

| 方法 | 路径 | 限制 | 说明 |
|---|---|---|---|
| `POST /v1/storage/buckets/{bucketId}/files` | multipart 字段 `file` | 请求体 `100MiB+1MiB`，`ParseMultipartForm(32MiB)` | 201 返回文件元数据 JSON |
| `GET .../files/{fileId}/download` | | | `Content-Disposition: attachment`；`/view` 与 `/download` 由同一 handler 承接 |
| `GET .../files/{fileId}/view` | | | 安全 MIME 内联（§6） |
| `GET .../files/{fileId}/preview` | `?width=&height=` | 见 §6 | 缩略图 |

**认证与授权**（`file_handler.go:839-859` + `auth.go:46-71`）：

- 方法到策略的映射：GET → `StorageService/GetFile`（storage read），其余方法 → `StorageService/CreateFile`（storage write），并携带 `ScopeTargets{BucketID}` 强制实例限定 scope（`storage:<bucketId>` 仅放行该桶）。
- **EndUser（端用户 JWT / 会话 cookie）全部放行**：读 / 写都不在 handler 层设文件级门槛——文件级 owner 校验在 app 层按 §4.2 生效（上传 = 建文件，无需先有权）。
- API key 与函数执行身份：必须持有所映射方法的 scope，实例限定 scope 按 bucketID 强制。
- Admin 会话：写方法额外要求 `CreateFile` 策略派生的 admin 角色（viewer 只读）；项目经 `X-Torchwood-Project` 指定并校验访问权（多值 header 直接拒绝）。
- 项目上下文缺失 → Unauthenticated；API key 主体项目随 key 绑定。

**读路径凭证解析**（`resolveReadContext`，`file_handler.go:543-577`），优先级从高到低：

1. 常规凭证（API key / admin / EndUser JWT / 会话 cookie）；
2. File Token（§7；有效 token 以 `databases.SystemPrincipal` 读取，token 内 project/bucket/file 必须与路径一致）；
3. 公开桶匿名读：URL 需带 `?project=` 定位项目，bucketID 先过格式校验（idgen 合法 + `^[0-9a-zA-Z_-]{1,64}$`，防拼入 query DSL 的注入）再查桶，`public==true` 才以 `GuestPrincipal` 放行。

三者均失败时返回最后一个认证错误。

### 5.3 分片上传（upload session，仅 HTTP 面）

常量（`internal/domain/storage/upload_session.go:48-55`）：`DefaultChunkSize = MaxChunkSize = 16MiB`、会话 TTL 24h、`MinComposePartSize = 5MiB`、`MaxComposePartCount = 10000`、单文件上限 ≈156.25GiB。

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST /v1/storage/buckets/{bucketId}/uploads` | JSON body `{name, mime_type, size, metadata?}`（1MiB 上限） | 校验 size>0 且 ≤ 上限、桶存在、mime 归一化；预生成 `upload_id`/`file_id`；返回 `{upload_id, file_id, chunk_size, part_count, expires_at}` |
| `GET .../uploads/{uploadId}` | 断点续传查询 | 返回升序 `{received:[...]}`；需会话与 bucket 匹配 |
| `POST .../uploads/{uploadId}/chunks/{partNumber}` | multipart 字段 `chunk`，请求体 `16MiB+1MiB` | 非末片必须恰等于 `chunk_size`；末片必须恰等于余量（`uploads.go:139-149`）；同号覆盖幂等；返回 `{part_number, received_count}`（SCARD 原子） |
| `POST .../uploads/{uploadId}/complete` | 合并 | 见下 |
| `DELETE .../uploads/{uploadId}` | 取消 | 删会话 + 逐片删分片对象；204 |

**会话存储**（Redis，`internal/infra/storage/redis_upload_session.go`）：`torchwood:upload:{id}` Hash（全部元数据）+ `:parts` Set（已收分片号）；Create 与 MarkChunk 都刷新两键的 24h TTL；MarkChunk 先 `EXISTS` 守卫，会话已消失时静默跳过（防重建无 TTL 的孤儿 parts 键）；Delete 一并清理 hash / parts / lock 三键。

**会话属主校验**（`checkUploadOwner`，`internal/app/storage/uploads.go:401-419`，作用于 Get / Chunk / Complete / Abort 四个操作）：会话 owner 为空（API key 创建）→ 仅项目归属 + scope 门禁；否则要求调用方 UserID 与会话 owner 一致；bypass / keys / admin 角色豁免。

**complete 算法**（`uploads.go:177-332`）：

1. 校验会话有效 + 项目归属 + 属主；
2. `LockComplete`：`SETNX torchwood:upload:{id}:lock`，随机 token、**TTL 1h**（非 300s）；获取失败 → FailedPrecondition（另一 complete 进行中）；
3. 锁内重读会话（防锁前快照过期），缺片 → FailedPrecondition 并列出缺片号（锁释放、会话保留可续传）；
4. 幂等重入检查：文件文档已存在（上次 complete 建档后被取消）→ 直接返回已有文档并 best-effort 清理分片 / 会话，绝不删最终对象；
5. `Compose` 合并 → `files.Insert`；插入遇 `AlreadyExists`（id 主键冲突）同样幂等返回；真插入失败时回滚删最终对象，且须 `IsLockOwner`（锁仍归自己）+ 会话仍在双重确认后才删——防误删锁 TTL 过期后另一 complete 的成果；
6. 后续清理（逐片删对象、删会话）全部 best-effort，失败仅 Warn（48h 孤儿清理兜底）；
7. 解锁走 compare-and-del（Lua 脚本比对 token），用 `WithoutCancel` + 2s 超时的独立 ctx——gateway 60s TimeoutHandler 取消请求 ctx 后锁仍能释放。

Abort 与 complete 竞争同一把锁：complete 进行中 abort 返回 FailedPrecondition 提示稍后重试。

**孤儿分片回收**：worker 的 `ChunkCleaner`（`worker/cleaner.go`，启动 1 分钟后首跑、此后每小时）按项目列表逐个扫描 `{projectID}/` 前缀，删除 key 含 `/chunks/` 段且 `LastModified` 早于 48h（会话 TTL 的 2 倍余量）的对象；项目枚举包 10s 超时（`internal/app/storage/cleanup.go:22`）。

Console 的 `ChunkedUploader`（`console/src/routes/storage/chunked-uploader.tsx`）自动分片、显示进度，并以 localStorage（键含 bucketId+fileName+size+lastModified）存 `uploadId` 支持刷新后续传。

## 6. 安全输出与预览

**响应加固**（download / view / preview 全部）：`X-Content-Type-Options: nosniff` + `Content-Security-Policy: default-src 'none'; sandbox`。

**内联判定**（`inlineSafeMime`，`file_handler.go:39-48, 600-608`）：白名单 `image/{png,jpeg,gif,webp,avif,svg+xml}`、`text/plain`、`application/pdf`，外加 `video/*`、`audio/*` 前缀；SVG 名义在白名单但显式强制附件；`/download` 恒为附件。文件名经 `safeFilename` 去控制字符、引号与反斜杠替换为 `_`，空名回退 `download`；响应头同时输出 `filename="<ASCII 回退>"` 与 `filename*=UTF-8''…`。

**缓存策略**：仅公开桶匿名路径允许 `Cache-Control: public, max-age=86400`；凭证路径与 File Token 路径一律 `private, no-store`（`file_handler.go:525-530`）。

**缩略图**（`/preview`，`file_handler.go:663-771`）：

- 仅 `image/{png,jpeg,gif,webp}` 可预览；源 >50MiB 拒绝；`width`/`height` 为 0..4096 的整数（两者都缺省时直接回源，仍带安全头）。
- **并发闸**：单实例并发解码 / 缩放上限 2（`previewMaxConcurrent`，`pkg/semaphore` 实现，可选 Redis 后端），在鉴权与读流之前 `TryAcquire`，满载立即 429 ResourceExhausted；信号量自身故障时放行降级（不因闸故障拒服务）。
- **解压炸弹防护**：先读最多 512KiB 的头部交给 `image.DecodeConfig` 解析宽高，任一边 >8192 直接 400（不读全量）；通过后才受限读取全文件（≤50MiB+1），头部已消费字节拼回。
- 缩放 `imaging.Fit(Lanczos)` + `AutoOrientation`，输出尺寸 clamp 4096；流式编码到响应（避免整图缓冲翻倍）；编码格式：png→PNG、gif→GIF、webp→JPEG（x/image 的 webp 仅可解码）、其余 JPEG。

## 7. File Token（短 TTL 匿名下载凭证）

- **签发**：gRPC `CreateFileToken`（SDK 侧 `sdk/go/server/storage.go:77` 透传）。签发前先过 `canAccessFile`；`expires_in` 未设或 ≤0 取默认 900s（15 分钟），>3600 clamp 到 3600（gRPC 面另有 protovalidate `1..3600` 前置拦截）。Token 仅在创建响应返回一次，任何接口不回显。
- **格式**：`{expiresAt}.{projectID}.{bucketID}.{fileID}.{hex(hmac)}`，HMAC-SHA256 对前四段；密钥由 `security.jwt.secret` 经 `jwtparser.DeriveKey(..., PurposeFileToken)` 域分离派生（与 JWT / 会话密钥不通用）；secret 未配置 → Internal（`internal/app/storage/storage.go:523-613`）。
- **校验** `ParseFileToken`：拆 5 段 → 过期判定 → `hmac.Equal` 全串比对；任一不符 Unauthenticated。返回绑定的 project/bucket/file，调用方仍须比对请求路径参数（handler 已做）。
- **携带方式**（`fileTokenFromRequest`，`file_handler.go:581-596`），优先级从高到低：请求头 `X-File-Token` → `X-Torchwood-File-Token` → `Authorization: Bearer <token>`（按 4 个 `.` 预筛）→ query `?token=`（兼容旧客户端；header 优先是为避免 token 泄进日志 / 代理）。
- 测试可注入假时钟拨快过期路径（`WithClock`，`storage.go:40-58`）。

## 8. 限速、审计与超时

**两条路径的治理面不同**：

- **HTTP 直传面**（`/v1/storage/*`，自定义 handler）不经过 gRPC 拦截器链：**无通用限速**，唯一的并发治理是 preview 信号量（§6）。但每次操作的成败都会：slog 结构化日志（`actor_id/actor_kind/credential_type/project_id/ip`，IP 走与 gRPC 一致的 trusted_proxies 规则）+ 经 `auditFromHTTP` 落一条 `audit_logs` 行（action=`http.storage.<op>`，status=success 或错误码，3s 超时 + `WithoutCancel`，best-effort，失败仅 Warn）。唯一例外：分片上传成功不记（防 64 片 64 条噪音），失败仍记（`file_handler.go:93-125, 307`）。
- **gRPC 面**（`/v1/server/storage/*`）经过完整拦截器链：`RateLimitInterceptor` 按 API key（6000/min）> 执行身份（6000/min）> user（1000/min）> IP（300/min）单一维度限流（60s 固定窗口，`security.rate_limit` 可配；超限 ResourceExhausted → HTTP 429 并附 RetryInfo；Redis 基础设施错误 fail-closed，连续 5 次/10s 触发 30s 熔断放行）。`AuthInterceptor` 另有 X-API-Key 认证失败的按 IP 频控。审计按"server 面非读方法"门控：Create/Update/Delete/CreateFileToken 落审计（带脱敏请求摘要），List/Get/Usage 不落（`internal/api/interceptor/audit.go:72-93`）。

**超时**：

- gateway 所有 HTTP 路由（除 `/v1/realtime`）统一包 60s `TimeoutHandler`（`grpc_gateway.go:137-148`）——大分片数的 complete 可能被它取消，故解锁逻辑用独立 ctx（§5.3）。
- 存储元数据 SQL（buckets/files repo）**没有** per-statement 超时（不同于 assets repo 的 5s/10s 做法），慢查询依赖 gateway 60s 兜底。
- 审计插入 3s（`WithoutCancel`）；`CleanupOrphanChunks` 项目枚举 10s。
- 对象存储侧无额外重试：Compose 失败直接透传（分片保留可重试），分片大小违例由服务端返回 InvalidArgument。

## 9. 测试

- `internal/api/serverhttp/`：`file_handler_integration_test.go`（multipart、download / view / Token、public 匿名）、`file_handler_uploads_test.go`（分片全流程与 scope）、`file_handler_preview_test.go`（缩略图与炸弹防护）、`file_handler_gate_test.go`（preview 并发闸）、`file_handler_dsl_test.go`（bucketID 校验）。
- `internal/app/storage/`：用例层单测与集成——桶 CRUD、文件更新、usage 聚合（`storage_unit_test.go` / `storage_meta_test.go` / `storage_integration_test.go`）、Token 签发与过期（`file_token_test.go`，假时钟）、分片末片精确校验（`uploads_finalpart_test.go`）、complete 幂等 / 锁竞态（`uploads_idempotency_test.go` / `uploads_integration_test.go`）、孤儿清理（`cleanup_integration_test.go`）。
- `internal/infra/storage/`：`redis_upload_session_test.go`（miniredis 往返 / TTL / 锁）、`minio_integration_test.go`（真实 ComposeObject；`TORCHWOOD_TEST_MINIO_ENDPOINT` 未设跳过）、`purger_test.go`。
- 集成测试统一经 `internal/pkg/testutil/db.go:SetupTestDB` 建隔离项目库。

## 10. 已知边界

- 单 S3 bucket 多租户靠键前缀隔离；`storage.provider` / `storage.local.path` 为死配置键，本地盘后端未实现。
- 分片上传仅存在于 HTTP 面；`StorageService.CreateFile` 的 `bytes data` 受 gRPC `MaxRecvMsgSize(8MiB)` 约束，大对象走 multipart / 分片。
- `UpdateFile.metadata` 为整体替换（map 非 nil 即全量覆盖）；"空 map 不修改"若未来 optional 化属破坏性变更。
- 存储元数据路径无 per-statement SQL 超时；`DeleteBucket` 的逐对象删除失败仅 Warn 继续（残留由前缀清尾与运维兜底）。

## 相关文档

- `06-databases.md` — 三层 schema 与静态表定位
- `05-authentication.md` — API Key scope（`storage.write` / `storage:<bucket_id>` 实例限定）与限流 / 审计
- `docs/implementation-storage-chunked-upload.md` — 分片上传设计细节与偏离说明
