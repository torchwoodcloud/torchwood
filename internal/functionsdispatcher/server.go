package functionsdispatcher

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dispatchServer 是 dispatcher 的内网 HTTP API 面：
//
//	POST /v1/dispatch/builds        zip（base64）构建镜像（v2 runner 模板）
//	POST /v1/dispatch/executions    执行分发（池管理热路径）
//	POST /v1/dispatch/images/remove 删除镜像（幂等）
//	GET  /healthz                   进程存活
//
// 认证：可选静态共享密钥 header（x-tw-dispatcher-token，constant time 比对；
// config 空 = 不校验，仅限可信内网）。全部端点接受调用方 ctx 超时。
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
	mux.HandleFunc("POST /v1/dispatch/images/remove", s.handleRemoveImage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	return mux
}

// ServeHTTP 挂认证中间件。
func (s *dispatchServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
// DeadlineExceeded → 504，InvalidArgument → 400，其余 → 500（内网 API，
// 调用方是 server/worker 的 DispatcherExecutor，按码还原 grpc status）。
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
	if req.FunctionID == "" || req.DeploymentID == "" || req.ZipBase64 == "" {
		writeError(w, status.Error(codes.InvalidArgument, "function_id/deployment_id/zip_base64 are required"))
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
	if err := s.daemon.BuildImage(r.Context(), req.FunctionID, req.DeploymentID, zip); err != nil {
		writeJSON(w, http.StatusOK, BuildResponse{Error: errorMessage(err)})
		return
	}
	// 部署更新：旧 deployment 池 drain（宽限上限 ≤ 函数超时，设计 §6）。
	if req.FunctionTimeoutSeconds > 0 {
		s.pool.DrainForDeployment(r.Context(), req.ProjectID, req.FunctionID, req.DeploymentID,
			time.Duration(req.FunctionTimeoutSeconds)*time.Second)
	}
	writeJSON(w, http.StatusOK, BuildResponse{})
}

func (s *dispatchServer) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req ExecuteRequest
	if err := decodeJSON(r, &req, maxBuildBodyBytes); err != nil {
		writeError(w, err)
		return
	}
	// DrainForDeployment 的 project 语义由执行请求路径携带；build 请求无
	// project 字段（镜像名全局唯一），drain 按函数号全池扫描兜底。
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
