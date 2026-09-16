package model

import (
	"time"

	"github.com/uptrace/bun"
)

// RuntimeVars 四表（db/migrations/000011，docs/design/runtime-vars.md §2.2，
// D1：放 public 控制面）。value/vars JSONB 列用 string 承载 JSON 文本——
// bun/pgdriver 对 jsonb 列的 string 参数按文本协议发送由 PG 隐式转型，
// 扫描侧 []byte→string 直通（catalog 三列同模式）；value 可能是标量，
// 禁止用 map 承载。updated_at 无触发器，由应用写。

// RuntimeVarSet 是变量集合行；epoch 为创建时随机 8 字节 hex（D2 etag 防碰撞）。
type RuntimeVarSet struct {
	bun.BaseModel `bun:"table:runtime_var_sets,alias:rvs"`

	ProjectID   string    `bun:"project_id,pk"`
	VarSetID    string    `bun:"var_set_id,pk"`
	Visibility  string    `bun:"visibility,notnull,default:'public'"`
	Epoch       string    `bun:"epoch,notnull"`
	Description string    `bun:"description,notnull,default:''"`
	CreatedAt   time.Time `bun:"created_at,notnull"`
	UpdatedAt   time.Time `bun:"updated_at,notnull"`
}

// RuntimeVar 是单个类型化变量行；value_type 是类型锚点列（D3）。
type RuntimeVar struct {
	bun.BaseModel `bun:"table:runtime_vars,alias:rv"`

	ProjectID   string    `bun:"project_id,pk"`
	VarSetID    string    `bun:"var_set_id,pk"`
	Key         string    `bun:"key,pk"`
	ValueType   string    `bun:"value_type,notnull"`
	Value       string    `bun:"value,notnull"`
	Description string    `bun:"description,notnull,default:''"`
	CreatedAt   time.Time `bun:"created_at,notnull"`
	UpdatedAt   time.Time `bun:"updated_at,notnull"`
}

// RuntimeVarHead 是集合级版本头：revision 严格单调，写事务先 FOR UPDATE 锁
// 此行串行化同集合并发写（§2.5）；行不存在视同 revision 0。
type RuntimeVarHead struct {
	bun.BaseModel `bun:"table:runtime_var_heads,alias:rvh"`

	ProjectID string    `bun:"project_id,pk"`
	VarSetID  string    `bun:"var_set_id,pk"`
	Revision  int64     `bun:"revision,notnull,default:0"`
	UpdatedAt time.Time `bun:"updated_at,notnull"`
}

// RuntimeVarVersion 是版本链一版：vars 为全列全量快照 JSON 文本
// {key: {t, v, d, c}}（D11）；revision = 快照时点的 heads.revision。
type RuntimeVarVersion struct {
	bun.BaseModel `bun:"table:runtime_var_versions,alias:rvv"`

	ProjectID string    `bun:"project_id,pk"`
	VarSetID  string    `bun:"var_set_id,pk"`
	Revision  int64     `bun:"revision,pk"`
	Vars      string    `bun:"vars,notnull"`
	Action    string    `bun:"action,notnull"`
	Summary   string    `bun:"summary,notnull,default:''"`
	Actor     string    `bun:"actor,notnull"`
	CreatedAt time.Time `bun:"created_at,notnull"`
}
