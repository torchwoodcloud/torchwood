import * as React from "react"
import { CalendarDays, ChevronLeft, ChevronRight } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { cn } from "@/lib/utils"

// 统一日期时间选择器：浏览器原生 <input type="datetime-local"> 的弹层样式与
// locale 跟浏览器走（中文系统渲染中文日历），CSS 管不到，与 Console 其余
// 元素不一致，故自绘替换。值域与原生 input 相同（"yyyy-MM-ddTHH:mm" | ""），
// 时区换算仍由调用方在边界做（lib/datetime 的 from/toDateTimeLocalValue）。
// 月历用原生 Date 数学自绘，不引入日期库；文案硬编码英文（Console 约定）。

interface DateTimeParts {
  year: number
  month: number // 0 起
  day: number
  hour: number
  minute: number
}

// 严格 "yyyy-MM-dd[ T]HH:mm"（含真实日历校验，2 月 30 日不通过）。
function parseLocal(value: string): DateTimeParts | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})$/.exec(value.trim())
  if (!m) return null
  const [year, month, day, hour, minute] = m.slice(1).map(Number)
  if (month < 1 || month > 12 || hour > 23 || minute > 59) return null
  const probe = new Date(year, month - 1, day, hour, minute)
  if (
    probe.getFullYear() !== year ||
    probe.getMonth() !== month - 1 ||
    probe.getDate() !== day
  ) {
    return null
  }
  return { year, month: month - 1, day, hour, minute }
}

function pad(n: number): string {
  return String(n).padStart(2, "0")
}

function composeLocal(p: DateTimeParts): string {
  return `${p.year}-${pad(p.month + 1)}-${pad(p.day)}T${pad(p.hour)}:${pad(p.minute)}`
}

function formatDisplay(p: DateTimeParts): string {
  return `${p.year}-${pad(p.month + 1)}-${pad(p.day)} ${pad(p.hour)}:${pad(p.minute)}`
}

// 6×7 固定网格（周日起始，与 shadcn calendar 默认一致）：当月 1 号所在周的
// 周日起铺 42 格，跨月/跨年格子原样渲染、淡显。
function monthGrid(year: number, month: number): DateTimeParts[] {
  const first = new Date(year, month, 1)
  const start = new Date(first.getFullYear(), first.getMonth(), 1 - first.getDay())
  const cells: DateTimeParts[] = []
  for (let i = 0; i < 42; i++) {
    const d = new Date(start.getFullYear(), start.getMonth(), start.getDate() + i)
    cells.push({
      year: d.getFullYear(),
      month: d.getMonth(),
      day: d.getDate(),
      hour: 0,
      minute: 0,
    })
  }
  return cells
}

const WEEKDAYS = ["Su", "Mo", "Tu", "We", "Th", "Fr", "Sa"]
const MONTHS = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
]

export interface DateTimePickerProps {
  id?: string
  /** "yyyy-MM-ddTHH:mm" 墙钟串（与原生 datetime-local value 同形）；"" 为空 */
  value: string
  onChange: (value: string) => void
  disabled?: boolean
  className?: string
}

export function DateTimePicker({ id, value, onChange, disabled, className }: DateTimePickerProps) {
  const parsed = parseLocal(value)
  const [open, setOpen] = React.useState(false)
  // 视图月与文本草稿都在打开瞬间从 value 派生；draft 为 null 表示非编辑态，
  // 文本框回显 value（非法输入失焦即弹回，不外传）。
  const [view, setView] = React.useState({ year: 1970, month: 0 })
  const [draft, setDraft] = React.useState<string | null>(null)

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (next) {
      const p = parseLocal(value)
      const now = new Date()
      setView(
        p
          ? { year: p.year, month: p.month }
          : { year: now.getFullYear(), month: now.getMonth() }
      )
      setDraft(null)
    }
  }

  const commitDraft = (raw: string) => {
    const p = parseLocal(raw)
    if (p) onChange(composeLocal(p))
  }

  // 点日只改日期、保留已选时间（无则 00:00），与原生 picker 行为一致。
  const pickDay = (year: number, month: number, day: number) => {
    const time = parsed ?? { hour: 0, minute: 0 }
    onChange(composeLocal({ year, month, day, hour: time.hour, minute: time.minute }))
  }

  const setNow = () => {
    const now = new Date()
    onChange(
      composeLocal({
        year: now.getFullYear(),
        month: now.getMonth(),
        day: now.getDate(),
        hour: now.getHours(),
        minute: now.getMinutes(),
      })
    )
  }

  const shown = draft ?? (parsed ? formatDisplay(parsed) : "")
  const today = new Date()
  const cells = open ? monthGrid(view.year, view.month) : []

  return (
    <Popover open={open} onOpenChange={handleOpenChange}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          id={id}
          variant="outline"
          disabled={disabled}
          className={cn(
            "h-10 w-full justify-between px-3 font-normal",
            "focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30",
            !parsed && "text-muted-foreground",
            className
          )}
        >
          {parsed ? formatDisplay(parsed) : "yyyy-MM-dd HH:mm"}
          <CalendarDays className="h-4 w-4 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-[248px] p-3">
        <div className="mb-1 flex items-center justify-between">
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="h-7 w-7"
            aria-label="Previous month"
            onClick={() =>
              setView((v) =>
                v.month === 0 ? { year: v.year - 1, month: 11 } : { year: v.year, month: v.month - 1 }
              )
            }
          >
            <ChevronLeft className="h-4 w-4" />
          </Button>
          <div className="text-sm font-medium">
            {MONTHS[view.month]} {view.year}
          </div>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="h-7 w-7"
            aria-label="Next month"
            onClick={() =>
              setView((v) =>
                v.month === 11 ? { year: v.year + 1, month: 0 } : { year: v.year, month: v.month + 1 }
              )
            }
          >
            <ChevronRight className="h-4 w-4" />
          </Button>
        </div>
        <div className="grid grid-cols-7 text-center">
          {WEEKDAYS.map((w) => (
            <div key={w} className="pb-1 text-xs text-muted-foreground">
              {w}
            </div>
          ))}
          {cells.map((c, i) => {
            const selected =
              parsed != null &&
              parsed.year === c.year &&
              parsed.month === c.month &&
              parsed.day === c.day
            const isToday =
              today.getFullYear() === c.year &&
              today.getMonth() === c.month &&
              today.getDate() === c.day
            const outside = c.month !== view.month
            return (
              <Button
                key={i}
                type="button"
                variant="ghost"
                className={cn(
                  "h-8 w-8 justify-center rounded-md p-0 text-sm font-normal",
                  outside && "text-muted-foreground/50",
                  isToday && !selected && "ring-1 ring-inset ring-ring",
                  selected && "bg-primary text-primary-foreground hover:bg-primary/90"
                )}
                aria-label={`${MONTHS[c.month]} ${c.day}, ${c.year}`}
                aria-pressed={selected}
                onClick={() => pickDay(c.year, c.month, c.day)}
              >
                {c.day}
              </Button>
            )
          })}
        </div>
        <div className="mt-2 flex items-center gap-2 border-t pt-2">
          <Input
            aria-label="Date and time"
            autoFocus
            className="h-8 flex-1 px-2 font-mono text-xs"
            placeholder="yyyy-MM-dd HH:mm"
            value={shown}
            onChange={(e) => {
              setDraft(e.target.value)
              commitDraft(e.target.value)
            }}
            onBlur={() => setDraft(null)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                commitDraft(shown)
                setDraft(null)
              }
            }}
          />
          <Button
            type="button"
            variant="outline"
            className="h-8 px-2 text-xs"
            onClick={setNow}
          >
            Now
          </Button>
          <Button
            type="button"
            variant="ghost"
            className="h-8 px-2 text-xs"
            onClick={() => onChange("")}
          >
            Clear
          </Button>
        </div>
      </PopoverContent>
    </Popover>
  )
}
