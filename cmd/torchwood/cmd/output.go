package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/lynx-go/commands"

	"github.com/torchwoodcloud/torchwood/sdk/go/server"
)

// invoke 建立连接并以全局超时发起一次 InvokeJSON 调用。
// req 为 nil / string（原始 JSON，如 --data）/ map[string]any。
func invoke(g *globalFlags, method string, req any) ([]byte, error) {
	var opts []server.Option
	if g.apiKey != "" {
		opts = append(opts, server.WithAPIKey(g.apiKey))
	}
	if g.tls {
		opts = append(opts, server.WithTLS())
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
			return nil, fmt.Errorf("failed to encode request: %v", err)
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

// call 是 RPC 叶子动词的统一执行尾：invoke 后把响应渲染到 env.Stdout。
func call(g *globalFlags, env *commands.Environment, method string, req any) error {
	resp, err := invoke(g, method, req)
	if err != nil {
		return err
	}
	return printJSON(env.Stdout, resp)
}

// rpcError 携带原始 RPC 错误的 CLI 错误：rpcExitCode 据此映射进程退出码。
type rpcError struct {
	msg   string
	cause error
}

func (e *rpcError) Error() string { return e.msg }
func (e *rpcError) Unwrap() error { return e.cause }

// formatRPCError 把 gRPC 调用错误转成 CLI 可读文本并附下一步动作提示：
// PermissionDenied 提示 scope（scope 格式见 internal/api/interceptor/apikey_scope.go），
// Unauthenticated 提示 API Key 自诊断。
func formatRPCError(err error) string {
	if server.IsPermissionDenied(err) {
		return fmt.Sprintf("rpc failed: %v\nhint: check the API key's scopes (e.g. users.read / users.write, or * / all), or regenerate the key in the Console", err)
	}
	if server.IsUnauthenticated(err) {
		return fmt.Sprintf("rpc failed: %v\nhint: credential rejected — make sure the API key is set (--api-key, TORCHWOOD_CLI_API_KEY, or the api-key field of the config profile) and not expired/deleted; the key must belong to the instance the endpoint points to; use `torchwood health` to verify connectivity", err)
	}
	return fmt.Sprintf("rpc failed: %v", err)
}
