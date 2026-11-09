package agenthub

// store_search.go：人类视角的群/DM 全文搜索（P4-04b-1）。
//
// 与 SearchMessages 的分工（**不要合并**）：
//   - SearchMessages 是 agent-safe 视图，group_history 的 query 走它：owner-only 只
//     给占位、routing/round_summary/approval_stub 不进结果。改它会打开「query 布尔
//     探测 owner-only 正文」的口子（P3a-07 B3）。
//   - SearchMessagesHuman / SearchDirectMessages 只服务人类 UI：owner-only 正文可见
//     （由 bridge 打 owner_only 标记），系统噪声 kind 仍然默认排除 —— 它们只是投影，
//     不是人写的内容。
//
// 匹配口径：大小写不敏感的 substring。用 strings.ToLower 折 ASCII、汉字恒等，所以
// CJK 不需要分词也能「找得到」；v1 不做相关度/FTS（见 docs/agent-hub/P4-04B-DESIGN.md）。
// 返回顺序与 ReadMessages 一致（旧→新），newest-first 排序由调用方（bridge）做。

import (
	"context"
	"fmt"
	"strings"
)

// humanSearchVisible 是一条消息能否进入人类搜索结果：
//   - owner-only 对人类可见（这一行是权限口径，绝不能改成 VisibleTo(false)）；
//   - 回执/轮次摘要/审批占位是 UI 投影，正文可能含内部 id，默认不进搜索。
func humanSearchVisible(m Message) bool {
	if !m.VisibleTo(true) {
		return false
	}
	switch m.Kind {
	case KindRouting, KindRoundSummary, KindApprovalStub:
		return false
	default:
		return true
	}
}

// matchHumanSearch 在已读入内存的消息里做 substring 匹配并取「最新 limit 条」。
// 空查询返回 nil：搜索空串不该等于「导出整份日志」。
func matchHumanSearch(msgs []Message, query string, beforeSeq int64, limit int) []Message {
	query = strings.TrimSpace(strings.ToLower(query))
	if query == "" {
		return nil
	}
	matches := make([]Message, 0, limit)
	for _, m := range msgs {
		if beforeSeq > 0 && m.Seq >= beforeSeq {
			continue
		}
		if !humanSearchVisible(m) {
			continue
		}
		if strings.Contains(strings.ToLower(m.Text), query) {
			matches = append(matches, m)
		}
	}
	return tail(matches, limit)
}

// SearchMessagesHuman 群日志的人类搜索：含 owner-only，排除系统噪声，newest 优先。
// beforeSeq 只返回 seq < beforeSeq 的命中（0 = 从最新开始），返回旧→新。
//
// 锁：只在读日志时持 s.mu（与 SearchMessages 同口径），匹配/截断在锁外做 ——
// 桥接层的 all 搜索会对每个会话各调一次，不能把整轮 fan-out 包在同一把锁里。
func (s *FileStore) SearchMessagesHuman(ctx context.Context, gid GroupID, query string, beforeSeq int64, limit int) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, fmt.Errorf("%w: group id %q", ErrInvalid, gid)
	}
	limit = normalizeLimit(limit)
	s.mu.Lock()
	msgs, err := readJSONLTolerantTail[Message](s.messagesPath(gid))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return matchHumanSearch(msgs, query, beforeSeq, limit), nil
}

// SearchDirectMessages 是 DM 索引（dms/<agent>/log.jsonl）的人类搜索，口径与
// SearchMessagesHuman 完全一致（含 owner-only、排除噪声、旧→新）。
func (s *FileStore) SearchDirectMessages(ctx context.Context, agentID AgentID, query string, beforeSeq int64, limit int) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(agentID)) {
		return nil, fmt.Errorf("%w: agent id %q", ErrInvalid, agentID)
	}
	limit = normalizeLimit(limit)
	s.mu.Lock()
	msgs, err := readJSONLTolerantTail[Message](s.dmLogPath(agentID))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return matchHumanSearch(msgs, query, beforeSeq, limit), nil
}

// SearchMessagesHuman 与 FileStore 同语义（内存实现）。
func (s *MemoryStore) SearchMessagesHuman(ctx context.Context, gid GroupID, query string, beforeSeq int64, limit int) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, fmt.Errorf("%w: group id %q", ErrInvalid, gid)
	}
	s.mu.Lock()
	msgs := cloneMessages(s.messages[gid])
	s.mu.Unlock()
	return matchHumanSearch(msgs, query, beforeSeq, normalizeLimit(limit)), nil
}

// SearchDirectMessages 与 FileStore 同语义（内存实现）。
func (s *MemoryStore) SearchDirectMessages(ctx context.Context, agentID AgentID, query string, beforeSeq int64, limit int) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(agentID)) {
		return nil, fmt.Errorf("%w: agent id %q", ErrInvalid, agentID)
	}
	s.mu.Lock()
	msgs := cloneMessages(s.dmMessages[agentID])
	s.mu.Unlock()
	return matchHumanSearch(msgs, query, beforeSeq, normalizeLimit(limit)), nil
}
