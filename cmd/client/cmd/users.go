package cmd

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

const (
	methodUsersList           = "/torchwood.server.v1.UsersService/ListUsers"
	methodUsersGet            = "/torchwood.server.v1.UsersService/GetUser"
	methodUsersCreate         = "/torchwood.server.v1.UsersService/CreateUser"
	methodUsersUpdate         = "/torchwood.server.v1.UsersService/UpdateUser"
	methodUsersUpdatePassword = "/torchwood.server.v1.UsersService/UpdateUserPassword"
	methodUsersDelete         = "/torchwood.server.v1.UsersService/DeleteUser"
	methodUsersListSessions   = "/torchwood.server.v1.UsersService/ListUserSessions"
	methodUsersDeleteSession  = "/torchwood.server.v1.UsersService/DeleteUserSession"
	methodUsersCreateToken    = "/torchwood.server.v1.UsersService/CreateUserToken"
)

// newUsersCmd 覆盖 UsersService 全部 9 个方法：
// list/get/create/update/update-password/delete、sessions list/delete、tokens create。
// 标量参数用具名 flag，labels/prefs 等 Struct 字段用 --data 传入 JSON 合并。
func newUsersCmd(g *globalFlags) *group {
	return newGroup(g, "users", "用户管理（UsersService 全部方法）", func(sub *commands.App) {
		sub.Register(
			newUsersListCmd(g),
			newUsersGetCmd(g),
			newUsersCreateCmd(g),
			newUsersUpdateCmd(g),
			newUsersUpdatePasswordCmd(g),
			newUsersDeleteCmd(g),
			newUsersSessionsCmd(g),
			newUsersTokensCmd(g),
		)
	})
}

func newUsersListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出用户", "users list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodUsersList, listJSON(pageSize, pageToken))
		})
}

func newUsersGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取用户", "users get <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodUsersGet, map[string]any{"id": args[0]})
		})
}

func newUsersCreateCmd(g *globalFlags) *verb {
	var email, password, name, status, data string
	return newVerb(g, "create", "创建用户", "users create --email <e> --password <p>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&email, "email", "", "邮箱（必填）")
			fs.StringVar(&password, "password", "", "密码（必填）")
			fs.StringVar(&name, "name", "", "姓名")
			fs.StringVar(&status, "status", "", "状态（如 active/inactive）")
			fs.StringVar(&data, "data", "", "labels/prefs 等字段的 JSON（与 flag 冲突时以 --data 为准）")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			req, err := buildCreateUserReq(email, password, name, status, data)
			if err != nil {
				return err
			}
			return call(g, env, methodUsersCreate, req)
		})
}

func newUsersUpdateCmd(g *globalFlags) *verb {
	var emailVerified bool
	var name, email, status, data string
	return newVerb(g, "update", "更新用户（仅更新显式传入的字段；清空字段请用 --data）", "users update <id> [--name] [--email] [--status] [--email-verified] [--data]",
		func(fs *flag.FlagSet) {
			fs.BoolVar(&emailVerified, "email-verified", false, "是否已验证邮箱（显式传 --email-verified=true/false 才生效）")
			fs.StringVar(&name, "name", "", "姓名")
			fs.StringVar(&email, "email", "", "邮箱")
			fs.StringVar(&status, "status", "", "状态（如 active/inactive）")
			fs.StringVar(&data, "data", "", "labels/prefs 等字段的 JSON（与 flag 冲突时以 --data 为准）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildUpdateUserReq(v, args[0], emailVerified, name, email, status, data)
			if err != nil {
				return err
			}
			return call(g, env, methodUsersUpdate, req)
		})
}

func newUsersUpdatePasswordCmd(g *globalFlags) *verb {
	var password string
	return newVerb(g, "update-password", "重置用户密码", "users update-password <id> --password <p>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&password, "password", "", "新密码（必填）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			if password == "" {
				return fmt.Errorf("--password 必填")
			}
			return call(g, env, methodUsersUpdatePassword, map[string]any{"id": args[0], "password": password})
		})
}

func newUsersDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除用户", "users delete <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodUsersDelete, map[string]any{"id": args[0]})
		})
}

// newUsersSessionsCmd: users sessions list <id> / delete <id> <session-id>
func newUsersSessionsCmd(g *globalFlags) *group {
	return newGroup(g, "sessions", "用户会话管理", func(sub *commands.App) {
		sub.Register(
			newVerb(g, "list", "列出用户会话", "users sessions list <user-id>", nil,
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 1); err != nil {
						return err
					}
					return call(g, env, methodUsersListSessions, map[string]any{"id": args[0]})
				}),
			newVerb(g, "delete", "删除用户会话", "users sessions delete <user-id> <session-id>", nil,
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 2); err != nil {
						return err
					}
					return call(g, env, methodUsersDeleteSession, map[string]any{"id": args[0], "sessionId": args[1]})
				}),
		)
	})
}

// newUsersTokensCmd: users tokens create <id>
func newUsersTokensCmd(g *globalFlags) *group {
	return newGroup(g, "tokens", "用户令牌管理", func(sub *commands.App) {
		sub.Register(
			newVerb(g, "create", "为用户创建访问/刷新令牌", "users tokens create <user-id>", nil,
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 1); err != nil {
						return err
					}
					return call(g, env, methodUsersCreateToken, map[string]any{"id": args[0]})
				}),
		)
	})
}

// buildCreateUserReq 由 flag 参数构造 CreateUserRequest JSON map；
// --data 以 JSON 覆盖合并（labels/prefs 等 Struct 字段），与 flag 冲突时以 --data 为准。
func buildCreateUserReq(email, password, name, status, data string) (map[string]any, error) {
	if email == "" || password == "" {
		return nil, fmt.Errorf("--email 与 --password 必填")
	}
	req := map[string]any{"email": email, "password": password}
	if name != "" {
		req["name"] = name
	}
	if status != "" {
		req["status"] = status
	}
	if err := mergeJSON(req, data); err != nil {
		return nil, err
	}
	return req, nil
}

// buildUpdateUserReq 构造 UpdateUserRequest JSON map：仅设置显式传入的字段，
// emailVerified 依赖 flag presence（proto3 optional 语义用键存在性表达）。
func buildUpdateUserReq(v *verb, id string, emailVerified bool, name, email, status, data string) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("缺少用户 ID")
	}
	req := map[string]any{"id": id}
	setChanged(v, "email-verified", req, "emailVerified", emailVerified)
	if name != "" {
		req["name"] = name
	}
	if email != "" {
		req["email"] = email
	}
	if status != "" {
		req["status"] = status
	}
	if err := mergeJSON(req, data); err != nil {
		return nil, err
	}
	return req, nil
}
