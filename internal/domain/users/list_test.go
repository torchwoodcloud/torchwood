package users

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

func TestParseUserList_AllowsWhitelist(t *testing.T) {
	t.Parallel()

	q, err := ParseUserList([]string{
		query.BuildEqual("email", "a@b.c"),
		`greaterThan("created_at","2020-01-01T00:00:00Z")`,
		`orderDesc("updated_at")`,
		"limit(10)",
		"offset(2)",
	})
	require.NoError(t, err)
	require.Equal(t, 10, q.Limit)
	require.Equal(t, 2, q.Offset)
	require.Equal(t, "updated_at", q.Orders[0].Attribute)
	require.True(t, q.Orders[0].Desc)
}

func TestParseUserList_RejectsUnknownAttrAndOp(t *testing.T) {
	t.Parallel()

	_, err := ParseUserList([]string{query.BuildEqual("password_hash", "x")})
	require.ErrorIs(t, err, ErrInvalidListQuery)
	require.Contains(t, err.Error(), "password_hash")

	_, err = ParseUserList([]string{query.BuildFilter("contains", "name", "a")})
	require.ErrorIs(t, err, ErrInvalidListQuery)
	require.True(t, errors.Is(err, ErrInvalidListQuery))
}

func TestNormalizeUpdateColumns(t *testing.T) {
	t.Parallel()

	got, err := NormalizeUpdateColumns(map[string]any{
		"email":         " Alice@Torchwood.local ",
		"password_hash": "h1",
	})
	require.NoError(t, err)
	require.Equal(t, "alice@torchwood.local", got["email"])
	require.Equal(t, "h1", got["password_hash"])

	_, err = NormalizeUpdateColumns(map[string]any{"unknown": 1})
	require.ErrorIs(t, err, ErrInvalidUpdate)

	_, err = NormalizeUpdateColumns(nil)
	require.ErrorIs(t, err, ErrInvalidUpdate)
}

func TestIsEmailUniqueViolation(t *testing.T) {
	t.Parallel()

	require.True(t, IsEmailUniqueViolation(`duplicate key value violates unique constraint "sys_users_email_unique"`))
	require.True(t, IsEmailUniqueViolation(`duplicate key value violates unique constraint "users_email_unique"`))
	require.True(t, IsEmailUniqueViolation(`Key (email)=(a@b.c) already exists.`))
	require.False(t, IsEmailUniqueViolation(`duplicate key value violates unique constraint "users_pkey"`))
}
