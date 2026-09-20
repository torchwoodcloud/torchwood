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

/** timeoutMs 缺省值：单请求 30s 兜底（对齐 Go SDK conn.DefaultTimeout）。 */
export const DEFAULT_TIMEOUT_MS = 30000;

/**
 * timeoutSignal 为单次请求构造超时中止信号；ms<=0 返回 undefined（禁用）。
 * AbortSignal.timeout 在 Node >=17.3（SDK engines 为 >=18）与各常青浏览器
 * 均可用，是主路径；仅在缺失该 API 的老运行时降级为 AbortController +
 * setTimeout 等价实现，dispose 清掉定时器避免请求完成后仍悬挂事件循环
 * （AbortSignal.timeout 的内置定时器由运行时自动弱化，无需清理）。
 */
function timeoutSignal(ms: number): { signal?: AbortSignal; dispose(): void } {
  if (ms <= 0) return { dispose() {} };
  if (typeof AbortSignal !== "undefined" && typeof AbortSignal.timeout === "function") {
    return { signal: AbortSignal.timeout(ms), dispose() {} };
  }
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), ms);
  return { signal: controller.signal, dispose: () => clearTimeout(timer) };
}

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
  /**
   * 单请求超时（毫秒）。缺省 / 显式 undefined = 30000（与 Go SDK
   * conn.DefaultTimeout 的 30s 兜底对齐）；传 0 = 显式禁用超时——服务端
   * 停顿时调用将无限挂起，由调用方自行治理（例如外层已有多余超时的场景）。
   * undefined 不作"禁用"解，保证 `{...defaults, cfg}` 展开合并时不会意外
   * 关掉超时。
   */
  timeoutMs?: number;
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
  private timeoutMs: number;
  private fetchImpl: typeof fetch;

  constructor(config: TorchwoodConfig) {
    this.endpoint = config.endpoint.replace(/\/+$/, "");
    this.projectId = config.projectId;
    this.apiKey = config.apiKey;
    this.accessToken = config.accessToken;
    this.executionToken = config.executionToken;
    // undefined 与缺省同义（默认 30s）；仅显式 0（及负数）禁用——见
    // TorchwoodConfig.timeoutMs 注释。
    this.timeoutMs = config.timeoutMs === undefined ? DEFAULT_TIMEOUT_MS : config.timeoutMs;
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

  /**
   * doFetch 是 fetch 的统一出口：注入单请求超时信号（timeoutMs，见
   * TorchwoodConfig 注释），并把超时中止转译成可辨识的 TorchwoodError
   * （code "timeout"）。响应头到达后的响应体读取阶段，主路径（AbortSignal.
   * timeout）同样受同一信号约束；降级路径只覆盖到响应头。
   */
  private async doFetch(url: URL | string, init: RequestInit): Promise<Response> {
    const { signal, dispose } = timeoutSignal(this.timeoutMs);
    try {
      return await this.fetchImpl(url, { ...init, signal });
    } catch (e) {
      // fetch 因超时信号中止时：规范运行时抛 TimeoutError（AbortSignal.
      // timeout），老运行时抛 AbortError——两者且信号已触发才归因为超时，
      // 调用方自建 fetch 的其他 AbortError 原样透传。
      if (signal?.aborted && e instanceof Error && (e.name === "TimeoutError" || e.name === "AbortError")) {
        throw new TorchwoodError(
          `Request timed out after ${this.timeoutMs}ms: ${init.method ?? "GET"} ${String(url)}`,
          0,
          "timeout",
        );
      }
      throw e;
    } finally {
      dispose();
    }
  }

  /**
   * isEmptyBody 是 request()/requestForm() 共用的空响应体判定。注意两处的
   * 求值次序有意不同：request() 沿袭原实现先判空体后判 !ok（content-length
   * 为 0 的非 2xx 响应返回 undefined）；requestForm() 先判 !ok（非 2xx 一律
   * 抛错），空体判定只作用于成功响应。
   */
  private isEmptyBody(res: Response): boolean {
    return res.status === 204 || res.headers.get("content-length") === "0";
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

    const res = await this.doFetch(url, {
      method,
      headers,
      body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
    });

    if (this.isEmptyBody(res)) {
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

    const res = await this.doFetch(url, { method, headers, body: form });
    if (!res.ok) {
      throw await parseErrorResponse(res);
    }
    // 与 request() 同款空体判定：204 / content-length: 0 / 空文本一律返回
    // undefined，不做 res.json()（空体直接解析会抛 SyntaxError）。
    if (this.isEmptyBody(res)) {
      return undefined as T;
    }
    const text = await res.text();
    if (!text) {
      return undefined as T;
    }
    return JSON.parse(text) as T;
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
