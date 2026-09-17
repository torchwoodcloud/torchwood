package main

import (
	"log"
	"time"

	"github.com/joho/godotenv"
	"github.com/lynx-go/lynx"
	lynxzap "github.com/lynx-go/lynx/contrib/zap"
	"github.com/spf13/pflag"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

var version, commit, date string

// packer 是 git 部署源的独立打包进程（二期阶段二，owner 裁决）：
// 专职承载不可信 git 输入的重资源操作（浅克隆 + worktree 核算 + 子目录
// 物化为 zip），把 url@ref[:directory] 归一为与 zip 源同构的代码包交回
// server 落既有构建路径；server/worker 零 git 流量，clone 的内存尖峰/
// 磁盘消耗全部收敛在本进程（重启即恢复，API/函数执行/node 构建无感）。
// 与 dispatcher 同模式：无 Redis/DB/docker 依赖，配置共 schema
// （server 只消费 functions.packer.url/shared_token，本进程消费其余）。
func main() {
	_ = godotenv.Load()

	// commit/date 注入时拼进版本串，便于日志定位构建来源。
	buildVersion := version
	if commit != "" {
		buildVersion = version + " (" + commit + ")"
	}
	if date != "" {
		buildVersion += " built " + date
	}

	runner := lynx.NewRunner(func(app lynx.App) error {
		app.SetLogger(lynxzap.MustNewLogger(app))

		bootstrap, cleanup, err := wireBootstrap(app)
		if err != nil {
			return err
		}
		// cleanup 挂 OnPostStop（lynx v1.10.0，与 server/worker/dispatcher
		// 同约定）：所有服务停止后执行，自带预算。
		app.OnPostStop(cleanup)
		bootstrap.Apply(app)
		return nil
	},
		lynx.WithName("Torchwood Functions Packer"),
		lynx.WithVersion(buildVersion),
		lynx.WithBindFlagsFunc(func(f *pflag.FlagSet) {
			f.String("config-dir", "./configs", "config file path")
			f.String("log-level", "info", "log level")
		}),
		lynx.WithBindConfigFunc(config.NewBindConfigFunc()),
		// 内网 API 面（无 LB 摘流需求）：packer 无状态，在途 pack 请求由
		// 调用方整请求重试，短排水即可。
		lynx.WithShutdownTimeout(30*time.Second),
	)

	if err := runner.RunE(); err != nil {
		log.Fatalln(err)
	}
}
