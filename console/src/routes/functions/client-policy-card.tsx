import { useEffect, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  updateFunction,
  type FunctionItem,
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

// ——客户端调用策略（P2 客户端调用面）——
//
// 函数详情页的「客户端调用」卡片：client_callable 开关 + 每用户限频
// 配额/窗口 + 幂等键说明。client_anonymous_allowed 字段保留但一期禁用
// （服务端遇 true 显式报错），不在表单中提供。
//
// 开启后终端用户可 POST /v1/functions/{id}:invoke 同步调用本函数：
//   - 每用户限频由平台强制（固定窗口计数，超限 429 + RetryInfo）；
//   - 每用户并发闸门（平台默认每用户 2）；
//   - 可选客户端幂等键（网络超时重试防重复执行，执行记录级去重）；
//   - 函数容器进入 internal 隔离网络（出网全 deny）——开启 client_callable
//     或存在 http/cron 触发器的函数都不可出网。

export function FunctionClientPolicyCard({
  fn,
  writeable,
}: {
  fn: FunctionItem;
  writeable: boolean;
}) {
  const queryClient = useQueryClient();
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

  const save = useMutation({
    mutationFn: () => {
      const parsedLimit = Number.parseInt(limit, 10);
      return updateFunction(fn.id, {
        client_callable: callable,
        // client_callable=true 要求 limit >= 1（服务端校验同规则）。
        client_per_user_limit: Number.isFinite(parsedLimit) && parsedLimit >= 0 ? parsedLimit : 0,
        client_limit_window: limitWindow,
      });
    },
    onSuccess: () => {
      toast.success("客户端调用策略已保存");
      queryClient.invalidateQueries({ queryKey: ["functions"] });
    },
  });

  return (
    <Card>
      <CardHeader className="space-y-0 pb-3">
        <CardTitle className="text-sm">客户端调用</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="space-y-3 max-w-3xl">
          <p className="text-xs text-muted-foreground">
            开启后终端用户（Client API 登录态）可
            <code className="font-mono"> POST /v1/functions/&#123;id&#125;:invoke</code>
            同步调用本函数。平台强制每用户限频（超限 429 + RetryInfo）与每用户
            并发闸门；函数容器进入 internal 隔离网络（出网全 deny，平台回访走
            TW_API_BASE_URL）。data 为 JSON object 且 ≤ 32KB。
          </p>
          <div className="text-xs text-muted-foreground">
            幂等：客户端可携带幂等键（≤128 字符），
            <code className="font-mono">(project, function, user, key)</code>
            唯一去重——网络超时重试返回既有执行原样（进行中返回 running），
            广告奖励等「调用即发资产」场景请配合以 execution_id 派生业务幂等键。
          </div>

          <label className="flex items-center gap-2 text-sm">
            <Checkbox
              checked={callable}
              onChange={(e) => setCallable(e.target.checked)}
              disabled={!writeable}
            />
            允许客户端调用（client_callable）
            {fn.client_callable && <Badge variant="secondary">已开启</Badge>}
          </label>

          <div className="flex items-center gap-3">
            <Label className="w-28">每用户配额</Label>
            <Input
              type="number"
              className="w-28"
              value={limit}
              onChange={(e) => setLimit(e.target.value)}
              min={1}
              disabled={!writeable || !callable}
            />
            <span className="text-xs text-muted-foreground">次 / 窗口（开启 client_callable 时必须 ≥ 1）</span>
          </div>

          <div className="flex items-center gap-3">
            <Label className="w-28">限频窗口</Label>
            <Select
              value={limitWindow}
              onValueChange={(v) => setLimitWindow(v as "minute" | "hour" | "day")}
              disabled={!writeable || !callable}
            >
              <SelectTrigger className="w-56">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="minute">minute（每分钟，UTC 对齐）</SelectItem>
                <SelectItem value="hour">hour（每小时，UTC 对齐）</SelectItem>
                <SelectItem value="day">day（每日，UTC 日期）</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {writeable && (
            <Button
              type="button"
              size="sm"
              disabled={save.isPending}
              onClick={() => save.mutate()}
            >
              {save.isPending ? "保存中..." : "保存策略"}
            </Button>
          )}
        </div>
      </CardContent>
    </Card>
  );
}
