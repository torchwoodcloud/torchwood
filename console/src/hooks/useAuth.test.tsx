import { act, cleanup, render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { InternalAxiosRequestConfig } from "axios";
import { AuthProvider, useAuth } from "./useAuth";
import { useProjectScopeSync } from "./useProjectScopeSync";
import { api, PROJECT_STORAGE_KEY } from "@/api/client";
import { refreshSession } from "@/api/auth";

// 只 mock 会话探测（避免真实 HTTP）；client 保持真实实现——storage 订阅与
// 请求拦截器都是被测对象。
vi.mock("@/api/auth", () => ({
  login: vi.fn(),
  logout: vi.fn(),
  refreshSession: vi.fn(),
}));

function Probe({ urlId }: { urlId?: string }) {
  const { projectId } = useAuth();
  const synced = useProjectScopeSync(urlId);
  return (
    <div data-testid="state">
      {`${projectId ?? "null"}|${urlId ?? "none"}|${synced ? "synced" : "desynced"}`}
    </div>
  );
}

async function renderAuth(urlId?: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const result = render(
    <QueryClientProvider client={client}>
      <AuthProvider>
        <Probe urlId={urlId} />
      </AuthProvider>
    </QueryClientProvider>
  );
  await flush(); // 让 refreshSession 的异步 settle 落在 act 内
  return result;
}

// 模拟「另一标签页」对 localStorage 的写入：真实浏览器里 storage 事件只在
// 其它 document 触发，jsdom 需手动改存储并派发同名事件。key=null 表示整体
// clear()（此时不改动具体键，仅派发事件）。
function otherTabWrites(key: string | null, value: string | null, oldValue: string | null = null) {
  act(() => {
    if (key !== null && value === null) {
      localStorage.removeItem(key);
    } else if (key !== null && value !== null) {
      localStorage.setItem(key, value);
    }
    window.dispatchEvent(
      new StorageEvent("storage", { key, newValue: value, oldValue, storageArea: localStorage })
    );
  });
}

async function flush() {
  await act(async () => {
    await Promise.resolve();
  });
}

describe("useAuth 项目作用域跨标签同步", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.mocked(refreshSession).mockResolvedValue(undefined);
  });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("其它标签切换项目后，本标签状态经 storage 事件跟随", async () => {
    const { getByTestId } = await renderAuth();
    expect(getByTestId("state").textContent).toBe("null|none|desynced");

    otherTabWrites(PROJECT_STORAGE_KEY, "proj-b");
    expect(getByTestId("state").textContent).toBe("proj-b|none|desynced");
  });

  it("其它标签登出清项目后，本标签状态复位", async () => {
    localStorage.setItem(PROJECT_STORAGE_KEY, "proj-a");
    const { getByTestId } = await renderAuth();
    expect(getByTestId("state").textContent).toBe("proj-a|none|desynced");

    otherTabWrites(PROJECT_STORAGE_KEY, null, "proj-a");
    expect(getByTestId("state").textContent).toBe("null|none|desynced");
  });

  it("无关 key 的 storage 事件不改变项目状态", async () => {
    localStorage.setItem(PROJECT_STORAGE_KEY, "proj-a");
    const { getByTestId } = await renderAuth();

    otherTabWrites("TORCHWOOD_console_sidebar_collapsed", "1");
    expect(getByTestId("state").textContent).toBe("proj-a|none|desynced");
  });

  it("其它标签整体 clear()（key=null 事件）后，本标签状态复位", async () => {
    localStorage.setItem(PROJECT_STORAGE_KEY, "proj-a");
    const { getByTestId } = await renderAuth();
    expect(getByTestId("state").textContent).toBe("proj-a|none|desynced");

    act(() => {
      localStorage.clear();
      window.dispatchEvent(
        new StorageEvent("storage", { key: null, oldValue: null, newValue: null, storageArea: localStorage })
      );
    });
    expect(getByTestId("state").textContent).toBe("null|none|desynced");
  });

  it("跨标签采纳后，后续请求携带新项目的 X-Torchwood-Project", async () => {
    const seen: InternalAxiosRequestConfig[] = [];
    const original = api.defaults.adapter;
    api.defaults.adapter = async (config) => {
      seen.push(config);
      return { data: {}, status: 200, statusText: "OK", headers: {}, config };
    };
    try {
      localStorage.setItem(PROJECT_STORAGE_KEY, "proj-a");
      await api.get("/console/auth/setup-status");

      otherTabWrites(PROJECT_STORAGE_KEY, "proj-b", "proj-a");
      await api.get("/console/auth/setup-status");

      expect(seen[0]?.headers["X-Torchwood-Project"]).toBe("proj-a");
      expect(seen[1]?.headers["X-Torchwood-Project"]).toBe("proj-b");
    } finally {
      api.defaults.adapter = original;
    }
  });
});

describe("useProjectScopeSync 与跨标签同步的共存", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.mocked(refreshSession).mockResolvedValue(undefined);
  });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("停在项目页的标签采纳远端切换后不回写（无乒乓循环）", async () => {
    // 本标签停在 /projects/pb 且已同步到 pb。
    localStorage.setItem(PROJECT_STORAGE_KEY, "pb");
    const { getByTestId } = await renderAuth("pb");
    expect(getByTestId("state").textContent).toBe("pb|pb|synced");

    // 另一标签把全局切到 pa：本标签采纳 pa，但不得把 URL 的 pb 顶回全局。
    otherTabWrites(PROJECT_STORAGE_KEY, "pa", "pb");
    expect(getByTestId("state").textContent).toBe("pa|pb|desynced");
    await flush();
    expect(localStorage.getItem(PROJECT_STORAGE_KEY)).toBe("pa");
  });

  it("离开项目页再回来仍会重新同步 URL 项目（浏览即选中语义不变）", async () => {
    localStorage.setItem(PROJECT_STORAGE_KEY, "pa");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const ui = (urlId?: string) => (
      <QueryClientProvider client={client}>
        <AuthProvider>
          <Probe urlId={urlId} />
        </AuthProvider>
      </QueryClientProvider>
    );
    const { getByTestId, rerender } = render(ui("pa"));
    await flush();
    expect(getByTestId("state").textContent).toBe("pa|pa|synced");

    // 离开项目页（URL 无 :id）→ 另一标签切到 pc → 本标签采纳。
    rerender(ui());
    await flush();
    otherTabWrites(PROJECT_STORAGE_KEY, "pc", "pa");
    expect(getByTestId("state").textContent).toBe("pc|none|desynced");

    // 回到 /projects/pa：URL 导航重新推送 pa。
    rerender(ui("pa"));
    await flush();
    expect(getByTestId("state").textContent).toBe("pa|pa|synced");
    expect(localStorage.getItem(PROJECT_STORAGE_KEY)).toBe("pa");
  });
});
