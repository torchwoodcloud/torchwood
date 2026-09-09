import type { HttpTransport } from "../http.js";
import { TorchwoodError } from "../errors.js";
import type {
  Account,
  AuthResult,
  Factor,
  LogEntry,
  Session,
  TokenBundle,
  TOTPFactor,
} from "../types.js";

export class AccountService {
  constructor(private readonly http: HttpTransport) {}

  async signUp(input: {
    email: string;
    password: string;
    name: string;
  }): Promise<AuthResult> {
    const res = await this.http.request<AuthResult>(
      "POST",
      "/v1/account/sign-up",
      {
        auth: "none",
        body: {
          project_id: this.http.getProjectId(),
          email: input.email,
          password: input.password,
          name: input.name,
        },
      }
    );
    // MFA 分支：无 tokens，先返回 challenge 信息，调用方应引导二次认证。
    if (res.mfa_required) return res;
    this.http.setAccessToken(res.tokens?.access_token);
    return res;
  }

  async signIn(input: {
    email: string;
    password: string;
  }): Promise<AuthResult> {
    const res = await this.http.request<AuthResult>(
      "POST",
      "/v1/account/sign-in",
      {
        auth: "none",
        body: {
          project_id: this.http.getProjectId(),
          email: input.email,
          password: input.password,
        },
      }
    );
    if (res.mfa_required) return res;
    this.http.setAccessToken(res.tokens?.access_token);
    return res;
  }

  async signOut(): Promise<void> {
    await this.http.request<void>("POST", "/v1/account/sign-out", {
      body: { project_id: this.http.getProjectId() },
    });
    this.http.setAccessToken(undefined);
  }

  async refresh(refreshToken: string): Promise<TokenBundle> {
    const res = await this.http.request<{ tokens: TokenBundle }>("POST", "/v1/account/refresh", {
      auth: "none",
      body: {
        project_id: this.http.getProjectId(),
        refresh_token: refreshToken,
      },
    });
    this.http.setAccessToken(res.tokens.access_token);
    return res.tokens;
  }

  async me(): Promise<Account> {
    return this.http.request<Account>("GET", "/v1/account/me", {
      query: { project_id: this.http.getProjectId() },
    });
  }

  async updateAccount(input: {
    name?: string;
    email?: string;
    password?: string;
    old_password?: string;
    // 改邮箱时必填：新邮箱验证链接模板（staging：验证通过前 email 保持旧值）。
    url?: string;
  }): Promise<Account> {
    return this.http.request<Account>("PATCH", "/v1/account", { body: input });
  }

  // 消费邮件链接中的一次性 secret 完成邮箱变更（公开方法，无需登录：
  // 凭 user_id + secret，与 recovery 同一安全模型——随机 secret + TTL + 一次性消费）。
  async confirmEmailChange(input: {
    user_id: string;
    secret: string;
  }): Promise<Account> {
    return this.http.request<Account>("PUT", "/v1/account/email-change", {
      auth: "none",
      body: {
        project_id: this.http.getProjectId(),
        user_id: input.user_id,
        secret: input.secret,
      },
    });
  }

  async listSessions(): Promise<Session[]> {
    const res = await this.http.request<{ sessions: Session[] }>("GET", "/v1/account/sessions");
    return res.sessions ?? [];
  }

  async deleteSession(sessionId: string): Promise<void> {
    await this.http.request<void>("DELETE", `/v1/account/sessions/${sessionId}`);
  }

  async deleteSessions(keepCurrent = false): Promise<void> {
    // keep_current 经 grpc-gateway 绑定为查询参数（DELETE 无 body）。
    await this.http.request<void>("DELETE", "/v1/account/sessions", {
      query: { keep_current: String(keepCurrent) },
    });
  }

  async deleteAccount(): Promise<void> {
    await this.http.request<void>("DELETE", "/v1/account");
  }

  async getPrefs(): Promise<Record<string, unknown>> {
    const res = await this.http.request<{ prefs: Record<string, unknown> }>(
      "GET",
      "/v1/account/prefs"
    );
    return res.prefs ?? {};
  }

  async updatePrefs(prefs: Record<string, unknown>): Promise<Record<string, unknown>> {
    const res = await this.http.request<{ prefs: Record<string, unknown> }>(
      "PUT",
      "/v1/account/prefs",
      { body: { prefs } }
    );
    return res.prefs ?? {};
  }

  /**
   * 构建 OAuth2 浏览器流发起地址：调用方整页跳转（`window.location.href =
   * 返回值`），由网关 authorize 端点种回调 nonce cookie 后 302 到 provider
   * 授权页。同步方法，不发网络请求。浏览器场景必须用它替代已废弃的
   * createOAuth2Session——跨源 fetch 会丢弃网关种下的 nonce cookie，回调校验必败。
   */
  buildOAuth2AuthorizeURL(input: {
    provider: string;
    success: string;
    failure: string;
  }): string {
    const query = new URLSearchParams({
      project_id: this.http.getProjectId(),
      success: input.success,
      failure: input.failure,
    });
    const endpoint = this.http.getEndpoint().replace(/\/+$/, "");
    return `${endpoint}/v1/account/oauth2/${encodeURIComponent(input.provider)}/authorize?${query.toString()}`;
  }

  /**
   * @deprecated 浏览器流已不可用：本方法经跨源 fetch 调用网关，响应种下的
   * 回调 nonce cookie（`TORCHWOOD_oauth_nonce_<project>`）会被浏览器在跨源
   * fetch 中丢弃，回调 nonce 校验必败（fail-closed 302 到 `?error=oauth_failed`）。
   * 浏览器场景改用 buildOAuth2AuthorizeURL 整页跳转 + parseOAuth2CallbackFragment。
   * 方法保留仅为向后兼容。
   */
  async createOAuth2Session(input: {
    provider: string;
    success: string;
    failure: string;
  }): Promise<{ redirect_url: string }> {
    return this.http.request("GET", `/v1/account/sessions/oauth2/${encodeURIComponent(input.provider)}`, {
      auth: "none",
      query: {
        project_id: this.http.getProjectId(),
        success: input.success,
        failure: input.failure,
      },
    });
  }

  /**
   * 用 OAuth 回调 code 换会话。适用边界：仅限定制流程——state 由调用方自行
   * 保管、未被网关 GET 回调端点消费。浏览器标准流中 state 已被网关回调以
   * GETDEL 一次性消费，此方法不可用；标准浏览器流见 buildOAuth2AuthorizeURL。
   */
  async createOAuth2TokenSession(input: {
    provider: string;
    code: string;
    state: string;
    success?: string;
    failure?: string;
  }): Promise<{ account: Account; tokens: TokenBundle }> {
    const res = await this.http.request<{ account: Account; tokens: TokenBundle }>(
      "POST",
      `/v1/account/sessions/oauth2/${encodeURIComponent(input.provider)}/token`,
      {
        auth: "none",
        body: {
          project_id: this.http.getProjectId(),
          code: input.code,
          state: input.state,
          success: input.success,
          failure: input.failure,
        },
      }
    );
    this.http.setAccessToken(res.tokens.access_token);
    return res;
  }

  async createEmailOTP(input: {
    email: string;
  }): Promise<{ challenge_id: string; expire_at: string }> {
    return this.http.request("POST", "/v1/account/sessions/email-otp", {
      auth: "none",
      body: {
        project_id: this.http.getProjectId(),
        email: input.email,
      },
    });
  }

  async createEmailOTPSession(input: {
    email: string;
    challenge_id: string;
    otp: string;
  }): Promise<{ account: Account; tokens: TokenBundle }> {
    const res = await this.http.request<{ account: Account; tokens: TokenBundle }>(
      "POST",
      "/v1/account/sessions/email-otp/verify",
      {
        auth: "none",
        body: {
          project_id: this.http.getProjectId(),
          email: input.email,
          challenge_id: input.challenge_id,
          otp: input.otp,
        },
      }
    );
    this.http.setAccessToken(res.tokens.access_token);
    return res;
  }

  async createPhoneOTP(input: {
    phone: string;
  }): Promise<{ challenge_id: string; expire_at: string }> {
    return this.http.request("POST", "/v1/account/sessions/phone-otp", {
      auth: "none",
      body: {
        project_id: this.http.getProjectId(),
        phone: input.phone,
      },
    });
  }

  async createPhoneOTPSession(input: {
    phone: string;
    challenge_id: string;
    otp: string;
  }): Promise<{ account: Account; tokens: TokenBundle }> {
    const res = await this.http.request<{ account: Account; tokens: TokenBundle }>(
      "POST",
      "/v1/account/sessions/phone-otp/verify",
      {
        auth: "none",
        body: {
          project_id: this.http.getProjectId(),
          phone: input.phone,
          challenge_id: input.challenge_id,
          otp: input.otp,
        },
      }
    );
    this.http.setAccessToken(res.tokens.access_token);
    return res;
  }

  async createWeChatMiniProgramSession(input: {
    code: string;
  }): Promise<{ account: Account; tokens: TokenBundle }> {
    const res = await this.http.request<{ account: Account; tokens: TokenBundle }>(
      "POST",
      "/v1/account/sessions/wechat/miniprogram",
      {
        auth: "none",
        body: {
          project_id: this.http.getProjectId(),
          code: input.code,
        },
      }
    );
    this.http.setAccessToken(res.tokens.access_token);
    return res;
  }

  // 创建匿名会话；返回的匿名用户可通过 signIn 升级为正式账号。
  async createAnonymousSession(): Promise<{ account: Account; tokens: TokenBundle }> {
    const res = await this.http.request<{ account: Account; tokens: TokenBundle }>(
      "POST",
      "/v1/account/sessions/anonymous",
      {
        auth: "none",
        body: { project_id: this.http.getProjectId() },
      }
    );
    this.http.setAccessToken(res.tokens.access_token);
    return res;
  }

  /**
   * 生成第三方账号绑定授权链接（需已登录）。
   * @deprecated 与 createOAuth2Session 同源的跨源 fetch 丢 Set-Cookie 问题；
   * 后端尚无 link 流的 authorize 端点，暂无替代——维持现状，待网关补齐
   * link 流浏览器端点后迁移。方法保留仅为向后兼容。
   */
  async createOAuth2LinkSession(input: {
    provider: string;
    success: string;
    failure: string;
  }): Promise<{ redirect_url: string }> {
    return this.http.request("GET", `/v1/account/sessions/oauth2/${encodeURIComponent(input.provider)}/link`, {
      query: {
        project_id: this.http.getProjectId(),
        success: input.success,
        failure: input.failure,
      },
    });
  }

  // 用 OAuth 回调 code 绑定第三方账号（需已登录）。
  async createOAuth2LinkTokenSession(input: {
    provider: string;
    code: string;
    state: string;
  }): Promise<Account> {
    return this.http.request<Account>(
      "POST",
      `/v1/account/sessions/oauth2/${encodeURIComponent(input.provider)}/link/token`,
      {
        body: {
          project_id: this.http.getProjectId(),
          code: input.code,
          state: input.state,
        },
      }
    );
  }

  // 发送邮箱验证邮件（url 为带 {{code}} 占位的确认链接模板）。
  async createVerification(input: {
    url: string;
  }): Promise<{ user_id: string; expire_at: string }> {
    return this.http.request("POST", "/v1/account/verification", {
      body: {
        project_id: this.http.getProjectId(),
        url: input.url,
      },
    });
  }

  // 用邮件中的 secret 确认邮箱（公开：无需登录）。
  async updateVerification(input: {
    user_id: string;
    secret: string;
  }): Promise<Account> {
    return this.http.request<Account>("PUT", "/v1/account/verification", {
      auth: "none",
      body: {
        project_id: this.http.getProjectId(),
        user_id: input.user_id,
        secret: input.secret,
      },
    });
  }

  // 发送密码找回邮件。
  async createRecovery(input: {
    email: string;
    url: string;
  }): Promise<void> {
    await this.http.request<void>("POST", "/v1/account/recovery", {
      auth: "none",
      body: {
        project_id: this.http.getProjectId(),
        email: input.email,
        url: input.url,
      },
    });
  }

  // 用邮件中的 secret 重置密码（公开：无需登录）。
  async updateRecovery(input: {
    user_id: string;
    secret: string;
    password: string;
  }): Promise<void> {
    await this.http.request<void>("PUT", "/v1/account/recovery", {
      auth: "none",
      body: {
        project_id: this.http.getProjectId(),
        user_id: input.user_id,
        secret: input.secret,
        password: input.password,
      },
    });
  }

  // ---- MFA ----
  async listFactors(): Promise<Factor[]> {
    const res = await this.http.request<{ factors: Factor[] }>("GET", "/v1/account/mfa");
    return res.factors ?? [];
  }

  // 创建 TOTP 因子；secret/otpauth_url 仅本次返回明文，之后不回显。
  async createTOTPFactor(): Promise<TOTPFactor> {
    return this.http.request<TOTPFactor>("POST", "/v1/account/mfa/totp", {
      body: {},
    });
  }

  // 校验并激活 TOTP 因子。
  async verifyTOTPFactor(input: {
    factor_id: string;
    code: string;
  }): Promise<Factor> {
    return this.http.request<Factor>("PUT", "/v1/account/mfa/totp", {
      body: input,
    });
  }

  // 删除 MFA 因子：pending 因子直接删除；verified 因子必须携带 TOTP code
  // 二次验证（code 经 query 传递：DELETE ?code=...，R05-P1-4）。
  async deleteFactor(factorId: string, code?: string): Promise<void> {
    await this.http.request<void>("DELETE", `/v1/account/mfa/${factorId}`, {
      query: code ? { code } : undefined,
    });
  }

  // 登录二次验证：携带 signIn/signUp 返回的 challenge_token 完成挑战，
  // 成功后自动保存 token。
  async createMFASession(input: {
    challenge_token: string;
    factor_id: string;
    code: string;
  }): Promise<{ account: Account; tokens: TokenBundle }> {
    const res = await this.http.request<{ account: Account; tokens: TokenBundle }>(
      "POST",
      "/v1/account/mfa/challenge",
      {
        auth: "none",
        body: {
          project_id: this.http.getProjectId(),
          challenge_token: input.challenge_token,
          factor_id: input.factor_id,
          code: input.code,
        },
      }
    );
    this.http.setAccessToken(res.tokens.access_token);
    return res;
  }

  // 用当前会话换取一次性 JWT（用于服务端安全回调/Webhook）。
  async createJWT(): Promise<{ token: string }> {
    return this.http.request("POST", "/v1/account/jwt", { body: {} });
  }

  // ---- Magic URL ----
  async createMagicURLSession(input: {
    email: string;
    url: string;
  }): Promise<{ challenge_id: string; expire_at: string }> {
    return this.http.request("POST", "/v1/account/sessions/magic-url", {
      auth: "none",
      body: {
        project_id: this.http.getProjectId(),
        email: input.email,
        url: input.url,
      },
    });
  }

  // 用邮件中的 secret 完成 Magic URL 登录。
  async updateMagicURLSession(input: {
    user_id: string;
    secret: string;
  }): Promise<{ account: Account; tokens: TokenBundle }> {
    const res = await this.http.request<{ account: Account; tokens: TokenBundle }>(
      "PUT",
      "/v1/account/sessions/magic-url",
      {
        auth: "none",
        body: {
          project_id: this.http.getProjectId(),
          user_id: input.user_id,
          secret: input.secret,
        },
      }
    );
    this.http.setAccessToken(res.tokens.access_token);
    return res;
  }

  // 账号操作日志（最近 limit 条，默认 25）。
  async listLogs(limit?: number): Promise<LogEntry[]> {
    const res = await this.http.request<{ logs: LogEntry[] }>("GET", "/v1/account/logs", {
      query: { limit: limit ?? 25 },
    });
    return res.logs ?? [];
  }
}

/**
 * OAuth2 回调重定向 fragment 的两种形态：
 * - `signed_in`：网关已完成 code 换发并建会话，fragment 携带 `access_token`
 *   与 `userId`（刻意不含 refresh_token，过期请引导重新登录）；
 * - `mfa_required`：账号启用了 MFA，无会话，需携带 `challengeToken` 走
 *   createMFASession 二次认证。
 */
export type OAuth2CallbackFragment =
  | { type: "signed_in"; accessToken: string; userId: string }
  | {
      type: "mfa_required";
      userId: string;
      challengeToken: string;
      factorTypes: string[];
    };

/**
 * 解析网关 OAuth2 回调重定向带来的 URL fragment（`#access_token=…&userId=…`，
 * 与 window.location.hash 同形）。返回 null 表示不是回调跳转；fragment 声称
 * 是回调但必填字段缺失时抛 TorchwoodError。建会话后建议调用方用
 * history.replaceState 清掉地址栏 fragment，避免 token 留在浏览器历史。
 */
export function parseOAuth2CallbackFragment(
  fragment: string
): OAuth2CallbackFragment | null {
  const params = new URLSearchParams(fragment.replace(/^#/, ""));
  const userId = params.get("userId") ?? "";
  const accessToken = params.get("access_token");
  if (accessToken) {
    if (!userId) {
      throw new TorchwoodError("OAuth2 回调 fragment 缺少 userId", 0);
    }
    return { type: "signed_in", accessToken, userId };
  }
  if (params.get("mfaRequired") === "true") {
    const challengeToken = params.get("challengeToken");
    if (!challengeToken) {
      throw new TorchwoodError("OAuth2 回调 fragment 缺少 challengeToken", 0);
    }
    // mfaFactorTypes 为逗号分隔串（网关 url.Values.Encode 输出，值经百分号编码）。
    const factorTypes = (params.get("mfaFactorTypes") ?? "")
      .split(",")
      .map((t) => t.trim())
      .filter((t) => t.length > 0);
    return { type: "mfa_required", userId, challengeToken, factorTypes };
  }
  return null;
}
