package functions

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dockerAvailable 检查 Docker daemon 可用（TORCHWOOD_RUN_DOCKER_TESTS=1 且能 ping）。
func dockerAvailable(t *testing.T) bool {
	if os.Getenv("TORCHWOOD_RUN_DOCKER_TESTS") != "1" {
		t.Log("TORCHWOOD_RUN_DOCKER_TESTS != 1, skipping docker integration tests")
		return false
	}
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		t.Logf("docker client error: %v, skipping", err)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Logf("docker daemon unavailable: %v, skipping", err)
		return false
	}
	return true
}

func testExecutor(t *testing.T) *DockerExecutor {
	cfg := &config.AppConfig{
		Functions: &config.Functions{
			Executor: "docker",
			Docker: &config.Functions_Docker{
				Host:     client.DefaultDockerHost,
				Network:  "torchwood-functions-test",
				Registry: "torchwood-funcs-test",
			},
		},
	}
	d := NewDockerExecutor(cfg)
	require.NotNil(t, d)
	return d
}

// makeZip 在内存中构建 zip 包。
func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func writeZipTemp(t *testing.T, code []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "code.zip")
	require.NoError(t, os.WriteFile(path, code, 0o600))
	return path
}

const nodeHello = `
exports.main = (data) => {
  console.log("hello " + (data && data.name ? data.name : "world"));
  return { ok: true, echo: data || {} };
};
`

func TestDockerExecutor_BuildAndRunNode(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("docker not available")
	}
	d := testExecutor(t)
	ctx := context.Background()

	zipPath := writeZipTemp(t, makeZip(t, map[string]string{"index.js": nodeHello}))
	require.NoError(t, d.Build(ctx, "fn_node", "dep_1", zipPath), "build node function")
	defer func() { _ = d.RemoveImage(ctx, "fn_node", "dep_1") }()

	res, err := d.Execute(ctx, domainfunctions.Execution{
		FunctionID:   "fn_node",
		DeploymentID: "dep_1",
		Runtime:      "node-18.0",
		Spec:         "shared-1x",
		Timeout:      60,
		Data:         `{"name":"torchwood"}`,
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.StatusCode)
	require.Contains(t, res.Stdout, "hello torchwood")
	require.JSONEq(t, `{"ok":true,"echo":{"name":"torchwood"}}`, res.Response)
}

func TestDockerExecutor_TimeoutCleansContainer(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("docker not available")
	}
	d := testExecutor(t)
	ctx := context.Background()

	// sleep 函数：timeout=1s 应超时。
	zipPath := writeZipTemp(t, makeZip(t, map[string]string{"index.js": `
exports.main = async () => {
  await new Promise(r => setTimeout(r, 10000));
  return {ok:true};
};
`}))
	require.NoError(t, d.Build(ctx, "fn_sleep", "dep_1", zipPath))
	defer func() { _ = d.RemoveImage(ctx, "fn_sleep", "dep_1") }()

	_, err := d.Execute(ctx, domainfunctions.Execution{
		FunctionID:   "fn_sleep",
		DeploymentID: "dep_1",
		Runtime:      "node-18.0",
		Timeout:      1,
	})
	require.Error(t, err, "超时应返回错误")

	// 容器无残留（镜像保留，容器必须已清理）。
	cli, err := d.client()
	require.NoError(t, err)
	filters := filters.NewArgs()
	filters.Add("ancestor", d.imageName("fn_sleep", "dep_1"))
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters})
	require.NoError(t, err)
	require.Empty(t, containers, "超时后不应有容器残留")
}

func TestDockerExecutor_RejectsZipSlip(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("docker not available")
	}
	d := testExecutor(t)

	// 路径穿越条目（../evil.js）。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("../evil.js")
	require.NoError(t, err)
	_, err = f.Write([]byte("console.log('evil')"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	zipPath := writeZipTemp(t, buf.Bytes())
	err = d.Build(context.Background(), "fn_slip", "dep_1", zipPath)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err), "zip slip 应返回 InvalidArgument")
}

func TestDockerExecutor_RejectsInvalidZip(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("docker not available")
	}
	d := testExecutor(t)

	zipPath := writeZipTemp(t, []byte("this is not a zip file at all"))
	err := d.Build(context.Background(), "fn_bad", "dep_1", zipPath)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestDockerExecutor_RejectsMissingEntrypoint(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("docker not available")
	}
	d := testExecutor(t)

	zipPath := writeZipTemp(t, makeZip(t, map[string]string{"foo.txt": "no entrypoint here"}))
	err := d.Build(context.Background(), "fn_nop", "dep_1", zipPath)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "missing entrypoint")
}

// depPackageJSON / depLockfile 是带依赖函数的最小清单（v3 §3.1 平台代装集成
// 用例）。lockfile 为 npm ci 兼容的 lockfileVersion 3 最小形态；ms@2.1.3 为
// 零依赖纯 JS 包（无生命周期脚本，与 --ignore-scripts 恒定语义兼容），
// integrity 取自 registry 真实值。
const depPackageJSON = `{
  "name": "torchwood-fn-deps",
  "version": "1.0.0",
  "private": true,
  "dependencies": { "ms": "2.1.3" }
}
`

const depLockfile = `{
  "name": "torchwood-fn-deps",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "torchwood-fn-deps",
      "version": "1.0.0",
      "dependencies": { "ms": "2.1.3" }
    },
    "node_modules/ms": {
      "version": "2.1.3",
      "resolved": "https://registry.npmjs.org/ms/-/ms-2.1.3.tgz",
      "integrity": "sha512-6FlzubTLZG3J2a/NVCAleEhjzq5oxgHyaCU9yYXvcLsvoVaHJq/s5xXI6/XXP6tz7R9xAOtHnSO/tXtF3WRTlA=="
    }
  }
}
`

// depIndexJS 用户代码 require 平台代装的依赖——执行结果证明 npm ci 真装上了。
const depIndexJS = `
const ms = require("ms");
exports.main = (data) => {
  return { parsed: ms(data.duration), isFn: typeof ms === "function" };
};
`

// TestDockerExecutor_BuildAndRunWithDependencies 带依赖 + lockfile 端到端
// （v3 §3.1/D11 平台代装）：分层模板构建（构建期 npm ci --omit=dev
// --ignore-scripts 真实拉包）→ 执行结果证明依赖在镜像内可用；用户代码不
// 携带 node_modules（构建期拒收，单元测试覆盖）。
func TestDockerExecutor_BuildAndRunWithDependencies(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("docker not available")
	}
	d := testExecutor(t)
	ctx := context.Background()

	zipPath := writeZipTemp(t, makeZip(t, map[string]string{
		"index.js":          depIndexJS,
		"package.json":      depPackageJSON,
		"package-lock.json": depLockfile,
	}))
	require.NoError(t, d.Build(ctx, "fn_deps", "dep_1", zipPath),
		"带依赖 + lockfile 的部署必须构建成功（平台代装）")
	defer func() { _ = d.RemoveImage(ctx, "fn_deps", "dep_1") }()

	res, err := d.Execute(ctx, domainfunctions.Execution{
		FunctionID:   "fn_deps",
		DeploymentID: "dep_1",
		Runtime:      "node-18.0",
		Spec:         "shared-1x",
		Timeout:      60,
		Data:         `{"duration":"100ms"}`,
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.StatusCode)
	require.JSONEq(t, `{"parsed":100,"isFn":true}`, res.Response)
}

// TestDockerExecutor_RejectsDepsWithoutLockfile lockfile 强制端到端：带依赖
// 无 package-lock.json → 构建失败（deployment failed），错误信息指向提交
// lockfile（v3 §3.1；错误在模板决策层产生，不触碰 docker build）。
func TestDockerExecutor_RejectsDepsWithoutLockfile(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("docker not available")
	}
	d := testExecutor(t)

	zipPath := writeZipTemp(t, makeZip(t, map[string]string{
		"index.js":     depIndexJS,
		"package.json": depPackageJSON,
	}))
	err := d.Build(context.Background(), "fn_nolock", "dep_1", zipPath)
	require.Error(t, err)
	require.ErrorContains(t, err, "检测到 dependencies 但缺少 package-lock.json")
}
