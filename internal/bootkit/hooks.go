// Package bootkit 收纳 cmd/server 与 cmd/worker 两个组装根共享的 Lynx
// 装配样板（Round4 J4-1 去重）：日志暴露、组件注册顺序、项目 schema
// 启动钩子。两处逐字复制的实现统一收敛于此，防止后续再次分叉。
package bootkit

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/infra/projectschema"
)

// NewLogger 暴露 app 装配的 *slog.Logger（zap 后端），供各层构造器注入。
func NewLogger(app lynx.App) *slog.Logger {
	return app.Logger()
}

// NewComponentBuilders 返回空工厂集（服务均为静态构造，无延迟装配需求）。
func NewComponentBuilders() []lynx.ServiceFactory {
	return nil
}

// GrantsReconcileHook / ScaleMetricsHook / SchemaReconcileHook 是组合根注入
// 的扩展启动钩子类型（门禁 A1 / B12 / B3）：wire 无法区分多个同型
// func(context.Context) error 参数，故以命名类型分流——语义同底层签名，
// nil 表示跳过。
type (
	// GrantsReconcileHook 是启动期列授权全量 reconcile 闭包（cmd/server 注入，
	// cmd/worker 传 nil）。
	GrantsReconcileHook func(context.Context) error
	// ScaleMetricsHook 是启动期规模预警线表计数采集闭包（cmd/server 注入，
	// cmd/worker 传 nil）。
	ScaleMetricsHook func(context.Context) error
	// SchemaReconcileHook 是启动期 schema 漂移对账闭包（门禁 B3：缺列/
	// INVALID·failed 索引/幽灵表三类漂移自动修复 + 告警；cmd/server 注入，
	// cmd/worker 传 nil——边界论证同 GrantsReconcileHook）。
	SchemaReconcileHook func(context.Context) error
)

// NewOnStarts 注册启动钩子集：项目 schema 确保钩子（对全部项目幂等 EnsureAll）
// 与调用方注入的可选扩展钩子。roles 签名密钥同步钩子已退役（转出 POC 门禁
// B15）：密钥落库改部署期 owner 一次性作业（`torchwood admin sync-roles-sig`，
// clients.SyncRolesSigKey），运行 DSN 对 public.tw_secrets 零权限（迁移
// 000004）——进程内派生钥仍由 InitRolesSigSigning 启用（注入 GUC 用进程内钥，
// 无需读库）。
// grantsReconcile / scaleMetrics / schemaReconcile 由组合根注入（cmd/server
// 分别传 documentdb 列授权全量 reconcile（门禁 A1）、规模预警线表计数采集
// （门禁 B12）与 schema 漂移对账（门禁 B3）闭包；cmd/worker 传 nil——worker
// 不碰文档层，import guard 守此边界）。
func NewOnStarts(repo projects.Repository, db *clients.Database, logger *slog.Logger, grantsReconcile GrantsReconcileHook, scaleMetrics ScaleMetricsHook, schemaReconcile SchemaReconcileHook) boot.OnStartHooks {
	hooks := boot.OnStartHooks{
		ProjectSchemaEnsureHook(repo, db, logger),
	}
	if grantsReconcile != nil {
		hooks = append(hooks, lynx.HookFunc(grantsReconcile))
	}
	if scaleMetrics != nil {
		hooks = append(hooks, lynx.HookFunc(scaleMetrics))
	}
	if schemaReconcile != nil {
		hooks = append(hooks, lynx.HookFunc(schemaReconcile))
	}
	return hooks
}

// NewOnStops 返回空钩子集：底层资源清理不进 Lynx（见 cmd/*/main.go 注释，
// 在 runner.RunE() 返回后由 main 统一执行）。
func NewOnStops() boot.OnStopHooks {
	return boot.OnStopHooks{}
}

// ProjectSchemaEnsureHook 返回启动期项目数据面 schema 确保钩子：
// 列出全部项目并对每个 tw_<project.id> 幂等执行 EnsureAll。
func ProjectSchemaEnsureHook(repo projects.Repository, db *clients.Database, logger *slog.Logger) lynx.HookFunc {
	return func(ctx context.Context) error {
		if repo == nil || db == nil {
			return nil
		}
		list, err := repo.ListProjects(ctx)
		if err != nil {
			if logger != nil {
				logger.Error("list projects for schema ensure", "error", err)
			}
			return err
		}
		ids := make([]string, len(list))
		for i := range list {
			ids[i] = list[i].ID
		}
		return projectschema.EnsureAll(ctx, db, ids)
	}
}

// （RolesSigKeySyncHook 已退役（转出 POC 门禁 B15）：密钥落库改部署期 owner
// 一次性作业 `torchwood admin sync-roles-sig`——运行 DSN 持 tw_secrets 四权
// 意味着 DSN 泄漏可读密钥伪造 app.roles GUC 提权，000004 REVOKE 后伪造通道
// 封死；进程内派生（InitRolesSigSigning）保留，注入 GUC 用的是进程内钥。
// CollectionGrantsReconcileHook 与 ScaleMetricsHook 已移至 cmd/server 组合根：
// 两者都是 documentdb 域职责（门禁 A1 / B12），bootkit 为 server/worker 共享
// 装配包，直接实现会把 documentdb 拖进 worker 的依赖闭包（import guard
// TestWorkerDepsGraph 守此边界）。server 侧经 NewOnStarts 的对应可选参数注入
// 闭包，worker 传 nil。）
