# 授权矩阵（Authorization Matrix）

> **本文件由策略注册表生成（`task gen:authz-matrix`），勿手改。**
>
> - 声明源：`proto/shared/v1/authz.proto` 的 `method_auth`/`service_auth` 注解 →
>   `internal/runtime.BuildMethodPolicies`（启动期经 `ProvideMethodPolicies` 注入执行点）。
> - 策略变更后重新生成：`task gen:authz-matrix`；漂移由
>   `internal/runtime/authz_matrix_doc_test.go` 字节级锁定（重渲染 ≠ 磁盘即红）。
> - 执行器消费同一策略的行为一致性证明见 `internal/runtime/authz_matrix_test.go`
>   （全方法 × 凭证档过真实拦截器，与独立推导全量比对）。
> - Access 语义：PUBLIC 匿名可调；END_USER 端用户会话专属（client 面）；
>   SERVER admin 会话（admin_roles）或 API key（scope）；PERMISSION admin 会话
>   专属（permissions），key 一律拒绝；SYSTEM 预留禁用。

方法总数 207：PUBLIC 28 · END_USER 43 · SERVER 120 · PERMISSION 16 · SYSTEM 0。

## /torchwood.client.v1.AccountService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.client.v1.AccountService/ConfirmEmailChange` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateAnonymousSession` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateEmailOTP` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateEmailOTPSession` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateJWT` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateMFASession` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateMagicURLSession` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateOAuth2LinkSession` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateOAuth2LinkTokenSession` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateOAuth2Session` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateOAuth2TokenSession` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreatePhoneOTP` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreatePhoneOTPSession` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateRecovery` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateTOTPFactor` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateVerification` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/CreateWeChatMiniProgramSession` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/DeleteAccount` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/DeleteFactor` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/DeleteSession` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/DeleteSessions` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/GetPrefs` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/ListFactors` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/ListLogs` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/ListSessions` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/Me` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/RefreshToken` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/SignIn` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/SignOut` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/SignUp` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/UpdateAccount` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/UpdateMagicURLSession` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/UpdatePrefs` | END_USER | — | — | — | |
| `/torchwood.client.v1.AccountService/UpdateRecovery` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/UpdateVerification` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.AccountService/VerifyTOTPFactor` | END_USER | — | — | — | |

## /torchwood.client.v1.AssetsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.client.v1.AssetsService/ListAssetDefs` | END_USER | — | — | — | |
| `/torchwood.client.v1.AssetsService/ListMyAssetLedger` | END_USER | — | — | — | |
| `/torchwood.client.v1.AssetsService/ListMyAssets` | END_USER | — | — | — | |

## /torchwood.client.v1.DatabasesService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.client.v1.DatabasesService/CountDocuments` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.DatabasesService/CreateDocument` | END_USER | — | — | — | |
| `/torchwood.client.v1.DatabasesService/DeleteDocument` | END_USER | — | — | — | |
| `/torchwood.client.v1.DatabasesService/GetDocument` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.DatabasesService/ListChanges` | END_USER | — | — | — | |
| `/torchwood.client.v1.DatabasesService/ListDocuments` | PUBLIC | — | — | — | |
| `/torchwood.client.v1.DatabasesService/UpdateDocument` | END_USER | — | — | — | |
| `/torchwood.client.v1.DatabasesService/UpsertDocument` | END_USER | — | — | — | |

## /torchwood.client.v1.FunctionsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.client.v1.FunctionsService/InvokeFunction` | END_USER | — | — | — | |

## /torchwood.client.v1.GroupsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.client.v1.GroupsService/CreateGroup` | END_USER | — | — | — | |
| `/torchwood.client.v1.GroupsService/CreateMembership` | END_USER | — | — | — | |
| `/torchwood.client.v1.GroupsService/DeleteGroup` | END_USER | — | — | — | |
| `/torchwood.client.v1.GroupsService/DeleteMembership` | END_USER | — | — | — | |
| `/torchwood.client.v1.GroupsService/GetGroup` | END_USER | — | — | — | |
| `/torchwood.client.v1.GroupsService/ListGroups` | END_USER | — | — | — | |
| `/torchwood.client.v1.GroupsService/ListMemberships` | END_USER | — | — | — | |
| `/torchwood.client.v1.GroupsService/UpdateMembershipStatus` | END_USER | — | — | — | |

## /torchwood.client.v1.PaymentsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.client.v1.PaymentsService/CreateOrder` | END_USER | — | — | — | |
| `/torchwood.client.v1.PaymentsService/GetMyOrder` | END_USER | — | — | — | |
| `/torchwood.client.v1.PaymentsService/ListMyOrders` | END_USER | — | — | — | |
| `/torchwood.client.v1.PaymentsService/VerifyReceipt` | END_USER | — | — | — | |

## /torchwood.client.v1.SubscriptionsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.client.v1.SubscriptionsService/Cancel` | END_USER | — | — | — | |
| `/torchwood.client.v1.SubscriptionsService/GetMySubscription` | END_USER | — | — | — | |
| `/torchwood.client.v1.SubscriptionsService/ListPlans` | END_USER | — | — | — | |
| `/torchwood.client.v1.SubscriptionsService/Subscribe` | END_USER | — | — | — | |

## /torchwood.console.v1.AdminsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.console.v1.AdminsService/CreateAdmin` | PERMISSION | owner | — | — | |
| `/torchwood.console.v1.AdminsService/DeleteAdmin` | PERMISSION | owner | — | — | |
| `/torchwood.console.v1.AdminsService/GetCurrentAdmin` | PERMISSION | console | — | — | |
| `/torchwood.console.v1.AdminsService/ListAdmins` | PERMISSION | owner, admin | — | — | |
| `/torchwood.console.v1.AdminsService/UpdateAdmin` | PERMISSION | owner | — | — | |

## /torchwood.console.v1.ConsoleAuthService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.console.v1.ConsoleAuthService/GetSetupStatus` | PUBLIC | — | — | — | |
| `/torchwood.console.v1.ConsoleAuthService/RefreshToken` | PUBLIC | — | — | — | |
| `/torchwood.console.v1.ConsoleAuthService/SignIn` | PUBLIC | — | — | — | |
| `/torchwood.console.v1.ConsoleAuthService/SignOut` | PUBLIC | — | — | — | |
| `/torchwood.console.v1.ConsoleAuthService/SignUp` | PUBLIC | — | — | — | |

## /torchwood.server.v1.APIKeysService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.APIKeysService/CreateAPIKey` | PERMISSION | owner, admin | — | platform_only | |
| `/torchwood.server.v1.APIKeysService/DeleteAPIKey` | PERMISSION | owner, admin | — | platform_only | |
| `/torchwood.server.v1.APIKeysService/GetAPIKey` | PERMISSION | owner, admin | — | platform_only | |
| `/torchwood.server.v1.APIKeysService/ListAPIKeys` | PERMISSION | owner, admin | — | platform_only | |
| `/torchwood.server.v1.APIKeysService/UpdateAPIKey` | PERMISSION | owner, admin | — | platform_only | |

## /torchwood.server.v1.AssetsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.AssetsService/Consume` | SERVER | admin, owner | assets.write | delegated_platform | |
| `/torchwood.server.v1.AssetsService/CreateAssetDef` | SERVER | member, admin, owner | assets.write | business_write | |
| `/torchwood.server.v1.AssetsService/DeleteAssetDef` | SERVER | member, admin, owner | assets.write | business_write | |
| `/torchwood.server.v1.AssetsService/Expire` | SERVER | admin, owner | assets.write | delegated_platform | |
| `/torchwood.server.v1.AssetsService/GetAssetDef` | SERVER | 不限角色 | assets.read | read_only | |
| `/torchwood.server.v1.AssetsService/Grant` | SERVER | admin, owner | assets.write | delegated_platform | |
| `/torchwood.server.v1.AssetsService/ListAssetDefs` | SERVER | 不限角色 | assets.read | read_only | |
| `/torchwood.server.v1.AssetsService/ListUserAssets` | SERVER | 不限角色 | assets.read | read_only | |
| `/torchwood.server.v1.AssetsService/ListUserLedger` | SERVER | 不限角色 | assets.read | read_only | |
| `/torchwood.server.v1.AssetsService/Mutate` | SERVER | admin, owner | assets.write | delegated_platform | |
| `/torchwood.server.v1.AssetsService/Reconcile` | SERVER | admin, owner | assets.write | delegated_platform | |
| `/torchwood.server.v1.AssetsService/Transfer` | SERVER | admin, owner | assets.write | delegated_platform | |
| `/torchwood.server.v1.AssetsService/UpdateAssetDef` | SERVER | member, admin, owner | assets.write | business_write | |

## /torchwood.server.v1.BillingService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.BillingService/GetUsage` | SERVER | 不限角色 | billing.read | read_only | |
| `/torchwood.server.v1.BillingService/ListRollups` | SERVER | 不限角色 | billing.read | read_only | |
| `/torchwood.server.v1.BillingService/ListStatements` | SERVER | 不限角色 | billing.read | read_only | |

## /torchwood.server.v1.DatabasesService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.DatabasesService/AggregateDocuments` | SERVER | 不限角色 | databases.read | read_only | |
| `/torchwood.server.v1.DatabasesService/BulkDeleteDocuments` | SERVER | member, admin, owner | databases.write | business_write | |
| `/torchwood.server.v1.DatabasesService/BulkUpdateDocuments` | SERVER | member, admin, owner | databases.write | business_write | |
| `/torchwood.server.v1.DatabasesService/CountDocuments` | SERVER | 不限角色 | databases.read | read_only | |
| `/torchwood.server.v1.DatabasesService/CreateAttribute` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/CreateCollection` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/CreateDatabase` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/CreateDocument` | SERVER | member, admin, owner | databases.write | business_write | |
| `/torchwood.server.v1.DatabasesService/CreateIndex` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/DeleteAttribute` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/DeleteCollection` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/DeleteDatabase` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/DeleteDocument` | SERVER | member, admin, owner | databases.write | business_write | |
| `/torchwood.server.v1.DatabasesService/DeleteIndex` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/ExecuteTransactions` | SERVER | member, admin, owner | databases.write | business_write | |
| `/torchwood.server.v1.DatabasesService/ExportCollectionSchema` | SERVER | 不限角色 | databases.read | read_only | |
| `/torchwood.server.v1.DatabasesService/GetCollection` | SERVER | 不限角色 | databases.read | read_only | |
| `/torchwood.server.v1.DatabasesService/GetDatabase` | SERVER | 不限角色 | databases.read | read_only | |
| `/torchwood.server.v1.DatabasesService/GetDocument` | SERVER | 不限角色 | databases.read | read_only | |
| `/torchwood.server.v1.DatabasesService/ListChanges` | SERVER | 不限角色 | databases.read | read_only | |
| `/torchwood.server.v1.DatabasesService/ListCollections` | SERVER | 不限角色 | databases.read | read_only | |
| `/torchwood.server.v1.DatabasesService/ListDatabases` | SERVER | 不限角色 | databases.read | read_only | |
| `/torchwood.server.v1.DatabasesService/ListDocuments` | SERVER | 不限角色 | databases.read | read_only | |
| `/torchwood.server.v1.DatabasesService/MigrateAttribute` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/RestoreAttribute` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/RetireAttribute` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/UpdateCollection` | SERVER | admin, owner | databases.write | delegated_platform | |
| `/torchwood.server.v1.DatabasesService/UpdateDocument` | SERVER | member, admin, owner | databases.write | business_write | |
| `/torchwood.server.v1.DatabasesService/UpsertDocument` | SERVER | member, admin, owner | databases.write | business_write | |

## /torchwood.server.v1.FunctionsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.FunctionsService/CreateDeployment` | SERVER | admin, owner | functions.write | delegated_platform | |
| `/torchwood.server.v1.FunctionsService/CreateExecution` | SERVER | admin, owner | functions.write | delegated_platform | |
| `/torchwood.server.v1.FunctionsService/CreateFunction` | SERVER | admin, owner | functions.write | delegated_platform | |
| `/torchwood.server.v1.FunctionsService/CreateFunctionTrigger` | SERVER | admin, owner | functions.write | delegated_platform | |
| `/torchwood.server.v1.FunctionsService/DeleteDeployment` | SERVER | admin, owner | functions.write | delegated_platform | |
| `/torchwood.server.v1.FunctionsService/DeleteFunction` | SERVER | admin, owner | functions.write | delegated_platform | |
| `/torchwood.server.v1.FunctionsService/DeleteFunctionTrigger` | SERVER | admin, owner | functions.write | delegated_platform | |
| `/torchwood.server.v1.FunctionsService/GetDeployment` | SERVER | 不限角色 | functions.read | read_only | |
| `/torchwood.server.v1.FunctionsService/GetExecution` | SERVER | 不限角色 | functions.read | read_only | |
| `/torchwood.server.v1.FunctionsService/GetFunction` | SERVER | 不限角色 | functions.read | read_only | |
| `/torchwood.server.v1.FunctionsService/GetVariables` | SERVER | 不限角色 | functions.read | read_only | |
| `/torchwood.server.v1.FunctionsService/ListDeployments` | SERVER | 不限角色 | functions.read | read_only | |
| `/torchwood.server.v1.FunctionsService/ListExecutions` | SERVER | 不限角色 | functions.read | read_only | |
| `/torchwood.server.v1.FunctionsService/ListFunctionTriggers` | SERVER | 不限角色 | functions.read | read_only | |
| `/torchwood.server.v1.FunctionsService/ListFunctions` | SERVER | 不限角色 | functions.read | read_only | |
| `/torchwood.server.v1.FunctionsService/ListRuntimes` | SERVER | 不限角色 | functions.read | read_only | |
| `/torchwood.server.v1.FunctionsService/ListSpecifications` | SERVER | 不限角色 | functions.read | read_only | |
| `/torchwood.server.v1.FunctionsService/RotateFunctionTriggerToken` | SERVER | admin, owner | functions.write | delegated_platform | |
| `/torchwood.server.v1.FunctionsService/SetFunctionScopes` | SERVER | admin, owner | functions.write | delegated_platform | |
| `/torchwood.server.v1.FunctionsService/SetVariables` | SERVER | admin, owner | functions.write | delegated_platform | |
| `/torchwood.server.v1.FunctionsService/UpdateFunction` | SERVER | admin, owner | functions.write | delegated_platform | |

## /torchwood.server.v1.GroupsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.GroupsService/CreateGroup` | SERVER | member, admin, owner | groups.write | business_write | |
| `/torchwood.server.v1.GroupsService/CreateMembership` | SERVER | member, admin, owner | groups.write | business_write | |
| `/torchwood.server.v1.GroupsService/DeleteGroup` | SERVER | member, admin, owner | groups.write | business_write | |
| `/torchwood.server.v1.GroupsService/DeleteMembership` | SERVER | member, admin, owner | groups.write | business_write | |
| `/torchwood.server.v1.GroupsService/GetGroup` | SERVER | 不限角色 | groups.read | read_only | |
| `/torchwood.server.v1.GroupsService/GetGroupPrefs` | SERVER | 不限角色 | groups.read | read_only | |
| `/torchwood.server.v1.GroupsService/GetMembership` | SERVER | 不限角色 | groups.read | read_only | |
| `/torchwood.server.v1.GroupsService/ListGroups` | SERVER | 不限角色 | groups.read | read_only | |
| `/torchwood.server.v1.GroupsService/ListMemberships` | SERVER | 不限角色 | groups.read | read_only | |
| `/torchwood.server.v1.GroupsService/UpdateGroupPrefs` | SERVER | member, admin, owner | groups.write | business_write | |
| `/torchwood.server.v1.GroupsService/UpdateMembership` | SERVER | member, admin, owner | groups.write | business_write | |
| `/torchwood.server.v1.GroupsService/UpdateMembershipStatus` | SERVER | member, admin, owner | groups.write | business_write | |

## /torchwood.server.v1.HealthService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.HealthService/Check` | PUBLIC | — | — | — | |
| `/torchwood.server.v1.HealthService/GetVersion` | PUBLIC | — | — | — | |

## /torchwood.server.v1.OAuthProvidersService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.OAuthProvidersService/DeleteOAuthProvider` | SERVER | admin, owner | oauthproviders.write | delegated_platform | |
| `/torchwood.server.v1.OAuthProvidersService/ListOAuthProviders` | SERVER | 不限角色 | oauthproviders.read | read_only | |
| `/torchwood.server.v1.OAuthProvidersService/UpsertOAuthProvider` | SERVER | admin, owner | oauthproviders.write | delegated_platform | |

## /torchwood.server.v1.OutboxService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.OutboxService/ListDeadLetters` | SERVER | admin, owner | outbox.read | delegated_platform | |
| `/torchwood.server.v1.OutboxService/ReplayDeadLetter` | SERVER | admin, owner | outbox.write | delegated_platform | |

## /torchwood.server.v1.PaymentsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.PaymentsService/GetOrder` | SERVER | 不限角色 | payments.read | read_only | |
| `/torchwood.server.v1.PaymentsService/ListOrders` | SERVER | 不限角色 | payments.read | read_only | |
| `/torchwood.server.v1.PaymentsService/ManualFulfill` | SERVER | admin, owner | payments.write | delegated_platform | |
| `/torchwood.server.v1.PaymentsService/Refund` | SERVER | admin, owner | payments.write | delegated_platform | |

## /torchwood.server.v1.ProjectsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.ProjectsService/CreateInviteCode` | PERMISSION | owner, admin | — | platform_only | |
| `/torchwood.server.v1.ProjectsService/CreateProject` | PERMISSION | owner, admin | — | platform_only | |
| `/torchwood.server.v1.ProjectsService/DeleteInviteCode` | PERMISSION | owner, admin | — | platform_only | |
| `/torchwood.server.v1.ProjectsService/DeleteProject` | PERMISSION | owner, admin | — | platform_only | |
| `/torchwood.server.v1.ProjectsService/GetProject` | SERVER | 不限角色 | projects.read | read_only | |
| `/torchwood.server.v1.ProjectsService/ListInviteCodes` | PERMISSION | owner, admin | — | platform_only | |
| `/torchwood.server.v1.ProjectsService/ListProjects` | SERVER | 不限角色 | projects.read | read_only | |
| `/torchwood.server.v1.ProjectsService/UpdateOAuthRedirectAllowlist` | PERMISSION | owner, admin | — | platform_only | |
| `/torchwood.server.v1.ProjectsService/UpdateProject` | SERVER | member, admin, owner | projects.write | business_write | |

## /torchwood.server.v1.StorageService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.StorageService/CreateBucket` | SERVER | member, admin, owner | storage.write | business_write | |
| `/torchwood.server.v1.StorageService/CreateFile` | SERVER | member, admin, owner | storage.write | business_write | |
| `/torchwood.server.v1.StorageService/CreateFileToken` | SERVER | member, admin, owner | storage.write | business_write | |
| `/torchwood.server.v1.StorageService/DeleteBucket` | SERVER | member, admin, owner | storage.write | business_write | |
| `/torchwood.server.v1.StorageService/DeleteFile` | SERVER | member, admin, owner | storage.write | business_write | |
| `/torchwood.server.v1.StorageService/GetBucket` | SERVER | 不限角色 | storage.read | read_only | |
| `/torchwood.server.v1.StorageService/GetFile` | SERVER | 不限角色 | storage.read | read_only | |
| `/torchwood.server.v1.StorageService/GetStorageUsage` | SERVER | 不限角色 | storage.read | read_only | |
| `/torchwood.server.v1.StorageService/ListBuckets` | SERVER | 不限角色 | storage.read | read_only | |
| `/torchwood.server.v1.StorageService/ListFiles` | SERVER | 不限角色 | storage.read | read_only | |
| `/torchwood.server.v1.StorageService/UpdateBucket` | SERVER | member, admin, owner | storage.write | business_write | |
| `/torchwood.server.v1.StorageService/UpdateFile` | SERVER | member, admin, owner | storage.write | business_write | |

## /torchwood.server.v1.SubscriptionsService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.SubscriptionsService/CancelSubscription` | SERVER | admin, owner | subscriptions.write | delegated_platform | |
| `/torchwood.server.v1.SubscriptionsService/CreatePlan` | SERVER | member, admin, owner | subscriptions.write | business_write | |
| `/torchwood.server.v1.SubscriptionsService/DeletePlan` | SERVER | member, admin, owner | subscriptions.write | business_write | |
| `/torchwood.server.v1.SubscriptionsService/ExpireSubscription` | SERVER | admin, owner | subscriptions.write | delegated_platform | |
| `/torchwood.server.v1.SubscriptionsService/GetPlan` | SERVER | 不限角色 | subscriptions.read | read_only | |
| `/torchwood.server.v1.SubscriptionsService/GetSubscription` | SERVER | 不限角色 | subscriptions.read | read_only | |
| `/torchwood.server.v1.SubscriptionsService/ListPlans` | SERVER | 不限角色 | subscriptions.read | read_only | |
| `/torchwood.server.v1.SubscriptionsService/ListSubscriptions` | SERVER | 不限角色 | subscriptions.read | read_only | |
| `/torchwood.server.v1.SubscriptionsService/UpdatePlan` | SERVER | member, admin, owner | subscriptions.write | business_write | |

## /torchwood.server.v1.UsersService

| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `/torchwood.server.v1.UsersService/CreateUser` | SERVER | member, admin, owner | users.write | business_write | |
| `/torchwood.server.v1.UsersService/CreateUserToken` | SERVER | member, admin, owner | users.write | business_write | |
| `/torchwood.server.v1.UsersService/DeleteUser` | SERVER | member, admin, owner | users.write | business_write | |
| `/torchwood.server.v1.UsersService/DeleteUserSession` | SERVER | member, admin, owner | users.write | business_write | |
| `/torchwood.server.v1.UsersService/GetUser` | SERVER | 不限角色 | users.read | read_only | |
| `/torchwood.server.v1.UsersService/ListUserSessions` | SERVER | 不限角色 | users.read | read_only | |
| `/torchwood.server.v1.UsersService/ListUsers` | SERVER | 不限角色 | users.read | read_only | |
| `/torchwood.server.v1.UsersService/UpdateUser` | SERVER | member, admin, owner | users.write | business_write | |
| `/torchwood.server.v1.UsersService/UpdateUserPassword` | SERVER | member, admin, owner | users.write | business_write | |

## 附录：威胁模型已知取舍（静态文本）

以下条目无法从策略注册表（PolicySet）派生，属安全评审确认接受的已知取舍；
本节为渲染器内置静态文本，任何一项变更需重新过安全评审：

- **限流与熔断 fail-open**：限流器基础设施（Redis）故障进入熔断放行态时，
  请求跳过限流判定直接放行（可用性优先于精确限流）。攻击者打挂 Redis 可
  换取限流旁路窗口；鉴权链路本身不受影响（fail-closed）。
- **匿名公开读（guests 显式授予）**：PUBLIC 级方法与文档 ACL 的 guests 角色
  属"公开读"语义，匿名流量按显式授权放行，是产品设计而非漏洞；集合/文档级
  收紧由 ACL 声明负责。
- **file token 上限 1 小时、颁发后不可撤销**：存储下载/预览 token 为无状态
  HMAC 签名（服务端不保存颁发记录），有效期上限 1 小时（默认 15 分钟）；
  泄露窗口上限即剩余有效期，无吊销通道，依赖短有效期兜底。
- **单 jwt.secret**：端用户会话、console 会话与 file token 的签名密钥均从
  唯一 `security.jwt.secret` 派生（tw_roles 的 HMAC 亦同源）；该密钥泄露
  等于全部凭证面同时失守，轮换窗口内旧 token 不失效。
