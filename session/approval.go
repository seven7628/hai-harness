package session

import (
	"context"
	"errors"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"sync"
	"time"
)

// 默认审批等待时限：超时视为拒绝（Block 回传 LLM，模型可自行调整）。
// 2026-09-20（用户拍板）：60 秒 → 30 分钟 —— 审批是「人读 diff 再决定」，60 秒在人还没
// 看清/还在翻对话历史时就自动拒绝了（BROWSER_USE_IMPLEMENTATION.md 里也记过「60s 审批超时
// 误杀」）。与提问窗口对齐：最长 30 分钟。
const defaultApprovalTimeout = 30 * time.Minute

// MaxApprovalTimeout 审批等待窗口上限：newApprovalWaiter 把超过它的值钳到上限
// （与 events.MaxQuestionTimeout 同值同语义：HITL 两个通道的等待窗口都封顶 30 分钟）。
const MaxApprovalTimeout = 30 * time.Minute

// ErrNoPendingApproval 无待审批请求（Session.Approve 匹配不到）。
var ErrNoPendingApproval = errors.New("no pending approval for this call id")

// ErrApprovalNotHead 请求仍在 Session 审批队列中，但尚未轮到它。
// 非队首请求不会向 UI 发出可见审批事件，也不能提前注入决策。
var ErrApprovalNotHead = errors.New("approval is not at the head of the session queue")

// ErrDuplicateApproval 同一个 ToolCall.Id 不能同时注册两次。
// ToolCall.Id 是 Session.Approve 的决策锚；重复锚会让 UI 无法确定应决策哪一次调用。
var ErrDuplicateApproval = errors.New("duplicate pending approval id")

type approvalOutcome struct {
	approved bool
	err      error
}

// approvalRequest 是 Session 级 FIFO 中的一项。ready 只对当前队首关闭；
// outcome 为缓冲通道，允许决定方先于等待 goroutine 完成清理。
type approvalRequest struct {
	call    core.ToolCall
	ready   chan struct{}
	outcome chan approvalOutcome
}

// approvalWaiter 审批等待器（events.Approver 的会话侧实现）：
// 发 ToolApprovalRequested 事件由 UI 展示，用户经 Session.Approve(id, decision)
// 注入决策；等待期间单 goroutine 暂停（同步阻塞），超时 = 拒绝。
//
// queue 是整个 Session 的单一 FIFO：主 Agent 与所有继承 Approver 的并行
// SubAgent 共用它，不按运行或 SubAgent 拆分。只有队首请求会被 engine 发事件，
// 只有队首请求能接受 decide；队首结束后才唤醒下一项。
type approvalWaiter struct {
	timeout time.Duration

	mu       sync.Mutex
	queue    []*approvalRequest
	waits    map[string]*approvalRequest // 调用 id → FIFO 节点
	closed   bool
	closedCh chan struct{} // Session 关闭时唤醒仍在等候队首的调用
}

func newApprovalWaiter(timeout time.Duration) *approvalWaiter {
	if timeout <= 0 {
		timeout = defaultApprovalTimeout
	}
	if timeout > MaxApprovalTimeout {
		timeout = MaxApprovalTimeout // 等待窗口上限（与提问窗口对齐：最长 30 分钟）
	}
	return &approvalWaiter{
		timeout:  timeout,
		waits:    map[string]*approvalRequest{},
		closedCh: make(chan struct{}),
	}
}

// BeginApproval 实现 events.Approver。
// 旧接口没有把 ctx 传给注册阶段，因此用 background 等待队首；engine 对
// approvalWaiter 使用下面的 BeginApprovalWithContext，以便非队首请求可被
// 自身运行 ctx 取消而不会卡在注册阶段。
func (w *approvalWaiter) BeginApproval(call core.ToolCall) func(ctx context.Context) (bool, error) {
	wait, err := w.BeginApprovalWithContext(context.Background(), call)
	if err != nil {
		return func(context.Context) (bool, error) { return false, err }
	}
	return wait
}

// BeginApprovalWithContext 注册请求并等待其成为队首。返回 wait 之前不应发出
// ToolApprovalRequested 事件，因此调用方可以保证 UI 只看到当前队首。
// 这是可选的 events.ContextualApprover 扩展，不破坏旧 Approver 实现。
func (w *approvalWaiter) BeginApprovalWithContext(ctx context.Context, call core.ToolCall) (func(context.Context) (bool, error), error) {
	req, err := w.register(call)
	if err != nil {
		return nil, err
	}
	if err := w.awaitTurn(ctx, req); err != nil {
		return nil, err
	}
	return w.waitFunc(req), nil
}

func (w *approvalWaiter) register(call core.ToolCall) (*approvalRequest, error) {
	req := &approvalRequest{
		call:    call,
		ready:   make(chan struct{}),
		outcome: make(chan approvalOutcome, 1),
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, ErrSessionClosed
	}
	if _, exists := w.waits[call.Id]; exists {
		return nil, ErrDuplicateApproval
	}
	w.waits[call.Id] = req
	wasEmpty := len(w.queue) == 0
	w.queue = append(w.queue, req)
	if wasEmpty {
		close(req.ready)
	}
	return req, nil
}

// awaitTurn 只负责注册后的队首资格等待。ctx 取消时把节点从 FIFO 移除，
// 这样取消的 SubAgent 不会阻塞主 Agent 或后续 SubAgent。
func (w *approvalWaiter) awaitTurn(ctx context.Context, req *approvalRequest) error {
	select {
	case <-req.ready:
		return nil
	case <-ctx.Done():
		if w.cancel(req, approvalOutcome{err: ctx.Err()}) {
			return ctx.Err()
		}
		// 决策/关闭与 ctx 同时发生时，节点已经有唯一终态；若已轮到它，
		// 允许调用方继续到 wait，让已注册的事件与决策完成收敛。
		select {
		case <-req.ready:
			return nil
		default:
			return ctx.Err()
		}
	case <-w.closedCh:
		return ErrSessionClosed
	}
}

func (w *approvalWaiter) waitFunc(req *approvalRequest) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		timer := time.NewTimer(w.timeout)
		defer timer.Stop()
		select {
		case out := <-req.outcome:
			return out.approved, out.err
		case <-ctx.Done():
			if w.cancel(req, approvalOutcome{err: ctx.Err()}) {
				return false, ctx.Err()
			}
			return receiveApprovalOutcome(req)
		case <-timer.C:
			if w.cancel(req, approvalOutcome{}) {
				// 超时 = 拒绝（Block，模型可见），但带上「无人决策」的哨兵错：
				// engine 据此与「用户拒绝」区分措辞（避免模型以为用户否决了它）。
				return false, events.ErrApprovalTimeout
			}
			return receiveApprovalOutcome(req)
		case <-w.closedCh:
			if w.cancel(req, approvalOutcome{err: context.Canceled}) {
				return false, context.Canceled
			}
			return receiveApprovalOutcome(req)
		}
	}
}

func receiveApprovalOutcome(req *approvalRequest) (bool, error) {
	out := <-req.outcome
	return out.approved, out.err
}

// removeLocked 从队列中摘除 req。调用方必须持有 w.mu。
// 返回 false 表示该节点已经由另一个终态路径摘除。
func (w *approvalWaiter) removeLocked(req *approvalRequest) bool {
	if current, ok := w.waits[req.call.Id]; !ok || current != req {
		return false
	}
	index := -1
	for i, queued := range w.queue {
		if queued == req {
			index = i
			break
		}
	}
	if index < 0 {
		delete(w.waits, req.call.Id)
		return false
	}
	wasHead := index == 0
	w.queue = append(w.queue[:index], w.queue[index+1:]...)
	delete(w.waits, req.call.Id)
	if wasHead && len(w.queue) > 0 {
		close(w.queue[0].ready)
	}
	return true
}

// cancel 以指定 outcome 结束一个仍在队列中的节点，并唤醒下一项。
func (w *approvalWaiter) cancel(req *approvalRequest, out approvalOutcome) bool {
	w.mu.Lock()
	removed := w.removeLocked(req)
	w.mu.Unlock()
	if removed {
		req.outcome <- out
	}
	return removed
}

// decide 注入用户决策（Session.Approve 调用）。只有队首能决策；
// 非队首/未知/已结束 id 都不会改变队列。
func (w *approvalWaiter) decide(id string, approve bool) error {
	w.mu.Lock()
	req, ok := w.waits[id]
	if !ok {
		w.mu.Unlock()
		return ErrNoPendingApproval
	}
	if len(w.queue) == 0 || w.queue[0] != req {
		w.mu.Unlock()
		return ErrApprovalNotHead
	}
	removed := w.removeLocked(req)
	w.mu.Unlock()
	if !removed {
		return ErrNoPendingApproval
	}
	req.outcome <- approvalOutcome{approved: approve}
	return nil
}

// close 结束 Session 内所有未决审批，唤醒当前等待和队列中的 SubAgent。
// 幂等；已完成的审批不受影响。
func (w *approvalWaiter) close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	pending := append([]*approvalRequest(nil), w.queue...)
	w.queue = nil
	w.waits = map[string]*approvalRequest{}
	close(w.closedCh)
	w.mu.Unlock()
	for _, req := range pending {
		req.outcome <- approvalOutcome{err: context.Canceled}
	}
}
