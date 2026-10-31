import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useCollection, useCollectionData, useCommand, useRecord } from "@lynx-go/uikit-core";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { toast } from "sonner";
import { ArrowDown, ArrowUp, Plus, Search, X } from "lucide-react";
import type { AssetDef } from "@/api/assets";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, canWrite } from "@/hooks/useAdminRole";
import { useServerPaging } from "@/hooks/useServerPaging";
import { filterByQuery } from "@/hooks/useListParams";
import { ListPaginationKeyset } from "@/components/list/ListToolbar";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
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
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
import {
  assetDefsResource,
  assetDefsSearch,
  createAssetDefCommand,
  defHoldersResource,
  removeAssetDefCommand,
  userAssetsResource,
  userLedgerResource,
} from "@/kit/assets";
import { describeApiError } from "@/kit/errors";

const CLASSES = ["currency", "stack", "instance", "entitlement"] as const;

export function AssetDefsListPage() {
  const { role } = useAdminRole();
  const navigate = useNavigate();
  const tz = useUserTimezone();
  const writeable = canWrite(role);
  const [search, setSearch] = useState("");

  // 列表 fleet 化（§12.9-2）：游标/页大小/时间排序进 URL（分页栈 = 浏览器
  // 历史）；页内当前页搜索保持本地态（旧页语义）。scope/enabled 不再逐处
  // 传（§12.10-③）：资源声明了 projectId，Provider 单源 + 未就绪自动停用。
  const list = useCollection(assetDefsResource, {
    from: "/console/assets",
    search: assetDefsSearch,
  });

  // 行删除命令接线（§12.3）：挂 asset-defs 资源——失效 = descriptor 声明
  //（kit 键同资源全 kind：列表 collection、详情 record、定义下拉全量一并
  // 命中；存量前缀桥随子视图 kit 化退役）。toast 单源 onSettled。
  const remove = useCommand(removeAssetDefCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Asset definition deleted");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const getSearchText = useCallback(
    (d: AssetDef) => `${d.id} ${d.code} ${d.name} ${d.class}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((d) => getSearchText(d).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const columns: LegacyColumnDef<AssetDef, unknown>[] = [
    {
      id: "code",
      header: "Code",
      cell: ({ row }) => (
        <span className="font-mono text-xs">{row.original.code}</span>
      ),
    },
    { id: "name", header: "Name", cell: ({ row }) => row.original.name },
    { id: "class", header: "Class", cell: ({ row }) => row.original.class },
    {
      id: "status",
      header: "Status",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "archived" ? "secondary" : "default"}>
          {row.original.status ?? "active"}
        </Badge>
      ),
    },
    {
      id: "created",
      header: "Created",
      meta: { sortField: "created_at" },
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    ...(writeable
      ? [
          {
            id: "actions",
            header: "Actions",
            cell: ({ row }: { row: { original: AssetDef } }) => (
              <RowDeleteButton
                onConfirm={() => remove.mutate({ id: row.original.id })}
                loading={remove.pending}
              />
            ),
          } satisfies LegacyColumnDef<AssetDef, unknown>,
        ]
      : []),
  ];

  return (
    <div className="space-y-6">
      <PageHeader title="Asset definitions" description="Manage currency / item / entitlement catalogs. End users have no write access." />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search code / name / class on this page..."
              className="h-8 w-56"
            />
            <div className="ml-auto flex gap-2">
              <Button size="sm" variant="outline" asChild>
                <Link to="/console/assets/users">Query user assets</Link>
              </Button>
              {writeable ? (
                <Button size="sm" asChild>
                  <Link to="/console/assets/defs/new">
                    <Plus className="h-4 w-4 mr-2" />
                    New definition
                  </Link>
                </Button>
              ) : null}
            </div>
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
          </div>
        }
        emptyTitle={search !== "" ? "No matching asset definitions" : "No asset definitions yet"}
        emptyDescription={
          search !== ""
            ? "Adjust your search, or clear it to restore all rows on this page"
            : "Create currency / stack / instance / entitlement definitions"
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={columns}
            getRowId={(row) => row.id}
            onRowClick={(d) => navigate({ to: `/console/assets/defs/${d.id}` })}
          />
        </div>
      </KitListPage>
    </div>
  );
}

export function AssetDefNewPage() {
  const navigate = useNavigate();
  const [code, setCode] = useState("");
  const [name, setName] = useState("");
  const [klass, setKlass] = useState<string>("currency");
  const [decimals, setDecimals] = useState("0");
  const [maxQuantity, setMaxQuantity] = useState("");

  // 创建命令接线（§12.3）：挂 asset-defs 资源——失效 = descriptor 声明
  //（kit 键同资源全 kind，存量前缀桥随子视图 kit 化退役）；
  // decimals/上限文本展示形在 descriptor run 内收敛。toast 单源 onSettled。
  const create = useCommand(createAssetDefCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Asset definition created");
        navigate({ to: `/console/assets/defs/${result.payload.id}` });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <FormPageWrapper
      title="New asset definition"
      backTo="/console/assets"
      submitLabel="Create"
      onSubmit={(e) => {
        e.preventDefault();
        if (!isInt64Input(maxQuantity)) {
          toast.error("max_quantity must be an integer in the smallest unit");
          return;
        }
        create.mutate({
          code,
          name,
          class: klass,
          decimalsText: decimals,
          maxQuantityText: maxQuantity,
        });
      }}
      loading={create.pending}
      submitDisabled={!code || !name}
    >
      <FormField id="code" label="Code" value={code} onChange={setCode} required placeholder="gold" />
      <FormField id="name" label="Name" value={name} onChange={setName} required placeholder="Gold coin" />
      <div className="space-y-2">
        <Label>Class</Label>
        <Select value={klass} onValueChange={setKlass}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {CLASSES.map((c) => (
              <SelectItem key={c} value={c}>
                {c}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <FormField id="decimals" label="Decimals (display only)" value={decimals} onChange={setDecimals} />
      <FormField
        id="max_quantity"
        label="max_quantity (integer in smallest unit, optional)"
        value={maxQuantity}
        onChange={setMaxQuantity}
        placeholder="Leave empty for no limit"
      />
    </FormPageWrapper>
  );
}

export function AssetDefDetailPage() {
  const { id } = useParams({ strict: false });
  const navigate = useNavigate();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const writeable = canWrite(role);

  // 详情读换轨 useRecord（子视图 kit 化批）：id 未就绪即停用（与旧 enabled
  // 同义）；projectId 未就绪由声明键自动停用；404 → null 走 NotFound 分支。
  const defRecord = useRecord(assetDefsResource, {
    id: id ?? undefined,
    scope: { projectId: projectId ?? undefined },
  });
  const def = defRecord.data;
  const isLoading = defRecord.status === "loading";

  // 删除命令接线（§12.3）：挂 asset-defs 资源——失效 = descriptor 声明
  //（kit 键同资源全 kind，本详情 record 键命中，存量前缀桥退役）。
  // toast 单源 onSettled。
  const remove = useCommand(removeAssetDefCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Asset definition deleted");
        navigate({ to: "/console/assets" });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  // 用户持有列表（定义维度）：UserID 过滤 + keyset 分页；过滤变化经
  // resetKeys 自动回第一页。换轨 useCollectionData（子视图 kit 化批）：
  // defId 声明键定就绪门，owner 过滤进 scope 对象（进 queryKey 不进失效
  // 轴）；窗口吃 useServerPaging；行 = .rows、下一页 = .segment?.nextCursor；
  // 旧 placeholderData 同页复用 → keepPrevious 缺省 true（零代码）。
  const [ownerInput, setOwnerInput] = useState("");
  const [ownerFilter, setOwnerFilter] = useState("");
  const holdersPaging = useServerPaging(20, { resetKeys: [ownerFilter] });
  const holders = useCollectionData(defHoldersResource, {
    scope: {
      projectId: projectId ?? undefined,
      defId: id ?? undefined,
      owner: ownerFilter || undefined,
    },
    window: { size: holdersPaging.pageSize, cursor: holdersPaging.pageToken || undefined },
  });
  const holderRows = holders.rows;

  const applyOwnerFilter = () => {
    setOwnerFilter(ownerInput.trim());
  };
  const clearOwnerFilter = () => {
    setOwnerInput("");
    setOwnerFilter("");
  };

  if (isLoading) return <DetailSkeleton />;
  if (!def) return <NotFound backTo="/console/assets" />;

  return (
    <DetailPageWrapper
      title={def.name}
      description={def.code}
      backTo="/console/assets"
      actions={writeable ? <DeleteButton onConfirm={() => remove.mutate({ id: id! })} loading={remove.pending} /> : undefined}
    >
      <DetailGrid
        items={[
          { label: "ID", value: def.id, mono: true },
          { label: "Code", value: def.code, mono: true },
          { label: "Class", value: def.class },
          { label: "Status", value: def.status ?? "active" },
          { label: "decimals", value: String(def.decimals) },
          { label: "max_quantity", value: def.max_quantity ? formatInt64(def.max_quantity) : "—" },
          { label: "Created", value: formatDateTime(def.created_at, tz) },
        ]}
      />

      <Card>
        <CardHeader>
          <CardTitle>User holdings (read-only)</CardTitle>
        </CardHeader>
        <CardContent>
          <form
            className="mb-4 flex flex-wrap items-end gap-3"
            onSubmit={(e) => {
              e.preventDefault();
              applyOwnerFilter();
            }}
          >
            <div className="space-y-2 min-w-[240px]">
              <Label htmlFor="holder-owner">User ID</Label>
              <Input
                id="holder-owner"
                value={ownerInput}
                onChange={(e) => setOwnerInput(e.target.value)}
                placeholder="Filter by user ID, leave empty to show all"
              />
            </div>
            <Button type="submit" variant="outline">
              Query
            </Button>
            {ownerFilter ? (
              <Button type="button" variant="ghost" onClick={clearOwnerFilter}>
                Clear
              </Button>
            ) : null}
          </form>
          {holders.status === "loading" ? (
            <p className="text-sm text-muted-foreground">Loading…</p>
          ) : holderRows.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              {ownerFilter ? "This user holds none of this asset" : "No user holdings yet"}
            </p>
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>User ID</TableHead>
                    <TableHead>Quantity</TableHead>
                    <TableHead>Level</TableHead>
                    <TableHead>Expires at</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {holderRows.map((h) => (
                    <TableRow key={h.id}>
                      <TableCell>
                        <Link
                          to="/console/users/$id" params={{ id: encodeURIComponent(h.owner_id ?? "") }}
                          className="font-mono text-xs hover:underline"
                        >
                          {h.owner_id}
                        </Link>
                      </TableCell>
                      <TableCell className="font-mono text-xs">{formatInt64(h.quantity)}</TableCell>
                      <TableCell>{h.level ?? "—"}</TableCell>
                      <TableCell>{h.expires_at ? formatDateTime(h.expires_at, tz) : "—"}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              <ListPaginationKeyset
                page={holdersPaging.page}
                pageSize={holdersPaging.pageSize}
                rowCount={holderRows.length}
                hasPrev={holdersPaging.hasPrev}
                hasNext={!!holders.segment?.nextCursor}
                onPrev={holdersPaging.goPrev}
                onNext={() => holdersPaging.goNext(holders.segment?.nextCursor)}
                onPageSizeChange={holdersPaging.setPageSize}
              />
            </>
          )}
        </CardContent>
      </Card>
    </DetailPageWrapper>
  );
}

// 流水动词的中文标签；未知动词回退原值（服务端新增枚举时不至于显示空白）。
const LEDGER_KIND_LABELS: Record<string, string> = {
  grant: "Grant",
  consume: "Consume",
  transfer_out: "Transfer out",
  transfer_in: "Transfer in",
  mutate: "Adjust",
  expire: "Expire",
};

export function UserAssetsPage() {
  const { projectId } = useAuth();
  const tz = useUserTimezone();
  const search = useSearch({ strict: false }) as Record<string, unknown>;
  const navigate = useNavigate();
  // 查询目标以 URL owner 参数为事实源：支持 /console/assets/users?owner=<id> 直达（用户详情页入口跳入）。
  const queryOwner = String(search.owner ?? "").trim();
  const setSearchParams = (next: { owner?: string }) =>
    void navigate({
      to: ".",
      search: (prev: Record<string, unknown>) => ({ ...prev, ...next }),
      replace: true,
    });
  const [ownerId, setOwnerId] = useState(queryOwner);

  useEffect(() => {
    setOwnerId(queryOwner);
  }, [queryOwner]);

  // 持有/流水/定义下拉换轨 kit 伴读资源（子视图 kit 化批）：queryOwner 等
  // 过滤/主体参数进 scope 对象（全部键进 queryKey，不进声明键）——就绪门 =
  // 声明键 projectId（自动）+ enabled: !!queryOwner 手动门（随迁保留）；
  // 窗口吃 useServerPaging；行 = .rows、下一页 = .segment?.nextCursor。旧
  // placeholderData 的同项目同用户同源复用 → keepPrevious 缺省 true（复用
  // 面放宽为跨 scope——过渡帧短暂可见旧属主行，见切片报告）。
  const holdingsPaging = useServerPaging();
  const holdings = useCollectionData(userAssetsResource, {
    scope: { projectId: projectId ?? undefined, owner: queryOwner || undefined },
    window: { size: holdingsPaging.pageSize, cursor: holdingsPaging.pageToken || undefined },
    enabled: !!queryOwner,
  });
  const holdingsRows = holdings.rows;
  // 流水过滤与排序：defCode 空 = 全部资产；ascending 缺省最新在前。
  // 过滤/排序变化经 resetKeys 自动 reset（keyset 游标绑定参数组合）。
  const [ledgerDefCode, setLedgerDefCode] = useState("");
  const [ledgerAscending, setLedgerAscending] = useState(false);
  const ledgerPaging = useServerPaging(20, { resetKeys: [ledgerDefCode, ledgerAscending] });
  const ledger = useCollectionData(userLedgerResource, {
    scope: {
      projectId: projectId ?? undefined,
      owner: queryOwner || undefined,
      defCode: ledgerDefCode || undefined,
      // ResourceScope 值域是 string——boolean 以 "1" 编码，适配器解码。
      ascending: ledgerAscending ? "1" : undefined,
    },
    window: { size: ledgerPaging.pageSize, cursor: ledgerPaging.pageToken || undefined },
    enabled: !!queryOwner,
  });
  const ledgerRows = ledger.rows;
  // 资产类型下拉选项：项目全部定义（首屏一次拉取，页大小取服务端上限）。
  // 复用 assetDefsResource 的 collection 槽（size 100 整窗、staleTime 60s
  // 随迁；enabled 只剩 queryOwner 手动门——projectId 由声明键自动停用）。
  const defOptions = useCollectionData(assetDefsResource, {
    scope: { projectId: projectId ?? undefined },
    size: 100,
    staleTime: 60_000,
    enabled: !!queryOwner,
  });
  // def code/id → 名称映射：持有/流水行只带 code（id 兜底），名称从这里投影。
  const { defNameByCode, defNameById } = useMemo(() => {
    const byCode = new Map<string, string>();
    const byId = new Map<string, string>();
    for (const d of defOptions.rows) {
      byCode.set(d.code, d.name);
      byId.set(d.id, d.name);
    }
    return { defNameByCode: byCode, defNameById: byId };
  }, [defOptions.rows]);
  const defNameOf = (defCode: string | undefined, defId: string) =>
    (defCode && defNameByCode.get(defCode)) || defNameById.get(defId) || "—";

  // 持有按 code/名称/类别页内搜索：持有是服务端 keyset 分页，与列表页
  // ResourceListPage 同语义——客户端只过滤当前页，不做切片分页。
  const [holdingSearch, setHoldingSearch] = useState("");
  const filteredHoldings = useMemo(
    () =>
      // kit 行是 ReadonlyArray——filterByQuery 吃可变数组，浅拷贝过界（过滤
      // 是纯函数，拷贝成本可忽略）。
      filterByQuery([...holdingsRows], holdingSearch, (h) => {
        const name = defNameOf(h.def_code, h.def_id);
        return `${h.def_code} ${name === "—" ? "" : name} ${h.class ?? ""}`;
      }),
    // defNameOf 闭包随映射更新；filterByQuery 纯函数。
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [holdingsRows, holdingSearch, defNameByCode, defNameById]
  );

  const submit = () => {
    const v = ownerId.trim();
    setSearchParams(v ? { owner: v } : { owner: undefined });
  };

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>Query user assets</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-wrap items-end gap-3">
          <form
            className="flex flex-wrap items-end gap-3"
            onSubmit={(e) => {
              e.preventDefault();
              submit();
            }}
          >
            <div className="space-y-2 min-w-[240px]">
              <Label htmlFor="owner">User ID</Label>
              <Input
                id="owner"
                value={ownerId}
                onChange={(e) => setOwnerId(e.target.value)}
                placeholder="user id"
              />
            </div>
            <Button type="submit" disabled={!ownerId.trim()}>
              Query
            </Button>
          </form>
          <Button variant="outline" asChild>
            <Link to="/console/assets">Back to definitions</Link>
          </Button>
        </CardContent>
      </Card>

      {queryOwner ? (
        <>
          {/* 项目作用域提示：查询经 X-Torchwood-Project 头路由到全局 selector
              选中的项目；用户 ID 是项目内标识，项目不对时这里会显示「无持有」
              而不是目标项目的数据——把作用域显性化便于发现。 */}
          <p className="text-sm text-muted-foreground">
            Querying project: <span className="font-mono text-foreground">{projectId ?? "—"}</span>
          </p>
          <Card>
            <CardContent className="pt-6">
              <Tabs defaultValue="holdings">
                <TabsList>
                  <TabsTrigger value="holdings">Holdings</TabsTrigger>
                  <TabsTrigger value="ledger">Ledger</TabsTrigger>
                </TabsList>
                <TabsContent value="holdings">
                  <p className="mb-3 text-sm text-muted-foreground">
                    Read-only view, no Grant / Consume / Transfer actions.
                  </p>
                  {holdings.status === "loading" ? (
                    <p className="text-sm text-muted-foreground">Loading…</p>
                  ) : holdingsRows.length === 0 ? (
                    <p className="text-sm text-muted-foreground">No holdings</p>
                  ) : (
                    <>
                      <div className="relative mb-3 max-w-sm">
                        <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
                        <Input
                          value={holdingSearch}
                          onChange={(e) => setHoldingSearch(e.target.value)}
                          placeholder="Search code / name / class on this page..."
                          className="pl-9 pr-9"
                        />
                        {holdingSearch && (
                          <button
                            type="button"
                            onClick={() => setHoldingSearch("")}
                            className="absolute right-3 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
                          >
                            <X className="h-4 w-4" />
                          </button>
                        )}
                      </div>
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead>Asset code</TableHead>
                            <TableHead>Name</TableHead>
                            <TableHead>Class</TableHead>
                            <TableHead>Quantity</TableHead>
                            <TableHead>Level</TableHead>
                            <TableHead>Expires at</TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {filteredHoldings.map((h) => (
                            <TableRow key={h.id}>
                              <TableCell className="font-mono text-xs">{h.def_code || h.def_id}</TableCell>
                              <TableCell>{defNameOf(h.def_code, h.def_id)}</TableCell>
                              <TableCell>{h.class || "—"}</TableCell>
                              <TableCell className="font-mono text-xs">{formatInt64(h.quantity)}</TableCell>
                              <TableCell>{h.level ?? "—"}</TableCell>
                              <TableCell>{h.expires_at ? formatDateTime(h.expires_at, tz) : "—"}</TableCell>
                            </TableRow>
                          ))}
                        </TableBody>
                      </Table>
                      {filteredHoldings.length === 0 ? (
                        <p className="mt-3 text-sm text-muted-foreground">No matches on this page</p>
                      ) : null}
                      <ListPaginationKeyset
                        page={holdingsPaging.page}
                        pageSize={holdingsPaging.pageSize}
                        rowCount={filteredHoldings.length}
                        hasPrev={holdingsPaging.hasPrev}
                        hasNext={!!holdings.segment?.nextCursor}
                        onPrev={holdingsPaging.goPrev}
                        onNext={() => holdingsPaging.goNext(holdings.segment?.nextCursor)}
                        onPageSizeChange={holdingsPaging.setPageSize}
                      />
                    </>
                  )}
                </TabsContent>
                <TabsContent value="ledger">
                  <div className="mb-3 flex flex-wrap items-center gap-2">
                    <Select
                      value={ledgerDefCode || "all"}
                      onValueChange={(v) => {
                        setLedgerDefCode(v === "all" ? "" : v);
                      }}
                    >
                      <SelectTrigger className="h-8 w-[180px]">
                        <SelectValue placeholder="All assets" />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="all">All assets</SelectItem>
                        {defOptions.rows.map((d) => (
                          <SelectItem key={d.id} value={d.code}>
                            {d.code}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => {
                        setLedgerAscending(!ledgerAscending);
                      }}
                    >
                      {ledgerAscending ? <ArrowUp className="h-3.5 w-3.5 mr-1" /> : <ArrowDown className="h-3.5 w-3.5 mr-1" />}
                      {ledgerAscending ? "Oldest first" : "Newest first"}
                    </Button>
                  </div>
                  {ledger.status === "loading" ? (
                    <p className="text-sm text-muted-foreground">Loading…</p>
                  ) : ledgerRows.length === 0 ? (
                    <p className="text-sm text-muted-foreground">No ledger entries</p>
                  ) : (
                    <>
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead>Time</TableHead>
                            <TableHead>Type</TableHead>
                            <TableHead>Asset code</TableHead>
                            <TableHead>Name</TableHead>
                            <TableHead className="text-right">Change</TableHead>
                            <TableHead className="text-right">Balance after</TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {ledgerRows.map((e) => (
                            <TableRow key={e.id}>
                              <TableCell className="whitespace-nowrap">{e.created_at ? formatDateTime(e.created_at, tz) : "—"}</TableCell>
                              <TableCell>
                                <Badge variant={e.kind === "grant" || e.kind === "transfer_in" ? "default" : "secondary"}>
                                  {LEDGER_KIND_LABELS[e.kind] ?? e.kind}
                                </Badge>
                              </TableCell>
                              <TableCell className="font-mono text-xs">{e.def_code || e.def_id}</TableCell>
                              <TableCell>{defNameOf(e.def_code, e.def_id)}</TableCell>
                              <TableCell className="text-right font-mono text-xs">
                                {e.delta.startsWith("-") || e.delta === "0" ? "" : "+"}
                                {formatInt64(e.delta)}
                              </TableCell>
                              <TableCell className="text-right font-mono text-xs">{formatInt64(e.quantity_after)}</TableCell>
                            </TableRow>
                          ))}
                        </TableBody>
                      </Table>
                      <ListPaginationKeyset
                        page={ledgerPaging.page}
                        pageSize={ledgerPaging.pageSize}
                        rowCount={ledgerRows.length}
                        hasPrev={ledgerPaging.hasPrev}
                        hasNext={!!ledger.segment?.nextCursor}
                        onPrev={ledgerPaging.goPrev}
                        onNext={() => ledgerPaging.goNext(ledger.segment?.nextCursor)}
                        onPageSizeChange={ledgerPaging.setPageSize}
                      />
                    </>
                  )}
                </TabsContent>
              </Tabs>
            </CardContent>
          </Card>
        </>
      ) : null}
    </div>
  );
}
