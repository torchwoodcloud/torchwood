// 零依赖 JSON 编辑框：透明 <textarea> 叠加在高亮 <pre> 之上（文字透明、光标可见），
// 高度由 <pre> 内容撑起（min/max 约束），超过 max 后 textarea 内部滚动并同步回 <pre>。
import { useRef } from "react";
import { cn } from "@/lib/utils";

const escapeHtml = (s: string) =>
  s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

// 依次匹配：对象的 key（冒号 lookahead）| 字符串值 | true/false/null | 数字
// 输入已先做 HTML 转义，引号/反斜杠不受影响；外层用非捕获组，保证 replace 回调为 (match, offset, string)。
const TOKEN_RE =
  /(?:"(?:\\.|[^\\"])*"(?=\s*:)|"(?:\\.|[^\\"])*"|\b(?:true|false|null)\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g;

function highlightJson(code: string): string {
  return escapeHtml(code).replace(TOKEN_RE, (m, offset: number, whole: string) => {
    const cls = m.startsWith('"')
      ? /^\s*:/.test(whole.slice(offset + m.length))
        ? "text-sky-600"
        : "text-emerald-600"
      : /^(?:true|false|null)$/.test(m)
        ? "text-purple-600"
        : "text-amber-600";
    return `<span class="${cls}">${m}</span>`;
  });
}

export function JsonEditor({
  id,
  value,
  onChange,
  required,
  disabled,
  className,
  minHeightClass = "min-h-[320px]",
  maxHeightClass = "max-h-[65vh]",
}: {
  id?: string;
  value: string;
  onChange: (v: string) => void;
  required?: boolean;
  disabled?: boolean;
  className?: string;
  minHeightClass?: string;
  maxHeightClass?: string;
}) {
  const preRef = useRef<HTMLPreElement>(null);
  let invalid = false;
  if (value.trim() !== "") {
    try {
      JSON.parse(value);
    } catch {
      invalid = true;
    }
  }

  return (
    <div className={className}>
      <div
        className={cn(
          "relative overflow-hidden rounded-md border border-input bg-background focus-within:ring-2 focus-within:ring-ring focus-within:ring-offset-2",
          invalid && "border-destructive focus-within:ring-destructive"
        )}
      >
        <pre
          ref={preRef}
          aria-hidden
          className={cn(
            "pointer-events-none m-0 overflow-hidden whitespace-pre-wrap break-words px-3 py-2 font-mono text-sm leading-5 [scrollbar-gutter:stable]",
            minHeightClass,
            maxHeightClass
          )}
          dangerouslySetInnerHTML={{ __html: highlightJson(value) + "\n" }}
        />
        <textarea
          id={id}
          value={value}
          required={required}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
          onScroll={(e) => {
            if (preRef.current) preRef.current.scrollTop = e.currentTarget.scrollTop;
          }}
          spellCheck={false}
          className={cn(
            "absolute inset-0 h-full w-full resize-none overflow-y-auto whitespace-pre-wrap break-words bg-transparent px-3 py-2 font-mono text-sm leading-5 text-transparent caret-zinc-900 outline-none [scrollbar-gutter:stable] disabled:cursor-not-allowed disabled:opacity-50",
            maxHeightClass
          )}
        />
      </div>
      {invalid ? <p className="mt-2 text-xs text-destructive">JSON 格式无效</p> : null}
    </div>
  );
}
