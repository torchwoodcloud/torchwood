package cmd

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/lynx-go/commands"

	"github.com/torchwoodcloud/torchwood/sdk/go/server"
)

// 内建默认值（无 env、无配置 profile 时生效）。
const (
	defaultEndpoint = "127.0.0.1:9060"
	defaultTimeout  = "30s"
	defaultOutput   = "json"
)

// TORCHWOOD_CLI_* 环境变量（优先级介于显式旗标与配置文件 profile 之间）。
const (
	envEndpoint = "TORCHWOOD_CLI_ENDPOINT"
	envAPIKey   = "TORCHWOOD_CLI_API_KEY" //nolint:gosec // 环境变量名，非硬编码凭据
	envTimeout  = "TORCHWOOD_CLI_TIMEOUT"
	envOutput   = "TORCHWOOD_CLI_OUTPUT"
	envTLS      = "TORCHWOOD_CLI_TLS"
)

// globalFlags 是贯穿全部 RPC 子命令的全局参数（设计文档 §4.3）。
type globalFlags struct {
	endpoint   string // gRPC 地址
	apiKey     string // API Key secret
	timeout    string // 原始字符串，validate 校验后写入 timeoutDur
	timeoutDur time.Duration
	output     string // 输出格式（MVP 仅 json）
	tls        bool   // --tls：经 TLS 连接（系统根证书校验；反向代理终结 TLS 场景）
	profile    string // 配置文件中的 profile 名
	version    string // CLI 版本（ldflags 注入；自报 UA 用，非旗标）

	// explicit 记录各旗标在本进程内是否被显式设置（trackedVal 在任一分发
	// 层级解析时打标）：validate 据此落实「显式旗标 > env > profile > 内建
	// 默认」的取值优先级，分组层先解析的显式值也天然存活到叶子。
	explicit struct {
		endpoint, apiKey, timeout, output, tls, profile bool
	}
}

// trackedVal 是全局旗标的取值载体：解析时写入目标变量并打 explicit 标。
// 不用 StringVar/BoolVar 直绑，是因为分组层与叶子层各自声明并解析旗标、
// 共享同一组变量——只有 Set 回调能跨层级区分「显式传入」与「缺省」。
// String() 返回声明时的当前值（供帮助面 default 展示），bool 形态额外
// 实现 IsBoolFlag 以支持 --tls 裸旗标。
type trackedVal struct {
	defVal string
	isBool bool
	set    func(string) error
}

func (v *trackedVal) String() string { return v.defVal }

func (v *trackedVal) Set(s string) error { return v.set(s) }

func (v *trackedVal) IsBoolFlag() bool { return v.isBool }

func trackString(seen *bool, p *string) *trackedVal {
	return &trackedVal{defVal: *p, set: func(s string) error {
		*p = s
		*seen = true
		return nil
	}}
}

func trackBool(seen *bool, p *bool) *trackedVal {
	return &trackedVal{defVal: strconv.FormatBool(*p), isBool: true, set: func(s string) error {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		*p = b
		*seen = true
		return nil
	}}
}

// register 把全局旗标挂到动词的 FlagSet——每个动词独立声明（Go flag 语义下
// 旗标须在位置参数之前给出）。旗标默认值即变量当前值（首层为内建默认；
// 分组层解析到的显式值经共享变量存活到叶子的再声明），env 与配置 profile
// 的注入统一推迟到 validate 完成。
func (g *globalFlags) register(fs *flag.FlagSet) {
	fs.Var(trackString(&g.explicit.endpoint, &g.endpoint), "endpoint", "gRPC server address")
	fs.Var(trackString(&g.explicit.apiKey, &g.apiKey), "api-key", "API key secret (flag/env beat the config file profile)")
	fs.Var(trackString(&g.explicit.timeout, &g.timeout), "timeout", "per-call timeout (e.g. 30s, 1m)")
	fs.Var(trackString(&g.explicit.output, &g.output), "output", "output format (MVP: json only)")
	fs.Var(trackBool(&g.explicit.tls, &g.tls), "tls", "use TLS (system root CAs; for TLS-terminating proxies)")
	fs.Var(trackString(&g.explicit.profile, &g.profile), "profile", "profile in the config file (~/.torchwood/config.yaml; manage via: torchwood config)")
}

// resolveProfile 解析生效的配置 profile：--profile 旗标 > TORCHWOOD_CLI_PROFILE
// > 配置文件 default 键；无配置文件或未选中 profile 时返回零值（不报错，
// 旗标/env 单独使用始终可用）。未命中 profile 名是硬错误（显式指定的东西
// 不能静默吞掉）；default 悬空在 loadConfigFile 的 validate 已按错拦截。
func (g *globalFlags) resolveProfile() (profile, error) {
	var zero profile
	path, err := configPath()
	if err != nil {
		// HOME 无法定位：静默走旗标/env（config 管理命令会如实报错）。
		return zero, nil
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		return zero, err
	}
	if cfg == nil {
		return zero, nil
	}
	name := g.profile
	if !g.explicit.profile {
		if v := os.Getenv(envProfile); v != "" {
			name = v
		}
	}
	if name == "" {
		name = cfg.Default
	}
	if name == "" {
		return zero, nil
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return zero, fmt.Errorf("profile %q not found in %s (profiles: %s)", name, path, cfg.profileList())
	}
	return *p, nil
}

// applyGlobalDefaults 按「显式旗标 > TORCHWOOD_CLI_* env > profile > 内建默认」
// 归一各字段（内建默认已在变量初值里）。显式旗标与环境变量在解析/查 env 时
// 即可判定；profile 值仅在两者都缺席时兜底注入。env 值非法（如 TLS 非布尔）
// 属用户错误，如实报错而非静默忽略。
func (g *globalFlags) applyGlobalDefaults(p profile) error {
	pick := func(explicit bool, envKey, profileVal string, dst *string) {
		switch {
		case explicit:
		case os.Getenv(envKey) != "":
			*dst = os.Getenv(envKey)
		case profileVal != "":
			*dst = profileVal
		}
	}
	pick(g.explicit.endpoint, envEndpoint, p.Endpoint, &g.endpoint)
	pick(g.explicit.apiKey, envAPIKey, p.APIKey, &g.apiKey)
	pick(g.explicit.timeout, envTimeout, p.Timeout, &g.timeout)
	pick(g.explicit.output, envOutput, p.Output, &g.output)
	if !g.explicit.tls {
		if v := os.Getenv(envTLS); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("invalid %s %q: must be a boolean", envTLS, v)
			}
			g.tls = b
		} else if p.TLS {
			g.tls = true
		}
	}
	return nil
}

// validate 校验全局参数；needKey 为 false 时豁免 api-key 必填
// （health / uuid / version 等公开或本地命令）。配置文件在每次执行时读取，
// 错误如实上报（拼错的键、悬空 default 都不能静默跳过）。
func (g *globalFlags) validate(needKey bool) error {
	p, err := g.resolveProfile()
	if err != nil {
		return err
	}
	if err := g.applyGlobalDefaults(p); err != nil {
		return err
	}

	if g.output != "json" {
		return fmt.Errorf("unsupported output format %q: MVP supports json only", g.output)
	}
	d, err := time.ParseDuration(g.timeout)
	if err != nil {
		return fmt.Errorf("invalid --timeout %q: %v", g.timeout, err)
	}
	g.timeoutDur = d
	if needKey && g.apiKey == "" {
		return fmt.Errorf("missing API key: provide it via --api-key, TORCHWOOD_CLI_API_KEY, or the api-key field of a config profile (except for health / uuid)")
	}
	return nil
}

// NewApp 构造根命令表；version 由 main 包 ldflags 注入。
func NewApp(version string) *commands.App {
	g := &globalFlags{endpoint: defaultEndpoint, timeout: defaultTimeout, output: defaultOutput, version: version}
	app := commands.New()
	app.HelpHeader = `Torchwood CLI calls the Server API over gRPC (not the HTTP gateway);
authentication always uses the x-api-key metadata (scopes follow the API
key's scopes). Defaults to 127.0.0.1:9060 (the server gRPC listens on
loopback only; for remote use go through an SSH tunnel, change
server.grpc.addr, or terminate TLS on a reverse proxy that forwards h2c
to the backend and pass --tls).
Global flags (--endpoint/--api-key/--timeout/--output/--tls/--profile) go
after the subcommand path and before positional arguments. Precedence per
flag: explicit flag > TORCHWOOD_CLI_* env var > config file profile
(~/.torchwood/config.yaml, one profile per project — see ` + "`torchwood config`" + `)
> built-in default.`
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
		newAuditLogsCmd(g),
		newConfigCmd(),
		newRPCCmd(g),
	)
	return app
}

// newVersionCmd 打印 CLI 版本（`torchwood` 裸调用的帮助面 HelpFooter 亦带版本串）。
func newVersionCmd(g *globalFlags, version string) *verb {
	return newPublicVerb(g, "version", "print the CLI version", "version", nil, func(v *verb, env *commands.Environment, args []string) error {
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
