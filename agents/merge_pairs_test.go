package agents

import (
	"fmt"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// ---- 构造助手：造一段「assistant(tool_calls) + tool 结果」序列 ----

type pairSpec struct {
	name  string
	args  string
	text  string
	image bool
}

// buildMsgs 造 n 组工具调用，每组一条 assistant（可带 text）+ 一条 tool。
// assistantText 非空时给 assistant 挂可见文本（决定它能否被整条删除）。
func buildMsgs(specs []pairSpec, assistantText string) []core.Message {
	var msgs []core.Message
	for i, s := range specs {
		id := fmt.Sprintf("c%d", i)
		a := core.NewAssistantMessageWithSignature(
			[]core.Content{{Type: "text", Content: assistantText}},
			"", "", []core.ToolCall{{Id: id, Name: s.name, Arguments: s.args}},
		)
		msgs = append(msgs, a)
		if s.image {
			msgs = append(msgs, core.NewToolMessageWithImages(id, s.text,
				[]core.Content{{Type: core.ContentTypeImage, Content: "data:image/png;base64,AAA"}}))
		} else {
			msgs = append(msgs, core.NewToolMessage(id, s.text))
		}
	}
	return msgs
}

func msgsBytes(msgs []core.Message) int {
	n := 0
	for _, m := range msgs {
		for _, c := range m.Content {
			n += len(c.Content)
		}
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Arguments) + len(tc.Id)
		}
		if m.ToolCallId != "" {
			n += len(m.ToolCallId)
		}
	}
	return n
}

// ---- 基本合并 ----

// TestMergeRemovesDuplicateBashPairs：同参数 bash 重跑、输出逐字相同 → 只留
// 一组，且 assistant 消息（无文本）随之消失。
func TestMergeRemovesDuplicateBashPairs(t *testing.T) {
	specs := []pairSpec{}
	for i := 0; i < 6; i++ {
		specs = append(specs, pairSpec{name: "bash", args: `{"command":"go build ./..."}`, text: "ok\n"})
	}
	a := NewAgentLoop()
	ac := &AgentContext{Messages: buildMsgs(specs, "")}

	if !a.mergeToolPairs(ac, 0) {
		t.Fatal("未发生合并")
	}
	if v := assertNoOrphanToolMsgs(ac.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
	// 6 组、默认保护末尾 2 组：末尾 2 组原样保留 + 最早一条作代表 = 3 组
	if got := len(collectToolPairs(ac.Messages)); got != 3 {
		t.Fatalf("剩余工具对 = %d，期望 3", got)
	}
	if !strings.Contains(ac.Messages[1].Content[0].Content, "已合并 3 次") {
		t.Fatalf("代表条目未记录合并条数: %q", ac.Messages[1].Content[0].Content)
	}
}

// TestMergeKeepsDifferentOutputs：输出不同 = 信息演进（编译失败→修复），不合并。
func TestMergeKeepsDifferentOutputs(t *testing.T) {
	specs := []pairSpec{
		{name: "bash", args: `{"command":"go test"}`, text: "FAIL\n"},
		{name: "bash", args: `{"command":"go test"}`, text: "PASS\n"},
		{name: "bash", args: `{"command":"go test"}`, text: "PASS\n"},
		{name: "bash", args: `{"command":"go test"}`, text: "PASS\n"},
	}
	a := NewAgentLoop()
	ac := &AgentContext{Messages: buildMsgs(specs, "")}
	a.mergeToolPairs(ac, 0)

	if v := assertNoOrphanToolMsgs(ac.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
	// FAIL 必须留存（唯一一份），三条 PASS 折成一条
	var joined string
	for _, m := range ac.Messages {
		joined += m.Content[0].Content
	}
	if !strings.Contains(joined, "FAIL") {
		t.Fatalf("唯一的失败输出被吞: %q", joined)
	}
}

// TestMergeKeepsComplementaryReads 回归（最重要）：读 L10-40 与读 L100-140
// 参数不同、内容不同，是互补信息 —— 参数不同即不同组，天然不合并。
func TestMergeKeepsComplementaryReads(t *testing.T) {
	specs := []pairSpec{
		{name: "read_file", args: `{"path":"a.go","start_line":10,"end_line":40}`, text: "[read_file: a.go, 200 lines total, showing 10-40]\n10: x"},
		{name: "read_file", args: `{"path":"a.go","start_line":100,"end_line":140}`, text: "[read_file: a.go, 200 lines total, showing 100-140]\n100: y"},
		{name: "read_file", args: `{"path":"b.go"}`, text: "[read_file: b.go, 50 lines total, showing 1-50]\n1: z"},
		{name: "read_file", args: `{"path":"b.go"}`, text: "[read_file: b.go, 50 lines total, showing 1-50]\n1: z"},
	}
	a := NewAgentLoop()
	ac := &AgentContext{Messages: buildMsgs(specs, "")}
	a.mergeToolPairs(ac, 0)

	if v := assertNoOrphanToolMsgs(ac.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
	var joined string
	for _, m := range ac.Messages {
		joined += m.Content[0].Content
	}
	if !strings.Contains(joined, "10: x") || !strings.Contains(joined, "100: y") {
		t.Fatalf("互补的局部读被误删: %q", joined)
	}
}

// TestMergeRemovesPlaceholderPairs：已是 slim 占位的旧条目 → 整条删除。
// 这是 D 段折叠结果的自然归宿：文本已是摘要，结构开销也该省掉。
func TestMergeRemovesPlaceholderPairs(t *testing.T) {
	specs := []pairSpec{
		{name: "read_file", args: `{"path":"a.go"}`, text: slimPlaceholderPrefix + "read_file a.go L1-L100 的旧结果已折叠：…]"},
		{name: "read_file", args: `{"path":"a.go"}`, text: "[read_file: a.go, 120 lines total, showing 1-120]\n1: a"},
		{name: "read_file", args: `{"path":"a.go"}`, text: "[read_file: a.go, 120 lines total, showing 1-120]\n1: a"},
		{name: "read_file", args: `{"path":"a.go"}`, text: "[read_file: a.go, 120 lines total, showing 1-120]\n1: a"},
	}
	a := NewAgentLoop()
	ac := &AgentContext{Messages: buildMsgs(specs, "")}
	if !a.mergeToolPairs(ac, 0) {
		t.Fatal("占位条目未被合并")
	}
	if v := assertNoOrphanToolMsgs(ac.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
	var joined string
	for _, m := range ac.Messages {
		joined += m.Content[0].Content
	}
	if strings.Contains(joined, "已折叠") {
		t.Fatalf("占位条目未删除: %q", joined)
	}
}

// TestMergeKeepsAssistantWithText：assistant 带可见文本时，即使 tool call
// 全被摘掉也**不能**删整条消息 —— 那是模型自己的叙述。
func TestMergeKeepsAssistantWithText(t *testing.T) {
	specs := []pairSpec{
		{name: "bash", args: `{"command":"ls"}`, text: "a\n"},
		{name: "bash", args: `{"command":"ls"}`, text: "a\n"},
		{name: "bash", args: `{"command":"ls"}`, text: "a\n"},
		{name: "bash", args: `{"command":"ls"}`, text: "a\n"},
	}
	a := NewAgentLoop()
	ac := &AgentContext{Messages: buildMsgs(specs, "让我看看目录")}
	a.mergeToolPairs(ac, 0)

	nText := 0
	for _, m := range ac.Messages {
		if m.Role == core.Assistant && strings.Contains(m.Content[0].Content, "让我看看目录") {
			nText++
		}
	}
	if nText == 0 {
		t.Fatal("assistant 的可见叙述被误删")
	}
	if v := assertNoOrphanToolMsgs(ac.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
}

// TestMergeNeverTouchesImages：带图片的结果不参与结构合并。
func TestMergeNeverTouchesImages(t *testing.T) {
	specs := []pairSpec{
		{name: "read_file", args: `{"path":"a.png"}`, text: "", image: true},
		{name: "read_file", args: `{"path":"a.png"}`, text: "", image: true},
		{name: "read_file", args: `{"path":"a.png"}`, text: "", image: true},
		{name: "read_file", args: `{"path":"a.png"}`, text: "", image: true},
	}
	a := NewAgentLoop()
	ac := &AgentContext{Messages: buildMsgs(specs, "")}
	if a.mergeToolPairs(ac, 0) {
		t.Fatal("图片结果被合并了")
	}
}

// TestMergeRewritesStaleRefs 回归（不变式 2）：删消息后幸存的占位文本里
// `#N` 引用必须改写为新下标，否则模型会去读一条错位的消息。
func TestMergeRewritesStaleRefs(t *testing.T) {
	// 被删的重复条目排在占位**之前** ⇒ 删除后占位里的 #N 必须前移，
	// 否则模型会去读一条错位的消息（不变式 2）。
	dup := pairSpec{name: "bash", args: `{"command":"dup"}`, text: "1\n"}
	specs := []pairSpec{
		dup, dup, dup, dup, dup, // 5 条重复，全在前面
		{name: "grep", args: `{"pattern":"x"}`, text: slimPlaceholderPrefix + "占位]"},
		{name: "bash", args: `{"command":"target"}`, text: "TARGET\n"},
	}
	msgs := buildMsgs(specs, "")
	// 占位引用「target 那条 tool 消息」当时的下标
	var phIdx, tgtIdx = -1, -1
	for i, m := range msgs {
		if m.Role != core.Tool {
			continue
		}
		if strings.Contains(m.Content[0].Content, "占位]") {
			phIdx = i
		}
		if m.Content[0].Content == "TARGET\n" {
			tgtIdx = i
		}
	}
	if phIdx < 0 || tgtIdx < 0 || tgtIdx <= phIdx {
		t.Fatalf("前置条件不成立: ph=%d tgt=%d", phIdx, tgtIdx)
	}
	msgs[phIdx].Content[0].Content = fmt.Sprintf(slimPlaceholderPrefix+"同一搜索重复执行，结果与消息 #%d 逐字相同]", tgtIdx)

	a := NewAgentLoop()
	ac := &AgentContext{Messages: msgs}
	if !a.mergeToolPairs(ac, 1) {
		t.Fatal("未发生合并")
	}
	if v := assertNoOrphanToolMsgs(ac.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
	var ref string
	for _, m := range ac.Messages {
		if m.Role == core.Tool && strings.Contains(m.Content[0].Content, "逐字相同") {
			ref = m.Content[0].Content
		}
	}
	numStr := strings.Fields(strings.SplitN(ref, "#", 2)[1])[0]
	var got int
	fmt.Sscanf(numStr, "%d", &got)
	if got == tgtIdx {
		t.Fatalf("引用未随删除前移（仍为旧下标 %d）: %q", got, ref)
	}
	if got <= 0 || got >= len(ac.Messages) {
		t.Fatalf("引用 #%d 越界（消息数 %d）: %q", got, len(ac.Messages), ref)
	}
	if ac.Messages[got].Content[0].Content != "TARGET\n" {
		t.Fatalf("引用 #%d 未指向目标消息: %q", got, ac.Messages[got].Content[0].Content)
	}
}

// TestMergePreservesProtectedTail：末尾保护轮数内的调用不参与。
func TestMergePreservesProtectedTail(t *testing.T) {
	specs := []pairSpec{}
	for i := 0; i < 5; i++ {
		specs = append(specs, pairSpec{name: "bash", args: `{"command":"x"}`, text: "same\n"})
	}
	a := NewAgentLoop()
	ac := &AgentContext{Messages: buildMsgs(specs, "")}
	a.mergeToolPairs(ac, 3)
	// 5 组、保护末尾 3 组 ⇒ 末尾 3 组原样 + 最早一条代表 = 4 组
	if got := len(collectToolPairs(ac.Messages)); got != 4 {
		t.Fatalf("保护 3 轮时剩余 = %d，期望 4", got)
	}
}

// TestMergeSavings 量化：重复小结果的**结构**开销占比（这正是 E 段的意义 ——
// C/D 段只换文本，省不掉 assistant 的 tool_calls JSON）。
func TestMergeSavings(t *testing.T) {
	specs := []pairSpec{}
	for i := 0; i < 20; i++ {
		specs = append(specs, pairSpec{name: "bash", args: `{"command":"go build ./..."}`, text: "ok\n"})
	}
	msgs := buildMsgs(specs, "")
	before := msgsBytes(msgs)

	a := NewAgentLoop()
	ac := &AgentContext{Messages: msgs}
	a.mergeToolPairs(ac, 0)
	after := msgsBytes(ac.Messages)

	t.Logf("重复小结果：合并前 %d 字节 → 合并后 %d 字节（省 %.1f%%）", before, after, 100*float64(before-after)/float64(before))
	if after >= before {
		t.Fatalf("合并后未变小：%d >= %d", after, before)
	}
	if v := assertNoOrphanToolMsgs(ac.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
}

func TestIsSlimPlaceholder(t *testing.T) {
	if !isSlimPlaceholder(slimPlaceholderPrefix + "x]") {
		t.Fatal("占位未识别")
	}
	if isSlimPlaceholder("[read_file: a.go, 1 lines total, showing 1-1]") {
		t.Fatal("正常结果被误判为占位")
	}
}

func TestAssertNoOrphanToolMsgsDetects(t *testing.T) {
	// 构造一个孤儿 tool 消息（无对应 tool call）
	bad := []core.Message{core.NewToolMessage("ghost", "x")}
	if v := assertNoOrphanToolMsgs(bad); v == "" {
		t.Fatal("孤儿结果未被检出")
	}
	// 悬空 tool call（有调用无结果）
	msgs := []core.Message{core.NewAssistantMessageWithSignature(nil, "", "",
		[]core.ToolCall{{Id: "c1", Name: "bash", Arguments: "{}"}})}
	if v := assertNoOrphanToolMsgs(msgs); v == "" {
		t.Fatal("悬空调用未被检出")
	}
}

// TestMergeOutputSurvivesProviderTranslation 端到端不变式：合并后的消息序列
// 必须仍能通过 provider 翻译层（配对/角色不变量），否则会在真实请求里 400。
// 翻译函数在 provider 包内未导出，故此处以 Validate + 自检等效覆盖协议面：
// 每条消息合法 + 无孤儿/悬空，且消息序列非空。
func TestMergeOutputSurvivesProviderTranslation(t *testing.T) {
	specs := []pairSpec{
		{name: "bash", args: `{"command":"go build"}`, text: "ok\n"},
		{name: "bash", args: `{"command":"go build"}`, text: "ok\n"},
		{name: "read_file", args: `{"path":"a.go"}`, text: "[read_file: a.go, 3 lines total, showing 1-3]\n1: a"},
		{name: "read_file", args: `{"path":"a.go"}`, text: "[read_file: a.go, 3 lines total, showing 1-3]\n1: a"},
		{name: "bash", args: `{"command":"go test"}`, text: "FAIL\n"},
		{name: "bash", args: `{"command":"go test"}`, text: "PASS\n"},
	}
	a := NewAgentLoop()
	ac := &AgentContext{Messages: buildMsgs(specs, "")}
	a.mergeToolPairs(ac, 1)

	if v := assertNoOrphanToolMsgs(ac.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
	for i, m := range ac.Messages {
		if err := m.Validate(); err != nil {
			t.Fatalf("消息[%d] 非法: %v", i, err)
		}
	}
	if len(ac.Messages) == 0 {
		t.Fatal("合并后上下文为空")
	}
	// 失败那次的独有输出必须留存
	var joined string
	for _, m := range ac.Messages {
		joined += m.Content[0].Content
	}
	if !strings.Contains(joined, "FAIL") {
		t.Fatalf("独有输出被吞: %q", joined)
	}
}

// TestMergePairsToggle E 段开关（产品侧默认开，但必须留得住关闭能力）：
// MergePairs 显式 false 时，结算段不得删除任何消息。这是唯一的逃生舱 ——
// 一旦 E 段在某个模型/端点上有副作用，宿主要能只关它而保留 C/D/F。
func TestMergePairsToggle(t *testing.T) {
	specs := []pairSpec{
		{name: "read_file", args: `{"path":"a.go"}`, text: "v1"},
		{name: "read_file", args: `{"path":"a.go"}`, text: "v2"},
		{name: "read_file", args: `{"path":"a.go"}`, text: "v3"},
		{name: "read_file", args: `{"path":"a.go"}`, text: "v4"},
		{name: "read_file", args: `{"path":"a.go"}`, text: "v5"},
		{name: "bash", args: `{"command":"go test"}`, text: "FAIL\n"},
	}
	build := func(on bool) *AgentContext {
		a := NewAgentLoop(WithToolResultSlim(), WithSlimText(SlimTextOptions{
			MergePairs: BoolPtr(on),
		}))
		ac := &AgentContext{Messages: buildMsgs(specs, ""), slim: newSlimState(*a.cfg.Slim)}
		// 复现结算段的门控（agent_loop.go 的判定）；protectRounds=0 → 默认保护末尾 2 组
		if a.cfg.Slim != nil && a.cfg.Slim.Text != nil && a.cfg.Slim.Text.MergePairs != nil &&
			*a.cfg.Slim.Text.MergePairs && a.mergeToolPairs(ac, 0) {
			ac.slim.reset()
		}
		return ac
	}

	off, on := build(false), build(true)
	if len(off.Messages) != len(buildMsgs(specs, "")) {
		t.Fatalf("MergePairs=false 时仍删了消息：%d", len(off.Messages))
	}
	if len(on.Messages) >= len(off.Messages) {
		t.Fatalf("MergePairs=true 未产生合并：%d >= %d", len(on.Messages), len(off.Messages))
	}
	if v := assertNoOrphanToolMsgs(on.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
}
