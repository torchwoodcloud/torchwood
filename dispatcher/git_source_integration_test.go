package dispatcher

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/packer"
)

// —— git 部署源 docker 集成（二期阶段 4/4，设计
// docs/design/functions-runtimes-and-sources.md §2/§4）——
//
// 门控与既有 IP 路由型 e2e 同一口径（requireIPRoutingHost + daemon 探测，
// daemon_integration_test.go 同款）：验证 spawn 健康探针与池执行都依赖测试
// 进程 → 容器 bridge IP 的 Linux 直连路由；真实取证方式 = 挂 docker.sock
// 的 golang:1.26-alpine 容器内跑 go test（Linux 宿主语义）。
//
// 编排说明：packer 的 HTTP 壳（认证 / 并发自限 / fetch_timeout 封顶 / 错误
// 映射）由 packer 包的单元测试覆盖；packServer 与 Service 类型不
// 导出（进程装配内聚），故此处直接编排导出函数 PackGit——它是
// POST /v1/pack/git handler 的同源实现入口（handler 只是认证/预算薄壳）。
// 验收目标「git fixture → pack → BuildImage → 执行」语义与生产 server 侧
// SourcePacker.PackGit 调用完全一致（PackerClient 适配层的协议编解码由
// internal/infra/functions 的单元测试覆盖）。

// gitE2ERepo 是 go-git 就地造出的测试仓库（不依赖系统 git 二进制；与
// packer/fixture_test.go 同模式——测试夹具跨包不可导出，各自
// 持有一份）。仓库根的 README.md 是子目录构建上下文的对照物：directory=
// functions/greet 物化后 zip 根不含它。
type gitE2ERepo struct {
	dir  string
	head plumbing.Hash
}

// fileURL 返回仓库根的 file:// 形态 URL（Windows 盘符需三斜杠
// file:///C:/...；Linux 容器内即 file:///tmp/...）。
func (fx *gitE2ERepo) fileURL() string {
	return "file:///" + strings.TrimPrefix(filepath.ToSlash(fx.dir), "/")
}

// gitE2EGoMod 是 greet 子目录 module 的 go.mod：require 空 = 纯 stdlib
// （合法无 go.sum，模板 COPY go.mod go.sum* ./ 通配）。
const gitE2EGoMod = "module example.com/greet\n\ngo 1.26\n"

// gitE2EMainGo 是 main 风格契约入口（返回固定封套供执行断言）。
const gitE2EMainGo = `package greet

func Main(data map[string]any, ctx map[string]string) (any, error) {
	return map[string]any{"got": data["n"], "src": "git"}, nil
}
`

// writeGitE2EFixtureRepo 造 fixture 仓库：
//
//	README.md                    仓库根杂项（Directory 子目录物化后不入 zip）
//	functions/greet/go.mod       module 根 = 构建上下文根
//	functions/greet/main.go      Main 契约入口
func writeGitE2EFixtureRepo(t *testing.T) *gitE2ERepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	files := map[string]string{
		"README.md":               "# git e2e fixture\n",
		"functions/greet/go.mod":  gitE2EGoMod,
		"functions/greet/main.go": gitE2EMainGo,
	}
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
		_, err := wt.Add(rel)
		require.NoError(t, err)
	}
	sig := &object.Signature{Name: "tw-test", Email: "tw@test.local", When: time.Unix(1700000000, 0)}
	h, err := wt.Commit("fixture", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	return &gitE2ERepo{dir: dir, head: h}
}

// zipEntryNames 返回 zip 的条目名（保持 zip 内顺序；packer 物化为 walk
// 字典序）。
func zipEntryNames(t *testing.T, zipBytes []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	require.NoError(t, err)
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

// TestIntegration_GitSourceGoFunctionFullChainE2E 本二期旗舰用例：git 源
// + Go 函数全链——go-git 造 fixture 仓（module + Main 函数，构建上下文在
// 子目录）→ PackGit（dev file:// 本地树物化，缺省预算）→ 断言钉死 commit
// / checksum / zip 根 = 子目录 → 真实 docker BuildImage（verify=true = 内联
// 验证 spawn）→ 池 spawn 执行 → 断言 main 封套。覆盖设计 §2 的核心声明：
// git 源归一为与 zip 源同构的代码包后，构建/执行链路完全复用。
func TestIntegration_GitSourceGoFunctionFullChainE2E(t *testing.T) {
	requireIPRoutingHost(t)
	cli := requireDockerClient(t)
	// file:// 与裸本地路径 git 源仅 development 放行（validateSourceURL）；
	// CurrentRuntimeEnv 逐次读取，t.Setenv 注入即可（生产环境是静态的，
	// 测试可控是既定设计）。
	t.Setenv("TORCHWOOD_ENV", "development")

	fx := writeGitE2EFixtureRepo(t)

	// —— pack：与生产 server 侧 SourcePacker.PackGit 同一入口（dev
	// file:// 本地树物化；PackOptions 零值 = 缺省预算归一）——
	packResp, err := packer.PackGit(context.Background(),
		packer.PackRequest{
			URL:       fx.fileURL(),
			Ref:       "", // HEAD
			Directory: "functions/greet",
		}, packer.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, fx.head.String(), packResp.CommitSHA,
		"PackResponse.CommitSHA 必须钉死为解析后提交（部署审计四件之一）")
	require.Len(t, packResp.CommitSHA, 40)

	zipBytes, err := base64.StdEncoding.DecodeString(packResp.ZipBase64)
	require.NoError(t, err)
	sum := sha256.Sum256(zipBytes)
	require.Equal(t, hex.EncodeToString(sum[:]), packResp.Checksum,
		"checksum = zip 字节的 hex sha256（可审计一致性）")
	require.Equal(t, []string{"go.mod", "main.go"}, zipEntryNames(t, zipBytes),
		"zip 根 = directory 子目录（README 不入构建上下文；物化条目字典序）")

	// —— BuildImage（verify=true = 验证 spawn 内联通过）——
	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)
	reg := newFakeRegistry()
	pool := NewPoolManager(d, reg, PoolConfig{
		BootTimeout:      90 * time.Second,
		QueueHeadTimeout: 90 * time.Second,
	})

	fnID, depID := "fngite2e", fmt.Sprintf("dep%d", time.Now().UnixNano())
	imageRef := infrafunctions.ImageName(cfg, fnID, depID)
	cleanupImage(t, d, fnID, depID)
	cleanupPool(t, pool, "giteit", fnID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "giteit", FunctionID: fnID, DeploymentID: depID,
		Zip: zipBytes, Runtime: "go-1.26",
		Verify: true,
	}), "git 物化 zip 的构建 + 验证 spawn 必须成功（git 源与 zip 源同构消费）")

	// 验证实例已回收（此刻池尚未 spawn，凡挂本镜像的容器只能是验证残留）。
	requireNoLeftoverVerifyContainers(t, cli, imageRef)

	// —— 真实 spawn 执行（池冷启动 → 健康握手 → 分发）——
	resp, err := pool.Dispatch(ctx, ExecuteRequest{
		Image: imageRef, ProjectID: "giteit", FunctionID: fnID, DeploymentID: depID,
		Runtime: "go-1.26", Spec: "shared-1x", TimeoutSeconds: 30,
		Data:           `{"n":41}`,
		ExecutionToken: "twx_it-token",
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.Contains(t, resp.Response, `"got":41`)
	require.Contains(t, resp.Response, `"src":"git"`,
		"git 源函数的执行封套语义与 zip 源完全一致")

	pool.DrainForDeployment(ctx, "giteit", fnID, "none", 5*time.Second)
	waitPoolDrained(t, pool, ctx)
}

// TestIntegration_GitWideEntryZipBuild 大条目物化回归（设计 §2 条目维链条
// 的真实 docker 形态对照）：>1000 条目（zip 上传通道的声明侧预检预算）但
// ≤5000（packer 物化上限 = ExtractZipRelaxed 放宽口径）的 zip 经
// BuildImage 构建成功 + 验证 spawn 通过——git 源合法包不被 zip 通道的
// 1000 条目预算击毙。直接构造多文件 zip（条目维回归无需真 git；packer 侧
// 的 5000 条目上限由 packer 单元测试覆盖）。
func TestIntegration_GitWideEntryZipBuild(t *testing.T) {
	requireIPRoutingHost(t)
	requireDockerClient(t)

	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)

	fnID, depID := "fnwide", fmt.Sprintf("dep%d", time.Now().UnixNano())
	cleanupImage(t, d, fnID, depID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1201 条目 = 1200 个杂项文件 + index.js（node runtime 探测入口）：
	// >1000（默认 zip 预算必拒）且 ≤5000（放宽预算内）。
	files := map[string]string{
		"index.js": "module.exports.main = () => ({ wide: true });\n",
	}
	for i := 0; i < 1200; i++ {
		files[fmt.Sprintf("w%04d.txt", i)] = fmt.Sprintf("entry %d\n", i)
	}
	require.Len(t, files, 1201, "条目数必须越过 1000（旧预算必拒的形态）")

	zipBytes := makeEntryZipFiles(t, files)
	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "wideit", FunctionID: fnID, DeploymentID: depID,
		Zip: zipBytes, Runtime: "node-18.0",
		Verify: true,
	}), ">1000 条目的构建必须经放宽预算（5000 条目）成功 + 验证 spawn 通过")
}
