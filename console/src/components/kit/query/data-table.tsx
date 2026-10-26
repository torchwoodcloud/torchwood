// vendored from @lynx-go/ui registry/query/data-table.tsx @0.2.1（2026-10-25 §12.10-⑤ 上游回流 re-vendor）
// 拷贝适配（scripts/devtool.mjs --profile torchwood）：
// token 方言映射 text-muted→text-muted-foreground、bg-panel-2→bg-muted、border-line→border-border、text-fg→text-foreground；cn 统一 @/lib/utils。
// 其余与上游一致。
// LegacyColumnDef<T, any> 是上游形（列定义值类型开放）——vendored 保留。
/* eslint-disable @typescript-eslint/no-explicit-any */
import type { CollectionHandle } from "@lynx-go/uikit-core/react";
import { flexRender, type RowData } from "@tanstack/react-table";
import {
  getCoreRowModel,
  type LegacyColumnDef,
  useLegacyTable,
} from "@tanstack/react-table/legacy";
import { Checkbox } from "@/components/ui/checkbox";
import { cn } from "@/lib/utils";

/**
 * DataTable（query 块）—— 消费 useCollection 句柄的表格。
 *
 * 状态归属（design.md §6.1）：排序/过滤/窗口归 URL（CollectionQuery），表内禁用内建
 * sorting/filtering 分支；行选择为受控面（选择态归页面——批量动作是业务编排，
 * 块只渲染复选列并上报；§12.10-⑤ 自 torchwood 回流）。列经 meta.sortField 声明
 * 可排序，点击头循环 none→asc→desc（走 list.setOrder，URL 为真相）。
 *
 * 注：当前经 @tanstack/react-table/legacy（v8 兼容层，双 console 现行用法）；
 * 迁移 v9 原生 features 是块内改动，块 API 不变。RowData 取 react-table 主入口
 * （消费方不引 table-core）。
 */
export interface SortableColumnMeta {
  sortField?: string;
}

/** 受控行选择（选择态归页面；块只渲染复选列并上报事件）。 */
export interface RowSelection<T> {
  selectedIds: ReadonlySet<string>;
  onToggle: (id: string) => void;
  /** 表头全选/清空（visibleIds = 当前页可选行 id） */
  onToggleAll: (visibleIds: string[]) => void;
  isRowSelectable?: (row: T) => boolean;
}

export interface DataTableProps<T extends RowData> {
  list: CollectionHandle<T>;
  columns: LegacyColumnDef<T, any>[];
  getRowId: (row: T) => string;
  onRowClick?: (row: T) => void;
  selection?: RowSelection<T>;
}

export function DataTable<T extends RowData>({
  list,
  columns,
  getRowId,
  onRowClick,
  selection,
}: DataTableProps<T>) {
  const table = useLegacyTable({
    data: list.rows as T[],
    columns,
    getRowId,
    getCoreRowModel: getCoreRowModel(),
  });

  const currentSort = list.query.order?.[0];
  const toggleSort = (field: string) => {
    const current = currentSort?.field === field ? currentSort : undefined;
    const next =
      current === undefined
        ? [{ field, dir: "asc" as const }]
        : current.dir === "asc"
          ? [{ field, dir: "desc" as const }]
          : [];
    list.setOrder(next.length > 0 ? next : null);
  };

  const rows = table.getRowModel().rows;
  const selectableRows = selection
    ? rows.filter((r) => selection.isRowSelectable?.(r.original as T) ?? true)
    : [];
  const visibleIds = selectableRows.map((r) => r.id);
  const allSelected =
    selection !== undefined &&
    visibleIds.length > 0 &&
    visibleIds.every((id) => selection.selectedIds.has(id));

  return (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-sm">
        <thead>
          {table.getHeaderGroups().map((headerGroup) => (
            <tr
              key={headerGroup.id}
              className="border-b border-border text-left text-xs text-muted-foreground"
            >
              {selection ? (
                <th className="w-10 px-3 py-2">
                  <Checkbox
                    aria-label="Select all"
                    checked={allSelected}
                    indeterminate={
                      !allSelected && visibleIds.some((id) => selection.selectedIds.has(id))
                    }
                    onChange={() => selection.onToggleAll(visibleIds)}
                    disabled={visibleIds.length === 0}
                  />
                </th>
              ) : null}
              {headerGroup.headers.map((header) => {
                const sortField = (header.column.columnDef.meta as SortableColumnMeta | undefined)
                  ?.sortField;
                const active = currentSort?.field === sortField && sortField !== undefined;
                return (
                  <th key={header.id} className="px-3 py-2 font-medium">
                    {header.isPlaceholder ? null : sortField ? (
                      <button
                        type="button"
                        className={cn(
                          "inline-flex items-center gap-1 hover:text-foreground",
                          active && "text-foreground",
                        )}
                        onClick={() => toggleSort(sortField)}
                      >
                        {flexRender(header.column.columnDef.header, header.getContext())}
                        {active ? (
                          <span aria-hidden>{currentSort?.dir === "asc" ? "↑" : "↓"}</span>
                        ) : null}
                      </button>
                    ) : (
                      flexRender(header.column.columnDef.header, header.getContext())
                    )}
                  </th>
                );
              })}
            </tr>
          ))}
        </thead>
        <tbody>
          {rows.map((row) => {
            const rowSelectable = selection
              ? (selection.isRowSelectable?.(row.original as T) ?? true)
              : false;
            const rowSelected = selection?.selectedIds.has(row.id) ?? false;
            return (
              <tr
                key={row.id}
                className={cn(
                  "border-b border-border/60 hover:bg-muted/50",
                  onRowClick && "cursor-pointer",
                  rowSelected && "bg-muted/40",
                )}
                onClick={
                  onRowClick
                    ? (e) => {
                        // 行内交互元素（操作按钮/链接/复选/label）不触发行导航
                        if ((e.target as HTMLElement).closest("button, a, input, label")) return;
                        onRowClick(row.original as T);
                      }
                    : undefined
                }
              >
                {selection ? (
                  <td className="px-3 py-2" onClick={(e) => e.stopPropagation()}>
                    <Checkbox
                      aria-label={`Select ${row.id}`}
                      checked={rowSelected}
                      disabled={!rowSelectable}
                      onChange={() => selection.onToggle(row.id)}
                    />
                  </td>
                ) : null}
                {row.getVisibleCells().map((cell) => (
                  <td key={cell.id} className="px-3 py-2">
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                  </td>
                ))}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
