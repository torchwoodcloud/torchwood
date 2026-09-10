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
	return newGroup(g, "groups", "group management (all GroupsService methods)", func(sub *commands.App) {
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
	return newVerb(g, "create", "create a group", "groups create --name <name>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "group name (required)")
			fs.StringVar(&permissions, "permissions", "", "permissions JSON array")
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
	return newVerb(g, "list", "list groups", "groups list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default 50, max 1000)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodGroupsList, listJSON(pageSize, pageToken))
		})
}

func newGroupsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "get a group by ID", "groups get <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodGroupsGet, map[string]any{"id": args[0]})
		})
}

func newGroupsDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "delete a group", "groups delete <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodGroupsDelete, map[string]any{"id": args[0]})
		})
}

// newGroupsPrefsCmd: groups prefs get <id> / update <id> --data。
func newGroupsPrefsCmd(g *globalFlags) *group {
	return newGroup(g, "prefs", "group preferences management", func(sub *commands.App) {
		sub.Register(
			newVerb(g, "get", "get group preferences", "groups prefs get <id>", nil,
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
	return newVerb(g, "update", "replace group preferences (--data is the prefs object itself)", "groups prefs update <id> --data '{...}'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&data, "data", "", "prefs JSON object (required, e.g. '{\"theme\":\"dark\"}')")
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
	return newGroup(g, "memberships", "group membership management", func(sub *commands.App) {
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
	return newVerb(g, "create", "create a membership (--user-id or --email, at least one)", "groups memberships create <group-id> [--user-id <uid> | --email <email>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&userID, "user-id", "", "user ID (registered user)")
			fs.StringVar(&email, "email", "", "email (invite an unregistered user)")
			fs.StringVar(&name, "name", "", "member name (used for email invites)")
			fs.StringVar(&roles, "roles", "", "roles JSON array (e.g. '[\"admin\"]')")
			fs.StringVar(&status, "status", "", "status (pending/active/blocked)")
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
	return newVerb(g, "list", "list group memberships", "groups memberships list <group-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&queries, "queries", "", "Appwrite-style queries JSON array")
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default 50, max 1000)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
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
	return newVerb(g, "get", "get a membership by ID", "groups memberships get <group-id> <membership-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodGroupsGetMembership, map[string]any{"groupId": args[0], "membershipId": args[1]})
		})
}

func newGroupsMembershipsUpdateCmd(g *globalFlags) *verb {
	var roles string
	return newVerb(g, "update", "replace membership roles", "groups memberships update <group-id> <membership-id> --roles '[...]'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&roles, "roles", "", "roles JSON array (required, full replacement)")
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
	return newVerb(g, "update-status", "update membership status (active/blocked; pending cannot come back)", "groups memberships update-status <group-id> <membership-id> --status <status>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&status, "status", "", "target status (required: active/blocked)")
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
	return newVerb(g, "delete", "delete a membership", "groups memberships delete <group-id> <membership-id>", nil,
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
		return nil, fmt.Errorf("--name is required")
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
		return nil, fmt.Errorf("missing group id")
	}
	req := map[string]any{"id": id}
	prefs, err := jsonObject(data, "--data")
	if err != nil {
		return nil, err
	}
	if prefs == nil {
		return nil, fmt.Errorf("--data is required (prefs JSON object)")
	}
	req["prefs"] = prefs
	return req, nil
}

// buildCreateMembershipReq 构造 CreateMembershipRequest（user-id/email 至少一个）。
func buildCreateMembershipReq(groupID, userID, email, name, roles, status string) (map[string]any, error) {
	if groupID == "" {
		return nil, fmt.Errorf("missing group-id")
	}
	if userID == "" && email == "" {
		return nil, fmt.Errorf("provide at least one of --user-id and --email")
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
		return nil, fmt.Errorf("missing group-id")
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
		return nil, fmt.Errorf("missing group-id")
	}
	if membershipID == "" {
		return nil, fmt.Errorf("missing membership-id")
	}
	roleList, err := jsonStringList(roles, "--roles")
	if err != nil {
		return nil, err
	}
	if len(roleList) == 0 {
		return nil, fmt.Errorf("--roles is required")
	}
	return map[string]any{"groupId": groupID, "membershipId": membershipID, "roles": roleList}, nil
}

// buildUpdateMembershipStatusReq 构造 UpdateMembershipStatusRequest（status 必填）。
func buildUpdateMembershipStatusReq(groupID, membershipID, status string) (map[string]any, error) {
	if groupID == "" {
		return nil, fmt.Errorf("missing group-id")
	}
	if membershipID == "" {
		return nil, fmt.Errorf("missing membership-id")
	}
	if status == "" {
		return nil, fmt.Errorf("--status is required")
	}
	return map[string]any{"groupId": groupID, "membershipId": membershipID, "status": status}, nil
}
