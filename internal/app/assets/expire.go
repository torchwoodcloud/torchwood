package assets

import (
	"context"
	"time"

	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
)

// ExpireDue 扫描到期持有：产 expire 流水并删行（worker 周期任务）。
// 每项目一批短事务 + SKIP LOCKED；多项目轮转与全局预算在 app。
func (a *Assets) ExpireDue(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		now = a.ts()
	}
	if a.projects == nil {
		return 0, nil
	}
	all, err := a.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	n := len(all)
	start := a.scanCursor.Start(n)
	remaining := expireBatch
	var expired int64
	stopped := -1
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if remaining <= 0 {
			stopped = idx
			break
		}
		if all[idx].Status != "active" {
			continue
		}
		c, err := a.expireProject(ctx, all[idx].ID, now, remaining)
		if err != nil {
			a.logger.Error("expire holdings failed", "project_id", all[idx].ID, "error", err)
			continue
		}
		expired += c
		remaining -= int(c)
	}
	if stopped >= 0 {
		a.scanCursor.ResumeAt(stopped)
	} else {
		a.scanCursor.Complete()
	}
	return expired, nil
}

func (a *Assets) expireProject(ctx context.Context, projectID string, now time.Time, limit int) (int64, error) {
	ctx = withSystemPrincipal(ctx, projectID)
	if err := requireAssetWrite(ctx); err != nil {
		return 0, err
	}
	// 扫描键是循环项目；sticky Service principal 的 ProjectID 不能复用。
	scope := domainassets.Scope{ProjectID: projectID, Operator: operatorFrom(ctx)}
	return a.svc.ExpireDue(ctx, scope, now, limit)
}
