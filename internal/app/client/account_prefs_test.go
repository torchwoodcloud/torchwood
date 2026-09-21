package client

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestMergePrefs：RFC 7386 JSON Merge Patch 纯函数语义——递归合并 / null 删键 /
// 未提及键保留 / 非对象值整体替换 / 空 patch no-op / 入参不被修改。
func TestMergePrefs(t *testing.T) {
	t.Parallel()

	current := map[string]any{
		"theme": "dark",
		"audio": map[string]any{"bgm": true, "volume": 0.8},
		"stale": "to-be-deleted",
	}
	merged := MergePrefs(current, map[string]any{
		"theme": "light",
		"audio": map[string]any{"volume": 0.5},
		"codex": []any{"a", "b"},
		"stale": nil,
	})
	require.Equal(t, "light", merged["theme"])                                    // 标量覆写
	require.Equal(t, map[string]any{"bgm": true, "volume": 0.5}, merged["audio"]) // 对象递归合并
	require.Equal(t, []any{"a", "b"}, merged["codex"])                            // 数组整体替换
	require.NotContains(t, merged, "stale")                                       // null 删键

	// 入参保持原样（合并产出新 map）。
	require.Equal(t, "dark", current["theme"])
	require.Equal(t, 0.8, current["audio"].(map[string]any)["volume"])
	require.Contains(t, current, "stale")

	// 空 patch = 现值内容原样（新容器，后续变更不回渗）。
	noop := MergePrefs(current, map[string]any{})
	require.Equal(t, current, noop)
	noop["theme"] = "mutated"
	require.Equal(t, "dark", current["theme"])
}

// TestAccount_UpdatePrefsMerges：集成面 merge 语义——增量写保留未提及键、
// null 删键、GetPrefs 读到合并结果、合并结果超限拒绝且不落库；avatar 经
// UpdateAccount 设置/清除且不触碰 name（name/avatar 的格式门禁在 proto
// validate 注解，属契约层，此处只验用例透传与投影）。
func TestAccount_UpdatePrefsMerges(t *testing.T) {
	ctx, account, projectID, _, _, _ := setupG3Account(t)
	_, userID := signUpG3User(t, ctx, account, projectID, "prefs-merge@torchwood.local")
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		Roles:     []string{"users", "user:" + userID},
	})

	prefs, err := account.UpdatePrefs(authCtx, map[string]any{"theme": "dark"})
	require.NoError(t, err)
	require.Equal(t, "dark", prefs["theme"])

	// 增量写：未提及的 theme 保留。
	prefs, err = account.UpdatePrefs(authCtx, map[string]any{"color": "red"})
	require.NoError(t, err)
	require.Equal(t, "dark", prefs["theme"])
	require.Equal(t, "red", prefs["color"])

	// null 删键；GetPrefs 反映合并结果。
	prefs, err = account.UpdatePrefs(authCtx, map[string]any{"theme": nil})
	require.NoError(t, err)
	require.NotContains(t, prefs, "theme")
	got, err := account.GetPrefs(authCtx)
	require.NoError(t, err)
	require.Equal(t, "red", got["color"])
	require.NotContains(t, got, "theme")

	// avatar：设置 → 投影带出；与 name 同提互不影响；空串清除。
	updated, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Avatar: strPtr("https://cdn.example.test/a.png"),
	})
	require.NoError(t, err)
	require.Equal(t, "https://cdn.example.test/a.png", updated.Avatar)
	require.Equal(t, "G3 User", updated.Name)

	updated, err = account.UpdateAccount(authCtx, UpdateAccountCommand{
		Name:   strPtr("新昵称"),
		Avatar: strPtr(""),
	})
	require.NoError(t, err)
	require.Equal(t, "新昵称", updated.Name)
	require.Empty(t, updated.Avatar)

	// 合并结果超 64KB → InvalidArgument 且不落库（现值不含 blob）。
	_, err = account.UpdatePrefs(authCtx, map[string]any{"blob": strings.Repeat("x", maxPrefsBytes)})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	got, err = account.GetPrefs(authCtx)
	require.NoError(t, err)
	require.NotContains(t, got, "blob")
	require.Equal(t, "red", got["color"])
}
