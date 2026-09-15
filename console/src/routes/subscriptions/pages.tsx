import { useCallback, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
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
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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

  const { data: plans = [], isLoading } = useQuery({
    queryKey: ["sub-plans", projectId],
    queryFn: listPlans,
    enabled: !!projectId,
  });
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
      searchPlaceholder="搜索 code / 名称..."
      isLoading={isLoading}
      items={plans}
      columns={planColumns}
      getSearchText={getSearchText}
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

export function SubscriptionsListPage() {
  const { projectId } = useAuth();
  const { data: items = [], isLoading } = useQuery({
    queryKey: ["subscriptions", projectId],
    queryFn: listSubscriptions,
    enabled: !!projectId,
  });
  const getSearchText = useCallback(
    (s: Subscription) => `${s.id} ${s.user_id ?? ""} ${s.plan_code ?? ""} ${s.status}`,
    []
  );

  return (
    <ResourceListPage
      title="订阅"
      description="用户订阅合同（不是资产）"
      searchPlaceholder="搜索用户 / 计划 / 状态..."
      isLoading={isLoading}
      items={items}
      columns={subColumns}
      getSearchText={getSearchText}
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
