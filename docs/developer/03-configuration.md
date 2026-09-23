# 配置体系

本章说明 Torchwood 的配置如何定义、加载与覆盖：`internal/pkg/config/config.proto` 是配置 schema 的单一事实源，YAML 文件提供基础值，`TORCHWOOD_` 前缀环境变量逐键覆盖。读完本章可以定位任何一个配置项的声明位置、默认值与生效优先级。

> 事实源：`internal/pkg/config/config.proto`（schema）、`internal/pkg/config/bind.go`（绑定与解码）、`internal/pkg/config/runtime_env.go`（运行时环境）、`configs/config.yaml.template`（完整键模板）、`.env.example`（环境变量样例）。

---

## 1. config.proto：单一事实源

配置 schema 定义在 `internal/pkg/config/config.proto`（顶层 message `AppConfig`），经 `mise run generate:config` 生成 `config.pb.go`，YAML 结构与运行时结构体都从它派生。生成流程见 `04-codegen.md`。

| 分组 | 内容 |
|------|------|
| `server` | `grpc` / `http` / `metrics` 三组监听地址与超时；`http.cors`；`http.public_url` |
| `security` | `jwt`、`api_key`、`trusted_proxies`、`setup_token`、`sessions`、`rate_limit`、`login_throttle`、`encryption_key` |
| `data` | `database`（DSN / debug / 连接池 / 慢查询阈值）+ `redis` |
| `storage` | `s3` 对象存储连接（`provider` / `local` 为声明保留键，见 §6.4） |
| `functions` | `execution`、`docker`、`dispatcher`、`trigger`、`client_invoke`、`packer`、`image`、`storage`（见 §1.5） |
| `payments` | stripe / wechat / alipay / ios_iap 四渠道；secret 类字段一律走环境变量 |
| `analytics` | `retention_days` |
| `messaging` | SMTP、SMS(Twilio)、`dev_log_otp` / `dev_log_sms` 开发态开关 |
| `idgen` | 五种 ID 策略与每资源覆盖 |

### 1.1 server：监听与 HTTP 面

| 键 | 默认 | 说明 |
|----|------|------|
| `server.grpc.addr` | 模板 `127.0.0.1:9060` | gRPC 监听地址，仅回环供同进程 gateway 转发。gateway 转发目标由本键推导（保留主机，缺端口补 `127.0.0.1` 与 `9060`）；键完全未配置时 gRPC 监听回落 lynx 内置 `:9090`、gateway 却拨 9060，**生产必须显式配置** |
| `server.grpc.timeout` | 30s | gRPC 单请求超时 |
| `server.http.addr` | 模板 `:9080` | HTTP（gateway + Console SPA + 自定义 handler）监听地址 |
| `server.http.timeout` | 60s | gateway 侧 `TimeoutHandler` 兜底（`/v1/realtime` 长连接路径不套） |
| `server.http.public_url` | — | 对外基址；构造 OAuth 回调 URI，并以 `https://` 前缀决定 cookie `Secure` |
| `server.http.cors.*` | 不启用 | 仅当配置了 `cors` 段才挂 CORS 中间件 |
| `server.metrics.addr` | `127.0.0.1:9040` | `/metrics` 无鉴权，缺省只听回环 |
| `server.debug.addr` | `127.0.0.1:6060` | lynx debug 诊断服务（pprof 全端点 + `/healthz` + `/version`），仅 server 进程装配；pprof 暴露内存快照与源码路径，勿映射出容器 |

`server.http.cors` 字段：`allow_origins`（精确匹配或 `*`）、`allow_methods`、`allow_headers`、`expose_headers`（落在真实响应上，浏览器才可读）、`allow_credentials`（为 true 时 `*` origin 被剔除并告警；仅随匹配 origin 输出）、`max_age`（秒，>0 才输出）。反射 origin 时必设 `Vary: Origin`；预检统一 204。

### 1.2 security

**`security.jwt.secret`**：全局必填，server / worker / dispatcher / packer 四进程同一 fail-closed 口径。启动期校验 ≥32 字符且不含弱子串（黑名单 `internal/pkg/bootkit/config.go`：change-me / changeme / minioadmin / secret / password / torchwood），并从它派生页 token 与 roles GUC 的 HMAC 签名密钥。

- `security.jwt.access_ttl`：端用户 access token 默认 15m；Console admin 的 access（亦即会话 cookie Max-Age）缺省 1h 且**上限封顶 1h**。
- `security.jwt.refresh_ttl`：默认 7d。
- `security.encryption_key`：独立的静态字段加密密钥（OAuth client secret / TOTP secret，secretbox）。显式配置时套用与 jwt.secret 相同的强度规则（不合规拒绝启动）；未配置时回退 jwt.secret 并打印告警。
- `security.setup_token`：Console 首个管理员引导令牌。为空时 SignUp 直接拒绝；非空时同样按主密钥强度校验。

**`security.sessions.max_per_user`**：单用户并发会话上限。未配置/0 = 50；-1 = 不限。

**`security.rate_limit`**：gRPC 管道全量通用限流（API Key > user > IP 三维度按命中计数一个）。总开关 `enabled` 为 proto3 optional：未配置 = 开启，显式 `false` 才关闭。各维度 `limit` / `window` 未配置时回落内置默认：

| 维度 | 默认 |
|------|------|
| `ip` | 300 次 / 60s |
| `user` | 1000 次 / 60s |
| `api_key` | 6000 次 / 60s |
| `functions_execution` | 6000 次 / 60s（按 project:function 计数的独立维度；函数容器经 NAT 同 IP、每次执行 ActorID 独立，落 IP/user 维度都会失效） |

**`security.login_throttle`**：认证面局部频控（失败计数 + 窗口；成功登录/注册清零；超限 429 + Retry-After）：

| 维度 | 默认 | 语义 |
|------|------|------|
| `email` | 5 次 / 60s | 按账号失败计数；未注册邮箱的失败**不**计入（不锁死他人邮箱，IP 维度与账号存在性解耦） |
| `ip` | 5 次 / 60s | 按来源 IP（经 trusted_proxies 校验后） |
| `signup_ip` | 10 次 / 1h | 注册按来源 IP |
| `api_key_auth` | 10 次 / 60s | X-API-Key 认证失败按来源 IP（仅 server 进程装配） |

### 1.3 data

- `data.database.source`：PG DSN，server / worker 的运行期硬依赖（连接 + ping 失败拒绝启动；worker 另有启动期显式校验）。必须是非 superuser 运行账号——superuser 绕过文档面 RLS（见 `13-operations.md`）。
- `data.database.debug`：开启 SQL 调试日志。
- `data.database.pool`：`max_idle_conns` / `max_open_conns` / `conn_max_lifetime` / `conn_max_idle_time`。整段未配置时 `max_open = max_idle = 4×GOMAXPROCS`；显式配置 ≤0 视为误配，告警并回落同款默认；时长字段非法时告警并保持无限寿命。
- `data.database.slow_query_threshold`：慢查询日志阈值。空 = 500ms；`"0"` = 禁用；非法值告警并禁用。
- `data.redis.addr` / `password` / `db`：Redis 连接。`addr` 必填，无 localhost 静默回退。

### 1.4 storage

对象存储装配固定为 MinIO/S3（`internal/infra/storage`），连接参数全部来自 `storage.s3`：

| 键 | 默认 | 说明 |
|----|------|------|
| `storage.s3.endpoint` | — | 如 `http://127.0.0.1:9000`；带 scheme 时按 scheme 推断 TLS |
| `storage.s3.region` | `us-east-1` | |
| `storage.s3.bucket` | `torchwood-files` | 用户文件桶；模板显式 `torchwood-storage`。要求全小写 |
| `storage.s3.access_key_id` / `secret_access_key` | — | MinIO 凭据，走环境变量（见 §3） |
| `storage.s3.use_ssl` | false | endpoint 无 scheme 时的 TLS 开关 |

`storage.provider` 与 `storage.local.path` 当前无消费方（见 §6.4）。

### 1.5 functions

函数执行统一经 dispatcher 分发（docker.sock 唯一持有方是 dispatcher 进程）。

**`functions.execution`**：`api_base_url` 是函数容器回访 Server API 的基址（经 `TW_API_BASE_URL` 注入执行环境）。空 = 不注入（函数需自行解析；自托管部署应显式配置，且 server/worker 两进程同值）。

**`functions.docker`**（仅 dispatcher 进程消费）：

| 键 | 默认 | 说明 |
|----|------|------|
| `host` | 模板 `unix:///var/run/docker.sock` | docker daemon 地址 |
| `network` | 空 = per-project `tw-func-<project_id>` | 显式全局网络是 opt-in（跨租户互通风险）；不可信函数固定挂 internal 变体 `tw-func-<project_id>-int` |
| `registry` | `torchwood-funcs` | 函数镜像命名前缀；`routing_mode="registry"` 时升格为真实 registry |

**`functions.dispatcher`**：`url` 指向 dispatcher 内网 HTTP 端点，server/worker **必填**（启动期校验）；`shared_token` 为内网可选认证（空 = 不校验）。其余字段仅 dispatcher 进程消费：

| 键 | 默认 | 说明 |
|----|------|------|
| `max_resident_instances` | 16 | 每节点常驻实例总量上限（内存敞口 = 实例数 × spec 内存） |
| `queue_depth` | 32 | 单函数排队深度上限，超限立即 ResourceExhausted |
| `queue_head_timeout` | 10s | 等待空闲实例的队首超时 |
| `boot_timeout` | 60s | 实例启动健康探针等待上限 |
| `addr` | `:9070` | dispatcher HTTP 监听地址（仅内网） |
| `callback_container` | 空 = 不 attach | join 项目网络时一并 attach 的 server 容器名；函数经容器名 DNS 回访平台，attach 失败仅告警 |
| `timeout_budget` | 5 | 实例累计超时熔断阈值（达到即杀实例重建） |
| `build_timeout` | 5m | 部署构建整体超时（构建 ctx 与客户端断开解耦，由该值封顶；验证 spawn 预算嵌套其内） |
| `verify_build` | 默认开启 | 部署后验证 spawn（build 成功后起池外实例轮询 `/_tw/health`）；optional presence：未配置 = 开启，显式 `false` = 关闭 |
| `rebuild_on_missing_image` | 默认开启 | 执行命中「部署镜像在本节点缺失」时异步触发该部署重建（zip 源自愈；git 源桶 miss 回退 packer 重物化） |
| `node_id` | 空 = os.Hostname | 多机节点注册表与构建亲和的节点标识 |
| `node_url` | 空 = `http://127.0.0.1:<addr 端口>` | 本节点对等互达地址；多机部署必须显式配置 |
| `routing_mode` | `local` | `local` = 镜像不分发，冷启动转发构建节点；`registry` = 镜像全局化（push registry、任意节点 miss 后 pull）。其他值启动期拒绝 |
| `registry_push` | false | 构建成功后 push 镜像；`routing_mode="registry"` 时必须显式 true（启动期校验），local 模式忽略 |
| `max_resident_instances_global` | 0 = 不设 | 全集群常驻总量上限（Redis 容量键 SCAN 求和；Redis 不可用 fail-open 退化为仅本节点上限） |

函数池策略（per-function，不在本配置内）的缺省值：`max_instances` 4、`idle_ttl` 300s、`max_requests_per_instance` 1000。

**`functions.trigger`**：`http_ip_per_minute` = 3000。HTTP 触发器公开端点（`/f/{project}/{token}`）的 per-IP 固定窗口限频；独立于全局 300/min 档（微信等回调出口 IP 段集中，沿用全局档会在回调风暴下丢事件）。

**`functions.client_invoke`**：客户端调用面平台级参数（per-function 配额/窗口在 functions 表策略列上）：

| 键 | 默认 | 说明 |
|----|------|------|
| `per_user_concurrency` | 8 | 每用户并发闸门（进程内 keyed 信号量，多实例部署下全局上限 = 上限 × 实例数） |
| `queue_head_timeout` | 10s | 闸门排队队首超时（与 dispatcher 池队首口径一致） |

**`functions.packer`**（git 部署源打包服务）：`url`（server 侧寻址 packer；空 = git 源未启用，zip 源不受影响）与 `shared_token` 两进程共享；其余仅 packer 进程消费：

| 键 | 默认 |
|----|------|
| `addr` | `:9071` |
| `fetch_timeout` | 120s（单次 fetch+物化整体超时） |
| `max_repo_bytes` | 200MiB（克隆 worktree 磁盘预算） |
| `max_zip_bytes` | 50MiB（物化 zip 传输预算） |
| `concurrency` | 4（饱和请求立即 429） |
| `allow_insecure` | false（放行 http:// 与私网目标；仅自托管内网 git 场景开启） |

**`functions.image`**（BYO 镜像源 registry 准入，仅 dispatcher 消费）：`allowed_registries` 为 host 正向白名单（空 = 不设白名单；非空时未命中一律拒绝，条目命中精确域名或其后缀域）；`allow_insecure` 放行 IP 字面量与 `*.localhost` host（默认 false）。

**`functions.storage`**：`bucket` = 部署代码包专用物理桶（空 = 缺省 `torchwood-functions`）。平台内部资源，不进用户 bucket 命名空间；连接复用 `storage.s3` 的 endpoint/凭证，写路径失败整体回滚——MinIO/S3 是函数部署的硬依赖。

### 1.6 payments

四渠道（stripe / wechat / alipay / ios_iap）共用同一模式：**secret 类字段只走环境变量**（如 `TORCHWOOD_PAYMENTS_STRIPE_SECRET_KEY`、`TORCHWOOD_PAYMENTS_WECHAT_MERCHANT_PRIVATE_KEY`、`TORCHWOOD_PAYMENTS_ALIPAY_APP_PRIVATE_KEY`、`TORCHWOOD_PAYMENTS_IOS_IAP_PRIVATE_KEY`），不写入 config.yaml；未配置时服务可启动，相关操作 fail-closed。非敏感字段（`mch_id`、`app_id`、`bundle_id`、`notify_url`、`api_base_url` 等）可进 YAML；`notify_url` 为空时用 `server.http.public_url` + 各渠道回调路径。

### 1.7 analytics / messaging / idgen

- `analytics.retention_days`：原始事件与 user_days 保留天数。默认 90；可配域 [7, 365]，未配置与越界值统一归一钳制（`domainanalytics.NormalizeRetentionDays`）。
- `messaging.smtp`：`port` 缺省 587、`from` 缺省 `noreply@torchwood.local`（代码级默认）；`use_tls` 默认 true。`messaging.sms.provider` 目前仅 `twilio`。
- `messaging.dev_log_otp` / `dev_log_sms`：SMTP/SMS 未配置时把验证码打到 stdout 的开发态开关。生产环境（`TORCHWOOD_ENV` 归一为 production）发送路径直接拒绝——宁可发送失败也不把 OTP 写进日志。
- `idgen.default_strategy`：`uuid | ulid | snowflake | sequence | random`，缺省 uuid。`idgen.resources.users/sessions/documents` 可按资源覆盖平台缺省策略。`idgen.random`：`length` 10 / `charset` numeric / `redis_key_prefix` `Torchwood:id:random` / `max_retries` 10。`idgen.snowflake.node_id` 默认 0。`idgen.sequence.redis_key_prefix` 默认 `Torchwood:seq`。

---

## 2. 运行时绑定（bind.go）

`internal/pkg/config/bind.go` 的 `ConfigureConfigSource` 在 lynx `ConfigSource`（viper 适配）上装配：

1. 先调 `lynx.DefaultBindConfigFunc`：处理 `-c/--config`（指定具体文件）、`--config-dir`（搜索目录）与 `--config-type`（缺省 yaml）；两者都未给时把工作目录加入搜索路径。
2. `SetEnvPrefix("TORCHWOOD")` + `SetEnvKeyReplacer("." → "_", "-" → "_")` + `AutomaticEnv()`：任意点分键都能被同名环境变量覆盖。
3. `UnmarshalConfig` 调 `c.Unmarshal(out, lynx.WithEnvForAllKeys())`：按 AppConfig 结构体叶子逐键取值组装（tag 回退链 mapstructure → json → 小写字段名），再经 mapstructure 解码——弱类型转换、duration 字符串、逗号切分的 repeated 字段都在这一层完成；仅在环境变量中设置的键（YAML 无此键）同样参与解码。

四个入口（`cmd/server` / `cmd/worker` / `cmd/dispatcher` / `cmd/packer`）都以 `--config-dir` 默认 `./configs` 注册 flag 并传入 `config.NewBindConfigFunc()`。配置文件不存在不是错误（纯环境变量部署合法）；只有显式指定的 `-c` 文件缺失或 YAML 解析错误才硬失败。

`--config-dir` / `--log-level` 两个 flag 经 `BindPFlags` 进配置源；日志级别按 `logging.level`（YAML 键）→ `--log-level` → `log_level` 的优先级链解析，缺省 info。

---

## 3. 环境变量覆盖规则

- 前缀固定 `TORCHWOOD_`；键路径以 proto json tag 叶子为准，点号/连字符转下划线、整体大写：

```
data.database.source      →  TORCHWOOD_DATA_DATABASE_SOURCE
security.trusted_proxies  →  TORCHWOOD_SECURITY_TRUSTED_PROXIES
storage.s3.access_key_id  →  TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID
```

- 业务键优先级：**环境变量 > config.yaml > proto/代码默认值**；显式命令行 flag 优先于环境变量。
- `repeated` 字段用逗号分隔（`TORCHWOOD_SECURITY_TRUSTED_PROXIES=127.0.0.1/32,10.0.0.0/8`）。
- 映射覆盖 proto 全部叶子键，不限于下表常用项。

常用映射：

| 点号路径 | 环境变量 |
|----------|----------|
| `security.jwt.secret` | `TORCHWOOD_SECURITY_JWT_SECRET` |
| `security.encryption_key` | `TORCHWOOD_SECURITY_ENCRYPTION_KEY` |
| `security.setup_token` | `TORCHWOOD_SECURITY_SETUP_TOKEN` |
| `security.trusted_proxies` | `TORCHWOOD_SECURITY_TRUSTED_PROXIES` |
| `security.sessions.max_per_user` | `TORCHWOOD_SECURITY_SESSIONS_MAX_PER_USER` |
| `security.rate_limit.*` | `TORCHWOOD_SECURITY_RATE_LIMIT_<DIM>_<LIMIT|WINDOW>` |
| `security.login_throttle.*` | `TORCHWOOD_SECURITY_LOGIN_THROTTLE_<DIM>_<LIMIT|WINDOW>` |
| `data.database.source` | `TORCHWOOD_DATA_DATABASE_SOURCE` |
| `data.redis.*` | `TORCHWOOD_DATA_REDIS_ADDR` / `_PASSWORD` / `_DB` |
| `storage.s3.*` | `TORCHWOOD_STORAGE_S3_ENDPOINT` / `_BUCKET` / `_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY` 等 |
| `functions.dispatcher.*` | `TORCHWOOD_FUNCTIONS_DISPATCHER_*`（如 `_URL`、`_SHARED_TOKEN`、`_ROUTING_MODE`） |
| `functions.execution.api_base_url` | `TORCHWOOD_FUNCTIONS_EXECUTION_API_BASE_URL` |
| `functions.trigger.http_ip_per_minute` | `TORCHWOOD_FUNCTIONS_TRIGGER_HTTP_IP_PER_MINUTE` |
| `functions.client_invoke.*` | `TORCHWOOD_FUNCTIONS_CLIENT_INVOKE_*` |
| `functions.packer.*` | `TORCHWOOD_FUNCTIONS_PACKER_*` |
| `functions.image.*` | `TORCHWOOD_FUNCTIONS_IMAGE_*` |
| `functions.storage.bucket` | `TORCHWOOD_FUNCTIONS_STORAGE_BUCKET` |
| `payments.*` | `TORCHWOOD_PAYMENTS_*`（secret 类唯一注入通道，见 §1.6） |
| `analytics.retention_days` | `TORCHWOOD_ANALYTICS_RETENTION_DAYS` |
| `idgen.snowflake.node_id` | `TORCHWOOD_IDGEN_SNOWFLAKE_NODE_ID` |

MinIO 凭据变量名（`ACCESS_KEY_ID` / `SECRET_ACCESS_KEY`）由字段名直接映射而来，`bind.go` 推导、`config.yaml.template` 注释与 `.env.example` 三处一致。

---

## 4. TORCHWOOD_ENV 与关停排水

`TORCHWOOD_ENV` 与 `TORCHWOOD_SERVER_DRAIN_TIMEOUT` 不在 config.proto 里——lynx 在 `NewRunner` 时就需要排水窗口（早于 YAML 绑定），所以这两个变量在 `internal/pkg/config/runtime_env.go` 中于 Runner 构造前直接从进程环境读取。

环境归一化（`ParseRuntimeEnv`，大小写/空白容忍）：

| `TORCHWOOD_ENV` 取值 | 归一化 | 默认排水窗口 |
|----------------------|--------|--------------|
| `development` / `dev` / `local` / `test` | development | 0（跳过排水） |
| `production` / `prod` / `staging` / 空 / 未知 | production | 30s |

排水窗口仅 **server** 进程设置（`cmd/server/main.go` 传 `WithDrainTimeout(CurrentDrainTimeout())`）：关停时 readiness 先摘流，窗口内等待在途请求收尾，之后才进入 ShutdownTimeout(30s) 与服务 Stop。worker / dispatcher / packer 无 LB 摘流语义，不设排水窗口，仅有界关停（ShutdownTimeout 30s；packer 的 HTTP 层另有 15s 在途排空）。

`TORCHWOOD_SERVER_DRAIN_TIMEOUT` 配置合法非负 duration（`0s`、`5s`；裸 `0` 有特判）时覆盖默认值，非法或负值回退默认。生产部署应保证容器的 `terminationGracePeriodSeconds ≥ DrainTimeout + ShutdownTimeout(30s) + StopTimeout`。该环境同时影响运行期行为判定：`dev_log_otp` / `dev_log_sms` 的生产禁止即按归一化结果判断。

---

## 5. 配置文件与加载顺序

| 文件 | 角色 |
|------|------|
| `configs/config.yaml.template` | 完整键与默认值的模板（少量新增键可能滞后于 proto，以 proto 为准） |
| `configs/config.yaml` | 本地实际配置（已 gitignore），从模板复制修改 |
| `.env` / `.env.example` | 环境变量载体；secret 一律走环境变量，不进 YAML。`mise.toml` 的 `[env] _.file = ".env"` 使所有 mise 任务自动加载它——mise 会覆盖同名 shell 变量，临时覆盖请用任务专属变量 |

`cmd/server/main.go`（worker / dispatcher / packer 同构）的启动顺序：

```
godotenv.Load()                            1. 加载 .env（文件不存在则容忍跳过）
lynx.NewRunner(
  --config-dir ./configs                   2. 配置目录 flag（默认值）
  WithBindConfigFunc(config.NewBindConfigFunc())
                                           3. YAML 搜索路径 + 环境变量绑定（§2）
  WithDrainTimeout(CurrentDrainTimeout)    4. 仅 server；早于 YAML 绑定求值（§4）
)
→ wireBootstrap → NewAppConfig → UnmarshalConfig → 启动校验
```

启动校验按进程而不同（组合根 `NewAppConfig`）：

| 进程 | 校验 |
|------|------|
| server | `ValidateAppConfig`（jwt.secret 必填 + encryption_key/setup_token 条件强度校验）+ `ValidateFunctionsDispatchConfig`（`functions.dispatcher.url` 必填 + 路由模式校验） |
| worker | 同 server 全部校验 + `data.database.source` 显式必填校验 |
| dispatcher | `ValidateAppConfig` + `ValidateFunctionsRoutingConfig`（自身是通路终点，不消费 `url`） |
| packer | 仅 `ValidateAppConfig` |

---

## 6. 特殊配置项

### 6.1 trusted_proxies

`security.trusted_proxies` 声明可信反向代理的 CIDR 网段（裸 IP 按 `/32`、`/128` 处理）。仅当 gRPC 直连 peer 地址命中可信网段时，才采纳 `X-Forwarded-For` 首跳（无 XFF 退 `X-Real-Ip`）作为客户端地址；否则一律使用 peer 地址。默认为空 = 不信任任何代理，防止伪造头绕过限流与审计。gateway 与 gRPC 同进程部署时需包含 `127.0.0.1/32`。

```yaml
security:
  trusted_proxies: ["127.0.0.1/32"]
```

### 6.2 Console 会话 cookie

实现位于 `internal/api/consolegrpc/cookies.go` 与 `internal/app/console/auth.go`：

- 访问 cookie `TORCHWOOD_session_console`（`Path=/`）+ 刷新 cookie `TORCHWOOD_console_refresh`（`Path=/v1/console/auth`，限定刷新路径）。
- 均为 `HttpOnly` + `SameSite=Lax`。跨站 POST 不携带 cookie，因此无需额外 CSRF token；前端不使用 localStorage 存 token。
- 仅当 `server.http.public_url` 以 `https://` 开头时附加 `Secure`。
- 刷新请求体中的 `refresh_token` 为空时走 cookie-only 流程；`SignOut` 以 `Max-Age=-1` 清除 cookie；refresh 带 rotation 与重用检测——检测到 `RotateMismatch` 时撤销该管理员全部 token。

### 6.3 测试 DSN

`TORCHWOOD_TEST_DATABASE_SOURCE` 与 `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE` 不属于 `AppConfig`，由 `internal/pkg/testutil/db.go` 直接 `os.Getenv` 读取（前者为运行账号、后者为迁移用的特权账号）。每个集成测试创建独立的 `TORCHWOOD_test_<pid>_<seq>` 隔离库；`mise run test` 经 `[env] _.file` 自动从 `.env` 加载；`go test -short` 跳过集成测试。

### 6.4 schema 中声明但当前未被消费的键

以下键存在于 proto/template、反序列化不报错，但当前代码不消费，修改不产生效果：

- `security.api_key.header`：认证拦截器固定读取 `x-api-key` 头（gRPC metadata 大小写不敏感）。
- `storage.provider` / `storage.local.path`：对象存储装配固定为 MinIO/S3 适配器，无本地盘实现分支。

存量 YAML 中的已退役键（如 `functions.executor`、历史 `telemetry` 节）会被解码层静默容忍——proto 反序列化不拒绝未知字段，对应访问器已随 schema 删除。

---

## 相关文档

- `04-codegen.md` — config.pb.go 的生成流程
- `02-quickstart.md` — 本地 `.env` 最小可用配置
- `13-operations.md` — 生产配置要点、双账号契约与 roles_sig 部署时序
