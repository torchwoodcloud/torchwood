package clientgrpc

import (
	"time"

	"github.com/torchwoodcloud/torchwood/pkg/crud"
)

// encodeDescTimeCursor / decodeDescTimeCursor：client 面列表固定 created_at
// DESC（终端 API 不暴露 sort_order），时间 keyset 游标统一收口 crud.TimeCursor
// （方向前缀 / legacy 无前缀兼容 / 异向拒绝语义单点实现）。固定 DESC 列表的
// 方向闸 = 拒绝 "a:" 前缀 token；签发统一带 "d:" 前缀（存量无前缀 token 解码
// 照常兼容）。
func encodeDescTimeCursor(t time.Time) string {
	return crud.EncodeTimeCursor(crud.TimeCursor{Time: t})
}

func decodeDescTimeCursor(token string) (time.Time, error) {
	c, err := crud.DecodeTimeCursorDirection(token, false)
	return c.Time, err
}
