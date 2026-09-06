package cmd

import (
	"encoding/json"
	"flag"
	"testing"

	"github.com/lynx-go/commands"
	"github.com/stretchr/testify/require"
)

// newPresenceVerb 构造带指定旗标的测试动词：SetFlags 后在动词捕获的
// FlagSet 上 Set，模拟"显式传入"的 presence（与生产分发同构，供
// setChanged/verb.changed 判断）。
func newPresenceVerb(t *testing.T, decl func(fs *flag.FlagSet), set map[string]string) *verb {
	t.Helper()
	v := newVerb(nil, "test", "test", "test", decl,
		func(v *verb, env *commands.Environment, args []string) error { return nil })
	v.SetFlags(flag.NewFlagSet("test", flag.ContinueOnError))
	for k, val := range set {
		require.NoError(t, v.fs.Set(k, val))
	}
	return v
}

func TestBuildCreateDatabaseReq(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		label   string
		wantErr string
	}{
		{name: "缺 id", wantErr: "--id 必填"},
		{name: "缺 name", id: "app", wantErr: "--name 必填"},
		{name: "全字段", id: "app", label: "应用库", wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateDatabaseReq(tt.id, tt.label)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.id, req["id"])
			require.Equal(t, tt.label, req["name"])
		})
	}
}

func TestBuildCreateCollectionReq(t *testing.T) {
	tests := []struct {
		name           string
		databaseID     string
		id             string
		collectionName string
		permissions    string
		docSec         string
		wantErr        string
	}{
		{name: "缺 database-id", wantErr: "缺少 database-id"},
		{name: "缺 id", databaseID: "app", wantErr: "--id 必填"},
		{name: "缺 name", databaseID: "app", id: "notes", wantErr: "--name 必填"},
		{name: "未传 document-security", databaseID: "app", id: "notes", collectionName: "笔记", wantErr: ""},
		{name: "显式 true", databaseID: "app", id: "notes", collectionName: "笔记",
			docSec: "true", wantErr: ""},
		{name: "显式 false", databaseID: "app", id: "notes", collectionName: "笔记",
			docSec: "false", wantErr: ""},
		{name: "权限与安全", databaseID: "app", id: "notes", collectionName: "笔记",
			permissions: `["read(\"users\")"]`, docSec: "true", wantErr: ""},
		{name: "permissions 非法 JSON", databaseID: "app", id: "notes", collectionName: "笔记",
			permissions: `[not-json`, wantErr: "--permissions 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := map[string]string{}
			if tt.docSec != "" {
				set["document-security"] = tt.docSec
			}
			v := newPresenceVerb(t, func(fs *flag.FlagSet) {
				fs.Bool("document-security", false, "")
				fs.Bool("disabled", false, "")
			}, set)
			req, err := buildCreateCollectionReq(v, tt.databaseID, tt.id, tt.collectionName, tt.permissions, tt.docSec == "true")
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.databaseID, req["databaseId"])
			require.Equal(t, tt.id, req["id"])
			require.Equal(t, tt.collectionName, req["name"])
			if tt.docSec == "" {
				_, ok := req["documentSecurity"]
				require.False(t, ok, "未显式传 --document-security 不应设置键: %v", req)
			} else {
				require.Equal(t, tt.docSec == "true", req["documentSecurity"])
			}
			if tt.permissions != "" {
				require.Equal(t, []string{`read("users")`}, req["permissions"])
			} else {
				_, ok := req["permissions"]
				require.False(t, ok)
			}
		})
	}
}

func TestBuildUpdateCollectionReq(t *testing.T) {
	tests := []struct {
		name           string
		databaseID     string
		collectionID   string
		collectionName string
		permissions    string
		docSec         string
		disabled       string
		wantErr        string
	}{
		{name: "缺 database-id", wantErr: "缺少 database-id"},
		{name: "缺 collection-id", databaseID: "app", wantErr: "缺少 collection-id"},
		{name: "仅 id", databaseID: "app", collectionID: "c1", wantErr: ""},
		{name: "改名", databaseID: "app", collectionID: "c1", collectionName: "新名", wantErr: ""},
		{name: "权限替换", databaseID: "app", collectionID: "c1",
			permissions: `["read(\"all\")"]`, wantErr: ""},
		{name: "optional bool", databaseID: "app", collectionID: "c1",
			docSec: "false", disabled: "true", wantErr: ""},
		{name: "permissions 非法", databaseID: "app", collectionID: "c1",
			permissions: `nope`, wantErr: "--permissions 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := map[string]string{}
			if tt.docSec != "" {
				set["document-security"] = tt.docSec
			}
			if tt.disabled != "" {
				set["disabled"] = tt.disabled
			}
			v := newPresenceVerb(t, func(fs *flag.FlagSet) {
				fs.Bool("document-security", false, "")
				fs.Bool("disabled", false, "")
			}, set)
			req, err := buildUpdateCollectionReq(v, tt.databaseID, tt.collectionID, tt.collectionName,
				tt.permissions, tt.docSec == "true", tt.disabled == "true")
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.databaseID, req["databaseId"])
			require.Equal(t, tt.collectionID, req["collectionId"])
			if tt.collectionName == "" {
				_, ok := req["name"]
				require.False(t, ok, "未传 --name 不应设置 name: %v", req)
			} else {
				require.Equal(t, tt.collectionName, req["name"])
			}
			if tt.permissions == "" {
				_, ok := req["permissions"]
				require.False(t, ok)
			} else {
				require.Equal(t, map[string]any{"values": []string{`read("all")`}}, req["permissions"])
			}
			if tt.docSec == "" {
				_, ok := req["documentSecurity"]
				require.False(t, ok, "未显式传 --document-security 不应设置键: %v", req)
			} else {
				require.Equal(t, tt.docSec == "true", req["documentSecurity"])
			}
			if tt.disabled == "" {
				_, ok := req["disabled"]
				require.False(t, ok, "未显式传 --disabled 不应设置键: %v", req)
			} else {
				require.Equal(t, tt.disabled == "true", req["disabled"])
			}
		})
	}
}

func TestBuildCreateAttributeReq(t *testing.T) {
	tests := []struct {
		name         string
		databaseID   string
		collectionID string
		key          string
		typ          string
		size         int
		required     bool
		array        bool
		defaultValue string
		wantErr      string
	}{
		{name: "缺 key", databaseID: "app", collectionID: "c1", typ: "string", wantErr: "--key 必填"},
		{name: "缺 type", databaseID: "app", collectionID: "c1", key: "title", wantErr: "--type 必填"},
		{name: "最小字段", databaseID: "app", collectionID: "c1", key: "title", typ: "string", wantErr: ""},
		{name: "全字段", databaseID: "app", collectionID: "c1", key: "tags", typ: "string",
			size: 64, required: true, array: true, defaultValue: `["a"]`, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateAttributeReq(tt.databaseID, tt.collectionID, tt.key, tt.typ,
				tt.size, tt.required, tt.array, tt.defaultValue)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.databaseID, req["databaseId"])
			require.Equal(t, tt.collectionID, req["collectionId"])
			require.Equal(t, tt.key, req["key"])
			require.Equal(t, tt.typ, req["type"])
			require.Equal(t, tt.size, req["size"])
			require.Equal(t, tt.required, req["required"])
			require.Equal(t, tt.array, req["array"])
			if tt.defaultValue == "" {
				_, ok := req["defaultValue"]
				require.False(t, ok)
			} else {
				require.Equal(t, tt.defaultValue, req["defaultValue"])
			}
		})
	}
}

func TestBuildCreateIndexReq(t *testing.T) {
	tests := []struct {
		name         string
		databaseID   string
		collectionID string
		id           string
		typ          string
		attributes   string
		orders       string
		wantErr      string
	}{
		{name: "缺 id", databaseID: "app", collectionID: "c1", typ: "key", attributes: `["title"]`, wantErr: "--id 必填"},
		{name: "缺 type", databaseID: "app", collectionID: "c1", id: "ix1", attributes: `["title"]`, wantErr: "--type 必填"},
		{name: "缺 attributes", databaseID: "app", collectionID: "c1", id: "ix1", typ: "key", wantErr: "--attributes 必填"},
		{name: "最小字段", databaseID: "app", collectionID: "c1", id: "ix1", typ: "key",
			attributes: `["title"]`, wantErr: ""},
		{name: "全字段", databaseID: "app", collectionID: "c1", id: "ix1", typ: "unique",
			attributes: `["title","author"]`, orders: `["asc","desc"]`, wantErr: ""},
		{name: "orders 非法", databaseID: "app", collectionID: "c1", id: "ix1", typ: "key",
			attributes: `["title"]`, orders: `oops`, wantErr: "--orders 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateIndexReq(tt.databaseID, tt.collectionID, tt.id, tt.typ, tt.attributes, tt.orders)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.databaseID, req["databaseId"])
			require.Equal(t, tt.collectionID, req["collectionId"])
			require.Equal(t, tt.id, req["id"])
			require.Equal(t, tt.typ, req["type"])
			attrs, err := jsonStringList(tt.attributes, "--attributes")
			require.NoError(t, err)
			require.Equal(t, attrs, req["attributes"])
			if tt.orders == "" {
				_, ok := req["orders"]
				require.False(t, ok)
			} else {
				ords, err := jsonStringList(tt.orders, "--orders")
				require.NoError(t, err)
				require.Equal(t, ords, req["orders"])
			}
		})
	}
}

func TestBuildListDocumentsReq(t *testing.T) {
	tests := []struct {
		name         string
		databaseID   string
		collectionID string
		queries      string
		pageSize     int
		pageToken    string
		wantErr      string
	}{
		{name: "缺 database-id", collectionID: "c1", wantErr: "缺少 database-id"},
		{name: "缺 collection-id", databaseID: "app", wantErr: "缺少 collection-id"},
		{name: "成功路径", databaseID: "app", collectionID: "c1",
			queries: `["equal(\"title\",\"hi\")"]`, pageSize: 10, pageToken: "tok", wantErr: ""},
		{name: "queries 非法 JSON", databaseID: "app", collectionID: "c1",
			queries: `oops`, wantErr: "--queries 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildListDocumentsReq(tt.databaseID, tt.collectionID, tt.queries, tt.pageSize, tt.pageToken)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.databaseID, req["databaseId"])
			require.Equal(t, tt.collectionID, req["collectionId"])
			if tt.queries == "" {
				_, ok := req["query"]
				require.False(t, ok)
			} else {
				// C7 单 AST：DSL 串在客户端解析为 AST wire JSON（不再透传字符串）。
				wire, ok := req["query"].(map[string]any)
				require.True(t, ok)
				eq, ok := wire["filter"].(map[string]any)["eq"].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "title", eq["attribute"])
				require.Equal(t, []string{"hi"}, eq["values"])
			}
			if tt.pageSize > 0 {
				require.Equal(t, tt.pageSize, req["pageSize"])
			} else {
				_, ok := req["pageSize"]
				require.False(t, ok)
			}
			if tt.pageToken == "" {
				_, ok := req["pageToken"]
				require.False(t, ok)
			} else {
				require.Equal(t, tt.pageToken, req["pageToken"])
			}
		})
	}
}

func TestBuildCreateDocumentReq(t *testing.T) {
	tests := []struct {
		name         string
		databaseID   string
		collectionID string
		documentID   string
		data         string
		permissions  string
		wantErr      string
	}{
		{name: "缺 data", databaseID: "app", collectionID: "c1", wantErr: "--data 必填"},
		{name: "data 非对象", databaseID: "app", collectionID: "c1", data: `[1,2]`, wantErr: "--data 解析失败"},
		{name: "data 非法 JSON", databaseID: "app", collectionID: "c1", data: `{`, wantErr: "--data 解析失败"},
		{name: "最小字段", databaseID: "app", collectionID: "c1", data: `{"title":"hi"}`, wantErr: ""},
		{name: "指定 id 与权限", databaseID: "app", collectionID: "c1", documentID: "doc1",
			data: `{"title":"hi"}`, permissions: `["read(\"all\")"]`, wantErr: ""},
		{name: "permissions 非法", databaseID: "app", collectionID: "c1", data: `{"title":"hi"}`,
			permissions: `x`, wantErr: "--permissions 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateDocumentReq(tt.databaseID, tt.collectionID, tt.documentID, tt.data, tt.permissions)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.databaseID, req["databaseId"])
			require.Equal(t, tt.collectionID, req["collectionId"])
			if tt.documentID == "" {
				_, ok := req["documentId"]
				require.False(t, ok)
			} else {
				require.Equal(t, tt.documentID, req["documentId"])
			}
			require.Equal(t, map[string]any{"title": "hi"}, req["data"])
			if tt.permissions == "" {
				_, ok := req["permissions"]
				require.False(t, ok)
			} else {
				require.Equal(t, []string{`read("all")`}, req["permissions"])
			}
		})
	}
}

func TestBuildUpdateDocumentReq(t *testing.T) {
	tests := []struct {
		name         string
		databaseID   string
		collectionID string
		documentID   string
		data         string
		permissions  string
		increment    string
		version      int64
		wantErr      string
	}{
		{name: "缺 version", databaseID: "app", collectionID: "c1", documentID: "d1",
			data: `{"title":"new"}`, version: 0, wantErr: "--version 必填"},
		{name: "无可更新字段", databaseID: "app", collectionID: "c1", documentID: "d1", version: 1,
			wantErr: "--data/--permissions/--increment 至少提供一个"},
		{name: "仅 data", databaseID: "app", collectionID: "c1", documentID: "d1", version: 1,
			data: `{"title":"new"}`, wantErr: ""},
		{name: "仅 increment", databaseID: "app", collectionID: "c1", documentID: "d1", version: 2,
			increment: `{"views":1}`, wantErr: ""},
		{name: "全字段", databaseID: "app", collectionID: "c1", documentID: "d1", version: 3,
			data: `{"title":"new"}`, permissions: `["read(\"all\")"]`, increment: `{"views":1}`, wantErr: ""},
		{name: "increment 非法", databaseID: "app", collectionID: "c1", documentID: "d1", version: 1,
			increment: `{"views":"x"}`, wantErr: "--increment 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpdateDocumentReq(tt.databaseID, tt.collectionID, tt.documentID,
				tt.data, tt.permissions, tt.increment, tt.version)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.databaseID, req["databaseId"])
			require.Equal(t, tt.collectionID, req["collectionId"])
			require.Equal(t, tt.documentID, req["documentId"])
			require.Equal(t, tt.version, req["version"])
			if tt.data == "" {
				_, ok := req["data"]
				require.False(t, ok)
			} else {
				require.Equal(t, map[string]any{"title": "new"}, req["data"])
			}
			if tt.permissions == "" {
				_, ok := req["permissions"]
				require.False(t, ok)
			} else {
				require.Equal(t, []string{`read("all")`}, req["permissions"])
			}
			if tt.increment == "" {
				_, ok := req["increment"]
				require.False(t, ok)
			} else {
				incr, ok := req["increment"].(map[string]json.Number)
				require.True(t, ok)
				require.Equal(t, json.Number("1"), incr["views"])
			}
		})
	}
}

// TestDocumentsUpdateIncrementPrecision 回归 int64 精度：>2^53 的增量经
// json.Marshal 后必须是原始整数，不能变 1.234...e+18。
func TestDocumentsUpdateIncrementPrecision(t *testing.T) {
	req, err := buildUpdateDocumentReq("db1", "col1", "doc1", "", "", `{"big": 1234567890123456789}`, 1)
	require.NoError(t, err)
	b, err := json.Marshal(req)
	require.NoError(t, err)
	require.Contains(t, string(b), "1234567890123456789") // 不是 1.23...e+18
}

// TestBuildDeleteDocumentReq：documents delete 的 --version 校验与
// buildUpdateDocumentReq 对齐——缺 version 或 ≤0 一律拒绝。
func TestBuildDeleteDocumentReq(t *testing.T) {
	tests := []struct {
		name         string
		databaseID   string
		collectionID string
		documentID   string
		version      int64
		wantErr      string
	}{
		{name: "缺 database-id", collectionID: "c1", documentID: "d1", version: 1,
			wantErr: "缺少 database-id"},
		{name: "缺 collection-id", databaseID: "app", documentID: "d1", version: 1,
			wantErr: "缺少 collection-id"},
		{name: "缺 document-id", databaseID: "app", collectionID: "c1", version: 1,
			wantErr: "缺少 document-id"},
		{name: "缺 version", databaseID: "app", collectionID: "c1", documentID: "d1", version: 0,
			wantErr: "--version 必填"},
		{name: "version 为负", databaseID: "app", collectionID: "c1", documentID: "d1", version: -1,
			wantErr: "--version 必填"},
		{name: "全字段", databaseID: "app", collectionID: "c1", documentID: "d1", version: 3, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildDeleteDocumentReq(tt.databaseID, tt.collectionID, tt.documentID, tt.version)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.databaseID, req["databaseId"])
			require.Equal(t, tt.collectionID, req["collectionId"])
			require.Equal(t, tt.documentID, req["documentId"])
			require.Equal(t, tt.version, req["version"])
		})
	}
}

func TestBuildUpsertDocumentReq(t *testing.T) {
	tests := []struct {
		name            string
		databaseID      string
		collectionID    string
		documentID      string
		data            string
		permissions     string
		conflictColumns string
		wantErr         string
	}{
		{name: "缺 data", databaseID: "app", collectionID: "c1", documentID: "d1",
			conflictColumns: `["email"]`, wantErr: "--data 必填"},
		{name: "缺 conflict-columns", databaseID: "app", collectionID: "c1", documentID: "d1",
			data: `{"email":"a@b.c"}`, wantErr: "--conflict-columns 必填"},
		{name: "全字段", databaseID: "app", collectionID: "c1", documentID: "d1",
			data: `{"email":"a@b.c"}`, permissions: `["read(\"all\")"]`,
			conflictColumns: `["email"]`, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpsertDocumentReq(tt.databaseID, tt.collectionID, tt.documentID,
				tt.data, tt.permissions, tt.conflictColumns)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.databaseID, req["databaseId"])
			require.Equal(t, tt.collectionID, req["collectionId"])
			require.Equal(t, tt.documentID, req["documentId"])
			require.Equal(t, []string{"email"}, req["conflictColumns"])
			require.Equal(t, map[string]any{"email": "a@b.c"}, req["data"])
			if tt.permissions != "" {
				require.Equal(t, []string{`read("all")`}, req["permissions"])
			}
		})
	}
}

func TestBuildBulkDocumentsReq(t *testing.T) {
	tests := []struct {
		name         string
		databaseID   string
		collectionID string
		documentIDs  string
		data         string
		permissions  string
		bulkDelete   bool
		wantErr      string
	}{
		{name: "缺 document-ids", databaseID: "app", collectionID: "c1", data: `{}`, wantErr: "--document-ids 必填"},
		{name: "bulk-update 缺 data", databaseID: "app", collectionID: "c1",
			documentIDs: `["d1","d2"]`, wantErr: "--data 必填"},
		{name: "bulk-update 全字段", databaseID: "app", collectionID: "c1",
			documentIDs: `["d1","d2"]`, data: `{"status":"x"}`, permissions: `["read(\"all\")"]`, wantErr: ""},
		{name: "bulk-delete", databaseID: "app", collectionID: "c1",
			documentIDs: `["d1","d2"]`, bulkDelete: true, wantErr: ""},
		{name: "document-ids 非法", databaseID: "app", collectionID: "c1",
			documentIDs: `d1`, data: `{}`, wantErr: "--document-ids 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.bulkDelete {
				req, err := buildBulkDeleteDocumentsReq(tt.databaseID, tt.collectionID, tt.documentIDs)
				if tt.wantErr != "" {
					require.Error(t, err)
					require.Contains(t, err.Error(), tt.wantErr)
					return
				}
				require.NoError(t, err)
				require.Equal(t, []string{"d1", "d2"}, req["documentIds"])
				return
			}
			req, err := buildBulkUpdateDocumentsReq(tt.databaseID, tt.collectionID, tt.documentIDs, tt.data, tt.permissions)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.databaseID, req["databaseId"])
			require.Equal(t, tt.collectionID, req["collectionId"])
			require.Equal(t, []string{"d1", "d2"}, req["documentIds"])
			require.Equal(t, map[string]any{"status": "x"}, req["data"])
			if tt.permissions != "" {
				require.Equal(t, []string{`read("all")`}, req["permissions"])
			}
		})
	}
}
