const routeNames: Record<string, string> = {
  "": "Dashboard",
  projects: "Projects",
  "api-keys": "API Keys",
  users: "Users",
  groups: "Groups",
  storage: "Storage",
  databases: "Databases",
  functions: "Functions",
  analytics: "Analytics",
  events: "Events",
  retention: "Retention",
  admins: "Admins",
  "audit-logs": "Audit Logs",
  leaderboards: "Leaderboards",
  orders: "Orders",
  assets: "Assets",
  subscriptions: "Subscriptions",
  plans: "Plans",
  settings: "Settings",
  new: "新建",
  edit: "编辑",
  collections: "Collections",
  documents: "文档",
  files: "Files",
  listen: "Listen",
};

export interface Crumb {
  label: string;
  to: string;
}

function segmentLabel(segment: string, prevSegment?: string): string {
  if (routeNames[segment]) return routeNames[segment];
  if (prevSegment && segment.length > 20) return segment.slice(0, 20) + "…";
  return segment;
}

export function breadcrumbsFor(pathname: string): Crumb[] {
  const segments = pathname.replace("/console", "").split("/").filter(Boolean);
  if (segments.length === 0) {
    return [{ label: routeNames[""], to: "/console" }];
  }
  return segments.map((segment, idx) => ({
    label: segmentLabel(segment, idx > 0 ? segments[idx - 1] : undefined),
    to: "/console/" + segments.slice(0, idx + 1).join("/"),
  }));
}
