package groups

import (
	"context"
	"errors"
	"time"
)

var (
	ErrGroupIDRequired         = errors.New("group id is required")
	ErrMembershipIDRequired    = errors.New("membership id is required")
	ErrMembershipNotFound      = errors.New("membership not found")
	ErrMembershipNotPending    = errors.New("membership status is not pending")
	ErrMembershipAlreadyExists = errors.New("membership already exists")
	ErrInvalidUpdate           = errors.New("invalid group update")
)

// GroupRepository 把用户组持久化到项目 schema。
type GroupRepository interface {
	Insert(ctx context.Context, projectID string, group *Group) error
	GetByID(ctx context.Context, projectID, id string) (*Group, error)
	// LockByID 在当前事务内对组行取 FOR UPDATE 锁（组不存在时报错）。作为
	// "last owner 必须保留"这类组内不变量的串行化点：组内全部 owner 变更
	// （降级/删除）先锁组行再 count-act，消除 count 检查与变更之间的 TOCTOU。
	// 必须在事务内调用（autocommit 下锁立即释放，无串行化效果）。
	LockByID(ctx context.Context, projectID, id string) error
	// Update 只 SET 白名单列（name/permissions/prefs），禁止写 total。
	Update(ctx context.Context, projectID, id string, cols map[string]any) error
	Delete(ctx context.Context, projectID, id string) error
	List(ctx context.Context, projectID string) ([]*Group, error)
	// AddTotal 用 SQL GREATEST(total+delta, 0)，禁止读-改-写。
	AddTotal(ctx context.Context, projectID, groupID string, delta int64) error
	// RecountAccepted 把 total 重数为 accepted 成员数（DeleteUser 后）。
	RecountAccepted(ctx context.Context, projectID, groupID string) error
}

// MembershipRepository 把组成员持久化到项目 schema。
type MembershipRepository interface {
	// Insert：accepted 时与 AddTotal(+1) 同一 Tx。
	Insert(ctx context.Context, projectID string, m *Membership) error
	GetByID(ctx context.Context, projectID, id string) (*Membership, error)
	ListByGroup(ctx context.Context, projectID, groupID string) ([]*Membership, error)
	ListByUser(ctx context.Context, projectID, userID string) ([]*Membership, error)
	// Delete：accepted 时与 AddTotal(-1) 同一 Tx。guard 非空时在锁定目标行
	// 之后、删除之前于同一事务内调用（current 为已锁定的目标行投影），守卫
	// 报错则整个删除回滚——last-owner 守卫因此在与变更同事务的锁下判定；
	// 守卫拒绝语义由 app 层定义。nil = 无守卫。
	Delete(ctx context.Context, projectID, id string, guard func(ctx context.Context, current *Membership) error) error
	// Accept 在同一 Tx 内 CAS pending→accepted 再 AddTotal(+1)。
	Accept(ctx context.Context, projectID, id, userID string, joinedAt time.Time) error
	// Reject CAS pending→rejected，不加 total；禁止写回 pending。
	Reject(ctx context.Context, projectID, id string) error
	// UpdateRoles 同一 Tx：FOR UPDATE 当前行后回调；回调内 ListByGroup 走同一 ctx 才能做 last-owner 预检。
	UpdateRoles(ctx context.Context, projectID, id string, mutate func(ctx context.Context, current *Membership) ([]string, error)) error
}
