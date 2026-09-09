// Package runner 承载执行器 v2 的平台 runner 模板资产（常驻执行模型，
// docs/design/functions-execution-identity-and-triggers.md §6；P0.5）。
//
// runner 是镜像模板层：构建镜像时 COPY 进去并作为 CMD（现行模板无
// ENTRYPOINT，用户入口从 CMD 移交 runner）——「构建期不执行用户代码」
// 不变量保持。模板版本化（TemplateVersion）：模板变更必须递增，存量
// deployment 按新模板重建（function_deployments.template_version）。
//
// 本期只交付 node runner；python 留 TODO 占位——v2 路径下 python 部署
// 明确报错（不静默回落 v1 CMD：resident 语义下 v1 CMD 跑完即退，实例
// 永远不会 ready，错误会被推迟到首次调用且形态难排查），python 函数继续
// 走 functions.executor="docker"（v1）路径。
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
// 此处编译期引用防漂移）。
const TemplateVersion = int(domainfunctions.RunnerTemplateVersion)

// NodeRunnerJS 返回嵌入的 node runner 源码（构建镜像时写入 build context）。
func NodeRunnerJS() []byte { return nodeRunnerJS }

// DockerfileFor 生成 v2（runner CMD）运行时 Dockerfile。与 v1 模板
// （internal/infra/functions docker.go dockerfileFor）的差异仅在 CMD：
// 用户入口移交平台 runner，构建期不执行用户代码的不变量不变。
func DockerfileFor(runtime string) (string, error) {
	switch runtime {
	case "node-18.0":
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
