import {
  AccountService,
  ClientAssetsService,
  ClientDatabasesService,
  ClientPaymentsService,
  ClientSubscriptionsService,
  ClientGroupsService,
  RealtimeService,
} from "./client/index.js";
import type { TorchwoodConfig } from "./http.js";
import { HttpTransport } from "./http.js";
import {
  APIKeysService,
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
  readonly databases: ClientDatabasesService;
  readonly groups: ClientGroupsService;
  readonly realtime: RealtimeService;
  readonly payments: ClientPaymentsService;
  readonly assets: ClientAssetsService;
  readonly subscriptions: ClientSubscriptionsService;

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
    subscriptions: ServerSubscriptionsService;
    billing: BillingService;
    outbox: OutboxService;
  };

  private readonly transport: HttpTransport;

  constructor(config: TorchwoodConfig) {
    this.transport = new HttpTransport(config);
    this.account = new AccountService(this.transport);
    this.databases = new ClientDatabasesService(this.transport);
    this.groups = new ClientGroupsService(this.transport);
    this.realtime = new RealtimeService(this.transport);
    this.payments = new ClientPaymentsService(this.transport);
    this.assets = new ClientAssetsService(this.transport);
    this.subscriptions = new ClientSubscriptionsService(this.transport);
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
      subscriptions: new ServerSubscriptionsService(this.transport),
      billing: new BillingService(this.transport),
      outbox: new OutboxService(this.transport),
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

  setAccessToken(token: string | undefined): void {
    this.transport.setAccessToken(token);
  }

  getAccessToken(): string | undefined {
    return this.transport.getAccessToken();
  }

  getProjectId(): string {
    return this.transport.getProjectId();
  }
}
