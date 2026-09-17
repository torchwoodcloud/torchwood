import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { PageHeader } from "@/components/PageHeader";
import { EmptyState } from "@/components/EmptyState";
import { LoadingTable } from "@/components/LoadingTable";
import { ListToolbar, SelectionBar, ListPagination } from "@/components/list/ListToolbar";
import { DataTable, type ColumnDef } from "@/components/list/DataTable";
import { useListParams, filterByQuery, paginate } from "@/hooks/useListParams";
import { useRowSelection } from "@/hooks/useRowSelection";
import { useMemo, useEffect } from "react";

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
}: ResourceListPageProps<T>) {
  const { params, setParams } = useListParams();

  const filtered = useMemo(
    () => filterByQuery(items, params.q, getSearchText),
    [items, params.q, getSearchText]
  );

  const { items: pageItems, total, totalPages, page } = useMemo(
    () => paginate(filtered, params.page, params.pageSize),
    [filtered, params.page, params.pageSize]
  );

  const selection = useRowSelection(pageItems);

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
          ) : pageItems.length === 0 ? (
            <EmptyState
              title={emptyTitle}
              description={emptyDescription}
            />
          ) : (
            <>
              <DataTable
                items={pageItems}
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
              <ListPagination
                page={page}
                totalPages={totalPages}
                total={total}
                pageSize={params.pageSize}
                onPageChange={(p) => setParams({ page: p }, { resetPage: false })}
              />
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
