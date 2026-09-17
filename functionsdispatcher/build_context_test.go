package functionsdispatcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/functionspacker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖构建上下文准备接线（Go 一期阶段 2，设计
// docs/design/functions-runtimes-and-sources.md §0/§1）：prepareBuildContext
// 的顺序敏感编排——解压探测 → D7 runtime 一致性对账 → go 分支 twmain
// bootstrap 生成（DetectEntry 在写 twmain 之前）→ Dockerfile 渲染。

const goModFixture = "module example.com/fn\n\ngo 1.26\n"

const goMainFixture = "package fn\n\n" +
	"import \"net/http\"\n\n" +
	"func Main(data map[string]any, ctx map[string]string) (any, error) { return data, nil }\n\n" +
	"func Fetch(w http.ResponseWriter, r *http.Request) {}\n"

// goZip 无 Fetch/Main 会报错，这里 Main/Fetch 双轨并存（探测 Fetch 优先）。
const goFetchOnlyFixture = "package fn\n\n" +
	"import \"net/http\"\n\n" +
	"func Fetch(w http.ResponseWriter, r *http.Request) {}\n"

func TestPrepareBuildContext_RuntimeMismatch(t *testing.T) {
	buildDir := t.TempDir()
	// zip 探测为 go（go.mod + Main），声明 runtime 为 node → InvalidArgument
	// 且错误信息含两侧值（D7）。
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZipFiles(t, map[string]string{"go.mod": goModFixture, "main.go": goMainFixture}),
		Runtime: "node-18.0",
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	msg := status.Convert(err).Message()
	require.Contains(t, msg, "node-18.0", "错误信息须含声明 runtime")
	require.Contains(t, msg, "go-1.26", "错误信息须含探测 runtime")

	// 对账先于 bootstrap 生成：mismatch 时 twmain/ 不得已写入。
	_, statErr := os.Stat(filepath.Join(buildDir, "twmain"))
	require.True(t, os.IsNotExist(statErr), "runtime 对账失败时不得生成 twmain/")

	// 反向：zip 探测为 node（index.js）、声明 go-1.26 → 同样 InvalidArgument。
	err = prepareBuildContext(t.TempDir(), BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZip(t, "index.js", "module.exports.main = () => ({})"),
		Runtime: "go-1.26",
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "go-1.26")
}

func TestPrepareBuildContext_RuntimeEmptySkipsReconcile(t *testing.T) {
	buildDir := t.TempDir()
	// Runtime 空 = 跳过对账（兼容未携带 runtime 的历史调用方）：node zip 照常
	// 出 node 模板。
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip: makeEntryZip(t, "index.js", "module.exports.main = () => ({})"),
	})
	require.NoError(t, err)
	dockerfile, err := os.ReadFile(filepath.Join(buildDir, "Dockerfile"))
	require.NoError(t, err)
	require.Contains(t, string(dockerfile), "FROM node:18-alpine")
	_, statErr := os.Stat(filepath.Join(buildDir, ".tw-runner.js"))
	require.NoError(t, statErr, "node 分支照旧写入 .tw-runner.js")
}

// TestPrepareBuildContext_GoBootstrapWiring go 分支接线（fake/临时目录驱动，
// 不依赖真实 docker）：twmain 两文件生成、内容含 module import 与 serve 入口、
// Dockerfile 渲染前 twmain 已写入（COPY twmain/ 的源存在）。
func TestPrepareBuildContext_GoBootstrapWiring(t *testing.T) {
	buildDir := t.TempDir()
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZipFiles(t, map[string]string{"go.mod": goModFixture, "main.go": goMainFixture}),
		Runtime: "go-1.26",
	})
	require.NoError(t, err)

	mainGo, err := os.ReadFile(filepath.Join(buildDir, "twmain", "main.go"))
	require.NoError(t, err, "twmain/main.go 必须生成")
	require.Contains(t, string(mainGo), `import user "example.com/fn"`, "生成入口必须引用用户 module path")
	require.Contains(t, string(mainGo), "func main()", "生成入口必须是 main 包入口")
	require.Contains(t, string(mainGo), "serve(entry{fetch: user.Fetch})", "Fetch 优先（探测双轨优先级同 runner.js）")

	runtimeGo, err := os.ReadFile(filepath.Join(buildDir, "twmain", "runtime.go"))
	require.NoError(t, err, "twmain/runtime.go 必须生成")
	require.Contains(t, string(runtimeGo), "package main")

	dockerfile, err := os.ReadFile(filepath.Join(buildDir, "Dockerfile"))
	require.NoError(t, err)
	require.Contains(t, string(dockerfile), "FROM golang:1.26-alpine")
	require.Contains(t, string(dockerfile), "COPY twmain/ ./twmain/",
		"go 模板 COPY twmain/——写入时点必须在 DockerfileFor 之前")
	require.NotContains(t, string(dockerfile), "node:18-alpine")
}

// TestPrepareBuildContext_GoMainFallback main 风格兜底：仅 Main 时 serve
// 入口走 main 轨。
func TestPrepareBuildContext_GoMainFallback(t *testing.T) {
	buildDir := t.TempDir()
	goMainOnly := "package fn\n\nfunc Main(data map[string]any, ctx map[string]string) (any, error) { return nil, nil }\n"
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZipFiles(t, map[string]string{"go.mod": goModFixture, "main.go": goMainOnly}),
		Runtime: "go-1.26",
	})
	require.NoError(t, err)
	mainGo, err := os.ReadFile(filepath.Join(buildDir, "twmain", "main.go"))
	require.NoError(t, err)
	require.Contains(t, string(mainGo), "serve(entry{main: user.Main})")
}

// TestPrepareBuildContext_GoMissingEntry 两者皆无 → 构建期明确报错（对齐
// node「index.js must export main or fetch」）。
func TestPrepareBuildContext_GoMissingEntry(t *testing.T) {
	buildDir := t.TempDir()
	err := prepareBuildContext(buildDir, BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZipFiles(t, map[string]string{"go.mod": goModFixture, "util.go": "package fn\n\nfunc A() {}\n"}),
		Runtime: "go-1.26",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "must export Fetch or Main")
	_, statErr := os.Stat(filepath.Join(buildDir, "twmain"))
	require.True(t, os.IsNotExist(statErr), "探测失败不得留下半成品 twmain/")
}

// TestPrepareBuildContext_GoTwmainConflict zip 携带 twmain/ 保留目录 →
// DockerfileFor 拒收（TwmainConflict 基于解压期 zip 条目清单，先于平台
// 写入判定——写入时点正确性的另一面：平台产物不会先落盘再被冲突检查误伤）。
func TestPrepareBuildContext_GoTwmainConflict(t *testing.T) {
	err := prepareBuildContext(t.TempDir(), BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip: makeEntryZipFiles(t, map[string]string{
			"go.mod":      goModFixture,
			"main.go":     goMainFixture,
			"twmain/x.go": "package main\n",
		}),
		Runtime: "go-1.26",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "twmain")
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
	overLimit := make(map[string]string, functionspacker.MaxPackEntries+1)
	overLimit["index.js"] = "module.exports.main = () => ({})"
	for i := 0; i < functionspacker.MaxPackEntries; i++ {
		overLimit[fmt.Sprintf("pkg/dir%d/file%04d.txt", i%37, i)] = "x"
	}
	err = prepareBuildContext(t.TempDir(), BuildImageOptions{
		FunctionID: "fn1", DeploymentID: "dep1",
		Zip:     makeEntryZipFiles(t, overLimit),
		Runtime: "node-18.0",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), fmt.Sprintf("max %d", functionspacker.MaxPackEntries))
}
