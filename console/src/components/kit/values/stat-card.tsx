// vendored from fleet-ui registry/values/stat-card.tsx @0.1.1（2026-10-25 KPI 换轨）
// 拷贝适配（design.md §12.1）：token 方言映射 text-fg→text-foreground、
// text-muted→text-muted-foreground；tone 的 ok/warn 色本仓无 token，映射到
// 仓库既有 amber/emerald 惯例（§12.6 warn→amber 同裁决）。其余与上游一致。
import type { ReactNode } from "react";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { cn } from "@/lib/utils";

/**
 * StatCard（values 块）——仪表盘统计卡：label + value + 附注。
 * 值的语义色由页面给 tone；pending 态传 "…" 值即可。
 */
export interface StatCardProps {
  label: ReactNode;
  /** 统计值（pending 传 "…"；格式化由页面完成——千分位/单位都是页面词汇） */
  value: ReactNode;
  tone?: "default" | "ok" | "warn" | "err";
  /** 附注行 */
  hint?: ReactNode;
  className?: string;
}

const TONE_CLASS: Record<NonNullable<StatCardProps["tone"]>, string> = {
  default: "text-foreground",
  ok: "text-emerald-600 dark:text-emerald-400",
  warn: "text-amber-700 dark:text-amber-400",
  err: "text-destructive",
};

export function StatCard({ label, value, tone = "default", hint, className }: StatCardProps) {
  return (
    <Card className={className} data-testid="stat-card">
      <CardHeader>{label}</CardHeader>
      <CardContent className="space-y-0.5">
        <div className={cn("text-2xl font-semibold tabular-nums", TONE_CLASS[tone])}>{value}</div>
        {hint ? <div className="text-xs text-muted-foreground">{hint}</div> : null}
      </CardContent>
    </Card>
  );
}
