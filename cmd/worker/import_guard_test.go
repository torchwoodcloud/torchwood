package main

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/torchwoodcloud/torchwood"

// barrel 精确匹配：禁止把整个 app/infra 桶包拉进 worker。
var forbiddenExact = []string{
	modulePath + "/internal/app",
	modulePath + "/internal/infra",
}

// 前缀匹配：Account / gRPC / documentdb 等不得进入 worker 依赖图。
var forbiddenPrefix = []string{
	modulePath + "/internal/app/client",
	modulePath + "/internal/app/console",
	modulePath + "/internal/app/server",
	modulePath + "/internal/api",
	modulePath + "/internal/infra/auth",
	modulePath + "/internal/infra/documentdb",
	modulePath + "/internal/infra/server",
	modulePath + "/genproto",
}

func isForbiddenImport(path string) bool {
	for _, exact := range forbiddenExact {
		if path == exact {
			return true
		}
	}
	for _, prefix := range forbiddenPrefix {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// TestWorkerSourceImports 兜底：cmd/worker 生产源码不得 import 禁止包。
func TestWorkerSourceImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(".", name)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			impPath := strings.Trim(imp.Path.Value, `"`)
			if isForbiddenImport(impPath) {
				t.Errorf("%s imports forbidden package %s", path, impPath)
			}
		}
	}
}

// TestWorkerDepsGraph 兜底：go list -deps 传递依赖也不得包含禁止包。
func TestWorkerDepsGraph(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "go", "list", "-deps", ".")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list -deps .: %v\n%s", err, stderr.String())
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" {
			continue
		}
		if isForbiddenImport(pkg) {
			t.Errorf("go list -deps . includes forbidden package %s", pkg)
		}
	}
}
