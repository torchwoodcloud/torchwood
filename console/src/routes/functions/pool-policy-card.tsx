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
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

// ——池策略（v3 实例池与多路复用，docs/design/functions-v3.md §5/OQ2）——
//
// 函数详情页的「池策略」卡片：五列数字输入（min_instances / max_instances /
// idle_ttl_seconds / max_requests_per_instance / concurrency）。全部走
// PATCH UpdateFunction（proto3 optional——未修改的字段不随请求发送）。
// 值域与 DB CHECK 同源（迁移 000014/000017）：min≥0、max≥1、idle_ttl≥30、
// max_requests≥1、concurrency 1..16；min ≤ max 跨字段校验在服务端 app 层。
//
// concurrency > 1 要求函数代码可重入（单实例单事件循环内多请求交错）；
// v3 之前构建的部署自动按 concurrency=1 降级执行（D7），调大后新实例生效。

interface PoolFields {
  min_instances: string;
  max_instances: string;
  idle_ttl_seconds: string;
  max_requests_per_instance: string;
  concurrency: string;
}

export function FunctionPoolPolicyCard({
  fn,
  writeable,
}: {
  fn: FunctionItem;
  writeable: boolean;
}) {
  const queryClient = useQueryClient();
  const [fields, setFields] = useState<PoolFields>({
    min_instances: String(fn.min_instances),
    max_instances: String(fn.max_instances),
    idle_ttl_seconds: String(fn.idle_ttl_seconds),
    max_requests_per_instance: String(fn.max_requests_per_instance),
    concurrency: String(fn.concurrency),
  });

  useEffect(() => {
    setFields({
      min_instances: String(fn.min_instances),
      max_instances: String(fn.max_instances),
      idle_ttl_seconds: String(fn.idle_ttl_seconds),
      max_requests_per_instance: String(fn.max_requests_per_instance),
      concurrency: String(fn.concurrency),
    });
  }, [fn]);

  const setField = (key: keyof PoolFields, value: string) =>
    setFields((prev) => ({ ...prev, [key]: value }));

  const save = useMutation({
    mutationFn: () => {
      // 逐字段解析：非法/越界前端先拦（服务端 protovalidate + app 层兜底
      // 同规则）；跨字段 min ≤ max 交给服务端终值比较。
      const parsed = {
        min_instances: Number.parseInt(fields.min_instances, 10),
        max_instances: Number.parseInt(fields.max_instances, 10),
        idle_ttl_seconds: Number.parseInt(fields.idle_ttl_seconds, 10),
        max_requests_per_instance: Number.parseInt(
          fields.max_requests_per_instance,
          10
        ),
        concurrency: Number.parseInt(fields.concurrency, 10),
      };
      if (
        !Number.isFinite(parsed.min_instances) ||
        parsed.min_instances < 0
      ) {
        return Promise.reject(new Error("min_instances 需 ≥ 0"));
      }
      if (
        !Number.isFinite(parsed.max_instances) ||
        parsed.max_instances < 1
      ) {
        return Promise.reject(new Error("max_instances 需 ≥ 1"));
      }
      if (
        parsed.min_instances > parsed.max_instances
      ) {
        return Promise.reject(
          new Error("min_instances 不能大于 max_instances")
        );
      }
      if (
        !Number.isFinite(parsed.idle_ttl_seconds) ||
        parsed.idle_ttl_seconds < 30
      ) {
        return Promise.reject(new Error("idle_ttl_seconds 需 ≥ 30"));
      }
      if (
        !Number.isFinite(parsed.max_requests_per_instance) ||
        parsed.max_requests_per_instance < 1
      ) {
        return Promise.reject(
          new Error("max_requests_per_instance 需 ≥ 1")
        );
      }
      if (
        !Number.isFinite(parsed.concurrency) ||
        parsed.concurrency < 1 ||
        parsed.concurrency > 16
      ) {
        return Promise.reject(new Error("concurrency 需在 1..16 之间"));
      }
      return updateFunction(fn.id, parsed);
    },
    onSuccess: () => {
      toast.success("池策略已保存");
      queryClient.invalidateQueries({ queryKey: ["functions"] });
    },
    onError: (err: Error) => {
      toast.error(err.message);
    },
  });

  const numberRow = (
    key: keyof PoolFields,
    label: string,
    hint: string,
    min: number,
    max?: number
  ) => (
    <div className="flex items-center gap-3">
      <Label className="w-44" htmlFor={`pool-${key}`}>
        {label}
      </Label>
      <Input
        id={`pool-${key}`}
        type="number"
        className="w-28"
        value={fields[key]}
        onChange={(e) => setField(key, e.target.value)}
        min={min}
        max={max}
        disabled={!writeable}
      />
      <span className="text-xs text-muted-foreground">{hint}</span>
    </div>
  );

  return (
    <Card>
      <CardHeader className="space-y-0 pb-3">
        <CardTitle className="text-sm">池策略</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="space-y-3 max-w-3xl">
          <p className="text-xs text-muted-foreground">
            常驻实例池参数（改动对新实例即时生效，存量实例按旧值服务至回收）：
            min/max_instances 控制保温与突发上限，idle_ttl_seconds 控制空闲回收，
            max_requests_per_instance 控制实例请求数到期排空替换（防内存泄漏）。
          </p>

          {numberRow("min_instances", "最小实例数", "保温下限（0 = 纯 scale-from-zero）", 0)}
          {numberRow("max_instances", "最大实例数", "突发并发上限（超限有界排队）", 1)}
          {numberRow("idle_ttl_seconds", "空闲回收阈值（秒）", "实例数 > min 时空闲超时回收，≥ 30", 30)}
          {numberRow("max_requests_per_instance", "实例最大请求数", "到期排空替换（防内存泄漏），≥ 1", 1)}
          {numberRow("concurrency", "单实例并发上限", "1..16", 1, 16)}

          <p className="text-xs text-muted-foreground">
            concurrency &gt; 1 要求函数可重入（同一实例事件循环内多请求交错，模块级状态共享）。
            模板 v3 起生效，旧部署自动按 1 执行（降级可观测，存量实例按旧值服务至回收）。
          </p>

          {writeable && (
            <Button
              type="button"
              size="sm"
              disabled={save.isPending}
              onClick={() => save.mutate()}
            >
              {save.isPending ? "保存中..." : "保存池策略"}
            </Button>
          )}
        </div>
      </CardContent>
    </Card>
  );
}
