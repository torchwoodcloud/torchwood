package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/torchwooddev/torchwood/sdk/go/server"
)

// invoke 建立连接并以全局超时发起一次 InvokeJSON 调用。
// req 为 nil / string（原始 JSON，如 --data）/ map[string]any。
func invoke(g *globalFlags, method string, req any) ([]byte, error) {
	if g.tls {
		return nil, errors.New("--tls 尚未支持：服务端当前为明文 gRPC")
	}
	var opts []server.Option
	if g.apiKey != "" {
		opts = append(opts, server.WithAPIKey(g.apiKey))
	}
	c, err := server.New(g.endpoint, opts...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), g.timeoutDur)
	defer cancel()

	var reqJSON []byte
	switch v := req.(type) {
	case nil:
	case string:
		if v != "" {
			reqJSON = []byte(v)
		}
	case []byte:
		reqJSON = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("请求编码失败：%v", err)
		}
		reqJSON = b
	}
	resp, err := c.InvokeJSON(ctx, method, reqJSON)
	if err != nil {
		return nil, &rpcError{msg: formatRPCError(err), cause: err}
	}
	return resp, nil
}

// printJSON 把响应 JSON 字节原样渲染到 stdout（SDK 已按缩进格式编码）。
func printJSON(w io.Writer, b []byte) error {
	_, err := fmt.Fprintln(w, string(b))
	return err
}

// rpcError 携带原始 RPC 错误的 CLI 错误：ExitCode 据此映射进程退出码。
type rpcError struct {
	msg   string
	cause error
}

func (e *rpcError) Error() string { return e.msg }
func (e *rpcError) Unwrap() error { return e.cause }

// ExitCode 把根命令 Execute 返回的错误映射为脚本可分支的退出码：
// 成功=0；参数/校验错误等非 RPC 错误=1；40x=2；5xx=3；限流(429)=4。
// gRPC code → HTTP 类别的判定复用 SDK 的 HTTPErrorClass（CLI 不直接依赖 grpc）。
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	re, ok := err.(*rpcError)
	if !ok {
		return 1
	}
	return server.HTTPErrorClass(re.cause)
}

// formatRPCError 把 gRPC 调用错误转成 CLI 可读文本并附下一步动作提示：
// PermissionDenied 提示 scope（scope 格式见 internal/api/interceptor/apikey_scope.go），
// Unauthenticated 提示 API Key 自诊断。
func formatRPCError(err error) string {
	if server.IsPermissionDenied(err) {
		return fmt.Sprintf("rpc failed: %v\n提示：请检查 API Key 的 scope（如 users.read / users.write，或 * / all），或用 Console 重新生成 key", err)
	}
	if server.IsUnauthenticated(err) {
		return fmt.Sprintf("rpc failed: %v\n提示：凭证被拒——请确认 TORCHWOOD_CLI_API_KEY（或 --api-key）已设置且未过期/未删除，key 需属于目标 endpoint 对应实例；可用 `torchwood health` 验证连通性", err)
	}
	return fmt.Sprintf("rpc failed: %v", err)
}
