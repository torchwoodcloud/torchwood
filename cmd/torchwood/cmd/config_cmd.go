package cmd

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/lynx-go/commands"
)

// newConfigCmd 管理 CLI 本地配置文件（~/.torchwood/config.yaml）：profile
// 即项目上下文（endpoint + 该项目 scoped API Key），多 project 各占一个
// profile。这些命令只操作本地文件，不连 Server API，因此不挂全局旗标、
// 不做 api-key 校验（g 传 nil，与 admin 直连命令同构）。
func newConfigCmd() *group {
	return newGroup(nil, "config", "manage the local config file (~/.torchwood/config.yaml: profiles, API keys, default profile)", func(sub *commands.App) {
		sub.Register(
			newConfigPathCmd(),
			newConfigInitCmd(),
			newConfigListCmd(),
			newConfigShowCmd(),
			newConfigSetCmd(),
			newConfigUseCmd(),
			newConfigRemoveCmd(),
		)
	})
}

// newConfigPathCmd 打印配置文件路径（脚本/Agent 定位文件用）。
func newConfigPathCmd() *verb {
	return newPublicVerb(nil, "path", "print the config file path", "config path", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			path, err := configPath()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(env.Stdout, path)
			return err
		})
}

// newConfigInitCmd 创建带注释的初始配置（local profile 指向本机默认端口）；
// 已存在时报错不覆盖——配置里有 API Key，覆盖属于破坏性操作。
func newConfigInitCmd() *verb {
	return newPublicVerb(nil, "init", "create the config file with a starter local profile", "config init", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			path, err := configPath()
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("config file already exists at %s (edit it directly, or remove it first)", path)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("stat config %s: %w", path, err)
			}
			if err := saveConfigFileRaw(path, []byte(configTemplate)); err != nil {
				return err
			}
			_, err = fmt.Fprintf(env.Stdout, "created %s\nnext: torchwood config set local api-key <secret>   (value is stored locally, never printed)\n", path)
			return err
		})
}

// newConfigListCmd 列出全部 profile（API Key 打码；输出 JSON）。
func newConfigListCmd() *verb {
	return newPublicVerb(nil, "list", "list profiles (API keys masked, JSON output)", "config list", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			path, cfg, err := loadConfigOrDefault()
			if err != nil {
				return err
			}
			out := struct {
				ConfigPath string        `json:"configPath"`
				Default    string        `json:"default"`
				Profiles   []profileJSON `json:"profiles"`
			}{ConfigPath: path}
			if cfg != nil {
				out.Default = cfg.Default
				for _, name := range sortedProfileNames(cfg) {
					out.Profiles = append(out.Profiles, newProfileJSON(name, cfg.Profiles[name]))
				}
			} else {
				_, _ = fmt.Fprintln(env.Stderr, "hint: no config file yet — run `torchwood config init`")
			}
			return printJSONIndent(env.Stdout, out)
		})
}

// newConfigShowCmd 显示一个 profile 的落盘字段（API Key 打码）。
func newConfigShowCmd() *verb {
	return newPublicVerb(nil, "show", "show one profile (API key masked, JSON output)", "config show <profile>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			_, cfg, err := loadConfigOrDefault()
			if err != nil {
				return err
			}
			if cfg == nil || cfg.Profiles[args[0]] == nil {
				return fmt.Errorf("profile %q not found (see `torchwood config list`)", args[0])
			}
			return printJSONIndent(env.Stdout, newProfileJSON(args[0], cfg.Profiles[args[0]]))
		})
}

// newConfigSetCmd 写入 profile 字段：config set <profile> <key> <value>。
// api-key 支持 --stdin 从标准输入读值，避免密钥进 shell 历史。
func newConfigSetCmd() *verb {
	var fromStdin bool
	return newPublicVerb(nil, "set", "set a profile field (creates the profile if missing)", `config set [--stdin] <profile> <key> <value>

keys: endpoint | api-key | tls | timeout | output. --stdin (place it
before positionals) reads the value from stdin — use it with api-key to
keep the secret out of shell history.`,
		func(fs *flag.FlagSet) {
			fs.BoolVar(&fromStdin, "stdin", false, "read the value from stdin (for api-key; keeps secrets out of shell history)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			want := 3
			if fromStdin {
				want = 2
			}
			if err := exactArgs(v, args, want); err != nil {
				return err
			}
			name, key := args[0], args[1]
			value := ""
			if fromStdin {
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read value from stdin: %w", err)
				}
				value = trimSpaceBytes(b)
				if value == "" {
					return fmt.Errorf("no value received on stdin")
				}
			} else {
				value = args[2]
			}
			path, err := mutateConfigFile(func(cfg *cliConfig) error {
				created, err := setProfileField(cfg, name, key, value)
				if err != nil {
					return err
				}
				if created {
					_, _ = fmt.Fprintf(env.Stdout, "created profile %q\n", name)
				}
				return nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(env.Stdout, "set %s in profile %q (%s)\n", key, name, path)
			return err
		})
}

// newConfigUseCmd 把指定 profile 设为缺省（必须已存在）。
func newConfigUseCmd() *verb {
	return newPublicVerb(nil, "use", "set the default profile", "config use <profile>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			path, err := mutateConfigFile(func(cfg *cliConfig) error {
				if cfg.Profiles[args[0]] == nil {
					return fmt.Errorf("profile %q not found (profiles: %s)", args[0], cfg.profileList())
				}
				cfg.Default = args[0]
				return nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(env.Stdout, "default profile set to %q (%s)\n", args[0], path)
			return err
		})
}

// newConfigRemoveCmd 删除 profile；删的是缺省 profile 时一并清掉 default
// 键（悬空 default 在加载侧是硬错误，不能留）。
func newConfigRemoveCmd() *verb {
	return newPublicVerb(nil, "remove", "remove a profile", "config remove <profile>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			path, err := mutateConfigFile(func(cfg *cliConfig) error {
				if cfg.Profiles[args[0]] == nil {
					return fmt.Errorf("profile %q not found (profiles: %s)", args[0], cfg.profileList())
				}
				delete(cfg.Profiles, args[0])
				if cfg.Default == args[0] {
					cfg.Default = ""
				}
				return nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(env.Stdout, "removed profile %q (%s)\n", args[0], path)
			return err
		})
}
