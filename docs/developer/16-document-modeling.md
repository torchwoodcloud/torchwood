# 16 文档建模指南：跨集合引用模式

读者：**Agent 构建者 / 提示词作者**，以及用 DocumentDB 建业务模型的后端开发者。

现状约束：DocumentDB 属性类型为 `string/email/url/integer/float/boolean/datetime/json/vector`（`array=true` 可选，元素类型仅限 string/integer/float/boolean/datetime 五种标量），**没有 relationship 属性类型、没有跨集合 JOIN、没有外键**。本章给出在此约束下构造关系模型（1:N、M:N）的规范模式——全部使用现有原语，不需要服务端新特性。

> 相关：`06-databases.md`（查询 / 事务 / 权限 / 事件语义的权威参考）、`14-agent-tools.md`（工具调用形态与契约发现）、`docs/design/documentdb-relationships.md`（relationship 机制蓝图，未排期——本章模式在其落地后依然有效，relationship 属性本质是同一模式的语法糖）。
> 示例约定：gRPC / `InvokeJSON` / `InvokeTool` 用 protojson camelCase（`14-agent-tools.md` §3.1）；REST 路径见本文 §3 与 `14-agent-tools.md` §3.2。

## 0. 选型速查

| 关系形态 | 模式 | 一句话 |
|---|---|---|
| 1:N（分类→文章） | **子集合引用属性**：posts 上建 `category_id`（string, required）+ key 索引 | 父文档 ID 存在子文档上 |
| M:N（文章↔标签） | **数组属性**：posts 上建 `tag_ids`（string, `array=true`）+ key 索引 | 物理落 `TEXT[]` 列；key 索引由服务端自动落 `GIN (col array_ops)` 形态，`containsAny`/`containsAll` 可走索引 |
| M:N 需要关系自身元数据（如 tagged_at）、或单文档关系数超大 | **junction 集合**：`post_tags`（post_id + tag_id + 复合 unique 索引） | 关系升格为文档 |
| 跨集合原子写 | `ExecuteTransactions`（`mode` 缺省即 ATOMIC；op ≤1000；单 database 内） | 单事务多集合，批内事件带同一 `transaction_id` |
| 引用完整性 | **应用层协议**（§4 删除卫生） | 服务端无外键——删除不级联、不拒绝 |

**ID 约定**：引用属性存目标文档的信封字段 `id`。文档 ID 客户端自选（`^[a-zA-Z0-9_.:-]{1,64}$`），不传由服务端生成 UUID。引用属性命名统一 `_id` 后缀（单值 `category_id`、数组 `tag_ids`），Agent 与人类都能望文生义。

## 1. 博客模型：建 schema

目标模型：`categories` 1:N `posts`，`posts` M:N `tags`；`categories.post_count` 是可选的反范式计数列（§2.3 用到）。

**建集合**（`Databases.CreateCollection`；集合 ID `^[a-z_][a-z0-9_]*$` ≤40 小写——集合 ID 同时是物理表名；请求还可带 `permissions` 集合级默认权限与 `documentSecurity` 开关）：

```json
{"databaseId": "blog", "id": "categories", "name": "分类"}
{"databaseId": "blog", "id": "posts", "name": "文章"}
{"databaseId": "blog", "id": "tags", "name": "标签"}
```

**建属性**（`Databases.CreateAttribute`；属性 key `^[a-zA-Z_][a-zA-Z0-9_]*$` ≤63 字节，`_` 前缀系统保留）：

```json
{"databaseId": "blog", "collectionId": "categories", "key": "name",       "type": "string",  "required": true}
{"databaseId": "blog", "collectionId": "categories", "key": "slug",       "type": "string",  "required": true}
{"databaseId": "blog", "collectionId": "categories", "key": "post_count", "type": "integer", "required": false}

{"databaseId": "blog", "collectionId": "posts", "key": "title",       "type": "string", "required": true}
{"databaseId": "blog", "collectionId": "posts", "key": "category_id", "type": "string", "required": true}
{"databaseId": "blog", "collectionId": "posts", "key": "tag_ids",     "type": "string", "required": false, "array": true}

{"databaseId": "blog", "collectionId": "tags", "key": "name", "type": "string", "required": true}
{"databaseId": "blog", "collectionId": "tags", "key": "slug", "type": "string", "required": true}
```

**建索引**（`Databases.CreateIndex`；索引 ID ≤40，与集合 ID 拼成的物理名 `idx_<collectionID>_<索引ID>` 不得超 63 字节）：

```json
{"databaseId": "blog", "collectionId": "posts",      "id": "by_category", "type": "key",    "attributes": ["category_id"]}
{"databaseId": "blog", "collectionId": "posts",      "id": "by_tag",      "type": "key",    "attributes": ["tag_ids"]}
{"databaseId": "blog", "collectionId": "categories", "id": "by_slug",     "type": "unique", "attributes": ["slug"]}
{"databaseId": "blog", "collectionId": "tags",       "id": "by_slug",     "type": "unique", "attributes": ["slug"]}
```

注意：

- **数组属性的索引必须显式创建**：对 `tag_ids` 建普通 key 索引即可，服务端自动渲染为 `GIN (tag_ids array_ops)`（`&&` / `@>` 可走索引）；数组列仅支持单列索引，unique / fulltext 对数组列拒绝。不建索引查询仍合法（查询白名单只看属性声明），只是顺序扫描。
- slug 用 unique 索引，既做唯一性约束，也做按 slug 点查的加速。
- `required` 属性落 NOT NULL 列：写入缺列或显式 null → InvalidArgument。数组列不可设 `default`。
- 属性与索引建好后，`GET /v1/server/databases/{db}/collections/{coll}:exportSchema?as=jsonschema` 导出 JSON Schema 2020-12 文档，用于合成 / 校验文档载荷（`14-agent-tools.md` §2）。

## 2. 写路径

写入通用行为（单文档 API 与事务 op 同源）：载荷数据键非法（`_` 前缀 / 标识符语法不符 / 超 63 字节）**显式 InvalidArgument 拒绝，不静默丢弃**；值类型与属性列不符 → InvalidArgument；载荷总量 ≤1 MiB、单属性值 ≤256 KiB、数组值 ≤1000 元素；用户集合的 Update / Delete **必须带 `version`**（OCC；缺省 → `DOCUMENT.VERSION_REQUIRED`）。

### 2.1 发一篇挂 2 个标签的文章

```json
{"databaseId": "blog", "collectionId": "posts", "documentId": "post_001",
 "data": {"title": "你好世界", "category_id": "cat_tech", "tag_ids": ["tag_go", "tag_grpc"]},
 "requestId": "create-post-001"}
```

### 2.2 给文章追加 / 摘除标签（原子，无读改写竞态）

不要"读出 tag_ids → 客户端拼接 → 整列回写"——`data` 通道对数组列是整列替换。用 `arrayUpdates` 八算子（编译为单语句 SET 子句，与 OCC 兼容；与 `data` 同列冲突 → InvalidArgument）：

```json
{"databaseId": "blog", "collectionId": "posts", "documentId": "post_001", "version": 7,
 "arrayUpdates": {"tag_ids": {"op": "ARRAY_UPDATE_OP_APPEND", "values": ["tag_grpc"]}}}
```

| 算子 | 语义 |
|---|---|
| `APPEND` / `PREPEND` | 尾插 / 头插；NULL 列视为空数组 |
| `REMOVE` / `DIFF` / `FILTER` | 差集移除（三者同构受限形态：移除等于任一 values 的元素；不支持条件表达式；移空后为空数组非 NULL） |
| `UNIQUE` | 保首次出现序去重（忽略 values） |
| `INTERSECT` | 交集（去重并保本列首次出现序；移空后空数组） |
| `INSERT` | 定点插入（`index` 0 基、其后元素顺移；越界 = 尾插；要求 values 恰 1 且 index ≥0） |

values 数量约束：除 `UNIQUE`（忽略）与 `INSERT`（恰 1）外要求 ≥1，上限 1000。NULL 列语义二分：添加类（APPEND / PREPEND / INSERT）视为空数组归一；读改写类（REMOVE / UNIQUE / INTERSECT / DIFF / FILTER）保持 NULL。

### 2.3 跨集合原子操作（换分类 + 同步分类计数）

凡一次业务动作要写多个集合（或一写一删），一律走 `ExecuteTransactions`（REST `POST /v1/server/databases/{db}/documents:execute-tx`；仅 Server 面）：单事务内按请求序执行异构 op 批。ATOMIC（缺省，`TRANSACTION_MODE_UNSPECIFIED` 亦按 ATOMIC）任一失败整批回滚（错误带 op index 定位）；`PARTIAL` 逐 op SAVEPOINT 容错、已成功不回滚、返回 per-op 结果。批内事件序 = op 序，事件信封共享同一 `transaction_id`，订阅端可识别同批：

```json
{"databaseId": "blog", "mode": "TRANSACTION_MODE_ATOMIC", "requestId": "move-post-001",
 "ops": [
  {"type": "TRANSACTION_OP_TYPE_UPDATE", "collectionId": "posts",      "documentId": "post_001",
   "expectedVersion": 7, "data": {"category_id": "cat_arch"}},
  {"type": "TRANSACTION_OP_TYPE_UPDATE", "collectionId": "categories", "documentId": "cat_tech",
   "increment": {"post_count": -1}},
  {"type": "TRANSACTION_OP_TYPE_UPDATE", "collectionId": "categories", "documentId": "cat_arch",
   "increment": {"post_count": 1}}
 ]}
```

要点：

- **op 字段消费不对称**（按 `type`）：`data` / `permissions` → create/update/upsert；`increment` / `arrayUpdates` → 仅 update；`expectedVersion` → 仅 update/delete；`conflictColumns` → 仅 upsert（必须无序命中集合一个 unique 索引）。
- **计数器 op 只写 `increment`**。同一列同时出现在 `data` 与 `increment` 会生成同列双重 SET 赋值，被 PostgreSQL 拒绝（multiple assignments to same column）——增量语义自洽（NULL 列按 0 起步），无需 data 兜底。
- **update op 的 OCC 三态**：设置 `expectedVersion` → CAS；缺省 → 盲写 +1（LWW 契约）；显式 ≤0 → InvalidArgument（`DOCUMENT.VERSION_INVALID`）。**delete op 不支持盲删**：缺省即 `DOCUMENT.VERSION_REQUIRED`，必须显式给版本。
- op 批上限 1000；服务端按 (collection, documentID) 排序预取 advisory 锁防批间死锁，调用方无须自己排序。
- `requestId` 写幂等覆盖整批——重放返回首次完整结果（含 PARTIAL 的 per-op 结果）；同 key 不同请求体 → `IDEMPOTENCY.KEY_CONFLICT`。

## 3. 查询路径

`ListDocuments` 的过滤 / 排序 / 投影 / 分页**一律走 typed AST**（`shared.v1.Query`：`filter` 树 + `orders` + `select` + `pageSize`/`pageToken`，camelCase JSON）。DSL 字符串只是 Go/TS SDK 与 CLI 的客户端糖（解析为 AST 后发送），服务端 wire 上不消费（`queries` 字段已 reserved）。REST 形态：GET 面仅 `page_size`/`page_token` 简单分页（与 `query` 内同名字段不等 → InvalidArgument）；过滤一律 `POST .../documents:list`，**body 是 `shared.v1.Query` 本身**（非整包请求）。

**1:N——分类下的文章**：

```json
{"databaseId": "blog", "collectionId": "posts",
 "query": {"filter": {"eq": {"attribute": "category_id", "values": ["cat_tech"]}},
           "orders": [{"attribute": "_created_at", "desc": true}], "pageSize": 20}}
```

**M:N——含某标签的文章**（数组算子 `containsAny` = 交集非空 `&&`；`containsAll` = 子集 `@>`；NULL 列与空数组列不命中）：

```json
{"filter": {"containsAny": {"attribute": "tag_ids", "values": ["tag_go"]}}}
{"filter": {"containsAll": {"attribute": "tag_ids", "values": ["tag_go", "tag_grpc"]}}}
```

**反向——某文章的所有标签**：读出 `tag_ids` 后批量取回（`$id` 是 `_id` 的查询别名；`$createdAt` / `$updatedAt` / `$version` 同族）：

```json
{"filter": {"in": {"attribute": "$id", "values": ["tag_go", "tag_grpc"]}}}
```

**"按分类 slug 查文章"（跨集合条件的替代）**：服务端无跨集合 JOIN，两段式：

1. `categories` 上 `eq("slug", "tech")` 取得 `cat_tech`；
2. `posts` 上 `eq("category_id", "cat_tech")`。

Agent 应把 slug→ID 的解析结果缓存 / 冗余（或直接在 posts 上冗余 `category_slug` 属性做查询列——见 §5 反范式）。

**计数与聚合**：`CountDocuments`（带过滤走 POST `documents:count`；GET 面仅无过滤计数）；`AggregateDocuments`（POST `documents:aggregate`）支持 `sum/avg/min/max` + 可选单键 `group_by`，聚合目标必须是声明的数值属性（integer/float），且一律在可见行集上执行。两者对排序 / 分页算子显式拒绝（整集语义）。

**分页语义（keyset-only）**：`pageSize` 缺省 50、上限 100（超出 clamp）；无显式排序默认 `_created_at DESC`；排序 = 全部排序键 + `_id` tiebreaker（方向随首键），cursor 只编码 docID、服务端查行取全部键值。首页返回精确 `totalCount`，续页不再计数（`totalCount` ≤0 = unknown）；**是否还有下一页只看 `meta.nextPageToken` 是否为空**。

### 查询陷阱清单（Agent 高频踩坑）

| 陷阱 | 正确做法 |
|---|---|
| `contains` 用于数组成员判断 | **错**。`contains` 编译为 `ILIKE '%…%'` 子串模糊匹配；数组成员判断只用 `containsAny` / `containsAll` |
| `containsAny` / `containsAll` 用于非数组属性 | 服务端白名单拒绝；仅 `array=true` 属性可用 |
| 用关系目标集合的属性排序（如按 category.name 排 posts） | 排序 / 过滤键必须是**本集合**已声明属性或系统列；先把需要的值冗余为本集合属性 |
| offset 分页 | 已移除；只认 keyset `pageToken`（`ka:`/`kb:` token），`offset()` 与旧 offset token 携带即 InvalidArgument |
| 依赖 `totalCount` 判定翻页结束 | keyset 续页不计数（≤0 = unknown）；只看 `nextPageToken` 是否为空 |
| 一次 `in()` 塞上千 ID | 单个比较叶值 ≤1000；整条查询所有 filter 的绑定参数累计 ≤2000（超限 InvalidArgument）；大集合分批 |
| 对含 NULL 的排序键列直接分页 | cursor 行排序键含 NULL → InvalidArgument；数据行含 NULL 键在续页中被跳过——先 `isNull`/`isNotNull` 过滤再分页 |
| `search` 随手用 | `search`（全文匹配）要求目标属性已有单列 fulltext 索引，否则 InvalidArgument |
| 对 vector 属性做普通过滤 / 排序 | vector 属性仅支持 `isNull`/`isNotNull` filter，且不可作排序键；近邻查询走 `vectorSearch`（KNN 一等算子，需匹配 metric 的 hnsw 索引；DSL 字符串不支持） |

## 4. 引用完整性卫生（删除协议）

服务端无外键：删除父文档**不会**级联也不会拒绝，孤儿引用是应用的责任。Agent 构建者应把以下三步协议固化为标准删除流程：

1. **计数检查**（restrict 手工版）：走 `Databases.CountDocuments` 查引用数；非 0 → 先处理子文档或拒绝删除。

   ```json
   {"databaseId": "blog", "collectionId": "posts",
    "query": {"filter": {"eq": {"attribute": "category_id", "values": ["cat_tech"]}}}}
   ```

   （带过滤的计数走 POST `documents:count`；GET 面仅支持无过滤计数。）

2. **处置子文档**（三选一）：
   - **迁移**：批量 update 改 `category_id`。批量用 `BulkUpdateDocuments`（整批单事务、原子成功或整体回滚；`_version` 盲写 +1 的 LWW 语义，无 per-doc CAS），或 `ExecuteTransactions` 的 update op（可逐 op 带 `expectedVersion` CAS）。
   - **级联删除**：确认后 `BulkDeleteDocuments`（整批单事务）或逐个 `DeleteDocument`（每次带当前 `version`），或 `ExecuteTransactions` 的 delete op 批（每个 op 显式版本）。
   - **置空**：`data` 显式写 `{"category_id": null}` + OCC 版本。**仅适用于可选（required=false）引用属性**——required 属性落 NOT NULL 列，写 null → InvalidArgument；此时只剩迁移或级联删除两个选项。

3. **删除父文档**（`DeleteDocument` 带当前 `version`）。

**孤儿巡检**（Functions 定时任务模式）：keyset 分页拉取全量 `categories` 的 `$id`，再分页拉取 `posts` 的 `category_id`（`select` 投影省流量），客户端差集出孤儿，按第 2 步处置。规模大时由写路径保证"删除即清理"，巡检只兜底。

## 5. 反范式与 junction 的取舍

**冗余查询列**：跨集合过滤 / 排序频繁时，把目标集合的值冗余为本集合属性（如 posts 上加 `category_slug`），写路径用 `ExecuteTransactions` 保证两侧一致。这是无 JOIN 环境下的标准姿势——用受控冗余换单集合查询。

**junction 集合**（`post_tags`）仅在以下任一条件成立时升级使用：

- 关系自身需要属性（`tagged_at`、`tagged_by`）；
- 单文档关系数可能超过数组属性上限（单属性值 ≤256 KiB、数组值 ≤1000 元素）；
- 反向过滤（"某标签下的所有文章"）是高频一等查询且不想维护数组镜像。

junction 形态：

```json
{"databaseId": "blog", "collectionId": "post_tags",
 "data": {"post_id": "post_001", "tag_id": "tag_go", "tagged_at": "2026-09-07T12:00:00Z"}}
```

复合 unique 索引防重：`{"id": "by_pair", "type": "unique", "attributes": ["post_id", "tag_id"]}`；建关系用 upsert + `conflictColumns`（`["post_id", "tag_id"]`，必须无序命中该 unique 索引）幂等重放。"某标签的文章" = `eq("tag_id", …)` 查 junction → `in("$id", …)` 取文章；若同时保留 §2.2 的数组镜像，用同一个 `ExecuteTransactions` 维护两侧同步。

## 6. Agent 接入清单

1. **契约发现先行**：`GET /.well-known/torchwood`（查询算子全集含 array-only 标注、域码表 + retryable、databases 面动词与 scope 清单）+ `GET /v1/server/databases/{db}/collections/{coll}:exportSchema?as=jsonschema`（集合 JSON Schema 2020-12，校验文档载荷）——见 `14-agent-tools.md` §2。
2. **查询一律 typed AST**（本文 §3 的 JSON 形态）；DSL 字符串只是 SDK / CLI 客户端糖，服务端不消费。
3. **写入带 `requestId`**（Create / Update / Upsert / Delete / BulkUpdate / BulkDelete / ExecuteTransactions 七类写全覆盖），重放免费获得首次结果（`x-torchwood-replayed: true`）；多集合动作一律 `ExecuteTransactions` ATOMIC。
4. **单文档更新 / 删除带 `version`**（用户集合强制 OCC；冲突 `DOCUMENT.VERSION_CONFLICT` 的错误体携带 `current_version`，直接取值合并重试，不必先 GET 文档）。
5. **删除走 §4 三步协议**，不要直接删被引用的父文档。
6. **权限默认最小**：文档默认私有——空 ACE 种子按主体绑定（端用户 `read/update/delete:user:<自身>`，API key 主体 `…:key:<自身>`），keyB 看不到 keyA 建的文档（Get/List = NotFound 防枚举）；协作需显式授予文档 ACE（模板 `user:{id}` / `group:{id}` 只展开为调用者自身首个匹配角色，无法借模板指名他人）——语义见 `06-databases.md` §7。

## 7. 附：可嵌入 Agent 提示词的建模规约

以下为可直接复制进 Agent system prompt 的紧凑版（与本章等义的规范性压缩）：

> DocumentDB 无外键、无跨集合 JOIN。建模规约：(1) 1:N 在"多"方建 `<父>_id` string 引用属性并建 key 索引；(2) M:N 在主实体建 string 数组属性（array=true）并建 key 索引（服务端自动落 GIN），成员判断只用 containsAny/containsAll，contains 是字符串模糊匹配；(3) 关系需要自身属性、或单文档关系数超数组上限（≤1000 元素）时，建 junction 集合 + 复合 unique 索引；(4) 跨集合写必须用 ExecuteTransactions（ATOMIC）并携带 requestId，计数器列只写 increment（data 与 increment 同列会被拒绝）；(5) 单文档更新/删除必须带 version（OCC；冲突错误携带 current_version，取值合并重试）；事务内 update op 缺省版本为 LWW 盲写、delete op 必须显式版本；(6) 删除被引用文档前先 CountDocuments 检查引用数，再迁移/级联删除子文档（置空仅限可选属性），最后删父文档；(7) 排序键只能是本集合属性，跨集合排序需求用冗余列解决；(8) 按 slug 等外部键查询先解析为 ID 再查引用属性。

## 相关文档

- `06-databases.md` — DocumentDB 权威参考（查询 / 事务 / 权限 / 事件语义）
- `14-agent-tools.md` — 18 动词工具箱与契约发现面
- `docs/design/documentdb-relationships.md` — relationship 机制蓝图（未排期）
