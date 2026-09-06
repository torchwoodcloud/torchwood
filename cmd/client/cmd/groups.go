package cmd

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

const (
	methodGroupsCreate                 = "/torchwood.server.v1.GroupsService/CreateGroup"
	methodGroupsList                   = "/torchwood.server.v1.GroupsService/ListGroups"
	methodGroupsGet                    = "/torchwood.server.v1.GroupsService/GetGroup"
	methodGroupsDelete                 = "/torchwood.server.v1.GroupsService/DeleteGroup"
	methodGroupsGetPrefs               = "/torchwood.server.v1.GroupsService/GetGroupPrefs"
	methodGroupsUpdatePrefs            = "/torchwood.server.v1.GroupsService/UpdateGroupPrefs"
	methodGroupsCreateMembership       = "/torchwood.server.v1.GroupsService/CreateMembership"
	methodGroupsListMemberships        = "/torchwood.server.v1.GroupsService/ListMemberships"
	methodGroupsGetMembership          = "/torchwood.server.v1.GroupsService/GetMembership"
	methodGroupsUpdateMembership       = "/torchwood.server.v1.GroupsService/UpdateMembership"
	methodGroupsUpdateMembershipStatus = "/torchwood.server.v1.GroupsService/UpdateMembershipStatus"
	methodGroupsDeleteMembership       = "/torchwood.server.v1.GroupsService/DeleteMembership"
)

// newGroupsCmd 覆盖 GroupsService 全部 12 个方法：
// 用户组（create/list/get/delete）、prefs（get/update）、
// memberships（create/list/get/update/update-status/delete）。
func newGroupsCmd(g *globalFlags) *group {
	return newGroup(g, "groups", "用户组管理（GroupsService 全部方法）", func(sub *commands.App) {
		sub.Register(
			newGroupsCreateCmd(g),
			newGroupsListCmd(g),
			newGroupsGetCmd(g),
			newGroupsDeleteCmd(g),
			newGroupsPrefsCmd(g),
			newGroupsMembershipsCmd(g),
		)
	})
}

func newGroupsCreateCmd(g *globalFlags) *verb {
	var name, permissions string
	return newVerb(g, "create", "创建用户组", "groups create --name <name>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "用户组名称（必填）")
			fs.StringVar(&permissions, "permissions", "", "权限 JSON 数组")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			req, err := buildCreateGroupReq(name, permissions)
			if err != nil {
				return err
			}
			return call(g, env, methodGroupsCreate, req)
		})
}

func newGroupsListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出用户组", "groups list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodGroupsList, listJSON(pageSize, pageToken))
		})
}

func newGroupsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取用户组", "groups get <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodGroupsGet, map[string]any{"id": args[0]})
		})
}

func newGroupsDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除用户组", "groups delete <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodGroupsDelete, map[string]any{"id": args[0]})
		})
}

// newGroupsPrefsCmd: groups prefs get <id> / update <id> --data。
func newGroupsPrefsCmd(g *globalFlags) *group {
	return newGroup(g, "prefs", "用户组偏好管理", func(sub *commands.App) {
		sub.Register(
			newVerb(g, "get", "获取用户组偏好", "groups prefs get <id>", nil,
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 1); err != nil {
						return err
					}
					return call(g, env, methodGroupsGetPrefs, map[string]any{"id": args[0]})
				}),
			newGroupsPrefsUpdateCmd(g),
		)
	})
}

func newGroupsPrefsUpdateCmd(g *globalFlags) *verb {
	var data string
	return newVerb(g, "update", "全量替换用户组偏好（--data 为 prefs 对象本身）", "groups prefs update <id> --data '{...}'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&data, "data", "", "prefs JSON 对象（必填，如 '{\"theme\":\"dark\"}'）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildUpdateGroupPrefsReq(args[0], data)
			if err != nil {
				return err
			}
			return call(g, env, methodGroupsUpdatePrefs, req)
		})
}

// newGroupsMembershipsCmd: groups memberships create/list/get/update/
// update-status/delete。
func newGroupsMembershipsCmd(g *globalFlags) *group {
	return newGroup(g, "memberships", "用户组成员管理", func(sub *commands.App) {
		sub.Register(
			newGroupsMembershipsCreateCmd(g),
			newGroupsMembershipsListCmd(g),
			newGroupsMembershipsGetCmd(g),
			newGroupsMembershipsUpdateCmd(g),
			newGroupsMembershipsUpdateStatusCmd(g),
			newGroupsMembershipsDeleteCmd(g),
		)
	})
}

func newGroupsMembershipsCreateCmd(g *globalFlags) *verb {
	var userID, email, name, roles, status string
	return newVerb(g, "create", "创建用户组成员（--user-id 或 --email 至少一个）", "groups memberships create <group-id> [--user-id <uid> | --email <email>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&userID, "user-id", "", "用户 ID（已注册用户）")
			fs.StringVar(&email, "email", "", "邮箱（邀请未注册用户）")
			fs.StringVar(&name, "name", "", "成员姓名（邮箱邀请时使用）")
			fs.StringVar(&roles, "roles", "", "角色 JSON 数组（如 '[\"admin\"]'）")
			fs.StringVar(&status, "status", "", "状态（pending/active/blocked）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildCreateMembershipReq(args[0], userID, email, name, roles, status)
			if err != nil {
				return err
			}
			return call(g, env, methodGroupsCreateMembership, req)
		})
}

func newGroupsMembershipsListCmd(g *globalFlags) *verb {
	var queries string
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出用户组成员", "groups memberships list <group-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&queries, "queries", "", "Appwrite 风格查询 JSON 数组")
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildListMembershipsReq(args[0], queries, pageSize, pageToken)
			if err != nil {
				return err
			}
			return call(g, env, methodGroupsListMemberships, req)
		})
}

func newGroupsMembershipsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取用户组成员", "groups memberships get <group-id> <membership-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodGroupsGetMembership, map[string]any{"groupId": args[0], "membershipId": args[1]})
		})
}

func newGroupsMembershipsUpdateCmd(g *globalFlags) *verb {
	var roles string
	return newVerb(g, "update", "全量替换成员角色", "groups memberships update <group-id> <membership-id> --roles '[...]'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&roles, "roles", "", "角色 JSON 数组（必填，全量替换）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildUpdateMembershipReq(args[0], args[1], roles)
			if err != nil {
				return err
			}
			return call(g, env, methodGroupsUpdateMembership, req)
		})
}

func newGroupsMembershipsUpdateStatusCmd(g *globalFlags) *verb {
	var status string
	return newVerb(g, "update-status", "更新成员状态（active/blocked；pending 不可回退）", "groups memberships update-status <group-id> <membership-id> --status <status>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&status, "status", "", "目标状态（必填：active/blocked）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildUpdateMembershipStatusReq(args[0], args[1], status)
			if err != nil {
				return err
			}
			return call(g, env, methodGroupsUpdateMembershipStatus, req)
		})
}

func newGroupsMembershipsDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除用户组成员", "groups memberships delete <group-id> <membership-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodGroupsDeleteMembership, map[string]any{"groupId": args[0], "membershipId": args[1]})
		})
}

// buildCreateGroupReq 构造 CreateGroupRequest（name 必填）。
func buildCreateGroupReq(name, permissions string) (map[string]any, error) {
	if name == "" {
		return nil, fmt.Errorf("--name 必填")
	}
	req := map[string]any{"name": name}
	if permissions != "" {
		perms, err := jsonStringList(permissions, "--permissions")
		if err != nil {
			return nil, err
		}
		req["permissions"] = perms
	}
	return req, nil
}

// buildUpdateGroupPrefsReq 构造 UpdateGroupPrefsRequest（--data 为 prefs 对象本体）。
func buildUpdateGroupPrefsReq(id, data string) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("缺少用户组 ID")
	}
	req := map[string]any{"id": id}
	prefs, err := jsonObject(data, "--data")
	if err != nil {
		return nil, err
	}
	if prefs == nil {
		return nil, fmt.Errorf("--data 必填（prefs JSON 对象）")
	}
	req["prefs"] = prefs
	return req, nil
}

// buildCreateMembershipReq 构造 CreateMembershipRequest（user-id/email 至少一个）。
func buildCreateMembershipReq(groupID, userID, email, name, roles, status string) (map[string]any, error) {
	if groupID == "" {
		return nil, fmt.Errorf("缺少 group-id")
	}
	if userID == "" && email == "" {
		return nil, fmt.Errorf("--user-id 与 --email 至少提供一个")
	}
	req := map[string]any{"groupId": groupID}
	if userID != "" {
		req["userId"] = userID
	}
	if email != "" {
		req["email"] = email
	}
	if name != "" {
		req["name"] = name
	}
	if roles != "" {
		roleList, err := jsonStringList(roles, "--roles")
		if err != nil {
			return nil, err
		}
		req["roles"] = roleList
	}
	if status != "" {
		req["status"] = status
	}
	return req, nil
}

// buildListMembershipsReq 构造 ListMembershipsRequest。
func buildListMembershipsReq(groupID, queries string, pageSize int, pageToken string) (map[string]any, error) {
	if groupID == "" {
		return nil, fmt.Errorf("缺少 group-id")
	}
	req := map[string]any{"groupId": groupID}
	if queries != "" {
		qs, err := jsonStringList(queries, "--queries")
		if err != nil {
			return nil, err
		}
		req["queries"] = qs
	}
	if pageSize > 0 {
		req["pageSize"] = pageSize
	}
	if pageToken != "" {
		req["pageToken"] = pageToken
	}
	return req, nil
}

// buildUpdateMembershipReq 构造 UpdateMembershipRequest（roles 必填）。
func buildUpdateMembershipReq(groupID, membershipID, roles string) (map[string]any, error) {
	if groupID == "" {
		return nil, fmt.Errorf("缺少 group-id")
	}
	if membershipID == "" {
		return nil, fmt.Errorf("缺少 membership-id")
	}
	roleList, err := jsonStringList(roles, "--roles")
	if err != nil {
		return nil, err
	}
	if len(roleList) == 0 {
		return nil, fmt.Errorf("--roles 必填")
	}
	return map[string]any{"groupId": groupID, "membershipId": membershipID, "roles": roleList}, nil
}

// buildUpdateMembershipStatusReq 构造 UpdateMembershipStatusRequest（status 必填）。
func buildUpdateMembershipStatusReq(groupID, membershipID, status string) (map[string]any, error) {
	if groupID == "" {
		return nil, fmt.Errorf("缺少 group-id")
	}
	if membershipID == "" {
		return nil, fmt.Errorf("缺少 membership-id")
	}
	if status == "" {
		return nil, fmt.Errorf("--status 必填")
	}
	return map[string]any{"groupId": groupID, "membershipId": membershipID, "status": status}, nil
}
