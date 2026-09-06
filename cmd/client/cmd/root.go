package cmd

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lynx-go/commands"

	"github.com/torchwooddev/torchwood/sdk/go/server"
)

// globalFlags 是贯穿全部 RPC 子命令的全局参数（设计文档 §4.3）。
type globalFlags struct {
	endpoint   string // gRPC 地址（TORCHWOOD_CLI_ENDPOINT）
	apiKey     string // API Key secret（TORCHWOOD_CLI_API_KEY）
	timeout    string // 原始字符串，validate 校验后写入 timeoutDur
	timeoutDur time.Duration
	output     string // 输出格式（MVP 仅 json）
	tls        bool   // 占位：服务端当前为明文 gRPC，使用时报未支持

	defaultsApplied bool // 环境变量缺省只在进程内首次注册时写入（见 register）
}

// envOr 返回环境变量值（非空时），否则回退到默认值。
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// register 把全局旗标挂到动词的 FlagSet——每个动词独立声明（Go flag 语义下
// 旗标须在位置参数之前给出）。flag 包在声明时即把缺省值写入变量，因此环境
// 变量缺省只在进程内首次注册时应用；后续层级（分组→叶子）的再声明以当前值
// 为缺省，保证分组层已解析的全局旗标不被叶子声明覆盖（CLI 单进程单命令，
// 跨命令不复位）。
func (g *globalFlags) register(fs *flag.FlagSet) {
	if !g.defaultsApplied {
		g.endpoint = envOr("TORCHWOOD_CLI_ENDPOINT", "127.0.0.1:9060")
		g.apiKey = envOr("TORCHWOOD_CLI_API_KEY", "")
		g.timeout = envOr("TORCHWOOD_CLI_TIMEOUT", "30s")
		g.output = envOr("TORCHWOOD_CLI_OUTPUT", "json")
		g.defaultsApplied = true
	}
	fs.StringVar(&g.endpoint, "endpoint", g.endpoint, "gRPC 服务地址")
	fs.StringVar(&g.apiKey, "api-key", g.apiKey, "API Key secret（亦可用 TORCHWOOD_CLI_API_KEY 环境变量；health / uuid 除外必填）")
	fs.StringVar(&g.timeout, "timeout", g.timeout, "单次调用超时（如 30s、1m）")
	fs.StringVar(&g.output, "output", g.output, "输出格式（MVP 仅 json）")
	fs.BoolVar(&g.tls, "tls", g.tls, "使用 TLS（占位，暂未支持）")
}

// validate 校验全局参数；needKey 为 false 时豁免 api-key 必填
// （health / uuid / version 等公开或本地命令）。
func (g *globalFlags) validate(needKey bool) error {
	if g.output != "json" {
		return fmt.Errorf("不支持的输出格式 %q：MVP 仅支持 json", g.output)
	}
	d, err := time.ParseDuration(g.timeout)
	if err != nil {
		return fmt.Errorf("无效的 --timeout %q：%v", g.timeout, err)
	}
	g.timeoutDur = d
	if needKey && g.apiKey == "" {
		return fmt.Errorf("缺少 API key：请通过 --api-key 或 TORCHWOOD_CLI_API_KEY 提供（health / uuid 除外）")
	}
	return nil
}

// NewApp 构造根命令表；version 由 main 包 ldflags 注入。
func NewApp(version string) *commands.App {
	g := &globalFlags{}
	app := commands.New()
	app.HelpHeader = `Torchwood CLI 通过 gRPC（非 HTTP gateway）调用 Server API，认证一律使用
x-api-key metadata（scope 见 API Key 的 scopes）。默认连接 127.0.0.1:9060
（服务端 gRPC 仅监听回环，远程使用需走 SSH 隧道或调整 server.grpc.addr）。
全局旗标（--endpoint/--api-key/--timeout/--output/--tls）在任何子命令路径
之后、位置参数之前给出；环境变量 TORCHWOOD_CLI_* 提供缺省。`
	app.HelpFooter = "torchwood " + version
	app.ExitCode = rpcExitCode
	app.Register(
		newHealthCmd(g),
		newVersionCmd(g, version),
		newUUIDCmd(g),
		newProjectsCmd(g),
		newUsersCmd(g),
		newDatabasesCmd(g),
		newGroupsCmd(g),
		newStorageCmd(g),
		newFunctionsCmd(g),
		newOAuthProvidersCmd(g),
		newAdminCmd(g),
		newRPCCmd(g),
	)
	return app
}

// newVersionCmd 打印 CLI 版本（`torchwood` 裸调用的帮助面 HelpFooter 亦带版本串）。
func newVersionCmd(g *globalFlags, version string) *verb {
	return newPublicVerb(g, "version", "打印 CLI 版本", "version", nil, func(v *verb, env *commands.Environment, args []string) error {
		if err := noArgs(v, args); err != nil {
			return err
		}
		_, err := fmt.Fprintln(env.Stdout, "torchwood "+version)
		return err
	})
}

// rpcExitCode 把动词错误映射为脚本可分支的退出码：RPC 错误按 HTTP 类别
// （40x=2、5xx=3、限流 429=4，复用 SDK 的 HTTPErrorClass，CLI 不直接依赖
// grpc）；其余（参数/校验/未知动词等非 RPC 错误）一律 1。
func rpcExitCode(err error) int {
	var re *rpcError
	if errors.As(err, &re) {
		return server.HTTPErrorClass(re.cause)
	}
	return commands.ExitError
}
