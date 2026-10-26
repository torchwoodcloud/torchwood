import { useCallback, useMemo, useState } from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { useCollection, useCommand } from "@lynx-go/uikit-core";
import { predicateOf } from "@lynx-go/uikit-core/schema";
import type { Predicate } from "@lynx-go/uikit-core/kernel";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime, fromDateTimeLocalValue, toDateTimeLocalValue } from "@/lib/datetime";
import { toast } from "sonner";
import { getOrder, type PaymentOrder } from "@/api/payments";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
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
import { DetailGrid, DetailPageWrapper, DetailSkeleton, NotFound } from "@/components/resource/shared";
import { formatInt64 } from "@/lib/utils";
import { PageHeader } from "@/components/PageHeader";
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS, PAGINATION_ZH } from "@/components/kit/query/pagination-zh";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
import { filtersOf, ordersResource, ordersSearch, refundOrderCommand, fulfillOrderCommand, type FilterField } from "@/kit/payments";
import { describeApiError } from "@/kit/errors";

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
  const navigate = useNavigate();
  const tz = useUserTimezone();
  const [search, setSearch] = useState("");

  // 列表 fleet 化（§12.9-2）：服务端过滤（UserID/状态/创建时间区间）、时间
  // 排序、游标/页大小进 URL（过滤/换向 = replace + 清游标回第一页）；页内
  // 当前页搜索保持本地态（旧页语义）。scope/enabled 不再逐处传（§12.10-③）：
  // 资源声明了 projectId，Provider 单源 + 未就绪自动停用。
  const list = useCollection(ordersResource, {
    from: "/console/orders",
    search: ordersSearch,
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
    (o: PaymentOrder) => `${o.id} ${o.user_id ?? ""} ${o.status} ${o.purpose_kind}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((o) => getSearchText(o).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const columns: LegacyColumnDef<PaymentOrder, unknown>[] = [
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
      id: "amount",
      header: "金额",
      cell: ({ row }) => `${formatInt64(row.original.amount)} ${row.original.currency}`,
    },
    { id: "purpose", header: "用途", cell: ({ row }) => row.original.purpose_kind },
    {
      id: "status",
      header: "状态",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "paid" ? "default" : "secondary"}>
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
      <PageHeader title="订单" description="项目支付订单（金额为最小货币单位）" />

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
                  value={filters.status || "all"}
                  onValueChange={(v) => setFilter("status", v === "all" ? "" : v)}
                >
                  <SelectTrigger className="h-8 w-[130px]" aria-label="状态过滤">
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
                  value={toDateTimeLocalValue(filters.created_after, tz)}
                  onChange={(e) => setFilter("created_after", e.target.value)}
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
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="当前页内搜索订单 ID / 用户 / 状态..."
              className="h-8 w-56"
            />
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} labels={PAGINATION_ZH} />
          </div>
        }
        emptyTitle={search !== "" ? "没有匹配的订单" : filtered ? "该过滤条件下暂无订单" : "暂无订单"}
        emptyDescription={
          search !== ""
            ? "调整搜索词，或清空恢复当前页全部行"
            : filtered
              ? "调整过滤条件，或清除恢复全部"
              : "终端用户建单后将出现在此"
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={columns}
            getRowId={(row) => row.id}
            onRowClick={(o) => navigate({ to: `/console/orders/${o.id}` })}
          />
        </div>
      </KitListPage>
    </div>
  );
}

export function OrderDetailPage() {
  const { id } = useParams({ strict: false });
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const platformAdmin = isPlatformAdmin(role);

  const { data: order, isLoading } = useQuery({
    queryKey: ["payments-orders", id],
    queryFn: () => getOrder(id!),
    enabled: !!id,
  });

  // 命令接线（§12.3）：refund/fulfill 挂订单资源——失效 = kit 集合键 +
  // 存量 ["payments-orders"] 前缀（resource.legacyKeys 声明，§12.10-②——
  // 覆盖列表前缀与详情键两形）；toast 单源 onSettled。
  const refund = useCommand(refundOrderCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("已发起退款");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const fulfill = useCommand(fulfillOrderCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("已标记履约");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
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
              disabled={fulfill.pending || order.status !== "paid"}
              onClick={() => fulfill.mutate({ id: id! })}
            >
              人工履约
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={refund.pending || order.status !== "paid"}
              onClick={() => refund.mutate({ id: id! })}
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
