package cmd

import (
	"context"
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

// verb 是叶子动词的统一载体（commands.Command + Flagged）：声明式元信息 +
// 旗标声明与执行钩子，统一承担全局旗标注册（g 非 nil 时）、全局参数校验与
// FlagSet 捕获（presence 判断，支撑 proto3 optional 的"显式传入才生效"）。
type verb struct {
	name     string
	synopsis string
	usage    string
	g        *globalFlags // 非 nil：注册全局旗标并在 Run 前校验（直连 DB 的 admin 命令为 nil）
	noKey    bool         // 豁免 api-key 必填（health/uuid/version 等公开或本地命令）
	flags    func(fs *flag.FlagSet)
	run      func(v *verb, env *commands.Environment, args []string) error
	fs       *flag.FlagSet // SetFlags 时捕获，Run 里供 changed() 判断
}

func (v *verb) Name() string     { return v.name }
func (v *verb) Synopsis() string { return v.synopsis }
func (v *verb) Usage() string    { return v.usage }

// SetFlags 由框架在每次分发时以全新 FlagSet 调用：先挂全局旗标，再挂动词
// 自有旗标，最后捕获 FlagSet 供 Run 判断 presence。
func (v *verb) SetFlags(fs *flag.FlagSet) {
	if v.g != nil {
		v.g.register(fs)
	}
	if v.flags != nil {
		v.flags(fs)
	}
	v.fs = fs
}

func (v *verb) Run(_ context.Context, env *commands.Environment, args []string) error {
	if v.g != nil {
		if err := v.g.validate(!v.noKey); err != nil {
			return err
		}
	}
	return v.run(v, env, args)
}

// changed 报告旗标是否被显式设置（等价迁移前 cobra 的 Flags().Changed）。
func (v *verb) changed(name string) bool {
	found := false
	if v.fs != nil {
		v.fs.Visit(func(f *flag.Flag) {
			if f.Name == name {
				found = true
			}
		})
	}
	return found
}

// newVerb 构造需 API key 的 RPC 叶子动词（usage 建议带完整命令路径，
// 供 help/-h 的 usage 行使用）。
func newVerb(g *globalFlags, name, synopsis, usage string, flags func(fs *flag.FlagSet), run func(v *verb, env *commands.Environment, args []string) error) *verb {
	return &verb{name: name, synopsis: synopsis, usage: usage, g: g, flags: flags, run: run}
}

// newPublicVerb 构造无需 API key 的叶子动词（health/uuid/version）。
func newPublicVerb(g *globalFlags, name, synopsis, usage string, flags func(fs *flag.FlagSet), run func(v *verb, env *commands.Environment, args []string) error) *verb {
	v := newVerb(g, name, synopsis, usage, flags, run)
	v.noKey = true
	return v
}

// group 是分组动词：携带内层 App 复用同一套分发机器。分组层只注册全局旗标、
// 不做校验——校验归叶子（对齐迁移前 cobra PersistentPreRunE 仅在叶子执行）。
type group struct {
	name     string
	synopsis string
	usage    string
	g        *globalFlags
	sub      *commands.App
}

func (g *group) Name() string     { return g.name }
func (g *group) Synopsis() string { return g.synopsis }
func (g *group) Usage() string    { return g.usage }

func (g *group) SetFlags(fs *flag.FlagSet) {
	if g.g != nil {
		g.g.register(fs)
	}
}

func (g *group) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		// 裸分组命令：打子命令帮助面，退出 0（对齐迁移前 cobra 行为）。
		g.sub.Run(ctx, env, nil)
		return nil
	}
	return g.sub.SubDispatch(ctx, env, args)
}

// newGroup 构造分组动词；register 在内层 App 上登记子动词。
func newGroup(g *globalFlags, name, synopsis string, register func(sub *commands.App)) *group {
	sub := commands.New()
	register(sub)
	return &group{name: name, synopsis: synopsis, usage: name + " <subcommand> [flags]", g: g, sub: sub}
}

// exactArgs 校验位置参数个数（等价迁移前 cobra.ExactArgs）：报 UsageError
// 附 usage 行；退出码由 app.ExitCode 钩子归一为 1，对齐迁移前契约。
func exactArgs(v *verb, args []string, n int) error {
	if len(args) != n {
		return &commands.UsageError{Usage: v.usage, Err: fmt.Errorf("需要 %d 个位置参数，收到 %d 个", n, len(args))}
	}
	return nil
}

// noArgs 拒绝一切位置参数（等价迁移前 cobra.NoArgs）。
func noArgs(v *verb, args []string) error {
	if len(args) != 0 {
		return &commands.UsageError{Usage: v.usage, Err: fmt.Errorf("不接受位置参数：%v", args)}
	}
	return nil
}
