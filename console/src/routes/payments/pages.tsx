import { useCallback, useMemo, useState } from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { useCollection, useCommand, useRecord } from "@lynx-go/uikit-core";
import { predicateOf } from "@lynx-go/uikit-core/schema";
import type { Predicate } from "@lynx-go/uikit-core/kernel";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime, fromDateTimeLocalValue, toDateTimeLocalValue } from "@/lib/datetime";
import { toast } from "sonner";
import { type PaymentOrder } from "@/api/payments";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
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
import { DetailGrid, DetailPageWrapper, DetailSkeleton, NotFound } from "@/components/resource/shared";
import { formatInt64 } from "@/lib/utils";
import { PageHeader } from "@/components/PageHeader";
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
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
      header: "User",
      cell: ({ row }) => (
        <span className="font-mono text-xs">{row.original.user_id ?? "—"}</span>
      ),
    },
    {
      id: "amount",
      header: "Amount",
      cell: ({ row }) => `${formatInt64(row.original.amount)} ${row.original.currency}`,
    },
    { id: "purpose", header: "Purpose", cell: ({ row }) => row.original.purpose_kind },
    {
      id: "status",
      header: "Status",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "paid" ? "default" : "secondary"}>
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
      <PageHeader title="Orders" description="Project payment orders (amounts in minor currency units)" />

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
                  User ID
                </Label>
                <Input
                  id="orders-filter-user"
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
                  Created From
                </Label>
                <DateTimePicker
                  id="orders-filter-after"
                  value={toDateTimeLocalValue(filters.created_after, tz)}
                  onChange={(v) => setFilter("created_after", v)}
                  className="h-8 w-[210px]"
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="orders-filter-before" className="text-xs text-muted-foreground">
                  To
                </Label>
                <DateTimePicker
                  id="orders-filter-before"
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
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search this page by order ID / user / status..."
              className="h-8 w-56"
            />
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
          </div>
        }
        emptyTitle={search !== "" ? "No matching orders" : filtered ? "No orders match these filters" : "No orders yet"}
        emptyDescription={
          search !== ""
            ? "Adjust the search term, or clear it to restore all rows on this page"
            : filtered
              ? "Adjust the filters, or clear them to restore all"
              : "Orders created by end users will appear here"
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
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const platformAdmin = isPlatformAdmin(role);

  // 详情读换轨 useRecord（子视图 kit 化批）：id 未就绪即停用（与旧 enabled
  // 同义）；404 → null 空态直出；refund/fulfill 缺省同资源失效命中详情
  // record 键（全 kind）。
  const orderRecord = useRecord(ordersResource, {
    id: id ?? undefined,
    scope: { projectId: projectId ?? undefined },
  });
  const order = orderRecord.data;
  const isLoading = orderRecord.status === "loading";

  // 命令接线（§12.3）：refund/fulfill 挂订单资源——失效 = kit 键缺省同资源
  //（列表 collection 与详情 record 全 kind）；toast 单源 onSettled。
  const refund = useCommand(refundOrderCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Refund initiated");
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
        toast.success("Marked as fulfilled");
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
      title={`Order ${order.id}`}
      description="Order details"
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
              Fulfill Manually
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={refund.pending || order.status !== "paid"}
              onClick={() => refund.mutate({ id: id! })}
            >
              Refund
            </Button>
          </div>
        ) : undefined
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: order.id, mono: true },
          { label: "User", value: order.user_id ?? "—", mono: true },
          { label: "Amount", value: `${formatInt64(order.amount)} ${order.currency}`, mono: true },
          { label: "Provider", value: order.provider },
          { label: "Purpose", value: order.purpose_kind },
          { label: "Status", value: order.status },
          { label: "Idempotency Key", value: order.idempotency_key ?? "—", mono: true },
          { label: "Provider Session", value: order.provider_session_id ?? "—", mono: true },
          { label: "Created At", value: formatDateTime(order.created_at, tz) },
          { label: "Paid At", value: formatDateTime(order.paid_at, tz) },
          { label: "Expires At", value: formatDateTime(order.expires_at, tz) },
        ]}
      />
    </DetailPageWrapper>
  );
}
