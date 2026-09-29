// Package crud 提供列表查询的通用分页方言：
//   - AIP-158 offset 分页（page_size/page_token 双向 token + 终止契约，
//     本文件）；
//   - 时间 keyset 游标（timecursor.go，方向化前缀）。
//
// AIP-160 过滤与 AIP-132 排序的解析/校验/构造器已退役：动态查询的活面由
// proto/shared/v1 的 typed AST（pkg/query）承担，全仓列表端点均以空
// filter/orderBy 消费 ParseListParams（token 绑定通道保留以兼容历史 token）。
package crud

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

const (
	// DefaultPageSize is the default page size for list operations
	DefaultPageSize = 50

	// MaxPageSize is the maximum page size allowed
	MaxPageSize = 1000

	// DefaultPageSizeNoToken is the default page size when no page_token is provided
	DefaultPageSizeNoToken = 50
)

// ListParams represents the parsed parameters from a List request
// following Google AIP standards (AIP-132, AIP-158)
type ListParams struct {
	// PageSize is the maximum number of results to return
	PageSize int32

	// PageToken is the token for requesting the next page
	PageToken string

	// Filter is the filter expression (token 绑定通道；解析已退役，原样记录)
	Filter string

	// OrderBy specifies the sort order (token 绑定通道；解析已退役，原样记录)
	OrderBy string

	// Offset is the decoded offset from the page token (internal use)
	Offset int
}

// ParseListParams parses and validates list parameters from a List request
// following AIP-158 standards.
//
// It enforces:
//   - page_size between 1 and MaxPageSize (default: DefaultPageSize)
//   - Validates page_token format if provided
//   - page_token 签名验证（启用签名后伪造/跨环境 token 被拒）
//   - offset 上限 MaxQueryOffset（防伪造超深分页拖垮数据库）
//   - order_by/filter 与签发该 token 的请求一致（token 内记录 digest）
func ParseListParams(pageSize int32, pageToken, filter, orderBy string) (ListParams, error) {
	params := ListParams{
		PageSize:  pageSize,
		PageToken: pageToken,
		Filter:    filter,
		OrderBy:   orderBy,
	}

	// Validate and set page_size
	if params.PageSize <= 0 {
		if params.PageToken == "" {
			params.PageSize = DefaultPageSizeNoToken
		} else {
			params.PageSize = DefaultPageSize
		}
	}

	// Enforce maximum page size (AIP-158)
	if params.PageSize > MaxPageSize {
		params.PageSize = MaxPageSize
	}

	// Decode page token to get offset
	if params.PageToken != "" {
		data, err := DecodePageTokenFull(params.PageToken)
		if err != nil {
			return ListParams{}, fmt.Errorf("invalid page_token: %w", err)
		}
		// R4-J2-4：order_by/filter 跨页一致性。仅当 token 记录了对应约束时校验，
		// 兼容未记录约束的历史 token。
		canonicalOrderBy := strings.TrimSpace(strings.ToLower(orderBy))
		if data.OrderBy != "" && strings.TrimSpace(strings.ToLower(data.OrderBy)) != canonicalOrderBy {
			return ListParams{}, fmt.Errorf("order_by must match the original request when using page_token")
		}
		if data.FilterDigest != "" && data.FilterDigest != FilterDigest(filter) {
			return ListParams{}, fmt.Errorf("filter must match the original request when using page_token")
		}
		if data.Offset > MaxQueryOffset {
			return ListParams{}, fmt.Errorf("page_token offset exceeds the maximum of %d", MaxQueryOffset)
		}
		params.Offset = data.Offset
	}

	return params, nil
}

// OffsetPage 是一次 offset 分页查询的收尾面：终止契约与双向 token 的单点
// 裁决（AIP-158）。
type OffsetPage struct {
	// PageSize 是回给响应 meta 的页大小（ParseListParams 归一后的生效值）。
	PageSize int32
	// TotalCount 是本查询的总行数（repo count；<0 视为未知，不投影）。
	TotalCount int
	// NextToken / PrevToken 是双向游标；空串 = 无该方向页（终止契约：
	// offset+returned < total 才有下一页——满页即末页不发空页 token）。
	NextToken string
	PrevToken string
}

// FinalizeOffsetPage 对一页结果做分页收尾：裁决终止契约并编码双向 token。
// returned 是本页实际行数（repo 已按 offset/limit 切片）；total 是总行数，
// 未知传 -1（此时不发 next token——宁缺勿滥，不发会误导的空页游标）。
// 编码失败（签名通道故障）以 error 上抛，handler 映射 Internal。
func FinalizeOffsetPage(params ListParams, total, returned int) (OffsetPage, error) {
	page := OffsetPage{PageSize: params.PageSize, TotalCount: total}
	if total >= 0 && params.Offset+returned < total {
		token, err := EncodePageToken(params.Offset + int(params.PageSize))
		if err != nil {
			return OffsetPage{}, err
		}
		page.NextToken = token
	}
	if params.Offset > 0 {
		prev := params.Offset - int(params.PageSize)
		if prev < 0 {
			prev = 0
		}
		token, err := EncodePageToken(prev)
		if err != nil {
			return OffsetPage{}, err
		}
		page.PrevToken = token
	}
	return page, nil
}

// SliceOffsetPage 在内存全量列表上切页并收尾（repo 未做 LIMIT 的全量形状，
// 如小表枚举端点）：切片钳制、终止契约与双向 token 一步完成。
func SliceOffsetPage[T any](items []T, params ListParams) ([]T, OffsetPage, error) {
	start := params.Offset
	if start > len(items) {
		start = len(items)
	}
	end := start + int(params.PageSize)
	if end > len(items) {
		end = len(items)
	}
	page, err := FinalizeOffsetPage(params, len(items), end-start)
	if err != nil {
		return nil, OffsetPage{}, err
	}
	return items[start:end], page, nil
}

// Meta 投影为 ListResponseMeta（handler 响应的 meta 字段一步到位）。
func (p OffsetPage) Meta() *sharedv1.ListResponseMeta {
	meta := &sharedv1.ListResponseMeta{PageSize: p.PageSize}
	if p.TotalCount >= 0 {
		meta.TotalCount = int32(p.TotalCount)
	}
	meta.NextPageToken = p.NextToken
	meta.PrevPageToken = p.PrevToken
	return meta
}

// FilterDigest returns a deterministic digest for the filter expression
// (token 绑定)：归一化原文哈希。历史实现做结构化归一（顺序无关），依赖已
// 退役的 AIP-160 解析器；存量 token 全部携带空 digest（调用点均传空
// filter，绑定检查不触发），算法切换无在途兼容面。
func FilterDigest(filter string) string {
	normalized := strings.TrimSpace(strings.ToLower(filter))
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}
