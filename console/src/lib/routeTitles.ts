const routeNames: Record<string, string> = {
  "": "Dashboard",
  projects: "Projects",
  "api-keys": "API Keys",
  users: "Users",
  groups: "Groups",
  storage: "Storage",
  databases: "Databases",
  functions: "Functions",
  webhooks: "Webhooks",
  analytics: "Analytics",
  events: "Events",
  retention: "Retention",
  admins: "Admins",
  account: "Account",
  profile: "Profile",
  preferences: "Preferences",
  "audit-logs": "Audit Logs",
  leaderboards: "Leaderboards",
  orders: "Orders",
  assets: "Assets",
  subscriptions: "Subscriptions",
  plans: "Plans",
  settings: "Settings",
  new: "New",
  edit: "Edit",
  collections: "Collections",
  documents: "Documents",
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
