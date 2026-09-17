import { useState } from "react";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { X, Plus } from "lucide-react";

const PERM_TYPES = ["read", "create", "update", "delete"] as const;
const ROLE_PRESETS = [
  "any",
  "users",
  "keys",
  "admin",
] as const;

export function PermissionEditor({
  permissions,
  onChange,
}: {
  permissions: string[];
  onChange: (permissions: string[]) => void;
}) {
  const [permType, setPermType] = useState<string>("read");
  const [role, setRole] = useState<string>("");

  const addPermission = () => {
    const r = role.trim();
    if (!r) return;
    const entry = `${permType}:${r}`;
    if (!permissions.includes(entry)) {
      onChange([...permissions, entry]);
    }
    setRole("");
  };

  const removePermission = (entry: string) => {
    onChange(permissions.filter((p) => p !== entry));
  };

  const addPresetRole = (preset: string) => {
    const entry = `${permType}:${preset}`;
    if (!permissions.includes(entry)) {
      onChange([...permissions, entry]);
    }
  };

  return (
    <div className="space-y-3">
      <Label>权限规则 (Permissions)</Label>
      <p className="text-sm text-muted-foreground">
        格式 <code className="text-xs">操作类型:角色</code>，例如 <code className="text-xs">read:any</code>、<code className="text-xs">update:user:&lt;id&gt;</code>、<code className="text-xs">delete:group:&lt;id&gt;</code>
      </p>

      {permissions.length > 0 && (
        <div className="flex flex-wrap gap-2">
          {permissions.map((p) => (
            <Badge key={p} variant="secondary" className="gap-1">
              <span className="font-mono text-xs">{p}</span>
              <button
                type="button"
                onClick={() => removePermission(p)}
                className="ml-1 rounded-full hover:bg-muted-foreground/20 p-0.5"
              >
                <X className="h-3 w-3" />
              </button>
            </Badge>
          ))}
        </div>
      )}

      <div className="flex items-end gap-2">
        <div className="space-y-1">
          <Label className="text-xs">操作</Label>
          <select
            value={permType}
            onChange={(e) => setPermType(e.target.value)}
            className="h-9 rounded-lg border border-input bg-background px-3 text-sm"
          >
            {PERM_TYPES.map((t) => (
              <option key={t} value={t}>{t}</option>
            ))}
          </select>
        </div>
        <div className="flex-1 space-y-1">
          <Label className="text-xs">角色</Label>
          <Input
            value={role}
            onChange={(e) => setRole(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                addPermission();
              }
            }}
            placeholder="any / users / keys / admin / user:<id> / group:<id>"
          />
        </div>
        <Button type="button" size="sm" onClick={addPermission}>
          <Plus className="h-4 w-4 mr-1" />
          添加
        </Button>
      </div>

      <div className="flex flex-wrap gap-1.5">
        {ROLE_PRESETS.map((preset) => (
          <Button
            key={preset}
            type="button"
            variant="outline"
            size="sm"
            className="h-7 text-xs"
            onClick={() => addPresetRole(preset)}
          >
            + {permType}:{preset}
          </Button>
        ))}
      </div>
    </div>
  );
}
