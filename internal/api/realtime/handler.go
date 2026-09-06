// Package realtime 实现 GET /v1/realtime 的 WebSocket 门面（v2 设计
// §4）：hello 握手（SDK 首帧 JWT / Console cookie，拒 API Key 与 guest）、
// 订阅协议、配额（4 连 / 32 订）、ping 滑窗保活与 JWT 到期断开。
// handler 只做握手与帧编解码，订阅校验调 databases.DocumentDB 端口，
// 扇出走 shared.RealtimeHub 端口（实现位于 internal/infra/realtime）。
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	domainevents "github.com/torchwooddev/torchwood/internal/domain/events"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/grpc/interceptor"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"github.com/torchwooddev/torchwood/pkg/jwtparser"
)

// 保活与超时（v2 设计 §4.1 / §4.3）：http.Server 读写超时已由 gateway
// 置 0；这里每条 Read/Write 前设短 deadline（收到帧 / 写完即释放），
// 服务端每 30s 发 ping，2 个间隔未收到 pong 则断开。
const (
	helloTimeout      = 10 * time.Second
	frameReadTimeout  = 60 * time.Second
	frameWriteTimeout = 10 * time.Second
	pingInterval      = 30 * time.Second
	pongWindow        = 2 * pingInterval
	// maxClientFrameBytes 是客户端帧（hello/subscribe/ping）上限。
	maxClientFrameBytes = 1 << 20
)

// 出站错误码（与协议约定一致，客户端按码分支）。
const (
	errCodeUnauthenticated    = "UNAUTHENTICATED"
	errCodeResourceExhausted  = "RESOURCE_EXHAUSTED"
	errCodeNotFound           = "NOT_FOUND"
	errCodeInvalidArgument    = "INVALID_ARGUMENT"
	errCodeInternal           = "INTERNAL"
	errCodeEventsResumeExpired = "EVENTS.RESUME_EXPIRED"
)

// maxReplayChanges 是 subscribe 带 last_seq 时单次补发上限（阶段④ §4.5）：
// 超出则订阅确认帧带 has_more=true，客户端走 :changes 以末条 seq 续传。
const maxReplayChanges = 500

// CredentialValidator 是握手所需的凭证校验面（auth.Validator 满足；
// 接口化便于单测）。
type CredentialValidator interface {
	Authenticate(ctx context.Context, req shared.AuthnRequest) (*shared.Principal, error)
	ValidateToken(ctx context.Context, token string) (*shared.Principal, error)
	ValidateCredential(ctx context.Context, raw string, credentialType shared.CredentialType) (*shared.Principal, error)
	ValidateAdminProjectAccess(ctx context.Context, principal *shared.Principal) error
	// ParseClaims 解析 JWT claims（ttp / exp 检查用），非 JWT 凭证返回 false。
	ParseClaims(raw string) (*jwtparser.Claims, bool)
}

// Handler 是 /v1/realtime 的 WS handler。
type Handler struct {
	cfg       *config.AppConfig
	validator CredentialValidator
	docDB     databases.DocumentDB
	hub       shared.RealtimeHub
	conns     *connectionRegistry
	logger    *slog.Logger
}

// NewHandler 构造 WS handler。
func NewHandler(
	cfg *config.AppConfig,
	validator CredentialValidator,
	docDB databases.DocumentDB,
	hub shared.RealtimeHub,
	logger *slog.Logger,
) (*Handler, error) {
	if cfg == nil {
		return nil, errors.New("config cannot be nil")
	}
	if validator == nil {
		return nil, errors.New("validator cannot be nil")
	}
	if docDB == nil {
		return nil, errors.New("document db cannot be nil")
	}
	if hub == nil {
		return nil, errors.New("hub cannot be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		cfg:       cfg,
		validator: validator,
		docDB:     docDB,
		hub:       hub,
		conns:     newConnectionRegistry(),
		logger:    logger,
	}, nil
}

// ServeHTTP 处理 WS 升级。握手（hello 帧）在升级后进行。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	conn, err := websocket.Accept(w, r, h.acceptOptions())
	if err != nil {
		return // Accept 已写 HTTP 错误响应
	}
	// 关于 hijack 后 conn deadline（v2 设计 §4.1 第 2 件事）：gateway 的
	// http.Server 已置 ReadTimeout=0/WriteTimeout=0（grpc_gateway.go），
	// listener 从不向 net.Conn 打 deadline，hijack 后无 deadline 可清。
	// 设计示例里的 websocket.NetConn + SetReadDeadline 实测（coder/websocket
	// v1.8.14 netconn.go）只操作包装层自己的计时器，够不到底层 conn，
	// 故此处不做空调用；保活完全靠下面的帧级 deadline + ping 滑窗。
	defer func() { _ = conn.CloseNow() }()
	h.serveConn(r, conn)
}

// acceptOptions 组装 origin 校验：沿用现有 CORS 配置（allow_origins
// 作为 OriginPatterns；配置 "*" 时放开校验，与 CORSMiddleware 一致）。
func (h *Handler) acceptOptions() *websocket.AcceptOptions {
	opts := &websocket.AcceptOptions{}
	if srv := h.cfg.GetServer(); srv != nil && srv.GetHttp() != nil {
		if cors := srv.GetHttp().GetCors(); cors != nil {
			patterns := make([]string, 0, len(cors.GetAllowOrigins()))
			for _, o := range cors.GetAllowOrigins() {
				if o == "*" {
					opts.InsecureSkipVerify = true
					continue
				}
				patterns = append(patterns, o)
			}
			opts.OriginPatterns = patterns
		}
	}
	return opts
}

// helloFrame 是客户端首帧（SDK 必带 access_token；Console cookie 可省略）。
type helloFrame struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	AccessToken string `json:"access_token"`
}

// inboundFrame 是客户端控制帧。
type inboundFrame struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Channel string `json:"channel"`
	// LastSeq（阶段④ §4.5）：可选续传游标。> 0 时服务端先从 outbox 补发
	// 该频道 seq > last_seq 的可见事件再进入实时流；0/缺省 = 纯实时订阅。
	LastSeq int64 `json:"last_seq"`
}

// outboundFrame 是服务端出站帧（事件帧由 Hub 组装为 map 直写）。
type outboundFrame struct {
	Type         string `json:"type"`
	ID           string `json:"id,omitempty"`
	Code         string `json:"code,omitempty"`
	Message      string `json:"message,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
	Channel      string `json:"channel,omitempty"`
	// 重放摘要（阶段④）：subscribe 带 last_seq 时订阅确认帧携带。
	Replayed int64 `json:"replayed,omitempty"`
	HasMore  bool  `json:"has_more,omitempty"`
}

// connState 是一条 WS 连接的运行时状态。
type connState struct {
	id            string
	projectID     string
	principal     *shared.Principal
	platformAdmin bool
	docPrincipal  databases.Principal
	quotaKey      string
	expiresAt     time.Time // JWT 到期时间；WS 只收 JWT 凭证（SDK access_token / console cookie 均为 JWT），正常不会为零值

	hubConn *shared.RealtimeConn
	subs    map[string]struct{} // 已订阅频道（去重 + 计数）

	// resyncCh（阶段④水位断开）：Hub 满水位触发 OnSlow → writeLoop 收款
	// 后以 close reason "resync:<last_seq>" 断开；客户端重连带 last_seq
	// 即天然 RESYNC（断开即重放，语义等价、协议更简——B4 简化）。
	resyncCh   chan int64
	lastPong   atomic.Int64 // unix nano；hello_ok 后置初值
	cancel     context.CancelFunc
	// expiryTimer 是 JWT 到期关连接定时器；连接提前断开时必须 Stop
	//（P2-5：否则 timer 持有 conn 状态直至 token 到期，高 churn 下累积）。
	expiryTimer *time.Timer
	closeMu     sync.Mutex
	closed      bool
}

func newConnState(principal *shared.Principal, projectID, quotaKey string, claims *jwtparser.Claims) *connState {
	connID := idgen.ULID().String()
	st := &connState{
		id:            connID,
		projectID:     projectID,
		principal:     principal,
		platformAdmin: principal.IsPlatformAdmin,
		docPrincipal:  principal.DocPrincipal(),
		quotaKey:      quotaKey,
		subs:          make(map[string]struct{}),
		resyncCh:      make(chan int64, 1),
	}
	if claims != nil && claims.ExpiresAt > 0 {
		st.expiresAt = time.Unix(claims.ExpiresAt, 0)
	}
	st.lastPong.Store(time.Now().UnixNano())
	st.hubConn = &shared.RealtimeConn{
		ID:            connID,
		PlatformAdmin: st.platformAdmin,
		DocPrincipal:  st.docPrincipal,
		// B-1 隔离锚点：hello.project_id 已在 authenticate 与凭证一致性
		// 校验（admin 绑定 + end_user 等值断言），Hub 扇出按其等值过滤
		// 文档事件（频道名为全局命名空间，隔离由消费端 project 过滤保证）。
		ProjectID: st.projectID,
		Send:      make(chan map[string]any, shared.RealtimeSendBuffer),
		OnSlow:    func(lastSeq int64) { st.requestResync(lastSeq) },
	}
	return st
}

// requestResync 非阻塞投递慢断开信号（OnSlow 恰一次调用，容量 1 足够）。
func (st *connState) requestResync(lastSeq int64) {
	select {
	case st.resyncCh <- lastSeq:
	default:
	}
}

// quotaKeyOf 生成连接配额键（v2 设计 §4.4）：
// Client JWT → user:{UserID}；Console admin → admin:{ActorID}。
func quotaKeyOf(p *shared.Principal) string {
	if p.ActorKind == shared.ActorKindAdmin {
		return "admin:" + string(p.ActorID)
	}
	return "user:" + p.UserID
}

// serveConn 完成握手后进入读/写双循环，任一退出即清理。
func (h *Handler) serveConn(r *http.Request, c *websocket.Conn) {
	ctx := r.Context()
	c.SetReadLimit(maxClientFrameBytes)

	hello, err := readHello(ctx, c)
	if err != nil {
		h.failHandshake(c, errCodeUnauthenticated, "missing or invalid hello frame")
		return
	}
	principal, claims, err := h.authenticate(ctx, r, hello)
	if err != nil {
		RealtimeHandshakeTotal.WithLabelValues("unauthenticated").Inc()
		h.failHandshake(c, errCodeUnauthenticated, err.Error())
		return
	}
	quotaKey := quotaKeyOf(principal)
	if !h.conns.acquire(quotaKey) {
		RealtimeHandshakeTotal.WithLabelValues("exhausted").Inc()
		h.failHandshake(c, errCodeResourceExhausted, "too many connections for this user")
		return
	}
	RealtimeHandshakeTotal.WithLabelValues("ok").Inc()

	st := newConnState(principal, hello.ProjectID, quotaKey, claims)
	RealtimeConnections.WithLabelValues(st.projectID).Inc()

	// hello_ok 出站后进入保活循环。
	if err := h.writeFrame(ctx, c, &outboundFrame{Type: "hello_ok", ConnectionID: st.id}); err != nil {
		h.cleanup(st)
		return
	}

	// JWT 到期主动关连接（StatusPolicyViolation + 原因 token_expired）。
	// 独立定时器而非依赖读超时：coder 的 ctx 读超时会直接裸关连接
	// （无 close frame），客户端只能看到 EOF；Close 则在等待握手前
	// 先把 close frame 写上线。
	if !st.expiresAt.IsZero() {
		st.expiryTimer = time.AfterFunc(time.Until(st.expiresAt), func() {
			_ = c.Close(websocket.StatusPolicyViolation, "token_expired")
		})
	}

	runCtx, cancel := context.WithCancel(ctx)
	st.cancel = cancel
	// 任一循环退出即取消 runCtx，让另一循环尽快返回（避免 wg.Wait
	// 死锁导致 cleanup 不执行、配额不释放）。
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(cancel) }
	var wg sync.WaitGroup
	wg.Go(func() {
		defer stop()
		h.readLoop(runCtx, c, st)
	})
	wg.Go(func() {
		defer stop()
		h.writeLoop(runCtx, c, st)
	})
	wg.Wait()
	cancel()
	h.cleanup(st)
}

// cleanup 释放连接资源：从 Hub 摘除（扇出停止）、释放配额、更新指标。
func (h *Handler) cleanup(st *connState) {
	st.closeMu.Lock()
	if st.closed {
		st.closeMu.Unlock()
		return
	}
	st.closed = true
	st.closeMu.Unlock()

	if st.cancel != nil {
		st.cancel()
	}
	if st.expiryTimer != nil {
		st.expiryTimer.Stop()
	}
	h.hub.Remove(st.id)
	h.conns.release(st.quotaKey)
	RealtimeConnections.WithLabelValues(st.projectID).Dec()
}

// failHandshake 握手失败：发一帧 error 后以策略违规关闭。
func (h *Handler) failHandshake(c *websocket.Conn, code, message string) {
	_ = h.writeFrame(context.Background(), c, &outboundFrame{Type: "error", Code: code, Message: message})
	_ = c.Close(websocket.StatusPolicyViolation, code)
}

// readHello 读取并校验首帧 hello（10s 时限）。
func readHello(ctx context.Context, c *websocket.Conn) (*helloFrame, error) {
	ctxRead, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	var f helloFrame
	if err := readFrame(ctxRead, c, &f); err != nil {
		return nil, err
	}
	if f.Type != "hello" {
		return nil, errors.New("first frame must be hello")
	}
	return &f, nil
}

// readFrame 读取一帧文本消息并解码为 JSON（binary 帧拒绝）。
func readFrame(ctx context.Context, c *websocket.Conn, v any) error {
	typ, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("binary frames are not supported")
	}
	return json.Unmarshal(data, v)
}

// authenticate 完成身份与项目绑定校验（v2 设计 §4.2）：
//   - 与 gRPC/HTTP 共用 Authenticate；拒 API Key、只收 console cookie 是 Grant
//   - 拒 guest / 非法 / 过期 / ttp != access
//   - admin：先 principal.ProjectID = hello.project_id（空则拒），
//     再 ValidateAdminProjectAccess；非 platform admin 无项目访问权则拒
//   - end_user：hello.project_id 必须与身份（claims pid / 会话 cookie pid）一致
func (h *Handler) authenticate(ctx context.Context, r *http.Request, hello *helloFrame) (*shared.Principal, *jwtparser.Claims, error) {
	if hello.ProjectID == "" {
		return nil, nil, errors.New("project_id is required")
	}
	// Grant：Realtime 禁止 API key（不是第三份解析器）。
	if len(r.Header.Values("X-Api-Key")) > 0 {
		return nil, nil, errors.New("api key credentials are not allowed")
	}
	if raw := r.Header.Get("Authorization"); raw != "" {
		if ct, _, ok := interceptor.ParseAuthorizationHeader(raw); ok && ct == shared.CredentialTypeAPIKey {
			return nil, nil, errors.New("api key credentials are not allowed")
		}
	}

	req := shared.AuthnRequest{
		Authorization: r.Header.Values("Authorization"),
		CookieHeaders: realtimeSessionCookieHeaders(r),
		AccessToken:   hello.AccessToken,
	}
	principal, err := h.validator.Authenticate(ctx, req)
	if err != nil || principal == nil || !principal.IsAuthenticated() {
		if _, _, parseErr := shared.ParseAuthnRequest(req); parseErr != nil {
			if errors.Is(parseErr, shared.ErrMissingCredential) {
				return nil, nil, errors.New("authentication required")
			}
			return nil, nil, parseErr
		}
		return nil, nil, errors.New("invalid or expired credential")
	}
	if principal.ActorKind == shared.ActorKindService || principal.IsSystem() {
		return nil, nil, errors.New("api key credentials are not allowed")
	}

	// ttp / exp：读取 JWT claims 供握手后的到期断开用（SDK access_token
	// 与 console cookie 值都是 JWT）。
	ct, cred, parseErr := shared.ParseAuthnRequest(req)
	var claims *jwtparser.Claims
	if parseErr == nil && (ct == shared.CredentialTypeToken || ct == shared.CredentialTypeSession) {
		claims, _ = h.validator.ParseClaims(cred)
	}
	if claims != nil && claims.TokenType != "" && claims.TokenType != jwtparser.TokenTypeAccess {
		return nil, nil, errors.New("access token required")
	}

	switch principal.ActorKind {
	case shared.ActorKindAdmin:
		// 锁定顺序：先绑 ProjectID 再 ValidateAdminProjectAccess
		// （空 ProjectID 时 ValidateAdminProjectAccess 直接成功）。
		principal.ProjectID = hello.ProjectID
		if err := h.validator.ValidateAdminProjectAccess(ctx, principal); err != nil {
			return nil, nil, errors.New("admin has no access to this project")
		}
	case shared.ActorKindEndUser:
		if principal.ProjectID != hello.ProjectID {
			return nil, nil, errors.New("project_id does not match credential")
		}
	default:
		return nil, nil, errors.New("credential type not allowed")
	}
	return principal, claims, nil
}

// realtimeSessionCookieHeaders 收集 WS 握手可用的 cookie：
// - TORCHWOOD_session_console（Console admin 会话）
// - TORCHWOOD_session_<project>（B10 端用户 cookie，与 JWT 二选一，仍拒 API key）
func realtimeSessionCookieHeaders(r *http.Request) []string {
	var out []string
	for _, c := range r.Cookies() {
		if c.Value == "" {
			continue
		}
		if c.Name == shared.ConsoleSessionCookieName {
			out = append(out, c.Name+"="+c.Value)
			continue
		}
		if strings.HasPrefix(c.Name, shared.SessionCookiePrefix) {
			out = append(out, c.Name+"="+c.Value)
		}
	}
	return out
}

// consoleSessionCookieHeaders 保留别名兼容旧测试桩（若有外部调用），内部已由 realtimeSessionCookieHeaders 接管。

//nolint:unused
func consoleSessionCookieHeaders(r *http.Request) []string {
	return realtimeSessionCookieHeaders(r)
}

// readLoop 逐帧读取客户端控制帧；60s 无帧时按 pong 滑窗判定，
// 2 个 ping 间隔无 pong 则断开；JWT 到期由握手后的定时器断开
// （token_expired），循环顶部的 expired 检查只是兜底。
func (h *Handler) readLoop(ctx context.Context, c *websocket.Conn, st *connState) {
	for {
		if st.expired() {
			h.closeWithReason(c, "token_expired")
			return
		}
		ctxRead, cancel := context.WithTimeout(ctx, frameReadTimeout)
		var f inboundFrame
		err := readFrame(ctxRead, c, &f)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				if pongStale(st) {
					h.closeWithReason(c, "pong_timeout")
					return
				}
				continue
			}
			return // 客户端断开或协议错误
		}
		switch f.Type {
		case "ping":
			if err := h.writeFrame(ctx, c, &outboundFrame{Type: "pong"}); err != nil {
				return
			}
		case "pong":
			st.lastPong.Store(time.Now().UnixNano())
		case "subscribe":
			h.handleSubscribe(ctx, c, st, &f)
		case "unsubscribe":
			h.handleUnsubscribe(st, &f)
		default:
			_ = h.writeFrame(ctx, c, &outboundFrame{
				ID: f.ID, Type: "error", Code: errCodeInvalidArgument,
				Message: "unknown frame type",
			})
		}
	}
}

// writeLoop 消费 Hub 发送 chan、30s ping ticker 与 resync 信号。
// 水位断开（阶段④）：Send 满载 → OnSlow → 此处以 close reason
// "resync:<last_seq>" 主动断开；排空中的帧直接丢弃（客户端从 last_seq
// 重放补齐，等价 RESYNC）。
func (h *Handler) writeLoop(ctx context.Context, c *websocket.Conn, st *connState) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case lastSeq := <-st.resyncCh:
			_ = c.Close(websocket.StatusPolicyViolation, fmt.Sprintf("resync:%d", lastSeq))
			return
		case frame := <-st.hubConn.Send:
			data, err := json.Marshal(frame)
			if err != nil {
				return
			}
			if err := h.writeBytes(ctx, c, data); err != nil {
				return
			}
		case <-ticker.C:
			if st.expired() {
				h.closeWithReason(c, "token_expired")
				return
			}
			if err := h.writeFrame(ctx, c, &outboundFrame{Type: "ping"}); err != nil {
				return
			}
		}
	}
}

// handleSubscribe 校验并登记订阅（v2 设计 §4.3 / §4.4）：
// 集合频道：GetCollection 存在、!IsSystem、!Disabled（特权旁路）且以订阅者
// 文档主体通过一次 REST List 语义的 read 探测（一律 NOT_FOUND，防枚举）；
// 文档频道：GetDocument 存在且当前 principal 可 read（一律 NOT_FOUND）。
// 超 32 订返回 RESOURCE_EXHAUSTED，连接保持。
//
// last_seq 重放（阶段④ §4.5）：带 last_seq>0 的 databases 频道订阅走
// 「门控 → 订阅 → outbox 补发 → 刷入 backlog」——补发帧先于实时帧、
// 补发期间到达的实时帧去重后按序续在其后，无漏帧窗口。超出单次上限
// （500）时订阅确认带 has_more=true（:changes 续传）；游标早于窗口 →
// error 帧 EVENTS.RESUME_EXPIRED（订阅失败，连接保持）。
func (h *Handler) handleSubscribe(ctx context.Context, c *websocket.Conn, st *connState, f *inboundFrame) {
	if len(st.subs) >= MaxSubscriptionsPerConn {
		_ = h.writeFrame(ctx, c, &outboundFrame{
			ID: f.ID, Type: "error", Code: errCodeResourceExhausted,
			Message: "subscription limit reached",
		})
		return
	}
	parsed, ok := parseChannel(f.Channel)
	if !ok {
		_ = h.writeFrame(ctx, c, &outboundFrame{
			ID: f.ID, Type: "error", Code: errCodeInvalidArgument,
			Message: "invalid channel",
		})
		return
	}
	if f.LastSeq > 0 && parsed.kind != channelKindDatabases {
		_ = h.writeFrame(ctx, c, &outboundFrame{
			ID: f.ID, Type: "error", Code: errCodeInvalidArgument,
			Message: "last_seq is only supported on databases channels",
		})
		return
	}
	if !h.channelAllowed(ctx, st, parsed) {
		_ = h.writeFrame(ctx, c, &outboundFrame{
			ID: f.ID, Type: "error", Code: errCodeNotFound,
			Message: "channel not found",
		})
		return
	}

	ack := &outboundFrame{ID: f.ID, Type: "subscribed", Channel: f.Channel}
	if _, dup := st.subs[f.Channel]; !dup {
		st.subs[f.Channel] = struct{}{}
		if f.LastSeq > 0 {
			replayed, hasMore, nextSeq, err := h.subscribeWithReplay(ctx, st, parsed, f)
			if err != nil {
				delete(st.subs, f.Channel)
				if errors.Is(err, databases.ErrResumeExpired) {
					_ = h.writeFrame(ctx, c, &outboundFrame{
						ID: f.ID, Type: "error", Code: errCodeEventsResumeExpired,
						Message: "resume cursor predates the oldest available event; re-sync with a full listing and resume from the latest seq",
					})
					return
				}
				h.logger.Error("realtime replay failed",
					"connection_id", st.id, "channel", f.Channel, "error", err)
				_ = h.writeFrame(ctx, c, &outboundFrame{
					ID: f.ID, Type: "error", Code: errCodeInternal,
					Message: "replay failed",
				})
				return
			}
			ack.Replayed = replayed
			ack.HasMore = hasMore
			// 确认帧必须排在补发帧之后：经 Send 通道由 writeLoop 统一写
			//（readLoop 直写与 writeLoop 并发，直写会抢在补发帧之前上线）。
			// has_more=true 时 next_seq 携带续传游标（R15）：扫描游标优先
			//（越过不可见块），0 时回退末条补发事件 seq。
			ackFrame := map[string]any{
				"type": "subscribed", "id": ack.ID, "channel": ack.Channel,
				"replayed": ack.Replayed, "has_more": ack.HasMore,
			}
			if hasMore {
				ackFrame["next_seq"] = nextSeq
			}
			st.hubConn.TrySend(ackFrame, 0)
			return
		}
		h.hub.Subscribe(f.Channel, st.hubConn)
	}
	_ = h.writeFrame(ctx, c, ack)
}

// subscribeWithReplay 执行带 last_seq 的订阅（调用方已校验频道合法且非
// 重复订阅）：BeginReplay → Subscribe → ListChanges 补发 → EndReplay。
// 补发帧先入 Send；补发期间 Dispatch 到达的实时帧在 backlog 中去重
//（event_id 已在补发批）后续序刷入。nextSeq 为续传游标（R15）：扫描
// 游标优先，0 时回退末条补发事件 seq（自然耗尽时为 0——hasMore=false
// 不会消费它）。
func (h *Handler) subscribeWithReplay(ctx context.Context, st *connState, ch parsedChannel, f *inboundFrame) (int64, bool, int64, error) {
	// 门控必须先于 Subscribe：否则订阅生效到门控生效之间的 Dispatch
	// 会直接入 Send，插到补发帧之前造成乱序。
	h.hub.BeginReplay(st.hubConn)
	h.hub.Subscribe(f.Channel, st.hubConn)

	docID := ""
	if ch.docID != "" {
		docID = ch.docID
	}
	changes, hasMore, nextSinceSeq, err := h.docDB.ListChanges(ctx, st.projectID, ch.dbID, ch.collID,
		databases.ListChangesOptions{SinceSeq: f.LastSeq, DocumentID: docID, Limit: maxReplayChanges},
		st.docPrincipal)
	if err != nil {
		// 失败路径：解除订阅并保持门控清理（EndReplay 空 seen 即清空）。
		h.hub.Unsubscribe(f.Channel, st.hubConn.ID)
		h.hub.EndReplay(st.hubConn, nil)
		return 0, false, 0, err
	}

	seen := make(map[string]struct{}, len(changes))
	for i := range changes {
		change := changes[i]
		seen[change.EventID] = struct{}{}
		frame := map[string]any{
			"type":    "event",
			"channel": f.Channel,
			"payload": changePayload(st.projectID, ch, change),
		}
		if !st.hubConn.TrySend(frame, change.Seq) {
			// 补发即触发水位：连接已在断开路径上（writeLoop 会发 resync
			// close），后续补发无意义；仍完成 EndReplay 清理门控。
			break
		}
	}
	h.hub.EndReplay(st.hubConn, seen)
	nextSeq := nextSinceSeq
	if nextSeq == 0 && len(changes) > 0 {
		nextSeq = changes[len(changes)-1].Seq
	}
	return int64(len(seen)), hasMore, nextSeq, nil
}

// changePayload 把领域 Change 映射为与实时事件帧同形的 payload
//（Envelope.ClientPayload 语义：无 acl，seq/transaction_id 透出）。
func changePayload(projectID string, ch parsedChannel, c databases.DocumentChange) map[string]any {
	m := map[string]any{
		"event_id":       c.EventID,
		"event":          c.Event,
		"project_id":     projectID,
		"database_id":    ch.dbID,
		"collection_id":  ch.collID,
		"document_id":    c.DocumentID,
		"version":        c.Version,
		"created_at":     c.CreatedAt.UTC().Format(time.RFC3339),
		"truncated":      c.Truncated,
		"seq":            c.Seq,
	}
	if c.TransactionID != "" {
		m["transaction_id"] = c.TransactionID
	}
	if c.Data != nil {
		m["data"] = domainevents.DocumentPayload(c.Data)
	}
	return m
}

// channelAllowed 判定频道可订。databases：集合/文档存在且可读；
// accounts：本人（JWT sub == userId）或 platform admin（D17）；出站无 acl。
func (h *Handler) channelAllowed(ctx context.Context, st *connState, ch parsedChannel) bool {
	switch ch.kind {
	case channelKindAccounts:
		if st.platformAdmin {
			return true
		}
		return st.principal != nil && st.principal.UserID != "" && st.principal.UserID == ch.userID
	case channelKindDatabases:
		return h.databasesChannelAllowed(ctx, st, ch.dbID, ch.collID, ch.docID)
	default:
		return false
	}
}

// databasesChannelAllowed 判定集合或文档频道可订（存在且可读）。
func (h *Handler) databasesChannelAllowed(ctx context.Context, st *connState, dbID, collID, docID string) bool {
	if docID != "" {
		doc, err := h.docDB.GetDocument(ctx, st.projectID, dbID, collID, docID, st.docPrincipal)
		if err != nil {
			if isPermissionOrNotFound(err) {
				return false
			}
			h.logger.Error("realtime subscribe document check failed",
				"connection_id", st.id, "project_id", st.projectID,
				"database_id", dbID, "collection_id", collID, "document_id", docID, "error", err)
			return false
		}
		return doc != nil
	}
	coll, err := h.docDB.GetCollection(ctx, st.projectID, dbID, collID)
	if err != nil {
		h.logger.Error("realtime subscribe collection check failed",
			"connection_id", st.id, "project_id", st.projectID,
			"database_id", dbID, "collection_id", collID, "error", err)
		return false
	}
	if coll == nil || coll.IsSystem {
		return false
	}
	// Disabled 与 REST ensureReadableCollection 同口径：特权主体（system /
	// platform admin）旁路，普通主体按无权限拒绝（ListAccessDenied 语义）。
	if coll.Disabled && !st.docPrincipal.BypassesDocumentACL() {
		return false
	}
	// Round4 J5-5：补 read 权限判定。以订阅者文档主体做一次 limit=1 的列表
	// 探测——adapter 层的 listPermissionFilter 会施加与 REST List 完全相同的
	// 集合级 read 门（ListAccessDenied：!documentSecurity 且无 read 权限 →
	// PermissionDenied），堵住「仅凭存在性即可订频道」的存在性 oracle。
	// 特权主体已旁路文档 ACL，无需探测。
	if !st.docPrincipal.BypassesDocumentACL() {
		list, lerr := h.docDB.ListDocuments(ctx, st.projectID, dbID, collID,
			databases.Query{PageSize: 1}, st.docPrincipal)
		if lerr != nil {
			if isPermissionOrNotFound(lerr) {
				return false
			}
			// fail-closed：非权限类错误同样拒绝订阅并留日志。
			h.logger.Error("realtime subscribe collection read probe failed",
				"connection_id", st.id, "project_id", st.projectID,
				"database_id", dbID, "collection_id", collID, "error", lerr)
			return false
		}
		_ = list // 探测只关心是否被拒；内容为空不代表不可读（与 REST List 一致）
	}
	return true
}

// handleUnsubscribe 移除订阅（未订阅则忽略）。
func (h *Handler) handleUnsubscribe(st *connState, f *inboundFrame) {
	if _, ok := st.subs[f.Channel]; !ok {
		return
	}
	delete(st.subs, f.Channel)
	h.hub.Unsubscribe(f.Channel, st.id)
}

// expired 报告 JWT 是否已到期（expiresAt 零值恒 false；正常不会发生，
// 见 connState.expiresAt 注释）。
func (st *connState) expired() bool {
	return !st.expiresAt.IsZero() && !time.Now().Before(st.expiresAt)
}

// pongStale 报告超过 2 个 ping 间隔未收到 pong。
func pongStale(st *connState) bool {
	return time.Since(time.Unix(0, st.lastPong.Load())) > pongWindow
}

// closeWithReason 以策略违规 + 原因关闭（best-effort）。
func (h *Handler) closeWithReason(c *websocket.Conn, reason string) {
	_ = c.Close(websocket.StatusPolicyViolation, reason)
}

// writeFrame 序列化并写入一帧（带写 deadline）。
func (h *Handler) writeFrame(ctx context.Context, c *websocket.Conn, f *outboundFrame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return h.writeBytes(ctx, c, data)
}

// writeBytes 写入一帧（带写 deadline：写完即释放，不残留 conn deadline）。
func (h *Handler) writeBytes(ctx context.Context, c *websocket.Conn, payload []byte) error {
	ctxWrite, cancel := context.WithTimeout(ctx, frameWriteTimeout)
	defer cancel()
	return c.Write(ctxWrite, websocket.MessageText, payload)
}

// isPermissionOrNotFound 识别订阅校验中「无权限/不存在」类错误
// （GetDocument 失败统一 NOT_FOUND，防枚举）。
func isPermissionOrNotFound(err error) bool {
	return errors.Is(err, databases.ErrPermissionDenied) ||
		errors.Is(err, databases.ErrDocumentNotFound) ||
		strings.Contains(err.Error(), "not found")
}

// connectionRegistry 是每用户连接数配额表（4 连 / 用户）。
type connectionRegistry struct {
	mu     sync.Mutex
	counts map[string]int
}

func newConnectionRegistry() *connectionRegistry {
	return &connectionRegistry{counts: make(map[string]int)}
}

func (r *connectionRegistry) acquire(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts[key] >= MaxConnectionsPerUser {
		return false
	}
	r.counts[key]++
	return true
}

func (r *connectionRegistry) release(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts[key] <= 1 {
		delete(r.counts, key)
		return
	}
	r.counts[key]--
}
