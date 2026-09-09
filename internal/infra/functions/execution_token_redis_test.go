package functions_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	infrafunctions "github.com/torchwooddev/torchwood/internal/infra/functions"
)

func TestRedisExecutionTokenService_Lifecycle(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	svc := infrafunctions.NewRedisExecutionTokenService(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()

	// mint → validate：身份信息完整往返，token 带 twx_ 前缀。
	token, err := svc.Mint(ctx, domainfunctions.ExecutionTokenInfo{
		ProjectID:      "p1",
		FunctionID:     "fn1",
		ExecutionID:    "e1",
		Scopes:         []string{"assets:write", "databases:read"},
		InvokingUserID: "u1",
	}, time.Minute)
	require.NoError(t, err)
	require.True(t, len(token) > len(shared.ExecutionTokenPrefix))
	require.Equal(t, shared.ExecutionTokenPrefix, token[:len(shared.ExecutionTokenPrefix)])

	info, err := svc.Validate(ctx, token)
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "p1", info.ProjectID)
	require.Equal(t, "fn1", info.FunctionID)
	require.Equal(t, "e1", info.ExecutionID)
	require.Equal(t, []string{"assets:write", "databases:read"}, info.Scopes)
	require.Equal(t, "u1", info.InvokingUserID)

	// 服务端不存原值：Redis 里只有哈希键。
	require.NotContains(t, mr.Keys(), token)
	require.Len(t, mr.Keys(), 1)
	require.Contains(t, mr.Keys()[0], "torchwood:exec-token:")

	// revoke → validate 失效（主动吊销语义，不等 TTL）。
	require.NoError(t, svc.Revoke(ctx, token))
	info, err = svc.Validate(ctx, token)
	require.NoError(t, err)
	require.Nil(t, info)

	// 未知/过期 token：(nil, nil)，与基础设施故障可区分。
	info, err = svc.Validate(ctx, shared.ExecutionTokenPrefix+"nope")
	require.NoError(t, err)
	require.Nil(t, info)

	// Revoke 幂等。
	require.NoError(t, svc.Revoke(ctx, token))
}

func TestRedisExecutionTokenService_TTLExpiry(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	svc := infrafunctions.NewRedisExecutionTokenService(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()

	token, err := svc.Mint(ctx, domainfunctions.ExecutionTokenInfo{
		ProjectID: "p1", FunctionID: "fn1", ExecutionID: "e1",
	}, 2*time.Second)
	require.NoError(t, err)

	info, err := svc.Validate(ctx, token)
	require.NoError(t, err)
	require.NotNil(t, info)

	mr.FastForward(3 * time.Second)
	info, err = svc.Validate(ctx, token)
	require.NoError(t, err)
	require.Nil(t, info, "TTL 过期后 token 失效（崩溃兜底）")
}

func TestRedisExecutionTokenService_InputValidation(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	svc := infrafunctions.NewRedisExecutionTokenService(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()

	// TTL 非正拒绝。
	_, err = svc.Mint(ctx, domainfunctions.ExecutionTokenInfo{ProjectID: "p", FunctionID: "f", ExecutionID: "e"}, 0)
	require.Error(t, err)
	// 身份三元组缺失拒绝。
	_, err = svc.Mint(ctx, domainfunctions.ExecutionTokenInfo{ProjectID: "p", FunctionID: "f"}, time.Minute)
	require.Error(t, err)
	// token 唯一性（抽样验证 64 次，无碰撞）。
	seen := map[string]struct{}{}
	for i := 0; i < 64; i++ {
		tok, err := svc.Mint(ctx, domainfunctions.ExecutionTokenInfo{ProjectID: "p", FunctionID: "f", ExecutionID: "e"}, time.Minute)
		require.NoError(t, err)
		_, dup := seen[tok]
		require.False(t, dup)
		seen[tok] = struct{}{}
	}
}
