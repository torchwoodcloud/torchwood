import { useState } from "react";
import { useTorchwood } from "@/lib/torchwood-context";
import { suffix } from "@/lib/storage";
import { ErrorBanner, JsonPanel, MethodTag, PageHeader } from "@/components/Ui";

export function GroupsPage() {
  const { client, auth, setAuth, run, lastError } = useTorchwood();
  const [result, setResult] = useState<unknown>(null);
  const [loading, setLoading] = useState(false);
  const [groupName, setGroupName] = useState(`Web Group ${suffix()}`);
  const [inviteEmail, setInviteEmail] = useState("invitee@torchwood.local");
  const [selectedGroupId, setSelectedGroupId] = useState("");

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

  async function createGroupFlow() {
    setLoading(true);
    try {
      const group = await run(() => client.groups.createGroup(groupName));
      setSelectedGroupId(group.id);
      // OAuth 会话无 refresh_token：过期即视为登出，不做静默刷新分支。
      const refreshToken = auth?.refreshToken;
      if (refreshToken) {
        const tokens = await run(() => client.account.refresh(refreshToken));
        setAuth({
          ...auth!,
          accessToken: tokens.access_token,
          refreshToken: tokens.refresh_token,
        });
        setResult({ action: "createGroup() + refresh()", data: { group, tokens } });
      } else {
        setResult({ action: "createGroup()", data: group });
      }
    } catch {
      /* banner */
    } finally {
      setLoading(false);
    }
  }

  return (
    <div>
      <PageHeader
        title="Groups API"
        description="创建用户组、刷新 Token 获取 group 角色、邀请成员并列出成员。"
      />
      <ErrorBanner message={lastError} />

      <div className="mb-4 grid gap-3 md:grid-cols-2">
        <label className="block space-y-1">
          <span className="text-xs text-Torchwood-muted">用户组名称</span>
          <input className="field" value={groupName} onChange={(e) => setGroupName(e.target.value)} />
        </label>
        <label className="block space-y-1">
          <span className="text-xs text-Torchwood-muted">groupId（邀请/列表用）</span>
          <input
            className="field"
            value={selectedGroupId}
            onChange={(e) => setSelectedGroupId(e.target.value)}
            placeholder="创建用户组后自动填入"
          />
        </label>
        <label className="block space-y-1 md:col-span-2">
          <span className="text-xs text-Torchwood-muted">邀请邮箱</span>
          <input
            className="field"
            type="email"
            value={inviteEmail}
            onChange={(e) => setInviteEmail(e.target.value)}
          />
        </label>
      </div>

      <div className="mb-4 flex flex-wrap gap-2">
        <button type="button" className="btn-primary" disabled={loading} onClick={createGroupFlow}>
          <MethodTag method="POST" /> createGroup() + refresh()
        </button>
        <button
          type="button"
          className="btn-secondary"
          disabled={loading}
          onClick={() => exec("groups.listGroups()", () => client.groups.listGroups())}
        >
          <MethodTag method="GET" /> listGroups()
        </button>
        <button
          type="button"
          className="btn-secondary"
          disabled={loading || !selectedGroupId}
          onClick={() =>
            exec("groups.createMembership()", () =>
              client.groups.createMembership(selectedGroupId, {
                email: inviteEmail,
                name: "Invited Member",
                roles: ["member"],
              })
            )
          }
        >
          <MethodTag method="POST" /> createMembership()
        </button>
        <button
          type="button"
          className="btn-secondary"
          disabled={loading || !selectedGroupId}
          onClick={() =>
            exec("groups.listMemberships()", () =>
              client.groups.listMemberships(selectedGroupId)
            )
          }
        >
          <MethodTag method="GET" /> listMemberships()
        </button>
      </div>

      <JsonPanel title="SDK 响应" data={result} />
    </div>
  );
}
