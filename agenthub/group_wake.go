package agenthub

// group_wake.go：群 @ 唤醒消息的 hidden header（P3b-01a，方案 §6.5.6）。
//
// 为什么和群快照放在同一个非 text 块：
//   - bridge 在 UserInputsConsumed 里必须**确定**这一批用户输入属于哪个群/哪一轮，
//     不能靠文本匹配；
//   - 快照只给模型看，不进前端用户气泡；正文另起 text 块（触发消息原文），
//     它才算「用户输入」，同时保持气泡可读；
//   - 首行 JSON、其后快照文本：解析只读首行，不把 12k 快照塞进 JSON 字符串。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 群唤醒的**来源**（P6-11）：谁触发了这一轮。人类与智能体的消息在协议上必须分得开 ——
// 「同事的请求」不是「系统指令」，也不能被当成人类授权（审批/只读闸门按来源与群模式判定，
// 不按消息正文里自称的身份判定）。
const (
	WakeOriginHuman  = "human"
	WakeOriginOwner  = "owner"
	WakeOriginMember = "member"
)

// WakeOrigin 由**群成员表**推出唤醒来源，调用方不能自报（与 CheckHandoff 同口径）。
// 人类恒 human；智能体按落盘角色 owner/member；非成员返回 ""（未知，不冒充成员）。
func WakeOrigin(g *Group, sender AgentID, human bool) string {
	if human {
		return WakeOriginHuman
	}
	role, ok := g.RoleOf(sender)
	if !ok {
		return ""
	}
	switch role {
	case RoleOwner:
		return WakeOriginOwner
	case RoleMember:
		return WakeOriginMember
	default:
		return ""
	}
}

// WakeOriginKnown 是否是协议里的合法来源（"" = 旧块/未知，调用方按未知处理）。
func WakeOriginKnown(origin string) bool {
	switch origin {
	case "", WakeOriginHuman, WakeOriginOwner, WakeOriginMember:
		return true
	default:
		return false
	}
}

// GroupWakeHeader 群唤醒块的协议字段。MessageID 留给 P3a-09 的流式草稿；
// 当前为空表示由 store 在落账时分配 msg_<seq>。
type GroupWakeHeader struct {
	GroupID    GroupID `json:"group_id"`
	AgentID    AgentID `json:"agent_id"`
	RoundID    string  `json:"round_id"`
	TriggerSeq int64   `json:"trigger_seq"`
	MessageID  string  `json:"message_id,omitempty"`
	// SenderAgentID / Origin 是这条唤醒的**来源**（P6-11）：人类触发 = human，
	// 智能体触发 = owner/member（角色从群成员表解析）。omitempty：P6-11 之前落盘的
	// 旧 header 没有这两个字段，解码为空 = 来源未知，不能当成人类。
	SenderAgentID AgentID `json:"sender_agent_id,omitempty"`
	Origin        string  `json:"origin,omitempty"`
	// Depth/Turn 是对话控制器（P3b-04）的记账字段：本唤醒是第几跳 @（人类=0，首轮
	// 智能体=1）、本轮对话的第几轮。omitempty + 缺省 0：P3b-04 之前落在 transcript
	// 里的旧块必须继续能解析（「没有字段」= 深度/轮次未知，按 0 处理）。
	Depth int `json:"depth,omitempty"`
	Turn  int `json:"turn,omitempty"`
	// ConversationID 是这条唤醒所属的自动对话 id（P3b-08 Stop P1）：收尾时用它
	// 判断这一轮是不是已经被人类停止/换代，停止之后不再往群里写 final。
	// omitempty：P3b-04 之前落盘的旧 header 没有该字段，解码为 ""。
	ConversationID string `json:"conversation_id,omitempty"`
}

// EncodeGroupWake 编码 hidden 块内容：首行 header JSON，空行后快照。
func EncodeGroupWake(h *GroupWakeHeader, snapshot string) (string, error) {
	if h == nil {
		return "", fmt.Errorf("%w: 群唤醒 header 为空", ErrInvalid)
	}
	if !validID(string(h.GroupID)) || !strings.HasPrefix(string(h.GroupID), PrefixGroup+"_") {
		return "", fmt.Errorf("%w: 群唤醒 group_id %q", ErrInvalid, h.GroupID)
	}
	if !validID(string(h.AgentID)) || !strings.HasPrefix(string(h.AgentID), PrefixAgent+"_") {
		return "", fmt.Errorf("%w: 群唤醒 agent_id %q", ErrInvalid, h.AgentID)
	}
	if strings.TrimSpace(h.RoundID) == "" {
		return "", fmt.Errorf("%w: 群唤醒缺 round_id", ErrInvalid)
	}
	if h.TriggerSeq <= 0 {
		return "", fmt.Errorf("%w: 群唤醒 trigger_seq 必须为正", ErrInvalid)
	}
	// 来源只允许三选一/空（空 = 旧块/未知）。写坏值会让下游把「未知」当成人类，
	// 所以宁可在编码处 fail-closed。
	if !WakeOriginKnown(h.Origin) {
		return "", fmt.Errorf("%w: 群唤醒 origin %q（只允许 human/owner/member）", ErrInvalid, h.Origin)
	}
	if h.SenderAgentID != "" {
		if err := ValidateAgentID(h.SenderAgentID); err != nil {
			return "", fmt.Errorf("%w: 群唤醒 sender_agent_id: %v", ErrInvalid, err)
		}
	}
	// 智能体来源必须带发送者：只写 owner/member 而不写是谁，等于让下游无法追溯/审计。
	if (h.Origin == WakeOriginOwner || h.Origin == WakeOriginMember) && h.SenderAgentID == "" {
		return "", fmt.Errorf("%w: 群唤醒 origin=%s 必须带 sender_agent_id", ErrInvalid, h.Origin)
	}
	b, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	return string(b) + "\n\n" + strings.TrimSpace(snapshot), nil
}

// ParseGroupWake 解析 hidden 块；第二个返回值是快照正文（可为空）。
func ParseGroupWake(content string) (*GroupWakeHeader, string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, "", fmt.Errorf("%w: 空 hub_group_wake 块", ErrInvalid)
	}
	line, rest, _ := strings.Cut(content, "\n")
	var h GroupWakeHeader
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &h); err != nil {
		return nil, "", fmt.Errorf("%w: hub_group_wake header JSON: %v", ErrInvalid, err)
	}
	if !validID(string(h.GroupID)) || !strings.HasPrefix(string(h.GroupID), PrefixGroup+"_") {
		return nil, "", fmt.Errorf("%w: hub_group_wake 缺 group_id", ErrInvalid)
	}
	if !validID(string(h.AgentID)) || !strings.HasPrefix(string(h.AgentID), PrefixAgent+"_") {
		return nil, "", fmt.Errorf("%w: hub_group_wake 缺 agent_id", ErrInvalid)
	}
	if strings.TrimSpace(h.RoundID) == "" {
		return nil, "", fmt.Errorf("%w: hub_group_wake 缺 round_id", ErrInvalid)
	}
	if h.TriggerSeq <= 0 {
		return nil, "", fmt.Errorf("%w: hub_group_wake 缺 trigger_seq", ErrInvalid)
	}
	return &h, strings.TrimSpace(rest), nil
}
