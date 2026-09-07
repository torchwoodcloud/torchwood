# Torchwood 配置体系

> `config.proto` 为单一事实源，`bind.go` 完成 `TORCHWOOD_` 环境覆盖。以代码为准：`internal/pkg/config/config.proto`、`internal/pkg/config/bind.go`、`internal/pkg/config/runtime_env.go`、`configs/config.yaml.template`、`.env.example`。
> 最新更新：2026-08-23

---

## 1. config.proto 单一事实源

`internal/pkg/config/config.proto:7` 定义顶层 `AppConfig`，proto 生成 `config.pb.go`（`task generate:config`，见 `04-codegen.md §3`），避免 YAML 与结构体两处维护。

| 分组 | message | 说明 |
|------|---------|------|
| `server` | `Server` | `grpc` / `http` / `metrics` 三监听 |
| `security` | `Security` | `jwt` / `api_key` / `trusted_proxies` / `setup_token` / `sessions` / `rate_limit` / `encryption_key` |
| `data` | `Data` | `database`（DSN/池/慢查询）+ `redis` |
| `storage` | `Storage` | `s3` / `local` |
| `functions` | `Functions` | `docker.host`/`network`/`registry` |
| `payments` | `Payments` | `stripe`/`wechat`/`alipay`/`ios_iap` 渠道密钥（仅环境变量） |
| `telemetry` | `Telemetry` | OTLP |
| `messaging` | `Messaging` | SMTP / SMS(Twilio) / `dev_log_*` |
| `idgen` | `IdGen` | `uuid`/`ulid`/`snowflake`/`sequence`/`random` |

**关键字段选摘**

- `server.grpc.addr` 默认 `127.0.0.1:9060`（仅回环供 gateway 转发）；`server.http.addr` `:9080`；`server.metrics.addr` 空回退 `127.0.0.1:9040`（`internal/runtime/metrics.go`）；`server.http.public_url` 决定 OAuth 回调与 cookie `Secure`；
- `security.jwt.secret` **必填**，启动校验 ≥32 字符且不含弱子串（`internal/bootkit/config.go`，server/worker 共享）；`security.encryption_key` 独立静态加密密钥，未配回退 `jwt.secret` 并告警（`internal/pkg/config/crypto.go:10`）；
- `security.setup_token` 空则首个管理员注册 `FailedPrecondition`（`internal/app/console/setup.go`）；
- `security.trusted_proxies` `repeated string` CIDR（逗号分隔环境覆盖，见 §4）；
- `security.rate_limit` `optional bool enabled`（默认 true）+ 三维度 `ip`/`user`/`api_key` 固定窗口；
- `data.database.slow_query_threshold` 空=500ms，`0`=禁用。

关停排水窗口不在 proto：`TORCHWOOD_ENV` + `TORCHWOOD_SERVER_DRAIN_TIMEOUT` 在 Lynx `NewRunner` 前读取（`internal/pkg/config/runtime_env.go:12`）。

---

## 2. 运行时绑定（bind.go）

`internal/pkg/config/bind.go:21` 的 `ConfigureViper` 流程：

1. `lynx.DefaultBindConfigFunc` 设搜索路径；
2. 追加 `extraPaths`（默认 `./configs`）；
3. `SetEnvPrefix("TORCHWOOD")` + `AutomaticEnv()`；
4. 反射 `configKeys()` 遍历 `AppConfig` 全部叶子 `json tag` 路径，对每键 `BindEnv(key, envNameForKey(key))`（显式绑定，替代旧 `SetEnvKeyReplacer`）；
5. `UnmarshalConfig` 按 `json` Tag 逐叶子 `c.Get(path)` 组装嵌套 map，再 `mapstructure` 解码（`TagName=json`、`WeaklyTypedInput`、`StringToSliceHookFunc(",")` 支持逗号分隔的 `repeated`）。

`envNameForKey`（`bind.go:14`）：`.`/`-` → `_`，整体大写，加 `TORCHWOOD_` 前缀：

```
"data.database.source"        → TORCHWOOD_DATA_DATABASE_SOURCE
"security.trusted_proxies"    → TORCHWOOD_SECURITY_TRUSTED_PROXIES
"storage.s3.access_key_id"    → TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID
```

> `envBoundKeys` 仅注释说明，实际以反射全集为准——**所有叶子键均可被环境变量覆盖**。

---

## 3. 环境变量覆盖规则

- 前缀固定 `TORCHWOOD_`；
- 键路径以 proto `json tag` 叶子为准（非 YAML 注释）；
- `repeated` 用逗号分隔（如 `127.0.0.1/32,10.0.0.0/8`）；
- 优先级：环境变量 > `config.yaml` > 默认值。

**常用映射表**

| 点号路径 | 环境变量 | 说明 |
|----------|----------|------|
| `security.jwt.secret` | `TORCHWOOD_SECURITY_JWT_SECRET` | 必填，≥32 字符，弱子串拒绝 |
| `security.encryption_key` | `TORCHWOOD_SECURITY_ENCRYPTION_KEY` | 静态加密密钥（OAuth/TOTP secretbox） |
| `security.setup_token` | `TORCHWOOD_SECURITY_SETUP_TOKEN` | 首个管理员引导令牌 |
| `security.trusted_proxies` | `TORCHWOOD_SECURITY_TRUSTED_PROXIES` | 逗号分隔 CIDR |
| `security.sessions.max_per_user` | `TORCHWOOD_SECURITY_SESSIONS_MAX_PER_USER` | 单用户并发会话上限（0→50，-1 不限） |
| `security.rate_limit.enabled` | `TORCHWOOD_SECURITY_RATE_LIMIT_ENABLED` | 总开关（显式 false 才关） |
| `data.database.source` | `TORCHWOOD_DATA_DATABASE_SOURCE` | PG DSN（worker 必填） |
| `data.redis.addr`/`password`/`db` | `TORCHWOOD_DATA_REDIS_*` | Redis |
| `storage.s3.endpoint`/`bucket` | `TORCHWOOD_STORAGE_S3_ENDPOINT`/`BUCKET` | 对象存储 |
| `storage.s3.access_key_id`/`secret_access_key` | `TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID`/`SECRET_ACCESS_KEY` | MinIO 凭据（与 `AGENTS.md` 一致） |
| `payments.stripe.secret_key` 等 | `TORCHWOOD_PAYMENTS_*` | 渠道密钥一律环境变量 |
| `idgen.snowflake.node_id` | `TORCHWOOD_IDGEN_SNOWFLAKE_NODE_ID` | 雪花节点 |

MinIO 凭据变量名由字段 `access_key_id`/`secret_access_key` 映射而来，三处一致：`bind.go` 推导、`configs/config.yaml.template:82` 注释、`.env.example:23`。

---

## 4. TORCHWOOD_ENV 与排水

`internal/pkg/config/runtime_env.go:28` 归一化：

| `TORCHWOOD_ENV` | 归一化 | 默认 `DrainTimeout` |
|-----------------|--------|---------------------|
| `development`/`dev`/`local`/`test` | `development` | `0`（跳过排水） |
| `production`/`prod`/`staging`/空/未知 | `production` | `30s` |

`TORCHWOOD_SERVER_DRAIN_TIMEOUT` 合法非负 `duration`（如 `0s`/`5s`/`30s`，裸 `0` 特判）时覆盖默认；非法/负值回退。`cmd/server/main.go:23` 在 `lynx.NewRunner` 前 `config.CurrentDrainTimeout()` 求值，本地 `.env.example` 默认 `development`，生产应保证 `terminationGracePeriodSeconds ≥ DrainTimeout + ShutdownTimeout(30s) + StopTimeout`。

---

## 5. 文件与加载顺序

| 文件 | 角色 |
|------|------|
| `configs/config.yaml.template` | 完整键与默认值展示，敏感键注释环境变量 |
| `configs/config.yaml` | 本地实际配置（gitignore），从模板复制 |
| `.env` / `.env.example` | 环境变量载体，敏感信息一律走环境（不进 YAML） |

`cmd/server/main.go:18`（`cmd/worker/main.go` 同构）：

```
godotenv.Load()                         # 1. 加载 .env（失败容忍）
lynx.NewRunner(
  --config-dir ./configs                # 2. flag 默认
  WithBindConfigFunc(NewBindConfigFunc) # 3. 绑定 viper + 环境
  WithDrainTimeout(CurrentDrainTimeout) # 4. 排水（早于 YAML）
)
wireBootstrap → NewAppConfig → UnmarshalConfig → 校验 jwt.secret/encryption_key fallback
```

`server` 校验 `jwt.secret`，`worker` 校验 `data.database.source`（`cmd/worker/provides.go:108`）。

---

## 6. 特殊项

### 6.1 trusted_proxies

`security.trusted_proxies` 声明可信代理 CIDR（裸 IP 按 `/32`/`/128`）。仅当 gRPC 直连 `peer` 命中网段时才采纳 `X-Forwarded-For` 首跳或 `X-Real-Ip`，否则用 peer 地址（`internal/api/interceptor/trusted_proxy.go`）。默认空 = 不信任任何代理，防伪造绕过限流/审计；gateway 与 gRPC 同进程部署需含 `127.0.0.1/32`。

```yaml
security:
  trusted_proxies: ["127.0.0.1/32"]  # env: TORCHWOOD_SECURITY_TRUSTED_PROXIES=127.0.0.1/32,10.0.0.0/8
```

### 6.2 Console 会话 cookie

`internal/api/consolegrpc/cookies.go` + `internal/app/console/auth.go`：

- `TORCHWOOD_session_console`（`Path /`）+ `TORCHWOOD_console_refresh`（`Path /v1/console/auth` 限刷新路径）；
- `HttpOnly` + `SameSite=Lax`（跨站 POST 不携带，无需额外 CSRF；前端不存 localStorage）；
- `Secure` 仅当 `server.http.public_url` 以 `https://` 开头；
- `RefreshTokenRequest.refresh_token` 为空走 cookie-only 流；`SignOut` 以 `Max-Age=0` 清除；refresh 带 rotation + 重用检测（`RotateMismatch` → 撤销该 admin 全部 token）。

### 6.3 测试 DSN

`TORCHWOOD_TEST_DATABASE_SOURCE` / `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE` **不在 `AppConfig`**，由 `internal/testutil/db.go` `os.Getenv` 直读：每个测试建独立 `TORCHWOOD_test_<pid>_<seq>` 库，`task test` 自动从 `.env` 加载，`testing.Short` 时跳过集成测试。

---

## 7. 参考

- `internal/pkg/config/config.proto` schema 唯一定位
- `internal/pkg/config/bind.go` 绑定与解码
- `internal/pkg/config/runtime_env.go` 环境与排水
- `internal/pkg/config/crypto.go:10` 独立加密密钥
- `cmd/server/provides.go:55` / `cmd/worker/provides.go:108` 启动校验
- `configs/config.yaml.template`、`.env.example`
