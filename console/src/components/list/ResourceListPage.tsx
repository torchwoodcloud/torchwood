import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { PageHeader } from "@/components/PageHeader";
import { EmptyState } from "@/components/EmptyState";
import { LoadingTable } from "@/components/LoadingTable";
import {
  ListToolbar,
  SelectionBar,
  ListPagination,
  ListPaginationKeyset,
} from "@/components/list/ListToolbar";
import { DataTable, type ColumnDef } from "@/components/list/DataTable";
import { useListParams, filterByQuery, paginate } from "@/hooks/useListParams";
import { useRowSelection } from "@/hooks/useRowSelection";
import { useMemo, useEffect } from "react";

// 服务端 keyset 分页的受控接入口：传入时 items 视为「当前页已取回的行」，
// 翻页/换页大小交给服务端（游标经 useServerPaging 管理），不再客户端切片。
export interface ServerPaging {
  page: number;
  pageSize: number;
  hasPrev: boolean;
  hasNext: boolean;
  onPrev: () => void;
  onNext: () => void;
  onPageSizeChange: (n: number) => void;
}

interface ResourceListPageProps<T extends { id: string }> {
  title?: string;
  description?: string;
  cardTitle?: string;
  searchPlaceholder?: string;
  isLoading: boolean;
  items: T[];
  columns: ColumnDef<T>[];
  getSearchText: (item: T) => string;
  toolbarActions?: React.ReactNode;
  selectionActions?: (selected: T[], clear: () => void) => React.ReactNode;
  filters?: React.ReactNode;
  isRowSelectable?: (item: T) => boolean;
  detailPath?: (item: T) => string;
  editPath?: (item: T) => string;
  rowActions?: (item: T) => React.ReactNode;
  emptyTitle?: string;
  emptyDescription?: string;
  emptyAction?: React.ReactNode;
  serverPaging?: ServerPaging;
}

export function ResourceListPage<T extends { id: string }>({
  title,
  description,
  cardTitle = "列表",
  searchPlaceholder,
  isLoading,
  items,
  columns,
  getSearchText,
  toolbarActions,
  selectionActions,
  filters,
  isRowSelectable,
  detailPath,
  editPath,
  rowActions,
  emptyTitle = "暂无数据",
  emptyDescription,
  emptyAction,
  serverPaging,
}: ResourceListPageProps<T>) {
  const { params, setParams } = useListParams();

  const filtered = useMemo(
    () => filterByQuery(items, params.q, getSearchText),
    [items, params.q, getSearchText]
  );

  // 服务端分页：items 就是当前页，客户端只做页内搜索，不再切片。
  const pageItems = serverPaging ? filtered : undefined;
  const clientPage = useMemo(
    () =>
      serverPaging
        ? null
        : paginate(filtered, params.page, params.pageSize),
    [serverPaging, filtered, params.page, params.pageSize]
  );

  const shownItems = serverPaging ? pageItems! : clientPage!.items;
  const shownTotal = serverPaging ? filtered.length : clientPage!.total;
  const shownPage = serverPaging ? serverPaging.page : clientPage!.page;
  const shownTotalPages = serverPaging ? 0 : clientPage!.totalPages;

  const selection = useRowSelection(shownItems);

  useEffect(() => {
    selection.clear();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [params.q, params.page, params.pageSize, items.length]);

  return (
    <div className="space-y-6">
      {title && <PageHeader title={title} description={description} />}

      <Card>
        <CardHeader className="space-y-4 border-b">
          <CardTitle>{cardTitle}</CardTitle>
          <ListToolbar
            searchValue={params.q}
            onSearchChange={(q) => setParams({ q })}
            searchPlaceholder={searchPlaceholder}
            actions={toolbarActions}
            filters={filters}
          />
          <SelectionBar
            count={selection.count}
            onClear={selection.clear}
            actions={selectionActions?.(selection.selectedItems, selection.clear)}
          />
        </CardHeader>
        <CardContent className="pt-6">
          {isLoading ? (
            <LoadingTable columns={columns.length + 2} />
          ) : shownItems.length === 0 ? (
            <>
              <EmptyState
                title={emptyTitle}
                description={emptyDescription}
              />
              {/* 空页仍渲染分页栏：末页后的空页请求（token 停发）下只能靠「上一页」退出。 */}
              {serverPaging && (
                <ListPaginationKeyset
                  page={serverPaging.page}
                  pageSize={serverPaging.pageSize}
                  rowCount={0}
                  hasPrev={serverPaging.hasPrev}
                  hasNext={serverPaging.hasNext}
                  onPrev={serverPaging.onPrev}
                  onNext={serverPaging.onNext}
                  onPageSizeChange={serverPaging.onPageSizeChange}
                />
              )}
            </>
          ) : (
            <>
              <DataTable
                items={shownItems}
                columns={columns}
                allSelected={selection.allSelected}
                someSelected={selection.someSelected}
                isSelected={selection.isSelected}
                onToggle={selection.toggle}
                onToggleAll={selection.toggleAll}
                isRowSelectable={isRowSelectable}
                detailPath={detailPath}
                editPath={editPath}
                rowActions={rowActions}
              />
              {serverPaging ? (
                <ListPaginationKeyset
                  page={serverPaging.page}
                  pageSize={serverPaging.pageSize}
                  rowCount={filtered.length}
                  hasPrev={serverPaging.hasPrev}
                  hasNext={serverPaging.hasNext}
                  onPrev={serverPaging.onPrev}
                  onNext={serverPaging.onNext}
                  onPageSizeChange={serverPaging.onPageSizeChange}
                />
              ) : (
                <ListPagination
                  page={shownPage}
                  totalPages={shownTotalPages}
                  total={shownTotal}
                  pageSize={params.pageSize}
                  onPageChange={(p) => setParams({ page: p }, { resetPage: false })}
                />
              )}
            </>
          )}
          {!isLoading && items.length === 0 && emptyAction && (
            <div className="flex justify-center mt-4">{emptyAction}</div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
