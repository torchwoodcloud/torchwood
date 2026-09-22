import { useCallback, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime, fromDateTimeLocalValue } from "@/lib/datetime";
import { toast } from "sonner";
import {
  getOrder,
  listOrders,
  manualFulfillOrder,
  refundOrder,
  type PaymentOrder,
} from "@/api/payments";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
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
import { DetailGrid, DetailPageWrapper, DetailSkeleton, NotFound } from "@/components/resource/shared";
import { formatInt64 } from "@/lib/utils";

const orderColumns = (tz: string): ColumnDef<PaymentOrder>[] => [
  { key: "id", header: "ID", className: "font-mono text-xs max-w-[140px] truncate", cell: (o) => o.id },
  { key: "user", header: "用户", className: "font-mono text-xs", cell: (o) => o.user_id ?? "—" },
  { key: "amount", header: "金额", cell: (o) => `${formatInt64(o.amount)} ${o.currency}` },
  { key: "purpose", header: "用途", cell: (o) => o.purpose_kind },
  {
    key: "status",
    header: "状态",
    cell: (o) => <Badge variant={o.status === "paid" ? "default" : "secondary"}>{o.status}</Badge>,
  },
  { key: "created", header: "创建时间", cell: (o) => formatDateTime(o.created_at, tz) },
];

// 订单状态选项与 payments.proto 状态机一致。
const ORDER_STATUS_OPTIONS = [
  "created",
  "paying",
  "paid",
  "failed",
  "closed",
  "refunding",
  "refunded",
] as const;

export function OrdersListPage() {
  const { projectId } = useAuth();
  const tz = useUserTimezone();
  const paging = useServerPaging();
  // 服务端过滤（ListOrdersRequest 结构化字段）：UserID 精确 + 状态 + 创建时间
  // 范围；任何过滤变化 reset 回第一页。
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
      "payments-orders",
      projectId,
      filters,
      paging.pageSize,
      paging.pageToken,
    ],
    queryFn: () =>
      listOrders({ pageSize: paging.pageSize, pageToken: paging.pageToken, ...filters }),
    enabled: !!projectId,
    placeholderData: (prev) => prev,
  });
  const orders = data?.rows ?? [];

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
    (o: PaymentOrder) => `${o.id} ${o.user_id ?? ""} ${o.status} ${o.purpose_kind}`,
    []
  );

  return (
    <ResourceListPage
      title="订单"
      description="项目支付订单（金额为最小货币单位）"
      searchPlaceholder="当前页内搜索订单 ID / 用户 / 状态..."
      isLoading={isLoading}
      items={orders}
      columns={orderColumns(tz)}
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
            <Label htmlFor="orders-filter-user" className="text-xs text-muted-foreground">
              用户 ID
            </Label>
            <Input
              id="orders-filter-user"
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
                {ORDER_STATUS_OPTIONS.map((s) => (
                  <SelectItem key={s} value={s}>
                    {s}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1">
            <Label htmlFor="orders-filter-after" className="text-xs text-muted-foreground">
              创建时间从
            </Label>
            <Input
              id="orders-filter-after"
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
            <Label htmlFor="orders-filter-before" className="text-xs text-muted-foreground">
              到
            </Label>
            <Input
              id="orders-filter-before"
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
      detailPath={(o) => `/console/orders/${o.id}`}
      emptyTitle="暂无订单"
      emptyDescription="终端用户建单后将出现在此"
    />
  );
}

export function OrderDetailPage() {
  const { id } = useParams<{ id: string }>();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const platformAdmin = isPlatformAdmin(role);

  const { data: order, isLoading } = useQuery({
    queryKey: ["payments-orders", id],
    queryFn: () => getOrder(id!),
    enabled: !!id,
  });

  const refund = useMutation({
    mutationFn: () => refundOrder(id!, { reason: "console" }),
    onSuccess: () => {
      toast.success("已发起退款");
      queryClient.invalidateQueries({ queryKey: ["payments-orders", projectId] });
      queryClient.invalidateQueries({ queryKey: ["payments-orders", id] });
    },
  });

  const fulfill = useMutation({
    mutationFn: () => manualFulfillOrder(id!, "console"),
    onSuccess: () => {
      toast.success("已标记履约");
      queryClient.invalidateQueries({ queryKey: ["payments-orders", id] });
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!order) return <NotFound backTo="/console/orders" />;

  return (
    <DetailPageWrapper
      title={`订单 ${order.id}`}
      description="订单详情"
      backTo="/console/orders"
      actions={
        platformAdmin ? (
          <div className="flex gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={fulfill.isPending || order.status !== "paid"}
              onClick={() => fulfill.mutate()}
            >
              人工履约
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={refund.isPending || order.status !== "paid"}
              onClick={() => refund.mutate()}
            >
              退款
            </Button>
          </div>
        ) : undefined
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: order.id, mono: true },
          { label: "用户", value: order.user_id ?? "—", mono: true },
          { label: "金额", value: `${formatInt64(order.amount)} ${order.currency}`, mono: true },
          { label: "渠道", value: order.provider },
          { label: "用途", value: order.purpose_kind },
          { label: "状态", value: order.status },
          { label: "幂等键", value: order.idempotency_key ?? "—", mono: true },
          { label: "渠道会话", value: order.provider_session_id ?? "—", mono: true },
          { label: "创建时间", value: formatDateTime(order.created_at, tz) },
          { label: "支付时间", value: formatDateTime(order.paid_at, tz) },
          { label: "过期时间", value: formatDateTime(order.expires_at, tz) },
        ]}
      />
    </DetailPageWrapper>
  );
}
