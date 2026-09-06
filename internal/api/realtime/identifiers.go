package realtime

import "regexp"

// identifierRe 与 internal/app/server/databases.go 的 attribute 标识符校验
// 一致（不含 "."）。databaseId 走 pkg/ident。
var identifierRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// collectionIDRe 与 internal/app/server/databases.go 的 validateCollectionID
// 一致（2026-09-06 收紧小写：集合 ID 同时作为物理表名）。
var collectionIDRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// docIDRe 与 internal/infra/documentdb/postgres.go 的 docID 校验一致
// （可含 "." ":" "-"，最长 64）。
var docIDRe = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,64}$`)

// accountUserIDRe 匹配 accounts.{userId} 的 userId（ULID 可数字开头，
// 故不能复用 identifierRe；一段、最长 64）。
var accountUserIDRe = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,64}$`)
