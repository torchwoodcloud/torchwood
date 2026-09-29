package shared

import (
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
)

// OffsetPageMeta 把 crud.OffsetPage 投影为 ListResponseMeta（handler 响应的
// meta 字段一步到位）。proto 投影住在 api 层而非 crud——pkg/crud 保持零
// genproto 依赖（cmd/worker 经 app 层传递引用 crud，其 import guard 禁入
// genproto 面）；total 未知（<0）不投影 TotalCount（保持零值而非 -1）。
func OffsetPageMeta(p crud.OffsetPage) *sharedv1.ListResponseMeta {
	meta := &sharedv1.ListResponseMeta{PageSize: p.PageSize}
	if p.TotalCount >= 0 {
		meta.TotalCount = int32(p.TotalCount)
	}
	meta.NextPageToken = p.NextToken
	meta.PrevPageToken = p.PrevToken
	return meta
}
