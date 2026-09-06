package main

import (
	"context"
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"github.com/lynx-go/commands"

	"github.com/torchwooddev/torchwood/cmd/client/cmd"
)

// version/commit/date 由 Taskfile build 的 ldflags 注入（与 cmd/server、cmd/worker 一致）。
var version, commit, date string

func main() {
	// 与 cmd/server、cmd/worker 一致：加载仓库根 .env（可选）。
	_ = godotenv.Load()

	app := cmd.NewApp(buildVersion(version, commit, date))
	env := &commands.Environment{Stdout: os.Stdout, Stderr: os.Stderr}
	os.Exit(app.Run(context.Background(), env, os.Args[1:]))
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
