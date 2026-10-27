export const PERMISSION_PRESETS: Record<string, string[]> = {
  "Public read": ["read:any"],
  "Authenticated read-write": ["read:users", "create:users", "update:users", "delete:users"],
  "API Key only": ["read:keys", "create:keys", "update:keys", "delete:keys"],
  "Group read": ["read:group:<id>"],
};
