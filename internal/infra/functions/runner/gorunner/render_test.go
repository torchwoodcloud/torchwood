package gorunner

import (
	"bytes"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestRenderBootstrap_Main 入口渲染：import user "<module>" + 调 user.Main +
// serve(runtime)；产物即 gofmt 形态（go/format 幂等）。
func TestRenderBootstrap_Main(t *testing.T) {
	runtimeGo, mainGo, err := RenderBootstrap("example.com/fn", EntryMain)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.Contains(runtimeGo, []byte("func serve(")) {
		t.Errorf("runtime.go must define serve():\n%s", runtimeGo)
	}
	for _, want := range []string{
		`import user "example.com/fn"`,
		"serve(entry{main: user.Main})",
	} {
		if !bytes.Contains(mainGo, []byte(want)) {
			t.Errorf("main.go missing %q:\n%s", want, mainGo)
		}
	}
	// 负向断言检查完整调用表达式（模板头注释提及双入口符号属正常）。
	if bytes.Contains(mainGo, []byte("serve(entry{fetch: user.Fetch})")) {
		t.Errorf("main entry must not call user.Fetch:\n%s", mainGo)
	}
}

// TestRenderBootstrap_Fetch fetch 入口渲染：调 user.Fetch（优先级在探测期
// 定死，渲染产物只引用被选定的入口——引用不存在的符号 = 编译错）。
func TestRenderBootstrap_Fetch(t *testing.T) {
	_, mainGo, err := RenderBootstrap("example.com/fn", EntryFetch)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.Contains(mainGo, []byte("serve(entry{fetch: user.Fetch})")) {
		t.Errorf("main.go must call user.Fetch:\n%s", mainGo)
	}
	if bytes.Contains(mainGo, []byte("serve(entry{main: user.Main})")) {
		t.Errorf("fetch entry must not call user.Main:\n%s", mainGo)
	}
}

// TestRenderBootstrap_Rejects 注入面校验：空 module path / EntryNone /
// 含引号或空白的 module path 一律拒绝（module path 会拼进生成源码的
// import 语句）。
func TestRenderBootstrap_Rejects(t *testing.T) {
	cases := []struct {
		name       string
		modulePath string
		entry      Entry
		wantErr    string
	}{
		{"empty module path", "", EntryMain, "module path is required"},
		{"entry none", "example.com/fn", EntryNone, "entry must be Fetch or Main"},
		{"quoted module path", `"example.com/fn"`, EntryMain, "quote"},
		{"whitespace module path", "example.com/ fn", EntryMain, "whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := RenderBootstrap(tc.modulePath, tc.entry)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RenderBootstrap err = %v, want contains %q", err, tc.wantErr)
			}
		})
	}
}

// TestRenderBootstrap_RuntimeStdlibOnly 硬约束防回归（对抗审查 A3）：
// runtime.go 渲染产物的 import 白名单 = 标准库——vendor 模式下 twmain 的
// 依赖同样经 vendor 解析，任何第三方依赖都会让 vendor 用户构建失败。
// 判据：import 路径首段含点号 = 域名形态 = 第三方（标准库首段无点号）。
func TestRenderBootstrap_RuntimeStdlibOnly(t *testing.T) {
	runtimeGo, _, err := RenderBootstrap("example.com/fn", EntryMain)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "runtime.go", runtimeGo, 0)
	if err != nil {
		t.Fatalf("parse rendered runtime.go: %v", err)
	}
	for _, imp := range parsed.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		first := path
		if i := strings.IndexByte(first, '/'); i >= 0 {
			first = first[:i]
		}
		if strings.Contains(first, ".") {
			t.Errorf("runtime.go import %q is not stdlib (vendor 用户构建必炸)——twmain import 白名单 = 标准库", path)
		}
	}
}

// TestRenderBootstrap_RuntimeProtocolMarkers runtime.go 协议面抽查：
// health 路径、分发 header 族、生命周期 env 键、fetch 封套要点与头过滤
// 清单必须在产物中（模板改动导致协议面丢失时在此显式失败）。
func TestRenderBootstrap_RuntimeProtocolMarkers(t *testing.T) {
	runtimeGo, _, err := RenderBootstrap("example.com/fn", EntryFetch)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(runtimeGo)
	for _, want := range []string{
		"/_tw/health",
		"x-tw-trigger-envelope",
		"x-tw-timeout-seconds",
		"x-tw-execution-token",
		"x-tw-execution-id",
		"x-tw-source",
		"x-tw-invoking-user-id",
		"x-tw-project-id",
		"TW_RUNNER_PORT",
		"TW_MAX_REQUESTS",
		"TW_DRAIN_TIMEOUT_MS",
		"TW_API_BASE_URL",
		"http://function/",
		"http://trigger",
		"instance draining",
		"timed out after",
		"content-length",
		"body_base64",
		"truncated",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("runtime.go missing protocol marker %q", want)
		}
	}
	// 未使用 import 会直接编译失败：包名引用抽查兜底（httptest 形态捕获）。
	if !strings.Contains(s, "httptest.NewRecorder()") {
		t.Errorf("fetch 风格响应必须经 ResponseRecorder 同构捕获:\n%s", s)
	}
}
