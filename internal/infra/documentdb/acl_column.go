// _acl 列编解码（redesign §4.3 权限层，阶段③包 A）：权限集 ↔ text[]（元素
// "type:role"，与 _perms 行模型的 (_type,_permission) 二元组同构——只换存储，
// 语义模型不动）。读取侧统一走 JSON 投影（to_jsonb(d.*) / array_to_json(_acl)），
// pgdriver 不支持 text[] 原生扫描；写入侧走 pgTextArray 字面量 + ?::text[] 绑定。
package documentdb

import (
	"encoding/json"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
)

// aclEntries 把权限集编码为 _acl 数组元素（"type:role"，FormatPermissionString）。
func aclEntries(perms []databases.Permission) []string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, databases.FormatPermissionString(p))
	}
	return out
}

// aclParam 生成 _acl 写入参数（pgTextArray 字面量，配 "?::text[]" 绑定）。
func aclParam(perms []databases.Permission) string {
	return pgTextArray(aclEntries(perms))
}

// aclMatchKeys 生成 permType 在角色集上的 ACE 匹配键：read 只匹配 "read:<role>"；
// create/update/delete 同时匹配 "write:<role>"（matchTypes 的 write 展开，
// AllowsDocumentAccess 同源语义）。
func aclMatchKeys(permType string, roles []string) []string {
	out := make([]string, 0, len(roles)*2)
	for _, r := range roles {
		out = append(out, permType+":"+r)
		if permType != "read" {
			out = append(out, "write:"+r)
		}
	}
	return out
}

// parseACLStrings 解析 _acl JSON 投影中的 "type:role" 元素（首个冒号分隔——
// 角色可含冒号，如 user:<id>）。非法元素防御性跳过（写入路径保证格式）。
func parseACLStrings(items []string) []databases.Permission {
	perms := make([]databases.Permission, 0, len(items))
	for _, s := range items {
		typ, role, ok := strings.Cut(s, ":")
		if !ok || typ == "" || role == "" {
			continue
		}
		perms = append(perms, databases.Permission{Type: typ, Role: role})
	}
	return perms
}

// parseACLJSON 解析 text[] 的 JSON 投影（点查 getDocumentACL 的
// array_to_json(_acl) 返回）；NULL / 空数组 → (nil, nil)。
func parseACLJSON(raw []byte) ([]databases.Permission, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	return parseACLStrings(items), nil
}
