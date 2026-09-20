package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件覆盖 functions deployments create-from-image（三期阶段 3，BYO 镜像
// 部署源，设计 docs/design/functions-runtimes-and-sources.md §3）：请求装配
// （image oneof + 可选字段省略）与 registry token 环境变量解析（缺省变量名
// TORCHWOOD_REGISTRY_TOKEN、--registry-token-env 覆盖、缺失报错、token 不进
// argv）。import guard（TestNoProtoGRPCImports）覆盖新增源码文件。

func TestBuildCreateDeploymentFromImageReq(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == "MY_REGISTRY_TOKEN" {
			return "reg-tok-my", true
		}
		if name == "EMPTY_REGISTRY_TOKEN" {
			return "", true
		}
		if name == defaultRegistryTokenEnv {
			return "reg-tok-default", true
		}
		return "", false
	}
	tests := []struct {
		name       string
		functionID string
		image      string
		username   string
		tokenEnv   string
		wantErr    string
		wantImage  map[string]any
	}{
		{
			name:       "最小字段（缺省 token 变量）",
			functionID: "fn_1",
			image:      "registry.example.com/acme/greet:v1",
			wantImage: map[string]any{ // #nosec G101 -- 测试夹具伪凭证
				"image":         "registry.example.com/acme/greet:v1",
				"registryToken": "reg-tok-default",
			},
		},
		{
			name:       "全字段（--registry-token-env 覆盖变量名）",
			functionID: "fn_1",
			image:      "registry.example.com/acme/greet@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			username:   "bot",
			tokenEnv:   "MY_REGISTRY_TOKEN",
			wantImage: map[string]any{ // #nosec G101 -- 测试夹具伪凭证
				"image":            "registry.example.com/acme/greet@sha256:1111111111111111111111111111111111111111111111111111111111111111",
				"registryToken":    "reg-tok-my",
				"registryUsername": "bot",
			},
		},
		{
			name:       "缺 token 报错（变量未设置）",
			functionID: "fn_1",
			image:      "registry.example.com/acme/greet:v1",
			tokenEnv:   "NOT_SET",
			wantErr:    "environment variable NOT_SET is not set",
		},
		{
			name:       "缺 token 报错（变量为空）",
			functionID: "fn_1",
			image:      "registry.example.com/acme/greet:v1",
			tokenEnv:   "EMPTY_REGISTRY_TOKEN",
			wantErr:    "environment variable EMPTY_REGISTRY_TOKEN is not set",
		},
		{
			name:       "缺 image 报错",
			functionID: "fn_1",
			tokenEnv:   "MY_REGISTRY_TOKEN",
			wantErr:    "--image is required",
		},
		{
			name:     "缺 function-id 报错",
			image:    "registry.example.com/acme/greet:v1",
			tokenEnv: "MY_REGISTRY_TOKEN",
			wantErr:  "missing function-id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.tokenEnv == "" {
				tt.tokenEnv = defaultRegistryTokenEnv
			}
			req, err := buildCreateDeploymentFromImageReq(tt.functionID, tt.image, tt.username, tt.tokenEnv, lookup)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.True(t, strings.Contains(err.Error(), tt.wantErr), "want %q, got %v", tt.wantErr, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.functionID, req["functionId"])
			require.Equal(t, tt.wantImage, req["image"], "image oneof 载荷逐字段保真（空可选字段省略）")
		})
	}
}

// TestBuildCreateDeploymentFromImageReq_DefaultTokenEnv 缺省变量名走真实
// os.LookupEnv：t.Setenv 设置 TORCHWOOD_REGISTRY_TOKEN 后进请求；未设置报错。
func TestBuildCreateDeploymentFromImageReq_DefaultTokenEnv(t *testing.T) {
	t.Setenv(defaultRegistryTokenEnv, "reg-tok-from-env")

	req, err := buildCreateDeploymentFromImageReq("fn_1", "registry.example.com/acme/greet:v1", "", "", os.LookupEnv)
	require.NoError(t, err)
	require.Equal(t, "reg-tok-from-env", req["image"].(map[string]any)["registryToken"])

	_, err = buildCreateDeploymentFromImageReq("fn_1", "registry.example.com/acme/greet:v1", "", "UNSET_VAR_FOR_TEST", os.LookupEnv)
	require.ErrorContains(t, err, "environment variable UNSET_VAR_FOR_TEST is not set")
}
