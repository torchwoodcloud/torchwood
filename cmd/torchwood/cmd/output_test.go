package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lynx-go/commands"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestValidateMissingAPIKey(t *testing.T) {
	g := &globalFlags{output: "json", timeout: "30s"}

	// 非豁免命令缺 key 报错
	if err := g.validate(true); err == nil || !strings.Contains(err.Error(), "missing API key") {
		t.Fatalf("非豁免命令缺 key 应报错，got %v", err)
	}

	// 公开命令（health/uuid/version）豁免
	if err := g.validate(false); err != nil {
		t.Fatalf("公开命令应豁免 api-key 校验：%v", err)
	}

	// 带 key 通过
	g.apiKey = "k"
	if err := g.validate(true); err != nil {
		t.Fatalf("带 key 应通过：%v", err)
	}
}

func TestValidateOutputAndTimeout(t *testing.T) {
	g := &globalFlags{output: "yaml", timeout: "30s", apiKey: "k"}
	if err := g.validate(true); err == nil || !strings.Contains(err.Error(), "unsupported output format") {
		t.Fatalf("非法 output 应报错，got %v", err)
	}
	g.output = "json"
	g.timeout = "abc"
	if err := g.validate(true); err == nil || !strings.Contains(err.Error(), "invalid --timeout") {
		t.Fatalf("非法 timeout 应报错，got %v", err)
	}
}

func TestFormatRPCError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "PermissionDenied 附加 scope 提示",
			err:  status.Error(codes.PermissionDenied, "api key missing required scope"),
			want: []string{"PermissionDenied", "api key missing required scope", "scope"},
		},
		{
			name: "Unauthenticated 附加 API Key 自诊断提示",
			err:  status.Error(codes.Unauthenticated, "invalid or expired credential"),
			want: []string{"Unauthenticated", "invalid or expired credential", "TORCHWOOD_CLI_API_KEY", "--api-key", "expired", "torchwood health"},
		},
		{
			name: "非 status 错误原样输出",
			err:  errors.New("dial failed"),
			want: []string{"dial failed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatRPCError(tt.err)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("want %q in %q", w, got)
				}
			}
		})
	}
}

// TestRPCExitCode 固化退出码映射（rpcExitCode 钩子，nil 不会到达钩子——
// App.Run 对 nil/ErrHelp 直接返回 0）：参数错=1 / 40x=2 / 5xx=3 / 限流=4。
func TestRPCExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "非 RPC 错误（参数校验等）为 1", err: errors.New("invalid --timeout"), want: 1},
		{name: "UsageError（位置参数不符）为 1", err: &commands.UsageError{Err: errors.New("expects 1 positional argument(s)")}, want: 1},
		{name: "Unauthenticated(401) 为 2", err: &rpcError{cause: status.Error(codes.Unauthenticated, "401")}, want: 2},
		{name: "PermissionDenied(403) 为 2", err: &rpcError{cause: status.Error(codes.PermissionDenied, "403")}, want: 2},
		{name: "NotFound(404) 为 2", err: &rpcError{cause: status.Error(codes.NotFound, "404")}, want: 2},
		{name: "InvalidArgument(400) 为 2", err: &rpcError{cause: status.Error(codes.InvalidArgument, "400")}, want: 2},
		{name: "AlreadyExists(409) 为 2", err: &rpcError{cause: status.Error(codes.AlreadyExists, "409")}, want: 2},
		{name: "Canceled(408/499) 为 2", err: &rpcError{cause: status.Error(codes.Canceled, "canceled")}, want: 2},
		{name: "Internal(500) 为 3", err: &rpcError{cause: status.Error(codes.Internal, "500")}, want: 3},
		{name: "Unavailable(503) 为 3", err: &rpcError{cause: status.Error(codes.Unavailable, "503")}, want: 3},
		{name: "DeadlineExceeded(504) 为 3", err: &rpcError{cause: status.Error(codes.DeadlineExceeded, "504")}, want: 3},
		{name: "Unknown(500) 为 3", err: &rpcError{cause: status.Error(codes.Unknown, "unknown")}, want: 3},
		{name: "ResourceExhausted(429) 为 4", err: &rpcError{cause: status.Error(codes.ResourceExhausted, "429")}, want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rpcExitCode(tt.err); got != tt.want {
				t.Errorf("rpcExitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// TestInvokeWrapsRPCError 验证 invoke 对 gRPC 错误保留 code（供退出码映射）。
// 这里走 --tls 短路路径之外的真实错误：非法 endpoint 的拨号失败按 Unknown
// 归入 5xx 类（3）。
func TestInvokeMapsGRPCErrors(t *testing.T) {
	g := &globalFlags{endpoint: "127.0.0.1:1", apiKey: "k", timeoutDur: 300 * time.Millisecond}
	_, err := invoke(g, "/torchwood.server.v1.HealthService/Check", nil)
	re, ok := err.(*rpcError)
	if !ok {
		t.Fatalf("invoke 应返回 *rpcError，got %T", err)
	}
	if rpcExitCode(re) != 3 {
		t.Errorf("连接失败应归入 5xx 类（退出码 3），got %d", rpcExitCode(re))
	}
}

// TestInvokeTLSDials 验证 --tls 不再短路报错，而是把 TLS 凭据接进拨号：
// 对本机关闭端口表现为连接失败（Unknown → 5xx 类退出码 3），而非
// 「未支持」参数错误。
func TestInvokeTLSDials(t *testing.T) {
	g := &globalFlags{endpoint: "127.0.0.1:1", tls: true, timeoutDur: 300 * time.Millisecond}
	_, err := invoke(g, "/torchwood.server.v1.HealthService/Check", nil)
	re, ok := err.(*rpcError)
	if !ok {
		t.Fatalf("invoke 应返回 *rpcError，got %T", err)
	}
	if rpcExitCode(re) != 3 {
		t.Errorf("--tls 连接失败应归入 5xx 类（退出码 3），got %d", rpcExitCode(re))
	}
}

func TestPrintJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := printJSON(&buf, []byte("{\"version\":\"v1.2.3\"}")); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if out != "{\"version\":\"v1.2.3\"}\n" {
		t.Errorf("printJSON 应原样写字节 + 换行：%q", out)
	}
}

// TestAppRunExitCodes 端到端固化 commands App.Run → rpcExitCode 的退出码
// 面板：帮助 0 / 未知动词 1（附帮助面）/ 缺 key 1 / 拨号失败（5xx 类）3，
// 以及裸分组命令打子命令帮助退 0。
func TestAppRunExitCodes(t *testing.T) {
	t.Setenv("TORCHWOOD_CLI_API_KEY", "")
	t.Setenv("TORCHWOOD_CLI_ENDPOINT", "127.0.0.1:1")
	t.Setenv("TORCHWOOD_CLI_TIMEOUT", "300ms")

	app := NewApp("test")
	var out, errOut bytes.Buffer
	env := &commands.Environment{Stdout: &out, Stderr: &errOut}
	run := func(args ...string) int {
		out.Reset()
		errOut.Reset()
		return app.Run(context.Background(), env, args)
	}

	// 空参数：帮助面（含版本 footer）+ 0
	if code := run(); code != commands.ExitOK {
		t.Fatalf("空参数退出码 = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "torchwood test") {
		t.Fatalf("帮助面应含版本 footer：%q", out.String())
	}

	// 未知动词：1（钩子归一），stderr 渲染错误并附帮助面
	if code := run("bogus"); code != commands.ExitError {
		t.Fatalf("未知动词退出码 = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "unknown verb") || !strings.Contains(errOut.String(), "commands:") {
		t.Fatalf("未知动词应渲染错误并附帮助面：%q", errOut.String())
	}

	// 本地命令：uuid 打印一行 UUID；version 打印版本串；位置参数被拒
	if code := run("uuid"); code != commands.ExitOK {
		t.Fatalf("uuid 退出码 = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "-") {
		t.Fatalf("uuid 应输出 UUID：%q", out.String())
	}
	if code := run("version"); code != commands.ExitOK || !strings.Contains(out.String(), "torchwood test") {
		t.Fatalf("version 应输出版本（退出码 0）：code=%d out=%q", code, out.String())
	}
	if code := run("uuid", "extra"); code != commands.ExitError {
		t.Fatalf("uuid 多余位置参数退出码 = %d, want 1", code)
	}

	// 裸分组命令：打子命令帮助面，退出 0
	if code := run("users"); code != commands.ExitOK {
		t.Fatalf("裸分组退出码 = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "create") || !strings.Contains(out.String(), "list") {
		t.Fatalf("裸分组应列子命令：%q", out.String())
	}

	// 缺 API key：非豁免 RPC 命令校验失败 → 1
	if code := run("users", "list"); code != commands.ExitError {
		t.Fatalf("缺 key 退出码 = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "missing API key") {
		t.Fatalf("缺 key 应渲染校验错误：%q", errOut.String())
	}

	// RPC 拨号失败（loopback 关闭端口）→ Unknown → 5xx 类 → 3
	if code := run("users", "list", "--api-key", "k"); code != 3 {
		t.Fatalf("拨号失败退出码 = %d, want 3", code)
	}

	// 旗标须在位置参数之前：get 的旗标放在位置参数后不再被解析（Go flag 语义）
	if code := run("users", "get", "u1", "--api-key", "k"); code != commands.ExitError {
		t.Fatalf("位置参数后的旗标退出码 = %d, want 1", code)
	}

	// 全局旗标在分组层给出（分组与叶子之间）不被叶子的缺省声明覆盖，
	// 直达拨号失败路径 → 3
	if code := run("users", "--api-key", "k", "--endpoint", "127.0.0.1:1", "list"); code != 3 {
		t.Fatalf("分组层全局旗标退出码 = %d, want 3", code)
	}
}
