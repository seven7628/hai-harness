package subagent

import (
	"context"
	"sync"

	"github.com/seven7628/hai-harness/core"
)

// 子任务的**会话级状态持久化**（2026-10 起的语义变更：后台子任务从「一次性 Run」
// 升级为「有会话身份的长期实体」）。
//
// 与 journal（journal.go）的分工——两套并存的理由，务必不要混淆：
//
//	journal  = 进度/审计旁路。有损（turn 文本截断 4000 runes、只记工具名不记参数与
//	           结果），给人和 TaskOutput 看的，恢复**不能**依赖它还原上下文。
//	StateStore = 恢复用的**权威状态**。与主会话逐字段同构：有效上下文（发模型的、
//	           可能压缩过的 Messages）+ 累计用量。每轮 OnTurn 落一条，崩溃后据此
//	           原地续跑（不重做已完成的工作、不丢上下文）。
//
// 落盘语义与主会话一致（Session.persist）：History 是 append-only 的全量消息日志，
// State 是当前有效上下文（压缩后形态）；恢复只读最后一条 State。
//
// **存放位置**：与 journal 同目录（`~/.go-code/sessions/<wsKey>/<sid>/agents/`），
// 只是文件不同（`<taskId>.jsonl` 同时承载两者：StateStore 写 history/state 类型行，
// TaskJournal 写自身记录行）。实现由宿主注入（desktop/bridge），SDK 不感知路径约定。
//
// 关键约束：本文件**只依赖 core** —— tools/events/todo/agents 都不 import subagent，
// 反向也不能引入（否则成环）。故 RunState 是 State 的最小镜像，由宿主适配器
// （session.State ⇄ subagent.RunState）做字段映射，而不是直接复用 session.State。
type RunState struct {
	// Messages 当前有效上下文（发给模型的形态；压缩后为摘要形态）——恢复的唯一
	// 上下文来源。
	Messages []core.Message
	// Usage 该任务累计用量（含成本）。恢复后作为已知基数继续累加。
	Usage core.Usage
}

// StateStore 单任务的状态持久化端口（宿主注入；实现通常是 session.FileStore 的
// 薄适配器——同一份 jsonl 格式，主会话与子任务共用）。
//
// 单写者模型：一个任务在任一时刻至多有一个运行 goroutine 写它，故实现无需自身
// 互斥（FileStore 的 appendLine 另有进程内锁，防与自压缩竞争）。
type StateStore struct {
	// Load 读该任务最后一次 checkpoint 的状态。文件不存在（新任务）/尚无 state 行
	// 返回 (nil, nil)——调用方按「无历史可续」处理，不是错误。
	Load func(ctx context.Context, taskID string) (*RunState, error)
	// Save 落一条 state（每轮 checkpoint 调用）。失败由调用方降级处理
	//（记录 + 继续运行，与 journal 同一条防线：落盘环节零失败面传导到子运行）。
	Save func(ctx context.Context, taskID string, st *RunState) error
	// AppendHistory 追加消息日志（append-only 全量历史；与主会话同语义，
	// 供事后审计与跨进程重建——恢复本身只读 Save 的 state）。
	AppendHistory func(ctx context.Context, taskID string, msgs []core.Message) error
}

// nilSafe 返回可用于调用的副本：端口为零值（未注入 / 未启用落盘）时全部方法
// 退化为 no-op 与空状态，调用点因此零分支（与 TaskJournal 的 nil 接收者同理）。
func (s StateStore) nilSafe() StateStore {
	if s.Load != nil && s.Save != nil && s.AppendHistory != nil {
		return s
	}
	out := s
	if out.Load == nil {
		out.Load = func(context.Context, string) (*RunState, error) { return nil, nil }
	}
	if out.Save == nil {
		out.Save = func(context.Context, string, *RunState) error { return nil }
	}
	if out.AppendHistory == nil {
		out.AppendHistory = func(context.Context, string, []core.Message) error { return nil }
	}
	return out
}

// Enabled 报告端口是否真的接了存储（三个方法齐备）。调用方用它区分「有落盘」
// 与「降级为纯内存」——零值端口的 Load 恒返回空状态，会让恢复以为「无历史可续」。
func (s StateStore) Enabled() bool {
	return s.Load != nil && s.Save != nil && s.AppendHistory != nil
}

// memoryStore 进程内状态存储（默认实现；测试与「未注入宿主存储」的降级路径用）。
// 语义：进程内可恢复（同一 Registry/进程内重跑），但**跨进程无效**——那正是
// 宿主注入文件存储的意义。
type memoryStore struct {
	mu    sync.Mutex
	state map[string]*RunState
	hist  map[string][]core.Message
}

// NewMemoryStateStore 构造进程内状态存储（进程重启即失）。
func NewMemoryStateStore() StateStore {
	m := &memoryStore{state: map[string]*RunState{}, hist: map[string][]core.Message{}}
	return StateStore{
		Load: func(_ context.Context, taskID string) (*RunState, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			st, ok := m.state[taskID]
			if !ok || st == nil {
				return nil, nil
			}
			return &RunState{
				Messages: append([]core.Message{}, st.Messages...),
				Usage:    st.Usage,
			}, nil
		},
		Save: func(_ context.Context, taskID string, st *RunState) error {
			if st == nil {
				return nil
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			cp := &RunState{Messages: append([]core.Message{}, st.Messages...), Usage: st.Usage}
			m.state[taskID] = cp
			return nil
		},
		AppendHistory: func(_ context.Context, taskID string, msgs []core.Message) error {
			if len(msgs) == 0 {
				return nil
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			m.hist[taskID] = append(m.hist[taskID], msgs...)
			return nil
		},
	}
}
