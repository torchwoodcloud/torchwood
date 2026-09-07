package projects

import (
	"context"
	"time"
)

type Repository interface {
	CreateProject(ctx context.Context, p *Project) error
	GetProject(ctx context.Context, id string) (*Project, error)
	GetProjectByName(ctx context.Context, name string) (*Project, error)
	ListProjects(ctx context.Context) ([]Project, error)
	UpdateProject(ctx context.Context, p *Project) error
	DeleteProject(ctx context.Context, id string) error
	// DeleteProjectControlPlaneRows 清理 public 控制面中该项目的派生行
	// （outbox/死信/api_keys/audit_logs/admin_projects/provider_resource_index）。
	// 感知调用方事务：项目删除事务内执行，与 schema DROP 原子提交。
	DeleteProjectControlPlaneRows(ctx context.Context, projectID string) error
}

// SchemaManager 管理项目数据面 schema 的生命周期（infra/projectschema 适配）。
// Ensure/DropCascade 感知调用方事务：ctx 携带事务时并入同一事务，
// 与项目行写入/删除原子提交。
type SchemaManager interface {
	// Ensure 幂等确保 tw_<projectID> schema 存在且迁移到最新版本。
	Ensure(ctx context.Context, projectID string) error
	// DropCascade 删除项目数据面 schema（CASCADE，连带其全部对象）。
	DropCascade(ctx context.Context, projectID string) error
	// Invalidate 清除本进程的 schema 就绪缓存（DropCascade 后自动调用；
	// 迁移器之外带外改动 schema 状态时同样需要）。
	Invalidate(projectID string)
	// ReconcileOrphanSchemas 对账物理 schema 与 catalog 清单（Round4 J5-5）：
	// 删除属于该项目命名空间（tw_<projectID>_<db> 两段式、db 合法且不在
	// document_databases 目录中）的孤儿 schema（DROP CASCADE）。须在项目
	// 删除事务之外执行；返回清理的 schema 数。失败仅告警，不阻断删除。
	ReconcileOrphanSchemas(ctx context.Context, projectID string) (int, error)
}

type APIKeyRepository interface {
	CreateAPIKey(ctx context.Context, key *APIKey) error
	GetAPIKey(ctx context.Context, projectID, id string) (*APIKey, error)
	GetAPIKeyBySecretHash(ctx context.Context, hash string) (*APIKey, error)
	ListAPIKeys(ctx context.Context, projectID string) ([]APIKey, error)
	DeleteAPIKey(ctx context.Context, projectID, id string) error
}

type AdminRepository interface {
	GetAdmin(ctx context.Context, id string) (*Admin, error)
	GetAdminByEmail(ctx context.Context, email string) (*Admin, error)
	ListAdmins(ctx context.Context) ([]Admin, error)
	CreateAdmin(ctx context.Context, admin *Admin) error
	UpdateAdmin(ctx context.Context, admin *Admin) error
	// RevokeCredentials 持久化 admin 凭证撤销时间戳：撤销时刻之前签发的
	// 全部 token 在验证时失效。幂等且只前推（已有更晚撤销时间不回退）。
	// 感知调用方事务：与 admins 行写同事务提交。
	RevokeCredentials(ctx context.Context, adminID string, revokedAt time.Time) error
	DeleteAdmin(ctx context.Context, id string) error
	CountAdminsByRole(ctx context.Context, role string) (int64, error)
	// WithBootstrapLock 在事务内持 pg_advisory_xact_lock(key) 执行 fn，
	// 串行化首个管理员引导的首次性检查与创建。fn 内通过 ctx 使用同一事务
	// （ListAdmins/CreateAdmin/DeleteAdmin 等方法自动感知事务上下文）；
	// 锁随事务提交/回滚自动释放。
	WithBootstrapLock(ctx context.Context, key int64, fn func(ctx context.Context) error) error
}
