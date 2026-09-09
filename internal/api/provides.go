package api

import (
	"github.com/google/wire"
	"github.com/torchwooddev/torchwood/internal/api/clientgrpc"
	"github.com/torchwooddev/torchwood/internal/api/consolegrpc"
	apirealtime "github.com/torchwooddev/torchwood/internal/api/realtime"
	"github.com/torchwooddev/torchwood/internal/api/servergrpc"
	"github.com/torchwooddev/torchwood/internal/api/serverhttp"
)

var ProviderSet = wire.NewSet(
	clientgrpc.NewAccountService,
	clientgrpc.NewDatabasesService,
	clientgrpc.NewGroupsService,
	clientgrpc.NewPaymentsService,
	clientgrpc.NewAssetsService,
	clientgrpc.NewSubscriptionsService,
	clientgrpc.NewFunctionsService,
	servergrpc.NewHealthService,
	servergrpc.NewProjectsService,
	servergrpc.NewStorageService,
	servergrpc.NewUsersService,
	servergrpc.NewAPIKeysService,
	servergrpc.NewOAuthProvidersService,
	servergrpc.NewGroupsService,
	servergrpc.NewDatabasesService,
	servergrpc.NewFunctionsService,
	servergrpc.NewPaymentsService,
	servergrpc.NewAssetsService,
	servergrpc.NewSubscriptionsService,
	servergrpc.NewBillingService,
	servergrpc.NewOutboxService,
	serverhttp.NewFileHandler,
	serverhttp.NewOAuthHandler,
	serverhttp.NewFunctionsHandler,
	serverhttp.NewPaymentsHandler,
	// P1 触发器模块：/f/{project_id}/{trigger_token} 公开入口。
	serverhttp.NewFunctionTriggersHandler,
	consolegrpc.NewAuthService,
	consolegrpc.NewAdminsService,
	apirealtime.NewHandler,
)
