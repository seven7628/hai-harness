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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/session"
	"github.com/seven7628/hai-harness/todo"
)

// verifyEveryCommits 是「全量比对块内容」的周期。
//
// 脏块快路径依赖 reducer 精确标脏；万一某条变更路径漏标，磁盘会停在旧版本
// （用户看到永不更新的卡片）。因此每 N 次提交强制走一次全量 sha256 校验，
// 把「漏标」的影响限制在 N 次提交以内。首次提交（revKnown=false）也强制全量。
const verifyEveryCommits = 32

// commitSlowLog 是「慢提交」日志阈值：超过它就把一次提交的阶段分解打到
// stderr。失败提交无论耗时多少都会打（30s 超时必须能归因到具体阶段）。
const commitSlowLog = 3 * time.Second

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
	// commitsSinceVerify 自上次全量比对以来的提交次数（脏块快路径的安全网）。
	commitsSinceVerify int
	skipped            uint64 // 因缓冲满跳过的请求计数

	// committedMeta 缓存「最近一次成功提交」写进 record 的那三个标量。
	// 打开会话（open_session）只需要它们来填响应，原本却要 Load 整条
	// record（线上 24~90MB）再解出来，解析完把 payload 丢掉 —— 实测 650ms。
	// 这里在提交成功后顺手记下，导航时零磁盘 I/O 直接命中。
	// 由 mu 保护；metaValid=false 时调用方退回磁盘读取。
	committedMeta session.RecordMetadata
	metaValid     bool

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

// logCommit 打一行提交阶段分解（失败必打，慢提交按阈值打）。
//
// 一行里同时给出「谁慢」和「多大」：total 由 marshal/revision/sdk/lock/
// sidecar/encode/baseline/temp/bak/rename 组成，不足 1ms 的段省略。
func (c *checkpointCoordinator) logCommit(err error, rev uint64, total, marshalDur, revDur, stateDur time.Duration, ct session.CommitPhaseTiming) {
	phase := func(name string, d time.Duration) string {
		if d < time.Millisecond {
			return ""
		}
		return fmt.Sprintf(" %s=%s", name, d.Round(time.Millisecond))
	}
	status := "slow"
	if err != nil {
		status = "failed"
	}
	fmt.Fprintf(os.Stderr, "[checkpoint] %s commit total=%s session=%s rev=%d view=%.1fMB record=%.1fMB blocks=%d hashed=%.1fMB"+
		" phase:%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s err=%v\n",
		status, total.Round(time.Millisecond), c.sessID, rev,
		float64(ct.ViewBytes)/1e6, float64(ct.RecordBytes)/1e6, ct.BlocksWrote, float64(ct.HashedBytes)/1e6,
		phase("marshal", marshalDur), phase("revision", revDur), phase("sdk", stateDur),
		phase("lock", ct.LockWait), phase("sidecar", ct.Sidecar), phase("compact", ct.Compact),
		phase("sc-open", ct.SidecarOpen), phase("sc-ref", ct.SidecarRef), phase("sc-flush", ct.SidecarFlush),
		// sc-ref 的再分解（2026-11-12，P4 归因）：这一段有五件事，不拆开只能猜。
		phase("ref-cmp", ct.Ref.Compare), phase("ref-put", ct.Ref.Put), phase("ref-prune", ct.Ref.Prune),
		phase("ref-sum", ct.Ref.SetSum), phase("ref-doc", ct.Ref.Doc), phase("ref-repl", ct.Ref.Replace),
		phase("encode", ct.Encode), phase("baseline", ct.Baseline), phase("temp", ct.Temp),
		phase("bak+rename+dir", ct.Backup+ct.Rename+ct.SyncDir), err)
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
	// 先失效元数据缓存：删除后若仍命中缓存，open_session 会报「checkpoint 恢复成功」，
	// 而磁盘上已经没有 record 了（本会话已删）。
	c.mu.Lock()
	c.metaValid = false
	c.mu.Unlock()
	return c.store.DeleteSessionRecord(ctx, c.wsKey, c.sessID)
}

func (c *checkpointCoordinator) commitOnce() {
	// 全程持锁：async worker 单协程处理；syncMode 下多 emit 并发 Request 也串行化。
	c.mu.Lock()
	defer c.mu.Unlock()

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 用 MarshalWithSpans：除 view 字节外还拿到每个 block 的切片，供存储层
	// 做 sidecar 增量（免去对 86MB 的二次扫描，实测省 106ms/次提交）。
	marshalStart := time.Now()
	// S1：只要「骨架 + 各块字节」，不拼 210MB 整份 view（record 里的 blocks
	// 最终会被替换成引用，拼进去纯属浪费内存带宽 + 峰值内存）。
	spans, err := c.reducer.MarshalBodyWithSpans()
	marshalDur := time.Since(marshalStart)
	if err != nil {
		c.reportError(fmt.Errorf("checkpoint: marshal view: %w", err))
		return
	}
	view := spans.Body

	revStart := time.Now()
	rev, ok, rerr := c.nextRevisionLocked(ctx)
	revDur := time.Since(revStart)
	if !ok {
		if rerr != nil {
			c.reportError(fmt.Errorf("checkpoint: read revision: %w", rerr))
		} else {
			c.reportError(fmt.Errorf("checkpoint: read revision failed"))
		}
		return
	}
	stateStart := time.Now()
	state := sdkState(c.s)
	stateDur := time.Since(stateStart)
	record := session.NewSessionRecord(
		c.wsKey, c.sessID, rev, c.reducer.LastSeq(), c.reducer.ClearGeneration(), state, view,
	)
	record.BlockSpans = spans.Spans
	record.BlockIDs = spans.IDs
	record.AllBlockIDs = spans.AllIDs
	// 脏块快路径：只重写本轮真正变化的块。周期性（及失败后第一次）退回全量比对。
	c.commitsSinceVerify++
	record.DirtyBlocks = spans.Dirty
	if record.DirtyBlocks != nil && (c.commitsSinceVerify >= verifyEveryCommits || !c.revKnown) {
		record.DirtyBlocks = nil
		c.commitsSinceVerify = 0
	}
	// turns 分块（无损）：body 只带尾部窗口，历史按批追加到 <record>.turns。
	//
	// 顺序必须是「先日志、后 record」：反过来的话，一旦在这两步之间崩溃，
	// 这批轮次既不在日志里、也随着被覆盖的 record 永久消失。先写日志的坏情况
	// 只是日志里多出一批「record 还没引用」的轮次（下次提交照旧追加，幂等无害）。
	if recPath, perr := c.store.RecordPath(c.wsKey, c.sessID); perr == nil {
		if pending := c.reducer.TakePendingTurns(); len(pending) > 0 {
			rawTurns, merr := json.Marshal(pending)
			if merr != nil {
				c.reducer.RestorePendingTurns(pending)
				c.reportError(fmt.Errorf("checkpoint: marshal turns: %w", merr))
				return
			}
			if aerr := session.AppendTurnChunk(recPath, rawTurns, len(pending)); aerr != nil {
				// 日志没写成时要不要中止这次提交，取决于「不写日志会不会丢轮次」：
				//   · 总条数还在 body 窗口内 → 这批轮次本来就在 body 里，日志失败不致命；
				//   · 超出窗口 → body 只带尾部，不中止就会永久丢一批，必须中止并重试。
				if c.reducer.TurnsTotal() > turnsBodyWindow {
					c.reducer.RestorePendingTurns(pending)
					c.reportError(fmt.Errorf("checkpoint: append turns log: %w", aerr))
					return
				}
				c.reportError(fmt.Errorf("checkpoint: append turns log (ignored, all turns fit in body): %w", aerr))
			}
		}
	}
	ct, cerr := c.store.CommitRecordTraced(ctx, record)
	if cerr != nil {
		// 提交失败不阻塞事件流：失效 revision 缓存，下一边界重读磁盘基线；
		// 同时上报结构化错误（持久化失败必须可观察）。
		c.revKnown = false
		c.logCommit(cerr, rev, time.Since(started), marshalDur, revDur, stateDur, ct)
		c.reportError(fmt.Errorf("checkpoint: commit record: %w", cerr))
		return
	}
	if total := time.Since(started); total > commitSlowLog {
		c.logCommit(nil, rev, total, marshalDur, revDur, stateDur, ct)
	}
	c.lastRev = rev
	c.revKnown = true
	c.commits++
	// 记下刚落盘的元数据（与磁盘内容一致：rev/reducerSeq/clearGeneration 就是
	// 本次写进 record 的值）。导航路径据此跳过整条 record 的读取与解析。
	c.committedMeta = session.RecordMetadata{
		Revision:        rev,
		ReducerSeq:      record.ReducerSeq,
		ClearGeneration: record.ClearGeneration,
	}
	c.metaValid = true
}

// MetadataFromStore 在内存没有元数据时（进程刚启动/新会话）读磁盘头部。
// 只读三个标量，不 materialize payload。
func (c *checkpointCoordinator) MetadataFromStore(ctx context.Context) (session.RecordMetadata, error) {
	if c == nil || c.store == nil {
		return session.RecordMetadata{}, session.ErrRecordNotFound
	}
	meta, err := c.store.LoadRecordMetadata(ctx, c.wsKey, c.sessID)
	if err != nil {
		return session.RecordMetadata{}, err
	}
	// 回填缓存：本次读到的就是磁盘上的权威值，后续导航直接用内存。
	c.mu.Lock()
	c.committedMeta = *meta
	c.metaValid = true
	c.mu.Unlock()
	return *meta, nil
}

// Metadata 返回最近一次成功提交的 record 元数据。
// ok=false 表示本进程还没提交过（新会话尚无 record / 刚启动），
// 调用方应退回 LoadRecordMetadata 读磁盘。
func (c *checkpointCoordinator) Metadata() (session.RecordMetadata, bool) {
	if c == nil {
		return session.RecordMetadata{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.metaValid {
		return session.RecordMetadata{}, false
	}
	return c.committedMeta, true
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
			// record 读不出来（损坏）但**它存在**：revision 基线仍可从头部元数据拿到。
			// 自愈提交必须能继续 —— 否则修好的视图永远落不了盘，每次打开都重放一遍。
			if meta, merr := c.store.LoadRecordMetadata(ctx, c.wsKey, c.sessID); merr == nil {
				return meta.Revision + 1, true, nil
			}
			return 0, false, err
		}
		rec = nil
	}
	if rec != nil {
		rev = rec.Revision
	}
	return rev + 1, true, nil
}

// isRecordNotFound 只认真正的「没有 record」。
//
// 历史实现是子串判定（strings.Contains(err, "not found")），而 record 损坏时的错误
// 文本是「formal=<损坏> backup=session record not found」—— **备份缺失的措辞**让它
// 被判成「没有 record」，于是 revision 基线从 0 起算、自愈提交撞上 revision 冲突
// （2026-10-21 复盘时实测踩到：修好的视图永远落不了盘，每次打开都要重放一遍）。
func isRecordNotFound(err error) bool {
	return errors.Is(err, session.ErrRecordNotFound) || os.IsNotExist(err)
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

// todoResponseCarriesSnapshot 这条工具响应是否携带待办快照（extras 注入与 reducer
// meta 共用的**唯一**判据）。
//
// 判据是「事件带快照」，不是「工具名以 todo_ 开头」：编排型工具（codemode）的响应
// 同样会带快照 —— 脚本里的 todo_* 是嵌套调用、按契约不发射工具事件，快照只能由外层
// 响应代发（见 tools/engine.go finish 的 todoWrites）。只认名字的话，脚本改过的清单
// 永远到不了宿主：待办面板停在脚本执行之前（用户 2026-11 报障）。
//
// 名字判据保留给**没有** Todos 字段的旧事件日志（重放）：那时仍回退读会话 store，
// 与从前行为一致（见 todosOf）。
func todoResponseCarriesSnapshot(ev *events.ToolResponse) bool {
	return ev.Todos != nil || strings.HasPrefix(ev.Name, "todo_")
}

// todosOf 取一条事件携带的待办快照（**属主 = 发出该事件的运行**）。
//
// 事件自带快照时以它为准：子 agent 的 todo_* 带的是它自己那份独立清单，而
// bs.s.TodoItems() 永远是**会话**清单 —— 一律读会话曾让子 agent 的 TODO 覆盖成
// 主会话的（用户 2026-10 报障）。事件缺快照（旧日志重放 / 未接线 todo 的运行）
// 才回退会话清单，行为与从前一致。
func todosOf(bs *bridgeSession, snap []events.TodoSnapshot) []TodoItem {
	if snap == nil {
		// 没带快照（旧事件日志重放 / 未接线 todo 的运行）→ 回退会话清单（旧行为）。
		// 注意与「带了一个空清单」区分：后者是事实（清单被清空），必须照实生效。
		if bs == nil || bs.s == nil {
			return nil
		}
		return todoItems(bs.s.TodoItems())
	}
	out := make([]TodoItem, 0, len(snap))
	for _, it := range snap {
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
		if todoResponseCarriesSnapshot(ev) {
			meta.Todos = todosOf(bs, ev.Todos)
		}
	case *events.CompressEnd:
		meta.Todos = todosOf(bs, ev.Todos)
	}
	_ = bs.reducer.Apply(e, meta)

	if commitBoundary(e) {
		// record 损坏且 reducer 仍是空的（还没重放自愈）→ **不许提交**：
		// 那会把一份 0 块/残缺的视图写成新的权威 record，覆盖掉还能从事件日志
		// 完整重建的历史（2026-10-21 事故的第二步）。等打开路径重放完再恢复提交。
		if bs.coord != nil && !(bs.recordCorrupt && bs.reducer.BlockCount() == 0) {
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
	// reducer 已有内容 → 绝不能重放（事件会在内存里被应用两遍，块与用量翻倍）。
	// 正常入口只在「新建/损坏」时调用，那时 reducer 是空的；这里防御异常调用顺序。
	if bs.reducer.BlockCount() > 0 {
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
	// 重放成功 = reducer 重新装上了完整历史 → 解除「损坏」状态，恢复常规提交。
	bs.recordCorrupt = false
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

// parseCheckpointFromRecord 从读回的 record 还原快照（恢复路径主入口）。
//
// 慢路径（旧格式 / 无 spans）：整份 json.Unmarshal（210MB，实测 1.1~2.2s）。
// 快路径（sidecar 引用形态）：骨架解码（2~5MB）+ 各块字节并行解码。
//
// 两条路径必须产出一致的 *Checkpoint：骨架与 spans 来自同一次
// readRecordCandidate，spans 与展开后的 blocks 数组元素一一对应（顺序相同）。
func parseCheckpointFromRecord(rec *session.SessionRecord) (*Checkpoint, error) {
	if rec == nil {
		return nil, os.ErrNotExist
	}
	spans := rec.LoadedBlockSpans
	if len(spans) == 0 {
		if len(rec.ViewCheckpoint) == 0 {
			return nil, os.ErrNotExist
		}
		return parseCheckpoint(rec.ViewCheckpoint) // 旧格式：blocks 内嵌在 view 里
	}
	if len(rec.BodyView) == 0 {
		// 有 spans 但没骨架（不该发生）：退回整份 view，正确性优先。
		return parseCheckpoint(rec.ViewCheckpoint)
	}
	skel, err := rec.SkeletonView()
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(skel, &cp); err != nil {
		return nil, err
	}
	if cp.SchemaVersion != checkpointSchemaVersion {
		return nil, os.ErrInvalid
	}
	blocks := make([]Block, len(spans))
	var hashes [][8]byte
	if len(rec.LoadedBlockHashes) == len(spans) {
		hashes = rec.LoadedBlockHashes // 映射路径：完整性校验在解码时逐块做
	}
	if err := decodeBlocksParallel(spans, hashes, blocks); err != nil {
		return nil, err
	}
	cp.Blocks = blocks
	return &cp, nil
}

// decodeBlocksParallel 把块字节分给多个 worker 并行 json.Unmarshal。
//
// 用原子取号而不是静态分片：块大小差异极大（一行文本 vs 整段工具输出），
// 静态切分会让某个 worker 拖尾。实测 30.8k 块：1 worker 850ms、8 worker 234ms。
func decodeBlocksParallel(spans [][]byte, hashes [][8]byte, out []Block) error {
	if len(spans) != len(out) {
		return fmt.Errorf("block spans %d != out %d", len(spans), len(out))
	}
	if hashes != nil && len(hashes) != len(spans) {
		return fmt.Errorf("block hashes %d != spans %d", len(hashes), len(spans))
	}
	verify := func(i int) error {
		if hashes == nil {
			return nil
		}
		if d := session.PayloadDigest(spans[i]); d != hashes[i] {
			return fmt.Errorf("block %d: payload checksum mismatch", i)
		}
		return nil
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8 // 实测 8 worker 已到拐点，再多收益递减（内存带宽受限）
	}
	if workers <= 1 || len(spans) < 64 {
		for i := range spans {
			if err := verify(i); err != nil {
				return err
			}
			if err := json.Unmarshal(spans[i], &out[i]); err != nil {
				return fmt.Errorf("block %d: %w", i, err)
			}
		}
		return nil
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(spans) {
					return
				}
				if err := verify(i); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				if err := json.Unmarshal(spans[i], &out[i]); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("block %d: %w", i, err)
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// parseCheckpointFromRecordWindow 只解码尾部 limit 块 + 所有卡片类块。
//
// 为什么要带卡片类块：窗口外的子智能体/任务卡片必须照常出现在首屏（左栏树），
// 而它们可能早于窗口。有了索引里的 kind，就能只解这几十个，不必全解 209MB。
//
// 其余块进 PagedBlocks（指向 sidecar 映射，零拷贝），由持有者决定何时 Release。
// limit <= 0 或覆盖全部块时，退回完整解析（行为与之前一致）。
func parseCheckpointFromRecordWindow(rec *session.SessionRecord, limit int) (*Checkpoint, error) {
	spans := rec.LoadedBlockSpans
	if len(spans) == 0 || limit <= 0 || limit >= len(spans) || len(rec.LoadedBlockKinds) != len(spans) {
		return parseCheckpointFromRecord(rec)
	}
	skel, err := rec.SkeletonView()
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(skel, &cp); err != nil {
		return nil, err
	}
	if cp.SchemaVersion != checkpointSchemaVersion {
		return nil, os.ErrInvalid
	}

	n := len(spans)
	start := n - limit
	need := make([]int, 0, limit+16)
	decoded := make(map[int]struct{}, limit+16)
	for i := 0; i < start; i++ {
		if session.HasCardKind(rec.LoadedBlockKinds[i]) {
			need = append(need, i)
			decoded[i] = struct{}{}
		}
	}
	for i := start; i < n; i++ {
		need = append(need, i)
		decoded[i] = struct{}{}
	}

	selSpans := make([][]byte, len(need))
	selHashes := make([][8]byte, len(need))
	hasHashes := len(rec.LoadedBlockHashes) == n
	for j, i := range need {
		selSpans[j] = spans[i]
		if hasHashes {
			selHashes[j] = rec.LoadedBlockHashes[i]
		}
	}
	blocks := make([]Block, len(need))
	var hsh [][8]byte
	if hasHashes {
		hsh = selHashes
	}
	if err := decodeBlocksParallel(selSpans, hsh, blocks); err != nil {
		return nil, err
	}

	pagedIDs := make([]uint64, 0, start)
	pagedSpans := make([][]byte, 0, start)
	pagedHashes := make([][8]byte, 0, start)
	for i := 0; i < start; i++ {
		if _, ok := decoded[i]; ok {
			continue
		}
		pagedIDs = append(pagedIDs, rec.LoadedBlockIDs[i])
		pagedSpans = append(pagedSpans, spans[i])
		if hasHashes {
			pagedHashes = append(pagedHashes, rec.LoadedBlockHashes[i])
		}
	}

	cp.Blocks = blocks
	cp.BlocksTotal = n
	cp.PagedBlocks = &PagedBlocks{
		IDs:     pagedIDs,
		Spans:   pagedSpans,
		Hashes:  pagedHashes,
		Release: rec.ReleaseView,
	}
	return &cp, nil
}
