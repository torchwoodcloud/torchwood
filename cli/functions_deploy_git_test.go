package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件覆盖 functions deployments create-from-git（二期阶段 3，git 部署
// 源）：请求装配（git oneof + 可选字段省略）与 token 环境变量解析（缺省
// 变量名 TORCHWOOD_GIT_TOKEN、--git-token-env 覆盖、缺失报错）。

func TestBuildCreateDeploymentFromGitReq(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == "MY_TOKEN" {
			return "tok-my", true
		}
		if name == "EMPTY_TOKEN" {
			return "", true
		}
		if name == defaultGitTokenEnv {
			return "tok-default", true
		}
		return "", false
	}
	tests := []struct {
		name       string
		functionID string
		url        string
		ref        string
		dir        string
		username   string
		tokenEnv   string
		wantErr    string
		wantGit    map[string]any
	}{
		{
			name:       "最小字段（缺省 token 变量）",
			functionID: "fn_1",
			url:        "https://git.example.com/acme/widget.git",
			wantGit: map[string]any{
				"url":   "https://git.example.com/acme/widget.git",
				"token": "tok-default",
			},
		},
		{
			name:       "全字段（--git-token-env 覆盖变量名）",
			functionID: "fn_1",
			url:        "https://git.example.com/acme/widget.git",
			ref:        "v1.2.3",
			dir:        "functions/greet",
			username:   "octocat",
			tokenEnv:   "MY_TOKEN",
			wantGit: map[string]any{
				"url":       "https://git.example.com/acme/widget.git",
				"ref":       "v1.2.3",
				"directory": "functions/greet",
				"username":  "octocat",
				"token":     "tok-my",
			},
		},
		{
			name:       "缺 token 报错（变量未设置）",
			functionID: "fn_1",
			url:        "https://git.example.com/acme/widget.git",
			tokenEnv:   "NOT_SET",
			wantErr:    "environment variable NOT_SET is not set",
		},
		{
			name:       "缺 token 报错（变量为空）",
			functionID: "fn_1",
			url:        "https://git.example.com/acme/widget.git",
			tokenEnv:   "EMPTY_TOKEN",
			wantErr:    "environment variable EMPTY_TOKEN is not set",
		},
		{
			name:       "缺 url 报错",
			functionID: "fn_1",
			tokenEnv:   "MY_TOKEN",
			wantErr:    "--url is required",
		},
		{
			name:     "缺 function-id 报错",
			url:      "https://git.example.com/acme/widget.git",
			tokenEnv: "MY_TOKEN",
			wantErr:  "missing function-id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.tokenEnv == "" {
				tt.tokenEnv = defaultGitTokenEnv
			}
			req, err := buildCreateDeploymentFromGitReq(tt.functionID, tt.url, tt.ref, tt.dir, tt.username, tt.tokenEnv, lookup)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.True(t, strings.Contains(err.Error(), tt.wantErr), "want %q, got %v", tt.wantErr, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.functionID, req["functionId"])
			require.Equal(t, tt.wantGit, req["git"], "git oneof 载荷逐字段保真（空可选字段省略）")
		})
	}
}

// TestBuildCreateDeploymentFromGitReq_DefaultTokenEnv 缺省变量名走真实
// os.LookupEnv：t.Setenv 设置 TORCHWOOD_GIT_TOKEN 后进请求；未设置报错。
func TestBuildCreateDeploymentFromGitReq_DefaultTokenEnv(t *testing.T) {
	t.Setenv(defaultGitTokenEnv, "tok-from-env")

	req, err := buildCreateDeploymentFromGitReq("fn_1", "https://git.example.com/a/b.git", "", "", "", "", os.LookupEnv)
	require.NoError(t, err)
	require.Equal(t, "tok-from-env", req["git"].(map[string]any)["token"])

	_, err = buildCreateDeploymentFromGitReq("fn_1", "https://git.example.com/a/b.git", "", "", "", "UNSET_VAR_FOR_TEST", os.LookupEnv)
	require.ErrorContains(t, err, "environment variable UNSET_VAR_FOR_TEST is not set")
}
