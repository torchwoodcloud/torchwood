package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lynx-go/commands"

	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

// adminJWTSecretFlagEnv 是 roles 签名主密钥（security.jwt.secret）的环境
// 变量缺省来源——与运行态 server/worker 同源同值：密钥 = HMAC-SHA256(主密钥,
// "tw-roles-guc-v1")，两侧不一致时 tw_roles() 验签 fail-closed（文档查询
// 不可用而非静默放行）。
const adminJWTSecretFlagEnv = "TORCHWOOD_SECURITY_JWT_SECRET"

// newAdminSyncRolesSigCmd 提供部署期 roles 签名密钥落库作业（转出 POC 门禁
// B15）：`torchwood admin sync-roles-sig`——把 HMAC-SHA256(主密钥,
// "tw-roles-guc-v1") 派生钥经双钥平移（旧 current 降级 previous、third 条
// 直接删，clients.SyncRolesSigKey）落进 public.tw_secrets，供 tw_roles()/
// tw_tenant() 验签。
//
// B15 形态约束（docs/developer/13-operations.md §4.5）：
//   - 执行身份必须是 **owner/引导账号**（迁移 DSN），不是运行态 authenticator
//     DSN——000004 起 authenticator 对 tw_secrets 零权限，伪造通道封死；
//   - 幂等（重跑安全）；换钥 = 改运行态 security.jwt.secret 后重跑本作业，
//     滚动重启窗口内旧 sig 经 previous 槽验签（双钥语义，门禁 A4）；
//   - 部署时序：迁移（000004）→ 本作业 → server/worker 启动。服务在密钥
//     未落库时启动，文档查询 fail-closed（零角色）属预期——首个业务查询
//     暴露而非静默放行。
//
// 直连 DB 的部署期作业（对齐 export/import 形态）：不持 Server API key，
// 不挂全局旗标、不做 api-key 校验。
func newAdminSyncRolesSigCmd() *verb {
	var dsn string
	var jwtSecret string
	return newVerb(nil, "sync-roles-sig", "persist the roles signing key into tw_secrets (deployment-time owner job, B15)", "admin sync-roles-sig",
		func(fs *flag.FlagSet) {
			fs.StringVar(&dsn, "dsn", os.Getenv(adminDBFlagDsn),
				"Postgres DSN (owner/bootstrap account, not the runtime authenticator; defaults to "+adminDBFlagDsn+")")
			fs.StringVar(&jwtSecret, "jwt-secret", os.Getenv(adminJWTSecretFlagEnv),
				"master key security.jwt.secret (must match the runtime value; defaults to "+adminJWTSecretFlagEnv+")")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			secret := strings.TrimSpace(jwtSecret)
			if secret == "" {
				return fmt.Errorf("jwt secret is empty: pass --jwt-secret or set %s", adminJWTSecretFlagEnv)
			}
			db, closeDB, err := openAdminProjectDB(dsn)
			if err != nil {
				return err
			}
			defer closeDB()
			if err := clients.InitRolesSigKey(secret); err != nil {
				return fmt.Errorf("init roles sig key: %w", err)
			}
			if err := clients.SyncRolesSigKey(context.Background(), db); err != nil {
				return err
			}
			fmt.Fprintln(env.Stderr, "roles sig key synced into public.tw_secrets (current slot; dual-key rotation preserved)")
			return nil
		})
}
