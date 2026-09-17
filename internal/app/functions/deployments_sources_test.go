package functions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖部署源多元化二期阶段 3/4 的 app 层 git 分支（设计
// docs/design/functions-runtimes-and-sources.md §2「app 时序」「重建语义」）：
// 形状校验 → SourcePacker.PackGit → 写盘 → INSERT（source 投影）→
// buildDeployment——与 zip 源同构；失败清理分流（pack/写盘/INSERT 失败无行
// 无 zip；构建失败 git zip 保留 / zip 源删除）。

// serverCtx 返回携带 service 主体（API key 形态）的上下文——
// RequireServerPrincipal 放行的三类主体之一。
func serverCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:   "svc-1",
		ActorKind: shared.ActorKindService,
		ProjectID: "p1",
	})
}

// gitSource 是测试用 git 源样板（一次性凭证随结构体）。
func gitSource() *domainfunctions.GitSource {
	return &domainfunctions.GitSource{
		URL:       "https://git.example.com/acme/widget.git",
		Ref:       "main",
		Directory: "functions/greet",
		Username:  "git",
		Token:     "one-shot-token",
	}
}

// fakeSourcePacker 是 SourcePacker 端口的可编程 fake：记录收到的 GitSource，
// 返回预置结果/错误（zip 恒为合法 zip 魔数开头，走真实写盘）。
type fakeSourcePacker struct {
	got      []domainfunctions.GitSource
	commit   string
	checksum string
	zip      []byte
	packErr  error
}

func (p *fakeSourcePacker) PackGit(_ context.Context, src domainfunctions.GitSource) (string, string, []byte, error) {
	p.got = append(p.got, src)
	if p.packErr != nil {
		return "", "", nil, p.packErr
	}
	return p.commit, p.checksum, p.zip, nil
}

func newFakePacker() *fakeSourcePacker {
	return &fakeSourcePacker{
		commit:   "0123456789abcdef0123456789abcdef01234567",
		checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		zip:      []byte("PK\x03\x04fake-materialized-zip"),
	}
}

// gitTestUC 组装带 fake packer 的用例聚合（fn 预置到 repo）。
func gitTestUC(t *testing.T, exec *mockExecutor, packer domainfunctions.SourcePacker) (*Functions, *mockRepo) {
	t.Helper()
	repo := newMockRepo()
	require.NoError(t, repo.CreateFunction(context.Background(), &domainfunctions.Function{
		ID: "fn_1", ProjectID: "p1", Runtime: "node-18.0", TimeoutSeconds: 10, Enabled: true,
	}))
	uc := NewFunctions(&config.AppConfig{}, exec, repo, newMockQueue())
	uc.packer = packer
	return uc, repo
}

// TestCreateDeployment_GitSuccess 成功主链路：packer 收到保真 GitSource →
// zip 落既有 zipPath → INSERT 行 source 投影（source_type=git、source_ref=
// 钉死 commit SHA、context_sha256=checksum、size=len(zip)）→
// buildDeployment 被调 → ready。
func TestCreateDeployment_GitSuccess(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	packer := newFakePacker()
	uc, repo := gitTestUC(t, exec, packer)

	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID:  "p1",
		FunctionID: "fn_1",
		Git:        gitSource(),
	})
	require.NoError(t, err)
	require.Len(t, packer.got, 1)
	require.Equal(t, "https://git.example.com/acme/widget.git", packer.got[0].URL)
	require.Equal(t, "main", packer.got[0].Ref)
	require.Equal(t, "functions/greet", packer.got[0].Directory)
	require.Equal(t, "git", packer.got[0].Username)
	require.Equal(t, "one-shot-token", packer.got[0].Token, "凭证仅随调用栈送达 packer")

	require.Equal(t, 1, exec.builds, "buildDeployment 与 zip 源同路被调")
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
	require.Equal(t, domainfunctions.DeploymentSourceGit, dep.SourceType)
	require.Equal(t, "0123456789abcdef0123456789abcdef01234567", dep.SourceRef, "source_ref = 钉死 commit SHA")
	require.Equal(t, "https://git.example.com/acme/widget.git", dep.SourceURL)
	require.Equal(t, "functions/greet", dep.SourceDir)
	require.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", dep.ContextSHA256)
	require.Equal(t, int64(len("PK\x03\x04fake-materialized-zip")), dep.Size)

	path := zipPath("p1", "fn_1", dep.ID)
	t.Cleanup(func() { _ = removeZip("p1", "fn_1", dep.ID) })
	require.FileExists(t, path, "物化 zip 落既有 zipPath")

	stored, err := repo.GetDeployment(context.Background(), "p1", "fn_1", dep.ID)
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusReady, stored.Status)
	require.Equal(t, domainfunctions.DeploymentSourceGit, stored.SourceType)
	require.Equal(t, dep.ContextSHA256, stored.ContextSHA256)
}

// TestCreateDeployment_GitPackerFailure pack 失败：无行无 zip（设计 §2 app
// 时序——pack 在行落库之前，失败路径与 zip 魔数校验失败同类）。
func TestCreateDeployment_GitPackerFailure(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	packer := newFakePacker()
	packer.packErr = status.Error(codes.ResourceExhausted, "packer at concurrency limit")
	uc, repo := gitTestUC(t, exec, packer)

	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{ProjectID: "p1", FunctionID: "fn_1", Git: gitSource()})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Nil(t, dep)
	require.Empty(t, repo.deployments, "无行")
	require.Equal(t, 0, exec.builds, "未进入构建")
}

// TestCreateDeployment_GitNoResidueAfterInsertFailure INSERT 失败：清理
// 已写盘的 zip + 无行（写盘失败走同一清理分支——先写盘后 INSERT 的顺序下
// 两侧失败都不得留残骸）。zip 路径含 idgen 生成的 deployment id，残留检查
// 用目录扫描。
func TestCreateDeployment_GitNoResidueAfterInsertFailure(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	packer := newFakePacker()
	uc, repo := gitTestUC(t, exec, packer)
	repo.mu.Lock()
	repo.createDeploymentErr = errors.New("db down")
	repo.mu.Unlock()

	_, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{ProjectID: "p1", FunctionID: "fn_1", Git: gitSource()})
	require.Error(t, err)
	require.Empty(t, repo.deployments, "INSERT 失败无行")

	dir := filepath.Join(zipRoot(), "p1", "fn_1")
	entries, readErr := os.ReadDir(dir)
	if os.IsNotExist(readErr) {
		return
	}
	require.NoError(t, readErr)
	require.Empty(t, entries, "INSERT 失败后 zip 目录不得残留文件（清理分支覆盖）")
}

// TestCreateDeployment_BuildFailureZipRetentionBySourceType 构建失败的 zip
// 分流（设计 §2 重建语义）表驱动：git 源 zip 保留（worker 补构建以盘上
// zip 为输入、不依赖一次性凭证）、zip 源 zip 删除（D13 一期形态）；两者
// 状态机同收敛 failed。
func TestCreateDeployment_BuildFailureZipRetentionBySourceType(t *testing.T) {
	for _, tc := range []struct {
		name    string
		git     bool
		keepZip bool
	}{
		{name: "git 源构建失败 zip 保留", git: true, keepZip: true},
		{name: "zip 源构建失败 zip 删除", git: false, keepZip: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := newMockExecutor(nil, nil)
			exec.buildErr = context.DeadlineExceeded
			packer := newFakePacker()
			uc, repo := gitTestUC(t, exec, packer)

			cmd := CreateDeploymentCommand{ProjectID: "p1", FunctionID: "fn_1"}
			if tc.git {
				cmd.Git = gitSource()
			} else {
				cmd.Code = []byte("PK\x03\x04fake-zip")
			}
			dep, err := uc.CreateDeployment(serverCtx(), cmd)
			require.NoError(t, err, "构建失败在状态机内收敛，不向上抛")
			require.Equal(t, domainfunctions.DeploymentStatusFailed, dep.Status)
			require.NotEmpty(t, dep.Error)

			stored, err := repo.GetDeployment(context.Background(), "p1", "fn_1", dep.ID)
			require.NoError(t, err)
			require.Equal(t, domainfunctions.DeploymentStatusFailed, stored.Status, "failed 行保留（补构建入口）")

			path := zipPath("p1", "fn_1", dep.ID)
			t.Cleanup(func() { _ = removeZip("p1", "fn_1", dep.ID) })
			if tc.keepZip {
				require.FileExists(t, path, "git 源构建失败 zip 保留（重建语义）")
				require.Equal(t, domainfunctions.DeploymentSourceGit, stored.SourceType)
			} else {
				require.NoFileExists(t, path, "zip 源构建失败即删（D13 一期形态）")
			}
		})
	}
}

// TestCreateDeployment_GitPackerUnavailable packer 未注入（旧构造/worker
// 装配）：git 部署 fail-fast FailedPrecondition，文案与 client url 未配置
// 一致（对用户同一事实——git 源未启用）；zip 路径不受影响。
func TestCreateDeployment_GitPackerUnavailable(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	uc, repo := gitTestUC(t, exec, nil)

	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{ProjectID: "p1", FunctionID: "fn_1", Git: gitSource()})
	require.Nil(t, dep)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "functions.packer.url is not configured (git deployment source disabled)")
	require.Empty(t, repo.deployments)
}

// TestCreateDeployment_GitShapeValidation 形状校验（防御性复核层）表驱动：
// url https 前缀（allow_insecure 放行 http）/ref 白名单/dir 无穿越段；
// packer 不应被调用（坏形状在调 packer 前拒绝）。
func TestCreateDeployment_GitShapeValidation(t *testing.T) {
	cases := []struct {
		name          string
		url           string
		ref           string
		dir           string
		allowInsecure bool
		wantCode      codes.Code
		wantMsg       string
	}{
		{name: "http url 默认拒绝", url: "http://git.example.com/a.git", wantCode: codes.InvalidArgument, wantMsg: "must use https://"},
		{name: "http url allow_insecure 放行到 packer（此处不拦）", url: "http://gitea.internal/a.git", allowInsecure: true},
		{name: "ftp url 拒绝", url: "ftp://git.example.com/a.git", allowInsecure: true, wantCode: codes.InvalidArgument, wantMsg: "must use https://"},
		{name: "空 url 拒绝", wantCode: codes.InvalidArgument, wantMsg: "url is required"},
		{name: "ref 非白名单字符", url: "https://git.example.com/a.git", ref: "main;rm -rf", wantCode: codes.InvalidArgument, wantMsg: "ref may only contain"},
		{name: "ref shell 元字符", url: "https://git.example.com/a.git", ref: "$(id)", wantCode: codes.InvalidArgument, wantMsg: "ref may only contain"},
		{name: "dir 穿越段", url: "https://git.example.com/a.git", dir: "functions/../../etc", wantCode: codes.InvalidArgument, wantMsg: "'..'"},
		{name: "dir 绝对路径", url: "https://git.example.com/a.git", dir: "/etc/passwd", wantCode: codes.InvalidArgument, wantMsg: "relative path"},
		{name: "dir 反斜杠", url: "https://git.example.com/a.git", dir: `functions\greet`, wantCode: codes.InvalidArgument, wantMsg: "'/' separators"},
		{name: "合法形状走到 packer", url: "https://git.example.com/a.git", ref: "release/v1.2", dir: "funcs/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := newMockExecutor(nil, nil)
			packer := newFakePacker()
			uc, repo := gitTestUC(t, exec, packer)
			uc.cfg = &config.AppConfig{Functions: &config.Functions{
				Packer: &config.Functions_Packer{AllowInsecure: tc.allowInsecure},
			}}

			dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
				ProjectID:  "p1",
				FunctionID: "fn_1",
				Git:        &domainfunctions.GitSource{URL: tc.url, Ref: tc.ref, Directory: tc.dir},
			})
			if tc.wantCode == codes.OK {
				// 形状合法：应走到 packer（zip 落盘成功、函数存在）。
				require.NoError(t, err)
				require.NotNil(t, dep)
				t.Cleanup(func() { _ = removeZip("p1", "fn_1", dep.ID) })
				require.Len(t, packer.got, 1)
				return
			}
			require.Nil(t, dep)
			require.Equal(t, tc.wantCode, status.Code(err))
			require.ErrorContains(t, err, tc.wantMsg)
			require.Empty(t, packer.got, "坏形状不得调 packer")
			require.Empty(t, repo.deployments)
		})
	}
}

// TestCreateDeployment_ZipPathUnaffected 占位拒绝移除后 zip 路径的既有校验
// 顺序不变：Git 为 nil 时仍走 code 非空 → zip 魔数链（空 code 的
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
