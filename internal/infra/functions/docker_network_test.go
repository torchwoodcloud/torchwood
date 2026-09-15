package functions

import (
	"testing"

	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// netTestCfg 构造指定网络配置的 AppConfig。
func netTestCfg(network string) *config.AppConfig {
	return &config.AppConfig{
		Functions: &config.Functions{
			Docker: &config.Functions_Docker{
				Host:     client.DefaultDockerHost,
				Network:  network,
				Registry: "torchwood-funcs-test",
			},
		},
	}
}

// TestResolveNetwork_PerProjectDefault：未配置全局网络时按项目派生独立网络名。
func TestResolveNetwork_PerProjectDefault(t *testing.T) {
	name, err := ResolveNetworkName(netTestCfg(""), "shop")
	require.NoError(t, err)
	require.Equal(t, perProjectNetworkPrefix+"shop", name)

	// 不同项目得到不同网络（隔离的前提）。
	name2, err := ResolveNetworkName(netTestCfg(""), "other")
	require.NoError(t, err)
	require.NotEqual(t, name, name2)
}

// TestResolveNetwork_ExplicitOptIn：显式配置的全局网络优先生效（opt-in）。
func TestResolveNetwork_ExplicitOptIn(t *testing.T) {
	for _, pid := range []string{"shop", "other"} {
		name, err := ResolveNetworkName(netTestCfg("torchwood-functions-global"), pid)
		require.NoError(t, err)
		require.Equal(t, "torchwood-functions-global", name)
	}
}

// TestResolveNetwork_FailClosed：空 projectID / 非法 projectID 拒绝，
// 不回落共享或 none 网络。
func TestResolveNetwork_FailClosed(t *testing.T) {
	_, err := ResolveNetworkName(netTestCfg(""), "")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = ResolveNetworkName(netTestCfg(""), "Bad_ID")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestResolveInternalNetworkName（P2 egress 默认 deny）：internal 变体网络名
// = 常规网络名 + "-int"；校验与常规解析同源（fail-closed）。
func TestResolveInternalNetworkName(t *testing.T) {
	name, err := ResolveInternalNetworkName(netTestCfg(""), "shop")
	require.NoError(t, err)
	require.Equal(t, perProjectNetworkPrefix+"shop"+perProjectInternalNetworkSuffix, name)

	// 显式全局网络配置同样加后缀（trusted/untrusted 分网不因 opt-in 失效）。
	name, err = ResolveInternalNetworkName(netTestCfg("torchwood-functions-global"), "shop")
	require.NoError(t, err)
	require.Equal(t, "torchwood-functions-global-int", name)

	// 非法 projectID 拒绝（fail-closed 与常规解析一致）。
	_, err = ResolveInternalNetworkName(netTestCfg(""), "")
	require.Error(t, err)
	_, err = ResolveInternalNetworkName(netTestCfg(""), "Bad_ID")
	require.Error(t, err)
}
