package agents

// Step 3 注入在系统提示词里的落位（MemoryInjection 装配）。
//
// 放在 agents 包内而不是 bridge：这两条断言要读 composeSystemPrompt（未导出），
// 而它们要保护的恰恰是**装配层看不见的那一段** —— bridge 只能保证传进来什么，
// 拼在什么位置、有没有破坏「无记忆时逐字节不变」，只有这一层能验。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMemoryInjectionAppearsAfterWorkingMemory：索引必须出现在 AGENTS.md **之后**。
//
// 官方要求"与 AGENTS.md 同级或其后"。顺序有语义：工作记忆是模型的**项目指令**，
// 索引是模型**自己写下的**东西，指令在前、自身记录在后。
func TestMemoryInjectionAppearsAfterWorkingMemory(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("# P\n\nMARKER_AGENTS_MD\n"), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	loop := NewAgentLoop(WithAgentMDDir(ws), WithMemoryInjection("MARKER_MEMORY_INDEX"))
	sp := loop.composeSystemPrompt()
	if !strings.Contains(sp, "MARKER_MEMORY_INDEX") {
		t.Fatalf("索引未进系统提示词：\n%s", sp)
	}
	iMD, iMem := strings.Index(sp, "MARKER_AGENTS_MD"), strings.Index(sp, "MARKER_MEMORY_INDEX")
	if iMD < 0 || iMem < 0 || iMem < iMD {
		t.Fatalf("注入位置错误：AGENTS.md@%d 记忆索引@%d（要求索引在其后）", iMD, iMem)
	}
}

// TestNoMemoryLeavesPromptByteIdentical：没有记忆时系统提示词**逐字节不变**。
//
// 技术方案 §5.2 的硬要求。接线最容易破坏它的地方是"无索引时也塞一段占位说明"
// —— 那既占 context，又让模型以为存在某种它读不到的记忆。
//
// 这里同时钉住两种"没有记忆"的表达方式：完全不调 WithMemoryInjection，以及显式
// 传空串。宿主在实际装配里用的是后者（opts 里那个 if 不成立时不追加，但显式空串
// 也必须等价）。
func TestNoMemoryLeavesPromptByteIdentical(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("# P\n"), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	baseline := NewAgentLoop(WithAgentMDDir(ws)).composeSystemPrompt()
	withEmpty := NewAgentLoop(WithAgentMDDir(ws), WithMemoryInjection("")).composeSystemPrompt()
	if baseline != withEmpty {
		t.Fatalf("空注入改变了系统提示词：\n--- baseline ---\n%s\n--- with empty ---\n%s", baseline, withEmpty)
	}
}

// TestMemoryInjectionIsStableAcrossCompositions：composeSystemPrompt 每轮循环都会
// 重跑，注入必须每次一致（宿主只在装配期算一次；若本层有任何状态化拼接，提示词会
// 逐轮漂移，而那种漂移会以"模型行为忽好忽坏"的形式出现，极难定位）。
func TestMemoryInjectionIsStableAcrossCompositions(t *testing.T) {
	ws := t.TempDir()
	loop := NewAgentLoop(WithAgentMDDir(ws), WithMemoryInjection("MARKER"))
	first := loop.composeSystemPrompt()
	for i := 0; i < 3; i++ {
		if got := loop.composeSystemPrompt(); got != first {
			t.Fatalf("第 %d 次拼接结果漂移", i+2)
		}
	}
}

// TestMemoryInjectionAccountedAsOwnPart：记忆索引在 ctx 面板里有**独立分区**。
//
// 为什么必须单列而不是混在 system_prompt 里：索引会随记忆条数增长（上限 200 行 /
// 25KB），而用户可以在面板上看到它吃了多少 —— 「记忆悄悄吃掉上下文预算」是产品
// 风险，看不见就防不住。
//
// 同时钉住两个方向：system 侧**不得**把记忆字节扣掉（那是重复计量），总量侧
// **必须**如实增加（索引确实进了请求）。消息分区按「真实锚点 − 静态合计」取残差，
// 静态合计一旦虚高，消息就会被算成负数并被钳到 0 —— 那是更隐蔽的错账。
func TestMemoryInjectionAccountedAsOwnPart(t *testing.T) {
	ws := t.TempDir()
	const mem = "MEMORY_INDEX_TEXT_FOR_ACCOUNTING"
	plain := NewAgentLoop(WithAgentMDDir(ws))
	withMem := NewAgentLoop(WithAgentMDDir(ws), WithMemoryInjection(mem))

	pick := func(l *AgentLoop, key string) int64 {
		bd := l.ContextBreakdown(context.Background(), nil, nil)
		for _, p := range bd.Parts {
			if p.Key == key {
				return p.Tokens
			}
		}
		t.Fatalf("缺分区 %s：%+v", key, bd.Parts)
		return 0
	}

	// 无记忆时该分区必须为 0（不能凭空出现一行）
	if got := pick(plain, ContextPartMemory); got != 0 {
		t.Fatalf("无记忆时 memory 分区应为 0，实际 %d", got)
	}
	// 有记忆时该分区应等于注入文本的 /4 折算
	want := int64(len(mem)) / 4
	if got := pick(withMem, ContextPartMemory); got != want {
		t.Fatalf("memory 分区应为 %d，实际 %d", want, got)
	}
	// system 分区应当**基本不变**，而不是等量减少：注入文本被**全额**划到 memory
	// 分区，system 里只多留 composeSystemPrompt 写的那 2 字符 "\n\n" 分隔符
	// （与 skills / subagent 清单的同一处理），折算后不足 1 token。
	// 若这里看到 system 大幅下降，说明 memory 被重复扣减了。
	delta := pick(plain, ContextPartSystem) - pick(withMem, ContextPartSystem)
	if delta > 1 || delta < -1 {
		t.Fatalf("system 分区应基本不变（±1 token），实际变化 %d", delta)
	}
	// 总量**应当**增加约 memory 分区那么多 —— 索引真的进了请求，是实打实的开销。
	// 单列分区是为了让它**可见**（用户能看到记忆吃掉多少预算），不是为了让它免费。
	// 真正的防错方向是"别重复计量"：system 已在上面断言基本不变，所以这里若出现
	// 2×want 以上的增长，就说明同一段字节被算进了两个分区。
	tb := withMem.ContextBreakdown(context.Background(), nil, nil)
	tp := plain.ContextBreakdown(context.Background(), nil, nil)
	diff := tb.TotalTokens - tp.TotalTokens
	if diff < want-1 || diff > want+1 {
		t.Fatalf("总量应只增加约 %d token（记忆本体），实际 %d（%d → %d）—— 疑似重复计量",
			want, diff, tp.TotalTokens, tb.TotalTokens)
	}
}
