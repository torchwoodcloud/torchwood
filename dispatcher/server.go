package dispatcher

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dispatchServer 是 dispatcher 的内网 HTTP API 面：
//
//	POST /v1/dispatch/builds        zip（base64）构建镜像（v2 runner 模板）
//	POST /v1/dispatch/executions    执行分发（池管理热路径）
//	POST /v1/dispatch/images/import 镜像源导入（pull/digest 钉死/retag/契约验证）
//	POST /v1/dispatch/images/remove 删除镜像（幂等）
//	GET  /healthz                   进程存活
//	GET  /metrics                   Prometheus 指标（只读观测面）
//
// 认证：可选静态共享密钥 header（x-tw-dispatcher-token，constant time 比对；
// config 空 = 不校验，仅限可信内网）；GET /healthz 与 GET /metrics 豁免
// （liveness 静态探针与指标抓取器不带凭据）。其余端点接受调用方 ctx 超时。
type dispatchServer struct {
	pool   *PoolManager
	daemon Daemon
	token  string
}

func newDispatchServer(pool *PoolManager, daemon Daemon, sharedToken string) *dispatchServer {
	return &dispatchServer{pool: pool, daemon: daemon, token: sharedToken}
}

func (s *dispatchServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/dispatch/builds", s.handleBuild)
	mux.HandleFunc("POST /v1/dispatch/executions", s.handleExecute)
	mux.HandleFunc("POST /v1/dispatch/images/import", s.handleImportImage)
	mux.HandleFunc("POST /v1/dispatch/images/remove", s.handleRemoveImage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.Handle("GET /metrics", promhttp.Handler())
	return mux
}

// ServeHTTP 挂认证中间件。
func (s *dispatchServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /healthz 与 /metrics 豁免 token 校验（内网绑定不变）：前者是 liveness
	// 静态探针（固定 200，零信息泄露），后者是 Prometheus 只读抓取面；编排
	// 健康检查与抓取器的命令行不应携带密钥（docker inspect 可见）。豁免面
	// 仅这两条 GET 路径——其余方法/路径（含 dispatch 端点）一律走校验。
	if r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/metrics") {
		s.routes().ServeHTTP(w, r)
		return
	}
	if s.token != "" {
		got := r.Header.Get("X-Tw-Dispatcher-Token")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid dispatcher token"})
			return
		}
	}
	s.routes().ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 把 dispatcher 侧错误映射为 HTTP 状态码：ResourceExhausted → 429，
// DeadlineExceeded → 504，InvalidArgument → 400，FailedPrecondition（镜像
// 缺失类型化上抛，rebuild 链路）→ 412，其余 → 500（内网 API，调用方是
// server/worker 的 DispatcherExecutor，按码还原 grpc status）。
func writeError(w http.ResponseWriter, err error) {
	code := status.Code(err)
	httpStatus := http.StatusInternalServerError
	switch code {
	case codes.ResourceExhausted:
		httpStatus = http.StatusTooManyRequests
	case codes.DeadlineExceeded:
		httpStatus = http.StatusGatewayTimeout
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
	case codes.NotFound:
		httpStatus = http.StatusNotFound
	case codes.FailedPrecondition:
		httpStatus = http.StatusPreconditionFailed
	}
	writeJSON(w, httpStatus, map[string]string{"error": errorMessage(err)})
}

func errorMessage(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}

func decodeJSON(r *http.Request, v any, maxBytes int64) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBytes))
	if err := dec.Decode(v); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid request body: %v", err)
	}
	return nil
}

func (s *dispatchServer) handleBuild(w http.ResponseWriter, r *http.Request) {
	var req BuildRequest
	if err := decodeJSON(r, &req, maxBuildBodyBytes*2); err != nil {
		writeError(w, err)
		return
	}
	// project_id 必填（D14 顺手修复的另一半）：drain 走项目语义（旧池扫描
	// 按 (project, function) 收窄），镜像名全局唯一不等于池键可省 project。
	if req.ProjectID == "" || req.FunctionID == "" || req.DeploymentID == "" || req.ZipBase64 == "" {
		writeError(w, status.Error(codes.InvalidArgument, "project_id/function_id/deployment_id/zip_base64 are required"))
		return
	}
	zip, err := base64.StdEncoding.DecodeString(req.ZipBase64)
	if err != nil {
		writeError(w, status.Errorf(codes.InvalidArgument, "invalid zip_base64: %v", err))
		return
	}
	if len(zip) > maxBuildBodyBytes {
		writeError(w, status.Errorf(codes.InvalidArgument, "zip exceeds %d bytes", maxBuildBodyBytes))
		return
	}
	if err := s.daemon.BuildImage(r.Context(), BuildImageOptions{
		ProjectID:              req.ProjectID,
		FunctionID:             req.FunctionID,
		DeploymentID:           req.DeploymentID,
		Zip:                    zip,
		Runtime:                req.Runtime,
		FunctionTimeoutSeconds: req.FunctionTimeoutSeconds,
		Env:                    req.Env,
		EgressUntrusted:        req.EgressUntrusted,
		Verify:                 req.Verify,
	}); err != nil {
		writeJSON(w, http.StatusOK, BuildResponse{Error: errorMessage(err)})
		return
	}
	// 部署更新：旧 deployment 池 drain（宽限上限 ≤ 函数超时，设计 §6）。
	// D14 顺手修复：FunctionTimeoutSeconds 曾因 server 侧恒不携带而恒 0、
	// drain 从不触发；Build 载荷补齐后按函数超时真正生效。
	if req.FunctionTimeoutSeconds > 0 {
		s.pool.DrainForDeployment(r.Context(), req.ProjectID, req.FunctionID, req.DeploymentID,
			time.Duration(req.FunctionTimeoutSeconds)*time.Second)
	}
	// 构建亲和（四期 4a-1 M5）：响应携带本节点 ID，server 侧落
	// function_deployments.build_node（local 路由模式 4a-2 固定路由该节点）。
	writeJSON(w, http.StatusOK, BuildResponse{NodeID: s.pool.NodeID()})
}

func (s *dispatchServer) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req ExecuteRequest
	if err := decodeJSON(r, &req, maxBuildBodyBytes); err != nil {
		writeError(w, err)
		return
	}
	// 节点转发防环（四期 4a-2 M3）：带 X-Tw-Forwarded-For-Node 的请求来自
	// 他节点转发——强制本地池路径（DispatchForwarded），不得再转发。转发
	// 发起方已按实例亲和/BuildNode 语义选定本节点为镜像所在节点。
	if from := r.Header.Get(forwardedForNodeHeader); from != "" {
		resp, err := s.pool.DispatchForwarded(r.Context(), req, from)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// DrainForDeployment 的 project 语义由执行/构建请求一并携带（BuildRequest
	// 一期定稿含 project_id，D14）——drain 按 (project, function) 精确收窄。
	resp, err := s.pool.Dispatch(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *dispatchServer) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	var req RemoveImageRequest
	if err := decodeJSON(r, &req, 1<<20); err != nil {
		writeError(w, err)
		return
	}
	if req.FunctionID == "" || req.DeploymentID == "" {
		writeError(w, status.Error(codes.InvalidArgument, "function_id/deployment_id are required"))
		return
	}
	if err := s.daemon.RemoveImage(r.Context(), req.FunctionID, req.DeploymentID); err != nil {
		// 幂等删除失败不阻塞删除链路（调用方 best-effort 语义），原样透传
		// 500 供观测。
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleImportImage 承接镜像源导入（三期阶段三，设计 §3）：必填形状校验 →
// daemon.ImportImage（host 校验/pull/digest 钉死/retag/删原始引用/强制契约
// 验证）→ 成功回 Digest；导入失败与 builds 同风格走 200 + Error（部署业务
// 结果，由调用方落 deployment.error），仅请求形状错误走状态码映射
// （writeError）。导入成功且 function_timeout_seconds > 0 时旧池 drain
// （与 handleBuild 同语义：镜像部署换版同样回收旧 deployment 实例）。
func (s *dispatchServer) handleImportImage(w http.ResponseWriter, r *http.Request) {
	var req ImportImageRequest
	if err := decodeJSON(r, &req, 1<<20); err != nil {
		writeError(w, err)
		return
	}
	if req.ProjectID == "" || req.FunctionID == "" || req.DeploymentID == "" || req.Reference == "" {
		writeError(w, status.Error(codes.InvalidArgument, "project_id/function_id/deployment_id/reference are required"))
		return
	}
	digest, err := s.daemon.ImportImage(r.Context(), ImportImageOptions{
		ProjectID:              req.ProjectID,
		FunctionID:             req.FunctionID,
		DeploymentID:           req.DeploymentID,
		Reference:              req.Reference,
		RegistryUsername:       req.RegistryUsername,
		RegistryToken:          req.RegistryToken,
		ExpectedDigest:         req.ExpectedDigest,
		FunctionTimeoutSeconds: req.FunctionTimeoutSeconds,
		Env:                    req.Env,
		EgressUntrusted:        req.EgressUntrusted,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, ImportImageResponse{Error: errorMessage(err)})
		return
	}
	if req.FunctionTimeoutSeconds > 0 {
		s.pool.DrainForDeployment(r.Context(), req.ProjectID, req.FunctionID, req.DeploymentID,
			time.Duration(req.FunctionTimeoutSeconds)*time.Second)
	}
	writeJSON(w, http.StatusOK, ImportImageResponse{Digest: digest})
}
