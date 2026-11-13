package agenthub

// task_index.go：任务账本的内存索引（P2.5-02）。
//
// 为什么需要：Queue.Submit 每次都要 FindByClientMsgID + Guard.Check，而两者原先
// 都走 Store.ListTasks —— 每次提交把 tasks/*/task.json 全读一遍。P3b 群会话把
// 提交频率放大后，这会变成磁盘 IO 与全局锁的瓶颈。
//
// 索引由 Store 维护（FileStore 在 Open 时重建、SaveTask 增量更新；MemoryStore
// 同步维护），Ledger/Guard 通过 TaskIndex 可选接口查询；未实现该接口的 Store
// 仍然回退 ListTasks，不改变第三方测试替身的行为。
//
// 口径必须与 Ledger/Guard 原实现一致：
//   - client_msg_id 只在同一 (kind, clean(workspace), trim(session)) 内命中；
//   - active = 非终态任务数，按 Origin.AgentID；
//   - pair = 非终态 target=agent 任务数，按 (Origin.GroupID, Origin.AgentID, Target.AgentID)；
//   - 日成本只累加 Result.Usage.CostPriced=true，按 UpdatedAt 的 UTC 日归日；
//   - 去重窗口内同 (origin, target, instruction) 命中；同 client_msg_id 的重试跳过。

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TaskIndex 是 Store 可选实现的内存查询接口。查询必须是无 IO 的。
type TaskIndex interface {
	// TaskIndexReady 为 false 时调用方必须回退 ListTasks（零值 Store 没有索引，
	// 不能把“查不到”当成“没有”，否则护栏会静默 fail-open）。
	TaskIndexReady() bool
	FindByClientMsgID(target TaskTarget, clientMsgID string) (*Task, bool)
	ActiveCount(agent AgentID) int
	DailyCost(agent AgentID, group GroupID, day time.Time) (agentCost, groupCost, globalCost float64)
	FindDuplicate(t *Task, since time.Time) (TaskID, bool)
}

// PairActiveIndex 是 TaskIndex 的可选扩展：按「起始群 + 起始智能体 → 目标智能体」
// 统计非终态任务数（assign_task 的同对并发上限）。
//
// 刻意不并进 TaskIndex：第三方测试替身只实现 TaskIndex 也能继续编译；未实现本接口
// 时 Queue 回退 ListTasks（与 Guard 的兼容策略一致）。
type PairActiveIndex interface {
	TaskIndexReady() bool
	PairActiveCount(group GroupID, from, to AgentID) int
}

type clientTaskKey struct {
	Kind        string
	Workspace   string
	SessionID   string
	ClientMsgID string
}

type dedupeTaskKey struct {
	AgentID     AgentID
	Kind        string
	Workspace   string
	SessionID   string
	TargetAgent AgentID
	GroupID     GroupID
	Instruction string
}

// pairActiveKey 是「同一起始群 + 起始智能体 → 同一目标智能体」的在途计数键
// （assign_task 的同对并发上限，见 Queue.SubmitAgent）。
type pairActiveKey struct {
	Group GroupID
	From  AgentID
	To    AgentID
}

type agentDayKey struct {
	Day   string
	Agent AgentID
}

type groupDayKey struct {
	Day   string
	Group GroupID
}

type taskIndex struct {
	mu       sync.RWMutex
	byID     map[TaskID]*Task
	byClient map[clientTaskKey][]TaskID
	active   map[AgentID]int
	pair     map[pairActiveKey]int

	agentDay  map[agentDayKey]float64
	groupDay  map[groupDayKey]float64
	globalDay map[string]float64

	byDedupe map[dedupeTaskKey][]TaskID
}

func newTaskIndex() *taskIndex {
	x := &taskIndex{}
	x.reset(nil)
	return x
}

// reset 用一组任务重建索引（OpenFileStore 启动时调用）。
func (x *taskIndex) reset(tasks []*Task) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.byID = map[TaskID]*Task{}
	x.byClient = map[clientTaskKey][]TaskID{}
	x.active = map[AgentID]int{}
	x.pair = map[pairActiveKey]int{}
	x.agentDay = map[agentDayKey]float64{}
	x.groupDay = map[groupDayKey]float64{}
	x.globalDay = map[string]float64{}
	x.byDedupe = map[dedupeTaskKey][]TaskID{}
	for _, t := range tasks {
		x.putLocked(t)
	}
}

// put 增量更新一条任务；旧值的存在性从 byID 取，调用方不需要保留旧快照。
func (x *taskIndex) put(t *Task) {
	if x == nil || t == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.putLocked(t)
}

func (x *taskIndex) putLocked(t *Task) {
	if t == nil {
		return
	}
	old := x.byID[t.ID]
	if old != nil {
		x.removeLocked(old)
	}
	cp := cloneTask(t)
	x.byID[cp.ID] = cp
	x.addLocked(cp)
	x.updateClientLocked(old, cp)
	x.updateDedupeLocked(old, cp)
}

func (x *taskIndex) removeLocked(t *Task) {
	if t == nil {
		return
	}
	if !t.Status.Terminal() {
		x.decActiveLocked(t)
	}
	x.addCostLocked(t, -1)
}

func (x *taskIndex) addLocked(t *Task) {
	if t == nil {
		return
	}
	if !t.Status.Terminal() {
		x.active[t.Origin.AgentID]++
		if key, ok := pairActiveKeyOf(t); ok {
			x.pair[key]++
		}
	}
	x.addCostLocked(t, 1)
}

// decActiveLocked 与 addLocked 严格对称：active 与 pair 的加减必须同一分支，
// 否则同对并发上限会随快照更新（put → remove+add）缓慢漂移。
func (x *taskIndex) decActiveLocked(t *Task) {
	n := x.active[t.Origin.AgentID] - 1
	if n <= 0 {
		delete(x.active, t.Origin.AgentID)
	} else {
		x.active[t.Origin.AgentID] = n
	}
	key, ok := pairActiveKeyOf(t)
	if !ok {
		return
	}
	if n := x.pair[key] - 1; n <= 0 {
		delete(x.pair, key)
	} else {
		x.pair[key] = n
	}
}

func (x *taskIndex) addCostLocked(t *Task, sign float64) {
	if t == nil || t.Result == nil || !t.Result.Usage.CostPriced {
		return
	}
	day := taskDayKey(t.UpdatedAt)
	if day == "" {
		return
	}
	cost := sign * t.Result.Usage.CostUSD
	x.agentDay[agentDayKey{Day: day, Agent: t.Origin.AgentID}] += cost
	if t.Origin.GroupID != "" {
		x.groupDay[groupDayKey{Day: day, Group: t.Origin.GroupID}] += cost
	}
	x.globalDay[day] += cost
}

func (x *taskIndex) updateClientLocked(old, cur *Task) {
	oldKey, oldOK := clientKeyOf(old)
	newKey, newOK := clientKeyOf(cur)
	if oldOK && (!newOK || oldKey != newKey) {
		// 只摘掉这一条；同一 client key 可能还有别的 attempt（合法重试）。
		// 不能因为 holder 改键就整条删除 —— 否则 FindByClientMsgID 会对整条
		// key 失忆，退化成双派发（P2.5-02 复审 B1）。
		ids := removeTaskID(x.byClient[oldKey], old.ID)
		if len(ids) == 0 {
			delete(x.byClient, oldKey)
		} else {
			x.byClient[oldKey] = ids
		}
	}
	if !newOK {
		return
	}
	ids := x.byClient[newKey]
	if !containsTaskID(ids, cur.ID) {
		x.byClient[newKey] = append(ids, cur.ID)
	}
}

func (x *taskIndex) updateDedupeLocked(old, cur *Task) {
	oldKey, oldOK := dedupeKeyOf(old)
	newKey, newOK := dedupeKeyOf(cur)
	if oldOK && (!newOK || oldKey != newKey) {
		x.byDedupe[oldKey] = removeTaskID(x.byDedupe[oldKey], old.ID)
		if len(x.byDedupe[oldKey]) == 0 {
			delete(x.byDedupe, oldKey)
		}
	}
	if !newOK {
		return
	}
	ids := x.byDedupe[newKey]
	if !containsTaskID(ids, cur.ID) {
		x.byDedupe[newKey] = append(ids, cur.ID)
	}
}

func (x *taskIndex) FindByClientMsgID(target TaskTarget, clientMsgID string) (*Task, bool) {
	if x == nil || clientMsgID == "" {
		return nil, false
	}
	key := clientTaskKey{
		Kind:        target.Kind,
		Workspace:   filepath.Clean(strings.TrimSpace(target.Workspace)),
		SessionID:   strings.TrimSpace(target.SessionID),
		ClientMsgID: clientMsgID,
	}
	x.mu.RLock()
	ids := x.byClient[key]
	var best *Task
	for _, id := range ids {
		t := x.byID[id]
		if t == nil {
			continue
		}
		if best == nil || newerTask(t, best) {
			best = t
		}
	}
	x.mu.RUnlock()
	if best == nil {
		return nil, false
	}
	return cloneTask(best), true
}

func (x *taskIndex) ActiveCount(agent AgentID) int {
	if x == nil {
		return 0
	}
	x.mu.RLock()
	n := x.active[agent]
	x.mu.RUnlock()
	return n
}

// PairActiveCount 返回 (origin group, origin agent → target agent) 的非终态任务数。
func (x *taskIndex) PairActiveCount(group GroupID, from, to AgentID) int {
	if x == nil {
		return 0
	}
	x.mu.RLock()
	n := x.pair[pairActiveKey{Group: group, From: from, To: to}]
	x.mu.RUnlock()
	return n
}

func pairActiveKeyOf(t *Task) (pairActiveKey, bool) {
	if t == nil || t.Target.Kind != TargetAgent || t.Target.AgentID == "" {
		return pairActiveKey{}, false
	}
	return pairActiveKey{Group: t.Origin.GroupID, From: t.Origin.AgentID, To: t.Target.AgentID}, true
}

func (x *taskIndex) DailyCost(agent AgentID, group GroupID, day time.Time) (agentCost, groupCost, globalCost float64) {
	if x == nil {
		return 0, 0, 0
	}
	d := taskDayKey(day)
	if d == "" {
		return 0, 0, 0
	}
	x.mu.RLock()
	agentCost = x.agentDay[agentDayKey{Day: d, Agent: agent}]
	if group != "" {
		groupCost = x.groupDay[groupDayKey{Day: d, Group: group}]
	}
	globalCost = x.globalDay[d]
	x.mu.RUnlock()
	agentCost = nonNegativeCost(agentCost)
	groupCost = nonNegativeCost(groupCost)
	globalCost = nonNegativeCost(globalCost)
	return agentCost, groupCost, globalCost
}

// FindDuplicate 返回窗口内最匹配的一条任务；同 client_msg_id 的重试按原
// Guard 口径跳过（那是显式重投，不算重复派发）。
func (x *taskIndex) FindDuplicate(t *Task, since time.Time) (TaskID, bool) {
	key, ok := dedupeKeyOf(t)
	if x == nil || !ok {
		return "", false
	}
	x.mu.RLock()
	ids := x.byDedupe[key]
	var best *Task
	for _, id := range ids {
		old := x.byID[id]
		if old == nil || old.CreatedAt.Before(since) {
			continue
		}
		if t.ClientMsgID != "" && old.ClientMsgID == t.ClientMsgID {
			continue
		}
		if best == nil || newerTask(old, best) {
			best = old
		}
	}
	x.mu.RUnlock()
	if best == nil {
		return "", false
	}
	return best.ID, true
}

func clientKeyOf(t *Task) (clientTaskKey, bool) {
	if t == nil || t.ClientMsgID == "" {
		return clientTaskKey{}, false
	}
	return clientTaskKey{
		Kind:        t.Target.Kind,
		Workspace:   filepath.Clean(strings.TrimSpace(t.Target.Workspace)),
		SessionID:   strings.TrimSpace(t.Target.SessionID),
		ClientMsgID: t.ClientMsgID,
	}, true
}

func dedupeKeyOf(t *Task) (dedupeTaskKey, bool) {
	if t == nil {
		return dedupeTaskKey{}, false
	}
	return dedupeTaskKey{
		AgentID:     t.Origin.AgentID,
		Kind:        t.Target.Kind,
		Workspace:   t.Target.Workspace,
		SessionID:   t.Target.SessionID,
		TargetAgent: t.Target.AgentID,
		GroupID:     t.Target.GroupID,
		Instruction: t.Instruction,
	}, true
}

func newerTask(a, b *Task) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID > b.ID
	}
	return a.CreatedAt.After(b.CreatedAt)
}

func taskDayKey(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

func nonNegativeCost(v float64) float64 {
	if v < 0 && v > -1e-9 {
		return 0
	}
	return v
}

func containsTaskID(ids []TaskID, id TaskID) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

func removeTaskID(ids []TaskID, id TaskID) []TaskID {
	out := ids[:0]
	for _, got := range ids {
		if got != id {
			out = append(out, got)
		}
	}
	return out
}
