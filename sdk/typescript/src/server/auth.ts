import type { HttpTransport } from "../http.js";

/**
 * Server API 凭证校验面（token introspection，proto server.v1.AuthService）。
 * 语义与请求侧认证同源：任何校验失败（过期/撤销/封禁/不存在/签名无效/
 * 跨项目）→ valid=false + 200，仅基础设施故障返回 5xx。
 */

/** 调用方声明的凭证分派方式；缺省/AUTO 按结构分派（sk- 前缀走 API key）。 */
export type VerifyCredentialType =
  | "VERIFY_CREDENTIAL_TYPE_UNSPECIFIED"
  | "VERIFY_CREDENTIAL_TYPE_AUTO"
  | "VERIFY_CREDENTIAL_TYPE_TOKEN"
  | "VERIFY_CREDENTIAL_TYPE_API_KEY";

export interface VerifyTokenGroupRef {
  id: string;
  role: string;
}

export interface VerifyTokenResponse {
  /** false = 任何校验失败；此时其余字段为零值（不泄露主体信息）。 */
  valid: boolean;
  /** 端用户 id；API key 主体为空。 */
  user_id: string;
  username: string;
  project_id: string;
  /** 端用户会话 id（活查会话表，登出即 invalid）。 */
  session_id: string;
  /** 凭证到期时刻（RFC3339）；API key 未设 expire_at 则省略。 */
  expires_at?: string;
  /** 实时解析的角色词汇（与请求侧授权同源）。 */
  roles: string[];
  /** 已接受的组成员关系；无角色的成员关系 role 为空串。 */
  groups: VerifyTokenGroupRef[];
  /** 端用户 labels（与 label:<l> 角色同源）。 */
  labels: string[];
  /** end_user / service。 */
  actor_kind: string;
  /** API key 主体时为密钥行 id。 */
  api_key_id: string;
}

export class AuthService {
  constructor(private readonly http: HttpTransport) {}

  /** verifyToken 校验凭证原文并返回主体信息（introspection 契约，不抛判定错误）。 */
  async verifyToken(
    token: string,
    type?: VerifyCredentialType
  ): Promise<VerifyTokenResponse> {
    return this.http.request<VerifyTokenResponse>("POST", "/v1/server/auth/tokens:verify", {
      auth: "apiKey",
      body: { token, type },
    });
  }
}
