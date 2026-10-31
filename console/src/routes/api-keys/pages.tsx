import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  invalidateAfterCommand,
  useCollection,
  useCommand,
  useCommandForm,
  useRecord,
} from "@lynx-go/uikit-core";
import { toast } from "sonner";
import { ArrowLeft, Plus, Copy, Pencil, RefreshCw } from "lucide-react";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import {
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
  expireOldApiKeyCommand,
  type CreateApiKeyForm,
  type EditApiKeyForm,
} from "@/kit/apiKeys";
import { describeApiError } from "@/kit/errors";
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
import { DateTimePicker } from "@/components/ui/datetime-picker";
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
import { Form as KitForm } from "@/components/kit/command/form";
import { TextField } from "@/components/kit/command/fields";
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";

// 命令族切片（design.md §12.3）：写路径换 useCommand/useCommandForm（结果
// 两分 + __skipToast + onSettled toast 单源）；失效 = descriptor 声明（kit
// 键——列表 collection 与详情 record 同资源全 kind，子视图 kit 化批换绑后
// 存量前缀桥退役）。列表已 fleet 化（§12.9-1）：useCollection + DataTable
// selection 受控面（批量删/行删接线）。

export function ApiKeysListPage() {
  const { role } = useAdminRole();
  const { projectId } = useAuth();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const platformAdmin = isPlatformAdmin(role);

  // 列表 fleet 化（§12.9-2）：游标/页大小/enabled 过滤进 URL（分页栈 =
  // 浏览器历史）；页内当前页搜索保持本地态（旧页语义）。
  // scope/enabled 不再逐处传（§12.10-③）：资源声明了 projectId，Provider 单源 + 未就绪自动停用
  const list = useCollection(apiKeysResource, {
    from: "/console/api-keys",
    search: apiKeysSearch,
  });

  const enabled = enabledOf(list.query.predicate);
  const setEnabledFilter = (v: string) => {
    list.setPredicate(v === "all" ? null : { field: "enabled", op: "eq", value: v });
  };

  // 命令接线：toast 单源 onSettled；失效 = descriptor 声明（kit 键同资源全 kind）。
  const remove = useCommand(removeApiKeyCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("API Key deleted");
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
        toast.error(`Delete completed: ${succeeded} succeeded, ${failed} failed`);
      } else {
        toast.success(`Deleted ${selectedKeys.length} API Keys`);
      }
      // 直调批量路径无命令容器（§12.3 裁决 8 面不变）：复用 remove 命令的
      // 失效声明（invalidateAfterCommand 单源——kit api-keys 键全 kind：列表
      // collection 全游标页 + 详情 record 一并命中；存量 ["api-keys"] 前缀
      // 桥随子视图 kit 化退役）。
      invalidateAfterCommand(queryClient, removeApiKeyCommand, {
        projectId: projectId ?? undefined,
      });
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
    { id: "name", header: "Name", cell: ({ row }) => row.original.name },
    {
      id: "scopes",
      header: "Scopes",
      cell: ({ row }) => row.original.scopes.join(", ") || "—",
    },
    {
      id: "status",
      header: "Status",
      cell: ({ row }) => (
        <Badge variant={row.original.enabled ? "default" : "secondary"}>
          {row.original.enabled ? "Active" : "Disabled"}
        </Badge>
      ),
    },
    {
      id: "created",
      header: "Created",
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    ...(platformAdmin
      ? [
          {
            id: "actions",
            header: "Actions",
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
      <PageHeader title="API Keys" description="Manage server API Keys for the current project" />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search name or ID on this page..."
              className="h-8 w-56"
            />
            <Select
              value={enabled === undefined ? "all" : enabled ? "enabled" : "disabled"}
              onValueChange={setEnabledFilter}
            >
              <SelectTrigger className="h-8 w-[120px]" aria-label="Filter by status">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All statuses</SelectItem>
                <SelectItem value="enabled">Enabled</SelectItem>
                <SelectItem value="disabled">Disabled</SelectItem>
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
              <Button size="sm" asChild className="ml-auto">
                <Link to="/console/api-keys/new">
                  <Plus className="h-4 w-4 mr-2" />
                  New API Key
                </Link>
              </Button>
            ) : null}
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
          </div>
        }
        emptyTitle={search !== "" ? "No matching API Keys" : filtered ? "No API Keys with this status" : "No API Keys yet"}
        emptyDescription={
          search !== ""
            ? "Adjust the search term, or clear it to restore all rows on this page"
            : filtered
              ? "Change the status filter or clear it to show all"
              : "Create an API Key to access server APIs"
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={tableColumns}
            getRowId={(row) => row.id}
            onRowClick={(k) => navigate({ to: `/console/api-keys/${k.id}` })}
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
      </KitListPage>
    </div>
  );
}

export function ApiKeyNewPage() {
  const navigate = useNavigate();

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
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("API Key created. Copy the secret now");
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
      toast.success("Secret copied");
    }
  };

  if (createdSecret !== undefined) {
    return (
      <FormPageWrapper
        title="API Key created"
        description="The secret is shown only once. Store it safely"
        backTo="/console/api-keys"
        submitLabel="Back to list"
        onSubmit={(e) => {
          e.preventDefault();
          navigate({ to: "/console/api-keys" });
        }}
      >
        <div className="rounded-md bg-muted p-4 flex items-center justify-between gap-4">
          <code className="break-all text-xs flex-1">{createdSecret}</code>
          <Button variant="secondary" size="sm" type="button" onClick={copySecret}>
            <Copy className="h-4 w-4 mr-1" />
            Copy
          </Button>
        </div>
      </FormPageWrapper>
    );
  }

  return (
    <div className="mx-auto max-w-4xl space-y-6 p-6">
      <div className="flex items-start gap-4">
        <Button variant="ghost" size="icon" asChild aria-label="Back">
          <Link to="/console/api-keys">
            <ArrowLeft className="h-4 w-4" />
          </Link>
        </Button>
        <div>
          <h1 className="text-lg font-semibold">New API Key</h1>
          <p className="text-sm text-muted-foreground">The secret is shown only once after creation</p>
        </div>
      </div>
      {/* Form 块自带提交/取消（宿主不渲染 footer——FormPageWrapper 的 onSubmit
          形态与 Form 内芯互斥，§12.3 裁决 1/7） */}
      <div className="max-w-4xl">
        <KitForm
          form={form}
          submitting={create.submitting}
          result={create.result}
          submitLabel="Create"
          submittingLabel="Creating…"
          cancelLabel="Cancel"
          onCancel={() => navigate({ to: "/console/api-keys" })}
        >
          <TextField
            form={form}
            name="name"
            label="Name"
            required="Required"
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
                      All permissions (<code className="font-mono text-xs">*</code> wildcard, includes future resources)
                    </Label>
                  </div>
                )}
              </form.Field>
              {catalog.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  {catalogFailed
                    ? "Failed to load the scope catalog. Enter scopes manually below instead."
                    : "Loading scope catalog…"}
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
                      <Label htmlFor="manual-scopes">Custom scopes (optional, comma- or newline-separated)</Label>
                      <textarea
                        id="manual-scopes"
                        value={String(field.state.value ?? "")}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder={"databases:blog.read\nmyapp:reports.read"}
                        rows={4}
                        className="flex w-full rounded-lg border border-input bg-background px-3 py-2 font-mono text-xs text-sm transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-50"
                      />
                      <p className="text-xs text-muted-foreground">
                        Add scopes beyond the catalog, merged with the selections above: supports
                        resource-scoped scopes (e.g.{" "}
                        <code>databases:blog.read</code> for read-only on a single database) and
                        custom service scopes; if the catalog is unavailable, you can also enter
                        scopes entirely here.
                      </p>
                    </>
                  )}
                </form.Field>
              </div>
            </div>
          </div>
        </KitForm>
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
  const { id } = useParams({ strict: false });
  const navigate = useNavigate();
  const { role } = useAdminRole();
  const { projectId } = useAuth();
  const [editOpen, setEditOpen] = useState(false);
  const [rotateOpen, setRotateOpen] = useState(false);
  const tz = useUserTimezone();

  // 详情读换轨 useRecord（子视图 kit 化批）：id 未就绪即停用（与旧 enabled
  // 同义）；projectId 未就绪由声明键自动停用；404 → null 走 NotFound 分支。
  const keyRecord = useRecord(apiKeysResource, {
    id: id ?? undefined,
    scope: { projectId: projectId ?? undefined },
  });
  const key = keyRecord.data;
  const isLoading = keyRecord.status === "loading";

  const remove = useCommand(removeApiKeyCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("API Key deleted");
        navigate({ to: "/console/api-keys" });
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
      description="API Key details"
      backTo="/console/api-keys"
      actions={
        // 与列表页一致：仅平台 admin 可编辑/轮换/删除 API Key（G8-5，R11-P1-2）。
        editable ? (
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => setEditOpen(true)}>
              <Pencil className="h-4 w-4 mr-2" />
              Edit
            </Button>
            <Button variant="outline" onClick={() => setRotateOpen(true)}>
              <RefreshCw className="h-4 w-4 mr-2" />
              Rotate
            </Button>
            <DeleteButton onConfirm={() => remove.mutate({ id: key.id })} loading={remove.pending} />
          </div>
        ) : undefined
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: key.id, mono: true },
          { label: "Name", value: key.name },
          { label: "Scopes", value: key.scopes.join(", ") || "—" },
          { label: "Status", value: key.enabled ? "Active" : "Disabled" },
          { label: "Expires", value: key.expire_at ? formatDateTime(key.expire_at, tz) : "Never expires" },
          { label: "Created", value: formatDateTime(key.created_at, tz) },
          { label: "Updated", value: formatDateTime(key.updated_at, tz) },
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
  const { projectId } = useAuth();
  const tz = useUserTimezone();

  const edit = useCommandForm(editApiKeyDescriptor(key_), {
    // 裸 descriptor 不采纳 Provider context：显式传 scope（与详情 record 键
    // 同 projectId 轴），ok 后 invalidates 声明命中 kit 键刷新详情。
    scope: { projectId: projectId ?? undefined },
    defaultValues: {
      name: key_.name,
      scopes: key_.scopes.join(", "),
      enabled: key_.enabled,
      expireAt: key_.expire_at ? new Date(key_.expire_at).toISOString() : "",
    } satisfies EditApiKeyForm,
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("API Key updated");
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
          <DialogTitle>Edit API Key</DialogTitle>
          <DialogDescription>
            Disabling and expiry take effect immediately (checked on every request); the secret cannot be changed.
          </DialogDescription>
        </DialogHeader>
        <KitForm
          form={form}
          submitting={edit.submitting}
          result={edit.result}
          submitLabel="Save"
          submittingLabel="Saving…"
          cancelLabel="Cancel"
          onCancel={() => onOpenChange(false)}
        >
          <TextField form={form} name="name" label="Name" required="Required" />
          <form.Field name="scopes">
            {(field: {
              state: { value: unknown; meta: { errors: ReadonlyArray<unknown> } };
              handleChange(value: unknown): void;
            }) => (
              <label className="block space-y-1">
                <span className="text-xs font-medium text-muted-foreground">
                  Scopes (comma-separated)
                </span>
                <Input
                  value={String(field.state.value ?? "")}
                  onChange={(e) => field.handleChange(e.target.value)}
                  placeholder="databases:blog.read, storage:media"
                  className="font-mono text-xs"
                />
                <p className="text-xs text-muted-foreground">
                  Supports resource-scoped scopes (e.g. <code>databases:blog</code> targets a
                  single database, <code>databases:blog.read</code> makes it read-only); to clear
                  all scopes, delete and recreate the key.
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
                  Enabled (uncheck to disable immediately; all requests return 401)
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
                    Expiry (leave empty to keep unchanged / never expires)
                  </span>
                  <DateTimePicker
                    value={local}
                    onChange={(v) => field.handleChange(fromDateTimeLocalValue(v, tz))}
                  />
                  <p className="text-xs text-muted-foreground">Once expired, this key stops working immediately (401).</p>
                </label>
              );
            }}
          </form.Field>
        </KitForm>
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
  const navigate = useNavigate();
  const [step, setStep] = useState<"intro" | "created">("intro");
  const [newKey, setNewKey] = useState<APIKey | null>(null);
  const [newSecret, setNewSecret] = useState("");

  const create = useCommand(createApiKeyCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        setNewKey(result.payload.api_key);
        setNewSecret(result.payload.secret);
        setStep("created");
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 资源挂载句柄（非裸 descriptor）：缺省同资源失效生效（Provider context
  // 采纳——kit 键全 kind，列表与详情 record 一并刷新）
  const expireOld = useCommand(expireOldApiKeyCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Old key set to expire immediately; rotation complete");
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
          <DialogTitle>Rotate API Key</DialogTitle>
          <DialogDescription>
            The secret cannot be replaced in place; smooth rotation means two keys coexist during the transition.
          </DialogDescription>
        </DialogHeader>
        {step === "intro" ? (
          <div className="space-y-4 text-sm">
            <ol className="list-decimal list-inside space-y-1.5 text-muted-foreground">
              <li>Create a new key (same scopes pre-filled; the name gets a -rotated suffix)</li>
              <li>Switch your app to the new key (the old key keeps working in the meantime)</li>
              <li>Come back here to set an expiry on the old key to retire it</li>
            </ol>
            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={() => close(false)}>
                Cancel
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
                {create.pending ? "Creating…" : "① Create new key"}
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
                  toast.success("Secret copied");
                }}
              >
                <Copy className="h-4 w-4 mr-1" />
                Copy
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">
              The secret of new key <span className="font-mono">{newKey?.id}</span> is shown only
              this once. After switching your app over, set an expiry on the old key (you can also
              do this later from the old key's detail page).
            </p>
            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={() => close(false)}>
                Retire later manually
              </Button>
              <Button
                variant="outline"
                onClick={() => {
                  close(false);
                  navigate({ to: `/console/api-keys/${key_.id}` });
                }}
              >
                Set old key expiry
              </Button>
              <Button onClick={() => expireOld.mutate({ id: key_.id })} disabled={expireOld.pending}>
                {expireOld.pending ? "Processing…" : "② Expire old key now"}
              </Button>
            </div>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
