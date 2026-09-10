// Package runner 承载平台 runner 模板资产（常驻执行模型：v2 P0.5 → v3
// 实例内多路复用 §1.2 → v4 Web 标准 fetch 接口 §2.1，docs/design/
// functions-v3.md）。
//
// runner 是镜像模板层：构建镜像时 COPY 进去并作为 CMD（现行模板无
// ENTRYPOINT，用户入口从 CMD 移交 runner）——「构建期不执行用户代码」
// 不变量保持。模板版本化（TemplateVersion）：模板变更必须递增，存量
// deployment 按新模板重建（function_deployments.template_version）。
//
// 本期只交付 node runner；python 留 TODO 占位——常驻路径下 python 部署
// 明确报错（不静默回落 v1 CMD：resident 语义下 v1 CMD 跑完即退，实例
// 永远不会 ready，错误会被推迟到首次调用且形态难排查），python 函数继续
// 走 functions.executor="docker"（v1）路径。python runner 落地时直接实现
// v3 全部语义（并发 + per-request 基建，模板版本同源，v3 范围裁决）。
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
// 扩展，v3 §2.1–§2.4）。
const TemplateVersion = int(domainfunctions.RunnerTemplateVersion)

// NodeRunnerJS 返回嵌入的 node runner 源码（构建镜像时写入 build context）。
func NodeRunnerJS() []byte { return nodeRunnerJS }

// DockerfileFor 生成 v2（runner CMD）运行时 Dockerfile。与 v1 模板
// （internal/infra/functions docker.go dockerfileFor）的差异仅在 CMD：
// 用户入口移交平台 runner，构建期不执行用户代码的不变量不变。
//
// node 分支支持平台代装依赖（v3 §3.1/D11，functions-v3.md）：nodeDeps=true
// （zip 根 package.json dependencies 非空，探测在 zip 校验层
// ExtractZip）时改用经典分层模板——先 COPY 清单并 npm ci，再 COPY 全部代码，
// lockfile 不变即命中 Docker 层缓存（二次部署免费获得增量构建）；lockfile
// 强制在此决策。无依赖函数维持一次性 COPY 模板（零变化、不白跑 npm ci）。
// 模板语义变化不 bump 模板版本：构建管道变化、产物等价（CMD/ENV 不动）。
func DockerfileFor(runtime string, nodeDeps, hasLockfile bool) (string, error) {
	switch runtime {
	case "node-18.0":
		if nodeDeps {
			// lockfile 强制（v3 §3.1）：无锁安装不可复现，与「构建是平台
			// 确定性操作」不变量对齐。
			if !hasLockfile {
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
	case "python-3.11":
		// TODO(P1+)：python runner（常驻 WSGI/ASGI 形态）。v2 路径下 python
		// 部署明确报错（见包注释），函数暂走 v1 docker executor。
		return "", fmt.Errorf("runtime %q is not supported by the resident executor (v2, node only); use functions.executor=\"docker\" for python functions", runtime)
	default:
		return "", fmt.Errorf("unsupported runtime %q", runtime)
	}
}
