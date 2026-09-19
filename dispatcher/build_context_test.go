package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/packer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖构建上下文准备接线（五期 5b：Go 用户契约反转，设计
// docs/design/functions-runtimes-and-sources.md 顶部立项段）：prepareBuildContext
// 的顺序敏感编排——解压探测 → D7 runtime 一致性对账 → Dockerfile 渲染；
// go 分支零平台注入（zip 根 = 用户 package main，构建 = go build .），
// node 分支照旧写 .tw-runner.js。

// goModFixture 是纯 stdlib go 模块的 go.mod（require 空 = 合法无 go.sum）。
const goModFixture = "module example.com/fn\n\ngo 1.26\n"

// goMainFixture 是用户持有的根 main 包（五期 5b 契约：SDK 或自写 HTTP 服务
// 在 main 里承接平台分发；本单测不编译，形态正确即可）。
const goMainFixture = "package main\n\nfunc main() {}\n"

func TestPrepareBuildContext_RuntimeMismatch(t *testing.T) {
	buildDir := t.TempDir()
	// zip 探测为 go（go.mod），声明 runtime 为 node → InvalidArgument
	// 且错误信息含声明 ID 与探测 family（D7'：探测产出语言族，版本轴来自
	// 声明，docs/design/functions-runtime-selection.md §2）。
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZipFiles(t, map[string]string{"go.mod": goModFixture, "main.go": goMainFixture}),
		Runtime: "node-18.0",
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	msg := status.Convert(err).Message()
	require.Contains(t, msg, "node-18.0", "错误信息须含声明 runtime")
	require.Contains(t, msg, `"go"`, "错误信息须含探测 family")

	// 对账先于模板渲染：mismatch 时 Dockerfile 不得已落盘。
	_, statErr := os.Stat(filepath.Join(buildDir, "Dockerfile"))
	require.True(t, os.IsNotExist(statErr), "runtime 对账失败时不得渲染 Dockerfile")

	// 反向：zip 探测为 node（index.js）、声明 go-1.26 → 同样 InvalidArgument。
	err = prepareBuildContext(t.TempDir(), BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZip(t, "index.js", "module.exports.main = () => ({})"),
		Runtime: "go-1.26",
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "go-1.26")

	// 未知 runtime ID 同样 fail-closed（表外 ID 无法解析 family）。
	err = prepareBuildContext(t.TempDir(), BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZip(t, "index.js", "module.exports.main = () => ({})"),
		Runtime: "node-99.0",
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "node-99.0")
}

// TestPrepareBuildContext_VersionAxisFromDeclaration 版本轴来自声明（D7'
// 核心）：同 family（node）下探测恒产出 node 标记，声明的 runtime ID 决定
// 渲染基座——node-22.0 声明渲染 node:22-alpine，与 node-18.0 声明互不干扰。
func TestPrepareBuildContext_VersionAxisFromDeclaration(t *testing.T) {
	for _, tc := range []struct{ runtime, baseImage string }{
		{"node-18.0", "node:18-alpine"},
		{"node-22.0", "node:22-alpine"},
		{"node-24.0", "node:24-alpine"},
	} {
		buildDir := t.TempDir()
		err := prepareBuildContext(buildDir, BuildImageOptions{
			FunctionID: "fn1", DeploymentID: "dep1",
			Zip:     makeEntryZip(t, "index.js", "module.exports.main = () => ({})"),
			Runtime: tc.runtime,
		})
		require.NoError(t, err)
		dockerfile, err := os.ReadFile(filepath.Join(buildDir, "Dockerfile")) // #nosec G304 -- 读取本测试 t.TempDir() 构建目录内的产物
		require.NoError(t, err)
		require.Contains(t, string(dockerfile), "FROM "+tc.baseImage,
			"声明的 runtime ID 决定渲染基座")
	}
}

func TestPrepareBuildContext_RuntimeEmptySkipsReconcile(t *testing.T) {
	buildDir := t.TempDir()
	// Runtime 空 = 遗留调用方（跳过对账）：渲染基准取探测 family 的首个
	// active 表项（node-24.0，平台缺省）——不再隐含历史 node:18 耦合（D8）。
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip: makeEntryZip(t, "index.js", "module.exports.main = () => ({})"),
	})
	require.NoError(t, err)
	dockerfile, err := os.ReadFile(filepath.Join(buildDir, "Dockerfile"))
	require.NoError(t, err)
	require.Contains(t, string(dockerfile), "FROM node:24-alpine")
	_, statErr := os.Stat(filepath.Join(buildDir, ".tw-runner.js"))
	require.NoError(t, statErr, "node 分支照旧写入 .tw-runner.js")
}

// TestPrepareBuildContext_GoUserMainContract go 分支接线（fake/临时目录驱动，
// 不依赖真实 docker；五期 5b 用户契约反转）：平台零注入——不生成 twmain/、
// 不做入口探测，Dockerfile 构建目标 = zip 根 main 包（go build .）。
func TestPrepareBuildContext_GoUserMainContract(t *testing.T) {
	buildDir := t.TempDir()
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZipFiles(t, map[string]string{"go.mod": goModFixture, "main.go": goMainFixture}),
		Runtime: "go-1.26",
	})
	require.NoError(t, err)

	// 平台零注入：无 twmain/ 目录（twmain 概念已删，平台不再生成任何 Go 源码）。
	_, statErr := os.Stat(filepath.Join(buildDir, "twmain"))
	require.True(t, os.IsNotExist(statErr), "平台不得生成 twmain/（零注入契约）")

	dockerfile, err := os.ReadFile(filepath.Join(buildDir, "Dockerfile"))
	require.NoError(t, err)
	require.Contains(t, string(dockerfile), "FROM golang:1.26-alpine")
	require.Contains(t, string(dockerfile), `RUN go build -trimpath -ldflags="-s -w" -o /out/tw-app .`,
		"构建目标 = zip 根 main 包（go build .）")
	require.NotContains(t, string(dockerfile), "twmain", "twmain 生成机制已删除")
	require.NotContains(t, string(dockerfile), "node:18-alpine")
}

// TestPrepareBuildContext_GoNonMainRootNoPrecheck 根包非 main 不做前置校验
// （最小变更取舍）：平台不在探测层校验根包形态——实测 `go build -o` 对非
// main 根包 exit 0（产物为包档案非可执行），失败推迟到验证 spawn 拦截
// （真实 daemon 形态对照 TestIntegration_GoNonMainRootBuildRejected）。
func TestPrepareBuildContext_GoNonMainRootNoPrecheck(t *testing.T) {
	buildDir := t.TempDir()
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip: makeEntryZipFiles(t, map[string]string{
			"go.mod":  goModFixture,
			"util.go": "package hello\n\nfunc A() {}\n",
		}),
		Runtime: "go-1.26",
	})
	require.NoError(t, err, "平台不做根包 main 前置校验（构建日志透传）")
}

// TestPrepareBuildContext_GoTwmainDirAccepted twmain/ 不再是平台保留目录
// （五期 5b：生成式 bootstrap 已删）：用户 zip 携带同名目录照常构建上下文。
func TestPrepareBuildContext_GoTwmainDirAccepted(t *testing.T) {
	err := prepareBuildContext(t.TempDir(), BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip: makeEntryZipFiles(t, map[string]string{
			"go.mod":      goModFixture,
			"main.go":     goMainFixture,
			"twmain/x.go": "package main\n",
		}),
		Runtime: "go-1.26",
	})
	require.NoError(t, err, "twmain/ 概念已删，用户目录不受平台约束")
}

// TestPrepareBuildContext_GoMissingSum 纯 stdlib（require 空）无 go.sum 合法；
// require 非空且无 go.sum → 模板层拒收（探测标记 → DockerfileFor 报错链）。
func TestPrepareBuildContext_GoMissingSum(t *testing.T) {
	err := prepareBuildContext(t.TempDir(), BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip: makeEntryZipFiles(t, map[string]string{
			"go.mod":  goModFixture + "\nrequire github.com/some/dep v1.0.0\n",
			"main.go": goMainFixture,
		}),
		Runtime: "go-1.26",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "go.sum")
}

// TestPrepareBuildContext_VendorBranch vendor 受纳（D4）：有 vendor/ 时缺
// go.sum 不再拒收，GOFLAGS 走 -mod=vendor 分支。
func TestPrepareBuildContext_GoVendorBranch(t *testing.T) {
	buildDir := t.TempDir()
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip: makeEntryZipFiles(t, map[string]string{
			"go.mod":             goModFixture + "\nrequire github.com/some/dep v1.0.0\n",
			"main.go":            goMainFixture,
			"vendor/modules.txt": "# github.com/some/dep v1.0.0\n",
			"vendor/x/x.go":      "package x\n",
		}),
		Runtime: "go-1.26",
	})
	require.NoError(t, err)
	dockerfile, err := os.ReadFile(filepath.Join(buildDir, "Dockerfile"))
	require.NoError(t, err)
	require.Contains(t, string(dockerfile), "ENV GOFLAGS=-mod=vendor")
	require.NotContains(t, string(dockerfile), "go mod download")
	require.Contains(t, string(dockerfile), `-o /out/tw-app .`, "vendor 分支构建目标同样是根 main 包")
}

// TestPrepareBuildContext_NodeDepsLayerfile node 代装模板回归（接线不回退）：
// 依赖 + lockfile → 分层模板含 npm ci。
func TestPrepareBuildContext_NodeDepsLayered(t *testing.T) {
	buildDir := t.TempDir()
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip: makeEntryZipFiles(t, map[string]string{
			"index.js":          "module.exports.main = () => ({})",
			"package.json":      `{"dependencies": {"left-pad": "1.3.0"}}`,
			"package-lock.json": `{"lockfileVersion": 1}`,
		}),
		Runtime: "node-18.0",
	})
	require.NoError(t, err)
	dockerfile, err := os.ReadFile(filepath.Join(buildDir, "Dockerfile"))
	require.NoError(t, err)
	require.True(t, strings.Contains(string(dockerfile), "npm ci --omit=dev --ignore-scripts"))
}

// TestPrepareBuildContext_EnginesNodeReconcile engines.node 接线
// （functions-runtime-selection.md §5）：声明 range 排除所选 runtime 的
// major → 构建期 InvalidArgument（错误含声明 range 与可选 runtime）；相交
// 或未声明 → 照常渲染。
func TestPrepareBuildContext_EnginesNodeReconcile(t *testing.T) {
	mkZip := func(engines string) []byte {
		pkg := `{"dependencies": {"left-pad": "1.3.0"}`
		if engines != "" {
			pkg += `, "engines": {"node": "` + engines + `"}`
		}
		pkg += `}`
		return makeEntryZipFiles(t, map[string]string{
			"index.js":          "module.exports.main = () => ({})",
			"package.json":      pkg,
			"package-lock.json": `{"lockfileVersion": 1}`,
		})
	}

	// 不相交：engines 要求 <23、声明 node-24.0 → InvalidArgument。
	err := prepareBuildContext(t.TempDir(), BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     mkZip("<23"),
		Runtime: "node-24.0",
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	msg := status.Convert(err).Message()
	require.Contains(t, msg, "<23")
	require.Contains(t, msg, "node-24.0")

	// 相交（宽松范围）与未声明：照常构建。
	for _, engines := range []string{"", ">=18", "^24"} {
		err := prepareBuildContext(t.TempDir(), BuildImageOptions{
			FunctionID: "fn1", DeploymentID: "dep1",
			Zip:     mkZip(engines),
			Runtime: "node-24.0",
		})
		require.NoError(t, err, "engines %q", engines)
	}
}

// TestPrepareBuildContext_RelaxedEntryBudget 二期阶段 3（设计 §2 条目维链条）：
// BuildImage 的解压走放宽版预算（ExtractZipRelaxed，条目对齐 packer 物化
// 口径 5000）——超过默认 zip 上传预算（1000 条目）的 zip 照常构建；超过
// 放宽上限仍 InvalidArgument。
func TestPrepareBuildContext_RelaxedEntryBudget(t *testing.T) {
	files := map[string]string{"index.js": "module.exports.main = () => ({})"}
	for i := 0; i < 1001; i++ {
		files[fmt.Sprintf("pkg/dir%d/file%04d.txt", i%37, i)] = "x"
	}
	err := prepareBuildContext(t.TempDir(), BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZipFiles(t, files),
		Runtime: "node-18.0",
	})
	require.NoError(t, err, "1001 条目 zip 须被放宽预算放行（默认 1000 条目预算会击毙 git 源合法 zip）")

	// 超过放宽上限（5000）仍拒绝。
	overLimit := make(map[string]string, packer.MaxPackEntries+1)
	overLimit["index.js"] = "module.exports.main = () => ({})"
	for i := 0; i < packer.MaxPackEntries; i++ {
		overLimit[fmt.Sprintf("pkg/dir%d/file%04d.txt", i%37, i)] = "x"
	}
	err = prepareBuildContext(t.TempDir(), BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZipFiles(t, overLimit),
		Runtime: "node-18.0",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), fmt.Sprintf("max %d", packer.MaxPackEntries))
}
