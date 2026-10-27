import { act } from "react";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { Outlet } from "@/router/outlet-context.tsx";
import { TestRouter } from "@/test-utils/TestRouter";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { refreshSession } from "@/api/auth";
import { ListenPanel } from "./ListenPanel";

// Mock console 会话刷新（/v1/console/auth refresh），断线重连路径由它驱动。
vi.mock("@/api/auth", () => ({ refreshSession: vi.fn() }));
// useAuth 直接返回当前 project（AuthProvider 依赖 QueryClient，单测绕开）。
vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "proj-1" }) }));
// useUserTimezone 依赖 admins/me 查询（QueryClientProvider），单测固定时区即可。
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));

// MockWebSocket 模拟服务端一侧：记录客户端帧，测试手动触发 open/帧/close。
class MockWebSocket {
  static instances: MockWebSocket[] = [];

  onopen: (() => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onclose: ((ev: { code: number; reason: string }) => void) | null = null;
  onerror: (() => void) | null = null;

  readonly sent: string[] = [];
  readonly url: string;

  constructor(url: string) {
    this.url = url;
    MockWebSocket.instances.push(this);
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close(): void {}

  frames(): Record<string, unknown>[] {
    return this.sent.map((s) => JSON.parse(s) as Record<string, unknown>);
  }

  serverOpen(): void {
    this.onopen?.();
  }

  serverSend(frame: Record<string, unknown>): void {
    this.onmessage?.({ data: JSON.stringify(frame) });
  }

  serverClose(code: number, reason: string): void {
    this.onclose?.({ code, reason });
  }
}

async function renderPanel() {
  return render(
    <TestRouter
      initialEntries={["/console/databases/app/collections/posts/listen"]}
      routes={[
        {
          path: "/console/databases/:dbId/collections/:collId",
          element: <Outlet context={{ dbId: "app", collId: "posts" }} />,
          children: [{ path: "listen", element: <ListenPanel /> }],
        },
      ]}
    />
  );
  // TanStack RouterProvider 首帧异步挂载——flush 后引擎开流
  await act(async () => {});
}

// 引擎开流链（mount→useSubscription→引擎 start→迭代器首 next→WS 构造）
// 跨多个 Effect 微任务——waitFor 轮询首个实例。
async function firstMockWs(): Promise<MockWebSocket> {
  return waitFor(() => {
    const ws = MockWebSocket.instances[0];
    expect(ws).toBeTruthy();
    return ws;
  });
}

async function connectFirst(): Promise<MockWebSocket> {
  const ws = await firstMockWs();
  act(() => ws.serverOpen());
  act(() => ws.serverSend({ type: "hello_ok", connection_id: "conn-1" }));
  return ws;
}

describe("ListenPanel", () => {
  beforeEach(() => {
    MockWebSocket.instances = [];
    vi.stubGlobal("WebSocket", MockWebSocket);
    vi.mocked(refreshSession).mockReset().mockResolvedValue(undefined);
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("cookie 握手绑 project_id（不带 access_token），订阅集合频道并展示事件（最新在上）", async () => {
    await renderPanel();
    const ws = await firstMockWs();
    act(() => ws.serverOpen());

    // hello 先绑 project_id，cookie 场景不带 access_token。
    expect(ws.url).toBe(`${location.origin.replace(/^http/, "ws")}/v1/realtime`);
    expect(ws.frames()).toEqual([{ type: "hello", project_id: "proj-1" }]);
    // hello_ok 后订阅当前集合频道。
    act(() => ws.serverSend({ type: "hello_ok", connection_id: "conn-1" }));
    expect(ws.frames()[1]).toEqual({
      type: "subscribe",
      id: "c1",
      channel: "databases.app.collections.posts",
    });
    expect(await screen.findByText("Connected")).toBeTruthy();

    act(() =>
      ws.serverSend({
        type: "event",
        channel: "databases.app.collections.posts",
        payload: { event: "databases.app.collections.posts.documents.d1.create", event_id: "e1" },
      })
    );
    expect(
      await screen.findByText("databases.app.collections.posts.documents.d1.create")
    ).toBeTruthy();

    // 第二条事件到达——最新在上（行序倒序）。
    act(() =>
      ws.serverSend({
        type: "event",
        channel: "databases.app.collections.posts",
        payload: { event: "databases.app.collections.posts.documents.d2.delete", event_id: "e2" },
      })
    );
    await waitFor(() => expect(screen.getByText("2 events")).toBeTruthy());
    const rows = document.querySelectorAll("ul > li[data-key]");
    expect(rows[0]?.getAttribute("data-key")).toBe("2"); // d2.delete 最新在上

    // 服务端 30s ping → 回 pong。
    act(() => ws.serverSend({ type: "ping" }));
    expect(ws.frames().at(-1)).toEqual({ type: "pong" });

    // 暂停/恢复（页面编排）：暂停 → paused 指示灯 + 连接关闭；恢复 → 重连。
    act(() => {
      const btn = [...document.querySelectorAll("button")].find(
        (b) => b.textContent?.trim() === "Pause"
      );
      btn?.click();
    });
    await waitFor(() =>
      expect(screen.getByTestId("paused-indicator")).toBeTruthy()
    );
    act(() => {
      const btn = [...document.querySelectorAll("button")].find(
        (b) => b.textContent?.trim() === "Resume"
      );
      btn?.click();
    });
    await waitFor(() =>
      expect(screen.queryByTestId("paused-indicator")).toBeNull()
    );
  });

  it("15m 到期断线（token_expired）：走 console auth refresh 后重新 hello + 重订", async () => {
    await renderPanel();
    const ws1 = await connectFirst();
    expect(await screen.findByText("Connected")).toBeTruthy();

    act(() => ws1.serverClose(1008, "token_expired"));
    await waitFor(() => expect(vi.mocked(refreshSession)).toHaveBeenCalledTimes(1));

    // refresh 成功后自动重连（引擎退避首档 250ms）：新 WS、重新 hello、
    // 重订频道（不补历史——无 backlog 前缀/游标重放帧）。
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(2));
    const ws2 = MockWebSocket.instances[1];
    act(() => ws2.serverOpen());
    expect(ws2.frames()).toEqual([{ type: "hello", project_id: "proj-1" }]);
    act(() => ws2.serverSend({ type: "hello_ok", connection_id: "conn-2" }));
    expect(ws2.frames()[1]).toEqual({
      type: "subscribe",
      id: "c1",
      channel: "databases.app.collections.posts",
    });
    expect(await screen.findByText("Connected")).toBeTruthy();
  });

  it("refresh 持续失败：多轮退避后 stalled「已断开，点击重连」，点击后重连恢复", { timeout: 20000 }, async () => {
    // 引擎语义（§12.4-2）：refresh 失败不阻断重订——服务端持续 1008 时
    // 逐档退避（250+500+1000+2000ms），预算耗尽 stalled；手动重连 restart
    // 重置预算。模拟死会话：每次新连接都被服务端以 token_expired 关闭。
    vi.mocked(refreshSession).mockRejectedValue(new Error("session gone"));
    await renderPanel();
    const ws1 = await connectFirst();
    expect(await screen.findByText("Connected")).toBeTruthy();

    // 首连 + 4 次重试 = 5 个连接；每次服务端关闭 → 退避 → refresh（失败）
    // → 重开。第 5 次关闭后预算耗尽 → stalled。
    act(() => ws1.serverClose(1008, "token_expired"));
    for (let n = 1; n < 5; n += 1) {
      const index = n;
      await waitFor(
        () => expect(MockWebSocket.instances.length).toBe(index + 1),
        { timeout: 4000 }
      );
      act(() => MockWebSocket.instances[index].serverClose(1008, "token_expired"));
    }
    // stalled 徽章（精确匹配——按钮文案是「Disconnected — click to reconnect」，substring 会撞两者）
    expect(await screen.findByText("Disconnected")).toBeTruthy();
    // refresh 按重试次数调用（每轮退避后一次），非按 close 次数。
    expect(vi.mocked(refreshSession)).toHaveBeenCalledTimes(4);

    const reconnectBtn = await screen.findByText("Disconnected — click to reconnect");
    // 会话已在别处恢复（refresh 转成功）；restart 重置退避预算立即重开。
    vi.mocked(refreshSession).mockResolvedValue(undefined);
    act(() => reconnectBtn.click());
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(6), { timeout: 4000 });
    const ws6 = MockWebSocket.instances[5];
    act(() => ws6.serverOpen());
    expect(ws6.frames()).toEqual([{ type: "hello", project_id: "proj-1" }]);
    act(() => ws6.serverSend({ type: "hello_ok", connection_id: "conn-6" }));
    expect(await screen.findByText("Connected")).toBeTruthy();
  });
});
