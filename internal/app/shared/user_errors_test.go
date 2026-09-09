package shared

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMapUserError(t *testing.T) {
	other := errors.New("something else")
	cases := []struct {
		name string
		err  error
		want codes.Code
		msg  string
	}{
		{"nil passthrough", nil, codes.OK, ""},
		{"email already registered", users.ErrEmailAlreadyRegistered, codes.AlreadyExists, "email already registered"},
		{"email required", users.ErrEmailRequired, codes.InvalidArgument, "email is required"},
		{"user id required", users.ErrUserIDRequired, codes.InvalidArgument, "user id is required"},
		{"password too short", users.ErrPasswordTooShort, codes.InvalidArgument, users.ErrPasswordTooShort.Error()},
		{"password too long", users.ErrPasswordTooLong, codes.InvalidArgument, users.ErrPasswordTooLong.Error()},
		{"password weak", users.ErrPasswordWeak, codes.InvalidArgument, users.ErrPasswordWeak.Error()},
		{"wrapped sentinel still matches", fmt.Errorf("insert user: %w", users.ErrEmailAlreadyRegistered), codes.AlreadyExists, "insert user: email already registered"},
		{"unknown error untouched", other, codes.OK, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := MapUserError(tc.err)
			if tc.err == nil {
				require.NoError(t, got)
				return
			}
			if tc.want == codes.OK {
				require.Equal(t, tc.err, got, "未知错误必须原样返回（mapped == err 语义）")
				return
			}
			require.Equal(t, tc.want, status.Code(got))
			require.Equal(t, tc.msg, status.Convert(got).Message())
		})
	}
}
