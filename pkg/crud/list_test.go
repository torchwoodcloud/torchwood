package crud

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseListParams(t *testing.T) {
	tests := []struct {
		name       string
		pageSize   int32
		pageToken  string
		filter     string
		orderBy    string
		wantSize   int32
		wantOffset int
		wantErr    bool
	}{
		{
			name:       "valid params with defaults",
			pageSize:   0,
			pageToken:  "",
			wantSize:   DefaultPageSizeNoToken,
			wantOffset: 0,
			wantErr:    false,
		},
		{
			name:       "custom page size",
			pageSize:   100,
			wantSize:   100,
			wantOffset: 0,
			wantErr:    false,
		},
		{
			name:       "page size exceeds max",
			pageSize:   2000,
			wantSize:   MaxPageSize,
			wantOffset: 0,
			wantErr:    false,
		},
		{
			name:       "with page token",
			pageSize:   50,
			pageToken:  mustEncodePageToken(t, 100),
			wantSize:   50,
			wantOffset: 100,
			wantErr:    false,
		},
		{
			name:       "invalid page token",
			pageSize:   50,
			pageToken:  "invalid-token",
			wantSize:   50,
			wantOffset: 0,
			wantErr:    true,
		},
		{
			name:       "negative page size",
			pageSize:   -10,
			wantSize:   DefaultPageSizeNoToken,
			wantOffset: 0,
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, err := ParseListParams(tt.pageSize, tt.pageToken, tt.filter, tt.orderBy)

			if (err != nil) != tt.wantErr {
				t.Errorf("ParseListParams() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if params.PageSize != tt.wantSize {
					t.Errorf("PageSize = %v, want %v", params.PageSize, tt.wantSize)
				}
				if params.Offset != tt.wantOffset {
					t.Errorf("Offset = %v, want %v", params.Offset, tt.wantOffset)
				}
			}
		})
	}
}

func TestEncodeDecodePageToken(t *testing.T) {
	offsets := []int{0, 10, 100, 1000, 9999}

	for _, offset := range offsets {
		t.Run("", func(t *testing.T) {
			token, err := EncodePageToken(offset)
			if err != nil {
				t.Errorf("EncodePageToken() error = %v", err)
				return
			}
			decoded, err := DecodePageToken(token)

			if err != nil {
				t.Errorf("DecodePageToken() error = %v", err)
				return
			}

			if decoded != offset {
				t.Errorf("DecodePageToken() = %v, want %v", decoded, offset)
			}
		})
	}
}

// TestFilterDigest 确定性 + 归一化（大小写/首尾空白）；空 filter digest 为
// 空（绑定检查不触发——全仓调用点均传空 filter）。
func TestFilterDigest(t *testing.T) {
	require.Equal(t, FilterDigest(`Status = "active"`), FilterDigest(`  status = "active"  `),
		"归一化后同串同 digest")
	require.Equal(t, FilterDigest(`status = "active"`), FilterDigest(`status = "active"`),
		"确定性")
	require.NotEqual(t, FilterDigest(`status = "active"`), FilterDigest(`status = "paused"`),
		"不同 filter 不同 digest")
	require.Empty(t, FilterDigest(""), "空 filter digest 为空")
	require.Empty(t, FilterDigest("   "), "纯空白 digest 为空")
}

// TestFinalizeOffsetPage 终止契约单点矩阵：中间页双向发 token；满页即末页
// 不发 next（空页 token 是 console 空页补丁的根源）；首页不发 prev；total
// 未知（-1）宁缺勿滥；prev offset 钳零。
func TestFinalizeOffsetPage(t *testing.T) {
	params := ListParams{PageSize: 50}

	// 中间页（offset 50, total 200, 满页 50 行）：双向 token。
	page, err := FinalizeOffsetPage(ListParams{PageSize: 50, Offset: 50}, 200, 50)
	require.NoError(t, err)
	next, err := DecodePageToken(page.NextToken)
	require.NoError(t, err)
	require.Equal(t, 100, next, "next token 指向下一页 offset")
	prev, err := DecodePageToken(page.PrevToken)
	require.NoError(t, err)
	require.Equal(t, 0, prev, "prev token 指向上一页 offset")
	require.Equal(t, int32(50), page.PageSize)
	require.Equal(t, 200, page.TotalCount)

	// 满页即末页：不发 next（终止契约收敛点）。
	last, err := FinalizeOffsetPage(ListParams{PageSize: 50, Offset: 150}, 200, 50)
	require.NoError(t, err)
	require.Empty(t, last.NextToken, "满页即末页不发空页 token")
	require.NotEmpty(t, last.PrevToken)

	// 未满页即末页：同样不发 next；prev 照常（offset 200 → prev 150）。
	partial, err := FinalizeOffsetPage(ListParams{PageSize: 50, Offset: 200}, 200, 25)
	require.NoError(t, err)
	require.Empty(t, partial.NextToken)
	prev, err = DecodePageToken(partial.PrevToken)
	require.NoError(t, err)
	require.Equal(t, 150, prev)

	// 首页：不发 prev。
	first, err := FinalizeOffsetPage(params, 200, 50)
	require.NoError(t, err)
	require.NotEmpty(t, first.NextToken)
	require.Empty(t, first.PrevToken)

	// 首页未满（total < pageSize）：两向都不发。
	single, err := FinalizeOffsetPage(params, 25, 25)
	require.NoError(t, err)
	require.Empty(t, single.NextToken)
	require.Empty(t, single.PrevToken)

	// total 未知（-1）：不发 next（宁缺勿滥）。
	unknown, err := FinalizeOffsetPage(params, -1, 50)
	require.NoError(t, err)
	require.Empty(t, unknown.NextToken)

	// 尾页 offset 溢出pageSize 边界：prev 钳零（页大小中途放大的历史 token）。
	clamped, err := FinalizeOffsetPage(ListParams{PageSize: 50, Offset: 10}, 100, 50)
	require.NoError(t, err)
	prev, err = DecodePageToken(clamped.PrevToken)
	require.NoError(t, err)
	require.Equal(t, 0, prev, "prev offset 钳零")
}
