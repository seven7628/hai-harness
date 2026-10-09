package agents

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// thresholdScriptProvider 单轮脚本化 provider：每次 LLM 请求都发一条 text=ok 的结束事件。
// 自包含（不依赖同包其它测试文件的 mockProvider）：本文件只验压缩阈值，不需要真流式。
type thresholdScriptProvider struct{}

func (thresholdScriptProvider) Stream(_ context.Context, _ *provider.StreamRequest, handler func(provider.StreamEvent) error) error {
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
}

// thresholdProbeCompressor 记录每次 ShouldCompact 收到的触发线（Budget），并照抄
// LLMCompressor 的触发公式（EstimatedTokens >= Budget）—— 阈值变化只该影响 Budget，
// 预估不变，于是「压缩/不压缩」的差异必然来自阈值。
type thresholdProbeCompressor struct {
	budgets      []int64
	compactCalls int
}

func (c *thresholdProbeCompressor) ShouldCompact(_ context.Context, stats CompressStats) bool {
	c.budgets = append(c.budgets, stats.Budget)
	return stats.EstimatedTokens >= stats.Budget
}

func (c *thresholdProbeCompressor) Compact(_ context.Context, messages []core.Message) (CompressResult, error) {
	c.compactCalls++
	return CompressResult{Messages: messages}, nil
}

// runWithThreshold 以指定阈值跑一轮空转 Run，返回压缩器（已记账）。
func runWithThreshold(t *testing.T, threshold float64) *thresholdProbeCompressor {
	t.Helper()
	probe := &thresholdProbeCompressor{}
	loop := NewAgentLoop(
		WithProvider(thresholdScriptProvider{}),
		WithCompressor(probe),
		WithContextWindow(125),
	)
	loop.SetCompressThreshold(threshold)
	_ = loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, nil)
	return probe
}

// TestCompressThresholdDrivesCompaction 阈值的**行为**证明（值 → Config → 消费 三段）：
// 断言触发线数值本身 = 窗口 × 阈值，且「触发线远超预估时真的不压缩」。
// 预估（默认 system prompt + 一条 user 消息）远小于 125，所以两条腿的差异必然来自阈值。
//
// 这是 settings 面板那个滑杆的 agents 侧落点测试：前端 set_compress_threshold 命令 →
// bridge → AgentLoop.SetCompressThreshold → 这里跑的就是「下一次压缩判定」。
func TestCompressThresholdDrivesCompaction(t *testing.T) {
	low := runWithThreshold(t, 0.05) // 触发线 6
	high := runWithThreshold(t, 1.0) // 触发线 125
	if low.budgets[len(low.budgets)-1] != 6 {
		t.Fatalf("阈值 0.05 的触发线 = %d, want 6（125 × 0.05）", low.budgets[len(low.budgets)-1])
	}
	if high.budgets[len(high.budgets)-1] != 125 {
		t.Fatalf("阈值 1.0 的触发线 = %d, want 125（125 × 1.0）", high.budgets[len(high.budgets)-1])
	}

	// 窗口放大到远超预估：阈值 1.0 时触发线也远高于预估 → 不压缩。
	// 这条腿证明 compactCalls 真的对触发线敏感（不是恒为 1）。
	probe := &thresholdProbeCompressor{}
	loop := NewAgentLoop(
		WithProvider(thresholdScriptProvider{}),
		WithCompressor(probe),
		WithContextWindow(125),
	)
	loop.SetCompressThreshold(1.0)
	loop.SetContextWindow(1 << 40) // 触发线 = 窗口 × 1.0，远大于任何预估
	_ = loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, nil)
	if probe.compactCalls != 0 {
		t.Fatalf("触发线远超预估时不应压缩，got %d", probe.compactCalls)
	}
}

// TestSetCompressThresholdRejectsInvalid 非法阈值不得把压缩静默关掉：
// 0 / 负数按默认兜底（0 = 永不压缩，是误配不是选项）。bridge 侧读设置时已钳过一次，
// 这里是 agents 侧的同一道防线（两处独立钳，防任一处漏）。
func TestSetCompressThresholdRejectsInvalid(t *testing.T) {
	loop := NewAgentLoop(WithCompressThreshold(0.8))
	// `!(t>0)` 那一侧：0 / 负数 / NaN（Go 里 NaN 的所有比较都是 false，被这句覆盖）
	for _, bad := range []float64{0, -0.3, -1, math.NaN()} {
		loop.SetCompressThreshold(bad)
		if got := loop.CompressThreshold(); got != defaultCompressThreshold {
			t.Errorf("SetCompressThreshold(%v) → %v, want 默认 %v（不得把压缩关掉）", bad, got, defaultCompressThreshold)
		}
	}
	// `t>1` 那一侧：>1 与 +Inf。它们让触发线 = int64(窗口 × t) 永远够不着
	//（+Inf 还会饱和成 MaxInt64），压缩静默永不触发 —— 与 0 同属"误配不是选项"。
	for _, bad := range []float64{1.0000001, 1.5, 3, math.Inf(1)} {
		loop.SetCompressThreshold(bad)
		if got := loop.CompressThreshold(); got != defaultCompressThreshold {
			t.Errorf("SetCompressThreshold(%v) → %v, want 默认 %v（>1 会让压缩永不触发）", bad, got, defaultCompressThreshold)
		}
	}
	// 合法值必须生效（含端点 1.0 = 窗口占满才压缩）
	for _, ok := range []float64{0.05, 0.5, 0.8, 0.95, 1} {
		loop.SetCompressThreshold(ok)
		if got := loop.CompressThreshold(); got != ok {
			t.Errorf("SetCompressThreshold(%v) → %v, want %v（合法值必须生效）", ok, got, ok)
		}
	}
}

// TestSetCompressThresholdTakesEffectBeforeNextCompact 热更新生效时机：阈值改动在
// **下一次**压缩判定生效（同一 loop 实例上立刻可读），不重建 loop、不重置状态。
func TestSetCompressThresholdTakesEffectBeforeNextCompact(t *testing.T) {
	probe := &thresholdProbeCompressor{}
	loop := NewAgentLoop(
		WithProvider(thresholdScriptProvider{}),
		WithCompressor(probe),
		WithContextWindow(125),
	)
	_ = loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, nil)
	if got := probe.budgets[len(probe.budgets)-1]; got != 100 {
		t.Fatalf("默认阈值触发线 = %d, want 100（125 × 0.8）", got)
	}

	loop.SetCompressThreshold(1.0)
	_ = loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, nil)
	if got := probe.budgets[len(probe.budgets)-1]; got != 125 {
		t.Fatalf("改成 1.0 后触发线 = %d, want 125（同一 loop 实例上立即生效）", got)
	}
}

// TestCompressThresholdConcurrentSetAndCompact 数据竞争回归：SetCompressThreshold（bridge
// 命令 goroutine）与 maybeCompact 算触发线（run goroutine）并发跑，-race 下必须干净。
//
// 为什么需要这条（2026-10 修）：CompressThreshold 是 cfg 里**第一个运行期可写的压缩
// 字段**（此前 CompressMargin 等只读）。写侧走 cfgMu、读侧若直接摸 a.cfg 就是竞争 ——
// 单测顺序跑看不出来，只在生产「设置面板拖动滑杆的同时会话在压缩」时炸。
// maybeCompact 的读必须经 currentCompressThreshold()（与 currentContextWindow 同口径）。
//
// 读侧用**真的 loop.Run()**（它会进 maybeCompact 算 Budget），不是复制的读表达式 ——
// 只有跑生产路径，变异验证（把读改回无锁 a.cfg）才抓得住。
func TestCompressThresholdConcurrentSetAndCompact(t *testing.T) {
	probe := &thresholdProbeCompressor{}
	loop := NewAgentLoop(
		WithProvider(thresholdScriptProvider{}),
		WithCompressor(probe),
		WithContextWindow(125),
	)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读侧（生产路径）：单 goroutine 连续跑 Run —— 每轮都会进 maybeCompact 读
	// CompressThreshold。必须是**一个** run：同一 loop 并发跑多个 Run 本身就违反
	// AgentLoop 的使用契约（会话是串行轮次），也只会测到夹具自身的竞争。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = loop.Run(context.Background(),
					[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, nil)
			}
		}
	}()

	// 写侧：模拟多个来源并发改阈值（set_compress_threshold 命令 + reload_settings）
	for _, v := range []float64{0.3, 0.5, 0.7, 0.9} {
		wg.Add(1)
		go func(threshold float64) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					loop.SetCompressThreshold(threshold)
				}
			}
		}(v)
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	// 竞争期间必须真的发生过压缩判定 —— 否则测试会退化成"什么都没测到也绿"
	// （例如将来 maybeCompact 提前 return / ShouldCompact 被跳过）。
	if len(probe.budgets) == 0 {
		t.Fatal("并发期间一次压缩判定都没发生 —— 读侧没走到生产路径，测试是空跑")
	}

	// 生效性兜底：最后一次触达的 run 仍算出合法触发线（没被竞争破坏成野值）
	_ = loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, nil)
	got := probe.budgets[len(probe.budgets)-1]
	if got < 6 || got > 125 {
		t.Fatalf("触发线 = %d，超出 [6,125]（竞争写坏了值？）", got)
	}
}
