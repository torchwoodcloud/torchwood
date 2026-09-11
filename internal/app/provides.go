package app

import (
	"github.com/google/wire"
	"github.com/torchwoodcloud/torchwood/internal/app/assets"
	"github.com/torchwoodcloud/torchwood/internal/app/billing"
	"github.com/torchwoodcloud/torchwood/internal/app/client"
	"github.com/torchwoodcloud/torchwood/internal/app/console"
	"github.com/torchwoodcloud/torchwood/internal/app/events"
	"github.com/torchwoodcloud/torchwood/internal/app/functions"
	"github.com/torchwoodcloud/torchwood/internal/app/leaderboards"
	"github.com/torchwoodcloud/torchwood/internal/app/payments"
	"github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/app/storage"
	"github.com/torchwoodcloud/torchwood/internal/app/subscriptions"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

var ProviderSet = wire.NewSet(
	client.NewUserRoles,
	wire.Bind(new(domainauth.UserRoleResolver), new(*client.UserRoles)),
	client.NewAccount,
	client.NewDatabases,
	client.NewGroups,
	server.NewProjects,
	server.NewUsers,
	server.NewAPIKeys,
	server.NewAuditLogs,
	server.NewInviteCodes,
	server.NewOAuthProviders,
	server.NewGroups,
	server.NewDatabases,
	console.NewAuth,
	console.NewAdmins,
	console.NewSetup,
	storage.NewStorage,
	// P2 客户端调用面：Wire 装配入口换为带每用户限频端口的版本。
	functions.NewFunctionsWithClientQuota,
	functions.ProvideSemaphores,
	events.NewOutboxAdmin,
	payments.NewPayments,
	assets.NewAssets,
	leaderboards.NewLeaderboards,
	leaderboards.NewAssetsRewardGranter,
	subscriptions.NewSubscriptions,
	subscriptions.NewOrderFulfiller,
	wire.Bind(new(domainpayments.SubscriptionCallbackHandler), new(*subscriptions.Subscriptions)),
	billing.NewBilling,
)
