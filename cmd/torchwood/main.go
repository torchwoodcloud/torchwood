package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lynx-go/commands"
)

// version/commit/date 由 mise run build 的 ldflags 注入（与 cmd/server、cmd/worker 一致）。
var version, commit, date string

func main() {
	os.Exit(run(&commands.Environment{Stdout: os.Stdout, Stderr: os.Stderr}, os.Args[1:]))
}

// run 装配并执行 CLI，返回进程退出码（与 main 分离以便测试直呼）。
//
// 安全边界（与 cmd/server、cmd/worker 刻意不同）：CLI 不加载 cwd 的 .env。
// server 等常驻入口加载的是部署环境自己的 .env，属合理便利；而 CLI 常在
// 不可信仓库目录运行（runbook / Agent 工作流），自动加载 cwd .env 会让恶意
// 仓库借 TORCHWOOD_CLI_ENDPOINT 把 profile 内的 API Key 以 x-api-key 重定向
// 到攻击者端点（TORCHWOOD_DATA_DATABASE_SOURCE 等 DSN 同理可被劫持）。
// TORCHWOOD_CLI_* 环境变量需用户自行 export（或在 shell 里 set -a; source
// .env; set +a），取值优先级不变（显式旗标 > env > profile > 内建默认）。
// 守卫测试：TestMainNoDotEnvAutoLoad。
func run(env *commands.Environment, args []string) int {
	app := NewApp(buildVersion(version, commit, date))
	return app.Run(context.Background(), env, args)
}

// buildVersion 把 ldflags 注入的 commit/date 拼进版本串，避免元数据被丢弃；
// 未注入时保持原值（如 "dev"）。
func buildVersion(v, commitHash, builtAt string) string {
	if v == "" {
		v = "dev"
	}
	if commitHash == "" && builtAt == "" {
		return v
	}
	if builtAt == "" {
		return fmt.Sprintf("%s (commit %s)", v, commitHash)
	}
	if commitHash == "" {
		return fmt.Sprintf("%s (built %s)", v, builtAt)
	}
	return fmt.Sprintf("%s (commit %s, built %s)", v, commitHash, builtAt)
}
