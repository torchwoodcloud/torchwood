# 16 文档建模指南：跨集合引用模式

> 读者：**Agent 构建者 / 提示词作者**，以及用 DocumentDB 建业务模型的后端开发者。
> 现状：DocumentDB 属性类型为 `string/email/url/integer/float/boolean/datetime/json/vector`（`array=true` 可选），**暂无 relationship 属性类型与跨集合 JOIN**。本文给出在此约束下构造关系模型（1:N、M:N）的规范模式——它们全部使用现有原语，不需要服务端新特性。
> 相关：`06-databases.md`（查询/事务/事件语义）、`14-agent-tools.md`（工具调用形态与契约发现）、`docs/design/documentdb-relationships.md`（relationship 机制蓝图，未排期——本文模式在其落地后依然有效，relationship 属性本质是同一模式的语法糖）。
> 示例约定：gRPC / `InvokeJSON` / `InvokeTool` 用 protojson camelCase（`14-agent-tools.md` §3.1）；REST 路径见 `14-agent-tools.md` §3.2。

---

## 0. 选型速查

| 关系形态 | 模式 | 一句话 |
|---|---|---|
| 1:N（分类→文章） | **子集合引用属性**：posts 上建 `category_id`（string, required）+ key 索引 | 父文档 ID 存在子文档上 |
| M:N（文章↔标签） | **数组属性**：posts 上建 `tag_ids`（string, `array=true`） | 物理落 `TEXT[]` 列，**GIN 索引自动创建**，无需手工建索引 |
| M:N 需要关系自身元数据（如 tagged_at）、或单文档关系数超大 | **junction 集合**：`post_tags`（post_id + tag_id + 复合 unique 索引） | 关系升格为文档 |
| 跨集合原子写 | `ExecuteTransactions`（mode=ATOMIC） | 单事务多集合，事件带同一 `transaction_id` |
| 引用完整性 | **应用层协议**（§4 删除卫生） | 服务端无外键——删除不级联、不拒绝 |

**ID 约定**：引用属性里存目标文档的 `id`（Document 信封字段，客户端自选 `document_id`）。引用属性命名建议统一 `_id` 后缀（`category_id` / `tag_ids`），Agent 与人类都能望文生义。

---

## 1. 博客模型：建 schema

目标模型：`categories` 1:N `posts`，`posts` M:N `tags`。

**建集合**（`Databases.CreateCollection`，集合 ID `^[a-z_][a-z0-9_]*$` ≤40 小写）：

```json
{"databaseId": "blog", "id": "categories", "name": "分类"}
{"databaseId": "blog", "id": "posts", "name": "文章"}
{"databaseId": "blog", "id": "tags", "name": "标签"}
```

**建属性**（`Databases.CreateAttribute`，`DatabasesService` 同族）：

```json
{"databaseId": "blog", "collectionId": "categories", "key": "name", "type": "string", "required": true}
{"databaseId": "blog", "collectionId": "categories", "key": "slug",  "type": "string", "required": true}

{"databaseId": "blog", "collectionId": "posts", "key": "title",      "type": "string", "required": true}
{"databaseId": "blog", "collectionId": "posts", "key": "category_id","type": "string", "required": true}
{"databaseId": "blog", "collectionId": "posts", "key": "tag_ids",    "type": "string", "required": false, "array": true}

{"databaseId": "blog", "collectionId": "tags", "key": "name", "type": "string", "required": true}
{"databaseId": "blog", "collectionId": "tags", "key": "slug", "type": "string", "required": true}
```

**建索引**（`Databases.CreateIndex`）：

```json
{"databaseId": "blog", "collectionId": "posts",      "id": "by_category", "type": "key",    "attributes": ["category_id"]}
{"databaseId": "blog", "collectionId": "categories", "id": "by_slug",     "type": "unique", "attributes": ["slug"]}
{"databaseId": "blog", "collectionId": "tags",       "id": "by_slug",     "type": "unique", "attributes": ["slug"]}
```

注意：

- `tag_ids` 数组列的 GIN 索引由服务端自动创建，**不要也无法**对数组列手工建 unique（服务端拒绝）。
- slug 用 unique 索引既做唯一性约束，也做按 slug 点查的加速。

## 2. 写路径

### 2.1 发一篇挂 2 个标签的文章

```json
{"databaseId": "blog", "collectionId": "posts", "documentId": "post_001",
 "data": {"title": "你好世界", "category_id": "cat_tech", "tag_ids": ["tag_go", "tag_grpc"]},
 "requestId": "create-post-001"}
```

### 2.2 给文章追加/摘除标签（原子，无读改写竞态）

不要"读出 tag_ids → 客户端拼接 → 整列回写"，用 `array_updates` 八算子（`Databases.UpdateDocumentRequest.array_updates`），编译为单语句 SET，与 OCC 兼容：

```json
{"databaseId": "blog", "collectionId": "posts", "documentId": "post_001", "version": 7,
 "arrayUpdates": {"tag_ids": {"op": "ARRAY_UPDATE_OP_APPEND", "values": ["tag_grpc"]}}}
```

常用算子：`APPEND` / `PREPEND` / `REMOVE`（按值删）/ `UNIQUE`（去重）/ `INTERSECT` / `DIFF`；`INSERT`（0 基 `index`，越界尾插）与 `FILTER` 另见 `06-databases.md`。

### 2.3 跨集合原子操作（换分类 + 同步分类计数）

凡一次业务动作要写多个集合（或一写一删），一律走 `ExecuteTransactions`（ATOMIC：任一 op 失败整批回滚；事件信封带同一 `transaction_id`，订阅端可识别同批）：

```json
{"databaseId": "blog", "mode": "TRANSACTION_MODE_ATOMIC", "requestId": "move-post-001",
 "ops": [
  {"type": "TRANSACTION_OP_TYPE_UPDATE", "collectionId": "posts",      "documentId": "post_001",
   "expectedVersion": 7, "data": {"category_id": "cat_arch"}},
  {"type": "TRANSACTION_OP_TYPE_UPDATE", "collectionId": "categories", "documentId": "cat_tech",
   "data": {"post_count": 41}, "increment": {"post_count": -1}},
  {"type": "TRANSACTION_OP_TYPE_UPDATE", "collectionId": "categories", "documentId": "cat_arch",
   "data": {"post_count": 12}, "increment": {"post_count": 1}}
 ]}
```

要点：每个 op 独立 OCC（update 缺省 `expectedVersion` = 盲写 +1 的 LWW 契约；delete 缺省拒收必须显式给版本）；`requestId` 幂等覆盖整批——重放返回首次完整结果。

## 3. 查询路径

`ListDocuments` 的过滤用 typed AST（`shared.v1.Query`，camelCase JSON）。常用形态：

**1:N——分类下的文章**：

```json
{"databaseId": "blog", "collectionId": "posts",
 "query": {"filter": {"eq": {"attribute": "category_id", "values": ["cat_tech"]}},
           "orders": [{"attribute": "_created_at", "desc": true}], "pageSize": 20}}
```

**M:N——含某标签的文章**（数组算子 `contains_any` = 交集非空；`contains_all` = 子集）：

```json
{"filter": {"containsAny": {"attribute": "tag_ids", "values": ["tag_go"]}}}
{"filter": {"containsAll": {"attribute": "tag_ids", "values": ["tag_go", "tag_grpc"]}}}
```

**反向——某文章的所有标签**：读出 `tag_ids` 后批量取回：

```json
{"filter": {"in": {"attribute": "$id", "values": ["tag_go", "tag_grpc"]}}}
```

**"按分类 slug 查文章"（跨集合条件的替代）**：服务端无跨集合 JOIN，两段式：

1. `categories` 上 `eq("slug", "tech")` 取得 `cat_tech`；
2. `posts` 上 `eq("category_id", "cat_tech")`。

Agent 应把 slug→ID 的解析结果缓存/冗余（或直接在 posts 上冗余 `category_slug` 属性做查询列——见 §5 反范式）。

### 查询陷阱清单（Agent 高频踩坑）

| 陷阱 | 正确做法 |
|---|---|
| `contains` 用于数组成员判断 | **错**。`contains` 编译为 `ILIKE '%…%'` 模糊匹配；数组成员判断只用 `containsAny` / `containsAll` |
| `containsAny`/`containsAll` 用于非数组属性 | 服务端白名单拒绝；仅 `array=true` 属性可用 |
| 用关系目标集合的属性排序（如按 category.name 排 posts） | 排序键必须是**本集合**属性（keyset 分页游标只编码本集合键值）；先把需要的值冗余为本集合属性 |
| offset 分页 | 已移除；只认 `pageToken` keyset（续页以响应 `meta.next_page_token` 是否为空判定，`total_count` 在 keyset 下不可靠） |
| 一次 `in()` 塞上千 ID | 单语句绑定参数累计上限 2000；大集合分批 |

## 4. 引用完整性卫生（删除协议）

服务端无外键：删除父文档**不会**级联也不会拒绝，孤儿引用是应用的责任。Agent 构建者应把以下三步协议固化为标准删除流程：

1. **计数检查**（restrict 手工版）：

   ```json
   {"databaseId": "blog", "collectionId": "posts",
    "query": {"filter": {"eq": {"attribute": "category_id", "values": ["cat_tech"]}}}}
   ```

   走 `Databases.CountDocuments`；非 0 → 先处理子文档或拒绝删除。

2. **处置子文档**（三选一）：迁移（批量 `update` 改 `category_id`，用 `BulkUpdateDocuments`）；级联删除（确认后 `BulkDeleteDocuments`，或逐个 `delete_document`——均整批单事务）；置空（`data` 显式写 `{"category_id": null}` + OCC 版本）。

3. **删除父文档**（带 OCC 版本的 `delete_document`）。

**孤儿巡检**（Functions 定时任务模式）：分页拉取全量 `categories` 的 `$id` 集合，再分页拉取 `posts` 的 `category_id`（`select` 投影），客户端差集出孤儿，按第 2 步处置。规模大时可由写路径保证"删除即清理"，巡检只兜底。

## 5. 反范式与 junction 的取舍

**冗余查询列**：跨集合过滤/排序频繁时，把目标集合的值冗余为本集合属性（如 posts 上加 `category_slug`），写路径用 `ExecuteTransactions` 保证两侧一致。这是无 JOIN 环境下的标准姿势——用受控冗余换单集合查询。

**junction 集合**（`post_tags`）仅在以下任一条件成立时升级使用：

- 关系自身需要属性（`tagged_at`、`tagged_by`）；
- 单文档关系数可能超过数组属性上限（每属性 256KB / 元素数上限，见 `06-databases.md`）；
- 反向过滤（"某标签下的所有文章"）是高频一等查询且不想维护数组镜像。

junction 形态：

```json
{"databaseId": "blog", "collectionId": "post_tags",
 "data": {"post_id": "post_001", "tag_id": "tag_go", "tagged_at": "2026-09-07T12:00:00Z"}}
```

复合 unique 索引防重：`{"id": "by_pair", "type": "unique", "attributes": ["post_id", "tag_id"]}`（upsert + `conflictColumns` 幂等建关系）。"某标签的文章" = `eq("tag_id", …)` 查 junction → `in("$id", …)` 取文章；与 §2.2 的数组维护同用一个 `ExecuteTransactions` 保持两侧同步。

## 6. Agent 接入清单

1. **契约发现先行**：`GET /.well-known/torchwood`（动词/算子/错误码目录）+ `GET /v1/server/databases/{db}/collections/{coll}:exportSchema?as=jsonschema`（集合 JSON Schema，校验文档载荷）——见 `14-agent-tools.md` §2。
2. **查询一律 typed AST**（本文 §3 的 JSON 形态）；DSL 字符串只是 SDK/CLI 糖。
3. **写入带 `requestId`**，重放免费获得首次结果；多集合动作一律 `ExecuteTransactions` ATOMIC。
4. **更新/删除带 `version`**（用户集合强制 OCC；冲突错误码 `DOCUMENT.VERSION_CONFLICT`，重读重试）。
5. **删除走 §4 三步协议**，不要直接删被引用的父文档。
6. **权限默认最小**：文档默认私有（空 ACE = 创建者私有），协作需显式授予文档 ACE——见 `14-agent-tools.md` §B14。

## 7. 附：可嵌入 Agent 提示词的建模规约

以下为可直接复制进 Agent system prompt 的紧凑版（与本文等义的规范性压缩）：

> DocumentDB 无外键与跨集合 JOIN。建模规约：(1) 1:N 在"多"方建 `<父>_id` string 引用属性并建 key 索引；(2) M:N 在主实体建 string 数组属性（array=true，GIN 自动），成员判断只用 containsAny/containsAll，contains 是字符串模糊匹配；(3) 关系需要自身属性时建 junction 集合 + 复合 unique 索引；(4) 跨集合写必须用 ExecuteTransactions(ATOMIC) 并携带 requestId；(5) 更新/删除必须带 version（OCC，冲突则重读重试）；(6) 删除被引用文档前先 CountDocuments 检查引用数，再迁移/级联/置空子文档，最后删父文档；(7) 排序键只能是本集合属性，跨集合排序需求用冗余列解决；(8) 按 slug 等外部键查询先解析为 ID 再查引用属性。
