package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var forbiddenImportPrefixes = []string{
	"github.com/torchwoodcloud/torchwood/genproto",
	"google.golang.org/grpc",
	"google.golang.org/protobuf",
}

// scanForbiddenImports 递归扫描 root 下所有非测试 Go 源文件的 import，返回
// 违规描述列表（J6-5：替代旧的 os.ReadDir 两层扫描——新增子目录即绕过；
// filepath.WalkDir 全深度覆盖）。跳过 vendor/.git/node_modules/testdata 目录、
// *_test.go 与本守卫文件自身。
func scanForbiddenImports(t *testing.T, root string) []string {
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
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, imp := range f.Imports {
			impPath := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbiddenImportPrefixes {
				if strings.HasPrefix(impPath, bad) {
					violations = append(violations, fmt.Sprintf("%s imports forbidden package %s", path, impPath))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return violations
}

// TestNoProtoGRPCImports 兜底：CLI 源码不得直接 import genproto/grpc/protobuf。
func TestNoProtoGRPCImports(t *testing.T) {
	for _, v := range scanForbiddenImports(t, ".") {
		t.Error(v)
	}
}

// TestNoProtoGRPCImportsDetectsNestedViolation 守卫自测：在临时目录构造
// 嵌套两层以上的子包并植入违规 import，验证递归扫描能抓到（旧的二层
// os.ReadDir 扫描对此静默漏报）。
func TestNoProtoGRPCImportsDetectsNestedViolation(t *testing.T) {
	dir := t.TempDir()

	nested := filepath.Join(dir, "pkg", "nested", "deeper")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(nested, "bad.go")
	badSrc := "package deeper\n\nimport _ \"google.golang.org/grpc\"\n"
	if err := os.WriteFile(badPath, []byte(badSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	goodPath := filepath.Join(dir, "clean.go")
	goodSrc := "package main\n\nimport \"fmt\"\n\nvar _ = fmt.Sprintf\n"
	if err := os.WriteFile(goodPath, []byte(goodSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	got := scanForbiddenImports(t, dir)
	if len(got) != 1 {
		t.Fatalf("expect exactly 1 violation, got %d: %v", len(got), got)
	}
	if want := badPath + " imports forbidden package google.golang.org/grpc"; got[0] != want {
		t.Errorf("violation mismatch:\n got: %s\nwant: %s", got[0], want)
	}
}
