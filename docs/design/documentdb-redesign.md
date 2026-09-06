# DocumentDB 子系统重设计（从零方案）

> 状态：**设计提案，已全量落地（2026-09-05 演进路径总览闭环）；当前为 POC 阶段，无向后兼容义务**。架构现状以 `docs/developer/06-databases.md`（含 §0 子系统定义与边界）为准。
> POC 含义：proto/API 可直接做破坏性修改（删除字段仍用 `reserved` 登记字段号——字段号卫生习惯而非兼容义务，跳过 `deprecated` 过渡态）；错误码与语义直接替换、不留旧文案映射；无灰度双读、无特性开关；本地/测试数据可随时重建（`docker:purge` + `db:migrate`），不做存量数据迁移。状态头义务（"转出 POC 前需重审本文所有'直接切换'类表述并补迁移方案"）已履行：全部直切点的存量升级路径见 `docs/design/poc-to-release-migration.md`（2026-09-05，15-exit-poc A5 拍板附件）。
> 来源：2026-09 两轮深度评审（documentdb 适配器 + 全方案外围）得出的问题清单作为设计输入，三路独立设计（正确性/一致性、开发者体验/API 面、规模/性能/运维）交叉验证后裁决成文；2026-09-03 按维护者决策修订 §3.1（Typed 真实列）并补 §3.2（RLS 语义等价性验证）、§9（竞品路线调研）；同日实施前评审修订 §2-C4（缺省语义分层表述，消除与 §4.1 的矛盾）、§4.8（现状 Bulk 锁纪律注意）、§6（标识符组合校验与错误码格式登记）、§11-A4（POC 空问题注记），包 0 锚点经代码逐项核验全部成立。
> 相关：`docs/developer/06-databases.md`、`docs/design/schema-naming.md`、`docs/design/v2-events-and-realtime.md`。
> **迁移基线重定（2026-09-06）**：`db/migrations` 已从 33 个迁移重定为 5 个基线迁移（000001 控制面 / 000002 事件脊柱 / 000003 全局 catalog+账本 / 000004 RBAC·RLS·roles_sig / 000005 pgvector）；拍板依据 = 无生产存量库、全部环境重建。本文出现的历史迁移号按此映射、正文不改写：000025/000032→000003，000026/000027/000029/000031/000033→000004，000028→000002，000030→000005。

---

## 1. 方法与输入

三路设计互相独立完成（各自先读代码校准地面真值），再交叉验证：三方**独立收敛**的决策视为高置信度直接采纳；实质分歧由交叉验证裁决（§3）；各方案中被证伪/过度的部分在裁决中修正。2026-09-03 追加三路外部调研（竞品路线、PG 技术栈、RLS 等价性，见 §9）校准裁决。

设计输入（两轮评审结论摘要，问题锚点见 `docs/developer/06-databases.md` §0 不变量 9/10 与历轮 review）：

1. 标识符 63 字节截断：`collectionID`/属性 key/索引 ID 无长度上限，两个仅 64+ 字节不同的集合映射同一物理表；索引名 `idx_<coll>_<id>` 同面碰撞。
2. 权限 SQL 谓词（`listPermissionFilter`）与内存判定（`AllowsDocumentAccess`）双实现靠纪律对齐；update 路径 check-then-write 无行锁（已接受 TOCTOU）。
3. `_perms` 独立表：列表查询每条带 EXISTS 子查询；权限行与数据行跨表原子性靠应用层事务。
4. OCC 语义混乱：`version_required`/`version_mismatch` 错误码错位（version=0 报 FailedPrecondition）；Upsert 空 ACE 种子与 Create 不一致会锁死文档。
5. 分页：自定义排序缺 `_id` tiebreaker → 重复键跨页丢行；cursor 只取 `Orders[0]`；offset 与 keyset 双 token 族语义漂移。
6. 契约断裂：`default_value` 物理生效但 catalog 不落库；CreateCollection/CreateAttribute 响应是请求回显；`queries` 在部分 List 接口被静默忽略。
7. DDL 全在事务内持 AccessExclusiveLock（CREATE INDEX 非 CONCURRENTLY）。
8. 事件：at-least-once 但慢消费者静默丢帧与"已发布"并存、无顺序保证、PUBLISH 0 订阅者算成功。
9. 规模：schema-per-project 扇出（数千项目 = 数千 schema × catalog 四表），`EnsureAll` 启动风暴、pg_dump/迁移重放压力；每项目 catalog 无全局视图。
10. 查询双栈（DSL 字符串 vs typed AST）算子集不对齐、互斥校验复杂。

## 2. 三方收敛点（高置信度决策）

| # | 决策 | 要点 |
|---|---|---|
| C1 | **catalog 全局化** | 单套全局 catalog（含 default/数组/size 全量属性契约），消灭"每项目四表"的漂移与无全局视图；属性定义 JSONB 落库为唯一契约源 |
| C2 | **keyset-only 分页** | 废弃 offset 双 token 族；cursor 编码完整排序键 + `$id`；ORDER BY 编译器强制追加 `$id` tiebreaker（现状缺陷的机制化修复）；`total` 移出 list，count 独立 API 且带 statement_timeout |
| C3 | **权限 ACE 内嵌文档行** | `_perms` 独立表退役：文档行内嵌 `_acl text[]` + GIN 索引；与数据行同表同事务天然原子；空数组=无文档级 ACE 回退集合级（B1 语义不变） |
| C4 | **OCC 显式三态** | `expected_version` **原语语义**三态：设置 → CAS（进同一条 DML 的 WHERE）；缺省 → 盲写 +1；0 → InvalidArgument。消灭 check-then-write 与错误码错位。各 API 面对"缺省态"的暴露由 §4.1/§4.4 分别定夺：单文档 Update/Delete 缺省即拒（强制乐观锁，与现状一致），Upsert/Bulk 与 §4.8 op 模型把缺省显式契约化为 LWW 盲写 +1（评审修订：2026-09-03，消除原"一律缺省盲写"表述与 §4.1 的矛盾） |
| C5 | **在线 DDL（expand-contract）** | 一律 CONCURRENTLY + 独立事务 + `lock_timeout` 重试；catalog 侧索引/迁移两阶段状态机（building→active）；后台 reconcile 对账（缺列/INVALID 索引/幽灵表） |
| C6 | **事件顺序化 + 补偿** | 事务性 outbox 保留；事件携带单调 `seq`（全局分配、按 collection 过滤重放）；断线带 `last_seq` 重放保留窗口 + `:changes` 补偿拉取；慢消费者超水位主动断开下发 RESYNC，不再静默丢帧 |
| C7 | **查询单栈** | 规范模型只有一个 typed AST；Appwrite DSL 降级为 SDK/URL 层语法糖（构造器内部序列化为 AST）；双栈互斥校验消失 |
| C8 | **写响应一律读回** | 一切写操作返回服务端完整读回（含 created_at、default 生效后的值），禁止请求回显 |

## 3. 冲突裁决

### 3.1 存储模型：Typed 真实列为唯一形态（2026-09-03 维护者决策）

- **分歧**：正确性路线主张每集合一张 typed 真实列表（约束下推为 DB NOT NULL/UNIQUE/CHECK）；规模路线主张默认 JSONB 共享分区表 + 大集合晋升 typed 专表（relation 数量是单 PG 死亡线）。
- **裁决**：**Typed 真实列**。依据：① 类型化集合是产品契约本身（Appwrite TablesDB 同型卖点，且竞品调研证实 Appwrite 生产环境就是 table-per-collection，见 §9）；② NOT NULL/UNIQUE/CHECK/DEFAULT 约束下推、查询性能、索引表达力都是 JSONB 给不了的；③ pgvector 等 PG 扩展列与 typed 列同域管辖，直接强化 AI/Agent-Native 定位；④ FerretDB（"协议 shim + 通用翻译层"）的失败反证：文档 API 的正确外壳是"typed 专属 schema + 自有契约"，不是通用文档堆。
- **规模代价的替代缓解**（不再引入 JSONB 长尾/晋升双路径，避免双形态复杂度）：
  1. **全局 catalog** 消灭每项目 4 张固定表（relation 膨胀的最大常数项：项目数 × 4）；
  2. **标识符治理**：逻辑 ID 限长（collectionID ≤36 `[a-z0-9-]`）+ 物理名服务端分配（`c_<base32>`），63 字节截断类缺陷机制性不可达；
  3. **量化预警线**：`pg_class` 计数、pg_dump 时长、迁移重放耗时纳入 SLO 指标——社区阈值：几百 schema 舒适、逼近 1–2 千 schema 或表数上万后 pg_dump/relcache/autovacuum/迁移先后劣化（§9 引证）；
  4. **超限演进 = 多集群分片**（project → cluster 路由抽象，横向复制现有模型），不改存储形态、不动产品语义；
  5. 每项目集合数/索引数配额下推。
- **类型系统补全**：数组落地为 PG 原生 `T[]` 列（现状被拒的 array 属性补齐）；`vector` 列（pgvector + HNSW）作为可选类型；`relation(target, onDelete)` 远期（物理 FK 列，仅 eq/in 按目标 id，无服务端 join）。

### 3.2 RLS 能否等价表达 `_perms` 语义——核心判定完全可下沉；唯一语义冲突已由产品决策消解，RLS 为**判定执行点**

外部调研（PG 官方文档 + Supabase/PostgREST 实践，引证见 §9）逐语义验证结果：

| # | 现有语义 | RLS 机制 | 等价性 |
|---|---|---|---|
| 1 | read/update/delete 分离 | `FOR SELECT/UPDATE/DELETE` 独立 policy（USING=可见行静默过滤；WITH CHECK=新行校验报错） | ✓ 完全同构 |
| 2 | create 只看集合级 | `FOR INSERT ... WITH CHECK (集合级子查询)` | ✓ |
| 3 | 文档级覆盖 + 集合级回退（B1） | `_acl && read_aces OR (无 read ACE AND 集合级命中)` | ✓ |
| 4 | documentSecurity 开关 | policy 内条件分支（catalog 子查询） | ✓ |
| 5 | 角色注入（any/users/user:/group:…） | 应用层计算 → `SET LOCAL app.roles`（`\x1f` 分隔）→ STABLE 函数 + `(SELECT ...)` InitPlan 化 + `GIN(_acl)` 索引条件 | ✓（需性能纪律） |
| 6 | 列表 = 单条同一语义 | 同一 policy 服务一切查询路径 | ✓（单源化的机制保证） |
| 7 | Upsert 冲突行按 update 检查 | ON CONFLICT 同时受 INSERT WITH CHECK 与 UPDATE USING 约束，失败**报错**不静默跳过 | ≈ 略收紧（拟插入行也须过 create 检查，语义从"分叉"变"交集"） |
| 8 | 系统/平台管理员旁路 | 独立 `tw_system BYPASSRLS` 连接角色（绝不编码进 GUC 或 policy 白名单） | ✓ |
| 9 | 元数据列保护（防伪造 `_acl`/`_version`） | 列级 GRANT（只授数据列；表级与列级权限是并集，必须从一开始就只按列授予）；A6 勘误（2026-09-05）：仅 `_tenant` 锁列，`_acl` 经 `tw_set_document_acl` 唯一通道，`_version` 不锁列（CAS 守卫已足，见 §6 ③-b A6 表述修正） | ✓ |
| 10 | ~~"可 update 不可 read"（现状 D3 决策）~~ | PG 规定 UPDATE/DELETE 的行必须**同时**通过 SELECT policy（AND 叠加） | **已消解**（2026-09-03 维护者决策：接受"可 update/delete 就能 read"），见下 |
| 11 | Get 不可见 → 404（防枚举） | RLS 静默过滤天然产出"不可见=不存在"，与现状一致 | ✓ |
| 12 | 授予治理（不可授未持有角色/any 写） | 不属 RLS 职责（RLS 管 ACL 上的数据访问，不管 ACL 变更权；`_acl` 列已被列级 GRANT 锁死） | 留应用层 |
| 13 | 存在性侧信道 | 唯一键冲突/FK/ON CONFLICT arbiter 报错会泄露不可见行存在（官方 "covert channel"）；TRUNCATE 不受 RLS（须 REVOKE 挡）；COPY FROM 对 RLS 表直接不支持 | 已知豁免面，写进威胁模型 |

**对 #10 的落地（2026-09-03 决策："可 update 就能 read"）**：产品语义变更为**写权蕴含可见**，以"可见谓词"形式在两层同时生效：

```
tw_visible = tw_can('read') ∨ tw_can('update') ∨ tw_can('delete')   -- 能读、能改、能删 → 可见
```

- **SELECT policy = `tw_visible`**（而非仅 read），UPDATE/DELETE policy 分别查各自 op——持 update-only ACE 者可见且可改，持 read-only 者可见不可改。这不是 DB 层妥协，而是**产品语义本身**：应用层的 Get/List 可见性判定使用同一 `tw_visible`（现状 D3"仅查 update 不预检 read"被有意取代）。
- **RLS 升级为判定执行点**：应用查询构造器不再手工拼权限 WHERE——policy 的 securityQuals 隐式过滤即判定（列表、count、单条、bulk 全部同源）；`tw_can`/`tw_visible` 仍单源，宿主是 policy 与测试基准。
- **错误映射**：读路径 0 行 → `NOT_FOUND`（防枚举，与现状一致）；写路径 policy 违例（SQLSTATE 42501 "violates row-level security policy"）→ `PERMISSION_DENIED`。
- **两个连带的语义变化，随契约层显式声明**：
  1. **写权=可见**（本决策）：授予 `update/delete` 隐含授予读可见性；存量 update-only ACE 自然获得可见性（良性放宽，无数据迁移）；
  2. **Upsert 收紧**（#7 的既成后果）：ON CONFLICT 路径下拟插入行须过 INSERT WITH CHECK（create）、冲突行须过 UPDATE USING（update），即 **upsert 需同时持有 create 与 update 权限**（现状是"命中行只查 update"的分叉语义）。

**落地勘误与定稿（2026-09-04 会话 #6 实施后，PG 18 实证修正三处机制假设——语义决策不变，实现路径修正）**：

1. **自锁与 `_acl` 第二语句**：PG 对修改 SELECT policy USING 引用列（`_acl`）的 UPDATE，**以新行复检 SELECT policy**——上文"WITH CHECK=恒真即保自锁"的假设不成立（复检与 WITH CHECK 无关）。定稿：主语句只 SET 数据+审计+`_version`（policy USING 裁决行级 update），**`_acl` 替换走同事务内 `tw_system` 身份的第二条 UPDATE**（以主语句成功为门槛；越权面=主语句 policy + app 层授予治理，与旧 `_perms` 可写面等价）。自锁语义完整保留。
2. **Upsert 拆掉 ON CONFLICT**：PG 的 ON CONFLICT 推测插入要求**拟插入行通过 SELECT policy**，与 `tw_visible` 结构性冲突（无读授权集合上的 upsert 必失败）。定稿：预查（advisory lock 串行）分支——纯插入走普通 INSERT（WITH CHECK 裁决 create）、命中走普通 UPDATE（USING 裁决 update）；上文连带变化 2 的"需同时持有 create 与 update"修正为**"分支两侧分别裁决"**。残余语义变化：与并发普通 Create 撞唯一键报 DuplicateKey（可重试）而非转 update 支；存在性泄露经唯一索引属 #13 已知豁免面（普通 Create 同样可探测，非新增暴露类）。
3. **delete 路径去 FOR UPDATE**：`SELECT ... FOR UPDATE` 叠加应用 UPDATE policy USING——delete-only 用户会被误拒。定稿：delete 预读走无锁 SELECT，OCC 由 **DELETE 语句内 `_version` CAS 守卫**（单语句 compare-and-delete）承载，比"锁行读版本再删"的竞态窗口更小。
4. **错误映射定稿（含对上文 #11 的修正）**：不可见行 Get/Update/Delete → `NOT_FOUND`（**这是 403→404 的有意翻转**——上文"与现状一致"表述有误，现状为 403；翻转符合防枚举与 PostgREST 惯例）；**可见不可写行**（如 read-only 用户发起 Update）→ 0 行三态探测后 `PERMISSION_DENIED`（"可写即可读"使不可见⇒不存在成立，可见不可写是独立态）；42501 WITH CHECK → `PERMISSION_DENIED`。
5. **`tw_visible` 签名定稿**：增加 `docsec` 参数——`document_security=false` 时走纯集合级分支（ACE 不参与；集合级 read ∨ update ∨ delete 蕴含可见，即"可写即可读"在集合级的一致延伸）；UPDATE/DELETE policy 以 `CASE docsec` 分派。

**工程纪律**（违反则性能崩塌，均有实测数字背书，见 §9）：
- **事务模型（A1，2026-09-03 接受）**：所有读写一律经显式事务（autocommit 退役），事务首条 `set_config('app.roles',…,true)` 注入身份——漏注入=空结果（fail-closed），事务结束自动失效零残留；否决会话级 GUC（错配路径静默继承上一用户角色）。
- 所有跨表取值（catalog、角色函数）一律 `(SELECT ...)` 标量子查询包裹强制 InitPlan（每语句一次）——裸写是逐行 SubPlan：实测 100 万行 8100ms vs InitPlan 93.7ms；Supabase 官方基准裸 policy 1840ms vs initplan+索引 0.21ms；
- 身份函数 `STABLE`（`current_setting` 本身 STABLE，但不包裹仍退化逐行）；
- `_acl` 建 GIN（`&&` 可作索引条件）；
- 每 policy 上线前 EXPLAIN 验证。
- **GUC 伪造面**：自定义 `app.*` GUC 任何能执行 SQL 的会话都能 SET（Supabase 的 `request.jwt.claims` 同样如此，其防线是通道隔离）。缓解（**已落地，2026-09-04 会话 #8**）：应用独占连接通道 + **HMAC 签名 `app.roles_sig`**（签名密钥 = `HMAC-SHA256(jwt.secret, "tw-roles-guc-v1")` 进程派生 + 启动钩子落 `tw_secrets`（REVOKE FROM PUBLIC，密钥不进 prosrc）；GUC 复合格式 `"<exp_unix>|<hexmac>"`，消息 = **`tenant|roles|exp`（R16 扩展：tenant 进签名**，见 §6 ③-b 状态）；`tw_roles()` 为 SECURITY DEFINER 验签函数，失败/缺失/过期 → 空数组 fail-closed）+ `tw_app` 非 owner + 全表 `FORCE ROW LEVEL SECURITY` + 池事务级 `SET LOCAL`。单密钥 + 滚动重启轮换（双钥窗口挂账转出 POC 前）。
- **DB 角色分层**：`tw_owner`（DDL/迁移专用，不跑业务查询）、`tw_app`（运行时，非 owner 无 BYPASSRLS）、`tw_system`（BYPASSRLS，内部调用）。

### 3.3 权限判定单源：SQL 函数 vs 掩码预计算缓存

- **裁决**：**两层叠加**。存储与查询层判定唯一源 = `tw_can` SQL 函数（应用查询构造器与 RLS policy 共用同一函数；配 SQL golden 测试矩阵锁语义，禁止 Go 侧重写等价逻辑）；realtime 扇出侧叠加短 TTL 掩码缓存作纯性能优化，不参与正确性。

## 4. 综合设计（五层）

### 4.1 契约层（DX）

- **查询（2026-09-04 单 AST 会话落地定稿）**：单 typed AST 为唯一 wire 形态（server/client 两面 `queries` 字段已 reserved，服务端零 DSL 消费——**范围限定文档查询栈**；users/storage 等静态表面遗留的 DSL 消费为 §0 边界邻居，另行收敛，已记录）。算子全集定稿：`eq ne lt lte gt gte in between isNull isNotNull contains startsWith endsWith search` + **not\* 变体族**（notEqual/notBetween/notContains/notStartsWith/notEndsWith/notSearch）+ `and or` 嵌套（深度 ≤8，MaxDepth 已收紧）——**通用 NOT 移出算子集**（2026-09-04 作者收敛：索引不可走、总可德摩根展开为 not\* 变体，跟随 Appwrite 实证）。`select[]` 投影已进 proto；`orderAsc/Desc` 服务端强制 `_id` tiebreaker（方向随首键）；`limit 1..200 默认 25`（0 = InvalidArgument）。
- **多键完整游标（C2 完成态，2026-09-04 落地）**：ORDER BY = 全部排序键各自方向 + `_id`（随首键）；keyset 谓词——方向统一时行比较 `(k1,…,kn,_id) op (…)`，**方向混合时逐键 OR 展开**（等值链 + 逐键严格比较，末项 `_id`）与全序严格一致；token 仍只编码 docID，服务端查行取全部键值。**NULL 排序键限制（已知，文档化）**：cursor 行含 NULL 键 → InvalidArgument；**数据行含 NULL 键在续页中被跳过**（谓词对 NULL 求值为 NULL，单键时代即同源行为）——NULL 密集列请先 isNull/isNotNull 过滤；NULLS LAST 谓词改写仅在需求出现时评估。
- **并发**：`$version` 兼作弱 ETag；Update/Delete 必须 `if_match`（HTTP）/`expected_version`（gRPC），缺省即拒、0 即参数错误。
- **幂等**：写统一 `request_id`（HTTP 头 `Idempotency-Key`），24h 去重，重放返回原响应 + `replayed=true`——Agent 重试安全的关键。**实施语义（2026-09-04 已落地）**：键作用域 `(project_id, actor_id, request_id)`（actor = 稳定归因身份）；指纹 = method+请求体规范序列化 hash，同 key 异体 → `IDEMPOTENCY.KEY_CONFLICT`；只缓存成功响应（失败 Release 释放重执行）；同 key in-flight 短轮询 ≤2s → `IDEMPOTENCY.IN_PROGRESS`（Aborted，retryable）；TTL 分离——done 24h / in_flight 兜底 5min（崩溃残留期间重试收 IN_PROGRESS 是保守正确）；重放标记走 `x-torchwood-replayed` 响应头（零 proto 响应侵入）；惰性清理不加 worker。
- **聚合**（2026-09-04 已落地，单 AST 会话完成类型化升级）：`documents:aggregate` 支持 sum/avg/min/max + 单键 group_by；**聚合一律在权限过滤后的可见行集上执行**（D1 规范的落地形态）；结果契约 **oneof 类型化**——integer 属性的 sum/min/max 返回 int64（`SUM(bigint)::int8` + 溢出拒绝 `AGGREGATE.OVERFLOW`），avg 恒 double，float 属性恒 double；**Data 维持 proto Struct double（2^53 精度界，文档化：int64 > 2^53 的业务值用 string 属性承载）**；排序/分页算子显式拒绝（R9/R9b）。
- **错误**：`{code, retryable, violations[], doc_url}`，域码稳定 snake_case（如 `DOCUMENT.VERSION_CONFLICT`）静态映射 gRPC code；infra 错误必须带 `error_id`，禁止裸 "document database error"。
- **Agent 面**：`GET …/collections/{c}?as=jsonschema` 导出 JSON Schema 2020-12；`GET /.well-known/torchwood` 资源/算子/错误码目录。
- **类型系统**：标量（string/int64/float64/bool/datetime/email/url/uuid/enum）+ `array<T>`（PG 原生数组）+ **`vector(dims)`（已落地：pgvector ≥0.8 原生列 + `hnsw` 索引 + `vector_search` 算子——SQL 形态定稿（R18 偏差①）：距离为 SELECT 平行列 + `ORDER BY col <op> $vec LIMIT k`（cosine `<=>`/L2 `<->`/inner `<#>`），**max_distance 为 top-k 后置过滤**（距离谓词进 WHERE 会使规划器放弃 HNSW——10 万行实证；top-k ⊇ 全部 ≤ 阈值行，语义等价）；iterative scan `strict_order` 经 SET LOCAL 事务内启用（GUC 默认 off）；读回 Data 契约 = JSON 数组（`::text::jsonb` 投影覆盖））** + `object`（jsonb + JSON Schema 子集校验，首期不建内部索引）；`relation` 远期。

```json
POST …/documents:query
{ "and":[{"in":{"attr":"status","values":["open","todo"]}},
         {"gte":{"attr":"priority","value":2}}],
  "orders":[{"attr":"$createdAt","desc":true}],
  "select":["$id","title"], "limit":25, "cursor":"b64(排序键+$id)" }
→ { "documents":[…], "next_cursor":"…" }   // next_cursor 省略=尾页
```

### 4.2 存储与隔离层（Typed 真实列 + 全局 catalog）

```sql
-- public：全局 catalog（已落地，迁移 000025；G1：catalog 为 cluster 内全局——多集群时随
-- project 所在 cluster 分布，project→cluster 路由表在控制面，迁移 = schema+catalog 行成套搬）
CREATE TABLE catalog_databases (project_id TEXT, database_id TEXT, name TEXT, 时间戳,
  PRIMARY KEY (project_id, database_id), UNIQUE (project_id, name));   -- FK→projects CASCADE
CREATE TABLE catalog_collections (
  project_id TEXT, database_id TEXT, collection_id TEXT,   -- 逻辑 ID（字符集维持 ^[a-zA-Z_]\w*$ ≤40，见下）
  physical_name TEXT NOT NULL,   -- c_<base32(8)> 服务端分配；sentinel 系统集合 = 静态表名
  attrs JSONB NOT NULL,          -- 全量契约（key/type/size/required/array/default/options，default 类型化保留标量）
  indexes JSONB, permissions JSONB, doc_security BOOL, disabled BOOL, is_system BOOL,
  schema_version BIGINT DEFAULT 1, ddl_seq BIGINT DEFAULT 1,   -- CAS 乐观锁（写路径递增，0 行→CATALOG.DDL_CONFLICT）
  PRIMARY KEY (project_id, database_id, collection_id), FK→catalog_databases CASCADE);
CREATE UNIQUE INDEX uq_..._physical_name ON catalog_collections (physical_name)
  WHERE database_id <> '_';   -- 部分唯一：分配名全局唯一；sentinel 物理名=静态表名（schema 内局部）跨项目同名，排除在外

-- 业务文档面保留 tw_<project>_<database>.<物理名>（每集合一张 typed 表）
CREATE TABLE tw_shop_app.c_ab12cd34 (
  _id TEXT NOT NULL, _tenant BIGINT NOT NULL,
  _created_at TIMESTAMPTZ NOT NULL DEFAULT now(), _updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  _created_by TEXT, _updated_by TEXT,
  _version BIGINT NOT NULL DEFAULT 1,
  _acl TEXT[] NOT NULL DEFAULT '{}',          -- 内嵌 ACE + GIN
  title VARCHAR(255) NOT NULL DEFAULT 'untitled',
  tags TEXT[],                                 -- array<T> 原生列
  embedding VECTOR(768),                       -- pgvector 可选
  PRIMARY KEY (_tenant, _id));
CREATE INDEX … ON tw_shop_app.c_ab12cd34 USING gin (_acl);
ALTER TABLE tw_shop_app.c_ab12cd34 ENABLE ROW LEVEL SECURITY;
ALTER TABLE tw_shop_app.c_ab12cd34 FORCE ROW LEVEL SECURITY;
-- policy 四条（模板见 4.3）；列级 GRANT 只授数据列（_tenant 锁死；_acl 经 tw_set_document_acl 唯一通道；_version 不锁列——CAS 已足，A6 勘误）
```

- **标识符治理（2026-09-04 落地形态）**：逻辑 ID 与物理名解耦——**collectionID 字符集维持 `^[a-zA-Z_][a-zA-Z0-9_]*$` ≤40 不放宽**（原草图的 `[a-z0-9-]` ≤36 挂账待需求信号：snake_case 与属性键习惯一致，且避免 `_perms`/realtime 频道约定动荡）、属性 key ≤63、索引 ID ≤40；物理名 `c_<base32(8)>` 服务端分配（碰撞重试），截断/碰撞类缺陷机制性不可达；`_perms._collection` 与 realtime 频道保持逻辑 ID；**物理名不出现在任何 API 响应**（内部实现细节）。
- **隔离**：应用谓词由单一查询构建器强制生成（`_tenant = ?`）+ 跨租户探针集成测试常驻；RLS policy 为判定执行点（见 4.3）；schema-per-project 布局保留（`tw_<p>_<db>`），配量化预警线与多集群分片出口（§3.1）。
- **DDL**：加列 `ADD COLUMN ... DEFAULT`（PG11+ 元数据级）；索引一律 CONCURRENTLY + 独立事务 + `lock_timeout=2s` 重试 + catalog 两阶段状态机（building→active）+ reconcile 对账。

### 4.3 权限层（单源函数 + RLS 判定执行点 + 列级 GRANT + 角色分层）

- **存储**：`_acl text[]` 内嵌 + GIN；空数组回退集合级（B1 语义）；集合级权限与 doc_security 存 catalog（policy 经子查询**实时读取**——集合级权限变更即时生效，无需改 policy DDL）。
- **判定唯一源** = `tw_can` SQL 函数（示意）：

```sql
CREATE FUNCTION tw_can(acl text[], roles text[], typ text, coll_allows bool)
RETURNS bool LANGUAGE sql STABLE AS $$
  SELECT acl && (SELECT array_agg(typ || ':' || r) FROM unnest(roles) r)
      OR (NOT EXISTS (SELECT 1 FROM unnest(acl) a WHERE a LIKE typ || ':%')
          AND coll_allows) $$;
```

应用查询**不再手工拼权限 WHERE**——RLS policy 的 securityQuals 隐式过滤即判定（列表/count/单条/bulk 全部同源）；`tw_can`/`tw_visible` 的宿主是 policy 与测试基准：SQL golden 矩阵（角色 × ACE 型 × 空 ACE 回退 × update-only 可见性）随 CI 锁语义，lint 禁止 Go 侧等价实现。读路径 0 行 → `NOT_FOUND`；写路径 42501 policy 违例 → `PERMISSION_DENIED`（§3.2）。

- **RLS 判定 policy 模板**（每集合表生成一次；SELECT = `tw_visible` 可见谓词，见 §3.2）：

```sql
CREATE POLICY p_read ON tw_shop_app.c_ab12cd34 FOR SELECT
USING ( tw_can(_acl, (SELECT tw_ctx.roles()), 'read',  (SELECT tw_ctx.coll_allows('c_ab12cd34','read')))
     OR tw_can(_acl, (SELECT tw_ctx.roles()), 'update', ...)
     OR tw_can(_acl, (SELECT tw_ctx.roles()), 'delete', ...) );   -- tw_visible：可写即可读（产品语义，应用层同谓词）
CREATE POLICY p_create ON ... FOR INSERT WITH CHECK (集合级 create);
CREATE POLICY p_update ON ... FOR UPDATE USING (tw_can update) WITH CHECK (同);
CREATE POLICY p_delete ON ... FOR DELETE USING (tw_can delete);
-- 所有跨表取值 (SELECT ...) InitPlan 化；身份函数 STABLE；tw_ctx.roles() 可校验 roles_sig HMAC
```

- **角色分层**：`tw_owner`（DDL）/ `tw_app`（运行时，列级 GRANT 只授数据列）/ `tw_system`（BYPASSRLS，系统旁路）。
- **授予治理**（`ValidateGrantablePermissions` 语义）与应用层 404/403 策略保留在用例层。
- **realtime 扇出**：预计算"频道×角色集→放行"缓存（短 TTL），ACL 评估 O(1)，不参与正确性。

### 4.4 正确性与一致性层

- **OCC**：CAS 进 WHERE 的单语句三态（见 C4）；bulk/upsert 盲写 +1 显式化为 LWW 契约；Upsert 保留 advisory_xact_lock + 锁内 `FOR UPDATE` 分支，判定换用 `tw_can`。
- **原子性**：ACE 与数据同表同行（`_perms` 跨表对账删除）；catalog 行与 DDL 同事务（CONCURRENTLY 除外，走两阶段状态机）；写响应一律读回（见 C8）。
- **漂移防护**：`ddl_seq` 乐观锁防并发 schema 变更；后台 reconcile 对账 catalog ↔ pg_catalog（缺列/INVALID 索引/幽灵表）自动修复 + 告警；`torchwood admin schema repair` CLI。
- **参数上限**：filter 绑定参数**跨 filter 累计**上限（封死 65535 风险）。

### 4.5 事件层

- 写事务内 outbox INSERT（全局 `seq` identity）+ `pg_notify` 唤醒（替代 200ms 轮询，5s 兜底轮询；NOTIFY 仅作信号——无持久化、每消费者占一条连接，不承担投递）。**落地形态定稿（R14，2026-09-04）**：NOTIFY 走 **AFTER INSERT 行级触发器**而非应用侧逐条 `SELECT pg_notify`——零额外客户端语句（应用侧会使 Bulk 语句数翻倍，违反 R5-P2-6 预算），PG 自动合并同事务相同 NOTIFY（execute-tx 100 op 批一次唤醒），随 commit 投递/回滚丢弃语义不变；实测 commit→LISTEN 平均 4.8ms。pgdriver 自带 Listener（专属连接 + 断线重连 + Channel API），无需退化短轮询。
- 投递：dispatcher 批拉（SKIP LOCKED，批 256）→ Redis **Stream** `torchwood:events`（XADD 不带 MAXLEN + worker 周期 XTRIM 10 万；**每 server 实例一消费组**，组名 hostname:pid——同机多进程不共组，重启即新组从 `$` 起步不回放，恢复靠客户端 last_seq 重放）→ XREADGROUP → hub 扇出 → XACK；XAUTOCLAIM 回收 idle>15min PEL；`published_at` 批量回写保留（XACK 后幂等 UPDATE，清理与 `:changes` 数据源统一为 outbox 表）。重放窗口在 outbox 表（published 行 24h 清理覆盖 1h 承诺）。回到 Stream 是有意决策：当年改 Pub/Sub 为解决多副本可见性，消费组模式同样解决且换来不丢帧与位点回放。
- 顺序承诺（B1，2026-09-03 定稿）：**单文档全序**（行锁保证 seq 随提交序）；**集合内为分配序**——跨文档不保证与提交序一致，且 seq 有空洞（回滚事务消耗 seq 但无事件，不表示丢事件）；seq 仅作续传游标与去重 ID；不承诺跨集合因果。已写入 API/SDK 契约注释。execute-tx 批事件共带 `transaction_id`（Agent 端原子批分组）。
- 补偿：断线重连带 `last_seq`（subscribe 帧门控重放：补发先于实时、确认帧经 Send 通道防双写乱序）；窗口外返回 `EVENTS.RESUME_EXPIRED` 并指引全量重拉；`GET :changes?since_seq=` 双面（Server+Client），可见性过滤与 hub 扇出同语义。**载荷上限修订（B4 撤回）**：事件上限对齐 H1 = **1MiB**（写入面已拒超限，正常路径无截断；超限仅防御性截断 + truncated 标志）——原 `data_ref = GET ?version=N` 撤回（按版本拉取无地基，文档表只存最新行，挂账远期版本化读取）。
- 慢消费者：**RESYNC 帧撤回，改为带因断开**——send buffer 64→1024，满水位 OnSlow 恰一次 → writeLoop 以 `resync:<last_seq>` close 断开，客户端零退避重连带 last_seq 即天然重同步（断开即重放，语义等价、协议更简）；lastSeq 游标仅在成功入队后上抬（客户端从该 seq 续传不漏未投递帧）；close reason 为 best-effort（正确性不依赖——SDK 自行跟踪 seq）。

### 4.6 Schema 演进契约

| 操作 | API 契约 | 物理 |
|---|---|---|
| 加列 nullable/带 default | 即时生效，响应读回新 schema | `ADD COLUMN DEFAULT`（元数据级） |
| 加列 required | 必须带 default，否则走迁移 | backfill 后 SET NOT NULL |
| 放宽（required→optional、扩宽） | 即时 | ALTER |
| 收紧/改类型 | 创建迁移（validate→rewrite→commit），期间 `schema_status=migrating`，写按目标 schema 校验 | 异步任务限速回填 |
| 删列 | 两段：`deprecated`（读屏蔽写拒收，可回滚）→ `retired`（归档后物理删，不可逆） | 软删列 |
| 重命名 | 不提供（= 新列 + copy 迁移 + 旧列 deprecated） | — |

### 4.7 运维

- `torchwood export --project` / 导入：COPY 流式 NDJSON + catalog 快照；schema-per-project 形态下 `pg_dump -n tw_<project>` 即项目级备份（现模型红利，写成 runbook）。
- 规模预警线：`pg_class` 计数、pg_dump 时长、迁移耗时纳入 SLO 指标；超限（数百→低千 schema）触发多集群分片规划（project → cluster 路由抽象）。
- 配额：全局 catalog 维护 per-project 行数/字节（增量 + 周期精确化），写入前置令牌桶。
- 可观测：全局视图表（项目×集合×行数/字节/索引数/末次写入）、outbox lag / 扇出延迟指标、auto_explain 按 project 打标。

### 4.8 事务内核与多文档原子性（2026-09-03 接受，细化见 §11-E1）

- **内核 = op 模型 + 单事务执行器**：可序列化异构 op 列表（复用旧 `document_transaction_ops` 字段族：type/database/collection/document_id/data/permissions/increment/expected_version/conflict_columns）在一个 `RunInTx` 内顺序执行——RLS/GUC 一次注入、逐 op 判定（提交时权限）、按 `(_tenant,_id)` 排序加锁防批内死锁、OCC 逐 op、outbox 事件同事务（可共带 transaction_id）、all-or-nothing、失败返回带 op index 的 violations。**实现注意（2026-09-03 评审登记）**：排序加锁是执行器的目标态纪律，现状 Bulk 并不具备——`BulkUpdateDocuments` 无行锁、`BulkDeleteDocuments` 按输入顺序 `FOR UPDATE`；"Bulk 的泛化"指复用其单事务/事件/outbox 骨架，锁纪律须按本节新建，不得照抄现状 Bulk。
- **三种消费形态**（按消费者执行位置选）：A `documents:execute-tx`（远程客户端一次性原子 op 批，Bulk 的泛化，无暂存表）；B Functions 事务上下文（服务端真事务，`InTx` 管道 + GUC 注入 + 生命周期，**不走 staged**——命令式代码无法 replay）；C staged session（跨请求暂存，复用旧 D-6 表设计与教训，等 A 的需求证据再启用）。
- **Phase 1 实施裁决（2026-09-03 方案作者复审实施报告后）**：① op 模型收敛为**请求级单 database**（database 为 RPC 路径参数而非 per-op 字段——与旧 D-6"事务级单库"一致，跨库批无需求证据，上文字段族中的 per-op database 撤回）；② `expected_version=0` 一律 **InvalidArgument**（新代码不得继承旧错位；单文档 `UpdateDocumentVersionRequired` 同步拆分 nil→FailedPrecondition(version_required) vs ≤0→InvalidArgument——这正是 C4"消灭错误码错位"的本意）；③ grant 校验**严格 per-op**：种子 op 仅豁免自身，不得提升同批其他 op 的授予校验（批级提升是越权面）；④ upsert 不参与预排序锁（冲突目标预查前未知），死锁窗口由 PG 死锁检测（中止一方）+ request_id 幂等重试兜底——接受，可选改进（预锁冲突值键）挂账。
- **分期**：Phase 1 = A（已落地）；Phase 2 = B **定稿为形态乙（2026-09-04 会话 #8 探查）**——函数运行于外部 Docker 容器（node/python 镜像，env 进 stdout 出），无共享 ctx/无回环通道，"函数即事务"在该执行模型下不成立；多写原子性由 Phase 1 的 `execute-tx` RPC 完整承载（函数经 API/SDK 调用），文档与 SDK 注释已落；Phase 3 = C 视需求。Agent 联动：op[] 即工具参数（结构化、整体幂等、可 dry_run）；批内事件顺序 = op 顺序（B1 的分配序问题在批内不存在）。

## 5. 与当前架构对照

- **保留（两轮评审验证为正确的设计）**：**每集合一张 typed 真实表**（维护者决策定案）；`tw_<p>_<db>` schema-per-database 布局与删除原子性（DROP SCHEMA CASCADE）；事务性 outbox 模式；OCC fail-closed 意识与 `_version` 概念；权限 SQL 下推思想（升级为单源 `tw_can`）；标识符白名单 + 全程参数绑定的注入防御；`DocumentDB` 三端口分层；角色认证期实时解析注入（不信任 JWT claims）；Upsert advisory lock；系统静态表与文档路径解耦的现状布局；Appwrite 式 `type:role` 权限模型（**D3 语义被有意取代：可 update/delete 即可读**，2026-09-03 决策，见 §3.2）。
- **替换**：每项目 catalog 四表 → 全局 catalog（attrs JSONB 全量契约含 default）；`_perms` 独立表 → `_acl` 内嵌；双判定实现 → 唯一 `tw_can` 函数（应用构造器与 RLS policy 共用）；offset+keyset 双 token → keyset-only；DSL/AST 双栈 → 单 AST（DSL 为 SDK 糖）；check-then-write OCC → WHERE 内 CAS；事务内 DDL → CONCURRENTLY + 状态机；Pub/Sub 扇出 → Stream 位点 + seq + 补偿；表/索引名=逻辑 ID → 物理名服务端分配；应用层唯一判定 → RLS policy 判定执行点（`tw_visible` 单源）+ 列级 GRANT + DB 角色分层。
- **新增**：`request_id` 幂等、`if_match`、机器可读错误码体系、JSON Schema 导出、`:changes` 补偿、schema_version + 迁移 RPC、`:aggregate`（首期 count）、`array<T>`/`vector` 列、流式导出/导入、配额下推、schema repair CLI、DB 角色分层（tw_owner/tw_app/tw_system）、**事务内核与三形态消费**（§4.8：`execute-tx` / Functions 事务上下文 / 按需 staged session）。

## 6. 演进路径（四阶段，每阶段可独立发布）

| 阶段 | 内容 | 物理层变动 |
|---|---|---|
| ① 契约收敛 | 单 AST（DSL 降级为糖）、keyset 统一 + tiebreaker、错误码体系、default 落 catalog、写响应读回、幂等 `request_id`、**`documents:execute-tx`（事务内核 Phase 1，Bulk 泛化）** | 无 |

**阶段①完成状态（2026-09-04 会话 #4 复审后整体完成，R11 亦已落地）**：**含单 AST 全部落地**——keyset-only（ListDocuments 拒 offset()/offset 族 token、首页满页发 ka: token）、多键完整游标与 NULL 限制（§4.1）、错误契约（BadRequest violations + error_id 全路径 + 域码命名空间按子系统扩展）、幂等与聚合（语义见 §4.1，聚合 oneof 类型化）、**单 AST**（queries 双栈 reserved 退役、算子全集含 not\* 变体族、select 进 proto、SDK typed builder + FromDSL 糖、R9b 分页字段归一）。**R11 已落地（2026-09-04，commit 758db77）**：`pkg/query/testdata/dsl_ast_golden.json` 共享 golden 语料（47 条含 10 错误条目）以中立 JSON 形态锁定两侧 DSL 文法，`root`/`sdk` override 块表达 offset 的设计内单侧差异；语料即仲裁——上线即暴露并修复 SDK 侧两处缺口（MaxQueries/MaxQueryLen 输入上限、limit 错误文案拆分），未发现语义级分歧。剩余记录项：users/storage 等静态表面遗留的 DSL 消费为 §0 边界邻居，归阶段②收敛。
| ② catalog 全局化 + 标识符治理 | 全局 catalog 上线（含 default/数组契约）；collectionID/属性 key/索引 ID 长度上限；新集合物理名服务端分配 | 中 |

**阶段②完成状态（2026-09-04 会话 #5 复审并经 #5-R 返工后收口）**：**整体落地**——public 全局 catalog 两表（JSONB 合一，GetCollection 3 查询→1；default 类型化往返闭环，包 0-4 的旧记录项就此消解）；物理名解耦（`c_<base32(8)>` 分配、DDL/行查询全物理名、`_perms`/频道保持逻辑 ID、物理名零 API 泄漏；索引名组合长度对物理名自然满足）；ddl_seq CAS 五写路径 + `CATALOG.DDL_CONFLICT`（**Aborted + retryable**，R12a 裁决落地：CAS 冲突非参数错误，对齐 `IDEMPOTENCY.IN_PROGRESS` 先例）；projectschema 四表退役（000001 no-op + 000011 DROP）；静态表面显式化（storage + **groups**（R12b）queries 拒绝、users 面独立 DSL 契约注释）。复审：9 判断点 8 接受 + 1 纠正（R12 两小项已落地：`cacbe87`/`60dfe41`）。挂账：物理名进程内缓存评估（业务库热路径 +1 主键点查）；collectionID 字符集放宽待需求。
| ③ 权限内嵌 + RLS 判定 | **③-a（会话 #6 已落地）**：`_acl` 内嵌直切（无双读）、`tw_can/tw_coll_allows/tw_visible/tw_roles` 单源函数（000027）、RLS policy 四条 + FORCE + 列级 GRANT + DB 角色分层（000026）、GUC/每请求一事务（A1）、错误映射定稿（§3.2 勘误块）；**③-b（后续）**：array 列与算子、Functions 事务上下文（内核 Phase 2）、roles_sig HMAC、`_acl` 变更通道唯一化与 SECURITY DEFINER ACL 函数（原"`_acl/_version` 完整列级锁死"经 A6 表述修正收敛：`_acl` 经函数唯一通道、`_version` 不锁列——CAS 已足，见下总览） | 中 |

**阶段③-a 完成状态（2026-09-04 会话 #6 复审，d2f2713..c5d7551）**：**权限内核换轴整体落地**——`_perms` 退役、`_acl TEXT[]` 内嵌 + GIN（权限随 `to_jsonb(d.*)` 免费回填，B6 批量 IN 查询删除）；RLS 为判定执行点（应用层 `listPermissionFilter`/`checkDocumentPermission` 双实现退役；三处 PG 18 实证勘误见 §3.2 落地勘误块：`_acl` 第二语句/自锁保留、upsert 拆 ON CONFLICT 分支裁决、delete 单语句 CAS）；连接模型 = **单一变色龙身份 + `SET LOCAL ROLE` 三选一 + `set_config('app.roles')` 合并注入**（A1 原型结论：BYPASSRLS 经 SET ROLE 按 current_user 生效，无需独立 system DSN；loopback 每请求一事务合并注入 1.37ms vs 未合并 3.33ms vs autocommit 290µs，合并已采纳）；列级 GRANT 务实版（仅 `_tenant` 锁死；**`_acl` 经 R13a 从 tw_app 的 UPDATE 列授权移除，变更通道收敛为单一 choke point（tw_system 第二语句，列权限 + 代码路径双侧强制）**；INSERT 保留 `_acl`——create 单语句必需且无新行复检面）；A1 范围 = 文档面（静态 bun 表面维持 authenticator 本身份，无 RLS 无注入）；门禁（EXPLAIN InitPlan 形态断言 + 10 万行 RLS 开/关相对基准 4.9x < 30x 阈值）；golden 矩阵函数级+行为级双层。**R13a 已落地（`e85b897`）——阶段③-a 正式收口。**挂账：③-b 全部内容；测试 DSN superuser 豁免面（生产配非 superuser 应用账号，已入 06-databases 不变量 #14 + runbook）；**转出 POC 检查项：启动/迁移路径加一次全量列授权 reconcile 扫描**（存量表旧授权形态现依赖 DDL touch 矫正，测试库每次重建无此面，真实存量环境需要一次性扫齐）。
| ④ 事件 Stream 化 | outbox seq + NOTIFY 触发器唤醒 + Redis Stream 消费组位点 + last_seq 重放/`:changes` 补偿 + 水位带因断开 | 中 |

**阶段④完成状态（2026-09-04 会话 #7 复审并经 #7-R 返工后收口）**：**事件链换轴整体落地**——seq identity + UNIQUE（B1 语义入契约注释）、NOTIFY AFTER INSERT 触发器（R14：零语句数 + 同事务自动合并 + 4.8ms）、Redis Stream 消费组位点（hostname:pid 组名、$ 起步、XAUTOCLAIM、XTRIM 10 万）、`:changes` 双面补偿 + WS last_seq 门控重放（补发先于实时、确认帧经 Send 防乱序）、水位 1024 带因断开（resync:\<last_seq\> close）、transaction_id 批标识、载荷上限对齐 H1 1MiB（B4 data_ref 撤回挂账）。判断点 ①-⑦ 接受（孤儿消费组 XGROUP DESTROY 治理挂账；claimMinIdle 15min 超服务端去重窗的重复帧由客户端幂等吸收，at-least-once 契约内）；**R15 已落地（`3be9482`）**——`:changes` 两级扫描游标（收满退出=末条返回 seq / 触顶退出=扫描位置越过不可见块 + has_more=true，自然耗尽=0），proto `next_since_seq` + SDK pager/WS 确认帧跟随（"优先游标、0 回退末条事件 seq"两级契约），不可见块后不再静默漏失（600 连续不可见块跨页无重无漏用例锁定）——**阶段④正式收口**。

**演进路径总览（2026-09-05 全量闭环）**：**重设计全部落地并收口**——① 契约收敛（含单 AST）✅、② catalog 全局化 + 物理名 ✅、③-a 权限内核 ✅、④ 事件链 ✅、**③-b ✅（会话 #8 + R16/R17 返工收口）**。③-b 落地内容：array<T> 原生列 + containsAny/All + 写侧四算子；Functions 事务上下文**形态乙定稿**（外部容器模型，原子性经 execute-tx，§4.8）；roles_sig HMAC 验签（pgcrypto + tw_secrets，fail-closed 三态）；`_acl` 变更通道唯一化 `tw_set_document_acl`。**R16 已落地（`910d661`）**——收紧四件套：sig 消息 `tenant|roles|exp`（`app.tenant` 同签 GUC，`tw_tenant()` 验签函数）+ 函数内两道强制（`p_tenant = 验签 tenant` 死锁跨租户/跨项目、`tw_visible` 可见性门堵项目内提权）+ create 恢复 INSERT(`_acl`) 通道（新行无旧行，撤回"INSERT 也移除"）+ definer 调用链授权（tw_system 显式 EXECUTE）。**通道终态**：tw_app → 函数（tenant 绑定 + 可见性门）；tw_system → 信任根直写（正确性依据：函数可见性门以调用者验签 roles 运行，而 tw_system 按 A2 不注入签名 roles——授 EXECUTE 会被自己的门掐死，直写与 ③-b 前第二语句语义严格等价）；create → INSERT 随行携带。**R17 已落地（`ee0d9ea`）**：TS SDK 双面补登 ListChanges（合同测试红转绿，26/26）。**A6 表述修正**：`_acl` 锁死（经函数唯一通道）；`_version` 不锁列（CAS 守卫已足）。000029 为原地修订（POC 未发布同一语义单元，本地库重建）。剩余挂账：vector 列与 KNN 算子（§10.5 P0 首项，专项会话）、export/snapshot_seq、孤儿消费组治理、并行测试基建（SetupTestDB 争用 + **迁移循环集群级角色竞态**：DROP ROLE 撞并行库中 tw_owner 对象，串行绿）、双钥轮换窗口、转出 POC 检查项两条（全量 reconcile 扫描、非 superuser 应用 DSN）。**数组算子补全（Intersect/Diff/Insert/Filter、TransactionOp 数组）已于 2026-09-05 转出 POC B1 落地销账**（查询侧裁决不补——Appwrite 实证四算子均为写侧，见 §10.5 P0 行）。

**B 区全清（2026-09-06，转出 POC 门禁 A 区 10 条闭环 + B 区 13 条全清，活跃清单见 `docs/developer/15-exit-poc.md`）**：第一波 B6（孤儿消费组三条件销毁）/B8（encodeTokenData 兜底改返回 error，21 调用点透传）/B9（`_version` 口径统一——接受不锁列，redesign 四处旧口径勘误）/B11（H2 上限族 enforcement：`_acl`≤64、数组≤1000、列数≤200、object≤8 层，域码齐备）/B12（规模预警线三指标 + 13-operations 阈值告警）；第二波 B1（写侧 INTERSECT/DIFF/INSERT/FILTER + execute-tx op array_updates——**PG 18.6 实证无 `array_insert` 内建**，unnest WITH ORDINALITY 实现；查询侧不补）/B2（多页 KNN：`kvc:` 距离游标 + 首页完整距离组切页 + 续页 `(dist,_id)` 精确全序——朴素阈值形态实测 tie 组永久丢行；内积 `<#>` 负值三 metric 统一升序）/B5（export/import NDJSON 流式 + REPEATABLE READ 快照 snapshot_seq 闭合 + 行保真恢复走现役汇聚点）/B10（JSON Schema 导出 + well-known 四重防漂移 + OCC current_version）；第三波 **B3（在线索引两阶段状态机：catalog building→active|failed + 事务外 CONCURRENTLY + lock_timeout 重试 + 崩溃残留可重入；reconcile 三类漂移 + `torchwood admin schema repair` CLI；DDL touch 语义收窄——存量表索引改走 CONCURRENTLY 通道）**/**B4（attrs status 三态 deprecated→retired + 改类型 copy 迁移任务（批 500 限速可恢复、锁窗全量追平重算、原子 swap + schema_version 递增；迁移期写拒收读放行——§4.6 契约修订））**/B7（ef_search 查询级暴露 [1,500] 显式拒绝，缺省零注入逐字节一致）/B13（a 死锁注入 8/8→预锁后 24 轮 0 死锁、c 点查 867µs→缓存 509ns、b/d 压测/登记维持）。**§10.5 全部 P0 清零；B 区 13 条全清**。剩余：C 区 7 条产品决策（15-exit-poc C 区，待维护者拍板）、转出 POC 检查单执行复核（A 区闭环证据已在档）。

**转出门禁与挂账的活跃清单见 `docs/developer/15-exit-poc.md`（2026-09-05 成文，会话 #11 全量盘点收拢）；本文各节挂账清单为同日快照，此后新增与闭环只更新该文件。**

**清扫会话 #9（2026-09-05，1ee21ae/da38153/7ab7cef）**：死代码与过渡代码清扫，净删 −1684 行——pkg/crud 脚手架（cursor token 全家/玩具 checksum/未用构造器）、DecodePageToken 未签名旧格式回退、projectschema cut 迁移机器（copy.go 456 行 + staging 探测；**sentinel 分支的 EnsureCatalog 调用联动删除**——testutil fixture 停 000008 保 sys_* staging 名，保留该调用会令 000009 rename 与播种表撞名；生产不可达，仅测试面依赖）、**`ListRequest.filter/order_by` reserved 退役（W-K 终结：字段存在唯一意义是被拒的过渡态清零）**、Seed 更名去 Legacy。验证期顺带修复两笔存量：outbox 信封 ACL 截断 O(n²)（events 6min→12s）、并发 upsert 竞态断言对齐（③-a 分支化后 attacker 路径产出 ErrDuplicateKey 而非 ErrPermissionDenied——安全不变量不变，姊妹用例本已双认）。记录项：encodeTokenData 的 marshal 失败兜底仍产出不可解码的 v1:offset 形态（不可达路径，语义自不一致但无害，随下次触碰 crud 时顺手清理）。

**vector 专项会话 #10（2026-09-05，201ed36..61ac141）——§10.5 P0 全清**：三个原型结论——① pgvector 0.8.6-pg18 镜像（compose/CI 同步；**vector 非 trusted extension，安装需 superuser——runbook 化并入转出 POC 检查单**；镜像基座 glibc vs musl 的 collation 翻转连带修复：initdb 锁 `locale=C` 字节序，产品期语言学排序另行决策）；② **iterative scan 为契约被实证**（iterative OFF = 先取全局 top-k 再滤（1/5 行），ON（strict_order）= k 个可见近邻全召回且与 ground truth 同序同距）；③ to_jsonb(vector) 输出字符串，投影 `::text::jsonb` 覆盖保证 Data=JSON 数组。复审裁决六项全接受：偏差①（max_distance 后置过滤，等价性证明核验成立）、HNSW 近重复簇召回边界（近似索引固有，文档化 + ef_search 挂账）、**偏差②（CONCURRENTLY 通道不存在——prompt 引用了 C5 目标态当现状，C5 的在线 DDL 机器从未实施，现显式入挂账）**、locale=C、非 trusted runbook、CreateAttribute 列级 GRANT 滞后的存量修复（refreshColumnGrants，影响全部属性类型）。**§10.5 P0 至此全清**（向量/数组/`_created_by` 三项均落地）。

POC 阶段各阶段**直接切换、不留兼容回退**：阶段③的 `_perms → _acl` 无需双读灰度（直接重建），阶段②无需存量四表迁移任务；"每阶段附回退方案"的要求在转出 POC 时再引入。阶段①②可先行单独收割正确性收益（当前评审的 P1/P2 大多在①②③消除）。

**实施前评审登记（2026-09-03，对照代码全量核验后）**：包 0 修复清单的全部代码锚点经逐项核实成立。两点实施补充：① 标识符静态上限（POC 期 collectionID ≤40 / attr key ≤63 / 索引 ID ≤40）**不能单独封死索引名截断**——`idx_<coll>_<id>` 拼接最长 85 字节仍超 PG 63，须叠加组合校验 `4 + len(collID) + 1 + len(idxID) ≤ 63`（阶段②逻辑/物理名解耦后该组合约束随物理名分配自然消失）；② 域错误码一律点分格式 `NAMESPACE.SNAKE_CODE`（如 `DOCUMENT.TOO_LARGE`），单下划线写法为笔误。

## 7. 风险与缓解

| 风险 | 缓解 |
|---|---|
| schema-per-project 的 relation 数量劣化（pg_dump/relcache/autovacuum/迁移重放） | 量化预警线（pg_class 计数、pg_dump 时长、迁移耗时 SLO 化）+ 集合数配额 + 超限走多集群分片（不改存储形态）；全局 catalog 已削掉最大常数项 |
| RLS policy 性能劣化（SubPlan 逐行） | InitPlan 化纪律 + STABLE 函数 + GIN 索引 + 上线前 EXPLAIN 门禁 + 基准 CI（实测差距可达 100 倍量级，见 §9 引证） |
| 语义变更迁移面（"可写即可读"取代 D3；upsert 收紧为 create ∪ update 双检查） | 变更写入 API 文档/changelog 与 SDK 说明；存量 update-only ACE 自然获得可见性（良性放宽，无数据迁移）；upsert 双权限要求在契约层显式声明；SQL golden 矩阵覆盖 update-only 可见与 upsert 越权两个用例 |
| policy 生命周期漂移（collection 增删需同步 policy DDL） | policy 由 catalog 派生统一生成（RLS-as-code）+ CI 漂移检查；集合级权限存 catalog 被 policy 实时读取，权限变更不需要 DDL |
| GUC 伪造（`app.*` 可被任意会话 SET） | 应用独占通道 + 可选 HMAC `roles_sig` 验签 + `tw_app` 非 owner + FORCE RLS + 威胁模型明示直连通道不信任 `app.*` |

## 8. 附录：评审痛点 → 设计决策映射

| 评审痛点（§1） | 消解它的决策 |
|---|---|
| 1 标识符截断 | 4.2 标识符治理（逻辑/物理名解耦） |
| 2 权限双实现 + TOCTOU | C3 + 3.2/4.3（`tw_can`/`tw_visible` 单源，policy 即判定） |
| 3 `_perms` EXISTS 热点 | C3（内嵌 + GIN） |
| 4 OCC 语义混乱 / 种子锁死 | C4（三态 CAS）+ 4.4（upsert 同谓词） |
| 5 分页丢行/双 token | C2（keyset-only + 强制 tiebreaker + cursor 排序校验） |
| 6 契约断裂/静默忽略 | C8 + 4.1（显式拒绝 + 读回 + 全量 catalog 契约） |
| 7 DDL 锁 | C5（CONCURRENTLY + 状态机） |
| 8 事件丢帧/无序 | C6（seq + Stream 位点 + RESYNC/`:changes`） |
| 9 schema 扇出 | 3.1（全局 catalog + 量化预警线 + 多集群分片出口） |
| 10 双栈漂移 | C7（单 AST） |

## 9. 技术路线定位与 PG 候选（2026-09 竞品调研结论）

### 9.1 路线定位：不是"走向 Supabase"，是第三条路

**"Appwrite 的产品契约 × Supabase 的执行面 × 用户永不手写 SQL/RLS 的编译层"**。两条路线不是二元对立（外部证据）：

- **Appwrite 2.0（2026-09-02）自己拆成三轨**：TablesDB（typed 列，即 torchwood 现形态）+ DocumentsDB（schemaless）+ 原生裸库（直卖 PG/MySQL）——头部玩家公开承认单一存储路线覆盖不了所有负载。
- **Supabase 的 Realtime（WALRUS）走了反方向**：最初应用层逐订阅者判定，变更数×订阅者数不可控，最终把安全判定**移回数据库内**（回库按主键逐订阅者校验，1k 订阅者 64.7ms、10k 303.8ms 近似线性）——"授权下推到 DB"是规模化平台的共同归宿，不是 Supabase 专利。
- **正面先例**：EdgeDB/Gel（PG 之上的图-关系语言 + access policy 在内核执行 + globals 承载会话上下文）；PocketBase（规则 DSL 编译为 SQLite 表达式随查询执行——与本设计 `type:role` → RLS policy 编译同构的微缩版）。
- **反例**：FerretDB（Mongo 协议 shim + PG 后端）证明"通用协议翻译层"此路不通；Hasura/Nhost 没有文档层产品——"文档 API + PG 内核 + DB 层授权"这格**没有大厂占位**。
- **torchwood 与 Supabase 的本质差异保持不变**：集合/DSL 是唯一对外契约（不暴露表、不暴露 SQL）；schema/索引演进由平台 API 承接（用户零迁移——Supabase 用户必须自己管 migrations）；RLS policy 由平台的 role 模型编译生成，用户永不手写。守住这三条红线，RLS 下推是"Appwrite 路线的规模化补完"而非路线切换（红线之二"不暴露 SQL"的复核论证见 §10.4）。

### 9.2 PG 候选方案采纳表

| 候选 | 结论 | 要点 |
|---|---|---|
| RLS per-command policy | **采用（判定执行点）** | 语义映射见 §3.2（唯一冲突已由"可写即可读"决策消解）；性能纪律：InitPlan 化 + STABLE + GIN + EXPLAIN 门禁（实测裸 policy 100 万行 1840ms → 0.21ms） |
| `SET LOCAL` GUC 身份注入 | **采用** | PostgREST/Supabase 同款管道（每请求一事务 + `set_config(..., true)` fail-closed）；直连通道永不信任 `app.*` |
| 列级 GRANT | **采用（终态口径勘误，B9，2026-09-05）** | `_tenant` 列锁死；`_acl` 经 `tw_set_document_acl` 唯一通道（R13a/R16，INSERT/UPDATE 列授权双向排除）；`_version` **不锁列**——`UPDATE … WHERE _version = ?` 的 CAS 守卫已保证并发安全，写错版本只令语句自身失败（自伤不伤人），列级锁死边际收益为零（裁决见 §6 ③-b A6 表述修正）。仍必须从一开始只按列授予（表级+列级是并集，无法事后挖洞） |
| pgvector | **采用（尽快）** | typed 列模型的独占加分项：向量检索与文档同事务、同 RLS/GRANT 管辖，直接强化 AI/Agent-Native |
| PostgREST / Hasura（整体） | **不采用** | 双真相源 + 把 schema 管理权让渡给反射层；抄走其鉴权管道与"单请求编译为单 SQL + prepared statement" |
| schema-per-project（现状布局） | **维持 + 量化预警线** | 社区阈值：几百 schema 舒适、1–2 千起劣化（pg_dump 24h+、relcache、autovacuum XID 风险）；超限走多集群分片而非改共享表 |
| 共享表 + tenant_id + RLS | **不采用** | Citus 官方推荐路线但与 typed 表冲突；记为备选 |
| Citus / 原生分区 per-tenant | **不采用 / 观察** | LIST-per-tenant 几千分区即 planner 舒适区上限；Citus 仅在超大集群时评估 |
| LISTEN/NOTIFY | **辅助采用** | 仅作 outbox 唤醒信号（无持久化、每消费者占连接、payload<8KB） |
| Logical decoding | **观察** | 流上无会话上下文（RLS 不可用）；适合未来"增量索引/pgvector 嵌入"等无 ACL 需求场景 |
| Debezium+Kafka / FDW / pglogical | **不采用** | 运维重负（Kafka 全家桶）/ 语义坑（无跨 server join）/ PG15+ 原生行过滤已够 |
| Neon 分支 | **观察** | 秒级项目沙箱/预览副本，绑定存储引擎，托管版再评估 |
| `pg_dump -n tw_<project>` | **采用（runbook）** | schema-per-project 的项目级备份红利 |

主要来源（一手）：PostgreSQL 官方（CREATE POLICY / §5.9 Row Security / GRANT / COPY / GUC）；Supabase（RLS 指南、WALRUS 博客、Discussion #9311、Supavisor）；PostgREST（db_authz、errors）；Appwrite（Native vs Appwrite databases、DocumentsDB 公告、Permissions、Discussion #4838）；Firebase（rules-query、queries 限制）；Convex / PocketBase / Gel 文档；Citus / Crunchy 多租户指南；PlanetScale《RLS sounds great until it isn't》；nmfay / DevriQ RLS 性能实测。

### 9.3 Appwrite 五轨制的经验教训（2026-09-03 二次核验修正口径）

口径修正：Appwrite 2.0 官方是 "**five database types**"——TablesDB（typed）、DocumentsDB（schemaless）、**VectorsDB**（固定 dimension + HNSW + cosine/dot/euclidean + 内置 embedding 模型）、原生 PostgreSQL（默认 18）、原生 MySQL（8.4）。此前"三轨"的说法遗漏了 VectorsDB。

| # | 教训 | 对 torchwood 的落点 |
|---|---|---|
| 1 | **"不收抽象税"的意识形态转向**（官方原话 "no abstraction tax" / "Give the raw engine when the abstraction is wrong"）；Appwrite 数据库新增 shared/dedicated 二态 | §10.4 红线不变，但吸收 **dedicated 供给档位**：同一 API 面下提供独享库（可 resize/replicate），把规模问题变成计费问题而不是接口问题——比导向裸库平滑 |
| 2 | **VectorsDB 是独立产品线**而非某库的列类型 | 对 AI-Native 定位是直接竞品信号。我们的差异化：vector 列与文档同事务、同 RLS/GRANT 管辖（VectorsDB 无文档级权限）。但必须在 DSL 层给 KNN **一等算子**（§10.5 P0），否则 vector 列是死列 |
| 3 | **DocumentsDB 不是新引擎**：GitHub 源码确认与 TablesDB 共用同一 Utopia\Database 层——"双轨"是 API/产品层分轨，非存储层分叉 | 若未来补 schemaless，正确形态是**同一 typed 基座上的 schemaless API 视图**（未声明字段进受控 overflow），不是第二套存储——与 §3.1 拒绝"默认存储双形态"不冲突（拒的是存储分裂，不是 API 分层） |
| 4 | **五轨之间无官方互迁工具**，DocumentsDB 靠 JSON 导入导出兜底 | import/export 的 NDJSON 面要先行（§4.7 已有 export，**补 import**），否则多形态之日就是迁移骂声之日 |
| 5 | **规模问题被产品化绕行**：TablesDB 无硬数字承诺，用 dedicated 档 + native 导流；#6968（连接打满 DDL 卡死）无官方回应即关闭 | 反面教训：规模化问题市场化绕行会积累社区信任债——§3.1 的多集群分片出口要有排期承诺，不能永远停在预警线 |
| 6 | **权限/事务/批量/原子计数跨轨全保留**；DocumentsDB 加了带 max/min clamp 的 increment 专用端点 | 验证 type:role + RLS 编译层是跨形态资产；**原子更新算子家族值得整包吸收**（写侧 DSL 缺口的主要参照，见 §10.5） |
| 7 | **自托管后端演进 MariaDB → MongoDB(1.9) → PostgreSQL(2.0 默认)**，API 面稳定 | 互证 §3.1 的核心主张：底座可换的前提是物理布局不出现在契约里 |
| 8 | **native = 裸引擎 + 托管运维**（备份/PITR/Branches/HA/池化/监控，$10/mo 起步），零互操作、零 Appwrite 权限层（引擎自管凭据） | 若远期做 native 产品，卖的是运维能力；权限/审计/配额故事（裸库上没有我们的 ACL）要提前想清楚 |

DocumentsDB 的量化限制参照：每请求 100 条 query、每条 4096 字符（与 torchwood 现状上限一致）；批量/事务 Free 100 / Pro 1,000 每请求（torchwood MaxBulkOperations=1000 同量级）。

## 10. Agent 与真人双一等用户的设计影响（2026-09-03 补充）

前提：产品定位（`docs/roadmap.md` §0）本就把 Agent 当一等消费者，重设计已落地的 agent 面向能力——单 typed AST（C7）、`request_id` 幂等、机器可读错误码（code/retryable/violations/doc_url）、JSON Schema 导出、`:changes` 补偿、事件 seq、`vector` 列。本节回答"**双一等**"额外要求什么。核心判据：**真人靠直觉与 UI 容错，Agent 靠契约与归因；差异必须显式建模，不让 Agent 将就人类的接口，也不为 Agent 降级真人的体验。**

### 10.1 六维差异 → 设计决策

| 维度 | Agent 的使用特征 | 设计影响 | 状态 |
|---|---|---|---|
| **身份与归因** | 以 API key 认证；行为必须可追责 | `key:{keyID}` 成为一等可授予角色（`_acl text[]` 机制上零改动支持）；主体支持 **on-behalf-of 委托**（agent 代表某 user 行动：roles = key 自身角色 ∪ 被委托者角色，**带来源标记**，事件/审计记录双身份）；`_created_by/_updated_by` 落 keyID | 新增；**现架构即刻缺口**：keys-only 主体的审计列为空（见 10.2） |
| **信任边界** | 多 agent 共项目；误操作有放大器 | per-key ACL 沙箱（`key:{id}` ACE 隔离 agent 间数据）；**默认权限收敛**：空 ACE 种子从"全体 keys 可写"改为"创建者 key 私有"（`read/update/delete:key:<id>`——与 creatorSeedRole 同构，随 §3.1 的 keys 默认写权定调一并决策）；key scope 从 service 级细化到 database/collection/operation | 修订默认权限与 scope 表 |
| **消费方式** | LLM 生成查询；无法可靠手拼 DSL 字符串（引号/转义/算子名） | **AST 是唯一 wire 形态**——这是 C7 的根本理由，DSL 只服务真人 URL 手写；SDK 类型安全构造器 + JSON Schema 即 Agent 工具参数；新增 **`:query?dry_run=true` explain 模式**（返回命中索引、预估扫描量、复杂度预算余量）供 Agent 自校正、保护平台防全表扫描 | dry_run 新增 |
| **重试与一致性** | 自动重试；需要机器可判定的恢复路径 | 已有：`request_id` 幂等、`retryable` 标志、OCC 三态、seq。补：**429 带机器可读 `retry_after`/配额余量**；**OCC 冲突错误体带 `current_version`**（Agent 可直接取新版本合并重试）；read-your-writes（写响应读回已保证）与"写 → 事件 → `:changes`"因果链写入一致性契约文档 | 部分新增 |
| **批量与同步** | RAG/摄取/全量爬取；增量维护本地副本 | 已有：keyset 爬取（C2）、流式 export（§4.7）、`:changes`。补：**snapshot+changes 闭合**——export 返回 `snapshot_seq`，`:changes?since_seq=snapshot_seq` 无缝续接（一致性窗口闭合的关键）；**tombstone 语义显式化**（delete 事件带 document_id，`:changes` 返回删除标记，本地副本可精确收敛） | snapshot_seq 新增 |
| **配额与成本** | 高频、突发、按 key 可计费 | per-key 令牌桶（§4.7 的 per-project 配额下钻一层）；usage metering per key（计费/成本归因）；realtime 连接配额从 per-user 扩展到 per-principal（Agent 长连接订阅） | 新增 |

### 10.2 现架构即刻受影响的两个点（登记为评审遗留，不必等重设计落地）

1. **Agent 写入无归因**：`userIDFromPrincipal` 只认 `user:` 前缀角色，仅含 `keys` 的主体 `_created_by/_updated_by` 为空（`docs/developer/06-databases.md` §10 明载）——一等 Agent 的最低要求是"行为可归因"，建议现架构就把审计列落 keyID。
2. **Agent 间零隔离**：全体 key 共享 `keys` 角色 + `DefaultCollectionPermissions` 含 keys 全写（本轮评审 P2-1 未定调项）——任一 key 可改其他 key 创建的一切文档。per-key 角色落地前，至少应把默认种子收敛为创建者 key 私有。

### 10.3 人机分工（共享表面不降级）

- **错误体双语**：`message`（人）与 `code/violations/retryable`（Agent）并存——已在 §4.1 契约中定案。
- **DSL 保留为真人语法糖**：URL 手写查询是人类的便利，不是 Agent 的通道。
- **Console（真人）增 Agent 视图**：按 key 的活动审计、权限/配额管理；ACL 编辑器预设增加 `key:{id}`（现仅 any/users/keys/admin）。
- **工具目录对齐**：`docs/developer/14-agent-tools.md` 的工具与 DocumentDB 操作一一对应，每个工具的参数面即 typed AST 的子集（结构化参数，非字符串拼装）。

### 10.4 红线复核：为什么不暴露 SQL 仍然成立（2026-09-03）

背景：当初"不暴露 SQL"的动机之一是限制与规范 Agent 能力。在 Agent 一等化 + 判定下沉 RLS 之后复核该决定——**结论：成立，且比当时更成立，但性质从"限制 Agent 的产品禁令"升级为两项结构性依赖 + 一份有对价的承诺**。

四条论据（前两条是复核后新增的结构性依赖，分量重于原始动机）：

1. **物理独立性是重设计的根基**：§3.1 的全部演进自由——物理名服务端分配（`c_<base32>`）、全局 catalog、多集群分片出口、schema 演进状态机——都建立在"无人能依赖物理布局"之上。暴露 SQL 即永久冻结物理 schema，分片出口与演进契约同时报废。
2. **RLS 信任模型依赖通道隔离**：§3.2 已定"直连通道永不信任 `app.*` GUC"——开放 SQL 直连等于让调用方可以 `SET app.roles` 伪造身份，判定执行点的信任根坍塌。此外 RLS 的豁免面（唯一键/FK 检查的 covert channel、TRUNCATE 不受 RLS、COPY FROM 不支持）在任意 SQL 下全部变成可达攻击面。"RLS 让裸 SQL 变安全"不成立——RLS 只是让它没那么危险。
3. **Prompt injection 的爆炸半径控制**：Agent 是可被注入指令的执行器。SQL 是图灵完备的查询语言——注入载荷获得任意数据塑形能力；AST 是有限状态的动作空间（算子白名单、深度/参数上限、`dry_run` 可预检）。对 Agent 自身的可靠性同样如此：bounded tool schema 的执行成功率高于自由 text-to-SQL（幻觉、schema 漂移）。
4. **配额与审计的归因粒度**：§10.1 的 per-key 令牌桶与 usage metering 以"操作可枚举"为前提；任意 SQL 下一条 join 的成本无法归因、资源形态不可治理（planning 成本、锁、笛卡尔积），statement_timeout 只能兜底不能预防。

**承诺的对价（决定成立的条件）**：不暴露 SQL 的压力与 AST 表达力缺口成正比——DSL 表达不了的真实需求每多一个，"开口子"的诱惑就大一格。因此该红线必须捆绑一项纪律：**AST 表达力持续还债**（C7 算子全集、`array<T>`/`vector`、`:aggregate` 扩展面、`dry_run` 预检）。泄压阀的优先序：分析型需求 → 先扩 `:aggregate`；重度 SQL 用户 → 远期评估"原生数据库"**独立产品**（资源池隔离、不共用文档面信任模型——Appwrite 2.0 的三轨已验证该形态）；**永不**在文档面上开 SQL 口子。触发器：当高级用户索取 SQL 的诉求成为规模化声音时，按上述优先序评估，而非直接开口。

### 10.5 AST 表达力缺口清单（§10.4 的还债 backlog，2026-09-03，以 Appwrite 算子全集为参照系）

| 优先级 | 缺口 | 现状（torchwood） | 参照（Appwrite） | 说明 |
|---|---|---|---|---|
| **P0** | ~~**向量近邻查询**~~ **已落地（2026-09-05 会话 #10）**：`vector(dims)` 属性（pgvector ≥0.8，dims 2..2000）+ `hnsw` 索引（三 metric opclass，同列多 metric 共在）+ `vector_search` 一等算子（limit=k、距离平行列回传、与 filter 组合、须匹配 metric 的 hnsw 索引；~~与 orders·分页互斥~~ orders 维持互斥、**分页已放开（2026-09-05 转出 POC B2）**：`kvc:` 距离游标 + 首页完整距离组切页 + 续页 (dist,_id) 精确全序，跨页不重不漏）——**同事务同 RLS 管辖**（iterative scan `strict_order` 经 SET LOCAL 事务内启用；稀疏可见行 5/5 全召回对 1/5 的灵魂差异已测试锁定） | 曾：无 KNN 算子，列落地即死列 | VectorsDB 独立产品线（无文档级权限） | 挂账：ef_search/迭代调参暴露（**已知边界：近重复不可见簇 + 极低可见率下 iterative scan 仍可能 < k——HNSW 近似索引固有边界，已文档化**）、halfvec/sparsevec、embedding 接入、DSL 字符串形态 |
| **P0** | ~~**数组查询/写侧算子**~~ **已落地（2026-09-04 会话 #8）**：查询 `containsAny/containsAll`（`&&/@>`，仅 array 属性）+ 写侧 `APPEND/PREPEND/REMOVE/UNIQUE` 四算子（单语句 SET，NULL 语义：append/prepend 归一、remove/unique 保真）；~~Intersect/Diff/Insert/Filter、TransactionOp 数组算子~~ **已补齐（2026-09-05 转出 POC B1）**：INTERSECT（交集去重保原序）/DIFF/FILTER（受限形态=REMOVE 同构）/INSERT（0 基 index 越界尾插）+ execute-tx op `array_updates`（仅 update 消费）；NULL 语义二分：添加类归一、读改写类保真；**查询侧不设对应算子**（Appwrite 的 arrayIntersect/Diff 是写侧算子，查询谓词由 containsAny/All 覆盖，B1 裁决） | 曾：无配套算子 | Appwrite 全集参照 | 挂账：数组列多列索引（需 btree_gin，已拒） |
| **P0** | **`_created_by/_updated_by` 不可查询** | 系统字段白名单只有 `_id/_created_at/_updated_at/_version` | 系统字段可查 | "查我创建的文档"不可表达；**现架构即刻可修**（`systemQueryFields` 加两列） |
| **P1** | ~~`not*` 取反变体家族~~ **已落地（2026-09-04 单 AST 会话）** | 曾无 notBetween/notContains 等 | 全套 not* 变体 + 通用 NOT 移出算子集（作者收敛，见 §4.1） | 算子级取反可走索引；DSL/proto 双栈不对称随 C7 落地一并消解 |
| **P1** | 聚合家族 | count 独立 RPC，无 sum/avg/min/max/group_by | （无 group_by；靠 native 库承接） | `:aggregate` 扩展面是分析需求的泄压阀（§10.4）；group_by 的权限/语义要单独设计 |
| **P1** | object 字段访问 | `object`(jsonb) 类型无路径查询/投影 | `exists/notExists`、`elemMatch`（DocumentsDB） | 路径访问 `a.b.c`、存在性、`elemMatch`（数组内对象元素匹配）；select 子字段投影 |
| **P1** | increment clamp | increment 无上下界保护 | increment/decrement 带 `max/min` 硬上限（越界报错）；另有 multiply/divide/modulo/power、toggle | clamp 对 Agent 场景价值高（计数器防溢出/竞态）；数值算子家族按需扩展 |
| **P2** | 关系查询/加载 | relation 列为远期 | 按对端字段过滤（`reviews.author`）、`select "rel.*"`（默认不加载）、双边 read 权限、关系深度 ≤3 | 产品决策后再排；Appwrite 验证了形态与边界（权限双边检查、深度限制） |
| **P2** | regex 匹配 | 无 | `regex`（DocumentsDB） | 需防 ReDoS/全表扫描：仅锚定语法 + statement_timeout + 索引引导 |
| **P2** | 全文增强 | 单列 search + 需显式 fulltext 索引 | search 同构（需索引） | 多列联合、`ts_rank` 排序、短语/前缀控制 |
| **P3** | 时间助手算子 | greaterThan("$createdAt") 冗长但可表达 | `createdBefore/After`、`updatedBefore/After`、`createdBetween/updatedBetween` | DX 债非能力债（AST 编译层加糖即可） |
| **P3** | orderRandom / TTL 缓存 / geo | 无 | orderRandom（两库均有）；DocumentsDB list 支持 1~86400s TTL 响应缓存；TablesDB 空间算子全家（仅 TablesDB） | orderRandom 有 TABLESAMPLE 性能陷阱需限流；TTL 缓存对 Agent 重复查询有成本价值；geo 待 PostGIS 决策 |

现状双栈不对称（DSL 缺 `in/or/and`，proto AST 缺 `between/isNull/isNotNull/select/cursor`）已由 C7 单 AST 消解，不列入 backlog。

## 11. 未决设计问题登记册（2026-09-03）

重设计目前定的是方向（决策层）；以下是需要进一步深化细化讨论的机制/语义级开放问题，按"阻塞程度"排序。**标注⚠的项会反过来影响已定决策**——⚠ 已全部于 2026-09-03 收敛（三项架构级 A1/B1/G1 + D1/H1/E2，连同默认值整包），决议记录见 §11-J。

### A. RLS 落地机制（阻塞阶段③）

| # | 开放问题 | 说明 |
|---|---|---|
| A1 ~~⚠~~ | **GUC 注入与连接池的集成形态** | **已接受（2026-09-03）：每请求一事务（含读，autocommit 退役）+ 事务首条 `set_config(...,true)`**——漏注入=空结果（fail-closed），事务结束自动失效零残留；否决会话级 GUC（错配路径静默继承上一用户角色，结构性不可防）。遗留原型任务：验证 pgdriver 流水线把额外往返压到 ≤1 |
| A2 | roles_sig HMAC 细节 | 密钥派生与轮换、签名覆盖面（roles+tenant+时间戳防重放？）、SECURITY DEFINER 函数加固（search_path、LEAKPROOF） |
| A3 | policy × 集合规模 | 每集合 4 policy × 千集合 = 4000 policy 对 plan cache/relcache 的影响；EXPLAIN 门禁怎么进 CI（基准集见 I） |
| A4 | 双读迁移期的 policy 数据源 | `_perms` → `_acl` 灰度期间 policy 读哪个源：过渡 policy 读 `_perms`（复杂）vs 先全量回填 `_acl` 再开 policy（简单但窗口长）——影响阶段③的切分。**POC 阶段本项为空**（无存量数据，直接重建不双读），决议预置于转出 POC 后的阶段③方案 |
| A5 | realtime 掩码缓存失效 | 集合级 perms 变更后 TTL 5s 内的可见性延迟窗口是否接受；要不要主动失效（权限变更发内部事件） |
| A6 | `tw_owner` 的 DDL/运维通道 | FORCE RLS 下 owner 的排查查询被自己 policy 挡（DDL 本身不受 RLS，但 SELECT 验证会）——运维走 `tw_system`，需 runbook 化 |

### B. 事件顺序语义（阶段④的隐藏深坑）

| # | 开放问题 | 说明 |
|---|---|---|
| B1 ~~⚠~~ | **事件顺序承诺与 seq 分配序** | **已接受（2026-09-03）：放宽为分配序**——承诺定稿为「**单文档全序**（行锁保证 seq 随提交序，免费且稳固）；集合内为分配序（跨文档不保证与提交序一致）；seq 仅作续传游标与去重」。seq 空洞=回滚事务（不表示丢事件），客户端缓冲重排因此天然不可行，一并否决；不引入排序器组件（事后可收紧、反向不可） |
| B2 | outbox 单表写热点 | 全局 seq 的 `nextval` + 单表 INSERT 是全部文档写的串行点；要不要按 project 分区 outbox + 项目内 seq（与 B1 联动） |
| B3 | Redis Stream 拓扑 | 消费组 per-server 还是共享组；XACK 位点与 `published_at` 回写的对应关系；MAXLEN trimming 与 1h 重放窗口的耦合 |
| B4 | RESYNC/data_ref 契约 | RESYNC 帧格式与客户端 SDK 重放指引；`data_ref` 按版本拉取的 API 形态 |

### C. Schema 演进状态机（阶段②③配套）

| # | 开放问题 | 说明 |
|---|---|---|
| C1 | migrating 期间的读写矩阵 | 新列可见性（读旧还是新）、写按目标 schema 校验的具体行为、DSL 对新旧列的白名单切换时机 |
| C2 | backfill 与并发写竞态 | 版本号守卫 vs 触发器 vs 锁窗口；限速与失败恢复 |
| C3 | unique 索引上线遇存量重复 | validate 阶段的报告形态（冲突行清单？）与阻断语义 |
| C4 | vector 维度变更流程 | 列维度不可变 → 必然是"新列+回填+切流"——作为迁移状态机的标准模板固化 |
| C5 | deprecated 的兼容窗口 | 与 Proto 规范的 `deprecated=true` 一个版本周期约定对齐；sunset 日期进 JSON Schema（§10.1 已提，格式未定） |

### D. 聚合与 KNN 的语义

| # | 开放问题 | 说明 |
|---|---|---|
| D1 ~~⚠~~ | group_by 的键泄露 | **已定（2026-09-03，降级为规范澄清）**：聚合一律在 policy 过滤后的行集上执行（securityQuals 先于 GROUP BY，不可见行的键不会出现）——写成显式规范 + golden 测试防实现走样；最小桶/k-匿名记为可选产品功能（默认关）；权限变更前后聚合不可比属固有属性，文档化 |
| D2 | KNN + filter 的召回语义 | HNSW 先近邻后过滤会漏召回——需 pgvector ≥0.8 的 iterative index scan（先过滤后迭代）；ef_search 暴露面与默认值 |
| D3 | dry_run 输出契约 | 成本估算来源（EXPLAIN 解析 vs pg_statistic）、复杂度预算的计量单位与扣减规则 |

### E. 事务与批量 API 面（需重审的历史决策）

| # | 开放问题 | 说明 |
|---|---|---|
| E1 ~~⚠~~ | ~~跨文档事务是否重开~~ **已接受（2026-09-03 维护者决策）：共享事务内核 + 三形态消费**，方案见下方；主设计正文见 §4.8 | 旧 D-6 设计（v2 §5）资产可复用：op 表结构（seq/op_type/document/data/permissions/increment/version/conflict_columns）、锁行追加、60s TTL、单 pending——删除理由是"内测无消费者"，非设计错误 |
| E2 ~~⚠~~ | execute-tx 的 per-item results | **已接受（2026-09-03）**：`mode: ATOMIC（默认）| PARTIAL`——PARTIAL 逐 op 独立执行、已成功不回滚、返回 per-op `{status, error}`；`request_id` 覆盖整批（重放返回首次结果）；现有 Bulk API 保持纯原子不变 |

**E1 收敛方案：抽象层级 = 事务内核，不是 Staged Session**

```
内核（共享）：
  ① op 模型       —— 可序列化异构 op 列表（复用旧 document_transaction_ops 字段族）
                     {type, database, collection, document_id, data, permissions,
                      increment, expected_version, conflict_columns}
  ② 单事务执行器  —— op[] 在一个 RunInTx 内顺序执行：
                     RLS/GUC 一次注入、逐 op 判定（commit 时权限，与"写权即可读"一致）；
                     按 (_tenant,_id) 排序加锁防批内死锁；OCC 逐 op（expected_version）；
                     outbox 事件同事务（可共带 transaction_id）；all-or-nothing；
                     失败返回带 op index 的 violations
消费形态（三个，按消费者位置选）：
  A. documents:execute-tx  —— 一次性原子 op 批（客户端组装，单请求提交）。
     本质是 Bulk 的泛化（现 Bulk 只支持同构 update/delete），无暂存表/无 TTL/无会话。
     覆盖大多数跨文档原子性需求；op 上限对齐 MaxBulkOperations=1000。
  B. Functions 事务上下文  —— 函数代码在服务端执行，可持有真实事务：执行上下文
     注入 txCtx（InTx/RunInTx 管道已有，缺 GUC 注入 + 生命周期），SDK 的每次文档
     写自动 JoinTx；成功 commit、panic/超时 rollback。**不走 staged**——命令式代码
     依赖中间读结果，replay 语义无法表达。
  C. staged session（按需）—— 跨请求暂存 + 服务端中间态才需要：复用旧 D-6 表设计
     （锁行追加/60s TTL/单 pending/100 ops 上限），Commit 重放走同一执行器。
     等 A 的需求证据（"多轮交互且需要服务端中间态"）出现再启用。
```

分期：Phase 1 = A（增量最小，Bulk 已验证单事务异构锁与回滚）；Phase 2 = B（补 GUC 注入与函数生命周期）；Phase 3 = C（等证据）。Agent 联动：op[] 是天然的工具参数——结构化、可整体幂等（request_id）、可 dry_run 校验；旧设计的 `created_by = key:{APIKeyID}` 与 §10.1 per-key 归因天然对齐。事件联动：批内 N 事件同一次 COMMIT，事件间顺序 = op 顺序（B1 的分配序问题在批内不存在）。

### F. key:{id}/scope/delegation 实体设计（阶段③前置）

| # | 开放问题 | 说明 |
|---|---|---|
| F1 | scope 表达式扩展 | collection 级 scope 的语法与语义；API 层 scope 与 DB 层 RLS 两道检查的对齐与错误码 |
| F2 | on-behalf-of 令牌形态 | key 认证 + 用户委托的组合载体（JWT 结构）、roles 并集的来源标记、审计双身份的落库字段 |
| F3 | per-key 配额维度 | QPS/日配额/并发连接；429 载荷与 Retry-After 规范 |
| F4 | ACL 批量管理 API | "给 N 篇文档加 reader"的原子性与上限 |

### G. 全局 catalog 与多集群的张力（阶段②内嵌未决）

| # | 开放问题 | 说明 |
|---|---|---|
| G1 ~~⚠~~ | catalog 全局化 vs 多集群分片 | **已接受（2026-09-03）：catalog 定位 cluster 内全局**——project→cluster 路由表存控制面；项目迁移 = schema + catalog 行 + 路由重指的成套迁移协议；跨集群视图降级为控制面聚合指标（不提供跨集群实时 SQL）；否决 catalog 独立服务（文档操作热路径多一次跨库查询） |
| G2 | 存量四表迁移任务 | 幂等/断点续跑/EnsureCatalog 语义切换点的具体设计；projectschema migrator 的退役路径（现有 schema_migrations 记录处置） |
| G3 | ddl_seq 冲突的 API 语义 | 409 + 重读模式还是版本化 PATCH |

### H. 数据边界与限制（现状即有缺口）

| # | 开放问题 | 说明 |
|---|---|---|
| H1 ~~⚠~~ | 文档大小上限 | **已接受（2026-09-03）**：文档总编码大小默认 **1 MiB**（可配置，对齐 Firestore 锚点）、object/jsonb 单属性 ≤256 KiB（与事件截断阈值对齐）；app 层写入前校验 + `DOCUMENT.TOO_LARGE`；**现架构即刻可修** |
| H2 | 上限族 | `_acl` ACE 数、数组长度、每集合列数（PG 1600 硬限的软预算）、object 嵌套深度 |
| H3 | list 响应 TTL 缓存 | Appwrite DocumentsDB 有 1~86400s TTL——对 Agent 重复查询是成本杠杆，要不要做（与配额联动） |

### I. 验收基准（阶段③④的门禁前置）

| # | 开放问题 | 说明 |
|---|---|---|
| I1 | RLS 性能基准集 | CI 基准（百万行集合 × policy 查询 P99 门禁），EXPLAIN 计划断言（InitPlan 化验证）自动化 |
| I2 | 语义等价测试 | `tw_can`/`tw_visible` 的 SQL golden 矩阵构建方式；双读迁移期 `_perms` vs `_acl` 判定一致性 fuzz |
| I3 | 事件顺序性测试 | B1 定案后：重放窗口内乱序注入的回归用例 |

### J. 决议记录（2026-09-03）

**架构级三项（按推荐接受）**：

| 项 | 决议 | 正文落点 |
|---|---|---|
| A1 | 每请求一事务（含读，autocommit 退役）+ 事务级 `set_config` 注入身份，fail-closed；否决会话级 GUC。遗留原型：pgdriver 流水线压缩往返 | §3.2 工程纪律首条 |
| B1 | 顺序承诺定稿：单文档全序（行锁保证）+ 集合内分配序 + seq 仅作续传/去重；seq 空洞=回滚事务；不引入排序器 | §4.5 |
| G1 | catalog 定位 **cluster 内全局**：project→cluster 路由在控制面，项目迁移成套搬；跨集群视图=控制面聚合指标；否决 catalog 独立服务 | §4.2 DDL 注释 |

**决策项（按推荐接受）**：D1 降级为规范澄清（聚合在 policy 过滤后执行 + golden 测试；最小桶为可选功能默认关）；H1 文档 ≤1 MiB（可配置）+ object ≤256 KiB + `DOCUMENT.TOO_LARGE`（现架构即刻可修）；E2 `execute-tx` 增加 `mode: ATOMIC（默认）| PARTIAL`（per-op 结果、已成功不回滚、request_id 覆盖整批）。

**默认值整包采纳**：A2（roles_sig 派生复用 page-token 模式 + 双密钥轮换窗口 + 短时效签名）、A4（先全量回填 `_acl` 再开 policy）、A5（接受掩码缓存 5s TTL 窗口并文档化）、A6（运维查询走 `tw_system`，runbook 化）、B2（outbox 先不分区，吞吐超阈值再按 project HASH）、B3（Stream 每副本一消费组；XACK 为主位点，保留 published_at 回写统一清理与重放）、B4（RESYNC 帧格式与 `data_ref=GET documents/{id}?version=N` 后置到阶段④设计稿）、C4（vector 维度变更"新列+回填+切流"为标准模板）、D2（锁定 pgvector ≥0.8 iterative scan，ef_search 首期不暴露）、G4（表内分区=最后治理手段，默认 none，阈值告警触发建议）、H2（`_acl` ≤64 ACE；数组 ≤1000 元素；每集合列数软限 200；object 嵌套 ≤8 层）、H3（list TTL 缓存不做）。

**维持"随对应阶段设计稿细化"**：A3、B4 细节、C1-C3/C5、D3、F1-F4、G2-G3、I1-I3。
