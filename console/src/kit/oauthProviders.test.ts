import { runCommand } from "@lynx-go/uikit-core/kernel";
import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  deleteOAuthProvider,
  listOAuthProviders,
  upsertOAuthProvider,
} from "@/api/oauthProviders";
import {
  oauthProvidersResource,
  removeOAuthProviderCommand,
  upsertOAuthProviderCommand,
} from "@/kit/oauthProviders";

// oauth-providers 分页列表资源（子视图 kit 化批）与 upsert/remove 命令的失效
// 声明锚：collection ctx 缺参静默空段；失效 = kit oauth-providers 键（存量
// 前缀桥退役——最后读者 settings 面板列表已换轨 useCollectionData）。

vi.mock("@/api/oauthProviders", () => ({
  deleteOAuthProvider: vi.fn(),
  listOAuthProviders: vi.fn(),
  upsertOAuthProvider: vi.fn(),
}));

function httpError(status: number, message: string, code = "NotFound"): AxiosError {
  return new AxiosError("Request failed", "ERR_BAD_REQUEST", undefined, undefined, {
    status,
    statusText: "",
    headers: {},
    config: {} as never,
    data: { error: { code, message } },
  } as never);
}

beforeEach(() => {
  vi.mocked(listOAuthProviders).mockReset();
  vi.mocked(upsertOAuthProvider).mockReset();
  vi.mocked(deleteOAuthProvider).mockReset();
});

describe("oauthProvidersResource.collection（settings 面板列表，子视图 kit 化批）", () => {
  it("窗口映射 pageSize/pageToken，Page→Segment", async () => {
    vi.mocked(listOAuthProviders).mockResolvedValue({
      rows: [{ provider: "google" } as never],
      nextPageToken: "t2",
    });
    const seg = await oauthProvidersResource.collection!(
      { window: { size: 20, cursor: "t1" } },
      { projectId: "p1" }
    );
    expect(vi.mocked(listOAuthProviders)).toHaveBeenCalledWith({
      pageSize: 20,
      pageToken: "t1",
    });
    expect(seg).toEqual({ rows: [{ provider: "google" }], nextCursor: "t2" });
  });

  it("projectId 缺失时静默空段（不发起请求）", async () => {
    const seg = await oauthProvidersResource.collection!({ window: { size: 20 } }, {});
    expect(seg).toEqual({ rows: [] });
    expect(vi.mocked(listOAuthProviders)).not.toHaveBeenCalled();
  });

  it("axios 失败收敛为 ApiError 封闭集", async () => {
    vi.mocked(listOAuthProviders).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(
      oauthProvidersResource.collection!({ window: { size: 20 } }, { projectId: "p1" })
    ).rejects.toEqual({ kind: "server", status: 500, message: "boom" });
  });
});

describe("upsert/remove 命令的失效声明锚", () => {
  it("invalidates = kit oauth-providers 键；legacy 前缀桥退役", () => {
    for (const verb of [upsertOAuthProviderCommand, removeOAuthProviderCommand]) {
      expect(verb.invalidates).toEqual([{ resource: "oauth-providers" }]);
      expect(verb.legacyKeys).toBeUndefined();
    }
  });

  it("upsert ok：input 透传 + __skipToast 单源", async () => {
    vi.mocked(upsertOAuthProvider).mockResolvedValue({ provider: "google" } as never);
    const result = await runCommand(
      upsertOAuthProviderCommand,
      { provider: "google", enabled: true, client_id: "cid" },
      { projectId: "p1" }
    );
    expect(result).toEqual({ status: "ok", payload: { provider: "google" } });
    expect(vi.mocked(upsertOAuthProvider)).toHaveBeenCalledWith(
      { provider: "google", enabled: true, client_id: "cid" },
      { __skipToast: true }
    );
  });

  it("remove 404 传播失败通道（无 treatAs——与旧行为一致）", async () => {
    vi.mocked(deleteOAuthProvider).mockRejectedValue(httpError(404, "gone"));
    await expect(
      runCommand(removeOAuthProviderCommand, { provider: "nope" }, { projectId: "p1" })
    ).rejects.toEqual({ kind: "not-found" });
  });
});
