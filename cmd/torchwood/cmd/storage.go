package cmd

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

const (
	methodStorageCreateBucket = "/torchwood.server.v1.StorageService/CreateBucket"
	methodStorageListBuckets  = "/torchwood.server.v1.StorageService/ListBuckets"
	methodStorageGetBucket    = "/torchwood.server.v1.StorageService/GetBucket"
	methodStorageDeleteBucket = "/torchwood.server.v1.StorageService/DeleteBucket"
	methodStorageUpdateBucket = "/torchwood.server.v1.StorageService/UpdateBucket"
	methodStorageListFiles    = "/torchwood.server.v1.StorageService/ListFiles"
	methodStorageGetFile      = "/torchwood.server.v1.StorageService/GetFile"
	methodStorageDeleteFile   = "/torchwood.server.v1.StorageService/DeleteFile"
	methodStorageUpdateFile   = "/torchwood.server.v1.StorageService/UpdateFile"
	methodStorageUsage        = "/torchwood.server.v1.StorageService/GetStorageUsage"
)

// newStorageCmd 覆盖 StorageService 元数据操作：buckets（create/list/get/
// update/delete）、files（list/get/update/delete）、usage。
// 不做文件上传/下载（独立 HTTP handler）与分片上传会话；CreateFile（bytes
// 上传）与 CreateFileToken 亦不提供（token 用途为 HTTP 下载签名）。
func newStorageCmd(g *globalFlags) *group {
	return newGroup(g, "storage", "存储管理（StorageService 元数据操作；上传/下载走独立 HTTP handler，CLI 不提供）", func(sub *commands.App) {
		sub.Register(
			newStorageBucketsCmd(g),
			newStorageFilesCmd(g),
			newStorageUsageCmd(g),
		)
	})
}

// newStorageBucketsCmd: storage buckets create/list/get/update/delete。
func newStorageBucketsCmd(g *globalFlags) *group {
	return newGroup(g, "buckets", "存储桶管理", func(sub *commands.App) {
		sub.Register(
			newStorageBucketsCreateCmd(g),
			newStorageBucketsListCmd(g),
			newStorageBucketsGetCmd(g),
			newStorageBucketsUpdateCmd(g),
			newStorageBucketsDeleteCmd(g),
		)
	})
}

func newStorageBucketsCreateCmd(g *globalFlags) *verb {
	var name, permissions string
	var public bool
	return newVerb(g, "create", "创建存储桶", "storage buckets create --name <name>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "存储桶名称（必填）")
			fs.StringVar(&permissions, "permissions", "", "权限 JSON 数组")
			fs.BoolVar(&public, "public", false, "是否公开可读")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			req, err := buildCreateBucketReq(name, permissions, public)
			if err != nil {
				return err
			}
			return call(g, env, methodStorageCreateBucket, req)
		})
}

func newStorageBucketsListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出存储桶", "storage buckets list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodStorageListBuckets, listJSON(pageSize, pageToken))
		})
}

func newStorageBucketsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取存储桶", "storage buckets get <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodStorageGetBucket, map[string]any{"id": args[0]})
		})
}

func newStorageBucketsUpdateCmd(g *globalFlags) *verb {
	var name string
	var public bool
	return newVerb(g, "update", "更新存储桶（仅更新显式传入的字段）", "storage buckets update <id> [--name] [--public]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "新名称")
			fs.BoolVar(&public, "public", false, "公开可读开关（显式传 --public=true/false 才生效）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildUpdateBucketReq(v, args[0], name, public)
			if err != nil {
				return err
			}
			return call(g, env, methodStorageUpdateBucket, req)
		})
}

func newStorageBucketsDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除存储桶", "storage buckets delete <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodStorageDeleteBucket, map[string]any{"id": args[0]})
		})
}

// newStorageFilesCmd: storage files list/get/update/delete。
func newStorageFilesCmd(g *globalFlags) *group {
	return newGroup(g, "files", "文件元数据管理（上传/下载走独立 HTTP handler，CLI 不提供）", func(sub *commands.App) {
		sub.Register(
			newStorageFilesListCmd(g),
			newStorageFilesGetCmd(g),
			newStorageFilesUpdateCmd(g),
			newStorageFilesDeleteCmd(g),
		)
	})
}

func newStorageFilesListCmd(g *globalFlags) *verb {
	var queries string
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出桶内文件", "storage files list <bucket-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&queries, "queries", "", "Appwrite 风格查询 JSON 数组")
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildListFilesReq(args[0], queries, pageSize, pageToken)
			if err != nil {
				return err
			}
			return call(g, env, methodStorageListFiles, req)
		})
}

func newStorageFilesGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取文件元数据", "storage files get <bucket-id> <file-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodStorageGetFile, map[string]any{"bucketId": args[0], "fileId": args[1]})
		})
}

func newStorageFilesUpdateCmd(g *globalFlags) *verb {
	var name, mimeType, metadata string
	return newVerb(g, "update", "更新文件元数据（仅更新显式传入的字段）", "storage files update <bucket-id> <file-id> [--name] [--mime-type] [--metadata]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "新文件名")
			fs.StringVar(&mimeType, "mime-type", "", "新 MIME 类型")
			fs.StringVar(&metadata, "metadata", "", "元数据 JSON 对象（如 '{\"author\":\"x\"}'）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildUpdateFileReq(v, args[0], args[1], name, mimeType, metadata)
			if err != nil {
				return err
			}
			return call(g, env, methodStorageUpdateFile, req)
		})
}

func newStorageFilesDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除文件", "storage files delete <bucket-id> <file-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodStorageDeleteFile, map[string]any{"bucketId": args[0], "fileId": args[1]})
		})
}

func newStorageUsageCmd(g *globalFlags) *verb {
	return newVerb(g, "usage", "获取存储用量（桶数/文件数/总大小）", "storage usage", nil,
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodStorageUsage, nil)
		})
}

// buildCreateBucketReq 构造 CreateBucketRequest（name 必填）。
func buildCreateBucketReq(name, permissions string, public bool) (map[string]any, error) {
	if name == "" {
		return nil, fmt.Errorf("--name 必填")
	}
	req := map[string]any{"name": name, "public": public}
	if permissions != "" {
		perms, err := jsonStringList(permissions, "--permissions")
		if err != nil {
			return nil, err
		}
		req["permissions"] = perms
	}
	return req, nil
}

// buildUpdateBucketReq 构造 UpdateBucketRequest：仅设置显式传入的字段；
// name 非空即设置，public 依赖 flag presence。
func buildUpdateBucketReq(v *verb, id, name string, public bool) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("缺少存储桶 ID")
	}
	req := map[string]any{"id": id}
	if name != "" {
		req["name"] = name
	}
	setChanged(v, "public", req, "public", public)
	return req, nil
}

// buildListFilesReq 构造 ListFilesRequest。
func buildListFilesReq(bucketID, queries string, pageSize int, pageToken string) (map[string]any, error) {
	if bucketID == "" {
		return nil, fmt.Errorf("缺少 bucket-id")
	}
	req := map[string]any{"bucketId": bucketID}
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

// buildUpdateFileReq 构造 UpdateFileRequest（name/mime-type/metadata 至少一个）。
func buildUpdateFileReq(v *verb, bucketID, fileID, name, mimeType, metadata string) (map[string]any, error) {
	if bucketID == "" {
		return nil, fmt.Errorf("缺少 bucket-id")
	}
	if fileID == "" {
		return nil, fmt.Errorf("缺少 file-id")
	}
	if name == "" && mimeType == "" && metadata == "" {
		return nil, fmt.Errorf("--name/--mime-type/--metadata 至少提供一个")
	}
	req := map[string]any{"bucketId": bucketID, "fileId": fileID}
	setChanged(v, "name", req, "name", name)
	setChanged(v, "mime-type", req, "mimeType", mimeType)
	if metadata != "" {
		md, err := jsonStringMap(metadata, "--metadata")
		if err != nil {
			return nil, err
		}
		req["metadata"] = md
	}
	return req, nil
}
