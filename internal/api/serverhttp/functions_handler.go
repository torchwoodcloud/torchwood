package serverhttp

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/torchwooddev/torchwood/internal/api/interceptor"
	appfunctions "github.com/torchwooddev/torchwood/internal/app/functions"
	"github.com/torchwooddev/torchwood/internal/domain/audit"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FunctionsServiceCreateDeployment 是 multipart 上传对应的 scope 检查方法
// （与 gRPC CreateDeployment 一致）。
const FunctionsServiceCreateDeployment = "/torchwood.server.v1.FunctionsService/CreateDeployment"

// maxCodePackageBytes 是 zip 代码包上传上限（§5.6，50 MiB）。
const maxCodePackageBytes = 50 << 20

// FunctionsHandler 提供 deployment 代码包 multipart 上传。
type FunctionsHandler struct {
	policies  *domainauth.PolicySet
	cfg       *config.AppConfig
	auth      *httpAuth
	functions *appfunctions.Functions
	auditRepo audit.Repository
	trusted   *interceptor.TrustedProxies
	logger    *slog.Logger
}

// NewFunctionsHandler creates a new functions HTTP handler.
func NewFunctionsHandler(
	cfg *config.AppConfig,
	validator AuthValidator,
	functions *appfunctions.Functions,
	auditRepo audit.Repository,
	logger *slog.Logger,
	policies *domainauth.PolicySet,
) (*FunctionsHandler, error) {
	if logger == nil {
		logger = slog.Default()
	}
	trusted, err := interceptor.ParseTrustedProxies(cfg.GetSecurity().GetTrustedProxies())
	if err != nil {
		return nil, fmt.Errorf("parse security.trusted_proxies: %w", err)
	}
	return &FunctionsHandler{cfg: cfg, auth: newHTTPAuth(validator, policies), functions: functions, auditRepo: auditRepo,
		trusted: trusted, logger: logger, policies: policies}, nil
}

// Register attaches the deployment upload route to the gateway mux.
func (h *FunctionsHandler) Register(mux *runtime.ServeMux) {
	_ = mux.HandlePath("POST", "/v1/server/functions/{functionId}/deployments/code", h.upload)
}

// logOp 输出 deployment 上传的结构化访问日志（成功/失败各一条）。
func (h *FunctionsHandler) logOp(r *http.Request, op, functionID, deploymentID string, principal *shared.Principal, err error) {
	attrs := []any{
		slog.String("op", op),
		slog.String("function_id", functionID),
		slog.String("deployment_id", deploymentID),
		slog.String("ip", h.clientIP(r)),
	}
	if principal != nil {
		attrs = append(attrs,
			slog.String("actor_id", string(principal.ActorID)),
			slog.String("actor_kind", string(principal.ActorKind)),
			slog.String("credential_type", string(principal.CredentialType)),
			slog.String("project_id", principal.ProjectID),
		)
	}
	// slog 之外落持久审计（P2-6；特权写操作——代码包上传——必须有审计轨迹）。
	meta := map[string]any{"op": op, "function_id": functionID}
	if principal != nil {
		meta["project_id"] = principal.ProjectID
	}
	if deploymentID != "" {
		meta["deployment_id"] = deploymentID
	}
	auditFromHTTP(r, h.clientIP(r), h.auditRepo, h.logger, "http.functions."+op, meta, deploymentID, principal, err)
	if err != nil {
		st, _ := status.FromError(err)
		attrs = append(attrs, slog.String("error", st.Code().String()))
		h.logger.Warn("deployment operation failed", attrs...)
		return
	}
	h.logger.Info("deployment operation", attrs...)
}

func (h *FunctionsHandler) clientIP(r *http.Request) string {
	return h.trusted.ResolveClientIP(
		interceptor.PeerIPFromAddr(r.RemoteAddr),
		r.Header.Get("X-Forwarded-For"),
		r.Header.Get("X-Real-Ip"),
	)
}

// upload POST /v1/server/functions/{functionId}/deployments/code
// multipart 字段 code（zip 文件，≤50MiB）。
func (h *FunctionsHandler) upload(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	functionID := pathParams["functionId"]
	principal, err := h.authorize(r)
	if err != nil {
		h.logOp(r, "deployment-upload", functionID, "", nil, err)
		httpError(w, err)
		return
	}
	projectID := h.auth.projectID(r, principal)
	if projectID == "" {
		err := status.Error(codes.Unauthenticated, "missing project context")
		h.logOp(r, "deployment-upload", functionID, "", principal, err)
		httpError(w, err)
		return
	}
	if functionID == "" {
		httpError(w, status.Error(codes.InvalidArgument, "missing function id"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxCodePackageBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httpError(w, status.Error(codes.InvalidArgument, "invalid multipart form or file too large"))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	fileHeaders := r.MultipartForm.File["code"]
	if len(fileHeaders) == 0 {
		httpError(w, status.Error(codes.InvalidArgument, "missing code file"))
		return
	}
	fh := fileHeaders[0]
	f, err := fh.Open()
	if err != nil {
		httpError(w, status.Error(codes.Internal, "cannot open uploaded code package"))
		return
	}
	defer func() { _ = f.Close() }()

	code, err := io.ReadAll(io.LimitReader(f, maxCodePackageBytes+1))
	if err != nil {
		httpError(w, status.Error(codes.Internal, "cannot read uploaded code package"))
		return
	}
	if len(code) > maxCodePackageBytes {
		httpError(w, status.Error(codes.InvalidArgument, "code package exceeds 50 MiB limit"))
		return
	}
	if !isZipMagic(code) {
		httpError(w, status.Error(codes.InvalidArgument, "invalid zip file: missing PK zip signature"))
		return
	}

	// Round3 H2-1：把已鉴权 principal 注入 ctx 再调 use-case——HTTP 路径不经
	// gRPC 拦截器，RequireServerPrincipal 依赖 ctx 中的 Principal，缺了会对
	// 合法 admin / functions.write API Key 恒 401。
	ctx := contexts.WithPrincipal(r.Context(), principal)
	dep, err := h.functions.CreateDeployment(ctx, appfunctions.CreateDeploymentCommand{
		ProjectID:  projectID,
		FunctionID: functionID,
		Code:       code,
	})
	if err != nil {
		h.logOp(r, "deployment-upload", functionID, "", principal, err)
		httpError(w, err)
		return
	}
	h.logOp(r, "deployment-upload", functionID, dep.ID, principal, nil)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          dep.ID,
		"function_id": dep.FunctionID,
		"size":        dep.Size,
		"status":      dep.Status,
		"error":       dep.Error,
		"created_at":  dep.CreatedAt,
		"updated_at":  dep.UpdatedAt,
	})
}

// authorize 与 gRPC 拦截器对齐：API key 走 FunctionsService/CreateDeployment
// scope（functions.write）；admin 会话要求 owner/admin 角色（对齐
// internal/api/interceptor/admin_roles.go，viewer/member 只读）；端用户
// （Bearer JWT / 会话 cookie）一律禁止上传部署代码包：任意注册用户可借此
// 触发 Docker 构建并部署恶意代码窃取函数环境变量（安全评审 03）。
// 认证/项目解析等公共逻辑见 httpAuth（auth.go）。
func (h *FunctionsHandler) authorize(r *http.Request) (*shared.Principal, error) {
	principal, err := h.auth.authorize(r, func(*http.Request) string {
		return FunctionsServiceCreateDeployment
	}, domainauth.ScopeTargets{})
	if err != nil {
		return nil, err
	}
	if principal.ActorKind == shared.ActorKindEndUser {
		return nil, status.Error(codes.PermissionDenied, "end-user credentials cannot upload deployments")
	}
	if principal.ActorKind == shared.ActorKindAdmin &&
		!principal.HasAnyRole(domainauth.RoleStrings(h.policies.AllowedAdminRoles(FunctionsServiceCreateDeployment))) {
		return nil, status.Error(codes.PermissionDenied, "admin role not permitted for deployment upload")
	}
	return principal, nil
}

// isZipMagic 校验 zip 魔数 PK\x03\x04。
func isZipMagic(code []byte) bool {
	return len(code) >= 4 && bytes.Equal(code[:4], []byte{0x50, 0x4B, 0x03, 0x04})
}
