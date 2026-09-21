# 配置体系

本章说明 Torchwood 的配置如何定义、加载与覆盖：`config.proto` 是配置 schema 的单一事实源，YAML 文件提供基础值，`TORCHWOOD_` 前缀环境变量逐键覆盖。读完本章可以准确定位任何一个配置项的声明位置与生效优先级。

> 事实源：`internal/pkg/config/config.proto`、`internal/pkg/config/bind.go`、`internal/pkg/config/runtime_env.go`、`configs/config.yaml.template`、`.env.example`。

---

## 1. config.proto：单一事实源

配置 schema 定义在 `internal/pkg/config/config.proto`（顶层 message `AppConfig`），经 `mise run generate:config` 生成 `config.pb.go`。YAML 与结构体都从它派生，避免两处维护。生成流程见 `04-codegen.md`。

| 分组 | 说明 |
|------|------|
| `server` | `grpc` / `http` / `metrics` 三组监听地址与 `http.public_url` |
| `security` | `jwt`、`api_key`、`trusted_proxies`、`setup_token`、`sessions`、`rate_limit`（含 `functions_execution` 维度）、`login_throttle`、`encryption_key` |
| `data` | `database`（DSN / 连接池 / 慢查询阈值）+ `redis` |
| `storage` | `s3` / `local` 对象存储 |
| `functions` | `execution.api_base_url`、`dispatcher` 子节、`trigger.http_ip_per_minute`、`client_invoke` 子节（详见 §1.1） |
| `payments` | stripe / wechat / alipay / ios_iap 渠道密钥（一律走环境变量，不进 YAML） |
| `analytics` | `retention_days`（默认 90，可配域 7–365，越界钳制） |
| `telemetry` | OTLP 导出 |
| `messaging` | SMTP、SMS(Twilio)、`dev_log_otp` / `dev_log_sms` 开发态开关 |
| `idgen` | uuid / ulid / snowflake / sequence / random |

### 1.1 关键字段

**监听地址**：`server.grpc.addr` 默认 `127.0.0.1:9060`（仅回环，供 gateway 转发）；`server.http.addr` 默认 `:9080`；`server.metrics.addr` 为空时回退 `127.0.0.1:9040`。`server.http.public_url` 决定 OAuth 回调地址与 cookie 的 `Secure` 标志。

**安全必填项**：

- `security.jwt.secret` 必填，启动期校验 ≥32 字符且不含弱子串（黑名单见 `internal/pkg/bootkit/config.go`：change-me / changeme / minioadmin / secret / password / torchwood）。server 与 worker 共享该校验。
- `security.encryption_key` 是独立的静态字段加密密钥（用于 OAuth / TOTP secretbox 加密）；未配置时回退 `jwt.secret` 并打印告警。
- `security.setup_token` 为空时，首个管理员注册接口直接返回 `FailedPrecondition`。

**频控**：`security.rate_limit` 有总开关（`optional bool enabled`，默认开启）与 ip / user / api_key / functions_execution 四个固定窗口维度（默认 6000 次/60s）。`security.login_throttle` 是认证面的局部频控：email / ip 失败各默认 5 次/60s、signup_ip 10 次/1h、api_key_auth 10 次/60s；成功登录或注册会重置相关计数键。

**杂项**：`data.database.slow_query_threshold` 为空表示 500ms，`0` 表示禁用慢查询日志。

### 1.2 Functions 配置

函数执行统一经 dispatcher 分发。v1 的进程内 docker 执行器已移除，`functions.executor` 字段已 `reserved` 删除（残留的环境变量被忽略）。

| 键 | 默认 | 说明 |
|----|------|------|
| `functions.execution.api_base_url` | — | 函数执行回调基址 |
| `functions.dispatcher.url` | — | **必填**（server/worker 启动校验），dispatcher 服务地址，如 `http://dispatcher:9070` |
| `functions.dispatcher.shared_token` | — | 与 dispatcher 的共享令牌 |
| `functions.dispatcher.max_resident_instances` | 16 | resident 实例池上限（内存敞口 = 实例数 × spec 内存） |
| `functions.dispatcher.queue_depth` | 32 | 分发队列深度 |
| `functions.dispatcher.queue_head_timeout` | 10s | 队头等待超时 |
| `functions.dispatcher.boot_timeout` | 60s | 实例启动超时 |
| `functions.dispatcher.addr` | `:9070` | dispatcher HTTP 监听地址（仅内网） |
| `functions.dispatcher.callback_container` | — | 回调容器配置 |
| `functions.dispatcher.timeout_budget` | 5 | 超时预算 |
| `functions.trigger.http_ip_per_minute` | 3000 | HTTP 触发器每 IP 限频 |
| `functions.client_invoke.per_user_concurrency` | 8 | client 面每用户并发 |
| `functions.client_invoke.queue_head_timeout` | 10s | client 面队头超时（同步调用方不得无界等待） |

---

## 2. 运行时绑定（bind.go）

`internal/pkg/config/bind.go` 的 `ConfigureViper` 按以下顺序装配：

1. `lynx.DefaultBindConfigFunc` 设置 YAML 搜索路径，并追加 `extraPaths`（默认 `./configs`）；
2. `SetEnvPrefix("TORCHWOOD")` + `AutomaticEnv()`；
3. 反射遍历 `AppConfig` 的全部叶子 json tag 路径，对每个键显式 `BindEnv`——环境变量名由 `envNameForKey` 推导；
4. `UnmarshalConfig` 按 json tag 逐叶子 `c.Get(path)` 组装嵌套 map，再经 mapstructure 解码（支持弱类型转换、逗号分隔的 repeated 字段）。

`envNameForKey` 的映射规则：点号与连字符转下划线，整体大写，加 `TORCHWOOD_` 前缀：

```
data.database.source      →  TORCHWOOD_DATA_DATABASE_SOURCE
security.trusted_proxies  →  TORCHWOOD_SECURITY_TRUSTED_PROXIES
storage.s3.access_key_id  →  TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID
```

因为映射基于反射全集，**所有叶子键都可以被环境变量覆盖**，不限于下表的常用项。

---

## 3. 环境变量覆盖规则

- 前缀固定 `TORCHWOOD_`；键路径以 proto json tag 叶子为准。
- `repeated` 字段用逗号分隔（如 `TORCHWOOD_SECURITY_TRUSTED_PROXIES=127.0.0.1/32,10.0.0.0/8`）。
- 优先级：**环境变量 > config.yaml > proto/YAML 默认值**。

常用映射：

| 点号路径 | 环境变量 | 说明 |
|----------|----------|------|
| `security.jwt.secret` | `TORCHWOOD_SECURITY_JWT_SECRET` | 必填，≥32 字符，弱子串拒绝启动 |
| `security.encryption_key` | `TORCHWOOD_SECURITY_ENCRYPTION_KEY` | 静态加密密钥（OAuth/TOTP） |
| `security.setup_token` | `TORCHWOOD_SECURITY_SETUP_TOKEN` | 首个管理员引导令牌 |
| `security.trusted_proxies` | `TORCHWOOD_SECURITY_TRUSTED_PROXIES` | 逗号分隔 CIDR |
| `security.sessions.max_per_user` | `TORCHWOOD_SECURITY_SESSIONS_MAX_PER_USER` | 单用户并发会话上限（0→50，-1 不限） |
| `security.rate_limit.enabled` | `TORCHWOOD_SECURITY_RATE_LIMIT_ENABLED` | 总开关（显式 false 才关） |
| `security.rate_limit.functions_execution.*` | `TORCHWOOD_SECURITY_RATE_LIMIT_FUNCTIONS_EXECUTION_*` | 函数执行限流维度 |
| `security.login_throttle.*` | `TORCHWOOD_SECURITY_LOGIN_THROTTLE_*` | 认证失败频控四维度 |
| `data.database.source` | `TORCHWOOD_DATA_DATABASE_SOURCE` | PG DSN（worker 启动必填） |
| `data.redis.addr` / `password` / `db` | `TORCHWOOD_DATA_REDIS_*` | Redis 连接 |
| `storage.s3.endpoint` / `bucket` | `TORCHWOOD_STORAGE_S3_ENDPOINT` / `TORCHWOOD_STORAGE_S3_BUCKET` | 对象存储 |
| `storage.s3.access_key_id` / `secret_access_key` | `TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID` / `TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY` | MinIO 凭据 |
| `payments.*` | `TORCHWOOD_PAYMENTS_*` | 渠道密钥一律走环境变量 |
| `idgen.snowflake.node_id` | `TORCHWOOD_IDGEN_SNOWFLAKE_NODE_ID` | 雪花 ID 节点号 |
| `functions.dispatcher.*` | `TORCHWOOD_FUNCTIONS_DISPATCHER_*` | dispatcher 分发通路（见 §1.2） |
| `functions.trigger.http_ip_per_minute` | `TORCHWOOD_FUNCTIONS_TRIGGER_HTTP_IP_PER_MINUTE` | HTTP 触发器限频 |
| `functions.client_invoke.*` | `TORCHWOOD_FUNCTIONS_CLIENT_INVOKE_*` | client 面调用并发与队头超时 |
| `analytics.retention_days` | `TORCHWOOD_ANALYTICS_RETENTION_DAYS` | 事件保留天数（默认 90，域 7–365） |

MinIO 凭据的变量名（`ACCESS_KEY_ID` / `SECRET_ACCESS_KEY`）由字段名直接映射而来，`bind.go` 推导、`config.yaml.template` 注释与 `.env.example` 三处一致。

---

## 4. TORCHWOOD_ENV 与关停排水

`TORCHWOOD_ENV` 与 `TORCHWOOD_SERVER_DRAIN_TIMEOUT` 不在 config.proto 里——Lynx 在绑定 YAML 之前就需要排水超时值，所以这两个变量在 `internal/pkg/config/runtime_env.go` 中于 `lynx.NewRunner` 之前直接读取。

环境归一化与默认排水窗口：

| `TORCHWOOD_ENV` 取值 | 归一化 | 默认排水窗口 |
|----------------------|--------|--------------|
| `development` / `dev` / `local` / `test` | development | 0（跳过排水） |
| `production` / `prod` / `staging` / 空 / 未知 | production | 30s |

`TORCHWOOD_SERVER_DRAIN_TIMEOUT` 配置合法非负 duration（如 `0s`、`5s`、`30s`；裸 `0` 有特判）时覆盖默认值，非法或负值回退默认。生产部署应保证容器的 `terminationGracePeriodSeconds ≥ DrainTimeout + ShutdownTimeout(30s) + StopTimeout`。

---

## 5. 配置文件与加载顺序

| 文件 | 角色 |
|------|------|
| `configs/config.yaml.template` | 完整键与默认值的模板，敏感键注释标注对应环境变量 |
| `configs/config.yaml` | 本地实际配置（已 gitignore），从模板复制修改 |
| `.env` / `.env.example` | 环境变量载体；敏感信息一律走环境变量，不进 YAML |

`cmd/server/main.go`（`cmd/worker` 等入口同构）的启动顺序：

```
godotenv.Load()                            1. 加载 .env（文件不存在则容忍跳过）
lynx.NewRunner(
  --config-dir ./configs                   2. 配置目录 flag（默认值）
  WithBindConfigFunc(NewBindConfigFunc)    3. viper 绑定 + 环境变量覆盖
  WithDrainTimeout(CurrentDrainTimeout)    4. 排水窗口（早于 YAML 绑定求值）
)
→ wireBootstrap → NewAppConfig → UnmarshalConfig → 启动校验
```

启动校验按进程而不同：server 校验 `security.jwt.secret`；worker 校验 `data.database.source`（`cmd/worker/provides.go`）。

---

## 6. 特殊配置项

### 6.1 trusted_proxies

`security.trusted_proxies` 声明可信反向代理的 CIDR 网段（裸 IP 按 `/32` 或 `/128` 处理）。仅当 gRPC 直连 peer 地址命中可信网段时，才采纳 `X-Forwarded-For` 首跳或 `X-Real-Ip` 作为客户端地址；否则一律使用 peer 地址。默认为空 = 不信任任何代理，防止伪造头绕过限流与审计。gateway 与 gRPC 同进程部署时，需包含 `127.0.0.1/32`。

```yaml
security:
  trusted_proxies: ["127.0.0.1/32"]
```

### 6.2 Console 会话 cookie

实现位于 `internal/api/consolegrpc/cookies.go` 与 `internal/app/console/auth.go`：

- 访问 cookie `TORCHWOOD_session_console`（`Path=/`）+ 刷新 cookie `TORCHWOOD_console_refresh`（`Path=/v1/console/auth`，限定刷新路径）。
- 均为 `HttpOnly` + `SameSite=Lax`。跨站 POST 不携带 cookie，因此无需额外 CSRF token；前端不使用 localStorage 存 token。
- 仅当 `server.http.public_url` 以 `https://` 开头时附加 `Secure`。
- 刷新请求体中的 `refresh_token` 为空时走 cookie-only 流程；`SignOut` 以 `Max-Age=0` 清除 cookie；refresh 带 rotation 与重用检测——检测到 `RotateMismatch` 时撤销该管理员全部 token。

### 6.3 测试 DSN

`TORCHWOOD_TEST_DATABASE_SOURCE` 与 `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE` 不属于 `AppConfig`，由 `internal/pkg/testutil/db.go` 直接 `os.Getenv` 读取。每个集成测试创建独立的 `TORCHWOOD_test_<pid>_<seq>` 隔离库；`mise run test` 自动从 `.env` 加载这些变量；`go test -short` 时跳过集成测试。

---

## 相关文档

- `04-codegen.md` — config.pb.go 的生成流程
- `02-quickstart.md` — 本地 `.env` 最小可用配置
- `13-operations.md` — 生产配置要点与双账号契约
