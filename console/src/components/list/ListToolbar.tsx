import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import { Search, X } from "lucide-react";

interface ListToolbarProps {
  searchValue: string;
  onSearchChange: (value: string) => void;
  searchPlaceholder?: string;
  actions?: React.ReactNode;
  filters?: React.ReactNode;
}

export function ListToolbar({
  searchValue,
  onSearchChange,
  searchPlaceholder = "搜索...",
  actions,
  filters,
}: ListToolbarProps) {
  return (
    <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
      <div className="flex flex-1 flex-col gap-3 sm:flex-row sm:items-center">
        <div className="relative max-w-sm flex-1">
          <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={searchValue}
            onChange={(e) => onSearchChange(e.target.value)}
            placeholder={searchPlaceholder}
            className="pl-9 pr-9"
          />
          {searchValue && (
            <button
              type="button"
              onClick={() => onSearchChange("")}
              className="absolute right-3 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
            >
              <X className="h-4 w-4" />
            </button>
          )}
        </div>
        {filters}
      </div>
      {actions && <div className="flex items-center gap-2 shrink-0">{actions}</div>}
    </div>
  );
}

interface SelectionBarProps {
  count: number;
  onClear: () => void;
  actions?: React.ReactNode;
}

export function SelectionBar({ count, onClear, actions }: SelectionBarProps) {
  if (count === 0) return null;

  return (
    <div className="flex items-center justify-between rounded-lg border bg-muted/50 px-4 py-2">
      <span className="text-sm font-medium">已选择 {count} 项</span>
      <div className="flex items-center gap-2">
        {actions}
        <Button variant="ghost" size="sm" onClick={onClear}>
          取消选择
        </Button>
      </div>
    </div>
  );
}

interface ListPaginationProps {
  page: number;
  totalPages: number;
  total: number;
  pageSize: number;
  onPageChange: (page: number) => void;
}

export function ListPagination({
  page,
  totalPages,
  total,
  pageSize,
  onPageChange,
}: ListPaginationProps) {
  if (total === 0) return null;

  const start = (page - 1) * pageSize + 1;
  const end = Math.min(page * pageSize, total);

  return (
    <div className="flex items-center justify-between pt-4">
      <p className="text-sm text-muted-foreground">
        显示 {start}–{end}，共 {total} 条
      </p>
      <div className="flex items-center gap-2">
        <Button
          variant="outline"
          size="sm"
          disabled={page <= 1}
          onClick={() => onPageChange(page - 1)}
        >
          上一页
        </Button>
        <span className="text-sm text-muted-foreground">
          {page} / {totalPages}
        </span>
        <Button
          variant="outline"
          size="sm"
          disabled={page >= totalPages}
          onClick={() => onPageChange(page + 1)}
        >
          下一页
        </Button>
      </div>
    </div>
  );
}

interface ListPaginationKeysetProps {
  page: number;
  pageSize: number;
  rowCount: number;
  hasPrev: boolean;
  hasNext: boolean;
  onPrev: () => void;
  onNext: () => void;
  onPageSizeChange: (n: number) => void;
}

// 服务端 keyset 分页的分页栏：游标语义下 total 未知、不能跳页，
// 只有上一页/下一页与页大小切换（样式对齐 audit-logs）。
export function ListPaginationKeyset({
  page,
  pageSize,
  rowCount,
  hasPrev,
  hasNext,
  onPrev,
  onNext,
  onPageSizeChange,
}: ListPaginationKeysetProps) {
  const start = rowCount === 0 ? 0 : (page - 1) * pageSize + 1;
  const end = (page - 1) * pageSize + rowCount;

  return (
    <div className="flex items-center justify-between pt-4">
      <p className="text-sm text-muted-foreground">
        显示 {start}–{end}
      </p>
      <div className="flex items-center gap-2">
        <Select
          value={String(pageSize)}
          onValueChange={(v) => onPageSizeChange(Number(v))}
        >
          <SelectTrigger className="w-24">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PAGE_SIZE_OPTIONS.map((n) => (
              <SelectItem key={n} value={String(n)}>
                {n} 条
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button variant="outline" size="sm" disabled={!hasPrev} onClick={onPrev}>
          上一页
        </Button>
        <Button variant="outline" size="sm" disabled={!hasNext} onClick={onNext}>
          下一页
        </Button>
      </div>
    </div>
  );
}
