package functions

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 触发器管理（P1 触发器模块，设计 §3）：Create/List/Delete/RotateToken 走
// Server 面（镜像 SetFunctionScopes 的 method_auth 先例）；公开调用
// （/f/{project}/{token}）与 cron 领取经 InvokeTrigger / DispatchDueCronTriggers。
// token 门禁在 handler（不可猜即鉴权），InvokeTrigger 不再要求 principal——
// 执行记录 trigger_source/source_ip 即审计载体（该路由不经 gRPC 拦截器链）。

// InvokeTriggerCommand 是触发器调用命令；Source 形如 http:{trigger_id} /
// cron:{trigger_id}（执行记录 trigger_source 列）。
type InvokeTriggerCommand struct {
	ProjectID  string
	FunctionID string
	Data       string
	Async      bool
	Source     string
	SourceIP   string
	// BodyLimitBytes 是本次调用的 data 上限（HTTP 触发器：封套含透传 body，
	// 上限随执行器模式放宽——v1 env 通道硬上限 32KB，v2 dispatcher body
	// 通道 ≤1MB；cron 触发器封套恒小于 1KB，传 0 用默认）。
	BodyLimitBytes int
}

// CreateTriggerCommand 创建触发器命令。
type CreateTriggerCommand struct {
	ProjectID  string
	FunctionID string
	Type       string // http | cron
	HTTP       domainfunctions.TriggerConfig
	Cron       domainfunctions.TriggerConfig
	// Enabled 缺省 true。
	Enabled *bool
}

// CreateFunctionTrigger 创建触发器：函数必须存在；按 type 校验配置
// （response_mode/ack_body/handshake/body_limit；expr 可解析 + misfire）；
// http 的 token 服务端生成（128bit）；cron 计算 next_run_at = now 之后的
// 第一个计划时刻（UTC）。
func (f *Functions) CreateFunctionTrigger(ctx context.Context, cmd CreateTriggerCommand) (*domainfunctions.Trigger, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if _, err := f.GetFunction(ctx, cmd.ProjectID, cmd.FunctionID); err != nil {
		return nil, err
	}
	enabled := true
	if cmd.Enabled != nil {
		enabled = *cmd.Enabled
	}
	now := time.Now()
	trg := &domainfunctions.Trigger{
		ID:         "trg-" + idgen.UUID().String(),
		ProjectID:  cmd.ProjectID,
		FunctionID: cmd.FunctionID,
		Enabled:    enabled,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	switch cmd.Type {
	case domainfunctions.TriggerTypeHTTP:
		cfg, err := normalizeHTTPTriggerConfig(cmd.HTTP)
		if err != nil {
			return nil, err
		}
		trg.Type = domainfunctions.TriggerTypeHTTP
		trg.Config = cfg
		token, err := generateTriggerToken()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "generate trigger token: %v", err)
		}
		trg.Token = token
	case domainfunctions.TriggerTypeCron:
		cfg, err := normalizeCronTriggerConfig(cmd.Cron)
		if err != nil {
			return nil, err
		}
		trg.Type = domainfunctions.TriggerTypeCron
		trg.Config = cfg
		next, err := firstCronRun(cfg.Expr, now)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		trg.NextRunAt = &next
	default:
		return nil, status.Errorf(codes.InvalidArgument, "type must be %q or %q", domainfunctions.TriggerTypeHTTP, domainfunctions.TriggerTypeCron)
	}
	if err := f.triggers.CreateTrigger(ctx, trg); err != nil {
		return nil, err
	}
	return trg, nil
}

// ListFunctionTriggers 列出函数触发器（函数不存在 → 404）。
func (f *Functions) ListFunctionTriggers(ctx context.Context, projectID, functionID string) ([]domainfunctions.Trigger, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if _, err := f.GetFunction(ctx, projectID, functionID); err != nil {
		return nil, err
	}
	return f.triggers.ListTriggers(ctx, projectID, functionID)
}

// DeleteFunctionTrigger 删除触发器（不存在 → 404）。函数删除经 FK CASCADE
// 级联；dispatcher 不需要通知（P0.5 交接既定：靠 idle TTL 收敛）。
func (f *Functions) DeleteFunctionTrigger(ctx context.Context, projectID, functionID, triggerID string) error {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if _, err := f.getTrigger(ctx, projectID, functionID, triggerID); err != nil {
		return err
	}
	return f.triggers.DeleteTrigger(ctx, projectID, functionID, triggerID)
}

// RotateFunctionTriggerToken 轮换 http 触发器 token（旧 token 立即失效）。
func (f *Functions) RotateFunctionTriggerToken(ctx context.Context, projectID, functionID, triggerID string) (*domainfunctions.Trigger, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	trg, err := f.getTrigger(ctx, projectID, functionID, triggerID)
	if err != nil {
		return nil, err
	}
	if trg.Type != domainfunctions.TriggerTypeHTTP {
		return nil, status.Error(codes.InvalidArgument, "token rotation applies to http triggers only")
	}
	token, err := generateTriggerToken()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate trigger token: %v", err)
	}
	trg.Token = token
	trg.UpdatedAt = time.Now()
	if err := f.triggers.UpdateTrigger(ctx, trg); err != nil {
		return nil, err
	}
	return trg, nil
}

// GetHTTPTriggerByToken 公开调用查找：(project, token) → 启用的 http 触发器；
// 未命中（含禁用/cron 类型）一律返回 nil——handler 按 404 处理，不泄露存在性。
// 该方法供 /f/ 路由使用，token 即鉴权，不要求 principal。
func (f *Functions) GetHTTPTriggerByToken(ctx context.Context, projectID, token string) (*domainfunctions.Trigger, error) {
	trg, err := f.triggers.GetTriggerByToken(ctx, projectID, token)
	if err != nil {
		return nil, err
	}
	if trg == nil || !trg.Enabled || trg.Type != domainfunctions.TriggerTypeHTTP {
		return nil, nil
	}
	return trg, nil
}

// InvokeTrigger 触发器执行入口：与 CreateExecution 同一核心路径（两写预占
// 语义/异步队列/执行身份铸造全部一致），差异仅两点——token 门禁在 handler
// 已完成（不要求 principal）；data 上限随执行器模式放宽（触发器封套含透传
// body）。Source/SourceIP 记入执行记录（审计载体）。
func (f *Functions) InvokeTrigger(ctx context.Context, cmd InvokeTriggerCommand) (*domainfunctions.ExecutionRecord, error) {
	return f.createExecution(ctx, CreateExecutionCommand{
		ProjectID:      cmd.ProjectID,
		FunctionID:     cmd.FunctionID,
		Data:           cmd.Data,
		Async:          cmd.Async,
		Source:         cmd.Source,
		SourceIP:       cmd.SourceIP,
		DataLimitBytes: cmd.BodyLimitBytes,
	})
}

// DispatchDueCronTriggers 领取并投递全部 active 项目内到期的 cron 触发器
// （worker 每分钟 ticker 驱动）。项目遍历带轮转游标 + 全局预算
// （镜像 RecoverOrphanExecutions 模式——function_triggers 在项目 schema 内）。
// 先 CAS 后入队（repo ClaimDueCron 保证）；单条入队失败记日志并把
// next_run_at 回滚到原到期值（best-effort），不阻塞批内其他——下轮扫描
// catch_up 语义兜底。返回投递条数。
func (f *Functions) DispatchDueCronTriggers(ctx context.Context, now time.Time, budget int) (int, error) {
	if f.projects == nil || f.triggers == nil {
		return 0, nil
	}
	all, err := f.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	n := len(all)
	start := f.scanCursor.Start(n)
	remaining := budget
	dispatched := 0
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
		claims, err := f.triggers.ClaimDueCron(ctx, all[idx].ID, now, remaining, domainfunctions.DefaultCronNext)
		if err != nil {
			f.logger().Warn("claim due cron triggers failed", "project_id", all[idx].ID, "error", err)
			continue
		}
		for _, c := range claims {
			data, err := json.Marshal(cronEnvelope{Type: "cron", TriggerID: c.TriggerID, ScheduledFor: c.ScheduledFor.UTC().Format(time.RFC3339)})
			if err != nil {
				f.undoCronClaim(ctx, c, now)
				continue
			}
			if _, err := f.InvokeTrigger(ctx, InvokeTriggerCommand{
				ProjectID:  c.ProjectID,
				FunctionID: c.FunctionID,
				Data:       string(data),
				Async:      true,
				Source:     domainfunctions.TriggerTypeCron + ":" + c.TriggerID,
			}); err != nil {
				f.logger().Warn("dispatch cron trigger failed", "project_id", c.ProjectID,
					"function_id", c.FunctionID, "trigger_id", c.TriggerID, "error", err)
				f.undoCronClaim(ctx, c, now)
				continue
			}
			ObserveInvoke(c.ProjectID, c.FunctionID, metricSource(domainfunctions.TriggerTypeCron+":"+c.TriggerID), InvokeResultOK)
			dispatched++
		}
		remaining -= len(claims)
	}
	if stopped >= 0 {
		f.scanCursor.ResumeAt(stopped)
	} else {
		f.scanCursor.Complete()
	}
	return dispatched, nil
}

// undoCronClaim 入队失败后把 next_run_at 回滚到原到期值（CAS 到推进值，
// best-effort）：失败即该节拍已丢，回滚让下轮扫描 catch_up 语义兜底重领。
func (f *Functions) undoCronClaim(ctx context.Context, c domainfunctions.CronClaim, scheduledFor time.Time) {
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	trg, err := f.triggers.GetTrigger(undoCtx, c.ProjectID, c.FunctionID, c.TriggerID)
	if err != nil || trg == nil || trg.NextRunAt == nil {
		return
	}
	if !trg.NextRunAt.After(scheduledFor) {
		return // 已被其他路径推进（或并发恢复），不回退。
	}
	// 回滚为原到期值（下一轮扫描按 catch_up 语义重领；触发器 UpdateTrigger
	// 的可变列白名单含 next_run_at）。
	trg.NextRunAt = &c.ScheduledFor
	trg.UpdatedAt = time.Now()
	if err := f.triggers.UpdateTrigger(undoCtx, trg); err != nil {
		f.logger().Warn("undo cron claim failed", "trigger_id", c.TriggerID, "error", err)
	}
}

// cronEnvelope 是 cron 触发入队 data（函数内可读 scheduled_for 做幂等键）。
type cronEnvelope struct {
	Type         string `json:"type"`
	TriggerID    string `json:"trigger_id"`
	ScheduledFor string `json:"scheduled_for"`
}

// MaxTriggerBodyLimit 返回 HTTP 触发器请求体的生效上限（字节）：配置值
// （0 = 平台缺省 64KB）与执行器通道能力取小——v2 dispatcher 走 body 通道，
// 配置值（≤1MB）全额生效；v1 env 通道 data 预算 32KB 减封套余量。handler
// 据此做 413 判定，保证超限请求在入口被拒而不是执行创建期报 400。
func (f *Functions) MaxTriggerBodyLimit(configured int) int {
	limit := configured
	if limit <= 0 {
		limit = domainfunctions.DefaultHTTPBodyLimitBytes
	}
	if limit > domainfunctions.MaxHTTPBodyLimitBytes {
		limit = domainfunctions.MaxHTTPBodyLimitBytes
	}
	if !f.executorV2() {
		v1Cap := maxExecutionDataBytes - 8<<10 // 封套（method/path/headers）余量
		if limit > v1Cap {
			limit = v1Cap
		}
	}
	return limit
}

// getTrigger 取触发器；不存在 → 404。
func (f *Functions) getTrigger(ctx context.Context, projectID, functionID, triggerID string) (*domainfunctions.Trigger, error) {
	trg, err := f.triggers.GetTrigger(ctx, projectID, functionID, triggerID)
	if err != nil {
		return nil, err
	}
	if trg == nil {
		return nil, status.Error(codes.NotFound, "trigger not found")
	}
	return trg, nil
}

// normalizeHTTPTriggerConfig 校验并归一 http 触发器配置（创建期；app 层
// 跨字段规则——ack_body ≤1KB、body_limit ≤1MB、handshake 词表）。
func normalizeHTTPTriggerConfig(cfg domainfunctions.TriggerConfig) (domainfunctions.TriggerConfig, error) {
	switch cfg.ResponseMode {
	case domainfunctions.ResponseModeSync, domainfunctions.ResponseModeAsyncAck:
	case "":
		return cfg, status.Error(codes.InvalidArgument, "response_mode is required for http triggers")
	default:
		return cfg, status.Errorf(codes.InvalidArgument, "response_mode must be %q or %q",
			domainfunctions.ResponseModeSync, domainfunctions.ResponseModeAsyncAck)
	}
	if len(cfg.AckBody) > domainfunctions.MaxAckBodyBytes {
		return cfg, status.Errorf(codes.InvalidArgument, "ack_body exceeds maximum of %d bytes", domainfunctions.MaxAckBodyBytes)
	}
	switch cfg.Handshake {
	case "", domainfunctions.HandshakeEcho:
	default:
		return cfg, status.Errorf(codes.InvalidArgument, "handshake must be %q", domainfunctions.HandshakeEcho)
	}
	if cfg.BodyLimitBytes < 0 || cfg.BodyLimitBytes > domainfunctions.MaxHTTPBodyLimitBytes {
		return cfg, status.Errorf(codes.InvalidArgument, "body_limit_bytes must be between 1 and %d", domainfunctions.MaxHTTPBodyLimitBytes)
	}
	return cfg, nil
}

// normalizeCronTriggerConfig 校验并归一 cron 触发器配置（misfire 缺省
// catch_up_once；expr 可解析）。
func normalizeCronTriggerConfig(cfg domainfunctions.TriggerConfig) (domainfunctions.TriggerConfig, error) {
	switch cfg.Misfire {
	case domainfunctions.MisfireSkip, domainfunctions.MisfireCatchUpOnce:
	case "":
		cfg.Misfire = domainfunctions.MisfireCatchUpOnce
	default:
		return cfg, status.Errorf(codes.InvalidArgument, "misfire must be %q or %q",
			domainfunctions.MisfireSkip, domainfunctions.MisfireCatchUpOnce)
	}
	if err := domainfunctions.ValidateCronExpr(cfg.Expr); err != nil {
		return cfg, status.Errorf(codes.InvalidArgument, "invalid cron expr: %v", err)
	}
	return cfg, nil
}

// firstCronRun 计算创建后的首个计划时刻。
func firstCronRun(expr string, now time.Time) (time.Time, error) {
	sched, err := domainfunctions.ParseCronExpr(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(now)
}

// generateTriggerToken 生成 128bit 随机 token（base64url 22 字符，不可猜
// 即鉴权）。
func generateTriggerToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	if len(token) > domainfunctions.MaxTriggerTokenBytes {
		return "", status.Errorf(codes.Internal, "token too long")
	}
	return token, nil
}
