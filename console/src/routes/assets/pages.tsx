import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus } from "lucide-react";
import {
  createAssetDef,
  deleteAssetDef,
  getAssetDef,
  listAssetDefs,
  listUserAssets,
  listUserLedger,
  type AssetDef,
} from "@/api/assets";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, canWrite } from "@/hooks/useAdminRole";
import { ResourceListPage } from "@/components/list/ResourceListPage";
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

function formatTime(value?: string) {
  if (!value) return "—";
  return new Date(value).toLocaleString();
}

const defColumns: ColumnDef<AssetDef>[] = [
  { key: "code", header: "Code", className: "font-mono text-xs", cell: (d) => d.code },
  { key: "name", header: "名称", cell: (d) => d.name },
  { key: "class", header: "类别", cell: (d) => d.class },
  {
    key: "status",
    header: "状态",
    cell: (d) => <Badge variant={d.status === "archived" ? "secondary" : "default"}>{d.status ?? "active"}</Badge>,
  },
  { key: "created", header: "创建时间", cell: (d) => formatTime(d.created_at) },
];

export function AssetDefsListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const queryClient = useQueryClient();
  const writeable = canWrite(role);

  const { data: defs = [], isLoading } = useQuery({
    queryKey: ["asset-defs", projectId],
    queryFn: listAssetDefs,
    enabled: !!projectId,
  });

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
      searchPlaceholder="搜索 code / 名称 / 类别..."
      isLoading={isLoading}
      items={defs}
      columns={defColumns}
      getSearchText={getSearchText}
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
          { label: "创建时间", value: formatTime(def.created_at) },
        ]}
      />
    </DetailPageWrapper>
  );
}

export function UserAssetsPage() {
  const { projectId } = useAuth();
  const [searchParams, setSearchParams] = useSearchParams();
  // 查询目标以 URL owner 参数为事实源：支持 /console/assets/users?owner=<id> 直达（用户详情页入口跳入）。
  const queryOwner = searchParams.get("owner")?.trim() ?? "";
  const [ownerId, setOwnerId] = useState(queryOwner);

  useEffect(() => {
    setOwnerId(queryOwner);
  }, [queryOwner]);

  const holdings = useQuery({
    queryKey: ["user-assets", projectId, queryOwner],
    queryFn: () => listUserAssets(queryOwner),
    enabled: !!projectId && !!queryOwner,
  });
  const ledger = useQuery({
    queryKey: ["user-ledger", projectId, queryOwner],
    queryFn: () => listUserLedger(queryOwner),
    enabled: !!projectId && !!queryOwner,
  });

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
          <Card>
            <CardHeader>
              <CardTitle>持有（只读，无 Grant / Consume / Transfer）</CardTitle>
            </CardHeader>
            <CardContent>
              {holdings.isLoading ? (
                <p className="text-sm text-muted-foreground">加载中…</p>
              ) : (holdings.data ?? []).length === 0 ? (
                <p className="text-sm text-muted-foreground">无持有</p>
              ) : (
                <ul className="space-y-2 text-sm">
                  {(holdings.data ?? []).map((h) => (
                    <li key={h.id} className="font-mono">
                      {h.def_code} {h.class} qty={formatInt64(h.quantity)}
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>流水</CardTitle>
            </CardHeader>
            <CardContent>
              {ledger.isLoading ? (
                <p className="text-sm text-muted-foreground">加载中…</p>
              ) : (ledger.data ?? []).length === 0 ? (
                <p className="text-sm text-muted-foreground">无流水</p>
              ) : (
                <ul className="space-y-2 text-sm">
                  {(ledger.data ?? []).map((e) => (
                    <li key={e.id} className="font-mono">
                      {e.kind} {e.def_code ?? e.def_id} Δ{formatInt64(e.delta)} after={formatInt64(e.quantity_after)}
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>
        </>
      ) : null}
    </div>
  );
}
