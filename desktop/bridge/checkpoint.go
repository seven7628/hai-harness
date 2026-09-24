// checkpoint.go —— bridge 侧 SessionRecord 提交协调器与 checkpoint 恢复入口。
//
// 目标架构（docs/SESSION_RECOVERY_FIX_2026-08-26.md §3.1.2/3.1.3）：
//   - 每个 bridgeSession 持有 ViewReducer + 一个 commit 协调器；
//   - emit 是唯一事件漏斗：先 Apply（reducer 维护稳定视图），再发 stdout/IPC；
//   - 到达语义边界（llm_end / tool / task / agent / compression 结束、clear、
//     session close、用户输入确认等）时异步请求 commit；
//   - commit 在单 worker 内串行：reducer.Snapshot()（深拷贝，不阻塞事件流）+
//     sdk_state 快照 + 同一 revision 原子写 record；
//   - 冷恢复优先读取 SessionRecord.view_checkpoint 直接 hydrate；
//     无 record（新会话/旧数据）回退 legacy buildSnapshot 路径（不破坏现状）。
//
// 提交节流（性能优化，2026-08-28）：
//
//	每个语义边界请求都立即提交 = 每次 llm_end 一次 347ms 的 Marshal+fsync 原子落盘，
//	长会话下累积延迟显著。改为「节流窗口合并」：
//	  - worker 收到请求先进入 debounce 等待（commitDebounce，默认 800ms）；
//	  - 窗口内到达的多个边界请求合并为一次提交（拍最新快照，revision 只 +1）；
//	  - 窗口结束或队列再次排空后提交；stop（关闭）时立即 drain 全部剩余。
//	效果：连续 llm_end/tool_end 从「每事件 347ms」变为「每 800ms 一次」，
//	中间边界全部合并，单次 commit 成本摊薄。
//
// 已知边界（渐进式，后续阶段补齐）：
//   - sdk_state.pending 已填充（Session.PendingSnapshot 非破坏性读 inbox）；
//   - warm 会话（进程内复用）仍走 legacy snapshot 路径（renderer/IPC 契约在后续阶段接入）。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/session"
	"github.com/seven7628/hai-harness/todo"
)

// commitDebounce 是提交节流的窗口时长：边界请求到达后等待该时长，窗口内
// 到达的请求合并为一次提交。0 = 关闭节流（立即提交，兼容旧行为/测试）。
const commitDebounce = 800 * time.Millisecond

// recordRoot 是 session-record namespace 的根目录（~/.go-code/sessions）。
// FileStorage.RecordPath 会在其下拼 <workspaceKey>/<sid>.record.json，
// 因此这里必须传 sessions 根而不是单工作区目录。
func recordRoot() string {
	return filepath.Join(appDataDir(), "sessions")
}

// newRecordStore 创建 session-record 存储（root = ~/.go-code/sessions）。
func newRecordStore() (*session.FileStorage, error) {
	return session.NewSessionRecordStorage(recordRoot())
}

// checkpointCoordinator 是单个主 Session 的提交协调器（单写者）：
// 请求入缓冲队列，worker 串行拍摄 reducer 快照并原子提交 record。
// 提交失败不阻塞事件流：coordinator 仅失效 revision 缓存，等下一次边界重试。
//
// 节流：请求先入队列，worker 在 debounce 窗口内合并多个请求为一次提交
// （见文件头注释）。窗口由 timer 触发，关闭时 drain 全部剩余。
//
// syncMode：测试隔离环境（appDataDirOverride 非空）启用同步提交——Request 直接
// 完成 commitOnce（emit 返回时 record 已落盘），避免异步 worker 与 t.TempDir()
// 清理产生竞态；生产（覆盖为空）保持异步 + 节流 + drain。
type checkpointCoordinator struct {
	wsKey    string
	sessID   string
	store    *session.FileStorage
	reducer  *ViewReducer
	s        *session.Session
	syncMode bool

	ch   chan struct{} // 有缓冲；非阻塞请求（满则跳过，极端 fsync 卡死时保护事件流）
	stop <-chan struct{}
	done chan struct{}

	mu       sync.Mutex // 序列化 commitOnce（revision 分配 + 提交）
	lastRev  uint64
	revKnown bool
	commits  uint64 // 成功提交计数（测试观测）
	skipped  uint64 // 因缓冲满跳过的请求计数

	// onError 提交失败回调（宿主注入）：发结构化 SessionPersistError 事件，
	// 让持久化失败可观察（V2 P1-PERSIST-04）——不能静默吞掉（用户会以为已保存）。
	onError func(err error)
}

// newCheckpointCoordinator 构造协调器。stop 为会话级 ctx.Done()（关闭时 drain 收尾）。
// reducer 与 store 必须已初始化；s 为 nil 时 sdk_state 记录为空 State（仍提交 view）。
func newCheckpointCoordinator(wsKey, sessID string, store *session.FileStorage, reducer *ViewReducer, s *session.Session, stop <-chan struct{}, syncMode bool) *checkpointCoordinator {
	c := &checkpointCoordinator{
		wsKey: wsKey, sessID: sessID, store: store, reducer: reducer,
		s: s, syncMode: syncMode, ch: make(chan struct{}, 256), stop: stop, done: make(chan struct{}),
	}
	go c.run()
	return c
}

// Request 非阻塞请求一次提交；syncMode 下同步完成（见类型注释）。
func (c *checkpointCoordinator) Request() {
	if c == nil {
		return
	}
	if c.syncMode {
		c.commitOnce()
		return
	}
	select {
	case c.ch <- struct{}{}:
	default:
		c.mu.Lock()
		c.skipped++
		c.mu.Unlock()
	}
}

// run worker：节流窗口合并 + stop drain。
// 语义：ch 收到请求 → 启动 debounce timer；timer 触发时把窗口内积压的
// 请求合并为一次 commit（ch 里剩余的也一起 drain 掉）。stop 到达时立即
// 清空 ch 完成全部剩余提交（关闭不丢最后状态）。
func (c *checkpointCoordinator) run() {
	defer close(c.done)
	var timer *time.Timer
	var timerC <-chan time.Time
	resetTimer := func() {
		if timer == nil {
			timer = time.NewTimer(commitDebounce)
			timerC = timer.C
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(commitDebounce)
			timerC = timer.C // 关键：stopTimer 置 nil 后必须恢复，否则后续窗口永不触发
		}
	}
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timerC = nil
		}
	}
	for {
		select {
		case <-c.ch:
			// 收到边界请求：启动/重置节流窗口（窗口内后续请求合并）
			resetTimer()
		case <-timerC:
			// 窗口结束：drain 队列中全部积压（合并为一次提交），提交
			stopTimer()
			c.drainAndCommit()
		case <-c.stop:
			// 优雅关闭：立即提交剩余（不等待窗口）。关闭后并发 emit 已停止
			//（Session.Shutdown 先于 ctx.Done 完成），此处循环 drain 直到队列稳定空，
			// 保证 stop 与最后一次 Request 的竞争不丢状态。
			// syncMode 下请求已同步提交（队列恒空），无需再 drain——避免空提交
			// 与测试 TempDir 清理竞态。
			stopTimer()
			if !c.syncMode {
				for {
					c.drainAndCommit()
					select {
					case <-c.ch:
						continue // drain 后又有新请求（极端竞争）→ 再提交
					default:
					}
					break
				}
			}
			return
		}
	}
}

// drainAndCommit 把队列中当前积压全部取出，合并为一次提交。
// 注意：新到达的请求可能晚于 drain 进入队列（并发 emit），它们会启动新的
// 窗口继续合并——因此这里只处理「当前已入队」的请求，不保证包含 drain 后
// 到达的事件（由下一次窗口覆盖，revision 单调不丢数据）。
func (c *checkpointCoordinator) drainAndCommit() {
	for {
		select {
		case <-c.ch:
			continue // 继续取，直到队列空
		default:
		}
		break
	}
	c.commitOnce()
}

// WaitDrained 返回时队列已空（不代表 worker 正在处理的 commit 已完成）。
// 优雅关闭/删除前等待全部落盘请依赖 stop channel 的 drain（coord.done），
// 此方法仅供测试观测队列水位。
func (c *checkpointCoordinator) WaitDrained() {
	if c == nil {
		return
	}
	for {
		c.mu.Lock()
		empty := len(c.ch) == 0
		c.mu.Unlock()
		if empty {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stats 返回 (commits, skipped) 供测试观测。
func (c *checkpointCoordinator) Stats() (uint64, uint64) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commits, c.skipped
}

// LastRevision 返回最近一次成功提交的 revision（无则 0）。
func (c *checkpointCoordinator) LastRevision() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRev
}

// Load 读取磁盘 record（恢复入口；失败返回 nil）。
func (c *checkpointCoordinator) Load(ctx context.Context) (*session.SessionRecord, error) {
	if c == nil || c.store == nil {
		return nil, session.ErrRecordNotFound
	}
	return c.store.LoadRecord(ctx, c.wsKey, c.sessID)
}

// Delete 删除磁盘 record（含 .bak；delete_session 调用）。
func (c *checkpointCoordinator) Delete(ctx context.Context) error {
	if c == nil || c.store == nil {
		return nil
	}
	return c.store.DeleteSessionRecord(ctx, c.wsKey, c.sessID)
}

func (c *checkpointCoordinator) commitOnce() {
	// 全程持锁：async worker 单协程处理；syncMode 下多 emit 并发 Request 也串行化。
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	view, err := c.reducer.Marshal()
	if err != nil {
		c.reportError(fmt.Errorf("checkpoint: marshal view: %w", err))
		return
	}

	rev, ok, rerr := c.nextRevisionLocked(ctx)
	if !ok {
		if rerr != nil {
			c.reportError(fmt.Errorf("checkpoint: read revision: %w", rerr))
		} else {
			c.reportError(fmt.Errorf("checkpoint: read revision failed"))
		}
		return
	}
	state := sdkState(c.s)
	record := session.NewSessionRecord(
		c.wsKey, c.sessID, rev, c.reducer.LastSeq(), c.reducer.ClearGeneration(), state, view,
	)
	if err := c.store.CommitRecord(ctx, record); err != nil {
		// 提交失败不阻塞事件流：失效 revision 缓存，下一边界重读磁盘基线；
		// 同时上报结构化错误（持久化失败必须可观察）。
		c.revKnown = false
		c.reportError(fmt.Errorf("checkpoint: commit record: %w", err))
		return
	}
	c.lastRev = rev
	c.revKnown = true
	c.commits++
}

// reportError 上报提交错误（幂等保护：onError 回调内不再回调自身）。
func (c *checkpointCoordinator) reportError(err error) {
	if c.onError != nil {
		c.onError(err)
	}
}

// nextRevisionLocked 计算下一次提交的 revision（持锁调用）：优先内存缓存；
// 失效/首次时读磁盘基线。
func (c *checkpointCoordinator) nextRevisionLocked(ctx context.Context) (uint64, bool, error) {
	rev, known := c.lastRev, c.revKnown
	if known {
		return rev + 1, true, nil
	}
	rec, err := c.store.LoadRecord(ctx, c.wsKey, c.sessID)
	if err != nil {
		if !isRecordNotFound(err) {
			return 0, false, err
		}
		rec = nil
	}
	if rec != nil {
		rev = rec.Revision
	}
	return rev + 1, true, nil
}

func isRecordNotFound(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "not found") || os.IsNotExist(err))
}

// sdkState 构造 record 中的 sdk_state 快照。
// Pending 来自 Session.PendingSnapshot()（非破坏性读 inbox）——崩溃恢复时由
// NewSession 从 record 恢复并重新入队，输入不丢（对齐 legacy State.Pending 语义）。
func sdkState(s *session.Session) session.State {
	now := time.Now().UTC()
	if s == nil {
		return session.State{UpdatedAt: now}
	}
	// 运行中快照优先（V2 P1-PERSIST-01）：OnTurn checkpoint 后、absorb 前，Session 的
	// s.Messages() 仍是旧值——record 提交必须用 ac.Messages 的语义点，否则崩溃恢复时
	// SDK 状态落后于 UI（reducer 已 Apply 最新事件）。absorb 后 RunningSnapshot 为 nil，
	// 回退 s.Messages()（已吸收，权威一致）。
	var messages []core.Message
	if snap := s.RunningSnapshot(); snap != nil {
		messages = snap
	} else {
		messages = s.Messages()
	}
	return session.State{
		Messages:  messages,
		Usage:     s.Usage(),
		Cost:      s.Cost(),
		Pending:   s.PendingSnapshot(),
		Todo:      s.TodoItems(),
		UpdatedAt: now,
	}
}

// todoItems 把 SDK todo.Item 映射为 checkpoint 的 TodoItem（展示副本）。
func todoItems(in []todo.Item) []TodoItem {
	out := make([]TodoItem, 0, len(in))
	for _, it := range in {
		out = append(out, TodoItem{
			ID: it.Id, Title: it.Title, Done: it.Done,
			BlockedBy: append([]string{}, it.BlockedBy...),
		})
	}
	return out
}

// observe 在 emit 漏斗中：reducer.Apply + 提交边界检测。
// 必须在写 stdout/事件日志之前调用（保证 checkpoint 与实时事件同序）。
func (bs *bridgeSession) observe(e events.Event) {
	if bs == nil || bs.reducer == nil {
		return
	}
	meta := EventMeta{Provider: bs.provider}
	switch ev := e.(type) {
	case *events.LLMEnd:
		if ev.Usage != nil {
			// 与 emit 现有打标同表：单次 cost 随 turn 落 checkpoint
			meta.CostUSD = costUsd(bs.provider, ev.Model, *ev.Usage)
		}
	case *events.ToolResponse:
		if strings.HasPrefix(ev.Name, "todo_") {
			meta.Todos = todoItems(bs.s.TodoItems())
		}
	case *events.CompressEnd:
		meta.Todos = todoItems(bs.s.TodoItems())
	}
	_ = bs.reducer.Apply(e, meta)

	if commitBoundary(e) {
		if bs.coord != nil {
			bs.coord.Request()
		}
	}
}

// commitBoundary 判定语义提交边界（docs §3.1.3：用户输入消费、llm_end、
// tool/task/agent/compression 结束、clear、session close、显式 persist；
// 后台结果交付改变 SDK state 也提交）。
func commitBoundary(e events.Event) bool {
	switch e.(type) {
	case *events.LLMEnd, *events.ToolResponse, *events.TaskEnd,
		*events.TaskResultDelivered, *events.AgentEnd, *events.CompressEnd,
		*events.CommandResult, *events.SessionRunError, *events.SessionClosed,
		*events.UserInputsConsumed:
		return true
	}
	return false
}

// migrateLegacyToCheckpoint 懒迁移：无 record 的旧会话首次访问时，读 legacy events.jsonl
// 重放进 reducer 生成 checkpoint 并落盘。一次性成本（buildSnapshot 级 + reducer 重放），
// 之后该会话走 checkpoint 秒出。events 缺失/空 → 返回 false（保持 legacy 路径）。
func (bs *bridgeSession) migrateLegacyToCheckpoint() bool {
	if bs == nil || bs.coord == nil || bs.reducer == nil || bs.eventsDir == "" {
		return false
	}
	path := filepath.Join(bs.eventsDir, bs.id+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLineBytes)
	applied := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ev, err := parseLegacyEvent(line)
		if err != nil || ev == nil {
			continue // 坏行跳过（与 buildSnapshot 一致）
		}
		_ = bs.reducer.Apply(ev, EventMeta{Provider: bs.provider})
		applied++
	}
	// 扫描错误（超长行/IO 失败）：已读取前缀不完整，**不得提交为权威 record**
	//（V2 P1-PERSIST-03：部分迁移结果会覆盖/遮蔽可恢复数据）。返回 false 保持原状。
	if err := sc.Err(); err != nil {
		return false
	}
	if applied == 0 {
		return false // 无有效事件：不生成空 checkpoint
	}
	// 落盘 record（同步提交：迁移是一次性的，响应需要 record 已存在才能返回 checkpoint）
	if bs.coord != nil {
		// 直接调 commitOnce（持锁同步），不等异步 worker
		bs.coord.commitSync()
	}
	return true
}

// commitSync 同步执行一次提交（懒迁移用：响应前必须保证 record 已落盘）。
func (c *checkpointCoordinator) commitSync() {
	if c == nil {
		return
	}
	c.commitOnce()
}

// parseLegacyEvent 把 events.jsonl 一行解析为 typed events.Event。
// 支持 events 包的全部事件类型；未知类型返回 nil（跳过）。
func parseLegacyEvent(line string) (events.Event, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil, err
	}
	var et string
	if err := json.Unmarshal(raw["event_type"], &et); err != nil || et == "" {
		return nil, nil // 非事件行
	}
	// 用 events 包的已知类型构造（通过临时 struct 解析公共字段）
	// 简化：直接按 event_type 分发到各 typed struct
	switch events.EventType(et) {
	case events.AgentStartType:
		var e events.AgentStart
		return &e, json.Unmarshal([]byte(line), &e)
	case events.AgentEndType:
		var e events.AgentEnd
		return &e, json.Unmarshal([]byte(line), &e)
	case events.LLMStartType:
		var e events.LLMStart
		return &e, json.Unmarshal([]byte(line), &e)
	case events.LLMEndType:
		var e events.LLMEnd
		return &e, json.Unmarshal([]byte(line), &e)
	case events.LLMErrorType:
		var e events.LLMError
		return &e, json.Unmarshal([]byte(line), &e)
	case events.ContentChunkType:
		var e events.ContentChunk
		return &e, json.Unmarshal([]byte(line), &e)
	case events.ReasoningChunkType:
		var e events.ReasoningChunk
		return &e, json.Unmarshal([]byte(line), &e)
	case events.ToolRunStartType:
		var e events.ToolStart
		return &e, json.Unmarshal([]byte(line), &e)
	case events.ToolRunEndType:
		var e events.ToolResponse
		return &e, json.Unmarshal([]byte(line), &e)
	case events.ToolApprovalType:
		var e events.ToolApprovalRequested
		return &e, json.Unmarshal([]byte(line), &e)
	case events.TaskStartedType:
		var e events.TaskStarted
		return &e, json.Unmarshal([]byte(line), &e)
	case events.TaskEndType:
		var e events.TaskEnd
		return &e, json.Unmarshal([]byte(line), &e)
	case events.TaskResultDeliveredType:
		var e events.TaskResultDelivered
		return &e, json.Unmarshal([]byte(line), &e)
	case events.TaskPromotedType:
		var e events.TaskPromoted
		return &e, json.Unmarshal([]byte(line), &e)
	case events.CompressStartType:
		var e events.CompressStart
		return &e, json.Unmarshal([]byte(line), &e)
	case events.CompressEndType:
		var e events.CompressEnd
		return &e, json.Unmarshal([]byte(line), &e)
	case events.UserInputsConsumedType:
		var e events.UserInputsConsumed
		return &e, json.Unmarshal([]byte(line), &e)
	case events.CommandResultType:
		var e events.CommandResult
		return &e, json.Unmarshal([]byte(line), &e)
	case events.SessionRunErrorType:
		var e events.SessionRunError
		return &e, json.Unmarshal([]byte(line), &e)
	case events.GoalAlignmentReminderType:
		var e events.GoalAlignmentReminder
		return &e, json.Unmarshal([]byte(line), &e)
	case events.ToolHookWarningType:
		var e events.ToolHookWarning
		return &e, json.Unmarshal([]byte(line), &e)
	case events.AskUserQuestionType:
		var e events.AskUserQuestion
		return &e, json.Unmarshal([]byte(line), &e)
	case events.RefsLoadedType:
		var e events.RefsLoaded
		return &e, json.Unmarshal([]byte(line), &e)
	default:
		return nil, nil // 未知类型跳过（诊断计数由 reducer 处理）
	}
}

// parseCheckpoint 解析 record 内 view_checkpoint 为快照（恢复入口校验用）。
func parseCheckpoint(raw session.ViewCheckpoint) (*Checkpoint, error) {
	if len(raw) == 0 {
		return nil, os.ErrNotExist
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, err
	}
	if cp.SchemaVersion != checkpointSchemaVersion {
		return nil, os.ErrInvalid
	}
	return &cp, nil
}
