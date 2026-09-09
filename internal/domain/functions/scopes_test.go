package functions

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeDeclaredScopes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{name: "空集合法（fail-closed 默认）", in: nil, want: []string{}},
		{name: "空列表合法", in: []string{}, want: []string{}},
		{name: "单读写项", in: []string{"assets:write"}, want: []string{"assets:write"}},
		{name: "白名单资源全接受", in: []string{
			"assets:read", "databases:write", "users:read", "groups:write",
			"storage:read", "subscriptions:write", "payments:read",
		}, want: []string{
			"assets:read", "databases:write", "groups:write", "payments:read",
			"storage:read", "subscriptions:write", "users:read",
		}},
		{name: "自动去重", in: []string{"assets:write", "assets:write", "databases:read"}, want: []string{"assets:write", "databases:read"}},
		{name: "字典序稳定排序", in: []string{"users:read", "assets:write"}, want: []string{"assets:write", "users:read"}},
		{name: "空白项 trim 后合法", in: []string{" assets:write "}, want: []string{"assets:write"}},
		{name: "拒绝自我复制面 functions", in: []string{"functions:write"}, wantErr: true},
		{name: "拒绝 projects", in: []string{"projects:read"}, wantErr: true},
		{name: "拒绝 billing", in: []string{"billing:write"}, wantErr: true},
		{name: "拒绝 outbox", in: []string{"outbox:read"}, wantErr: true},
		{name: "拒绝 oauthproviders", in: []string{"oauthproviders:read"}, wantErr: true},
		{name: "拒绝未知资源", in: []string{"widgets:read"}, wantErr: true},
		{name: "拒绝非法方向", in: []string{"assets:admin"}, wantErr: true},
		{name: "拒绝缺失方向", in: []string{"assets"}, wantErr: true},
		{name: "拒绝缺失资源", in: []string{":write"}, wantErr: true},
		{name: "拒绝裸资源点形态", in: []string{"assets.write"}, wantErr: true},
		{name: "拒绝实例限定形态", in: []string{"databases:blog.read"}, wantErr: true},
		{name: "拒绝通配符", in: []string{"*"}, wantErr: true},
		{name: "拒绝 all", in: []string{"all"}, wantErr: true},
		{name: "空串拒绝", in: []string{""}, wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDeclaredScopes(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestDeclaredScopePermissions(t *testing.T) {
	t.Parallel()

	perms := DeclaredScopePermissions([]string{"assets:write", "databases:read"})
	require.Equal(t, []string{"assets.write", "databases.read"}, perms)

	// 非法项跳过、权限串去重。
	perms = DeclaredScopePermissions([]string{"assets:read", "assets:read", "bogus", "functions:write"})
	require.Equal(t, []string{"assets.read"}, perms)

	// 空/非法输入安全。
	require.Empty(t, DeclaredScopePermissions(nil))
}
