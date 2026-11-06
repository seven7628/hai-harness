package agenthub

// snapshot.go：群内智能体的上下文快照（P3a-04，§6.6.2）。
//
// 口径：
//   - 每 (群, 智能体) 一个会话；快照只包含**上次游标之后**的消息（delta）；
//   - 冻结本轮：seq > round.TriggerSeq 的消息不进本次快照（@ 之后的回复属于下一轮）；
//   - 预算：最多 40 条 / 12000 rune；超了从最旧折叠，Folded 计数；
//   - 系统/轮次回执（round_summary）与活动流不进快照；
//   - 输出文本确定性（消息按 seq 升序，成员按 JoinedAt/ID），重放同输入同输出。

import (
	"fmt"
	"sort"
	"strings"
)

const (
	DefaultSnapshotMessages = 40
	DefaultSnapshotRunes    = 12000
)

// SnapshotOptions 快照预算；零值用默认（40 条 / 12000 rune）。
type SnapshotOptions struct {
	MaxMessages int
	MaxRunes    int
}

func (o SnapshotOptions) defaults() SnapshotOptions {
	if o.MaxMessages <= 0 {
		o.MaxMessages = DefaultSnapshotMessages
	}
	if o.MaxRunes <= 0 {
		o.MaxRunes = DefaultSnapshotRunes
	}
	return o
}

// GroupSnapshot 是构建结果（Text 进系统提示词，Messages 供调试/事件）。
type GroupSnapshot struct {
	GroupID    GroupID   `json:"group_id"`
	AgentID    AgentID   `json:"agent_id"`
	RoundID    string    `json:"round_id,omitempty"`
	TriggerSeq int64     `json:"trigger_seq"`
	FromSeq    int64     `json:"from_seq"`
	Messages   []Message `json:"messages"`
	Folded     int       `json:"folded,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	Text       string    `json:"text"`
}

// BuildGroupSnapshot 构建某智能体本轮看到的群上下文。
// round 为空 = 不冻结（构建通用快照）；cursor 是该智能体上次读到的 seq。
func BuildGroupSnapshot(g *Group, agentID AgentID, msgs []Message, cursor int64, round Round, opts SnapshotOptions) GroupSnapshot {
	opt := opts.defaults()
	out := GroupSnapshot{GroupID: "", AgentID: agentID, FromSeq: cursor}
	if g != nil {
		out.GroupID = g.ID
	}
	out.RoundID = round.ID
	out.TriggerSeq = round.TriggerSeq
	limitSeq := int64(0)
	if round.TriggerSeq > 0 {
		limitSeq = round.TriggerSeq
	}
	eligible := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq <= cursor {
			continue
		}
		if limitSeq > 0 && m.Seq > limitSeq {
			continue
		}
		switch m.Kind {
		case KindRoundSummary, KindRouting, KindApprovalStub:
			continue // 回执/审批占位不进上下文（审批正文只进 Needs you）
		}
		if !m.VisibleTo(false) {
			continue // owner-only 消息 fail-closed：智能体上下文一律不可见
		}
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		eligible = append(eligible, m)
	}
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].Seq < eligible[j].Seq })
	kept := eligible
	if len(kept) > opt.MaxMessages {
		out.Folded = len(kept) - opt.MaxMessages
		kept = kept[len(kept)-opt.MaxMessages:]
	}
	for snapshotRunes(kept) > opt.MaxRunes && len(kept) > 1 {
		kept = kept[1:]
		out.Folded++
	}
	out.Text = renderGroupSnapshot(g, agentID, round, kept, out.Folded)
	// 12k 是硬上限：块头（群/成员/@行）与折叠行也计入最终 Text。
	// 先丢最旧整条；只剩一条时截断正文；块头本身超预算时硬切兜底。
	for len([]rune(out.Text)) > opt.MaxRunes {
		over := len([]rune(out.Text)) - opt.MaxRunes
		if len(kept) == 0 {
			break
		}
		r := []rune(strings.TrimSpace(kept[0].Text))
		if over >= len(r) {
			kept = kept[1:]
			out.Folded++
		} else {
			kept[0].Text = string(r[:len(r)-over-1]) + "…"
			out.Truncated = true
		}
		out.Text = renderGroupSnapshot(g, agentID, round, kept, out.Folded)
	}
	if lines := []rune(out.Text); len(lines) > opt.MaxRunes {
		out.Text = string(lines[:opt.MaxRunes])
		out.Truncated = true
	}
	out.Messages = append([]Message(nil), kept...)

	return out
}

// renderGroupSnapshot 渲染最终进提示词的文本；预算校验以它的输出为准。
func renderGroupSnapshot(g *Group, agentID AgentID, round Round, kept []Message, folded int) string {
	var b strings.Builder
	if g != nil {
		b.WriteString(fmt.Sprintf("群：%s（%d 名成员）\n", g.Name, len(g.Members)))
		names := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			names = append(names, string(m.AgentID))
		}
		sort.Strings(names)
		b.WriteString("成员：" + strings.Join(names, "、") + "\n")
		if round.MentionAll || len(round.Targets) > 0 {
			b.WriteString("本轮你被 @：")
			hit := round.MentionAll
			for _, t := range round.Targets {
				if t == agentID {
					hit = true
				}
			}
			if hit {
				b.WriteString("是\n")
			} else {
				b.WriteString("否（只读旁听）\n")
			}
		}
	}
	if folded > 0 {
		b.WriteString(fmt.Sprintf("[已折叠 %d 条较早消息]\n", folded))
	}
	for _, m := range kept {
		b.WriteString(fmt.Sprintf("#%d %s: %s\n", m.Seq, snapshotWho(m), strings.TrimSpace(m.Text)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// snapshotWho 与渲染行使用同一发送者口径，预算估算不能另算一套。
func snapshotWho(m Message) string {
	who := string(m.Sender.AgentID)
	switch m.Sender.Kind {
	case SenderHuman:
		who = "人"
	case SenderSystem:
		who = "系统"
	}
	if who == "" {
		who = "未知"
	}
	return who
}

func snapshotLineRunes(m Message) int {
	prefix := fmt.Sprintf("#%d %s: ", m.Seq, snapshotWho(m))
	return len([]rune(prefix)) + len([]rune(strings.TrimSpace(m.Text))) + 1
}

func snapshotRunes(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += snapshotLineRunes(m)
	}
	return n
}
