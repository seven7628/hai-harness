// Session Mesh 工具面：session_send / session_list。
//
// 设计（docs/SESSION_MESH_COLLABORATION.md §4.3）：对齐 agentSend 架构——
// 工具 CanParallel=true；本机与远程共用一套工具（寻址在 Hub 层分派）；
// 发送成功经 events.ToolContext.Handler 发 TaskMessage 送达事件（审计）。
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/session/remote"
	"github.com/seven7628/hai-harness/tools"
)

// MeshToolNames 全部 mesh 工具名。当前热移除走 rebuildAll（Engine 无 UnregisterTool，
// newSessionEngine 按开关决定注册），故本清单暂为「声明性契约」——若未来 Engine 提供
// UnregisterTool 精确热移除，此处即移除清单；也供测试/文档断言工具面。
var MeshToolNames = []string{"session_send", "session_list"}

// NewMeshTools 创建 Session Mesh 工具集（session_send / session_list）。
// hub 为会话级共享 Hub；self 返回当前会话（取 sid/name 用；nil = 无来源标注）。
// 返回的工具逐个注册进 ToolEngine（仅 session_enabled 时由桌面层注册）。
func NewMeshTools(hub *Hub, self func() *Session) []tools.Tool {
	return []tools.Tool{
		&sessionSendTool{hub: hub, self: self},
		&sessionListTool{hub: hub, self: self},
	}
}

// ---------- session_send ----------

type sessionSendTool struct {
	hub  *Hub
	self func() *Session
}

type sessionSendArgs struct {
	Target string `json:"target"`
	Body   string `json:"body"`
}

func (t *sessionSendTool) Name() string { return "session_send" }
func (t *sessionSendTool) Description() string {
	return "Send a message TO another session. " +
		"Target is a SHORT REFERENCE (never a raw uuid): \"#N\" (session index from session_list, local or remote), " +
		"\"node://#K/#N\" (remote session), or \"node://#K\" (remote node, auto-creates a NEW session whose id is returned in created_session). " +
		"References are resolved server-side against the CURRENT session list — if a ref is stale (out of range), re-run session_list and retry. " +
		"One-way, asynchronous: the target session receives it as a new user message at its next run boundary; " +
		"this tool does NOT return the target's reply. Requires 'Session communication' enabled in Base Settings."
}
func (t *sessionSendTool) Parameters() any {
	return tools.Obj(map[string]any{
		"target": tools.Str("Target short ref: #N (session), node://#K/#N (remote session), node://#K (remote node auto-create)"),
		"body":   tools.Str("Message body to send (unstructured text)"),
	}, "target", "body")
}
func (t *sessionSendTool) CanParallel() bool { return true }

func (t *sessionSendTool) ValidParams(_ context.Context, _, arguments string) error {
	var a sessionSendArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("session_send: %w", err)
	}
	if a.Target == "" || a.Body == "" {
		return fmt.Errorf("session_send: target and body are required")
	}
	return nil
}

func (t *sessionSendTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (t *sessionSendTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a sessionSendArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	// #N 快捷寻址：按 session_list 当前顺序解析为真实 target（实时列表，防过期 index）
	target, err := t.hub.ResolveTarget(ctx, a.Target)
	if err != nil {
		return "", fmt.Errorf("session_send: %w", err)
	}
	a.Target = target
	// 来源信息：当前会话（sid/node/name）
	from := remote.InboundMessage{
		FromNode: t.hub.NodeID(),
	}
	if t.self != nil {
		if s := t.self(); s != nil {
			from.FromSession = s.ID()
			// FromName = 本会话 AI 标题（session_list 的 Title 同源）：接收方据此识别
			// 是谁发来的（模型上下文/UI 展示），比裸 sid 可读。无标题 = 空（兼容旧行为）。
			if name := s.AITitle(); name != "" {
				from.FromName = name
			}
		}
	}
	// 事件信封：RunId 关联来源运行（回信/审计/UI 归树）
	if tc := events.ToolContextFrom(ctx); tc != nil {
		from.RunId = tc.ParentRunId
	}
	created, err := t.hub.Send(ctx, a.Target, a.Body, from)
	if err != nil {
		return "", fmt.Errorf("session_send: %w", err)
	}
	// TaskMessage 事件（审计：谁在什么时机发了什么；对齐 agent_send）
	if tc := events.ToolContextFrom(ctx); tc != nil && tc.Handler != nil {
		tc.Handler(ctx, &events.TaskMessage{
			RunId:     tc.ParentRunId,
			TaskId:    a.Target,
			Message:   a.Body,
			Timestamp: time.Now(),
			EventType: events.TaskMessageType,
		})
	}
	// 无 sid 自动建会话（node://<node> 不带 sid）：目标新建会话 id 回给模型
	if created != "" {
		return fmt.Sprintf(`{"target":%q,"status":"delivered","created_session":%q}`, a.Target, created), nil
	}
	return fmt.Sprintf(`{"target":%q,"status":"delivered"}`, a.Target), nil
}

func (t *sessionSendTool) AfterCall(context.Context, core.ToolCall) {}

// ---------- session_list ----------

type sessionListTool struct {
	hub  *Hub
	self func() *Session // 当前会话（标注 self 条目，帮模型避开自己）
}

func (t *sessionListTool) Name() string { return "session_list" }
func (t *sessionListTool) Description() string {
	return "List all reachable sessions with SHORT references (node/id shown as #K/#N — raw uuids are hidden). " +
		"Each entry includes node, id, target, name (conversation title), status (online/busy); self=true marks the CURRENT session (you). " +
		"Use the entry's `target` VERBATIM as session_send's target (#N local, node://#K/#N remote, node://#K to auto-create on a node). " +
		"Do NOT message the self=true entry unless you intend to message yourself. " +
		"References are only valid for the CURRENT list — after sessions change, re-run this tool."
}
func (t *sessionListTool) Parameters() any {
	return tools.Obj(map[string]any{})
}
func (t *sessionListTool) CanParallel() bool { return true }

func (t *sessionListTool) ValidParams(context.Context, string, string) error { return nil }
func (t *sessionListTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

// sessionListEntry session_list 单条输出的 LLM 视图：**只暴露短标识 #N/#K**，
// 真实 node_id/session_id 由服务端内部映射（模型无需接触/复制长 id，省 token 且防错）。
type sessionListEntry struct {
	// Node 节点短标识（本机 = "#0"；远程按 id 字典序 "#1"、"#2"...）。
	Node string `json:"node"`
	// ID 会话短标识（全局稳定序 "#0"、"#1"...，与 List 排序一致）。
	ID string `json:"id"`
	// Name 会话标题（AI 生成或首条正文；空 = 无）。
	Name string `json:"name,omitempty"`
	// Status online / busy。
	Status string `json:"status"`
	// Target session_send 的 target 取值（同样短标识：本机 "#N"；远程 "node://#K/#N"）。
	// 服务端实时解析（会话增删后编号可能变化，用前先重查列表）。
	Target string `json:"target"`
	// Self true = 当前调用会话自己（模型发消息时通常应选其它条目）。
	Self bool `json:"self,omitempty"`
}

func (t *sessionListTool) Call(ctx context.Context, _, _ string) (string, error) {
	// sessionRefs 与 List 同序；一次遍历产出展示 + target
	refs := t.hub.sessionRefs(ctx)
	items := t.hub.List(ctx)
	selfID := ""
	if t.self != nil {
		if s := t.self(); s != nil {
			selfID = s.ID()
		}
	}
	out := make([]sessionListEntry, 0, len(refs))
	for i, r := range refs {
		it := items[i]
		entry := sessionListEntry{
			Node:   r.NodeRef,
			ID:     r.Ref,
			Name:   it.Name,
			Status: it.Status,
			Self:   r.RealID == selfID,
		}
		if r.NodeRef == "#0" {
			entry.Target = r.Ref // 本机会话：直接 #N
		} else {
			entry.Target = "node://" + r.NodeRef + "/" + r.Ref // 远程：node://#K/#N（N=全局会话序）
		}
		out = append(out, entry)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (t *sessionListTool) AfterCall(context.Context, core.ToolCall) {}
