# Functions 运行时扩展与部署源多元化（Go / Git / 镜像）

> 状态：**已拍板（2026-09-17 owner）→ 同日复查修正 → 独立设计交叉验证
> 修订 → 二轮深度复查修正 → 对抗审查（4 处修复并入）→ 竞品调研
> （业界对照，零推翻 + 2 处补强）→ 多机演进立项（owner 裁决路径 1
> 细胞模型，排为阶段四，§4）→ 三子代理独立复核修订（13 处并入）→
> **一期已实现**（2026-09-17，四阶段还原点 b760cb1 / 53a3ff1 / 462470b /
> af9dbfb，全量回归绿、集成 9 用例实跑绿）→ **D2 再裁决**（owner 否决
> server 进程内 fetch，定向独立 functions-packer 服务，§2 改写）→
> **二期已实现**（2026-09-17，四阶段还原点 faf0de5 / f2c23d5 / 7fa08ff /
> 450089b；真实 DB 迁移/往返全绿、docker 集成 11 用例容器内实跑全绿、
> 带 DSN 全量回归 78 包零失败）→ **三期已实现**（2026-09-17，四阶段
> 还原点 e21f20a / 2e7ead5 / 05229dc / c1ad0c9；docker 集成 14 用例
> 容器内实跑全绿、带 DSN 全量回归 79 包零失败；Console 三入口 + 源
> 徽章同批交付；实施修正一处：registry host 防护为名称级校验——
> daemon pull 拨号点 guard 不可实施，残余面随 egress 原语后置，§3
> 诚实口径）**。
> Key Decisions 与 Open Questions 记录保留（D2/D6 按 packer 案修订）；
> 交叉验证推翻项已全部收口：D3 一期落地验证、D2 owner 再裁决、OQ5 随
> packer 案成立；后续各轮记录见文末各节。
> 现状基线：`docs/developer/08-functions.md`（§3.1-§3.6 已含 Go 运行时 /
> Runner 协议 / 验证 spawn 质量门 / git 部署源 / packer 运维 / 镜像源）。
> **v3 裁决边界修正（独立复核 A7）**：functions-v3.md Non-Goals 点名的
> 「多机分发」经 owner 2026-09-17 裁决**显式推翻**（阶段四细胞模型）；
> v3 其余 Non-Goals（WASM/isolate/Firecracker/K8s/每实例多进程等）
> 本稿维持不重开。
>
> 一期实施裁决补记：① OQ3 定稿 **go-1.26**（golang:1.26-alpine /
> alpine:3.22，与本仓库 toolchain 主版本对齐）；② verify_build 为全局
> 开关（未按 runtime 分档强制——部署方级质量门，简化可接受）；
> ③ worker 补构建 5m 预算收敛进 buildDeployment 的 build_timeout（同源
> 默认，config 调大同享）；④ Go fetch 身份通道按 §0 设计落地（x-tw-*
> 注入还原 Request，信息等价 node fetch env）。

## Overview

三个需求——**Go 运行时**、**Git 仓库源**、**Docker 镜像源**——切在两条正交轴上，
但部署源轴（git + image）共享同一套模型，因此**统一设计、分三期实施**：

| 需求 | 切入层 | 一句话 |
|---|---|---|
| Go 运行时 | 语言/模板层（探测 + `DockerfileFor` + 平台生成 bootstrap） | 编译型语言下平台改用「生成引用用户包的 main」保持 runner 注入模型 |
| Git 仓库源 | 源获取层（zip 之前的打包） | 独立 functions-packer 服务物化为 zip 落既有路径，业务服务零 git 流量 |
| Docker 镜像源 | 构建层整层跳过 | 平台不构建，pull + digest 钉死 + retag + 契约验证 |

**共享的横切决策**（统一设计的理由）：

1. **部署源模型**：`CreateDeploymentRequest` 的 source 形状一次定型
   （zip / git / image 三变体 oneof），避免三期各改一次 proto/DB；
2. **函数运行协议公开化**：`:18080` runner 契约从「平台内部实现」升格为
   **公开契约**（文档化 + 平台 Go bootstrap 源码即参考实现 + 契约基础镜像），
   Go 函数与 BYO 镜像是同一契约的两类消费方；
3. **可复现性标识统一**：deployment 记录源快照（zip sha256 / git commit SHA
   + 物化 zip sha256 / image digest），审计与重建共用；
4. **凭证面**：git token 与 registry pull secret 同一分期（MVP 一次性内联
   不落库 → 远期随 VCS 集成做托管凭证，对齐 roadmap §4.3）。

依赖关系：镜像期依赖协议契约文档化（与 Go 期同交付）；Git 期独立。

## Background & Motivation

现状（三条收敛链，新语言需同时改三处）：

- 运行时静态表 `internal/app/functions/runtimes.go:9`：仅 `node-18.0`；
- zip 内容探测 `internal/infra/functions/docker.go`（`ExtractZip`）：
  根目录 `index.js` → node-18.0、`main.py` → python-3.11（仅用于报错指认）；
- 模板裁决 `internal/infra/functions/runner/runner.go:55`（`DockerfileFor`）：
  node 分支可用，python 明确报错。

部署链：`CreateDeployment`（zip bytes ≤1MiB gRPC / ≤50MiB multipart，proto
`functions.proto:380`）→ 落库 pending → 同步构建 → `DispatcherExecutor.Build`
（zip base64 内联，`functionsdispatcher/types.go:84`）→ dispatcher
`BuildImage`（`daemon.go:344`：解压 → 写 `.tw-runner.js` + Dockerfile → tar →
`docker build`，tag = `func-<fid>-<did>`）→ ready。执行 = 常驻实例池 spawn，
runner 监听 `:18080`，`GET /_tw/health` 握手就绪（`runner.js:317`）；池与
分发协议对 runtime 零消费——语言无关，这是 Go 与 BYO 镜像低成本落地的关键。

**「runner 注入」模型对编译型语言的适配**：node runner 靠构建期 COPY 一个
`.tw-runner.js` 进去 require 用户模块——用户零平台依赖。Go 用户代码要编译，
平台无法注入解释器，但可以**生成一个引用用户包的 main 包**写进构建上下文
（同 module 内），保持「用户零平台依赖、CMD = 平台产物」的对称体验。

**源模型是单通道的**：proto/DB/领域模型均无 `git_url` / `image` 类字段。
Git 集成仅存在于 roadmap §4.3 VCS（P3，GitHub OAuth + webhook 自动部署）；
镜像源无任何预留。

## Goals & Non-Goals

**Goals**

- Go 运行时（zip 源）：`go-1.25` 表项 + go.mod 探测 + 多阶段构建模板 +
  平台生成 bootstrap（AST 探测 Fetch/Main 双轨入口）+ 部署后验证 spawn；
- Git 源：`https://` 仓库 @ ref + 子目录，**独立 functions-packer 服务**
  打包为 zip（业务服务零 git 流量，资源尖峰隔离在可牺牲的基础设施
  进程），commit SHA 钉死，重建不依赖凭证（物化 zip 在盘）；
- 镜像源：契约镜像引用 → pull + digest 钉死 + retag 进平台命名 + 强制
  契约验证，runtime 专用 ID `image`。

**Non-Goals**（沿 v3 裁决，不重开）

- WASM / isolate / Firecracker / K8s 底座、每实例多进程（v3「明确不做」）；
- python 运行时（独立事项）；
- 存储型凭证与 webhook 自动部署（P3，随 roadmap §4.3 VCS 集成）；
- 镜像漏洞扫描（留挂点不实现）、registry 白名单与构建出网限制（沿
  v3 OQ5 后置口径）；
- Go 函数 per-request stdout 分桶（封套 stdout/stderr 恒空，日志走容器
  stdout / docker logs，文档明示）；
- SSH git 协议、submodule、LFS；`--watch` 自动重部署仅限 zip 源语义
  （git/image watch 属自动部署范畴）；
- 不做独立函数 SDK 包（生成路线已覆盖入口契约；远期若需类型化 DX，
  可在生成契约之上加 SDK 糖，不破坏本设计）；
- **不做 K8s/overlay 式完整集群（路径 2）**：四期细胞模型（§4）已同时
  解决可用性与吞吐，不引入 overlay 网络、跨机容器寻址、任意路由的
  集群复杂度——「函数容器永不跨机」是四期不变的细胞不变量；
- **zip 快照迁对象存储不在四期最小范围**（M5 裁决：构建亲和 + 节点死
  标 failed 重部署的最小改动；对象存储仅在该语义损失被实测为真实
  痛点时作为四期二步启动）。

## Proposed Design

### 0. 统一模型：部署源与运行协议（横切，随二期落 schema）

**源归一化**——部署源三变体，git 由独立 functions-packer 服务归一为
zip，image 跳过构建：

```
DeploymentSource:
  zip    (inline bytes)                    → 构建路径（既有，零改动）
  git    (url, ref, dir, token)            → packer 打包 zip → 既有构建路径
  image  (reference, digest)               → 免构建路径：pull→digest 钉死→retag→verify
```

- packer 打包的 zip 落**既有 `zipPath`**（共享盘），worker 补构建、模板
  版本语义自动成立（这是 packer 输出对齐 zip 通道的核心论据）；
  **「零改动」的强表述经独立复核证伪并降级为「构建路径复用」**——两处
  显式改造：dispatcher 解压预算按源注入（条目 5000，§2）、构建失败
  zip 保留分流（git 保留/zip 删除，§2）；image 源不产生 zip，worker
  补构建走幂等 ImportImage（§3）。
- 镜像源 pull 后 **retag 成 `ImageName(fid, did)` 并删除原始引用**（防本
  地 daemon 残留）——「镜像名 = 平台命名」不变式保持，池 spawn /
  `RemoveImage` 零改动。

**proto 形状**（二期一次性落全，三期只实现 image 分支）：

```proto
message CreateDeploymentRequest {
  string function_id = 1;
  oneof source {
    bytes code = 2;        // zip（原字段移入 oneof；wire 兼容，无存量用户）
    GitSource git = 3;
    ImageSource image = 4;
  }
}
message GitSource {
  string url = 1;        // protovalidate：https 前缀 + max_len 2048
  string ref = 2;        // branch/tag/commit；缺省 HEAD
  string directory = 3;  // 仓库内子目录 = 构建上下文根；缺省根目录
  string username = 4;   // 可选 Basic 凭证（PAT）；仅本次请求内存，不落库不回显
  string token = 5;
}
message ImageSource {
  string image = 1;      // host/repo[:tag|@digest]
  string registry_username = 2;  // 可选；一次性转发 dispatcher 拉取
  string registry_token = 3;
}
```

`Deployment`（只读投影）增：`source_type = 8`、`source_url = 9`（git url /
image 原始引用）、`source_ref = 10`（git 钉死 commit SHA / image digest）、
`source_dir = 11`（git 子目录）。**凭证字段不出现**在任何响应面。

**DB 迁移**（`function_deployments`，projectschema 项目数据面迁移，编号以
实施时序列为准、下文 000023 为示意）：增列 `source_type TEXT NOT
NULL DEFAULT 'zip' CHECK (IN ('zip','git','image'))`、`source_url/source_ref/
source_dir TEXT NOT NULL DEFAULT ''`、`context_sha256 TEXT NOT NULL DEFAULT
''`（zip/git = 物化 zip sha256——现状 zip 连 checksum 都没有，顺手补上）。
存量行回填 `zip`。source 列 INSERT 期写全、之后不可变：**不登记进
`UpdateDeployment` 列白名单**（对齐 update_guard 护栏约定——不可变列，
漏登记正是期望行为）。

**multipart 通道不变**：仅承载 zip；git/image 载荷极小走 gRPC JSON。

**源/运行时互斥**：`image` runtime 的函数只收 image 源；node/go 函数只收
zip/git 源（app 层校验）。**探测结果必须与 `fn.runtime` 一致**（现状探测
结果静默覆盖函数声明是隐患；不一致 → 构建期 InvalidArgument，收严无存量
负担）。

**函数运行协议公开化**：从 `runner.js` 提炼 `docs/developer/08-functions.md`
新节「Runner 协议（契约镜像规范）」：`:18080`（`TW_RUNNER_PORT`）、
`GET /_tw/health`、`POST /` main/fetch 双轨与封套、`x-tw-*` 分发 header 族、
`TW_MAX_REQUESTS` 自回收 / SIGTERM drain / `TW_DRAIN_TIMEOUT_MS`。协议版本
随 `RunnerTemplateVersion`（共享单一常量，Go 首发即 5，不 bump——新增模板
分支不改存量语义，`MinConcurrencyTemplateVersion=3` 降级判定对 Go 天然
放行）。**双实现同版本纪律（第二轮复查补充）**：v5 契约自此有 node
runner.js 与 Go bootstrap 两份平台实现，任一实现的语义变更都必须同步
另一实现并 bump 同一常量——单一版本号管双实现，禁止「node 到 v6 而 Go
停在 v5」的漂移。**公开协议演进宪法（独立复核补充，D5 的约束缺口）**：
契约公开后消费方分平台可控（node runner / Go bootstrap，受同版本纪律
约束）与平台不可控（用户 BYO 契约镜像）两类，`RunnerTemplateVersion`
的「重建即升级」对后者无强制力——因此协议演进必须遵守：**只加不改不删、
未知 `x-tw-*` header 镜像侧必须忽略、封套新字段必须可选、分发侧
（dispatcher）对响应封套的解析必须防御式（未知/缺失字段不致命）**。
这是 §0 协议文档节的必写内容，不是实现巧合。平台 Go bootstrap
源码 + 契约基础镜像（§3）即公开参考实现。

**顺手修复（交叉验证 3/3 收敛发现；独立复核强化论据）**：
`DispatcherExecutor.Build` 现状只发 fid/did/zip 三键，而 `handleBuild` 的
drain 分支以 `FunctionTimeoutSeconds > 0` 为门槛且该值恒为 0——**部署后
旧池 drain 现状完全不触发**（旧实例仅靠 idle TTL 自然淘汰，比「全池扫描
兜底」更弱）。Build 载荷补齐后 drain 才真正生效。

**构建链载荷与接口定稿（独立复核 B1-B4/M4 收口，一期第一个 PR 的依据）**：

domain 端口（`internal/domain/functions/executor.go`）：

```go
type BuildSpec struct {
    ProjectID, FunctionID, DeploymentID string
    ZipPath   string // zip 源：本地 zip 路径（server/worker 共享盘）
    Runtime   string // fn.runtime 原值——D7 一致性校验的比对基准
    FunctionTimeoutSeconds int64 // 池 drain 宽限
    Env       map[string]string  // 验证 spawn 携带的函数 variables
    EgressUntrusted bool         // 验证实例选网（internal 变体）
    Verify    bool               // config verify_build 的解析值
}
// Build 签名：Build(ctx, spec BuildSpec) error——四期纪律①以此为基线，
// 多机路由/广播演化收敛在适配器内部，不再改此签名。
```

dispatcher 内网 API（`BuildRequest` 一期定稿字段，现状 5 字段之上新增）：
`project_id`、`runtime`（daemon 在 `DockerfileFor` 前比对探测结果，不一致
InvalidArgument——D7 的实施位置裁决）、`function_timeout_seconds`、
`env`（仅验证 spawn 消费）、`egress_untrusted`、`verify`。

`Daemon.BuildImage` 形态：`BuildImage(ctx, opts BuildImageOptions)`（zip +
上列字段），`handleBuild` 负责组装；`extractZipWithLimits` 本就可注入
预算，构建路径用放宽 limits（见 §2 条目预算）。

一期 config 键（`functions.dispatcher.*`，字段号/类型/默认值）：
`string build_timeout = 10`（默认 "5m"，string 先例 = boot_timeout）；
`bool verify_build = 11`（默认 true）。二期预留 `Git git = 7`
（fetch_timeout=1 / allow_insecure=2 / max_repo_bytes=3）、三期预留
`Image image = 8`（allowed_registries=1）、四期 `string routing_mode`
（dispatcher 子消息）。**Rollout 表一期「无变更」据此修正为「config.proto
两字段，无 API proto/DB 变更」**。

### 1. 阶段一：Go 运行时（不碰 proto，zip 通道不变）

**运行时表**：`runtimes.go` 增 `{ID: "go-1.25", Name: "Go 1.25",
Entrypoint: "Main"}`（基础镜像 tag 实施时以 Docker Hub 实际为准锁版本，
runtime ID 随之定稿——Go 1.25/1.26 更替期，不承诺版本窗口策略，升级 =
新 runtime ID）。

**探测**（`ExtractZip`）：优先级 `index.js` > `go.mod` > `main.py`（混装按
node，冲突不报——与「探测即入口」现状一致）。`go.mod` 探测产出：
module 行解析（module path，读取上限对齐 package.json 的 4MiB 防护）、
require 非空判定、`go.sum` 存在性、`vendor/` 目录存在性、**`twmain/`
保留目录冲突拒收**（错误文案指明改名）。`ZipContents` 扩展为
`SourceContents`（字段见下）。

**依赖确定性（对齐 node「lockfile 强制」口径）**：require 非空且无
`go.sum` → 构建期报错（错误文案「检测到外部依赖但缺少 go.sum——go mod
tidy 生成后提交」）；**`vendor/` 受纳**（交叉验证独有发现：vendor 是 Go
官方钉版机制、源码形态无跨平台二进制问题，与拒收 `node_modules` 的理由
本质不同）——vendor 存在时 `go build` 自动 `-mod=vendor`，go.sum 不作要求。

**Go 函数形态（平台生成 bootstrap——交叉验证 3/3 独立收敛，推翻原 SDK
路线）**：用户 zip 根 = module 根（`go.mod` 必须），**非 main 的任意包名**；
用户契约纯 stdlib、零平台依赖：

```go
// fetch 风格（优先，AST 探测）：HTTP 触发器封套还原为真 *http.Request，
// 响应经 ResponseRecorder 捕获 → fetch 封套（status/headers/body 可控）。
// 请求级身份六件经 r.Header（x-tw-execution-token / x-tw-execution-id /
// x-tw-source / x-tw-invoking-user-id / x-tw-project-id 原样可达）——与
// node fetch 风格的 env 参数信息等价、通道不同，协议文档精确写明。
func Fetch(w http.ResponseWriter, r *http.Request)

// main 风格（兜底）：ctx 键 executionToken/apiBaseUrl/executionId/
// source/invokingUserId/projectId（string 值，文档化契约）
func Main(data map[string]any, ctx map[string]string) (any, error)
```

平台侧在构建上下文生成保留目录 `twmain/`（无点前缀，规避 go build 点
目录边角行为）：`runner/gorunner/` 包内嵌模板资产（`go:embed`）——
`runtime.go`（协议实现：health、POST / 双轨、`x-tw-*` header 还原、
封套 base64 解析、per-request 超时（到点 500 封套放弃等待，goroutine
残跑诚实声明同 node）、`TW_MAX_REQUESTS`/SIGTERM drain、64KB 截断、
hop-by-hop 头过滤）+ `main.go`（生成：`import user "<module-path>"`
引用用户根包，按 AST 探测结果调 `user.Fetch` / `user.Main`）。
**AST 探测**用 `go/parser`（标准库）扫根包导出函数——比文本探测可靠
（不误匹配注释/字符串），比编译探测前置；**文件枚举须按 build
constraints 评估**（`go/build` 的 MatchFile 规则，与编译器同口径），
否则 `//go:build ignore` 或平台特定文件里的 `Fetch` 会被误探测、随后
以 `undefined: user.Fetch` 编译错收场（用户明明写了，却看不懂为什么
找不到）。**两处口径收窄（独立复核 A6）**：①`MatchFile` 不排除
`_test.go`——测试文件里的 `Fetch`（`export_test.go` 向外暴露内部函数
是 Go 社区常见写法）会命中探测但被 `go build` 忽略，必须显式 skip
`*_test.go`；②dispatcher **宿主进程模式**（检测不到自身容器 ID，开发
态真实拓扑）下 `build.Default.GOOS` 是宿主 OS 而非 linux——探测必须用
显式 `GOOS=linux / GOARCH=amd64` 的 `build.Context`，不得用
`build.Default`。签名不匹配由编译器报错兜底。
两者皆无 → 构建期报错「go function must export Fetch or Main」（对齐
node「index.js must export main or fetch」）。
**bootstrap 源码仅限标准库（硬约束）**：vendor 模式下 `twmain/` 的
依赖同样经 vendor 解析，协议实现里出现任何第三方依赖都会让 vendor
用户构建失败——runtime.go 的 import 白名单 = 标准库。

**构建模板**（`DockerfileFor` go 分支，多阶段）：

```dockerfile
FROM golang:1.25-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
# GOFLAGS 按 vendor 探测分支（第二轮复查修正）：显式 -mod=readonly 会
# 覆盖「vendor 目录存在时自动 -mod=vendor」的默认行为，vendor 形同虚设；
# 故 HasVendor=true 时显式 -mod=vendor，否则 -mod=readonly。
ENV GOFLAGS=-mod=readonly        # （vendor 分支替换为 -mod=vendor）
# go.sum 通配（独立复核 A5）：require 为空的纯 stdlib 函数合法无 go.sum
# （vendor 分支亦不作 go.sum 要求），Docker COPY 任一源缺失即失败——
# node 模板先例正是 package-lock.json*（runner.go:75）。
COPY go.mod go.sum* ./
RUN go mod download              # （vendor 分支免此层）
COPY . .
COPY twmain/ ./twmain/
RUN go build -trimpath -ldflags="-s -w" -o /out/tw-app ./twmain
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /out/tw-app /tw-app
USER 65534:65534
ENV TW_RUNNER_PORT=18080
CMD ["/tw-app"]
```

- **「构建期不执行用户代码」保持且强于 node**：`go build` 只编译不执行
  （Go modules 无 npm 生命周期脚本等价物，`//go:generate` 不由 build
  触发）；`CGO_ENABLED=0` 结构化消灭 `#cgo`/pkg-config 构建期命令执行面
  （编译器开关，比包管理器旗标 `--ignore-scripts` 更彻底）；cgo 依赖
  （如 mattn/go-sqlite3）不可用，文档明示。
- 运行段 alpine 而非 scratch：函数 HTTPS 出访需要 CA 证书，但基础 alpine
  **不自带**，须显式 `apk add ca-certificates`；非 root 数字 UID。
- 分层缓存与 node 同构：go.mod/go.sum 不变 → `go mod download` 层命中。
- `DockerfileFor` 签名从三个 bool 改为 `(contents SourceContents)`。

**部署后验证 spawn（新质量门，复查修正版）**：dispatcher `BuildImage`
尾部追加——build 成功后 spawn 一枚验证实例 → `/_tw/health` 轮询（预算 =
`boot_timeout`）→ 就绪即 Stop/Remove。四条实施语义：

- **携带函数当前 variables**（与执行时同源组装，经 BuildRequest 传递）：
  模块顶层/init 依赖环境变量是常见模式，无 env 的验证会误杀合法部署
  （交叉验证中三方案均未发现此点，为原会话复查独有修正）；
- **egress 分类与执行一致（对抗审查最强修复）**：`EgressUntrusted` 随
  BuildRequest 传递，untrusted 函数的验证实例挂 **internal 变体网络**
  （`ResolveInternalNetworkName`，与 `spawnInstance` 同路）——否则验证期
  给不可信镜像开了一跳出网窗口，执行期 egress 约束被部署期旁路；
- **池外实例**：走 daemon 原语，不进 Redis 注册表、不受
  `MaxResidentInstances` 约束、不参与 reaper 对账，用完即删；
- **失败时回收容器日志尾部**（docker logs 尾部拼进 deployment.error）：
  编译错误在构建日志、运行期错误（panic/协议未实现）在容器 stdout，
  第一现场必须带回；
- **验证范围 = health 探针，不做 invoke 验证**：invoke 需要平台构造
  TW_DATA 并**执行用户代码**（副作用不可控），违反「部署期不执行用户
  代码」不变量；「health 通过 ≠ invoke 语义正确」的残余风险由运行期
  transport-error → 杀实例重建语义兜底（有意为之，非疏漏）；
- 资源：验证在构建信号量临界区内、spawn 同款限额。

Go 强制、node 同开（`verify_build` 默认 true 可关）；该门同时是镜像源
的契约验证（同一机制复用）。

**构建超时与 ctx 解耦（复查修正）**：`buildDeployment` 执行 ctx 脱离请求
ctx（`context.WithoutCancel`），由 `functions.dispatcher.build_timeout`
（默认 5min，对齐 worker 补构建）封顶；客户端断开后构建继续、状态照常
落库，客户端以 deployment.status 轮询兜底。MVP 维持同步返回语义，异步
构建队列不在范围。**超时画像标注（独立复核补充）**：Go 冷构建 =
dispatcher 拉 `golang:1.25-alpine` 基础镜像（数百 MB，计入构建时间）+
`go mod download` + 编译——冷 daemon + 大依赖场景可逼近 5min 默认值，
config 文档须标注该画像（首个 Go 部署在全新环境超时 = 高频第一体验），
必要时按 runtime 分档调大；验证 spawn 的 `boot_timeout`（60s）语义上
嵌套在 `build_timeout` 预算内（构建 + 验证共享一个上限）。

### 2. 阶段二：Git 源（独立 functions-packer 服务——2026-09-17 owner 裁决，
推翻「server 进程内物化」与「dispatcher 进程内 fetch」两案）

**裁决记录**：server 进程内 go-git 物化（原 D2）被 owner 否决——控制面
进程不承载不可信输入的重资源操作（clone 内存尖峰伤及全部 API 流量）；
dispatcher 进程内 fetch 同被否决——dispatcher 仍在函数执行关键路径。
最终形态 = **functionsdispatcher 模式复刻**：职责单一、可独立重启/扩缩的
基础设施服务，OOM 只影响 git 部署自身。zip 流向反转：不再是
「server 取好 zip 发给 dispatcher」，而是「packer 打好 zip 交回 server 落盘」。

**服务契约**（`functionspacker/` 顶层组件包 + `cmd/functions-packer`，
与 `functionsdispatcher/` 同级模式；无 Redis/DB/池依赖，状态可牺牲）：

```
POST /v1/pack/git
  {url, ref?, directory?, username?, token?}        ← token 一次性，仅内存
→ {commit_sha, checksum, zip_base64 ≤50MiB}          ← 错误走 HTTP 状态码映射
```

- **配置**：`functions.packer.{url, shared_token, fetch_timeout,
  max_repo_bytes, max_zip_bytes, concurrency}`（Functions 字段 9）；
  **url 未配置 = git 源未启用**（app 层 git 部署报明确错误；zip/node
  完全不受影响，增量启用）；shared_token 中间件与 dispatcher 同款；
- **并发自限**：packer 自带并发上限（concurrency，默认 4），饱和回 429
  → server 映射 ResourceExhausted——与构建信号量成**两道独立闸**，
  无嵌套（原 A8 信号量两相化问题整体消失：pack 在构建信号量之外）；
- **clone 实现**（packer 进程内 go-git v5；若个别 forge 兼容性失败，
  退路是换 shell-out git 或容器化 clone，端点契约不外溢）：
  - 浅克隆 `Depth: 1` + **单 ref refspec**（`+refs/heads/<ref>` /
    `+refs/tags/<ref>`，不取全量 refs）；ref 解析：40 位 hex → 直取
    commit（不可达则回退非浅 fetch + checkout）；branch → tag → HEAD
    依次解析；解析后回读 SHA；
  - 凭证：`http.BasicAuth`（username 空时字面量 `git`——GitHub/GitLab
    PAT 通用形态）；
  - **SSRF 防护**：自定义 `http.Transport.DialContext` 在 DNS 解析后
    校验 IP，默认拒绝 loopback/private/link-local（169.254.169.254 云
    元数据端点等）；**校验必须实施在拨号点（Dialer 的 Control/
    DialContext 钩子对将拨号的真实 IP 校验），而非请求前预解析——防
    DNS rebinding**；`functions.packer.allow_insecure=true` 时放行
    http + 私网（自托管内网 gitea 是真实场景，给显式开关而非逼出危险
    旁路）；URL 拒绝内嵌 userinfo（凭证走独立字段，防 URL 落日志泄密）；
    development 环境额外允许 `file://`（集成测试与本地开发，复用
    `TORCHWOOD_ENV` 语义）；
  - 物化：`directory` 穿越校验（Clean 后拒 `..`/绝对路径），walk 子目录
    跳过 `.git`、跳过 symlink 条目、拒 `node_modules`（与 zip 通道同
    口径）；**两级预算**：克隆 worktree ≤200MiB（磁盘侧，config
    `max_repo_bytes`）＋ 物化 zip ≤50MiB（传输侧，config `max_zip_bytes`，
    与 `maxBuildBodyBytes` 内联通道同源）；条目 ≤5000（git worktree 是
    真实文件，宽于 zip 上传的 1000 反炸弹声明侧预检）；fetch 超时 120s
    （config `fetch_timeout`）；
  - **条目维链条**（独立复核 A1）：dispatcher 侧 `ExtractZip` 默认
    `maxZipEntries=1000` 会击毙 5000 条物化 zip——`BuildImage` 用注入的
    放宽 limits（条目对齐 5000、总量对齐 200MiB 解压预算）；诚实声明：
    >5000 条的典型 vendor 项目仍受限，属声明边界。
- **资源画像与隔离声明**：clone 的内存尖峰/磁盘消耗全部收敛在 packer
  进程——它死了只有 git 部署不可用（重启即恢复，dokploy/compose
  restart 策略），API/函数执行/node 构建无感。最坏损失上界 = 一次
  ≤120s 的失败 clone（流量 + 临时盘 + 一个 packer 并发槽）。

**app 时序**（pack 在 deployment 行落库之前——失败路径无行无 zip，与
zip 魔数校验失败同类）：

```
CreateDeployment(git): 形状校验（https/ref/dir）→ SourcePacker.PackGit
（HTTP 调 packer）→ zip 写既有 zipPath + INSERT 行（source 列 + 钉死
SHA + checksum）→ buildDeployment —— 与 zip 源完全同构
```

**重建语义（推翻原 OQ5 限制）**：物化 zip 落既有 `zipPath` 且 **git 源
构建失败后 zip 保留**（zip 源维持现状删除）——worker 补构建以盘上 zip
为输入，**不依赖凭证**，原「一次性 token 部署不可自动重建」的限制就此
消失。zip 缺失（磁盘被清）报错 `source snapshot missing; redeploy`。

**审计**：`source_url + source_ref(钉死 SHA) + source_dir + context_sha256`
四件 = 不可变快照锚；分支后续移动不影响已部署内容。token 的审计脱敏
由 `internal/api/interceptor/audit_payload.go` 的敏感键关键词 mask
承担（清单已含 "token"——`GitSource.token` 自动命中）。

**多机四期衔接**：packer 无状态可多副本（无亲和需求——zip 由调用方
server 落在构建亲和节点的本地盘，M5 语义不变）。

### 3. 阶段三：镜像源（BYO Image）

**流程**：`CreateDeployment(image)` → 落库 pending → `Executor` 端口扩展
`ImportImage(ctx, fid, did, imageRef, registryAuth) (digest, error)` →
dispatcher 新端点 `POST /v1/dispatch/images/import`：`ImagePull`（RegistryAuth
base64，单次转发不落库）→ digest 解析（用户引用带 `@sha256:` 时校验一致，
防 tag 漂移）→ **retag 成 `ImageName(fid,did)` 并删原始引用** → **强制契约
验证 spawn**（§1 同一机制：带函数 variables、池外、日志尾部回收）→ ready，
digest 落 `source_ref`。worker 补构建：本地镜像在则零操作；镜像被外部删除
的边缘场景重 pull（公共镜像可补，私有无凭证失败标 failed——声明边界）。

**runtime = `image`**：`ListRuntimes` 增 `{ID: "image", Name: "Bring your
own image"}`；源/运行时互斥校验见 §0。

**并发语义（交叉验证独有并入）**：image 部署 `template_version = 0`
（未知模板）→ 既有 `< MinConcurrencyTemplateVersion` 判定自动把
`concurrency>1` 降为 1——BYO 镜像是否真支持并发无从验证，fail-safe 白拿
（写 5 等于做无法验证的承诺）。

**生态冷启动（交叉验证独有并入）**：契约基础镜像 `docker/functions-runtime-node/
Dockerfile` 随仓交付（FROM node:18-alpine + 预置 .tw-runner.js），用户
`FROM` 后 COPY 代码即得合规镜像；Go 侧平台 bootstrap 源码即参考实现；
Runner 协议文档（§0）即规格。发布是纯 ops 动作，不阻塞代码交付。
**本地测试指引（竞品调研补强，对标 Lambda RIE）**：协议文档含
「本地验证」一节——契约基础镜像可直接 `docker run` 起本地对照，twmain
的 runtime.go 源码即参考实现，BYO 镜像用户在推送前即可本地自测
`:18080` 契约符合性（AWS Lambda Runtime Interface Emulator 的等效物，
成本只是文档指引）。

**安全声明**：镜像内容不经平台构建护栏，但执行面 hardening 不变
（CapDrop ALL / no-new-privileges / 只读 rootfs + tmpfs / pids 512 /
内存 CPU 限额 / per-project 网络，untrusted 函数 internal 变体）。信任级 =
部署者（functions.write 特权主体，与 zip 上传同级）。**registry SSRF
基线（独立复核 A2——原「空 = 不限」与 git 源的 SSRF 防护自相矛盾：
pull 的网络发起方是宿主 daemon，可达内网，错误回显即端口扫描侧信道，
内网无认证制品可被拉进宿主执行）**：与 git 源对称——registry host
拨号点 IP guard（默认拒 loopback/private/link-local），
`functions.image.allow_insecure=true` 显式放行内网 registry（自托管
内网 registry 是真实场景，显式承认而非默认敞开）；`allowed_registries`
为可选的正向白名单（与 allow_insecure 正交，可叠加）。`DeleteDeployment`
正常走 `RemoveImage`（retag 后本地即平台镜像，删除不动上游 registry）。
**worker 补构建分流（独立复核 M5）**：现状补构建统一走
`buildDeployment(zipPath)`——image 源无 zipPath；裁决：按
`source_type` 分支，image → **幂等 ImportImage**（digest 已知，本地命中
零 pull、miss 重拉），复用 M7 补拉语义。

### 4. 阶段四：多机执行面（细胞模型，独立立项量级）

**动机**：dispatcher 是容量单点（非可用性单点）——所有构建与函数执行容量
绑定在一台机的 docker daemon 上，HA 双副本只换可用性不换吞吐；且镜像不
共享，HA 备副本接管时须自建全部镜像。owner 裁决（2026-09-17）：采纳
**细胞模型水平扩展**，排为阶段四（可在三期后按容量画像启动，不阻塞
三期交付）。

**拓扑**：N 节点 ×（dispatcher 进程 + 本机 docker daemon）。控制面共享
（Redis：实例注册表 / spawn 锁 / 执行队列 / **新增节点注册表**；
Postgres 元数据）；**函数容器生命周期完全本机**——`tw-func-<project>`
网络每节点自建，函数容器只与本机 dispatcher 和本机 attach 的回调容器
通信，永不跨机，无 overlay 网络（与 v3「不做 K8s」不冲突）。

**三块缺口与裁决（M1..M6）**：

- **M1 镜像全局化（前置，独立可交付）**：构建完成后 push 到
  `functions.docker.registry`（从命名前缀升格为真实 registry；本地
  `func-<fid>-<did>` 保留为 pull 后的本地别名，retag 语义不变）；各节点
  spawn 前本地 miss 则 pull。**顺序裁决：构建 → 验证 spawn → push**
  （验证失败不污染 registry）。BYO image 源天然全局。ready deployment
  的恢复语义变为「任意节点 pull」——worker 补构建对 ready 部署不再依赖
  本地 zip。
- **M2 节点注册表**：Redis `torchwood:fnnodes:*`——节点 ID、dispatcher
  URL、容量水位（常驻数 / 可用内存）、TTL 心跳；reaper 式对账。
- **M3 执行路由（实例亲和，非静态分片）**：`InstanceRecord` 增 `node`
  列；`DispatcherExecutor`（路由层收敛在适配器内，**Executor 端口签名
  不变**）：查 fninst → 有实例 → 路由其节点；无 → 按水位加权选节点冷
  启动并落账。节点不可达 → 实例视为孤儿 → 重选节点（自愈 = 池冷启动
  到幸存节点，前提是 M1）。
- **M4 容量共享**：`residentTotal` 移 Redis；`max_resident_instances`
  升格为全局配额 + 每节点配额两层（「每 daemon 上限」的 Q11 语义保留
  为节点层）。
- **M5 构建亲和 + zip 不迁移（最小改动裁决）**：deployment 首次构建
  选定节点 → zip（含 git 物化快照）落该节点本地 → worker 补构建固定
  路由该节点。节点死：在途 deployment 标 failed 要求重部署（语义损失
  明示）；ready deployment 无损（registry）。**zip 快照迁对象存储**
  （MinIO/S3 配置面现成）留作四期二步，仅在「节点亲和丢失导致 failed
  重部署」被实测为真实痛点时启动。
- **M6 回调与网络**：`callback_container` 语义从「一个容器名」升格为
  「每节点可达的回调地址」——server 多副本按节点部署（每节点 compose
  一个 server，callback = 本节点 server），或改节点间可达地址（实施时
  裁决，倾向前者：保持函数容器只与本机通信的细胞不变量）。
- **M8 reaper 对账范围（独立复核 A3——M1-M7 原清单的硬遗漏）**：现状
  reaper 经 `registry.ListFunctions` 遍历**全部函数的全部实例记录**并对
  每条做 `InspectInstance`——那是**本机** docker daemon 调用。多节点共享
  `fninst` 下，节点 B 的 reaper 对节点 A 的容器 Inspect 必然 NotFound →
  走幽灵清理 → **把 A 上健康运行的实例从池中蒸发**（容器泄漏、容量骤降、
  双 reaper 互相清除的稳态抖动）。修正为缺口清单必修项：①reaper 对账
  范围收窄 = 只处理 `InstanceRecord.node == 本节点` 的记录；②**死节点
  残留记录的收敛者**——节点心跳消失超过宽限后，其实例记录由任意存活
  节点批量清除（容器随机器死已不存在，记录清理无需本机），idle 记录
  无 lease 过期回收的现状（只有 busy stuck 有 grace）必须补这一路径。
  未做 M8 的多机交付 = 数据面互相残杀。
- **M5 补充（独立复核：worker 拓扑收口）**：「zip 落构建亲和节点本地 +
  worker 补构建固定路由该节点」隐含 worker 能读到该节点的盘——现状
  zip 语义 = server/worker **共享盘**，多机下若 worker 集中部署则路由
  落空。四期 M5 实施前必须收口 worker 拓扑（M6 倾向 server 每节点一
  个，worker 同理细胞化——每节点 worker 只补构建本节点 zip；此裁决
  留四期实施首日定稿，单机现状不受影响）。
- **M7 无 registry 的多机形态（owner 2026-09-17 追问补齐，三档；独立
  复核后 replicated 降档）**：
  `functions.dispatcher.routing_mode`：`local`（**缺省，向后兼容**）|
  `replicated`（**实验档，见下）| `registry`（完整形态）。
  - `registry` 模式 = M1 + M3 完整语义（实例亲和 + 跨节点冷启动自愈，
    容量与可用性同时解决）；
  - `replicated` 模式 = **每节点独立构建（降级为实验档，独立复核 A4 +
    裁决复核一致结论）**：原推荐「2-3 节点 replicated 起步」被推翻——
    它的广播与追平子系统需要 zip 全局可达，恰与 M5「zip 落节点本地、
    不迁移」互斥（要么 zip 入共享存储 = M5 已后置的对象存储变成 replicated
    前置必选，要么广播无输入）；且三笔代价（状态对账/扩容追平/漂移窗口）
    均为子系统级工程量，省下的只是一个低造价的 `registry:2` 容器——
    投入产出倒挂。**演进推荐改为：`local` 起步（4a 即可用）→ 直接
    `registry`（4b）**；replicated 仅在实测出现「坚决不跑 registry
    组件」的部署形态后再评估，且实现前置 = 先解决 zip 全局可达（对象
    存储），文档不再将其列为默认路径。三档模型与改档平滑切换的机制
    维持不变；
  - `local` 模式 = **函数亲和**：镜像不分发，函数的执行与冷启动固定
    路由到其镜像所在的构建节点（M5 构建亲和的自然延伸，禁用跨节点
    冷启动）。**容量照常 ×N**（函数分布 N 节点，每函数可用容量 =
    所在节点全部资源）；**可用性不升级**——节点死 = 该节点函数全部
    不可用，恢复 = 重新部署（构建路由按水位选存活节点，重建即迁移，
    比 replicated/registry 的自动迁移多一次管理动作）；某节点满载时
    其他节点无镜像可帮，该函数排队语义退化为单机时代。多节点 + local
    的降级语义必须显式配置承认，不静默发生；
  - **image 源豁免（replicated/local 均适用）**：BYO 镜像原始引用在
    `source_url` 有记录，任意节点缺镜像可重 pull 原始引用再 retag
    （worker 补拉语义复用）——仅 zip/git 构建产物受档位约束；
  - **推荐路径**：`local` 起步 → `registry:2`（官方镜像自包含、支持
    filesystem 或 S3 后端——后者可挂 MinIO 复用存储，与本地栈
    Postgres/Redis/MinIO 同量级）。

**节点故障语义**：节点死 = 其上实例同死，执行请求冷启动到幸存节点
（M1 前提下自愈）；dispatcher 进程崩（机器活着）= 实例存活但无分发，
路由层视节点不可达处理，旧实例由本机 dispatcher 重启后的 reaper 或
TTL 收敛。**节点级冗余取代节点内双副本仲裁**——HA 的形态从「双副本
抢主」变为「多节点互为冗余」，这正是「HA 不解决容量」的答案：细胞
模型同时解决可用性与吞吐。

**四期内切片顺序**：4a = M2 + M3 + **M8**（节点注册 + 路由层 + reaper
对账收窄——`local` 模式即刻可用，无 registry 也能拿到容量 ×N 的降级
形态；**M8 与 M2/M3 同片：没有它，多节点交付即互相残杀**）→ 4b = M1
（registry push，`registry` 模式解锁跨节点冷启动与节点级自愈）→
4c = M4（全局配额）→ 4d = M6 + M5 的 worker 拓扑收口（回调多节点化）。
replicated 实验档不在切片内（按需另立）。每片独立可交付。

**三期不堵路纪律（写给三期实施者，四条）**：① `DispatcherExecutor`
的路由演化收敛在适配器内部，Executor 端口与 Build/Execute 签名不因
多机改变；② 构建链预留 registry push 的配置面（不实现）；③
InstanceRecord 的 Redis 结构本三期不加 node 列；④ zip/物化路径维持
单机共享盘假设（四期 M5 裁决构建亲和）。

### Security & 语义声明（跨阶段汇总）

- 三源部署的**执行面不变**：spawn hardening、执行身份、触发器、变量注入
  全部透明复用——只动「镜像从哪来」，不动「容器怎么跑」；
- Go 构建供应链面严格窄于 node（无生命周期脚本 + CGO=0 + go.sum 内容
  寻址 + proxy 校验）；
- git clone 限 IP guard + 预算 + 超时 + https-only 默认；
- 凭证三不：不落库、不落日志（审计 mask）、不回显；**绝不进
  function_variables**（会注入函数运行时 env，等于把 git 凭证交给用户
  代码——交叉验证 3/3 明确论证的硬边界）。

## Observability

- 新增 `torchwood_functions_builds_total{runtime,source,result}`（三源两
  运行时交汇，成败归因需统一计数器——交叉验证独有并入）；
- `functions_dispatcher_build_duration` 按 source_type 分桶（Go 冷/热构建
  画像）；验证 spawn 失败计数（契约违规画像）。

## 测试策略（独立复核 M1 补节，门控变量名与仓库基建实测对齐）

- **单测**：gorunner AST 探测表驱动（Fetch/Main 双签名、`//go:build
  ignore` 文件、`*_test.go` 排除、GOOS=linux 显式上下文、两者皆无报错、
  twmain 冲突拒收）；`DockerfileFor` 快照（node 分支逐字节回归 + go
  分支 golden + vendor/无依赖两形态）；gitfetch SSRF 表驱动（拨号点
  拒 loopback/private/link-local、DNS rebinding、userinfo 拒绝、穿越/
  symlink/预算两级）；app 用例（mock fetcher/executor：三源状态机、
  信号量两相、git 失败保留 zip / zip 失败删除分流、凭证不进持久化
  断言）；BuildRequest/runtime 对账（探测不符 InvalidArgument）。
- **docker 集成**（门控 **`TORCHWOOD_FUNCTIONS_DOCKER_HOST`**——注意
  不是想当然的 RUN_DOCKER_TESTS 变体名；另有 `TORCHWOOD_TEST_REDIS_ADDR`）：
  go 函数 zip（main + fetch 双风格）构建 → 验证 spawn → 执行 → 封套/
  超时断言；镜像源 fixture（现制契约镜像）import → digest → retag →
  验证 → 执行 → RemoveImage 幂等；git 源 file:// 端到端（dev 门控）。
- **护栏回归**：node 构建模板 golden 不变；`update_guard_test`（source
  列不可变——新列天然不进 `UpdateDeployment` 白名单，加显式防回归
  断言）；`grpc_swagger_test`（无新方法，投影字段变更过断言）；
  `cli/import_guard_test`；`sdk/go/server` 覆盖测试自动覆盖新字段；
  审计脱敏断言（GitSource.token 不出现在审计载荷——`audit_payload.go`
  mask 命中验证）。
- **fake 基建复用**：`functionsdispatcher/daemon_test.go` 的 fake 网络
  客户端驱动验证 spawn 路径；mock executor 驱动 app 分流。

## Rollout Plan / 阶段与切片

| 阶段 | 内容 | proto/DB | 依赖 |
|---|---|---|---|
| 一：Go 运行时 | gorunner 模板资产 + AST 探测 + go 模板/表项 + BuildSpec 定稿 + 验证 spawn（带 variables）+ build_timeout + ctx 解耦 + Runner 协议文档节 | config.proto 两字段（dispatcher.build_timeout=10/verify_build=11），**无 API proto/DB 变更** | 无（**不再依赖 SDK 发布**——生成路线无发布物） |
| 二：Git 源 | functions-packer 服务（go-git + IP guard + 两级预算 + 并发自限）+ source oneof + DB 迁移 + SourcePacker HTTP 适配器 + dispatcher 放宽解压预算 + CLI/Console | oneof、Deployment 投影、000023 迁移、config packer=9 | 无（可与三调序） |
| 三：镜像源 | ImageSource 实现 + runtime=`image` + pull/digest/retag + 强制验证 + template_version=0 + 基础镜像 | 复用阶段二 schema 的 image 分支 | 阶段一（协议文档 + 参考实现） |
| 四：多机执行面 | 细胞模型：M1 registry push → M2/M3 节点注册 + 实例亲和路由 → M4 容量共享 → M6 回调多节点化（§4，独立立项量级） | config `functions.docker.registry` 语义升格 | 阶段三交付后按容量画像启动（不阻塞） |

每阶段独立可交付、可回退（一期不动存量路径；迁移 000023 带 down；
proto 增量）。**装配与生成物清单（独立复核 M2/M3 补齐，漏项多为静默
性风险）**：二期起 `servergrpc/functions.go` 的 CreateDeployment 按
oneof 分发（现状直取 `req.GetCode()`）+ `mapDeployment` 补 source 四列
投影（**漏映射不报编译错，须显式断言**）+ `serverhttp` multipart 维持
zip-only 分支确认；SourcePacker HTTP 适配器进 `provides.go` + `task wire:all`；
config 变更 `task generate:config`；proto 变更 `task generate:proto`；
Console 改动后 `task console:build` 再 `task build`（embed 旧版本风险，
AGENTS.md 明文）。CLI：`functions deployments create-from-git <fn> --url …
[--ref --dir --git-username --git-token-env]` / `create-from-image <fn>
--image … [--registry-username --registry-token-env]`（token 走环境变量，
`--watch` 仅 zip 源）；`functions deploy` 目录校验放宽为 index.js/go.mod
二选一。Console 部署面板三 tab + 源徽章（`git@<短SHA>` / digest 短码 /
zip）。SDK 方法覆盖自动可用，CLI 仅需旗标。

## Key Decisions（2026-09-17 owner 拍板 + 同日交叉验证修订；来源标注：
〔收敛〕多方案独立一致 /〔裁决〕分歧审论据后裁 /〔独有〕单方案发现验证并入）

| # | 裁决 | 来源 |
|---|---|---|
| D1 | 三需求一份设计、三期实施（源模型统一 + 协议公开化） | 收敛 |
| D2 | git 打包收敛**独立 functions-packer 服务**（2026-09-17 owner 否决 server 进程内方案并定向此形态；zip 回流共享盘保 worker 补构建免凭证）；image pull+retag 免构建 | owner 裁决 |
| D3 | **Go = 平台生成 twmain/ bootstrap（AST 探测 Fetch(w,r)/Main(map,map) 双轨，stdlib 契约，用户零平台依赖）**；SDK 路线否决（见 Alternatives） | 收敛 3/3（推翻原拍板，待 owner 重新确认） |
| D4 | go.sum 强制（require 非空）+ GOFLAGS 按 vendor 分支（vendor → `-mod=vendor`，否则 `-mod=readonly`）+ `CGO_ENABLED=0` + **vendor/ 受纳**；多阶段模板 alpine 运行段 + ca-certificates + 非 root | 收敛 + vendor 独有 + 二轮复查修正（GOFLAGS 显式值覆盖 vendor 自动检测） |
| D5 | `:18080` 协议升格公开契约（文档 + bootstrap 源码 + 基础镜像三件参考实现），版本随 RunnerTemplateVersion（共享 5 不 bump） | 收敛 |
| D6 | git clone = packer 服务内 go-git + IP guard（拨号点拒 loopback/private/link-local）+ allow_insecure 开关 + dev file:// | 裁决（SSRF 防护独有并入；位置随 D2 修订） |
| D7 | 探测结果必须 == fn.runtime，不一致构建期报错；image 源专用 runtime ID `image`，源/运行时互斥 | 收敛 |
| D8 | 凭证一次性内联不落库（否决持久化凭证表——packer 物化 zip 后重建不依赖凭证，持久化只剩边缘场景收益）；绝不进 function_variables | 裁决 3/4 |
| D9 | `source_url + source_ref + source_dir + context_sha256` 统一可复现性锚；git 钉死 commit、image 钉死 digest | 收敛 |
| D10 | 验证 spawn：强制（Go/BYO）、带函数 variables、池外实例、失败回收容器日志尾部；`verify_build` 默认 true | 收敛（验证必须有）+ 带 env/日志回收为原会话复查独有 |
| D11 | 构建 ctx 与客户端断开解耦（WithoutCancel + build_timeout 5min 封顶） | 独有（原会话复查） |
| D12 | image `template_version=0` → concurrency 自动降级 fail-safe；retag 后删原始引用 | 独有 |
| D13 | git 物化条目预算 5000（zip 上传维持 1000）；git 构建失败 zip 保留 / zip 源维持删除 | 独有 |
| D14 | 顺手修复：Build 载荷补 project_id/function_timeout_seconds（drain 精确化） | 收敛 3/3（原会话漏） |
| D15 | `builds_total{runtime,source,result}` 指标；`--watch` 仅 zip；multipart 维持 zip-only | 独有 |

## Open Questions（2026-09-17 owner 全部收口；交叉验证后两项作废、两项改判）

- ~~OQ1 验证 spawn 是否对 node 统一开启~~ **已拍板：统一开启**
  （`verify_build` 默认 true，可关；且必须带 variables——复查修正）；
- ~~OQ2 TemplateVersion 是否 bump~~ **已拍板：不 bump，共享 5**
  （交叉验证 4/4 一致）；
- ~~OQ3 Go 版本与基础镜像~~ **已拍板：`go-1.25` + `golang:1.25-alpine`，
  实施时以 Docker Hub 实际 tag 锁版本，runtime ID 随之定稿**（竞品调研
  补强论据：AWS go1.x runtime 绑死语言版本，废弃时全体用户被迫迁移——
  「升级 = 新 runtime ID、旧 ID 不日落」正是此教训的解法。独立复核
  环境提示：当前（2026-09）Go stable 已至 1.27 系、本仓库 toolchain 为
  go 1.26.5——实施时按此活口锁最新 stable，勿照抄 1.25）；
- ~~OQ4 source 用 oneof 还是平铺~~ **已拍板：oneof**（field 2 移入，
  wire 兼容，无存量用户）；
- ~~OQ5 git 重建限制~~ **原拍板（接受不可重建限制）作废**：交叉验证改判
  server 物化 zip + 失败保留后，重建不依赖凭证，限制消失——更优语义，
  待 owner 确认；
- ~~OQ6 平台侧工具镜像 digest 固定~~ **作废**：dispatcher 容器化 clone
  方案被推翻（D6），无工具镜像；
- ~~OQ7 SDK module 路径~~ **作废**：SDK 路线被 D3 推翻，无发布物；
- 新增裁决（交叉验证）：git `allow_insecure` 默认 false；dev 环境
  `file://` 仅测试；image 源 worker 补拉为声明边界。

## Alternatives Considered

- **server 侧进程内 go-git 物化（原 D2，2026-09-17 owner 否决）**：隔离
  论据曾胜出（worker 补构建论据），但控制面进程承载不可信输入的 clone
  内存尖峰会伤及全部 API 流量——「其他都是业务服务」原则下重资源操作
  必须收敛进可牺牲的基础设施进程；由此演进出独立 functions-packer 服务
  （§2，zip 流向反转为 packer→server），D2 的全部收益（worker 补构建
  免凭证、快照可复现、预算护栏）在 packer 案中原样保留；
- **dispatcher 进程内 fetch + 快照回传（过渡方案，同日被否）**：zip 回传
  保住了补构建免凭证，但 dispatcher 仍在函数执行关键路径——clone 尖峰
  炸的是全平台函数面；owner 定向「单独一个服务只负责拉取和打包」后
  演进为 packer 服务案；

- **Go SDK 路线（`twfn.Serve(handler)`，原拍板 D3）——被交叉验证推翻**：
  三个独立方案一致否决：用户必须 `go get` 平台模块 → 平台必须先发布
  （proxy.golang.org 可用性成硬前置）、体验与 node 不对称（node 用户零
  平台依赖）、入口校验推迟到运行期。生成路线的反论据（module 行解析、
  包名约定、协议烧模板）经复核可控：AST 探测用标准库、无点目录名规避
  go build 边角、TemplateVersion 本就是模板资产的版本机制。SDK 若未来
  需要（类型化 DX），可在生成契约之上加糖，不破坏本设计。
- **dispatcher 侧容器化 clone（原拍板 D4，`docker run alpine/git`）——被
  推翻**：SSRF/资源隔离动机被 IP guard + 预算 + 超时覆盖；容器化引入
  工具镜像依赖与 dispatcher 协议扩展，且物化 zip 论据（worker 补构建/
  删除/模板版本全链路零改动 + 重建免凭证）压倒性 favor server 侧。
- **dispatcher 侧进程内 go-git（方案 B）**：构建权威论据成立，但物化 zip
  不落在共享盘 → worker 补构建断链或需凭证回传 → 为此引出持久化凭证表
  （B 的选择，被 D8 否决）。连带成本高于收益。
- **持久化凭证表（方案 B）**：为 image 补拉边缘场景引入明文凭证面，
  不值（D8）。
- **镜像源原生 ref@digest 寻址、不 retag（方案 B）**：外部镜像名作内部
  寻址键不稳定；retag 后全链路零改动且删除语义干净（本地 tag，不动
  registry）。
- **镜像源不做部署期验证（方案 B 自报薄弱点）**：v1 python「跑不起来的
  镜像错误推迟到首次调用」教训的反面，否决。
- **Go sidecar runner 双容器 / go plugin / WASM**：v3 Non-Goals 或技术上
  死亡区，不重开。
- **server 侧 fetch 转 zip 但 zip 通道 base64 内联传输到 dispatcher**：
  现状即如此（Build 内联 zip），git 物化 zip 完全复用，无新传输问题。

## 复查记录（2026-09-17，拍板后深度复查）

视角：运维故障时间轴、Go 工具链/开发者、存量回归、替代方案再审。
修正 3（验证 spawn 携带 variables / 构建 ctx 解耦 / SDK 发布前置）、
补充 5（池外语义、日志尾部回收、探测优先级、ca-certificates、retag 删
原始引用）、维持 5。其中 SDK 发布前置随 SDK 路线整体作废；其余全部
保留进修订稿。三项修正的处置：验证 env 与 ctx 解耦为最终方案部件
（D10/D11，交叉验证中无人独立发现——本会话独有贡献）；SDK 发布随
D3 推翻消失。

## 交叉验证记录（2026-09-17，independent-design：三子代理隔离并行）

三份独立设计（A/B/C）与原方案对照。关键矩阵（行=维度，列=方案）：

| 维度 | A | B | C | 原方案 |
|---|---|---|---|---|
| Go 形态 | 生成 bootstrap（AST） | 注入 .tw-main（点导入） | 生成 twmain（文本+二pass） | SDK |
| Go 入口 | Fetch(w,r)/Main 双轨 | 仅 Main | Main/Fetch(6参4返) | SDK Serve |
| git 位置 | server 物化 zip | dispatcher go-git | server 物化 zip | dispatcher 容器化 |
| 凭证 | 一次性 | 持久化表 | 一次性 | 一次性 |
| image 寻址 | retag | ref@digest | retag | retag |
| 部署验证 | 冒烟（无用户 env） | 无 | 验证（无用户 env） | 验证（带 env） |
| SSRF | 不做（自报欠账） | IP guard | 不做 | 文档声明 |
| runtime 一致性 | 不做 | 做 | 做 | 做 |
| vendor | 未提 | 未提 | 受纳 | 未提 |
| ctx 解耦 | 未提 | 未提（自报风险） | 未提（自报风险） | WithoutCancel |

**收敛**（直接进最终方案）：源模型 oneof/一次迁移、Go→git→image 分期、
多阶段模板全套（CGO=0/mod=readonly/go.sum/alpine/非 root）、
TemplateVersion 共享 5、契约镜像 + digest 钉死、凭证不进 variables、
重建按钉死值、Build 补 project_id（3/3 发现、原方案漏）、stdout 恒空、
CLI/Console 三入口。

**分歧裁决**：Go 形态（3/3 生成 vs 原 SDK → 改判生成，形态取 C 的无点
目录 + A 的 AST 探测与 Fetch(w,r) 签名——B 的点导入有点前缀目录风险、
C 的二pass 在 AST 下冗余、B 砍 fetch 使 HTTP 触发器透传对 Go 不可用）；
git 位置（2/3 server 物化 + worker 补构建论据 → 改判 server，原容器化
与 B 的 dispatcher 进程内同被否）；凭证（3/4 一次性）；image 寻址
（3/4 retag）；验证必须有（B 自认薄弱）。

**独有收割**（验证为真并入）：A——AST 探测、context_sha256、基础镜像
随仓交付、dev file://、builds_total、--watch 边界；B——IP guard +
allow_insecure、`.tw-main` 点目录风险（→ 用 C 的无点目录规避）、
5000 条目预算、image 补拉边界；C——vendor 受纳、构建失败 zip 保留
分流、template_version=0 并发降级、twmain 冲突拒收、FetchGit 行前时序；
原会话——验证带 variables、ctx 解耦、日志尾部回收（三者交叉验证中
无人独立发现，为反向补强）。

**盲区终检**：三方案自报盲点（go-git 兼容性、点目录构建、镜像冷启动、
基础镜像 tag 存在性、zip 预算 vs vendor）全部已处置；契约维度扫描，
三方案与原方案**均未覆盖**的维度：①多机部署时物化 zip 迁对象存储的
前瞻（列为远期，明确不做）；②Console 构建分钟级时的前端轮询 UX 细节
（随一期客户端指引交付，实现期定）；③Go runtime 的 entrypoint 字段
占位语义（文档化即可，无分叉）。无新增开放问题。

## 复查记录 II（2026-09-17，交叉验证修订稿的二轮深度复查）

视角：事实核查层（合成方案的技术假设逐条验证）、协议长期演进（双实现
维护）、预算链路一致性（各处数字限制互相咬合）、多数暴政警惕（3/3 收敛
的生成路线重新对抗）。

**修正 2（均已并入正文）**：

1. **git 物化 zip 预算与内联通道矛盾**：修订稿采纳的「物化总量
   ≤200MiB」与 `maxBuildBodyBytes`（Build zip base64 内联上限 50MiB）
   冲突——超 50MiB 的物化 zip 根本送不进 dispatcher，报错形态还是
   用户看不懂的 dispatcher 侧 exceeds。修正为两级预算：克隆 worktree
   ≤200MiB（磁盘侧）＋ 物化 zip ≤50MiB（传输侧，物化期收紧并给清晰
   错误）。此矛盾是收割 B 方案 200MiB 时未对齐内联上限引入的——
   第一版设计的「50MiB 同源」原本是对的；
2. **GOFLAGS 与 vendor 的覆盖关系**：显式 `GOFLAGS=-mod=readonly` 会
   覆盖「vendor 目录存在时自动 -mod=vendor」的 Go 默认行为，vendor
   形同虚设。模板按 `HasVendor` 分支（vendor → `-mod=vendor` 且免
   `go mod download` 层；无 vendor → `-mod=readonly`）。

**补充 4（已并入）**：协议双实现同版本纪律（node runner.js 与 Go
bootstrap 共享 `RunnerTemplateVersion`，任一变更同步另一实现并 bump，
禁止漂移）；Go fetch 风格身份通道精确化（`x-tw-*` 经 `r.Header` 可达，
与 node fetch 的 env 参数信息等价、通道不同，协议文档写明）；DB 迁移
编号标注示意 + schema 归属 projectschema；Non-Goals 的 SDK 措辞收敛。

**维持（复查过，结论不变）**：

- **生成路线 vs SDK（重新对抗）**：3/3 收敛不自动等于对，重新审了三条
  论据——「编译期入口校验」在有验证 spawn 的最终方案里已被削弱（生成
  与 SDK 的入口错误都能被质量门捕获），真正硬的是「SDK 发布前置」
  （事实性阻塞）与「DX 与 node 对称」（用户零平台依赖）。类型化 ctx
  的永久损失有远期加糖路径。裁决维持生成路线；
- 混装探测不报错（优先级 index.js > go.mod 固定，与现状一致），误部署
  由 D7 的「探测 == fn.runtime」对账兜底；
- 信号量持有期含 clone（最长 ~6min/槽，低频管理操作，接受）；
- AST 探测 + 编译器兜底（无需 C 的二 pass 回退）；
- twmain 无点目录 + 保留冲突拒收；其余修订稿内容。

## 对抗审查记录（2026-09-17，交付前反方攻击轮）

论点：「两个正交新增环节（源物化、验证门）的失败模式全部收敛在部署期」。
反方按杀伤力排序的攻击与处置：

| # | 攻击路径 | 判定 | 处置 |
|---|---|---|---|
| A1 | **验证 spawn 的 egress 洞**：untrusted 函数（client_callable/触发器）执行期挂 internal 网出网全 deny，但验证实例若挂常规网络 = 部署期给不可信镜像一跳出网窗口（60s），执行期 egress 约束被部署期旁路 | 成立概率高（每次 untrusted 部署即触发）、影响中高、修复低 | **修复**：EgressUntrusted 随 BuildRequest 传递，验证实例选网与 spawnInstance 同路（untrusted → internal 变体）。修复后重审：EnsureProjectNetwork(untrusted) 既有路径覆盖 dispatcher 自 attach，无新问题 |
| A2 | **AST 探测忽略 build constraints**：`//go:build ignore` / 平台特定文件里的 `Fetch` 被误探测 → 生成调用 → 编译器 `undefined: user.Fetch`，用户明明写了却看不懂 | 概率中（示例/平台文件不罕见）、影响低中（DX）、修复低 | **修复**：文件枚举按 `go/build` MatchFile 评估（与编译器同口径）。重审：dispatcher linux 容器与 build.Default 的 GOOS/GOARCH 对齐，无偏差 |
| A3 | **bootstrap 引第三方依赖**：vendor 模式下 twmain 依赖也走 vendor 解析，协议实现用任何非标准库 → vendor 用户构建必炸 | 概率高（实现者无意识就会引入）、影响中、修复极低 | **写明硬约束**：twmain 源码 import 白名单 = 标准库 |
| A4 | **DNS rebinding**：预解析校验通过、连接时 DNS 换内网 IP，IP guard 被绕过 | 概率低（针对性攻击）、影响高、修复低 | **修复**：校验实施在拨号点（Dialer Control 钩子对将拨号的真实 IP） |
| A5 | **验证为何不做 invoke**：health 通过 ≠ invoke 语义正确（函数仍可能首调 500） | — | **裁决不做**：invoke 验证 = 部署期执行用户代码（副作用），违反不变量；残余风险由运行期 transport-error → 杀实例重建兜底。写明理由防后人手贱 |
| A6 | **孤儿构建占信号量**：客户端断开后构建继续（ctx 解耦的代价），重试可占满 4 槽 → 新部署 ResourceExhausted | 概率低、影响低（5min 超时释放有界 + 需 functions.write 权限） | **接受**：有界、低频管理操作、权限门槛高 |
| A7 | **FetchGit 后 INSERT 失败的孤儿 zip** | 概率低（DB 抖动）、影响低 | 测试覆盖项：INSERT 失败路径删 zip |

结论：A1-A4 已修复并入正文（A3/A5 为约束/裁决声明），A6/A7 接受并留
测试项。无可击穿方案骨架的攻击——「复用既有状态机 + 两个正交环节」
的论点经受住了反方轮。

## 竞品调研记录（2026-09-17，交付前业界对照）

边界：编译型语言函数的入口契约形态、git 部署的 ref 语义、镜像部署的
契约与验证。对象分三层：FaaS 直接竞品（Lambda/Cloud Run/Azure/OpenFaaS）、
相邻实现（Knative、Vercel/Netlify、Coolify）、先例尸检（go1.x 废弃、
classic watchdog → of-watchdog、Fn 停滞）。比较矩阵：

| 维度 | Lambda | Cloud Run | Azure | OpenFaaS | Knative | Vercel/Coolify |
|---|---|---|---|---|---|---|
| Go 入口 | SDK（lambda.Start = 协议客户端库） | n/a（任意容器） | 双轨：custom handler（零 SDK HTTP）+ native worker（SDK） | 模板生成 + `func(w,r)` 签名 + of-watchdog 库同进程 | 容器听 8012（sidecar 转发） | n/a |
| 协议通道 | HTTP Runtime API（provided 后） | HTTP `$PORT` | HTTP host→handler | HTTP（of-watchdog） | HTTP（queue-proxy） | n/a |
| git ref 语义 | n/a | n/a | n/a | n/a | n/a | 钉死 commit（Vercel SHA / Coolify specific commit）+ 子目录 |
| 镜像部署验证 | 运行期 | **部署期 TCP health，失败=部署失败** | n/a | readiness probe | probe 聚合 | 构建即验证 |

**收敛点（业界验证过的默认选项，我们全部在位）**：常驻 HTTP 协议 +
用户二进制/容器自听端口；协议公开 + SDK 只是便利层；git 部署钉死
commit；镜像部署的部署期 health 验证（我们的 HTTP 探测 + 容器日志
尾部回收强于 Cloud Run 的 TCP 探测）。

**分歧点（不同选择的代价）**：Go 入口三派——SDK（Lambda/Fn，适合
用户自带二进制）、零依赖约定（Azure custom handler）、模板生成
（OpenFaaS，适合平台构建用户源码）。我们 = 生成 + 零依赖签名，与
**OpenFaaS golang-middleware 官方模板逐字同款**（`func(w http.
ResponseWriter, r *http.Request)` + 平台协议代码同进程）——场景同构
（自托管、平台构建、用户交源码），裁决获直接佐证。平台组件位置：
sidecar（Knative）依赖 K8s 生态，裸 docker 场景死亡——我们 Non-Goal
禁 sidecar 有场景依据。

**死亡区（已被证伪，我们未踩）**：stdin/stdout 进程协议（go1.x 废弃、
classic watchdog 被取代、Fn 停滞——我们 v2 起即 :18080 HTTP）；平台
深度托管 runtime（go1.x 教训：语言版本断档 + 废弃迁移痛 → OQ3 的
「升级 = 新 ID 不日落」即解法）。

**差评区教训（最便宜的坑）**：Cloud Run 最高频部署错误是「failed to
listen on PORT」——印证验证 spawn + 错误文案直接指向契约文档的必要性
（已在设计）；Lambda bootstrap 命名 gotcha（二进制必须叫 bootstrap）
我们无此暴露面（twmain 对用户不可见）；Vercel monorepo Ignore Build
Step 痛点留给 P3 webhook 时注意（目录级变更检测）。

**借鉴动作（2 项，已并入）**：① 协议文档加「本地测试指引」（契约
基础镜像 `docker run` 本地对照 = Lambda RIE 等效物，BYO 用户推送前
自测）；② OQ3 补 go1.x 废弃论据。**避开清单**：stdin/stdout 协议、
sidecar、runtime 日落。**我们与业界不同的地方及理由**：验证比 Cloud
Run 更强（HTTP + 日志回收 vs TCP）——差异化优势；twmain 服务端生成
对用户不可见（OpenFaaS 是 CLI 生成用户可见骨架）——权衡后接受（编译
错误日志透传 + 参考实现公开兜底，Lambda 用户同样看不到 runtime 内部）。

## 独立复核记录（2026-09-17，三子代理并行：对抗攻击 / 可实现性核对 / 裁决重审）

三视角正交、互不可见。收割与处置（13 处修订已全部并入正文）：

**收敛发现（多方一致，必修已修）**：模板 `COPY go.sum` 缺通配符（无依赖
Go 函数构建必炸，node 模板 `package-lock.json*` 先例在侧——两方独立
发现）；Build 链载荷/接口未定稿（BuildSpec / BuildRequest 字段 /
BuildImageOptions，§0 已收口为「一期第一个 PR 的依据」）；D7 校验位置
（BuildRequest 增 runtime、daemon 侧比对）；config 键位与 Rollout 矛盾
（一期实为 config.proto 两字段）。

**对抗独有（已修）**：A1 条目预算链断裂（dispatcher `ExtractZip` 1000
条上限击毙 5000 条物化 zip——二轮复查只咬合了字节维，漏了条目维；
构建路径注入放宽 limits + vendor 大项目诚实声明）；A2 镜像源 registry
SSRF 基线与 git 自相矛盾（拨号点 IP guard + allow_insecure 对称化）；
A3 四期 reaper 互相残杀（M8 必修：对账按 node 过滤 + 死节点记录收敛
者）；A4 replicated 广播输入与 M5 互斥；A6 AST 探测 `_test.go` 与宿主
模式 GOOS 偏差；A7 v3「多机分发」Non-Goal 推翻未声明；A8 信号量双占 +
TTL 360s 与 7min 链路不咬合（两相化 + TTL 联动）。

**可实现性独有（已修）**：测试策略整节补齐（门控变量名实测为
`TORCHWOOD_FUNCTIONS_DOCKER_HOST`）；api 透传点（oneof 分发 +
mapDeployment 静默漏投影风险）；wire/generate/console embed 装配清单；
worker 补构建 image 分流（幂等 ImportImage）；审计 mask 引用修正
（`audit_payload.go` 而非 secretMask）。

**裁决重审独有（已修）**：公开协议演进宪法（只加不改不删/未知 header
忽略/新字段可选/防御式解析——D5 的约束缺口）；drain 现状比原论述更弱
（恒不触发，D14 论据强化）；M7 replicated 推荐顺序推翻（local →
registry 直进，replicated 降实验档——与 A4 合并处置）；build_timeout
的 Go 冷构建画像标注；四期 worker 拓扑收口提醒；`gorunner` 模板资产
须命名 `*.tmpl`（包目录内字面 main.go 会炸平台构建——实现注意项）。

**判定汇总**：12 条关键裁决 11 维持 1 质疑（M7 顺序，已按质疑改判）；
一至三期骨架三视角均判「可实施」（实现者卡点已由 §0 构建链定稿消解）；
四期原清单被判定「不可照单实施」（A3/A4），已按 M8/降档修订。最脆弱
根基假设（对抗方指出）：「git 物化 zip 落既有路径下游零改动」——已由
A1/A8 证伪其强形式并逐处修正，正文不再以「零改动」作论据（§0 表述
已改为「构建路径复用，预算/信号量/清理分流三处显式改造」见 §2）。

## References

- 现状权威文档：`docs/developer/08-functions.md`（§3 运行时与构建、§4.3.x 执行器演进）
- 常驻/并发/fetch 双轨裁决：`docs/design/functions-v3.md`
- 执行器 v2 分发通路：`docs/design/functions-execution-identity-and-triggers.md` §6
- 运行协议事实源：`internal/infra/functions/runner/runner.js`（头注释即契约全文）
- 模板层：`internal/infra/functions/runner/runner.go`（`DockerfileFor`）
- 构建链：`internal/app/functions/deployments.go`、`functionsdispatcher/daemon.go`、`functionsdispatcher/types.go`
- worker 补构建锚点：`internal/app/functions/executions.go`（zipPath 补构建）
- VCS 远期集成：`docs/roadmap.md` §4.3
