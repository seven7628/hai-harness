package agenthub

// grouplog.go：群日志/活动/汇总（P3a-02，§6.6/§7.1.3）。
//
// 日志与活动是两个独立 append-only 流：
//   - log.jsonl：消息（人/智能体/系统/轮次汇总），seq 每群单调；
//   - activity.jsonl：工具行/压缩/状态，AfterSeq 表示跟随哪条群消息，不进快照。
//
// RoundSummary 必须**确定**（同输入同输出）：按 (agent_id, tool, status, summary) 排序后
// 拼行，不能按 map 迭代或到达顺序，否则重放/多端渲染会出现不同文本。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RoundItem 是一轮动作清单项（工具/子 agent 结果）。
type RoundItem struct {
	AgentID AgentID `json:"agent_id"`
	Tool    string  `json:"tool,omitempty"`
	Status  string  `json:"status,omitempty"`
	Summary string  `json:"summary,omitempty"`
	Ms      int64   `json:"ms,omitempty"`
}

// BuildRoundSummary 生成确定性的轮次回执消息（KindRoundSummary，不发模型）。
func BuildRoundSummary(group GroupID, roundID string, items []RoundItem, at time.Time) Message {
	sorted := append([]RoundItem(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.AgentID != b.AgentID {
			return a.AgentID < b.AgentID
		}
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		if a.Status != b.Status {
			return a.Status < b.Status
		}
		if a.Summary != b.Summary {
			return a.Summary < b.Summary
		}
		return a.Ms < b.Ms
	})
	var b strings.Builder
	for _, it := range sorted {
		line := string(it.AgentID)
		if it.Tool != "" {
			line += " · " + it.Tool
		}
		if it.Status != "" {
			line += " · " + it.Status
		}
		if it.Summary != "" {
			line += "：" + it.Summary
		}
		if it.Ms > 0 {
			line += fmt.Sprintf("（%dms）", it.Ms)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return Message{
		GroupID:   group,
		Kind:      KindRoundSummary,
		Sender:    Sender{Kind: SenderSystem},
		Text:      strings.TrimRight(b.String(), "\n"),
		Refs:      map[string]any{"round_id": roundID, "items": sorted},
		CreatedAt: at,
	}
}

// BuildDigest 生成确定性的群摘要（最近 limit 条，按 seq 升序，截断到 maxRunes）。
// 只做「谁说了什么」的拼接，不调用模型；摘要本身不落群日志（调用方决定）。
func BuildDigest(msgs []Message, limit int, maxRunes int) string {
	if limit <= 0 {
		limit = 20
	}
	start := 0
	if len(msgs) > limit {
		start = len(msgs) - limit
	}
	var b strings.Builder
	for _, m := range msgs[start:] {
		who := string(m.Sender.AgentID)
		if m.Sender.Kind == SenderHuman {
			who = "人"
		} else if m.Sender.Kind == SenderSystem || who == "" {
			who = "系统"
		}
		text := strings.TrimSpace(strings.ReplaceAll(m.Text, "\n", " "))
		if text == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("#%d %s: %s\n", m.Seq, who, text))
	}
	out := strings.TrimRight(b.String(), "\n")
	if maxRunes > 0 {
		r := []rune(out)
		if len(r) > maxRunes {
			out = string(r[:maxRunes]) + "…"
		}
	}
	return out
}

// AppendGroupMessage 校验群存在且未删除后追加消息并分配 seq。
func AppendGroupMessage(ctx context.Context, st Store, m *Message) (int64, error) {
	if st == nil || m == nil {
		return 0, fmt.Errorf("%w: group log 参数为空", ErrInvalid)
	}
	g, err := st.LoadGroup(ctx, m.GroupID)
	if err != nil {
		return 0, err
	}
	if g == nil || g.Deleted {
		return 0, fmt.Errorf("%w: group %s 已删除", ErrNotFound, m.GroupID)
	}
	return st.AppendMessage(ctx, m)
}

// AppendGroupMessageRouted 校验群存活后原子追加消息与路由回执。
// round=nil 表示没有 @（只写消息，不写回执）；client_msg_id 命中时返回 deduped。
func AppendGroupMessageRouted(ctx context.Context, st Store, m *Message, round *Round) (int64, bool, error) {
	if st == nil || m == nil {
		return 0, false, fmt.Errorf("%w: group log 参数为空", ErrInvalid)
	}
	g, err := st.LoadGroup(ctx, m.GroupID)
	if err != nil {
		return 0, false, err
	}
	if g == nil || g.Deleted {
		return 0, false, fmt.Errorf("%w: group %s 已删除", ErrNotFound, m.GroupID)
	}
	return st.AppendMessageWithRound(ctx, m, round)
}

// ReadGroupLog 分页读群日志（beforeSeq=0 表示最新一页）。
func ReadGroupLog(ctx context.Context, st Store, gid GroupID, beforeSeq int64, limit int) ([]Message, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: store 为空", ErrInvalid)
	}
	g, err := st.LoadGroup(ctx, gid)
	if err != nil {
		return nil, err
	}
	if g == nil || g.Deleted {
		return nil, fmt.Errorf("%w: group %s 已删除", ErrNotFound, gid)
	}
	return st.ReadMessages(ctx, gid, beforeSeq, limit)
}

// ReadGroupActivity 分页读活动流（不进快照，独立游标）。
func ReadGroupActivity(ctx context.Context, st Store, gid GroupID, afterSeq int64, limit int) ([]Activity, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: store 为空", ErrInvalid)
	}
	return st.ReadActivity(ctx, gid, afterSeq, limit)
}
