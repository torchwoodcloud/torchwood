package cmd

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"

	"github.com/torchwoodcloud/torchwood/pkg/query"
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
	return newGroup(g, "databases", "database management (all DatabasesService methods: databases/collections/attributes/indexes/documents)", func(sub *commands.App) {
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
	return newVerb(g, "create", "create a database", "databases create --id <id> --name <name>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "database ID (required, lowercase letters/digits/underscores)")
			fs.StringVar(&name, "name", "", "database name (required)")
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
	return newVerb(g, "list", "list databases", "databases list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default 50, max 1000)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodDBListDatabases, listJSON(pageSize, pageToken))
		})
}

func newDatabasesGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "get a database by ID", "databases get <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodDBGetDatabase, map[string]any{"id": args[0]})
		})
}

func newDatabasesDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "delete a database (the default database cannot be deleted)", "databases delete <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodDBDeleteDatabase, map[string]any{"id": args[0]})
		})
}

// newDatabasesCollectionsCmd: databases collections create/list/get/update/delete。
func newDatabasesCollectionsCmd(g *globalFlags) *group {
	return newGroup(g, "collections", "collection management", func(sub *commands.App) {
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
	return newVerb(g, "create", "create a collection", "databases collections create <database-id> --id <id> --name <name>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "collection ID (required)")
			fs.StringVar(&name, "name", "", "collection name (required)")
			fs.StringVar(&permissions, "permissions", "", "permissions JSON array (e.g. '[\"read(\\\"users\\\")\"]')")
			fs.BoolVar(&documentSecurity, "document-security", false, "document-level security switch (pass --document-security=true/false explicitly to take effect)")
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
	return newVerb(g, "list", "list collections", "databases collections list <database-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&queries, "queries", "", "Appwrite-style queries JSON array (e.g. '[\"equal(\\\"status\\\",\\\"active\\\")\"]')")
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default 50, max 1000)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
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
	return newVerb(g, "get", "get a collection by ID", "databases collections get <database-id> <collection-id>", nil,
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
	return newVerb(g, "update", "update a collection (only explicitly passed fields)", "databases collections update <database-id> <collection-id> [--name] [--permissions] [--document-security] [--disabled]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "collection name")
			fs.StringVar(&permissions, "permissions", "", "permissions JSON array (full replacement)")
			fs.BoolVar(&documentSecurity, "document-security", false, "document-level security switch (pass --document-security=true/false explicitly to take effect)")
			fs.BoolVar(&disabled, "disabled", false, "disable the collection (pass --disabled=true/false explicitly to take effect)")
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
	return newVerb(g, "delete", "delete a collection", "databases collections delete <database-id> <collection-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodDBDeleteCollection, map[string]any{"databaseId": args[0], "collectionId": args[1]})
		})
}

// newDatabasesAttributesCmd: databases attributes create/delete。
func newDatabasesAttributesCmd(g *globalFlags) *group {
	return newGroup(g, "attributes", "collection attribute management", func(sub *commands.App) {
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
	return newVerb(g, "create", "create an attribute (types: string/integer/float/boolean/datetime)", "databases attributes create <database-id> <collection-id> --key <key> --type <type>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&key, "key", "", "attribute key (required)")
			fs.StringVar(&typ, "type", "", "attribute type (required)")
			fs.IntVar(&size, "size", 0, "max length (string attributes)")
			fs.BoolVar(&required, "required", false, "whether the attribute is required")
			fs.BoolVar(&array, "array", false, "whether the attribute is an array")
			fs.StringVar(&defaultValue, "default-value", "", "default value (JSON text)")
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
	return newVerb(g, "delete", "delete an attribute", "databases attributes delete <database-id> <collection-id> <key>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 3); err != nil {
				return err
			}
			return call(g, env, methodDBDeleteAttribute, map[string]any{"databaseId": args[0], "collectionId": args[1], "key": args[2]})
		})
}

// newDatabasesIndexesCmd: databases indexes create/delete。
func newDatabasesIndexesCmd(g *globalFlags) *group {
	return newGroup(g, "indexes", "collection index management", func(sub *commands.App) {
		sub.Register(
			newDatabasesIndexesCreateCmd(g),
			newDatabasesIndexesDeleteCmd(g),
		)
	})
}

func newDatabasesIndexesCreateCmd(g *globalFlags) *verb {
	var id, typ, attributes, orders string
	return newVerb(g, "create", "create an index (types: key/unique/fulltext)", "databases indexes create <database-id> <collection-id> --id <id> --type <type> --attributes '[...]'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "index ID (required)")
			fs.StringVar(&typ, "type", "", "index type (required: key/unique/fulltext)")
			fs.StringVar(&attributes, "attributes", "", "index attributes JSON array (required)")
			fs.StringVar(&orders, "orders", "", "orders JSON array (e.g. '[\"asc\",\"desc\"]')")
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
	return newVerb(g, "delete", "delete an index", "databases indexes delete <database-id> <collection-id> <index-id>", nil,
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
	return newGroup(g, "documents", "document management", func(sub *commands.App) {
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
	return newVerb(g, "create", "create a document (--data is the document data JSON object)", "databases documents create <database-id> <collection-id> --data '{...}'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&documentID, "document-id", "", "document ID (auto-generated by default)")
			fs.StringVar(&data, "data", "", "document data JSON object (required, e.g. '{\"title\":\"hi\"}')")
			fs.StringVar(&permissions, "permissions", "", "permissions JSON array")
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
	return newVerb(g, "list", "list documents", "databases documents list <database-id> <collection-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&queries, "queries", "", "Appwrite-style queries JSON array")
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default 50, max 1000)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
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
	return newVerb(g, "get", "get a document by ID", "databases documents get <database-id> <collection-id> <document-id>", nil,
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
	return newVerb(g, "update", "update a document (--version required; at least one of --data/--permissions/--increment)", "databases documents update <database-id> <collection-id> <document-id> --version <n> [--data] [--permissions] [--increment]",
		func(fs *flag.FlagSet) {
			fs.Int64Var(&version, "version", 0, "current document version (required for OCC on user collections, from the version field returned by get)")
			fs.StringVar(&data, "data", "", "document data JSON object (full replacement)")
			fs.StringVar(&permissions, "permissions", "", "permissions JSON array (full replacement)")
			fs.StringVar(&increment, "increment", "", "increment fields JSON object (e.g. '{\"views\":1}')")
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
	return newVerb(g, "upsert", "upsert a document: update if the document-id exists, create otherwise", "databases documents upsert <database-id> <collection-id> <document-id> --data '{...}' --conflict-columns '[...]'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&data, "data", "", "document data JSON object (required)")
			fs.StringVar(&permissions, "permissions", "", "permissions JSON array")
			fs.StringVar(&conflictColumns, "conflict-columns", "", "conflict columns JSON array (required)")
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
	return newVerb(g, "delete", "delete a document (--version required, OCC on user collections)", "databases documents delete <database-id> <collection-id> <document-id> --version <n>",
		func(fs *flag.FlagSet) {
			fs.Int64Var(&version, "version", 0, "current document version (required for OCC on user collections, from the version field returned by get)")
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
		return nil, fmt.Errorf("missing database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("missing collection-id")
	}
	if documentID == "" {
		return nil, fmt.Errorf("missing document-id")
	}
	if version <= 0 {
		return nil, fmt.Errorf("--version is required (positive integer, from the version field returned by get)")
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
	return newVerb(g, "count", "count documents matching the query", "databases documents count <database-id> <collection-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&queries, "queries", "", "Appwrite-style queries JSON array")
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
	return newVerb(g, "bulk-update", "bulk update documents (sharing one set of data/permissions)", "databases documents bulk-update <database-id> <collection-id> --document-ids '[...]' --data '{...}'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&documentIDs, "document-ids", "", "document IDs JSON array (required)")
			fs.StringVar(&data, "data", "", "shared update data JSON object (required)")
			fs.StringVar(&permissions, "permissions", "", "permissions JSON array")
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
	return newVerb(g, "bulk-delete", "bulk delete documents", "databases documents bulk-delete <database-id> <collection-id> --document-ids '[...]'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&documentIDs, "document-ids", "", "document IDs JSON array (required)")
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
		return nil, fmt.Errorf("--id is required")
	}
	if name == "" {
		return nil, fmt.Errorf("--name is required")
	}
	return map[string]any{"id": id, "name": name}, nil
}

// buildCreateCollectionReq 构造 CreateCollectionRequest JSON map；
// documentSecurity 依赖 flag presence（proto3 optional 语义用键存在性表达）。
func buildCreateCollectionReq(v *verb, databaseID, id, name, permissions string, documentSecurity bool) (map[string]any, error) {
	if databaseID == "" {
		return nil, fmt.Errorf("missing database-id")
	}
	if id == "" {
		return nil, fmt.Errorf("--id is required")
	}
	if name == "" {
		return nil, fmt.Errorf("--name is required")
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
		return nil, fmt.Errorf("missing database-id")
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
		return nil, fmt.Errorf("missing database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("missing collection-id")
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
		return nil, fmt.Errorf("missing database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("missing collection-id")
	}
	if key == "" {
		return nil, fmt.Errorf("--key is required")
	}
	if typ == "" {
		return nil, fmt.Errorf("--type is required")
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
		return nil, fmt.Errorf("missing database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("missing collection-id")
	}
	if id == "" {
		return nil, fmt.Errorf("--id is required")
	}
	if typ == "" {
		return nil, fmt.Errorf("--type is required")
	}
	attrs, err := jsonStringList(attributes, "--attributes")
	if err != nil {
		return nil, err
	}
	if len(attrs) == 0 {
		return nil, fmt.Errorf("--attributes is required")
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
		return nil, fmt.Errorf("missing database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("missing collection-id")
	}
	docData, err := jsonObject(data, "--data")
	if err != nil {
		return nil, err
	}
	if docData == nil {
		return nil, fmt.Errorf("--data is required (document data JSON object)")
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
		return nil, fmt.Errorf("missing database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("missing collection-id")
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
			return nil, fmt.Errorf("failed to parse --queries: %w", err)
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
		return nil, fmt.Errorf("missing database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("missing collection-id")
	}
	if documentID == "" {
		return nil, fmt.Errorf("missing document-id")
	}
	if version <= 0 {
		return nil, fmt.Errorf("--version is required (positive integer, from the version field returned by get)")
	}
	if data == "" && permissions == "" && increment == "" {
		return nil, fmt.Errorf("provide at least one of --data/--permissions/--increment")
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
		return nil, fmt.Errorf("missing database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("missing collection-id")
	}
	if documentID == "" {
		return nil, fmt.Errorf("missing document-id")
	}
	docData, err := jsonObject(data, "--data")
	if err != nil {
		return nil, err
	}
	if docData == nil {
		return nil, fmt.Errorf("--data is required (document data JSON object)")
	}
	cols, err := jsonStringList(conflictColumns, "--conflict-columns")
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("--conflict-columns is required")
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
		return nil, fmt.Errorf("missing database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("missing collection-id")
	}
	ids, err := jsonStringList(documentIDs, "--document-ids")
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("--document-ids is required")
	}
	docData, err := jsonObject(data, "--data")
	if err != nil {
		return nil, err
	}
	if docData == nil {
		return nil, fmt.Errorf("--data is required (shared update data JSON object)")
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
		return nil, fmt.Errorf("missing database-id")
	}
	if collectionID == "" {
		return nil, fmt.Errorf("missing collection-id")
	}
	ids, err := jsonStringList(documentIDs, "--document-ids")
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("--document-ids is required")
	}
	return map[string]any{
		"databaseId":   databaseID,
		"collectionId": collectionID,
		"documentIds":  ids,
	}, nil
}
