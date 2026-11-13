package main

// hub_group_mode.go：群模式（auto / ask / readonly）→ 内核 agents.Mode 的边界翻译（P6-06）。
//
// 群策略的 Mode 与工具的 tools.preset 是两个独立概念（agenthub.GroupPolicy 注释）：
// 前者管「能不能产生副作用 / 要不要人类审批」，后者管「模型看不看得见工具」。
// 这里把前者翻成内核既有的 agents.Mode（A 可见性 + 执行前防御、B 审批策略、C 行为提示），
// 不另造执行通道 —— 审批仍然走 ToolApprovalRequested → session approvalWaiter →
// recordHubNeed → Needs you，超时/拒绝由引擎按既有语义把调用判失败：
//
//   - auto     → 无群级审批（Approve 全 Auto）。profile 工具撤下、沙箱、全局权限照常生效；
//   - ask      → 非只读工具经审批通道（读工具/群通信工具直接放行，不刷屏）；
//   - readonly → 非只读工具在 schema 与执行前双重拒绝，错误文案说明是群只读模式。
//
// 只读判定复用工具元数据 tools.ReadOnlyTool（BaseTool.ReadOnly_）；群通信工具
// （group_say / group_members / group_history / agent_status）按契约**始终允许**：
// group_say 会往群日志写一条消息，但它是群聊的通信原语，不是对外部世界的副作用
// —— readonly 群连发言都不许，成员就没法说话（与「只读旁听」的旧 observer 语义无关）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// 群模式翻成的 Mode.Name。带 group- 前缀：与项目会话的 auto/manual 同名但语义不同
// （项目 auto 还会自动放行沙箱重跑），别让日志/面板把两者混成同一个开关。
const (
	groupModeNameAuto     = "group-auto"
	groupModeNameAsk      = "group-ask"
	groupModeNameReadonly = "group-readonly"
)

// hubGroupCommsTools 群通信/只读工具：群只读闸门对它们恒开（契约 §P6-06）。
var hubGroupCommsTools = map[string]struct{}{
	"group_say":     {},
	"group_members": {},
	"group_history": {},
	"agent_status":  {},
}

// hubGroupCommsTool 是否是群通信工具（按工具名，不看实例 —— 撤下/未注册也算）。
func hubGroupCommsTool(name string) bool {
	_, ok := hubGroupCommsTools[strings.TrimSpace(name)]
	return ok
}

// hubGroupToolReadOnly 群口径的只读判定：群通信工具恒真，其余看工具元数据。
// 未实现 ReadOnlyTool 的工具 = 非只读（保守），与 agents.isReadOnly 同一口径。
func hubGroupToolReadOnly(t tools.Tool) bool {
	if t == nil {
		return false
	}
	if hubGroupCommsTool(t.Name()) {
		return true
	}
	ro, ok := t.(tools.ReadOnlyTool)
	return ok && ro.ReadOnly()
}

// hubGroupCallReadOnly 按**工具名**判只读（Mode.Approve 只拿得到 ToolCall）：
// 引擎里查得到实例就看元数据，查不到（未注册/被撤下）= 非只读（保守）。
func hubGroupCallReadOnly(eng *tools.Engine, name string) bool {
	if hubGroupCommsTool(name) {
		return true
	}
	if eng == nil {
		return false
	}
	t, err := eng.GetTool(context.Background(), name)
	if err != nil || t == nil {
		return false
	}
	return hubGroupToolReadOnly(t)
}

// hubGroupReadonlyReason readonly 模式下非只读工具被拒的文案（回传模型，必须可操作）。
func hubGroupReadonlyReason(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "this tool"
	}
	return fmt.Sprintf(
		"blocked: this group is in readonly mode, so %q did not run — only read-only tools and "+
			"group communication (group_say/group_members/group_history/agent_status) are allowed here. "+
			"Ask the human to switch the group mode to ask or auto if the work needs side effects.",
		name)
}

const hubGroupReadonlyHint = `
## Group mode: readonly
This group is in readonly mode. Tools with side effects (bash, write/edit, run_python, subagent,
codemode, assign_task, session_send, handoff, …) are not available and will be refused.
You can still read (group_history/group_members/agent_status/read-only tools) and speak with
group_say. If the work needs side effects, say what you would do and ask the human to switch the
group mode to ask or auto — do not pretend you ran a tool.`

const hubGroupAskHint = `
## Group mode: ask
This group is in ask mode. Calls with side effects (bash, write/edit, run_python, subagent,
codemode, assign_task, session_send, handoff, …) are shown to the human for approval before they
run; read-only calls and group communication run immediately. If a call is denied or the approval
times out, it did NOT run: tell the human, then ask again or propose an alternative.`

// hubGroupModeFor 把群策略 Mode 翻成内核 Mode（唯一翻译点）。
// eng 用于按工具名解析只读元数据（ask 的放行判定）；nil 时除群通信工具外一律要审批。
func hubGroupModeFor(policyMode string, eng *tools.Engine) *agents.Mode {
	switch (agenthub.GroupPolicy{Mode: policyMode}).EffectiveMode() {
	case agenthub.GroupModeReadonly:
		return &agents.Mode{
			Name: groupModeNameReadonly,
			AllowTool: func(t tools.Tool) bool {
				return hubGroupToolReadOnly(t)
			},
			Approve: func(core.ToolCall) agents.ApprovalDecision { return agents.ApprovalAuto },
			BlockReason: func(t tools.Tool) string {
				if t == nil {
					return hubGroupReadonlyReason("")
				}
				return hubGroupReadonlyReason(t.Name())
			},
			SystemHint: hubGroupReadonlyHint,
		}
	case agenthub.GroupModeAsk:
		return &agents.Mode{
			Name: groupModeNameAsk,
			Approve: func(c core.ToolCall) agents.ApprovalDecision {
				if hubGroupCallReadOnly(eng, c.Name) {
					return agents.ApprovalAuto
				}
				return agents.ApprovalRequired
			},
			SystemHint: hubGroupAskHint,
		}
	default: // auto：不加群级审批（profile/sandbox/全局权限仍各自生效）
		return &agents.Mode{
			Name:    groupModeNameAuto,
			Approve: func(core.ToolCall) agents.ApprovalDecision { return agents.ApprovalAuto },
		}
	}
}

// applyHubGroupMode 把群策略模式落到已装配的群会话（SetMode 快照：下一次 Run 生效，
// 不打断在跑的回合）。bs.mode 与 session 模式同源 —— 面板/沙箱闸门看到的是同一份事实。
func applyHubGroupMode(bs *bridgeSession, g *agenthub.Group) {
	if bs == nil || bs.s == nil || g == nil {
		return
	}
	mode := hubGroupModeFor(g.Policy.EffectiveMode(), bs.hubEngine)
	bs.mode = mode
	bs.s.SetMode(mode)
}

// refreshHubGroupMode 群策略变更后，把新模式推到该群**已装配**的成员会话。
// 没装配过的会话首次唤醒时按当前群策略装配，不需要在这里预建。
func (m *manager) refreshHubGroupMode(g *agenthub.Group) error {
	if m == nil || g == nil || g.ID == "" {
		return nil
	}
	var firstErr error
	for _, member := range g.Members {
		bs := m.hubGroupSessionIfPresent(member.AgentID, g.ID)
		if bs == nil {
			continue
		}
		applyHubGroupMode(bs, g)
		if bs.s.Mode() == nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("群模式未生效：%s 的会话没有模式", member.AgentID)
			}
		}
	}
	return firstErr
}

// hubGroupSessionIfPresent 只查已装配的群会话（不懒创建）。
func (m *manager) hubGroupSessionIfPresent(agentID agenthub.AgentID, groupID agenthub.GroupID) *bridgeSession {
	if m == nil || agentID == "" || groupID == "" {
		return nil
	}
	ws := m.hubRuntimeIfExists(string(agentID))
	if ws == nil {
		return nil // 没装配过运行时 = 没有已装配的群会话（别为刷新去建 runtime）
	}
	sid := hubSessionID(string(agentID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: groupID})
	ws.mu.Lock()
	bs := ws.sessions[sid]
	ws.mu.Unlock()
	if bs == nil || bs.ctx.Err() != nil {
		return nil
	}
	return bs
}
