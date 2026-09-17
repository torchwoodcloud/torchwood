package gorunner

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// Entry 是 AST 探测选定的用户入口形态（双轨优先级同 runner.js：Fetch 优先，
// Main 兜底）。
type Entry int

const (
	// EntryNone 表示根包未导出任何入口函数。
	EntryNone Entry = iota
	// EntryFetch 表示探测到导出函数 Fetch(w http.ResponseWriter, r *http.Request)
	// （HTTP 触发器封套还原为真 *http.Request）。
	EntryFetch
	// EntryMain 表示探测到导出函数 Main(data map[string]any, ctx map[string]string) (any, error)。
	EntryMain
)

// detectContext 是探测用的构建上下文（独立复核 A6 两处口径收窄）：
//   - 显式 GOOS=linux / GOARCH=amd64：dispatcher 宿主进程模式（检测不到自身
//     容器 ID，开发态真实拓扑）下 build.Default.GOOS 是宿主 OS 而非 linux，
//     平台特定文件里的 Fetch 会被误探测/误排除，随后以 undefined: user.Fetch
//     编译错收场——探测必须与镜像构建（linux/amd64）同口径；
//   - CgoEnabled=false：对齐构建模板 ENV CGO_ENABLED=0，cgo 门控文件在镜像
//     里本就不可编译，探测不入选与编译器同口径；
//   - ReleaseTags 沿 build.Default（go1.x 系列标签，//go:build go1.21 类
//     约束按工具链代次评估）。
var detectContext = func() build.Context {
	ctxt := build.Default
	ctxt.GOOS = "linux"
	ctxt.GOARCH = "amd64"
	ctxt.CgoEnabled = false
	return ctxt
}()

// DetectEntry 用 go/parser 扫描构建目录根包（单目录 = 单 Go 包，不递归
// 子目录）的导出函数，选定 twmain bootstrap 的调用入口。签名宽松匹配：
// 名字命中即选，签名不符交给编译器报错兜底（错误日志透传）。文件枚举按
// detectContext 的 MatchFile 评估（与编译器同口径处理 //go:build 约束与
// GOOS/GOARCH 平台后缀），并显式跳过 *_test.go——MatchFile 不排除测试文件，
// 而 export_test.go 向外暴露内部函数是 Go 社区常见写法，测试文件里的
// Fetch 会被 go build 忽略，探测若命中会生成 undefined: user.Fetch 的
// 编译错误（用户明明写了却看不懂为什么找不到）。两者皆无 → 明确错误
// （对齐 node「index.js must export main or fetch」）。
func DetectEntry(dir string) (Entry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return EntryNone, fmt.Errorf("scan function source dir: %w", err)
	}
	found := EntryNone
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// 显式跳过测试文件（go build 不编译 *_test.go，探测口径必须一致）。
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		// MatchFile 评估 //go:build 约束与平台后缀；错误（读取失败等）按
		// 不匹配处理——与编译器对不可读文件的容错同向，后续编译自会报错。
		if match, mErr := detectContext.MatchFile(dir, name); mErr != nil || !match {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			// 语法错误是用户代码包的明确错误（该文件本就过不了编译），
			// 提前报错并携带解析错误，不静默跳过。
			return EntryNone, fmt.Errorf("parse %s: %w", name, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !ast.IsExported(fn.Name.Name) {
				// 方法（带 receiver）不构成包级入口。
				continue
			}
			switch fn.Name.Name {
			case "Fetch":
				// Fetch 优先（优先级固定）：命中即返，不再扫后续文件。
				return EntryFetch, nil
			case "Main":
				if found == EntryNone {
					found = EntryMain
				}
			}
		}
	}
	if found == EntryNone {
		return EntryNone, errors.New("go function must export Fetch or Main")
	}
	return found, nil
}
