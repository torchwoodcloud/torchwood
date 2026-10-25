import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@/router/compat";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCollection } from "@lynx-go/ui-kit";
import type { Predicate } from "@lynx-go/ui-kit/kernel";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime, fromDateTimeLocalValue, toDateTimeLocalValue } from "@/lib/datetime";
import { toast } from "sonner";
import { Plus } from "lucide-react";
import {
  cancelSubscription,
  createPlan,
  deletePlan,
  expireSubscription,
  getPlan,
  getSubscription,
  type Subscription,
  type SubscriptionPlan,
} from "@/api/subscriptions";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, canWrite, isPlatformAdmin } from "@/hooks/useAdminRole";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
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
import { formatInt64, isInt64Input } from "@/lib/utils";
import { PageHeader } from "@/components/PageHeader";
import { DataTable } from "@/components/fleet/query/data-table";
import { KeysetPagination } from "@/components/fleet/query/keyset-pagination";
import { ListPage as FleetListPage } from "@/components/fleet/query/list-page";
import {
  filtersOf,
  plansResource,
  plansSearch,
  subscriptionsResource,
  subscriptionsSearch,
  type FilterField,
} from "@/fleet/subscriptions";

export function PlansListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const writeable = canWrite(role);
  const [search, setSearch] = useState("");

  // 列表 fleet 化（§12.9-2）：游标/页大小/时间排序进 URL（分页栈 = 浏览器
  // 历史）；页内当前页搜索保持本地态（旧页语义）。
  const list = useCollection(plansResource, {
    from: "/console/subscriptions/plans",
    search: plansSearch,
    scope: { projectId: projectId ?? undefined },
    query: { enabled: !!projectId },
  });

  // 行删除保持存量 useMutation（toast 语义不变）；失效 = 存量 ["sub-plans"]
  // 键（详情页仍消费）+ kit 句柄 refetch 桥接。
  const remove = useMutation({
    mutationFn: (id: string) => deletePlan(id),
    onSuccess: () => {
      toast.success("计划已删除");
      queryClient.invalidateQueries({ queryKey: ["sub-plans", projectId] });
      list.refetch();
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
    { id: "name", header: "名称", cell: ({ row }) => row.original.name },
    {
      id: "amount",
      header: "金额",
      cell: ({ row }) => `${formatInt64(row.original.amount)} ${row.original.currency}`,
    },
    { id: "interval", header: "周期", cell: ({ row }) => row.original.interval },
    {
      id: "status",
      header: "状态",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "archived" ? "secondary" : "default"}>
          {row.original.status ?? "active"}
        </Badge>
      ),
    },
    {
      id: "created",
      header: "创建时间",
      meta: { sortField: "created_at" },
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    ...(writeable
      ? [
          {
            id: "actions",
            header: "操作",
            cell: ({ row }: { row: { original: SubscriptionPlan } }) => (
              <RowDeleteButton
                onConfirm={() => remove.mutate(row.original.id)}
                loading={remove.isPending}
              />
            ),
          } satisfies LegacyColumnDef<SubscriptionPlan, unknown>,
        ]
      : []),
  ];

  return (
    <div className="space-y-6">
      <PageHeader title="订阅计划" description="平台托管 / 渠道托管共用计划" />

      <FleetListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="当前页内搜索 code / 名称..."
              className="h-8 w-56"
            />
            <div className="ml-auto flex gap-2">
              <Button variant="outline" asChild>
                <Link to="/console/subscriptions">订阅列表</Link>
              </Button>
              {writeable ? (
                <Button asChild>
                  <Link to="/console/subscriptions/plans/new">
                    <Plus className="h-4 w-4 mr-2" />
                    新建计划
                  </Link>
                </Button>
              ) : null}
            </div>
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} />
          </div>
        }
        emptyTitle={search !== "" ? "没有匹配的计划" : "暂无计划"}
        emptyDescription={
          search !== "" ? "调整搜索词，或清空恢复当前页全部行" : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={columns}
            getRowId={(row) => row.id}
            onRowClick={(p) => navigate(`/console/subscriptions/plans/${p.id}`)}
          />
        </div>
      </FleetListPage>
    </div>
  );
}

export function PlanNewPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const [code, setCode] = useState("");
  const [name, setName] = useState("");
  const [amount, setAmount] = useState("");
  const [currency, setCurrency] = useState("USD");
  const [interval, setInterval] = useState("month");
  const [graceDays, setGraceDays] = useState("3");

  const mutation = useMutation({
    mutationFn: () =>
      createPlan({
        code,
        name,
        amount,
        currency,
        interval,
        grace_days: Number.parseInt(graceDays, 10) || 0,
      }),
    onSuccess: (plan) => {
      toast.success("计划已创建");
      queryClient.invalidateQueries({ queryKey: ["sub-plans", projectId] });
      navigate(`/console/subscriptions/plans/${plan.id}`);
    },
  });

  return (
    <FormPageWrapper
      title="新建订阅计划"
      backTo="/console/subscriptions/plans"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        if (!isInt64Input(amount) || amount === "") {
          toast.error("amount 必须是最小货币单位整数");
          return;
        }
        mutation.mutate();
      }}
      loading={mutation.isPending}
      submitDisabled={!code || !name || !amount}
    >
      <FormField id="code" label="Code" value={code} onChange={setCode} required placeholder="pro" />
      <FormField id="name" label="名称" value={name} onChange={setName} required />
      <FormField id="amount" label="金额（最小单位整数）" value={amount} onChange={setAmount} required placeholder="999" />
      <FormField id="currency" label="币种" value={currency} onChange={setCurrency} required />
      <div className="space-y-2">
        <Label>周期</Label>
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
      <FormField id="grace" label="宽限天数" value={graceDays} onChange={setGraceDays} />
    </FormPageWrapper>
  );
}

export function PlanDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const writeable = canWrite(role);

  const { data: plan, isLoading } = useQuery({
    queryKey: ["sub-plans", id],
    queryFn: () => getPlan(id!),
    enabled: !!id,
  });
  const remove = useMutation({
    mutationFn: () => deletePlan(id!),
    onSuccess: () => {
      toast.success("计划已删除");
      queryClient.invalidateQueries({ queryKey: ["sub-plans", projectId] });
      navigate("/console/subscriptions/plans");
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!plan) return <NotFound backTo="/console/subscriptions/plans" />;

  return (
    <DetailPageWrapper
      title={plan.name}
      description={plan.code}
      backTo="/console/subscriptions/plans"
      actions={writeable ? <DeleteButton onConfirm={() => remove.mutate()} loading={remove.isPending} /> : undefined}
    >
      <DetailGrid
        items={[
          { label: "ID", value: plan.id, mono: true },
          { label: "金额", value: `${formatInt64(plan.amount)} ${plan.currency}`, mono: true },
          { label: "周期", value: plan.interval },
          { label: "宽限天数", value: String(plan.grace_days ?? 0) },
          { label: "状态", value: plan.status ?? "active" },
        ]}
      />
    </DetailPageWrapper>
  );
}

// 订阅状态选项与 subscriptions.proto 状态机一致（含终态）。
const SUB_STATUS_OPTIONS = ["trialing", "active", "past_due", "canceled", "expired"] as const;

export function SubscriptionsListPage() {
  const { projectId } = useAuth();
  const navigate = useNavigate();
  const tz = useUserTimezone();
  const [search, setSearch] = useState("");

  // 列表 fleet 化（§12.9-2）：服务端过滤（UserID/状态/创建时间区间）、时间
  // 排序、游标/页大小进 URL（过滤/换向 = replace + 清游标回第一页）；页内
  // 当前页搜索保持本地态（旧页语义）。
  const list = useCollection(subscriptionsResource, {
    from: "/console/subscriptions",
    search: subscriptionsSearch,
    scope: { projectId: projectId ?? undefined },
    query: { enabled: !!projectId },
  });

  const filters = filtersOf(list.query.predicate);
  // UserID 是自由文本：输入区本地态，提交（查询/回车）才写 URL 谓词。
  const [userIdInput, setUserIdInput] = useState(filters.user_id ?? "");

  // setFilter 写谓词（replace + 清游标回第一页）；datetime 字段在写入边界
  // 做墙钟↔绝对换算——URL 存绝对时刻，跨时区分享同一过滤窗口。
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
    list.setPredicate(leaves.length === 0 ? null : leaves.length === 1 ? leaves[0] : { all: leaves });
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
      header: "用户",
      cell: ({ row }) => (
        <span className="font-mono text-xs">{row.original.user_id ?? "—"}</span>
      ),
    },
    {
      id: "plan",
      header: "计划",
      cell: ({ row }) => row.original.plan_code ?? row.original.plan_id ?? "—",
    },
    { id: "mode", header: "模式", cell: ({ row }) => row.original.mode },
    {
      id: "status",
      header: "状态",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "active" ? "default" : "secondary"}>
          {row.original.status}
        </Badge>
      ),
    },
    {
      id: "created",
      header: "创建时间",
      meta: { sortField: "created_at" },
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
  ];

  const filtered = list.query.predicate != null;

  return (
    <div className="space-y-6">
      <PageHeader title="订阅" description="用户订阅合同（不是资产）" />

      <FleetListPage
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
                  用户 ID
                </Label>
                <Input
                  id="subs-filter-user"
                  value={userIdInput}
                  onChange={(e) => setUserIdInput(e.target.value)}
                  placeholder="按 UserID 精确过滤"
                  className="h-8 w-[280px] font-mono text-xs"
                />
              </div>
              <div className="space-y-1">
                <Label className="text-xs text-muted-foreground">状态</Label>
                <Select
                  value={filters.status || "all"}
                  onValueChange={(v) => setFilter("status", v === "all" ? "" : v)}
                >
                  <SelectTrigger className="h-8 w-[130px]" aria-label="状态过滤">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">全部</SelectItem>
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
                  创建时间从
                </Label>
                <Input
                  id="subs-filter-after"
                  type="datetime-local"
                  value={toDateTimeLocalValue(filters.created_after, tz)}
                  onChange={(e) => setFilter("created_after", e.target.value)}
                  className="h-8 w-[210px]"
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="subs-filter-before" className="text-xs text-muted-foreground">
                  到
                </Label>
                <Input
                  id="subs-filter-before"
                  type="datetime-local"
                  value={toDateTimeLocalValue(filters.created_before, tz)}
                  onChange={(e) => setFilter("created_before", e.target.value)}
                  className="h-8 w-[210px]"
                />
              </div>
              <Button type="submit" variant="outline" size="sm">
                查询
              </Button>
              {filtersDirty && (
                <Button type="button" variant="ghost" size="sm" onClick={clearFilters}>
                  清除
                </Button>
              )}
            </form>
            <div className="flex flex-wrap items-center gap-2">
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="当前页内搜索用户 / 计划 / 状态..."
                className="h-8 w-56"
              />
              <Button variant="outline" asChild className="ml-auto">
                <Link to="/console/subscriptions/plans">计划</Link>
              </Button>
            </div>
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} />
          </div>
        }
        emptyTitle={search !== "" ? "没有匹配的订阅" : filtered ? "该过滤条件下暂无订阅" : "暂无订阅"}
        emptyDescription={
          search !== ""
            ? "调整搜索词，或清空恢复当前页全部行"
            : filtered
              ? "调整过滤条件，或清除恢复全部"
              : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={columns}
            getRowId={(row) => row.id}
            onRowClick={(s) => navigate(`/console/subscriptions/${s.id}`)}
          />
        </div>
      </FleetListPage>
    </div>
  );
}

export function SubscriptionDetailPage() {
  const { id } = useParams<{ id: string }>();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const platformAdmin = isPlatformAdmin(role);

  const { data: sub, isLoading } = useQuery({
    queryKey: ["subscriptions", id],
    queryFn: () => getSubscription(id!),
    enabled: !!id,
  });

  const cancel = useMutation({
    mutationFn: () => cancelSubscription(id!, "console"),
    onSuccess: () => {
      toast.success("已标记期末取消");
      queryClient.invalidateQueries({ queryKey: ["subscriptions", projectId] });
      queryClient.invalidateQueries({ queryKey: ["subscriptions", id] });
    },
  });
  const expire = useMutation({
    mutationFn: () => expireSubscription(id!, "console"),
    onSuccess: () => {
      toast.success("已强制过期");
      queryClient.invalidateQueries({ queryKey: ["subscriptions", id] });
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!sub) return <NotFound backTo="/console/subscriptions" />;

  return (
    <DetailPageWrapper
      title={`订阅 ${sub.id}`}
      backTo="/console/subscriptions"
      actions={
        platformAdmin ? (
          <div className="flex gap-2">
            <Button variant="outline" size="sm" disabled={cancel.isPending} onClick={() => cancel.mutate()}>
              期末取消
            </Button>
            <Button variant="destructive" size="sm" disabled={expire.isPending} onClick={() => expire.mutate()}>
              强制过期
            </Button>
          </div>
        ) : undefined
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: sub.id, mono: true },
          { label: "用户", value: sub.user_id ?? "—", mono: true },
          { label: "计划", value: sub.plan_code ?? sub.plan_id ?? "—" },
          { label: "模式", value: sub.mode },
          { label: "状态", value: sub.status },
          { label: "期末取消", value: sub.cancel_at_period_end ? "是" : "否" },
          { label: "当前周期开始", value: formatDateTime(sub.current_period_start, tz) },
          { label: "当前周期结束", value: formatDateTime(sub.current_period_end, tz) },
          { label: "宽限至", value: formatDateTime(sub.grace_until, tz) },
        ]}
      />
    </DetailPageWrapper>
  );
}
