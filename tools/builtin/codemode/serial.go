package codemode

// serial.go：写类调用的**保序**门（对抗复核 F3）。
//
// 问题：脚本里 `const a = tools.write_file(...); const b = tools.edit_file(...)` 两条
// 写类调用各由传输层一个 goroutine 处理，ExecScope.Acquire 只保证**互斥**、不保证
// **脚本次序** —— 实测 20 次里 13 次反序执行（先写后改变成先改后写，静默改错结果），
// 而教学正文鼓励 Promise.allSettled。根因是「帧一到就各起一个 goroutine」，故谁先到
// OnCall 取决于调度。
//
// 修法：用传输层在读帧处发的**帧序号**（`ScriptCall.Index`，见 execproc/script.go 的
// dispatch）当票号，让写类调用等到「所有更早的票号都**到场且跑完**」再执行。
//
// 为什么这个等待是确定的、不是启发式：票号在**读帧协程里连续发号**（1,2,3,…），所以
// 「我的票号是 k」本身就证明了「1..k-1 都已被派发」；而每个被派发的帧都会走到这里
// （onCall 对所有帧无差别登记）⇒ 更早的票号**一定会到**，等待不会落空、也不需要超时猜测。
//
// 三处纪律（错一处就退化成「只互斥不保序」或死锁）：
//  1. **所有**帧都要登记（arrive/done），包括保留操作（store/image/search/describe）与
//     被拒的工具调用 —— 少登记一个，后面的写类调用就会永远等它；
//  2. 写类调用的等待必须发生在 `ExecScope.Acquire` **之前**（先持锁再等更早的票 = 死锁）；
//  3. 票号缺失（`<=0`：直接构造 ScriptCall 的调用面）时整个门退化为直通：既不能因为
//     「少了一个号」把调用卡死，也不能把没号与有号混着数。

import (
	"context"
	"sync"
)

// turnGate 按帧序号（票号）串行化写类调用。
//
// 并发安全：全部字段受 mu 保护。**不会死锁**：写类等待的两个条件都由「票号连续派发」
// 保证（见文件头），且一旦出现过无票号的调用，整个门立刻转为直通（mixed）。
type turnGate struct {
	mu   sync.Mutex
	cond *sync.Cond
	// live 已到场、尚未跑完的票号（arrive 进、done 出）。
	live map[int64]struct{}
	// prefix 最大的 k，使「1..k 都**已到场**」。刻意不用「到达次数」：帧号是乱序到达的
	// （8 可能先于 1 到），次数到位不代表**更早的号**到位 —— 这是 -race 下抓到过的真缺陷
	//（当时写类调用会越过一个还没到场的更早票号）。票号连续 ⇒ prefix 单调推进。
	prefix int64
	// seen 已到场但还没被 prefix 覆盖的票号（缺口）。
	seen map[int64]struct{}
	// mixed 见过无票号的调用：从此刻起 waitTurn 直通（宁可退化成「只互斥不保序」，
	// 也不能因为一个缺号把调用卡死）。
	mixed bool
}

func newTurnGate() *turnGate {
	g := &turnGate{live: map[int64]struct{}{}, seen: map[int64]struct{}{}}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// arrive 登记一次调用到场（**所有**帧都要调，见文件头纪律 1）。返回是否是有票号的调用。
func (g *turnGate) arrive(seq int64) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if seq <= 0 {
		g.mixed = true
		g.cond.Broadcast()
		return
	}
	g.live[seq] = struct{}{}
	g.seen[seq] = struct{}{}
	for {
		if _, ok := g.seen[g.prefix+1]; !ok {
			break
		}
		delete(g.seen, g.prefix+1)
		g.prefix++
	}
	g.cond.Broadcast()
}

// waitTurn 写类调用等到「所有更早的票号都到场且跑完」。必须在 ExecScope.Acquire 之前调。
//
// 带上 ctx：票号缺口在理论上不可能（见文件头），但「万一卡住」的后果是**永久卡住**
// （一个写类调用挂在门上，脚本已经死了它也不会醒）—— 所以 ctx 一取消就必须放行，
// 让上层的「脚本已不在跑」复查（runTool 里的 F4 那道）把它变成可读拒绝。宁可退化，
// 不能挂死。
func (g *turnGate) waitTurn(seq int64, ctx context.Context) {
	if g == nil || seq <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if ctx != nil {
		stop := context.AfterFunc(ctx, func() {
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		})
		defer stop()
	}
	for !g.mixed && (g.prefix < seq || g.earlierLiveLocked(seq)) && (ctx == nil || ctx.Err() == nil) {
		g.cond.Wait()
	}
}

// done 注销（arrive 的配对，必须 defer）。
func (g *turnGate) done(seq int64) {
	if g == nil || seq <= 0 {
		return
	}
	g.mu.Lock()
	delete(g.live, seq)
	g.cond.Broadcast()
	g.mu.Unlock()
}

// earlierLiveLocked 是否还有票号更早的调用没跑完。
func (g *turnGate) earlierLiveLocked(seq int64) bool {
	for s := range g.live {
		if s < seq {
			return true
		}
	}
	return false
}
