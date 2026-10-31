import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useCollection, useCommand, useRecord } from "@lynx-go/uikit-core";
import { predicateOf } from "@lynx-go/uikit-core/schema";
import type { Predicate } from "@lynx-go/uikit-core/kernel";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime, fromDateTimeLocalValue, toDateTimeLocalValue } from "@/lib/datetime";
import { toast } from "sonner";
import { Plus } from "lucide-react";
import {
  type Subscription,
  type SubscriptionPlan,
} from "@/api/subscriptions";
import { useAdminRole, canWrite, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useAuth } from "@/hooks/useAuth";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  DeleteButton,
  DetailGrid,
  DetailPageWrapper,
  DetailSkeleton,
  FormField,
  FormPageWrapper,
  NotFound,
  RowDeleteButton,
} from "@/components/resource/shared";
import { formatInt64 } from "@/lib/utils";
import { PageHeader } from "@/components/PageHeader";
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
import { describeApiError } from "@/kit/errors";
import {
  cancelSubscriptionCommand,
  createPlanCommand,
  expireSubscriptionCommand,
  filtersOf,
  plansResource,
  plansSearch,
  removePlanCommand,
  subscriptionsResource,
  subscriptionsSearch,
  type FilterField,
} from "@/kit/subscriptions";

export function PlansListPage() {
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const navigate = useNavigate();
  const writeable = canWrite(role);
  const [search, setSearch] = useState("");

  // 列表 fleet 化（§12.9-2）：游标/页大小/时间排序进 URL（分页栈 = 浏览器
  // 历史）；页内当前页搜索保持本地态（旧页语义）。scope/enabled 不再逐处
  // 传（§12.10-③）：资源声明了 projectId，Provider 单源 + 未就绪自动停用。
  const list = useCollection(plansResource, {
    from: "/console/subscriptions/plans",
    search: plansSearch,
  });

  // 行删除命令化：失效 = kit plans 键缺省同资源（列表 collection 与详情
  // record 全 kind）。RowDeleteButton 无表单容器——结果分支 toast。
  const remove = useCommand(removePlanCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Plan deleted");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const getSearchText = useCallback(
    (p: SubscriptionPlan) => `${p.id} ${p.code} ${p.name}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((p) => getSearchText(p).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const columns: LegacyColumnDef<SubscriptionPlan, unknown>[] = [
    {
      id: "code",
      header: "Code",
      cell: ({ row }) => <span className="font-mono text-xs">{row.original.code}</span>,
    },
    { id: "name", header: "Name", cell: ({ row }) => row.original.name },
    {
      id: "amount",
      header: "Amount",
      cell: ({ row }) => `${formatInt64(row.original.amount)} ${row.original.currency}`,
    },
    { id: "interval", header: "Interval", cell: ({ row }) => row.original.interval },
    {
      id: "status",
      header: "Status",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "archived" ? "secondary" : "default"}>
          {row.original.status ?? "active"}
        </Badge>
      ),
    },
    {
      id: "created",
      header: "Created At",
      meta: { sortField: "created_at" },
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    ...(writeable
      ? [
          {
            id: "actions",
            header: "Actions",
            cell: ({ row }: { row: { original: SubscriptionPlan } }) => (
              <RowDeleteButton
                onConfirm={() => remove.mutate({ id: row.original.id })}
                loading={remove.pending}
              />
            ),
          } satisfies LegacyColumnDef<SubscriptionPlan, unknown>,
        ]
      : []),
  ];

  return (
    <div className="space-y-6">
      <PageHeader title="Subscription Plans" description="Plans shared across platform and provider modes" />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search this page by code / name..."
              className="h-8 w-56"
            />
            <div className="ml-auto flex gap-2">
              <Button size="sm" variant="outline" asChild>
                <Link to="/console/subscriptions">Subscriptions</Link>
              </Button>
              {writeable ? (
                <Button size="sm" asChild>
                  <Link to="/console/subscriptions/plans/new">
                    <Plus className="h-4 w-4 mr-2" />
                    New Plan
                  </Link>
                </Button>
              ) : null}
            </div>
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
          </div>
        }
        emptyTitle={search !== "" ? "No matching plans" : "No plans yet"}
        emptyDescription={
          search !== "" ? "Adjust the search term, or clear it to restore all rows on this page" : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={columns}
            getRowId={(row) => row.id}
            onRowClick={(p) => navigate({ to: `/console/subscriptions/plans/${p.id}` })}
          />
        </div>
      </KitListPage>
    </div>
  );
}

export function PlanNewPage() {
  const navigate = useNavigate();
  const [code, setCode] = useState("");
  const [name, setName] = useState("");
  const [amount, setAmount] = useState("");
  const [currency, setCurrency] = useState("USD");
  const [interval, setInterval] = useState("month");
  const [graceDays, setGraceDays] = useState("3");

  // 命令化：amount int64 校验 + grace_days 文本→整数收敛在 descriptor run 内
  //（非法 → rejected 结果分支，toast）；ok 后失效 = kit plans 键缺省同资源
  //（列表 collection 与详情 record 全 kind）。
  const create = useCommand(createPlanCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Plan created");
        navigate({ to: `/console/subscriptions/plans/${result.payload.id}` });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <FormPageWrapper
      title="New Subscription Plan"
      backTo="/console/subscriptions/plans"
      submitLabel="Create"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate({ code, name, amount, currency, interval, graceDays });
      }}
      loading={create.pending}
      submitDisabled={!code || !name || !amount}
    >
      <FormField id="code" label="Code" value={code} onChange={setCode} required placeholder="pro" />
      <FormField id="name" label="Name" value={name} onChange={setName} required />
      <FormField id="amount" label="Amount (minor units)" value={amount} onChange={setAmount} required placeholder="999" />
      <FormField id="currency" label="Currency" value={currency} onChange={setCurrency} required />
      <div className="space-y-2">
        <Label>Interval</Label>
        <Select value={interval} onValueChange={setInterval}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="month">month</SelectItem>
            <SelectItem value="year">year</SelectItem>
            <SelectItem value="custom_days">custom_days</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <FormField id="grace" label="Grace Period (days)" value={graceDays} onChange={setGraceDays} />
    </FormPageWrapper>
  );
}

export function PlanDetailPage() {
  const { id } = useParams({ strict: false });
  const navigate = useNavigate();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const writeable = canWrite(role);

  // 详情读换轨 useRecord（子视图 kit 化批）：id 未就绪即停用（与旧 enabled
  // 同义）；404 → null 空态直出；remove 缺省同资源失效命中详情 record 键。
  const planRecord = useRecord(plansResource, {
    id: id ?? undefined,
    scope: { projectId: projectId ?? undefined },
  });
  const plan = planRecord.data;
  const isLoading = planRecord.status === "loading";

  const remove = useCommand(removePlanCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Plan deleted");
        navigate({ to: "/console/subscriptions/plans" });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!plan) return <NotFound backTo="/console/subscriptions/plans" />;

  return (
    <DetailPageWrapper
      title={plan.name}
      description={plan.code}
      backTo="/console/subscriptions/plans"
      actions={writeable ? <DeleteButton onConfirm={() => remove.mutate({ id: id! })} loading={remove.pending} /> : undefined}
    >
      <DetailGrid
        items={[
          { label: "ID", value: plan.id, mono: true },
          { label: "Amount", value: `${formatInt64(plan.amount)} ${plan.currency}`, mono: true },
          { label: "Interval", value: plan.interval },
          { label: "Grace Period (days)", value: String(plan.grace_days ?? 0) },
          { label: "Status", value: plan.status ?? "active" },
        ]}
      />
    </DetailPageWrapper>
  );
}

// 订阅状态选项与 subscriptions.proto 状态机一致（含终态）。
const SUB_STATUS_OPTIONS = ["trialing", "active", "past_due", "canceled", "expired"] as const;

export function SubscriptionsListPage() {
  const navigate = useNavigate();
  const tz = useUserTimezone();
  const [search, setSearch] = useState("");

  // 列表 fleet 化（§12.9-2）：服务端过滤（UserID/状态/创建时间区间）、时间
  // 排序、游标/页大小进 URL（过滤/换向 = replace + 清游标回第一页）；页内
  // 当前页搜索保持本地态（旧页语义）。scope/enabled 不再逐处传（§12.10-③）。
  const list = useCollection(subscriptionsResource, {
    from: "/console/subscriptions",
    search: subscriptionsSearch,
  });

  const filters = filtersOf(list.query.predicate);
  // UserID 是自由文本：输入区本地态，提交（查询/回车）才写 URL 谓词。
  const [userIdInput, setUserIdInput] = useState(filters.user_id ?? "");

  // setFilter 写谓词（replace + 清游标回第一页）；datetime 字段在写入边界
  // 做墙钟↔绝对换算——URL 存绝对时刻，跨时区分享同一过滤窗口。叶子归一化
  // 用 kit predicateOf（0→null / 1→裸叶 / n→AND，§12.10-④）。
  const setFilter = (field: FilterField, value: string) => {
    const next = { ...filters, [field]: value || undefined };
    const leaves: Predicate[] = [];
    for (const [f, v] of Object.entries(next)) {
      if (f === "created_after" || f === "created_before") continue;
      if (typeof v === "string" && v !== "") leaves.push({ field: f, op: "eq", value: v });
    }
    const after = fromDateTimeLocalValue(next.created_after ?? "", tz);
    if (after) leaves.push({ field: "created_after", op: "eq", value: after });
    const before = fromDateTimeLocalValue(next.created_before ?? "", tz);
    if (before) leaves.push({ field: "created_before", op: "eq", value: before });
    list.setPredicate(predicateOf(leaves));
  };

  const applyUserId = () => setFilter("user_id", userIdInput.trim());
  const clearFilters = () => {
    setUserIdInput("");
    list.setPredicate(null);
  };
  const filtersDirty = list.query.predicate != null || userIdInput.trim() !== "";

  const getSearchText = useCallback(
    (s: Subscription) => `${s.id} ${s.user_id ?? ""} ${s.plan_code ?? ""} ${s.status}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((s) => getSearchText(s).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const columns: LegacyColumnDef<Subscription, unknown>[] = [
    {
      id: "id",
      header: "ID",
      cell: ({ row }) => (
        <span className="inline-block max-w-[140px] truncate font-mono text-xs">
          {row.original.id}
        </span>
      ),
    },
    {
      id: "user",
      header: "User",
      cell: ({ row }) => (
        <span className="font-mono text-xs">{row.original.user_id ?? "—"}</span>
      ),
    },
    {
      id: "plan",
      header: "Plan",
      cell: ({ row }) => row.original.plan_code ?? row.original.plan_id ?? "—",
    },
    { id: "mode", header: "Mode", cell: ({ row }) => row.original.mode },
    {
      id: "status",
      header: "Status",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "active" ? "default" : "secondary"}>
          {row.original.status}
        </Badge>
      ),
    },
    {
      id: "created",
      header: "Created At",
      meta: { sortField: "created_at" },
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
  ];

  const filtered = list.query.predicate != null;

  return (
    <div className="space-y-6">
      <PageHeader title="Subscriptions" description="User subscription contracts (not assets)" />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="space-y-2 px-4 pb-2 pt-2">
            <form
              className="flex flex-wrap items-end gap-3"
              onSubmit={(e) => {
                e.preventDefault();
                applyUserId();
              }}
            >
              <div className="space-y-1">
                <Label htmlFor="subs-filter-user" className="text-xs text-muted-foreground">
                  User ID
                </Label>
                <Input
                  id="subs-filter-user"
                  value={userIdInput}
                  onChange={(e) => setUserIdInput(e.target.value)}
                  placeholder="Filter by exact UserID"
                  className="h-8 w-[280px] font-mono text-xs"
                />
              </div>
              <div className="space-y-1">
                <Label className="text-xs text-muted-foreground">Status</Label>
                <Select
                  value={filters.status || "all"}
                  onValueChange={(v) => setFilter("status", v === "all" ? "" : v)}
                >
                  <SelectTrigger className="h-8 w-[130px]" aria-label="Filter by status">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">All</SelectItem>
                    {SUB_STATUS_OPTIONS.map((s) => (
                      <SelectItem key={s} value={s}>
                        {s}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1">
                <Label htmlFor="subs-filter-after" className="text-xs text-muted-foreground">
                  Created From
                </Label>
                <DateTimePicker
                  id="subs-filter-after"
                  value={toDateTimeLocalValue(filters.created_after, tz)}
                  onChange={(v) => setFilter("created_after", v)}
                  className="h-8 w-[210px]"
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="subs-filter-before" className="text-xs text-muted-foreground">
                  To
                </Label>
                <DateTimePicker
                  id="subs-filter-before"
                  value={toDateTimeLocalValue(filters.created_before, tz)}
                  onChange={(v) => setFilter("created_before", v)}
                  className="h-8 w-[210px]"
                />
              </div>
              <Button type="submit" variant="outline" size="sm">
                Apply
              </Button>
              {filtersDirty && (
                <Button type="button" variant="ghost" size="sm" onClick={clearFilters}>
                  Clear
                </Button>
              )}
            </form>
            <div className="flex flex-wrap items-center gap-2">
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="Search this page by user / plan / status..."
                className="h-8 w-56"
              />
              <Button variant="outline" asChild className="ml-auto">
                <Link to="/console/subscriptions/plans">Plans</Link>
              </Button>
            </div>
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
          </div>
        }
        emptyTitle={search !== "" ? "No matching subscriptions" : filtered ? "No subscriptions match these filters" : "No subscriptions yet"}
        emptyDescription={
          search !== ""
            ? "Adjust the search term, or clear it to restore all rows on this page"
            : filtered
              ? "Adjust the filters, or clear them to restore all"
              : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={columns}
            getRowId={(row) => row.id}
            onRowClick={(s) => navigate({ to: `/console/subscriptions/${s.id}` })}
          />
        </div>
      </KitListPage>
    </div>
  );
}

export function SubscriptionDetailPage() {
  const { id } = useParams({ strict: false });
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const platformAdmin = isPlatformAdmin(role);

  // 详情读换轨 useRecord（子视图 kit 化批）：id 未就绪即停用（与旧 enabled
  // 同义）；404 → null 空态直出；cancel/expire 缺省同资源失效命中详情
  // record 键（全 kind）。
  const subRecord = useRecord(subscriptionsResource, {
    id: id ?? undefined,
    scope: { projectId: projectId ?? undefined },
  });
  const sub = subRecord.data;
  const isLoading = subRecord.status === "loading";

  // 写路径命令化（useCommand×2）：失效 = kit subscriptions 键缺省同资源
  //（列表 collection 与详情 record 全 kind）；结果分支 toast（无表单容器）。
  const cancel = useCommand(cancelSubscriptionCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Scheduled to cancel at period end");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });
  const expire = useCommand(expireSubscriptionCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Force expired");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!sub) return <NotFound backTo="/console/subscriptions" />;

  return (
    <DetailPageWrapper
      title={`Subscription ${sub.id}`}
      backTo="/console/subscriptions"
      actions={
        platformAdmin ? (
          <div className="flex gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={cancel.pending}
              onClick={() => cancel.mutate({ id: id!, reason: "console" })}
            >
              Cancel at Period End
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={expire.pending}
              onClick={() => expire.mutate({ id: id!, reason: "console" })}
            >
              Force Expire
            </Button>
          </div>
        ) : undefined
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: sub.id, mono: true },
          { label: "User", value: sub.user_id ?? "—", mono: true },
          { label: "Plan", value: sub.plan_code ?? sub.plan_id ?? "—" },
          { label: "Mode", value: sub.mode },
          { label: "Status", value: sub.status },
          { label: "Cancel at Period End", value: sub.cancel_at_period_end ? "Yes" : "No" },
          { label: "Current Period Start", value: formatDateTime(sub.current_period_start, tz) },
          { label: "Current Period End", value: formatDateTime(sub.current_period_end, tz) },
          { label: "Grace Until", value: formatDateTime(sub.grace_until, tz) },
        ]}
      />
    </DetailPageWrapper>
  );
}
