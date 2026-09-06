package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/torchwooddev/torchwood/internal/infra/clients"
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
func newAdminSyncRolesSigCmd() *cobra.Command {
	var dsn string
	var jwtSecret string
	cmd := &cobra.Command{
		Use:   "sync-roles-sig",
		Short: "roles 签名密钥落库 tw_secrets（部署期 owner 作业，B15）",
		// 直连 DB 的部署期作业（对齐 health/uuid 的豁免形态）：不持 Server
		// API key——root 的 api-key 必填校验按 annotation 豁免。
		Annotations: map[string]string{annotationNoKey: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
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
			cmd.Println("roles sig key synced into public.tw_secrets (current slot; dual-key rotation preserved)")
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", os.Getenv(adminDBFlagDsn),
		"Postgres DSN（owner/引导账号，非运行态 authenticator；缺省读 "+adminDBFlagDsn+"）")
	cmd.Flags().StringVar(&jwtSecret, "jwt-secret", os.Getenv(adminJWTSecretFlagEnv),
		"主密钥 security.jwt.secret（须与运行态一致；缺省读 "+adminJWTSecretFlagEnv+"）")
	return cmd
}
