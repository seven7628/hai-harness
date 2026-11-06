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
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Queue 是 Hub 的派发闸门。零值不可用，用 NewQueue 构造。
type Queue struct {
	ledger *Ledger
	ops    WorkspaceOps

	// mu 只保护 pending 的短临界区（索引），不得在持锁时做账本/会话 I/O。
	mu      sync.Mutex
	pending map[string][]TaskID // key = workspace + "\x00" + session_id

	// targets 是每个 (workspace, session) 一把**不可重入**的互斥锁：
	// Pump/Dispatch/Submit 的查重+建账+Cancel 只在同一目标内串行，
	// 不再让一个忙碌会话阻塞其他会话（P2.5-03）。
	// 契约：ops.Dispatch / SessionStatus / busy 回调不得同步反向调用本 Queue
	// 的同目标方法（Dispatch 在持锁时调用，重入会自锁）。
	targets keyedLocks
}

// keyedLocks 是带引用计数的按 key 互斥锁表：lock 返回时该 key 已加锁，
// 调用方必须执行返回的 unlock。refs 归零即回收，避免 client_msg_id 之类
// 一次性 key 无限增长。
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &keyedLock{}
		k.m[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

func NewQueue(ledger *Ledger, ops WorkspaceOps) *Queue {
	return &Queue{ledger: ledger, ops: ops, pending: map[string][]TaskID{}}
}

func queueKey(workspace, sessionID string) string {
	// 归一尾斜杠/`.`/`..`：knownWorkspace 允许尾斜杠，队列 key 不容忍会让
	// 同一会话出现两个 key、任务永远泵不出（P2-03 复审 NB-4）。
	return filepath.Clean(strings.TrimSpace(workspace)) + "\x00" + strings.TrimSpace(sessionID)
}

// taskRetryable 表示该终态允许用同一个 client_msg_id 重投：任务没有真正
// 送达/跑过，返回旧条目会让调用方以为重试成功（P2-02 复审 B1）。
func taskRetryable(st TaskStatus) bool {
	switch st {
	case TaskFailed, TaskCanceled, TaskDeclined, TaskBlockedBudget, TaskInterrupted:
		// interrupted（进程重启时没有运行记录）也可以同 client_msg_id 重投；
		// 否则重启后的任务会永远返回同一 interrupted 快照（P2.5-07 N1）。
		return true
	}
	return false
}

func queueContains(ids []TaskID, id TaskID) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// pendingIDs 返回队列切片头（只读）。调用方必须持有该 key 的目标锁，
// 否则 append/切片替换会让读到的 id 与实际队列分叉。
func (q *Queue) pendingIDs(key string) []TaskID {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending[key]
}

// hasPendingLocked 在持有目标锁时判断 id 是否已入队（q.mu 仅短临界区）。
func (q *Queue) hasPendingLocked(key string, id TaskID) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return queueContains(q.pending[key], id)
}

// enqueueLocked 追加到内存队列并去重；调用方必须持有目标锁。
func (q *Queue) enqueueLocked(key string, id TaskID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if queueContains(q.pending[key], id) {
		return
	}
	q.pending[key] = append(q.pending[key], id)
}

// dequeueLocked 从内存队列摘除 id；调用方必须持有目标锁。
func (q *Queue) dequeueLocked(key string, id TaskID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	cur := q.pending[key]
	for i, got := range cur {
		if got == id {
			q.pending[key] = append(append([]TaskID(nil), cur[:i]...), cur[i+1:]...)
			return
		}
	}
}

// CheckGuard 在建账前跑护栏（预算/并发/去重/链长）；nil guard = 不检查。
func (q *Queue) CheckGuard(ctx context.Context, t *Task, g *Guard) error {
	if g == nil {
		return nil
	}
	if q == nil || q.ledger == nil {
		return fmt.Errorf("%w: queue 未初始化", ErrInvalid)
	}
	return g.Check(ctx, q.ledger.store, t)
}

// Submit 创建任务并尝试派发。返回值是**账本条目**：Status=dispatched 表示已投递，
// queued 表示目标忙、留在队列里（结果回推在 P2-05）。
func (q *Queue) Submit(ctx context.Context, t *Task) (*Task, error) {
	return q.submit(ctx, t, nil)
}

// SubmitGuarded 是带护栏的 Submit：查重命中直接返回/重投，只有真正要建新任务时
// 才跑 Guard.Check。护栏放在查重之后，合法同 client_msg_id 重试才不会被
// 「重复任务」误杀（P2-02 复审 §5）。
func (q *Queue) SubmitGuarded(ctx context.Context, t *Task, g *Guard) (*Task, error) {
	return q.submit(ctx, t, g)
}

func (q *Queue) submit(ctx context.Context, t *Task, g *Guard) (*Task, error) {
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
	// 键口径统一：workspace 归一到 knownWorkspace 的形态，session_id 去首尾空白。
	// 否则 "/w/" 与 "/w" 会各自查重、建出两条任务，目标侧却按归一路径判重，
	// 第二条被静默丢弃（P2-02 复审 N1）。
	t.Target.Workspace = filepath.Clean(strings.TrimSpace(t.Target.Workspace))
	t.Target.SessionID = strings.TrimSpace(t.Target.SessionID)
	// 同一目标的所有「查重 + 建账 + 入队」由目标锁串行：不同会话互不阻塞，
	// 同 client_msg_id 并发提交仍只会建一账（P2.5-03）。
	key := queueKey(t.Target.Workspace, t.Target.SessionID)
	unlockTarget := q.targets.lock(key)
	locked := true
	release := func() {
		if locked {
			unlockTarget()
			locked = false
		}
	}
	defer release()

	if t.ClientMsgID != "" {
		existing, ferr := q.ledger.FindByClientMsgID(ctx, t.Target, t.ClientMsgID)
		if ferr != nil {
			return nil, ferr
		}
		if existing != nil {
			switch {
			case existing.Status == TaskQueued && !q.hasPendingLocked(key, existing.ID):
				// 进程重启后内存队列为空，但账本里任务仍是 queued。只返回不
				// 入队会让 session_send 说「已排队」、实际永远没人泵（B2）。
				q.enqueueLocked(key, existing.ID)
				release()
				if _, perr := q.Pump(ctx, existing.Target); perr != nil {
					return existing, perr
				}
				if latest, rerr := q.ledger.Replay(ctx, existing.ID); rerr == nil && latest != nil {
					return latest, nil
				}
				return existing, nil
			case taskRetryable(existing.Status):
				// 上一次没送达/没跑完：同 id 重试是新 attempt，不能返回旧终态。
				// 先分配 ID 才能把新任务接进原链（parent 必须在 chain 里）。
				id, ierr := q.ledger.store.AllocateID(ctx, PrefixTask)
				if ierr != nil {
					return nil, ierr
				}
				t.ID = TaskID(id)
				t.RootTaskID = existing.RootTaskID
				t.ParentTaskID = existing.ID
				t.Attempt = existing.Attempt + 1
				t.Chain = append(append([]TaskID{}, existing.Chain...), t.ID)
			default:
				release()
				// 命中可能是「账本说 queued、实际已被 pump 派走」的旧快照：
				// 返回前重读，别让调用方拿到过期状态（P2-02 N4）。
				if latest, rerr := q.ledger.Replay(ctx, existing.ID); rerr == nil && latest != nil {
					return latest, nil
				}
				return existing, nil
			}
		}
	}
	if g != nil {
		if gerr := g.Check(ctx, q.ledger.store, t); gerr != nil {
			return nil, gerr
		}
	}
	created, err := q.ledger.Create(ctx, t)
	if err != nil {
		return nil, err
	}
	q.enqueueLocked(key, created.ID)
	release()
	dispatched, err := q.Pump(ctx, created.Target)
	if err != nil {
		return created, err // 任务已落账本（failed/canceled）：调用方仍拿得到 task_id
	}
	// 只有「刚提交的这个」被派出去时才返回它；Pump 可能泵的是队首的旧任务，
	// 把别人的账本条目返回给调用方会让 session_send 把错 task_id 交给模型（P2-03 B2）。
	if dispatched != nil && dispatched.ID == created.ID {
		return dispatched, nil
	}
	// 目标忙/队首是旧任务：本任务已落账本（queued）。并发 pump 可能在 append 后
	// 把它派走 —— 返回前重读账本，别让 Status/queued 说谎（P2-03 复审 NB-2）。
	if latest, rerr := q.ledger.Replay(ctx, created.ID); rerr == nil && latest != nil {
		return latest, nil
	}
	return created, nil
}

// Pump 在目标空闲时派发队首任务；每次最多派发一个。
// 目标忙 / 队列空时返回 (nil, nil)。
func (q *Queue) Pump(ctx context.Context, target TaskTarget) (*Task, error) {
	if q == nil || q.ledger == nil || q.ops == nil {
		return nil, fmt.Errorf("%w: queue 未初始化", ErrInvalid)
	}
	key := queueKey(target.Workspace, target.SessionID)
	unlockTarget := q.targets.lock(key)
	defer unlockTarget()
	ids := q.pendingIDs(key)
	if len(ids) == 0 {
		return nil, nil
	}
	st, err := q.ops.SessionStatus(ctx, target.Workspace, target.SessionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// 目标会话已删除/关闭：不能让它永远占住队首（后续任务全被堵死）。
			// 与 B3 同一不变量：Transition 成功后才出队；失败保留队首等存储恢复。
			id := ids[0]
			settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			_, terr := q.ledger.Transition(settleCtx, id, TaskCanceled, TransitionOptions{
				At: time.Now().UTC(), Error: "目标会话不存在",
			})
			cancel()
			if terr != nil {
				return nil, terr
			}
			q.dequeueLocked(key, id)
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
		// 账本状态已不是 queued（被恢复/取消等旁路改成终态）：这条队首是陈旧
		// 内存条目，摘掉让后面的任务继续；否则每次 Pump 都死在队首，目标永久堵死。
		if latest, rerr := q.ledger.Replay(ctx, id); rerr == nil && latest != nil && latest.Status != TaskQueued {
			q.dequeueLocked(key, id)
			return nil, nil
		}
		// 仍是 queued 说明是存储/事件错误：保留队首等恢复（P2-03 B3）。
		return nil, err
	}
	q.dequeueLocked(key, id)
	header, _ := EncodeTaskHeader(dispatched)
	res, err := q.ops.Dispatch(ctx, DispatchReq{
		AgentID:   dispatched.Origin.AgentID,
		Workspace: dispatched.Target.Workspace,
		SessionID: dispatched.Target.SessionID,
		Text:      dispatched.Instruction,
		// 有调用方幂等键就用它（目标侧去重表按此判重）；重试 attempt 用
		// 新 task id 作为键，避免被上一次投递的去重记录挡住（P2-02 B1）。
		ClientMsgID: dispatchClientMsgID(dispatched),
		TaskHeader:  header, // 目标会话据此绑定 task（§6.7.2）
	})
	if err != nil {
		// 已 dispatched 再落 failed：状态机有序，账本里有失败原因可查。
		// 一旦出队，收尾不能被调用方 ctx 取消（否则任务永久停在 dispatched，
		// 只有下次重启恢复才会被标 interrupted，P2.5-06 复审 N1）。
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, ferr := q.ledger.Transition(settleCtx, id, TaskFailed, TransitionOptions{
			At: time.Now().UTC(), Error: "派发失败: " + err.Error(),
		})
		cancel()
		if ferr != nil {
			return nil, fmt.Errorf("派发失败且无法落账: %v（原错误: %w）", ferr, err)
		}
		return nil, err
	}
	if res.Message == DispatchDuplicateMessage {
		// 目标侧按相同 client_msg_id 判重：说明这条没有真正 Ask。不能把
		// dispatched 当成功返回（静默丢任务，P2-02 复审 N1）；落 canceled 留痕。
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		canceled, cerr := q.ledger.Transition(settleCtx, id, TaskCanceled, TransitionOptions{
			At: time.Now().UTC(), Error: "目标侧去重命中：同一 client_msg_id 已投递过（本次未投递；先 task_status 确认，必要时换新 client_msg_id）",
		})
		cancel()
		if cerr != nil {
			return nil, cerr
		}
		return canceled, nil
	}
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
	if t.Attempt > 1 {
		// 同 client_msg_id 的重试是新的投递意图；目标侧去重表还留着上一次的键，
		// 复用它会让重试静默命中 duplicate。新 attempt 用新 task id。
		return string(t.ID)
	}
	if t.ClientMsgID != "" {
		return t.ClientMsgID
	}
	return string(t.ID)
}

// Restore 把账本里 queued 的任务放回内存队列（进程重启恢复；不立即派发）。
// 重复调用幂等：同一 id 只入队一次。
//
// 传入的 t 可能已在锁外被派发/结算（恢复扫描与 session_send 并发）：目标锁内
// 必须重读账本，只有仍为 queued 才入队。否则会把 dispatched 任务再 Ask 一次
// （目标侧判重 → canceled，真实 run 的结果被静默丢弃），或把 completed 塞到
// 队首让该目标永久 Transition 失败（P2.5-05 复审 B1）。
func (q *Queue) Restore(t *Task) {
	if q == nil || q.ledger == nil || t == nil || t.Status != TaskQueued {
		return
	}
	key := queueKey(t.Target.Workspace, t.Target.SessionID)
	unlockTarget := q.targets.lock(key)
	defer unlockTarget()
	cur, err := q.ledger.Replay(context.Background(), t.ID)
	if err != nil || cur == nil || cur.Status != TaskQueued {
		return
	}
	q.enqueueLocked(key, cur.ID)
}

// CancelDispatchedIfIdle 在目标锁内重读任务并取消「已派发但会话不忙」的任务。
// busy 在锁内求值：Pump 的 Transition(dispatched)→Dispatch 全程持目标锁，
// 因此这里看到的 busy/latch 一定是 Dispatch 完成后的状态，不会把「已经 Ask
// 进会话」的任务误判为空闲取消（P2-06 复审 B-2）。不同目标互不阻塞。
// 返回 (任务, 是否本次取消, err)；已终态返回最新快照 + canceled=false。
func (q *Queue) CancelDispatchedIfIdle(ctx context.Context, id TaskID, reason string, busy func() bool) (*Task, bool, error) {
	if q == nil || q.ledger == nil {
		return nil, false, fmt.Errorf("%w: queue 未初始化", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// 先无锁读出目标（目标字段不可变），再只锁该目标重读；锁内不碰其他会话。
	pre, err := q.ledger.Replay(ctx, id)
	if err != nil {
		return nil, false, err
	}
	unlockTarget := q.targets.lock(queueKey(pre.Target.Workspace, pre.Target.SessionID))
	defer unlockTarget()
	t, err := q.ledger.Replay(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if t.Status.Terminal() {
		return t, false, nil
	}
	if t.Status != TaskDispatched && t.Status != TaskRunning {
		return t, false, fmt.Errorf("%w: task %s 状态 %q，不能按已派发取消", ErrInvalid, id, t.Status)
	}
	if busy != nil && busy() {
		return t, false, nil
	}
	canceled, terr := q.ledger.Transition(ctx, id, TaskCanceled, TransitionOptions{
		At: time.Now().UTC(), Error: reason,
	})
	if terr != nil {
		if latest, rerr := q.ledger.Replay(ctx, id); rerr == nil && latest.Status.Terminal() {
			return latest, false, nil
		}
		return nil, false, terr
	}
	return canceled, true, nil
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
	// 先无锁读出目标，再只锁该目标：Pump 的「出队 → Transition(dispatched)
	// → Dispatch」也持同一目标锁，cancel 与派发不会交错（P2-06 复审）。
	pre, err := q.ledger.Replay(ctx, id)
	if err != nil {
		return nil, err
	}
	key := queueKey(pre.Target.Workspace, pre.Target.SessionID)
	unlockTarget := q.targets.lock(key)
	defer unlockTarget()
	t, err := q.ledger.Replay(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Status != TaskQueued {
		return nil, fmt.Errorf("%w: task %s 状态 %q，不能按未开始取消", ErrInvalid, id, t.Status)
	}
	canceled, err := q.ledger.Transition(ctx, id, TaskCanceled, TransitionOptions{
		At: time.Now().UTC(), Error: "用户取消（未开始）",
	})
	if err != nil {
		return nil, err
	}
	q.dequeueLocked(key, id)
	return canceled, nil
}
