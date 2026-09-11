// Package analytics 是事件摄入用例（docs/design/analytics.md §4.3；PR2 摄入链）：
// 逐事件校验（形状/钳制/标量化截断/16KiB）、归因落定（client 面 Principal /
// server 面可信代报）、字典 upsert 前置软上限（D12）与 accepted/skipped
// 部分接收语义。红线（执行计划）：不进 outbox、不发 realtime、不触发函数
// （D1）；不做去重（D14，at-least-once）。
package analytics

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"time"

	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// meterTimeout 是摄入计量写入的预算（best-effort，镜像 usage 拦截器模式：
// WithoutCancel + 200ms，计量故障不影响摄入响应）。
const meterTimeout = 200 * time.Millisecond

// IncomingEvent 是待校验的单事件（传输层从 proto 原样搬运，归因/校验/钳制
// 全部在用例层落定——请求体无法伪造 source 与 client 面 user_id）。
type IncomingEvent struct {
	Name       string
	OccurredAt *time.Time // nil = 服务端 now（批级统一时钟）
	Props      map[string]*structpb.Value
	SessionID  string
	// UserID 仅 server 面可信代报（D3）；client 面恒空——归因唯一来源是
	// command.ClientUserID（Principal 注入，红线）。
	UserID string
}

// IngestEventsCommand 是一次摄入批。
type IngestEventsCommand struct {
	ProjectID string
	// Source 服务端落定的摄入通道：domainanalytics.SourceClient |
	// SourceServer（请求体不携带，由双面 handler 各自绑定）。
	Source string
	// ClientUserID 是 client 面的 Principal 归因（p.UserID）。Source=client
	// 时必填；Source=server 时忽略（per-event UserID 可信代报）。
	ClientUserID string
	Events       []IncomingEvent
}

// IngestResult 是摄入结果（部分接收语义，D12）。
type IngestResult struct {
	Accepted int32
	Skipped  int32
}

// Ingest 是摄入用例聚合。
type Ingest struct {
	repo   domainanalytics.IngestRepository
	usage  domainbilling.UsageCounter // 可选：nil 不计量（测试装配）
	logger *slog.Logger
	now    func() time.Time
}

// NewIngest 构造用例（Wire）。
func NewIngest(repo domainanalytics.IngestRepository, usage domainbilling.UsageCounter, logger *slog.Logger) *Ingest {
	if logger == nil {
		logger = slog.Default()
	}
	return &Ingest{repo: repo, usage: usage, logger: logger, now: time.Now}
}

// IngestEvents 处理一批事件：逐事件校验 → 字典软上限过滤 → 多行单语句
// INSERT → 字典 upsert → 计量。任何单事件违规只计入 skipped（不拒整批）；
// 批级错误（存储故障/项目缺失）才返回 error。
func (u *Ingest) IngestEvents(ctx context.Context, cmd IngestEventsCommand) (*IngestResult, error) {
	if u.repo == nil {
		return nil, status.Error(codes.Internal, "analytics repository is not configured")
	}
	if cmd.ProjectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if cmd.Source != domainanalytics.SourceClient && cmd.Source != domainanalytics.SourceServer {
		return nil, status.Error(codes.Internal, "invalid analytics source")
	}
	if cmd.Source == domainanalytics.SourceClient && cmd.ClientUserID == "" {
		// client 面归因唯一来源是 Principal（D3 红线）——匿名会话也是真实
		// 用户行（users 表 anonymous label），UserID 恒非空。
		return nil, status.Error(codes.Unauthenticated, "analytics ingestion requires an end-user principal")
	}
	if len(cmd.Events) == 0 {
		return nil, status.Error(codes.InvalidArgument, "events must not be empty")
	}
	if len(cmd.Events) > domainanalytics.MaxBatchEvents {
		return nil, status.Errorf(codes.InvalidArgument, "events exceed batch limit %d", domainanalytics.MaxBatchEvents)
	}

	// 批级统一时钟：occurred_at 缺省、钳制窗、ingested_at、字典 first/last_seen
	// 共用同一 now（批内判定一致）。
	now := u.now().UTC()

	// 字典软上限预检（D12）：未达上限放行全部（批内新增可轻微超扣，可容忍）；
	// 已达上限时取存量名集合，批内新名事件 skip、存量名照常。
	existing, err := u.eventNameAllowlist(ctx, cmd.ProjectID)
	if err != nil {
		return nil, err
	}

	events := make([]domainanalytics.Event, 0, len(cmd.Events))
	nameSet := make(map[string]struct{}, len(cmd.Events))
	var accepted, skipped int32
	for _, e := range cmd.Events {
		normalized, ok := normalizeEvent(cmd, e, now)
		if !ok {
			skipped++
			continue
		}
		if existing != nil {
			if _, isOld := existing[normalized.Name]; !isOld {
				// 软上限已达：仅拒新名（存量名不受影响，D12）。
				skipped++
				continue
			}
		}
		events = append(events, normalized)
		nameSet[normalized.Name] = struct{}{}
		accepted++
	}

	if len(events) > 0 {
		if err := u.repo.InsertEvents(ctx, cmd.ProjectID, events); err != nil {
			return nil, err
		}
		// 字典 upsert 失败不回滚事件（事件是业务数据、字典是发现元数据；
		// first_seen 由本次 upsert 补记，at-least-once 重试可自愈）。
		defs := make([]domainanalytics.EventDefinition, 0, len(nameSet))
		for name := range nameSet {
			defs = append(defs, domainanalytics.EventDefinition{Name: name, FirstSeen: now, LastSeen: now})
		}
		if err := u.repo.UpsertEventDefinitions(ctx, cmd.ProjectID, defs); err != nil {
			u.logger.WarnContext(ctx, "analytics event definitions upsert failed",
				slog.String("project_id", cmd.ProjectID),
				slog.Int("names", len(defs)),
				slog.String("error", err.Error()))
		}
		// 计量（D15）：accepted 条数进 usage 脊柱，best-effort。
		u.meterAccepted(ctx, cmd.ProjectID, int64(accepted))
	}

	return &IngestResult{Accepted: accepted, Skipped: skipped}, nil
}

// eventNameAllowlist 返回软上限判定量：未达上限返回 nil（全放行）；已达上限
// 返回存量名集合（含本批前已 upsert 的名字——调用方逐批查询，无跨批记忆）。
func (u *Ingest) eventNameAllowlist(ctx context.Context, projectID string) (map[string]struct{}, error) {
	count, err := u.repo.CountEventDefinitions(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if count < domainanalytics.MaxEventNames {
		return nil, nil
	}
	names, err := u.repo.ListEventDefinitionNames(ctx, projectID)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(names))
	for _, n := range names {
		existing[n] = struct{}{}
	}
	return existing, nil
}

// normalizeEvent 对单事件执行全部逐项校验与规范化（任一违规 → false=skip）：
//   - name/session_id/user_id 形状（protovalidate 已挡请求级形状，此处防御
//     性复检——用例层可被非 gRPC 链路复用）；
//   - occurred_at 缺省=now + 钳制 [now-24h, now+5min]（D4，越界 skip）；
//   - props 键正则/键数、值标量化（拒绝 object 与嵌套 array，D9 浅数组合法）、
//     字符串值截断 256；
//   - 规范化 props JSON 序列化 ≤16KiB（存储形态体积护栏）。
func normalizeEvent(cmd IngestEventsCommand, e IncomingEvent, now time.Time) (domainanalytics.Event, bool) {
	if !domainanalytics.EventNamePattern.MatchString(e.Name) {
		return domainanalytics.Event{}, false
	}
	if len(e.SessionID) > domainanalytics.MaxSessionIDLen {
		return domainanalytics.Event{}, false
	}
	userID := cmd.ClientUserID
	if cmd.Source == domainanalytics.SourceServer {
		userID = e.UserID // 可信代报；空 = 无归属（不进 user_days/first_seen）
		if len(userID) > domainanalytics.MaxUserIDLen {
			return domainanalytics.Event{}, false
		}
	}

	occurredAt := now
	if e.OccurredAt != nil {
		occurredAt = e.OccurredAt.UTC()
		if occurredAt.Before(now.Add(domainanalytics.ClampPast)) || occurredAt.After(now.Add(domainanalytics.ClampFuture)) {
			return domainanalytics.Event{}, false
		}
	}

	props, ok := normalizeProps(e.Props)
	if !ok {
		return domainanalytics.Event{}, false
	}

	return domainanalytics.Event{
		Name:       e.Name,
		UserID:     userID,
		SessionID:  e.SessionID,
		Source:     cmd.Source,
		OccurredAt: occurredAt,
		IngestedAt: now,
		Props:      props,
	}, true
}

// normalizeProps 校验并规范化 props：键正则 + 键数上限；值仅接受标量或浅
// 数组（object/嵌套结构拒绝——props 是维度键值不是自由文档）；字符串值
// 超 256 字符截断入库（不拒收）。输出为键序稳定的规范 JSON（空集 '{}'），
// 序列化体积超 MaxEventBytes 判 false。
func normalizeProps(props map[string]*structpb.Value) (json.RawMessage, bool) {
	if len(props) == 0 {
		return json.RawMessage(`{}`), true
	}
	if len(props) > domainanalytics.MaxPropsKeys {
		return nil, false
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		if !domainanalytics.PropKeyPattern.MatchString(k) {
			return nil, false
		}
		keys = append(keys, k)
	}
	sort.Strings(keys) // 键序稳定：序列化确定性 + 16KiB 判定可复现

	var b []byte
	b = append(b, '{')
	for i, k := range keys {
		if i > 0 {
			b = append(b, ',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, false
		}
		b = append(b, kb...)
		b = append(b, ':')
		vb, ok := appendPropValue(b, props[k])
		if !ok {
			return nil, false
		}
		b = vb
	}
	b = append(b, '}')
	if len(b) > domainanalytics.MaxEventBytes {
		return nil, false
	}
	return json.RawMessage(b), true
}

// appendPropValue 把单个 structpb.Value 渲染为规范 JSON 片段（追加到 dst）。
// 标量：null/bool/number/string（string 截断 PropValueMaxLen 个字符）；
// 浅数组：元素递归标量化，空数组合法；struct/object：拒绝（返回 false）。
func appendPropValue(dst []byte, v *structpb.Value) ([]byte, bool) {
	if v == nil {
		return append(dst, "null"...), true
	}
	switch kind := v.GetKind().(type) {
	case *structpb.Value_NullValue:
		return append(dst, "null"...), true
	case *structpb.Value_BoolValue:
		if kind.BoolValue {
			return append(dst, "true"...), true
		}
		return append(dst, "false"...), true
	case *structpb.Value_NumberValue:
		// NaN/Inf：protojson 的 number_value 可携带（JSON 字面 NaN），但
		// 不是合法 JSON、JSONB 列必拒——按坏事件 skip，不让单事件炸整批。
		if math.IsNaN(kind.NumberValue) || math.IsInf(kind.NumberValue, 0) {
			return nil, false
		}
		return strconv.AppendFloat(dst, kind.NumberValue, 'g', -1, 64), true
	case *structpb.Value_StringValue:
		s := []rune(kind.StringValue)
		if len(s) > domainanalytics.PropValueMaxLen {
			s = s[:domainanalytics.PropValueMaxLen]
		}
		sb, err := json.Marshal(string(s))
		if err != nil {
			return nil, false
		}
		return append(dst, sb...), true
	case *structpb.Value_ListValue:
		items := kind.ListValue.GetValues()
		dst = append(dst, '[')
		for i, item := range items {
			if i > 0 {
				dst = append(dst, ',')
			}
			var ok bool
			if dst, ok = appendPropValue(dst, item); !ok {
				return nil, false
			}
		}
		return append(dst, ']'), true
	default:
		// StructValue（object）与 nil kind：标量化拒绝（含数组内嵌套）。
		return nil, false
	}
}

// meterAccepted 把 accepted 条数写入当前小时 Redis bucket（best-effort：
// WithoutCancel + 200ms 超时，失败只告警不影响响应，镜像 usage 拦截器模式）。
func (u *Ingest) meterAccepted(ctx context.Context, projectID string, accepted int64) {
	if u.usage == nil || accepted <= 0 {
		return
	}
	meterCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), meterTimeout)
	defer cancel()
	if err := u.usage.Incr(meterCtx, projectID, domainbilling.MetricAnalyticsEvents, accepted); err != nil {
		u.logger.WarnContext(ctx, "analytics usage meter incr failed",
			slog.String("project_id", projectID),
			slog.String("error", err.Error()))
	}
}
