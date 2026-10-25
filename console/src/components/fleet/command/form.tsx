// vendored from fleet-ui registry/command/form.tsx @0.1.1（2026-10-25 命令族切片）
// 拷贝适配（design.md §12.1）：token 方言映射 text-err→text-destructive；
// 按钮文案中文化（submitLabel/submitting 由调用方传）；
// 本地扩展 resultAction 结果盒动作槽（OCC 冲突的「刷新重载」——B2 批，上游
// 候选）。其余与上游一致。
import type { CommandResult } from "@lynx-go/ui-kit/kernel";
import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";

/**
 * Form 内芯（command 块）—— 字段族 × 错误盒 × 提交生命周期；宿主无关
 * （Dialog/Sheet/整页由外壳决定，submit 由内芯渲染，宿主 footer 经 onCancel 协作）。
 * 结果分支渲染在此：rejected/conflict 展示 message-first 错误盒（结果通道，非异常）。
 */
export interface FormProps<Payload> {
  /**
   * 结构化：任何带 handleSubmit 的 form api（useCommandForm / 裸 TanStack Form 皆可）。
   * 注意：TanStack Form 1.33 的 handleSubmit(submitMeta?) 不接收事件、不 preventDefault
   * ——本块统一在此拦截原生提交（直连 onSubmit={form.handleSubmit} 会导致整页原生 GET 提交）。
   */
  form: { handleSubmit: (submitMeta?: unknown) => void | Promise<void> };
  submitting?: boolean;
  result?: CommandResult<Payload> | null;
  /** 结果盒内的动作槽（conflict 的「刷新重载」等页面编排；本地扩展） */
  resultAction?: ReactNode;
  submitLabel?: string;
  onCancel?: () => void;
  children: ReactNode;
}

export function Form<Payload>({
  form,
  submitting = false,
  result = null,
  resultAction,
  submitLabel = "保存",
  onCancel,
  children,
}: FormProps<Payload>) {
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        void form.handleSubmit();
      }}
      className="space-y-3"
      noValidate
    >
      {children}
      {result !== null && result.status !== "ok" ? (
        <div
          role="alert"
          className="flex items-center justify-between gap-3 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs text-destructive"
        >
          <span>
            {result.status === "rejected" && result.code ? (
              <span className="font-mono">[{result.code}] </span>
            ) : null}
            {result.message}
          </span>
          {resultAction}
        </div>
      ) : null}
      <div className="flex justify-end gap-2 pt-1">
        {onCancel ? (
          <Button type="button" variant="ghost" onClick={onCancel}>
            取消
          </Button>
        ) : null}
        <Button type="submit" disabled={submitting}>
          {submitting ? "保存中…" : submitLabel}
        </Button>
      </div>
    </form>
  );
}
