package gorunner

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"go/format"
	"strings"
	"text/template"
	"unicode"
)

//go:embed assets/runtime.go.tmpl
var runtimeTmplSrc string

//go:embed assets/main.go.tmpl
var mainTmplSrc string

var mainGoTemplate = template.Must(template.New("twmain").Parse(mainTmplSrc))

// RenderBootstrap 渲染 twmain/ 目录的两份构建产物：
//
//   - runtimeGo：runner 协议实现（assets/runtime.go.tmpl，当前无占位符，
//     经 text/template 管线求值——未来占位符统一在此替换，调用方不变）；
//   - mainGo：生成的用户入口引用（import user "<module>" + 调
//     user.Fetch / user.Main + serve(runtime)），渲染后经 go/format 规范化
//     （消除模板分支缩进痕迹，产物即 gofmt 形态）。
//
// modulePath 是 zip 根 go.mod 的 module 行路径（探测层解析）；本处再做
// 注入面校验——module path 会被拼进生成源码的 import 语句，含引号/空白
// 的路径一律拒绝。
func RenderBootstrap(modulePath string, entryKind Entry) (runtimeGo, mainGo []byte, err error) {
	if err := validateModulePath(modulePath); err != nil {
		return nil, nil, err
	}
	switch entryKind {
	case EntryFetch, EntryMain:
	default:
		return nil, nil, errors.New("entry must be Fetch or Main (got none)")
	}

	var runtimeBuf bytes.Buffer
	runtimeTmpl, err := template.New("runtime").Parse(runtimeTmplSrc)
	if err != nil {
		return nil, nil, fmt.Errorf("parse runtime.go.tmpl: %w", err)
	}
	if err := runtimeTmpl.Execute(&runtimeBuf, nil); err != nil {
		return nil, nil, fmt.Errorf("render runtime.go: %w", err)
	}

	var mainBuf bytes.Buffer
	if err := mainGoTemplate.Execute(&mainBuf, struct {
		ModulePath string
		UseFetch   bool
	}{ModulePath: modulePath, UseFetch: entryKind == EntryFetch}); err != nil {
		return nil, nil, fmt.Errorf("render main.go: %w", err)
	}
	formatted, err := format.Source(mainBuf.Bytes())
	if err != nil {
		return nil, nil, fmt.Errorf("format generated main.go: %w", err)
	}
	return runtimeBuf.Bytes(), formatted, nil
}

// validateModulePath 校验 module path 可安全注入生成源码（非空、无引号、
// 无空白；探测层已剥引号/注释，此处兜底防注入）。
func validateModulePath(p string) error {
	if p == "" {
		return errors.New("go.mod module path is required")
	}
	if strings.ContainsAny(p, "\"'`") {
		return fmt.Errorf("invalid go.mod module path %q: quote characters are not allowed", p)
	}
	if strings.IndexFunc(p, unicode.IsSpace) >= 0 {
		return fmt.Errorf("invalid go.mod module path %q: whitespace is not allowed", p)
	}
	return nil
}
