package cmd

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/lynx-go/commands"
)

// newRunbookCmd 是版本化资源迁移命令组（docs/design/runbook.md §2.1）。
// 阶段 B 注册本地动词 new；阶段 C（引擎）补齐 up/down/status/forgive——RPC
// 动词经 runbookCaller 抽象调用 Server API（invoke 适配），new 保持免 key。
func newRunbookCmd(g *globalFlags) *group {
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

// wrapRunbookUsageError 把引擎的 D14 越界错误转成 UsageError（带 usage 行）。
func wrapRunbookUsageError(v *verb, err error) error {
	var ue *runbookUsageError
	if errors.As(err, &ue) {
		return &commands.UsageError{Usage: v.usage, Err: ue.err}
	}
	return err
}

// newRunbookUpCmd 应用到最新/指定版：加载+对账 → 顺序应用（禁跳版）→ 逐动作
// reconcile → RecordRunbookStep（CAS 收敛）。stdout 恒为单个 JSON summary，
// 进度行走 stderr（--quiet 静默）。
func newRunbookUpCmd(g *globalFlags) *verb {
	var dir string
	var to int64
	var dryRun, quiet bool
	return newVerb(g, "up", "apply pending runbook steps (ordered, idempotent, CAS-guarded)", "runbook up [--dir <dir>] [--to N] [--dry-run] [--quiet]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dir, "dir", runbookDefaultDir, "runbook directory")
			fs.Int64Var(&to, "to", -1, "target version (default: latest local file)")
			fs.BoolVar(&dryRun, "dry-run", false, "plan only: reconcile reads run, no writes, no step recording (always exits 0)")
			fs.BoolVar(&quiet, "quiet", false, "suppress progress lines on stderr")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			err := runRunbookUp(newInvokeRunbookCaller(g), runbookRunOptions{
				Dir: dir, To: to, ToSet: v.changed("to"), DryRun: dryRun, Quiet: quiet,
			}, env.Stdout, env.Stderr)
			return wrapRunbookUsageError(v, err)
		})
}

// newRunbookDownCmd 回退到指定版：默认回退一步，--all → 0；down 段空的版本
// 不可逆（fail）；delete 动作 NotFound 一律按成功。
func newRunbookDownCmd(g *globalFlags) *verb {
	var dir string
	var to int64
	var all, dryRun, quiet bool
	return newVerb(g, "down", "roll back applied runbook steps (default: one step; --all reverts to 0)", "runbook down [--dir <dir>] [--to N] [--all] [--dry-run] [--quiet]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dir, "dir", runbookDefaultDir, "runbook directory")
			fs.Int64Var(&to, "to", -1, "target version (must be below the current version; 0 reverts everything)")
			fs.BoolVar(&all, "all", false, "revert all applied steps (equivalent to --to 0)")
			fs.BoolVar(&dryRun, "dry-run", false, "plan only: no writes, no step deletion (always exits 0)")
			fs.BoolVar(&quiet, "quiet", false, "suppress progress lines on stderr")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			err := runRunbookDown(newInvokeRunbookCaller(g), runbookRunOptions{
				Dir: dir, To: to, ToSet: v.changed("to"), All: all, DryRun: dryRun, Quiet: quiet,
			}, env.Stdout, env.Stderr)
			return wrapRunbookUsageError(v, err)
		})
}

// newRunbookStatusCmd 对账报告（D16）：本地文件 vs 服务端状态；恒退 0（能产出
// 报告就不算失败——目录/加载错误按 D13 退出 1，由错误返回承担）。
func newRunbookStatusCmd(g *globalFlags) *verb {
	var dir string
	return newVerb(g, "status", "report local runbook files vs server state (applied/pending/modified/orphan)", "runbook status [--dir <dir>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dir, "dir", runbookDefaultDir, "runbook directory")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			return runRunbookStatus(newInvokeRunbookCaller(g), dir, env.Stdout)
		})
}

// newRunbookForgiveCmd 是 D21 逃生门：从顶版起逐个 DeleteRunbookStep 摘到 ≥N
// 全清，不执行任何资源动作；随后重跑 up 即以新 checksum 重放。
func newRunbookForgiveCmd(g *globalFlags) *verb {
	var dir string
	return newVerb(g, "forgive", "drop server-side step records >= N without touching resources (checksum escape hatch, D21)", "runbook forgive <version> [--dir <dir>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dir, "dir", runbookDefaultDir, "runbook directory")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			version, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return &commands.UsageError{Usage: v.usage, Err: fmt.Errorf("version must be an integer, got %q", args[0])}
			}
			return runRunbookForgive(newInvokeRunbookCaller(g), dir, version, env.Stdout)
		})
}

// newRunbookNewCmd 生成下一序号迁移骨架（D17）：序号 = 本地最大序号 + 1，
// 与服务端状态无关；目录不存在则创建——new 承担引导空目录，D13 的「目录
// 必须有合法文件」只约束 up/down/status 的加载路径。纯本地命令，免 API key
// （verb 的 noKey 机制）。
func newRunbookNewCmd(g *globalFlags) *verb {
	var dir string
	return newPublicVerb(g, "new", "scaffold the next numbered runbook file (local only, no API key)", "runbook new [--dir <dir>] <name>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dir, "dir", runbookDefaultDir, "runbook directory (created when missing)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			path, version, err := writeRunbookSkeleton(dir, args[0])
			if err != nil {
				return err
			}
			return printJSONIndent(env.Stdout, struct {
				File    string `json:"file"`
				Version int64  `json:"version"`
			}{File: path, Version: version})
		})
}

// runbookMaxVersion 返回目录内最大序号：只看文件名、不解析内容、不校验链
// （new 只需要下一号）；目录不存在返回 0。
func runbookMaxVersion(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read runbook directory %s: %w", dir, err)
	}
	var max int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if v, _, ok := parseRunbookFileName(entry.Name()); ok && v > max {
			max = v
		}
	}
	return max, nil
}

// writeRunbookSkeleton 生成 NNNNNN_name.yaml 骨架（O_EXCL 不覆盖已有文件），
// 返回（路径, 版本号）。
func writeRunbookSkeleton(dir, name string) (string, int64, error) {
	if !runbookNameRe.MatchString(name) {
		return "", 0, fmt.Errorf("invalid runbook name %q (lowercase letters, digits and '_', 1-64 chars)", name)
	}
	version, err := runbookMaxVersion(dir)
	if err != nil {
		return "", 0, err
	}
	if version >= 999999 {
		return "", 0, fmt.Errorf("runbook version exhausted: the sequence is 6-digit and cannot go past 999999")
	}
	version++
	path := filepath.Join(dir, fmt.Sprintf("%06d_%s.yaml", version, name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("create runbook directory %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", 0, fmt.Errorf("runbook file already exists: %s", path)
		}
		return "", 0, fmt.Errorf("write runbook %s: %w", path, err)
	}
	if _, err := f.WriteString(runbookSkeleton(version, name)); err != nil {
		_ = f.Close()
		return "", 0, fmt.Errorf("write runbook %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", 0, fmt.Errorf("write runbook %s: %w", path, err)
	}
	return path, version, nil
}

// runbookSkeleton 是 `runbook new` 落盘的骨架：注释给出动作写法模板与
// 「down 段为空 = 不可逆」提示（D4/D17）。骨架必须能通过本文件层的严格
// 解析与引擎层的动词 schema 校验——自己生成的文件过不了自己的校验就是
// 自摆乌龙（create_collection 的集合 ID 字段按 CreateCollectionRequest 的
// proto 字段名写 `id`；delete/update 才是 `collection_id`）。
func runbookSkeleton(version int64, name string) string {
	return fmt.Sprintf(`# Runbook step %06d_%s - scaffolded by "torchwood runbook new"; edit below.
# Naming: NNNNNN_name.yaml; versions start at 1 and must stay contiguous.
# One action = exactly one verb key + its request body; body fields mirror the
# matching *Request proto message (camelCase), e.g. create_collection uses "id"
# (CreateCollectionRequest.id) while delete_collection uses "collection_id".
# Example:
#
#   - create_collection:
#       database_id: app
#       id: configs
#       name: configs
#       document_security: true
#       permissions: ['read:users']
#       attributes:
#         - { key: key, type: string, size: 64, required: true }
#       indexes:
#         - { id: key, type: unique, attributes: [key] }
up: []
down: [] # empty down = this step is IRREVERSIBLE; add reverse actions (e.g. delete_collection) to allow rollback
`, version, name)
}
