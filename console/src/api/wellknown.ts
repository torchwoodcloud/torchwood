import axios from "axios";

// /.well-known/torchwood 可发现性目录（公开端点，无鉴权）。api_key_scopes 段
// 由服务端策略注册表派生（internal/api/serverhttp/wellknown.go），是建 API Key
// 多选 scope 控件的单一数据源。目录挂在根路径（不在 /v1 下），用裸 axios
// 绕开 api 实例的 baseURL 与 401 刷新拦截器。

export interface WellKnownScopeResource {
  resource: string;
  read: boolean;
  write: boolean;
}

interface WellKnownCatalog {
  api_key_scopes?: WellKnownScopeResource[];
}

export async function fetchApiKeyScopeCatalog(): Promise<WellKnownScopeResource[]> {
  const res = await axios.get<WellKnownCatalog>("/.well-known/torchwood");
  return res.data.api_key_scopes ?? [];
}
