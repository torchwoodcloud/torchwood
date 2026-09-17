// Package runner 承载平台 runner 模板资产（常驻执行模型：v2 P0.5 → v3
// 实例内多路复用 §1.2 → v4 Web 标准 fetch 接口 §2.1，docs/design/
// functions-v3.md）。
//
// runner 是镜像模板层：构建镜像时 COPY 进去并作为 CMD（现行模板无
// ENTRYPOINT，用户入口从 CMD 移交 runner）——「构建期不执行用户代码」
// 不变量保持。模板版本化（TemplateVersion）：模板变更必须递增，存量
// deployment 按新模板重建（function_deployments.template_version）。
//
// 本包保持叶子资产包形态（CLI functions dev 与 dispatcher 双
// 消费方），不 import infra/functions 根包：DockerfileFor 的入参载体
// SourceContents 在本包定义，探测层产物（infrafunctions.SourceContents）
// 由调用方逐字段映射，杜绝潜在 import 环。Go 模板资产（twmain bootstrap
// 源码模板与渲染器）在子包 gorunner——它同样不回溯 import 本包与根包。
//
// python 探测保留但构建期明确报错（不静默构建一个跑不起来的镜像——
// resident 语义下一次性 CMD 跑完即退，实例永远不会 ready，错误会被推迟到
// 首次调用且形态难排查；v1 docker 执行器已移除，无回退路径）。python
// runner 落地时直接实现 v3 全部语义（并发 + per-request 基建，模板版本
// 同源，v3 范围裁决）。
package runner

import (
	_ "embed"
	"fmt"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
)

//go:embed runner.js
var nodeRunnerJS []byte

// RunnerFileName 是 runner 在镜像内 go:embed COPY 后的文件名（点前缀避免
// 与用户代码撞名；`COPY . .` 从 build context 带入）。
const RunnerFileName = ".tw-runner.js"

// RunnerPort 是 runner 在容器内监听的 HTTP 端口（避开常见服务端口冲突；
// 仅 per-project 桥网络内可达，无外部暴露）。
const RunnerPort = 18080

// TemplateVersion 是 runner 镜像模板版本（runner.js 或 Dockerfile 模板任何
// 语义变更都必须递增；单一事实源为 domainfunctions.RunnerTemplateVersion，
// 此处编译期引用防漂移。v4 = fetch 入口探测/触发器封套还原/Response 封套
// 扩展，v3 §2.1–§2.4；v5 = ctx/env 调用身份三件
// source/invokingUserId/projectId，mlbridge fn-rpc 设计 §2.5）。
// Go 分支新增（go-1.26 模板）不改存量 node 语义，按 D5/OQ2 裁决不 bump；
// Go runner 协议实现（gorunner 资产）与本常量同源——双实现同版本纪律：
// 任一实现的语义变更必须同步另一实现并递增同一常量。
const TemplateVersion = int(domainfunctions.RunnerTemplateVersion)

// NodeRunnerJS 返回嵌入的 node runner 源码（构建镜像时写入 build context）。
func NodeRunnerJS() []byte { return nodeRunnerJS }

// SourceContents 是 DockerfileFor 的入参载体：部署源探测结果的模板投影，
// 字段与探测层产物（infrafunctions.SourceContents）一一对应。探测层只产出
// 标记；报错收敛在本包（缺 go.sum / twmain 冲突拒收），与 node 缺 lockfile
// 同构——构建期错误统一从 DockerfileFor 冒出。
type SourceContents struct {
	// Runtime 是运行时 ID（node-18.0 / go-1.26；python-3.11 仅探测保留）。
	Runtime string
	// NodeDeps 表示 zip 根 package.json 的 dependencies 键非空。
	NodeDeps bool
	// HasLockfile 表示 zip 根含 package-lock.json。
	HasLockfile bool
	// GoModulePath 是 zip 根 go.mod 的 module 行路径（探测层已剥引号/注释）。
	GoModulePath string
	// GoHasRequires 表示 go.mod 存在非空 require（单行或块形态）。
	GoHasRequires bool
	// GoHasSum 表示 zip 根含 go.sum。
	GoHasSum bool
	// HasVendor 表示 zip 根存在 vendor/ 目录（受纳；存在时 go.sum 不作要求）。
	HasVendor bool
	// TwmainConflict 表示 zip 根存在 twmain/ 前缀条目（平台保留目录名）。
	TwmainConflict bool
}

// DockerfileFor 生成常驻执行模型的运行时 Dockerfile（唯一执行路径，经
// dispatcher 构建）：用户入口移交平台 runner（CMD），
// 构建期不执行用户代码的不变量由此保持。
//
// node 分支支持平台代装依赖（v3 §3.1/D11，functions-v3.md）：nodeDeps=true
// （zip 根 package.json dependencies 非空，探测在 zip 校验层
// ExtractZip）时改用经典分层模板——先 COPY 清单并 npm ci，再 COPY 全部代码，
// lockfile 不变即命中 Docker 层缓存（二次部署免费获得增量构建）；lockfile
// 强制在此决策。无依赖函数维持一次性 COPY 模板（零变化、不白跑 npm ci）。
// 模板语义变化不 bump 模板版本：构建管道变化、产物等价（CMD/ENV 不动）。
//
// go 分支（Go 一期，设计 functions-runtimes-and-sources.md §1）多阶段构建：
// 构建段 golang:1.26-alpine（CGO_ENABLED=0 结构化消灭 #cgo/pkg-config 构建
// 期命令执行面；GOFLAGS 按 vendor 探测分支——显式 -mod=readonly 会覆盖
// 「vendor 目录存在时自动 -mod=vendor」的默认行为，HasVendor=true 时必须
// 显式 -mod=vendor），运行段 alpine + ca-certificates（函数 HTTPS 出访需要
// CA 证书而基础 alpine 不自带）+ 非 root 数字 UID。go.sum 通配
// （`go.sum*`）：纯 stdlib 函数合法无 go.sum，COPY 任一源缺失即失败——
// node 模板 package-lock.json* 先例同构。go build 只编译不执行（Go modules
// 无 npm 生命周期脚本等价物），「构建期不执行用户代码」强于 node。
func DockerfileFor(contents SourceContents) (string, error) {
	switch contents.Runtime {
	case "node-18.0":
		if contents.NodeDeps {
			// lockfile 强制（v3 §3.1）：无锁安装不可复现，与「构建是平台
			// 确定性操作」不变量对齐。
			if !contents.HasLockfile {
				return "", fmt.Errorf("检测到 dependencies 但缺少 package-lock.json——请提交 lockfile 以保证确定性构建（npm install 会生成）")
			}
			// --ignore-scripts 恒定（v3 §3.2/D11：一期不提供 opt-in）。不变量：
			// 构建期不执行用户代码/第三方脚本（npm 生命周期脚本如 postinstall
			// 可执行任意代码含出网）。残余风险声明：--ignore-scripts 不消除供应
			// 链面本身——lockfile 是用户可控输入，npm 解析器漏洞仍可能在构建
			// 容器内执行代码；缓解 = lockfile integrity hash 固定 + 构建容器
			// 既有 hardening（非 root、无 sock、资源限额），见 functions-v3.md
			// §3.2。代价：依赖原生编译/postinstall 下载二进制的包不可用（如
			// esbuild/swc 安装版），文档明示（docs/developer/08-functions.md §3）。
			return "FROM node:18-alpine\n" +
				"WORKDIR /app\n" +
				"COPY package.json package-lock.json* ./\n" +
				"RUN npm ci --omit=dev --ignore-scripts\n" +
				"COPY . .\n" +
				"USER node\n" +
				fmt.Sprintf("ENV TW_RUNNER_PORT=%d\n", RunnerPort) +
				fmt.Sprintf("CMD [\"node\",%q]\n", RunnerFileName), nil
		}
		return "FROM node:18-alpine\n" +
			"WORKDIR /app\n" +
			"COPY . .\n" +
			"USER node\n" +
			fmt.Sprintf("ENV TW_RUNNER_PORT=%d\n", RunnerPort) +
			fmt.Sprintf("CMD [\"node\",%q]\n", RunnerFileName), nil
	case "go-1.26":
		// twmain/ 保留目录冲突拒收（设计 §1「错误文案指明改名」）：用户 zip
		// 携带同名目录会与平台构建期生成的 bootstrap 撞包。
		if contents.TwmainConflict {
			return "", fmt.Errorf("请勿在代码包中携带 twmain/ 目录——该目录为平台保留（构建期生成 Go runner bootstrap），请改名后重试")
		}
		// 依赖确定性（对齐 node lockfile 强制口径）：require 非空且无 go.sum
		// 且无 vendor → 拒收（无锁依赖不可复现）；vendor 存在时 go build
		// -mod=vendor 不消费 go.sum，不作要求（D4）。
		if contents.GoHasRequires && !contents.GoHasSum && !contents.HasVendor {
			return "", fmt.Errorf("检测到外部依赖但缺少 go.sum——go mod tidy 生成后提交")
		}
		goFlags := "-mod=readonly"
		downloadLayer := "RUN go mod download\n"
		if contents.HasVendor {
			// GOFLAGS 显式 -mod=vendor：否则显式 -mod=readonly 会覆盖 Go
			// 「vendor 目录存在时自动切换」的默认行为，vendor 形同虚设
			// （二轮复查修正）；vendor 分支免 go mod download 层。
			goFlags = "-mod=vendor"
			downloadLayer = ""
		}
		return "FROM golang:1.26-alpine AS build\n" +
			"WORKDIR /src\n" +
			"ENV CGO_ENABLED=0\n" +
			fmt.Sprintf("ENV GOFLAGS=%s\n", goFlags) +
			"COPY go.mod go.sum* ./\n" +
			downloadLayer +
			"COPY . .\n" +
			"COPY twmain/ ./twmain/\n" +
			"RUN go build -trimpath -ldflags=\"-s -w\" -o /out/tw-app ./twmain\n" +
			"\n" +
			"FROM alpine:3.22\n" +
			"RUN apk add --no-cache ca-certificates\n" +
			"COPY --from=build /out/tw-app /tw-app\n" +
			"USER 65534:65534\n" +
			fmt.Sprintf("ENV TW_RUNNER_PORT=%d\n", RunnerPort) +
			"CMD [\"/tw-app\"]\n", nil
	case "python-3.11":
		// TODO(P1+)：python runner（常驻 WSGI/ASGI 形态）。python 探测保留、
		// 构建期明确报错（见包注释）——无 v1 回退路径，报错不得引导用户切换
		// 执行器。
		return "", fmt.Errorf("runtime %q is not available: the resident executor is node-only (python runner not implemented yet)", contents.Runtime)
	default:
		return "", fmt.Errorf("unsupported runtime %q", contents.Runtime)
	}
}
