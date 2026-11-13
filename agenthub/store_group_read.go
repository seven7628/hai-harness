package agenthub

// store_group_read.go：群级人类已读游标（HumanReadSeq）的存储实现（P3b-06，§6.5.2 / §7.1.1）。
//
// 为什么游标挂在 Group 上而不是 Cursor 上：
//   - Cursor 是每 (group, agent)，那是**智能体上下文**游标；
//   - 人只有一条：读到哪里是群级共享的（§7.5.7：一端读了，另一端也变已读）。
//
// 并发：游标写与群元数据更新（SaveGroup 整份写回）是两件事。这里都走存储锁，
// 且 SaveGroup 会保留磁盘上更大的 HumanReadSeq，避免「改群名 → 未读点倒退」。
// 游标只前进：乱序/重放的 hub_mark_read 不能把已读变回未读。

import (
	"context"
	"fmt"
)

// MarkGroupRead 在存储锁内读-改-写群级已读游标，只前移。
func (s *FileStore) MarkGroupRead(ctx context.Context, groupID GroupID, seq int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !validID(string(groupID)) || seq <= 0 {
		return 0, fmt.Errorf("%w: group read cursor", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var g Group
	if err := readJSON(s.groupPath(groupID), &g); err != nil {
		return 0, err
	}
	if seq <= g.HumanReadSeq {
		return g.HumanReadSeq, nil
	}
	g.HumanReadSeq = seq
	if err := writeJSONAtomic(s.groupPath(groupID), &g); err != nil {
		return 0, err
	}
	return seq, nil
}

// GroupReadSeq 读群级已读游标；群缺失返回 ErrNotFound（调用方据此拒绝幽灵群）。
func (s *FileStore) GroupReadSeq(ctx context.Context, groupID GroupID) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !validID(string(groupID)) {
		return 0, fmt.Errorf("%w: group id %q", ErrInvalid, groupID)
	}
	var g Group
	if err := readJSON(s.groupPath(groupID), &g); err != nil {
		return 0, err
	}
	return g.HumanReadSeq, nil
}

// MarkGroupRead 与 FileStore 同语义（内存实现）。
func (s *MemoryStore) MarkGroupRead(ctx context.Context, groupID GroupID, seq int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !validID(string(groupID)) || seq <= 0 {
		return 0, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[groupID]
	if !ok || g == nil {
		return 0, ErrNotFound
	}
	if seq <= g.HumanReadSeq {
		return g.HumanReadSeq, nil
	}
	g.HumanReadSeq = seq
	return seq, nil
}

// GroupReadSeq 与 FileStore 同语义（内存实现）。
func (s *MemoryStore) GroupReadSeq(ctx context.Context, groupID GroupID) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !validID(string(groupID)) {
		return 0, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[groupID]
	if !ok || g == nil {
		return 0, ErrNotFound
	}
	return g.HumanReadSeq, nil
}
