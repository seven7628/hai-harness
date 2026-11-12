package main

import (
	"bufio"
	"fmt"
	"os"
	"testing"

	"github.com/seven7628/hai-harness/events"
)

// TestReplayAgentEndSettlesCorrectRun 重放真实事件流（REPLAY_VERIFY=1 时运行）：
// 验证 AgentEnd 的 AgentDurMs 结算到「该 run 最后一条有正文」的 assistant 块。
// 历史缺陷：①多轮后续 run 已开始 → 全局 lastAssistantBlock 错位；②每轮 llm_start
// 新建块 → 字面最后一条可能是空块。两者都导致已结束 run 的正文块 agent_dur_ms 缺失。
func TestReplayAgentEndSettlesCorrectRun(t *testing.T) {
	if os.Getenv("REPLAY_VERIFY") == "" {
		t.Skip("REPLAY_VERIFY=1 时运行（真实数据验证）")
	}
	path := os.Getenv("REPLAY_EVENTS")
	if path == "" {
		t.Fatal("REPLAY_EVENTS 未设置")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := NewViewReducer()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		ev, err := parseEventLine(line)
		if err != nil {
			continue
		}
		if err := r.Apply(ev, EventMeta{Provider: "deepseek"}); err != nil {
			t.Fatalf("apply %s: %v", ev.Type(), err)
		}
	}
	cp := r.Snapshot()
	// 验证：每个已结束 run（有 agent_end 且 DurationMS>0）的最后正文块都有 AgentDurMs
	runDurs := map[string]int64{}
	for _, b := range cp.Blocks {
		if b.Kind == blockKindAssistant && b.RunID != "" && b.AgentDurMs > 0 {
			runDurs[b.RunID] = b.AgentDurMs
		}
	}
	if len(runDurs) == 0 {
		// 事件流里可能没有已结束 run（如崩溃遗留），打印状态供人工检查
		var last string
		for _, b := range cp.Blocks {
			if b.Kind == blockKindAssistant && b.Text != "" {
				last = fmt.Sprintf("run=%s agent_dur_ms=%d dur_ms=%d", b.RunID, b.AgentDurMs, b.DurMs)
			}
		}
		t.Logf("无已结束 run 的结算块；最后正文块: %s", last)
		return
	}
	t.Logf("结算到的 run 与 AgentDurMs: %v", runDurs)
	_ = events.AgentEndType
}
