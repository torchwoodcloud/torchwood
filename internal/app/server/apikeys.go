package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// API key scope 上限（B2）：每项 ≤64 字符、最多 32 项。
const (
	maxAPIKeyScopes      = 32
	maxAPIKeyScopeLength = 64
)

type APIKeys struct {
	repo projects.APIKeyRepository
	// vocab 是合法 scope 词表（从 PolicySet 派生，组合根注入）——key 创建
	// 校验与 proto 声明单一事实源对齐。
	vocab *domainauth.ScopeVocabulary
}

func NewAPIKeys(repo projects.APIKeyRepository, vocab *domainauth.ScopeVocabulary) *APIKeys {
	return &APIKeys{repo: repo, vocab: vocab}
}

type CreateAPIKeyCommand struct {
	ProjectID string
	Name      string
	Scopes    []string
	ExpireAt  *time.Time
}

// Create 创建 API Key；平台级写操作，仅限平台 admin（安全评审 M7）。
// 引导（console setup）等系统路径请调用 CreateInternal，调用方负责授权。
func (a *APIKeys) Create(ctx context.Context, cmd CreateAPIKeyCommand) (*projects.APIKey, string, error) {
	if err := appshared.RequirePlatformPrincipal(ctx); err != nil {
		return nil, "", err
	}
	// G2-5（R06-P3）：cmd.Scopes 不得超出调用者自身权限——当前入口仅平台
	// admin 可达（恒放行），该校验是纵深防御：未来若放宽为受限 admin 可
	// 创建 key，受限主体不得铸出超出自身 scope 的 key。
	if err := ensureScopesWithinCaller(ctx, cmd.Scopes); err != nil {
		return nil, "", err
	}
	return a.CreateInternal(ctx, cmd)
}

// ensureScopesWithinCaller 校验 scopes 全部包含于调用者 principal.Permissions；
// 平台 admin 放行（其会话权限不按 scope 建模）。匿名/无权限主体拒绝。
func ensureScopesWithinCaller(ctx context.Context, scopes []string) error {
	principal, ok := contexts.Principal(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if principal.IsPlatformAdmin {
		return nil
	}
	for _, s := range scopes {
		if !principal.HasScope(s) {
			return status.Errorf(codes.PermissionDenied, "scope %q exceeds caller permissions", s)
		}
	}
	return nil
}

// CreateInternal 执行 API Key 创建（不做 principal 检查）。
func (a *APIKeys) CreateInternal(ctx context.Context, cmd CreateAPIKeyCommand) (*projects.APIKey, string, error) {
	if cmd.Name == "" {
		return nil, "", status.Error(codes.InvalidArgument, "name is required")
	}
	if len(cmd.Scopes) == 0 {
		return nil, "", status.Error(codes.InvalidArgument, "scopes is required (use \"*\" for all scopes)")
	}
	if len(cmd.Scopes) > maxAPIKeyScopes {
		return nil, "", status.Errorf(codes.InvalidArgument, "scopes exceeds maximum of %d", maxAPIKeyScopes)
	}
	for _, s := range cmd.Scopes {
		if len(s) > maxAPIKeyScopeLength || !a.vocab.Valid(s) {
			return nil, "", status.Errorf(codes.InvalidArgument, "invalid scope %q (allowed: * | all | <resource> | <resource>.read | <resource>.write)", s)
		}
	}
	id := idgen.UUID().String()
	secret := idgen.UUID().String() + idgen.UUID().String()
	hash := sha256.Sum256([]byte(secret))
	key := &projects.APIKey{
		ID:         id,
		ProjectID:  cmd.ProjectID,
		Name:       cmd.Name,
		SecretHash: hex.EncodeToString(hash[:]),
		Scopes:     cmd.Scopes,
		ExpireAt:   cmd.ExpireAt,
		Enabled:    true,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := a.repo.CreateAPIKey(ctx, key); err != nil {
		return nil, "", fmt.Errorf("create api key: %w", err)
	}
	return key, secret, nil
}

func (a *APIKeys) List(ctx context.Context, projectID string) ([]projects.APIKey, error) {
	return a.repo.ListAPIKeys(ctx, projectID)
}

func (a *APIKeys) Get(ctx context.Context, projectID, id string) (*projects.APIKey, error) {
	key, err := a.repo.GetAPIKey(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, status.Error(codes.NotFound, "api key not found")
	}
	return key, nil
}

func (a *APIKeys) Delete(ctx context.Context, projectID, id string) error {
	// 纵深防御（Round3 H1-3）：与 Create 对齐，平台级写操作仅限平台 admin；
	// 即使绕过拦截器，viewer/member/API key 也不能删除 API Key。
	if err := appshared.RequirePlatformPrincipal(ctx); err != nil {
		return err
	}
	key, err := a.repo.GetAPIKey(ctx, projectID, id)
	if err != nil {
		return err
	}
	if key == nil {
		return status.Error(codes.NotFound, "api key not found")
	}
	return a.repo.DeleteAPIKey(ctx, projectID, id)
}
