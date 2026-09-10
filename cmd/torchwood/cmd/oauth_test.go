package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildUpsertOAuthProviderReq(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		enabled      bool
		clientID     string
		clientSecret string
		scopes       string
		wantErr      string
	}{
		{name: "缺 provider", clientID: "cid", wantErr: "缺少 provider"},
		{name: "缺 client-id", provider: "google", wantErr: "--client-id 必填"},
		{name: "禁用最小字段", provider: "google", clientID: "cid", wantErr: ""},
		{name: "启用无 secret 放行（服务端按既有 secret 兜底）", provider: "google", enabled: true, clientID: "cid", wantErr: ""},
		{name: "启用全字段", provider: "google", enabled: true, clientID: "cid",
			clientSecret: "sec", scopes: `["email","profile"]`, wantErr: ""},
		{name: "scopes 非法", provider: "google", clientID: "cid", scopes: `x`,
			wantErr: "--scopes 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpsertOAuthProviderReq(tt.provider, tt.enabled, tt.clientID, tt.clientSecret, tt.scopes)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.provider, req["provider"])
			require.Equal(t, tt.enabled, req["enabled"])
			require.Equal(t, tt.clientID, req["clientId"])
			if tt.clientSecret != "" {
				require.Equal(t, tt.clientSecret, req["clientSecret"])
			} else {
				require.NotContains(t, req, "clientSecret")
			}
			if tt.scopes != "" {
				require.Equal(t, []string{"email", "profile"}, req["scopes"])
			} else {
				require.NotContains(t, req, "scopes")
			}
		})
	}
}
