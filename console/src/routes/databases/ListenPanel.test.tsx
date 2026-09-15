import { act } from "react";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Outlet, Route, Routes } from "react-router-dom";
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

function renderPanel() {
  return render(
    <MemoryRouter initialEntries={["/console/databases/app/collections/posts/listen"]}>
      <Routes>
        <Route
          path="/console/databases/:dbId/collections/:collId"
          element={<Outlet context={{ dbId: "app", collId: "posts" }} />}
        >
          <Route path="listen" element={<ListenPanel />} />
        </Route>
      </Routes>
    </MemoryRouter>
  );
}

// 握手并进入已连接状态（hello → hello_ok → subscribe）。
function connectFirst(): MockWebSocket {
  const ws = MockWebSocket.instances[0];
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

  it("cookie 握手绑 project_id（不带 access_token），订阅集合频道并展示事件", async () => {
    renderPanel();
    const ws = MockWebSocket.instances[0];
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
    expect(await screen.findByText("已连接")).toBeTruthy();

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

    // 服务端 30s ping → 回 pong。
    act(() => ws.serverSend({ type: "ping" }));
    expect(ws.frames().at(-1)).toEqual({ type: "pong" });
  });

  it("15m 到期断线（token_expired）：走 console auth refresh 后重新 hello + 重订", async () => {
    renderPanel();
    const ws1 = connectFirst();
    expect(await screen.findByText("已连接")).toBeTruthy();

    act(() => ws1.serverClose(1008, "token_expired"));
    await waitFor(() => expect(vi.mocked(refreshSession)).toHaveBeenCalledTimes(1));

    // refresh 成功后自动重连：新 WS、重新 hello、重订频道（不补历史）。
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
    expect(await screen.findByText("已连接")).toBeTruthy();
  });

  it("refresh 失败显示「已断开，点击重连」，点击后重新连接", async () => {
    renderPanel();
    const ws1 = connectFirst();
    expect(await screen.findByText("已连接")).toBeTruthy();

    vi.mocked(refreshSession).mockRejectedValueOnce(new Error("session gone"));
    act(() => ws1.serverClose(1008, "token_expired"));
    const reconnectBtn = await screen.findByText("已断开，点击重连");
    expect(MockWebSocket.instances.length).toBe(1); // 未自动重连

    act(() => reconnectBtn.click());
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(2));
    const ws2 = MockWebSocket.instances[1];
    act(() => ws2.serverOpen());
    expect(ws2.frames()).toEqual([{ type: "hello", project_id: "proj-1" }]);
    act(() => ws2.serverSend({ type: "hello_ok", connection_id: "conn-2" }));
    expect(await screen.findByText("已连接")).toBeTruthy();
  });
});
