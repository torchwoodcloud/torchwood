package interceptor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"buf.build/go/protovalidate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// validateExemptPrefixes 与 rateLimit/usage 维持同一框架服务白名单：
// health/reflection 请求不带 buf.validate 规则，豁免以省去求值开销。
var validateExemptPrefixes = []string{
	"/grpc.health.v1.",
	"/grpc.reflection.",
}

// ValidateInterceptor 按 proto 上的 buf.validate 注解（protovalidate，
// CEL 运行时求值）对请求消息做形状校验：required/长度/正则/枚举/范围
// 一类约束在 proto 声明、在此统一生效；跨字段与业务规则仍留在 app 用例
// 层（P3-18 取舍不变，两层语义边界见 docs/developer/09-api-guide.md §2.3）。
//
// 链上位于 audit/usage 之后、handler 之前（链尾）：校验失败的请求与
// 手写校验时期行为完全一致——产生 InvalidArgument 审计行并计入用量，
// 本拦截器只是把 handler 开头的形状检查外提为 proto 声明。仅 unary，
// 与整条拦截器链一致（业务服务无流式 RPC）。
type ValidateInterceptor struct {
	validator protovalidate.Validator
}

// NewValidateInterceptor 构造校验拦截器；CEL 环境初始化失败（配置级
// 故障）在启动期暴露。注解按 descriptor 惰性编译并缓存，求值为微秒级。
func NewValidateInterceptor() (*ValidateInterceptor, error) {
	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("construct protovalidate validator: %w", err)
	}
	return &ValidateInterceptor{validator: validator}, nil
}

func (v *ValidateInterceptor) UnaryValidateMiddleware(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if v == nil || v.validator == nil || validateExempt(info.FullMethod) {
		return handler(ctx, req)
	}
	msg, ok := req.(proto.Message)
	if !ok {
		// 非 proto 消息（理论不可达）不校验，交由后续链路处理。
		return handler(ctx, req)
	}
	if err := v.validator.Validate(msg); err != nil {
		return nil, validateStatusError(err)
	}
	return handler(ctx, req)
}

// validateStatusError 映射校验错误：规则违规 → InvalidArgument（多条以
// "; " 连接为单行，经 HTTPErrorHandler 原样进入 error.message 与 CLI
// 退出码 2 契约）；CEL 编译/求值故障 → Internal（注解缺陷属服务端 bug，
// fail-closed 拒绝而非放行未校验请求）。
func validateStatusError(err error) error {
	var violations *protovalidate.ValidationError
	if errors.As(err, &violations) {
		return status.Error(codes.InvalidArgument, formatViolations(violations.Violations))
	}
	return status.Error(codes.Internal, "request validation rules failed to evaluate: "+err.Error())
}

func formatViolations(violations []*protovalidate.Violation) string {
	parts := make([]string, 0, len(violations))
	for _, violation := range violations {
		parts = append(parts, violation.String())
	}
	return strings.Join(parts, "; ")
}

func validateExempt(fullMethod string) bool {
	for _, prefix := range validateExemptPrefixes {
		if strings.HasPrefix(fullMethod, prefix) {
			return true
		}
	}
	return false
}
