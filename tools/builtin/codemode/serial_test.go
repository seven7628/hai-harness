package codemode

// serial_test.go：写类**保序**门（turnGate）的单元测试。
//
// 门依赖一条不变量：「票号在读帧处连续发号」⇒ 票号 k 意味着 1..k-1 一定会到场。
// 这里用假票号把这条不变量造出来（真链路由 execproc 的
// TestScriptCallIndexFollowsFrameOrder + bridge 的 TestWriteClassCallsRunInScriptOrder 钉）。

import (
	"context"
	"sync"
	"testing"
	"time"
)

// runThrough 走一次完整路径：到场 → 等到轮次 → 记录 → 跑完。
func runThrough(g *turnGate, seq int64, log *[]int64, mu *sync.Mutex) {
	g.arrive(seq)
	defer g.done(seq)
	g.waitTurn(seq, nil)
	mu.Lock()
	*log = append(*log, seq)
	mu.Unlock()
	time.Sleep(time.Millisecond) // 模拟写类调用的临界区
}

func TestTurnGateKeepsTicketOrder(t *testing.T) {
	g := newTurnGate()
	const n = 8
	var mu sync.Mutex
	var order []int64
	var wg sync.WaitGroup
	// 故意**反序**发起：票号 8 先进门，票号 1 最后才到场 —— 这正是修前会反序的形态
	//（先到的票号不能倒回，必须等更早的票号到场）。
	for i := n; i >= 1; i-- {
		wg.Add(1)
		seq := int64(i)
		go func() {
			defer wg.Done()
			runThrough(g, seq, &order, &mu)
		}()
	}
	wg.Wait()
	if len(order) != n {
		t.Fatalf("放行次数 = %d, want %d（有调用被卡住）", len(order), n)
	}
	for i, seq := range order {
		if seq != int64(i+1) {
			t.Fatalf("执行次序 = %v, want 1..%d（脚本先发起必须先执行）", order, n)
		}
	}
}

// 并行（非写类）调用只登记、不等待：它们可以与别的调用重叠，但**写类**调用仍然要等它们跑完。
func TestTurnGateParallelCallsDoNotBlockButAreWaitedFor(t *testing.T) {
	g := newTurnGate()
	var wg sync.WaitGroup
	// 票号 1：并行调用，跑 50ms（登记但不等待）
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.arrive(1)
		defer g.done(1)
		time.Sleep(50 * time.Millisecond)
	}()
	// 票号 2：写类调用，必须等 1 跑完
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		g.arrive(2)
		defer g.done(2)
		g.waitTurn(2, nil)
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		if d < 40*time.Millisecond {
			t.Errorf("写类调用只等了 %v —— 没有等更早的并行调用跑完", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("写类调用被卡住（等不到更早的票号）")
	}
	wg.Wait()
}

// 无票号的调用（直接构造 ScriptCall 的调用面）：直通、不阻塞、也不会把有票号的调用卡死。
func TestTurnGateDegradesToPassThroughOnUnticketedCalls(t *testing.T) {
	g := newTurnGate()
	g.arrive(0) // 无票号：标记 mixed
	g.arrive(0)
	g.done(0)
	done := make(chan struct{})
	go func() {
		g.arrive(5)
		defer g.done(5)
		g.waitTurn(5, nil) // mixed 之后直通：不会因为「票号 1..4 永远不来」而卡住
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("出现无票号调用后，有票号的调用被卡住了（缺号不该变成死锁）")
	}
	var nilGate *turnGate
	nilGate.arrive(1)
	nilGate.waitTurn(1, nil)
	nilGate.done(1) // 零值（未装配）不能 panic
}

// 缺口形态（-race 下抓到过的真缺陷）：票号是**乱序**到达的，所以「已到场次数」不能代表
// 「更早的号都到场了」。这里把票号 5 单独先放进来 —— 它必须一直等到 1..4 到场**且跑完**。
func TestTurnGateWaitsForMissingEarlierTickets(t *testing.T) {
	g := newTurnGate()
	g.arrive(5)
	passed := make(chan struct{})
	go func() {
		g.waitTurn(5, nil)
		close(passed)
	}()
	// 1..4 还没到场：不许放行（修前把 arrived 当「最大票号」，这里就会过）。
	select {
	case <-passed:
		t.Fatal("票号 5 在 1..4 还没到场时就放行了（把「到达次数」当成了「更早的号都到了」）")
	case <-time.After(150 * time.Millisecond):
	}
	// 1..4 到场但都没跑完：仍然不许放行（写类必须等到更早的**跑完**）。
	for i := int64(1); i <= 4; i++ {
		g.arrive(i)
	}
	select {
	case <-passed:
		t.Fatal("票号 5 在更早的票号还没跑完时就放行了")
	case <-time.After(150 * time.Millisecond):
	}
	for i := int64(1); i <= 4; i++ {
		g.done(i)
	}
	select {
	case <-passed:
	case <-time.After(2 * time.Second):
		t.Fatal("更早的票号全部跑完后，票号 5 仍被卡住")
	}
	g.done(5)
}

// ctx 取消必须让等待立刻返回：票号缺口在理论上不可能，但「万一」的后果是永久卡住 ——
// 宁可退化成「不保序」，也绝不能让一个写类调用挂在门上等一个永远不会来的号。
func TestTurnGateWakesOnContextCancel(t *testing.T) {
	g := newTurnGate()
	g.arrive(9) // 1..8 永远不来（模拟缺口）
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.waitTurn(9, ctx)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("缺口尚未补上时不该放行")
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后等待没有返回（缺口 = 永久卡住）")
	}
	g.done(9)
}
