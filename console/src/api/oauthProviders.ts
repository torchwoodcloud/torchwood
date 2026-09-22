import { api } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

export interface OAuthProvider {
  provider: string;
  enabled: boolean;
  client_id: string;
  has_client_secret: boolean;
  scopes: string[];
  created_at?: string;
  updated_at?: string;
}

export interface ListOAuthProvidersResponse {
  oauth_providers: OAuthProvider[];
}

export const OAUTH_PROVIDER_OPTIONS = [
  { id: "google", label: "Google", defaultScopes: ["openid", "email", "profile"] },
  { id: "github", label: "GitHub", defaultScopes: ["read:user", "user:email"] },
  { id: "wechat_web", label: "微信 · 网站扫码", defaultScopes: [] },
  { id: "wechat_mp", label: "微信 · 公众号 H5", defaultScopes: [] },
  { id: "wechat_miniprogram", label: "微信 · 小程序", defaultScopes: [] },
] as const;

// 服务端 providers 列表（in-memory crud，默认 page_size=50）；对接服务端分页。
export async function listOAuthProviders(params: ListParams): Promise<Page<OAuthProvider>> {
  const res = await api.get<ListOAuthProvidersResponse & ListMeta>("/server/oauth-providers", {
    params: pageQuery(params),
  });
  return { rows: res.data.oauth_providers ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function upsertOAuthProvider(input: {
  provider: string;
  enabled: boolean;
  client_id: string;
  client_secret?: string;
  scopes?: string[];
}): Promise<OAuthProvider> {
  const res = await api.put<OAuthProvider>(
    `/server/oauth-providers/${encodeURIComponent(input.provider)}`,
    {
      provider: input.provider,
      enabled: input.enabled,
      client_id: input.client_id,
      client_secret: input.client_secret ?? "",
      scopes: input.scopes ?? [],
    }
  );
  return res.data;
}

export async function deleteOAuthProvider(provider: string): Promise<void> {
  await api.delete(`/server/oauth-providers/${encodeURIComponent(provider)}`);
}

export function oauthCallbackURL(provider: string, publicBase = window.location.origin): string {
  return `${publicBase.replace(/\/$/, "")}/v1/account/oauth2/${provider}/callback`;
}
