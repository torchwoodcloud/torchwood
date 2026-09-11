import { Link } from "react-router-dom";
import { useState } from "react";
import { FormPage, DetailPage } from "@/components/FormPage";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { Trash2 } from "lucide-react";

export function FormPageWrapper({
  title,
  description,
  backTo,
  backLabel,
  onSubmit,
  loading,
  submitLabel = "保存",
  submitDisabled,
  formClassName = "space-y-4 max-w-lg",
  children,
}: {
  title: string;
  description?: string;
  backTo: string;
  backLabel?: string;
  onSubmit: (e: React.FormEvent) => void;
  loading?: boolean;
  submitLabel?: string;
  submitDisabled?: boolean;
  formClassName?: string;
  children: React.ReactNode;
}) {
  return (
    <FormPage title={title} description={description} backTo={backTo} backLabel={backLabel}>
      <Card>
        <CardContent className="pt-6">
          <form onSubmit={onSubmit} className={formClassName}>
            {children}
            <Button type="submit" disabled={loading || submitDisabled}>
              {loading ? "保存中..." : submitLabel}
            </Button>
          </form>
        </CardContent>
      </Card>
    </FormPage>
  );
}

export function FormField({
  id,
  label,
  value,
  onChange,
  required,
  placeholder,
  type = "text",
  hint,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (v: string) => void;
  required?: boolean;
  placeholder?: string;
  type?: string;
  hint?: string;
}) {
  return (
    <div className="space-y-2">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        type={type}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        required={required}
        placeholder={placeholder}
      />
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

export function DetailPageWrapper({
  title,
  description,
  backTo,
  backLabel,
  actions,
  children,
}: {
  title: string;
  description?: string;
  backTo: string;
  backLabel?: string;
  actions?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <DetailPage title={title} description={description} backTo={backTo} backLabel={backLabel} actions={actions}>
      {children}
    </DetailPage>
  );
}

export function DetailGrid({
  items,
}: {
  items: { label: string; value: React.ReactNode; mono?: boolean }[];
}) {
  return (
    <Card>
      <CardContent className="pt-6">
        <dl className="grid gap-4 sm:grid-cols-2">
          {items.map((item) => (
            <div key={item.label}>
              <dt className="text-sm text-muted-foreground">{item.label}</dt>
              <dd className={`mt-1 font-medium ${item.mono ? "font-mono text-sm break-all" : ""}`}>
                {item.value}
              </dd>
            </div>
          ))}
        </dl>
      </CardContent>
    </Card>
  );
}

export function DetailSkeleton() {
  return (
    <div className="space-y-4">
      <Skeleton className="h-8 w-48" />
      <Skeleton className="h-40 w-full" />
    </div>
  );
}

export function NotFound({ backTo }: { backTo: string }) {
  return (
    <div className="text-center py-12">
      <p className="text-muted-foreground mb-4">资源不存在或已被删除</p>
      <Button asChild variant="outline">
        <Link to={backTo}>返回列表</Link>
      </Button>
    </div>
  );
}

export function DeleteButton({
  onConfirm,
  loading,
  label = "删除",
  description = "此操作不可撤销，确定要删除吗？",
}: {
  onConfirm: () => void;
  loading?: boolean;
  label?: string;
  description?: string;
}) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button variant="destructive" size="sm" onClick={() => setOpen(true)}>
        <Trash2 className="h-4 w-4 mr-2" />
        {label}
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="确认删除"
        description={description}
        confirmLabel="删除"
        loading={loading}
        onConfirm={() => {
          onConfirm();
          setOpen(false);
        }}
      />
    </>
  );
}

export function BulkDeleteButton({
  count,
  onConfirm,
  loading,
}: {
  count: number;
  onConfirm: () => void;
  loading?: boolean;
}) {
  const [open, setOpen] = useState(false);
  if (count === 0) return null;
  return (
    <>
      <Button variant="destructive" size="sm" onClick={() => setOpen(true)}>
        <Trash2 className="h-4 w-4 mr-2" />
        删除 ({count})
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="确认批量删除"
        description={`确定要删除选中的 ${count} 项吗？此操作不可撤销。`}
        confirmLabel="删除"
        loading={loading}
        onConfirm={() => {
          onConfirm();
          setOpen(false);
        }}
      />
    </>
  );
}

export function RowDeleteButton({
  onConfirm,
  loading,
  disabled,
}: {
  onConfirm: () => void;
  loading?: boolean;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button variant="ghost" size="icon" onClick={() => setOpen(true)} title="删除" disabled={disabled}>
        <Trash2 className="h-4 w-4 text-destructive" />
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="确认删除"
        description="此操作不可撤销，确定要删除吗？"
        confirmLabel="删除"
        loading={loading}
        onConfirm={() => {
          onConfirm();
          setOpen(false);
        }}
      />
    </>
  );
}
