package agenthub

// queue.go：Hub 的任务账本 + 空闲闸门 + 派发队列（方案 §6.6.4，P2-03）。
//
// 为什么队列在 Hub 层而不是 Session.inbox：Session 的 inbox 会把「人刚说的话」
// 和「智能体派的任务」聚成同一批（takeBatch），模型看到的是混在一起的多条输入，
// 一个任务就不再是「一次干净的 Run」。这里的闸门保证：
//
//   - 目标会话 Running 或 Pending>0 → 任务只落账本（queued），不投递；
//   - 每次 Pump **最多**派发一个任务（派完就停）：即使 Ask 之后状态还没翻转，
//     也不会把下一个任务塞进同一轮；
//   - AgentEnd（根 run 结束）时由 bridge 调 Pump，队列顺次推进。
//
// 持久化：任务本身在 Ledger（queued 也落盘）；进程内的 FIFO 只是索引。
// 重启后的队列恢复归 P2-08（读取 queued 任务重新入队）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Queue 是 Hub 的派发闸门。零值不可用，用 NewQueue 构造。
type Queue struct {
	ledger *Ledger
	ops    WorkspaceOps

	mu      sync.Mutex
	pending map[string][]TaskID // key = workspace + "\x00" + session_id
}

func NewQueue(ledger *Ledger, ops WorkspaceOps) *Queue {
	return &Queue{ledger: ledger, ops: ops, pending: map[string][]TaskID{}}
}

func queueKey(workspace, sessionID string) string {
	return workspace + "\x00" + sessionID
}

// Submit 创建任务并尝试派发。返回值是**账本条目**：Status=dispatched 表示已投递，
// queued 表示目标忙、留在队列里（结果回推在 P2-05）。
func (q *Queue) Submit(ctx context.Context, t *Task) (*Task, error) {
	if q == nil || q.ledger == nil || q.ops == nil {
		return nil, fmt.Errorf("%w: queue 未初始化", ErrInvalid)
	}
	if t == nil {
		return nil, fmt.Errorf("%w: task is nil", ErrInvalid)
	}
	if t.Target.Kind == "" {
		t.Target.Kind = TargetWorkspace
	}
	if t.Target.Kind != TargetWorkspace {
		return nil, fmt.Errorf("%w: P2 只支持工作区目标，得到 %q", ErrInvalid, t.Target.Kind)
	}
	if strings.TrimSpace(t.Target.Workspace) == "" || strings.TrimSpace(t.Target.SessionID) == "" {
		return nil, fmt.Errorf("%w: 目标 workspace/session_id 不能为空", ErrInvalid)
	}
	if strings.TrimSpace(t.Instruction) == "" {
		return nil, fmt.Errorf("%w: instruction 不能为空", ErrInvalid)
	}
	// 幂等：同一 (target, client_msg_id) 已存在就直接返回原任务，不再建账/入队。
	// 锁覆盖「查找 + 建账 + 入队」：并发同 id 不能双建（P2-02 复审 B2）。
	q.mu.Lock()
	if t.ClientMsgID != "" {
		existing, ferr := q.ledger.FindByClientMsgID(ctx, t.Target, t.ClientMsgID)
		if ferr != nil {
			q.mu.Unlock()
			return nil, ferr
		}
		if existing != nil {
			q.mu.Unlock()
			return existing, nil
		}
	}
	created, err := q.ledger.Create(ctx, t)
	if err != nil {
		q.mu.Unlock()
		return nil, err
	}
	key := queueKey(created.Target.Workspace, created.Target.SessionID)
	q.pending[key] = append(q.pending[key], created.ID)
	q.mu.Unlock()
	dispatched, err := q.Pump(ctx, created.Target)
	if err != nil {
		return created, err // 任务已落账本（failed/canceled）：调用方仍拿得到 task_id
	}
	// 只有「刚提交的这个」被派出去时才返回它；Pump 可能泵的是队首的旧任务，
	// 把别人的账本条目返回给调用方会让 session_send 把错 task_id 交给模型（P2-03 B2）。
	if dispatched != nil && dispatched.ID == created.ID {
		return dispatched, nil
	}
	return created, nil // 目标忙/队首是旧任务：本任务已落账本（queued）
}

// Pump 在目标空闲时派发队首任务；每次最多派发一个。
// 目标忙 / 队列空时返回 (nil, nil)。
func (q *Queue) Pump(ctx context.Context, target TaskTarget) (*Task, error) {
	if q == nil || q.ledger == nil || q.ops == nil {
		return nil, fmt.Errorf("%w: queue 未初始化", ErrInvalid)
	}
	key := queueKey(target.Workspace, target.SessionID)
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := q.pending[key]
	if len(ids) == 0 {
		return nil, nil
	}
	st, err := q.ops.SessionStatus(ctx, target.Workspace, target.SessionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// 目标会话已删除/关闭：不能让它永远占住队首（后续任务全被堵死）。
			id := ids[0]
			q.pending[key] = ids[1:]
			_, _ = q.ledger.Transition(ctx, id, TaskCanceled, TransitionOptions{
				At: time.Now().UTC(), Error: "目标会话不存在",
			})
			return nil, err
		}
		return nil, err
	}
	if st.Running || st.Pending > 0 {
		return nil, nil // 闸门：忙就等下一次 AgentEnd
	}
	id := ids[0]
	now := time.Now().UTC()
	dispatched, err := q.ledger.Transition(ctx, id, TaskDispatched, TransitionOptions{At: now})
	if err != nil {
		// Transition 失败时**不能先出队**：否则账本说 queued、内存队列说没有，
		// 任务永远泵不出去（P2-03 B3）。
		return nil, err
	}
	q.pending[key] = ids[1:]
	header, _ := EncodeTaskHeader(dispatched)
	res, err := q.ops.Dispatch(ctx, DispatchReq{
		AgentID:   dispatched.Origin.AgentID,
		Workspace: dispatched.Target.Workspace,
		SessionID: dispatched.Target.SessionID,
		Text:      dispatched.Instruction,
		// 有调用方幂等键就用它（目标侧去重表按此判重）；没有才回落到账本 id。
		ClientMsgID: dispatchClientMsgID(dispatched),
		TaskHeader:  header, // 目标会话据此绑定 task（§6.7.2）
	})
	if err != nil {
		// 已 dispatched 再落 failed：状态机有序，账本里有失败原因可查。
		if _, ferr := q.ledger.Transition(ctx, id, TaskFailed, TransitionOptions{
			At: time.Now().UTC(), Error: "派发失败: " + err.Error(),
		}); ferr != nil {
			return nil, fmt.Errorf("派发失败且无法落账: %v（原错误: %w）", ferr, err)
		}
		return nil, err
	}
	_ = res
	// 派完就停：Ask 之后目标状态可能还没翻转，继续循环会把下一个任务
	// 塞进同一批（违反「一个任务 = 一次干净 Run」）。
	return dispatched, nil
}

// QueuedCount 目标的排队长度（UI/诊断用）。
func (q *Queue) QueuedCount(workspace, sessionID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending[queueKey(workspace, sessionID)])
}

// dispatchClientMsgID 派发时使用的幂等键：优先调用方给的 client_msg_id。
func dispatchClientMsgID(t *Task) string {
	if t == nil {
		return ""
	}
	if id := strings.TrimSpace(t.ClientMsgID); id != "" {
		return id
	}
	return string(t.ID)
}

// CancelQueued 取消一条仍在 Hub 队列里的任务：先从内存队列摘除，再落 canceled。
// 非 queued（已派发/已结束）返回 ErrInvalid，由调用方走 interrupt 或幂等返回。
func (q *Queue) CancelQueued(ctx context.Context, id TaskID) (*Task, error) {
	if q == nil || q.ledger == nil {
		return nil, fmt.Errorf("%w: queue 未初始化", ErrInvalid)
	}
	id = TaskID(strings.TrimSpace(string(id)))
	if id == "" {
		return nil, fmt.Errorf("%w: task_id 不能为空", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t, err := q.ledger.Replay(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Status != TaskQueued {
		return nil, fmt.Errorf("%w: task %s 状态 %q，不能按未开始取消", ErrInvalid, id, t.Status)
	}
	key := queueKey(t.Target.Workspace, t.Target.SessionID)
	q.mu.Lock()
	ids := q.pending[key]
	idx := -1
	for i, v := range ids {
		if v == id {
			idx = i
			break
		}
	}
	q.mu.Unlock()
	canceled, err := q.ledger.Transition(ctx, id, TaskCanceled, TransitionOptions{
		At: time.Now().UTC(), Error: "用户取消（未开始）",
	})
	if err != nil {
		return nil, err
	}
	if idx >= 0 {
		q.mu.Lock()
		// 重新查一次：Transition 期间队首可能已被 Pump 派走（那时 idx 已失效）。
		cur := q.pending[key]
		for i, v := range cur {
			if v == id {
				q.pending[key] = append(append([]TaskID(nil), cur[:i]...), cur[i+1:]...)
				break
			}
		}
		q.mu.Unlock()
	}
	return canceled, nil
}
