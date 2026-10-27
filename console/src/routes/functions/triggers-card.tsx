import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useCommand } from "@lynx-go/uikit-core";
import { toast } from "sonner";
import { AlertTriangle, Plus } from "lucide-react";
import { listFunctionTriggers, type FunctionTrigger } from "@/api/functions";
import {
  createTriggerDescriptor,
  removeTriggerDescriptor,
  rotateTriggerTokenDescriptor,
} from "@/kit/functions";
import { describeApiError } from "@/kit/errors";
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
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";

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
  const tz = useUserTimezone();
  const [showCreate, setShowCreate] = useState(false);
  const [type, setType] = useState<"http" | "cron" | "event">("http");
  // http 表单
  const [responseMode, setResponseMode] = useState<"sync" | "async_ack">("async_ack");
  const [ackBody, setAckBody] = useState('{"is_valid":true}');
  const [handshake, setHandshake] = useState(true);
  const [bodyLimitKB, setBodyLimitKB] = useState("64");
  // cron 表单
  const [expr, setExpr] = useState("0 3 * * *");
  const [misfire, setMisfire] = useState<"skip" | "catch_up_once">("catch_up_once");
  // event 表单（v3 切片 D）：订阅串多行输入，一行一条。
  const [eventsText, setEventsText] = useState(
    "databases.app.collections.*.documents.create"
  );

  const queryKey = ["function-triggers", functionId];

  const { data: triggers = [], isLoading } = useQuery({
    queryKey,
    queryFn: () => listFunctionTriggers(functionId),
  });

  // 命令接线（§12.3，裸 descriptor——触发器是 function 子资源）：不涉
  // functions 集合失效；存量 ["function-triggers"] 键由 descriptor 级
  // legacyKeys 桥（§12.10-②）。创建的表单展示形在 descriptor run 内收敛
  // 为提交形。toast 单源 onSettled。
  const create = useCommand(createTriggerDescriptor, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Trigger created");
        setShowCreate(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const remove = useCommand(removeTriggerDescriptor, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Trigger deleted");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const rotate = useCommand(rotateTriggerTokenDescriptor, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Token rotated (the old URL is invalidated immediately)");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <Card>
      <CardHeader className="space-y-0 pb-3 flex flex-row items-center justify-between">
        <CardTitle className="text-sm">Triggers</CardTitle>
        {writeable && (
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => setShowCreate((v) => !v)}
          >
            <Plus className="size-4 mr-1" />
            New Trigger
          </Button>
        )}
      </CardHeader>
      <CardContent>
        <div className="space-y-3">
          <p className="text-xs text-muted-foreground">
            HTTP trigger: public URL
            <code className="font-mono"> /f/&#123;project&#125;/&#123;token&#125;</code>; the
            platform only routes and does not verify signatures (the envelope passes through
            method/path/raw_query/headers/body), signature verification belongs to the function
            code. Callbacks such as WeChat SSV should use the async_ack mode (200 means
            "accepted"). Cron triggers parse 5-field expressions in UTC.
          </p>

          {showCreate && writeable && (
            <div className="rounded-md border p-3 space-y-3">
              <div className="flex items-center gap-3">
                <Label className="w-16">Type</Label>
                <Select
                  value={type}
                  onValueChange={(v) => setType(v as "http" | "cron" | "event")}
                >
                  <SelectTrigger className="w-40">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="http">HTTP callback</SelectItem>
                    <SelectItem value="cron">Scheduled (cron)</SelectItem>
                    <SelectItem value="event">Event (document / system behavior)</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              {type === "http" ? (
                <>
                  <div className="flex items-center gap-3">
                    <Label className="w-16">Response mode</Label>
                    <Select
                      value={responseMode}
                      onValueChange={(v) => setResponseMode(v as "sync" | "async_ack")}
                    >
                      <SelectTrigger className="w-72">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="async_ack">
                          async_ack (immediate 200 + ack_body, recommended for callbacks)
                        </SelectItem>
                        <SelectItem value="sync">sync (synchronous execution, response passed through)</SelectItem>
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
                    GET handshake echo (WeChat server URL verification: echostr is returned as-is,
                    the function is not executed)
                  </label>
                  <div className="flex items-center gap-3">
                    <Label className="w-16">Body limit</Label>
                    <Input
                      type="number"
                      className="w-28"
                      value={bodyLimitKB}
                      onChange={(e) => setBodyLimitKB(e.target.value)}
                      min={1}
                      max={1024}
                    />
                    <span className="text-xs text-muted-foreground">KB (limit 1024 = 1MB)</span>
                  </div>
                </>
              ) : type === "cron" ? (
                <>
                  <div className="flex items-center gap-3">
                    <Label className="w-16">Expression</Label>
                    <Input
                      className="w-72 font-mono text-xs"
                      value={expr}
                      onChange={(e) => setExpr(e.target.value)}
                      placeholder="0 3 * * *"
                    />
                    <span className="text-xs text-muted-foreground">
                      minute hour day month weekday (UTC)
                    </span>
                  </div>
                  <div className="flex items-center gap-3">
                    <Label className="w-16">Misfire policy</Label>
                    <Select
                      value={misfire}
                      onValueChange={(v) => setMisfire(v as "skip" | "catch_up_once")}
                    >
                      <SelectTrigger className="w-72">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="catch_up_once">
                          catch_up_once (run once after downtime recovery, recommended)
                        </SelectItem>
                        <SelectItem value="skip">skip (do not run missed schedules)</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                </>
              ) : (
                <>
                  <div className="space-y-1">
                    <Label className="w-16">Subscribed events</Label>
                    <textarea
                      className="flex min-h-20 w-full rounded-lg border border-input bg-background px-3 py-2 font-mono text-xs text-sm transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-50"
                      value={eventsText}
                      onChange={(e) => setEventsText(e.target.value)}
                      placeholder={
                        "databases.app.collections.notes.documents.create\ndatabases.app.collections.*.documents.*"
                      }
                      spellCheck={false}
                    />
                    <p className="text-xs text-muted-foreground">
                      One per line, two formats: document write events
                      <code className="font-mono">
                        {" "}
                        databases.&#123;database&#125;.collections.&#123;collection&#125;.documents.&#123;create|update|delete|*&#125;
                      </code>
                      (the collection and op segments may be <code className="font-mono">*</code>;
                      the database segment must be exact in phase 1); system behavior events
                      <code className="font-mono"> auth.users.* / payments.orders.* </code>
                      etc. (domains = auth/payments/economy/subscriptions, the trailing segment may
                      be <code className="font-mono">*</code>; a misspelled domain is rejected
                      outright). Document event data carries only a summary; read back the full
                      document by document_id with databases:read; system event data carries the
                      sanitized payload (attrs) directly.
                    </p>
                  </div>
                  {/* 自环警告（v3 D13：平台一期不做硬防护，Console 编辑处警告） */}
                  <div className="flex items-start gap-2 rounded-md border border-amber-500/50 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
                    <AlertTriangle className="size-4 mt-0.5 shrink-0" />
                    <span>
                      Do not subscribe to collections written by this function itself — the write →
                      event → re-trigger loop amplifies without bound (the platform has no hard
                      protection; only the
                      <code className="font-mono"> invoke_total&#123;source=event&#125;</code>
                      rate alert acts as a backstop). Also watch for cycles when chaining multiple
                      functions.
                    </span>
                  </div>
                </>
              )}
              <Button
                type="button"
                size="sm"
                disabled={create.pending}
                onClick={() =>
                  create.mutate({
                    functionId,
                    type,
                    responseMode,
                    ackBody,
                    handshake,
                    bodyLimitKB,
                    expr,
                    misfire,
                    eventsText,
                  })
                }
              >
                {create.pending ? "Creating..." : "Create"}
              </Button>
            </div>
          )}

          {isLoading ? (
            <p className="text-sm text-muted-foreground">Loading...</p>
          ) : triggers.length === 0 ? (
            <p className="text-sm text-muted-foreground">No triggers yet</p>
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
                        <Badge variant="secondary">Enabled</Badge>
                      ) : (
                        <Badge variant="destructive">Disabled</Badge>
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
                            disabled={rotate.pending}
                            onClick={() =>
                              rotate.mutate({ functionId, triggerId: trg.id })
                            }
                          >
                            Rotate token
                          </Button>
                        )}
                        <RowDeleteButton
                          onConfirm={() =>
                            remove.mutate({ functionId, triggerId: trg.id })
                          }
                          loading={remove.pending}
                        />
                      </div>
                    )}
                  </div>
                  {trg.type === "http" ? (
                    <>
                      <div className="text-xs text-muted-foreground">
                        Mode: {trg.response_mode}
                        {trg.response_mode === "async_ack" && trg.ack_body
                          ? ` · ack_body=${trg.ack_body}`
                          : ""}
                        {trg.handshake ? ` · handshake: ${trg.handshake}` : ""}
                        {` · body ≤ ${triggerBytes(trg.body_limit_bytes ?? 65536)}`}
                      </div>
                      {trg.invoke_path && (
                        <div className="font-mono text-xs break-all">
                          {trg.invoke_path}
                        </div>
                      )}
                    </>
                  ) : trg.type === "cron" ? (
                    <div className="text-xs text-muted-foreground">
                      <span className="font-mono">{trg.expr}</span> · {trg.misfire}
                      {trg.next_run_at
                        ? ` · next run ${formatDateTime(trg.next_run_at, tz)}`
                        : ""}
                    </div>
                  ) : (
                    <div className="space-y-1">
                      {(trg.events ?? []).map((ev) => (
                        <div key={ev} className="font-mono text-xs break-all">
                          {ev}
                        </div>
                      ))}
                      <p className="text-xs text-muted-foreground">
                        Event trigger: delivery is at-least-once; deduplicate inside the function
                        using event_id/seq in data. Watch for self-loops (do not subscribe to
                        collections this function writes to).
                      </p>
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
