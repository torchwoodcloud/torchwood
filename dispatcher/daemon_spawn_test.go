package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖 SpawnInstance 的镜像缺失类型化上抛（执行链自愈 rebuild 链路的
// daemon 层判定面）：create 命中 No such image → FailedPrecondition +
// ImageMissingMarker（server 凭「code + 标记」触发自动重建）；其余 create
// 错误与 NotFound 的非镜像形态（网络缺失）保持原样。经 containerCli 窄缝
// 注入 fake（与 netCli/imgCli 同款），不依赖真实 daemon。

// fakeContainerClient 是 containerClient 窄缝的可编程 fake。
type fakeContainerClient struct {
	createErr error
	created   int
	started   int
	removed   []string
}

func (f *fakeContainerClient) ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error) {
	f.created++
	if f.createErr != nil {
		return container.CreateResponse{}, f.createErr
	}
	return container.CreateResponse{ID: "cid-1"}, nil
}

func (f *fakeContainerClient) ContainerStart(_ context.Context, _ string, _ container.StartOptions) error {
	f.started++
	return nil
}

func (f *fakeContainerClient) ContainerRemove(_ context.Context, containerID string, _ container.RemoveOptions) error {
	f.removed = append(f.removed, containerID)
	return nil
}

func spawnTestDaemon(cc *fakeContainerClient) *dockerDaemon {
	d := &dockerDaemon{cfg: &config.AppConfig{}, bootTimeout: time.Second, maxRequestsDefault: 100}
	d.containerCli = cc
	return d
}

// TestSpawnInstance_ImageMissingTypedError 镜像缺失（NotFound + No such image
// 文案，docker daemon 404 的客户端映射形态）→ FailedPrecondition 类型化错误。
func TestSpawnInstance_ImageMissingTypedError(t *testing.T) {
	cc := &fakeContainerClient{
		createErr: fmt.Errorf("Error response from daemon: No such image: torchwood-funcs/func-fn1-dep1:latest: %w", errdefs.ErrNotFound),
	}
	d := spawnTestDaemon(cc)

	_, err := d.SpawnInstance(context.Background(), SpawnOptions{
		Image: "torchwood-funcs/func-fn1-dep1", Name: "tw-fn-p1-fn1", MaxRequests: 100,
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	msg := status.Convert(err).Message()
	require.Contains(t, msg, domainfunctions.ImageMissingMarker, "错误必须携带稳定标记（server 识别依据）")
	require.Contains(t, msg, "torchwood-funcs/func-fn1-dep1", "错误必须携带镜像引用")
	require.Contains(t, msg, "rebuild required", "错误必须引导重建语义")
}

// TestSpawnInstance_NonImageNotFoundStaysPlain NotFound 的非镜像形态（如网络
// 缺失）不得误标：保持普通包装错误（Internal 形态），不触发重建链路。
func TestSpawnInstance_NonImageNotFoundStaysPlain(t *testing.T) {
	cc := &fakeContainerClient{
		createErr: fmt.Errorf("Error response from daemon: network tw-func-p1 not found: %w", errdefs.ErrNotFound),
	}
	d := spawnTestDaemon(cc)

	_, err := d.SpawnInstance(context.Background(), SpawnOptions{
		Image: "torchwood-funcs/func-fn1-dep1", Name: "tw-fn-p1-fn1", MaxRequests: 100,
	})
	require.Equal(t, codes.Unknown, status.Code(err))
	require.NotContains(t, status.Convert(err).Message(), domainfunctions.ImageMissingMarker)
	require.Contains(t, err.Error(), "create resident instance", "保持原始包装语义")
}

// TestSpawnInstance_GenericCreateErrorStaysPlain 普通 create 错误（daemon
// 不可达等）保持原样。
func TestSpawnInstance_GenericCreateErrorStaysPlain(t *testing.T) {
	cc := &fakeContainerClient{createErr: errors.New("connection refused")}
	d := spawnTestDaemon(cc)

	_, err := d.SpawnInstance(context.Background(), SpawnOptions{
		Image: "torchwood-funcs/func-fn1-dep1", Name: "tw-fn-p1-fn1", MaxRequests: 100,
	})
	require.Equal(t, codes.Unknown, status.Code(err))
	require.NotContains(t, status.Convert(err).Message(), domainfunctions.ImageMissingMarker)
}

// TestSpawnInstance_InspectFailureCleansUp create/start 成功但 inspect 失败
// （fake 未覆盖 InspectInstance，d.cli 为 nil 即失败形态）→ 清理容器原语
// 经同一窄缝生效。
func TestSpawnInstance_InspectFailureCleansUp(t *testing.T) {
	cc := &fakeContainerClient{}
	d := spawnTestDaemon(cc)

	_, err := d.SpawnInstance(context.Background(), SpawnOptions{
		Image: "torchwood-funcs/func-fn1-dep1", Name: "tw-fn-p1-fn1", MaxRequests: 100,
	})
	require.Error(t, err)
	require.Equal(t, 1, cc.started)
	require.Equal(t, []string{"cid-1"}, cc.removed, "inspect 失败必须清理已创建容器")
}
