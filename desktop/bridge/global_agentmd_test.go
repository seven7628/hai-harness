package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/skills"
)

// globalAgentMDDir 必须落在 ~/.agents（与 skills / subagents 同一命名空间主路径），
// 且 home 解析失败时返回**空串**而非 "."。
//
// 后者是本文件的主要防线：globalAgentMDDir 的结果直接进 WithUserAgentMDDir，
// 兜底成 cwd 会让工作区根的 AGENTS.md 被当"用户级"再注入一遍 —— 同一文件两份。
func TestGlobalAgentMDDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无 home 目录")
	}
	want := filepath.Join(home, skills.AgentDirName)
	if got := globalAgentMDDir(); got != want {
		t.Fatalf("globalAgentMDDir = %q, want %q", got, want)
	}
	// 空串 = 无用户级层（agents 层返回 nil、不注入任何块；该行为由
	// agents 包的 TestUserAgentMDAbsentIsNoop 覆盖，此处不重复断言 ——
	// composeSystemPrompt 未导出，bridge 侧无法直接观察 system 文本）
}

// 三宿主（主会话 / spawn / explore）都必须装配用户级层。
// 漏一个 = 该 loop 的子 agent 读不到用户的个人全局指令，且**不会报错**
// —— 正是"看起来有、实际失效"的静默退化。
//
// 装配点分散在 main.go 三处 build*Loop 内、没有可枚举的单一入口，故此处用
// 源码断言钉住：三处 WithAgentMDDir(workspace) 旁都紧跟 WithUserAgentMDDir。
func TestUserAgentMDWiredForAllHosts(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	const n = 3 // buildLoop / buildPersonaLoop / buildExploreLoop
	if got := strings.Count(text, "agents.WithAgentMDDir(workspace)"); got != n {
		t.Fatalf("工作区层装配点数 = %d, want %d（三宿主各一）", got, n)
	}
	if got := strings.Count(text, "agents.WithUserAgentMDDir(globalAgentMDDir())"); got != n {
		t.Fatalf("用户级层装配点数 = %d, want %d（漏装 = 子 agent 读不到个人全局指令）", got, n)
	}
	// 用户级必须排在工作区层**之后**入 opts（选项顺序无关，语义顺序在
	// composeSystemPrompt 里由 agents 层固定；此处只防装配注释与实际脱节）
	userIdx := strings.Index(text, "agents.WithUserAgentMDDir(globalAgentMDDir())")
	wsIdx := strings.Index(text, "agents.WithAgentMDDir(workspace)")
	if userIdx < wsIdx {
		t.Fatal("主装配点顺序异常：用户级层出现在工作区层之前")
	}
}
