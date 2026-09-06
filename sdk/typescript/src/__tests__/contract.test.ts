import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync, existsSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

import { Torchwood } from "../graviton.js";
import { AccountService } from "../client/account.js";
import { ClientAssetsService } from "../client/assets.js";
import { ClientDatabasesService } from "../client/databases.js";
import { ClientPaymentsService } from "../client/payments.js";
import { ClientSubscriptionsService } from "../client/subscriptions.js";
import { ClientGroupsService } from "../client/groups.js";
import { HttpTransport } from "../http.js";
import {
  APIKeysService,
  FunctionsService,
  HealthService,
  OAuthProvidersService,
  OutboxService,
  ProjectsService,
  BillingService,
  ServerAssetsService,
  ServerDatabasesService,
  ServerPaymentsService,
  ServerSubscriptionsService,
  ServerGroupsService,
  StorageService,
  UsersService,
} from "../server/index.js";

// ---- 契约测试：proto（swagger.json 产物）↔ TS SDK 方法集合比对（F11-2）----
// 前提：仓库根目录执行过 `task generate-proto`（genproto/**/*.swagger.json 存在）。
// 任一 proto 新增 RPC 后，本测试失败并提示在 SDK 补齐对应方法。

const GENPROTO_DIR = join(dirname(fileURLToPath(import.meta.url)), "../../../../genproto");

// 服务 → TS SDK 类（swagger 的 operationId 格式为 "{Service}_{RPC}"）。
// 仅用于读取 prototype 方法，构造参数类型不参与检查。
interface ClassLike {
  prototype: object;
}
const SDK_SERVICES: Record<string, ClassLike> = {
  // Server API
  DatabasesService: ServerDatabasesService,
  FunctionsService: FunctionsService,
  StorageService: StorageService,
  GroupsService: ServerGroupsService,
  UsersService: UsersService,
  APIKeysService: APIKeysService,
  OAuthProvidersService: OAuthProvidersService,
  ProjectsService: ProjectsService,
  HealthService: HealthService,
  PaymentsService: ServerPaymentsService,
  AssetsService: ServerAssetsService,
  SubscriptionsService: ServerSubscriptionsService,
  BillingService: BillingService,
  OutboxService: OutboxService,
  // Client API（swagger 服务名与 Server API 重名，加 client. 前缀区分）
  "client.AccountService": AccountService,
  "client.DatabasesService": ClientDatabasesService,
  "client.GroupsService": ClientGroupsService,
  "client.PaymentsService": ClientPaymentsService,
  "client.AssetsService": ClientAssetsService,
  "client.SubscriptionsService": ClientSubscriptionsService,
};

// RPC 名 → TS SDK 方法名（SDK 使用简短命名，与 proto 非一一对应，显式登记）。
// grpc-gateway 对同一 RPC 的额外 HTTP 绑定会在 operationId 追加数字后缀
// （如 documents:count 的 GET/POST 双绑定产生 DatabasesService_CountDocuments2），
// 契约检查按剥离后缀后的 RPC 名进行。
function parseOperationId(operationId: string): [service: string, rpc: string] {
  const [service, rpc] = operationId.split("_");
  return [service, rpc.replace(/\d+$/, "")];
}

const RPC_TO_METHOD: Record<string, Record<string, string>> = {
  DatabasesService: {
    CreateDatabase: "createDatabase",
    ListDatabases: "listDatabases",
    GetDatabase: "getDatabase",
    DeleteDatabase: "deleteDatabase",
    CreateCollection: "createCollection",
    ListCollections: "listCollections",
    GetCollection: "getCollection",
    DeleteCollection: "deleteCollection",
    UpdateCollection: "updateCollection",
    ExportCollectionSchema: "exportCollectionSchema",
    CreateAttribute: "createAttribute",
    DeleteAttribute: "deleteAttribute",
    RestoreAttribute: "restoreAttribute",
    RetireAttribute: "retireAttribute",
    MigrateAttribute: "migrateAttribute",
    CreateIndex: "createIndex",
    DeleteIndex: "deleteIndex",
    CreateDocument: "createDocument",
    ListDocuments: "listDocuments",
    GetDocument: "getDocument",
    UpdateDocument: "updateDocument",
    UpsertDocument: "upsertDocument",
    DeleteDocument: "deleteDocument",
    CountDocuments: "countDocuments",
    AggregateDocuments: "aggregateDocuments",
    BulkUpdateDocuments: "bulkUpdateDocuments",
    BulkDeleteDocuments: "bulkDeleteDocuments",
    ExecuteTransactions: "executeTransactions",
    ListChanges: "listChanges",
  },
  FunctionsService: {
    ListRuntimes: "listRuntimes",
    ListSpecifications: "listSpecifications",
    CreateFunction: "create",
    ListFunctions: "list",
    GetFunction: "get",
    UpdateFunction: "update",
    DeleteFunction: "delete",
    CreateDeployment: "createDeployment",
    ListDeployments: "listDeployments",
    GetDeployment: "getDeployment",
    DeleteDeployment: "deleteDeployment",
    SetVariables: "setVariables",
    GetVariables: "getVariables",
    CreateExecution: "createExecution",
    ListExecutions: "listExecutions",
    GetExecution: "getExecution",
  },
  StorageService: {
    CreateBucket: "createBucket",
    ListBuckets: "listBuckets",
    GetBucket: "getBucket",
    DeleteBucket: "deleteBucket",
    UpdateBucket: "updateBucket",
    CreateFile: "uploadFile",
    ListFiles: "listFiles",
    GetFile: "getFile",
    DeleteFile: "deleteFile",
    UpdateFile: "updateFile",
    CreateFileToken: "createFileToken",
    GetStorageUsage: "getStorageUsage",
  },
  GroupsService: {
    CreateGroup: "create",
    ListGroups: "list",
    GetGroup: "get",
    DeleteGroup: "delete",
    GetGroupPrefs: "getPrefs",
    UpdateGroupPrefs: "updatePrefs",
    CreateMembership: "createMembership",
    ListMemberships: "listMemberships",
    GetMembership: "getMembership",
    UpdateMembership: "updateMembership",
    UpdateMembershipStatus: "updateMembershipStatus",
    DeleteMembership: "deleteMembership",
  },
  UsersService: {
    CreateUser: "create",
    ListUsers: "list",
    GetUser: "get",
    UpdateUser: "update",
    UpdateUserPassword: "updatePassword",
    DeleteUser: "delete",
    ListUserSessions: "listSessions",
    DeleteUserSession: "deleteSession",
    CreateUserToken: "createToken",
  },
  APIKeysService: {
    CreateAPIKey: "create",
    ListAPIKeys: "list",
    GetAPIKey: "get",
    DeleteAPIKey: "delete",
  },
  OAuthProvidersService: {
    ListOAuthProviders: "list",
    UpsertOAuthProvider: "upsert",
    DeleteOAuthProvider: "delete",
  },
  ProjectsService: {
    CreateProject: "create",
    ListProjects: "list",
    GetProject: "get",
    UpdateProject: "update",
    DeleteProject: "delete",
  },
  HealthService: {
    Check: "check",
    Check2: "check", // additional_bindings 生成 operationId 后缀（/v1/server/health）
    GetVersion: "getVersion",
  },
  "client.AccountService": {
    SignUp: "signUp",
    SignIn: "signIn",
    SignOut: "signOut",
    RefreshToken: "refresh",
    Me: "me",
    UpdateAccount: "updateAccount",
    ConfirmEmailChange: "confirmEmailChange",
    ListSessions: "listSessions",
    DeleteSession: "deleteSession",
    DeleteSessions: "deleteSessions",
    GetPrefs: "getPrefs",
    UpdatePrefs: "updatePrefs",
    CreateEmailOTP: "createEmailOTP",
    CreateEmailOTPSession: "createEmailOTPSession",
    CreateOAuth2Session: "createOAuth2Session",
    CreateOAuth2TokenSession: "createOAuth2TokenSession",
    CreatePhoneOTP: "createPhoneOTP",
    CreatePhoneOTPSession: "createPhoneOTPSession",
    CreateWeChatMiniProgramSession: "createWeChatMiniProgramSession",
    CreateAnonymousSession: "createAnonymousSession",
    CreateOAuth2LinkSession: "createOAuth2LinkSession",
    CreateOAuth2LinkTokenSession: "createOAuth2LinkTokenSession",
    CreateVerification: "createVerification",
    UpdateVerification: "updateVerification",
    CreateRecovery: "createRecovery",
    UpdateRecovery: "updateRecovery",
    ListFactors: "listFactors",
    CreateTOTPFactor: "createTOTPFactor",
    VerifyTOTPFactor: "verifyTOTPFactor",
    DeleteFactor: "deleteFactor",
    CreateMFASession: "createMFASession",
    CreateJWT: "createJWT",
    CreateMagicURLSession: "createMagicURLSession",
    UpdateMagicURLSession: "updateMagicURLSession",
    ListLogs: "listLogs",
  },
  // Client API（swagger 文件内服务名为 DatabasesService / GroupsService，
  // 与 Server API 重名，SDK 类为 Client* 前缀，见 loadServiceFiles 重映射）。
  "client.DatabasesService": {
    CreateDocument: "createDocument",
    ListDocuments: "listDocuments",
    GetDocument: "getDocument",
    UpdateDocument: "updateDocument",
    UpsertDocument: "upsertDocument",
    DeleteDocument: "deleteDocument",
    CountDocuments: "countDocuments",
    ListChanges: "listChanges",
  },
  "client.GroupsService": {
    CreateGroup: "createGroup",
    ListGroups: "listGroups",
    GetGroup: "getGroup",
    DeleteGroup: "deleteGroup",
    CreateMembership: "createMembership",
    ListMemberships: "listMemberships",
    UpdateMembershipStatus: "updateMembershipStatus",
    DeleteMembership: "deleteMembership",
  },
  PaymentsService: {
    ListOrders: "listOrders",
    GetOrder: "getOrder",
    Refund: "refund",
    ManualFulfill: "manualFulfill",
  },
  AssetsService: {
    CreateAssetDef: "createAssetDef",
    ListAssetDefs: "listAssetDefs",
    GetAssetDef: "getAssetDef",
    UpdateAssetDef: "updateAssetDef",
    DeleteAssetDef: "deleteAssetDef",
    Grant: "grant",
    Consume: "consume",
    Transfer: "transfer",
    Mutate: "mutate",
    Expire: "expire",
    Reconcile: "reconcile",
    ListUserAssets: "listUserAssets",
    ListUserLedger: "listUserLedger",
  },
  SubscriptionsService: {
    CreatePlan: "createPlan",
    ListPlans: "listPlans",
    GetPlan: "getPlan",
    UpdatePlan: "updatePlan",
    DeletePlan: "deletePlan",
    ListSubscriptions: "listSubscriptions",
    GetSubscription: "getSubscription",
    CancelSubscription: "cancelSubscription",
    ExpireSubscription: "expireSubscription",
  },
  BillingService: {
    GetUsage: "getUsage",
    ListRollups: "listRollups",
    ListStatements: "listStatements",
  },
  OutboxService: {
    ListDeadLetters: "listDeadLetters",
    ReplayDeadLetter: "replayDeadLetter",
  },
  "client.PaymentsService": {
    CreateOrder: "createOrder",
    GetMyOrder: "getMyOrder",
    ListMyOrders: "listMyOrders",
    VerifyReceipt: "verifyReceipt",
  },
  "client.AssetsService": {
    ListAssetDefs: "listAssetDefs",
    ListMyAssets: "listMyAssets",
    ListMyAssetLedger: "listMyAssetLedger",
  },
  "client.SubscriptionsService": {
    ListPlans: "listPlans",
    Subscribe: "subscribe",
    GetMySubscription: "getMySubscription",
    Cancel: "cancel",
  },
};

// securityDefinitions 中必须存在的三个统一 scheme 名（F11-1）。
const REQUIRED_SCHEMES = ["apiKey", "Bearer", "cookie"];

// 服务默认 access（swagger 顶层 x-torchwood-access）→ 期望的全局 security。
const EXPECTED_GLOBAL_SECURITY: Record<string, string[]> = {
  server: ["apiKey"],
  end_user: ["Bearer"],
  permission: ["cookie"],
  public: [],
};

interface SwaggerFile {
  name: string;
  doc: {
    securityDefinitions?: Record<string, unknown>;
    security?: Array<Record<string, unknown>>;
    "x-torchwood-access"?: string;
    definitions?: Record<
      string,
      { properties?: Record<string, unknown>; required?: string[] }
    >;
    paths?: Record<
      string,
      Record<
        string,
        {
          operationId?: string;
          security?: unknown[];
          "x-torchwood-access"?: string;
          parameters?: Array<{
            name: string;
            in: string;
            required?: boolean;
            schema?: {
              $ref?: string;
              properties?: Record<string, unknown>;
              required?: string[];
            };
          }>;
        }
      >
    >;
  };
}

function loadSwaggerFiles(): SwaggerFile[] {
  const files: SwaggerFile[] = [];
  for (const sub of ["server", "client", "console", "shared"]) {
    const dir = join(GENPROTO_DIR, sub, "v1");
    if (!existsSync(dir)) continue;
    for (const f of readdirSync(dir).filter((n) => n.endsWith(".swagger.json"))) {
      files.push({ name: `${sub}/${f}`, doc: JSON.parse(readFileSync(join(dir, f), "utf8")) });
    }
  }
  return files;
}

describe("contract: swagger ↔ TS SDK", () => {
  const files = loadSwaggerFiles();
  assert.ok(files.length >= 14, `genproto swagger 文件缺失（当前 ${files.length} 个），请先执行 task generate-proto`);

  it("所有服务 swagger 声明统一 securityDefinitions", () => {
    for (const f of files) {
      // shared 包为纯消息定义（无 HTTP 绑定），不生成 security。
      if (f.name.startsWith("shared/")) continue;
      const defs = f.doc.securityDefinitions ?? {};
      for (const scheme of REQUIRED_SCHEMES) {
        assert.ok(defs[scheme], `${f.name} 缺少 securityDefinitions.${scheme}`);
      }
    }
  });

  it("顶层 x-torchwood-access 与全局 security 一致", () => {
    for (const f of files) {
      if (f.name.startsWith("shared/")) continue;
      const access = f.doc["x-torchwood-access"];
      assert.ok(access, `${f.name} 缺少顶层 x-torchwood-access`);
      const expected = EXPECTED_GLOBAL_SECURITY[access!];
      assert.ok(expected, `${f.name} 的 x-torchwood-access=${access} 非法`);
      const sec = (f.doc.security ?? []).map((r) => Object.keys(r)[0]);
      assert.deepEqual(sec.sort(), [...expected].sort(), `${f.name} 全局 security 与 ${access} 不符`);
    }
  });

  it("ACCESS_PUBLIC 方法必须声明 security: []（匿名）", () => {
    for (const f of files) {
      for (const path of Object.values(f.doc.paths ?? {})) {
        for (const op of Object.values(path)) {
          if (op?.["x-torchwood-access"] === "public") {
            assert.deepEqual(op.security ?? [], [], `${f.name} ${op.operationId} 公开方法必须 security: []`);
          }
        }
      }
    }
  });

  it("proto RPC 集合 ⊆ TS SDK 方法集合", () => {
    let rpcCount = 0;
    for (const f of files) {
      if (!f.name.startsWith("server/") && !f.name.startsWith("client/")) continue;
      const isClient = f.name.startsWith("client/");
      for (const path of Object.values(f.doc.paths ?? {})) {
        for (const op of Object.values(path)) {
          const operationId = op?.operationId;
          if (!operationId) continue;
          const [service, rpc] = parseOperationId(operationId);
          const key = isClient ? `client.${service}` : service;
          const cls = SDK_SERVICES[key];
          assert.ok(cls, `${f.name} 未登记服务 ${service} 的 SDK 映射`);
          const method = RPC_TO_METHOD[key]?.[rpc];
          assert.ok(method, `${f.name} ${operationId} 未登记 RPC→方法映射`);
          const proto = cls.prototype as unknown as Record<string, unknown>;
          assert.equal(
            typeof proto[method],
            "function",
            `${operationId} 应映射到 ${key}.${method}()`
          );
          rpcCount++;
        }
      }
    }
    // 服务器 80 + 客户端 34 的基线，防误删。
    assert.ok(rpcCount >= 110, `RPC 总数异常（当前 ${rpcCount}），请核对 genproto 产物`);
  });

// Server swagger 服务名 → Torchwood.server 门面属性名（graviton.ts）。
const FACADE_SERVICES: Record<string, string> = {
  DatabasesService: "databases",
  FunctionsService: "functions",
  StorageService: "storage",
  GroupsService: "groups",
  UsersService: "users",
  APIKeysService: "apiKeys",
  OAuthProvidersService: "oauthProviders",
  ProjectsService: "projects",
  HealthService: "health",
  PaymentsService: "payments",
  AssetsService: "assets",
  SubscriptionsService: "subscriptions",
  BillingService: "billing",
  OutboxService: "outbox",
};

// Round3 H4-1：Server swagger 的每个服务都必须经 `Torchwood.server.<svc>`
// 门面实例可达（直接 import 类的契约测试漏掉「门面未挂载」的洞——
// 此前 FunctionsService 实现存在但未挂到 Torchwood.server，常规 Agent
// 经 Torchwood.withApiKey() 调不到）。
it("Torchwood.server 门面可达全部 Server swagger 服务（含 functions）", () => {
  const tw = new Torchwood({ endpoint: "http://torchwood.test:9080", projectId: "p" });
  const facade = tw.server as unknown as Record<string, Record<string, unknown>>;
  let svcCount = 0;
  for (const f of files) {
    if (!f.name.startsWith("server/")) continue;
    for (const path of Object.values(f.doc.paths ?? {})) {
      for (const op of Object.values(path)) {
        const operationId = op?.operationId;
        if (!operationId) continue;
        const [service, rpc] = parseOperationId(operationId);
        const facadeKey = FACADE_SERVICES[service];
        assert.ok(facadeKey, `${f.name}: 未登记服务 ${service} 的门面映射`);
        const svc = facade[facadeKey];
        assert.ok(svc, `${f.name}: Torchwood.server.${facadeKey} 必须挂到门面`);
        const method = RPC_TO_METHOD[service]?.[rpc];
        assert.ok(method, `${f.name} ${operationId} 未登记 RPC→方法映射`);
        assert.equal(
          typeof svc[method],
          "function",
          `${operationId} 应经门面 Torchwood.server.${facadeKey}.${method}() 可达`
        );
        svcCount++;
      }
    }
  }
  assert.ok(svcCount >= 80, `Server 门面覆盖不足（当前 ${svcCount} 个 RPC）`);
});

  it("写方法 HTTP 绑定（method/path/query/body 参数）与 swagger 一致", async () => {
    // 从 swagger 提取 operationId → {method, path, parameters}，与 SDK 方法
    // 实际发出的请求（mock fetch 捕获）做结构化比对，防「方法同名但契约不同」。
    // server 与 client 的 swagger 存在同名 operationId（如 DatabasesService_CreateDocument），
    // 按 side 分别建索引，用例用 side 显式消歧。
    type OpEntry = {
      method: string;
      path: string;
      parameters: NonNullable<
        NonNullable<SwaggerFile["doc"]["paths"]>[string][string]["parameters"]
      >;
      bodyProps?: Record<string, unknown>;
      bodyRequired?: string[];
    };
    const opsBySide = new Map<"server" | "client", Map<string, OpEntry>>([
      ["server", new Map()],
      ["client", new Map()],
    ]);
    for (const f of files) {
      if (!f.name.startsWith("server/") && !f.name.startsWith("client/")) continue;
      const side = f.name.startsWith("server/") ? "server" : "client";
      const sideOps = opsBySide.get(side)!;
      for (const [path, ops] of Object.entries(f.doc.paths ?? {})) {
        for (const [method, op] of Object.entries(ops)) {
          if (!op?.operationId) continue;
          const bodyParam = op.parameters?.find((p) => p.in === "body");
          const entry: OpEntry = {
            method,
            path,
            parameters: op.parameters ?? [],
          };
          if (bodyParam?.schema) {
            if (bodyParam.schema.$ref) {
              const defName = bodyParam.schema.$ref.replace("#/definitions/", "");
              const def = f.doc.definitions?.[defName];
              if (def) {
                entry.bodyProps = def.properties;
                entry.bodyRequired = def.required;
              }
            } else {
              entry.bodyProps = bodyParam.schema.properties;
              entry.bodyRequired = bodyParam.schema.required;
            }
          }
          sideOps.set(op.operationId, entry);
        }
      }
    }

    const cases: {
      side: "server" | "client";
      operationId: string;
      invoke: (h: HttpTransport) => Promise<unknown>;
    }[] = [];
    const server = (h: HttpTransport) => ({
      users: new UsersService(h),
      db: new ServerDatabasesService(h),
      storage: new StorageService(h),
      functions: new FunctionsService(h),
      groups: new ServerGroupsService(h),
      projects: new ProjectsService(h),
      apiKeys: new APIKeysService(h),
      oauth: new OAuthProvidersService(h),
      payments: new ServerPaymentsService(h),
      assets: new ServerAssetsService(h),
      subscriptions: new ServerSubscriptionsService(h),
    });
    const client = (h: HttpTransport) => ({
      account: new AccountService(h),
      payments: new ClientPaymentsService(h),
      assets: new ClientAssetsService(h),
      subscriptions: new ClientSubscriptionsService(h),
    });

    // 覆盖各服务的 Create/Update/Delete 写方法（及代表性 Get/其他写方法）。
    cases.push(
      { side: "server", operationId: "UsersService_CreateUser", invoke: (h) => server(h).users.create({ email: "a@b.c", password: "pw", name: "A", status: "active", labels: { region: "cn" }, prefs: { lang: "zh" } }) },
      { side: "server", operationId: "UsersService_UpdateUser", invoke: (h) => server(h).users.update("u1", { name: "B", email_verified: true }) },
      { side: "server", operationId: "UsersService_DeleteUser", invoke: (h) => server(h).users.delete("u1") },
      { side: "server", operationId: "UsersService_UpdateUserPassword", invoke: (h) => server(h).users.updatePassword("u1", "new-pw") },
      { side: "server", operationId: "DatabasesService_CreateDatabase", invoke: (h) => server(h).db.createDatabase({ id: "app", name: "App" }) },
      { side: "server", operationId: "DatabasesService_DeleteDatabase", invoke: (h) => server(h).db.deleteDatabase("app") },
      { side: "server", operationId: "DatabasesService_CreateCollection", invoke: (h) => server(h).db.createCollection("app", { id: "members", name: "Members", permissions: ["read"], document_security: true }) },
      { side: "server", operationId: "DatabasesService_UpdateCollection", invoke: (h) => server(h).db.updateCollection("app", "members", { name: "M2", permissions: ["read:all"], document_security: false, disabled: true }) },
      { side: "server", operationId: "DatabasesService_DeleteCollection", invoke: (h) => server(h).db.deleteCollection("app", "members") },
      { side: "server", operationId: "DatabasesService_CreateDocument", invoke: (h) => server(h).db.createDocument("app", "members", { document_id: "d1", data: { n: 1 }, permissions: ["read"] }) },
      { side: "server", operationId: "DatabasesService_UpdateDocument", invoke: (h) => server(h).db.updateDocument("app", "members", "d1", { data: { n: 2 }, increment: { cnt: 1 }, version: 1 }) },
      { side: "server", operationId: "DatabasesService_DeleteDocument", invoke: (h) => server(h).db.deleteDocument("app", "members", "d1", 1) },
      { side: "server", operationId: "DatabasesService_BulkUpdateDocuments", invoke: (h) => server(h).db.bulkUpdateDocuments("app", "members", { document_ids: ["d1"], data: { x: 1 } }) },
      { side: "server", operationId: "DatabasesService_BulkDeleteDocuments", invoke: (h) => server(h).db.bulkDeleteDocuments("app", "members", ["d1"]) },
      { side: "server", operationId: "DatabasesService_CreateAttribute", invoke: (h) => server(h).db.createAttribute("app", "members", { key: "k", type: "string", size: 64, required: true }) },
      { side: "server", operationId: "DatabasesService_DeleteAttribute", invoke: (h) => server(h).db.deleteAttribute("app", "members", "k") },
      { side: "server", operationId: "DatabasesService_RestoreAttribute", invoke: (h) => server(h).db.restoreAttribute("app", "members", "k") },
      { side: "server", operationId: "DatabasesService_RetireAttribute", invoke: (h) => server(h).db.retireAttribute("app", "members", "k") },
      { side: "server", operationId: "DatabasesService_MigrateAttribute", invoke: (h) => server(h).db.migrateAttribute("app", "members", "k", { type: "string", size: 128 }) },
      { side: "server", operationId: "DatabasesService_CreateIndex", invoke: (h) => server(h).db.createIndex("app", "members", { id: "idx1", type: "unique", attributes: ["a"] }) },
      { side: "server", operationId: "DatabasesService_DeleteIndex", invoke: (h) => server(h).db.deleteIndex("app", "members", "idx1") },
      { side: "server", operationId: "StorageService_CreateBucket", invoke: (h) => server(h).storage.createBucket({ name: "b1", permissions: ["read"], public: true }) },
      { side: "server", operationId: "StorageService_UpdateBucket", invoke: (h) => server(h).storage.updateBucket("b1", { name: "b2", public: false }) },
      { side: "server", operationId: "StorageService_DeleteBucket", invoke: (h) => server(h).storage.deleteBucket("b1") },
      { side: "server", operationId: "StorageService_UpdateFile", invoke: (h) => server(h).storage.updateFile("b1", "f1", { name: "n", mime_type: "text/plain", metadata: { k: "v" } }) },
      { side: "server", operationId: "StorageService_DeleteFile", invoke: (h) => server(h).storage.deleteFile("b1", "f1") },
      { side: "server", operationId: "StorageService_CreateFileToken", invoke: (h) => server(h).storage.createFileToken("b1", "f1", { expires_in: 300 }) },
      { side: "server", operationId: "FunctionsService_CreateFunction", invoke: (h) => server(h).functions.create({ id: "fn1", name: "F", runtime: "nodejs20", entrypoint: "index.js", timeout_seconds: 30, spec: "shared-1x", enabled: true }) },
      { side: "server", operationId: "FunctionsService_UpdateFunction", invoke: (h) => server(h).functions.update("fn1", { name: "F2", enabled: false }) },
      { side: "server", operationId: "FunctionsService_DeleteFunction", invoke: (h) => server(h).functions.delete("fn1") },
      { side: "server", operationId: "FunctionsService_SetVariables", invoke: (h) => server(h).functions.setVariables("fn1", [{ key: "K", value: "V" }]) },
      { side: "server", operationId: "FunctionsService_GetVariables", invoke: (h) => server(h).functions.getVariables("fn1") },
      { side: "server", operationId: "GroupsService_CreateGroup", invoke: (h) => server(h).groups.create({ name: "T", permissions: ["read"] }) },
      { side: "server", operationId: "GroupsService_DeleteGroup", invoke: (h) => server(h).groups.delete("t1") },
      { side: "server", operationId: "GroupsService_UpdateGroupPrefs", invoke: (h) => server(h).groups.updatePrefs("t1", { locale: "zh" }) },
      { side: "server", operationId: "ProjectsService_CreateProject", invoke: (h) => server(h).projects.create({ id: "p", name: "P", description: "d" }) },
      { side: "server", operationId: "ProjectsService_UpdateProject", invoke: (h) => server(h).projects.update("p1", { name: "P2" }) },
      { side: "server", operationId: "ProjectsService_DeleteProject", invoke: (h) => server(h).projects.delete("p1") },
      { side: "server", operationId: "APIKeysService_CreateAPIKey", invoke: (h) => server(h).apiKeys.create({ name: "k", scopes: ["users"] }) },
      { side: "server", operationId: "APIKeysService_DeleteAPIKey", invoke: (h) => server(h).apiKeys.delete("k1") },
      { side: "server", operationId: "OAuthProvidersService_UpsertOAuthProvider", invoke: (h) => server(h).oauth.upsert({ provider: "github", enabled: true, client_id: "cid", client_secret: "sec", scopes: ["user"] }) },
      { side: "server", operationId: "OAuthProvidersService_DeleteOAuthProvider", invoke: (h) => server(h).oauth.delete("github") },
      { side: "client", operationId: "AccountService_UpdateAccount", invoke: (h) => client(h).account.updateAccount({ name: "N", email: "e@x.c", password: "pw", old_password: "old", url: "https://x/confirm" }) },
      { side: "client", operationId: "AccountService_ConfirmEmailChange", invoke: (h) => client(h).account.confirmEmailChange({ user_id: "u1", secret: "s" }) },
      { side: "client", operationId: "AccountService_DeleteSession", invoke: (h) => client(h).account.deleteSession("s1") },
      { side: "client", operationId: "AccountService_DeleteSessions", invoke: (h) => client(h).account.deleteSessions(true) },
      { side: "client", operationId: "AccountService_CreateVerification", invoke: (h) => client(h).account.createVerification({ url: "https://x/{{code}}" }) },
      { side: "client", operationId: "AccountService_UpdateVerification", invoke: (h) => client(h).account.updateVerification({ user_id: "u1", secret: "s" }) },
      { side: "client", operationId: "AccountService_CreateRecovery", invoke: (h) => client(h).account.createRecovery({ email: "a@b.c", url: "https://x/{{code}}" }) },
      { side: "client", operationId: "AccountService_UpdateRecovery", invoke: (h) => client(h).account.updateRecovery({ user_id: "u1", secret: "s", password: "newpw" }) },
      { side: "client", operationId: "AccountService_CreateJWT", invoke: (h) => client(h).account.createJWT() },
      { side: "client", operationId: "AccountService_CreateMagicURLSession", invoke: (h) => client(h).account.createMagicURLSession({ email: "a@b.c", url: "https://x" }) },
      { side: "client", operationId: "AccountService_UpdateMagicURLSession", invoke: (h) => client(h).account.updateMagicURLSession({ user_id: "u1", secret: "s" }) },
      { side: "client", operationId: "AccountService_ListFactors", invoke: (h) => client(h).account.listFactors() },
      { side: "client", operationId: "AccountService_CreateTOTPFactor", invoke: (h) => client(h).account.createTOTPFactor() },
      { side: "client", operationId: "AccountService_VerifyTOTPFactor", invoke: (h) => client(h).account.verifyTOTPFactor({ factor_id: "f1", code: "123456" }) },
      { side: "client", operationId: "AccountService_DeleteFactor", invoke: (h) => client(h).account.deleteFactor("f1", "123456") },
      { side: "client", operationId: "AccountService_CreateMFASession", invoke: (h) => client(h).account.createMFASession({ challenge_token: "ct", factor_id: "f1", code: "123456" }) },
      { side: "server", operationId: "PaymentsService_Refund", invoke: (h) => server(h).payments.refund("o1", { amount: "1999", reason: "demo" }) },
      { side: "server", operationId: "PaymentsService_ManualFulfill", invoke: (h) => server(h).payments.manualFulfill("o1", "demo") },
      { side: "server", operationId: "AssetsService_CreateAssetDef", invoke: (h) => server(h).assets.createAssetDef({ code: "gold", name: "Gold", class: "currency" }) },
      { side: "server", operationId: "AssetsService_UpdateAssetDef", invoke: (h) => server(h).assets.updateAssetDef("d1", { name: "Gold v2" }) },
      { side: "server", operationId: "AssetsService_DeleteAssetDef", invoke: (h) => server(h).assets.deleteAssetDef("d1") },
      { side: "server", operationId: "AssetsService_Grant", invoke: (h) => server(h).assets.grant({ owner_id: "u1", def_code: "gold", quantity: "100", idempotency_key: "g1" }) },
      { side: "server", operationId: "AssetsService_Consume", invoke: (h) => server(h).assets.consume({ owner_id: "u1", def_code: "gold", quantity: "10", idempotency_key: "c1" }) },
      { side: "server", operationId: "AssetsService_Transfer", invoke: (h) => server(h).assets.transfer({ from_owner_id: "u1", to_owner_id: "u2", def_code: "gold", quantity: "1", idempotency_key: "t1" }) },
      { side: "server", operationId: "AssetsService_Mutate", invoke: (h) => server(h).assets.mutate({ holding_id: "h1", idempotency_key: "m1" }) },
      { side: "server", operationId: "AssetsService_Expire", invoke: (h) => server(h).assets.expire({ holding_id: "h1", idempotency_key: "x1" }) },
      { side: "server", operationId: "SubscriptionsService_CreatePlan", invoke: (h) => server(h).subscriptions.createPlan({ code: "pro", name: "Pro", amount: "999", currency: "USD", interval: "month" }) },
      { side: "server", operationId: "SubscriptionsService_UpdatePlan", invoke: (h) => server(h).subscriptions.updatePlan("p1", { name: "Pro+" }) },
      { side: "server", operationId: "SubscriptionsService_DeletePlan", invoke: (h) => server(h).subscriptions.deletePlan("p1") },
      { side: "server", operationId: "SubscriptionsService_CancelSubscription", invoke: (h) => server(h).subscriptions.cancelSubscription("s1", "demo") },
      { side: "server", operationId: "SubscriptionsService_ExpireSubscription", invoke: (h) => server(h).subscriptions.expireSubscription("s1", "demo") },
      { side: "client", operationId: "PaymentsService_CreateOrder", invoke: (h) => client(h).payments.createOrder({ idempotency_key: "idem-1", provider: "stripe", amount: "1999", currency: "USD", purpose_kind: "topup", purpose: { currency_code: "gold", amount: "100" } }) },
      { side: "client", operationId: "SubscriptionsService_Subscribe", invoke: (h) => client(h).subscriptions.subscribe({ plan_code: "pro", mode: "platform", idempotency_key: "s1" }) },
      { side: "client", operationId: "SubscriptionsService_Cancel", invoke: (h) => client(h).subscriptions.cancel("sub-1") }
    );
    assert.ok(cases.length >= 40, `HTTP 绑定用例不足（当前 ${cases.length}）`);

    // swagger 参数名是 camelCase（如 pageSize），SDK 发 snake_case（如 page_size）。
    const toSnake = (s: string): string => s.replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`);
    const pathToRegex = (tmpl: string): RegExp => {
      // 先把 {param} 占位替换成不含正则特殊字符的 token，转义后再换回通配段。
      const tokenized = tmpl.replace(/\{[^/}]+}/g, "__TW_WILDCARD__");
      const escaped = tokenized.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      return new RegExp(`^${escaped.replace(/__TW_WILDCARD__/g, "[^/]+")}$`);
    };

    for (const tc of cases) {
      const op = opsBySide.get(tc.side)?.get(tc.operationId);
      if (!op) {
        throw new Error(`swagger 缺少 ${tc.side} operation ${tc.operationId}（先执行 task generate-proto）`);
      }
      const calls: { method: string; url: URL; body?: Record<string, unknown> }[] = [];
      const http = new HttpTransport({
        endpoint: "http://torchwood.test:9080",
        projectId: "default",
        apiKey: "key-1",
        accessToken: "jwt-1",
        fetch: async (input, init) => {
          calls.push({
            method: init?.method ?? "GET",
            url: new URL(String(input)),
            body: init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined,
          });
          return new Response(
            JSON.stringify({
              account: { id: "u1" },
              tokens: { access_token: "at", refresh_token: "rt", expires_at: "2026-08-13T00:00:00Z" },
              sessions: [], factors: [], logs: [], prefs: {}, variables: [], deployments: [],
            }),
            { status: 200, headers: { "content-type": "application/json" } }
          );
        },
      });
      await tc.invoke(http);

      assert.equal(calls.length, 1, `${tc.operationId} 应恰好发出 1 次请求`);
      const req = calls[0];
      assert.equal(req.method.toUpperCase(), op.method.toUpperCase(), `${tc.operationId} HTTP 方法应与 swagger 一致`);
      assert.ok(
        pathToRegex(op.path).test(req.url.pathname),
        `${tc.operationId} 路径 ${req.url.pathname} 应与 swagger ${op.path} 一致`
      );

      const pathParamNames = new Set(op.parameters.filter((p) => p.in === "path").map((p) => toSnake(p.name)));
      const queryNames = new Set(op.parameters.filter((p) => p.in === "query").map((p) => toSnake(p.name)));
      for (const key of req.url.searchParams.keys()) {
        assert.ok(queryNames.has(key), `${tc.operationId} 发送了 swagger 未声明的 query 参数 ${key}`);
      }
      for (const p of op.parameters ?? []) {
        if (p.in === "query" && p.required) {
          assert.ok(req.url.searchParams.has(toSnake(p.name)), `${tc.operationId} 缺少必填 query 参数 ${p.name}`);
        }
      }

      if (op.bodyProps && Object.keys(op.bodyProps).length > 0) {
        const bodyKeys = new Set(Object.keys(req.body ?? {}));
        const allowed = new Set([...Object.keys(op.bodyProps).map(toSnake), ...pathParamNames]);
        for (const key of bodyKeys) {
          assert.ok(allowed.has(key), `${tc.operationId} body 发送了 schema 未声明的字段 ${key}`);
        }
        for (const required of op.bodyRequired ?? []) {
          assert.ok(bodyKeys.has(toSnake(required)), `${tc.operationId} body 缺少必填字段 ${required}`);
        }
      }
    }
  });
});
