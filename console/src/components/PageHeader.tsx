import { Link, useLocation } from "react-router-dom";
import { ChevronRight, Home } from "lucide-react";

const routeNames: Record<string, string> = {
  "": "Dashboard",
  projects: "Projects",
  "api-keys": "API Keys",
  users: "Users",
  storage: "Storage",
  databases: "Databases",
  functions: "Functions",
  analytics: "Analytics",
  events: "Events",
  retention: "Retention",
  settings: "Settings",
  new: "新建",
  edit: "编辑",
  collections: "Collections",
  documents: "文档",
  files: "Files",
};

function segmentLabel(segment: string, prevSegment?: string): string {
  if (routeNames[segment]) return routeNames[segment];
  if (prevSegment && segment.length > 8) return segment.slice(0, 8) + "…";
  return segment;
}

export function PageHeader({ title, description, actions }: { title: React.ReactNode; description?: string; actions?: React.ReactNode }) {
  const location = useLocation();
  const segments = location.pathname.replace("/console", "").split("/").filter(Boolean);

  return (
    <div className="mb-8 space-y-2">
      <nav className="flex items-center gap-2 text-sm text-muted-foreground flex-wrap">
        <Link to="/console" className="flex items-center gap-1 hover:text-foreground">
          <Home className="h-3.5 w-3.5" />
          <span className="sr-only">Dashboard</span>
        </Link>
        {segments.map((segment, idx) => {
          const path = "/console/" + segments.slice(0, idx + 1).join("/");
          const isLast = idx === segments.length - 1;
          const prev = idx > 0 ? segments[idx - 1] : undefined;
          const label = segmentLabel(segment, prev);
          return (
            <div key={path} className="flex items-center gap-2">
              <ChevronRight className="h-3.5 w-3.5" />
              {isLast ? (
                <span className="text-foreground font-medium">{label}</span>
              ) : (
                <Link to={path} className="hover:text-foreground">
                  {label}
                </Link>
              )}
            </div>
          );
        })}
      </nav>
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">{title}</h1>
          {description && <p className="text-muted-foreground">{description}</p>}
        </div>
        {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
      </div>
    </div>
  );
}
