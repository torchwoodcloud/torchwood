package realtime

import (
	"strings"

	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

// channelKind 区分 Realtime 频道族（v3 设计 §5.2：parseChannel 改派发表）。
type channelKind string

const (
	channelKindDatabases channelKind = "databases"
	channelKindAccounts  channelKind = "accounts"
)

// parsedChannel 是派发表解析结果。databases 填 dbID/collID/docID；
// accounts 填 userID（D17 单一 accounts.{userId}）。
type parsedChannel struct {
	kind   channelKind
	raw    string
	dbID   string
	collID string
	docID  string
	userID string
}

// channelParser 按频道前缀解析；格式非法返回 ok=false（INVALID_ARGUMENT）。
type channelParser func(ch string) (parsedChannel, bool)

// channelParsers 是频道族派发表（设计预留 seam：新增族只登记一行）。
var channelParsers = map[string]channelParser{
	"databases": parseDatabasesChannel,
	"accounts":  parseAccountsChannel,
}

// parseChannel 按首段派发解析频道名。未知前缀 / 格式非法 → ok=false。
func parseChannel(ch string) (parsedChannel, bool) {
	prefix, _, ok := strings.Cut(ch, ".")
	if !ok || prefix == "" {
		return parsedChannel{}, false
	}
	parser, found := channelParsers[prefix]
	if !found {
		return parsedChannel{}, false
	}
	return parser(ch)
}

// parseDatabasesChannel 按字面量分段解析（v2 设计 §2.4）：
//
//	databases . <db> . collections . <coll> [ . documents . <doc...> ]
//
// databaseId 须满足 schema 资源 id（pkg/ident）；collectionId 须满足
// collectionIDRe（小写，与 app 层 validateCollectionID 一致，不含 "."）；
// documentId 取余下全部（可含 "." ":" "-"）。
// 禁止 strings.Split 到 channel 任意位置后凭片段拼写，防止文档 id 含 "." 造成越权。
func parseDatabasesChannel(ch string) (parsedChannel, bool) {
	parts := strings.Split(ch, ".")
	if len(parts) < 4 || parts[0] != "databases" || parts[2] != "collections" {
		return parsedChannel{}, false
	}
	dbID, collID := parts[1], parts[3]
	if ident.ValidateSchemaResourceID(dbID) != nil || !collectionIDRe.MatchString(collID) {
		return parsedChannel{}, false
	}
	rest := parts[4:]
	if len(rest) == 0 {
		return parsedChannel{kind: channelKindDatabases, raw: ch, dbID: dbID, collID: collID}, true
	}
	if len(rest) < 2 || rest[0] != "documents" {
		return parsedChannel{}, false
	}
	docID := strings.Join(rest[1:], ".")
	if !docIDRe.MatchString(docID) {
		return parsedChannel{}, false
	}
	return parsedChannel{kind: channelKindDatabases, raw: ch, dbID: dbID, collID: collID, docID: docID}, true
}

// parseAccountsChannel 解析单一 accounts.{userId}（D17）。userId 一段，
// 禁止再拆；ULID 可数字开头，故不用 identifierRe。
func parseAccountsChannel(ch string) (parsedChannel, bool) {
	parts := strings.Split(ch, ".")
	if len(parts) != 2 || parts[0] != "accounts" {
		return parsedChannel{}, false
	}
	userID := parts[1]
	if !accountUserIDRe.MatchString(userID) {
		return parsedChannel{}, false
	}
	return parsedChannel{kind: channelKindAccounts, raw: ch, userID: userID}, true
}
