import { Link, NavLink } from "react-router-dom";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

// Analytics 区共享小组件（仅组件导出，满足 react-refresh 约束）。

// SourceBadge 口径徽标（D8）：响应级 source（rollup | raw）诚实标注。
// raw 时期（rollup worker 未上线或覆盖缺失）明示"实时扫描口径"。
export function SourceBadge({ source }: { source?: string }) {
  if (source === "rollup") {
    return (
      <Badge
        variant="secondary"
        title="rollup 口径：数据聚合作业（小时级刷新）的预聚合结果"
      >
        rollup 预聚合
      </Badge>
    );
  }
  if (source === "raw") {
    return (
      <Badge
        variant="outline"
        title="raw 口径：实时扫描原始事件（预聚合作业未上线或该窗口无覆盖）"
      >
        raw 实时扫描
      </Badge>
    );
  }
  return null;
}

// SourceNote 图表旁的口径说明行：raw 时给出"实时扫描口径"提示。
export function SourceNote({ source }: { source?: string }) {
  if (source !== "raw") return null;
  return (
    <p className="text-xs text-muted-foreground">
      实时扫描口径（原始事件直查；数据聚合作业上线后此窗口将切换为 rollup 预聚合）。
    </p>
  );
}

// AnalyticsTabs 分析区二级导航（概览 / 事件 / 留存）。
const tabs = [
  { to: "/console/analytics", label: "概览", end: true },
  { to: "/console/analytics/events", label: "事件", end: false },
  { to: "/console/analytics/retention", label: "留存", end: false },
];

export function AnalyticsTabs() {
  return (
    <div className="mb-4 flex gap-1 border-b">
      {tabs.map((tab) => (
        <NavLink
          key={tab.to}
          to={tab.to}
          end={tab.end}
          className={({ isActive }) =>
            cn(
              "px-3 py-2 text-sm font-medium border-b-2 -mb-px transition-colors",
              isActive
                ? "border-primary text-foreground"
                : "border-transparent text-muted-foreground hover:text-foreground"
            )
          }
        >
          {tab.label}
        </NavLink>
      ))}
    </div>
  );
}

// EventLink 事件名链接（进事件详情页）。
export function EventLink({ name }: { name: string }) {
  return (
    <Link
      to={`/console/analytics/events/${encodeURIComponent(name)}`}
      className="font-mono text-sm hover:underline"
    >
      {name}
    </Link>
  );
}
