package main

// hub_projection.go：hub_group_list 的左栏投影（P3b-06，§6.5.7 / §7.1.3 / §7.5.5）。
//
// 群行要显示「任务完成：pricing-v3.md」+ 未读点，所以列表不能只回裸 Group：
//   - groups 保持原样（不破坏旧前端/旧测试）；
//   - progress 以 group_id 为 key，给未读/预览/最近任务结论；
//   - 计算走 agenthub.BuildGroupPerception（纯函数，口径与单测在领域包）。
//
// 读日志必须**分页**（beforeSeq 独占）：一次只读最近 500 条会把「501 条未读」
// 静默算成 500，左栏数字就骗人。这里从新到旧翻页，直到覆盖 human_read_seq
//（更早的消息都已读过，与未读/预览无关）。
//
// 刻意不在这里推进游标：进入群 1s 后标记已读是前端节奏（§7.5.7），
// bridge 只在「人自己发消息」和显式 hub_mark_read 时前移。

import (
	"context"
	"fmt"

	"github.com/seven7628/hai-harness/agenthub"
)

// hubGroupProgressPage 是单页消息数（与 DM 分页同为 500 量级）。
const hubGroupProgressPage = 500

// hubGroupProgress 读一个群的日志并构建左栏投影。
func (m *manager) hubGroupProgress(ctx context.Context, st agenthub.Store, g *agenthub.Group) (agenthub.GroupPerception, error) {
	readSeq, err := st.GroupReadSeq(ctx, g.ID)
	if err != nil {
		return agenthub.GroupPerception{}, fmt.Errorf("读取已读游标失败: %w", err)
	}
	msgs, err := readGroupMessagesSince(ctx, st, g.ID, readSeq)
	if err != nil {
		return agenthub.GroupPerception{}, err
	}
	return agenthub.BuildGroupPerception(msgs, readSeq, agenthub.DefaultGroupPreviewRunes), nil
}

// readGroupMessagesSince 从新到旧分页读，直到覆盖 readSeq，返回旧→新。
//
// 停止条件：本页最旧 seq <= readSeq（再往前都已读，不影响未读/预览）。
// MemoryStore/FileStore 的 ReadMessages 都是 beforeSeq 独占，翻页不会原地打转。
func readGroupMessagesSince(ctx context.Context, st agenthub.Store, gid agenthub.GroupID, readSeq int64) ([]agenthub.Message, error) {
	var out []agenthub.Message
	before := int64(0)
	for {
		page, err := st.ReadMessages(ctx, gid, before, hubGroupProgressPage)
		if err != nil {
			return nil, fmt.Errorf("读取群日志失败: %w", err)
		}
		if len(page) == 0 {
			break
		}
		// page 是旧→新；拼到已有切片前面。新开切片，别复用 store 返回的底层数组。
		next := make([]agenthub.Message, 0, len(page)+len(out))
		next = append(next, page...)
		next = append(next, out...)
		out = next
		oldest := page[0].Seq
		if oldest <= readSeq {
			break
		}
		before = oldest
	}
	return out, nil
}

// hubGroupProgressMap 给 hub_group_list 组装 progress 映射。
// 单个群的日志坏掉不能让整个左栏空白：跳过并挂一条 warning（前端仍能列出群）。
func (m *manager) hubGroupProgressMap(ctx context.Context, st agenthub.Store, groups []*agenthub.Group, data map[string]any) map[string]agenthub.GroupPerception {
	progress := make(map[string]agenthub.GroupPerception, len(groups))
	for _, g := range groups {
		p, err := m.hubGroupProgress(ctx, st, g)
		if err != nil {
			addHubWarning(data, fmt.Sprintf("群 %s 预览读取失败：%v", g.ID, err))
			continue
		}
		progress[string(g.ID)] = p
	}
	return progress
}
