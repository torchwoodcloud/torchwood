package servergrpc

import (
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖部署源 oneof 分发与 Deployment 源投影（Functions 部署源多元化
// 二期阶段 1/4，docs/design/functions-runtimes-and-sources.md §0）：
// oneof 分发漏写分支与 mapDeployment 漏投影都不报编译错，须显式断言。

func gitSourceReq() *serverv1.GitSource {
	return &serverv1.GitSource{
		Url:       "https://git.example.com/acme/widget.git",
		Ref:       "main",
		Directory: "functions/greet",
		Username:  "git",
		Token:     "one-shot-token",
	}
}

func imageSourceReq() *serverv1.ImageSource {
	return &serverv1.ImageSource{ // #nosec G101 -- 测试夹具伪凭证
		Image:            "registry.example.com/acme/greet:v1",
		RegistryUsername: "bot",
		RegistryToken:    "one-shot-registry-token",
	}
}

// TestFunctionsService_CreateDeploymentOneOfDispatch oneof 分发表驱动：
//   - 纯 code：进 zip 路径（stubRepo 无函数 → NotFound，而非 source 缺失）；
//   - 纯 git：映射为命令 Git 字段 → 进入 app 层真实 git 分支（形状校验通过、
//     stubRepo 无函数 → NotFound，证明未被分发层拦截）；
//   - 纯 image：映射为命令 Image 字段 → 进入 app 层真实 image 分支（stubRepo
//     无函数 → NotFound，证明未被分发层拦截；三期阶段 1）；
//   - 三空：InvalidArgument（分发层拦截，app 层 code required 为纵深防御）。
func TestFunctionsService_CreateDeploymentOneOfDispatch(t *testing.T) {
	s := newTestService(&stubRepo{})
	ctx := principalCtx("p1")

	// 纯 code（zip 路径：function 不存在 → NotFound，证明未被分发层拦下）。
	_, err := s.CreateDeployment(ctx, &serverv1.CreateDeploymentRequest{
		FunctionId: "fn_1",
		Source:     &serverv1.CreateDeploymentRequest_Code{Code: []byte("PK\x03\x04")},
	})
	require.Equal(t, codes.NotFound, status.Code(err), "code oneof 应进入 zip 用例路径")
	require.ErrorContains(t, err, "function not found")

	// 纯 git（形状校验 → GetFunction NotFound；未配置 packer 的 fail-fast
	// 在 packer 端口缺失/url 未配置时才发生）。
	_, err = s.CreateDeployment(ctx, &serverv1.CreateDeploymentRequest{
		FunctionId: "fn_1",
		Source:     &serverv1.CreateDeploymentRequest_Git{Git: gitSourceReq()},
	})
	require.Equal(t, codes.NotFound, status.Code(err), "git oneof 应进入 app 层真实 git 分支")
	require.ErrorContains(t, err, "function not found")

	// 纯 image（三期阶段 1）：经 mapImageSource 进 app 层真实 image 分支。
	_, err = s.CreateDeployment(ctx, &serverv1.CreateDeploymentRequest{
		FunctionId: "fn_1",
		Source:     &serverv1.CreateDeploymentRequest_Image{Image: imageSourceReq()},
	})
	require.Equal(t, codes.NotFound, status.Code(err), "image oneof 应进入 app 层真实 image 分支")
	require.ErrorContains(t, err, "function not found")

	// 三空 → InvalidArgument。
	_, err = s.CreateDeployment(ctx, &serverv1.CreateDeploymentRequest{FunctionId: "fn_1"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "source is required")
	require.ErrorContains(t, err, "image", "错误文案须覆盖三源词表")
}

// TestMapDeployment_SourceProjection Deployment 源投影断言：git 源四字段
// 原样；image 源 source_url=原始引用、source_ref=digest、source_dir 恒空；
// zip 源恒 source_type=zip + 空串。凭证字段在域模型上不存在，投影天然不带。
func TestMapDeployment_SourceProjection(t *testing.T) {
	gitDep := mapDeployment(&domainfunctions.Deployment{
		ID:         "dep_git",
		FunctionID: "fn_1",
		Status:     domainfunctions.DeploymentStatusReady,
		SourceType: domainfunctions.DeploymentSourceGit,
		SourceURL:  "https://git.example.com/acme/widget.git",
		SourceRef:  "0123456789abcdef0123456789abcdef01234567",
		SourceDir:  "functions/greet",
	})
	require.Equal(t, "git", gitDep.SourceType)
	require.Equal(t, "https://git.example.com/acme/widget.git", gitDep.SourceUrl)
	require.Equal(t, "0123456789abcdef0123456789abcdef01234567", gitDep.SourceRef)
	require.Equal(t, "functions/greet", gitDep.SourceDir)

	// image 源（三期阶段 1）：source_url = 原始引用、source_ref = 钉死
	// digest、source_dir 恒空；registry 凭证不出现在投影。
	imageDep := mapDeployment(&domainfunctions.Deployment{
		ID:         "dep_image",
		FunctionID: "fn_1",
		Status:     domainfunctions.DeploymentStatusReady,
		SourceType: domainfunctions.DeploymentSourceImage,
		SourceURL:  "registry.example.com/acme/greet:v1",
		SourceRef:  "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	})
	require.Equal(t, "image", imageDep.SourceType)
	require.Equal(t, "registry.example.com/acme/greet:v1", imageDep.SourceUrl)
	require.Equal(t, "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", imageDep.SourceRef)
	require.Empty(t, imageDep.SourceDir, "image 源 source_dir 恒空")

	zipDep := mapDeployment(&domainfunctions.Deployment{
		ID:         "dep_zip",
		FunctionID: "fn_1",
		Status:     domainfunctions.DeploymentStatusReady,
		SourceType: domainfunctions.DeploymentSourceZip,
	})
	require.Equal(t, "zip", zipDep.SourceType)
	require.Empty(t, zipDep.SourceUrl, "zip 源 source_url 恒空")
	require.Empty(t, zipDep.SourceRef, "zip 源 source_ref 恒空")
	require.Empty(t, zipDep.SourceDir, "zip 源 source_dir 恒空")
}

// TestMapGitSource proto GitSource → 领域值对象逐字段映射（一次性凭证
// 随结构体仅在请求生命周期内存活）。
func TestMapGitSource(t *testing.T) {
	require.Nil(t, mapGitSource(nil))
	g := mapGitSource(gitSourceReq())
	require.Equal(t, "https://git.example.com/acme/widget.git", g.URL)
	require.Equal(t, "main", g.Ref)
	require.Equal(t, "functions/greet", g.Directory)
	require.Equal(t, "git", g.Username)
	require.Equal(t, "one-shot-token", g.Token)
}

// TestMapImageSource proto ImageSource → 领域值对象逐字段映射（三期阶段 1；
// 一次性 registry 凭证随结构体仅在请求生命周期内存活——不落库不回显，D8）。
func TestMapImageSource(t *testing.T) {
	require.Nil(t, mapImageSource(nil))
	g := mapImageSource(imageSourceReq())
	require.Equal(t, "registry.example.com/acme/greet:v1", g.Reference)
	require.Equal(t, "bot", g.RegistryUsername)
	require.Equal(t, "one-shot-registry-token", g.RegistryToken)
}
