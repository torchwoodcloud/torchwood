package runtime

import (
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lynx-go/grpcapi/gateway"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// HTTPErrorHandler converts gRPC errors to a consistent JSON error body.
// P3-3：Internal/Unknown 对外统一文案，原文只进日志（fail-closed，不泄内部细节）。
//
// grpcapi 阶段 1 换库：机制本体（非状态错误归 Internal、Internal/Unknown
// 脱敏 + error_id 关联、Retry-After 提取（整秒向上取整、至少 1s）、
// Canceled→499 映射、经 mux marshaler 写 JSON）内置于 gateway.NewErrorHandler；
// 项目错误契约（sharedv1.ErrorResponse 形状 + code→error_type/error_code
// 映射表）经 errorBodyBuilder 注入。
//
// 与原本地实现的可见差异：error_id 由 uuid 改为库生成的 128 位随机十六进
// 制（仅日志关联用途，格式不对客户端承诺）；脱敏日志键 "method" 更名
// "path"（取值同为 r.URL.Path）；错误体序列化从 encoding/json 改为 mux 注入
// 的 protojson marshaler（字段名同为 proto snake_case，行为由
// errors_retry_after/observability 测试锁定）。
var HTTPErrorHandler runtime.ErrorHandlerFunc = gateway.NewErrorHandler(errorBodyBuilder{}, gateway.HTTPOptions{})

// errorBodyBuilder 实现 gateway.ErrorBodyBuilder，承载 torchwood 对外错误
// 契约：错误体形状（sharedv1.ErrorResponse）与 gRPC code → error_type /
// error_code 的映射表（自原 HTTPErrorHandler 原样搬运，映射语义不变）。
type errorBodyBuilder struct{}

// Build 构造错误体：message 为脱敏判定后的对外文案，errorID 为库生成的
// 错误追踪 ID。
func (errorBodyBuilder) Build(code codes.Code, message, errorID string) proto.Message {
	return &sharedv1.ErrorResponse{
		Error: &sharedv1.Error{
			Type:      errorTypeForCode(code),
			Code:      code.String(),
			Message:   message,
			ErrorId:   errorID,
			ErrorCode: errorCodeFor(code),
		},
	}
}

// MapErrorCode 声明 gRPC code → torchwood error_code 映射（无映射返回 nil，
// 库约定）；Build 用同一张表填充错误体的业务错误码字段。
func (errorBodyBuilder) MapErrorCode(code codes.Code) any {
	switch code {
	case codes.InvalidArgument:
		return sharedv1.ErrorCode_ERROR_CODE_INVALID_REQUEST
	case codes.FailedPrecondition:
		return sharedv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED
	case codes.NotFound:
		return sharedv1.ErrorCode_ERROR_CODE_RESOURCE_NOT_FOUND
	case codes.AlreadyExists:
		return sharedv1.ErrorCode_ERROR_CODE_RESOURCE_CONFLICT
	case codes.Aborted:
		return sharedv1.ErrorCode_ERROR_CODE_CONCURRENT_MODIFICATION
	case codes.Unauthenticated:
		return sharedv1.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS
	case codes.PermissionDenied:
		return sharedv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED
	case codes.ResourceExhausted:
		return sharedv1.ErrorCode_ERROR_CODE_QUOTA_EXCEEDED
	case codes.DeadlineExceeded:
		return sharedv1.ErrorCode_ERROR_CODE_TIMEOUT
	default:
		return nil
	}
}

// errorCodeFor 归一 error_code：未登记的 code 统一回落 INTERNAL_ERROR
// （与原实现"errorCode 零值 = INTERNAL_ERROR"一致）。
func errorCodeFor(code codes.Code) sharedv1.ErrorCode {
	if ec, ok := (errorBodyBuilder{}).MapErrorCode(code).(sharedv1.ErrorCode); ok {
		return ec
	}
	return sharedv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR
}

func errorTypeForCode(code codes.Code) string {
	switch code {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return "invalid_request_error"
	case codes.Unauthenticated:
		return "authentication_error"
	case codes.PermissionDenied:
		return "permission_error"
	case codes.NotFound:
		return "not_found_error"
	case codes.AlreadyExists, codes.Aborted:
		return "conflict_error"
	case codes.ResourceExhausted:
		return "rate_limit_error"
	default:
		return "server_error"
	}
}

// NewCustomMarshaler 返回统一 protojson 序列化器（UseProtoNames=true +
// EmitUnpopulated=false + DiscardUnknown=true）。grpcapi 阶段 1 换库：机制
// 委托 gateway.NewMarshaler，保留原名以维持既有调用点与测试引用。
func NewCustomMarshaler() runtime.Marshaler {
	return gateway.NewMarshaler()
}
