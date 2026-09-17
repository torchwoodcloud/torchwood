package functions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖部署源多元化二期阶段 1/4 的 app 层行为（设计
// docs/design/functions-runtimes-and-sources.md §0/§2）：git 源在
// functions-packer 服务接线（阶段 3）前占位拒绝；zip 路径不受影响。

// serverCtx 返回携带 service 主体（API key 形态）的上下文——
// RequireServerPrincipal 放行的三类主体之一。
func serverCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:   "svc-1",
		ActorKind: shared.ActorKindService,
		ProjectID: "p1",
	})
}

// TestCreateDeployment_GitPlaceholderRejected git 源占位拒绝（阶段 3 换
// packer 真实调用）：schema/proto/DB 已落（本阶段），物化链路未接——
// 显式 Unimplemented 且文案指向 functions-packer，不做任何 repo/zip 落盘。
func TestCreateDeployment_GitPlaceholderRejected(t *testing.T) {
	uc := NewFunctions(nil, newMockExecutor(nil, nil), newMockRepo(), newMockQueue())
	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID:  "p1",
		FunctionID: "fn_1",
		Git: &domainfunctions.GitSource{
			URL:       "https://git.example.com/acme/widget.git",
			Ref:       "main",
			Directory: "functions/greet",
			Username:  "git",
			Token:     "one-shot-token",
		},
	})
	require.Nil(t, dep)
	require.Equal(t, codes.Unimplemented, status.Code(err))
	require.ErrorContains(t, err, "git deployment source requires functions-packer service")
}

// TestCreateDeployment_ZipPathUnaffected 占位拒绝不影响 zip 路径的既有校验
// 顺序：Git 为 nil 时仍走 code 非空 → zip 魔数链（Git=nil + 空 code 的
// InvalidArgument 文案不变）。
func TestCreateDeployment_ZipPathUnaffected(t *testing.T) {
	uc := NewFunctions(nil, newMockExecutor(nil, nil), newMockRepo(), newMockQueue())

	_, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "code is required")

	_, err = uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{ProjectID: "p1", FunctionID: "fn_1", Code: []byte("not-a-zip")})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "missing PK zip signature")
}
