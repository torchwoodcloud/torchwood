// torchwood /v1/realtime WebSocket 协议件（v2 设计 §4.2/§4.3）：同源 cookie
// （TORCHWOOD_session_console）握手——hello 绑 project_id、不带 access_token；
// hello_ok 后 subscribe 频道；服务端 30s ping 回 pong；15m 会话到期服务端以
// close 1008 token_expired 关连接（无连接内 refresh）。重连/退避/refresh
// 编排归 fleet-ui 订阅引擎（src/kit/realtime.ts，design.md §12.4 裁决）——
// 本模块只持帧方言与连接 URL，不自带监听状态机。

/** 可注入的 WebSocket 最小接口（浏览器原生 WebSocket 满足；测试注入）。 */
export interface ListenWebSocket {
  onopen: (() => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: ((ev: { code: number; reason: string }) => void) | null;
  onerror: (() => void) | null;
  send(data: string): void;
  close(code?: number, reason?: string): void;
}

export interface RealtimeServerFrame {
  type: string;
  connection_id?: string;
  channel?: string;
  payload?: Record<string, unknown>;
}

export function realtimeURL(): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}/v1/realtime`;
}

export function helloFrame(projectId: string): string {
  return JSON.stringify({ type: "hello", project_id: projectId });
}

export function subscribeFrame(id: string, channel: string): string {
  return JSON.stringify({ type: "subscribe", id, channel });
}

export function pongFrame(): string {
  return JSON.stringify({ type: "pong" });
}

/** 服务端帧解析（非 JSON/无 type 的载荷按协议噪声丢弃）。 */
export function parseServerFrame(data: unknown): RealtimeServerFrame | null {
  try {
    const frame = JSON.parse(String(data)) as RealtimeServerFrame;
    return typeof frame?.type === "string" ? frame : null;
  } catch {
    return null;
  }
}
