package subagent

import (
	"context"
	"sync"
)

// DefaultConcurrency 子 agent 并发槽默认容量。
//
// 2026-09-20：原硬编码 4（Codex 客户端 6 / 服务端 3 区间取保守值）太小 —— 槽获取是
// **工具调用栈上的同步阻塞**（registry.start），槽满时 agent_spawn 不返回，整个工具批
// 挂住主 Agent，而任何超时都解不开它（ToolTimeout 显式 -1 + 批超时豁免 + 自动摘离默认关）：
// 实测 4 个长任务在跑 → 后续每次 spawn 死等 → 主会话卡 51 分钟，直到某个长任务结束。
//
// 默认值抬到 1000：上限退化为「防失控护栏」，真正失控交给成本预算 / 轮数上限兜。
// 宿主可按需调小（Registry.SetConcurrency / SetAuxConcurrency，运行期即生效）。
const DefaultConcurrency = 1000

// SlotPool 并发槽池：容量可在运行期调整（SetCapacity），获取 ctx 感知。
//
// 为什么不直接用一个 chan struct{} 当信号量：
//  1. 容量可配 —— Go 通道容量不可变，改容量必须换通道；而「在途任务持有 N 个槽」
//     要跟着**它自己获取的那个通道**走。这里让 Acquire 返回的释放闭包捕获所属通道：
//     换池后旧任务归还旧池、新任务用新池，计数各自守恒（缩小容量的瞬间在途总量可能
//     短暂超过新容量，随旧任务收尾收敛；不追求瞬时精确）。
//  2. 扩容即时生效 —— 已排队的等待者不能死等在旧池上（那会让「调大上限」变成
//     必须等在途任务收尾才生效）。每个等待者同时监听 changed 信号：容量一变就重取
//     当前池，于是调大上限能立刻放行排在最前面的调用（正是「主 Agent 被槽卡住」时
//     用户最需要的那条出路）。
//  3. 分池 —— 不同工具族需要各自独立的上限（agent_spawn 与只读的 subagent_explore
//     各一池：explore 占满不再饿死 spawn，见 Registry.auxSlots）。
type SlotPool struct {
	mu      sync.Mutex
	ch      chan struct{}
	n       int           // 当前容量（通道本身是权威，此处供诊断/断言读取）
	changed chan struct{} // 每次换池关闭并换新：排队者据此重取当前池
}

// NewSlotPool 创建容量为 capacity 的槽池（<=0 → DefaultConcurrency）。
func NewSlotPool(capacity int) *SlotPool {
	if capacity <= 0 {
		capacity = DefaultConcurrency
	}
	return &SlotPool{ch: make(chan struct{}, capacity), n: capacity, changed: make(chan struct{})}
}

// Acquire 获取一个槽：返回释放函数（幂等，defer 调用即可）。
// ctx 取消（会话关闭 / run abort）时返回 ctx.Err()，不占用槽。
func (p *SlotPool) Acquire(ctx context.Context) (func(), error) {
	for {
		p.mu.Lock()
		ch, changed := p.ch, p.changed
		p.mu.Unlock()
		select {
		case ch <- struct{}{}:
			var once sync.Once
			return func() { once.Do(func() { <-ch }) }, nil
		case <-changed:
			continue // 容量变了：重取当前池（扩容 → 立即放行；缩容 → 按新池继续排）
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// SetCapacity 调整容量（<=0 → DefaultConcurrency；同值幂等）。
// 语义 = 对「此后新派发 + 当前排队者」的即时上限：在途任务继续持有旧池的槽并归还旧池；
// 排队者被唤醒重取当前池（调大立刻放行，调小则回到新池继续排）。
func (p *SlotPool) SetCapacity(n int) {
	if n <= 0 {
		n = DefaultConcurrency
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n == p.n {
		return
	}
	p.ch = make(chan struct{}, n)
	p.n = n
	close(p.changed) // 唤醒排队者重取当前池
	p.changed = make(chan struct{})
}

// Capacity 当前容量。
func (p *SlotPool) Capacity() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

// InFlight 当前占用的槽数（诊断/测试断言用；换池后旧池在途不计入，权威上限看 Capacity）。
func (p *SlotPool) InFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ch)
}
