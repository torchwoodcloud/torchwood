package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lynx-go/commands"

	"github.com/torchwoodcloud/torchwood/internal/infra/functions/runner"
)

// functions dev（docs/design/functions-v3.md §5.2 本地开发闭环）：
// 复用生产 runner 本体（与镜像模板同源的 go:embed 资产）写盘后以本机 node
// 启动——本地与生产同一 runner，零分叉。热重载 watch index.js mtime（500ms
// 轮询，零依赖），变更即重启子进程；Ctrl-C 优雅回收子进程与临时文件。
//
// 注入通道与生产 runner 契约一致：TW_RUNNER_PORT（监听端口）、
// TW_EXECUTION_TOKEN（--token 显式传入，指向本地 server 实测鉴权链）、
// TW_API_BASE_URL（ctx.apiBaseUrl 来源）、TW_DATA（--data-file 内容，兼容
// v1 同步段读取习惯；常驻 runner 的主通道是 POST body）、--env-file 的
// variables（KEY=VALUE 逐行注入子进程 env）。
const (
	devPollInterval = 500 * time.Millisecond // index.js mtime 轮询间隔（§5.2 一期语义）
	devStopGrace    = 3 * time.Second        // 优雅停机宽限（SIGTERM 后等待再强杀）
)

// devOptions 是 functions dev 的参数载体（旗标解析后装配；测试直接构造）。
type devOptions struct {
	dir        string // 函数目录（须含 index.js；子进程 cwd）
	port       int    // runner 监听端口（TW_RUNNER_PORT）
	token      string // execution token（TW_EXECUTION_TOKEN；可空 = 纯本地 mock）
	apiBaseURL string // TW_API_BASE_URL（ctx.apiBaseUrl 来源；可空）
	dataFile   string // TW_DATA 来源 JSON 文件（可空）
	envFile    string // variables .env 文件（可空）
	nodePath   string // node 可执行文件（空 = PATH 解析）
	pollEvery  time.Duration
	stopGrace  time.Duration
	stdout     io.Writer
	startProc  func(ctx context.Context, nodePath, runnerPath, dir string, env []string) (*exec.Cmd, error)
}

func newFunctionsDevCmd(g *globalFlags) *verb {
	var opts devOptions
	return newPublicVerb(g, "dev", "run the function locally with the production runner (hot reload)", "functions dev [--dir .] [--port 18080] [--token <execution-token>] [--api-base-url <url>] [--data-file <file>] [--env-file <file>] [--node <path>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&opts.dir, "dir", ".", "function directory containing index.js")
			fs.IntVar(&opts.port, "port", runner.RunnerPort, "runner listen port (TW_RUNNER_PORT)")
			fs.StringVar(&opts.token, "token", "", "execution token (TW_EXECUTION_TOKEN; omit for a pure local mock)")
			fs.StringVar(&opts.apiBaseURL, "api-base-url", "", "TW_API_BASE_URL injected into ctx.apiBaseUrl")
			fs.StringVar(&opts.dataFile, "data-file", "", "JSON file whose content is injected as TW_DATA")
			fs.StringVar(&opts.envFile, "env-file", "", "KEY=VALUE lines file injected into the child environment (local variables)")
			fs.StringVar(&opts.nodePath, "node", "", "node executable (default: resolve from PATH)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			opts.pollEvery = devPollInterval
			opts.stopGrace = devStopGrace
			opts.stdout = env.Stdout
			opts.startProc = startNodeProcess
			return runDev(context.Background(), opts) // 生命周期由本命令的信号处理管理
		})
}

// validateFunctionDir 校验函数目录形态：index.js 必须存在（runner 启动即
// 同步 require ./index.js；缺失时 runner 常驻 not-ready，提前拦截体验更好）。
func validateFunctionDir(dir string) error {
	info, err := os.Stat(filepath.Join(dir, "index.js"))
	if err != nil {
		return fmt.Errorf("--dir must contain index.js (%v)", err)
	}
	if info.IsDir() {
		return fmt.Errorf("--dir/index.js is a directory")
	}
	return nil
}

// resolveNodePath 解析 node 可执行文件：显式 --node 优先，其次 PATH。
func resolveNodePath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := exec.LookPath(explicit); err != nil {
			return "", fmt.Errorf("--node %q is not an executable: %v", explicit, err)
		}
		return explicit, nil
	}
	p, err := exec.LookPath("node")
	if err != nil {
		return "", fmt.Errorf("node not found in PATH: install Node.js >= 18 or pass --node")
	}
	return p, nil
}

// parseEnvFile 解析本地 .env（KEY=VALUE 逐行；# 注释与空行忽略；值中不再
// 展开引号——variables 语义保持原样）。零依赖，不复用 godotenv（CLI 不引
// 额外依赖）。
func parseEnvFile(content string) ([][2]string, error) {
	var out [][2]string
	for i, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.Index(trimmed, "=")
		if eq <= 0 {
			return nil, fmt.Errorf("env file line %d: expected KEY=VALUE, got %q", i+1, trimmed)
		}
		key := strings.TrimSpace(trimmed[:eq])
		if key == "" {
			return nil, fmt.Errorf("env file line %d: empty KEY", i+1)
		}
		out = append(out, [2]string{key, strings.TrimSpace(trimmed[eq+1:])})
	}
	return out, nil
}

// buildDevEnv 装配子进程环境（父 env 之上叠加 runner 契约键；TW_* 平台键
// 不允许经 --env-file 伪注入——篡改监听端口/平台契约属配置错误）。
func buildDevEnv(opts devOptions) ([]string, error) {
	// 剥掉父进程残留的 TW_EXECUTION_TOKEN，按 opts 重建（避免上一个函数的
	// 凭证串进本地 mock 会话——functions-v3.md 安全声明 §1 同源教训）。
	env := make([]string, 0, len(os.Environ())+8)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TW_EXECUTION_TOKEN=") {
			continue
		}
		env = append(env, e)
	}
	set := func(k, v string) {
		env = append(env, k+"="+v)
	}
	set("TW_RUNNER_PORT", strconv.Itoa(opts.port))
	if opts.token != "" {
		set("TW_EXECUTION_TOKEN", opts.token)
	}
	if opts.apiBaseURL != "" {
		set("TW_API_BASE_URL", opts.apiBaseURL)
	}
	if opts.dataFile != "" {
		data, err := os.ReadFile(opts.dataFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read --data-file: %v", err)
		}
		set("TW_DATA", string(data))
	}
	if opts.envFile != "" {
		raw, err := os.ReadFile(opts.envFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read --env-file: %v", err)
		}
		kv, err := parseEnvFile(string(raw))
		if err != nil {
			return nil, err
		}
		for _, pair := range kv {
			switch pair[0] {
			case "TW_RUNNER_PORT", "TW_EXECUTION_TOKEN", "TW_API_BASE_URL", "TW_DATA", "TW_MAX_REQUESTS", "TW_DRAIN_TIMEOUT_MS":
				return nil, fmt.Errorf("env file: %s is a platform-reserved key and cannot be set via --env-file", pair[0])
			}
			set(pair[0], pair[1])
		}
	}
	return env, nil
}

// writeRunnerToTemp 把嵌入的 runner 源码写到临时目录（与镜像模板同源资产，
// runner.NodeRunnerJS；进程退出由调用方清理）。
func writeRunnerToTemp() (dir string, err error) {
	dir, err = os.MkdirTemp("", "torchwood-runner-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %v", err)
	}
	p := filepath.Join(dir, "runner.js")
	if err := os.WriteFile(p, runner.NodeRunnerJS(), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("failed to write runner: %v", err)
	}
	return dir, nil
}

// mtimeOf 返回文件修改时间（热重载 watch 的比较键）。
func mtimeOf(path string) (time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// startNodeProcess 启动 node 子进程：cwd = 函数目录（runner 按
// process.cwd()/index.js 加载用户模块），env = buildDevEnv 装配结果。ctx
// 取消时子进程被强杀兜底（优雅停机由 stopProcess 的 SIGTERM/Kill 两段负责）。
func startNodeProcess(ctx context.Context, nodePath, runnerPath, dir string, env []string) (*exec.Cmd, error) {
	// nodePath 经 exec.LookPath 解析（显式 --node 校验过可执行）、runnerPath
	// 是本 CLI 写入临时目录的嵌入资产——无外部输入直连进程面（G204）。
	cmd := exec.CommandContext(ctx, nodePath, runnerPath) // #nosec G204
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd, cmd.Start()
}

// stopProcess 优雅停子进程：SIGTERM（非 Windows）→ 宽限 → Kill。runner 自身
// 对 SIGTERM 走 drain 语义（runner.js gracefulExit）。
func stopProcess(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if runtime.GOOS != "windows" {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
			return
		case <-time.After(grace):
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// runDev 主循环：校验 → 写 runner → 起子进程 → watch index.js → 变更重启。
// Ctrl-C（SIGINT）优雅杀子进程并清理临时目录。
func runDev(ctx context.Context, opts devOptions) error {
	if opts.pollEvery <= 0 {
		opts.pollEvery = devPollInterval
	}
	if opts.stopGrace <= 0 {
		opts.stopGrace = devStopGrace
	}
	if opts.stdout == nil {
		opts.stdout = os.Stdout
	}
	if opts.startProc == nil {
		opts.startProc = startNodeProcess
	}
	if err := validateFunctionDir(opts.dir); err != nil {
		return err
	}
	nodePath, err := resolveNodePath(opts.nodePath)
	if err != nil {
		return err
	}
	childEnv, err := buildDevEnv(opts)
	if err != nil {
		return err
	}
	tmpDir, err := writeRunnerToTemp()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	runnerPath := filepath.Join(tmpDir, "runner.js")
	indexJS := filepath.Join(opts.dir, "index.js")

	start := func() (*exec.Cmd, error) {
		return opts.startProc(ctx, nodePath, runnerPath, opts.dir, childEnv)
	}

	abs, _ := filepath.Abs(opts.dir)
	// banner 输出失败不致命（stdout 仅为提示面），显式弃错满足 errcheck。
	banner := func(format string, a ...any) {
		_, _ = fmt.Fprintf(opts.stdout, format, a...)
	}
	banner("torchwood functions dev (runner template v%d; 比对目标 server 的 RunnerTemplateVersion，functions-v3.md §5.2)\n", runner.TemplateVersion)
	banner("  dir:      %s\n", abs)
	banner("  endpoint: POST http://127.0.0.1:%d/  (body = TW_DATA JSON)\n", opts.port)
	if opts.dataFile != "" {
		banner("  curl:     curl -X POST http://127.0.0.1:%d/ --data @%s\n", opts.port, opts.dataFile)
	} else {
		banner("  curl:     curl -X POST http://127.0.0.1:%d/ -d '{\"hello\":\"world\"}'\n", opts.port)
	}
	banner("  health:   GET  http://127.0.0.1:%d/_tw/health\n", opts.port)
	banner("watching %s (change → restart; Ctrl-C to stop)\n", indexJS)

	var proc *exec.Cmd
	restart := func() error {
		if proc != nil {
			_, _ = fmt.Fprintln(opts.stdout, "restarting runner...")
			stopProcess(proc, opts.stopGrace)
		}
		p, err := start()
		if err != nil {
			return fmt.Errorf("failed to start node: %v", err)
		}
		proc = p
		return nil
	}
	if err := restart(); err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	last, err := mtimeOf(indexJS)
	if err != nil {
		last = time.Time{}
	}
	ticker := time.NewTicker(opts.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-sigs:
			_, _ = fmt.Fprintln(opts.stdout, "\nstopping...")
			stopProcess(proc, opts.stopGrace)
			return nil
		case <-ctx.Done():
			stopProcess(proc, opts.stopGrace)
			return nil
		case <-ticker.C:
			cur, err := mtimeOf(indexJS)
			if err != nil || cur.Equal(last) {
				continue
			}
			last = cur
			if err := restart(); err != nil {
				return err
			}
		}
	}
}
