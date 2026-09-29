package servergrpc

import (
	"errors"
	"testing"
	"time"

	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 时间 keyset 游标语义（方向化格式 / legacy 兼容 / 异向拒绝）单点收口在
// pkg/crud.TimeCursor 并在那里全量测试；此处只断言 handler 家族薄壳的
// 接线：编码→解码往返，与错误到 InvalidArgument 的映射。

func TestServerOrderCursorWrapperRoundTrip(t *testing.T) {
	ts := time.Date(2026, 9, 22, 10, 30, 0, 123456789, time.UTC)
	for _, ascending := range []bool{true, false} {
		token := encodeServerOrderCursor(ts, ascending)
		got, err := decodeServerOrderPage(token, ascending)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !got.Equal(ts) {
			t.Fatalf("time = %v, want %v", got, ts)
		}
		// 异向复用被闸拒绝（换向必须从第一页重来）。
		if _, err := decodeServerOrderPage(token, !ascending); !errors.Is(err, crud.ErrCursorDirection) {
			t.Fatalf("mismatched cursor: err = %v, want crud.ErrCursorDirection", err)
		}
	}
}

func TestInvalidServerOrderCursorMapping(t *testing.T) {
	err := invalidServerOrderCursor(crud.ErrCursorDirection)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
	generic := invalidServerOrderCursor(errors.New("not base64"))
	if status.Code(generic) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(generic))
	}
}
