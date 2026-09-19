# Functions 运行时指定：平台版本化枚举、函数级选择、部署级固化

> 状态：**已批准（2026-09-19 owner 会话拍板：不引入 mise；执行环境指定落
> 「平台版本化枚举 + 函数级选择 + 部署级固化 + engines.node 校验」四层，
> 全部复用既有先例，不引入新机制）**
> 前置结论（同日）：mise（`.mise.toml` / `.tool-versions`）作为部署侧
> 机制被否决——它解决的是开发机工具链切换（shim + PATH），不是容器镜像
> 工具链选择；构建期拉 mise 二进制与 D11（`--ignore-scripts` 恒定、构建
> 期不执行第三方脚本）冲突，版本解析随 mise 自身版本/registry 漂移与
> 「构建是平台确定性操作」不变量冲突，「任意版本用户自选」与「升级 =
> 新 runtime ID、旧 ID 不日落」（OQ3，go1.x 废弃教训）冲突。完整论证见
> Alternatives。
> 现状基线：`docs/developer/08-functions.md` §3；前置设计
> `docs/design/functions-runtimes-and-sources.md`（运行时表与部署源）。

---

## Overview

「指定运行时」拆成四个正交关注点，各给一个机制、各循一个既有先例：

```
┌─────────────────────────────────────────────────────────────────┐
│ ① 枚举：运行时表升格为唯一事实源（Family/BaseImage/Status）       │
│    先例：Appwrite 式 runtime ID + domain 单表                    │
│ ② 选择：函数级声明（Create 必填 + Update 可改）                  │
│    先例：UpdateFunction proto3 optional（池策略 ×5 同款）        │
│ ③ 固化：部署级快照列（deployment.runtime）                       │
│    先例：迁移 000023 source 四列（INSERT 写全、之后不可变）       │
│ ④ 校验：探测 family 对账 + 源内 engines.node 范围校验            │
│    先例：D7 一致性对账 + lockfile 强制的确定性口径                │
└─────────────────────────────────────────────────────────────────┘
```

版本轴语义（Lambda `nodejs20.x` 同款）：**同 runtime ID 内 minor/patch 由
平台滚动升级**（基座镜像 digest bump，用户无感），**major 升级 = 新
runtime ID、旧 ID 不日落**——后者是既有裁决（runtimes-and-sources OQ3），
本稿为其补上缺失的生命周期状态机与补丁滚动纪律。

## Background：三个结构性事实（决定方案形状）

1. **探测层把语言与版本焊死**：`internal/infra/functions/docker.go` 的
   zip 探测把 `index.js` 直接映射为完整 runtime ID `"node-18.0"`，而 D7
   要求探测结果 == `fn.runtime`（dispatcher `prepareBuildContext` 比对）。
   即使用户能选 `node-22`，构建必炸——探测恒返回 `node-18.0`。**多版本的
   第一刀是探测语义重构（family 与版本分离），不是加表项。**
2. **`fn.runtime` 是 create-only**：`UpdateFunctionRequest` 无 runtime
   字段，选错或升级只能新建函数。
3. **基座镜像映射散落两处**：`runtimes.go` 管 ID 清单，
   `runner.DockerfileFor` 的 switch 硬编码 `FROM node:18-alpine`——加一个
   runtime 要同步改两处。
4. **存量 node runtime 已 EOL（现实紧迫性）**：Node 18 于 2025-04-30
   EOL，当前运行时表 node 侧唯一选项已停止安全补丁一年以上。本稿落地
   后 node-18.0 保留可跑但标 eol，新建函数默认最新 active LTS。

## Goals & Non-Goals

**Goals**

- 多版本 node 运行时：`node-22.0`、`node-24.0` 表项上线，`node-18.0`
  标 eol（可跑、拒新建）；
- 函数创建与更新均可指定 runtime（含迁出 eol 的路径）；
- 探测语义重构：探测产出语言族（family），版本轴来自声明；
- 部署行固化 runtime 快照（补构建/审计完全忠实）；
- 源内 `package.json` `engines.node` 与所选 runtime 的 major 范围校验；
- 生命周期状态机（active/deprecated/eol）随 `ListRuntimes` 投影。

**Non-Goals**

- mise/asdf/nvmrc 等开发机工具链文件作为部署侧输入（前置结论，不重开；
  `torchwood functions dev` 本地开发路径未来可单独尊重本机工具链——那是
  开发者自己的机器，CLI 侧事项，不属本稿）；
- per-deployment 的 runtime 声明入口（CreateDeploymentRequest 不加
  runtime 字段——选择权在函数级，部署级只做快照固化，见 D4）；
- python / 其他语言新 runtime（python runner 落地时按本稿机制直接入表）；
- 基座镜像自动化 patch 滚动的基础设施（renovate 配置等，ops 事项）；
- Console UI 的 runtime 下拉/徽章（API/CLI 已可用，Console 随后续切片）。

## Proposed Design

### 1. 运行时表升格：domain 单表唯一事实源（①）

`internal/domain/functions/runtime.go` 的 `RuntimeInfo` 扩展为完整运行时
记录，表定义与查询helper收敛于此（app / runner / dispatcher 三方只读，
单一事实源杜绝 ID↔BaseImage 两处漂移）：

```go
type RuntimeInfo struct {
    ID         string     // node-24.0 / go-1.26 / image
    Name       string     // 展示名
    Entrypoint string     // MVP 占位（既有）
    Family     string     // node | go | image | python——探测对账轴（D7 修订）
    BaseImage  string     // node: 渲染 FROM；go: 多阶段构建段（运行段 alpine 为模板内部细节）
    Status     string     // active | deprecated | eol
    EolAt      *time.Time // eol/deprecated 附加：上游 EOL 日期（Node 官方时间表）
    IsDefault  bool       // 新建函数缺省 runtime（家族内首个 active）
}
```

- 表项（首个版本）：`node-24.0`（active，default）、`node-22.0`
  （active）、`node-18.0`（**eol**，EolAt=2025-04-30）、`go-1.26`
  （active）、`image`（active）。表序 = 展示序（新→旧）；
- ID 语法沿既有 `<lang>-<major>.0`：`.0` 是平台管理的 minor/patch 滚动
  槽位——**同 ID 内 patch 升级 = 换基座镜像 digest，ID 不变、用户无感、
  模板结构不变不 bump `RunnerTemplateVersion`**（patch 滚动正交于 runner
  协议，且正要它对重建静默生效）；go 因自身版本形态即 `1.26`，ID 无槽位
  后缀，下一个是 `go-1.27`；
- 完整性护栏测试：每个非 python 表项必须能渲染出 Dockerfile、每个
  active/deprecated 表项必须有 BaseImage、有且仅有一个 `IsDefault` 的
  node 表项——防「表里有 ID、模板没分支」的半截状态；
- 新增 runtime 的全部改动 = domain 表加一行 + 一条 golden 测试。

### 2. 探测语义重构：family 与版本分离（④a）

- zip 探测（`infrafunctions.SourceContents`）字段 `Runtime` → `Family`，
  取值 `node | go | python`（语言族标记，不再携带版本）；探测优先级
  `index.js > go.mod > main.py` 不变，混装不报错不变；
- **D7 修订**：对账从「探测 ID == fn.runtime」改为
  「探测 family == 声明 runtime 的 family」（family 经 domain 表解析；
  声明 ID 不在表中 = InvalidArgument fail-closed）。校验意图不变（别把
  go 源码部署进 node 函数），版本轴不再由文件探测决定——源码文件本来就
  感知不了平台 runtime 表；
- `runner.SourceContents.Runtime` 语义改为「**声明**的 runtime ID（已
  通过与探测 family 的对账）」，`DockerfileFor` 据此查表渲染：
  family=node 分支 `FROM <BaseImage>`（代装依赖分层结构不变），
  family=go 分支 `FROM <BaseImage> AS build`（运行段 alpine + CA 证书 +
  非 root 为模板内部细节不变）；
- 兼容缺口：`BuildRequest.Runtime` 为空的遗留调用方（跳过对账的分支，
  五期引入、仅 dogfood 数日窗口）按「探测 family 的首个 active 表项」
  解析渲染基准——即平台缺省 runtime，不再隐含 node:18。

### 3. 函数级选择：Create + Update（②）

- Create 路径现状已必填 runtime（`runtimeExists` 校验），新增**状态门**：
  `eol` 的 runtime 拒绝（错误文案列出可选 active runtime）；
  `deprecated` 允许（过渡态，Console/CLI 警告由投影字段驱动）；
- `UpdateFunctionRequest` 增 `optional string runtime = 16`（proto3
  optional，未设置 = 不修改——池策略 ×5 同款惯例）。语义：**只影响后续
  新 deployment 的构建**，存量 ready deployment 的镜像已构建完毕不受
  影响；升级流程 = 改 runtime → 建新 deployment，回滚 = 改回 → 重部署
  旧源。校验同 Create（存在性 + 状态门）；从 eol 迁出到新版本是合法且
  期望的操作路径；
- 诚实弱点（S3 落地后消除）：runtime 变更与部署构建之间存在窗口，worker
  补构建若在窗口后重读 `fn.runtime` 会以新版本重建旧 deployment——S3 的
  deployment.runtime 快照列落地后补构建改读快照，弱点消除；
- repo 侧：`UpdateFunction` 列白名单登记 `runtime`（bun 更新写规范：新
  可变列显式登记，漏登记 = 改不动）。

### 4. 部署级固化：`function_deployments.runtime` 快照列（③）

- 迁移 000025（projectschema，模式 = 000023 source 列先例）：
  `ADD COLUMN runtime TEXT NOT NULL DEFAULT ''` + 存量行回填
  （`UPDATE ... SET runtime = f.runtime FROM functions f`）；
- **INSERT 期写全、之后不可变**：不登记进 `UpdateDeployment` 列白名单
  （不可变列，漏登记正是期望行为）；三个部署源分支同写（zip/git =
  `fn.Runtime`，image = `image`）；
- `buildSpec` 的 `BuildSpec.Runtime` 改读 `dep.Runtime`（快照忠实：补
  构建/审计与首次构建永远同一 runtime；防御性回退 fn.Runtime 仅剩理论
  意义）；proto `Deployment` 只读投影增 `runtime` 字段。

### 5. 源内声明：`engines.node` 校验（④b）

mise 思路保留的内核是「代码声明它需要的运行时」，该声明在 Node 生态的
正规位置是 `package.json` 的 `engines.node`（npm 自身消费它）——不发明
新文件、不读 `.nvmrc`/`.mise.toml`（每多一个声明位就多一分「哪个文件
说了算」的歧义；文档一句话指明 engines 是唯一校验位）。

- 探测层扩展：根 `package.json` 读取（4MiB 上限沿用既有防护）顺带提取
  `engines.node` 原始串入 `SourceContents`；
- 对账点：dispatcher 构建期（与 D7 family 对账同处，顺序 = family 对账
  → engines 校验 → 模板渲染）——node family 且 `engines.node` 非空时，
  声明 runtime 的 major 不在 range 内 → InvalidArgument，错误文案列出
  可选 runtime 表；
- range 匹配实现为零依赖 major 粒度判定（npm 语义子集：`||` 或组、
  比较 `= != >= > <= <`、`^ ~`、x-range（`22` / `22.x` / `*`）、连字符
  range；prerelease 剥离忽略）。语义 = 「range 与 major M 的版本空间
  相交即放行」，宽松范围（`>=18`）与 node-24 相交放行、`>=20 <23` 对
  node-24 拒绝；
- 无 `engines` / 无 `package.json` 的函数零变化；image 源与 go family
  不消费该字段。

### 6. 生命周期状态机（「不日落」的负责任版本）

```
active ──(上游 EOL 公告/平台决定)──▶ deprecated ──(EolAt 到点/平台决定)──▶ eol
  新建✓ 新部署✓                      新建✓(警告) 新部署✓                新建✗ 新部署✗
                                     存量 ready 继续运行                 存量 ready 继续运行
                                                                        （补构建失败才被迫迁移）
```

- **执行面永不因状态停机**：ready deployment 的镜像已在本地/registry，
  eol 只关闭「新的构建」入口（CreateFunction/UpdateFunction/新
  CreateDeployment 的 app 层校验；dispatcher 内部补构建不设状态门——
  平台内部路径，快照 runtime 是什么就构建什么）；
- 状态随 `ListRuntimes` 投影（proto `RuntimeInfo` 增 `family` / `status`
  / `is_default` / `eol_at`），Console 徽章与 CLI 提示的数据源；
- 该状态机把「旧 ID 不日落」从哲学变为可操作收敛：防止 go1.x 式僵尸
  runtime——用户永不迁移、平台永不敢下线基础镜像的僵局。

## Security & 语义声明

1. **构建安全档位零变化**：engines 校验是纯读取 + 比较，构建路径不新增
   任何第三方工具/脚本执行（对照 mise 案的供应链面扩张）；
2. **eol 不等于下电**：执行面与存量 deployment 不受状态门影响，只有新
   构建入口关闭——不存在「升级平台导致存量函数停摆」的暴露面；
3. **engines 是声明校验不是沙箱**：它与「构建期不执行用户代码」正交，
   唯一目的是把「本地 node 22、线上 node 18」类的漂移在部署期显式化，
   不承担安全语义。

## Observability

- `torchwood_functions_builds_total{runtime,source,result}`（既有）的
  runtime 维度自动携带新表项——版本分布与 eol 迁移进度可直接观测；
- 无新增指标（状态门拒绝走标准 InvalidArgument 计数路径）。

## Rollout Plan / 阶段与切片

| 切片 | 内容 | proto/DB | 依赖 |
|---|---|---|---|
| S1 | domain 表升格 + 探测 family 化 + `DockerfileFor` 表驱动 + node-22/24 上线 + node-18 标 eol + ListRuntimes 投影 + 完整性/golden 测试 | RuntimeInfo 五字段 | 无 |
| S2 | `UpdateFunction.runtime`（optional ×1）+ app 状态门（Create/Update/新部署）+ repo 白名单 + CLI `--runtime` | UpdateFunctionRequest 字段 16 | S1 |
| S3 | 迁移 000025 + bun model/映射 + BuildSpec 换源 + `engines.node` 校验 + Deployment 投影 | 迁移 000025、Deployment.runtime | S1 |

每片独立可交付。proto 变更走 `mise run generate:proto`；`08-functions.md`
§3 随 S1–S3 合并增补（运行时表、选择与升级路径、engines 校验、生命周期）。

## Key Decisions

| # | 裁决 | 理由 |
|---|---|---|
| D1 | 不引入 mise；声明位 = 平台 runtime ID 枚举 + 源内 `engines.node` | 构建确定性/供应链面/支持矩阵三重冲突；engines 是 npm 生态正规声明位 |
| D2 | 探测产出 family，版本轴来自声明（D7 修订为 family 对账） | 探测恒返回 node-18.0 是多版本的结构性阻塞；源码感知不了平台表 |
| D3 | runtime 表 = domain 单表唯一事实源（含 Family/BaseImage/Status） | 消灭 runtimes.go ↔ DockerfileFor 两处漂移；新增 runtime = 一行 + 一测试 |
| D4 | 选择权在函数级（UpdateFunction），部署级只做快照固化；不设 per-deployment 声明入口 | 部署间散落版本破坏心智；快照列保补构建/审计忠实（000023 先例） |
| D5 | 同 ID 内 minor/patch 平台滚动（digest bump），major = 新 ID | Lambda `nodejs20.x` 同款；`.0` 后缀由此获得语义 |
| D6 | 生命周期 active/deprecated/eol；eol 只关新构建入口，执行面不停机 | 「不日落」的可操作收敛；无存量停摆暴露面 |
| D7' | engines 校验收敛在构建期（与 D7 family 对账同处） | 与既有探测/对账链同一位置，错误形态统一 InvalidArgument |
| D8 | 遗留空 Runtime 调用方按 family 首个 active 表项渲染 | 消灭「隐含 node:18」的历史耦合；窗口仅 dogfood 数日 |

## Alternatives Considered

- **mise/`.mise.toml` 作为部署侧输入（本稿前置议题，否决）**：①层次错位
  ——容器构建的工具链声明就是基座镜像（`FROM`），mise 的 shim/PATH 模型
  是开发机形态；②供应链——构建容器需拉 mise 二进制 + 插件解析，与 D11
  「构建期不执行第三方脚本」刚收口的档位冲突；③确定性——版本解析随
  mise 版本与 registry 漂移，`node = "22"` 不是 digest，打破「构建是平台
  确定性操作」；④支持矩阵——「任意版本自选」重开 go1.x 废弃教训的伤口
  且形态更糟（EOL 版本被静默钉住，CVE 补丁责任归属不清）；⑤runtime ID
  参与池语义（template_version 门控），自由格式版本松动该语义。
- **仅 BYO 镜像承担版本选择（否决）**：要求用户持有 registry/CI 才能选
  版本，把平台构建主通道降级为二等公民——版本选择是 zip/git 主通道的
  一等需求。
- **runtime 声明进 CreateDeployment（否决，D4）**：函数内部署间版本散
  落；升级/回滚语义变成「部署参数」而非「函数属性」，Console/CLI/审计
  三面心智成本高于收益。
- **读 `.nvmrc`/`.node-version`（否决）**：与 engines 双声明位歧义；
  `.nvmrc` 是开发工具惯例，engines 是包管理正规契约（npm 自己消费）。
- **config/yaml 驱动运行时表（否决）**：运营者自配基座镜像会绕过模板
  兼容性与探测对账的成套保证；静态表 + 评审 PR + golden 测试是该平台
  规模下的正确重量。

## References

- 代码锚点：`internal/domain/functions/runtime.go`（运行时表）、
  `internal/app/functions/runtimes.go`（表投影与校验）、
  `internal/infra/functions/docker.go`（zip 探测）、
  `internal/infra/functions/runner/runner.go`（`DockerfileFor`）、
  `dispatcher/daemon.go` `prepareBuildContext`（对账与渲染编排）、
  `internal/app/functions/deployments.go`（`buildSpec`）、
  `internal/infra/bun/bunrepo/function_repo.go`（列白名单）
- 前置设计：`docs/design/functions-runtimes-and-sources.md`（运行时表与
  D7/OQ3 裁决）、`docs/design/functions-v3.md`（D11 构建安全档位）
- 现状文档：`docs/developer/08-functions.md` §3
- 业界对照：AWS Lambda runtime 版本化与 go1.x 废弃（OQ3 论据）、
  Cloud Run 容器即接口（BYO 侧）、Appwrite runtime ID 形态（`node-18.0`
  同构）
