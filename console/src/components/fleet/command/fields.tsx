// vendored from fleet-ui registry/command/fields.tsx @0.1.1（2026-10-25 命令族
// 切片）。拷贝适配（design.md §12.1）：token 方言映射 text-muted→
// text-muted-foreground、text-err→text-destructive、border-line/bg-panel/
// outline-accent→torchwood 输入类；TextField 补 required 客户端校验（本地
// 扩展，上游 PR 候选）；其余与上游一致。
//
// AnyFormApi 的 any 泛型是上游形（校验器泛型从简、字段名保留类型）——
// vendored 块保留上游写法。
/* eslint-disable @typescript-eslint/no-explicit-any */
import type { ReactFormExtendedApi } from "@tanstack/react-form";
import type { ReactNode } from "react";
import { Input } from "@/components/ui/input";

/**
 * TextField / SelectField（command 块）—— TanStack Form 的字段接线。
 * 字段级错误（含 onSubmit 返回的 fieldErrors 映射）经 meta.errors 渲染。
 */

function FieldErrors({ errors }: { errors: ReadonlyArray<unknown> }) {
  const messages = [
    ...new Set(
      errors
        .map((error) =>
          typeof error === "string"
            ? error
            : error && typeof error === "object" && "message" in error
              ? String((error as { message: unknown }).message)
              : null,
        )
        .filter((message): message is string => message !== null),
    ),
  ];
  if (messages.length === 0) return null;
  return (
    <div className="space-y-0.5" role="alert">
      {messages.map((message) => (
        <p key={message} className="text-[11px] text-destructive">
          {message}
        </p>
      ))}
    </div>
  );
}

/** 宽化的 form api 类型（保留字段名类型，校验器泛型从简） */
type AnyFormApi<T extends object> = ReactFormExtendedApi<
  T,
  any,
  any,
  any,
  any,
  any,
  any,
  any,
  any,
  any,
  any,
  any
>;

interface FieldShellProps {
  label: string;
  children: ReactNode;
}

function requiredMessage({ value }: { value: unknown }): string | undefined {
  return value === undefined || value === null || String(value).trim() === "" ? "必填" : undefined;
}

export function FieldShell({ label, children }: FieldShellProps) {
  return (
    <label className="block space-y-1">
      <span className="text-xs font-medium text-muted-foreground">{label}</span>
      {children}
    </label>
  );
}

export interface TextFieldProps<T extends object> {
  form: AnyFormApi<T>;
  name: string & keyof T;
  label: string;
  placeholder?: string;
  type?: "text" | "password" | "email";
  /** 本地扩展（上游 @0.1.1 无）：必填客户端校验（onChange，空串/纯空白拒绝）。 */
  required?: boolean;
}

export function TextField<T extends object>({
  form,
  name,
  label,
  placeholder,
  type = "text",
  required = false,
}: TextFieldProps<T>) {
  return (
    <FieldShell label={label}>
      <form.Field
        name={name}
        validators={
          required
            ? {
                // onChange 即时反馈 + onSubmit 兜住"从未输入直接提交"
                onChange: requiredMessage,
                onSubmit: requiredMessage,
              }
            : undefined
        }
      >
        {(field: {
          state: { value: unknown; meta: { errors: ReadonlyArray<unknown> } };
          handleChange(value: unknown): void;
        }) => (
          <>
            <Input
              type={type}
              placeholder={placeholder}
              value={String(field.state.value ?? "")}
              onChange={(e) => field.handleChange(e.target.value)}
            />
            <FieldErrors errors={field.state.meta.errors} />
          </>
        )}
      </form.Field>
    </FieldShell>
  );
}

export interface SelectFieldProps<T extends object> {
  form: AnyFormApi<T>;
  name: string & keyof T;
  label: string;
  options: ReadonlyArray<{ value: string; label: string }>;
}

export function SelectField<T extends object>({ form, name, label, options }: SelectFieldProps<T>) {
  return (
    <FieldShell label={label}>
      <form.Field name={name}>
        {(field: {
          state: { value: unknown; meta: { errors: ReadonlyArray<unknown> } };
          handleChange(value: unknown): void;
        }) => (
          <>
            <select
              className="flex h-10 w-full rounded-lg border border-input bg-background px-3 py-2 text-sm transition-colors focus-visible:border-ring focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
              value={String(field.state.value ?? "")}
              onChange={(e) => field.handleChange(e.target.value)}
            >
              {options.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </select>
            <FieldErrors errors={field.state.meta.errors} />
          </>
        )}
      </form.Field>
    </FieldShell>
  );
}
