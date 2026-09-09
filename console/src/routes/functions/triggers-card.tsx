import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus } from "lucide-react";
import {
  listFunctionTriggers,
  createFunctionTrigger,
  deleteFunctionTrigger,
  rotateFunctionTriggerToken,
  type CreateTriggerInput,
  type FunctionTrigger,
} from "@/api/functions";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { RowDeleteButton } from "@/components/resource/shared";

// ——触发器管理（P1 触发器模块）——
//
// 函数详情页的「触发器」卡片：列表 / 创建（http 双响应模式与 cron 表单）/
// 删除 / 轮换 token。http 触发器的公开调用路径 /f/{project}/{token} 仅在
// 管理面可见（token 即鉴权），疑似泄漏时轮换使其立即失效。

function triggerBytes(bytes: number): string {
  if (bytes >= 1024) return `${Math.round(bytes / 1024)}KB`;
  return `${bytes}B`;
}

export function FunctionTriggersCard({
  functionId,
  writeable,
}: {
  functionId: string;
  writeable: boolean;
}) {
  const queryClient = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);
  const [type, setType] = useState<"http" | "cron">("http");
  // http 表单
  const [responseMode, setResponseMode] = useState<"sync" | "async_ack">("async_ack");
  const [ackBody, setAckBody] = useState('{"is_valid":true}');
  const [handshake, setHandshake] = useState(true);
  const [bodyLimitKB, setBodyLimitKB] = useState("64");
  // cron 表单
  const [expr, setExpr] = useState("0 3 * * *");
  const [misfire, setMisfire] = useState<"skip" | "catch_up_once">("catch_up_once");

  const queryKey = ["function-triggers", functionId];

  const { data: triggers = [], isLoading } = useQuery({
    queryKey,
    queryFn: () => listFunctionTriggers(functionId),
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey });

  const create = useMutation({
    mutationFn: () => {
      const bodyLimit = Number.parseInt(bodyLimitKB, 10);
      const input: CreateTriggerInput =
        type === "http"
          ? {
              type,
              http: {
                response_mode: responseMode,
                ...(responseMode === "async_ack" && ackBody.trim() !== ""
                  ? { ack_body: ackBody }
                  : {}),
                ...(handshake ? { handshake: "echo" } : {}),
                ...(Number.isFinite(bodyLimit) && bodyLimit > 0
                  ? { body_limit_bytes: bodyLimit * 1024 }
                  : {}),
              },
            }
          : {
              type,
              cron: { expr, misfire },
            };
      return createFunctionTrigger(functionId, input);
    },
    onSuccess: () => {
      toast.success("触发器已创建");
      setShowCreate(false);
      invalidate();
    },
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteFunctionTrigger(functionId, id),
    onSuccess: () => {
      toast.success("触发器已删除");
      invalidate();
    },
  });

  const rotate = useMutation({
    mutationFn: (id: string) => rotateFunctionTriggerToken(functionId, id),
    onSuccess: () => {
      toast.success("token 已轮换（旧 URL 立即失效）");
      invalidate();
    },
  });

  return (
    <Card>
      <CardHeader className="space-y-0 pb-3 flex flex-row items-center justify-between">
        <CardTitle className="text-sm">触发器</CardTitle>
        {writeable && (
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => setShowCreate((v) => !v)}
          >
            <Plus className="size-4 mr-1" />
            新建触发器
          </Button>
        )}
      </CardHeader>
      <CardContent>
        <div className="space-y-3">
          <p className="text-xs text-muted-foreground">
            HTTP 触发器：公开 URL
            <code className="font-mono"> /f/&#123;project&#125;/&#123;token&#125;</code>
            ，平台只路由不验签（封套透传 method/path/raw_query/headers/body），
            验签归函数代码；微信 SSV 等回调建议 async_ack 模式（200 是「已受理」语义）。
            cron 触发器按 UTC 解析 5 字段表达式。
          </p>

          {showCreate && writeable && (
            <div className="rounded-md border p-3 space-y-3">
              <div className="flex items-center gap-3">
                <Label className="w-16">类型</Label>
                <Select value={type} onValueChange={(v) => setType(v as "http" | "cron")}>
                  <SelectTrigger className="w-40">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="http">HTTP 回调</SelectItem>
                    <SelectItem value="cron">定时（cron）</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              {type === "http" ? (
                <>
                  <div className="flex items-center gap-3">
                    <Label className="w-16">响应模式</Label>
                    <Select
                      value={responseMode}
                      onValueChange={(v) => setResponseMode(v as "sync" | "async_ack")}
                    >
                      <SelectTrigger className="w-72">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="async_ack">
                          async_ack（立即 200 + ack_body，回调场景推荐）
                        </SelectItem>
                        <SelectItem value="sync">sync（同步执行，透传响应）</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  {responseMode === "async_ack" && (
                    <div className="flex items-center gap-3">
                      <Label className="w-16">ack_body</Label>
                      <Input
                        className="font-mono text-xs"
                        value={ackBody}
                        onChange={(e) => setAckBody(e.target.value)}
                        placeholder={'{"is_valid":true}'}
                        maxLength={1024}
                      />
                    </div>
                  )}
                  <label className="flex items-center gap-2 text-sm">
                    <Checkbox
                      checked={handshake}
                      onChange={(e) => setHandshake(e.target.checked)}
                    />
                    GET 握手回显（微信服务器 URL 验证：echostr 原样返回，不执行函数）
                  </label>
                  <div className="flex items-center gap-3">
                    <Label className="w-16">body 上限</Label>
                    <Input
                      type="number"
                      className="w-28"
                      value={bodyLimitKB}
                      onChange={(e) => setBodyLimitKB(e.target.value)}
                      min={1}
                      max={1024}
                    />
                    <span className="text-xs text-muted-foreground">KB（上限 1024 = 1MB）</span>
                  </div>
                </>
              ) : (
                <>
                  <div className="flex items-center gap-3">
                    <Label className="w-16">表达式</Label>
                    <Input
                      className="w-72 font-mono text-xs"
                      value={expr}
                      onChange={(e) => setExpr(e.target.value)}
                      placeholder="0 3 * * *"
                    />
                    <span className="text-xs text-muted-foreground">
                      分 时 日 月 周（UTC）
                    </span>
                  </div>
                  <div className="flex items-center gap-3">
                    <Label className="w-16">错过策略</Label>
                    <Select
                      value={misfire}
                      onValueChange={(v) => setMisfire(v as "skip" | "catch_up_once")}
                    >
                      <SelectTrigger className="w-72">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="catch_up_once">
                          catch_up_once（宕机恢复后补跑一次，推荐）
                        </SelectItem>
                        <SelectItem value="skip">skip（错过不补跑）</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                </>
              )}
              <Button
                type="button"
                size="sm"
                disabled={create.isPending}
                onClick={() => create.mutate()}
              >
                {create.isPending ? "创建中..." : "创建"}
              </Button>
            </div>
          )}

          {isLoading ? (
            <p className="text-sm text-muted-foreground">加载中...</p>
          ) : triggers.length === 0 ? (
            <p className="text-sm text-muted-foreground">暂无触发器</p>
          ) : (
            <div className="space-y-2">
              {triggers.map((trg: FunctionTrigger) => (
                <div
                  key={trg.id}
                  className="rounded-md border p-3 space-y-2 text-sm"
                >
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex items-center gap-2">
                      <Badge variant="secondary">{trg.type}</Badge>
                      {trg.enabled ? (
                        <Badge variant="secondary">启用</Badge>
                      ) : (
                        <Badge variant="destructive">禁用</Badge>
                      )}
                      <span className="font-mono text-xs text-muted-foreground">
                        {trg.id}
                      </span>
                    </div>
                    {writeable && (
                      <div className="flex items-center gap-1">
                        {trg.type === "http" && (
                          <Button
                            type="button"
                            size="sm"
                            variant="ghost"
                            disabled={rotate.isPending}
                            onClick={() => rotate.mutate(trg.id)}
                          >
                            轮换 token
                          </Button>
                        )}
                        <RowDeleteButton
                          onConfirm={() => remove.mutate(trg.id)}
                          loading={remove.isPending}
                        />
                      </div>
                    )}
                  </div>
                  {trg.type === "http" ? (
                    <>
                      <div className="text-xs text-muted-foreground">
                        模式：{trg.response_mode}
                        {trg.response_mode === "async_ack" && trg.ack_body
                          ? ` · ack_body=${trg.ack_body}`
                          : ""}
                        {trg.handshake ? ` · 握手：${trg.handshake}` : ""}
                        {` · body ≤ ${triggerBytes(trg.body_limit_bytes ?? 65536)}`}
                      </div>
                      {trg.invoke_path && (
                        <div className="font-mono text-xs break-all">
                          {trg.invoke_path}
                        </div>
                      )}
                    </>
                  ) : (
                    <div className="text-xs text-muted-foreground">
                      <span className="font-mono">{trg.expr}</span> · {trg.misfire}
                      {trg.next_run_at
                        ? ` · 下次 ${new Date(trg.next_run_at).toLocaleString()}`
                        : ""}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      </CardContent>
    </Card>
  );
}
