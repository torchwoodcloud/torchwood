package cmd

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildCreatePlanReq(t *testing.T) {
	tests := []struct {
		name         string
		code, name2  string
		amount       int64
		currency     string
		interval     string
		intervalDays int64
		benefits     string
		stripeID     string
		wantErr      string
		wantKeys     []string
	}{
		{name: "缺 code", name2: "会员", amount: 1999, currency: "cny", interval: "month",
			wantErr: "--code, --name and --currency are required"},
		{name: "缺 amount", code: "pro", name2: "会员", currency: "cny", interval: "month",
			wantErr: "--amount is required"},
		{name: "缺 interval 与 interval-days", code: "pro", name2: "会员", amount: 1999, currency: "cny",
			wantErr: "--interval or --interval-days is required"},
		{name: "interval-days 替代 interval", code: "pro", name2: "会员", amount: 1999, currency: "cny",
			intervalDays: 30, wantKeys: []string{"amount", "code", "currency", "graceDays", "intervalDays", "name", "trialDays"}},
		{name: "全字段", code: "pro", name2: "会员", amount: 1999, currency: "cny", interval: "month",
			benefits: `{"grants":[{"assetCode":"gems","quantity":100}]}`, stripeID: "price_123",
			wantKeys: []string{"amount", "benefits", "code", "currency", "graceDays", "interval", "name", "providerOverrides", "trialDays"}},
		{name: "benefits 非法 JSON", code: "pro", name2: "会员", amount: 1999, currency: "cny", interval: "month",
			benefits: `{bad`, wantErr: "failed to parse --benefits"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreatePlanReq(tt.code, tt.name2, tt.amount, tt.currency, tt.interval, tt.intervalDays, 3, 7, tt.benefits, tt.stripeID)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantKeys, keysOf(req))
			if tt.stripeID != "" {
				require.Equal(t, map[string]any{"stripePriceId": "price_123"}, req["providerOverrides"])
			}
			if tt.benefits != "" {
				_, ok := req["benefits"].(map[string]any)
				require.True(t, ok, "benefits 应落在 benefits 键下且为对象: %v", req)
			}
			require.Equal(t, 3, req["graceDays"])
			require.Equal(t, 7, req["trialDays"])
		})
	}
}

func TestBuildUpdatePlanReq(t *testing.T) {
	decl := func(fs *flag.FlagSet) {
		fs.String("name", "", "")
		fs.Int64("amount", 0, "")
		fs.String("currency", "", "")
		fs.String("interval", "", "")
		fs.Int64("interval-days", 0, "")
		fs.Int("grace-days", 0, "")
		fs.Int("trial-days", 0, "")
		fs.String("status", "", "")
		fs.String("benefits", "", "")
		fs.String("stripe-price-id", "", "")
	}
	t.Run("缺 plan-id", func(t *testing.T) {
		v := newPresenceVerb(t, decl, nil)
		_, err := buildUpdatePlanReq(v, "", subPlanPatch{})
		require.ErrorContains(t, err, "missing plan-id")
	})
	t.Run("零改动", func(t *testing.T) {
		v := newPresenceVerb(t, decl, nil)
		req, err := buildUpdatePlanReq(v, "p1", subPlanPatch{})
		require.NoError(t, err)
		require.Equal(t, map[string]any{"planId": "p1"}, req)
	})
	t.Run("benefits 仅显式传入才携带", func(t *testing.T) {
		v := newPresenceVerb(t, decl, map[string]string{"benefits": `{`, "status": "archived"})
		_, err := buildUpdatePlanReq(v, "p1", subPlanPatch{benefits: `{`, status: "archived"})
		require.ErrorContains(t, err, "failed to parse --benefits")

		v2 := newPresenceVerb(t, decl, map[string]string{"stripe-price-id": "price_x"})
		req, err := buildUpdatePlanReq(v2, "p1", subPlanPatch{stripePriceID: "price_x"})
		require.NoError(t, err)
		require.Equal(t, map[string]any{"planId": "p1",
			"providerOverrides": map[string]any{"stripePriceId": "price_x"}}, req)
	})
}

// keysOf 返回 map 键的排序切片（断言请求形状用）。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
