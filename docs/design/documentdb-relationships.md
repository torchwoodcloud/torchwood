# DocumentDB Relationship 模式设计提案（关系属性 / 关系谓词过滤 / 级联删除）

> 状态：**设计提案（蓝图），未排期**——对应 `docs/roadmap.md` §3 P3 条目 "Relationships"。POC 阶段无向后兼容包袱，proto 可直接扩展。
> 来源：2026-09-07 会话研究（代码地面真值核查 + 竞品对照：Appwrite / PocketBase / Supabase(PostgREST) / Parse / Hasura / Firebase·Convex），两轮结论含对早期草案的 2 处修正（§5 D1/D2）。
> 相关：`docs/developer/06-databases.md`（§0 十五条关键不变量为本文评审锚点）、`docs/design/documentdb-redesign.md`、`docs/roadmap.md` §0/§3。

---

## 0. 摘要

DocumentDB 当前无 relationship 属性类型、无跨集合 JOIN、无引用完整性。三块能力的补齐**均可行**，但与现有架构的贴合度差异显著：

| 能力 | 可行性 | 架构贴合度 | 关键约束 |
|---|---|---|---|
| Relationship 属性类型 | 高 | 好——catalog attrs 为 JSONB（`db/migrations/000003_catalog_global.up.sql:31`），物理存储复用现有 string/string[] 列 | onDelete 不可变；同 database 作用域；生命周期互锁 |
| 关系谓词过滤 | 高 | 好——EXISTS 半连接天然享受每表 RLS，判定单源不破 | 禁止作为排序键（keyset-only 不变量 12）；深度 ≤3 |
| 关联展开（水合） | 中 | 好——页内批量二次查询，AIP expand 形态 | 必须 opt-in（Appwrite 自动加载的性能教训） |
| 级联删除 | 中 | 差——PG 原生 FK 级联绕过 RLS，与判定单源（不变量 6）冲突 | 语义采用"服务端完整性"（业界共识，§5 D1）；事件扇出上限 |

**分期建议：R1 属性类型+restrict → R2 关系过滤 → R3 级联 → R4 水合 → R5 M:N 镜像/back-relations**。每期配触发条件（§6），当前（POC 转正前）均不启动。**2026-09-07 产品拍板：目标用户暂定为 Agent 原生构建者**（§7）——据此调整触发条件权重（§6）：迁移类触发降为次要，Agent 模板需求信号为主；近期唯一动作已落地为 `docs/developer/16-document-modeling.md`（手工引用模式指南 + Agent 提示词规约）。

---

## 1. 现状与差距

### 1.1 今天的正确用法（手工引用模式）

当前属性类型全集 `string/email/url/integer/float/boolean/datetime/json/vector`（`postgres_collection_ddl.go:1043` `pgTypeFor`），无 relationship kind。博客类模型（categories 1:N posts、posts M:N tags）用引用建模已完整可用：

- **1:N**：`posts.categoryId`（string, required）+ key 索引；查询 `equal("categoryId", …)`。
- **M:N**：`posts.tagIds`（string, `array=true`）→ PG `TEXT[]` + 自动 GIN；查询 `containsAny`/`containsAll`（⚠️ `contains` 编译为 `ILIKE '%…%'` 模糊匹配，`postgres_query_compile.go:440`，不适用于数组成员判断）；写侧 `array_updates` 八算子原子维护（OCC 兼容）。
- **跨集合原子写**：`ExecuteTransactions` 单事务 + 事件 `transaction_id`（`postgres_transactions.go:48`）——手工 M:N 最难的"双写一致性"已被现有原语覆盖。
- 反向查询（"某 post 的 tags"）：读 `tagIds` 后 `in("$id", […])` 批量取回（`$id` 为合法查询字段，映射物理 `_id`，`postgres_query_compile.go:27`）。

**缺口**：引用完整性（孤儿引用无守卫）、关系路径过滤（"按 category.slug 查 posts"需客户端两段查询）、反向查询非一等、模板样板代码。

### 1.2 现状事实（已核实）

- 属性 key 正则 `safeNameRe`（`postgres.go:27`）不含点号 → `category.slug` 点路径语法无歧义；查询白名单 `checkField`（`postgres_query_compile.go:175`）今日对未知字段一律拒绝——fail-closed 基线，扩展点明确。
- `ListDocuments` 单表编译（`postgres_document_query.go:228`）；keyset-only（不变量 12）；查询单栈 typed AST（不变量 10）。
- 删除为单语句 compare-and-delete（`postgres_document_crud.go:772` `execDeleteVersioned`），事件经 `publishDocumentEvent` 与数据行同 COMMIT（不变量 3）。
- `BulkDeleteDocuments` 整批一事务（`postgres_permissions.go:650`）；`ExecuteTransactions` 单事务 + 批内 op 排序预取 advisory lock（`postgres_transactions.go:53`）。
- `DeleteCollection` app 层仅有系统集合拦截（`internal/app/server/databases.go:230`），**无任何关系依赖守卫**（新增工作量）。
- 无既有级联基础设施（`internal/app/server/cascade_guards_test.go` 为 groups/users last-owner 保护，与本文无关）。
- SECURITY DEFINER 函数有成熟先例：`tw_set_document_acl`、`tw_roles`（迁移 000004）。

---

## 2. 竞品对照

| 能力 | Appwrite | PocketBase | Supabase/PostgREST | Parse | Hasura | Firebase/Convex | Torchwood 现状 |
|---|---|---|---|---|---|---|---|
| 关系类型 | 4 类型 + twoWay/side | relation + maxSelect | FK 推断 | Pointer/Relation | FK 声明 | 无 | 手工 ID/数组 |
| 关系过滤 | dot-path 全算子（2026-02） | filter + back-relations `via` | embed + filter | 无（手动） | 嵌套过滤 | 无 | 无 |
| 关联展开 | 曾自动加载 → 改 opt-in | `expand` | `?select=tbl(*)` | `include` 点路径 | 嵌套查询 | 无 | 无 |
| 级联 | restrict/cascade/setNull，服务端 | cascadeDelete，服务端 | 原生 FK（绕 RLS） | 无（Cloud Code） | 交给 DB | 无 | 无 |

要点（详细来源见 §8）：

1. **Appwrite（直接对标面）**：属性模型 `relatedTable/relationshipType(oneToOne|oneToMany|manyToOne|manyToMany)/twoWay/twoWayKey/onDelete(restrict|cascade|setNull)/side(parent|child)`；关系查询 `Query.equal("author.name", …)` 点路径 + 全算子 + twoWay 双向，官方 12–18x 提升；嵌套展开深度上限 **3 层**。教训两条：关联文档自动加载因性能改为 opt-in；CLI 修改 onDelete 反而删属性毁数据（→ 本文 D4：onDelete 不可变）。另有同集合多重关系 bug 记录（issue #8058，→ 需测试覆盖）。
2. **Supabase/PostgREST**：RLS 逐表生效（嵌入子查询同样受 policy 约束）；**PG 原生 `ON DELETE CASCADE` 绕过 RLS** 为社区公认 footgun，共识是"级联不做鉴权、鉴权不靠级联"。
3. **PocketBase**：`maxSelect` 单/多对应 1:N/M:N（数组存储）；`cascadeDelete` 服务端完整性语义，不看调用者 API rules；back-relations（`via` 过滤/排序/expand）是一等公民。
4. **Parse**：`include('a.b.c')` 点路径嵌套水合——expand 语法先例；Pointer/Relation 无级联（Cloud Code 手动）。
5. **Hasura**：权限逐表独立——遍历关系要求基表与关联表都有 select 权限，嵌套行按子表权限过滤。**本文采纳为 R2 验收基准**（与 EXISTS + 每表 `tw_visible` 语义同构）。
6. **Firebase/Convex**：无关系/join/级联，全手动——"无关系模型"非能力缺陷而是定位选择；两者均为商业成功案例。

---

## 3. 设计方案

### 3.1 Relationship 属性类型（R1）

**API 面**（`CreateAttributeRequest` 扩展，protovalidate 声明形状）：

```
related_collection_id  string   // 必填，同 database 内
relation_type          enum     // ONE_TO_ONE | ONE_TO_MANY | MANY_TO_ONE | MANY_TO_MANY
two_way                bool
two_way_key            string   // two_way=true 时必填（对侧虚拟属性名）
on_delete              enum     // RESTRICT | SET_NULL | CASCADE，默认 RESTRICT，不可变（D4）
```

**物理存储**：1:N / manyToOne 存"多"侧单 ID 列（string）；oneToOne 同 + unique 索引（PG unique 允许多 NULL，天然匹配"未关联"多行）；twoWay 为元数据对称注册（对侧虚拟属性，物理存储单侧）；M:N 首版 = 声明侧数组列 + twoWay 镜像双写（同事务），复用数组内核（D2）。catalog `attrs` JSONB 增加 relationship options。

**校验与生命周期互锁**：

- 建属性时：目标集合存在性、环检测（cascade 前提：关系图为 DAG）、two_way_key 与对侧既有属性不冲突。
- `MigrateAttribute` 对 relationship kind 拒绝改类型（only 生命周期状态迁移）。
- `DeleteCollection` 前置出入边检查（restrict 语义，要求先清边）。
- 作用域：**限同 database**（同 schema）。跨库 = 跨 schema，业务库删除是 `DROP SCHEMA CASCADE`（不变量 2），引用语义与 catalog 范围失控；系统静态表（users 等，边界邻居）不得作为关系目标。

### 3.2 关系谓词过滤（R2）

- **语法**：typed AST `Filter.attribute` 允许点路径（`category.slug`），按 catalog 关系注册表逐跳解析 + 别名；深度 ≤3（对齐 Appwrite）。SDK/CLI DSL 糖同步（不变量 10：DSL 仍只是客户端糖）。
- **编译**：半连接——`EXISTS (SELECT 1 FROM <related> sub WHERE sub._id = d.<fk> AND sub._tenant = ? AND <谓词>)`。不产生父行扇出，keyset 全序不变量无损；**子表 SELECT policy（`tw_visible`）在子查询内自动生效**——不可见子行不进 EXISTS，判定单源（不变量 6）零破坏。manyToMany 数组列用 `sub._id = ANY(d.<tags>)` 形态。
- **硬约束**（validateQueryFields 显式拒绝，fail-closed）：
  - 关系路径**禁止作排序键**——cursor token 需服务端回查排序键值，路径键值不在驱动表（不变量 12）；
  - 聚合（不变量 13）与 vector_search（不变量 10）不接受关系路径；
  - 全算子放行但 `search` 需对侧 fulltext 索引（沿用现行校验）。
- **验收基准**：Hasura 逐表鉴权语义——两表 policy 均参与、子表不可见行视同不存在。

### 3.3 关联展开 / 水合（R4）

- **显式 opt-in**：List/Get 请求 `expand` 参数（点路径，≤3 层；Appwrite 自动加载的性能教训直接采纳）。
- **实现**：页解析后按外键值批量二次查询（`in("$id", […])`，每页一查，无 N+1；参数上限沿用不变量 10 的 ≤2000 绑定参数约束）。
- **响应形状**：关联对象内嵌 `data.<key>`（同 key 冲突 = InvalidArgument，建属性时已禁）；对侧集合 RLS 过滤后**缺失即省略**（防枚举，对齐不变量 15 的 NotFound 语义）。

### 3.4 级联删除（R3）

**三形态裁决**：

| 形态 | 裁决 | 理由 |
|---|---|---|
| PG 原生 FK 级联 | **否** | RI 检查与级联绕过 RLS（击穿不变量 6/8）；子行删除不产生 outbox 事件（Go 侧 `publishDocumentEvent` 写事件，触发器补事件 = SQL 拼 JSON 信封双源） |
| 适配器层同步级联（服务端完整性语义） | **是** | 见下 |
| 事件驱动异步级联 | 否（清理类任务可用） | 孤儿窗口与"删除即原子"直觉冲突 |

**执行模型**（D1，与 Appwrite/PocketBase/PG 对齐的"服务端完整性"语义）：

- `DeleteDocument` 事务内：SECURITY DEFINER 函数（owner 属主，模式对齐 `tw_set_document_acl`）按关系注册表解析出边；`restrict` = 子行存在性计数（**可见性无关**——RLS 过滤的 EXISTS 会漏不可见子行，产生"幽灵可删"）；`cascade` = `DELETE … WHERE fk = ? RETURNING _id, _version, _acl`（system 权限，单语句取回全部子行事件凭证）→ 逐子行发 delete 事件（同 outbox 同事务，seq 有序）；`set_null` = `UPDATE … SET fk = NULL RETURNING …`（产生 **update 事件 + `_version` bump**，非 delete 事件）。
- 语义声明：cascade/setNull 为 schema 拥有者声明时显式选择的**完整性机制**，执行不做调用者可见性过滤（业界共识）；restrict 的存在性计数同理可见性无关。RLS 单源判定管"请求授权"，级联管"schema 完整性"，两者正交。
- **扇出上限**：单次级联子行数上限（建议 10k，超限 InvalidArgument 拒删并提示先减子行）——1 万子行事件突发会触发实时消费者满水位 resync-close（协议有 `last_seq` 重同步兜底，但需文档化）。
- **死锁面**：execute-tx advisory lock 预排序只覆盖声明 op，M:N 镜像双写会触碰对侧行——并发级联可能 40001（PG 解一，客户端重试）；进测试矩阵。
- 组合：`BulkDeleteDocuments`/`ExecuteTransactions` 单事务边界天然继承级联（`postgres_permissions.go:650` / `postgres_transactions.go:48`）；幂等（不变量 11）无扩展——级联内含于删除响应，execute-tx op 结果附级联行数供审计。

### 3.5 周边面

- **import 路径豁免 restrict**（导入顺序自由）；export 携带 relationship 元数据。
- **OpenAPI/swagger 暴露 relationship 元数据**（roadmap §0 Agent-Native：Agent 可机器发现关系拓扑——对 Appwrite 的差异化位）。
- 双向 twoWay 写：首版仅物理侧可写，虚拟侧只读（收窄面，后续按需求放开双写）。

---

## 4. 不变量影响矩阵（`06-databases.md` §0）

| # | 不变量 | 影响 | 说明 |
|---|---|---|---|
| 1 | 租户隔离 | 无 | 同库同 schema；fk 值为文档 ID |
| 2 | DDL 两段式 | 无 | 关系列仍是普通列；B4 两段式照旧 |
| 3 | 同事务原子性 | 强化 | 级联子行删除与事件同 outbox 同 COMMIT |
| 4 | OCC | 无 | setNull 走 update 语义（version bump + update 事件） |
| 5 | 注入防御 | 触碰 | 路径→物理列解析全程 `quoteIdent`/白名单；子查询谓词参数绑定 |
| 6 | 判定单源 | 触碰+缓解 | 过滤：子查询 RLS 天然生效（强化）；级联/restrict 计数：SECURITY DEFINER 完整性机制，文档化为豁免面（对齐 `tw_set_document_acl`/`tw_roles` 先例） |
| 7 | 事件语义 | 触碰 | 扇出上限 + 突发文档化；同文档 seq 全序不受影响 |
| 8 | 默认私有 | 触碰 | cascade 可删调用者不可见子行——完整性语义的显式让步（D1），产品文档明示 |
| 9 | 标识长度 | 无 | （R5 若做 junction：表名守 63 字节组合规则） |
| 10 | 查询单栈 | 触碰 | AST attribute 点路径；聚合/vector 路径拒绝；DSL 糖同步 |
| 11 | 写幂等 | 无 | 级联内含于删除响应 |
| 12 | keyset-only | 触碰 | 关系路径禁作排序键（首期显式拒绝） |
| 13 | 聚合可见行 | 无 | 首期拒绝关系路径聚合 |
| 14 | 连接模型角色分层 | 触碰 | 新增 SECURITY DEFINER 函数（owner 属主 + 权限收敛，对齐 000004 模式） |
| 15 | 可写即可读 | 触碰 | 水合缺失即省略、半连接不可见即不存在——防枚举语义对齐 |

---

## 5. 关键决策记录

- **D1（级联权限语义）**：cascade/setNull/restrict = 服务端完整性操作，可见性无关，SECURITY DEFINER 执行。修正记录：早期草案曾推荐"调用者角色执行 + 任一子行不可删整体失败"，被否——(a) 与 PG/Appwrite/PocketBase/Supabase 全部实践相反；(b) 存在正确性漏洞：RLS 过滤的 `DELETE…RETURNING` 会静默跳过不可见子行，无法区分"无不可见子行"与"跳过不可见子行"，产生孤儿。若未来需要 fail-closed 变体：SECURITY DEFINER 先计数、调用者可见删除后比对数量、不一致整体回滚（复杂度显著更高，按需再议）。
- **D2（M:N 存储）**：首版镜像数组（声明侧数组列 + twoWay 同事务双写），复用数组内核（GIN/containsAny/containsAll/array_updates）；junction 物理表留作规模优化（需新发明 RLS 语义，POC 阶段不值）。修正记录：早期草案"不建议双侧数组"作废——数组内核是现成且经受过测试的。
- **D3（作用域）**：关系限同 database；系统静态表不得为目标。
- **D4（onDelete 不可变）**：变更 = 删属性重建（B4 两段式生命周期）；Appwrite CLI 修改 onDelete 毁数据为直接教训。
- **D5（排序/聚合/vector 拒绝路径）**：keyset-only 与聚合并重不变量下的 fail-closed 收敛。
- **D6（水合 opt-in）**：不自动加载关联（Appwrite 性能教训）。

## 6. 分期与触发条件

| 期 | 内容 | 触发条件（2026-09-07 拍板后权重：Agent 需求信号为主，迁移信号为次） |
|---|---|---|
| R1 | 属性类型 + restrict + 生命周期/DeleteCollection 守卫 | Agent 模板/真实会话中手工引用模式反复出现（主）；Appwrite 迁移需求实体化（次） |
| R2 | 关系谓词过滤（EXISTS，深度 ≤3） | R1 落地后首个真实场景提出跨关系过滤 |
| R3 | setNull/cascade（扇出上限 + 死锁测试） | 客户实际抱怨孤儿数据清理负担（R1 的 restrict 已挡最痛场景） |
| R4 | expand 水合 | 客户抱怨往返延迟/请求次数 |
| R5 | M:N twoWay 镜像、back-relations 一等化、跨关系聚合 | M:N 使用量起来之后 |

**近期动作（已完成）**：手工引用模式固化为 `docs/developer/16-document-modeling.md`——含选型速查、博客模型全流程（建 schema/写/查/删除协议）、junction 升级条件、查询陷阱清单与可嵌入 Agent 提示词的建模规约；已登记 `docs/developer/README.md` 索引与 Agent 阅读路径（12 → 14 → 16）。

## 7. 产品必要性（摘要）

**2026-09-07 产品拍板：目标用户暂定为 Agent 原生构建者**（roadmap §0 定位收敛的结果，Appwrite 迁移者不是当前目标群体）。第三方评估结论：**P3 定位正确，不应提前**。该拍板下的论据权重：(a) 决定性——Agent 对样板代码不敏感、怕语义歧义不怕机制缺失，"模式指南 + 契约发现面（OpenAPI/`:exportSchema`/`/.well-known`）"以小成本捕获 Agent 价值的大部分，机制类 relationship 的边际收益集中于人类开发者与迁移者（均非当前目标群体）；(b) 手工模式 + `ExecuteTransactions` 已覆盖能力面（Firebase/Convex 先例）；(c) R1–R5 触碰 15 条不变量中的 9 条，集中在权限内核/事件脊柱/查询编译器三个最危险子系统，与转正门禁（15-exit-poc A 区清零）争抢同一评审带宽；(d) roadmap 本已将 Relationships 挂 P3（与 Vectors/Geo 同行），维持不动。反转条件见 §6 触发器——Agent 模板中手工引用模式反复出现是 R1 的主触发信号。

## 8. 参考资料

- Appwrite：[relationships 博客](https://appwrite.io/blog/post/simplify-your-data-management-with-relationships) · [relationship queries](https://appwrite.io/blog/post/announcing-relationship-queries) · [ColumnRelationship 模型](https://appwrite.io/docs/references/cloud/models/columnRelationship) · [issue #8058](https://github.com/appwrite/appwrite/issues/8058) · [CLI onDelete 事故](https://appwrite.io/threads/1387844184005808249) · [3 层深度限制](https://www.reddit.com/r/appwrite/comments/1hxmtk6/help_with_relations_in_appwrite/)
- Supabase/PostgREST：[Resource Embedding](https://docs.postgrest.org/en/v12/references/api/resource_embedding.html) · [RLS footguns](https://www.bytebase.com/blog/postgres-row-level-security-footguns/) · [Row Level Security](https://supabase.com/docs/guides/database/postgres/row-level-security)
- PocketBase：[Working with relations](https://pocketbase.io/docs/working-with-relations/) · [Collections](https://pocketbase.io/docs/collections/)
- Parse：[include 指针嵌套](https://stackoverflow.com/questions/24515784/parse-include-nested-pointers-in-query)
- Hasura：[Create Relationships](https://hasura.io/docs/2.0/schema/postgres/table-relationships/create/) · [Row-level permissions](https://hasura.io/docs/2.0/auth/authorization/permissions/row-level-permissions/)
