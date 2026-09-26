package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// 真实会话回放基准：用仓库外采样的真实 session 状态消息，度量 C/D/E 三段
// 各自以及合计的上下文降幅。
//
// 为什么必须用真实数据：合成会话（重复读同一文件 ×6 + 重复 bash ×5）会系统性
// 高估收益 —— 它按「重复率 100%」构造，而真实会话里绝大多数工具结果都是** uniques
// 的**（读了 20 个不同文件、跑了 12 条不同命令）。降幅必须以真实重复率为基。
//
// 夹具由 tools 外的脚本从 ~/.go-code/sessions 的 state 记录导出（见文件尾说明）。

type realSession struct {
	File     string          `json:"file"`
	Messages []core.Message  `json:"messages"`
	Raw      json.RawMessage `json:"-"`
}

// replayCtx 按真实顺序回放：每条 tool 结果入上下文前过一遍 slimToolResult
// （与 agent_loop 回写循环同序），每轮结算段过 applyStale + mergeToolPairs。
func replaySession(t *testing.T, msgs []core.Message, stage string) (int, int) {
	t.Helper()
	opts := []Option{WithToolResultSlim()}
	switch stage {
	case "off":
	case "cd":
		opts = append(opts, WithSlimText(SlimTextOptions{MergePairs: boolPtr(false)}))
	case "cde":
		opts = append(opts, WithSlimText(SlimTextOptions{}))
	default:
		t.Fatalf("未知 stage %q", stage)
	}
	a := NewAgentLoop(opts...)
	ac := &AgentContext{slim: newSlimState(*a.cfg.Slim)}

	// ToolCallId → (工具名, 参数)：真实日志的 tool 消息只带 id
	meta := map[string][2]string{}
	for _, m := range msgs {
		if m.Role != core.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			meta[tc.Id] = [2]string{tc.Name, tc.Arguments}
		}
	}

	before, after := 0, 0
	for _, m := range msgs {
		if m.Role == core.System || m.Role == core.User {
			ac.Messages = append(ac.Messages, m)
			before += msgBytes(m)
			after += msgBytes(m)
			continue
		}
		if m.Role == core.Assistant {
			ac.Messages = append(ac.Messages, m)
			before += msgBytes(m)
			after += msgBytes(m)
			continue
		}
		// tool 结果：过一遍瘦身再入上下文（与生产同序）
		before += msgBytes(m)
		kv := meta[m.ToolCallId]
		name, args := kv[0], kv[1]
		var text string
		var imgs []core.Content
		for _, c := range m.Content {
			if c.Type == core.ContentTypeImage {
				imgs = append(imgs, c)
			} else {
				text += c.Content
			}
		}
		call := core.ToolCall{Id: m.ToolCallId, Name: name, Arguments: args}
		idx := len(ac.Messages)
		text = a.slimToolResult(ac, call, text, idx)
		// 图片原样携带（生产同款：slim 只作用于 text）
		ac.Messages = append(ac.Messages, core.NewToolMessageWithImages(m.ToolCallId, text, imgs))
		after += len(text) + len(m.ToolCallId) + 16 + imgBytes(imgs)
	}
	// 末尾跑一次结算段（applyStale + merge）
	if stage != "off" {
		a.applyStale(ac)
		if stage == "cde" && a.mergeToolPairs(ac, 0) {
			ac.slim.reset()
		}
		after = sumMsgBytes(ac.Messages)
	}
	return before, after
}

func imgBytes(imgs []core.Content) int {
	n := 0
	for _, c := range imgs {
		n += len(c.Content)
	}
	return n
}

func msgBytes(m core.Message) int {
	n := 0
	for _, c := range m.Content {
		n += len(c.Content)
	}
	for _, tc := range m.ToolCalls {
		n += len(tc.Name) + len(tc.Arguments) + len(tc.Id) + 24
	}
	if m.ToolCallId != "" {
		n += len(m.ToolCallId) + 16
	}
	return n
}

func sumMsgBytes(msgs []core.Message) int {
	n := 0
	for _, m := range msgs {
		n += msgBytes(m)
	}
	return n
}

// TestRealSessionReplay 实测真实会话的降幅。夹具缺失时 t.Skip（不伪造数字）。
func TestRealSessionReplay(t *testing.T) {
	raw, err := os.ReadFile("../.testdata/real_sessions.json")
	if err != nil {
		t.Skipf("真实会话夹具缺失（%v）——先跑 tools/bench/export_real_sessions.py 生成", err)
	}
	var sessions []realSession
	if err := json.Unmarshal(raw, &sessions); err != nil {
		t.Fatalf("夹具解析失败: %v", err)
	}
	if len(sessions) == 0 {
		t.Skip("夹具为空")
	}

	type row struct {
		file         string
		msgs         int
		off, cd, cde int
	}
	var rows []row
	totOff, totCD, totCDE := 0, 0, 0
	for _, s := range sessions {
		if len(s.Messages) < 50 {
			continue
		}
		off, _ := replaySession(t, s.Messages, "off")
		_, cd := replaySession(t, s.Messages, "cd")
		_, cde := replaySession(t, s.Messages, "cde")
		rows = append(rows, row{s.File, len(s.Messages), off, cd, cde})
		totOff += off
		totCD += cd
		totCDE += cde
	}
	if len(rows) == 0 {
		t.Skip("夹具中无足够大的会话")
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].off > rows[j].off })

	t.Logf("%-40s %6s %12s %12s %12s %7s %7s", "会话", "消息数", "原始", "C+D", "C+D+E", "CD", "CDE")
	for _, r := range rows {
		t.Logf("%-40s %6d %12d %12d %12d %6.1f%% %6.1f%%",
			r.file[:min(40, len(r.file))], r.msgs, r.off, r.cd, r.cde,
			100*float64(r.off-r.cd)/float64(r.off), 100*float64(r.off-r.cde)/float64(r.off))
	}
	t.Logf("合计：%d → C+D %d（-%.1f%%） → C+D+E %d（-%.1f%%）",
		totOff, totCD, 100*float64(totOff-totCD)/float64(totOff),
		totCDE, 100*float64(totOff-totCDE)/float64(totOff))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestRealSessionRepeatProfile 量化真实会话的重复率 —— 决定 E 段收益上限的
// 关键事实（合成基准里重复率是 100%，真实会话远低于此）。
// TestRealSessionCompressibleSlice 换一个口径：在**可瘦身文本**上（工具结果里的
// 文本部分，剔除 base64 图片与 assistant 自己的叙述）度量降幅。
//
// 为什么必须分口径：真实会话的上下文里 base64 截图能占到 90%+（单个 ~1MB），
// 而 C/D/E 三段**按设计不碰图片**。用全会话口径衡量，降幅会被图片稀释到 0.2% ——
// 那个数字真实但无意义；用纯文本口径衡量，才回答「这些机制值不值得开」。
func TestRealSessionCompressibleSlice(t *testing.T) {
	raw, err := os.ReadFile("../.testdata/real_sessions.json")
	if err != nil {
		t.Skip("夹具缺失")
	}
	var sessions []realSession
	if err := json.Unmarshal(raw, &sessions); err != nil {
		t.Fatalf("夹具解析失败: %v", err)
	}
	type row struct {
		file          string
		toolText, img int
		cdOff, cdOn   int
		cdeOff, cdeOn int
	}
	var rows []row
	totImg, totOff, totCD, totCDE := 0, 0, 0, 0
	for _, s := range sessions {
		if len(s.Messages) < 50 {
			continue
		}
		// 工具结果文本总量（含图片）——图片单列，用于说明稀释效应
		var toolText, img int
		for _, m := range s.Messages {
			if m.Role != core.Tool {
				continue
			}
			for _, c := range m.Content {
				if c.Type == core.ContentTypeImage {
					img += len(c.Content)
				} else {
					toolText += len(c.Content)
				}
			}
		}
		// 三个口径分别回放
		offB, _ := replaySlice(t, s.Messages, "off")
		_, cdA := replaySlice(t, s.Messages, "cd")
		_, cdeA := replaySlice(t, s.Messages, "cde")
		rows = append(rows, row{s.File, toolText, img, offB, cdA, offB, cdeA})
		totImg += img
		totOff += offB
		totCD += cdA
		totCDE += cdeA
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].cdOff > rows[j].cdOff })
	t.Logf("%-40s %11s %11s %11s %7s %7s", "会话", "可瘦身文本", "CD降幅", "CDE降幅", "图片占比", "")
	for _, r := range rows {
		t.Logf("%-40s %11d %10.1f%% %10.1f%% %8.1f%%",
			r.file[:min(40, len(r.file))], r.cdOff,
			100*float64(r.cdOff-r.cdOn)/float64(r.cdOff),
			100*float64(r.cdeOff-r.cdeOn)/float64(r.cdeOff),
			100*float64(r.img)/float64(r.img+r.cdOff))
	}
	t.Logf("合计：可瘦身文本 %d → C+D %d（-%.1f%%） → C+D+E %d（-%.1f%%）；同批会话图片合计 %d 字节（占总量 %.0f%%，三段不触碰）",
		totOff, totCD, 100*float64(totOff-totCD)/float64(totOff),
		totCDE, 100*float64(totOff-totCDE)/float64(totOff),
		totImg, 100*float64(totImg)/float64(totImg+totOff))
}

// replaySlice 只统计工具结果文本的字节（可瘦身口径），忽略图片/叙述/协议开销。
func replaySlice(t *testing.T, msgs []core.Message, stage string) (int, int) {
	t.Helper()
	opts := []Option{WithToolResultSlim()}
	switch stage {
	case "off":
	case "cd":
		opts = append(opts, WithSlimText(SlimTextOptions{MergePairs: boolPtr(false)}))
	case "cde":
		opts = append(opts, WithSlimText(SlimTextOptions{}))
	}
	a := NewAgentLoop(opts...)
	ac := &AgentContext{slim: newSlimState(*a.cfg.Slim)}
	meta := map[string][2]string{}
	for _, m := range msgs {
		if m.Role != core.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			meta[tc.Id] = [2]string{tc.Name, tc.Arguments}
		}
	}
	before := 0
	for _, m := range msgs {
		if m.Role != core.Tool {
			ac.Messages = append(ac.Messages, m)
			continue
		}
		var text string
		for _, c := range m.Content {
			if c.Type != core.ContentTypeImage {
				text += c.Content
			}
		}
		before += len(text)
		kv := meta[m.ToolCallId]
		idx := len(ac.Messages)
		text = a.slimToolResult(ac, core.ToolCall{Id: m.ToolCallId, Name: kv[0], Arguments: kv[1]}, text, idx)
		ac.Messages = append(ac.Messages, core.NewToolMessage(m.ToolCallId, text))
	}
	if stage != "off" {
		a.applyStale(ac)
		if stage == "cde" && a.mergeToolPairs(ac, 0) {
			ac.slim.reset()
		}
	}
	after := 0
	for _, m := range ac.Messages {
		if m.Role == core.Tool {
			for _, c := range m.Content {
				if c.Type != core.ContentTypeImage {
					after += len(c.Content)
				}
			}
		}
	}
	return before, after
}

func TestRealSessionRepeatProfile(t *testing.T) {
	raw, err := os.ReadFile("../.testdata/real_sessions.json")
	if err != nil {
		t.Skip("夹具缺失")
	}
	var sessions []realSession
	if err := json.Unmarshal(raw, &sessions); err != nil {
		t.Fatalf("夹具解析失败: %v", err)
	}
	for _, s := range sessions {
		meta := map[string][2]string{}
		for _, m := range s.Messages {
			if m.Role != core.Assistant {
				continue
			}
			for _, tc := range m.ToolCalls {
				meta[tc.Id] = [2]string{tc.Name, tc.Arguments}
			}
		}
		var total, dupArgs, dupExact int
		seenArgs := map[string]bool{}
		seenExact := map[string]bool{}
		for _, m := range s.Messages {
			if m.Role != core.Tool {
				continue
			}
			total++
			kv := meta[m.ToolCallId]
			name, args := kv[0], kv[1]
			if name == "" {
				continue
			}
			ka := name + "\x00" + args
			if seenArgs[ka] {
				dupArgs++
			}
			seenArgs[ka] = true
			txt := ""
			if len(m.Content) > 0 {
				txt = m.Content[0].Content
			}
			ke := ka + "\x00" + txt
			if seenExact[ke] {
				dupExact++
			}
			seenExact[ke] = true
		}
		if total == 0 {
			continue
		}
		t.Logf("%-42s 工具结果 %4d  同参数重跑 %3d (%.1f%%)  同参数同结果 %3d (%.1f%%)",
			s.File[:min(42, len(s.File))], total,
			dupArgs, 100*float64(dupArgs)/float64(total),
			dupExact, 100*float64(dupExact)/float64(total))
	}
	_ = strings.TrimSpace
	_ = fmt.Sprintf
}
