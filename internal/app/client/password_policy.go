package client

import (
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// validatePasswordStrength 委托领域层策略，保持单一事实来源。
func validatePasswordStrength(pw string) error {
	if err := users.ValidatePasswordStrength(pw); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}
