import {
  AccountService,
  ClientAnalyticsService,
  ClientAssetsService,
  ClientDatabasesService,
  ClientFunctionsService,
  ClientPaymentsService,
  ClientSubscriptionsService,
  ClientGroupsService,
  ClientLeaderboardsService,
  RealtimeService,
} from "./client/index.js";
import type { TorchwoodConfig } from "./http.js";
import { HttpTransport } from "./http.js";
import { TorchwoodError } from "./errors.js";
import {
  AnalyticsService,
  APIKeysService,
  AuditLogsService,
  FunctionsService,
  HealthService,
  OAuthProvidersService,
  ProjectsService,
  BillingService,
  ServerAssetsService,
  ServerDatabasesService,
  ServerPaymentsService,
  ServerSubscriptionsService,
  ServerGroupsService,
  StorageService,
  UsersService,
  OutboxService,
  ServerLeaderboardsService,
} from "./server/index.js";

export type { TorchwoodConfig } from "./http.js";
export { TorchwoodError } from "./errors.js";
export { accountsChannel } from "./client/realtime.js";
export type {
  RealtimeConnectOptions,
  RealtimeConnection,
  RealtimeEvent,
  RealtimeHandler,
  RealtimeStatus,
  RealtimeSubscription,
  RealtimeWebSocket,
} from "./client/realtime.js";
export * from "./types.js";

export class Torchwood {
  readonly account: AccountService;
  readonly analytics: ClientAnalyticsService;
  readonly databases: ClientDatabasesService;
  readonly groups: ClientGroupsService;
  readonly realtime: RealtimeService;
  readonly payments: ClientPaymentsService;
  readonly assets: ClientAssetsService;
  readonly leaderboards: ClientLeaderboardsService;
  readonly subscriptions: ClientSubscriptionsService;
  readonly functions: ClientFunctionsService;

  readonly server: {
    health: HealthService;
    projects: ProjectsService;
    users: UsersService;
    groups: ServerGroupsService;
    databases: ServerDatabasesService;
    apiKeys: APIKeysService;
    oauthProviders: OAuthProvidersService;
    storage: StorageService;
    functions: FunctionsService;
    payments: ServerPaymentsService;
    assets: ServerAssetsService;
    leaderboards: ServerLeaderboardsService;
    subscriptions: ServerSubscriptionsService;
    billing: BillingService;
    outbox: OutboxService;
    auditLogs: AuditLogsService;
    analytics: AnalyticsService;
  };

  private readonly transport: HttpTransport;

  constructor(config: TorchwoodConfig) {
    this.transport = new HttpTransport(config);
    this.account = new AccountService(this.transport);
    this.analytics = new ClientAnalyticsService(this.transport);
    this.databases = new ClientDatabasesService(this.transport);
    this.groups = new ClientGroupsService(this.transport);
    this.realtime = new RealtimeService(this.transport);
    this.payments = new ClientPaymentsService(this.transport);
    this.assets = new ClientAssetsService(this.transport);
    this.leaderboards = new ClientLeaderboardsService(this.transport);
    this.subscriptions = new ClientSubscriptionsService(this.transport);
    this.functions = new ClientFunctionsService(this.transport);
    this.server = {
      health: new HealthService(this.transport),
      projects: new ProjectsService(this.transport),
      users: new UsersService(this.transport),
      groups: new ServerGroupsService(this.transport),
      databases: new ServerDatabasesService(this.transport),
      apiKeys: new APIKeysService(this.transport),
      oauthProviders: new OAuthProvidersService(this.transport),
      storage: new StorageService(this.transport),
      functions: new FunctionsService(this.transport),
      payments: new ServerPaymentsService(this.transport),
      assets: new ServerAssetsService(this.transport),
      leaderboards: new ServerLeaderboardsService(this.transport),
      subscriptions: new ServerSubscriptionsService(this.transport),
      billing: new BillingService(this.transport),
      outbox: new OutboxService(this.transport),
      auditLogs: new AuditLogsService(this.transport),
      analytics: new AnalyticsService(this.transport),
    };
  }

  static create(config: TorchwoodConfig): Torchwood {
    return new Torchwood(config);
  }

  /** Server API + optional Client API with a project API key. */
  static withApiKey(endpoint: string, projectId: string, apiKey: string): Torchwood {
    return new Torchwood({ endpoint, projectId, apiKey });
  }

  /** Client API with an existing user access token. */
  static withAccessToken(endpoint: string, projectId: string, accessToken: string): Torchwood {
    return new Torchwood({ endpoint, projectId, accessToken });
  }

  /**
   * 函数内入口（docs/design/functions-v3.md §5.1）：以函数执行身份
   * （execution principal）构造 client，方法面 = server 服务类全量
   * （assets grant / databases / users / functions ...）。
   *
   * executionToken 来源优先级：显式参数 > `process.env.TW_EXECUTION_TOKEN`
   * （runner 在执行期注入；D4——env 仅同步段读取安全，并发函数请改用
   * fetch 风格 `new Torchwood({ executionToken })` 的参数通道）。
   * 两者皆缺时抛错（fail-closed，不做无身份调用）。
   */
  static fromExecution(
    apiBaseUrl: string,
    opts?: { executionToken?: string; fetch?: typeof fetch }
  ): Torchwood {
    const token = opts?.executionToken ?? readEnvExecutionToken();
    if (!token) {
      throw new TorchwoodError(
        "Execution token is required: pass opts.executionToken or set TW_EXECUTION_TOKEN (functions-v3.md §5.1)",
        0
      );
    }
    return new Torchwood({
      endpoint: apiBaseUrl,
      executionToken: token,
      fetch: opts?.fetch,
    });
  }

  setAccessToken(token: string | undefined): void {
    this.transport.setAccessToken(token);
  }

  getAccessToken(): string | undefined {
    return this.transport.getAccessToken();
  }

  setExecutionToken(token: string | undefined): void {
    this.transport.setExecutionToken(token);
  }

  getExecutionToken(): string | undefined {
    return this.transport.getExecutionToken();
  }

  getProjectId(): string {
    return this.transport.getProjectId();
  }
}

/**
 * readEnvExecutionToken 读取 TW_EXECUTION_TOKEN（runner 注入的执行身份凭证）。
 * 独立函数便于测试注入；浏览器环境（无 process）返回 undefined。
 */
function readEnvExecutionToken(): string | undefined {
  const proc = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process;
  const token = proc?.env?.TW_EXECUTION_TOKEN;
  return token !== undefined && token !== "" ? token : undefined;
}
