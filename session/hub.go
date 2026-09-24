// Session Mesh Hub：本机 Session 注册表 + 多节点寻址分派 + 能力门。
//
// 设计（docs/SESSION_MESH_COLLABORATION.md §4.2）：
//   - 本机注册表：session_id → *Session（创建/关闭时由桌面层 Register/Unregister）。
//   - 统一寻址：session_send 走 Hub.Send —— 本机 sid 直接投递 Session inbox（Ask 路径）；
//     node://<node-id>/<session-id> 走远程网关（remote.NodeSender，直连或一跳转发）。
//   - 能力门：SetToolsEnabled（C1 开关）/ SetRemote（C2 开关）运行时热切换。
//   - 事件上报归工具/宿主（经 events.ToolContext / bs.emit），Hub 只负责投递，
//     不 import 桌面层（remote 包不 import session，无循环依赖）。
package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/session/remote"
)

var (
	// ErrSessionExists 会话重复注册。
	ErrSessionExists = errors.New("session already registered")
	// ErrTargetNotFound 目标会话不存在（本机注册表未命中）。
	ErrTargetNotFound = errors.New("target session not found")
	// ErrNodeOffline 目标节点不可达（远程）。
	ErrNodeOffline = errors.New("target node offline")
	// ErrRemoteDisabled 远程能力未开启。
	ErrRemoteDisabled = errors.New("remote mesh disabled")
	// ErrToolsDisabled 会话通信能力未开启。
	ErrToolsDisabled = errors.New("session communication disabled")
)

// hubDeliver 投递实现（可注入便于测试）：默认本机 Ask；远程经 gateway。
type hubDeliver func(ctx context.Context, to sessionRef, msg *remote.InboundMessage) error

// sessionRef 本机会话的注册表视图（*Session 满足；测试可用 fake）。
type sessionRef interface {
	ID() string
	IsRunning() bool
}

// Hub 管理本机 Session + 远程多节点 mesh，统一分派 session_* 工具的投递。
// 线程安全：内部 RWMutex。
type Hub struct {
	mu       sync.RWMutex
	local    map[string]sessionRef // session_id → 本机会话（注册表）
	nodeID   string                // 本机节点标识（远程寻址前缀）
	senders  remote.NodeSender     // 远程网关；nil = 未启用
	remoteOn atomic.Bool           // C2 开关
	toolsOn  atomic.Bool           // C1 开关（决定 session_* 工具可见性）

	// onDelivered 投递成功回调（桌面层注入）：本机会话收到消息后唤醒其 run loop
	// （session 包不 import 桌面层，靠回调解耦——空闲会话收到 mesh 消息也必须续跑）。
	onDelivered func(sid string)
	// titleFn 会话标题提供者（桌面层注入：取第一条 user 文本；session_list 的 Name 用）。
	titleFn func(sid string) string

	// uiLang 界面语言（桌面层按 settings.lang 注入；"zh" 或 "en"，缺省 "zh"）。
	// mesh_from 来源提示文案跟随它（i18n：模型上下文的可读性对齐用户语言）。
	uiLang string

	// deliver 本机投递实现（默认 Ask；测试可注入 fake）。
	deliver func(ctx context.Context, to sessionRef, msg *remote.InboundMessage) error
}

// NewHub 创建 Session Mesh Hub。nodeID 为本机节点标识
// （"auto" = 由宿主解析持久化 UUID 后传入；Hub 不落盘）。
func NewHub(nodeID string) *Hub {
	h := &Hub{
		local:  make(map[string]sessionRef),
		nodeID: nodeID,
		uiLang: "zh", // 缺省中文（产品默认界面语言）
	}
	h.deliver = func(ctx context.Context, to sessionRef, msg *remote.InboundMessage) error {
		s, ok := to.(*Session)
		if !ok {
			return fmt.Errorf("deliver: not a *Session")
		}
		// 来源提示（独立块，非正文）：接收方模型需要知道消息从哪来、如何回信——
		// 但来源不是用户原话，不能混进对话正文（否则污染标题/UI/上下文语义）。
		// mesh_from 块 provider 翻译时按文本送 LLM（模型可见、可回信寻址），
		// 而 collectUserTexts/标题 fallback/UI 只认 text 块 → 正文保持纯净。
		var blocks []core.Content
		if msg.FromNode != "" {
			origin := msg.FromNode
			if msg.FromSession != "" {
				origin = msg.FromNode + "/" + msg.FromSession
			}
			if msg.FromName != "" && msg.FromName != msg.FromSession {
				origin = origin + "（" + msg.FromName + "）"
			}
			// i18n（跟随界面语言）：本机互发 / 远程投递两套文案
			localFrom, remoteFrom := "来自会话", "来自远程会话"
			if h.lang() == "en" {
				localFrom, remoteFrom = "From session", "From remote session"
			}
			// 本机互发：[来自会话 node-a/sA1（标题）]\n；远程：[来自远程会话 node://node-a/sA1（标题）]\n
			// 尾部换行：模型上下文里来源提示与正文分属两行（部分端点把多文本块拼成单串时
			// 无分隔会粘连成"[来自...]正文"一句，可读性差；显式 \n 保证分行）。
			from := fmt.Sprintf("[%s %s]\n", localFrom, origin)
			if msg.FromNode != h.nodeID {
				from = fmt.Sprintf("[%s node://%s]\n", remoteFrom, origin)
			}
			// 接收方身份提示：标明本会话 sid（自动建会话等场景模型易混淆"我是谁"——
			// 实测新会话把自己误认成同节点的既有会话）。UI 不渲染此块，仅模型可见。
			if h.lang() == "en" {
				from += fmt.Sprintf("(you are in session %s)\n", s.ID())
			} else {
				from += fmt.Sprintf("（你所在的会话是 %s）\n", s.ID())
			}
			blocks = append(blocks, core.Content{Type: core.ContentTypeMeshFrom, Content: from})
		}
		// 正文块：纯净的发送内容（用户原话/模型正文），不带来源前缀
		blocks = append(blocks, core.Content{Type: core.ContentTypeText, Content: msg.Body})
		err := s.Ask(ctx, core.NewUserMessage(blocks...))
		return err
	}
	return h
}

// SetUILang 设置界面语言（桌面层注入：settings.lang；"en" 之外一律按中文处理）。
// 影响 mesh_from 来源提示的文案语言（i18n）。
func (h *Hub) SetUILang(lang string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if lang == "en" {
		h.uiLang = "en"
	} else {
		h.uiLang = "zh"
	}
}

// lang 返回当前界面语言（锁内读）。
func (h *Hub) lang() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.uiLang
}

// SetOnDelivered 注入「投递成功」回调（桌面层在创建 Hub 后接线）：
// 消息入队成功后调用（参数 = 目标本机会话 id），宿主据此 wakeRun 让空闲会话立即续跑。
// nil = 不回调（纯 SDK 场景 / 测试）。
func (h *Hub) SetOnDelivered(fn func(sid string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onDelivered = fn
}

// SetTitleProvider 注入会话标题提供者（桌面层接线：取第一条 user 文本）。
// session_list 输出的本机项 Name 用它填充（远程项由对端广播自带 Name）。
// nil = 不填标题（缺省：ID 即显示名）。
func (h *Hub) SetTitleProvider(fn func(sid string) string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.titleFn = fn
}

// afterDelivered 投递成功后的宿主通知（唤醒目标会话 run loop）。
func (h *Hub) afterDelivered(sid string) {
	h.mu.RLock()
	fn := h.onDelivered
	h.mu.RUnlock()
	if fn != nil {
		fn(sid)
	}
}

// SetToolsEnabled C1 开关（热更新）：true = session_* 工具可用；false = 拒绝投递。
// 注意：工具注册由桌面层按开关决定（newSessionEngine），Hub 侧再挡一层防误调。
func (h *Hub) SetToolsEnabled(on bool) { h.toolsOn.Store(on) }

// ToolsEnabled 当前 C1 开关状态。
func (h *Hub) ToolsEnabled() bool { return h.toolsOn.Load() }

// SetRemote C2 开关（热更新）：启动/停止远程网关。
// senders nil = 关闭远程（不投递 node://）；cfg 用于 Gateway 启动（桌面层接线）。
// 关闭时同时清 remoteOn；node:// 投递返回 ErrRemoteDisabled。
func (h *Hub) SetRemote(on bool, s remote.NodeSender) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if on {
		h.senders = s
		h.remoteOn.Store(true)
	} else {
		h.senders = nil
		h.remoteOn.Store(false)
	}
}

// RemoteEnabled 当前 C2 开关状态。
func (h *Hub) RemoteEnabled() bool { return h.remoteOn.Load() }

// NodeID 返回本机节点标识。
func (h *Hub) NodeID() string { return h.nodeID }

// Register 注册本机会话（创建时桌面层调用）。重复注册报 ErrSessionExists。
func (h *Hub) Register(s sessionRef) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.local[s.ID()]; ok {
		return ErrSessionExists
	}
	h.local[s.ID()] = s
	return nil
}

// Unregister 注销本机会话（关闭时桌面层调用）。幂等（不存在不报错）。
func (h *Hub) Unregister(sid string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.local, sid)
}

// Get 按本机 session id 取 Session（未命中返回 nil）。
func (h *Hub) Get(sid string) *Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, _ := h.local[sid].(*Session)
	return s
}

// List 列出可寻址会话：本机（online）+ 远程已知节点在线会话（经网关）。
// 本机项 Name = 标题提供者结果（桌面层接第一条 user 文本；nil 提供者 = 空）。
// **稳定排序**（index 寻址前提）：本机会话在前（node == 本机），远程在后；
// 组内按 (node, id) 字典序。任何时刻调用结果顺序一致——session_list 输出 index
// 供模型用 #N 快捷寻址（session_send 按当前列表实时解析，不缓存）。
func (h *Hub) List(ctx context.Context) []remote.SessionInfo {
	now := time.Now()
	h.mu.RLock()
	local := make([]remote.SessionInfo, 0, len(h.local))
	titleFn := h.titleFn
	for id, s := range h.local {
		st := "online"
		if s.IsRunning() {
			st = "busy"
		}
		si := remote.SessionInfo{
			Node:     h.nodeID,
			ID:       id,
			Status:   st,
			LastSeen: now,
		}
		if titleFn != nil {
			si.Name = titleFn(id)
		}
		local = append(local, si)
	}
	senders := h.senders
	h.mu.RUnlock()

	out := local
	if senders != nil {
		for _, n := range senders.KnownNodes(ctx) {
			for _, si := range n.Sessions {
				si.Node = n.ID
				out = append(out, si)
			}
		}
	}
	// 稳定排序：本机在前（node == h.nodeID），组内 (node,id) 字典序
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := out[i].Node == h.nodeID, out[j].Node == h.nodeID
		if li != lj {
			return li // 本机恒在前
		}
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ---------- LLM 可见短标识（#N）视图 ----------

// llmNodeRef 一个可寻址节点的短标识视图：真实 node id ↔ 节点级编号 #K。
// #0 恒为本机；远程节点按 id 字典序编号（稳定；节点离线后编号可能前移——模型需重查）。
type llmNodeRef struct {
	RealID string // 真实节点 id（本机 = nodeID；远程 = 对端 node id）
	Ref    string // "#0", "#1", ...
}

// llmSessionRef 一个可寻址会话的短标识视图：真实 (node,sid) ↔ 全局会话编号 #N。
// #N = List 稳定序（本机会话在前、组内字典序；远程跟随节点字典序 + 组内字典序）。
type llmSessionRef struct {
	NodeRef string // 所属节点短标识 "#K"
	RealID  string // 真实会话 id
	Ref     string // "#N"
}

// sessionRefs 返回全部可寻址会话的稳定序 + 短标识（节点 #K 序 + 组内字典序）。
// 一次遍历同时产出节点短标识映射（本机 #0 + 远程按 id 字典序）。
func (h *Hub) sessionRefs(ctx context.Context) []llmSessionRef {
	items := h.List(ctx) // 已稳定排序（本机前，组内字典序）
	// 预建 node → #K 映射（与 List 排序同规则：本机 #0；远程按 id 字典序从 #1）
	nodeRef := map[string]string{h.nodeID: "#0"}
	var remotes []string
	seen := map[string]bool{}
	for _, it := range items {
		if it.Node != h.nodeID && !seen[it.Node] {
			seen[it.Node] = true
			remotes = append(remotes, it.Node)
		}
	}
	sort.Strings(remotes)
	for i, id := range remotes {
		nodeRef[id] = fmt.Sprintf("#%d", i+1)
	}
	refs := make([]llmSessionRef, 0, len(items))
	for i, it := range items {
		refs = append(refs, llmSessionRef{
			NodeRef: nodeRef[it.Node],
			RealID:  it.ID,
			Ref:     fmt.Sprintf("#%d", i),
		})
	}
	return refs
}

// nodeRefs 返回本机 + 全部已知远程节点的稳定序（本机 #0 恒在前；远程按 id 字典序）。
// 注意：与会话列表可见的 node 短标识同规则（sessionRefs 内部也用它推导）。
func (h *Hub) nodeRefs(ctx context.Context) []llmNodeRef {
	nodes := []llmNodeRef{{RealID: h.nodeID, Ref: "#0"}}
	if senders := h.senders; senders != nil {
		var remotes []string
		for _, n := range senders.KnownNodes(ctx) {
			if n.ID != "" && n.ID != h.nodeID {
				remotes = append(remotes, n.ID)
			}
		}
		sort.Strings(remotes)
		for i, id := range remotes {
			nodes = append(nodes, llmNodeRef{RealID: id, Ref: fmt.Sprintf("#%d", i+1)})
		}
	}
	return nodes
}

// RealTarget 把 LLM 短标识解析为真实寻址；同时兼容真实 sid / node:// 直传。
// 支持的短标识形态：
//   - "#N"：会话（本机或远程，按全局稳定序）→ 本机 sid 或 node://<node>/<sid>
//   - "node://#K/#N"：远程会话（K = 节点短标识，N = 全局会话序）→ node://<node>/<sid>
//   - "node://#K"：远程节点自动建会话 → node://<node>（Send 走自动建分支）
func (h *Hub) RealTarget(ctx context.Context, target string) (string, error) {
	if !strings.HasPrefix(target, "#") && !strings.HasPrefix(target, "node://#") {
		return target, nil // 已是真实地址（向后兼容）
	}
	// node://#K[/#N] → 远程
	if strings.HasPrefix(target, "node://#") {
		rest := strings.TrimPrefix(target, "node://") // "#K" 或 "#K/#N"
		slash := strings.IndexByte(rest, '/')
		nodeRef := rest
		sessPart := "" // "#N" 或 ""
		if slash >= 0 {
			nodeRef, sessPart = rest[:slash], rest[slash+1:]
		}
		realNode := ""
		for _, nr := range h.nodeRefs(ctx) {
			if nr.Ref == nodeRef {
				realNode = nr.RealID
				break
			}
		}
		if realNode == "" {
			return "", fmt.Errorf("unknown node ref %q (re-run session_list)", nodeRef)
		}
		if sessPart == "" {
			return "node://" + realNode, nil // 自动建会话
		}
		// #K/#N：N 是全局会话序——从列表找第 N 项并校验其 node 短标识 == #K
		idx, err := strconv.Atoi(strings.TrimPrefix(sessPart, "#"))
		if err != nil || idx < 0 {
			return "", fmt.Errorf("invalid session ref %q", sessPart)
		}
		items := h.List(ctx)
		if idx >= len(items) {
			return "", fmt.Errorf("session ref #%d out of range (current list has %d sessions; re-run session_list)", idx, len(items))
		}
		it := items[idx]
		if it.Node != realNode {
			return "", fmt.Errorf("session #%d is on node %s not %s (re-run session_list)", idx, it.Node, realNode)
		}
		return "node://" + realNode + "/" + it.ID, nil
	}
	// #N → 会话（本机或远程，全局序）
	idxStr := strings.TrimPrefix(target, "#")
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		return "", fmt.Errorf("invalid ref %q (want #N / node://#K/#N / node://#K)", target)
	}
	items := h.List(ctx)
	if idx >= len(items) {
		return "", fmt.Errorf("session ref %s out of range (current list has %d sessions; re-run session_list)", target, len(items))
	}
	it := items[idx]
	if it.Node == h.nodeID {
		return it.ID, nil
	}
	return "node://" + it.Node + "/" + it.ID, nil
}

// ResolveTarget 解析 session_send 的 target 参数（兼容新旧两种寻址）：
//   - "#N" / "node://#K"：LLM 短标识 → 真实寻址（见 RealTarget）
//   - 其它：原样返回（真实 sid / node:// 地址，向后兼容）
func (h *Hub) ResolveTarget(ctx context.Context, target string) (string, error) {
	return h.RealTarget(ctx, target)
}

// Send 统一寻址入口（session_send 调这里）。
// target 本机 sid → 直接投递；node://<node-id>/<session-id> → 远程网关路由；
// node://<node-id>（无 sid）→ 请求目标节点自动创建新会话。
// 返回 createdSID：远程自动建会话时 = 目标新建的会话 id（否则空串）。
// from 为来源信息（工具构造时注入当前会话）。
func (h *Hub) Send(ctx context.Context, target, body string, from remote.InboundMessage) (createdSID string, err error) {
	if !h.toolsOn.Load() {
		return "", ErrToolsDisabled
	}
	from.Body = body
	from.SendAt = time.Now()
	if from.MsgID == "" {
		from.MsgID = newMsgID()
	}

	// node:// 远程寻址
	if strings.HasPrefix(target, "node://") {
		return h.sendRemote(ctx, target, from)
	}
	// 本机 sid
	h.mu.RLock()
	s, ok := h.local[target]
	h.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrTargetNotFound, target)
	}
	if err := h.deliver(ctx, s, &from); err != nil {
		return "", err
	}
	h.afterDelivered(s.ID()) // 投递成功 → 宿主唤醒（空闲会话立即续跑）
	return "", nil
}

// sendRemote 远程投递：解析 node://<node-id>[/<sid>]，经网关路由。
// 无 sid（node://<node-id>）→ 请求目标节点自动创建新会话，返回其 id。
// 并发安全：senders 在锁内读取并校验 remoteOn，避免与 SetRemote 竞态
// （读到 nil senders 视为远程关闭）。
func (h *Hub) sendRemote(ctx context.Context, target string, from remote.InboundMessage) (string, error) {
	rest := strings.TrimPrefix(target, "node://")
	nodeID := rest
	sid := ""
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		if slash == 0 || slash == len(rest)-1 {
			return "", fmt.Errorf("invalid remote target %q (want node://<node-id> or node://<node-id>/<session-id>)", target)
		}
		nodeID, sid = rest[:slash], rest[slash+1:]
	}
	if nodeID == "" {
		return "", fmt.Errorf("invalid remote target %q (want node://<node-id> or node://<node-id>/<session-id>)", target)
	}

	h.mu.RLock()
	senders := h.senders
	on := h.remoteOn.Load()
	h.mu.RUnlock()
	if !on || senders == nil {
		return "", ErrRemoteDisabled
	}
	from.SessionId = sid
	return senders.SendMessage(ctx, nodeID, sid, &from)
}

// nodeIDSuffix 供工具层展示用（取 node:// 的节点段）。
func nodeIDSuffix(target string) string {
	if !strings.HasPrefix(target, "node://") {
		return ""
	}
	rest := strings.TrimPrefix(target, "node://")
	if i := strings.IndexByte(rest, '/'); i > 0 {
		return rest[:i]
	}
	return rest
}

// 消息 id 生成（时间戳+随机，进程内唯一；对齐 tooltask 的随机风格）。
func newMsgID() string {
	return fmt.Sprintf("m-%d-%x", time.Now().UnixNano(), time.Now().UnixNano()&0xffff)
}
