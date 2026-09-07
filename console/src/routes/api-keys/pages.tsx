import { useCallback, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus, Copy } from "lucide-react";
import {
  listAPIKeys,
  getAPIKey,
  createAPIKey,
  deleteAPIKey,
  type APIKey,
} from "@/api/apiKeys";
import { fetchApiKeyScopeCatalog, type WellKnownScopeResource } from "@/api/wellknown";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import type { ColumnDef } from "@/components/list/DataTable";
import {
  FormPageWrapper,
  FormField,
  DetailPageWrapper,
  DetailGrid,
  DetailSkeleton,
  NotFound,
  BulkDeleteButton,
  RowDeleteButton,
  DeleteButton,
} from "@/components/resource/shared";

const columns: ColumnDef<APIKey>[] = [
  {
    key: "id",
    header: "ID",
    className: "font-mono text-xs max-w-[140px] truncate",
    cell: (k) => k.id,
  },
  { key: "name", header: "名称", cell: (k) => k.name },
  { key: "scopes", header: "Scopes", cell: (k) => k.scopes.join(", ") || "—" },
  {
    key: "status",
    header: "状态",
    cell: (k) => (
      <Badge variant={k.enabled ? "default" : "secondary"}>
        {k.enabled ? "Active" : "Disabled"}
      </Badge>
    ),
  },
  {
    key: "created",
    header: "创建时间",
    cell: (k) => new Date(k.created_at).toLocaleString(),
  },
];

export function ApiKeysListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const queryClient = useQueryClient();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const platformAdmin = isPlatformAdmin(role);

  const { data: keys = [], isLoading } = useQuery({
    queryKey: ["api-keys", projectId],
    queryFn: listAPIKeys,
    enabled: !!projectId,
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteAPIKey(id),
    onSuccess: () => {
      toast.success("API Key 已删除");
      queryClient.invalidateQueries({ queryKey: ["api-keys", projectId] });
    },
  });

  const getSearchText = useCallback(
    (k: APIKey) => `${k.id} ${k.name} ${k.scopes.join(" ")}`,
    []
  );

  const handleBulkDelete = async (selected: APIKey[], clear: () => void) => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selected.map((k) => deleteAPIKey(k.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selected.length} 个 API Key`);
      }
      queryClient.invalidateQueries({ queryKey: ["api-keys", projectId] });
      clear();
    } finally {
      setBulkDeleting(false);
    }
  };

  return (
    <ResourceListPage
      title="API Keys"
      description="管理当前项目的服务端 API Key"
      searchPlaceholder="搜索名称或 ID..."
      isLoading={isLoading}
      items={keys}
      columns={columns}
      getSearchText={getSearchText}
      detailPath={(k) => `/console/api-keys/${k.id}`}
      toolbarActions={
        platformAdmin ? (
          <Button asChild>
            <Link to="/console/api-keys/new">
              <Plus className="h-4 w-4 mr-2" />
              新建 API Key
            </Link>
          </Button>
        ) : undefined
      }
      selectionActions={
        platformAdmin
          ? (selected, clear) => (
              <BulkDeleteButton
                count={selected.length}
                loading={bulkDeleting}
                onConfirm={() => handleBulkDelete(selected, clear)}
              />
            )
          : undefined
      }
      rowActions={
        platformAdmin
          ? (k) => (
              <RowDeleteButton
                onConfirm={() => remove.mutate(k.id)}
                loading={remove.isPending}
              />
            )
          : undefined
      }
      emptyTitle="暂无 API Key"
      emptyDescription="创建 API Key 以访问服务端 API"
      emptyAction={
        platformAdmin ? (
          <Button asChild>
            <Link to="/console/api-keys/new">新建 API Key</Link>
          </Button>
        ) : undefined
      }
    />
  );
}

export function ApiKeyNewPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const [name, setName] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [wildcard, setWildcard] = useState(false);
  const [manualMode, setManualMode] = useState(false);
  const [manualScopes, setManualScopes] = useState("");
  const [createdSecret, setCreatedSecret] = useState<string | null>(null);

  // scope 词表来自 /.well-known/torchwood（服务端策略注册表派生的静态目录，
  // 长缓存即可）；加载失败时退回手动高级输入。
  const {
    data: catalog = [],
    isError: catalogFailed,
  } = useQuery({
    queryKey: ["wellknown-scopes"],
    queryFn: fetchApiKeyScopeCatalog,
    staleTime: Infinity,
  });

  const mutation = useMutation({
    mutationFn: createAPIKey,
    onSuccess: (data) => {
      setCreatedSecret(data.secret);
      toast.success("API Key 创建成功，请立即复制 Secret");
      queryClient.invalidateQueries({ queryKey: ["api-keys", projectId] });
    },
  });

  const toggleScope = (scope: string, checked: boolean) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (checked) {
        next.add(scope);
      } else {
        next.delete(scope);
      }
      return next;
    });
  };

  const buildScopes = (): string[] => {
    if (manualMode) {
      return manualScopes
        .split(",")
        .map((s) => s.trim())
        .filter(Boolean);
    }
    if (wildcard) {
      return ["*"];
    }
    return Array.from(selected).sort();
  };

  const copySecret = () => {
    if (createdSecret) {
      navigator.clipboard.writeText(createdSecret);
      toast.success("Secret 已复制");
    }
  };

  if (createdSecret) {
    return (
      <FormPageWrapper
        title="API Key 已创建"
        description="Secret 仅显示一次，请妥善保存"
        backTo="/console/api-keys"
        submitLabel="返回列表"
        onSubmit={(e) => {
          e.preventDefault();
          navigate("/console/api-keys");
        }}
      >
        <div className="rounded-md bg-muted p-4 flex items-center justify-between gap-4">
          <code className="break-all text-xs flex-1">{createdSecret}</code>
          <Button variant="secondary" size="sm" type="button" onClick={copySecret}>
            <Copy className="h-4 w-4 mr-1" />
            复制
          </Button>
        </div>
      </FormPageWrapper>
    );
  }

  return (
    <FormPageWrapper
      title="新建 API Key"
      description="Secret 创建后仅显示一次"
      backTo="/console/api-keys"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        mutation.mutate({
          name,
          scopes: buildScopes(),
        });
      }}
      loading={mutation.isPending}
      submitDisabled={!isPlatformAdmin(role)}
    >
      <FormField id="name" label="名称" value={name} onChange={setName} required placeholder="Production API Key" />
      {manualMode ? (
        <FormField
          id="manual-scopes"
          label="Scopes（逗号分隔）"
          value={manualScopes}
          onChange={setManualScopes}
          placeholder="users.read, users.write"
        />
      ) : (
        <div className="space-y-3">
          <Label>Scopes</Label>
          <div className="space-y-3 rounded-md border p-4">
            <div className="flex items-center gap-2">
              <Checkbox
                id="scope-wildcard"
                checked={wildcard}
                onChange={(e) => setWildcard(e.target.checked)}
              />
              <Label htmlFor="scope-wildcard" className="font-normal">
                全部权限（<code className="font-mono text-xs">*</code> 通配，含未来新增资源）
              </Label>
            </div>
            {catalog.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                {catalogFailed
                  ? "Scope 词表加载失败，请改用手动高级输入。"
                  : "Scope 词表加载中…"}
              </p>
            ) : (
              <div className="grid gap-x-8 gap-y-2 sm:grid-cols-2">
                {catalog.map((entry) => (
                  <ScopeResourceRow
                    key={entry.resource}
                    entry={entry}
                    disabled={wildcard}
                    selected={selected}
                    onToggle={toggleScope}
                  />
                ))}
              </div>
            )}
          </div>
        </div>
      )}
      <Button
        type="button"
        variant="link"
        className="h-auto px-0 text-xs text-muted-foreground"
        onClick={() => setManualMode((m) => !m)}
      >
        {manualMode ? "返回词表多选" : "手动输入 Scopes（高级，词表接口不可用时）"}
      </Button>
    </FormPageWrapper>
  );
}

// ScopeResourceRow 渲染单资源的 read/write 勾选行（词表条目由服务端目录
// 下发：read/write 为 false 的方向不渲染勾选框）。
function ScopeResourceRow({
  entry,
  disabled,
  selected,
  onToggle,
}: {
  entry: WellKnownScopeResource;
  disabled: boolean;
  selected: Set<string>;
  onToggle: (scope: string, checked: boolean) => void;
}) {
  return (
    <div className="flex items-center gap-3 text-sm">
      <span className="w-32 font-mono text-xs truncate" title={entry.resource}>
        {entry.resource}
      </span>
      {entry.read && (
        <label className="flex items-center gap-1.5">
          <Checkbox
            checked={selected.has(`${entry.resource}.read`)}
            disabled={disabled}
            onChange={(e) => onToggle(`${entry.resource}.read`, e.target.checked)}
          />
          read
        </label>
      )}
      {entry.write && (
        <label className="flex items-center gap-1.5">
          <Checkbox
            checked={selected.has(`${entry.resource}.write`)}
            disabled={disabled}
            onChange={(e) => onToggle(`${entry.resource}.write`, e.target.checked)}
          />
          write
        </label>
      )}
      {!entry.read && !entry.write && <span className="text-muted-foreground">—</span>}
    </div>
  );
}

export function ApiKeyDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();

  const { data: key, isLoading } = useQuery({
    queryKey: ["api-keys", id],
    queryFn: () => getAPIKey(id!),
    enabled: !!id,
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteAPIKey(id),
    onSuccess: () => {
      toast.success("API Key 已删除");
      queryClient.invalidateQueries({ queryKey: ["api-keys", projectId] });
      navigate("/console/api-keys");
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!key) return <NotFound backTo="/console/api-keys" />;

  return (
    <DetailPageWrapper
      title={key.name}
      description="API Key 详情"
      backTo="/console/api-keys"
      actions={
        // 与列表页一致：仅平台 admin 可删除 API Key（G8-5，R11-P1-2）。
        isPlatformAdmin(role) ? (
          <DeleteButton onConfirm={() => remove.mutate(key.id)} loading={remove.isPending} />
        ) : undefined
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: key.id, mono: true },
          { label: "名称", value: key.name },
          { label: "Scopes", value: key.scopes.join(", ") || "—" },
          { label: "状态", value: key.enabled ? "Active" : "Disabled" },
          { label: "过期时间", value: key.expire_at ? new Date(key.expire_at).toLocaleString() : "永不过期" },
          { label: "创建时间", value: new Date(key.created_at).toLocaleString() },
          { label: "更新时间", value: new Date(key.updated_at).toLocaleString() },
        ]}
      />
    </DetailPageWrapper>
  );
}
