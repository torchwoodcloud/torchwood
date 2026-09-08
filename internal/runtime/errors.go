package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// HTTPErrorHandler converts gRPC errors to a consistent JSON error body.
// P3-3：Internal/Unknown 对外统一文案，原文只进日志（fail-closed，不泄内部细节）。
var HTTPErrorHandler runtime.ErrorHandlerFunc = func(ctx context.Context, mux *runtime.ServeMux, marshaler runtime.Marshaler, w http.ResponseWriter, r *http.Request, err error) {
	st, ok := status.FromError(err)
	if !ok {
		st = status.New(codes.Internal, "internal server error")
		slog.ErrorContext(ctx, "http error: non-status error converted to internal", "error", err.Error(), "code", st.Code().String())
	}

	httpStatus := grpcCodeToHTTP(st.Code())
	errorCode := sharedv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR
	switch st.Code() {
	case codes.InvalidArgument:
		errorCode = sharedv1.ErrorCode_ERROR_CODE_INVALID_REQUEST
	case codes.FailedPrecondition:
		errorCode = sharedv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED
	case codes.NotFound:
		errorCode = sharedv1.ErrorCode_ERROR_CODE_RESOURCE_NOT_FOUND
	case codes.AlreadyExists:
		errorCode = sharedv1.ErrorCode_ERROR_CODE_RESOURCE_CONFLICT
	case codes.Aborted:
		errorCode = sharedv1.ErrorCode_ERROR_CODE_CONCURRENT_MODIFICATION
	case codes.Unauthenticated:
		errorCode = sharedv1.ErrorCode_ERROR_CODE_INVALID_CREDENTIALS
	case codes.PermissionDenied:
		errorCode = sharedv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED
	case codes.ResourceExhausted:
		errorCode = sharedv1.ErrorCode_ERROR_CODE_QUOTA_EXCEEDED
	case codes.DeadlineExceeded:
		errorCode = sharedv1.ErrorCode_ERROR_CODE_TIMEOUT
	}

	// Internal/Unknown 统一对外文案，原始消息仅进日志。
	message := st.Message()
	errorID := uuid.NewString()
	if st.Code() == codes.Internal || st.Code() == codes.Unknown {
		slog.ErrorContext(ctx, "http response: internal error sanitized", "code", st.Code().String(), "original_message", st.Message(), "error_id", errorID, "method", r.URL.Path)
		message = "internal server error"
	}

	// 429 携带 Retry-After（T-01）：从 status 的 RetryInfo detail 提取建议
	// 退避（整秒向上取整）；无 detail 时不设头，由客户端退避。
	if httpStatus == http.StatusTooManyRequests {
		if retryAfter, ok := retryAfterSeconds(st); ok {
			w.Header().Set("Retry-After", retryAfter)
		}
	}

	resp := &sharedv1.ErrorResponse{
		Error: &sharedv1.Error{
			Type:      errorTypeForCode(st.Code()),
			Code:      st.Code().String(),
			Message:   message,
			ErrorId:   errorID,
			ErrorCode: errorCode,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(ctx, "failed to encode error response", "error", err, "error_id", errorID)
	}
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

// retryAfterSeconds 从 status details 提取 RetryInfo 的建议退避秒数
// (向上取整，至少 1s)；无 detail 或时长非法时返回 false。
func retryAfterSeconds(st *status.Status) (string, bool) {
	for _, d := range st.Details() {
		if ri, ok := d.(*errdetails.RetryInfo); ok && ri.GetRetryDelay().AsDuration() > 0 {
			secs := int64(math.Ceil(ri.GetRetryDelay().AsDuration().Seconds()))
			if secs < 1 {
				secs = 1
			}
			return strconv.FormatInt(secs, 10), true
		}
	}
	return "", false
}

func grpcCodeToHTTP(code codes.Code) int {
	switch code {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return 499
	case codes.Unknown:
		return http.StatusInternalServerError
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// CustomMarshaler uses protojson with stable settings.
type CustomMarshaler struct {
	*runtime.JSONPb
}

func NewCustomMarshaler() runtime.Marshaler {
	return &CustomMarshaler{
		JSONPb: &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				UseProtoNames:   true,
				EmitUnpopulated: false,
			},
			UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
		},
	}
}

func (m *CustomMarshaler) ContentType(_ interface{}) string { return "application/json" }

func (m *CustomMarshaler) Marshal(v interface{}) ([]byte, error) {
	return m.JSONPb.Marshal(v)
}

func (m *CustomMarshaler) Unmarshal(data []byte, v interface{}) error {
	return m.JSONPb.Unmarshal(data, v)
}
