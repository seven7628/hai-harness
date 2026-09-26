package agents

import (
	"fmt"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

func imgToolMsg(id string) core.Message {
	return core.NewToolMessageWithImages(id, "[screenshot taken]",
		[]core.Content{{Type: core.ContentTypeImage, Content: "data:image/png;base64," + strings.Repeat("A", 1000), MimeType: "image/png"}})
}

func assistantTool(id, name, args string) core.Message {
	return core.NewAssistantMessageWithSignature(nil, "", "",
		[]core.ToolCall{{Id: id, Name: name, Arguments: args}})
}

func newImageAgent(t *testing.T, cfg ImageSlimConfig) (*AgentLoop, *AgentContext) {
	t.Helper()
	a := NewAgentLoop(WithToolResultSlim(), WithImageSlim(cfg))
	return a, &AgentContext{slim: newSlimState(*a.cfg.Slim)}
}

func countImages(msgs []core.Message) int {
	n := 0
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.Type == core.ContentTypeImage {
				n++
			}
		}
	}
	return n
}

// TestImageSlimKeepsRecentN：只保最近 N 张，更早的替换为文本占位。
func TestImageSlimKeepsRecentN(t *testing.T) {
	a, ac := newImageAgent(t, ImageSlimConfig{Keep: 2, MaxRounds: -1})
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("c%d", i)
		ac.Messages = append(ac.Messages, assistantTool(id, "screenshot", "{}"))
		ac.Messages = append(ac.Messages, imgToolMsg(id))
	}
	a.applyImageSlim(ac)

	if got := countImages(ac.Messages); got != 2 {
		t.Fatalf("保留图片 %d 张，期望 2", got)
	}
	// 被淘汰的留下可寻址说明
	var notes int
	for _, m := range ac.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Content, "screenshot removed from context") {
				notes++
			}
		}
	}
	if notes != 4 {
		t.Fatalf("占位说明 %d 条，期望 4", notes)
	}
	// 占位必须写明「不要以为还记得」——静默消失会让模型基于错误记忆推理
	for _, m := range ac.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Content, "screenshot removed") &&
				!strings.Contains(c.Content, "do NOT assume") {
				t.Fatalf("占位缺少失效告警: %q", c.Content)
			}
		}
	}
}

// TestImageSlimRespectsMaxRounds：轮龄未到的旧图不淘汰（刚截的图不会被立刻抹）。
func TestImageSlimRespectsMaxRounds(t *testing.T) {
	a, ac := newImageAgent(t, ImageSlimConfig{Keep: 0, MaxRounds: 30})
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("c%d", i)
		ac.Messages = append(ac.Messages, assistantTool(id, "screenshot", "{}"))
		ac.Messages = append(ac.Messages, imgToolMsg(id))
	}
	a.applyImageSlim(ac)
	if got := countImages(ac.Messages); got != 3 {
		t.Fatalf("轮龄未到却被淘汰，剩 %d 张", got)
	}
}

// TestImageSlimEvictsStaleByAge：轮龄超限即淘汰。
// 前置：必须**超出 keep 窗口**才轮到轮龄闸 —— 「最近 N 张」是主规则，
// 轮龄是次级闸（只对已不在最近窗口里的图片生效）。故这里放 1 张老图 +
// 大量文本 + 2 张新图（keep=2 ⇒ 老图已在窗口外，且轮龄远超上限）。
func TestImageSlimEvictsStaleByAge(t *testing.T) {
	a, ac := newImageAgent(t, ImageSlimConfig{Keep: 2, MaxRounds: 3})
	ac.Messages = append(ac.Messages, assistantTool("old", "screenshot", "{}"))
	ac.Messages = append(ac.Messages, imgToolMsg("old"))
	for i := 0; i < 40; i++ {
		ac.Messages = append(ac.Messages, core.NewToolMessage(fmt.Sprintf("t%d", i), "text"))
	}
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("new%d", i)
		ac.Messages = append(ac.Messages, assistantTool(id, "screenshot", "{}"))
		ac.Messages = append(ac.Messages, imgToolMsg(id))
	}
	a.applyImageSlim(ac)
	if got := countImages(ac.Messages); got != 2 {
		t.Fatalf("超龄老图未被淘汰（剩 %d 张，期望只剩最近 2 张）", got)
	}
}

// TestImageSlimKeepsInsideKeepWindowEvenIfOld：仍在 keep 窗口内的图片，
// 即使轮龄很大也保留 —— 「最近 N 张」优先于轮龄（模型可能正在引用它）。
func TestImageSlimKeepsInsideKeepWindowEvenIfOld(t *testing.T) {
	a, ac := newImageAgent(t, ImageSlimConfig{Keep: 3, MaxRounds: 1})
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("c%d", i)
		ac.Messages = append(ac.Messages, assistantTool(id, "screenshot", "{}"))
		ac.Messages = append(ac.Messages, imgToolMsg(id))
	}
	for i := 0; i < 50; i++ {
		ac.Messages = append(ac.Messages, core.NewToolMessage(fmt.Sprintf("t%d", i), "text"))
	}
	a.applyImageSlim(ac)
	if got := countImages(ac.Messages); got != 3 {
		t.Fatalf("keep 窗口内的图片被轮龄闸误删，剩 %d 张", got)
	}
}

// TestImageSlimNeverTouchesUserImages：用户自己发的截图是需求本身，绝不动。
func TestImageSlimNeverTouchesUserImages(t *testing.T) {
	a, ac := newImageAgent(t, ImageSlimConfig{Keep: 0, MaxRounds: 1})
	ac.Messages = append(ac.Messages,
		core.NewUserMessage(
			core.Content{Type: core.ContentTypeText, Content: "看这张图"},
			core.Content{Type: core.ContentTypeImage, Content: "data:image/png;base64,AAAA", MimeType: "image/png"},
		))
	for i := 0; i < 30; i++ {
		ac.Messages = append(ac.Messages, core.NewToolMessage(fmt.Sprintf("t%d", i), "text"))
	}
	before := countImages(ac.Messages)
	a.applyImageSlim(ac)
	if got := countImages(ac.Messages); got != before {
		t.Fatalf("user 消息里的图片被改动：%d -> %d", before, got)
	}
}

// TestImageSlimDisabled：Keep<0 = 关闭。
func TestImageSlimDisabled(t *testing.T) {
	a := NewAgentLoop(WithToolResultSlim(), WithImageSlim(ImageSlimConfig{Keep: -1}))
	ac := &AgentContext{slim: newSlimState(*a.cfg.Slim)}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("c%d", i)
		ac.Messages = append(ac.Messages, assistantTool(id, "screenshot", "{}"))
		ac.Messages = append(ac.Messages, imgToolMsg(id))
	}
	a.applyImageSlim(ac)
	if got := countImages(ac.Messages); got != 6 {
		t.Fatalf("关闭后仍被改动，剩 %d 张", got)
	}
}

// TestImageSlimPreservesTextAndPairing：淘汰只摘图片块，文本与 tool 配对不动。
func TestImageSlimPreservesTextAndPairing(t *testing.T) {
	a, ac := newImageAgent(t, ImageSlimConfig{Keep: 1, MaxRounds: -1})
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("c%d", i)
		ac.Messages = append(ac.Messages, assistantTool(id, "screenshot", "{}"))
		ac.Messages = append(ac.Messages, imgToolMsg(id))
	}
	a.applyImageSlim(ac)
	if v := assertNoOrphanToolMsgs(ac.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
	// 被淘汰那条的原始文本仍在（只是没了图）
	found := false
	for _, m := range ac.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Content, "[screenshot taken]") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("原文本被误删（只应摘图片块）")
	}
}

// TestImageSlimThenMergePairs F 段与 E 段协同：被 F 段占位的图片条目，
// 其 tool 结果已只剩占位说明，E 段可整条删除而不丢失任何图片信息
// （图片本来就不再看）。
func TestImageSlimThenMergePairs(t *testing.T) {
	a := NewAgentLoop(WithToolResultSlim(), WithImageSlim(ImageSlimConfig{Keep: 1, MaxRounds: -1}), WithSlimText(SlimTextOptions{}))
	ac := &AgentContext{slim: newSlimState(*a.cfg.Slim)}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		ac.Messages = append(ac.Messages, assistantTool(id, "screenshot", "{}"))
		ac.Messages = append(ac.Messages, imgToolMsg(id))
	}
	a.applyImageSlim(ac)
	if got := countImages(ac.Messages); got != 1 {
		t.Fatalf("F 段后应剩 1 张，实际 %d", got)
	}
	if v := assertNoOrphanToolMsgs(ac.Messages); v != "" {
		t.Fatalf("配对被破坏: %s", v)
	}
}
