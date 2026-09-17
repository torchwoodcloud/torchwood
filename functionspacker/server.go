package functionspacker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxPackBodyBytes 是打包请求的读取上限：入参只有 url/ref/directory 与
// 一次性凭证（≤数 KB），取 1MiB 防御性封顶（响应侧大载荷不受此限）。
const maxPackBodyBytes = 1 << 20

// packServer 是 packer 的内网 HTTP API 面：
//
//	POST /v1/pack/git  git 仓库 @ref[:directory] 物化为 zip（base64 内联）
//	GET  /healthz      进程存活
//
// 认证：可选静态共享密钥 header（x-tw-packer-token，constant time 比对；
// config 空 = 不校验，仅限可信内网）；GET /healthz 豁免（liveness 静态
// 探针由编排发起，命令行不应携带密钥——docker inspect 可见）。错误映射
// 与 dispatcher 同款（ResourceExhausted → 429 / DeadlineExceeded → 504 /
// InvalidArgument → 400 / NotFound → 404 / 其余 → 500）。
type packServer struct {
	token string
	opts  PackOptions
	// sem 是进程内并发自限信号量（容量 = config concurrency）：饱和立即
	// 429——与 dispatcher 构建信号量成两道独立闸、无嵌套（设计 §2；
	// 原 A8 信号量两相化问题整体消失：pack 在构建信号量之外）。
	sem chan struct{}
	// pack 可注入替换（HTTP 面测试桩）；生产恒 PackGit。
	pack func(ctx context.Context, req PackRequest, opts PackOptions) (*PackResponse, error)
}

func newPackServer(sharedToken string, opts PackOptions, concurrency int) *packServer {
	opts = normalizePackOptions(opts)
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	return &packServer{
		token: sharedToken,
		opts:  opts,
		sem:   make(chan struct{}, concurrency),
		pack:  PackGit,
	}
}

func (s *packServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pack/git", s.handlePackGit)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	return mux
}

// ServeHTTP 挂认证中间件（照抄 functionsdispatcher 模式）：仅 GET /healthz
// 豁免 token 校验（固定 200、零信息泄露的 liveness 面），其余一律校验。
func (s *packServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		s.routes().ServeHTTP(w, r)
		return
	}
	if s.token != "" {
		got := r.Header.Get("X-Tw-Packer-Token")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid packer token"})
			return
		}
	}
	s.routes().ServeHTTP(w, r)
}

func (s *packServer) handlePackGit(w http.ResponseWriter, r *http.Request) {
	var req PackRequest
	if err := decodeJSON(r, &req, maxPackBodyBytes); err != nil {
		writeError(w, err)
		return
	}
	if req.URL == "" {
		writeError(w, status.Error(codes.InvalidArgument, "url is required"))
		return
	}
	// 并发自限：非阻塞获取，饱和立即 429（排队会把重资源压力后置恶化，
	// 明确拒绝让 server 侧按 ResourceExhausted 语义处置）。
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		writeError(w, status.Error(codes.ResourceExhausted, "packer at concurrency limit"))
		return
	}
	// fetch_timeout 对单次打包整体封顶（clone+核算+物化共享同一预算）；
	// 挂在请求 ctx 之下——客户端断开同样中止（pack 结果落库前无状态，
	// 断开即弃，无孤儿副作用）。
	packCtx, cancel := context.WithTimeout(r.Context(), s.opts.FetchTimeout)
	defer cancel()
	resp, err := s.pack(packCtx, req, s.opts)
	if err != nil {
		// fetch_timeout 封顶触发的超时归一为 DeadlineExceeded（504）：
		// 底层 clone/materialize 的超时错误形态随实现不同（包装/裸 ctx
		// 错误），在服务层按封顶 ctx 统一判定。
		if errors.Is(packCtx.Err(), context.DeadlineExceeded) {
			err = status.Errorf(codes.DeadlineExceeded, "git pack exceeded functions.packer.fetch_timeout")
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, httpStatus int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 把 packer 侧错误映射为 HTTP 状态码（与 dispatcher 同款）：
// ResourceExhausted → 429，DeadlineExceeded → 504，InvalidArgument → 400，
// NotFound → 404，其余 → 500（内网 API，调用方是 server 侧 SourcePacker
// 适配器，按码还原 grpc status）。
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
