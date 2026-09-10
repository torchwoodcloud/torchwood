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
	return newGroup(g, "oauth-providers", "OAuth provider management (all OAuthProvidersService methods)", func(sub *commands.App) {
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
	return newVerb(g, "list", "list OAuth providers", "oauth-providers list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default 50, max 1000)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodOAuthList, listJSON(pageSize, pageToken))
		})
}

func newOAuthProvidersUpsertCmd(g *globalFlags) *verb {
	var enabled bool
	var clientID, clientSecret, scopes string
	return newVerb(g, "upsert", "create or update an OAuth provider (e.g. google/github)", "oauth-providers upsert <provider> --client-id <id>",
		func(fs *flag.FlagSet) {
			fs.BoolVar(&enabled, "enabled", false, "whether enabled (when enabling with no existing secret, the server requires --client-secret)")
			fs.StringVar(&clientID, "client-id", "", "OAuth client ID (required)")
			fs.StringVar(&clientSecret, "client-secret", "", "OAuth client secret (required when enabling)")
			fs.StringVar(&scopes, "scopes", "", "requested scopes JSON array (e.g. '[\"email\",\"profile\"]')")
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
	return newVerb(g, "delete", "delete an OAuth provider", "oauth-providers delete <provider>", nil,
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
		return nil, fmt.Errorf("missing provider")
	}
	if clientID == "" {
		return nil, fmt.Errorf("--client-id is required")
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
