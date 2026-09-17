import { Link, useLocation } from "react-router-dom";
import { ArrowLeft, type LucideIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { pageIconElement } from "@/lib/nav";

export interface PageCardProps {
  title: string;
  description?: string;
  icon?: LucideIcon;
  actions?: React.ReactNode;
  backTo?: string;
  backLabel?: string;
  children: React.ReactNode;
  contentClassName?: string;
}

export function PageCard({
  title,
  description,
  icon: Icon,
  actions,
  backTo,
  backLabel = "返回列表",
  children,
  contentClassName,
}: PageCardProps) {
  const { pathname } = useLocation();
  const iconClass = "h-5 w-5 shrink-0 text-muted-foreground";

  return (
    <Card>
      <div className="flex flex-wrap items-center justify-between gap-4 p-6">
        <div className="flex min-w-0 flex-1 items-center gap-3">
          {backTo && (
            <Button variant="ghost" size="icon" asChild className="-ml-2 shrink-0">
              <Link to={backTo} title={backLabel}>
                <ArrowLeft className="h-4 w-4" />
                <span className="sr-only">{backLabel}</span>
              </Link>
            </Button>
          )}
          {Icon ? <Icon className={iconClass} /> : pageIconElement(pathname, iconClass)}
          <div className="min-w-0">
            <h1 className="flex items-center gap-2 truncate text-xl font-semibold tracking-tight">
              {title}
            </h1>
            {description && (
              <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>
            )}
          </div>
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
      <CardContent className={contentClassName ?? "border-t py-8"}>{children}</CardContent>
    </Card>
  );
}
