import type {
  Cursor,
  FrameSource,
  ResourceScope,
  SubscriptionDescriptor,
  Transport,
} from "@lynx-go/ui-kit/kernel";
import { refreshSession } from "@/api/auth";
import {
  helloFrame,
  type ListenWebSocket,
  parseServerFrame,
  pongFrame,
  realtimeURL,
  subscribeFrame,
} from "@/api/realtime";

// fleet-ui 订阅族切片（ListenPanel 首个，design.md §12.4）：/v1/realtime
// 握手式 WebSocket → kit Transport<EventFrame>。重连/退避归订阅引擎——
// beforeReconnect = 先续会话再重订（15m 到期路径；close 1008 不映射 auth
// 终态，首遇即终态会杀死续期——refresh 只活在重连前夜）；退避耗尽 stalled
// 由页面 sub.restart() 手动重连。无 backlog/续订语义（服务端不补历史）：
// cursor 恒 null（活尾），帧 = 到达恒等（seq 跨重连单调递增——transport
// 实例被引擎重试复用）。

export interface EventFrame {
  channel: string;
  payload: Record<string, unknown>;
  /** 到达时刻（客户端时钟——服务端事件帧无时间戳面） */
  at: Date;
  /** transport 闭包内跨重连单调递增的到达序（EventFeed 的行键） */
  seq: number;
}

export interface CollectionEventsParams {
  dbId: string;
  collId: string;
}

export function collectionChannel(params: CollectionEventsParams): string {
  return `databases.${params.dbId}.collections.${params.collId}`;
}

type WebSocketFactory = (url: string) => ListenWebSocket;

const globalWebSocket: WebSocketFactory = (url) =>
  new WebSocket(url) as unknown as ListenWebSocket;

class TransportAbort extends Error {
  constructor() {
    super("kit: realtime transport aborted");
    this.name = "AbortError";
  }
}

// wsTransport：连接生命周期 = 一个 async 迭代器——open/hello/subscribe 在
// 首 next() 内完成，事件帧经步骤队列递送，一切非用户 onclose（含 1008
// token_expired）按传输中断抛 network 交引擎退避重连。
export function realtimeTransport(
  channel: string,
  projectId: string | null,
  webSocketFactory?: WebSocketFactory,
): Transport<EventFrame> {
  const factory = webSocketFactory ?? globalWebSocket;
  let seq = 0;

  return {
    // torchwood 15m 到期路径：断线先续会话再重订（引擎在退避等待后调用；
    // 失败不阻断重订——服务端仍拒绝时按中断继续退避，耗尽 stalled）。
    async beforeReconnect() {
      await refreshSession();
    },

    open(_cursor: Cursor | null, signal: AbortSignal): FrameSource<EventFrame> {
      // 本方言无服务端终态（流永续，一切收流皆中断）——closeReason 恒 transport
      const self = {
        closeReason: "transport" as const,
        async *[Symbol.asyncIterator](): AsyncIterator<EventFrame> {
          // 作用域未就绪：待命至 abort，不发任何帧（对齐现状 !projectId 守卫；
          // scope 就位后 useSubscription 以新 transport 重建订阅）。
          if (projectId === null) {
            await new Promise<never>((_, reject) => {
              if (signal.aborted) reject(new TransportAbort());
              else
                signal.addEventListener("abort", () => reject(new TransportAbort()), {
                  once: true,
                });
            });
            return; // 不可达（上式只 reject）——满足 null 收窄
          }
          if (signal.aborted) throw new TransportAbort();

          type Step = { kind: "frame"; frame: EventFrame } | { kind: "fail"; error: unknown };
          const steps: Step[] = [];
          const waiters: Array<() => void> = [];
          let settled = false;

          const ws = factory(realtimeURL());
          const post = (step: Step): void => {
            steps.push(step);
            waiters.shift()?.();
          };
          const failOnce = (error: unknown): void => {
            if (settled) return;
            settled = true;
            post({ kind: "fail", error });
            try {
              ws.close();
            } catch {
              // 已在关闭中
            }
          };

          ws.onopen = () => {
            ws.send(helloFrame(projectId));
          };
          ws.onmessage = (ev) => {
            const frame = parseServerFrame(ev.data);
            if (frame === null) return;
            switch (frame.type) {
              case "hello_ok":
                ws.send(subscribeFrame("c1", channel));
                break;
              case "event":
                if (frame.channel === channel) {
                  seq += 1;
                  post({
                    kind: "frame",
                    frame: {
                      channel: frame.channel,
                      payload: frame.payload ?? {},
                      at: new Date(),
                      seq,
                    },
                  });
                }
                break;
              case "ping":
                ws.send(pongFrame());
                break;
              default:
                break;
            }
          };
          ws.onerror = () => {
            // onclose 随后触发——统一经 failOnce 入列。
          };
          ws.onclose = () => {
            failOnce(signal.aborted ? new TransportAbort() : { kind: "network" });
          };
          const onAbort = () => failOnce(new TransportAbort());
          signal.addEventListener("abort", onAbort, { once: true });

          try {
            while (true) {
              while (steps.length === 0) {
                await new Promise<void>((resolve) => {
                  waiters.push(resolve);
                });
              }
              const step = steps.shift() as Step;
              if (step.kind === "frame") {
                yield step.frame;
              } else {
                throw step.error;
              }
            }
          } finally {
            signal.removeEventListener("abort", onAbort);
            try {
              ws.close();
            } catch {
              // 已关
            }
          }
        },
      };
      return self;
    },
  };
}

// 集合事件订阅 descriptor（裸 SubscriptionDescriptor——collections 资源化
// 不在本批，demo eventsStream 同款先例）。scope.projectId 即 hello 的
// project 绑定（作用域概念本体）；policy：100 条环形缓冲 + 4 档退避
// （~3.75s 到 stalled，试听面板量级）。
export const collectionEvents: SubscriptionDescriptor<CollectionEventsParams, EventFrame> = {
  policy: { buffer: 100, pauseable: true, retry: { backoffMs: [250, 500, 1000, 2000] } },
  open(params, scope: ResourceScope) {
    return realtimeTransport(collectionChannel(params), scope.projectId ?? null);
  },
};
