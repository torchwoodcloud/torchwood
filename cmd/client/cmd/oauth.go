package cmd

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

const (
	methodOAuthList   = "/torchwood.server.v1.OAuthProvidersService/ListOAuthProviders"
	methodOAuthUpsert = "/torchwood.server.v1.OAuthProvidersService/UpsertOAuthProvider"
	methodOAuthDelete = "/torchwood.server.v1.OAuthProvidersService/DeleteOAuthProvider"
)

// newOAuthProvidersCmd 覆盖 OAuthProvidersService 全部 3 个方法：
// list/upsert/delete（proto 无 get 方法；upsert 即 create+update 语义）。
func newOAuthProvidersCmd(g *globalFlags) *group {
	return newGroup(g, "oauth-providers", "OAuth 提供商管理（OAuthProvidersService 全部方法）", func(sub *commands.App) {
		sub.Register(
			newOAuthProvidersListCmd(g),
			newOAuthProvidersUpsertCmd(g),
			newOAuthProvidersDeleteCmd(g),
		)
	})
}

func newOAuthProvidersListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出 OAuth 提供商", "oauth-providers list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodOAuthList, listJSON(pageSize, pageToken))
		})
}

func newOAuthProvidersUpsertCmd(g *globalFlags) *verb {
	var enabled bool
	var clientID, clientSecret, scopes string
	return newVerb(g, "upsert", "创建或更新 OAuth 提供商（如 google/github）", "oauth-providers upsert <provider> --client-id <id>",
		func(fs *flag.FlagSet) {
			fs.BoolVar(&enabled, "enabled", false, "是否启用（启用且无既有 secret 时服务端要求 --client-secret）")
			fs.StringVar(&clientID, "client-id", "", "OAuth client ID（必填）")
			fs.StringVar(&clientSecret, "client-secret", "", "OAuth client secret（启用时必填）")
			fs.StringVar(&scopes, "scopes", "", "请求 scope JSON 数组（如 '[\"email\",\"profile\"]'）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildUpsertOAuthProviderReq(args[0], enabled, clientID, clientSecret, scopes)
			if err != nil {
				return err
			}
			return call(g, env, methodOAuthUpsert, req)
		})
}

func newOAuthProvidersDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除 OAuth 提供商", "oauth-providers delete <provider>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodOAuthDelete, map[string]any{"provider": args[0]})
		})
}

// buildUpsertOAuthProviderReq 构造 UpsertOAuthProviderRequest（client-id 必填；
// client_secret 仅在启用且无既有 secret 时由服务端校验必填，此处不做本地拦截）。
func buildUpsertOAuthProviderReq(provider string, enabled bool, clientID, clientSecret, scopes string) (map[string]any, error) {
	if provider == "" {
		return nil, fmt.Errorf("缺少 provider")
	}
	if clientID == "" {
		return nil, fmt.Errorf("--client-id 必填")
	}
	req := map[string]any{"provider": provider, "enabled": enabled, "clientId": clientID}
	if clientSecret != "" {
		req["clientSecret"] = clientSecret
	}
	if scopes != "" {
		scopeList, err := jsonStringList(scopes, "--scopes")
		if err != nil {
			return nil, err
		}
		req["scopes"] = scopeList
	}
	return req, nil
}
