package billing

import (
	"github.com/google/wire"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
)

// ProviderSet 装配 Redis 用量计数器。
var ProviderSet = wire.NewSet(
	NewRedisCounter,
	wire.Bind(new(domainbilling.UsageCounter), new(*RedisCounter)),
)
