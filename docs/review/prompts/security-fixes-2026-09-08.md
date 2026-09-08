# Prompt:Torchwood 平台安全修复(T-01/T-02/T-03)

> 使用方式:在 `D:\Codes\qiulin\torchwood` 仓库根目录开一个新 agent 会话,把本文件全文作为任务输入。
> 依据:`docs/review/security-audit-2026-09-08-blog-dev.md`(2026-09-08 安全检测,含实测证据)。
> 三个工作包相互独立,建议按 T-01 → T-02 → T-03 顺序实施,每个包独立提交。

---

你在 Torchwood 仓库(Go + grpc-gateway 后端,Console 前端在 `console/`)工作。本次任务:落地安全检测报告 `docs/review/security-audit-2026-09-08-blog-dev.md` 中的三个平台侧整改项。先读该报告和相关代码再动手;完成后更新报告中对应条目的"状态"列为已修复/已决策。

已知代码地图(动手前自行确认,以实际为准):

- 客户端认证面:`proto/client/v1/account.proto`(SignUp/SignIn/SignOut/RefreshToken/Me/UpdateAccount/ConfirmEmailChange;**没有** DeleteAccount),对应实现在 `internal/` 下(grep SignUp 的 service 实现定位)
- API Key:`proto/server/v1/apikeys.proto`(CreateAPIKey/List/Get/Delete;`scopes repeated string`、`enabled`、`expire_at`;明文仅创建时返回一次,库中只存哈希;**没有** Update/轮换)
- scope 词表与鉴权策略:`internal/domain/auth/policy.go`(`ScopeResource`:databases/users/groups/storage/projects/…,**服务级粒度,无资源级**)
- 审计域(已存在):`internal/domain/audit/audit.go`
- 项目域:`internal/domain/projects/project.go`;数据库迁移:`db/migrations`;代码生成:`buf.gen.yaml` + `Taskfile.yml`
- 开发者文档:`docs/developer/05-authentication.md`、`09-api-guide.md`、`10-console.md`
- 全仓库目前**没有任何限速基础设施**(已 grep 确认),需从零建

全局约束:

- proto 变更走既有 buf 代码生成流程;HTTP 语义用 grpc-gateway 注解,错误体沿用 `shared.v1.ErrorResponse`
- **向后兼容**:现有 API key 的 scope 写法(`databases` 等无资源限定)行为不得变化;现有项目注册行为默认不变
- SDK(`sdk/` 与外部 `@torchwood/sdk`)的既有调用不得破坏;新增能力可以是纯服务端或新增字段
- 每个工作包:Go 单测 + 必要的迁移 + 文档更新 + `golangci-lint` 通过;不要顺手重构无关代码

---

## 工作包 T-01:认证接口限速与锁定(优先级最高,最小闭环)

**现状(实测证据)**:`POST signIn` 连续 5 次错误密码均稳定返回 `401 invalid credentials`——无 429、无锁定、无退避。公网网关可无限暴力尝试。

**要求**:

1. 新建限速组件(建议 `internal/infra/ratelimit` 或你评估后更合适的位置):
   - 令牌桶/滑动窗口,内存实现即可(当前单实例部署);**定义接口抽象**并留 Redis 实现的扩展点,不在本次实现
   - 客户端 IP 解析必须走可信代理链(部署在 Traefik 后,确认现有代码取 X-Forwarded-For / X-Real-IP 的方式,勿直接信任可伪造的头——若现有网关已有统一取值函数则复用)
2. 应用到客户端认证面:
   - `SignIn`:按**账号标识(email 的小写规范化)+ 来源 IP** 双维度;默认阈值各 5 次/分钟(可配置,配置进 `configs/` 既有机制)
   - `SignUp`:按 IP 限速(默认 10 次/小时量级,防批量注册)
   - 超限返回 `429` + `Retry-After` 头,错误体用 ErrorResponse;**限速响应不得区分"账号存在与否"**(防止借 429 做用户枚举,保持现有 invalid credentials 的统一语义)
   - 登录成功重置该账号维度的失败计数
3. 审计:连续失败触发限速时写一条审计事件(挂 `internal/domain/audit` 既有机制)
4. 文档:更新 `docs/developer/05-authentication.md`(限速行为、429 语义、配置项)

**验收标准**:

- [ ] 同账号连续第 6 次 signIn 失败 → 429 + Retry-After;换 IP 不放行账号维度,换账号不放行 IP 维度
- [ ] 正确密码在未触限时可正常登录,且登录成功后计数归零
- [ ] signUp 超限 → 429
- [ ] 单测覆盖:两维度计数、成功重置、429 不泄露账号存在性
- [ ] 报告 T-01 状态更新

---

## 工作包 T-02:API Key 细粒度 scope、轮换与操作审计

**现状(实测证据)**:blog 供给所需的 key scope 是**整个项目的 databases 读+写**——一旦泄露即全项目所有库沦陷;Console 只能删除重建 key(无轮换);server-key 的写操作没有审计日志;key 认证面无失败限速。

**要求**:

1. **资源级 scope 语法**(向后兼容扩展):
   - 现格式 `databases` / `storage` = 该服务全项目权限(行为不变)
   - 新格式 `databases:<database_id>`、`storage:<bucket_id>` = 限定到单个资源;`databases:blog` 的 key 访问其他 database 一律 403
   - 校验点在 `internal/domain/auth/policy.go` 的既有 scope 判定处扩展;CreateAPIKey 时对非法 scope(未知服务/非法资源名)直接 400
   - 注意 blog 场景回归:供给还需要建 database/集合/索引(DDL),评估 `databases:blog` 是否需要同时覆盖 DDL——若 DDL 走独立 scope(如 `projects` 或 catalog 类),在文档里写清组合示例
2. **key 治理**:
   - `apikeys.proto` 新增 `UpdateAPIKey`(可改 `enabled`/`expire_at`/`scopes`/`name`),PERMISSION owner/admin,保持"key 凭证禁入本服务"的既有决策
   - 轮换方案:利用"新建 + 双 key 并存 + 给旧 key 设 `expire_at`"实现平滑轮换——在 Console 与 `docs/developer/09-api-guide.md` 固化该流程;不必做原地换 secret
   - key 认证失败(X-API-Key 错误/哈希不匹配)按 IP 限速,复用 T-01 组件(阈值可独立配置)
3. **操作审计**:所有经 API key(ACCESS_SERVER 面)的**写操作**(create/update/delete 类 RPC)写审计事件:key id、项目、RPC/HTTP 路径、资源标识、结果。挂在网关鉴权中间件层统一记录,不要逐 handler 散写
4. Console(`console/`):API Keys 管理页支持编辑(scope/enabled/过期时间)与"轮换"引导流程
5. 文档:`docs/developer/09-api-guide.md`(scope 语法表 + 轮换流程)、`05-authentication.md`

**验收标准**:

- [ ] `scopes=["databases:blog"]` 的 key:读写 `blog` 库成功;访问同项目其他库 → 403
- [ ] 无资源限定 scope(`databases`)行为与现状完全一致(回归测试)
- [ ] UpdateAPIKey 可禁用 key(禁用后立即 401)、可设过期(过期后 401)
- [ ] 用 key 执行 createDocument/deleteDocument 后,审计日志有对应记录(含 key id 与资源)
- [ ] key 认证连续失败触发限速 429
- [ ] 报告 T-02 状态更新

---

## 工作包 T-03:项目级注册策略 + 账号删除

**现状(实测证据)**:任何人可对项目网关匿名注册账号(本次检测即用任意邮箱 `sec-test-873c7230@test.local` 自助注册获得写权限);且无账号删除 API,测试账号只能 Console 手工清。

**要求**:

1. **注册策略**(项目设置,默认 `open` 保持兼容):
   - `internal/domain/projects/project.go` 增加字段(枚举 `registration_policy`:`open` / `invite_only` / `closed`),配套 `db/migrations` 迁移,默认值 `open`
   - `closed`:SignUp 一律 403(新错误码,如 `registration_closed`)
   - `invite_only`:Console 生成邀请码(一次性、可设过期与使用次数);SignUp 请求携带邀请码字段(可选字段,proto 向后兼容),校验消费需原子(防并发重放)
   - `open`:现状不变
   - Console 项目设置页提供策略切换与邀请码管理 UI
2. **DeleteAccount**:`account.proto` 新增 `DeleteAccount` RPC(终端用户 JWT,删除自己):
   - 评估软删 vs 硬删:至少要做到凭据立即失效(会话/token 吊销)且 email 不可再被枚举出"曾存在";参考既有 SignOut/token 吊销机制
   - 数据处置(其名下文档/文件)定义清楚并在文档写明——第一版可以只做"账号失活 + 登录拒绝",把数据保留策略写成显式决策,不要默默级联删除
3. 文档:`docs/developer/05-authentication.md`(注册策略、邀请码、DeleteAccount 语义)、`10-console.md`(设置页)
4. 顺带清理:在 dev Console 用 DeleteAccount(或手工)删除报告残留的测试账号 `sec-test-873c7230@test.local`(项目 `blog`),完成后在报告残留事项中勾销

**验收标准**:

- [ ] `closed` 项目 SignUp → 403 `registration_closed`;`open` 项目行为不变(回归)
- [ ] `invite_only`:无码/错码/过期码/已用码 → 403;正确码注册成功且码作废;并发两个请求同码只有一个成功
- [ ] DeleteAccount 后:该账号 token 立即 401,SignIn 拒绝,且 SignIn 报错仍是统一 invalid credentials(不因删除而泄露存在性)
- [ ] 迁移可从现有库平滑升级(默认 open)
- [ ] 报告 T-03 状态与残留事项更新

---

## 完成定义

1. 三个工作包各自的验收标准全部满足,`go test ./...`、`golangci-lint`、buf 代码生成均通过
2. `docs/review/security-audit-2026-09-08-blog-dev.md` 的状态列更新为已修复/已决策,并附实现 PR/commit 引用
3. 在报告末尾追加"复验记录"小节:列出每条验收标准的实际复验命令与结果(例如对 dev 网关连错 6 次密码观察到 429)
4. 不引入新依赖除非必要;如引入,在提交说明中给出理由
