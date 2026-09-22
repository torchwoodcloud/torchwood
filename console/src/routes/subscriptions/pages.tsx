import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime, fromDateTimeLocalValue } from "@/lib/datetime";
import { toast } from "sonner";
import { Plus } from "lucide-react";
import {
  cancelSubscription,
  createPlan,
  deletePlan,
  expireSubscription,
  getPlan,
  getSubscription,
  listPlans,
  listSubscriptions,
  type Subscription,
  type SubscriptionPlan,
} from "@/api/subscriptions";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, canWrite, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useServerPaging } from "@/hooks/useServerPaging";
import { ResourceListPage } from "@/components/list/ResourceListPage";
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
import type { ColumnDef } from "@/components/list/DataTable";
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

const planColumns: ColumnDef<SubscriptionPlan>[] = [
  { key: "code", header: "Code", className: "font-mono text-xs", cell: (p) => p.code },
  { key: "name", header: "名称", cell: (p) => p.name },
  { key: "amount", header: "金额", cell: (p) => `${formatInt64(p.amount)} ${p.currency}` },
  { key: "interval", header: "周期", cell: (p) => p.interval },
  {
    key: "status",
    header: "状态",
    cell: (p) => <Badge variant={p.status === "archived" ? "secondary" : "default"}>{p.status ?? "active"}</Badge>,
  },
];

export function PlansListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const queryClient = useQueryClient();
  const writeable = canWrite(role);

  const paging = useServerPaging();
  const { data, isLoading } = useQuery({
    queryKey: ["sub-plans", projectId, paging.pageSize, paging.pageToken],
    queryFn: () => listPlans({ pageSize: paging.pageSize, pageToken: paging.pageToken }),
    enabled: !!projectId,
    placeholderData: (prev) => prev,
  });
  const plans = data?.rows ?? [];
  const remove = useMutation({
    mutationFn: (id: string) => deletePlan(id),
    onSuccess: () => {
      toast.success("计划已删除");
      queryClient.invalidateQueries({ queryKey: ["sub-plans", projectId] });
    },
  });
  const getSearchText = useCallback((p: SubscriptionPlan) => `${p.id} ${p.code} ${p.name}`, []);

  return (
    <ResourceListPage
      title="订阅计划"
      description="平台托管 / 渠道托管共用计划"
      searchPlaceholder="当前页内搜索 code / 名称..."
      isLoading={isLoading}
      items={plans}
      columns={planColumns}
      getSearchText={getSearchText}
      serverPaging={{
        page: paging.page,
        pageSize: paging.pageSize,
        hasPrev: paging.hasPrev,
        hasNext: !!data?.nextPageToken,
        onPrev: paging.goPrev,
        onNext: () => paging.goNext(data?.nextPageToken),
        onPageSizeChange: paging.setPageSize,
      }}
      detailPath={(p) => `/console/subscriptions/plans/${p.id}`}
      toolbarActions={
        <div className="flex gap-2">
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
          ) : undefined}
        </div>
      }
      rowActions={
        writeable
          ? (p) => <RowDeleteButton onConfirm={() => remove.mutate(p.id)} loading={remove.isPending} />
          : undefined
      }
      emptyTitle="暂无计划"
    />
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

const subColumns: ColumnDef<Subscription>[] = [
  { key: "id", header: "ID", className: "font-mono text-xs max-w-[140px] truncate", cell: (s) => s.id },
  { key: "user", header: "用户", className: "font-mono text-xs", cell: (s) => s.user_id ?? "—" },
  { key: "plan", header: "计划", cell: (s) => s.plan_code ?? s.plan_id ?? "—" },
  { key: "mode", header: "模式", cell: (s) => s.mode },
  {
    key: "status",
    header: "状态",
    cell: (s) => <Badge variant={s.status === "active" ? "default" : "secondary"}>{s.status}</Badge>,
  },
];

// 订阅状态选项与 subscriptions.proto 状态机一致（含终态）。
const SUB_STATUS_OPTIONS = ["trialing", "active", "past_due", "canceled", "expired"] as const;

export function SubscriptionsListPage() {
  const { projectId } = useAuth();
  const tz = useUserTimezone();
  const paging = useServerPaging();
  // 服务端过滤（ListSubscriptionsRequest 结构化字段）：UserID 精确 + 状态 +
  // 创建时间范围；任何过滤变化 reset 回第一页。
  const [userIdInput, setUserIdInput] = useState("");
  const [userIdFilter, setUserIdFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [createdAfter, setCreatedAfter] = useState("");
  const [createdBefore, setCreatedBefore] = useState("");

  const filters = useMemo(
    () => ({
      userId: userIdFilter.trim() || undefined,
      status: statusFilter || undefined,
      createdAfter: fromDateTimeLocalValue(createdAfter, tz) || undefined,
      createdBefore: fromDateTimeLocalValue(createdBefore, tz) || undefined,
    }),
    [userIdFilter, statusFilter, createdAfter, createdBefore, tz]
  );
  const filtersDirty = !!(
    userIdFilter ||
    statusFilter ||
    createdAfter ||
    createdBefore
  );

  const { data, isLoading } = useQuery({
    queryKey: [
      "subscriptions",
      projectId,
      filters,
      paging.pageSize,
      paging.pageToken,
    ],
    queryFn: () =>
      listSubscriptions({ pageSize: paging.pageSize, pageToken: paging.pageToken, ...filters }),
    enabled: !!projectId,
    placeholderData: (prev) => prev,
  });
  const items = data?.rows ?? [];

  const applyUserId = () => {
    setUserIdFilter(userIdInput);
    paging.reset();
  };
  const clearFilters = () => {
    setUserIdInput("");
    setUserIdFilter("");
    setStatusFilter("");
    setCreatedAfter("");
    setCreatedBefore("");
    paging.reset();
  };

  const getSearchText = useCallback(
    (s: Subscription) => `${s.id} ${s.user_id ?? ""} ${s.plan_code ?? ""} ${s.status}`,
    []
  );

  return (
    <ResourceListPage
      title="订阅"
      description="用户订阅合同（不是资产）"
      searchPlaceholder="当前页内搜索用户 / 计划 / 状态..."
      isLoading={isLoading}
      items={items}
      columns={subColumns}
      getSearchText={getSearchText}
      serverPaging={{
        page: paging.page,
        pageSize: paging.pageSize,
        hasPrev: paging.hasPrev,
        hasNext: !!data?.nextPageToken,
        onPrev: paging.goPrev,
        onNext: () => paging.goNext(data?.nextPageToken),
        onPageSizeChange: paging.setPageSize,
      }}
      filters={
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
              value={statusFilter || "all"}
              onValueChange={(v) => {
                setStatusFilter(v === "all" ? "" : v);
                paging.reset();
              }}
            >
              <SelectTrigger className="h-8 w-[130px]">
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
              value={createdAfter}
              onChange={(e) => {
                setCreatedAfter(e.target.value);
                paging.reset();
              }}
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
              value={createdBefore}
              onChange={(e) => {
                setCreatedBefore(e.target.value);
                paging.reset();
              }}
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
      }
      detailPath={(s) => `/console/subscriptions/${s.id}`}
      toolbarActions={
        <Button variant="outline" asChild>
          <Link to="/console/subscriptions/plans">计划</Link>
        </Button>
      }
      emptyTitle="暂无订阅"
    />
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
