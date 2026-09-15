# 19. Runbook（版本化资源迁移）

> 面向后端开发者与运维。来源：`docs/design/runbook.md`（2026-09-14 定稿，D1–D21 全部拍板）；
> 实现落位：引擎 `internal/pkg/runbook/`（文件层/编排层/动词对账决策层，`Caller` 注入
> RPC 通道），CLI 命令组 `cli/runbook.go`（旗标 + InvokeJSON 适配），状态服务
> `proto/server/v1/runbook.proto` + 迁移 `db/migrations/000009`（状态在服务端）。

## 1 定位与心智

Torchwood 的业务资源（DocumentDB 库/集合/属性/索引、storage bucket、asset def、
leaderboard board）此前只能命令式创建：Console 手工点、CLI 手敲、ad-hoc 脚本。资源的
形状没有版本化事实源——新环境不可复现、资源变更无法审阅、环境间漂移只能事后对账。

Runbook 把这类资源变更写成**有序号、可审阅、可回退的 YAML 文本文件**，由 CLI 引擎按
版本顺序驱动 Server API 执行，应用状态记录在服务端供多机 / CI 共享。心智模型是
**`sql migrate`（golang-migrate）**：命令式版本迁移，不是 Terraform 式声明收敛——一个
step 就是一次变更，历史不可变，配置演进必须写新 step，引擎永不自动 update。

**与 `admin schema repair` 的两层边界**（设计 D19，互补不重叠）：

| 层 | 工具 | 对账对象 |
|---|---|---|
| 意图层漂移 | `torchwood runbook status` / `up` | 声明文件（YAML）vs API 现状 |
| 存储层漂移 | `torchwood admin schema repair`（见 `06-databases.md`） | catalog 元数据 vs 物理表 |

repair 修的是「catalog 与物理表不一致」的存储层损伤；runbook 管的是「我声明过的资源是否
如声明存在」。线上被手工改出漂移时，runbook 会 fail-fast 报字段级 diff，指引写新 step
对齐或手工修复——宁可停也不猜。

**AI/Agent 友好性**：迁移是纯 YAML 声明、命令输出是单个 JSON summary，Agent 可经 CLI
（或 SDK 的 `RunbookService` typed wrapper）驱动整套 up/down/status 流程。

## 2 快速上手

前置：一把带 `runbooks.read` / `runbooks.write` 与相关资源域 scope 的 API key（§7）。
全局旗标（`--api-key` 等）在子命令路径之后、位置参数之前给出（CLI 通用约定）。

```bash
./bin/torchwood runbook new --dir runbooks create_configs_collection   # 生成下一序号骨架（本地命令，免 key）
$EDITOR runbooks/000001_create_configs_collection.yaml                # 编辑 up/down 段
./bin/torchwood runbook status --dir runbooks                          # 本地文件 vs 服务端状态对账
./bin/torchwood runbook up    --dir runbooks --dry-run                 # 先看计划（读路径照跑，零写入，恒退 0）
./bin/torchwood runbook up    --dir runbooks                           # 应用到最新版
./bin/torchwood runbook down  --dir runbooks                           # 回退一步（默认）
```

文件放项目仓库 `runbooks/` 目录（`--dir` 可覆盖），命名 `NNNNNN_name.yaml`（6 位零填充
序号 + name 匹配 `^[a-z0-9_]{1,64}$`，仅认 `.yaml` 后缀；版本链从 1 起、连续、无重号）。
**单文件双段**——up/down 在同一个 git diff 里被审阅，不会「改了 up 忘了 down」：

```yaml
# runbooks/000001_create_configs_collection.yaml
up:
  - create_collection:
      database_id: app
      id: configs            # 字段面与 proto 一一对应：Create 系请求用 id，Get/Update/Delete 系用 collection_id
      name: configs
      document_security: true
      permissions: ['read:users']   # 权限串格式 type:role，write: 自动展开为 create/update/delete
      attributes:
        - { key: key, type: string, size: 64, required: true }
        - { key: value, type: json }
      indexes:
        - { id: key, type: unique, attributes: [key] }
down:
  - delete_collection:
      database_id: app
      collection_id: configs
```

```yaml
# runbooks/000002_create_gold_asset.yaml
up:
  - create_asset_def:
      code: gold
      name: Gold
      class: currency       # string 词表字段按服务端词表小写值（currency/stack/instance/entitlement），非 proto 枚举
      decimals: 2
      max_quantity: 1000000
down:
  - delete_asset_def:
      code: gold          # 服务端语义为归档（soft），引擎复活语义见 §4
```

一个动作 = 恰好一个动词键 + 请求体（0 个或 ≥2 个动词键、体为 `null` 均是加载期错误）。
`down` 段为空 = 该 step **不可逆**，down 到它时会明确报错——空段即语义，没有额外开关。

## 3 安全须知（红字）

> **step 内多动作不可原子回滚。** 资源动作分布在多个 Server RPC 上，无法包成一个事务
> （这是与 sql migrate 的本质差异）。第 3 个动作失败时前 2 个已生效——**失败后重跑
> `runbook up` 即恢复**：动作全部幂等（先读后写 / 相等 skip / 缺者补建），失败 step 不
> 记录版本，重跑从头执行该 step。无 dirty 标记的必要。

> **down 只承诺资源形状回退，不承诺数据恢复。** `delete_collection` 级联硬删全部文档，
> `delete_bucket` 删桶内文件。down 默认单步、先 `--dry-run` 看计划；需要保留数据就
> 手写裁剪过的 down 段（如只删索引不删集合）。

## 4 动词参考（18 个）

动作动词是扁平的、与 Server RPC 一一映射的键，请求体直传 protojson。字段面规则：

- **动作体允许的字段集 = 对应 `*Request` 消息的 proto 字段集**（未知字段 / 缺必填 /
  类型不符在加载期即报错，拼错字段名时优先指向拼错本身）；
- 字段名按 proto 字段原名 **snake_case** 书写（`database_id`、`default_value`；
  protojson 兼容 camelCase，仓库示例与骨架统一 snake_case）；
- **Create 系用 `id`，Get/Update/Delete 系用 `collection_id`**（与 proto 请求消息的
  寻址字段一一对应；`create_index` 的索引 ID 也叫 `id`，`delete_index` 才是 `index_id`）；
- 权限串格式 `type:role`（如 `read:users`、`write:members`）；`update_collection` 的
  `permissions` 是 `PermissionsUpdate` 消息：`permissions: { values: ['read:users'] }`；
- 真枚举字段写 proto 枚举名（禁数字）；**string 词表字段按服务端词表小写值**——asset
  `class` 是 string 非 enum，写 `currency` 而非 `CLASS_CURRENCY`；
- update 动词用 YAML 键存在性表达 proto3 optional presence（写了 = 修改，没写 = 不动；
  键存在但值为 `null` 是解析期错误）；
- `create_collection` 的 `attributes` / `indexes` 是唯一复合字段（内嵌声明去掉
  `database_id`/`collection_id`，引擎注入后拆为逐个 CreateAttribute / CreateIndex）。

### 4.1 动词表

| 动词 | RPC | 引用键 | up 幂等语义要点 |
|---|---|---|---|
| `create_database` | `DatabasesService/CreateDatabase` | `id` | 不存在建 / 相等 skip / 不等 fail 带 diff |
| `delete_database` | `DatabasesService/DeleteDatabase` | `id` | 不存在 skip；NotFound 一律按成功 |
| `create_collection` | `DatabasesService/CreateCollection` | `database_id`+`id` | 复合动词，见 4.3 |
| `update_collection` | `DatabasesService/UpdateCollection` | `database_id`+`collection_id` | 直传执行不比较（update 天然幂等） |
| `delete_collection` | `DatabasesService/DeleteCollection` | `database_id`+`collection_id` | 不存在 skip；**级联硬删文档** |
| `create_attribute` | `DatabasesService/CreateAttribute` | db+coll+`key` | 生命周期参与判定，见 4.2 |
| `delete_attribute` | `DatabasesService/DeleteAttribute` | db+coll+`key` | 软删（deprecated）；已是目标态 skip |
| `restore_attribute` | `DatabasesService/RestoreAttribute` | db+coll+`key` | active skip / retired 报错指引 create |
| `create_index` | `DatabasesService/CreateIndex` | db+coll+`id` | 不存在建 / 相等 skip / 不等 fail |
| `delete_index` | `DatabasesService/DeleteIndex` | db+coll+`index_id` | 不存在 skip |
| `create_bucket` | `StorageService/CreateBucket` | **`name`**（ID 是服务端 UUID） | 不存在建 / 相等 skip / 不等 fail；重名多命中 fail 人工裁决 |
| `update_bucket` | `StorageService/UpdateBucket` | `name` | 引擎按 name 解析 id 注入；`name` 透传为同值自设，**重命名不在本动词表达**（改名走新 step 的 create+delete） |
| `delete_bucket` | `StorageService/DeleteBucket` | `name` | 不存在 skip |
| `create_asset_def` | `AssetsService/CreateAssetDef` | **`code`**（ID 服务端生成） | 不存在建 / 相等 skip / 不等 fail；遇同 code **归档** def 配置相等（除 status）→ 自动复活（见 4.2） |
| `update_asset_def` | `AssetsService/UpdateAssetDef` | `code` | 引擎反查 def_id 注入并剥离 `code`（code/class 不可变） |
| `delete_asset_def` | `AssetsService/DeleteAssetDef` | `code` | 服务端语义为归档软删；已是归档态 = 目标态 skip；holdings / 流水随 def 保留 |
| `create_leaderboard_board` | `LeaderboardsService/CreateLeaderboardBoard` | board `id` | **直传服务端原生幂等 provisioning**：相等 2xx / 不等 AlreadyExists 带服务端字段 diff 透传——唯一「比较在服务端」的资源域，引擎不先读后建 |
| `update_leaderboard_board` | `LeaderboardsService/UpdateLeaderboardBoard` | `board_id` | 直传执行不比较；board 无 Delete RPC，**down 不可逆**（down 段留空） |

### 4.2 生命周期状态与归档语义

**attribute**（`GetCollection` 回读的 `Attribute.status`）：

| 状态 | reconcile 行为 |
|---|---|
| `active` | 存在，走配置比较（相等 skip / 不等 fail） |
| `deprecated` 且声明需要该 key | 配置相等 → `RestoreAttribute` 复活（直接 Create 会撞 key）；不等 → fail |
| `retired`（物理列已删） | 视为不存在，正常 `CreateAttribute` |
| `migrating` | fail 提示「属性迁移进行中，完成后重跑」——中间态不可判定，不猜 |

**asset def**：`asset_defs` 的 `UNIQUE (project_id, code)` 不分 status，归档行占位 code。
`create_asset_def` 遇同 code 归档 def 且配置相等（除 status）→ 自动 `UpdateAssetDef
(status=active)` 复活——归档是逻辑删除，复活是其逆，down(归档)→再 up 的循环因此闭合。

### 4.3 `create_collection` 特例：声明 = 全集

内嵌 `attributes` / `indexes` 视为该集合的**全集**：线上缺的逐个补建（半应用后重入
安全）；线上多出未声明的属性 / 索引 → fail（「声明即全集」防半套配置漂移）。相等比较
按字段白名单归一（created_at 等服务端元数据不参与；permissions 排序归一且 `write:role`
展开为 create/update/delete 三条后比较；hnsw 索引缺省 `distance_metric` 归一为 COSINE）。

## 5 命令参考

```
torchwood runbook up      [--dir runbooks] [--to N] [--dry-run] [--quiet]
torchwood runbook down    [--dir runbooks] [--to N] [--all] [--dry-run] [--quiet]
torchwood runbook status  [--dir runbooks]
torchwood runbook new     [--dir runbooks] <name>
torchwood runbook forgive <version> [--dir runbooks]
```

| 命令 | 行为 | 旗标 |
|---|---|---|
| `up` | 应用到最新 / `--to N`（须 ≥ 当前版本且本地存在；等于当前 = no-op 退 0，CI 幂等）。强制顺序禁跳版 | `--to` `--dry-run` `--quiet` |
| `down` | 默认回退一步；`--to N`（须 < 当前，`--to 0` 全清）；`--all` = `--to 0`；down 段空的版本明确报不可逆 | `--to` `--all` `--dry-run` `--quiet` |
| `status` | 对账报告，恒退 0（目录 / 加载错误按本地错误退 1） | `--dir` |
| `new` | 生成下一序号骨架（序号 = 本地最大 + 1，与服务端状态无关；目录不存在则创建）。**本地命令免 API key** | `--dir` |
| `forgive` | 逃生门（§6）：删除 ≥N 的服务端 step 记录，**不执行任何资源动作** | `--dir` |

**输出契约**：进度行走 **stderr**（`[runbook] applying 000002_create_gold_asset:
create_asset_def(gold) ... ok`，`--quiet` 静默）；**stdout 恒为单个 JSON summary**：

- up：`{runbook, current_version, applied: [{version, name, actions: [{verb, target,
  result}]}], duration_ms, dry_run, would_fail?}`（`result` ∈ created / skipped /
  deleted / restored / updated / planned）；
- down 同构，键名为 `reverted`；
- status：`{runbook, current_version, clean, files: [{version, name, state,
  irreversible}]}`，`state` ∈ applied / pending / modified（checksum 不符）/ orphan
  （服务端有本地无）；
- forgive：`{runbook, requested_version, deleted_versions, current_version}`；
- new：`{file, version}`。

**退出码**（沿用 CLI 全局契约，`commands.ExitCode` 钩子）：`0` 成功（`--dry-run` 一律 0，
失败会在 summary 的 `would_fail` 里如实预报）；`1` 本地错误（文件层 / 目录 / 用法越界）；
`2` = 40x、`3` = 5xx、`4` = 429（RPC 错误按 HTTP 类别）。

**并发与中断**：两台机器同时 up，CAS（`expect_prev_version`）+ 唯一约束保证只有一台
记录版本，输家重拉状态校验 checksum 后自动收敛为 skip（checksum 不等 = 文件分叉，
fatal）。动作间 Ctrl-C → 该 step 未记录 → 重跑即收敛，引擎不做信号补偿。

## 6 checksum 防篡改与 `forgive`

每个文件应用时记录 checksum = `sha256(归一化后文件字节)`（计算前去 UTF-8 BOM +
CRLF→LF——Windows 编辑器 + git autocrlf 下原始字节哈希会跨环境误报；YAML 解析仍用
原始字节）。`up` / `down` 前对账：服务端每个已应用版本必须在本地存在且 checksum 相等，
否则拒绝执行——**改历史 = 环境分叉之源，注释改动也触发**（这是特性不是缺陷）。

建议仓库 `.gitattributes` 加一行（引擎不依赖它，归一化自身保证跨环境等价）：

```gitattributes
runbooks/*.yaml text eol=lf
```

被拦截时的三条出路：

1. `git checkout` 恢复文件（首选——历史就该不动）；
2. **`runbook forgive <version>` 承认变更后重放**：从顶版起逐个删除 ≥N 的服务端记录
   （只动状态记录，**不触碰任何资源**，需 `runbooks.write`，有审计）；随后重跑 `up`，
   动作幂等 skip + 以新 checksum 重新记录。适用场景：确认要接受对已应用文件的修改、
   或 orphan（服务端有记录本地无文件且不打算恢复）；
3. 把变更写成新 step。

`forgive` 只从顶版逐个摘，保证版本链单调；要求该版本本地文件存在（checksum 此刻正是
被质疑的对象，不参与校验）。

## 7 scope 与权限

API key 需同时覆盖**状态面**与动作触碰的**资源域**：

| 面 | scope | 说明 |
|---|---|---|
| 状态读写 | `runbooks.read` + `runbooks.write` | Record/Delete 同时要求 admin/owner 档 key——member 不可触碰迁移历史 |
| databases 域动作 | `databases.read` + `databases.write` | 读写路径都要（reconcile 先读后写）；写动作要求 admin/owner 档 |
| bucket 动作 | `storage.read` + `storage.write` | 引擎经 ListBuckets 按 name 解析 |
| asset def 动作 | `assets.read` + `assets.write` | 引擎经 ListAssetDefs 按 code 反查 |
| board 动作 | `leaderboards.read` + `leaderboards.admin` | 与「能提交分值的 key 不得改榜配置」的分离一致（`18-leaderboards.md` §2） |

全量方法→scope 映射见 `authz-matrix.md`（RunbookService 三方法与各资源域动作均在内）。

## 8 设计边界（初期明确不做）

- function 资源动作（deployments 是 zip 二进制）；文档 / 数据迁移（只管 schema 与配置资源面）；
- 声明式收敛 / drift 自动修复（Terraform 化）；down 自动生成（down 由作者手写，可审阅可裁剪）；
- 多 runbook 并行线（表留 `runbook` 列，恒为 `default`，CLI 不暴露分线参数）；
- env 变量替换 / 模板（同文件异环境产出不同结果，与 checksum 环境无关性冲突）；
- server 端 runner（定时执行 / CI webhook）——状态服务已为此预留，驱动者后续可加；
- users/groups 等 IAM 资源、`update_database`（服务端无此 RPC）。

设计与决策记录（D1–D21）见 `docs/design/runbook.md`。
