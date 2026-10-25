import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@/router/compat";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCollection, useCommand, useCommandForm } from "@lynx-go/ui-kit";
import { toast } from "sonner";
import { ArrowLeft, Plus, Copy, Pencil, RefreshCw } from "lucide-react";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import {
  getAPIKey,
  deleteAPIKey,
  type APIKey,
} from "@/api/apiKeys";
import { fetchApiKeyScopeCatalog, type WellKnownScopeResource } from "@/api/wellknown";
import {
  apiKeysResource,
  apiKeysSearch,
  enabledOf,
  createApiKeyCommand,
  removeApiKeyCommand,
  editApiKeyDescriptor,
  expireApiKeyDescriptor,
  type CreateApiKeyForm,
  type EditApiKeyForm,
} from "@/fleet/apiKeys";
import { describeApiError } from "@/fleet/errors";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime, fromDateTimeLocalValue, toDateTimeLocalValue } from "@/lib/datetime";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { PageHeader } from "@/components/PageHeader";
import {
  FormPageWrapper,
  DetailPageWrapper,
  DetailGrid,
  DetailSkeleton,
  NotFound,
  BulkDeleteButton,
  RowDeleteButton,
  DeleteButton,
} from "@/components/resource/shared";
import { Form as FleetForm } from "@/components/fleet/command/form";
import { TextField } from "@/components/fleet/command/fields";
import { DataTable } from "@/components/fleet/query/data-table";
import { KeysetPagination } from "@/components/fleet/query/keyset-pagination";
import { ListPage as FleetListPage } from "@/components/fleet/query/list-page";

// 命令族切片（design.md §12.3）：写路径换 useCommand/useCommandForm（结果
// 两分 + __skipToast + onSettled toast 单源）；失效 = kit 键自动 + 存量
// ["api-keys"] 键手动桥（isFleetKey 守卫）。列表骨架（ResourceListPage/
// useServerPaging/页内搜索/批量选择）留存量——kit DataTable 行选择未至。

export function ApiKeysListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const platformAdmin = isPlatformAdmin(role);

  // 列表 fleet 化（§12.9-2）：游标/页大小/enabled 过滤进 URL（分页栈 =
  // 浏览器历史）；页内当前页搜索保持本地态（旧页语义）。
  const list = useCollection(apiKeysResource, {
    from: "/console/api-keys",
    search: apiKeysSearch,
    scope: { projectId: projectId ?? undefined },
    query: { enabled: !!projectId },
  });

  const enabled = enabledOf(list.query.predicate);
  const setEnabledFilter = (v: string) => {
    list.setPredicate(v === "all" ? null : { field: "enabled", op: "eq", value: v });
  };

  // 命令接线：toast 单源 onSettled；kit 键自动失效 + 存量 ["api-keys"] 键手动桥。
  const remove = useCommand(removeApiKeyCommand, {
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("API Key 已删除");
        void queryClient.invalidateQueries({ queryKey: ["api-keys"] });
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const getSearchText = useCallback(
    (k: APIKey) => `${k.id} ${k.name} ${k.scopes.join(" ")}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((k) => getSearchText(k).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const toggleRow = (id: string) =>
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const toggleAll = (visibleIds: string[]) =>
    setSelectedIds((prev) =>
      new Set(visibleIds.length > 0 && visibleIds.every((id) => prev.has(id)) ? [] : visibleIds)
    );
  const selectedKeys = list.rows.filter((k) => selectedIds.has(k.id));

  const handleBulkDelete = async () => {
    setBulkDeleting(true);
    try {
      // N 命令聚合非 useCommand 面（§12.3 裁决 8）：单条失败汇总展示。
      const results = await Promise.allSettled(
        selectedKeys.map((k) => deleteAPIKey(k.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selectedKeys.length} 个 API Key`);
      }
      void queryClient.invalidateQueries({ queryKey: ["api-keys"] });
      setSelectedIds(new Set());
    } finally {
      setBulkDeleting(false);
    }
  };

  const tableColumns: LegacyColumnDef<APIKey, unknown>[] = [
    {
      id: "id",
      header: "ID",
      cell: ({ row }) => (
        <span className="inline-block max-w-[140px] truncate font-mono text-xs">
          {row.original.id}
        </span>
      ),
    },
    { id: "name", header: "名称", cell: ({ row }) => row.original.name },
    {
      id: "scopes",
      header: "Scopes",
      cell: ({ row }) => row.original.scopes.join(", ") || "—",
    },
    {
      id: "status",
      header: "状态",
      cell: ({ row }) => (
        <Badge variant={row.original.enabled ? "default" : "secondary"}>
          {row.original.enabled ? "Active" : "Disabled"}
        </Badge>
      ),
    },
    {
      id: "created",
      header: "创建时间",
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    ...(platformAdmin
      ? [
          {
            id: "actions",
            header: "操作",
            cell: ({ row }: { row: { original: APIKey } }) => (
              <RowDeleteButton
                onConfirm={() => remove.mutate({ id: row.original.id })}
                loading={remove.pending}
              />
            ),
          } satisfies LegacyColumnDef<APIKey, unknown>,
        ]
      : []),
  ];

  const filtered = list.query.predicate != null;

  return (
    <div className="space-y-6">
      <PageHeader title="API Keys" description="管理当前项目的服务端 API Key" />

      <FleetListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="当前页内搜索名称或 ID..."
              className="h-8 w-56"
            />
            <Select
              value={enabled === undefined ? "all" : enabled ? "enabled" : "disabled"}
              onValueChange={setEnabledFilter}
            >
              <SelectTrigger className="h-8 w-[120px]" aria-label="状态过滤">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">全部状态</SelectItem>
                <SelectItem value="enabled">启用</SelectItem>
                <SelectItem value="disabled">禁用</SelectItem>
              </SelectContent>
            </Select>
            {selectedKeys.length > 0 && platformAdmin ? (
              <BulkDeleteButton
                count={selectedKeys.length}
                loading={bulkDeleting}
                onConfirm={() => void handleBulkDelete()}
              />
            ) : null}
            {platformAdmin ? (
              <Button asChild className="ml-auto">
                <Link to="/console/api-keys/new">
                  <Plus className="h-4 w-4 mr-2" />
                  新建 API Key
                </Link>
              </Button>
            ) : null}
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} />
          </div>
        }
        emptyTitle={search !== "" ? "没有匹配的 API Key" : filtered ? "该状态下暂无 API Key" : "暂无 API Key"}
        emptyDescription={
          search !== ""
            ? "调整搜索词，或清空恢复当前页全部行"
            : filtered
              ? "切换状态过滤或清空恢复全部"
              : "创建 API Key 以访问服务端 API"
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={tableColumns}
            getRowId={(row) => row.id}
            onRowClick={(k) => navigate(`/console/api-keys/${k.id}`)}
            selection={
              platformAdmin
                ? {
                    selectedIds,
                    onToggle: toggleRow,
                    onToggleAll: toggleAll,
                  }
                : undefined
            }
          />
        </div>
      </FleetListPage>
    </div>
  );
}

export function ApiKeyNewPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();

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

  // useCommandForm：secret = reveal（ok payload，卸载即弃，§12.3 裁决 5）。
  const create = useCommandForm(createApiKeyCommand, {
    defaultValues: { name: "", wildcard: false, selected: [] as string[], manual: "" },
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("API Key 创建成功，请立即复制 Secret");
        void queryClient.invalidateQueries({ queryKey: ["api-keys"] });
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });
  const form = create.form;
  const createdSecret = create.reveal?.secret;

  const copySecret = () => {
    if (createdSecret) {
      navigator.clipboard.writeText(createdSecret);
      toast.success("Secret 已复制");
    }
  };

  if (createdSecret !== undefined) {
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
    <div className="mx-auto max-w-4xl space-y-6 p-6">
      <div className="flex items-start gap-4">
        <Button variant="ghost" size="icon" asChild aria-label="返回">
          <Link to="/console/api-keys">
            <ArrowLeft className="h-4 w-4" />
          </Link>
        </Button>
        <div>
          <h1 className="text-lg font-semibold">新建 API Key</h1>
          <p className="text-sm text-muted-foreground">Secret 创建后仅显示一次</p>
        </div>
      </div>
      {/* Form 块自带提交/取消（宿主不渲染 footer——FormPageWrapper 的 onSubmit
          形态与 Form 内芯互斥，§12.3 裁决 1/7） */}
      <div className="max-w-4xl">
        <FleetForm
          form={form}
          submitting={create.submitting}
          result={create.result}
          submitLabel="创建"
          onCancel={() => navigate("/console/api-keys")}
        >
          <TextField
            form={form}
            name="name"
            label="名称"
            required
            placeholder="Production API Key"
          />
          <div className="space-y-3">
            <Label>Scopes</Label>
            <div className="space-y-4 rounded-md border p-5">
              <form.Field name="wildcard">
                {(field: { state: { value: unknown }; handleChange(value: unknown): void }) => (
                  <div className="flex items-center gap-2">
                    <Checkbox
                      id="scope-wildcard"
                      checked={Boolean(field.state.value)}
                      onChange={(e) => field.handleChange(e.target.checked)}
                    />
                    <Label htmlFor="scope-wildcard" className="font-normal">
                      全部权限（<code className="font-mono text-xs">*</code> 通配，含未来新增资源）
                    </Label>
                  </div>
                )}
              </form.Field>
              {catalog.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  {catalogFailed
                    ? "Scope 词表加载失败，可直接在下方手动输入。"
                    : "Scope 词表加载中…"}
                </p>
              ) : (
                // wildcard 跨字段联动经 Subscribe（selected 字段子树单独订阅，
                // 直读 form.state.values 会在 wildcard 翻转时不重渲染）
                <form.Subscribe selector={(s: { values: CreateApiKeyForm }) => s.values.wildcard}>
                  {(wildcard: boolean) => (
                    <form.Field name="selected">
                      {(field: {
                        state: { value: unknown };
                        handleChange(value: unknown): void;
                      }) => {
                        const selected = new Set(
                          Array.isArray(field.state.value) ? (field.state.value as string[]) : [],
                        );
                        const toggle = (scope: string, checked: boolean) => {
                          const next = new Set(selected);
                          if (checked) next.add(scope);
                          else next.delete(scope);
                          field.handleChange(Array.from(next).sort());
                        };
                        return (
                          <div className="grid gap-x-10 gap-y-3 sm:grid-cols-2">
                            {catalog.map((entry) => (
                              <ScopeResourceRow
                                key={entry.resource}
                                entry={entry}
                                disabled={wildcard}
                                selected={selected}
                                onToggle={toggle}
                              />
                            ))}
                          </div>
                        );
                      }}
                    </form.Field>
                  )}
                </form.Subscribe>
              )}
              <div className="space-y-2 border-t pt-4">
                <form.Field name="manual">
                  {(field: {
                    state: { value: unknown };
                    handleChange(value: unknown): void;
                  }) => (
                    <>
                      <Label htmlFor="manual-scopes">自定义 Scopes（可选，逗号或换行分隔）</Label>
                      <textarea
                        id="manual-scopes"
                        value={String(field.state.value ?? "")}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder={"databases:blog.read\nmyapp:reports.read"}
                        rows={4}
                        className="flex w-full rounded-lg border border-input bg-background px-3 py-2 font-mono text-xs text-sm transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-50"
                      />
                      <p className="text-xs text-muted-foreground">
                        追加词表之外的 scope，与上方勾选合并生效：支持资源级限定（如{" "}
                        <code>databases:blog.read</code> 单库只读）与自定义服务 scope；词表不可用时也可完全在此手动输入。
                      </p>
                    </>
                  )}
                </form.Field>
              </div>
            </div>
          </div>
        </FleetForm>
      </div>
    </div>
  );
}

// ScopeResourceRow 渲染单资源的 read/write/admin 勾选行（词表条目由服务端
// 目录下发：为 false 的方向不渲染勾选框）。
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
      {entry.admin && (
        <label className="flex items-center gap-1.5">
          <Checkbox
            checked={selected.has(`${entry.resource}.admin`)}
            disabled={disabled}
            onChange={(e) => onToggle(`${entry.resource}.admin`, e.target.checked)}
          />
          admin
        </label>
      )}
      {!entry.read && !entry.write && !entry.admin && <span className="text-muted-foreground">—</span>}
    </div>
  );
}

export function ApiKeyDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const [editOpen, setEditOpen] = useState(false);
  const [rotateOpen, setRotateOpen] = useState(false);
  const tz = useUserTimezone();

  const { data: key, isLoading } = useQuery({
    queryKey: ["api-keys", id],
    queryFn: () => getAPIKey(id!),
    enabled: !!id,
  });

  const remove = useCommand(removeApiKeyCommand, {
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("API Key 已删除");
        void queryClient.invalidateQueries({ queryKey: ["api-keys"] });
        navigate("/console/api-keys");
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!key) return <NotFound backTo="/console/api-keys" />;

  const editable = isPlatformAdmin(role);

  return (
    <DetailPageWrapper
      title={key.name}
      description="API Key 详情"
      backTo="/console/api-keys"
      actions={
        // 与列表页一致：仅平台 admin 可编辑/轮换/删除 API Key（G8-5，R11-P1-2）。
        editable ? (
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => setEditOpen(true)}>
              <Pencil className="h-4 w-4 mr-2" />
              编辑
            </Button>
            <Button variant="outline" onClick={() => setRotateOpen(true)}>
              <RefreshCw className="h-4 w-4 mr-2" />
              轮换
            </Button>
            <DeleteButton onConfirm={() => remove.mutate({ id: key.id })} loading={remove.pending} />
          </div>
        ) : undefined
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: key.id, mono: true },
          { label: "名称", value: key.name },
          { label: "Scopes", value: key.scopes.join(", ") || "—" },
          { label: "状态", value: key.enabled ? "Active" : "Disabled" },
          { label: "过期时间", value: key.expire_at ? formatDateTime(key.expire_at, tz) : "永不过期" },
          { label: "创建时间", value: formatDateTime(key.created_at, tz) },
          { label: "更新时间", value: formatDateTime(key.updated_at, tz) },
        ]}
      />
      <ApiKeyEditDialog
        key_={key}
        open={editOpen}
        onOpenChange={setEditOpen}
      />
      <ApiKeyRotateDialog
        key_={key}
        open={rotateOpen}
        onOpenChange={setRotateOpen}
      />
    </DetailPageWrapper>
  );
}

// ApiKeyEditDialog 编辑 key 治理字段（T-02 UpdateAPIKey）：name/scopes/
// enabled/expire_at。useCommandForm + descriptor 工厂（diff 归 descriptor，
// 只传变更字段）；expireAt 字段值存 ISO（tz 换算在字段边界）。
function ApiKeyEditDialog({
  key_,
  open,
  onOpenChange,
}: {
  key_: APIKey;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const tz = useUserTimezone();
  const { projectId } = useAuth();

  const edit = useCommandForm(editApiKeyDescriptor(key_), {
    defaultValues: {
      name: key_.name,
      scopes: key_.scopes.join(", "),
      enabled: key_.enabled,
      expireAt: key_.expire_at ? new Date(key_.expire_at).toISOString() : "",
    } satisfies EditApiKeyForm,
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("API Key 已更新");
        void queryClient.invalidateQueries({ queryKey: ["api-keys"] });
        onOpenChange(false);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });
  const form = edit.form;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>编辑 API Key</DialogTitle>
          <DialogDescription>
            禁用与过期立即生效（每次请求鉴权实时校验）；Secret 不可修改。
          </DialogDescription>
        </DialogHeader>
        <FleetForm
          form={form}
          submitting={edit.submitting}
          result={edit.result}
          submitLabel="保存"
          onCancel={() => onOpenChange(false)}
        >
          <TextField form={form} name="name" label="名称" required />
          <form.Field name="scopes">
            {(field: {
              state: { value: unknown; meta: { errors: ReadonlyArray<unknown> } };
              handleChange(value: unknown): void;
            }) => (
              <label className="block space-y-1">
                <span className="text-xs font-medium text-muted-foreground">
                  Scopes（逗号分隔）
                </span>
                <Input
                  value={String(field.state.value ?? "")}
                  onChange={(e) => field.handleChange(e.target.value)}
                  placeholder="databases:blog.read, storage:media"
                  className="font-mono text-xs"
                />
                <p className="text-xs text-muted-foreground">
                  支持资源级限定（如 <code>databases:blog</code> 限定单个库，
                  <code>databases:blog.read</code> 单库只读）；清空 Scopes 请删除重建。
                </p>
              </label>
            )}
          </form.Field>
          <form.Field name="enabled">
            {(field: { state: { value: unknown }; handleChange(value: unknown): void }) => (
              <div className="flex items-center gap-2">
                <Checkbox
                  id="edit-enabled"
                  checked={Boolean(field.state.value)}
                  onChange={(e) => field.handleChange(e.target.checked)}
                />
                <Label htmlFor="edit-enabled" className="font-normal">
                  启用（取消勾选 = 立即禁用，所有请求返回 401）
                </Label>
              </div>
            )}
          </form.Field>
          <form.Field name="expireAt">
            {(field: {
              state: { value: unknown };
              handleChange(value: unknown): void;
            }) => {
              const iso = typeof field.state.value === "string" ? field.state.value : "";
              const local = iso ? toDateTimeLocalValue(iso, tz) : "";
              return (
                <label className="block space-y-1">
                  <span className="text-xs font-medium text-muted-foreground">
                    过期时间（留空 = 保持不变/永不过期）
                  </span>
                  <Input
                    type="datetime-local"
                    value={local}
                    onChange={(e) =>
                      field.handleChange(fromDateTimeLocalValue(e.target.value, tz))
                    }
                  />
                  <p className="text-xs text-muted-foreground">过期后该 Key 立即失效（401）。</p>
                </label>
              );
            }}
          </form.Field>
        </FleetForm>
      </DialogContent>
    </Dialog>
  );
}

// ApiKeyRotateDialog 轮换引导（T-02）：不提供原地换 secret——平滑轮换 =
// 新建 key（预填同名 -rotated 后缀 + 相同 scopes）→ 切换应用 → 旧 key 设
// 过期下线。命令接线 useCommand×2（§12.3 裁决 8）。
function ApiKeyRotateDialog({
  key_,
  open,
  onOpenChange,
}: {
  key_: APIKey;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const { projectId } = useAuth();
  const [step, setStep] = useState<"intro" | "created">("intro");
  const [newKey, setNewKey] = useState<APIKey | null>(null);
  const [newSecret, setNewSecret] = useState("");

  const create = useCommand(createApiKeyCommand, {
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        setNewKey(result.payload.api_key);
        setNewSecret(result.payload.secret);
        setStep("created");
        void queryClient.invalidateQueries({ queryKey: ["api-keys"] });
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const expireOld = useCommand(expireApiKeyDescriptor, {
    scope: { projectId: projectId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("旧 Key 已设置立即过期，轮换完成");
        void queryClient.invalidateQueries({ queryKey: ["api-keys"] });
        onOpenChange(false);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const close = (o: boolean) => {
    if (!o) {
      setStep("intro");
      setNewSecret("");
      setNewKey(null);
    }
    onOpenChange(o);
  };

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>轮换 API Key</DialogTitle>
          <DialogDescription>
            Secret 不支持原地更换；平滑轮换 = 双 Key 并存过渡。
          </DialogDescription>
        </DialogHeader>
        {step === "intro" ? (
          <div className="space-y-4 text-sm">
            <ol className="list-decimal list-inside space-y-1.5 text-muted-foreground">
              <li>创建新 Key（自动预填相同 Scopes，名称加 -rotated 后缀）</li>
              <li>将应用切换到新 Key（旧 Key 在此期间继续可用）</li>
              <li>回到本页为旧 Key 设置过期时间，完成下线</li>
            </ol>
            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={() => close(false)}>
                取消
              </Button>
              <Button
                onClick={() =>
                  create.mutate({
                    name: `${key_.name}-rotated`.slice(0, 120),
                    wildcard: false,
                    selected: key_.scopes,
                    manual: "",
                  } satisfies CreateApiKeyForm)
                }
                disabled={create.pending}
              >
                {create.pending ? "创建中…" : "① 创建新 Key"}
              </Button>
            </div>
          </div>
        ) : (
          <div className="space-y-4">
            <div className="rounded-md bg-muted p-3 flex items-center justify-between gap-3">
              <code className="break-all text-xs flex-1">{newSecret}</code>
              <Button
                variant="secondary"
                size="sm"
                type="button"
                onClick={() => {
                  navigator.clipboard.writeText(newSecret);
                  toast.success("Secret 已复制");
                }}
              >
                <Copy className="h-4 w-4 mr-1" />
                复制
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">
              新 Key <span className="font-mono">{newKey?.id}</span> 的 Secret 仅显示这一次。
              切换应用后，可为旧 Key 设置过期（也可稍后在旧 Key 详情页操作）。
            </p>
            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={() => close(false)}>
                稍后手动下线
              </Button>
              <Button
                variant="outline"
                onClick={() => {
                  close(false);
                  navigate(`/console/api-keys/${key_.id}`);
                }}
              >
                前往旧 Key 设置过期
              </Button>
              <Button onClick={() => expireOld.mutate({ id: key_.id })} disabled={expireOld.pending}>
                {expireOld.pending ? "处理中…" : "② 旧 Key 立即过期"}
              </Button>
            </div>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
