package builtin

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
)

// 运行中的输出尾巴（tail -f）——只给**人**看，绝不进模型上下文。
//
// 三条必须钉住的：
//  1. 节流：第一个时间片内一帧都不发（0.1s 的命令不该抖一下）；
//  2. 有界：单帧 ≤ maxTailBytes，但 TotalBytes 是**真值**（人要知道一共跑了多少输出）；
//  3. 不拖慢：回调在 os/exec 的拷贝协程里跑，必须是瞬时操作（这里用锁内计数验证）。

func newTestTailer() (*outputTailer, func() []*events.ToolOutput) {
	var mu sync.Mutex
	var got []*events.ToolOutput
	t := &outputTailer{
		lastFlush: time.Now(),
		emit: func(seq int64, tail string, total int64, truncated bool) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, &events.ToolOutput{Seq: seq, Tail: tail, TotalBytes: total, Truncated: truncated})
		},
	}
	return t, func() []*events.ToolOutput {
		mu.Lock()
		defer mu.Unlock()
		return append([]*events.ToolOutput(nil), got...)
	}
}

func TestOutputTailerThrottlesFirstSlice(t *testing.T) {
	tl, frames := newTestTailer()
	tl.write([]byte("early output\n"))
	if n := len(frames()); n != 0 {
		t.Fatalf("第一个时间片内不该发帧（短命令不闪），实际 %d 帧", n)
	}
	time.Sleep(tailMinInterval + 40*time.Millisecond)
	tl.write([]byte("later\n"))
	fs := frames()
	if len(fs) != 1 {
		t.Fatalf("过了时间片应发一帧，实际 %d 帧", len(fs))
	}
	if !strings.Contains(fs[0].Tail, "early output") || !strings.Contains(fs[0].Tail, "later") {
		t.Fatalf("帧应包含到目前为止的全部窗口：%q", fs[0].Tail)
	}
	if fs[0].Seq != 1 {
		t.Fatalf("帧号应从 1 起，实际 %d", fs[0].Seq)
	}
}

func TestOutputTailerBoundsWindowKeepsTrueTotal(t *testing.T) {
	tl, frames := newTestTailer()
	big := strings.Repeat("x", 10_000)
	tl.write([]byte(big))
	time.Sleep(tailMinInterval + 40*time.Millisecond)
	tl.write([]byte("x")) // 触发一帧
	fs := frames()
	if len(fs) != 1 {
		t.Fatalf("want 1 frame, got %d", len(fs))
	}
	if len(fs[0].Tail) > maxTailBytes {
		t.Fatalf("单帧窗口应 ≤ %d，实际 %d", maxTailBytes, len(fs[0].Tail))
	}
	if fs[0].TotalBytes != 10_001 {
		t.Fatalf("TotalBytes 应是真值 10001，实际 %d", fs[0].TotalBytes)
	}
	if !fs[0].Truncated {
		t.Fatal("窗口小于总量时必须标记 Truncated（否则人会以为这就是全部输出）")
	}
}

func TestOutputTailerWindowCutRuneSafe(t *testing.T) {
	tl, frames := newTestTailer()
	// 3 字节汉字连续写，制造「窗口左边界落在字符中间」的情形
	unit := "测试日志"
	var b strings.Builder
	for b.Len() < maxTailBytes+10 {
		b.WriteString(unit)
	}
	tl.write([]byte(b.String()))
	time.Sleep(tailMinInterval + 40*time.Millisecond)
	tl.write([]byte(unit))
	fs := frames()
	if len(fs) != 1 {
		t.Fatalf("want 1 frame, got %d", len(fs))
	}
	if !utf8.ValidString(fs[0].Tail) {
		t.Fatalf("窗口左边界必须落在 rune 边界（否则前端显示乱码）：%q", fs[0].Tail[:12])
	}
	if !strings.HasSuffix(fs[0].Tail, unit) {
		t.Fatalf("窗口尾部应是最新输出：%q", fs[0].Tail)
	}
}

// 走真实引擎：CallId 由引擎逐次注入，工具据此发出归属到本行的事件。
// 这条链路断开时不会报错 —— 只是一帧尾巴都不出现（静默失效），所以必须集成级验证。
func TestBashEmitsToolOutputThroughEngine(t *testing.T) {
	eng := tools.NewToolEngine()
	bt := NewBashTool(t.TempDir(), 30*time.Second, nil, nil)
	eng.RegisterTool(context.Background(), bt)

	var mu sync.Mutex
	var outs []*events.ToolOutput
	handler := func(_ context.Context, ev events.Event) {
		if o, ok := ev.(*events.ToolOutput); ok {
			mu.Lock()
			outs = append(outs, o)
			mu.Unlock()
		}
	}
	ctx := events.WithToolContextForRun(context.Background(), "run-1", "", 0, handler, nil, nil)

	// 命令前半段输出、然后睡过时间片、再输出：至少有一帧要带上前半段。
	_, err := eng.Sequence(ctx, []core.ToolCall{{
		Id:        "call-tail-1",
		Name:      "bash",
		Arguments: `{"command":"printf 'phase-1\\n'; sleep 0.45; printf 'phase-2\\n'"}`,
	}}, handler)
	if err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(outs) == 0 {
		t.Fatal("运行中应有 ToolOutput 帧（带 CallId），实际一帧都没有")
	}
	for _, o := range outs {
		if o.Id != "call-tail-1" {
			t.Fatalf("帧应归属到本次调用 id，实际 %q", o.Id)
		}
		if o.RunId != "run-1" {
			t.Fatalf("帧应带运行 id，实际 %q", o.RunId)
		}
		if o.EventType != events.ToolOutputType {
			t.Fatalf("事件类型应为 tool_output，实际 %q", o.EventType)
		}
	}
	if !strings.Contains(outs[0].Tail, "phase-1") {
		t.Fatalf("第一帧应包含已产生的前半段输出，实际 %q", outs[0].Tail)
	}
}

// 没有事件出口（直调工具/嵌套脚本）时：**不产生任何帧**，执行路径与历史一致。
func TestBashTailIsInertWithoutHandler(t *testing.T) {
	bt := NewBashTool(t.TempDir(), 30*time.Second, nil, nil)
	out, err := bt.Call(context.Background(), "bash", `{"command":"printf 'no-handler\\n'"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "no-handler") {
		t.Fatalf("结果应正常返回：%q", out)
	}
	if tl := newOutputTailer(context.Background()); tl != nil {
		t.Fatal("无 ToolContext 时 tailer 必须为 nil（OnOutput 保持 nil = 零开销）")
	}
}
