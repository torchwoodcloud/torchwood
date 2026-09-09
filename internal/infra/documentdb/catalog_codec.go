// 全局 catalog 的 JSONB 编解码（redesign §4.2 / C1，预决策 1）：attrs/indexes/
// permissions 以 JSON 列合一落库，default/size/array/options 全字段读写一致
// 以此为唯一契约源——四表模型 default_value 类契约断裂从结构上消灭。
package documentdb

import (
	"encoding/json"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
)

// attributeJSON 是 attrs 列的元素形态。Default 用 any 保留标量类型（bool/int/
// float/string），omitempty 仅在 nil 时省略——显式 false/0 是合法缺省值。
type attributeJSON struct {
	ID       string         `json:"id"`
	Key      string         `json:"key"`
	Type     string         `json:"type"`
	Size     int            `json:"size,omitempty"`
	Required bool           `json:"required,omitempty"`
	Array    bool           `json:"array,omitempty"`
	Default  any            `json:"default,omitempty"`
	Dims     int            `json:"dims,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
	// Status 是 schema 演进生命周期状态（B4）：active 缺省省略——存量行不带
	// 该字段，解码归一 active，零迁移。attrs 保持"每逻辑 key 恰一条目"不变量
	//（migrating/deprecated 均为该条目的状态迁移）；copy 迁移 swap 后的旧列
	// 物理名记录在迁移账本（catalog_migrations.old_physical），不进 attrs。
	Status string `json:"status,omitempty"`
}

type indexJSON struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Attributes []string `json:"attributes"`
	Orders     []string `json:"orders,omitempty"`
	// Metric 是 hnsw 索引的距离度量（会话 #10）：COSINE | L2 | INNER_PRODUCT；
	// 其余索引类型省略。
	Metric string `json:"metric,omitempty"`
	// Status 是在线 DDL 两阶段状态机的索引状态（B3）：active 缺省省略——
	// 建集合时的既有索引与 B3 前的存量行不带该字段，解码归一 active，零迁移。
	Status string `json:"status,omitempty"`
}

type permissionJSON struct {
	Type string `json:"type"`
	Role string `json:"role"`
}

func encodeAttributes(attrs []databases.Attribute) (string, error) {
	if len(attrs) == 0 {
		return "[]", nil
	}
	out := make([]attributeJSON, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, attributeJSON{
			ID:       a.ID,
			Key:      a.Key,
			Type:     a.Type,
			Size:     a.Size,
			Required: a.Required,
			Array:    a.Array,
			Default:  a.Default,
			Dims:     a.Dims,
			Options:  a.Options,
			Status:   a.Status,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", status.Error(codes.Internal, "encode catalog attributes")
	}
	return string(b), nil
}

func decodeAttributes(raw string) ([]databases.Attribute, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var in []attributeJSON
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, status.Error(codes.Internal, "decode catalog attributes")
	}
	out := make([]databases.Attribute, 0, len(in))
	for _, a := range in {
		out = append(out, databases.Attribute{
			ID:       a.ID,
			Key:      a.Key,
			Type:     a.Type,
			Size:     a.Size,
			Required: a.Required,
			Array:    a.Array,
			Default:  a.Default,
			Dims:     a.Dims,
			Options:  a.Options,
			Status:   a.Status,
		})
	}
	return out, nil
}

func encodeIndexes(idxs []databases.Index) (string, error) {
	if len(idxs) == 0 {
		return "[]", nil
	}
	out := make([]indexJSON, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, indexJSON{
			ID:         i.ID,
			Type:       i.Type,
			Attributes: i.Attributes,
			Orders:     i.Orders,
			Metric:     i.DistanceMetric,
			Status:     i.Status,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", status.Error(codes.Internal, "encode catalog indexes")
	}
	return string(b), nil
}

func decodeIndexes(raw string) ([]databases.Index, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var in []indexJSON
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, status.Error(codes.Internal, "decode catalog indexes")
	}
	out := make([]databases.Index, 0, len(in))
	for _, i := range in {
		out = append(out, databases.Index{
			ID:             i.ID,
			Type:           i.Type,
			Attributes:     i.Attributes,
			Orders:         i.Orders,
			DistanceMetric: i.Metric,
			Status:         i.Status,
		})
	}
	return out, nil
}

func encodePermissions(perms []databases.Permission) (string, error) {
	if len(perms) == 0 {
		return "[]", nil
	}
	out := make([]permissionJSON, 0, len(perms))
	for _, p := range perms {
		out = append(out, permissionJSON{Type: p.Type, Role: p.Role})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", status.Error(codes.Internal, "encode catalog permissions")
	}
	return string(b), nil
}

func decodePermissions(raw string) ([]databases.Permission, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var in []permissionJSON
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, status.Error(codes.Internal, "decode catalog permissions")
	}
	out := make([]databases.Permission, 0, len(in))
	for _, p := range in {
		out = append(out, databases.Permission{Type: p.Type, Role: p.Role})
	}
	return out, nil
}
