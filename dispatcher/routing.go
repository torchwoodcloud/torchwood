package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 执行路由层（四期 4a-2，设计 docs/design/functions-runtimes-and-sources.md
// §4 M3 实例亲和 + M7 local 模式语义；registry 模式的冷启动差异在 4b 补齐）。
//
// local 模式铁律（M7）：镜像不分发——函数镜像只存在于其构建节点上，因此
//  1. 绝不在本节点 spawn「镜像在别处」的函数（跨节点手段只有转发）；
//  2. 有实例 → 实例亲和（实例终生属于 spawn 它的节点，4a-1 固化 rec.Node）；
//  3. 无实例 → BuildNode 冷启动亲和：BuildNode 非空且 ≠ self → 转发构建
//     节点（镜像只在那儿）；BuildNode 空 / == self / 指向已失联节点
//     （fnnodes 查无）→ 本地 spawn 回落（单机与 self-build 场景天然回落；
//     镜像确实不在本节点时 spawn 自然失败并留现场，不吞错误）。
//
// registry 模式差异（四期 4b M1）：镜像全局化后决策 3 的冷启动半边不再
// 转发——无实例时直接 handled=false 走本地池路径（spawn 前 EnsureImage
// 按需 pull，任何节点都能拉到全局镜像，跨节点冷启动自愈就地完成）；
// 实例亲和转发（决策 2/3 的转发半边）保持不变。
//
// 防环（M3）：转发请求携带 X-Tw-Forwarded-For-Node: <转发方节点 ID>；目标
// 节点收到带该 header 的请求强制本地池路径，不得再转发——转发发起方已按
// 实例亲和/BuildNode 语义选定目标为镜像所在节点，二次转发只会指向没有
// 镜像的第三方节点（且 URL 误配回指时可能成环）。
//
// 错误语义（local 模式）：转发一旦发起即收场——目标不可达/超时以
// Unavailable/DeadlineExceeded 明确错误收场，不静默回落本地 spawn（本节点
// 没有镜像，回落必败且会掩盖真实故障；local 模式下回落无意义）。
const (
	// forwardedForNodeHeader 是节点间转发的防环标记 header（值 = 转发方
	// 节点 ID）：目标节点据此强制本地处理。
	forwardedForNodeHeader = "X-Tw-Forwarded-For-Node"

	// forwardExecutionsPath 是节点间执行转发的目标端点（复用 dispatcher
	// 既有 executions 面，载荷与直连请求完全同形）。
	forwardExecutionsPath = "/v1/dispatch/executions"

	// maxForwardResponseBytes 是节点转发响应的读取上限（与 server 侧
	// DispatcherExecutor 的 executions 响应上限同口径：fetch 封套 =
	// stdout/stderr 尾部 + 无损 body_base64 + headers ≈ 230KB 量级，取 1MB）。
	maxForwardResponseBytes = 1 << 20

	// forwardDialTimeout 是节点间转发的拨号超时（内网对等互达正常 <10ms；
	// 目标节点进程已死但心跳尚未过期的窗口内由此快速失败）。
	forwardDialTimeout = 3 * time.Second
)

// nodeForwarder 把执行请求转发到目标节点的 dispatcher HTTP 面（M3 路由层
// 的跨节点手段，local 模式下唯一的跨节点通路）。
type nodeForwarder interface {
	// Forward 把完整 ExecuteRequest 透传给目标节点并原样回传其
	// ExecuteResponse；传输失败 / 目标非 2xx 返回映射后的 grpc status 错误
	//（调用方 pool.forward 据此以明确错误收场，不回落本地）。
	Forward(ctx context.Context, target NodeRecord, req ExecuteRequest) (*ExecuteResponse, error)
}

// httpNodeForwarder 是 nodeForwarder 的真实实现：POST {node.url} +
// forwardExecutionsPath，JSON 载荷 = 完整 ExecuteRequest（透传语义：目标
// 节点的 Dispatch/DispatchForwarded 走与直连请求完全相同的池路径，执行
// 结果可完整还原）。header 带防环标记（X-Tw-Forwarded-For-Node: <self>）
// 与可选共享 token（节点间鉴权与 server→dispatcher 同一面：config
// functions.dispatcher.shared_token，同一部署内各节点同值）。
//
// 超时模型：拨号 forwardDialTimeout；无整体 client.Timeout——执行时长由
// 请求 ctx（调用方函数超时）控制，长执行不得被转发层提前截断。
type httpNodeForwarder struct {
	hc          *http.Client
	selfNodeID  string
	sharedToken string
}

// newNodeForwarder 构造节点转发客户端（service 装配处调用）。
func newNodeForwarder(selfNodeID, sharedToken string) *httpNodeForwarder {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // 内网对等直连，不走代理
	tr.DialContext = (&net.Dialer{Timeout: forwardDialTimeout}).DialContext
	return &httpNodeForwarder{
		hc:          &http.Client{Transport: tr},
		selfNodeID:  selfNodeID,
		sharedToken: sharedToken,
	}
}

func (f *httpNodeForwarder) Forward(ctx context.Context, target NodeRecord, req ExecuteRequest) (*ExecuteResponse, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal forwarded execution: %v", err)
	}
	url := strings.TrimRight(target.URL, "/") + forwardExecutionsPath
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build forward request: %v", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	// 防环标记（M3）：目标节点据此强制本地处理，不得再转发。
	hreq.Header.Set(forwardedForNodeHeader, f.selfNodeID)
	if f.sharedToken != "" {
		hreq.Header.Set("X-Tw-Dispatcher-Token", f.sharedToken)
	}
	resp, err := f.hc.Do(hreq)
	if err != nil {
		// 调用方超时/取消原样透传 DeadlineExceeded（上层 runExecution 依赖
		// 该判定，与 executeOn 的超时分类同口径）；其余传输失败（连接拒绝/
		// 拨号超时/reset）= 目标节点不可达 → Unavailable。
		if ctx.Err() != nil || isTimeoutErr(err) {
			return nil, status.Errorf(codes.DeadlineExceeded,
				"forward execution to node %s timed out: %v", target.NodeID, err)
		}
		return nil, status.Errorf(codes.Unavailable,
			"forward execution to node %s (%s) failed: %v", target.NodeID, target.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxForwardResponseBytes))
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"read forward response from node %s: %v", target.NodeID, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 目标节点的 writeError 状态码映射之逆映射（与 DispatcherExecutor.do
		// 的还原逻辑同构）：429/504/400/401/403 语义保真，其余 Internal。
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &errBody)
		msg := errBody.Error
		if msg == "" {
			msg = fmt.Sprintf("http %d", resp.StatusCode)
		}
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			return nil, status.Errorf(codes.ResourceExhausted, "node %s: %s", target.NodeID, msg)
		case http.StatusGatewayTimeout:
			return nil, status.Errorf(codes.DeadlineExceeded, "node %s: %s", target.NodeID, msg)
		case http.StatusBadRequest:
			return nil, status.Errorf(codes.InvalidArgument, "node %s: %s", target.NodeID, msg)
		case http.StatusPreconditionFailed:
			// 镜像缺失类型化错误（rebuild 链路）语义保真转发回源节点。
			return nil, status.Errorf(codes.FailedPrecondition, "node %s: %s", target.NodeID, msg)
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, status.Errorf(codes.PermissionDenied, "node %s authentication failed", target.NodeID)
		default:
			return nil, status.Errorf(codes.Internal, "node %s error (http %d): %s", target.NodeID, resp.StatusCode, msg)
		}
	}
	var out ExecuteResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, status.Errorf(codes.Internal,
			"decode forward response from node %s: %v", target.NodeID, err)
	}
	return &out, nil
}

// route 决定一次执行请求的落点（M3 实例亲和 + M7 local 冷启动语义）。
// 返回 handled=true 表示请求已在路由层收场（转发成功或转发明确失败）——
// 调用方直接返回其结果；handled=false 走本地池路径（与路由层引入前的
// 单机行为完全一致）。
//
// 决策表（local 模式；按序求值，deployment 维度收窄——旧 deployment 的
// 过渡态实例不参与本次路由）：
//  1. 本节点存在该 deployment 实例（含旧记录 rec.Node==""，与
//     ownedBySelf 同口径）→ 本地处理（现状不变）；
//  2. 他节点存在该 deployment 实例且节点在册（心跳未过期）→ 转发该节点
//     （实例亲和；实例终生属于 spawn 它的节点，其容器 IP 仅在其节点
//     docker 网络内可达，本地认领必然不可达）；
//  3. 他节点实例但节点已失联（fnnodes 查无）→ 记录视为待收敛的孤儿并顺手
//     删除（M8 ②同证据同动作），继续走冷启动规则（其容器随机器消失）；
//  4. 无可用实例 + BuildNode 非空且 ≠ self 且节点在册 → 转发 BuildNode
//     （M7 铁律：镜像只在其构建节点上）；
//  5. 其余（BuildNode 空 / == self / 指向已失联节点）→ 本地 spawn 回落
//     （单机与 self-build 天然回落；BuildNode 指向失联节点且镜像不在本
//     节点时 spawn 自然失败并留现场——错误经 lastSpawnError 进队首超时
//     消息，不吞）。
func (p *PoolManager) route(ctx context.Context, req ExecuteRequest) (*ExecuteResponse, bool, error) {
	ref := FunctionRef{ProjectID: req.ProjectID, FunctionID: req.FunctionID}
	records, err := p.registry.List(ctx, ref)
	if err != nil {
		return nil, false, err
	}
	hasLocal := false
	var heRecs []InstanceRecord
	for i := range records {
		rec := records[i]
		if rec.DeploymentID != req.DeploymentID {
			continue // 旧 deployment 的实例：换版/drain 过渡态，不参与路由
		}
		if p.ownedBySelf(rec) {
			hasLocal = true
			continue
		}
		heRecs = append(heRecs, rec)
	}
	if hasLocal {
		return nil, false, nil
	}
	// 实例亲和（多他节点时取稳定序——List 为 map 投影，顺序不定）。
	heNodes := heNodesOf(heRecs)
	sort.Strings(heNodes)
	for _, nodeID := range heNodes {
		node, err := p.registry.GetNode(ctx, nodeID)
		if err != nil {
			return nil, false, err
		}
		if node == nil {
			// 决策 3：节点已失联（心跳键消失，GetNode 确定性 miss）——
			// 其记录视为孤儿，顺手收敛后走冷启动规则。收敛与 reaper M8 ②
			// 同证据同动作（键消失 = 节点已死，只删记录不碰 daemon），只是
			// 提前到请求时点：不删则本地 ClaimIdle（按 deployment 匹配、
			// 不识节点）会认领孤儿记录，把请求浪费在必然不可达的容器 IP 上
			//（真实故障被「resident instance failed: dial tcp …」掩盖，
			// 自愈多绕一圈）。
			p.dropOrphanRecords(ctx, ref, nodeID, heRecs)
			continue
		}
		resp, err := p.forward(ctx, *node, req)
		return resp, true, err
	}
	// 冷启动亲和（决策 4/5；local 模式专属——M7 铁律「镜像不分发」的冷
	// 启动半边）。registry 模式（四期 4b M1）镜像全局化：任意节点都能从
	// registry pull 到构建产物，冷启动不再转发 BuildNode——直接 handled=false
	// 走本地池路径（spawnInstance 在 spawn 前 EnsureImage 按需 pull， miss
	// 即拉，跨节点自愈就地完成）；实例亲和（决策 2/3）不受模式影响，照旧
	// 转发——实例终生属于 spawn 它的节点，其容器 IP 仅在其节点 docker 网络
	// 内可达，与镜像分布无关。
	if req.BuildNode != "" && req.BuildNode != p.nodeID &&
		p.cfg.RoutingMode != config.FunctionsRoutingModeRegistry {
		node, err := p.registry.GetNode(ctx, req.BuildNode)
		if err != nil {
			return nil, false, err
		}
		if node != nil {
			resp, err := p.forward(ctx, *node, req)
			return resp, true, err
		}
		// 决策 5：fnnodes 查无 → 本地 spawn 回落（单机 node_id 变更——
		// hostname 缺省跨重启变化——场景的存续通道）。
	}
	return nil, false, nil
}

// heNodesOf 去重提取他节点 ID 列表。
func heNodesOf(recs []InstanceRecord) []string {
	seen := make(map[string]bool, len(recs))
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		if !seen[rec.Node] {
			seen[rec.Node] = true
			out = append(out, rec.Node)
		}
	}
	return out
}

// dropOrphanRecords 删除失联节点（deadNode）在本函数池内的孤儿实例记录
// （M8 ② 死节点收敛的请求时 opportunistic 版；orphan 为本 route 已读取的
// 记录快照，只删其中归属 deadNode 的条目）。
func (p *PoolManager) dropOrphanRecords(ctx context.Context, ref FunctionRef, deadNode string, orphan []InstanceRecord) {
	dropped := 0
	for _, rec := range orphan {
		if rec.Node != deadNode {
			continue
		}
		if err := p.registry.Delete(ctx, ref, rec.InstanceID); err == nil {
			dropped++
		}
	}
	if dropped > 0 {
		DeadNodeReclaimedTotal.WithLabelValues(ref.ProjectID, ref.FunctionID).Add(float64(dropped))
		slog.Warn("dispatcher: dropped orphan instance records of a dead node",
			"project", ref.ProjectID, "function", ref.FunctionID, "node", deadNode, "count", dropped)
	}
}

// forward 经 forwarder 把请求转发到目标节点（决策 2/4 的跨节点手段）。
// 转发一旦发起即收场：成功回传目标响应，失败以明确 status 错误收场——
// 不回落本地池（local 模式下本节点无镜像，回落必败且掩盖根因）。
func (p *PoolManager) forward(ctx context.Context, target NodeRecord, req ExecuteRequest) (*ExecuteResponse, error) {
	if p.forwarder == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"node forwarding is not configured (dispatcher node forwarder missing)")
	}
	slog.Debug("dispatcher: forwarding execution to peer node",
		"project", req.ProjectID, "function", req.FunctionID, "deployment", req.DeploymentID,
		"self", p.nodeID, "target_node", target.NodeID, "target_url", target.URL)
	resp, err := p.forwarder.Forward(ctx, target, req)
	result := "ok"
	if err != nil {
		result = "error"
	}
	DispatchForwardedTotal.WithLabelValues(req.ProjectID, req.FunctionID, target.NodeID, result).Inc()
	return resp, err
}
