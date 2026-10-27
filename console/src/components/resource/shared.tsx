import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { PageCard } from "@/components/PageCard";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { SearchX, Trash2 } from "lucide-react";

export function FormPageWrapper({
  title,
  description,
  backTo,
  backLabel,
  onSubmit,
  loading,
  submitLabel = "Save",
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
    <PageCard title={title} description={description} backTo={backTo} backLabel={backLabel}>
      <form onSubmit={onSubmit} className={formClassName}>
        {children}
        <Button type="submit" disabled={loading || submitDisabled}>
          {loading ? "Saving…" : submitLabel}
        </Button>
      </form>
    </PageCard>
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
    <PageCard
      title={title}
      description={description}
      backTo={backTo}
      backLabel={backLabel}
      actions={actions}
      contentClassName="border-t py-8 space-y-6"
    >
      {children}
    </PageCard>
  );
}

export function DetailGrid({
  items,
}: {
  items: { label: string; value: React.ReactNode; mono?: boolean }[];
}) {
  return (
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
  );
}

export function DetailSkeleton() {
  return (
    <div className="space-y-4 rounded-xl bg-card p-6 ring-1 ring-foreground/10">
      <Skeleton className="h-6 w-48" />
      <Skeleton className="h-40 w-full" />
    </div>
  );
}

export function NotFound({ backTo }: { backTo: string }) {
  return (
    <div className="flex min-h-[40vh] w-full flex-col items-center justify-center gap-4 rounded-lg border border-dashed p-8">
      <div className="rounded-full bg-muted p-4">
        <SearchX className="h-8 w-8 text-muted-foreground" />
      </div>
      <p className="text-sm font-medium">Resource not found or has been deleted</p>
      <Button asChild variant="outline">
        <Link to={backTo}>Back to list</Link>
      </Button>
    </div>
  );
}

export function DeleteButton({
  onConfirm,
  loading,
  label = "Delete",
  description = "Are you sure you want to delete? This action cannot be undone.",
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
        title="Confirm deletion"
        description={description}
        confirmLabel="Delete"
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
        Delete ({count})
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="Confirm bulk deletion"
        description={`Are you sure you want to delete the selected ${count} item(s)? This action cannot be undone.`}
        confirmLabel="Delete"
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
      <Button variant="ghost" size="icon" onClick={() => setOpen(true)} title="Delete" disabled={disabled}>
        <Trash2 className="h-4 w-4 text-destructive" />
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="Confirm deletion"
        description="Are you sure you want to delete? This action cannot be undone."
        confirmLabel="Delete"
        loading={loading}
        onConfirm={() => {
          onConfirm();
          setOpen(false);
        }}
      />
    </>
  );
}
