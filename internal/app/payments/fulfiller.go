package payments

import (
	"context"

	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

// recordOnlyFulfiller 是 PR1 的履约占位（设计 §1.5 / 执行计划 PR1：
// 履约端口就位、实际发放留 hook，PR2 联通 topup/item_purchase 资产 Grant）。
// 不发放任何资产，仅回占位 ref；调用点位于订单翻 paid 的同一事务内。
type recordOnlyFulfiller struct{}

// NewRecordOnlyFulfiller 构造占位履约器（wire 注入；PR2 替换为资产系统实现）。
func NewRecordOnlyFulfiller() domainpayments.Fulfiller {
	return recordOnlyFulfiller{}
}

func (recordOnlyFulfiller) Fulfill(_ context.Context, order *domainpayments.Order) (string, error) {
	return "order:" + order.ID, nil
}

func (recordOnlyFulfiller) Reverse(_ context.Context, _ *domainpayments.Order) error { return nil }
