package functions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/torchwooddev/torchwood/internal/domain/functions"
	config "github.com/torchwooddev/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// twExecutionTokenEnv 是执行身份环境变量名（与 app 层注入常量同值；infra
// 不反依赖 app 包，故本包自持一份——v2 客户端把该键从 env 摘出经分发
// header 传递）。
const twExecutionTokenEnv = "TW_EXECUTION_TOKEN"

// DispatcherExecutor 是 Executor 端口的 v2 适配实现：经 functions-dispatcher
// 的内网 HTTP API 承接 Build/Execute/RemoveImage（docker.sock 收敛到
// dispatcher 进程，server/worker 零 daemon 依赖，设计 §6 分发通路方案③）。
type DispatcherExecutor struct {
	cfg         *config.AppConfig
	baseURL     string
	sharedToken string
	hc          *http.Client
}

// NewDispatcherExecutor 构造 dispatcher HTTP 客户端（executor="dispatcher" 时
// 由 ProvideExecutor 装配；URL 未配置时延迟到首次调用报错，与 v1 同策略）。
func NewDispatcherExecutor(cfg *config.AppConfig) *DispatcherExecutor {
	d := cfg.GetFunctions().GetDispatcher()
	return &DispatcherExecutor{
		cfg:         cfg,
		baseURL:     d.GetUrl(),
		sharedToken: d.GetSharedToken(),
		hc: &http.Client{
			Transport: &http.Transport{
				// 内网同宿主回环/桥网络：短超时拨号 + 不走代理。
				Proxy:           nil,
				MaxIdleConns:    16,
				IdleConnTimeout: 90 * time.Second,
				DialContext:     (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				// 响应头超时不设：执行时长由请求 ctx 控制（函数超时上限 300s）。
			},
		},
	}
}

// do POST JSON 并解析响应体；非 2xx 时按 dispatcher 的状态码映射还原
// grpc status（ResourceExhausted → 429、DeadlineExceeded → 504 等）。
func (d *DispatcherExecutor) do(ctx context.Context, path string, in any, out any) error {
	if d.baseURL == "" {
		return status.Error(codes.FailedPrecondition, "functions.dispatcher.url is not configured (executor=dispatcher requires it)")
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal dispatcher request: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return status.Errorf(codes.Internal, "build dispatcher request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if d.sharedToken != "" {
		req.Header.Set("X-Tw-Dispatcher-Token", d.sharedToken)
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		// 调用方 ctx 超时原样透传（runExecution 依赖 DeadlineExceeded 判定）。
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBuildLogBytes))
	if err != nil {
		return status.Errorf(codes.Internal, "read dispatcher response: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &errBody)
		msg := errBody.Error
		if msg == "" {
			msg = truncateLog(string(body))
		}
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			return status.Error(codes.ResourceExhausted, msg)
		case http.StatusGatewayTimeout:
			return status.Error(codes.DeadlineExceeded, msg)
		case http.StatusBadRequest:
			return status.Error(codes.InvalidArgument, msg)
		case http.StatusUnauthorized, http.StatusForbidden:
			return status.Error(codes.PermissionDenied, "dispatcher authentication failed")
		default:
			return status.Errorf(codes.Internal, "dispatcher error (http %d): %s", resp.StatusCode, msg)
		}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return status.Errorf(codes.Internal, "decode dispatcher response: %v", err)
		}
	}
	return nil
}

// dispatchBuildResponse 是 builds 端点出参（Error 非空 = 构建失败）。
type dispatchBuildResponse struct {
	Error string `json:"error,omitempty"`
}

// dispatchExecuteResponse 是 executions 端点出参。
type dispatchExecuteResponse struct {
	Status     string `json:"status"`
	Response   string `json:"response,omitempty"`
	StdoutTail string `json:"stdout_tail,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error,omitempty"`
}

// Build 将 zip 代码包经 dispatcher 构建为镜像（v2 runner 模板在 dispatcher
// 侧应用；构建期不执行用户代码的不变量由模板层保持）。
func (d *DispatcherExecutor) Build(ctx context.Context, functionID, deploymentID, zipPath string) error {
	zip, err := os.ReadFile(zipPath)
	if err != nil {
		return status.Errorf(codes.Internal, "read function code package: %v", err)
	}
	var out dispatchBuildResponse
	err = d.do(ctx, "/v1/dispatch/builds", map[string]any{
		"function_id":   functionID,
		"deployment_id": deploymentID,
		"zip_base64":    base64.StdEncoding.EncodeToString(zip),
	}, &out)
	if err != nil {
		return err
	}
	if out.Error != "" {
		return fmt.Errorf("docker build failed: %s", out.Error)
	}
	return nil
}

// Execute 经 dispatcher 分发执行（池管理在 dispatcher 侧；本进程只做协议
// 适配）。执行身份注入通道切换：TW_EXECUTION_TOKEN 从 env 摘出经分发
// header 传递——P0 的 mint→注入→defer revoke 链路不变，只换注入通道。
func (d *DispatcherExecutor) Execute(ctx context.Context, exec functions.Execution) (*functions.ExecutionResult, error) {
	if exec.DeploymentID == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment id is required")
	}
	env := make(map[string]string, len(exec.Env))
	token := ""
	for k, v := range exec.Env {
		if k == twExecutionTokenEnv {
			token = v
			continue
		}
		env[k] = v
	}
	var out dispatchExecuteResponse
	err := d.do(ctx, "/v1/dispatch/executions", map[string]any{
		"image":            ImageName(d.cfg, exec.FunctionID, exec.DeploymentID),
		"project_id":       exec.ProjectID,
		"function_id":      exec.FunctionID,
		"deployment_id":    exec.DeploymentID,
		"runtime":          exec.Runtime,
		"spec":             exec.Spec,
		"timeout_seconds":  exec.Timeout,
		"env":              env,
		"execution_token":  token,
		"data":             exec.Data,
		"egress_untrusted": exec.EgressUntrusted,
		"pool": map[string]any{
			"min_instances":             exec.MinInstances,
			"max_instances":             exec.MaxInstances,
			"idle_ttl_seconds":          exec.IdleTTLSeconds,
			"max_requests_per_instance": exec.MaxRequestsPerInstance,
		},
	}, &out)
	if err != nil {
		return nil, err
	}
	failed := out.Status != "ok"
	result := &functions.ExecutionResult{
		// v2 runner 正常应答无容器退出码语义：ok=true → 0，函数报错 → 1
		// （与 v1「非零退出码 = failed」的记录语义对齐）。
		StatusCode: exitCodeOf(failed),
		Stdout:     out.StdoutTail,
		Stderr:     out.StderrTail,
		Response:   out.Response,
		DurationMS: out.DurationMS,
	}
	if failed {
		msg := out.Error
		if msg == "" && out.StderrTail != "" {
			msg = out.StderrTail
		}
		if msg == "" {
			msg = "function failed"
		}
		// Unknown code：runExecution/ProcessExecution 只取消息落 error 列。
		return result, status.Errorf(codes.Unknown, "%s", truncateLog(msg))
	}
	return result, nil
}

// RemoveImage 经 dispatcher 删除镜像（幂等）。
func (d *DispatcherExecutor) RemoveImage(ctx context.Context, functionID, deploymentID string) error {
	return d.do(ctx, "/v1/dispatch/images/remove", map[string]any{
		"function_id":   functionID,
		"deployment_id": deploymentID,
	}, nil)
}

func exitCodeOf(failed bool) int {
	if failed {
		return 1
	}
	return 0
}
