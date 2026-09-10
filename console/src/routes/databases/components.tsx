import {
  useState,
  useEffect,
} from "react";
import { toast } from "sonner";
import {
  Plus,
  Hash,
  ListTree,
} from "lucide-react";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { type Attribute, type Collection } from "@/api/databases";
import {
  ATTRIBUTE_TYPES,
  INDEX_TYPES,
  STRING_LIKE_TYPES,
} from "./collectionMeta";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Label } from "@/components/ui/label";
import { Input } from "@/components/ui/input";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  FormField,
  RowDeleteButton,
} from "@/components/resource/shared";
import { PermissionEditor } from "@/components/resource/PermissionEditor";

export function AttributeList({
  attributes,
  onAdd,
  onRemove,
  disabled,
}: {
  attributes: Attribute[];
  onAdd: () => void;
  onRemove?: (attr: Attribute) => void;
  disabled?: boolean;
}) {
  return (
    <Card className="flex flex-col">
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-4">
        <div>
          <CardTitle className="text-base">Attributes</CardTitle>
          <p className="mt-1 text-sm text-muted-foreground">定义文档字段类型与约束</p>
        </div>
        <Button size="sm" onClick={onAdd} disabled={disabled}>
          <Plus className="mr-2 h-4 w-4" />
          添加
        </Button>
      </CardHeader>
      <CardContent className="flex-1">
        {attributes.length === 0 ? (
          <div className="flex flex-col items-center justify-center rounded-lg border border-dashed py-10 text-center">
            <ListTree className="mb-3 h-8 w-8 text-muted-foreground/50" />
            <p className="text-sm text-muted-foreground">暂无字段定义</p>
            {!disabled && (
              <Button size="sm" variant="outline" className="mt-4" onClick={onAdd}>
                添加第一个 Attribute
              </Button>
            )}
          </div>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Key</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>约束</TableHead>
                {onRemove && <TableHead className="w-12" />}
              </TableRow>
            </TableHeader>
            <TableBody>
              {attributes.map((attr) => (
                <TableRow key={attr.id}>
                  <TableCell className="font-mono font-medium">{attr.key}</TableCell>
                  <TableCell>
                    <Badge variant="outline">
                      {attr.type}
                      {attr.array ? "[]" : ""}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap gap-1.5">
                      {attr.required && <Badge variant="secondary">required</Badge>}
                      {attr.array && <Badge variant="secondary">array</Badge>}
                      {attr.size ? <Badge variant="secondary">size {attr.size}</Badge> : null}
                      {!attr.required && !attr.array && !attr.size && (
                        <span className="text-sm text-muted-foreground">—</span>
                      )}
                    </div>
                  </TableCell>
                  {onRemove && (
                    <TableCell className="text-right">
                      <RowDeleteButton onConfirm={() => onRemove(attr)} />
                    </TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

export function IndexList({
  indexes,
  onAdd,
  onRemove,
  canAdd,
  disabled,
}: {
  indexes: Collection["indexes"];
  onAdd: () => void;
  onRemove?: (index: Collection["indexes"][number]) => void;
  canAdd: boolean;
  disabled?: boolean;
}) {
  return (
    <Card className="flex flex-col">
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-4">
        <div>
          <CardTitle className="text-base">Indexes</CardTitle>
          <p className="mt-1 text-sm text-muted-foreground">为查询性能创建索引</p>
        </div>
        <Button size="sm" onClick={onAdd} disabled={!canAdd || disabled}>
          <Plus className="mr-2 h-4 w-4" />
          添加
        </Button>
      </CardHeader>
      <CardContent className="flex-1">
        {!canAdd && !disabled && (
          <p className="mb-4 rounded-md bg-muted/50 px-3 py-2 text-sm text-muted-foreground">
            请先添加至少一个 Attribute，再创建 Index。
          </p>
        )}
        {indexes.length === 0 ? (
          <div className="flex flex-col items-center justify-center rounded-lg border border-dashed py-10 text-center">
            <Hash className="mb-3 h-8 w-8 text-muted-foreground/50" />
            <p className="text-sm text-muted-foreground">
              {disabled ? "暂无索引" : canAdd ? "暂无索引" : "添加 Attribute 后可创建索引"}
            </p>
            {canAdd && !disabled && (
              <Button size="sm" variant="outline" className="mt-4" onClick={onAdd}>
                添加第一个 Index
              </Button>
            )}
          </div>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Attributes</TableHead>
                <TableHead>Type</TableHead>
                {onRemove && <TableHead className="w-12" />}
              </TableRow>
            </TableHeader>
            <TableBody>
              {indexes.map((idx) => (
                <TableRow key={idx.id}>
                  <TableCell className="font-mono text-xs">{idx.id}</TableCell>
                  <TableCell className="font-mono text-sm">{idx.attributes.join(", ")}</TableCell>
                  <TableCell>
                    <Badge variant="outline">{idx.type}</Badge>
                  </TableCell>
                  {onRemove && (
                    <TableCell className="text-right">
                      <RowDeleteButton onConfirm={() => onRemove(idx)} />
                    </TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

export function AddAttributeDialog({
  open,
  onOpenChange,
  loading,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  loading: boolean;
  onSubmit: (input: {
    key: string;
    type: string;
    size?: number;
    required: boolean;
    array: boolean;
  }) => void;
}) {
  const [key, setKey] = useState("");
  const [type, setType] = useState("string");
  const [size, setSize] = useState("");
  const [required, setRequired] = useState(false);
  const [array, setArray] = useState(false);

  const reset = () => {
    setKey("");
    setType("string");
    setSize("");
    setRequired(false);
    setArray(false);
  };

  const handleOpenChange = (next: boolean) => {
    if (!next) reset();
    onOpenChange(next);
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>添加 Attribute</DialogTitle>
          <DialogDescription>为 Collection 定义字段类型与约束。</DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            onSubmit({
              key: key.trim(),
              type,
              size: size ? Number(size) : undefined,
              required,
              array,
            });
          }}
        >
          <div className="space-y-2">
            <Label htmlFor="attr-key">Key</Label>
            <Input
              id="attr-key"
              value={key}
              onChange={(e) => setKey(e.target.value)}
              placeholder="title"
              required
            />
          </div>
          <div className="space-y-2">
            <Label>Type</Label>
            <Select value={type} onValueChange={setType}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {ATTRIBUTE_TYPES.map((item) => (
                  <SelectItem key={item.value} value={item.value}>
                    {item.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {STRING_LIKE_TYPES.has(type) && (
            <div className="space-y-2">
              <Label htmlFor="attr-size">Size（可选）</Label>
              <Input
                id="attr-size"
                type="number"
                min={1}
                value={size}
                onChange={(e) => setSize(e.target.value)}
                placeholder="256"
              />
            </div>
          )}
          <div className="flex flex-wrap gap-6">
            <label className="flex items-center gap-2 text-sm">
              <Checkbox checked={required} onChange={(e) => setRequired(e.target.checked)} />
              Required
            </label>
            <label className="flex items-center gap-2 text-sm">
              <Checkbox checked={array} onChange={(e) => setArray(e.target.checked)} />
              Array
            </label>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => handleOpenChange(false)}>
              取消
            </Button>
            <Button type="submit" disabled={loading || !key.trim()}>
              {loading ? "添加中..." : "添加"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function AddIndexDialog({
  open,
  onOpenChange,
  loading,
  attributes,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  loading: boolean;
  attributes: Attribute[];
  onSubmit: (input: { id: string; type: string; attributes: string[] }) => void;
}) {
  const [id, setId] = useState("");
  const [type, setType] = useState("key");
  const [selected, setSelected] = useState<string[]>([]);

  const reset = () => {
    setId("");
    setType("key");
    setSelected([]);
  };

  const handleOpenChange = (next: boolean) => {
    if (!next) reset();
    onOpenChange(next);
  };

  const toggleAttribute = (key: string) => {
    setSelected((prev) =>
      prev.includes(key) ? prev.filter((item) => item !== key) : [...prev, key]
    );
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>添加 Index</DialogTitle>
          <DialogDescription>选择索引类型，并指定参与索引的 Attribute。</DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            onSubmit({ id: id.trim(), type, attributes: selected });
          }}
        >
          <div className="space-y-2">
            <Label htmlFor="index-id">ID</Label>
            <Input
              id="index-id"
              value={id}
              onChange={(e) => setId(e.target.value)}
              placeholder="title_key"
              required
            />
          </div>
          <div className="space-y-2">
            <Label>Type</Label>
            <Select value={type} onValueChange={setType}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {INDEX_TYPES.map((item) => (
                  <SelectItem key={item.value} value={item.value}>
                    {item.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label>Attributes</Label>
            <div className="rounded-md border divide-y max-h-48 overflow-y-auto">
              {attributes.map((attr) => (
                <label
                  key={attr.key}
                  className="flex items-center gap-2 px-3 py-2 text-sm cursor-pointer hover:bg-muted/50"
                >
                  <Checkbox
                    checked={selected.includes(attr.key)}
                    onChange={() => toggleAttribute(attr.key)}
                  />
                  <span className="font-mono">{attr.key}</span>
                  <Badge variant="outline" className="ml-auto">
                    {attr.type}
                  </Badge>
                </label>
              ))}
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => handleOpenChange(false)}>
              取消
            </Button>
            <Button type="submit" disabled={loading || !id.trim() || selected.length === 0}>
              {loading ? "添加中..." : "添加"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function EditPermissionsDialog({
  open,
  onOpenChange,
  loading,
  initialPermissions,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  loading: boolean;
  initialPermissions: string[];
  onSubmit: (permissions: string[]) => void;
}) {
  const [permissions, setPermissions] = useState<string[]>([]);

  useEffect(() => {
    if (open) {
      setPermissions(initialPermissions);
    }
  }, [open, initialPermissions]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>编辑 Collection 权限</DialogTitle>
          <DialogDescription>
            修改集合级权限规则。无文档级权限的文档将回退到此规则。
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            onSubmit(permissions);
          }}
        >
          <PermissionEditor permissions={permissions} onChange={setPermissions} />
          <DialogFooter className="mt-4">
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              取消
            </Button>
            <Button type="submit" disabled={loading}>
              {loading ? "保存中..." : "保存"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function EditCollectionDialog({
  open,
  onOpenChange,
  loading,
  initialName,
  initialDisabled,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  loading: boolean;
  initialName: string;
  initialDisabled: boolean;
  onSubmit: (input: { name?: string; disabled?: boolean }) => void;
}) {
  const [name, setName] = useState("");
  const [disabled, setDisabled] = useState(false);

  useEffect(() => {
    if (open) {
      setName(initialName);
      setDisabled(initialDisabled);
    }
  }, [open, initialName, initialDisabled]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>编辑集合设置</DialogTitle>
          <DialogDescription>
            修改集合名称或停用/启用集合。停用后客户端 API 将拒绝读写该集合。
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (!name.trim()) {
              toast.error("名称不能为空");
              return;
            }
            onSubmit({ name: name.trim(), disabled });
          }}
          className="space-y-4"
        >
          <FormField id="name" label="名称" value={name} onChange={setName} placeholder="posts" />
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={disabled} onChange={(e) => setDisabled(e.target.checked)} />
            停用集合
          </label>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              取消
            </Button>
            <Button type="submit" disabled={loading}>
              {loading ? "保存中..." : "保存"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

