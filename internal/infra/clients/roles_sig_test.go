package clients

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSignRolesSig_FormatAndVerify（阶段③-b 包 C + R16 ①）：签名格式
// "<exp>|<hexmac>"，消息 = tenant + "|" + roles + "|" + exp；与 SQL 侧
// public.hmac(msg, key_hex, 'sha256') 的 hex 输出逐字一致（键 = hex 字符串
// 本身的字节）。
func TestSignRolesSig_FormatAndVerify(t *testing.T) {
	require.NoError(t, InitRolesSigKey("unit-test-master-secret-0123456789abcdef"))
	keyHex, ok := RolesSigKeyHex()
	require.True(t, ok)

	const tenant = int64(42)
	now := time.Unix(1750000000, 0).UTC()
	sig := SignRolesSig(keyHex, tenant, "any\x1fuser:bob", now)
	parts := strings.SplitN(sig, "|", 2)
	require.Len(t, parts, 2)
	require.Equal(t, "1750000180", parts[0], "exp = now + rolesSigTTL(180s)")

	// 独立重算（不依赖实现自身）验证 mac——消息含 tenant（R16 ①）。
	mac := hmac.New(sha256.New, []byte(keyHex))
	_, _ = mac.Write([]byte("42|any\x1fuser:bob|1750000180"))
	require.Equal(t, hex.EncodeToString(mac.Sum(nil)), parts[1])

	// 主密钥派生：与 page-token 同模式（HMAC-SHA256(master, purpose)）。
	master := hmac.New(sha256.New, []byte("unit-test-master-secret-0123456789abcdef"))
	_, _ = master.Write([]byte(RolesSigPurpose))
	require.Equal(t, hex.EncodeToString(master.Sum(nil)), keyHex)

	// tenant 进消息：同 roles 不同 tenant → 不同 mac。
	other := SignRolesSig(keyHex, tenant+1, "any\x1fuser:bob", now)
	require.NotEqual(t, sig, other)

	// 密钥初始化门槛：空主密钥拒绝。
	require.Error(t, InitRolesSigKey("  "))
}

// TestInjectIdentitySQL_IncludesSig：tw_app 注入语句携带 app.roles_sig 与
// app.tenant（密钥已初始化时）；tw_owner/tw_system 与密钥未初始化的 tw_app
// 不带（DB 侧验签 fail-closed）。
func TestInjectIdentitySQL_IncludesSig(t *testing.T) {
	require.NoError(t, InitRolesSigKey("unit-test-master-secret-0123456789abcdef"))

	stmt, args := injectIdentitySQL(RoleApp, 7, "any")
	require.Contains(t, stmt, `set_config('app.roles_sig', ?, true)`)
	require.Contains(t, stmt, `set_config('app.tenant', ?, true)`)
	require.Len(t, args, 3)
	require.Equal(t, "7", args[2], "tenant 十进制串注入 app.tenant")
	require.Contains(t, args[1].(string), "|", "sig 形态 <exp>|<hexmac>")

	for _, role := range []string{RoleOwner, RoleSystem} {
		stmt, args = injectIdentitySQL(role, 0, "")
		require.NotContains(t, stmt, "app.roles_sig", "仅 tw_app 验签")
		require.NotContains(t, stmt, "app.tenant")
		require.Len(t, args, 1)
	}

	// 密钥未初始化的 tw_app：不注入 sig/tenant（fail-closed 由 DB 侧承载）。
	rolesSigKeyHex.Store("")
	stmt, args = injectIdentitySQL(RoleApp, 7, "any")
	require.NotContains(t, stmt, "app.roles_sig")
	require.Len(t, args, 1)
	// 恢复密钥，避免影响同包其他测试。
	require.NoError(t, InitRolesSigKey("unit-test-master-secret-0123456789abcdef"))
}

// TestSameExecIdentity_TenantAware：租户不同 = 身份不同（R16 ①——中段切换/
// 恢复比较必须感知 tenant，防陈旧身份覆盖新注入）。
func TestSameExecIdentity_TenantAware(t *testing.T) {
	base := ExecIdentity{Role: RoleApp, Roles: []string{"any"}, Tenant: 7}
	require.True(t, SameExecIdentity(base, ExecIdentity{Role: RoleApp, Roles: []string{"any"}, Tenant: 7}))
	require.False(t, SameExecIdentity(base, ExecIdentity{Role: RoleApp, Roles: []string{"any"}, Tenant: 8}))
}

// TestRolesSigTTL_CoversLongTransaction（S6 缺陷 2）：sig 的 exp 从事务首条
// 注入起算、事务内不再续签，TTL 必须显著大于 pgdriver ReadTimeout（60s），
// 否则接近/超过 TTL 的长事务后半段 tw_roles() 验签过期 → 零角色 fail-closed。
// 函数级断言：注入时刻起 170s（≈事务内长尾语句仍需通过验签的最坏窗）exp
// 未到、sig 仍有效；TTL 时刻整点过期。不真等——exp 由 SignRolesSig 从
// now+TTL 起算，直接对常量与 exp 差值断言。
func TestRolesSigTTL_CoversLongTransaction(t *testing.T) {
	require.Greater(t, RolesSigTTL, 2*60*time.Second,
		"RolesSigTTL 必须显著大于 ReadTimeout(60s)，长事务后半段验签才不塌缩")

	now := time.Unix(1750000000, 0).UTC()
	sig := SignRolesSig("aa", 1, "any", now)
	expStr := strings.SplitN(sig, "|", 2)[0]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	require.NoError(t, err)

	require.GreaterOrEqual(t, exp, now.Add(170*time.Second).Unix(),
		"注入后 170s（长事务后半段）sig 仍未过期")
	require.LessOrEqual(t, exp, now.Add(RolesSigTTL).Unix(),
		"exp 恰为 now+TTL，窗口不超发")
}
