import type { HttpTransport } from "../http.js";
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

  // 生成第三方账号绑定授权链接（需已登录）。
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
