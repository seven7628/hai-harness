package main

// memory_baseline_wiring_test.go —— 基线装置的**确定性**自检（不需要凭证、不打网关）。
//
// 为什么必须单独写一个文件而不是塞进 memory_baseline_test.go：三臂实验的每一个
// 数字都建立在"注入文本真的进了 context、真的只差那一段"之上。注入若因为某个
// 静默原因没进去，三臂会退化成同一个实验，而读数看起来仍然像模像样 ——
// 上一版 12%~62% 的教训正是"先有结论再找证据"。这里把最容易被静默破坏的前提
// 做成**不依赖 LLM 的断言**。
//
// 三条断言：
//  1. fixture 与常量不漂移（真值/decoy/过时值真的写进了该写的文件）；
//  2. 三臂注入形态正确（A 给自信答案不给指针、B 只给指针不给答案、C 空）；
//  3. **注入真的进了发给 provider 的请求**（用假 provider 截获，断言模型实际
//     收到的 system prompt 里含注入内容，且 A/C 的差集恰好是注入块本身）。
//
// 第 3 条是本文件存在的理由：前两条只证明"我们构造了正确的字符串"，不证明
// "AgentLoop 把它送出去了"。

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// baselineCaptureProvider 截获 AgentLoop 发给 provider 的请求（不联网），
// 并回一轮 stop。用它断言"模型真正收到的内容"。
type baselineCaptureProvider struct {
	mu   sync.Mutex
	sent [][]core.Message
}

func (p *baselineCaptureProvider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	p.sent = append(p.sent, append([]core.Message(nil), req.Messages...))
	p.mu.Unlock()
	return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
}

// systemText 把一次请求里全部 system 消息的文本拼起来。
func (p *baselineCaptureProvider) systemText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sent) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range p.sent[0] {
		if m.Role != core.System {
			continue
		}
		for _, c := range m.Content {
			if c.Type == core.ContentTypeText {
				b.WriteString(c.Content)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// baselineCaptureRun 用假 provider 跑一次，只为截获请求（不打网关，不需要凭证）。
func baselineCaptureRun(t *testing.T, root, inject string) string {
	t.Helper()
	eng := tools.NewToolEngine()
	for _, tl := range builtin.NewFileTools(root).Tools() {
		switch tl.Name() {
		case "read_file", "grep", "glob":
			eng.RegisterTool(context.Background(), tl)
		}
	}
	p := &baselineCaptureProvider{}
	opts := []agents.Option{
		agents.WithProvider(p),
		agents.WithProviderName("capture"),
		agents.WithModel("capture-model"),
		agents.WithToolEngine(eng),
		agents.WithWorkingDir(root),
		agents.WithMaxIterations(1),
		agents.WithMaxRetries(1),
	}
	if inject != "" {
		opts = append(opts, agents.WithSystemPrompt(inject))
	}
	loop := agents.NewAgentLoop(opts...)
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{
		Type: core.ContentTypeText, Content: baselineQuestion,
	})}, func(context.Context, events.Event) {})
	if ac == nil {
		t.Fatal("AgentLoop.Run 返回 nil")
	}
	return p.systemText()
}

func TestBaselineFixtureIsSelfConsistent(t *testing.T) {
	// 真值文件必须真的写着真值，decoy 必须真的写着另一个值。
	if !strings.Contains(baselineFixtureFiles[baselineTruthFile], "**"+baselineTrueValue+"**") {
		t.Errorf("真值文件 %s 不含 **%s**：fixture 与常量漂移", baselineTruthFile, baselineTrueValue)
	}
	if !strings.Contains(baselineFixtureFiles[baselineDecoyFile], "**"+baselineDecoyValue+"**") {
		t.Errorf("decoy 文件 %s 不含 **%s**：fixture 与常量漂移", baselineDecoyFile, baselineDecoyValue)
	}
	// 三个取值必须互不相同，否则"交的是哪一份"不再可判。
	vals := map[string]bool{baselineTrueValue: true, baselineDecoyValue: true, baselineStaleValue: true}
	if len(vals) != 3 {
		t.Errorf("真值/decoy/过时值必须互不相同，实得 %d 个不同取值: %v", len(vals), vals)
	}
	// 冲突臂的注入必须真的自信地给出过时值，且**不给指针**（给了就不是 A 臂了）。
	a := baselineInjectionArmA()
	if !strings.Contains(a, baselineStaleValue) {
		t.Errorf("A 臂注入不含过时值 %s", baselineStaleValue)
	}
	if strings.Contains(a, baselineTruthFile) {
		t.Errorf("A 臂注入不得包含真值文件路径 %s：那是 B 臂的形态", baselineTruthFile)
	}
	// 指针臂必须只给路径、**不含任何答案取值**（含了就不是 B 臂了）。
	b := baselineInjectionArmB()
	if !strings.Contains(b, baselineTruthFile) {
		t.Errorf("B 臂注入不含真值文件路径 %s", baselineTruthFile)
	}
	for _, v := range []string{baselineTrueValue, baselineStaleValue, baselineDecoyValue} {
		if strings.Contains(b, v) {
			t.Errorf("B 臂注入不得包含答案取值 %s：它只该是索引指针", v)
		}
	}
	if !strings.HasPrefix(a, baselineMemoryHeader) || !strings.HasPrefix(b, baselineMemoryHeader) {
		t.Error("A/B 注入必须共用同一头部，唯一变量是那一行内容")
	}
}

func TestBaselineArmsProduceExpectedInjection(t *testing.T) {
	arms := baselineArms()
	if len(arms) != 3 {
		t.Fatalf("三臂实验必须有 3 条臂，实得 %d", len(arms))
	}
	if !strings.Contains(arms[0].Inject, baselineStaleValue) {
		t.Error("第 1 条臂必须是冲突注入（自信答案）")
	}
	if !strings.Contains(arms[1].Inject, baselineTruthFile) {
		t.Error("第 2 条臂必须是指针注入")
	}
	if arms[2].Inject != "" {
		t.Errorf("第 3 条臂必须完全不注入，实得 %q", arms[2].Inject)
	}
	if reflect.DeepEqual(
		strings.TrimPrefix(arms[0].Inject, baselineMemoryHeader),
		strings.TrimPrefix(arms[1].Inject, baselineMemoryHeader)) {
		t.Error("A 与 B 的注入内容相同：两臂将退化成同一实验")
	}
}

// TestBaselineInjectionReachesProvider 是本文件的核心断言：注入真的进了
// 发给 provider 的 system prompt，且三臂之间**只差那一段注入**。
//
// 少了这条，前两条断言只证明"我们造了正确的字符串"，不证明"AgentLoop 把它
// 送出去了"——而后者才是三臂实验成立的前提。
func TestBaselineInjectionReachesProvider(t *testing.T) {
	root := baselineExecFixture(t)
	arms := baselineArms()

	sysA := baselineCaptureRun(t, root, arms[0].Inject)
	sysB := baselineCaptureRun(t, root, arms[1].Inject)
	sysC := baselineCaptureRun(t, root, arms[2].Inject)

	for i, sys := range []string{sysA, sysB, sysC} {
		if sys == "" {
			t.Fatalf("臂 %d：截获到空的 system prompt（AgentLoop 没发出请求？）", i)
		}
		if !strings.Contains(sys, "Current Workspace") {
			t.Errorf("臂 %d：system prompt 缺工作区层，截获面可能选错了消息", i)
		}
	}
	if !strings.Contains(sysA, baselineStaleValue) {
		t.Error("A 臂：注入的过时值没进 system prompt —— 注入没生效，三臂实验作废")
	}
	if !strings.Contains(sysB, baselineTruthFile) {
		t.Error("B 臂：注入的指针没进 system prompt —— 注入没生效，三臂实验作废")
	}
	if strings.Contains(sysB, baselineStaleValue) || strings.Contains(sysC, baselineStaleValue) {
		t.Error("B/C 臂出现了过时值：三臂的注入没有隔离干净")
	}
	if strings.Contains(sysC, "Project Memory") {
		t.Error("C 臂的 system prompt 含 Project Memory 段：C 臂必须完全不注入")
	}
	// 唯一变量：把 A 的注入块整段抹掉、并归一化拼接分隔空行后，A 必须与 C 逐字节相同。
	//
	// 为什么要归一化空行：composeSystemPrompt 用 "\n\n" 拼接各层，没有层 1b 时
	// 就少一个分隔符。那是拼接器行为，不是行为变量 —— 但它**必须**被显式认领，
	// 否则"A 与 C 的差无法归因"这句话就没人核过。归一化后必须完全一致：多出任何
	// 真实内容都会在这里暴露。
	if got := baselineNormalizeJoin(strings.Replace(sysA, arms[0].Inject, "", 1)); got != baselineNormalizeJoin(sysC) {
		t.Errorf("A 与 C 的 system prompt 差的不止注入块本身（归一化拼接空行后仍有差异）。\n"+
			"多出来的内容会让 A/C 之差无法归因到注入。\nA-C 首个差异行: %q", firstDiff(got, baselineNormalizeJoin(sysC)))
	}
	// 反向自查：C 里不得有任何 A 独有的内容（防止 A 少了东西而不是多了东西）。
	for _, frag := range []string{baselineMemoryHeader, baselineStaleValue} {
		if strings.Contains(sysC, frag) {
			t.Errorf("C 臂含 %q：C 臂必须完全没有注入", frag)
		}
	}
}

// baselineNormalizeJoin 归一化层间分隔空行（把连续空行压成一个空行、去掉首尾空行）。
func baselineNormalizeJoin(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func firstDiff(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) && i < len(lb); i++ {
		if la[i] != lb[i] {
			return la[i] + " | " + lb[i]
		}
	}
	return fmt.Sprintf("长度不同：%d vs %d 行", len(la), len(lb))
}
