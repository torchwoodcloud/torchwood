package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	domainstorage "github.com/torchwooddev/torchwood/internal/domain/storage"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/crud"
	"github.com/torchwooddev/torchwood/pkg/ident"
	"github.com/torchwooddev/torchwood/pkg/uow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxProjectDescriptionLen 是项目 description 的长度上限，
// CreateProject 与 UpdateProject 两侧一致约束（口径 a）。
const maxProjectDescriptionLen = 512

// projectObjectPurgeTimeout 是删除项目后异步清空对象存储前缀的总预算
// （含一次重试；Round4 J5-2）。purge 脱离请求与停机编排运行（60s > 30s drain），
// 失败仅留可追踪日志（含 bucket/prefix 定位残留），由运维按日志前缀手工清理
// 或重放删除；后续可演进为持久化 outbox/worker 任务以保障可重放。
const projectObjectPurgeTimeout = 60 * time.Second

type Projects struct {
	projectRepo      projects.Repository
	docDB            databases.DocumentDB
	tx               uow.Runner
	schema           projects.SchemaManager
	adminProjectRepo projects.AdminProjectRepository
	// purger/cfg 由 WithObjectPurger 注入（组合根装配）：项目事务提交后异步
	// 清空共享桶 {projectID}/ 前缀。未注入时跳过 purge（单测/旧构造路径）。
	purger domainstorage.Purger
	bucket string
}

// ProjectsOption 定制 Projects 可选依赖。
type ProjectsOption func(*Projects)

// WithObjectPurger 注入对象存储 Purger 与存储配置（解析共享桶名）。
func WithObjectPurger(purger domainstorage.Purger, cfg *config.AppConfig) ProjectsOption {
	return func(s *Projects) {
		s.purger = purger
		if b := cfg.GetStorage().GetS3().GetBucket(); b != "" {
			s.bucket = b
		} else {
			s.bucket = domainstorage.DefaultBucketName
		}
	}
}

// NewProjects 构造项目用例。tx 注入 uow.Runner 端口（事务编排），schema
// 注入 projects.SchemaManager 端口（数据面 schema 生命周期，infra 适配）。
func NewProjects(projectRepo projects.Repository, docDB databases.DocumentDB, tx uow.Runner, schema projects.SchemaManager, adminProjectRepo projects.AdminProjectRepository, opts ...ProjectsOption) *Projects {
	s := &Projects{projectRepo: projectRepo, docDB: docDB, tx: tx, schema: schema, adminProjectRepo: adminProjectRepo}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type CreateProjectCommand struct {
	ID              string
	Name            string
	Description     string
	FirstDatabaseID string // 缺省 "default"；CreateProject 内部调 infra 建空业务库
}

func (s *Projects) CreateProject(ctx context.Context, cmd CreateProjectCommand) (*projects.Project, error) {
	// 项目是平台级资源：PERMISSION [owner,admin]（proto 声明）+ 本守卫纵深防御。
	if err := appshared.RequirePlatformPrincipal(ctx); err != nil {
		return nil, err
	}
	return s.CreateProjectInternal(ctx, cmd)
}

// CreateProjectInternal 创建项目（校验 id/name/description、事务内插入
// project、Apply 静态表并建第一业务库）。不做 principal 检查，仅供 bootstrap 等
// 系统路径调用，调用方负责授权；外部入口 CreateProject 保留平台 admin 校验后
// 委托本方法。
func (s *Projects) CreateProjectInternal(ctx context.Context, cmd CreateProjectCommand) (*projects.Project, error) {
	if err := ident.ValidateSchemaResourceID(cmd.ID); err != nil {
		return nil, appshared.MapIdentError(err)
	}
	if cmd.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if len(cmd.Description) > maxProjectDescriptionLen {
		return nil, status.Error(codes.InvalidArgument, "description must be at most 512 characters")
	}
	firstDBID := strings.TrimSpace(cmd.FirstDatabaseID)
	if firstDBID == "" {
		firstDBID = "default"
	}
	if err := ident.ValidateSchemaResourceID(firstDBID); err != nil {
		return nil, appshared.MapIdentError(err)
	}
	p := &projects.Project{
		ID:          cmd.ID,
		Name:        cmd.Name,
		Description: cmd.Description,
		Status:      "active",
		Settings:    map[string]any{},
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	err := s.tx.Run(ctx, func(txCtx context.Context) error {
		if err := s.projectRepo.CreateProject(txCtx, p); err != nil {
			return fmt.Errorf("insert project: %w", err)
		}
		// Ensure 幂等（含 CREATE SCHEMA IF NOT EXISTS + 迁移），并入本事务。
		if err := s.schema.Ensure(txCtx, p.ID); err != nil {
			return fmt.Errorf("apply project schema: %w", err)
		}
		if err := s.docDB.CreateDatabase(txCtx, p.ID, firstDBID, firstDBID); err != nil {
			return fmt.Errorf("create first database: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

// DeleteProject 对外删除入口：仅平台 admin。校验存在后委托 DeleteProjectInternal。
func (s *Projects) DeleteProject(ctx context.Context, id string) error {
	if err := appshared.RequirePlatformPrincipal(ctx); err != nil {
		return err
	}
	if err := ident.ValidateSchemaResourceID(id); err != nil {
		return appshared.MapIdentError(err)
	}
	p, err := s.projectRepo.GetProject(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return status.Error(codes.NotFound, "project not found")
	}
	return s.DeleteProjectInternal(ctx, id)
}

// DeleteProjectInternal 级联删除项目（不做 principal 检查）。setup 回滚与
// 测试清理必须走这里；外部入口 DeleteProject 保留平台 admin 校验后委托本方法。
// 同一事务：业务 schema DROP → 清理 public 行 → DROP tw_<project> → 删 projects 行。
// 事务外前置：schema 对账清理孤儿（失败仅告警）；事务后异步：对象存储 purge。
func (s *Projects) DeleteProjectInternal(ctx context.Context, id string) error {
	if err := ident.ValidateSchemaResourceID(id); err != nil {
		return appshared.MapIdentError(err)
	}
	// schema 对账（Round4 J5-5）：information_schema tw_<p>_% 与 catalog 清单
	// 求差，孤儿 DROP CASCADE。在删除事务之外执行、失败仅告警——孤儿只可能
	// 来自历史部分失败，不得因对账问题阻断正常删除。
	if n, err := s.schema.ReconcileOrphanSchemas(ctx, id); err != nil {
		slog.Warn("project schema reconcile failed; continuing delete",
			"project_id", id, "error", err)
	} else if n > 0 {
		slog.Warn("dropped orphan project schemas before delete",
			"project_id", id, "count", n)
	}
	err := s.tx.Run(ctx, func(txCtx context.Context) error {
		dbs, err := s.docDB.ListDatabases(txCtx, id)
		if err != nil {
			return fmt.Errorf("list databases: %w", err)
		}
		for _, db := range dbs {
			if db.ID == ident.ProjectDataPlaneID {
				continue
			}
			if err := s.docDB.DeleteDatabase(txCtx, id, db.ID); err != nil {
				return fmt.Errorf("drop business schema %s: %w", db.ID, err)
			}
		}
		if err := s.projectRepo.DeleteProjectControlPlaneRows(txCtx, id); err != nil {
			return err
		}
		if err := s.schema.DropCascade(txCtx, id); err != nil {
			return err
		}
		if err := s.projectRepo.DeleteProject(txCtx, id); err != nil {
			return fmt.Errorf("delete project row: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// schema 已 DROP：清除就绪缓存，否则同 ID 重建项目时缓存直通会跳过重建
	//（DropCascade 语义的一部分，见 projects.SchemaManager）。
	s.schema.Invalidate(id)
	// 对象存储 purge（Round4 J5-2）：事务已提交，异步清空共享桶 {id}/ 前缀；
	// 失败仅告警，不影响删除结果。
	s.purgeObjectsAsync(id)
	return nil
}

// purgeObjectsAsync 异步清空项目的对象存储前缀（60s 总预算 + 失败重试一次）。
// goroutine 脱离请求与停机编排运行（见 projectObjectPurgeTimeout 注释）；
// 错误只留可追踪日志（含 bucket/prefix 定位残留），由运维按日志前缀手工清理
// 或重放删除。
func (s *Projects) purgeObjectsAsync(projectID string) {
	if s.purger == nil {
		return
	}
	bucket := s.bucket
	prefix := projectID + "/"
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), projectObjectPurgeTimeout)
		defer cancel()
		n, err := s.purger.PurgePrefix(ctx, bucket, prefix)
		if err != nil {
			slog.Warn("project object purge failed; retrying once",
				"project_id", projectID, "bucket", bucket, "prefix", prefix,
				"purged", n, "error", err)
			n, err = s.purger.PurgePrefix(ctx, bucket, prefix)
		}
		if err != nil {
			slog.Error("project object purge failed after retry; orphan objects remain",
				"project_id", projectID, "bucket", bucket, "prefix", prefix,
				"purged", n, "error", err)
			return
		}
		slog.Info("project objects purged",
			"project_id", projectID, "bucket", bucket, "prefix", prefix, "objects", n)
	}()
}

func (s *Projects) ListProjects(ctx context.Context, pageSize int32, pageToken string) ([]projects.Project, *crud.PaginationInfo, error) {
	principal, ok := contexts.Principal(ctx)
	if !ok {
		return nil, nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	params, err := crud.ParseListParams(pageSize, pageToken, "", "")
	if err != nil {
		return nil, nil, err
	}
	// 平台 admin 全表；否则返回 admin_projects 里的项目（B1）。
	if !principal.IsPlatformAdmin {
		// API key / 服务账号无 admin_project 关联，仍返回空列表。
		if principal.ActorKind != shared.ActorKindAdmin {
			info := crud.BuildPaginationInfo(params, 0, false)
			return []projects.Project{}, &info, nil
		}
		adminID := principal.AdminLookupID()
		if adminID == "" {
			info := crud.BuildPaginationInfo(params, 0, false)
			return []projects.Project{}, &info, nil
		}
		if s.adminProjectRepo == nil {
			info := crud.BuildPaginationInfo(params, 0, false)
			return []projects.Project{}, &info, nil
		}
		ids, err := s.adminProjectRepo.ListProjectIDs(ctx, adminID)
		if err != nil {
			return nil, nil, status.Errorf(codes.Internal, "list admin projects: %v", err)
		}
		if len(ids) == 0 {
			info := crud.BuildPaginationInfo(params, 0, false)
			return []projects.Project{}, &info, nil
		}
		all, err := s.projectRepo.ListProjects(ctx)
		if err != nil {
			return nil, nil, err
		}
		idSet := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			idSet[id] = struct{}{}
		}
		filtered := make([]projects.Project, 0, len(ids))
		for _, p := range all {
			if _, ok := idSet[p.ID]; ok {
				filtered = append(filtered, p)
			}
		}
		start := params.Offset
		if start > len(filtered) {
			start = len(filtered)
		}
		end := start + int(params.PageSize)
		if end > len(filtered) {
			end = len(filtered)
		}
		page := filtered[start:end]
		hasMore := end < len(filtered)
		info := crud.BuildPaginationInfo(params, len(filtered), hasMore)
		return page, &info, nil
	}
	all, err := s.projectRepo.ListProjects(ctx)
	if err != nil {
		return nil, nil, err
	}
	start := params.Offset
	if start > len(all) {
		start = len(all)
	}
	end := start + int(params.PageSize)
	if end > len(all) {
		end = len(all)
	}
	page := all[start:end]
	hasMore := end < len(all)
	info := crud.BuildPaginationInfo(params, len(all), hasMore)
	return page, &info, nil
}

func (s *Projects) GetProject(ctx context.Context, id string) (*projects.Project, error) {
	principal, ok := contexts.Principal(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	// 非平台 admin 仅能访问其绑定项目（API key 的所属项目 / admin 的
	// X-Torchwood-Project 已由拦截器 ValidateAdminProjectAccess 校验授权），
	// 越权一律返回 NotFound，避免项目存在性探测（安全评审 M7）。
	if !principal.IsPlatformAdmin && (principal.ProjectID == "" || principal.ProjectID != id) {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	return s.projectRepo.GetProject(ctx, id)
}

type UpdateProjectCommand struct {
	ProjectID   string // 目标项目 id
	Name        *string
	Description *string
	// 无 Principal 字段：use-case 内从 contexts.Principal(ctx) 取
	// （与 CreateProject/GetProject/ListProjects 的仓库模式一致）。
}

func (s *Projects) UpdateProject(ctx context.Context, cmd UpdateProjectCommand) (*projects.Project, error) {
	if cmd.ProjectID == "" {
		return nil, status.Error(codes.InvalidArgument, "project id is required")
	}
	// "nothing to update" 前置检查放在取数之前（对齐 storage.UpdateFile 先例），
	// 避免"项目不存在 + 全空请求"返回 NotFound 的语义歧义。
	if cmd.Name == nil && cmd.Description == nil {
		return nil, status.Error(codes.InvalidArgument, "nothing to update")
	}
	principal, ok := contexts.Principal(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	// 越权保护：非平台 admin 仅能更新其绑定项目，越权返回 NotFound（防枚举，
	// 与 GetProject 语义一致）。
	if !principal.IsPlatformAdmin && (principal.ProjectID == "" || principal.ProjectID != cmd.ProjectID) {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	project, err := s.projectRepo.GetProject(ctx, cmd.ProjectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	if cmd.Name != nil {
		// 编辑场景对空白名拒绝（有意收紧，严格于 CreateProject 的空名回落默认 id）。
		name := strings.TrimSpace(*cmd.Name)
		if name == "" {
			return nil, status.Error(codes.InvalidArgument, "name is required")
		}
		if name != project.Name {
			// 撞名查重：name 唯一索引存在，不查重会命中 DB unique violation → 500。
			existing, err := s.projectRepo.GetProjectByName(ctx, name)
			if err != nil {
				return nil, err
			}
			if existing != nil {
				return nil, status.Error(codes.InvalidArgument, "project name already exists")
			}
		}
		project.Name = name
	}
	if cmd.Description != nil {
		if len(*cmd.Description) > maxProjectDescriptionLen {
			return nil, status.Error(codes.InvalidArgument, "description must be at most 512 characters")
		}
		project.Description = *cmd.Description
	}
	// repo 的 UpdateProject 是全列覆盖写，不置当前时间则 updated_at 永远停滞。
	project.UpdatedAt = time.Now()
	if err := s.projectRepo.UpdateProject(ctx, project); err != nil {
		return nil, err
	}
	return project, nil
}
