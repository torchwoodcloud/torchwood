package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMemObjectStore_ListPrefix 验证 List 的前缀过滤与 LastModified 语义。
func TestMemObjectStore_ListPrefix(t *testing.T) {
	ctx := context.Background()
	store := NewMemObjectStore()

	require.NoError(t, store.Put(ctx, "b", "p1/b1/f1", nil, 0, ""))
	require.NoError(t, store.Put(ctx, "b", "p1/b1/f2/chunks/001", nil, 0, ""))
	require.NoError(t, store.Put(ctx, "b", "p2/b2/f3", nil, 0, ""))

	// 前缀过滤：p1/ 只返回前两个 key。
	objects, err := store.List(ctx, "b", "p1/")
	require.NoError(t, err)
	require.Len(t, objects, 2)
	keys := map[string]time.Time{}
	for _, o := range objects {
		keys[o.Key] = o.LastModified
	}
	_, ok := keys["p1/b1/f1"]
	require.True(t, ok)
	_, ok = keys["p1/b1/f2/chunks/001"]
	require.True(t, ok)

	// LastModified 接近 Put 时刻。
	require.WithinDuration(t, time.Now(), keys["p1/b1/f1"], time.Minute)

	// 空前缀返回全部；不存在的 bucket 返回空。
	all, err := store.List(ctx, "b", "")
	require.NoError(t, err)
	require.Len(t, all, 3)
	none, err := store.List(ctx, "missing", "")
	require.NoError(t, err)
	require.Empty(t, none)
}

// TestMemObjectStore_SetObjectTime 验证历史时间戳设置（孤儿清理测试依赖）。
func TestMemObjectStore_SetObjectTime(t *testing.T) {
	ctx := context.Background()
	store := NewMemObjectStore()
	require.NoError(t, store.Put(ctx, "b", "k", nil, 0, ""))

	old := time.Now().Add(-49 * time.Hour)
	require.NoError(t, store.SetObjectTime("b", "k", old))

	objects, err := store.List(ctx, "b", "")
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.Equal(t, old, objects[0].LastModified)

	// 不存在的 key → 报错。
	require.Error(t, store.SetObjectTime("b", "nope", old))
}
