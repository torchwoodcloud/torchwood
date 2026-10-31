// Package crud —— lynx-start 基础库的同源件转发层（dogfood 采纳，
// 2026-11-01，lynx-start battery-roadmap 首循环①）。
//
// 分页收尾/时间游标/过滤摘要/排序白名单解析等 ORM 中立基元直接消费
// github.com/lynx-go/lynx-start/crud v0.5.1，本仓不再维护逐字副本。
// page token 签名机械（pagination.go）暂留本地：密钥推导 purpose 是
// torchwood 专属（换库会作废在途游标），且白盒测试依赖进程内钩子
// （pageTokenMarshal/seal）——随 v2 keyset 分页换装整体退役。
// 转发层本体即 v1→v2 过渡垫片：对 Deprecated 库 API 的引用集中于此，
// 逐行 nolint 收口（golangci 文件级指令不跨行生效）。
package crud

import (
	base "github.com/lynx-go/lynx-start/crud"
)

const (
	DefaultPageSize        = base.DefaultPageSize
	MaxPageSize            = base.MaxPageSize
	DefaultPageSizeNoToken = base.DefaultPageSizeNoToken
)

// 类型别名：与库实现同一身份（可互传），调用面零改动。
type (
	ListParams = base.ListParams //nolint:staticcheck // v1→v2 过渡垫片
	OffsetPage = base.OffsetPage //nolint:staticcheck // v1→v2 过渡垫片
	TimeCursor = base.TimeCursor //nolint:staticcheck // v1→v2 过渡垫片
)

// 函数包装而非 var 别名：库侧 v1 API 已标 Deprecated，包装层阻断
// SA1019 向本仓调用面传播；本转发层整体随 v2 换装退役。
func ParseListParams(pageSize int32, pageToken, filter, orderBy string) (ListParams, error) {
	return base.ParseListParams(pageSize, pageToken, filter, orderBy) //nolint:staticcheck // v1→v2 过渡垫片
}

func FinalizeOffsetPage(params ListParams, total, returned int) (OffsetPage, error) {
	return base.FinalizeOffsetPage(params, total, returned) //nolint:staticcheck // v1→v2 过渡垫片
}

func SliceOffsetPage[T any](items []T, params ListParams) ([]T, OffsetPage, error) {
	return base.SliceOffsetPage[T](items, params) //nolint:staticcheck // v1→v2 过渡垫片
}

func FilterDigest(filter string) string {
	return base.FilterDigest(filter) //nolint:staticcheck // v1→v2 过渡垫片
}

func EncodeTimeCursor(c TimeCursor) string {
	return base.EncodeTimeCursor(c) //nolint:staticcheck // v1→v2 过渡垫片
}

func DecodeTimeCursor(token string) (TimeCursor, error) {
	return base.DecodeTimeCursor(token) //nolint:staticcheck // v1→v2 过渡垫片
}

func DecodeTimeCursorDirection(token string, ascending bool) (TimeCursor, error) {
	return base.DecodeTimeCursorDirection(token, ascending) //nolint:staticcheck // v1→v2 过渡垫片
}

var ErrCursorDirection = base.ErrCursorDirection
