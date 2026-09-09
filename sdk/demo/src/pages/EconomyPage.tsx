import { useEffect, useRef, useState } from "react";
import { accountsChannel, type RealtimeConnection, type RealtimeEvent } from "@torchwood/sdk";
import { useTorchwood } from "@/lib/torchwood-context";
import { suffix } from "@/lib/storage";
import { ErrorBanner, JsonPanel, MethodTag, PageHeader } from "@/components/Ui";

type DomainEvent = { domain: string; event: string; at: string; payload: Record<string, unknown> };

export function EconomyPage() {
  const { client, auth, settings, serverClient, run, lastError } = useTorchwood();
  const [result, setResult] = useState<unknown>(null);
  const [loading, setLoading] = useState(false);
  const [rtStatus, setRtStatus] = useState("closed");
  const [events, setEvents] = useState<DomainEvent[]>([]);
  const connRef = useRef<RealtimeConnection | null>(null);

  const uid = auth?.userId ?? "";
  const channel = uid ? accountsChannel(uid) : "";

  useEffect(() => {
    return () => {
      connRef.current?.close();
      connRef.current = null;
    };
  }, []);

  async function exec(label: string, fn: () => Promise<unknown>) {
    setLoading(true);
    try {
      const data = await run(fn);
      setResult({ action: label, data });
      return data;
    } catch {
      return null;
    } finally {
      setLoading(false);
    }
  }

  function connectRealtime() {
    connRef.current?.close();
    const conn = client.realtime.connect({
      getAccessToken: () => auth?.accessToken,
      onStatusChange: setRtStatus,
    });
    conn.subscribe(channel, (ev: RealtimeEvent) => {
      const payload = ev.payload ?? {};
      const domain = String(payload.domain ?? "unknown");
      setEvents((prev) => [
        {
          domain,
          event: String(payload.event ?? ""),
          at: new Date().toISOString(),
          payload,
        },
        ...prev,
      ].slice(0, 50));
    });
    connRef.current = conn;
  }

  async function bootstrapDef() {
    const tw = serverClient();
    return exec("server.assets.createAssetDef(gold)", () =>
      tw.server.assets.createAssetDef({
        code: "gold",
        name: "Gold",
        class: "currency",
        decimals: 0,
      }).catch(async () => tw.server.assets.listAssetDefs())
    );
  }

  async function createOrder() {
    return exec("payments.createOrder()", () =>
      client.payments.createOrder({
        idempotency_key: `demo-order-${suffix()}`,
        provider: "stripe",
        amount: "1999",
        currency: "USD",
        purpose_kind: "topup",
        purpose: { currency_code: "gold", amount: "100" },
      })
    );
  }

  async function simulateFulfill() {
    const tw = serverClient();
    return exec("server.assets.grant(模拟到账)", () =>
      tw.server.assets.grant({
        owner_id: uid,
        def_code: "gold",
        quantity: "100",
        idempotency_key: `demo-grant-${suffix()}`,
        ref_type: "demo",
        ref_id: `pay-sim-${suffix()}`,
      })
    );
  }

  const hasApiKey = Boolean(settings.apiKey);

  return (
    <div>
      <PageHeader
        title="Economy 最小流程"
        description="建单 → 支付模拟（Grant 到账）→ 资产查询 → Realtime accounts.{uid} 按 domain 分流。"
      />
      <ErrorBanner message={lastError} />

      <p className="mb-4 font-mono text-xs text-Torchwood-muted">
        channel: {channel || "(需登录)"} · realtime: {rtStatus}
      </p>

      <section className="mb-6 rounded-xl border border-Torchwood-border bg-Torchwood-panel/40 p-4">
        <h3 className="mb-3 text-sm font-medium text-slate-200">1. 订阅 accounts.{"{uid}"}</h3>
        <button type="button" className="btn-primary text-xs" disabled={!uid} onClick={connectRealtime}>
          连接 Realtime
        </button>
      </section>

      <section className="mb-6 rounded-xl border border-Torchwood-border bg-Torchwood-panel/40 p-4">
        <h3 className="mb-3 text-sm font-medium text-slate-200">2. 建单 / 模拟到账</h3>
        <div className="flex flex-wrap gap-2">
          <button type="button" className="btn-secondary text-xs" disabled={loading || !hasApiKey} onClick={bootstrapDef}>
            <MethodTag method="POST" /> 确保 gold 定义
          </button>
          <button type="button" className="btn-secondary text-xs" disabled={loading} onClick={createOrder}>
            <MethodTag method="POST" /> createOrder
          </button>
          <button
            type="button"
            className="btn-primary text-xs"
            disabled={loading || !hasApiKey || !uid}
            onClick={simulateFulfill}
          >
            <MethodTag method="POST" /> 模拟支付到账 Grant
          </button>
          <button
            type="button"
            className="btn-secondary text-xs"
            disabled={loading}
            onClick={() => exec("assets.listMyAssets()", () => client.assets.listMyAssets())}
          >
            <MethodTag method="GET" /> listMyAssets
          </button>
        </div>
        {!hasApiKey ? (
          <p className="mt-3 text-xs text-amber-200">模拟到账需要设置页填写 Server API Key。</p>
        ) : null}
      </section>

      <section className="mb-6 rounded-xl border border-Torchwood-border bg-Torchwood-panel/40 p-4">
        <h3 className="mb-3 text-sm font-medium text-slate-200">Realtime 事件（按 domain 分流）</h3>
        {events.length === 0 ? (
          <p className="text-xs text-Torchwood-muted">尚无事件。先连接频道，再 Grant。</p>
        ) : (
          <ul className="space-y-2 text-xs">
            {["payments", "economy", "subscriptions", "unknown"].map((dom) => {
              const group = events.filter((e) => e.domain === dom);
              if (group.length === 0) return null;
              return (
                <li key={dom}>
                  <div className="mb-1 font-semibold text-Torchwood-accent">{dom}</div>
                  {group.map((e, i) => (
                    <div key={`${e.event}-${i}`} className="font-mono text-slate-300">
                      {e.event}
                    </div>
                  ))}
                </li>
              );
            })}
          </ul>
        )}
      </section>

      <JsonPanel title="SDK 响应" data={result} />
    </div>
  );
}
