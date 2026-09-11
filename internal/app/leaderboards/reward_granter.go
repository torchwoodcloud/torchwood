package leaderboards

import (
	"context"

	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

// AssetsRewardGranter 把排行榜发奖适配到 Assets Grant（system 主体，对齐
// worker / 支付履约路径的红线 D6 放行）。幂等键经 Assets 项目级全局键
// 仲裁：同键重放返回首次结果，重跑不双发。
type AssetsRewardGranter struct {
	assets *appassets.Assets
}

// NewAssetsRewardGranter 构造 Assets 发放适配器。
func NewAssetsRewardGranter(assets *appassets.Assets) domainleaderboards.RewardGranter {
	return &AssetsRewardGranter{assets: assets}
}

func (g *AssetsRewardGranter) GrantReward(ctx context.Context, projectID, subjectID, assetCode string, amount int64, idempotencyKey string) error {
	ctx2 := contexts.WithPrincipal(ctx, shared.NewSystemPrincipal(projectID))
	_, err := g.assets.Grant(ctx2, appassets.GrantCommand{
		OwnerType:      domainassets.OwnerTypeUser,
		OwnerID:        subjectID,
		DefCode:        assetCode,
		Quantity:       amount,
		IdempotencyKey: idempotencyKey,
		RefType:        "leaderboard_settlement",
		RefID:          idempotencyKey,
	})
	return err
}
