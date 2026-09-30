package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/skills"
)

// globalAgentMDDir 必须落在 ~/.agents（与 skills / subagents 同一命名空间主路径）。
func TestGlobalAgentMDDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无 home 目录")
	}
	want := filepath.Join(home, skills.AgentDirName)
	if got := globalAgentMDDir(); got != want {
		t.Fatalf("globalAgentMDDir = %q, want %q", got, want)
	}
}

// globalAgentMDDir 在 home 解析失败时必须返回**空串**，不能是 "."。
//
// 空串 = agents 层不注入用户级；"." 则是工作区根 → 工作区根的 AGENTS.md 会被
// 当"用户级"再注入一遍（同一文件两份、两条路径标注）。
func TestGlobalAgentMDDirNeverFallsBackToCwd(t *testing.T) {
	got := globalAgentMDDir()
	if got == "" {
		return // home 解析失败 → 空串 = 正确降级
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Skip("无 cwd")
	}
	if got == wd || got == "." {
		t.Fatalf("globalAgentMDDir = %q，兜底成了 cwd（用户级层会重复注入工作区根文件）", got)
	}
}

// 三宿主（主会话 / spawn / explore）都必须装配用户级层：漏装**不报错**，只是子 agent
// 读不到用户的个人全局指令 —— 属"看起来有、实际失效"的静默退化。
//
// 为什么扫源码而不是断行为：装配点散在 main.go 的三个 build*Loop 内，没有可枚举的
// 单一入口，而真正可观察 system 字节的 composeSystemPrompt 未导出（ContextBreakdown
// 只给分区字符数、不给文本）。扫源码是这里的低成本兜底，代价是重命名/重构会误报 ——
// 故只断言"两个选项成对出现"，不断言选项在切片中的先后（那是无意义的：每个 With*
// 都是纯 setter，顺序不影响最终 Config；分层顺序固定在 agents 层内）。
func TestUserAgentMDWiredAlongsideWorkspaceLayer(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	ws := strings.Count(text, "agents.WithAgentMDDir(workspace)")
	user := strings.Count(text, "agents.WithUserAgentMDDir(globalAgentMDDir())")
	if ws == 0 {
		t.Fatal("未找到工作区层装配点 —— 本测试的前提已失效，请同步更新")
	}
	if user != ws {
		t.Fatalf("工作区层 %d 处、用户级层 %d 处 —— 有宿主漏装用户级 AGENTS.md", ws, user)
	}
}
