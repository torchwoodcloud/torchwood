# Torchwood Round 4 修复方案

> 依据：`docs/review/round4/audit-report.md`（主代理交叉核实后的结论）
> 基线：`main` @ `3398a26`（行号会漂移，实施时用 Grep / 读文件重新定位）
> 本方案是实施的**唯一事实来源**。审计报告中标「不修」或归档 P3 的项不得自行升级回来；需产品拍板的项见 §9，拍板前不得开工对应条目。

---

## 0. 批次总览

| 批次 | 名称 | 级别 | 依赖 | 文件冲突风险 |
|------|------|------|------|--------------|
| **J1** | 紧急修复四件套 | P1 | 无 | 低 |
| **J2** | 契约与机器可读性 | P1 | 无（可与 J1 并行） | 中（J2 与 J3 都碰 swagger 相关测试） |
| **J3** | SDK/CLI 交付链路 | P1 | 无 | 中 |
| **J4** | 架构收口 | P1+P2 | 建议 J1-3 合入后动工 | 高（碰 wire/provides，需 task wire-all） |
| **J5** | 可靠性与租户生命周期 | P1+P2 | 无 | 中 |
| **J6** | 测试与门禁加固 | P1+P2 | 无（建议最后合以免长期 rebase） | 低 |
| **J7** | P3 卫生批 | P3 | 全部前序合入后 | 低 |

并行约定：J1/J2/J3/J5/J6 文件集基本无重叠，可按矩阵并行；J4 涉及 Wire 与包移动，必须串行独占工作区。每批次完成即跑 `task test` + `task build`（Console 相关先 `task console-build`）。

**本轮不修（明确排除）**

- pkg/semaphore 的 go-redis 依赖、domain 空 ProviderSet、app 层 grpc/status 绑定（56 文件）——列为已接受的结构取舍，仅在 AGENTS.md 补一句约定说明（J7 一并做）
- 集成测试「每测试建库重放迁移」性能优化
- i18n 全量改造、MCP server 实现、TS SDK AST 封装、按项目批量 replay（spec 明确 out-of-scope）、CLI table 输出模式
- proto 大规模 enum 化迁移（仅 J7 做规范补充与两处 reserved 补登）

---

## 1. J1 紧急修复四件套

### J1-1 config.yaml.template 结构修正

- 位置：`configs/config.yaml.template:23-29`
- 步骤：
  1. `access_ttl` / `refresh_ttl` 移回 `jwt:` 块内（缩进对齐 `secret:`）；
  2. `encryption_key` 注释改为「可选；未配置时回退 jwt.secret（启动告警）。建议配置独立密钥 ≥32 字符」——与 `internal/pkg/config/config.proto` 实际语义一致；
  3. 新增 CI step：用脚本对 template 做 YAML 解析断言（Go 一段 `yaml.Unmarshal` 或 python -c），防回归。
- 验收：template 可被标准解析器解析且键结构与 config.proto 对齐；CI 有防回归校验。

### J1-2 encryption_key 校验兑现

- 位置：`cmd/server/provides.go:60-63`（调用点）、复用 `validateJWTSecret` 风格
- 步骤：
  1. 当显式配置 `security.encryption_key` 时套用与 JWT secret 同强度规则：min-len 32 + 弱词黑名单，不合规启动报错并带 env 名 `TORCHWOOD_SECURITY_ENCRYPTION_KEY`；
  2. 回退路径保持现状（warn 即可）；
  3. 单测覆盖：显式弱值拒绝 / 显式强值通过 / 回退路径 warn。
- 验收：模板注释、config.proto 注释、代码行为三者一致。

### J1-3 worker cleanup 时机对齐

- 位置：`cmd/worker/main.go:31-34`
- 步骤：
  1. 删除 `app.OnStop(func...)` 注册，改为 `runner.RunE()` 返回后执行 cleanup，带 10s 超时上限——逐字复制 `cmd/server/main.go:51-64` 模式（含注释）；
  2. 顺手修 J7 的 version 元数据（`_ = commit; _ = date` → 拼入 Version）。
- 验收：worker 关停期间在途任务不再因连接池先行关闭而写库失败；`go vet ./...` 干净。
- 注意：lynx Runner 的 Run vs RunE 差异以 server main 为准照搬。

### J1-4 roadmap 状态同步

- 位置：`docs/roadmap.md:4,29,53`
- 步骤：v3 经济系统状态改「已实施」（列出 payments/subscriptions/assets/billing 迁移与 proto 证据）；§0 Realtime 从「规划中」表移入「已具备」表；文首日期更新。
- 验收：roadmap 与代码现状无矛盾（对照 audit-report §H-P1-2/§H-P2-3 清单逐项核对）。

---

## 2. J2 契约与机器可读性（Agent-Native 主线）

### J2-1 OpenAPI 字段命名统一（产品决策 D-2 已拍板：文档侧改 snake_case）

- 位置：`buf.gen.yaml:21`
- 步骤：
  1. 移除 openapiv2 插件的 `json_names_for_fields=true`，使 swagger 字段名与运行时 marshaler（`errors.go:120 UseProtoNames:true`）一致为 snake_case；
  2. `task generate-proto` 重生成；
  3. grpc_swagger_test 增加一条命名一致性抽断言（取任一 operation 的 property 名必须 snake_case）。
- 理由：请求方向 protojson 双名兼容故客户端无感；响应方向现状每个字段都与文档不符。零运行时代码改动。
- 验收：抽任意 swagger response property 与实际 HTTP JSON 响应字段名一致。

### J2-2 OpenAPI 错误模型统一为 shared.v1.ErrorResponse

- 位置：swagger 生成链（openapiv2 插件选项 / service 级 `openapiv2_operation` responses）+ `internal/infra/server/grpc_swagger_test.go`
- 步骤：
  1. 为 default response 统一引用 `sharedv1.ErrorResponse` 定义（优先在 buf.gen.yaml 模板层注入，避免手改上百处 proto）；
  2. grpc_swagger_test 增加：所有 operation 的 default response $ref 必须指向 ErrorResponse（禁止 rpcStatus 出现）；
  3. 重生成并确认 `rpcStatus` 不再出现在任何 swagger.json。
- 验收：Agent 按 OpenAPI 解析错误体可拿到 type/code/message/error_id。

### J2-3 presence 语义落地（产品决策 D-1 已拍板：实现 Has*，契约保持「设置空串=清空」）

- 位置：`internal/api/servergrpc/users.go:103,112,115`、`internal/api/clientgrpc/account.go:102-103`、`internal/app/client/account.go:410,416` 及同类 update handler（实施时 grep `Get.*\(\) != ""` 于 Update 方法内逐一甄别）
- 步骤：
  1. handler 改用 `req.HasXxx()`（proto3 optional 生成方法）判断 presence，空串也写入 updates；
  2. app 层 Command 结构相应字段改指针或显式 Set 位；
  3. 对照正确范本 `servergrpc/projects.go:87-92`；
  4. 补集成测试：PATCH `{"name": ""}` 后 name 为空且返回 200；未设置 name 时不变。
- 验收：users/account 更新类接口「未设置=不改、设置含空串=更新/清空」全链路成立且有测试锁定。

### J2-4 signed page token 接线 + offset 上限统一

- 位置：`pkg/crud/pagination.go`（能力已有）、生产调用点（grep `EncodePageToken(` 共 ~14 处）、`pkg/crud/list.go` ParseListParams、`internal/infra/bun/bunrepo/outbox_repo.go:34`
- 步骤：
  1. 生产路径 `EncodePageToken(offset)` 全部切换为 signed token（注入 signing secret，来源与 jwtparser 派生体系一致的专用 purpose key）；legacy `v1:<offset>` 解码保留但标记 deprecated 并打 warn 日志一个版本周期后移除；
  2. ParseListParams 校验 token 内 order_by/filterDigest 与本次请求一致，不一致返回 InvalidArgument（文档声称的语义由此变为真实）;
  3. 统一 MaxOffset clamp（建议 10000，与 documentdb maxQueryOffset 对齐）到 ParseListParams 单点，outbox dead-letter 列表随之受控；
  4. 修订 `docs/developer/09-api-guide.md`：分页防护描述改为与实现一致；权威示例（guide:136-138）改为 storage ListBuckets 实际支持的形态（不带 filter/order_by）或改用 documents 示例。
- 验收：篡改 filter/order_by 后复用旧 token 返回 InvalidArgument；伪造深 offset token 被拒；guide 示例 curl 实测 200。

### J2-5 grpc_swagger_test 增强

- 位置：`internal/infra/server/grpc_swagger_test.go:142-194`
- 步骤：
  1. 反向覆盖率断言：collectMethodsByAccess 得到的每个方法必须在 swagger paths 出现 ≥1 次（含 additional_bindings）——拦截 RPC 缺 http 注解静默消失；
  2. security 匹配断言：access=PUBLIC ⇒ operation.security 为空数组；非 PUBLIC ⇒ 引用 ApiKeyAuth/securityDefinitions 存在；
  3. findMethodByOperationID 数字截断回退收紧为精确匹配 + 显式失败。
- 验收：人为删掉一个方法的 http 注解 / 错配 security 时测试红。

---

## 3. J3 SDK/CLI 交付链路

### J3-1 发布流水线

- 位置：`.github/workflows/`（新增 release workflow）、`sdk/go/go.mod`
- 步骤：
  1. release job：以 genproto tag 为输入，临时改写 sdk/go/go.mod 的 require 为真实 genproto 版本（去掉 replace 生效问题——replace 仅主模块生效，下游拉取时失效），打 `sdk/go/vX.Y.Z` tag；genproto 同理出 tag；
  2. 建 CHANGELOG.md（sdk/genproto 两节）。
- 验收：干净外部目录 `go get github.com/torchwoodcloud/torchwood/sdk/go@latest` 成功可编译。

### J3-2 SDK 默认超时与可选重试

- 位置：`sdk/go/internal/conn/conn.go:17-22`、`sdk/go/server/client.go`、`sdk/go/client/client.go`
- 步骤：
  1. Option 增加 `WithTimeout(default 30s)`，per-call interceptor 兜底注入 deadline（已有 ctx deadline 则尊重）；
  2. gRPC service config 暴露默认 retryPolicy（对 Unavailable 幂等读重试），提供 WithRetryDisabled 关闭。
- 验收：不传 ctx 的调用 30s 必返回；单测模拟慢服务验证。

### J3-3 CLI 自诊断与退出码

- 位置：`cmd/client/cmd/output.go:63-68`、`cmd/client/cmd/outbox.go:52`、`cmd/client/main.go:24-27`
- 步骤：
  1. formatRPCError 对 Unauthenticated 追加提示（检查 TORCHWOOD_CLI_API_KEY 是否设置/过期指引）；
  2. 删除 outbox help 中幽灵 `--project` 文案；
  3. 退出码映射：OK=0、参数错=1、40x=2、5xx=3、ResourceExhausted=4；更新 output_test 固化。
- 验收：无效 key 报错含下一步动作指引；脚本可按退出码分支。

### J3-4 SDK 类型化封装完整性测试

- 位置：`sdk/go/server/invoke_test.go:48-74`
- 步骤：
  1. 新增反射断言：registry 每个 server 方法 → Client 对应 Service wrapper 存在同名词导出方法（拦住类型化面静默滞后）；
  2. count 下限从 `> 60` 收紧为精确快照数（当前 ~112）+ 注释说明更新流程。
- 验收：新增 RPC 未加 wrapper 时测试红。

### J3-5 文档示例可编译化

- 位置：`docs/developer/12-sdk.md:196` 等
- 步骤：建 `//go:build docexample` 的 example 测试承载文档代码片段（至少 12-sdk 全部示例）；修正 ListDeadLetters 签名错误；README 示例一并纳入。
- 验收：`go test -tags docexample ./...` 绿。

### J3-6 限流 RetryInfo 与 SDK 错误 helper

- 位置：`internal/grpc/interceptor/ratelimit.go:106-108`、`sdk/go/server/errors.go`
- 步骤：ResourceExhausted 附 `google.rpc.RetryInfo`（剩余窗口秒数）；SDK 提供 ExtractRetryAfter helper。
- 验收：被限流响应携带 detail 且 SDK 可读出退避秒数。

---

## 4. J4 架构收口（串行执行，涉及 wire-all）

> 执行顺序即下列编号顺序；每步完成后 `task wire-all && go build ./... && task test` 再进行下一步。

### J4-1 worker/server 公共装配去重

- 位置：`cmd/server/provides.go:143-161` ≈ `cmd/worker/provides.go:129-147`；NewAppConfig/NewLogger/NewComponents/NewComponentBuilders/NewOnStops 四件套
- 步骤：抽 `internal/pkg/bootkit` 放 projectSchemaEnsureHook 与 AppConfig 公共校验；NewAppConfig 的 JWT 校验差异（server 有 worker 无）显式注释或补齐（推荐补齐——worker 也签发/校验 token 场景防御性更强，若确属有意差异写明原因）。

### J4-2 test_helpers 隔离

- 位置：`internal/app/client/test_helpers.go`
- 步骤：改名 `test_helpers_test.go` 并确认其消费方均在 _test 文件中；若有生产引用则将引用方改为构造真实依赖注入。
- 验收：`go list -deps ./internal/app/client` 不再包含 infra/{bun/bunrepo,clients,messaging,auth}。

### J4-3 OAuth/OTP 端口化

- 位置：`internal/domain/auth/`（新增端口）、`internal/infra/auth/`（实现）、`internal/app/client/oauth2.go:168,286`、`wechat.go:43`、`email_otp.go:69`、`phone_otp.go:62`、Wire provider
- 步骤：domain/auth 定义 `OAuthAuthenticator`（含 Factory by provider）、`WeChatCodeExchanger`、`OTPGenerator` 三端口；infra 实现；Account 结构加依赖字段（21 个依赖的上帝构造器问题不在本轮扩大处理，只按既有模式追加）。
- 验收：`go list -deps ./internal/app/client` 中 infra/auth 归零；oauth2 用例可用 fake 端口单测。

### J4-4 组装根迁出 infra

- 位置：`internal/infra/server/*` → `internal/runtime/`（新包）；`internal/infra/provides.go:85-87` 相应摘除；wire 全量重生成
- 步骤：git mv 保历史；import 路径替换；ProviderSet 重组（runtime.ProviderSet 收纳 NewGRPCServer/NewGRPCGatewayServer/NewMetricsServer/NewConsoleHandler）；`task wire-all`。
- 验收：`go list -deps ./internal/infra` 不含任何 internal/api 包；server/worker 启动行为无变化（冒烟：healthz、一条业务 RPC、console 页面）。

### J4-5 scope 策略表迁 domain/auth + api 层窄接口化

- 位置：`internal/grpc/interceptor/apikey_scope.go`（策略表+判定函数）→ `internal/domain/auth/`（或 internal/pkg/authz）；api→infra 11 处窄接口化仿 `realtime/handler.go CredentialValidator` 模式
- 步骤：策略表迁移保持 fail-closed 断言随迁；serverhttp/servergrpc 定义 `Authenticator`/`AdminProjectAccessChecker`/`HealthCheckers` 最小接口，具体类型只在 Wire 组合根出现。
- 验收：app/api 不再 import interceptor（除 interceptor 自身装配）；`AssertAPIKeyScopeCoverage` 仍生效。

### J4-6 SQLSTATE 映射下沉

- 位置：`internal/app/shared/docdb_errors.go` → `internal/infra/documentdb/errors.go`
- 步骤：adapter 将 pgdriver 错误翻译为领域哨兵（databases.ErrVersionMismatch 等）后再上抛；app 只做 domain→status 单向映射；删除 docdb_errors.go。
- 验收：`go list -deps ./internal/app/shared` 无 pgdriver 依赖；现有 OCC/冲突测试全绿。

---

## 5. J5 可靠性与租户生命周期

### J5-1 限流降级策略（产品决策 E-1 默认方案：熔断降级 + 观测分离）

- 位置：`internal/grpc/interceptor/ratelimit.go`、`internal/infra/auth/ratelimit_redis.go`
- 步骤：
  1. limiter 连续失败 N 次（建议 5 次/10s 窗口）进入短窗熔断放行（默认 30s）+ error log/metric 告警，半开探测恢复；
  2. metrics 区分 `ratelimit_infra_error_total` 与 `ratelimit_rejected_total`；
  3. 登录/MFA 等 auth 强依赖路径维持 fail-closed 不变（安全取向），但在部署文档声明 Redis=登录面强依赖、需 HA。
- 验收：单测模拟 Redis 宕机：业务 RPC 短暂失败后放行且告警指标递增；恢复后回到正常限流。

### J5-2 DeleteProject 对象存储 purge

- 位置：`internal/app/server/projects.go:133-168`（事务提交后）、`internal/app/storage/storage.go:172-212`（复用清尾逻辑）
- 步骤：DeleteProjectInternal 事务成功提交后异步（goroutine + 超时上限 + 重试一次 + 失败告警日志）执行 MinIO `ListObjects(prefix={projectID}/)` 分批 Delete；purge 失败不影响删除结果但必须留可追踪日志（error_id 关联）。
- 验收：集成测试删除项目后断言共享桶无该前缀残留对象。

### J5-3 internalIDCache 失效接线

- 位置：`internal/infra/documentdb/postgres_catalog.go:225-239`、`internal/infra/projectschema/migrator.go Invalidate`
- 步骤：SchemaManager.Invalidate 同时清除 documentdb internalIDCache（经回调或共享 invalidator 注入，避免反向依赖）。
- 验收：测试删除重建同 ID 项目后新实例写入 `_tenant` 为新 internal_id。

### J5-4 Functions per-project 网络

- 位置：`internal/infra/functions/docker.go:245-265`、`configs/config.yaml.template:91`
- 步骤：默认改为 per-project network（`tw-func-<project.id>`，id 已过 ident 白名单可直接用作网络名后缀，容器创建时 ensure network）；全局 network 配置保留为显式 opt-in 并在模板注释警告跨租户互通风险。
- 验收：项目 A 函数容器无法解析/连通项目 B 函数容器 IP（集成测试网络隔离断言）。

### J5-5 其余韧性小件打包

- redis.NewClient：addr 必填校验（缺失报错带 env 名，仿 database source 范本）+ DialTimeout(5s)/池参数显式化（clients/database.go:36-43）
- 连接池 duration 解析失败打 Warn（database.go:97-101）
- nil loginThrottle/mfaChallenges 分支改为显式 Noop 类型并注明仅测试可用（account.go:306-308、mfa.go:37-39）
- realtime broadcast published_at 标记攒批（200ms 或 32 条批量 UPDATE，subscriber.go:117-123）
- semaphore release Eval 加 2s 超时（pkg/semaphore/semaphore.go:85-89）
- realtime 集合频道订阅补 read 权限判定（handler.go:575-582，语义对齐 REST List）
- DeleteProject 前 schema 对账：information_schema `tw_<p>_%` vs catalog 清单，差异项 DROP+告警（projects.go:137-149）

---

## 6. J6 测试与门禁加固

### J6-1 CI coverage 门禁

- ci.yml go test 加 `-coverprofile` + 合并报告 + diff 新增代码最低覆盖率阈值（建议 70% 起步）；Taskfile 本地补 -race 对齐 CI。

### J6-2 down 迁移验证

- 新增集成测试：up→down(全部)→up 循环跑 db/migrations 22 组；CI 复用现有 Postgres service container。
- 验收：任何 down SQL 损坏 CI 红。

### J6-3 linter 扩容与棘轮收紧

- .golangci.yml 启用 gosec/bodyclose/sqlclosecheck/noctx；存量已清零（43→0），移除 `--new-from-rev` 棘轮改为全量门禁；同步更新 ci.yml:86-87 与 Taskfile.yml:169-171 过期注释。

### J6-4 flaky 治理

- storage/file_handler 两处 TTL sleep 测试注入 clock（storage UC 加 clock 接口，测试拨快）；realtime/health 固定 sleep 改 eventually-poll helper（testutil 提供 Eventually(t, timeout, fn)）。

### J6-5 守卫与小件

- import_guard_test 改 filepath.WalkDir 递归；functions docker e2e 在 CI 增加 executed 断言 step（skip 数=0）；testutil fixture 增加 CreateTestProjectT(t,...) 变体替代 panic。

---

## 7. J7 P3 卫生批（前序全部合入后执行）

| # | 事项 | 位置 |
|---|------|------|
| 1 | console admin access TTL 默认收紧 ≤1h（cookie Max-Age 同步） | app/console/auth.go:264-267 |
| 2 | 公开消费口 IP 维度频控 + locked 保留至 TTL 不 DEL | account_token_redis.go:134-138、proto PUBLIC 入口 |
| 3 | Internal/Unknown 错误对外统一文案，原文只进日志 | infra/server/errors.go:19-21 |
| 4 | X-Torchwood-Project 多值拒绝 | interceptor/jwt.go:135、serverhttp/auth.go:45 |
| 5 | CORS 反射 origin 必设 Vary;Allow-Credentials 仅随匹配 origin 输出 | infra/server/cors.go:32-46 |
| 6 | KDF 收敛单一入口（DeriveKey HMAC-SHA256 purpose 派生），secretbox/otp 迁移 | crypto.go/secretbox.go/otp_store_redis.go |
| 7 | AGENTS.md/proto 规范补「deprecated 保留为合法过渡态」条款；entities.proto Subscription 补 reserved 11,12,15；storage permissions 迁 reserved | entities.proto:69-86、storage.proto:147-148 |
| 8 | total_count 语义注释（≤0=未知）写入 common.proto 与 09-api-guide | common.proto:19 |
| 9 | CountDocuments 独立 Request；ListLogs 补 page_size/page_token/meta；ListAdmins 分页（走 generate-proto） | databases.proto、account.proto:753、admins.proto:107 |
| 10 | Console 缓存头策略：index.html no-cache、assets immutable、资源 404 不回退 index.html | infra/server/console.go:18-47 |
| 11 | functions queryKey 详情键改 ["functions", projectId, functionId] | routes/functions/pages.tsx:114,428 |
| 12 | databases/pages.tsx 拆分为按页面文件 | console/src/routes/databases/ |
| 13 | console/dist/index.html 占位入库（! 反向忽略） | .gitignore、console/embed.go |
| 14 | UI 文案统一语言基调（中文为准，toast/Login 页对齐） | Login.tsx、client.ts、useAuth.tsx |
| 15 | SDK 分页迭代器（AIP-158 pager）+ storage multipart helper（复用 FileHandler 路径） | sdk/go/server/databases.go:216、storage.go |
| 16 | Tool catalog 附 JSON Schema（由 descriptor 生成 inputSchema 字段） | sdk/go/server/tools.go:34-38 |
| 17 | RPC 计数脚本化写入文档；12-sdk/14-agent-tools 计数修正 | docs/developer/14-agent-tools.md:127 |
| 18 | AGENTS.md 补两条已接受取舍说明：app 层允许 grpc/status；SDK 覆盖测试保证范围措辞收敛 | AGENTS.md |

---

## 8. 文件冲突矩阵（并行排障用）

| 批次间重叠 | 说明 |
|------------|------|
| J1-1 ↔ J6-3 | 都碰 configs/.github——J1 先合 |
| J2-1/2 ↔ J2-5 | 同文件 grpc_swagger_test.go——同批次串行 |
| J2-4 ↔ J5-1 | 都可能碰 ratelimit/pagination 相邻文件——J2-4 先合 |
| J4-* | 内部严格串行；与其他批次无并行 |
| J3-1 ↔ CI 任何改动 | release workflow 新文件为主，冲突低 |

## 9. 产品拍板记录

| 决策 | 结论 | 影响 |
|------|------|------|
| D-1 presence 清空语义 | 实现 Has*（契约保持「设置空串=清空」） | J2-3 方向已定 |
| D-2 OpenAPI 命名方向 | 文档侧改 snake_case（零代码改动） | J2-1 方向已定 |
| E-1 限流降级选型 | 熔断短窗放行 + 观测分离；auth 面 fail-closed 保持 | J5-1 方向已定 |
| A-1 组装根迁移范围 | 本轮回（J4-4），迁至 internal/runtime | J4-4 |
| A-2 app 层 grpc/status | 接受为项目约定，AGENTS.md 补记 | J7-18 |
