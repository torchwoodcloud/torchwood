package main

import (
	"context"
	"os"

	"github.com/lynx-go/commands"
	lynxbuildinfo "github.com/lynx-go/lynx/buildinfo"
)

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
//
// 版本串：ldflags 由构建脚本注入 lynx 框架级 buildinfo 包（v1.20.0，唯一
// 注入目标，与 cmd/server、cmd/worker 一致）；Display 把 commit/date 拼进
// 人读串（空版本兜底 "dev"），与框架 debug /version 同源同形。此处 import
// buildinfo 叶子包（零依赖）而非 lynx 根包，CLI 二进制不拖入框架。
func run(env *commands.Environment, args []string) int {
	app := NewApp(lynxbuildinfo.BuildInfo{
		Version: lynxbuildinfo.Version,
		Commit:  lynxbuildinfo.Commit,
		Date:    lynxbuildinfo.Date,
	}.Display())
	return app.Run(context.Background(), env, args)
}
