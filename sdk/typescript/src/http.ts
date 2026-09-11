import { TorchwoodError, parseErrorResponse } from "./errors.js";

/**
 * AuthMode 说明：
 * - "apiKey"：`X-Api-Key` + `X-Torchwood-Project` 头（Server API 项目密钥）。
 * - "user"：`Authorization: Bearer <accessToken>`（Client API 终端用户登录态）。
 * - "execution"：`Authorization: Bearer <executionToken>`（函数执行身份；
 *   docs/design/functions-v3.md §5.1——函数内 SDK 经 TW_EXECUTION_TOKEN 获得的
 *   短期凭证调用 Server API，项目绑定在 token 内，无需 X-Torchwood-Project）。
 * - "none"：匿名（health 等公开端点）。
 *
 * 兼容规则：transport 配置了 executionToken 时，"apiKey" 模式的请求改走
 * execution Bearer（server 服务类统一硬编码 auth:"apiKey"；fromExecution()
 * 返回的 client 复用同一批服务类，凭据此一处切换即可全量可用，无需逐类改）。
 */
export type AuthMode = "apiKey" | "user" | "execution" | "none";

export interface TorchwoodConfig {
  endpoint: string;
  /**
   * 项目 ID：apiKey 模式经 `X-Torchwood-Project` 头发送；execution 模式
   * 可省略（执行凭证已携带项目绑定）。
   */
  projectId?: string;
  apiKey?: string;
  accessToken?: string;
  /** 函数执行身份短期凭证（functions-v3.md §5.1；优先经 fromExecution 注入）。 */
  executionToken?: string;
  fetch?: typeof fetch;
}

export interface RequestOptions {
  auth?: AuthMode;
  query?: Record<string, string | number | boolean | string[] | undefined>;
  body?: unknown;
}

export class HttpTransport {
  private endpoint: string;
  private projectId?: string;
  private apiKey?: string;
  private accessToken?: string;
  private executionToken?: string;
  private fetchImpl: typeof fetch;

  constructor(config: TorchwoodConfig) {
    this.endpoint = config.endpoint.replace(/\/+$/, "");
    this.projectId = config.projectId;
    this.apiKey = config.apiKey;
    this.accessToken = config.accessToken;
    this.executionToken = config.executionToken;
    this.fetchImpl = config.fetch ?? ((input, init) => globalThis.fetch(input, init));
  }

  getProjectId(): string {
    return this.projectId ?? "";
  }

  getEndpoint(): string {
    return this.endpoint;
  }

  getAccessToken(): string | undefined {
    return this.accessToken;
  }

  setAccessToken(token: string | undefined): void {
    this.accessToken = token;
  }

  setApiKey(key: string | undefined): void {
    this.apiKey = key;
  }

  getExecutionToken(): string | undefined {
    return this.executionToken;
  }

  setExecutionToken(token: string | undefined): void {
    this.executionToken = token;
  }

  /** execution 凭证是否就绪（server 服务类的 auth:"apiKey" 将解析为 execution）。 */
  private get executionMode(): boolean {
    return this.executionToken !== undefined && this.executionToken !== "";
  }

  /**
   * applyExecutionAuth 写入执行身份 Bearer 头；凭证缺失即抛错（fail-closed，
   * 与 apiKey 模式缺 key 同款）。
   */
  private applyExecutionAuth(headers: Record<string, string>): void {
    if (!this.executionToken) {
      throw new TorchwoodError("Execution token is required for this request (函数内请用 Torchwood.fromExecution 注入)", 0);
    }
    headers.Authorization = `Bearer ${this.executionToken}`;
  }

  async request<T>(method: string, path: string, options: RequestOptions = {}): Promise<T> {
    const url = new URL(`${this.endpoint}${path.startsWith("/") ? path : `/${path}`}`);
    if (options.query) {
      for (const [key, value] of Object.entries(options.query)) {
        if (value === undefined) continue;
        if (Array.isArray(value)) {
          for (const item of value) {
            url.searchParams.append(key, item);
          }
        } else {
          url.searchParams.set(key, String(value));
        }
      }
    }

    const headers: Record<string, string> = {
      Accept: "application/json",
    };
    if (options.body !== undefined) {
      headers["Content-Type"] = "application/json";
    }

    const auth = options.auth ?? "user";
    if (auth === "execution") {
      this.applyExecutionAuth(headers);
    } else if (auth === "apiKey") {
      // execution transport（fromExecution client）复用 server 服务类：其
      // 硬编码的 auth:"apiKey" 在此切换为执行身份 Bearer（见 AuthMode 注释）。
      if (this.executionMode) {
        this.applyExecutionAuth(headers);
      } else {
        if (!this.apiKey) {
          throw new TorchwoodError("API key is required for this request", 0);
        }
        headers["X-Api-Key"] = this.apiKey;
        headers["X-Torchwood-Project"] = this.projectId ?? "";
      }
    } else if (auth === "user") {
      // user 模式不做 execution 切换：函数执行身份的方法面 = Server API
      // （server 服务类全量）；Client API 终端用户语义不适用。
      if (this.accessToken) {
        headers.Authorization = `Bearer ${this.accessToken}`;
      }
    }

    const res = await this.fetchImpl(url, {
      method,
      headers,
      body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
    });

    if (res.status === 204 || res.headers.get("content-length") === "0") {
      return undefined as T;
    }

    if (!res.ok) {
      throw await parseErrorResponse(res);
    }

    const text = await res.text();
    if (!text) {
      return undefined as T;
    }
    return JSON.parse(text) as T;
  }

  async requestForm<T>(
    method: string,
    path: string,
    form: FormData,
    auth: AuthMode = "apiKey"
  ): Promise<T> {
    const url = `${this.endpoint}${path.startsWith("/") ? path : `/${path}`}`;
    const headers: Record<string, string> = {};
    if (auth === "execution") {
      this.applyExecutionAuth(headers);
    } else if (auth === "apiKey") {
      // 与 request() 同规则：execution transport 下 server 服务类的
      // auth:"apiKey" 切换为执行身份 Bearer。
      if (this.executionMode) {
        this.applyExecutionAuth(headers);
      } else {
        if (!this.apiKey) {
          throw new TorchwoodError("API key is required for this request", 0);
        }
        headers["X-Api-Key"] = this.apiKey;
        headers["X-Torchwood-Project"] = this.projectId ?? "";
      }
    } else if (auth === "user" && this.accessToken) {
      headers.Authorization = `Bearer ${this.accessToken}`;
    }

    const res = await this.fetchImpl(url, { method, headers, body: form });
    if (!res.ok) {
      throw await parseErrorResponse(res);
    }
    return (await res.json()) as T;
  }
}

export function listQuery(params?: { queries?: string[]; page_size?: number; page_token?: string }) {
  if (!params) return undefined;
  return {
    queries: params.queries,
    page_size: params.page_size,
    page_token: params.page_token,
  };
}
