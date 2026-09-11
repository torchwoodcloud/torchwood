import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { toast } from "sonner";
import { Pencil, Plus, Trophy } from "lucide-react";
import {
  createBoard,
  deleteBoard,
  deleteBoardEntry,
  getBoard,
  getBoardEntry,
  LEADERBOARD_PERIOD_KINDS,
  LEADERBOARD_POLICIES,
  LEADERBOARD_SORTS,
  LEADERBOARD_TIE_BREAKS,
  listBoardPeriods,
  listBoardTop,
  listBoards,
  updateBoard,
  type LeaderboardBoard,
  type LeaderboardScoreSnapshot,
} from "@/api/leaderboards";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { RowDeleteButton } from "@/components/resource/shared";
import type { ColumnDef } from "@/components/list/DataTable";

const boardColumns: ColumnDef<LeaderboardBoard>[] = [
  {
    key: "id",
    header: "榜 ID",
    cell: (b) => <Link className="font-medium underline-offset-2 hover:underline" to={`/console/leaderboards/${b.id}`}>{b.id}</Link>,
  },
  {
    key: "period",
    header: "期 / 时区",
    cell: (b) => (
      <span className="text-sm">
        {b.period_kind === "none" ? "全期" : `${b.period_kind}${b.period_tz ? ` · ${b.period_tz}` : ""}`}
      </span>
    ),
  },
  {
    key: "semantics",
    header: "排序 / 合并",
    cell: (b) => (
      <div className="flex flex-wrap gap-1">
        <Badge variant="secondary">{b.sort ?? "desc"}</Badge>
        <Badge variant="outline">{b.policy ?? "best"}</Badge>
        {b.tiebreak_order ? <Badge variant="outline">tb:{b.tiebreak_order}</Badge> : null}
        <Badge variant="outline">{b.tie_break ?? "parallel"}</Badge>
      </div>
    ),
  },
  {
    key: "client_submit",
    header: "客户端提交",
    cell: (b) =>
      b.client_submit ? <Badge>开放</Badge> : <Badge variant="outline">仅服务端</Badge>,
  },
  {
    key: "limit",
    header: "限频 / 保留",
    cell: (b) => (
      <span className="text-sm text-muted-foreground">
        {b.per_subject_submit_limit ?? 100}/期 · {b.retention_periods ? `${b.retention_periods} 期` : "永久"}
      </span>
    ),
  },
  {
    key: "updated_at",
    header: "更新时间",
    cell: (b) => (b.updated_at ? new Date(b.updated_at).toLocaleString() : "—"),
  },
];

export function LeaderboardsListPage() {
  const queryClient = useQueryClient();
  const { data: boards = [], isLoading } = useQuery({
    queryKey: ["leaderboards-boards"],
    queryFn: listBoards,
  });
  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ["leaderboards-boards"] });

  const remove = useMutation({
    mutationFn: deleteBoard,
    onSuccess: () => {
      toast.success("榜已删除（条目级联删除）");
      invalidate();
    },
  });

  return (
    <ResourceListPage
      title="排行榜"
      description="榜配置（期 / 排序 / 合并策略 / 限频 / 保留）。作弊处理：进入榜详情删条目——封榜后的期不可变。"
      searchPlaceholder="搜索榜 ID..."
      isLoading={isLoading}
      items={boards}
      columns={boardColumns}
      getSearchText={(b) => `${b.id} ${b.subject_kind ?? ""}`}
      toolbarActions={
        <CreateBoardDialog onCreated={invalidate} />
      }
      rowActions={(b) => (
        <div className="flex items-center gap-1">
          <EditBoardDialog board={b} onSaved={invalidate} />
          <RowDeleteButton
            onConfirm={() => remove.mutate(b.id)}
            loading={remove.isPending}
          />
        </div>
      )}
      emptyTitle="暂无排行榜"
      emptyDescription="创建第一个榜后即可开始提交分数"
    />
  );
}

const inputCls = "space-y-2";

function CreateBoardDialog({ onCreated }: { onCreated: () => void }) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({
    id: "",
    sort: "desc",
    tiebreakOrder: "",
    tieBreak: "parallel",
    periodKind: "daily",
    periodTz: "Asia/Shanghai",
    policy: "best",
    valueMin: "",
    valueMax: "",
    clientSubmit: true,
    limit: "100",
    retention: "0",
    subjectKind: "user",
  });

  const mutation = useMutation({
    mutationFn: createBoard,
    onSuccess: () => {
      toast.success("榜已创建");
      setOpen(false);
      onCreated();
    },
  });

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button>
          <Plus className="h-4 w-4 mr-2" />
          新建榜
        </Button>
      </DialogTrigger>
      <DialogContent className="max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>新建排行榜</DialogTitle>
          <DialogDescription>
            排序 / tiebreak / 期类型 / 合并策略在有条目后不可修改
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            mutation.mutate({
              id: form.id,
              sort: form.sort,
              tiebreak_order: form.tiebreakOrder || undefined,
              tie_break: form.tieBreak,
              period_kind: form.periodKind,
              period_tz: form.periodKind === "none" ? undefined : form.periodTz,
              policy: form.policy,
              value_min: form.valueMin === "" ? undefined : form.valueMin,
              value_max: form.valueMax === "" ? undefined : form.valueMax,
              client_submit: form.clientSubmit,
              per_subject_submit_limit: Number(form.limit) || 100,
              retention_periods: Number(form.retention) || 0,
              subject_kind: form.subjectKind,
            });
          }}
        >
          <div className={inputCls}>
            <Label htmlFor="board-id">榜 ID</Label>
            <Input
              id="board-id"
              value={form.id}
              onChange={(e) => setForm({ ...form, id: e.target.value })}
              placeholder="daily_final（^[a-z_][a-z0-9_]*$ ≤40）"
              required
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className={inputCls}>
              <Label>主值排序</Label>
              <Select value={form.sort} onValueChange={(v) => setForm({ ...form, sort: v })}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  {LEADERBOARD_SORTS.map((s) => <SelectItem key={s} value={s}>{s}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            <div className={inputCls}>
              <Label>tiebreak 列（可选）</Label>
              <Select
                value={form.tiebreakOrder || "none"}
                onValueChange={(v) => setForm({ ...form, tiebreakOrder: v === "none" ? "" : v })}
              >
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">不声明</SelectItem>
                  {LEADERBOARD_SORTS.map((s) => <SelectItem key={s} value={s}>{s}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            <div className={inputCls}>
              <Label>并列裁决</Label>
              <Select value={form.tieBreak} onValueChange={(v) => setForm({ ...form, tieBreak: v })}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  {LEADERBOARD_TIE_BREAKS.map((s) => <SelectItem key={s} value={s}>{s}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            <div className={inputCls}>
              <Label>合并策略</Label>
              <Select value={form.policy} onValueChange={(v) => setForm({ ...form, policy: v })}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  {LEADERBOARD_POLICIES.map((s) => <SelectItem key={s} value={s}>{s}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            <div className={inputCls}>
              <Label>期类型</Label>
              <Select value={form.periodKind} onValueChange={(v) => setForm({ ...form, periodKind: v })}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  {LEADERBOARD_PERIOD_KINDS.map((s) => <SelectItem key={s} value={s}>{s}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            <div className={inputCls}>
              <Label>期时区（IANA）</Label>
              <Input
                value={form.periodTz}
                onChange={(e) => setForm({ ...form, periodTz: e.target.value })}
                disabled={form.periodKind === "none"}
                placeholder="Asia/Shanghai"
              />
            </div>
            <div className={inputCls}>
              <Label>分数下限（可选）</Label>
              <Input value={form.valueMin} onChange={(e) => setForm({ ...form, valueMin: e.target.value })} placeholder="0" />
            </div>
            <div className={inputCls}>
              <Label>分数上限（可选）</Label>
              <Input value={form.valueMax} onChange={(e) => setForm({ ...form, valueMax: e.target.value })} placeholder="3080" />
            </div>
            <div className={inputCls}>
              <Label>每期每主体提交上限</Label>
              <Input type="number" min={1} max={10000} value={form.limit} onChange={(e) => setForm({ ...form, limit: e.target.value })} />
            </div>
            <div className={inputCls}>
              <Label>保留期数（0=永久）</Label>
              <Input type="number" min={0} value={form.retention} onChange={(e) => setForm({ ...form, retention: e.target.value })} />
            </div>
          </div>
          <div className={inputCls}>
            <Label>主体类型（展示提示，无语义）</Label>
            <Input value={form.subjectKind} onChange={(e) => setForm({ ...form, subjectKind: e.target.value })} />
          </div>
          <div className="flex items-center gap-2">
            <Checkbox
              id="client-submit"
              checked={form.clientSubmit}
              onChange={(e) => setForm({ ...form, clientSubmit: e.target.checked })}
            />
            <Label htmlFor="client-submit">允许终端用户直接提交（client_submit）</Label>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>取消</Button>
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? "创建中…" : "创建"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function EditBoardDialog({ board, onSaved }: { board: LeaderboardBoard; onSaved: () => void }) {
  const [open, setOpen] = useState(false);
  const [valueMin, setValueMin] = useState(board.value_min ?? "");
  const [valueMax, setValueMax] = useState(board.value_max ?? "");
  const [clientSubmit, setClientSubmit] = useState(board.client_submit ?? false);
  const [limit, setLimit] = useState(String(board.per_subject_submit_limit ?? 100));
  const [retention, setRetention] = useState(String(board.retention_periods ?? 0));
  const [subjectKind, setSubjectKind] = useState(board.subject_kind ?? "user");

  const mutation = useMutation({
    mutationFn: (input: Parameters<typeof updateBoard>[1]) => updateBoard(board.id, input),
    onSuccess: () => {
      toast.success("榜已更新");
      setOpen(false);
      onSaved();
    },
  });

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="ghost" size="icon">
          <Pencil className="h-4 w-4" />
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>编辑榜（可变字段）</DialogTitle>
          <DialogDescription>
            {board.id} · 排序 / 期 / 合并策略在有条目后不可改
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            mutation.mutate({
              clear_value_bounds: valueMin === "" && valueMax === "",
              value_min: valueMin === "" ? undefined : valueMin,
              value_max: valueMax === "" ? undefined : valueMax,
              client_submit: clientSubmit,
              per_subject_submit_limit: Number(limit) || 100,
              retention_periods: Number(retention) || 0,
              subject_kind: subjectKind,
            });
          }}
        >
          <div className="grid grid-cols-2 gap-3">
            <div className={inputCls}>
              <Label>分数下限</Label>
              <Input value={valueMin} onChange={(e) => setValueMin(e.target.value)} />
            </div>
            <div className={inputCls}>
              <Label>分数上限</Label>
              <Input value={valueMax} onChange={(e) => setValueMax(e.target.value)} />
            </div>
            <div className={inputCls}>
              <Label>每期每主体提交上限</Label>
              <Input type="number" min={1} max={10000} value={limit} onChange={(e) => setLimit(e.target.value)} />
            </div>
            <div className={inputCls}>
              <Label>保留期数（0=永久）</Label>
              <Input type="number" min={0} value={retention} onChange={(e) => setRetention(e.target.value)} />
            </div>
          </div>
          <div className={inputCls}>
            <Label>主体类型</Label>
            <Input value={subjectKind} onChange={(e) => setSubjectKind(e.target.value)} />
          </div>
          <div className="flex items-center gap-2">
            <Checkbox
              id={`cs-${board.id}`}
              checked={clientSubmit}
              onChange={(e) => setClientSubmit(e.target.checked)}
            />
            <Label htmlFor={`cs-${board.id}`}>允许终端用户直接提交</Label>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>取消</Button>
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function BoardDetailPage() {
  const { boardId } = useParams<{ boardId: string }>();
  const queryClient = useQueryClient();
  const { data: board } = useQuery({
    queryKey: ["leaderboards-board", boardId],
    queryFn: () => getBoard(boardId!),
    enabled: !!boardId,
  });
  const { data: periods = [] } = useQuery({
    queryKey: ["leaderboards-periods", boardId],
    queryFn: () => listBoardPeriods(boardId!),
    enabled: !!boardId,
  });
  const [period, setPeriod] = useState<string>("");
  const activePeriod = period || periods[0] || "";
  const { data: top, isLoading: topLoading } = useQuery({
    queryKey: ["leaderboards-top", boardId, activePeriod],
    queryFn: () => listBoardTop(boardId!, { period: activePeriod || undefined, page_size: 100 }),
    enabled: !!boardId,
  });

  const [lookupSubject, setLookupSubject] = useState("");
  const [lookupResult, setLookupResult] = useState<LeaderboardScoreSnapshot | null>(null);
  const lookup = useMutation({
    mutationFn: () => getBoardEntry(boardId!, lookupSubject.trim(), activePeriod || undefined),
    onSuccess: (snap) => setLookupResult(snap),
    onError: () => setLookupResult(null),
  });

  const removeEntry = useMutation({
    mutationFn: (subjectId: string) => deleteBoardEntry(boardId!, activePeriod, subjectId),
    onSuccess: () => {
      toast.success("条目已删除（仅未封榜期可删）");
      queryClient.invalidateQueries({ queryKey: ["leaderboards-top", boardId] });
      queryClient.invalidateQueries({ queryKey: ["leaderboards-periods", boardId] });
    },
  });

  if (!board) {
    return <div className="p-6 text-sm text-muted-foreground">加载中…</div>;
  }

  return (
    <div className="space-y-6 p-6">
      <div className="flex items-center gap-3">
        <Trophy className="h-6 w-6" />
        <div>
          <h1 className="text-xl font-semibold">{board.id}</h1>
          <p className="text-sm text-muted-foreground">
            {board.period_kind === "none" ? "全期" : `${board.period_kind} · ${board.period_tz}`}
            {" · "}{board.sort ?? "desc"} / {board.policy ?? "best"}
            {board.tiebreak_order ? ` · tiebreak:${board.tiebreak_order}` : ""}
            {` · ${board.tie_break ?? "parallel"}`}
            {board.value_min !== undefined || board.value_max !== undefined
              ? ` · 分数域 [${board.value_min ?? "-∞"}, ${board.value_max ?? "+∞"}]`
              : ""}
          </p>
        </div>
      </div>

      <div className="flex items-center gap-3">
        <Label>期</Label>
        <Select value={activePeriod} onValueChange={setPeriod}>
          <SelectTrigger className="w-48"><SelectValue placeholder="选择期" /></SelectTrigger>
          <SelectContent>
            {periods.length === 0 ? <SelectItem value="all">当前期（无历史期条目）</SelectItem> : null}
            {periods.map((p) => <SelectItem key={p} value={p}>{p}</SelectItem>)}
          </SelectContent>
        </Select>
        {top ? (
          <span className="text-sm text-muted-foreground">共 {top.total} 条</span>
        ) : null}
      </div>

      <div className="rounded-md border">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b bg-muted/50 text-left">
              <th className="p-2">rank</th>
              <th className="p-2">position</th>
              <th className="p-2">subject</th>
              <th className="p-2">value</th>
              {board.tiebreak_order ? <th className="p-2">tiebreak</th> : null}
              <th className="p-2">更新时间</th>
              <th className="p-2" />
            </tr>
          </thead>
          <tbody>
            {topLoading ? (
              <tr><td className="p-2 text-muted-foreground" colSpan={7}>加载中…</td></tr>
            ) : (top?.entries ?? []).length === 0 ? (
              <tr><td className="p-2 text-muted-foreground" colSpan={7}>该期暂无条目</td></tr>
            ) : (
              (top?.entries ?? []).map((e) => (
                <tr key={e.subject_id} className="border-b last:border-0">
                  <td className="p-2 font-medium">{e.rank}</td>
                  <td className="p-2 text-muted-foreground">{e.position}</td>
                  <td className="p-2 font-mono text-xs">{e.subject_id}</td>
                  <td className="p-2">{e.value}</td>
                  {board.tiebreak_order ? <td className="p-2">{e.tiebreak_value ?? "—"}</td> : null}
                  <td className="p-2 text-muted-foreground">
                    {e.updated_at ? new Date(e.updated_at).toLocaleString() : "—"}
                  </td>
                  <td className="p-2">
                    <RowDeleteButton
                      onConfirm={() => removeEntry.mutate(e.subject_id)}
                      loading={removeEntry.isPending}
                    />
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      <div className="rounded-md border p-4">
        <div className="flex items-end gap-2">
          <div className={inputCls}>
            <Label>按 subject 查条目</Label>
            <Input
              value={lookupSubject}
              onChange={(e) => setLookupSubject(e.target.value)}
              placeholder="subject_id"
              className="w-72 font-mono text-xs"
            />
          </div>
          <Button
            size="sm"
            disabled={!lookupSubject.trim() || lookup.isPending}
            onClick={() => lookup.mutate()}
          >
            查询
          </Button>
        </div>
        {lookupResult ? (
          lookupResult.entry ? (
            <div className="mt-3 text-sm">
              value = <span className="font-mono">{lookupResult.entry.value}</span>
              {" · "}rank = {lookupResult.rank}
              {" · "}position = {lookupResult.position}
              {" · "}below = {lookupResult.below}
              {" · "}提交 {lookupResult.entry.submit_count ?? 1} 次
            </div>
          ) : (
            <div className="mt-3 text-sm text-muted-foreground">
              该期无此条目（total = {lookupResult.total}）
            </div>
          )
        ) : null}
      </div>
    </div>
  );
}
