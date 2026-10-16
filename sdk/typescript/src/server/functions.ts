import { listQuery, type HttpTransport } from "../http.js";
import type {
  CronTriggerConfig,
  CreateFunctionResult,
  EventTriggerConfig,
  FunctionInfo,
  FunctionSecret,
  FunctionTrigger,
  HttpTriggerConfig,
  Invocation,
  ListMeta,
  ListParams,
} from "../types.js";

// FunctionsService 对接 Functions v4 服务面（docs/design/functions-v4.md）：
// 函数 = 常驻服务（endpoint target），torchwood 只做路由、身份、记账与投递；
// 部署/变量/运行时面已随执行面外包退役（v4 §2/§10）。
export class FunctionsService {
  constructor(private readonly http: HttpTransport) {}

  // 创建函数：endpoint target url 创建时录入，签名密钥服务端生成、明文仅在
  // 本响应出现一次（之后只出指纹——见 CreateFunctionResult.target_secret）。
  async create(input: {
    id: string;
    name: string;
    timeout_seconds?: number;
    enabled?: boolean;
    declared_scopes?: string[];
    client_callable?: boolean;
    client_per_user_limit?: number;
    client_limit_window?: "minute" | "hour" | "day";
    // 函数常驻服务的调用终点（生产强制 https）。
    target_url: string;
    // 可选自定义签名密钥（twfnsec_ 前缀；空 = 服务端生成）。
    target_secret?: string;
  }): Promise<CreateFunctionResult> {
    return this.http.request<CreateFunctionResult>("POST", "/v1/server/functions", {
      auth: "apiKey",
      body: input,
    });
  }

  async list(
    params?: ListParams
  ): Promise<{ functions: FunctionInfo[]; meta?: ListMeta }> {
    const res = await this.http.request<{ functions: FunctionInfo[]; meta?: ListMeta }>(
      "GET",
      "/v1/server/functions",
      { auth: "apiKey", query: listQuery(params) }
    );
    return { functions: res.functions ?? [], meta: res.meta };
  }

  async get(functionId: string): Promise<FunctionInfo> {
    return this.http.request<FunctionInfo>(
      "GET",
      `/v1/server/functions/${encodeURIComponent(functionId)}`,
      { auth: "apiKey" }
    );
  }

  async update(
    functionId: string,
    input: {
      name?: string;
      timeout_seconds?: number;
      enabled?: boolean;
      client_callable?: boolean;
      client_anonymous_allowed?: boolean;
      client_per_user_limit?: number;
      client_limit_window?: "minute" | "hour" | "day";
      // target url：未设置 = 不修改；密钥变更走 rotateSecret。
      target_url?: string;
    }
  ): Promise<FunctionInfo> {
    return this.http.request<FunctionInfo>(
      "PATCH",
      `/v1/server/functions/${encodeURIComponent(functionId)}`,
      {
        auth: "apiKey",
        body: { function_id: functionId, ...input },
      }
    );
  }

  async delete(functionId: string): Promise<void> {
    await this.http.request<void>(
      "DELETE",
      `/v1/server/functions/${encodeURIComponent(functionId)}`,
      { auth: "apiKey" }
    );
  }

  // 轮换 endpoint target 签名密钥：新钥即刻生效（v1 签名），旧钥保留一个
  // 宽限窗（v2 双签）——函数侧持旧钥的 FDK 过渡期不断流。新明文仅在响应
  // 出现一次，立即部署到函数侧。
  async rotateSecret(functionId: string): Promise<FunctionSecret> {
    return this.http.request<FunctionSecret>(
      "POST",
      `/v1/server/functions/${encodeURIComponent(functionId)}:rotate-secret`,
      { auth: "apiKey", body: {} }
    );
  }

  // 全量替换函数 declared_scopes（执行身份，P0；每项形如
  // "<resource>:<op>"，如 "assets:write"；空数组 = 撤销全部平台访问）。
  async setScopes(functionId: string, declaredScopes: string[]): Promise<FunctionInfo> {
    return this.http.request<FunctionInfo>(
      "PUT",
      `/v1/server/functions/${encodeURIComponent(functionId)}/scopes`,
      {
        auth: "apiKey",
        body: { function_id: functionId, declared_scopes: declaredScopes },
      }
    );
  }

  // 调用一个函数（v4 §2：CreateExecution 改名 InvokeFunction；目标恒为函数
  // 当前的 endpoint target）。async=true 时立即返回 queued invocation。
  async invoke(
    functionId: string,
    input?: { data?: string; async?: boolean }
  ): Promise<Invocation> {
    return this.http.request<Invocation>(
      "POST",
      `/v1/server/functions/${encodeURIComponent(functionId)}:invoke`,
      {
        auth: "apiKey",
        body: { function_id: functionId, ...input },
      }
    );
  }

  // 调用记录列表（ListInvocationsRequest）：分页 + status 精确过滤
  //（queued | running | completed | failed）+ created_at 闭区间（RFC3339）。
  async listInvocations(
    functionId: string,
    params?: {
      page_size?: number;
      page_token?: string;
      status?: string;
      created_after?: string;
      created_before?: string;
    }
  ): Promise<{ invocations: Invocation[]; meta?: ListMeta }> {
    const query: Record<string, string | number | undefined> = {
      page_size: params?.page_size,
      page_token: params?.page_token,
      status: params?.status,
      created_after: params?.created_after,
      created_before: params?.created_before,
    };
    for (const k of Object.keys(query)) {
      if (query[k] === undefined || query[k] === "") delete query[k];
    }
    const res = await this.http.request<{ invocations: Invocation[]; meta?: ListMeta }>(
      "GET",
      `/v1/server/functions/${encodeURIComponent(functionId)}/invocations`,
      { auth: "apiKey", query }
    );
    return { invocations: res.invocations ?? [], meta: res.meta };
  }

  async getInvocation(functionId: string, invocationId: string): Promise<Invocation> {
    return this.http.request<Invocation>(
      "GET",
      `/v1/server/functions/${encodeURIComponent(functionId)}/invocations/${encodeURIComponent(invocationId)}`,
      { auth: "apiKey" }
    );
  }

  // ——触发器管理（P1 触发器模块；三型 http / cron / event）——

  // 创建触发器：type=http 需带 http 配置段（token 服务端生成，公开调用
  // 路径 /f/{project_id}/{token}）；type=cron 需带 cron 配置段（UTC）；
  // type=event 需带 event 配置段（eventmatch 订阅串）。
  async createTrigger(
    functionId: string,
    input:
      | ({ type: "http" } & HttpTriggerConfig)
      | ({ type: "cron" } & CronTriggerConfig)
      | ({ type: "event" } & EventTriggerConfig)
  ): Promise<FunctionTrigger> {
    const body: Record<string, unknown> = { function_id: functionId, type: input.type };
    if (input.type === "http") {
      const { type: _type, ...cfg } = input as { type: "http" } & HttpTriggerConfig;
      body.http = cfg;
    } else if (input.type === "cron") {
      const { type: _type, ...cfg } = input as { type: "cron" } & CronTriggerConfig;
      body.cron = cfg;
    } else {
      const { type: _type, ...cfg } = input as { type: "event" } & EventTriggerConfig;
      body.event = cfg;
    }
    return this.http.request<FunctionTrigger>(
      "POST",
      `/v1/server/functions/${encodeURIComponent(functionId)}/triggers`,
      { auth: "apiKey", body }
    );
  }

  async listTriggers(functionId: string): Promise<FunctionTrigger[]> {
    const res = await this.http.request<{ triggers: FunctionTrigger[] }>(
      "GET",
      `/v1/server/functions/${encodeURIComponent(functionId)}/triggers`,
      { auth: "apiKey" }
    );
    return res.triggers ?? [];
  }

  async deleteTrigger(functionId: string, triggerId: string): Promise<void> {
    await this.http.request<void>(
      "DELETE",
      `/v1/server/functions/${encodeURIComponent(functionId)}/triggers/${encodeURIComponent(triggerId)}`,
      { auth: "apiKey" }
    );
  }

  // 轮换 http 触发器 token（旧 token 立即失效；URL 含 token 会被代理/
  // 访问日志记录，疑似泄漏即轮换）。
  async rotateTriggerToken(functionId: string, triggerId: string): Promise<FunctionTrigger> {
    return this.http.request<FunctionTrigger>(
      "POST",
      `/v1/server/functions/${encodeURIComponent(functionId)}/triggers/${encodeURIComponent(triggerId)}:rotate-token`,
      { auth: "apiKey", body: {} }
    );
  }
}
