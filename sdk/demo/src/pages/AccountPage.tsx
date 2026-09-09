import { useState } from "react";
import { useTorchwood } from "@/lib/torchwood-context";
import { ErrorBanner, JsonPanel, MethodTag, PageHeader } from "@/components/Ui";

export function AccountPage() {
  const { client, auth, setAuth, run, lastError } = useTorchwood();
  const [result, setResult] = useState<unknown>(null);
  const [loading, setLoading] = useState(false);
  const [theme, setTheme] = useState("sdk-playground");
  const [lang, setLang] = useState("zh");

  async function exec(label: string, fn: () => Promise<unknown>) {
    setLoading(true);
    try {
      const data = await run(fn);
      setResult({ action: label, data });
    } catch {
      /* error shown via banner */
    } finally {
      setLoading(false);
    }
  }

  return (
    <div>
      <PageHeader
        title="Account API"
        description="演示 Client Account SDK：当前用户、偏好、会话与 Token 刷新。"
      />
      <ErrorBanner message={lastError} />

      <div className="mb-4 flex flex-wrap gap-2">
        <button
          type="button"
          className="btn-secondary"
          disabled={loading}
          onClick={() => exec("account.me()", () => client.account.me())}
        >
          <MethodTag method="GET" /> me()
        </button>
        <button
          type="button"
          className="btn-secondary"
          disabled={loading}
          onClick={() =>
            exec("account.getPrefs()", () => client.account.getPrefs())
          }
        >
          <MethodTag method="GET" /> getPrefs()
        </button>
        <button
          type="button"
          className="btn-secondary"
          disabled={loading}
          onClick={() =>
            exec("account.updatePrefs()", () =>
              client.account.updatePrefs({ theme, lang })
            )
          }
        >
          <MethodTag method="PUT" /> updatePrefs()
        </button>
        <button
          type="button"
          className="btn-secondary"
          disabled={loading}
          onClick={() =>
            exec("account.listSessions()", () => client.account.listSessions())
          }
        >
          <MethodTag method="GET" /> listSessions()
        </button>
        <button
          type="button"
          className="btn-primary"
          disabled={loading || !auth?.refreshToken}
          onClick={() =>
            exec("account.refresh()", async () => {
              const refreshToken = auth?.refreshToken;
              if (!refreshToken) {
                // OAuth 会话无 refresh_token：过期即视为登出。
                return null;
              }
              const tokens = await client.account.refresh(refreshToken);
              setAuth({ ...auth!, accessToken: tokens.access_token, refreshToken: tokens.refresh_token });
              return tokens;
            })
          }
        >
          <MethodTag method="POST" /> refresh()
        </button>
      </div>

      <div className="mb-4 grid gap-3 md:grid-cols-2">
        <label className="block space-y-1">
          <span className="text-xs text-Torchwood-muted">prefs.theme</span>
          <input className="field" value={theme} onChange={(e) => setTheme(e.target.value)} />
        </label>
        <label className="block space-y-1">
          <span className="text-xs text-Torchwood-muted">prefs.lang</span>
          <input className="field" value={lang} onChange={(e) => setLang(e.target.value)} />
        </label>
      </div>

      <JsonPanel title="SDK 响应" data={result} />
    </div>
  );
}
