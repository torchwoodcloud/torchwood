import { listQuery, type HttpTransport } from "../http.js";
import type {
  CronTriggerConfig,
  Deployment,
  Execution,
  FunctionInfo,
  FunctionTrigger,
  HttpTriggerConfig,
  ListMeta,
  ListParams,
  RuntimeInfo,
  SpecificationInfo,
  Variable,
} from "../types.js";

export class FunctionsService {
  constructor(private readonly http: HttpTransport) {}

  async listRuntimes(): Promise<RuntimeInfo[]> {
    const res = await this.http.request<{ runtimes: RuntimeInfo[] }>(
      "GET",
      "/v1/server/functions:runtimes",
      { auth: "apiKey" }
    );
    return res.runtimes ?? [];
  }

  async listSpecifications(): Promise<SpecificationInfo[]> {
    const res = await this.http.request<{ specifications: SpecificationInfo[] }>(
      "GET",
      "/v1/server/functions:specifications",
      { auth: "apiKey" }
    );
    return res.specifications ?? [];
  }

  async create(input: {
    id: string;
    name: string;
    runtime: string;
    entrypoint?: string;
    timeout_seconds?: number;
    spec?: string;
    enabled?: boolean;
  }): Promise<FunctionInfo> {
    return this.http.request<FunctionInfo>("POST", "/v1/server/functions", {
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
      entrypoint?: string;
      timeout_seconds?: number;
      spec?: string;
      enabled?: boolean;
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

  // 创建部署。code 为 zip 代码包（≤1MiB 走此 JSON 通道，base64 编码）；
  // 更大的包请走 multipart 上传
  // （POST /v1/server/functions/{function_id}/deployments/code）。
  async createDeployment(functionId: string, code: string): Promise<Deployment> {
    return this.http.request<Deployment>(
      "POST",
      `/v1/server/functions/${encodeURIComponent(functionId)}/deployments`,
      {
        auth: "apiKey",
        body: { function_id: functionId, code },
      }
    );
  }

  async listDeployments(functionId: string): Promise<Deployment[]> {
    const res = await this.http.request<{ deployments: Deployment[] }>(
      "GET",
      `/v1/server/functions/${encodeURIComponent(functionId)}/deployments`,
      { auth: "apiKey" }
    );
    return res.deployments ?? [];
  }

  async getDeployment(functionId: string, deploymentId: string): Promise<Deployment> {
    return this.http.request<Deployment>(
      "GET",
      `/v1/server/functions/${encodeURIComponent(functionId)}/deployments/${encodeURIComponent(deploymentId)}`,
      { auth: "apiKey" }
    );
  }

  async deleteDeployment(functionId: string, deploymentId: string): Promise<void> {
    await this.http.request<void>(
      "DELETE",
      `/v1/server/functions/${encodeURIComponent(functionId)}/deployments/${encodeURIComponent(deploymentId)}`,
      { auth: "apiKey" }
    );
  }

  // 全量替换函数环境变量（variables 为空数组 = 清空）。
  async setVariables(functionId: string, variables: Variable[]): Promise<Variable[]> {
    const res = await this.http.request<{ variables: Variable[] }>(
      "PUT",
      `/v1/server/functions/${encodeURIComponent(functionId)}/variables`,
      {
        auth: "apiKey",
        body: { function_id: functionId, variables },
      }
    );
    return res.variables ?? [];
  }

  async getVariables(functionId: string): Promise<Variable[]> {
    const res = await this.http.request<{ variables: Variable[] }>(
      "GET",
      `/v1/server/functions/${encodeURIComponent(functionId)}/variables`,
      { auth: "apiKey" }
    );
    return res.variables ?? [];
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

  async createExecution(
    functionId: string,
    input: {
      deployment_id?: string;
      data?: string;
      async?: boolean;
    }
  ): Promise<Execution> {
    return this.http.request<Execution>(
      "POST",
      `/v1/server/functions/${encodeURIComponent(functionId)}/executions`,
      {
        auth: "apiKey",
        body: { function_id: functionId, ...input },
      }
    );
  }

  async listExecutions(functionId: string): Promise<Execution[]> {
    const res = await this.http.request<{ executions: Execution[] }>(
      "GET",
      `/v1/server/functions/${encodeURIComponent(functionId)}/executions`,
      { auth: "apiKey" }
    );
    return res.executions ?? [];
  }

  async getExecution(functionId: string, executionId: string): Promise<Execution> {
    return this.http.request<Execution>(
      "GET",
      `/v1/server/functions/${encodeURIComponent(functionId)}/executions/${encodeURIComponent(executionId)}`,
      { auth: "apiKey" }
    );
  }

  // ——触发器管理（P1 触发器模块）——

  // 创建触发器：type=http 需带 http 配置段（token 服务端生成，公开调用
  // 路径 /f/{project_id}/{token}）；type=cron 需带 cron 配置段（UTC）。
  async createTrigger(
    functionId: string,
    input:
      | ({ type: "http" } & HttpTriggerConfig)
      | ({ type: "cron" } & CronTriggerConfig)
  ): Promise<FunctionTrigger> {
    const body: Record<string, unknown> = { function_id: functionId, type: input.type };
    if (input.type === "http") {
      const { type: _type, ...cfg } = input as { type: "http" } & HttpTriggerConfig;
      body.http = cfg;
    } else {
      const { type: _type, ...cfg } = input as { type: "cron" } & CronTriggerConfig;
      body.cron = cfg;
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
