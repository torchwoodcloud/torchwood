package shared

import (
	"errors"

	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MapUserError 把 users 域 sentinel 错误映射为 gRPC status；未命中的错误
// 原样返回（调用方以 mapped != err 判定是否已映射，再决定包装或透传）。
// 是 client/server 两个用例面包 user 错误映射的唯一事实来源——此前两份
// 复制的 mapUserError 已漂移出两种消息口径。
func MapUserError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, users.ErrEmailAlreadyRegistered) {
		return status.Error(codes.AlreadyExists, err.Error())
	}
	if errors.Is(err, users.ErrEmailRequired) ||
		errors.Is(err, users.ErrUserIDRequired) ||
		errors.Is(err, users.ErrPasswordTooShort) ||
		errors.Is(err, users.ErrPasswordTooLong) ||
		errors.Is(err, users.ErrPasswordWeak) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return err
}
