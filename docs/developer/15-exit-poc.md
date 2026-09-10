# 15 转出 POC 检查单（发布前门禁）

> 面向：DocumentDB POC → **对外发布 / 有真实存量用户**的转出门禁。POC 定义见 `docs/design/documentdb-redesign.md` 状态头：无兼容义务、本地/测试数据可随时重建、各阶段直切不留回退。
> 成文：2026-09-05 会话 #11，对 redesign 全文、`db/migrations/*.sql` 注释、`docs/developer/06-databases.md`、`AGENTS.md`、git log 近 10 个会话与 `CHANGELOG.md` 做穷尽盘点后收拢而成。**本文件是转出门禁与挂账的单一事实源（活跃文档）**；redesign §6 各完成状态段的挂账清单为 2026-09-05 快照，此后新增与闭环只更新本文。
> 分区规则：**A 门禁区**——阻塞对外发布，清零前不得发布；**B 非阻塞功能债区**——不阻发布，按产品节奏排期；**C 决策确认区**——无需实现，需产品拍板并把决议回写本文。
> 每条四要素：**出处**（可回溯锚点）｜**要做什么**｜**完成判据**（可验证语句，能写出对应测试/命令/检查动作）｜**建议归属**。标〔新发现〕的条目为会话 #11 盘点相对种子清单新收拢的**既有**挂账（出处早已存在，非新发明）。
> **迁移基线重定（2026-09-06）**：`db/migrations` 已从 33 个迁移重定为 5 个基线迁移（000001 控制面 / 000002 事件脊柱 / 000003 全局 catalog+账本 / 000004 RBAC·RLS·roles_sig / 000005 pgvector）；拍板依据 = 无生产存量库、全部环境重建（`docker:purge` + `db:migrate`）。本文出现的历史迁移号按此映射、正文不改写：000025/000032→000003，000026/000027/000029/000031/000033→000004，000028→000002，000030→000005，000023（存量订阅索引补丁）随基线退役。

## 0 门禁使用方式

1. **触发定义**：任一情形即触发本门禁——公网开放注册、SDK 包管理器发布新版本（npm `@torchwood/sdk` / Go module tag）、或出现不可要求重建数据的真实存量部署。
2. **过门禁 = A 区全部条目满足完成判据**：在对应条目下追加闭环证据（commit 哈希 / 测试名 / 命令输出摘要 / 决议链接 + 日期）。B/C 区不阻塞；C 区条目允许"拍板后维持现状"，但必须有决议记录。
3. **新增挂账进本单**：阻塞发布的进 A；不阻塞的功能债进 B；纯产品决策进 C。措辞纪律：完成判据必须可验证，禁止"评估/考虑/研究"类虚词——需要评估的议题一律入 C 区并写明决策问题。

## A 门禁区（阻塞对外发布，10 条）

### A1 存量列授权全量 reconcile 扫描

- **出处**：redesign §6 阶段③-a 完成状态（"转出 POC 检查项：启动/迁移路径加一次全量列授权 reconcile 扫描——存量表旧授权形态现依赖 DDL touch 矫正"）；会话 #10 复审裁决（CreateAttribute 列级 GRANT 滞后的存量修复 `refreshColumnGrants`，影响全部属性类型，32f9660）；`internal/infra/documentdb/rls_policy.go:91` 注释。
- **要做什么**：在启动或迁移路径增加一次性全量扫描：遍历 catalog 全部业务集合物理表，按 R13a/R16 终态口径（SELECT 全列；INSERT 数据列 + 除 `_tenant` 外系统列含 `_acl`；UPDATE 排除 `_tenant`/`_acl`）重刷列级 GRANT，不再依赖 DDL touch 逐表矫正。R13a/R16 连带：终态口径以 ③-b 收口形态为准（INSERT 恢复 `_acl`、UPDATE 双向排除）。
- **完成判据**：构造一张列授权故意偏离终态的表（手工 REVOKE/GRANT），执行扫描后 `information_schema.column_privileges` 与 `refreshColumnGrants` 幂等重建的结果一致；扫描入口（启动钩子或迁移步骤）有集成测试锁定，且空库扫描为 no-op。
- **建议归属**：documentdb DDL/权限会话（`infra/documentdb`）。
- **闭环（2026-09-05 补正 2026-09-06）**：2026-09-05｜commit 0769ce6｜扫描入口选启动钩子（迁移路径是纯 SQL 文件，Go 侧 reconcile 无挂载点；OnStart 先于监听，矫正先于流量暴露）：`documentdb.ReconcileCollectionColumnGrants` 遍历全局 catalog 业务集合（sentinel 排除、ORDER BY 全键保证跨进程锁序），逐表经 tw_owner 事务执行 `refreshColumnGrants` 幂等重建（扫描执行体与门禁判据对照物同源；policy 重建仍留 DDL touch 路径）。**接线修正（2026-09-06，`7014f26`）**：钩子自 bootkit 共享装配移出——reconcile 是 documentdb 域职责，bootkit 直接实现会把 documentdb 拖进 worker 依赖闭包（import guard `TestWorkerDepsGraph` 曾因此红）；现经 `NewOnStarts` 可选闭包注入，**仅 server 注入、worker 传 nil**。单表失败不中断全量（幽灵 catalog 行跳过计数 + Warn 日志 + `torchwood_documentdb_grants_reconcile_failures_total` 指标），空库 no-op。集成测试锁定：`TestGrantsReconcile_DeviationRestored`（偏离种子 = REVOKE SELECT、REVOKE UPDATE(title)、GRANT UPDATE(_acl)、GRANT INSERT(_tenant) → 扫描 → column_privileges 与从未偏离的对照集合逐行一致 + 二次扫描零增量 + 功能级 42501 抽验）、`TestGrantsReconcile_EmptyCatalogNoOp`、`TestCollectionGrantsReconcileHook_WiredInOnStarts`（接线断言，已按注入形态更新）。实测稳态扫描 ≈8.8ms/表（本地单实例，含每表一个 tw_owner 事务），千表量级启动开销个位数秒。

### A2 非 superuser 应用 DSN

- **出处**：redesign §6 阶段③-a 挂账 + §6 总览（"转出 POC 检查项两条"）；06-databases 不变量 #14（"DSN 用户为 superuser 时绕过 policy——已知豁免面；生产应配非 superuser 应用账号，A6 runbook 化"）；迁移 000026（authenticator = DSN 用户，GRANT 三角色 membership）。
- **要做什么**：生产部署的应用 DSN 换为非 superuser 专用 authenticator（仅具 000026 所需 membership 与库级权限，无 BYPASSRLS 无 superuser）；部署文档写明创建步骤；测试面补一组非 superuser DSN 的迁移 + 运行验证。
- **完成判据**：`docs/developer/13-operations.md` 含创建非 superuser authenticator 的可复制 SQL 与验证命令（`SELECT rolsuper FROM pg_roles WHERE rolname=...` 返回 false）；以该 DSN 完成一次 `db:migrate` + 冒烟测试并留记录；`configs/config.yaml.template` 与部署示例不再示范 superuser 凭据。
- **建议归属**：部署/运维（configs + 13-operations）。
- **闭环**：2026-09-05｜13-operations §4.5「应用 DSN 与权限」+ §6.1 双账号注记（同 commit：`internal/testutil/nonsuperuser_test.go`、config 模板/quickstart/README/11-testing 去示范）｜双账号契约成文：迁移/扩展引导 = owner 引导账号（superuser，承担 000030/000029/000026 引导面），运行态 = `tw_authenticator`（非 superuser，五特权位全 f；授权面 = 000026 三角色 membership + CONNECT/CREATE + public 静态表 DML + `REFERENCES ON projects` + `tw_secrets` SELECT,INSERT,UPDATE,DELETE）。实测（本地 `pgvector/pgvector:0.8.6-pg18`）：临时库按序应用全部 30 个 up 迁移后按 §4.5 SQL 引导，`rolsuper=f`、membership 三行、`SET ROLE` 三角色可达、authenticator 装 untrusted 扩展/建 public 表均被拒（后者与 A3 探针一致）；集成测试 `TestNonSuperuserAuthenticator_MigrateAndSmoke` 以 authenticator 完成 roles_sig 落库 + 建项目/业务库/集合 + 文档写读（tw_system 写、tw_app+sig RLS 读）冒烟全绿。**残余风险注记（A4 连带）**：`tw_secrets` 四权是 SyncRolesSigKey 双钥四语句运行态落库的必要授权——DSN 账号可读 roles_sig 密钥（000029 "tw_app 不可读防自签"防线对 base identity 失效，可伪造 `app.roles` GUC）；消除路径 = 密钥落库改部署期 owner 一次性作业（代码变更），转出前未实施则在本文复核或入 B 区。**→ 已消除（B15 闭环，2026-09-05）**：`tw_secrets` 四权撤销（迁移 000033）、密钥落库改部署期 owner 作业 `torchwood admin sync-roles-sig`、A2 测试同步改造（owner 落库 + authenticator `tw_secrets` 七特权位全 false 断言），详见 B15 闭环行。

### A3 vector 扩展 superuser 安装引导（runbook）

- **出处**：迁移 000030 头注释（"vector 不是 trusted extension，CREATE EXTENSION 需 superuser……转出 POC 前的部署方案需把'扩展安装走 superuser 引导步骤'写进 runbook"）；redesign §6 会话 #10（"runbook 化并入转出 POC 检查单"）。
- **要做什么**：runbook 写明 pgvector 安装形态：镜像预装（`pgvector/pgvector:0.8.6-pg18` 基座，docker/local 与 CI 已同步）或 superuser 引导步骤（DBA/引导容器执行 `CREATE EXTENSION vector`）；明确非 superuser 迁移身份下 000030 的行为（前置检查给出可读错误或引导后执行）。
- **完成判据**：13-operations 有独立小节覆盖"非 superuser 部署下启用 vector"的步骤 + 验证 SQL（`SELECT extversion FROM pg_extension WHERE extname='vector'`）；在一个非 superuser 迁移身份的环境中按步骤走通一次并留记录（本文条目下附命令输出摘要）。
- **建议归属**：部署/运维（docker/local + 13-operations）。
- **闭环**：2026-09-05｜13-operations §6.6（同 commit 附全部命令输出）｜镜像基座（docker/local + CI 均为 `pgvector/pgvector:0.8.6-pg18`）superuser 迁移身份下验证 SQL 返回 `vector | 0.8.6`（路径一实测）；临时库 `vector_probe`（owner=非 superuser，`rolsuper=f`）实测路径二：未预装直接执行报 `ERROR: permission denied to create extension "vector"`（HINT: Must be superuser to create this extension.），superuser 预装后同一身份重跑 `CREATE EXTENSION IF NOT EXISTS vector` 输出 `NOTICE: extension "vector" already exists, skipping` 幂等通过、验证 SQL 有输出；探针库/角色已 DROP 清理。

### A4 roles_sig 双密钥轮换窗口

- **出处**：redesign §3.2 GUC 伪造面（"单密钥 + 滚动重启轮换（双钥窗口挂账转出 POC 前）"）；迁移 000029 注释同文；`internal/infra/clients/tx.go:204`（"本函数覆盖旧行即完成换钥（单密钥，双钥窗口挂账转出 POC 前）"）。
- **要做什么**：二选一并记录决议：① 实现双钥（`tw_secrets` 支持 current/previous 两把，`tw_sig_match` 任一命中，滚动窗口内换钥零停机）；② 显式决策接受滚动重启窗口（写清换钥期间旧进程签发 sig 与新密钥的 fail-closed 行为及可接受的时长界）。
- **完成判据**：方案①——双钥落地 + "换钥后旧 sig 在 60s 窗口内仍验签通过、窗口外拒绝"的集成测试；方案②——redesign §3.2 或 06-databases 不变量 #14 记录决策句（含重启窗口语义），并有"sig 失配 → 零角色 fail-closed"的既有测试引用（`roles_sig_test.go`）作为影响面佐证。
- **建议归属**：infra/clients（roles_sig）会话。
- **闭环（方案①双钥实现）**：2026-09-05｜commit 3a1da5e（Go 平移逻辑 + 集成测试；迁移载体修正与本回写同提交）｜`tw_secrets` 双钥槽位：`is_current` 布尔 + `(purpose, key_hex)` 主键 + 部分唯一索引 `tw_secrets_single_current`（每 purpose 至多一把 current 的表级约束），`tw_sig_match` 改 EXISTS 任一钥命中——过期判定先于钥匹配，previous 命中无法给窗口外 sig 续命；Go 侧 `SyncRolesSigKey` 四语句平移（previous 位目标钥先移除（回滚场景提回 current）→ 旧 current 降级 → 新钥落 current（同钥重启幂等 no-op）→ third 条直接删（updated_at+key_hex 决胜保留紧邻上一把），行数不变量 ≤2，单次往返隐式事务全或无）。**载体选 000031 前向补丁而非 000029 原地修订**：遵 A5 方案 §6"已应用的迁移文件永不原地修订"规则方向（A5 规划的 `000031_roles_sig_r16_reconcile` 编号被本迁移占用，实施立项时顺延并以当前函数面为重放内容），存量库 `db:migrate` 原地升级无需重建，down 侧双行收敛保留 current。60s 窗口语义与 R16（tenant|roles|exp 消息、tenant 绑定、可见性门）不变。集成测试 `TestRolesSig_DualKeyRotationWindow` 锁定验收判据：换钥后旧钥 sig 在 60s 窗口内验签通过（previous 命中，`tw_roles`/`tw_tenant`/RLS 可见全链路不降级）、窗口外（exp 过期）旧钥 sig 拒绝、G3 二次换钥后两代前 sig 拒绝（行数回到 2）、同钥重启幂等、第二条 current 直插被表级约束拒绝；既有 `TestRolesSig_FailClosed`（三态 fail-closed）、`TestSetDocumentACL_TenantBinding`/`_VisibilityGate`/`_InjectionSurface`、`TestRolesSig_LegitPathRegression` 于 000031 载体重跑全绿。

### A5 redesign 状态头义务：重审"直接切换"表述并补存量迁移方案

- **出处**：redesign 状态头（"转出 POC 前需重审本文所有'直接切换'类表述并补迁移方案"）；§6 末段（"'每阶段附回退方案'的要求在转出 POC 时再引入"）；§11-A4（双读迁移期 policy 数据源决议"预置于转出 POC 后的阶段③方案"）；§11-G2（存量四表迁移任务：幂等/断点续跑/EnsureCatalog 语义切换点）。〔新发现〕状态头"设计提案，未实施"与 `AGENTS.md` 数据库约定同句均已过时（§6 已宣告 2026-09-05 全量闭环），随本条一并修正。
- **要做什么**：对 POC 期全部直切点逐项补存量升级路径，至少覆盖：`_perms`→`_acl` 回填与切换（A4 预置决议的落地）、每项目 catalog 四表→全局两表的存量迁移器（G2）、客户端契约断裂的版本策略联动（offset token 失效、`queries` DSL 字段 reserved、`filter/order_by` reserved——归 A10）、000029 类"原地修订不可重放"迁移在存量库的处置、错误码直换是否需要旧码映射。产出迁移方案文档，每项写明"重建 or 迁移"与步骤。
- **完成判据**：存在一份覆盖上述直切点的迁移方案文档（redesign 附录或独立文档），每个直切点有可执行步骤或显式"无需迁移（理由）"；redesign 状态头与 AGENTS.md 的"未实施"过时表述已修正；方案经过一次评审（登记评审 commit）。
- **建议归属**：文档会话（redesign 维护）+ documentdb 各专项。
- **产出挂接（2026-09-05）**：迁移方案已落 `docs/design/poc-to-release-migration.md`——覆盖 `_perms`→`_acl`（回填 SQL 与 policy 启用顺序，落实 §11-A4 预置决议）、catalog 四表→全局两表（列映射 + 搬迁器 + **EnsureAll 先触发 000011 DROP 的语义切换点**，G2）、物理名 RENAME、000029 原地修订（幂等补丁 000031 推荐 + "禁改已应用迁移"规则固化）、客户端契约断裂（升级矩阵 + 可选未知字段守卫 + 错误码不映射裁决），每项含"重建 or 迁移"推荐与部署排序（其 §8）。同步修正：redesign 状态头与 `AGENTS.md` 的"设计提案、未实施"过时表述已改为"已全量落地"。**余项**：方案评审（登记 commit）；G2 搬迁器实现按方案 §9 立项（届时入 B 区）。

### A6 并行测试基建（CI 可靠性门禁）

- **出处**：redesign §6 演进路径总览剩余挂账（"并行测试基建（SetupTestDB 争用 + 迁移循环集群级角色竞态：DROP ROLE 撞并行库中 tw_owner 对象，串行绿）"）；06-databases §12（`internal/testutil/db.go:SetupTestDB`、`migrations_cycle_test.go`）。
- **要做什么**：修复两处并行不可用根因：① SetupTestDB 的隔离库创建/清理在多包并行下的争用；② 迁移循环 down 的 000026 `DROP ROLE`（集群级对象）与并行库中 tw_owner 持有对象的竞态。
- **完成判据**：`go test ./... -p 4` 在 CI 规格环境连续 3 次全绿、无重试无 flake；testutil 文档注明并行安全契约。
- **建议归属**：testutil/CI 会话。
- **闭环（2026-09-05，testutil/CI 会话）**：两根因修复落地——① 建库/迁移/删库段持集群级 advisory lock（`pg_try_advisory_lock` 轮询等锁 + admin 10s 读超时 + 瞬时过载指数退避重试 + admin 池进程级单例 + 测试库池上限 16），跨进程互斥不依赖进程内锁（`07c6e6a`、`89eb708`、`c181050`）；② 000026 down 改**集群角色保留**形态（不 DROP ROLE，只回滚本库作用域：REASSIGN/DROP OWNED + REVOKE membership；up 同步原子幂等 `1d77e8f`）——消除冲突源本身，无需任何测试间互斥锁，down 对称性收敛为"库内完全对称"（角色生命周期归集群供给方，A8 承接）。附带修复验收暴露的既有 flake：session evict 测试 expire tie（`32a781d`）、CreateTestProject 项目 schema apply 重试（`de88fb6`）。**证据**：隔离验收树（main `c181050` 同源）连续 3 次 `go test ./... -p 4 -count=1` 全绿——67 包 ok，唯一红灯 `cmd/worker.TestWorkerDepsGraph` 为 **A1 commit `0769ce6` 引入的 import guard 违规**（bootkit→documentdb 传递依赖；已实证在 A6 改动之前的 `3a1da5e` 上同样红），归属 A1 会话收尾。该红灯已由集中复审修复（`7014f26`）：reconcile 钩子移出 bootkit 共享装配，经 NewOnStarts 可选闭包注入（server 注入实现、worker 传 nil——reconcile 是 documentdb 域职责，worker 本不该跑）。**残余边界（集中复审后 4 轮 `-p 4` 实测：2 全绿 / 2 轮各一次随机包 i/o timeout，不同包、隔离复跑即绿）**：A6 的瞬时过载重试只接了 projectschema apply，未覆盖 documentdb DDL 路径（如 ensureCollectionRLS）——本地单 PG 实例 `-p 4` 偶发、串行稳定；补齐重试覆盖面随并行测试基建挂账。并行安全契约见 `internal/testutil/db.go` 包注释与 06-databases §12。

### A8 RBAC 角色生命周期 runbook（集群供给与清理）

- **出处**：迁移 000026（A6 ② 修订后为**集群角色保留**形态：down 不 DROP ROLE，只回滚本库作用域——REASSIGN/DROP OWNED + REVOKE membership；up 幂等创建。角色生命周期归集群供给方，见迁移头注释与 06-databases §12）。
- **要做什么**：runbook 写明 RBAC 角色的集群供给（部署期幂等创建）与常规清理流程（pg_roles/pg_shdepend 探测角色名下对象、逐库 REASSIGN OWNED/DROP OWNED 清单）——A6 修订后 down 已无跨库竞态，本条从"down 撞 2BP01 的处置"转为常规生命周期流程。
- **完成判据**：13-operations 含可复制的探测查询与逐库清理步骤；在同一集群双迁移库沙箱演练一次 down 并记录输出（本文条目下附摘要）。
- **建议归属**：DBA runbook（13-operations）。
- **闭环（2026-09-06，集中复审沙箱演练）**：13-operations §6.7 "RBAC 角色生命周期"成文——探测查询四条（角色特权位 / pg_auth_members membership / 角色名下对象分布 / pg_shdepend 全景）、逐库清理流程（REASSIGN/DROP OWNED → REVOKE membership → 角色保留不 DROP）。**双沙箱库演练实录**：sandbox_a/b 各应用全量 32 迁移 → sandbox_a 跑 000026 down（DO 成功，无 2BP01——角色保留形态生效）→ 探测实证 **membership 是集群级**：sandbox_a down 后 sandbox_b 的 bootstrap membership 同步清空（pg_auth_members 0 行），恢复 = 重新 GRANT（等价重跑 000026 up 段）立即恢复三行；`tw_system` 的 BYPASSRLS 属性独立于 membership 存续。**runbook 必备结论（实测产出）**：共享集群上对任一库跑 down = 对全集群撤销运行时身份，其他库需重新 GRANT 后方可继续服务——已写入 §6.7。

### A9 locale=C 的产品期决策

- **出处**：redesign §6 会话 #10（"镜像基座 glibc vs musl 的 collation 翻转连带修复：initdb 锁 `locale=C` 字节序，产品期语言学排序另行决策"）；commit 61ac141。
- **要做什么**：确认产品是否存在语言学排序需求（用户可见的 string 列 ORDER BY 语义，如中文按拼音）；如需要——定 ICU/libc locale 选型与集群初始化参数并评估存量重建影响；如不需要——记录"维持 locale=C"决策。
- **完成判据**：决策记录落在本文（含需求证据或"无需求"结论）；若改 locale：docker/local 与部署文档同步 + 中英文排序语义对比测试；若维持：一条显式决策句 + 06-databases 或 13-operations 注明 string 排序为字节序。
- **建议归属**：部署决策（docker/local + 运维）。
- **决策 memo（2026-09-05 成文，拍板材料）**：
  - **背景**：vector 专项把镜像基座从 `postgres:18-alpine`（musl）换为 `pgvector/pgvector:0.8.6-pg18`（Debian/glibc）时 collation 语义翻转——Debian initdb 默认 `en_US.utf8`（真 glibc 语言学序），musl 的同名 locale 是字节序伪装（实证：`'s-plain' > 's4'` 在 glibc en_US.utf8 为真、C/musl 为假，sessions 驱逐测试平局保留随之翻转）。61ac141 在 docker/local 与 CI 显式 `POSTGRES_INITDB_ARGS="--locale=C"` 锁字节序，恢复跨镜像/跨平台确定性；已有卷不受影响（initdb 仅首次建库生效）。
  - **影响面**：仅用户可见 string 列的 `ORDER BY`/范围比较——C locale 下按 UTF-8 码点字节序（中文非拼音序、`'Z'<'a'` 大小写混排、标点位置与词典不同）；等值与 equal/filter 完全不受影响（字节序自洽且 btree 索引有序）；BaaS 负载的排序键多为时间/数值/`_id`（查询编译的 `orders[]` + `_id` tiebreaker 主路径，见 06-databases §6），字符串词典序集中在"按名称列清单"等长尾场景。
  - **选项**：① **维持 `locale=C`**——确定性跨镜像/跨平台/跨 glibc·ICU 版本，免疫 collation 版本漂移引发的 btree 索引逻辑损坏（collation 变更后 PG 要求 REINDEX；自托管集群无法约束用户基座的 libc/ICU 版本）；多语言中立（不预设任何一种语言的词典序）；代价：中文等自然语言排序不合词典直觉。② **ICU**（`initdb --locale-provider=icu` 或列级 `COLLATE`）——语言学正确（zh 默认拼音序）；代价：引入 ICU 版本钉扎与基座依赖、跨平台不再逐字节一致、**存量集群改 locale 必须重建**（见下）、且集群级单一 locale 天然无法服务多语言租户——语言学排序本质是 per-用户/per-列需求，正确产品形态是未来把 `collate` 做成属性/列级可选项，而非改集群默认。
  - **推荐**：**①维持 `locale=C`**，并在产品/运维文档注明"string 排序 = UTF-8 码点字节序"（13-operations §2 已完成现状注记；本条目拍板后在该决策句下闭环）；真实语言学需求出现需求信号时，按"列级 `collate` 属性选项"立 B 区条目，不动集群 locale。
  - **若拍板②（改 locale）的存量重建影响**：initdb 参数仅建库时生效——存量集群切换必须 dump → 重 initdb → restore，或逐列 `ALTER TABLE … COLLATE` + 全部 text btree 索引 REINDEX（否则索引静默损坏）；docker/local、CI、部署文档、13-operations 需四处同步；中英文排序语义对比测试补齐；glibc `en_US.utf8` 时代建立的本地卷本就处于"需重建对齐"状态（61ac141 已知）。
  - **待维护者拍板句**：torchwood 产品期是否存在语言学排序需求？推荐决议——**维持 `locale=C`（string 列排序 = UTF-8 码点字节序），语言学排序需求由未来列级 `collate` 选项承载**。决议回写本条目。
- **决议（2026-09-06，维护者，按 memo 推荐）**：**维持 `locale=C`**——string 列排序 = UTF-8 码点字节序（13-operations §2 已注记）；语言学排序需求由未来"列级 `collate` 属性选项"承载（届时立 B 区条目），不改集群 locale。A9 闭环。

### A10 SDK 发版策略

- **出处**：`CHANGELOG.md`（npm `@torchwood/sdk` v0.1.0 已发布、sdk/go v0.1.2 tag、`.github/workflows/release.yml`）；redesign 状态头（POC 含义：proto/API 直接破坏性修改）——POC 期间 proto 已发生 reserved 级断裂（`queries` 双栈退役、`ListRequest.filter/order_by` 退役等），**已发布 SDK 与服务端契约已分叉**。
- **要做什么**：确定转出时的版本策略：TS/Go SDK 下一版本号（0.x 语义下 minor 携带破坏性 vs 升 1.0 前冻结契约）、破坏性变更的 migration note 义务、服务端与 SDK 的兼容矩阵（是否承诺 N-1）。
- **完成判据**：版本策略决议写入 `sdk/README.md` 或 CHANGELOG（含版本号规则与兼容承诺语句）；下一个 SDK 版本发布时附迁移说明，内容对照 A5 的客户端契约断裂清单。
- **建议归属**：SDK 发版（release.yml + CHANGELOG）。
- **决策 memo 挂接（2026-09-05 成文，拍板材料）**：版本策略 memo 已落 `sdk/README.md` §"版本策略与兼容承诺"——现状（v0.1.0/v0.1.2 与服务端契约已分叉，TS 合同测试 R17 前红为证；reserved 静默忽略为最危险档）、推荐决议（0.x 期 minor 携带破坏性：0.2.0 同 train 收拢全部断裂；**1.0.0 与本门禁 A 区清零绑定**自此冻结；migration note 以 A5 §7 矩阵为唯一底稿；兼容承诺 = 不承诺旧 SDK × 新服务端、服务端支持最近两个 SDK minor）、待维护者拍板句三项。拍板后决议句回写本条目闭环。
- **决议（2026-09-06，维护者，按 memo 推荐三项）**：① **0.x 期 minor 携带破坏性**——下一版本 0.2.0 同 release train 收拢全部已发生断裂（npm/Go/genproto 三面同发）；② **1.0.0 与本门禁 A 区清零绑定**，自此冻结契约；③ **兼容承诺**：不承诺旧 SDK × 新服务端，服务端支持最近两个 SDK minor；migration note 以 A5 §7 客户端升级矩阵为唯一底稿。A10 闭环（下一版发布时附 migration note 为执行义务，归 release 流程）。

### A7 绝对 P99 基准门禁（部分闭环：相对基准已常驻 CI；绝对 P99 挂转出后）

- **落地（2026-09-06）**：`.github/workflows/ci.yml` 新增 "RLS performance relative benchmark (A7)" 步骤——`TestRLS_RelativeBenchmark`（10 万行 RLS 开/关比值 < 30x 数值阈值，非口头约定）与 `TestRLS_ExplainInitPlanGate`（policy 计划形态断言）常驻 CI，主测试套件外独立步骤防管道吞噬。**绝对 P99 数值门禁（百万行级、专用基准规格）按原定位挂转出 POC 后上线**（06-databases §12 原文即"绝对 P99 门禁转出 POC 后上 CI 机器基准"）——本条目的"CI 存在基准 job"部分达成（相对基准 job 已在），"P99 数值阈值"部分待转出后以专用规格兑现，登记为转出后首批工作。
- **判据达成状态**：部分——相对基准维度闭环；绝对 P99 维度显式挂转出后（非静默缺失）。

## B 非阻塞功能债区（14 条，B14 为 C6 决议新立）

### B1 数组算子补全

- **出处**：redesign §6 总览（"数组算子补全（Intersect/Diff/Insert/Filter、TransactionOp 数组）"）；06-databases §6（"Intersect/Diff/Insert/Filter 挂账转出 POC 前"）；`internal/domain/databases/document.go:82`。
- **要做什么**：补齐数组写侧算子 Intersect/Diff/Insert/Filter（与现有四算子同形态：单语句 SET、NULL 语义定义、OCC 不变）+ TransactionOp 的数组算子支持。
- **完成判据**：四算子各有语义测试（含 NULL 列与空数组行为）；execute-tx 数组 op 路径有集成测试；typed builder/SDK 与 06-databases §6 同步。
- **建议归属**：documentdb 数组会话。
- **闭环（2026-09-05，documentdb 数组会话）**：四写算子补齐——`INTERSECT`（`col ∩ values`，去重并保 col 首次出现序，对齐 Appwrite arrayIntersect 产生唯一元素；移空→空数组）、`DIFF`（差集保序不去重，与 REMOVE 同构，独立命名对齐 Appwrite arrayDiff）、`INSERT`（0 基 `index` 对齐 Appwrite `arrayInsert(index,value)`；越界=尾插；PG 18 无 `array_insert` 内建——unnest WITH ORDINALITY + UNION ALL 重排等价实现，PG 18.6 实证）、`FILTER`（按修法指引选受限形态=移除等于任一 values，与 REMOVE 等价；条件表达式形态不支持，避免引入表达式求值）。**NULL 语义二分**：添加类（APPEND/PREPEND/INSERT）视为空数组归一、读改写类（REMOVE/UNIQUE/INTERSECT/DIFF/FILTER）保持 NULL；全部单语句 SET、与 data/increment 可组合、OCC 不变。**TransactionOp 数组支持**：`TransactionOp.array_updates`（server proto 字段 9）仅 update op 消费，api `transactionOpFromProto`→app（`ValidateArrayUpdates` per-op 超限前置拒绝，`ops[N].array_updates` violations 定位）→infra `executeTxOp` 复用同一 `buildArrayParts`。**查询侧裁决：不补 Intersect/Diff**——Appwrite 的 arrayIntersect/arrayDiff 是写侧 update operators、查询侧无可对齐语义，布尔谓词已由 containsAny/All 覆盖；golden 语料零改动（无新 DSL 算子）。SDK 无需变更（现有 SDK 未建模 array_updates，typed builder 面=查询构造器亦无新增）。测试：`array_columns_test.go TestArrayColumns_WriteOperatorsSetFamily`（四算子语义矩阵：去重/保序/越界尾插/NULL 三态/移空、校验拒绝 6 例、BIGINT[] 路径、increment 组合、OCC 拒绝不落变更）；`postgres_transactions_test.go TestExecuteTransactions_ArrayUpdates`（op 数组更新+increment 同批、ATOMIC 未知属性回滚/同列冲突/OCC 不变、PARTIAL per-op 失败续跑、非法 op 名拒绝）；`databases_limits_test.go TestExecuteTransactions_ArrayUpdatesTooLarge`（ops[0].array_updates 超限定位）；`databases_mapping_test.go TestMapArrayUpdates_SetFamily`（四算子映射 + index presence + execute-tx 全链透传）。06-databases §6/§8.1/§12、AGENTS.md、redesign §10.5 P0 行同步。

### B2 多页 KNN

- **出处**：redesign §10.5 P0 挂账行（"多页 KNN"）；`pkg/query/proto/proto.go:52`、`internal/infra/documentdb/postgres_document_query.go:399`（无续页 token，`pageToken` 拒绝）。
- **要做什么**：KNN 结果多页游标（基于距离 + `_id` 的 keyset 语义），解除"无下一页"限制。
- **完成判据**：`vectorSearch` + `pageToken` 组合可用且跨页不重不漏（确定性用例锁定）；与 filter 组合、与 orders 互斥的语义测试更新；proto/双面 SDK/06-databases 同步。
- **建议归属**：documentdb KNN。
- **闭环（2026-09-05，documentdb KNN 会话）**：`kvc:` 距离游标（`kvc:<dist_hex16>:<docID>`，pgvector float8 距离的比特定长 hex 精确往返，负距离/inner_product 原生支持；空 docID 特形 = "该距离起点"）+ **完整距离组切页**（首页 HNSW×iterative scan 取 k+1 行：第 k+1 行证明第 k 行距离组无越页 tie 时满页发射，tie 组不完整即整组顺延、游标落组起点——防"发射 tie 真子集 + 阈值游标"漏行）+ **续页 (dist,_id) 精确全序扫描**（`ORDER BY dist,_id` + `(dist=$d AND _id>$id) OR (dist>$d)` 阈值——HNSW 索引只承载距离单键序、同距组内跨查询不稳定，仅阈值谓词会丢 tie 行，续页放弃 HNSW 换结构化不重不漏）。**内积方向**：`<#>` 为负内积，"越大越好"取负后"越小越近"，与 cosine/L2 统一距离升序、阈值方向三 metric 一致。`maxDistance` 每页独立后置过滤（续页仍受约束）。`vector_search`+`orders` 维持互斥、+`pageToken` 放开（`pkg/query/proto` codec，token 形态校验在 infra fail-closed）。测试：`TestVectorSearch_Pagination_DeterministicStitch`（三 metric 确定性几何，多页拼接 == 单页大 k 全序、距离严格递增且逐一相等）、`_TieGroup`（5 同距 + 1 远行不重不漏——朴素阈值形态在此丢 2 行的杀手用例）、`_FilterSparseVisibility`（filter + RLS 稀疏可见行 6/6，首页 HNSW×iterative scan 与续页精确×RLS 两段覆盖）、`_MaxDistance`（续页受阈、滤空收尾）、`_RejectedTokens`（ka:/kb: 异族/垃圾/NaN/非法 docID 拒绝）+ `TestKNNCursorTokenRoundtrip` + `TestFromProto_VectorSearchCombinations`。proto/Go/TS SDK 注释与 06-databases §6/§12 同步。

### B3 C5 在线 DDL（从未实施的显式欠账）

- **出处**：redesign §2-C5（一律 CONCURRENTLY + 独立事务 + `lock_timeout=2s` 重试 + catalog 两阶段状态机 building→active + 后台 reconcile 对账 + `torchwood admin schema repair` CLI）；§6 会话 #10 偏差②（"CONCURRENTLY 通道不存在——prompt 引用了 C5 目标态当现状，**C5 的在线 DDL 机器从未实施**，现显式入挂账"）；§4.4 漂移防护（reconcile catalog ↔ pg_catalog：缺列/INVALID 索引/幽灵表 + 告警）。
- **要做什么**：实现在线索引通道（CREATE INDEX CONCURRENTLY 独立事务 + lock_timeout 重试 + catalog 索引两阶段状态机）与后台 reconcile/repair CLI（与 A1 的列授权扫描共用遍历骨架）。
- **完成判据**：大表建索引期间并发读写不被阻塞（持锁注入用例通过）；building→active 状态机有中断恢复用例（building 残留可重入）；repair CLI 对注入的缺列/INVALID 索引/幽灵表三类漂移各有修复测试。
- **建议归属**：documentdb DDL 专项会话。
- **闭环（2026-09-05，documentdb DDL 专项会话）**：**在线索引两阶段状态机 + CONCURRENTLY 通道 + reconcile/repair 落地**（commit 9b41f2a/ba64ed4）。① 两阶段状态机：catalog indexes JSONB 条目加 `status`（active 缺省省略零迁移|building|failed）；用户集合 `CreateIndex` = 事务 A（**纯 catalog DML** + CAS）→ 事务外 `CREATE INDEX CONCURRENTLY IF NOT EXISTS`（独立连接 `SET ROLE tw_owner` + `lock_timeout=2s`，55P03/40P01 重试 ≤3，重试前清理 INVALID 残留；RESET 失败连接强制出池）→ 事务 B（active|failed，failed 落位前清理 INVALID）。**分界（预决策 1）**：建集合既有索引（新表无并发读者）与 sentinel 维持事务内通道；**DDL touch 语义收窄**——事务 A 不再搭车默认索引/RLS/列授权自愈（锁型 DDL 阻塞并发读写，与在线通道目的相悖），自愈移交对账扫描（对应既有测试契约同步更新）。SQL 表达式单源 `buildIndexStatement`（concurrently 开关）防两通道漂移。② reconcile（`ReconcileSchemaDrift`，骨架对齐 A1 grants 扫描：catalog ORDER BY 全键、单集合失败不中断、指标告警）：启动钩子（server 侧 `bootkit.SchemaReconcileHook`；worker 传 nil，import guard 边界不变）+ `torchwood admin schema repair [--dry-run]`（cmd/torchwood 直连 DB，对齐 B5 admin 形态）同源逻辑；三类漂移——缺列（ADD COLUMN + 列授权重刷；required 无 default 且表非空不自动回填，记 failed 交运维）、索引漂移（stale building >30min 按 pg_index 分流：valid 补账 active / INVALID·缺失 DROP 后 CIC 重入；活 CIC 不动；failed 条目重入；active 物理缺失/INVALID 重建；无主 INVALID 清理；默认时间索引/_acl GIN 缺失 CIC 补齐——承接①的自愈移交）、幽灵表（业务 schema 内 catalog 无行 → DROP + 告警；catalog 行无表只报告不重建——重建 = 放弃存量数据）。③ 判据三件套：`TestOnlineIndex_ConcurrentDMLNotBlocked`（持锁注入：写事务持 ROW EXCLUSIVE 时对照组非并发 CREATE INDEX lock_timeout 触发证注入生效；CIC 挂起期间并发读写即时成功；释放后收敛 active）；`TestOnlineIndex_BuildingResidualReentry`（工作树内直改 catalog 模拟崩溃，物理缺失与 valid 残留两中断点重入收敛）；`TestSchemaReconcile_ThreeDriftClasses`（三类漂移注入 dry-run 零修复 → repair 全修复功能级生效 → 幂等零增量）+ `TestSchemaReconcile_BuildingResidualRecovery`（valid 补账/缺失重建/活 CIC 不动三态）。redesign 不动（作者复审后回写）。

### B4 schema 演进状态机与 §4.6 契约〔新发现〕

- **出处**：redesign §4.6（Schema 演进契约表：收紧/改类型走迁移任务 validate→rewrite→commit、删列两段 deprecated→retired）；§11-C1-C3/C5（"维持随对应阶段设计稿细化"——从未细化实施）；迁移 000025 注释与 06-databases §4（"`schema_version` 仅立列，演进状态机挂账 §4.6"）。
- **要做什么**：设计并实施 schema_version 演进状态机：migrating 期间读写矩阵、backfill 限速与失败恢复、unique 索引遇存量重复的 validate 报告、删列 deprecated→retired 生命周期。
- **完成判据**：§4.6 表每一行要么有实现 + 测试，要么有修订后的契约文档；`schema_version` 不再"仅立列"（被状态机消费，或显式退役并记录）。
- **建议归属**：documentdb DDL 专项（与 B3 同会话）。
- **闭环（2026-09-05，documentdb DDL 专项会话）**：**deprecated 生命周期 + copy 迁移任务 + schema_version 消费落地**（commit 413efe3）。API 面：proto 新增 `RestoreAttribute`/`RetireAttribute`（`:restore`/`:retire`）与 `MigrateAttribute`（`:migrate`，响应任务读回——物理列名不出契约）；`Attribute.status` 进 API（active 缺省省略 | migrating | deprecated | retired）；scope/admin-roles/well-known 目录同步；Go SDK 三 wrapper（方法数快照 116→119）+ TS SDK 三方法与 `AttributeMigration` 类型。**§4.6 契约逐行映射**：①加列 nullable/带 default = `CreateAttribute`（既有，`ADD COLUMN DEFAULT` 元数据级）；②加列 required = 必须带 default（`MigrateAttribute` 收紧路径前置拒绝 + swap 期 SET NOT NULL 回填，既有 CreateAttribute 语义保留）；③放宽（扩宽/required→optional）= `MigrateAttribute` 即时路径（varchar 扩宽元数据级 ALTER TYPE、DROP NOT NULL，零 copy 任务行）；④收紧/改类型 = `MigrateAttribute` copy 迁移（新列 `<key>__v<seq>` → 批 500 行/批间 5ms 限速回填、游标存 `catalog_migrations` 可恢复、MigrateAttribute 重入续跑（failed 重置续跑）→ ACCESS EXCLUSIVE 锁窗全量追平重算（回填与并发写竞态兜底）+ 行数校验 → 原子 swap（旧列 `<key>__d<seq>` deprecated 残留、新列接管逻辑名）→ `RetireAttribute` 退役残留；validate 失败（cast 不兼容）显式落账 failed 不静默回滚）；⑤删列 = 两段 `DeleteAttribute`（语义升级：段一 deprecated——读投影屏蔽 Get/List/KNN、查询白名单拒绝、写拒收 data/increment/array_updates 三通道、不可作索引目标，`RestoreAttribute` 可回滚）→ `RetireAttribute`（段二 DROP COLUMN CASCADE + catalog 索引条目级联清理 + 条目移除可重建）；⑥重命名 = 不提供（文档维持，等价新列 + copy 迁移 + 旧列 deprecated）。**migrating 期间读写矩阵（§11-C1 修订落地）**：读放行（读服务旧列）、写拒收（`ErrAttributeMigrating`，bypass 主体豁免——统一写封锁形态取代"按目标 schema 校验"的双列写，修订记录见 schema_evolution.go 注释）；**unique 索引遇存量重复（§11-C3）**：unique 建索引路径 CIC 失败 → INVALID 清理 + failed 可重入（B3 状态机承载）；**schema_version**：copy swap 与即时迁移 commit 各递增（消费证明：`TestCopyMigration_TypeChangeRoundTrip`/`TestCopyMigration_InstantRelaxation`）。判据测试：`TestAttributeLifecycle_DeprecateMaskRestore`（deprecated 读写屏蔽 + 回滚往返）、`TestCopyMigration_TypeChangeRoundTrip`（copy 迁移往返）、`TestCopyMigration_ValidateFailureAndResume`（可恢复）。vector 维度变更维持"新列 + 重灌"独立通道（会话 #10 定稿，不入状态机）。

### B5 export / import / snapshot_seq 闭合

- **出处**：redesign §6 总览挂账（export/snapshot_seq）；§10.1 批量与同步（snapshot+changes 闭合：export 返回 `snapshot_seq`，`:changes?since_seq=snapshot_seq` 无缝续接）；§4.7（COPY 流式 NDJSON + catalog 快照、`pg_dump -n tw_<project>` runbook）；〔新发现〕§9.3 教训 4（"import/export 的 NDJSON 面要先行（补 import）"——当前 export/import 均未实现）。
- **要做什么**：实现 `torchwood export --project`（流式 NDJSON + catalog 快照 + snapshot_seq）与 import；文档化 pg_dump 项目级备份 runbook；snapshot_seq 与 `:changes` 续接语义入契约。
- **完成判据**：export→drop→import 往返一致性测试（行数/内容/catalog 对照）；export 产出含 snapshot_seq 且 `:changes?since_seq=<snapshot_seq>` 无缝续接（一致性窗口用例）；13-operations 有备份/恢复小节。
- **建议归属**：export/import 专项会话。
- **闭环（2026-09-06，B5 会话 commit `635e214`）**：`torchwood admin export/import` 子命令（cmd/torchwood 直连 DB，import_guard 合规实测）——NDJSON 流式导出（每集合 keyset 500 行/页 + catalog manifest 末尾原子写出，半成品目录拒收）+ **REPEATABLE READ 单快照事务**内读取（outbox max(seq) → snapshot_seq，续接语义天然闭合）+ 行保真恢复（`_acl`/`_version`/时间戳原样，`tw_system` 直写 INSERT 500 行分批）。集合表重建复用 `createCollectionTable`/`reconcileVersionColumn`/`createCollectionIndex` 现役汇聚点（列授权/RLS policy/`_acl` GIN 零旁路）；physical_name 沿用导出值（数据文件按逻辑名寻址，与物理名解耦）。判据测试全绿：`TestExportImportRoundTrip`（2 库 3 集合含数组/vector 列、文档级 `_acl` 保真后 RLS 判定一致、catalog 逐字段相等）、`TestExportSnapshotSeqChangesContinuity`（snapshot_seq == max(seq)；导出后新建文档恰返回 1 条不含导出内容）、`TestExportImportEmptyProject`、`TestAdminExportImportRoundTripViaCLI`（cobra→直连全链路）。13-operations §6.3.1/§6.3.2 成文（用法 + 与 pg_dump 六维对照）。

### B6 孤儿消费组 XGROUP DESTROY 治理

- **出处**：redesign §6 阶段④完成状态（"孤儿消费组 XGROUP DESTROY 治理挂账"）；§4.5（组名 hostname:pid，重启即新组从 `$` 起步）。
- **要做什么**：worker 周期清理无成员且闲置超阈值的消费组，防组无限累积。
- **完成判据**：集成测试——伪造闲置组后触发清理、活跃组（有成员/PEL 未超时）不被删；清理行为有日志/指标可观测。
- **建议归属**：events/realtime worker。
- **闭环（2026-09-06，B6 会话 commit `f6529a7`）**：`streamTransport.SweepIdleGroups(ctx, idle)`（`internal/infra/realtime/stream.go:105`）——XINFO GROUPS 全列后按三条件销毁：①位点闲置（last-delivered-id 落后头部 >1h）②无活跃成员（consumer idle 超阈）③PEL 过时（空或全 idle；>1000 条保守保留）。worker `cleanupOnce` 经可选接口 `idleGroupSweeper` 触发（启动 + 每 10min），指标 `torchwood_realtime_idle_groups_destroyed_total` + 组名 Info 日志，失败 Error 不阻断。误删代价受控：NOGROUP → 实例重连重建 + outbox 重放兜底。miniredis v2.38 对 XINFO/DESTROY/XPENDING(idle) 全支持（源码核验），5 场景测试（`stream_test.go` 虚拟时钟：孤儿删/无证据+PEL 过时删/活跃留/位点未落后留/Stream 不存在空返回）+ worker 3 用例（触发传递/失败降级/无 sweeper 跳过）。

### B7 vector 配套暴露：ef_search 与调参

- **出处**：redesign §6 会话 #10（ef_search 挂账）；§10.5 P0 挂账行（ef_search/迭代调参暴露；〔新发现〕halfvec/sparsevec、embedding 接入同列挂账）；06-databases §6（"挂账转出 POC 前评估 ef_search/hnsw.iterative_scan 调参暴露"——注意：暴露本身是功能项归 B，是否暴露的拍板以决议记录闭环）。
- **要做什么**：将 ef_search（及 hnsw.iterative_scan 相关节流参数）暴露为查询级/集合级可调项（默认值 + 上限防滥用）；按需排期 halfvec/sparsevec 列类型与 embedding 接入。DSL 字符串形态维持拒绝（既定决策，重审归 C 区惯例）。
- **完成判据**：二选一闭环——实现：ef_search 可配置 + 召回/延迟对比测试佐证默认值 + 06-databases 同步；或决策不暴露：本文记录决议句（近重复簇召回边界维持文档化现状）。
- **建议归属**：vector 后续会话。
- **闭环（实现，2026-09-05，vector/B7 会话）**：ef_search 暴露为 **VectorSearch 查询级 optional int32**（`proto/shared/v1/query.proto` 字段 5；DSL 字符串维持拒绝——typed builder only 既定决策不变）。取值域 **[1,500]**：≤0 / >500 一律 InvalidArgument **显式拒绝、不静默 clamp**（R9 显式拒绝原则——静默改写让调用方误以为请求值生效；上限 500 防滥用）。注入 = 查询事务内 `SET LOCAL hnsw.ef_search = N`（与 iterative_scan 同款 GUC 事务级注入模式，`internal/infra/documentdb/postgres_document_query.go` listDocumentsKNN；校验在 `validateVectorSearch`）；**缺省不 emit 任何语句**——行为与 pgvector 缺省 40 逐字节一致。链路：proto → genproto（buf）→ `pkg/query.VectorSearch.EfSearch *int32`（presence 语义）→ `pkg/query/proto` codec 透传 → infra 校验/注入 → SDK 双面（Go `VectorSearchBuilder.EfSearch` / TS `vectorSearch().efSearch()`，缺省不设位）。测试（`vector_ef_search_test.go`）：`_RejectionMatrix`（0/-1/501/2^30 拒绝 + 边界 1/500 放行）、`_DefaultByteIdentical`（unset ≡ 显式 40 同果同距——缺省零回归锚点）、`_RecallAndLatency`（B2 近重复簇 + 稀疏可见行形态，995 隐藏 3 簇 × 5 可见，3 查询点聚合召回弱单调 recall(200) ≥ recall(40) 锁定 + 对比数字 t.Logf）。**取数**（本地 pgvector 0.8.6-pg18）：中等距离形态 recall 40/200 双双 100%（5/5×3 查询）；饱和形态同一索引状态下 40 与 200/500 召回一致（要么同满、要么同为 0——1k 行规模上该边界由 HNSW 图拓扑决定、跨构建 ~50% 波动，非 ef 梯度区间），**佐证默认值 40 在可达召回时不劣于更大 ef**；延迟（k=25、1k 行、RLS on，50 次均值）ef=40 ≈ 19–25ms、ef=200 ≈ 20–21ms（RLS 扫描主导，ef 差异不可辨）。06-databases §6 同步（ef_search 调参小节 + 饱和边界句改写——边界调参手段即本字段，iterative_scan 维持 strict_order 契约不暴露）。halfvec/sparsevec 与 embedding 接入仍按本条"按需排期"挂账。

### B8 encodeTokenData 不可达兜底清理

- **出处**：redesign §6 清扫会话 #9 记录项（"encodeTokenData 的 marshal 失败兜底仍产出不可解码的 v1:offset 形态（不可达路径，语义自不一致但无害，随下次触碰 crud 时顺手清理）"）。
- **要做什么**：清理该兜底分支——marshal 失败应返回错误，不再产出坏 token。
- **完成判据**：`pkg/crud` EncodePageToken 的 marshal 错误路径返回 error，且有注入 marshal 失败的单测。
- **建议归属**：pkg/crud 顺手清理（任何触碰 crud 的会话）。
- **闭环（2026-09-06，清扫 #9 S1-S5 会话 commit `cd8bbea`）**：`encodeTokenData`/`EncodePageToken` 签名改 `(string, error)`，删除 `v1:offset` 兜底产物；`PageTokenSeparator` 常量随 v1:offset 退役删除。**调用点 21 处全部透传**（encode 失败 = Internal）：gRPC handler 层 7 文件 14 处（account/consolegrpc/admins/apikeys/databases/functions/oauth_providers/projects）包装 `status.Error(codes.Internal)`；app 层（groups/storage paginate*）、bunrepo（outbox_repo/users_repo）、documentdb（collection DDL）沿既有错误路径。测试：`TestEncodePageToken_MarshalFailure` 包级 var `pageTokenMarshal` 注入必败 marshaler——无签名/签名两进程态均返回 error 且不产出 token，恢复后 round-trip 完好。

### B9 `_version` 列锁死完整版收口

- **出处**：redesign §9.2 采纳表（列级 GRANT"锁死 `_acl`/`_version`/`_tenant`；必须从一开始只按列授予"）vs §6 ③-b A6 表述修正（"`_acl` 锁死（经函数唯一通道）；`_version` 不锁列（CAS 守卫已足）"）——两处口径不一致，终态未留档。
- **要做什么**：拍板并收口：接受"不锁列"（勘误 §9.2 采纳表）或翻案实现锁列；redesign 与 06-databases 措辞对齐。
- **完成判据**：两份文档同口径（矛盾句消除）；若翻案锁列：列授权 + golden 测试更新。
- **建议归属**：文档勘误（或 documentdb DDL 会话，若翻案）。
- **闭环（接受"不锁列"）**：2026-09-05｜本条与文档勘误同一提交（`docs(design): B9 _version 列锁死口径统一`）｜裁决：接受"不锁列"——`UPDATE … WHERE _version = ?` 的 CAS 守卫已保证并发安全，写错版本只令语句自身失败（自伤不伤人），列级锁死边际收益为零，无需列授权与 golden 测试变更。redesign 同 commit 勘误四处旧口径：§9.2 采纳表"列级 GRANT"行改写为终态口径（`_tenant` 锁列 / `_acl` 经 `tw_set_document_acl` 唯一通道（R13a/R16）/ `_version` 不锁列 + 指向 §6 ③-b A6 表述修正）；§3.2 语义映射表 #9 机制列、§4.2 DDL 示意注释、§6 阶段表 ③ 行 ③-b 计划句同步补 A6 勘误注记。06-databases 复核无矛盾：§7 列级 GRANT 段已是终态口径（"`_version` 不锁列（CAS 守卫 … 已足，写错只会让自己失败）"），系统列表格 `_tenant`/`_acl` 锁死表述一致，零改动。全文 grep `_version`×锁 复核：其余命中均为 CAS/乐观锁/行锁语境，无第三处矛盾。

### B10 Agent 面契约补全〔新发现〕

- **出处**：redesign §4.1 Agent 面（`GET …/collections/{c}?as=jsonschema` 导出 JSON Schema 2020-12；`GET /.well-known/torchwood` 资源/算子/错误码目录）；§10.1（`:query?dry_run=true` explain（D3 未决）、OCC 冲突错误体带 `current_version`、on-behalf-of 委托（F2））。代码检索确认 as=jsonschema / well-known / dry_run / current_version 均未实现；已落地项不受此条影响（429 RetryInfo 精确退避、`api:apikey` 限流维度、`_created_by` 落 `key:<keyID>`）。
- **要做什么**：按 §4.1/§10.1 落地 Agent 面承诺：JSON Schema 导出、well-known 目录、（D3 契约定稿后的）dry_run、OCC current_version、（F2 形态定稿后的）on-behalf-of。
- **完成判据**：每个子项有 API + 测试 + 09-api-guide/14-agent-tools 文档；未排期子项在本条登记状态（做/不做 + 理由）。
- **建议归属**：Agent 面会话（api/proto + documentdb）。
- **闭环（2026-09-05，Agent 面会话；5 子项中 3 项落地、2 项依赖未决留待）**：
  ① **JSON Schema 导出**：`DatabasesService/ExportCollectionSchema`（REST `GET .../collections/{c}:exportSchema?as=jsonschema`，custom verb 与 `documents:count` 同惯例——网关按路径路由无法以 `?as=` 区分与 GetCollection 同形的裸路径，`as` 挂在动词上缺省 jsonschema），从 catalog attrs 生成 JSON Schema 2020-12（类型映射矩阵/required/系统字段 readOnly 注释/`deprecated` 关键字经 `Options.deprecated` 通道保留），物理名不出现；测试=类型映射矩阵 + 文档形态（纯函数）+ catalog 往返/NotFound/sentinel 拒绝（集成）+ handler `as` 白名单；文档=14-agent-tools §2 注。
  ② **well-known 目录**：`GET /.well-known/torchwood`（纯 HTTP 面，gateway mux HandlePath，公开端点）——算子全集（23 条，proto oneof 字段同步断言）、域码表 + retryable（构造期直读新增 `databases.ErrorCodeCatalog()`，零漂移）、databases 面 26 动词 REST 形态 + scope（`auth.APIKeyScopeRules` 直取）；四重防漂移测试；文档同上。
  ③ **OCC 冲突带 current_version**：`DOCUMENT.VERSION_CONFLICT` 的 ErrorInfo metadata 携带 `current_version=<探测 SELECT 读到的当前 _version>`（domain 新增 `VersionConflictError` 载荷类型，infra update/delete 三处冲突点零额外查询携带，app `MapDocumentDBError` 提取入 metadata）；测试=映射单测 + infra 两路 ErrorAs + app/server 端到端 metadata=="1" 且取值合并重试成功；文档同上。
  ④ **dry_run（:query?dry_run=true explain）——留待**：依赖 D3 契约定稿（explain 输出形态未决），D3 拍板后随查询契约专项落地。
  ⑤ **on-behalf-of 委托——留待**：依赖 F2 形态定稿（委托凭证/语义未决），F2 拍板后随认证专项落地。
  Commits：165abbd（current_version）→ 9606198（jsonschema）→ aab38ef（well-known）。

### B11 H2 上限族 enforcement〔新发现〕

- **出处**：redesign §11-J H2 决议数值（`_acl` ≤64 ACE；数组 ≤1000 元素；每集合列数软限 200；object 嵌套 ≤8 层）——检索确认无 enforcement（H1 的 1MiB/256KiB 已落地，见 06-databases §6 写入载荷上限）。
- **要做什么**：在 app 校验层落地 H2 上限族（写入/DDL 前置拒绝 + 明确域码）。
- **完成判据**：四个上限各有超限拒绝测试（含域码断言）；06-databases 输入上限小节同步。
- **建议归属**：app/documents 校验会话。
- **闭环（2026-09-05，app/documents 校验会话）**：四上限全部落地，常量集中 `internal/app/documents`（`MaxDocumentACL=64` / `MaxArrayElements=1000` / `MaxCollectionColumns=200` / `MaxObjectDepth=8`，注释标注 redesign §11-J H2，与 H1 常量同位）。`_acl` 校验收敛 app 层 documents 核（create/update/upsert/bulk 经 `validateACL` + Server execute-tx per-op，域码新设 `DOCUMENT.ACL_TOO_LARGE`；种子 ≤3 条天然合法无需豁免；RLS/adapter 层不设防——防御纵深已在列授权与 `tw_set_document_acl` 通道）；数组 1000 元素校验 data 通道（`ValidateDocumentPayload`，`[]any`/`[]string` 双形态）与 `array_updates` values 通道（超限复用 `DOCUMENT.TOO_LARGE`；DDL 通道无此面——array=true 已拒 default_value）；列数软限 200 落 CreateCollection（一次性声明）与 CreateAttribute（catalog 存量 +1）前置，域码新设 `CATALOG.COLUMN_LIMIT_EXCEEDED`（PG 1600 硬限留余量）；object 嵌套 ≤8 层并入 `ValidateDocumentPayload`（map 计一层、数组透明不计层，复用 `DOCUMENT.TOO_LARGE`）。测试：四上限各有超限拒绝（域码断言）+ 边界放行（`internal/app/documents/limits_test.go`、`internal/app/server/databases_limits_test.go`，execute-tx 面另有 ops[N].permissions violations 定位断言）；06-databases §6 输入上限小节已同步。

### B12 量化预警线 SLO 指标〔新发现〕

- **出处**：redesign §3.1 缓解 3 / §4.7（`pg_class` 计数、pg_dump 时长、迁移重放耗时纳入 SLO 指标；超限触发多集群分片规划）；§11-A3（policy × 集合规模对 plan cache/relcache 的影响观察项）。
- **要做什么**：三类规模指标接入 metrics/SLO（exporter 或控制面聚合），配阈值告警；A3 观察项随指标可评估。
- **完成判据**：metrics 端点暴露三指标 + 告警规则入库；阈值及其来源（§3.1 社区阈值：几百 schema 舒适、1–2 千起劣化）写入 13-operations。
- **建议归属**：运维可观测会话。
- **闭环（2026-09-05，运维可观测会话）**：7cf457c｜三指标落地——`torchwood_documentdb_tables_total{kind=project_schema|catalog|business}`（`pg_class`×`pg_namespace` 单语句聚合，`internal/infra/documentdb/scale_metrics.go`，server 启动钩子同步一次 + 小时级刷新，`cmd/server` `NewScaleMetricsHook` 经 `bootkit.NewOnStarts` 注入，A1 同形态；分片后每集群各自扫各自库）；`torchwood_documentdb_pgdump_duration_seconds`（进程内指标骨架恒 0，打点契约 = 外部 cron 经 Pushgateway/文本文件 collector 上报，13-operations §5.1 含脚本示例——POC 不做进程内 pg_dump 调度器，避免与关停排水/健康探测耦合）；`torchwood_documentdb_schema_migrate_duration_seconds`（`projectschema/migrator.go` `applyUpTo` 埋点，缓存命中直通不刷新，最近一次 Apply 语义）。阈值与告警规则入库 13-operations §5.1（表计数 >500/>1500 对齐 §3.1 社区阈值几百舒适/1–2 千劣化；pg_dump >1h/4h 对齐社区 24h+ 劣化谱系的早期档；迁移重放 >60s/项目经验基线），告警语义 = 触发多集群分片规划评估（§3.1/§11-G1，排期承诺挂 C7）；§11-A3 观察项随指标可评估。测试：`scale_metrics_test.go`（三平面计数与 pg_class 独立复核一致 + 增量敏感性 + nil 防御 + pg_dump 骨架）、`migrator_test.go` `TestApply_MigrateDurationGauge`（缓存直通不刷新/重放刷正）、`bootkit/hooks_test.go` `TestScaleMetricsHook_WiredInOnStarts`（钩子接线锁定，A1 测试共存）；documentdb/projectschema/bootkit `-p 2` 全绿。

### B13 低优先级小债打包〔新发现〕

四件既有挂账，优先级低、可各自独立闭环：

- **a. upsert 预锁冲突值键**（§4.8 Phase 1 裁决④："可选改进（预锁冲突值键）挂账"）。判据：死锁注入用例下 execute-tx/upsert 批内无 PG 死锁中止，或记录"依赖 PG 死锁检测 + 幂等重试"的维持决策。归属：documentdb 事务。
- **b. realtime 扇出掩码缓存**（§4.3/§11-J A5：预计算"频道×角色集→放行"短 TTL 缓存，纯性能优化未实施）。判据：扇出压测数据证明需要后实施，或记录"维持逐订阅者判定"。归属：realtime。
- **c. 物理名进程内缓存**（§6 阶段②挂账 + `internal/infra/documentdb/postgres_catalog.go:145`："业务库热路径 +1 主键点查……挂账未做（评估后置）"）。判据：基准证明点查开销可忽略则记录关闭，否则实现缓存 + 失效桥接测试。归属：documentdb catalog。
- **d. data_ref 版本化读取**（§4.5："原 data_ref = GET ?version=N 撤回……挂账远期版本化读取"）。判据：需求出现时立专项并在本条链接；无需求则维持登记。归属：远期专项。
- **闭环（2026-09-05，B13 打包会话；a/c 实现、b/d 维持，全部判据闭环）**：
  - **a. upsert 预锁冲突值键——实现（判据取数非零 → 预锁 + 测试转绿）**：死锁注入用例（`upsert_prelock_test.go` `TestExecuteTransactions_UpsertConflictKeyDeadlockInjection`：两并发批 documentID 集合不相交、冲突值键相反 X→Y vs Y→X，屏障同步 × 8 轮）实测**修复前 8/8 轮 40P01 死锁中止**（op[1] acquire upsert lock，同序对照组 0/8——判据取数非零 → 实现预锁）。修复 = `lockTxTargets`（`postgres_transactions.go`）批首把全部 upsert op 的冲突值键按**排序后预锁** `pg_advisory_xact_lock`：docID 族与冲突键族各自全局有序 + 每批先 docID 族后冲突键族 = 并发批拿锁全序兼容无环；`upsertDocument` 内原有取锁同事务可重入，P0-1 语义不变（冲突列非法/缺值 op 跳过预锁——执行体必然 InvalidArgument 无须互斥）。修复后同用例 3 次重复（24 轮）**0 死锁、0 其他错误**，单轮耗时 1s（死锁检测窗）→ 亚秒。
  - **b. realtime 扇出掩码缓存——维持逐订阅者判定（压测数字记录）**：扇出压测（`hub_load_test.go` `BenchmarkHubDispatch_FanoutLoadProfile`，200 订阅者 × 1200 事件，ACL 授予前半用户=拒绝路扫满 ACE 表的评估上界形态）：ACL 评估路径 Dispatch 均值 91.0µs、PlatformAdmin 旁路对照 42.3µs——**ACL 评估份额 ≈ 48.7µs/次（53.6%，244ns/订阅者/事件）**，`VisibleTo` 裸成本 61ns/eval；p50 实测 0s、p99 尖峰（~1ms）来自 GC/调度而非 ACL。绝对量级为纯内存 µs 级（同一事件链其余环节——Stream 消费/outbox 落库——均为 ms 级 I/O），不构成可测系统瓶颈；且掩码缓存键 =（频道×角色集）在 realtime 面"角色集 per-连接、ACL per-事件"命中率低、失效复杂。**决策：维持逐订阅者判定**，压测基准留存本文件，规模信号出现（单频道万级订阅 × 千级事件/秒）再立项。
  - **c. 物理名进程内缓存——实现（判据取数 ≥5% → 缓存 + 失效桥接测试）**：取数（`catalog_resolve_bench_test.go` `TestResolvePhysicalTable_PointQueryShare`，n=1000，SystemPrincipal 保守口径）：**缓存前 resolvePhysicalTable 主键点查 867µs/op，占 ListDocuments 全链（3.27ms）的 26.5%** ——远超 5% 判据 → 实现：`physicalNameCache`（`sync.Map`，逻辑 ID 三元组 → 物理名，`postgres.go` + `postgres_catalog.go`），业务库热路径命中后零额外往返。失效面 = catalog_collections 全部删除路径：`DeleteCollection` 单键失效、`DeleteDatabase` 库级批量失效、import 清位后写穿 manifest 物理名、`CreateCollection` 写穿覆盖（同名逻辑 ID 重建必得新物理名）；跨实例陈旧语义 fail-loud（42P01 表不存在，无静默错写，对比 internalIDCache 的静默分裂面）。**缓存后实测：resolve 509ns/op，占比 0.0%**。失效桥接测试锁定：`physical_name_cache_test.go` `TestPhysicalNameCache_DeleteRecreateBridge` / `_DeleteDatabaseBridge`（删建同逻辑 ID 后缓存收敛新名、新文档落新表、旧表消亡、旧文档不可达）。
  - **d. data_ref 版本化读取——维持登记（无需求）**：原契约 `data_ref = GET ?version=N` 已在 §4.5 撤回（文档表只存最新行，按版本拉取无地基）；本会话复查仍无需求信号（无版本化读取的用户请求/设计依赖），维持"远期版本化读取"登记，未立项、未实现。

### B14 per-key 角色与默认种子收敛〔C6 决议新立，2026-09-06〕

- **出处**：C6 决议（2026-09-06 维护者：**不接受"项目内全体 API key 互通"**——AI/Agent-Native 定位下多互信 Agent 同项目是主场景，共享语义与定位相悖）；redesign §10.1 信任边界 / §10.2-2；代码锚点 `internal/app/server/databases.go` `creatorSeedRole`。
- **要做什么**：① `key:{keyID}` 进入 API key 主体的角色集（`internal/infra/auth` validator + `databases.Principal`，机制基础已就绪——`_acl` 内嵌与 `type:role` 模型零改动支持）；② 空 ACE 种子收敛：API key 主体的种子从共享 `keys` 角色改为 `read/update/delete:key:<自身id>`（对齐 user 主体 owner ACE 语义，`creatorSeedRole` 分支调整）；③ 现存共享 `keys` 语义的兼容决策（存量集合的 `keys` ACE 保留与否 + 文档明示）；④ golden 测试补 per-key ACE 与种子用例。
- **完成判据**：不同 key 互不可见彼此私钥文档（集成测试：keyA 建、keyB 查 → NotFound）；显式授予 `key:<id>` ACE 后可跨 key 协作；RLS/policy 全套回归绿；06-databases 权限节与 §10.1 同步。
- **建议归属**：auth/权限会话（机制基础已就绪，预计中等工程量）。
- **闭环（2026-09-06）**：四步落地，commit `ff48ff9`（①validator 注入 `Roles=["keys","key:<id>"]`，三通道核对——EndUser 不触、DocPrincipal 透传、realtime 投影同步且 WS 拒 API key 语义不动）→ `0b0985e`（②`seedDocumentPermissions` 对 KeyID 主体种 `read/update/delete:key:<自身id>`，Create/Upsert/execute-tx 三入口一处收敛，R1 per-op 豁免结构与包 0-1 upsert 种子对齐不受影响，无 KeyID 合成主体维持历史行为）→ `ceac697`（③keys 兼容决策：**保留存量 keys ACE 不清除**——显式授予继续有效、零数据迁移；默认种子不再产生 keys ACE；06-databases §7/§10 + 05-authentication + 14-agent-tools 明示"默认私有（per-key）、跨 key 协作需显式授予 key:<id>、遗留 keys ACE 仍共享"）→ `3e4cdda`（④golden 矩阵 tw_can/tw_visible 补 key:<id> 行 + 行为级 ka/kb 双 key 断言；集成测试 `TestDatabases_PerKeyDocumentIsolation`——keyA 空 ACE 建、种子恰为 key:ka 三连、keyB Get=NotFound/List 不含/Count=0、显式授予 read:key:kb 后 keyB 可见且只读、keyA 全权不变；`TestDatabases_PerKeySeedViaExecuteTransactions` 锁事务批种子）。判据取数：单文档面与 execute-tx 面的 keyB 不可见断言均以 `NotFound`/空集通过；回归面 RLS 全套（golden/behavior/自锁/tenant/EXPLAIN/基准）、permissions、`TestRolesSig_*`（sig 消息自动覆盖新角色，4 用例绿）、execute-tx 全量、A1 grants reconcile（零角色串引用，确认与角色无关）、app/api/grpc/domain 全量、`go build ./... && go vet`、sdk/go 与 TS 全量——全绿。

### B15 roles_sig 密钥面收口〔A2 残余风险，2026-09-06 验收审查立条〕

- **出处**：A2 闭环行自认的残余风险（"authenticator 持 `tw_secrets` 四权可读 roles_sig 密钥……消除路径 = 密钥落库改部署期 owner 一次性作业"）；收官验收审查裁定必须登记去向（复核或入 B 区）。
- **威胁模型**：authenticator DSN（非 superuser、受 RLS）若泄漏，攻击者当前可读 `tw_secrets` 中的 roles_sig 密钥 → 伪造 `app.roles`/`app.roles_sig` GUC 提权（绕 RLS 读任意行）。前提是 DSN 已泄漏，但提权是真实增量（受 RLS 的账号 → 可任意提权）。
- **要做什么**：`SyncRolesSigKey`（运行 DSN 启动钩子）改为**部署期 owner 一次性作业**——`torchwood admin sync-roles-sig`（对齐 B5 admin 直连形态）或部署脚本步骤，运行 DSN 撤销 `tw_secrets` 全部权限（REVOKE 后运行态不可读密钥 → 伪造通道封死）。
- **完成判据**：authenticator 对 `tw_secrets` 无任何权限（`\dp tw_secrets` 断言）；密钥落库走 admin 作业且运行态注入验签全路径回归绿；13-operations 部署步骤同步。
- **建议归属**：auth/clients 会话（转出 POC 前必须完成，与 A2 同批部署验证）。
- **闭环（2026-09-05）**：三步落地。① **admin 作业**：`torchwood admin sync-roles-sig`（`cmd/torchwood/cmd/admin_roles_sig.go`，对齐 B5 admin 直连形态——`--dsn` 缺省读 `TORCHWOOD_DATA_DATABASE_SOURCE`（作业场景必须指向 owner/引导账号，与运行态 DSN 分别注入）、`--jwt-secret` 缺省读 `TORCHWOOD_SECURITY_JWT_SECRET`（必须与运行态 `security.jwt.secret` 同源同值，派生钥 = HMAC-SHA256(主密钥, "tw-roles-guc-v1")），内部复用 `clients.InitRolesSigKey` + `SyncRolesSigKey` 双钥四语句平移（R16/A4 语义不变），幂等重跑安全；② **运行钩子退役**：`bootkit.RolesSigKeySyncHook` 从 `NewOnStarts` 移除（server/worker 均不再落库，wire 重生成），`InitRolesSigSigning` 进程内派生保留——注入 GUC 用的是进程内钥，无需读库；③ **权限收口迁移 `000033_roles_sig_key_plane`**（前向补丁，遵 A5"已应用迁移永不原地修订"；规划编号 000032 已被 catalog_migrations 占用故顺延）——`REVOKE ALL ON public.tw_secrets FROM tw_authenticator`（DO 块守护角色存在性；000029 `REVOKE ALL FROM PUBLIC` 后 base identity 直接 GRANT 是唯一可达通道，撤销即收口），SECURITY DEFINER 验签链核对不受影响（`tw_sig_match`/`tw_roles`/`tw_tenant` owner = 迁移执行者 = `tw_secrets` 表 owner，走 owner 隐式全权，无需显式 GRANT；`tw_set_document_acl` owner = tw_system 不读密钥表），down 对称恢复旧授权形态（配套回滚 B15 前二进制）。**时序契约**（13-operations §4.5/§6.1 成文）：迁移（含 000033）→ `torchwood admin sync-roles-sig` → server/worker 启动；首次部署或换钥后未跑作业前文档查询 fail-closed（零角色不可见）属预期，跑完作业即恢复（验签实时读表，无需重启）。**测试证据**（判据四条）：`TestNonSuperuserAuthenticator_MigrateAndSmoke` 改造（owner 落库 + authenticator 只注入验签；`has_table_privilege` 七特权位 SELECT/INSERT/UPDATE/DELETE/TRUNCATE/REFERENCES/TRIGGER 全 false + `pg_get_userbyid(relowner) ≠ authenticator` 断言 + RLS 写读冒烟 count=1）；`TestAdminSyncRolesSigViaCLI`（cobra 全路径：首落库 current=新钥/previous=旧钥、同钥重跑幂等 no-op、二次换钥 previous 平移 + third 条裁剪行数 ≤2）+ `TestAdminSyncRolesSig_RequiresFlags`（缺 DSN/缺主密钥 fail-fast）；`TestRolesSig_KeyNotSeeded_FailClosed`（钥未落库 → `tw_roles()` 零角色/`tw_tenant()` NULL/文档不可见 → owner 重跑作业恢复可见）；`TestMigrations_UpDownUpCycle`（000033 up→down→up 循环全绿，DO 块守护幂等）。回归面：RLS 全套（golden/behavior/自锁/tenant 绑定/ef_search/KNN 抽样）、`TestRolesSig_*` 全部、`TestDatabases_PerKeyDocumentIsolation` per-key 隔离、`go build ./...` + `go vet` 全量——全绿（详见 13-operations §4.5 授权面结论更新：运行 DSN 泄漏不再可读密钥，GUC 伪造通道封死）。

## C 决策确认区（7 条）

### C1 collectionID 字符集放宽

- **出处**：redesign §4.2（"原草图的 `[a-z0-9-]` ≤36 挂账待需求信号：snake_case 与属性键习惯一致，且避免 `_perms`/realtime 频道约定动荡"）；06-databases 不变量 #9；§6 阶段②挂账。
- **决策问题**：是否放宽 collectionID 至 `[a-z0-9-]` ≤36——真实用户对连字符 ID 的需求是否存在；放宽对 realtime 频道约定与逻辑名组合校验的连带。
- **完成判据**：决议回写本文（放宽→B 区立条：校验/文档/测试更新；维持→记录"维持 snake_case"并关闭）。
- **建议归属**：产品 + pkg/ident 校验。
- **决议（2026-09-06，维护者）**：**维持 snake_case**——物理名解耦后用户对 collectionID 形态可见性低，连字符需求出现时仅需放宽入口校验（无数据迁移）。关闭。

### C2 users 面 typed AST 化

- **出处**：redesign §6 阶段①剩余记录项（"users/storage 等静态表面遗留的 DSL 消费为 §0 边界邻居，归阶段②收敛"）；§6 阶段②完成状态（storage + groups queries 显式拒绝、users 面独立 DSL 契约注释——收敛未完成，users 面仍消费 DSL）。
- **决策问题**：users 面查询是否统一到 typed AST（与文档面单栈对齐），或长期维持独立 DSL 契约。
- **完成判据**：决议回写本文（AST 化→B 区立条；维持→06-databases 注明长期边界 + 理由句）。
- **建议归属**：users 面/API 会话。
- **决议（2026-09-06，维护者）**：**维持独立 DSL 契约**——users 面是平台内部管理面（`ParseUserList` 白名单表达力恰好够用），统一到 typed AST 是纯粹一致性收益、需动 Account 链路。06-databases 注明长期边界。关闭。

### C3 dedicated 供给档位

- **出处**：redesign §9.3 教训 1（"吸收 dedicated 供给档位：同一 API 面下提供独享库（可 resize/replicate），把规模问题变成计费问题而不是接口问题"）。
- **决策问题**：是否提供 dedicated 数据库档位及计费形态；与 §3.1 多集群分片出口的关系。
- **完成判据**：产品决议回写本文（做→排期与计费方案链接；不做→理由句）。
- **建议归属**：产品/计费。
- **决议（2026-09-06，维护者）**：**远期——随云形态商业化排期**（触发条件 = 云形态上线且有单项目负载隔离诉求；与 C7 分片出口互补：dedicated 管中小隔离、分片管超大）。当前单实例 POC 形态无从谈起。关闭（远期监控）。

### C4 NULLS LAST 游标

- **出处**：redesign §4.1（"NULLS LAST 谓词改写仅在需求出现时评估"）；06-databases §9 预决策 4（NULL 排序键限制：cursor 行含 NULL 键拒绝、数据行 NULL 键续页被跳过——先 isNull/isNotNull 过滤）。
- **决策问题**：NULL 密集排序列的分页是否需要 NULLS LAST 谓词改写；当前过滤绕行是否够用。
- **完成判据**：决议回写本文（做→B 区立条并附行比较 × NULL 组合正确性测试方案；不做→维持现状句）。
- **建议归属**：query 会话。
- **决议（2026-09-06，维护者）**：**维持现状**——已文档化绕行（NULL 密集列先 isNull/isNotNull 过滤）是标准做法；行比较 × NULL 组合的谓词改写正确性风险高（B2 tie 组教训同类），无真实用户报告前不启动。关闭。

### C5 native 数据库独立产品（泄压阀）

- **出处**：redesign §10.4 承诺对价（"重度 SQL 用户 → 远期评估'原生数据库'独立产品（资源池隔离、不共用文档面信任模型）；触发器：当高级用户索取 SQL 的诉求成为规模化声音时按优先序评估，而非直接开口"）。
- **决策问题**：是否/何时启动 native 产品立项（裸库上无 ACL/审计/配额故事的提前量）。
- **完成判据**：触发器状态与决议回写本文（未触发→记录"监控中"；触发→立项文档链接）。
- **建议归属**：产品远期。
- **决议（2026-09-06，维护者）**：**监控中，不启动立项**——触发器（高级用户 SQL 诉求规模化）当前无信号。触发时按 §10.4 优先序评估立项（裸库 ACL/审计/配额故事需提前设计）。关闭（监控态）。

### C6 Agent 间隔离与默认种子语义〔新发现〕

- **出处**：redesign §10.2-2（"Agent 间零隔离：全体 key 共享 `keys` 角色……任一 key 可改其他 key 创建的一切文档。per-key 角色落地前，至少应把默认种子收敛为创建者 key 私有"）；§10.1 信任边界（"`key:{keyID}` 成为一等可授予角色……空 ACE 种子从'全体 keys 可写'改为'创建者 key 私有'，随 §3.1 的 keys 默认写权定调一并决策"）。代码锚点：`internal/app/server/databases.go` `creatorSeedRole`（空 ACE 种子绑首个常规角色——API key 主体即共享 `keys` 角色，非 `key:<id>`）。
- **决策问题**：对外发布是否接受"项目内全体 API key 互通"语义；若不接受，per-key 角色与种子收敛的排期。
- **完成判据**：决议回写本文（接受→契约文档明示共享语义；不接受→B 区立条：per-key 角色 + 种子收敛 + golden 测试）。
- **建议归属**：产品 + auth/权限会话。
- **决议（2026-09-06，维护者）**：**不接受"项目内全体 API key 互通"**——AI/Agent-Native 定位下"一个项目跑多个互不信任的 Agent"是主场景，共享语义与定位相悖（任一 key 泄漏即可改写全部 Agent 数据）。→ **B 区立条 B14**（per-key 角色 + 种子收敛 + golden 测试）。本条关闭（实现归 B14）。

### C7 多集群分片排期承诺〔新发现〕

- **出处**：redesign §9.3 教训 5（"§3.1 的多集群分片出口要有排期承诺，不能永远停在预警线"——Appwrite #6968 无回应即关闭的反面教训）；§3.1 缓解 4 / §11-G1（project→cluster 路由抽象，catalog 定位 cluster 内全局）。
- **决策问题**：分片出口的触发阈值与排期承诺（结合 B12 预警线指标定档）。
- **完成判据**：决议回写本文，含触发条件与时间窗（或"按指标触发、暂无日历排期"的显式承诺句）。
- **建议归属**：产品/架构。
- **决议（2026-09-06，维护者）**：**按 B12 预警线指标触发、暂无日历排期（显式承诺句）**——`torchwood_documentdb_tables_total`（project_schema+business 合计）**连续 7 天 > crit 阈值 1500** 即启动多集群分片规划评估（§3.1/§11-G1：project→cluster 路由抽象、catalog cluster 内全局）；warn 阈值 500 触发容量复盘。承诺不沉默：触发即评估，评估结论回写本文。

## 附：条目统计与闭环纪律

- **分区统计**（2026-09-05 成文时点）：A 区 10 条、B 区 13 条、C 区 7 条，合计 30 条；其中标〔新发现〕8 条（A5 并入 1、B4/B10/B11/B12/B13 独立 5、C6/C7 独立 2——B5/B7 各并入 1 处子项）。
- **统计更正（2026-09-06，收官验收审查）**：~~"A 区 10 条全部闭环 / 无活跃工程项"~~ **A 区实为 6/10 闭环**（A1-A6 有闭环证据；**A7 绝对 P99 基准 job 未实施、A8 沙箱演练记录缺失、A9/A10 维护者拍板句待回写**——此前统计行登记不实，验收审查抓出后本行如实更正）。B 区 14 条全部闭环属实（B14 per-key 四步已验证）。C 区 7 条决议闭环属实。
- **处置完成（2026-09-06，验收条件三项全落地）**：① A9/A10 拍板句按维护者"全部按推荐"授权回写**闭环**（A9 维持 locale=C；A10 0.x minor 携带破坏性 + 1.0 绑定门禁）；② A7 登记真实状态——**相对基准 + EXPLAIN 门禁已常驻 CI**（ci.yml "RLS performance relative benchmark" 步骤），绝对 P99 数值门禁按原定位显式挂转出后（专用基准规格）；③ A8 沙箱演练完成（13-operations §6.7 成文：探测四查询 + 清理流程 + **双沙箱库实测发现 membership 为集群级、单库 down 全集群撤销运行时身份**——runbook 必备结论）；B5/B6/B8 闭环证据行补登；A1 过时措辞修正；**A2 残余风险立 B15**（roles_sig 密钥面收口，转出 POC 前必须完成）。
- **当前状态（2026-09-06 收官验收处置后）**：**A 区 9/10 闭环**（A1-A6、A8、A9、A10；A7 部分闭环——相对基准常驻 CI、绝对 P99 显式挂转出后）；B 区 15 条（B14 落地 + **B15 新立待实施**）；C 区 7 条决议闭环。**活跃工程项 = B14 ✅ 已闭环 + B15（转出前必须）**。
- **B15 闭环（2026-09-05 实施回写）**：roles_sig 密钥面收口三步落地（admin 作业 / 运行钩子退役 / 迁移 000033 权限收口 + 时序契约成文 + A2 测试改造），判据四条全绿——**B 区 15/15 全部闭环，活跃工程项清零**（A 区维持 9/10：A7 绝对 P99 按定位挂转出后，非工程项）。转出 POC 前安全项清零。
- **闭环纪律**：满足完成判据后在条目下追加证据行（`闭环：<日期>｜<commit/测试/决议链接>｜<摘要>`）；A 区全部闭环前，发布流程（release workflow）不得执行对外发布步骤。
- **与 redesign §6 的关系**：redesign §6 挂账清单为 2026-09-05 快照；本文件为活跃清单，后续新挂账一律登记于此。
