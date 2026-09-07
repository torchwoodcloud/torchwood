// genauthzmatrix 生成 docs/developer/authz-matrix.md（授权矩阵文档）：
// 数据源 = 真实 proto 的 PolicySet（与启动期同源）。再生方式：task gen:authz-matrix。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"

	intruntime "github.com/torchwooddev/torchwood/internal/runtime"
)

func main() {
	payload, err := intruntime.RenderAuthzMatrixFromProto()
	if err != nil {
		fail(err)
	}
	// 输出路径锚定仓库根（本文件位于 <root>/internal/runtime/cmd/genauthzmatrix）。
	target := filepath.Join(repoRoot(), "docs", "developer", "authz-matrix.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fail(err)
	}
	if err := os.WriteFile(target, payload, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("authz-matrix 文档已生成：%s（%d 字节）\n", target, len(payload))
}

// repoRoot 以本文件编译期位置向上回溯到仓库根
// （<root>/internal/runtime/cmd/genauthzmatrix → 根）。
func repoRoot() string {
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		fail(fmt.Errorf("无法定位源文件位置"))
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "genauthzmatrix: %v\n", err)
	os.Exit(1)
}
