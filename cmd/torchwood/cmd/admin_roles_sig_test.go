package cmd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"net/url"
	"testing"

	"github.com/lynx-go/commands"
	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
)

// rolesSigKeyHexFor 独立重算派生钥（不触碰进程全局态）：key =
// HMAC-SHA256(master, clients.RolesSigPurpose) 的 hex，与 clients.
// InitRolesSigKey 同式。
func rolesSigKeyHexFor(t *testing.T, master string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(master))
	_, _ = mac.Write([]byte(clients.RolesSigPurpose))
	return hex.EncodeToString(mac.Sum(nil))
}

// slotKeyOf 读 tw_secrets 指定槽位的 key_hex（is_current=true 为 current 位，
// false 为 previous 位）。
func slotKeyOf(t *testing.T, ctx context.Context, db *clients.Database, current bool) string {
	t.Helper()
	var k string
	require.NoError(t, db.NewSelect().TableExpr("public.tw_secrets").
		Column("key_hex").Where("purpose = ? AND is_current = ?", clients.RolesSigPurpose, current).
		Scan(ctx, &k))
	return k
}

func twSecretsRowCount(t *testing.T, ctx context.Context, db *clients.Database) int {
	t.Helper()
	n, err := db.NewSelect().TableExpr("public.tw_secrets").
		Where("purpose = ?", clients.RolesSigPurpose).Count(ctx)
	require.NoError(t, err)
	return n
}

// TestAdminSyncRolesSigViaCLI 是 B15 admin 作业 CLI 集成测试（有测试库时真
// 跑，未设置 TORCHWOOD_TEST_* 时 skip）：经 CLI 动词完整执行
// `torchwood admin sync-roles-sig`（--dsn 直连 + --jwt-secret），覆盖
//   - 首次落库（owner 身份，SetupTestDB 的 DSN 即 owner 引导账号形态）；
//   - 幂等重跑（同钥二次执行整体 no-op）；
//   - 换钥重跑平移（新钥落 current、旧钥降级 previous、行数不变量 ≤2，
//     对齐门禁 A4 双钥矩阵）。
//
// 命令体会改写进程内派生钥全局态（clients.InitRolesSigKey），用例结束后还原
// 测试主密钥，避免影响同包其他用例。
func TestAdminSyncRolesSigViaCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	dsn := testutil.TestDSN()
	if dsn == "" || testutil.AdminDSN() == "" {
		t.Skip("TORCHWOOD_TEST_* not set (run via `task test`, which loads .env)")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// fixture 已同步测试主密钥（仅 current 一行）；结束后还原进程内钥。
	fixtureKey := rolesSigKeyHexFor(t, testutil.TestRolesSigMaster)
	t.Cleanup(func() { _ = clients.InitRolesSigKey(testutil.TestRolesSigMaster) })
	require.Equal(t, fixtureKey, slotKeyOf(t, ctx, db, true), "fixture 初始 current = 测试主密钥派生钥")

	// CLI 命令直连的 dsn 指向 SetupTestDB 的动态隔离库（current_database()
	// 回读库名，替换 base DSN 的 path）。
	var dbName string
	require.NoError(t, db.NewSelect().ColumnExpr("current_database()").Scan(ctx, &dbName))
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + dbName
	targetDSN := u.String()

	run := func(master string) {
		t.Helper()
		executeAdminCmd(t, newAdminSyncRolesSigCmd(), map[string]string{
			"dsn":        targetDSN,
			"jwt-secret": master,
		})
	}

	// 首次换钥作业（测试主密钥 → G1）：G1 落 current，旧钥（fixture）降级
	// previous——双钥平移而非覆盖删除。
	const masterG1 = "admincli-rotation-master-gen1-000000000000"
	const masterG2 = "admincli-rotation-master-gen2-000000000000"
	k1 := rolesSigKeyHexFor(t, masterG1)
	run(masterG1)
	require.Equal(t, k1, slotKeyOf(t, ctx, db, true), "新钥必须落在 current 位")
	require.Equal(t, fixtureKey, slotKeyOf(t, ctx, db, false), "旧钥必须降级 previous（而非删除）")
	require.Equal(t, 2, twSecretsRowCount(t, ctx, db))

	// 幂等重跑：同钥二次执行，槽位与行数不变（部署脚本可安全重跑）。
	run(masterG1)
	require.Equal(t, k1, slotKeyOf(t, ctx, db, true))
	require.Equal(t, fixtureKey, slotKeyOf(t, ctx, db, false))
	require.Equal(t, 2, twSecretsRowCount(t, ctx, db), "同钥重跑必须幂等（无新增行）")

	// 二次换钥（G1 → G2）：previous 平移为 G1（fixture 被 third 条裁掉），
	// 行数不变量 ≤2。
	k2 := rolesSigKeyHexFor(t, masterG2)
	run(masterG2)
	require.Equal(t, k2, slotKeyOf(t, ctx, db, true))
	require.Equal(t, k1, slotKeyOf(t, ctx, db, false), "previous 只保留紧邻上一把")
	require.Equal(t, 2, twSecretsRowCount(t, ctx, db), "third 条直接删——行数不变量 ≤2")
}

// TestAdminSyncRolesSig_RequiresFlags 锁命令的 fail-fast 参数校验：缺 DSN 或
// 缺主密钥时拒绝执行（不触碰数据库）。
func TestAdminSyncRolesSig_RequiresFlags(t *testing.T) {
	// 密闭化：本机 .env 常设 TORCHWOOD_DATA_DATABASE_SOURCE /
	// TORCHWOOD_SECURITY_JWT_SECRET（旗标默认值来源），不隔离时 "missing dsn"
	// 用例会带真实 DSN 连库同步并向 nil Environment 打印而 panic。
	t.Setenv("TORCHWOOD_DATA_DATABASE_SOURCE", "")
	t.Setenv("TORCHWOOD_SECURITY_JWT_SECRET", "")
	for _, tc := range []struct {
		name   string
		flags  map[string]string
		errHas string
	}{
		{name: "missing dsn", flags: map[string]string{"jwt-secret": "x"}, errHas: "database source is empty"},
		{name: "missing jwt secret", flags: map[string]string{"dsn": "postgres://u:p@127.0.0.1:5432/db"}, errHas: "jwt secret is empty"},
		{name: "blank jwt secret", flags: map[string]string{"dsn": "postgres://u:p@127.0.0.1:5432/db", "jwt-secret": "   "}, errHas: "jwt secret is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newAdminSyncRolesSigCmd()
			v.SetFlags(flag.NewFlagSet(v.Name(), flag.ContinueOnError))
			for k, val := range tc.flags {
				require.NoError(t, v.fs.Set(k, val))
			}
			err := v.Run(context.Background(), &commands.Environment{}, nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.errHas)
		})
	}
	// 注册形态断言（admin 分组下可见 sync-roles-sig）。
	admin := newAdminCmd(&globalFlags{})
	_, found := admin.sub.Lookup("sync-roles-sig")
	require.True(t, found, "torchwood admin sync-roles-sig 应已注册")
}
