package dispatcher

import (
	"bytes"
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是 IMPL-T2-3 引入、IMPL-T2-5 改口径的机制回归守卫：docker client
// 引用必须被隔离在 docker 执行底座子包（dispatcher/dockerdriver，本仓唯一
// docker client 持有面）内——fleetly 驱动路径结构性零 docker client，docker
// 形态的持有面唯一且经组合根显式链接。不是「今天没调用」，而是「代码路径
// 上不存在该依赖」。
//
// 三层：
//  1. 源扫描：fleetly 驱动路径（dispatcher 本包，跳过 dockerdriver/ 子包；
//     internal/infra/functions）的生产源文件不得 import docker/moby 包；
//     组合根（cmd/dispatcher）除 blank-import dockerdriver 完成驱动注册外
//     同样禁止；
//  2. 依赖图：整仓生产依赖图（go list ./... 排除 dockerdriver 隔离包与
//     cmd/dispatcher 组合根后取 go list -deps）不得包含 docker/moby 包
//     ——server/worker/packer 与 dispatcher 池/服务面在链接层可证零容器
//     运行时依赖；
//  3. 守卫自测：嵌套子目录植入违规 import 必须被抓到（递归扫描是契约）。

// dockerForbiddenPrefixes 是禁止出现的包前缀（docker client 与 moby 生态；
// 含 containers/image 相关实现包——fleetly 驱动路径不持有任何容器运行时依赖）。
var dockerForbiddenPrefixes = []string{
	"github.com/docker/",
	"github.com/moby/",
	"github.com/containerd/errdefs",
	"github.com/opencontainers/image-spec",
}

// guardedSourceRoots 是 fleetly 驱动路径的生产源扫描根（相对本测试文件
// 所在包目录）：dispatcher 本包（scanDockerImports 跳过 dockerdriver/
// 隔离子包）与函数共享 infra。
var guardedSourceRoots = []string{
	".",
	filepath.Join("..", "internal", "infra", "functions"),
}

// compositionRootSourceDir 是组合根（唯一允许 import dockerdriver 的包）：
// 该目录源码除 dockerdriver 隔离包注册 import 外，其余 docker/moby import
// 一律违规。
var compositionRootSourceDir = filepath.Join("..", "cmd", "dispatcher")

// compositionRootAllowedImport 是组合根唯一豁免的 import（驱动注册）。
const compositionRootAllowedImport = "github.com/torchwoodcloud/torchwood/dispatcher/dockerdriver"

// compositionRootExcludedFromDepGraph / driverPackageExcludedFromDepGraph
// 是依赖图断言的两个豁免包（import 路径前缀）：
//   - dockerdriver 是 docker 驱动隔离包本身（豁免即「docker 底座文件按包
//     边界豁免」口径）；
//   - cmd/dispatcher 是组合根（显式 blank-import 驱动包完成注册）。
var compositionRootExcludedFromDepGraph = "github.com/torchwoodcloud/torchwood/cmd/dispatcher"

var driverPackageExcludedFromDepGraph = "github.com/torchwoodcloud/torchwood/dispatcher/dockerdriver"

// TestNoDockerClientImportsInDispatcherSources 守卫（源扫描）：fleetly
// 驱动路径与组合根不得 import docker client（连兜底都不留——组合根仅允许
// driverdocker 驱动注册 import）。
func TestNoDockerClientImportsInDispatcherSources(t *testing.T) {
	for _, root := range guardedSourceRoots {
		for _, v := range scanDockerImports(t, root, "") {
			t.Error(v)
		}
	}
	for _, v := range scanDockerImports(t, compositionRootSourceDir, compositionRootAllowedImport) {
		t.Error(v)
	}
}

// TestNoDockerClientInFleetlyDriverDependencyGraph 守卫（依赖图）：整仓
// 生产依赖图（go list ./...，排除 docker 驱动隔离包与组合根后取
// go list -deps）不得出现 docker/moby 包。
func TestNoDockerClientInFleetlyDriverDependencyGraph(t *testing.T) {
	ctx := context.Background()
	pkgs := listPackages(t, ctx, filepath.Join(".."))
	fleetlyPathPkgs := make([]string, 0, len(pkgs))
	for _, pkg := range pkgs {
		if pkg == compositionRootExcludedFromDepGraph || pkg == driverPackageExcludedFromDepGraph {
			continue
		}
		fleetlyPathPkgs = append(fleetlyPathPkgs, pkg)
	}
	deps := listDeps(t, ctx, filepath.Join(".."), fleetlyPathPkgs)
	for _, pkg := range deps {
		if forbiddenDockerPackage(pkg) {
			t.Errorf("fleetly driver dependency graph includes forbidden package %s (the docker client must stay quarantined in dispatcher/dockerdriver)", pkg)
		}
	}
}

// TestNoDockerClientImportsInSourcesDetectsNestedViolation 守卫自测：嵌套
// 子目录植入违规 import 必须被抓到（递归扫描是契约，不是形式）。
func TestNoDockerClientImportsInSourcesDetectsNestedViolation(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "pkg", "nested", "deeper")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(nested, "bad.go")
	badSrc := "package deeper\n\nimport _ \"github.com/docker/docker/client\"\n"
	if err := os.WriteFile(badPath, []byte(badSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	goodPath := filepath.Join(dir, "clean.go")
	goodSrc := "package main\n\nimport \"fmt\"\n\nvar _ = fmt.Sprintf\n"
	if err := os.WriteFile(goodPath, []byte(goodSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	got := scanDockerImports(t, dir, "")
	if len(got) != 1 {
		t.Fatalf("expect exactly 1 violation, got %d: %v", len(got), got)
	}
	if want := badPath + " imports forbidden package github.com/docker/docker/client"; got[0] != want {
		t.Errorf("violation mismatch:\n got: %s\nwant: %s", got[0], want)
	}
}

// TestScanDockerImportsAllowsCompositionRootDriverRegistration 守卫自测：
// 组合根的 driverdocker blank import 豁免单点生效——其余 docker import
// 仍被抓到。
func TestScanDockerImportsAllowsCompositionRootDriverRegistration(t *testing.T) {
	dir := t.TempDir()
	register := filepath.Join(dir, "register.go")
	registerSrc := "package main\n\nimport _ \"github.com/torchwoodcloud/torchwood/dispatcher/dockerdriver\"\n"
	if err := os.WriteFile(register, []byte(registerSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(dir, "bad.go")
	badSrc := "package main\n\nimport _ \"github.com/moby/sys/atomicwriter\"\n"
	if err := os.WriteFile(badPath, []byte(badSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := scanDockerImports(t, dir, compositionRootAllowedImport); len(got) != 1 {
		t.Fatalf("expect exactly 1 violation (the non-driver docker import), got %d: %v", len(got), got)
	}
}

// listPackages 返回 root 下 go list ./... 的包列表（不含测试文件）。
func listPackages(t *testing.T, ctx context.Context, dir string) []string {
	t.Helper()
	return runGoList(t, ctx, dir, "list", "./...")
}

// listDeps 返回 pkgs 的生产依赖闭包（go list -deps，不含测试文件）。
func listDeps(t *testing.T, ctx context.Context, dir string, pkgs []string) []string {
	t.Helper()
	args := append([]string{"list", "-deps"}, pkgs...)
	return runGoList(t, ctx, dir, args...)
}

// runGoList 执行 go list 并按行返回输出。
func runGoList(t *testing.T, ctx context.Context, dir string, args ...string) []string {
	t.Helper()
	// #nosec G204 -- args 是「go」固定字面量 + go list 的模式/包路径清单
	//（包路径来自 go list ./... 的枚举，非用户输入）。
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var out []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if pkg := strings.TrimSpace(line); pkg != "" {
			out = append(out, pkg)
		}
	}
	return out
}

// scanDockerImports 递归扫描 root 下所有非测试 Go 源文件的 import，返回
// 违规描述列表；跳过 vendor/.git/node_modules/testdata/dockerdriver 目录与
// 本守卫文件。allowedImport 非空时该单一 import 路径豁免（组合根的驱动
// 注册 blank import；空 = 无豁免）。
func scanDockerImports(t *testing.T, root string, allowedImport string) []string {
	t.Helper()
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case "vendor", ".git", "node_modules", "testdata", "dockerdriver":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") ||
			name == "import_guard_test.go" {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		for _, imp := range f.Imports {
			impPath := strings.Trim(imp.Path.Value, `"`)
			if impPath == allowedImport {
				continue
			}
			if forbiddenDockerPackage(impPath) {
				violations = append(violations, fmt.Sprintf("%s imports forbidden package %s", path, impPath))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return violations
}

// forbiddenDockerPackage 报告包路径是否命中禁止前缀（精确/子包）。
func forbiddenDockerPackage(pkg string) bool {
	for _, prefix := range dockerForbiddenPrefixes {
		if pkg == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(pkg, prefix) {
			return true
		}
	}
	return false
}
