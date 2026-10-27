import { useEffect, useState } from "react";
import { useCommand } from "@lynx-go/uikit-core";
import { toast } from "sonner";
import type { FunctionItem } from "@/api/functions";
import { updateFunctionClientPolicyCommand } from "@/kit/functions";
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

// ——客户端调用策略（P2 客户端调用面）——
//
// 函数详情页的「客户端调用」卡片：client_callable 开关 + 每用户限频
// 配额/窗口 + 幂等键说明。client_anonymous_allowed 字段保留但一期禁用
// （服务端遇 true 显式报错），不在表单中提供。
//
// 开启后终端用户可 POST /v1/functions/{id}:invoke 同步调用本函数：
//   - 每用户限频由平台强制（固定窗口计数，超限 429 + RetryInfo）；
//   - 每用户并发闸门（平台默认每用户 2）；
//   - 可选客户端幂等键（网络超时重试防重复执行，调用记录级去重）；
//   - v4：调用经签名 HTTP 送达 endpoint target，执行位置归部署侧。

export function FunctionClientPolicyCard({
  fn,
  writeable,
}: {
  fn: FunctionItem;
  writeable: boolean;
}) {
  const [callable, setCallable] = useState(fn.client_callable);
  const [limit, setLimit] = useState(String(fn.client_per_user_limit || 10));
  const [limitWindow, setLimitWindow] = useState<"minute" | "hour" | "day">(
    (fn.client_limit_window as "minute" | "hour" | "day") || "day"
  );

  useEffect(() => {
    setCallable(fn.client_callable);
    setLimit(String(fn.client_per_user_limit || 10));
    setLimitWindow((fn.client_limit_window as "minute" | "hour" | "day") || "day");
  }, [fn]);

  // 命令接线（§12.3）：client_* 面是 function 实体更新——挂函数资源（kit
  // 集合自动失效；资源挂载句柄），存量 ["functions"] 键由 resource.legacyKeys
  // 桥；配额文本展示形在 descriptor run 内收敛整数。toast 单源 onSettled。
  const save = useCommand(updateFunctionClientPolicyCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Client policy saved");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <Card>
      <CardHeader className="space-y-0 pb-3">
        <CardTitle className="text-sm">Client Invocations</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="space-y-3 max-w-3xl">
          <p className="text-xs text-muted-foreground">
            When enabled, end users (signed in via the Client API) can invoke this function
            synchronously via
            <code className="font-mono"> POST /v1/functions/&#123;id&#125;:invoke</code>. The
            platform enforces per-user rate limiting (429 + RetryInfo when exceeded) and a
            per-user concurrency gate; invocations reach the endpoint target over signed HTTP
            (v4: execution location belongs to the deployment side). data must be a JSON object
            and ≤ 32KB.
          </p>
          <div className="text-xs text-muted-foreground">
            Idempotency: clients may carry an idempotency key (≤128 characters);
            <code className="font-mono">(project, function, user, key)</code>
            deduplicates uniquely — retries after network timeouts return the existing execution
            as-is (running if still in progress). For scenarios that grant assets on invocation,
            such as ad rewards, derive a business idempotency key from execution_id.
          </div>

          <label className="flex items-center gap-2 text-sm">
            <Checkbox
              checked={callable}
              onChange={(e) => setCallable(e.target.checked)}
              disabled={!writeable}
            />
            Allow client invocation (client_callable)
            {fn.client_callable && <Badge variant="secondary">Enabled</Badge>}
          </label>

          <div className="flex items-center gap-3">
            <Label className="w-28">Per-user quota</Label>
            <Input
              type="number"
              className="w-28"
              value={limit}
              onChange={(e) => setLimit(e.target.value)}
              min={1}
              disabled={!writeable || !callable}
            />
            <span className="text-xs text-muted-foreground">invocations / window (must be ≥ 1 when client_callable is enabled)</span>
          </div>

          <div className="flex items-center gap-3">
            <Label className="w-28">Rate limit window</Label>
            <Select
              value={limitWindow}
              onValueChange={(v) => setLimitWindow(v as "minute" | "hour" | "day")}
              disabled={!writeable || !callable}
            >
              <SelectTrigger className="w-56">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="minute">minute (per minute, UTC-aligned)</SelectItem>
                <SelectItem value="hour">hour (per hour, UTC-aligned)</SelectItem>
                <SelectItem value="day">day (per day, UTC date)</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {writeable && (
            <Button
              type="button"
              size="sm"
              disabled={save.pending}
              onClick={() =>
                save.mutate({
                  id: fn.id,
                  client_callable: callable,
                  clientPerUserLimit: limit,
                  client_limit_window: limitWindow,
                })
              }
            >
              {save.pending ? "Saving..." : "Save policy"}
            </Button>
          )}
        </div>
      </CardContent>
    </Card>
  );
}
