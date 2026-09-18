import { assertCtx, type Ctx } from "./identity.js";

/**
 * PlatformDocument 是平台 REST Document 的投影（proto shared.v1.Document 的
 * grpc-gateway JSON 形状）：`version` 为 int64，网关序列化为字符串——兼容
 * string | number 双形态（消费时 `Number(v)` 归一化，与 `@torchwood/sdk`
 * 的 Document 类型同口径）。
 */
export interface PlatformDocument {
  id: string;
  data: unknown;
  version: string | number;
  permissions?: string[];
  created_at?: string;
  updated_at?: string;
}

/**
 * APIError 是平台 API 非 2xx 响应（4xx/5xx 一律映射为本类型；网络层失败
 * 抛原生 fetch 错误）。`message` 优先取 grpc-gateway 错误封套的
 * `error.message`，否则回退 statusText / body 片段。
 */
export class APIError extends Error {
  /** HTTP 状态码。 */
  readonly status: number;
  /** HTTP 状态行原因短语（如 "Not Found"）。 */
  readonly statusText: string;
  /** 响应体原文（截取前 64KB，防日志炸裂）。 */
  readonly body: string;

  constructor(status: number, statusText: string, body: string) {
    super(`functions: platform api ${status} ${statusText || "Unknown"}: ${extractErrorMessage(body, statusText)}`);
    this.name = "APIError";
    this.status = status;
    this.statusText = statusText;
    this.body = body.length > 64 * 1024 ? body.slice(0, 64 * 1024) : body;
  }
}

/**
 * extractErrorMessage 从响应体提取人类可读错误信息：grpc-gateway 封套
 * `{error:{message,code}}` → message；JSON `{message}` → message；其余回退
 * body 片段或 statusText。
 */
function extractErrorMessage(body: string, statusText: string): string {
  if (body) {
    try {
      const parsed = JSON.parse(body) as {
        error?: { message?: unknown };
        message?: unknown;
      };
      if (typeof parsed.error?.message === "string" && parsed.error.message) {
        return parsed.error.message;
      }
      if (typeof parsed.message === "string" && parsed.message) {
        return parsed.message;
      }
    } catch {
      // 非 JSON body → 回退 body 片段。
    }
    const trimmed = body.trim();
    if (trimmed) return trimmed.slice(0, 512);
  }
  return statusText || "request failed";
}

export interface ClientOptions {
  /** 覆写 fetch 实现（测试注入 / 代理场景）；缺省用 globalThis.fetch。 */
  fetch?: typeof fetch;
}

/**
 * Client 是平台 API 客户端（业务核心）：鉴权自动——executionToken 与
 * apiBaseUrl 均取自 ctx（runner 注入的身份六件），零配置。方法面随阶段
 * 扩展（一期：databases 文档两法；Go 侧对称物为 `sdk/go/functions` 的
 * `functions.Client`，路径与鉴权口径一致）。
 *
 * 调用 Server API：`Authorization: Bearer <executionToken>`（项目绑定在
 * token 内，无需 X-Torchwood-Project），权限由函数的 declared_scopes 约束
 * （fail-closed）。ctx 形状在构造期校验；apiBaseUrl/executionToken 为空串
 * 时允许构造（本地装配/测试场景），发起请求时 fail-closed 抛错。
 */
export class Client {
  private readonly base: string;
  private readonly token: string;
  private readonly fetchImpl: typeof fetch;

  constructor(ctx: Ctx, opts?: ClientOptions) {
    assertCtx(ctx, "functions.Client");
    this.base = ctx.apiBaseUrl.replace(/\/+$/, "");
    this.token = ctx.executionToken;
    this.fetchImpl = opts?.fetch ?? ((input, init) => globalThis.fetch(input, init));
  }

  /**
   * createDocument 创建文档（REST：POST /v1/server/databases/{db}/collections/
   * {coll}/documents），返回新文档（含 id；document_id 缺省 = 服务端生成，
   * 与 Go Client.CreateDocument 同口径）。
   */
  async createDocument(
    databaseId: string,
    collectionId: string,
    data: unknown
  ): Promise<PlatformDocument> {
    if (data === undefined) {
      throw new Error("functions.Client: createDocument requires data (got undefined)");
    }
    return this.request("POST", documentsPath(databaseId, collectionId), { data });
  }

  /**
   * getDocument 读取文档（REST：GET /v1/server/databases/{db}/collections/
   * {coll}/documents/{id}）；不存在时抛 APIError（status 404）。事件触发的
   * 函数按 DocumentChange.documentId 回读全量的推荐路径。
   */
  async getDocument(
    databaseId: string,
    collectionId: string,
    documentId: string
  ): Promise<PlatformDocument> {
    return this.request("GET", `${documentsPath(databaseId, collectionId)}/${encodeURIComponent(documentId)}`);
  }

  /** request 统一通道：鉴权头 + JSON 编解码 + 非 2xx → APIError。 */
  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    if (!this.base) {
      throw new Error("functions.Client: ctx.apiBaseUrl is empty — cannot call platform api");
    }
    if (!this.token) {
      throw new Error(
        "functions.Client: ctx.executionToken is empty — refusing unauthenticated platform call (fail-closed)"
      );
    }
    const res = await this.fetchImpl(`${this.base}${path}`, {
      method,
      headers: {
        Accept: "application/json",
        ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
        Authorization: `Bearer ${this.token}`,
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    const text = await res.text();
    if (!res.ok) {
      throw new APIError(res.status, res.statusText, text);
    }
    if (!text) {
      throw new Error(`functions.Client: platform api returned an empty body (${method} ${path})`);
    }
    return JSON.parse(text) as T;
  }
}

/** documentsPath 组装文档集合路径（路径段逐段 encodeURIComponent，对齐 Go url.PathEscape）。 */
function documentsPath(databaseId: string, collectionId: string): string {
  return `/v1/server/databases/${encodeURIComponent(databaseId)}/collections/${encodeURIComponent(collectionId)}/documents`;
}
