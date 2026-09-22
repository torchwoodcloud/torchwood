import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { toast } from "sonner";
import { ArrowDown, ArrowUp, Plus, Search, X } from "lucide-react";
import {
  createAssetDef,
  deleteAssetDef,
  getAssetDef,
  listAssetDefs,
  listDefHolders,
  listUserAssets,
  listUserLedger,
  type AssetDef,
} from "@/api/assets";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, canWrite } from "@/hooks/useAdminRole";
import { useServerPaging } from "@/hooks/useServerPaging";
import { filterByQuery } from "@/hooks/useListParams";
import { ResourceListPage } from "@/components/list/ResourceListPage";
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

const CLASSES = ["currency", "stack", "instance", "entitlement"] as const;

const defColumns = (tz: string): ColumnDef<AssetDef>[] => [
  { key: "code", header: "Code", className: "font-mono text-xs", cell: (d) => d.code },
  { key: "name", header: "名称", cell: (d) => d.name },
  { key: "class", header: "类别", cell: (d) => d.class },
  {
    key: "status",
    header: "状态",
    cell: (d) => <Badge variant={d.status === "archived" ? "secondary" : "default"}>{d.status ?? "active"}</Badge>,
  },
  { key: "created", header: "创建时间", cell: (d) => formatDateTime(d.created_at, tz) },
];

export function AssetDefsListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const queryClient = useQueryClient();
  const tz = useUserTimezone();
  const writeable = canWrite(role);
  const paging = useServerPaging();

  const { data, isLoading } = useQuery({
    queryKey: ["asset-defs", projectId, paging.pageSize, paging.pageToken],
    queryFn: () => listAssetDefs({ pageSize: paging.pageSize, pageToken: paging.pageToken }),
    enabled: !!projectId,
    placeholderData: (prev) => prev,
  });
  const defs = data?.rows ?? [];
  const paging_ = {
    page: paging.page,
    pageSize: paging.pageSize,
    hasPrev: paging.hasPrev,
    hasNext: !!data?.nextPageToken,
    onPrev: paging.goPrev,
    onNext: () => paging.goNext(data?.nextPageToken),
    onPageSizeChange: paging.setPageSize,
  };

  const remove = useMutation({
    mutationFn: (id: string) => deleteAssetDef(id),
    onSuccess: () => {
      toast.success("资产定义已删除");
      queryClient.invalidateQueries({ queryKey: ["asset-defs", projectId] });
    },
  });

  const getSearchText = useCallback((d: AssetDef) => `${d.id} ${d.code} ${d.name} ${d.class}`, []);

  return (
    <ResourceListPage
      title="资产定义"
      description="管理代币 / 物品 / 权益目录。终端用户无写入口。"
      searchPlaceholder="当前页内搜索 code / 名称 / 类别..."
      isLoading={isLoading}
      items={defs}
      columns={defColumns(tz)}
      getSearchText={getSearchText}
      serverPaging={paging_}
      detailPath={(d) => `/console/assets/defs/${d.id}`}
      toolbarActions={
        <div className="flex gap-2">
          <Button variant="outline" asChild>
            <Link to="/console/assets/users">查询用户资产</Link>
          </Button>
          {writeable ? (
            <Button asChild>
              <Link to="/console/assets/defs/new">
                <Plus className="h-4 w-4 mr-2" />
                新建定义
              </Link>
            </Button>
          ) : undefined}
        </div>
      }
      rowActions={
        writeable
          ? (d) => <RowDeleteButton onConfirm={() => remove.mutate(d.id)} loading={remove.isPending} />
          : undefined
      }
      emptyTitle="暂无资产定义"
      emptyDescription="创建 currency / stack / instance / entitlement 定义"
    />
  );
}

export function AssetDefNewPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const [code, setCode] = useState("");
  const [name, setName] = useState("");
  const [klass, setKlass] = useState<string>("currency");
  const [decimals, setDecimals] = useState("0");
  const [maxQuantity, setMaxQuantity] = useState("");

  const mutation = useMutation({
    mutationFn: () =>
      createAssetDef({
        code,
        name,
        class: klass,
        decimals: Number.parseInt(decimals, 10) || 0,
        max_quantity: maxQuantity === "" ? undefined : maxQuantity,
      }),
    onSuccess: (def) => {
      toast.success("资产定义已创建");
      queryClient.invalidateQueries({ queryKey: ["asset-defs", projectId] });
      navigate(`/console/assets/defs/${def.id}`);
    },
  });

  return (
    <FormPageWrapper
      title="新建资产定义"
      backTo="/console/assets"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        if (!isInt64Input(maxQuantity)) {
          toast.error("max_quantity 必须是整数最小单位");
          return;
        }
        mutation.mutate();
      }}
      loading={mutation.isPending}
      submitDisabled={!code || !name}
    >
      <FormField id="code" label="Code" value={code} onChange={setCode} required placeholder="gold" />
      <FormField id="name" label="名称" value={name} onChange={setName} required placeholder="金币" />
      <div className="space-y-2">
        <Label>类别</Label>
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
      <FormField id="decimals" label="小数位（仅展示）" value={decimals} onChange={setDecimals} />
      <FormField
        id="max_quantity"
        label="max_quantity（最小单位整数，可空）"
        value={maxQuantity}
        onChange={setMaxQuantity}
        placeholder="留空表示不限"
      />
    </FormPageWrapper>
  );
}

export function AssetDefDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const writeable = canWrite(role);

  const { data: def, isLoading } = useQuery({
    queryKey: ["asset-defs", id],
    queryFn: () => getAssetDef(id!),
    enabled: !!id,
  });

  const remove = useMutation({
    mutationFn: () => deleteAssetDef(id!),
    onSuccess: () => {
      toast.success("资产定义已删除");
      queryClient.invalidateQueries({ queryKey: ["asset-defs", projectId] });
      navigate("/console/assets");
    },
  });

  // 用户持有列表（定义维度）：UserID 过滤 + keyset 分页；过滤条件变化 reset 回第一页。
  const [ownerInput, setOwnerInput] = useState("");
  const [ownerFilter, setOwnerFilter] = useState("");
  const holdersPaging = useServerPaging();
  const holders = useQuery({
    queryKey: ["def-holders", projectId, id, ownerFilter, holdersPaging.pageSize, holdersPaging.pageToken],
    queryFn: () =>
      listDefHolders(id!, {
        ownerId: ownerFilter || undefined,
        pageSize: holdersPaging.pageSize,
        pageToken: holdersPaging.pageToken,
      }),
    enabled: !!id,
    placeholderData: (prev) => prev,
  });
  const holderRows = holders.data?.rows ?? [];

  const applyOwnerFilter = () => {
    setOwnerFilter(ownerInput.trim());
    holdersPaging.reset();
  };
  const clearOwnerFilter = () => {
    setOwnerInput("");
    setOwnerFilter("");
    holdersPaging.reset();
  };

  if (isLoading) return <DetailSkeleton />;
  if (!def) return <NotFound backTo="/console/assets" />;

  return (
    <DetailPageWrapper
      title={def.name}
      description={def.code}
      backTo="/console/assets"
      actions={writeable ? <DeleteButton onConfirm={() => remove.mutate()} loading={remove.isPending} /> : undefined}
    >
      <DetailGrid
        items={[
          { label: "ID", value: def.id, mono: true },
          { label: "Code", value: def.code, mono: true },
          { label: "类别", value: def.class },
          { label: "状态", value: def.status ?? "active" },
          { label: "decimals", value: String(def.decimals) },
          { label: "max_quantity", value: def.max_quantity ? formatInt64(def.max_quantity) : "—" },
          { label: "创建时间", value: formatDateTime(def.created_at, tz) },
        ]}
      />

      <Card>
        <CardHeader>
          <CardTitle>用户持有（只读）</CardTitle>
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
              <Label htmlFor="holder-owner">用户 ID</Label>
              <Input
                id="holder-owner"
                value={ownerInput}
                onChange={(e) => setOwnerInput(e.target.value)}
                placeholder="按 UserID 过滤，留空显示全部"
              />
            </div>
            <Button type="submit" variant="outline">
              查询
            </Button>
            {ownerFilter ? (
              <Button type="button" variant="ghost" onClick={clearOwnerFilter}>
                清除
              </Button>
            ) : null}
          </form>
          {holders.isLoading ? (
            <p className="text-sm text-muted-foreground">加载中…</p>
          ) : holderRows.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              {ownerFilter ? "该用户无此资产持有" : "暂无用户持有"}
            </p>
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>用户 ID</TableHead>
                    <TableHead>数量</TableHead>
                    <TableHead>等级</TableHead>
                    <TableHead>到期时间</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {holderRows.map((h) => (
                    <TableRow key={h.id}>
                      <TableCell>
                        <Link
                          to={`/console/users/${encodeURIComponent(h.owner_id ?? "")}`}
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
                hasNext={!!holders.data?.nextPageToken}
                onPrev={holdersPaging.goPrev}
                onNext={() => holdersPaging.goNext(holders.data?.nextPageToken)}
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
  grant: "发放",
  consume: "消耗",
  transfer_out: "转出",
  transfer_in: "转入",
  mutate: "变更",
  expire: "失效",
};

// placeholderData 只在同项目同用户间复用：切换项目 / 用户后不再用旧数据占位，
// 避免「流水还是上一项目的、持有已是新项目的空结果」这类错位显示。
function sameScopePlaceholder<T>(
  prev: T | undefined,
  prevKey: readonly unknown[] | undefined,
  projectId: string | null,
  owner: string,
): T | undefined {
  if (!prev || !prevKey) return undefined;
  return prevKey[1] === projectId && prevKey[2] === owner ? prev : undefined;
}

export function UserAssetsPage() {
  const { projectId } = useAuth();
  const tz = useUserTimezone();
  const [searchParams, setSearchParams] = useSearchParams();
  // 查询目标以 URL owner 参数为事实源：支持 /console/assets/users?owner=<id> 直达（用户详情页入口跳入）。
  const queryOwner = searchParams.get("owner")?.trim() ?? "";
  const [ownerId, setOwnerId] = useState(queryOwner);

  useEffect(() => {
    setOwnerId(queryOwner);
  }, [queryOwner]);

  const holdingsPaging = useServerPaging();
  const holdings = useQuery({
    queryKey: ["user-assets", projectId, queryOwner, holdingsPaging.pageSize, holdingsPaging.pageToken],
    queryFn: () =>
      listUserAssets(queryOwner, { pageSize: holdingsPaging.pageSize, pageToken: holdingsPaging.pageToken }),
    enabled: !!projectId && !!queryOwner,
    placeholderData: (prev, prevQuery) => sameScopePlaceholder(prev, prevQuery?.queryKey, projectId, queryOwner),
  });
  const holdingsRows = holdings.data?.rows ?? [];
  // 流水过滤与排序：defCode 空 = 全部资产；ascending 缺省最新在前。
  // 变更过滤/排序都 reset 分页（keyset 游标绑定参数组合）。
  const [ledgerDefCode, setLedgerDefCode] = useState("");
  const [ledgerAscending, setLedgerAscending] = useState(false);
  const ledgerPaging = useServerPaging();
  const ledger = useQuery({
    queryKey: [
      "user-ledger",
      projectId,
      queryOwner,
      ledgerDefCode,
      ledgerAscending,
      ledgerPaging.pageSize,
      ledgerPaging.pageToken,
    ],
    queryFn: () =>
      listUserLedger(queryOwner, {
        pageSize: ledgerPaging.pageSize,
        pageToken: ledgerPaging.pageToken,
        defCode: ledgerDefCode || undefined,
        ascending: ledgerAscending || undefined,
      }),
    enabled: !!projectId && !!queryOwner,
    placeholderData: (prev, prevQuery) => sameScopePlaceholder(prev, prevQuery?.queryKey, projectId, queryOwner),
  });
  const ledgerRows = ledger.data?.rows ?? [];
  // 资产类型下拉选项：项目全部定义（首屏一次拉取，页大小取服务端上限）。
  const defOptions = useQuery({
    queryKey: ["asset-defs-all", projectId],
    queryFn: () => listAssetDefs({ pageSize: 100 }),
    enabled: !!projectId && !!queryOwner,
    staleTime: 60_000,
  });
  // def code/id → 名称映射：持有/流水行只带 code（id 兜底），名称从这里投影。
  const { defNameByCode, defNameById } = useMemo(() => {
    const byCode = new Map<string, string>();
    const byId = new Map<string, string>();
    for (const d of defOptions.data?.rows ?? []) {
      byCode.set(d.code, d.name);
      byId.set(d.id, d.name);
    }
    return { defNameByCode: byCode, defNameById: byId };
  }, [defOptions.data]);
  const defNameOf = (defCode: string | undefined, defId: string) =>
    (defCode && defNameByCode.get(defCode)) || defNameById.get(defId) || "—";

  // 持有按 code/名称/类别页内搜索：持有是服务端 keyset 分页，与列表页
  // ResourceListPage 同语义——客户端只过滤当前页，不做切片分页。
  const [holdingSearch, setHoldingSearch] = useState("");
  const filteredHoldings = useMemo(
    () =>
      filterByQuery(holdingsRows, holdingSearch, (h) => {
        const name = defNameOf(h.def_code, h.def_id);
        return `${h.def_code} ${name === "—" ? "" : name} ${h.class ?? ""}`;
      }),
    // defNameOf 闭包随映射更新；filterByQuery 纯函数。
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [holdingsRows, holdingSearch, defNameByCode, defNameById]
  );

  const submit = () => {
    const v = ownerId.trim();
    setSearchParams(v ? { owner: v } : {});
  };

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>查询用户资产</CardTitle>
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
              <Label htmlFor="owner">用户 ID</Label>
              <Input
                id="owner"
                value={ownerId}
                onChange={(e) => setOwnerId(e.target.value)}
                placeholder="user id"
              />
            </div>
            <Button type="submit" disabled={!ownerId.trim()}>
              查询
            </Button>
          </form>
          <Button variant="outline" asChild>
            <Link to="/console/assets">返回定义</Link>
          </Button>
        </CardContent>
      </Card>

      {queryOwner ? (
        <>
          {/* 项目作用域提示：查询经 X-Torchwood-Project 头路由到全局 selector
              选中的项目；用户 ID 是项目内标识，项目不对时这里会显示「无持有」
              而不是目标项目的数据——把作用域显性化便于发现。 */}
          <p className="text-sm text-muted-foreground">
            查询项目：<span className="font-mono text-foreground">{projectId ?? "—"}</span>
          </p>
          <Card>
            <CardContent className="pt-6">
              <Tabs defaultValue="holdings">
                <TabsList>
                  <TabsTrigger value="holdings">持有</TabsTrigger>
                  <TabsTrigger value="ledger">流水</TabsTrigger>
                </TabsList>
                <TabsContent value="holdings">
                  <p className="mb-3 text-sm text-muted-foreground">
                    只读视图，无 Grant / Consume / Transfer 操作入口。
                  </p>
                  {holdings.isLoading ? (
                    <p className="text-sm text-muted-foreground">加载中…</p>
                  ) : holdingsRows.length === 0 ? (
                    <p className="text-sm text-muted-foreground">无持有</p>
                  ) : (
                    <>
                      <div className="relative mb-3 max-w-sm">
                        <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
                        <Input
                          value={holdingSearch}
                          onChange={(e) => setHoldingSearch(e.target.value)}
                          placeholder="当前页内搜索 code / 名称 / 类别..."
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
                            <TableHead>资产 Code</TableHead>
                            <TableHead>名称</TableHead>
                            <TableHead>类别</TableHead>
                            <TableHead>数量</TableHead>
                            <TableHead>等级</TableHead>
                            <TableHead>到期时间</TableHead>
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
                        <p className="mt-3 text-sm text-muted-foreground">当前页无匹配项</p>
                      ) : null}
                      <ListPaginationKeyset
                        page={holdingsPaging.page}
                        pageSize={holdingsPaging.pageSize}
                        rowCount={filteredHoldings.length}
                        hasPrev={holdingsPaging.hasPrev}
                        hasNext={!!holdings.data?.nextPageToken}
                        onPrev={holdingsPaging.goPrev}
                        onNext={() => holdingsPaging.goNext(holdings.data?.nextPageToken)}
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
                        ledgerPaging.reset();
                      }}
                    >
                      <SelectTrigger className="h-8 w-[180px]">
                        <SelectValue placeholder="全部资产" />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="all">全部资产</SelectItem>
                        {(defOptions.data?.rows ?? []).map((d) => (
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
                        ledgerPaging.reset();
                      }}
                    >
                      {ledgerAscending ? <ArrowUp className="h-3.5 w-3.5 mr-1" /> : <ArrowDown className="h-3.5 w-3.5 mr-1" />}
                      {ledgerAscending ? "最早在前" : "最新在前"}
                    </Button>
                  </div>
                  {ledger.isLoading ? (
                    <p className="text-sm text-muted-foreground">加载中…</p>
                  ) : ledgerRows.length === 0 ? (
                    <p className="text-sm text-muted-foreground">无流水</p>
                  ) : (
                    <>
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead>时间</TableHead>
                            <TableHead>类型</TableHead>
                            <TableHead>资产 Code</TableHead>
                            <TableHead>名称</TableHead>
                            <TableHead className="text-right">变动</TableHead>
                            <TableHead className="text-right">变动后余额</TableHead>
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
                        hasNext={!!ledger.data?.nextPageToken}
                        onPrev={ledgerPaging.goPrev}
                        onNext={() => ledgerPaging.goNext(ledger.data?.nextPageToken)}
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
