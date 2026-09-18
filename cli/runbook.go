package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/lynx-go/commands"

	"github.com/torchwoodcloud/torchwood/internal/pkg/runbook"
	"github.com/torchwoodcloud/torchwood/sdk/go/server"
)

// newRunbookCmd 是版本化资源迁移命令组（docs/design/runbook.md §2.1）。
// 引擎本体在 internal/pkg/runbook（文件层/编排层/动词对账决策层）；本文件只
// 做命令皮：旗标声明、JSON 输出与生产 RPC 通道适配（invoke → runbook.Caller）。
// RPC 动词 up/down/status/forgive 经 Caller 抽象调用 Server API，new 纯本地、
// 保持免 key。
func newRunbookCmd(g *GlobalFlags) *group {
	return newGroup(g, "runbook", "versioned resource migrations (NNNNNN_name.yaml files under --dir)", func(sub *commands.App) {
		sub.Register(
			newRunbookNewCmd(g),
			newRunbookUpCmd(g),
			newRunbookDownCmd(g),
			newRunbookStatusCmd(g),
			newRunbookForgiveCmd(g),
		)
	})
}

// sdkServerErrorCode 经 SDK 错误分类（grpc status.Code 同源）取 gRPC code 名
// （如 "NotFound"；codes.Code.String() 的 CamelCase 输出）。CLI 不直接 import
// grpc——返回值上调用 String() 无需引入定义包，import 守卫不破。
func sdkServerErrorCode(err error) string {
	return server.ErrorCode(err).String()
}

// newInvokeRunbookCaller 把生产 RPC 通道（invoke → InvokeJSON）适配成引擎
// Caller：响应 protojson 解码为 map（UseNumber 保整型精度）；错误经 SDK 的
// ErrorCode 取 gRPC code 名分类为 *runbook.CallError（引擎据此走
// NotFound/AlreadyExists/FailedPrecondition 收敛分支）。
func newInvokeRunbookCaller(g *GlobalFlags) runbook.Caller {
	return func(method string, req map[string]any) (map[string]any, error) {
		respJSON, err := invoke(g, method, req)
		if err != nil {
			code := ""
			var rpcErr *rpcError
			if errors.As(err, &rpcErr) {
				code = sdkServerErrorCode(rpcErr.cause)
			}
			return nil, runbook.NewCallError(code, err)
		}
		out := map[string]any{}
		if len(bytes.TrimSpace(respJSON)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(respJSON))
			dec.UseNumber()
			if err := dec.Decode(&out); err != nil {
				return nil, fmt.Errorf("decode %s response: %w", shortRunbookMethod(method), err)
			}
		}
		return out, nil
	}
}

// shortRunbookMethod 把全方法名缩为 Service/Method（错误信息可读性）。
func shortRunbookMethod(method string) string {
	parts := strings.Split(strings.TrimPrefix(method, "/"), "/")
	if len(parts) == 2 {
		return parts[0][strings.LastIndex(parts[0], ".")+1:] + "/" + parts[1]
	}
	return method
}

// wrapRunbookUsageError 把引擎的 D14 越界错误转成 UsageError（带 usage 行）。
func wrapRunbookUsageError(v *verb, err error) error {
	var ue *runbook.UsageError
	if errors.As(err, &ue) {
		return &commands.UsageError{Usage: v.usage, Err: ue.Err}
	}
	return err
}

// newRunbookUpCmd 应用到最新/指定版：加载+对账 → 顺序应用（禁跳版）→ 逐动作
// reconcile → RecordRunbookStep（CAS 收敛）。stdout 恒为单个 JSON summary，
// 进度行走 stderr（--quiet 静默）。
func newRunbookUpCmd(g *GlobalFlags) *verb {
	var dir string
	var to int64
	var dryRun, quiet bool
	return newVerb(g, "up", "apply pending runbook steps (ordered, idempotent, CAS-guarded)", "runbook up [--dir <dir>] [--to N] [--dry-run] [--quiet]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dir, "dir", runbook.DefaultDir, "runbook directory")
			fs.Int64Var(&to, "to", -1, "target version (default: latest local file)")
			fs.BoolVar(&dryRun, "dry-run", false, "plan only: reconcile reads run, no writes, no step recording (always exits 0)")
			fs.BoolVar(&quiet, "quiet", false, "suppress progress lines on stderr")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			err := runbook.RunUp(newInvokeRunbookCaller(g), runbook.RunOptions{
				Dir: dir, To: to, ToSet: v.changed("to"), DryRun: dryRun, Quiet: quiet,
			}, env.Stdout, env.Stderr)
			return wrapRunbookUsageError(v, err)
		})
}

// newRunbookDownCmd 回退到指定版：默认回退一步，--all → 0；down 段空的版本
// 不可逆（fail）；delete 动作 NotFound 一律按成功。
func newRunbookDownCmd(g *GlobalFlags) *verb {
	var dir string
	var to int64
	var all, dryRun, quiet bool
	return newVerb(g, "down", "roll back applied runbook steps (default: one step; --all reverts to 0)", "runbook down [--dir <dir>] [--to N] [--all] [--dry-run] [--quiet]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dir, "dir", runbook.DefaultDir, "runbook directory")
			fs.Int64Var(&to, "to", -1, "target version (must be below the current version; 0 reverts everything)")
			fs.BoolVar(&all, "all", false, "revert all applied steps (equivalent to --to 0)")
			fs.BoolVar(&dryRun, "dry-run", false, "plan only: no writes, no step deletion (always exits 0)")
			fs.BoolVar(&quiet, "quiet", false, "suppress progress lines on stderr")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			err := runbook.RunDown(newInvokeRunbookCaller(g), runbook.RunOptions{
				Dir: dir, To: to, ToSet: v.changed("to"), All: all, DryRun: dryRun, Quiet: quiet,
			}, env.Stdout, env.Stderr)
			return wrapRunbookUsageError(v, err)
		})
}

// newRunbookStatusCmd 对账报告（D16）：本地文件 vs 服务端状态；恒退 0（能产出
// 报告就不算失败——目录/加载错误按 D13 退出 1，由错误返回承担）。
func newRunbookStatusCmd(g *GlobalFlags) *verb {
	var dir string
	return newVerb(g, "status", "report local runbook files vs server state (applied/pending/modified/orphan)", "runbook status [--dir <dir>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dir, "dir", runbook.DefaultDir, "runbook directory")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			return runbook.RunStatus(newInvokeRunbookCaller(g), dir, env.Stdout)
		})
}

// newRunbookForgiveCmd 是 D21 逃生门：从顶版起逐个 DeleteRunbookStep 摘到 ≥N
// 全清，不执行任何资源动作；随后重跑 up 即以新 checksum 重放。
func newRunbookForgiveCmd(g *GlobalFlags) *verb {
	var dir string
	return newVerb(g, "forgive", "drop server-side step records >= N without touching resources (checksum escape hatch, D21)", "runbook forgive [--dir <dir>] <version>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dir, "dir", runbook.DefaultDir, "runbook directory")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			version, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return &commands.UsageError{Usage: v.usage, Err: fmt.Errorf("version must be an integer, got %q", args[0])}
			}
			return runbook.RunForgive(newInvokeRunbookCaller(g), dir, version, env.Stdout)
		})
}

// newRunbookNewCmd 生成下一序号迁移骨架（D17）：序号 = 本地最大序号 + 1，
// 与服务端状态无关（骨架生成在引擎包 runbook.Scaffold）。纯本地命令，免
// API key（verb 的 noKey 机制）。
func newRunbookNewCmd(g *GlobalFlags) *verb {
	var dir string
	return newPublicVerb(g, "new", "scaffold the next numbered runbook file (local only, no API key)", "runbook new [--dir <dir>] <name>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dir, "dir", runbook.DefaultDir, "runbook directory (created when missing)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			path, version, err := runbook.Scaffold(dir, args[0])
			if err != nil {
				return err
			}
			return printJSONIndent(env.Stdout, struct {
				File    string `json:"file"`
				Version int64  `json:"version"`
			}{File: path, Version: version})
		})
}
