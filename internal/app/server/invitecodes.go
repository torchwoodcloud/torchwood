package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"time"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 邀请码约束（T-03）：次数 1..10000，一次性默认；明文 code 带 twi_ 前缀
// （128-bit 随机，无枚举面）。
const (
	inviteCodeMaxUsesLimit = 10000
	inviteCodePrefix       = "twi_"
)

type InviteCodes struct {
	repo projects.InviteCodeRepository
}

func NewInviteCodes(repo projects.InviteCodeRepository) *InviteCodes {
	return &InviteCodes{repo: repo}
}

type CreateInviteCodeCommand struct {
	ProjectID string
	// MaxUses nil = 默认 1（一次性）。
	MaxUses  *int32
	ExpireAt *time.Time
}

func (s *InviteCodes) Create(ctx context.Context, cmd CreateInviteCodeCommand) (*projects.InviteCode, error) {
	if err := appshared.RequirePlatformPrincipal(ctx); err != nil {
		return nil, err
	}
	if cmd.ProjectID == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	maxUses := 1
	if cmd.MaxUses != nil {
		maxUses = int(*cmd.MaxUses)
		if maxUses < 1 || maxUses > inviteCodeMaxUsesLimit {
			return nil, status.Errorf(codes.InvalidArgument, "max_uses must be between 1 and %d", inviteCodeMaxUsesLimit)
		}
	}
	creator := ""
	if p, ok := contexts.Principal(ctx); ok && p != nil {
		creator = p.AdminLookupID()
	}
	code := &projects.InviteCode{
		ID:        idgen.UUID().String(),
		ProjectID: cmd.ProjectID,
		Code:      inviteCodePrefix + newInviteCodeSecret(),
		MaxUses:   maxUses,
		ExpireAt:  cmd.ExpireAt,
		CreatedBy: creator,
		CreatedAt: time.Now(),
	}
	if err := s.repo.CreateInviteCode(ctx, code); err != nil {
		return nil, err
	}
	return code, nil
}

func (s *InviteCodes) List(ctx context.Context, projectID string) ([]projects.InviteCode, error) {
	if err := appshared.RequirePlatformPrincipal(ctx); err != nil {
		return nil, err
	}
	if projectID == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	return s.repo.ListInviteCodes(ctx, projectID, 100)
}

func (s *InviteCodes) Delete(ctx context.Context, projectID, id string) error {
	if err := appshared.RequirePlatformPrincipal(ctx); err != nil {
		return err
	}
	if projectID == "" || id == "" {
		return status.Error(codes.InvalidArgument, "project_id and id are required")
	}
	ok, err := s.repo.RevokeInviteCode(ctx, projectID, id)
	if err != nil {
		return err
	}
	if !ok {
		return status.Error(codes.NotFound, "invite code not found")
	}
	return nil
}

// newInviteCodeSecret 生成 128-bit 随机 URL-safe 串（16 字节 → ~22 字符）。
func newInviteCodeSecret() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 不可用属进程级故障，立即崩溃（与 idgen 同立场）
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
