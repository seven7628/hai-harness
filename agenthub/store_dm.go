package agenthub

// store_dm.go：DM（单智能体）消息索引与未读游标（§6.1.3 / §6.4，P1-04）。
//
// 与群组的差别只有作用域：DM 没有 GroupID，维度是 AgentID。消息体复用 Message
//（GroupID 留空），seq 在每个 DM 内单调递增 —— 前端用 seq 做游标翻页，不用 offset。
//
// **这里不是上下文源**：模型看到的上下文在 session jsonl 里（压缩会改写它）。
// 这份索引只追加、不被压缩影响，专门喂左栏预览/未读/搜索（§6.1.3 的 DM 表）。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func (s *FileStore) dmLogPath(id AgentID) string {
	return filepath.Join(s.root, "dms", string(id), "log.jsonl")
}

func (s *FileStore) dmCursorPath(id AgentID) string {
	return filepath.Join(s.root, "dms", string(id), "cursor.json")
}

// AppendDirectMessage 追加一条 DM 索引消息并分配 seq/ID。
// 消息体里的 GroupID 会被清空（DM 没有群），ID 默认由 seq 推导
// （调用方预分配的 msg_ 流式 ID 会被保留，见 messageIDFor）。
func (s *FileStore) AppendDirectMessage(ctx context.Context, agentID AgentID, m *Message) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if m == nil || !validID(string(agentID)) {
		return 0, fmt.Errorf("%w: direct message", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	seq := s.dmSeq[agentID] + 1
	stored := *m
	stored.Seq = seq
	stored.ID = messageIDFor(&stored, seq)
	stored.GroupID = ""
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	}
	if err := appendJSONL(s.dmLogPath(agentID), &stored); err != nil {
		return 0, err
	}
	s.dmSeq[agentID] = seq
	*m = stored
	return seq, nil
}

// AppendDirectMessage 与 FileStore 同语义（内存实现，供搜索/评测用）。
func (s *MemoryStore) AppendDirectMessage(ctx context.Context, agentID AgentID, m *Message) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if m == nil || !validID(string(agentID)) {
		return 0, fmt.Errorf("%w: direct message", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := int64(len(s.dmMessages[agentID]) + 1)
	stored := *m
	stored.Seq = seq
	stored.ID = messageIDFor(&stored, seq)
	stored.GroupID = ""
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	}
	s.dmMessages[agentID] = append(s.dmMessages[agentID], stored)
	*m = stored
	return seq, nil
}

// ReadDirectMessages 从新到旧取一页：seq < beforeSeq（0/负 = 从最新开始）。
// 返回顺序为**旧→新**（与 ReadMessages 同约定），方便前端直接渲染。
// 热日志 + 冷归档透明合并；按 manifest 从新到旧读，凑够 limit 即停。
func (s *FileStore) ReadDirectMessages(ctx context.Context, agentID AgentID, beforeSeq int64, limit int) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(agentID)) {
		return nil, fmt.Errorf("%w: agent id %q", ErrInvalid, agentID)
	}
	if limit <= 0 {
		limit = 50
	}
	s.mu.Lock()
	hot, err := readJSONLTolerantTail[Message](s.dmLogPath(agentID))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.pageMessages(ArchiveScopeDMs, string(agentID), hot, beforeSeq, limit)
}

// LoadDMCursor 读未读游标。没有游标 = 全未读（零值 + AgentID 已填，不报 ErrNotFound：
// 新智能体第一次打开不该 404）。
func (s *FileStore) LoadDMCursor(ctx context.Context, agentID AgentID) (*Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(agentID)) {
		return nil, fmt.Errorf("%w: agent id %q", ErrInvalid, agentID)
	}
	var c Cursor
	err := readJSON(s.dmCursorPath(agentID), &c)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	c.AgentID = agentID
	c.GroupID = ""
	return &c, nil
}

// SaveDMCursor 写未读游标（humanReadSeq = 人读到的位置）。
// 只前进不后退：并发/乱序的命令不会把未读点倒退（用户已经看过的消息不该再变未读）。
func (s *FileStore) SaveDMCursor(ctx context.Context, agentID AgentID, humanReadSeq int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(string(agentID)) {
		return fmt.Errorf("%w: agent id %q", ErrInvalid, agentID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Cursor
	err := readJSON(s.dmCursorPath(agentID), &c)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if humanReadSeq <= c.HumanReadSeq {
		return nil
	}
	c.AgentID = agentID
	c.GroupID = ""
	c.HumanReadSeq = humanReadSeq
	c.UpdatedAt = time.Now().UTC()
	return writeJSONAtomic(s.dmCursorPath(agentID), &c)
}

// DMUnread 返回未读条数与最后一条消息（左栏 DM 行：预览 + 未读点）。
// 未读 = seq > human_read_seq 的条数（DM 只有人读，没有智能体自己的已读游标）。
// 热+冷合并后按 seq 去重（热优先），完全归档后最后一条仍来自冷段。
func (s *FileStore) DMUnread(ctx context.Context, agentID AgentID) (unread int, last *Message, err error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	cur, err := s.LoadDMCursor(ctx, agentID)
	if err != nil {
		return 0, nil, err
	}
	s.mu.Lock()
	hot, rerr := readJSONLTolerantTail[Message](s.dmLogPath(agentID))
	s.mu.Unlock()
	if rerr != nil {
		return 0, nil, rerr
	}
	msgs, err := s.mergedLogMessages(ArchiveScopeDMs, string(agentID), hot)
	if err != nil {
		return 0, nil, err
	}
	if len(msgs) == 0 {
		return 0, nil, nil
	}
	for i := range msgs {
		if msgs[i].Seq > cur.HumanReadSeq {
			unread++
		}
	}
	lastMsg := msgs[len(msgs)-1]
	return unread, &lastMsg, nil
}

// FindDirectMessageByClientID 在**整份** DM 索引里找 client_msg_id（幂等重发用）。
//
// 为什么全量而不是最近 N 条：固定窗口外的重发会让模型看到两条相同消息，而窗口大小
// 没有任何语义依据；日志本身已经整份读盘，扫全量不多一次 I/O（P1-04 复核）。
func (s *FileStore) FindDirectMessageByClientID(ctx context.Context, agentID AgentID, clientMsgID string) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if !validID(string(agentID)) {
		return 0, false, fmt.Errorf("%w: agent id %q", ErrInvalid, agentID)
	}
	// 与群口径一致：查询和存储值都 trim，避免同一幂等键在归档前后匹配结果不同。
	clientMsgID = strings.TrimSpace(clientMsgID)
	if clientMsgID == "" {
		return 0, false, nil
	}
	s.mu.Lock()
	msgs, err := readJSONLTolerantTail[Message](s.dmLogPath(agentID))
	s.mu.Unlock()
	if err != nil {
		return 0, false, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if id, _ := msgs[i].Refs["client_msg_id"].(string); strings.TrimSpace(id) == clientMsgID {
			return msgs[i].Seq, true, nil
		}
	}
	// 热日志找不到再搜冷段：幂等重发跨归档仍去重。
	hit, err := s.findArchivedMessageByClientID(ArchiveScopeDMs, string(agentID), clientMsgID, true)
	if err != nil {
		return 0, false, err
	}
	if hit != nil {
		return hit.Seq, true, nil
	}
	return 0, false, nil
}
