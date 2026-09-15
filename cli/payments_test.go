package cli

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildRefundReq(t *testing.T) {
	tests := []struct {
		name    string
		orderID string
		amount  int64
		reason  string
		wantErr string
		want    map[string]any
	}{
		{name: "缺 order-id", wantErr: "missing order-id"},
		{name: "全额退款（amount 缺省）", orderID: "o1", want: map[string]any{"orderId": "o1"}},
		{name: "部分退款", orderID: "o1", amount: 500, reason: "duplicate",
			want: map[string]any{"orderId": "o1", "amount": int64(500), "reason": "duplicate"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildRefundReq(tt.orderID, tt.amount, tt.reason)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, req)
		})
	}
}
