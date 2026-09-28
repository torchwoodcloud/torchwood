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

// 本文件是 IMPL-T2-3 的机制回归守卫（守卫②）：docker client 引用在
// dispatcher 的执行底座改造后必须**结构性不可复现**——不是「今天没调用」，
// 而是「代码路径上不存在该依赖」。
//
// 两层：
//  1. 源扫描：dispatcher 实现（本包）+ 入口装配（cmd/dispatcher）+
//     函数共享 infra（internal/infra/functions）的生产源文件不得 import
//     docker/moby 包；
//  2. 依赖图：整仓生产依赖图（go list -deps ./...）不得包含 docker/moby
//     包——外部依赖偷偷拉入同样被拦住（链接即失败）。

// dockerForbiddenPrefixes 是禁止出现的包前缀（docker client 与 moby 生态；
// 含 containers/image 相关实现包——本仓不再持有任何容器运行时依赖）。
var dockerForbiddenPrefixes = []string{
	"github.com/docker/",
	"github.com/moby/",
	"github.com/containerd/errdefs",
	"github.com/opencontainers/image-spec",
}

// guardedSourceRoots 是生产源扫描根（相对本测试文件所在包目录）。
var guardedSourceRoots = []string{
	".",
	filepath.Join("..", "cmd", "dispatcher"),
	filepath.Join("..", "internal", "infra", "functions"),
}

// TestNoDockerClientImportsInDispatcherSources 守卫②（源扫描）：dispatcher
// 实现与入口装配不得 import docker client（连兜底都不留——旧 daemon.go 的
// 镜像/容器/网络原语面整体退役）。
func TestNoDockerClientImportsInDispatcherSources(t *testing.T) {
	for _, root := range guardedSourceRoots {
		violations := scanDockerImports(t, root)
		for _, v := range violations {
			t.Error(v)
		}
	}
}

// TestNoDockerClientInProductionDependencyGraph 守卫②（依赖图）：整仓生产
// 依赖图（go list -deps ./...，不含测试文件）不得出现 docker/moby 包。
func TestNoDockerClientInProductionDependencyGraph(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "go", "list", "-deps", "./...")
	cmd.Dir = filepath.Join("..")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list -deps ./...: %v\n%s", err, stderr.String())
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" {
			continue
		}
		if forbiddenDockerPackage(pkg) {
			t.Errorf("production dependency graph includes forbidden package %s (dispatcher must not link any container runtime client)", pkg)
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

	got := scanDockerImports(t, dir)
	if len(got) != 1 {
		t.Fatalf("expect exactly 1 violation, got %d: %v", len(got), got)
	}
	if want := badPath + " imports forbidden package github.com/docker/docker/client"; got[0] != want {
		t.Errorf("violation mismatch:\n got: %s\nwant: %s", got[0], want)
	}
}

// scanDockerImports 递归扫描 root 下所有非测试 Go 源文件的 import，返回
// 违规描述列表；跳过 vendor/.git/node_modules/testdata 目录与本守卫文件。
func scanDockerImports(t *testing.T, root string) []string {
	t.Helper()
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case "vendor", ".git", "node_modules", "testdata":
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
