package realtime

import (
	"github.com/google/wire"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
)

// ProviderSet 供 cmd/server（Hub + Subscriber）与 cmd/worker
// （RealtimeTransport）共用；wire 按需裁剪未使用的 provider。
var ProviderSet = wire.NewSet(
	NewHub,
	wire.Bind(new(shared.RealtimeHub), new(*Hub)),
	NewStreamTransport,
	NewRealtimeSubscriber,
	wire.Bind(new(shared.RealtimeFanout), new(*Subscriber)),
)
