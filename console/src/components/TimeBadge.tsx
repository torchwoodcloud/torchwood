import { useEffect, useState } from "react";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatTimeOnly } from "@/lib/datetime";

function utcOffsetLabel(timeZone: string, date: Date): string {
  try {
    const parts = new Intl.DateTimeFormat("en-US", {
      timeZone,
      timeZoneName: "longOffset",
    }).formatToParts(date);
    const name = parts.find((p) => p.type === "timeZoneName")?.value ?? "GMT";
    if (name === "GMT" || name === "UTC") return "UTC+00:00";
    return name.replace("GMT", "UTC");
  } catch {
    return "UTC";
  }
}

export function TimeBadge() {
  const timeZone = useUserTimezone();
  const [now, setNow] = useState(() => new Date());

  useEffect(() => {
    const timer = setInterval(() => setNow(new Date()), 1000);
    return () => clearInterval(timer);
  }, []);

  return (
    <div className="inline-flex max-w-full items-center gap-1 overflow-hidden whitespace-nowrap rounded-full border p-1 text-xs">
      <div className="inline-flex items-center gap-1.5 px-1.5">
        <span className="hidden text-muted-foreground sm:inline">Local time:</span>
        <span className="font-medium tabular-nums">{formatTimeOnly(now, timeZone)}</span>
      </div>
      <span className="hidden rounded-full bg-foreground/5 px-1.5 py-0.5 text-muted-foreground sm:inline">
        {timeZone} | {utcOffsetLabel(timeZone, now)}
      </span>
    </div>
  );
}
