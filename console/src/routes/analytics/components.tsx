import { Link } from "@tanstack/react-router";
import { NavLink } from "@/router/nav-link";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

// Analytics 区共享小组件（仅组件导出，满足 react-refresh 约束）。

// SourceBadge 口径徽标（D8）：响应级 source（rollup | raw）诚实标注。
// raw（HOUR 粒度恒 raw，或窗口超出 rollup 覆盖时的回退）明示"实时扫描口径"。
export function SourceBadge({ source }: { source?: string }) {
  if (source === "rollup") {
    return (
      <Badge
        variant="secondary"
        title="rollup source: pre-aggregated results from the hourly aggregation job"
      >
        rollup pre-aggregated
      </Badge>
    );
  }
  if (source === "raw") {
    return (
      <Badge
        variant="outline"
        title="raw source: realtime scan over raw events (HOUR granularity is always raw; DAY windows fall back to raw beyond rollup coverage)"
      >
        raw realtime scan
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
      Realtime scan source (direct query over raw events; HOUR granularity is always raw, and DAY windows also fall back to raw beyond rollup coverage).
    </p>
  );
}

// AnalyticsTabs 分析区二级导航（概览 / 事件 / 留存）。
const tabs = [
  { to: "/console/analytics", label: "Overview", end: true },
  { to: "/console/analytics/events", label: "Events", end: false },
  { to: "/console/analytics/retention", label: "Retention", end: false },
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
      to="/console/analytics/events/$name" params={{ name: encodeURIComponent(name) }}
      className="font-mono text-sm hover:underline"
    >
      {name}
    </Link>
  );
}
