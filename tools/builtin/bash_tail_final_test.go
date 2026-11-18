package builtin

// 末帧补发（第 5 轮爬山第 1 步）。
//
// 问题（实测）：节流窗口 250ms ⇒ 命令**最后 250ms 内**的输出永远发不出去。
// 实测 `echo FIRST; sleep 0.1; echo LAST` ⇒ 一帧都没有 —— 界面上什么都看不到，
// 而**错误信息通常就在结尾**。
//
// 两条必须钉住的：
//  1. 长命令：最后一段输出**必须**出现在某一帧里（哪怕它落在节流窗口内）；
//  2. 短命令：**仍然一帧都不发**（防抖是有意的 —— 0.1s 的命令不该闪一下）。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
)

// collectTailFrames 跑一条命令，收它发出的全部尾巴帧。
func collectTailFrames(t *testing.T, command string) ([]*events.ToolOutput, time.Duration) {
	t.Helper()
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
	ctx := events.WithToolContextForRun(context.Background(), "run-final", "", 0, handler, nil, nil)

	start := time.Now()
	if _, err := eng.Sequence(ctx, []core.ToolCall{{
		Id: "call-final", Name: "bash", Arguments: mustJSON(t, map[string]string{"command": command}),
	}}, handler); err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	elapsed := time.Since(start)

	mu.Lock()
	defer mu.Unlock()
	return append([]*events.ToolOutput(nil), outs...), elapsed
}

// mustJSON 把参数序列化成 JSON（命令里有引号/换行时不能手拼）。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

// TestBashTailFinalFrameCarriesLastOutput：最后一段输出必须发出去。
//
// 命令时序（关键）：phase-1 → 睡 0.45s（跨过节流窗口 ⇒ 发过一帧）→ phase-2 →
// 睡 0.1s → FINAL-LINE。FINAL-LINE 落在 phase-2 那一帧之后的 100ms 内（节流窗口内）
// ⇒ **不补帧就永远看不到它**。
func TestBashTailFinalFrameCarriesLastOutput(t *testing.T) {
	cmd := "printf 'phase-1\\n'; sleep 0.45; printf 'phase-2\\n'; sleep 0.1; printf 'FINAL-LINE\\n'"
	frames, _ := collectTailFrames(t, cmd)
	if len(frames) == 0 {
		t.Fatal("长命令应至少发出一帧（实际 0 帧）")
	}
	last := frames[len(frames)-1]
	if !strings.Contains(last.Tail, "FINAL-LINE") {
		t.Fatalf("最后一帧必须含命令的末尾输出（FINAL-LINE），实际尾帧=%q（共 %d 帧）",
			last.Tail, len(frames))
	}
}

// TestBashTailShortCommandStaysSilent：短命令仍然**一帧都不发**（防抖不许破）。
//
// 补帧只在「已经发过帧」时发生 ⇒ 短命令（连一帧都没发过）不受影响。
func TestBashTailShortCommandStaysSilent(t *testing.T) {
	frames, elapsed := collectTailFrames(t, "printf 'quick\\n'")
	if elapsed > tailMinInterval {
		t.Skipf("本机太慢（命令耗时 %v > 节流窗口 %v），这条断言不适用", elapsed, tailMinInterval)
	}
	if len(frames) != 0 {
		t.Fatalf("短命令不该发帧（防抖），实际 %d 帧：%q", len(frames), frames[0].Tail)
	}
}
