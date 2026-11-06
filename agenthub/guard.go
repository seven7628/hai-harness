package agenthub

// guard.go：预算/并发/去重/链长护栏（§6.9，P2-07）。
//
// 口径：
//   - 零值预算 = 不限额（profile 文档约定），但并发/去重/链长有安全默认值；
//   - 只累加 CostPriced=true 的成本（未知成本不能被当成 0 花掉）；
//   - 日预算按 UTC 日、任务 UpdatedAt 归日；群预算按 Origin.GroupID；
//   - 去重窗口内同 (origin agent, target, instruction) 视为重复派发；
//   - 根链步数 = Chain 长度，默认 6（防止 A→B→C 无限接力）。
//
// 这些检查在**建账前**跑（Queue.Submit → Check），失败不落任务；单任务成本只有在
// 结算时才知道，由 bridge 在 AgentEnd 结算处判（超限落 blocked_budget + hub_alert）。

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrBudgetExceeded = errors.New("hub guard: budget exceeded")
	ErrTooConcurrent  = errors.New("hub guard: too many concurrent tasks")
	ErrDuplicateTask  = errors.New("hub guard: duplicate task within window")
	ErrChainTooDeep   = errors.New("hub guard: task chain too deep")
)

// P2.5-04 产品口径（D1）：新智能体/新群默认日预算与全局日预算。
// 0 仍表示显式不限额；创建入口只在未填时补默认值，已有 profile 不强制迁移。
const (
	DefaultAgentDailyBudgetUSD  = 20
	DefaultGroupDailyBudgetUSD  = 50
	DefaultGlobalDailyBudgetUSD = 200
)

// GuardLimits 一次派发的护栏配置（来自 profile.policy / group.policy / hub.json defaults）。
type GuardLimits struct {
	MaxTaskCostUSD       float64
	DailyBudgetUSD       float64
	GroupDailyBudgetUSD  float64
	GlobalDailyBudgetUSD float64
	// MaxConcurrent 同一发起智能体的在途任务上限；0 = 默认 3。
	MaxConcurrent int
	// DuplicateWindow 同指令去重窗口；0 = 默认 10 分钟。
	DuplicateWindow time.Duration
	// MaxChainSteps 根任务链最大步数；0 = 默认 6。
	MaxChainSteps int
}

func (l GuardLimits) withDefaults() GuardLimits {
	if l.MaxConcurrent <= 0 {
		l.MaxConcurrent = 3
	}
	if l.DuplicateWindow <= 0 {
		l.DuplicateWindow = 10 * time.Minute
	}
	if l.MaxChainSteps <= 0 {
		l.MaxChainSteps = 6
	}
	return l
}

// Guard 是无状态护栏；Now 可注入（测试）。
type Guard struct {
	Limits GuardLimits
	Now    func() time.Time
	// GroupDailyBudget 按群取日预算：初始派发时 GuardLimits 里没有群上下文，
	// 由调用方注入从 Store 读 Group.Policy 的钩子。限制已显式设置时不调用。
	// 读取失败必须返回错误（不能把「读不到预算」当成「不限额」）。
	GroupDailyBudget func(context.Context, GroupID) (float64, error)
}

// Check 在建账前判定；返回包装后的哨兵错误，调用方用 errors.Is 分类。
func (g *Guard) Check(ctx context.Context, store Store, t *Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store == nil || t == nil {
		return fmt.Errorf("%w: guard 参数为空", ErrInvalid)
	}
	lim := g.Limits.withDefaults()
	if t.Origin.GroupID != "" && lim.GroupDailyBudgetUSD == 0 && g.GroupDailyBudget != nil {
		groupLimit, err := g.GroupDailyBudget(ctx, t.Origin.GroupID)
		if err != nil {
			return fmt.Errorf("hub guard: 读取群 %s 日预算失败: %w", t.Origin.GroupID, err)
		}
		lim.GroupDailyBudgetUSD = groupLimit
	}
	now := time.Now().UTC()
	if g.Now != nil {
		now = g.Now().UTC()
	}
	if steps := t.ChainSteps(); steps > lim.MaxChainSteps {
		return fmt.Errorf("%w: 链长 %d > %d（需要你决定是否继续）", ErrChainTooDeep, steps, lim.MaxChainSteps)
	}
	// P2.5-02：Store 有内存索引时不读全盘；没有索引的替身继续走 ListTasks。
	if idx, ok := store.(TaskIndex); ok && idx.TaskIndexReady() {
		return g.checkIndexed(idx, t, lim, now)
	}
	all, err := store.ListTasks(ctx, TaskFilter{})
	if err != nil {
		return err
	}
	active := 0
	var agentDay, groupDay, globalDay float64
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for _, old := range all {
		if old == nil {
			continue
		}
		if !old.Status.Terminal() {
			if old.Origin.AgentID == t.Origin.AgentID {
				active++
			}
		}
		if old.Result != nil && old.Result.Usage.CostPriced && !old.UpdatedAt.Before(startOfDay) {
			cost := old.Result.Usage.CostUSD
			globalDay += cost
			if old.Origin.AgentID == t.Origin.AgentID {
				agentDay += cost
			}
			if t.Origin.GroupID != "" && old.Origin.GroupID == t.Origin.GroupID {
				groupDay += cost
			}
		}
		// 同一 client_msg_id 的重试是显式重投（已由 Queue 查重分流），不算重复派发。
		if t.ClientMsgID != "" && old.ClientMsgID == t.ClientMsgID {
			continue
		}
		if old.Origin.AgentID == t.Origin.AgentID && sameTarget(old.Target, t.Target) &&
			old.Instruction == t.Instruction && !old.CreatedAt.Before(now.Add(-lim.DuplicateWindow)) {
			return fmt.Errorf("%w: 同一任务在 %s 内已派发（task %s）", ErrDuplicateTask, lim.DuplicateWindow, old.ID)
		}
	}
	if active >= lim.MaxConcurrent {
		return fmt.Errorf("%w: 发起者已有 %d 个在途任务（上限 %d）", ErrTooConcurrent, active, lim.MaxConcurrent)
	}
	if lim.GlobalDailyBudgetUSD > 0 && globalDay >= lim.GlobalDailyBudgetUSD {
		return fmt.Errorf("%w: 全局日预算 %.2f 已用 %.2f", ErrBudgetExceeded, lim.GlobalDailyBudgetUSD, globalDay)
	}
	if lim.GroupDailyBudgetUSD > 0 && t.Origin.GroupID != "" && groupDay >= lim.GroupDailyBudgetUSD {
		return fmt.Errorf("%w: 群日预算 %.2f 已用 %.2f", ErrBudgetExceeded, lim.GroupDailyBudgetUSD, groupDay)
	}
	if lim.DailyBudgetUSD > 0 && agentDay >= lim.DailyBudgetUSD {
		return fmt.Errorf("%w: 智能体日预算 %.2f 已用 %.2f", ErrBudgetExceeded, lim.DailyBudgetUSD, agentDay)
	}
	return nil
}

// checkIndexed 是 Check 的索引快路径；错误口径与全盘扫描分支保持一致。
func (g *Guard) checkIndexed(idx TaskIndex, t *Task, lim GuardLimits, now time.Time) error {
	if id, ok := idx.FindDuplicate(t, now.Add(-lim.DuplicateWindow)); ok {
		return fmt.Errorf("%w: 同一任务在 %s 内已派发（task %s）", ErrDuplicateTask, lim.DuplicateWindow, id)
	}
	if active := idx.ActiveCount(t.Origin.AgentID); active >= lim.MaxConcurrent {
		return fmt.Errorf("%w: 发起者已有 %d 个在途任务（上限 %d）", ErrTooConcurrent, active, lim.MaxConcurrent)
	}
	agentDay, groupDay, globalDay := idx.DailyCost(t.Origin.AgentID, t.Origin.GroupID, now)
	if lim.GlobalDailyBudgetUSD > 0 && globalDay >= lim.GlobalDailyBudgetUSD {
		return fmt.Errorf("%w: 全局日预算 %.2f 已用 %.2f", ErrBudgetExceeded, lim.GlobalDailyBudgetUSD, globalDay)
	}
	if lim.GroupDailyBudgetUSD > 0 && t.Origin.GroupID != "" && groupDay >= lim.GroupDailyBudgetUSD {
		return fmt.Errorf("%w: 群日预算 %.2f 已用 %.2f", ErrBudgetExceeded, lim.GroupDailyBudgetUSD, groupDay)
	}
	if lim.DailyBudgetUSD > 0 && agentDay >= lim.DailyBudgetUSD {
		return fmt.Errorf("%w: 智能体日预算 %.2f 已用 %.2f", ErrBudgetExceeded, lim.DailyBudgetUSD, agentDay)
	}
	return nil
}

func sameTarget(a, b TaskTarget) bool {
	return a.Kind == b.Kind && a.Workspace == b.Workspace && a.SessionID == b.SessionID && a.AgentID == b.AgentID && a.GroupID == b.GroupID
}

// OverTaskBudget 结算时判定单任务成本是否超限（CostPriced=false 不判：未知不等于 0）。
// 与日预算/Guard.Check 一致用 >=：预算用尽即到顶，不留下「刚好等于上限还能再跑」的缝。
func (l GuardLimits) OverTaskBudget(u Usage) (bool, string) {
	if l.MaxTaskCostUSD <= 0 || !u.CostPriced {
		return false, ""
	}
	if u.CostUSD >= l.MaxTaskCostUSD {
		return true, fmt.Sprintf("单任务成本 $%.4f 达到或超过上限 $%.2f", u.CostUSD, l.MaxTaskCostUSD)
	}
	return false, ""
}
