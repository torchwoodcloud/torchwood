package cmd

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"

	"github.com/torchwooddev/torchwood/pkg/query"
)

const (
	methodDBCreateDatabase      = "/torchwood.server.v1.DatabasesService/CreateDatabase"
	methodDBListDatabases       = "/torchwood.server.v1.DatabasesService/ListDatabases"
	methodDBGetDatabase         = "/torchwood.server.v1.DatabasesService/GetDatabase"
	methodDBDeleteDatabase      = "/torchwood.server.v1.DatabasesService/DeleteDatabase"
	methodDBCreateCollection    = "/torchwood.server.v1.DatabasesService/CreateCollection"
	methodDBListCollections     = "/torchwood.server.v1.DatabasesService/ListCollections"
	methodDBGetCollection       = "/torchwood.server.v1.DatabasesService/GetCollection"
	methodDBUpdateCollection    = "/torchwood.server.v1.DatabasesService/UpdateCollection"
	methodDBDeleteCollection    = "/torchwood.server.v1.DatabasesService/DeleteCollection"
	methodDBCreateAttribute     = "/torchwood.server.v1.DatabasesService/CreateAttribute"
	methodDBDeleteAttribute     = "/torchwood.server.v1.DatabasesService/DeleteAttribute"
	methodDBCreateIndex         = "/torchwood.server.v1.DatabasesService/CreateIndex"
	methodDBDeleteIndex         = "/torchwood.server.v1.DatabasesService/DeleteIndex"
	methodDBCreateDocument      = "/torchwood.server.v1.DatabasesService/CreateDocument"
	methodDBListDocuments       = "/torchwood.server.v1.DatabasesService/ListDocuments"
	methodDBGetDocument         = "/torchwood.server.v1.DatabasesService/GetDocument"
	methodDBUpdateDocument      = "/torchwood.server.v1.DatabasesService/UpdateDocument"
	methodDBUpsertDocument      = "/torchwood.server.v1.DatabasesService/UpsertDocument"
	methodDBDeleteDocument      = "/torchwood.server.v1.DatabasesService/DeleteDocument"
	methodDBCountDocuments      = "/torchwood.server.v1.DatabasesService/CountDocuments"
	methodDBBulkUpdateDocuments = "/torchwood.server.v1.DatabasesService/BulkUpdateDocuments"
	methodDBBulkDeleteDocuments = "/torchwood.server.v1.DatabasesService/BulkDeleteDocuments"
)

// newDatabasesCmd 覆盖 DatabasesService 全部 22 个方法：
// 库（create/list/get/delete）、集合（create/list/get/update/delete）、
// 属性（create/delete）、索引（create/delete）、文档（create/list/get/
// update/upsert/delete/count/bulk-update/bulk-delete）。
// 复杂结构（document data、queries、permissions 等）一律接受 JSON 字符串 flag。
func newDatabasesCmd(g *globalFlags) *group {
	return newGroup(g, "databases", "数据库管理（DatabasesService 全部方法：库/集合/属性/索引/文档）", func(sub *commands.App) {
		sub.Register(
			newDatabasesCreateCmd(g),
			newDatabasesListCmd(g),
			newDatabasesGetCmd(g),
			newDatabasesDeleteCmd(g),
			newDatabasesCollectionsCmd(g),
			newDatabasesAttributesCmd(g),
			newDatabasesIndexesCmd(g),
			newDatabasesDocumentsCmd(g),
		)
	})
}

func newDatabasesCreateCmd(g *globalFlags) *verb {
	var id, name string
	return newVerb(g, "create", "创建数据库", "databases create --id <id> --name <name>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "数据库 ID（必填，小写字母/数字/下划线）")
			fs.StringVar(&name, "name", "", "数据库名称（必填）")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			req, err := buildCreateDatabaseReq(id, name)
			if err != nil {
				return err
			}
			return call(g, env, methodDBCreateDatabase, req)
		})
}

func newDatabasesListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出数据库", "databases list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodDBListDatabases, listJSON(pageSize, pageToken))
		})
}

func newDatabasesGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取数据库", "databases get <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodDBGetDatabase, map[string]any{"id": args[0]})
		})
}

func newDatabasesDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除数据库（default 库不可删除）", "databases delete <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodDBDeleteDatabase, map[string]any{"id": args[0]})
		})
}

// newDatabasesCollectionsCmd: databases collections create/list/get/update/delete。
func newDatabasesCollectionsCmd(g *globalFlags) *group {
	return newGroup(g, "collections", "集合管理", func(sub *commands.App) {
		sub.Register(
			newDatabasesCollectionsCreateCmd(g),
			newDatabasesCollectionsListCmd(g),
			newDatabasesCollectionsGetCmd(g),
			newDatabasesCollectionsUpdateCmd(g),
			newDatabasesCollectionsDeleteCmd(g),
		)
	})
}

func newDatabasesCollectionsCreateCmd(g *globalFlags) *verb {
	var id, name, permissions string
	var documentSecurity bool
	return newVerb(g, "create", "创建集合", "databases collections create <database-id> --id <id> --name <name>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "集合 ID（必填）")
			fs.StringVar(&name, "name", "", "集合名称（必填）")
			fs.StringVar(&permissions, "permissions", "", "权限 JSON 数组（如 '[\"read(\\\"users\\\")\"]'）")
			fs.BoolVar(&documentSecurity, "document-security", false, "文档级安全开关（显式传 --document-security=true/false 才生效）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildCreateCollectionReq(v, args[0], id, name, permissions, documentSecurity)
			if err != nil {
				return err
			}
			return call(g, env, methodDBCreateCollection, req)
		})
}

func newDatabasesCollectionsListCmd(g *globalFlags) *verb {
	var queries string
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出集合", "databases collections list <database-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&queries, "queries", "", "Appwrite 风格查询 JSON 数组（如 '[\"equal(\\\"status\\\",\\\"active\\\")\"]'）")
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildListCollectionsReq(args[0], queries, pageSize, pageToken)
			if err != nil {
				return err
			}
			return call(g, env, methodDBListCollections, req)
		})
}

func newDatabasesCollectionsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取集合", "databases collections get <database-id> <collection-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodDBGetCollection, map[string]any{"databaseId": args[0], "collectionId": args[1]})
		})
}

func newDatabasesCollectionsUpdateCmd(g *globalFlags) *verb {
	var name, permissions string
	var documentSecurity, disabled bool
	return newVerb(g, "update", "更新集合（仅更新显式传入的字段）", "databases collections update <database-id> <collection-id> [--name] [--permissions] [--document-security] [--disabled]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "集合名称")
			fs.StringVar(&permissions, "permissions", "", "权限 JSON 数组（全量替换）")
			fs.BoolVar(&documentSecurity, "document-security", false, "文档级安全开关（显式传 --document-security=true/false 才生效）")
			fs.BoolVar(&disabled, "disabled", false, "禁用集合（显式传 --disabled=true/false 才生效）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildUpdateCollectionReq(v, args[0], args[1], name, permissions, documentSecurity, disabled)
			if err != nil {
				return err
			}
			return call(g, env, methodDBUpdateCollection, req)
		})
}

func newDatabasesCollectionsDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除集合", "databases collections delete <database-id> <collection-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodDBDeleteCollection, map[string]any{"databaseId": args[0], "collectionId": args[1]})
		})
}

// newDatabasesAttributesCmd: databases attributes create/delete。
func newDatabasesAttributesCmd(g *globalFlags) *group {
	return newGroup(g, "attributes", "集合属性管理", func(sub *commands.App) {
		sub.Register(
			newDatabasesAttributesCreateCmd(g),
			newDatabasesAttributesDeleteCmd(g),
		)
	})
}

func newDatabasesAttributesCreateCmd(g *globalFlags) *verb {
	var key, typ, defaultValue string
	var size int
	var required, array bool
	return newVerb(g, "create", "创建属性（类型：string/integer/float/boolean/datetime）", "databases attributes create <database-id> <collection-id> --key <key> --type <type>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&key, "key", "", "属性名（必填）")
			fs.StringVar(&typ, "type", "", "属性类型（必填）")
			fs.IntVar(&size, "size", 0, "长度上限（string 属性）")
			fs.BoolVar(&required, "required", false, "是否必填")
			fs.BoolVar(&array, "array", false, "是否为数组")
			fs.StringVar(&defaultValue, "default-value", "", "默认值（JSON 文本）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildCreateAttributeReq(args[0], args[1], key, typ, size, required, array, defaultValue)
			if err != nil {
				return err
			}
			return call(g, env, methodDBCreateAttribute, req)
		})
}

func newDatabasesAttributesDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除属性", "databases attributes delete <database-id> <collection-id> <key>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 3); err != nil {
				return err
			}
			return call(g, env, methodDBDeleteAttribute, map[string]any{"databaseId": args[0], "collectionId": args[1], "key": args[2]})
		})
}

// newDatabasesIndexesCmd: databases indexes create/delete。
func newDatabasesIndexesCmd(g *globalFlags) *group {
	return newGroup(g, "indexes", "集合索引管理", func(sub *commands.App) {
		sub.Register(
			newDatabasesIndexesCreateCmd(g),
			newDatabasesIndexesDeleteCmd(g),
		)
	})
}

func newDatabasesIndexesCreateCmd(g *globalFlags) *verb {
	var id, typ, attributes, orders string
	return newVerb(g, "create", "创建索引（类型：key/unique/fulltext）", "databases indexes create <database-id> <collection-id> --id <id> --type <type> --attributes '[...]'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "索引 ID（必填）")
			fs.StringVar(&typ, "type", "", "索引类型（必填：key/unique/fulltext）")
			fs.StringVar(&attributes, "attributes", "", "索引属性 JSON 数组（必填）")
			fs.StringVar(&orders, "orders", "", "排序 JSON 数组（如 '[\"asc\",\"desc\"]'）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildCreateIndexReq(args[0], args[1], id, typ, attributes, orders)
			if err != nil {
				return err
			}
			return call(g, env, methodDBCreateIndex, req)
		})
}

func newDatabasesIndexesDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除索引", "databases indexes delete <database-id> <collection-id> <index-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 3); err != nil {
				return err
			}
			return call(g, env, methodDBDeleteIndex, map[string]any{"databaseId": args[0], "collectionId": args[1], "indexId": args[2]})
		})
}

// newDatabasesDocumentsCmd: databases documents create/list/get/update/upsert/
// delete/count/bulk-update/bulk-delete。
func newDatabasesDocumentsCmd(g *globalFlags) *group {
	return newGroup(g, "documents", "文档管理", func(sub *commands.App) {
		sub.Register(
			newDatabasesDocumentsCreateCmd(g),
			newDatabasesDocumentsListCmd(g),
			newDatabasesDocumentsGetCmd(g),
			newDatabasesDocumentsUpdateCmd(g),
			newDatabasesDocumentsUpsertCmd(g),
			newDatabasesDocumentsDeleteCmd(g),
			newDatabasesDocumentsCountCmd(g),
			newDatabasesDocumentsBulkUpdateCmd(g),
			newDatabasesDocumentsBulkDeleteCmd(g),
		)
	})
}

func newDatabasesDocumentsCreateCmd(g *globalFlags) *verb {
	var documentID, data, permissions string
	return newVerb(g, "create", "创建文档（--data 为文档数据 JSON 对象）", "databases documents create <database-id> <collection-id> --data '{...}'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&documentID, "document-id", "", "文档 ID（缺省自动生成）")
			fs.StringVar(&data, "data", "", "文档数据 JSON 对象（必填，如 '{\"title\":\"hi\"}'）")
			fs.StringVar(&permissions, "permissions", "", "权限 JSON 数组")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildCreateDocumentReq(args[0], args[1], documentID, data, permissions)
			if err != nil {
				return err
			}
			return call(g, env, methodDBCreateDocument, req)
		})
}

func newDatabasesDocumentsListCmd(g *globalFlags) *verb {
	var queries string
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出文档", "databases documents list <database-id> <collection-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&queries, "queries", "", "Appwrite 风格查询 JSON 数组")
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildListDocumentsReq(args[0], args[1], queries, pageSize, pageToken)
			if err != nil {
				return err
			}
			return call(g, env, methodDBListDocuments, req)
		})
}

func newDatabasesDocumentsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取文档", "databases documents get <database-id> <collection-id> <document-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 3); err != nil {
				return err
			}
			return call(g, env, methodDBGetDocument, map[string]any{"databaseId": args[0], "collectionId": args[1], "documentId": args[2]})
		})
}

func newDatabasesDocumentsUpdateCmd(g *globalFlags) *verb {
	var data, permissions, increment string
	var version int64
	return newVerb(g, "update", "更新文档（--version 必填；--data/--permissions/--increment 至少一个）", "databases documents update <database-id> <collection-id> <document-id> --version <n> [--data] [--permissions] [--increment]",
		func(fs *flag.FlagSet) {
			fs.Int64Var(&version, "version", 0, "当前文档版本（用户集合 OCC 必填，来自 get 的 version 字段）")
			fs.StringVar(&data, "data", "", "文档数据 JSON 对象（全量替换）")
			fs.StringVar(&permissions, "permissions", "", "权限 JSON 数组（全量替换）")
			fs.StringVar(&increment, "increment", "", "自增字段 JSON 对象（如 '{\"views\":1}'）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 3); err != nil {
				return err
			}
			req, err := buildUpdateDocumentReq(args[0], args[1], args[2], data, permissions, increment, version)
			if err != nil {
				return err
			}
			return call(g, env, methodDBUpdateDocument, req)
		})
}

func newDatabasesDocumentsUpsertCmd(g *globalFlags) *verb {
	var data, permissions, conflictColumns string
	return newVerb(g, "upsert", "按 document-id 存在则更新、否则创建", "databases documents upsert <database-id> <collection-id> <document-id> --data '{...}' --conflict-columns '[...]'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&data, "data", "", "文档数据 JSON 对象（必填）")
			fs.StringVar(&permissions, "permissions", "", "权限 JSON 数组")
			fs.StringVar(&conflictColumns, "conflict-columns", "", "冲突列 JSON 数组（必填）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 3); err != nil {
				return err
			}
			req, err := buildUpsertDocumentReq(args[0], args[1], args[2], data, permissions, conflictColumns)
			if err != nil {
				return err
			}
			return call(g, env, methodDBUpsertDocument, req)
		})
}

func newDatabasesDocumentsDeleteCmd(g *globalFlags) *verb {
	var version int64
	return newVerb(g, "delete", "删除文档（--version 必填，用户集合 OCC）", "databases documents delete <database-id> <collection-id> <document-id> --version <n>",
		func(fs *flag.FlagSet) {
			fs.Int64Var(&version, "version", 0, "当前文档版本（用户集合 OCC 必填，来自 get 的 version 字段）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 3); err != nil {
				return err
			}
			req, err := buildDeleteDocumentReq(args[0], args[1], args[2], version)
			if err != nil {
				return err
			}
			return call(g, env, methodDBDeleteDocument, req)
		})
}

// buildDeleteDocumentReq 构造 DeleteDocumentRequest JSON map（version 必填，
// 与服务端 OCC 校验一致）。
func buildDeleteDocumentReq(databaseID, collectionID, documentID string, version int64) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("缺少 collection-id")
	}
	if documentID == "" {
		return nil, fmt.Errorf("缺少 document-id")
	}
	if version <= 0 {
		return nil, fmt.Errorf("--version 必填（正整数，来自 get 的 version 字段）")
	}
	return map[string]any{
		"databaseId":   databaseID,
		"collectionId": collectionID,
		"documentId":   documentID,
		"version":      version,
	}, nil
}

func newDatabasesDocumentsCountCmd(g *globalFlags) *verb {
	var queries string
	return newVerb(g, "count", "统计匹配查询的文档数", "databases documents count <database-id> <collection-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&queries, "queries", "", "Appwrite 风格查询 JSON 数组")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildListDocumentsReq(args[0], args[1], queries, 0, "")
			if err != nil {
				return err
			}
			return call(g, env, methodDBCountDocuments, req)
		})
}

func newDatabasesDocumentsBulkUpdateCmd(g *globalFlags) *verb {
	var documentIDs, data, permissions string
	return newVerb(g, "bulk-update", "批量更新文档（共享同一份 data/permissions）", "databases documents bulk-update <database-id> <collection-id> --document-ids '[...]' --data '{...}'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&documentIDs, "document-ids", "", "文档 ID JSON 数组（必填）")
			fs.StringVar(&data, "data", "", "共享更新数据 JSON 对象（必填）")
			fs.StringVar(&permissions, "permissions", "", "权限 JSON 数组")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildBulkUpdateDocumentsReq(args[0], args[1], documentIDs, data, permissions)
			if err != nil {
				return err
			}
			return call(g, env, methodDBBulkUpdateDocuments, req)
		})
}

func newDatabasesDocumentsBulkDeleteCmd(g *globalFlags) *verb {
	var documentIDs string
	return newVerb(g, "bulk-delete", "批量删除文档", "databases documents bulk-delete <database-id> <collection-id> --document-ids '[...]'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&documentIDs, "document-ids", "", "文档 ID JSON 数组（必填）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildBulkDeleteDocumentsReq(args[0], args[1], documentIDs)
			if err != nil {
				return err
			}
			return call(g, env, methodDBBulkDeleteDocuments, req)
		})
}

// buildCreateDatabaseReq 构造 CreateDatabaseRequest JSON map（id/name 必填，
// 与服务端校验一致）。
func buildCreateDatabaseReq(id, name string) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("--id 必填")
	}
	if name == "" {
		return nil, fmt.Errorf("--name 必填")
	}
	return map[string]any{"id": id, "name": name}, nil
}

// buildCreateCollectionReq 构造 CreateCollectionRequest JSON map；
// documentSecurity 依赖 flag presence（proto3 optional 语义用键存在性表达）。
func buildCreateCollectionReq(v *verb, databaseID, id, name, permissions string, documentSecurity bool) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if id == "" {
		return nil, fmt.Errorf("--id 必填")
	}
	if name == "" {
		return nil, fmt.Errorf("--name 必填")
	}
	req := map[string]any{
		"databaseId": databaseID,
		"id":         id,
		"name":       name,
	}
	if permissions != "" {
		perms, err := jsonStringList(permissions, "--permissions")
		if err != nil {
			return nil, err
		}
		req["permissions"] = perms
	}
	setChanged(v, "document-security", req, "documentSecurity", documentSecurity)
	return req, nil
}

// buildListCollectionsReq 构造 ListCollectionsRequest JSON map（queries 数组 + 分页）。
func buildListCollectionsReq(databaseID, queries string, pageSize int, pageToken string) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	req := map[string]any{"databaseId": databaseID}
	if queries != "" {
		qs, err := jsonStringList(queries, "--queries")
		if err != nil {
			return nil, err
		}
		req["queries"] = qs
	}
	if pageSize > 0 {
		req["pageSize"] = pageSize
	}
	if pageToken != "" {
		req["pageToken"] = pageToken
	}
	return req, nil
}

// buildUpdateCollectionReq 构造 UpdateCollectionRequest JSON map：仅设置显式传入的字段。
func buildUpdateCollectionReq(v *verb, databaseID, collectionID, name, permissions string, documentSecurity, disabled bool) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("缺少 collection-id")
	}
	req := map[string]any{
		"databaseId":   databaseID,
		"collectionId": collectionID,
	}
	if name != "" {
		req["name"] = name
	}
	if permissions != "" {
		values, err := jsonStringList(permissions, "--permissions")
		if err != nil {
			return nil, err
		}
		req["permissions"] = map[string]any{"values": values}
	}
	setChanged(v, "document-security", req, "documentSecurity", documentSecurity)
	setChanged(v, "disabled", req, "disabled", disabled)
	return req, nil
}

// buildCreateAttributeReq 构造 CreateAttributeRequest JSON map（key/type 必填）。
func buildCreateAttributeReq(databaseID, collectionID, key, typ string, size int, required, array bool, defaultValue string) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("缺少 collection-id")
	}
	if key == "" {
		return nil, fmt.Errorf("--key 必填")
	}
	if typ == "" {
		return nil, fmt.Errorf("--type 必填")
	}
	req := map[string]any{
		"databaseId":   databaseID,
		"collectionId": collectionID,
		"key":          key,
		"type":         typ,
		"size":         size,
		"required":     required,
		"array":        array,
	}
	if defaultValue != "" {
		req["defaultValue"] = defaultValue
	}
	return req, nil
}

// buildCreateIndexReq 构造 CreateIndexRequest JSON map（id/type/attributes 必填）。
func buildCreateIndexReq(databaseID, collectionID, id, typ, attributes, orders string) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("缺少 collection-id")
	}
	if id == "" {
		return nil, fmt.Errorf("--id 必填")
	}
	if typ == "" {
		return nil, fmt.Errorf("--type 必填")
	}
	attrs, err := jsonStringList(attributes, "--attributes")
	if err != nil {
		return nil, err
	}
	if len(attrs) == 0 {
		return nil, fmt.Errorf("--attributes 必填")
	}
	req := map[string]any{
		"databaseId":   databaseID,
		"collectionId": collectionID,
		"id":           id,
		"type":         typ,
		"attributes":   attrs,
	}
	if orders != "" {
		ords, err := jsonStringList(orders, "--orders")
		if err != nil {
			return nil, err
		}
		req["orders"] = ords
	}
	return req, nil
}

// buildCreateDocumentReq 构造 CreateDocumentRequest JSON map（--data 为文档数据本体）。
func buildCreateDocumentReq(databaseID, collectionID, documentID, data, permissions string) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("缺少 collection-id")
	}
	docData, err := jsonObject(data, "--data")
	if err != nil {
		return nil, err
	}
	if docData == nil {
		return nil, fmt.Errorf("--data 必填（文档数据 JSON 对象）")
	}
	req := map[string]any{
		"databaseId":   databaseID,
		"collectionId": collectionID,
		"data":         docData,
	}
	if documentID != "" {
		req["documentId"] = documentID
	}
	if permissions != "" {
		perms, err := jsonStringList(permissions, "--permissions")
		if err != nil {
			return nil, err
		}
		req["permissions"] = perms
	}
	return req, nil
}

// buildListDocumentsReq 构造 ListDocumentsRequest JSON map（供 list/count
// 复用）。C7 单 AST：--queries 的 DSL 串在客户端经 pkg/query 解析为 AST 后
// 以 "query" 字段发送（服务端零 DSL 消费）。
func buildListDocumentsReq(databaseID, collectionID, queries string, pageSize int, pageToken string) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("缺少 collection-id")
	}
	req := map[string]any{
		"databaseId":   databaseID,
		"collectionId": collectionID,
	}
	if queries != "" {
		qs, err := jsonStringList(queries, "--queries")
		if err != nil {
			return nil, err
		}
		ast, err := query.ParseMany(qs)
		if err != nil {
			return nil, fmt.Errorf("--queries 解析失败: %w", err)
		}
		req["query"] = ast.ToWireJSON()
	}
	if pageSize > 0 {
		req["pageSize"] = pageSize
	}
	if pageToken != "" {
		req["pageToken"] = pageToken
	}
	return req, nil
}

// buildUpdateDocumentReq 构造 UpdateDocumentRequest JSON map（version 必填；
// data/permissions/increment 至少一个，与服务端校验一致；increment 用
// json.Number 保持 int64 精度）。
func buildUpdateDocumentReq(databaseID, collectionID, documentID, data, permissions, increment string, version int64) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("缺少 collection-id")
	}
	if documentID == "" {
		return nil, fmt.Errorf("缺少 document-id")
	}
	if version <= 0 {
		return nil, fmt.Errorf("--version 必填（正整数，来自 get 的 version 字段）")
	}
	if data == "" && permissions == "" && increment == "" {
		return nil, fmt.Errorf("--data/--permissions/--increment 至少提供一个")
	}
	req := map[string]any{
		"databaseId":   databaseID,
		"collectionId": collectionID,
		"documentId":   documentID,
		"version":      version,
	}
	if data != "" {
		docData, err := jsonObject(data, "--data")
		if err != nil {
			return nil, err
		}
		req["data"] = docData
	}
	if permissions != "" {
		perms, err := jsonStringList(permissions, "--permissions")
		if err != nil {
			return nil, err
		}
		req["permissions"] = perms
	}
	if increment != "" {
		incr, err := jsonInt64Map(increment, "--increment")
		if err != nil {
			return nil, err
		}
		req["increment"] = incr
	}
	return req, nil
}

// buildUpsertDocumentReq 构造 UpsertDocumentRequest JSON map（data/conflict-columns 必填）。
func buildUpsertDocumentReq(databaseID, collectionID, documentID, data, permissions, conflictColumns string) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("缺少 collection-id")
	}
	if documentID == "" {
		return nil, fmt.Errorf("缺少 document-id")
	}
	docData, err := jsonObject(data, "--data")
	if err != nil {
		return nil, err
	}
	if docData == nil {
		return nil, fmt.Errorf("--data 必填（文档数据 JSON 对象）")
	}
	cols, err := jsonStringList(conflictColumns, "--conflict-columns")
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("--conflict-columns 必填")
	}
	req := map[string]any{
		"databaseId":      databaseID,
		"collectionId":    collectionID,
		"documentId":      documentID,
		"data":            docData,
		"conflictColumns": cols,
	}
	if permissions != "" {
		perms, err := jsonStringList(permissions, "--permissions")
		if err != nil {
			return nil, err
		}
		req["permissions"] = perms
	}
	return req, nil
}

// buildBulkUpdateDocumentsReq 构造 BulkUpdateDocumentsRequest JSON map。
func buildBulkUpdateDocumentsReq(databaseID, collectionID, documentIDs, data, permissions string) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("缺少 collection-id")
	}
	ids, err := jsonStringList(documentIDs, "--document-ids")
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("--document-ids 必填")
	}
	docData, err := jsonObject(data, "--data")
	if err != nil {
		return nil, err
	}
	if docData == nil {
		return nil, fmt.Errorf("--data 必填（共享更新数据 JSON 对象）")
	}
	req := map[string]any{
		"databaseId":   databaseID,
		"collectionId": collectionID,
		"documentIds":  ids,
		"data":         docData,
	}
	if permissions != "" {
		perms, err := jsonStringList(permissions, "--permissions")
		if err != nil {
			return nil, err
		}
		req["permissions"] = perms
	}
	return req, nil
}

// buildBulkDeleteDocumentsReq 构造 BulkDeleteDocumentsRequest JSON map。
func buildBulkDeleteDocumentsReq(databaseID, collectionID, documentIDs string) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("缺少 database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("缺少 collection-id")
	}
	ids, err := jsonStringList(documentIDs, "--document-ids")
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("--document-ids 必填")
	}
	return map[string]any{
		"databaseId":   databaseID,
		"collectionId": collectionID,
		"documentIds":  ids,
	}, nil
}
