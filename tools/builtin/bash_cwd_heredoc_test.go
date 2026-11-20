package builtin

// splitCd 的多行匹配（R1）与「后台命令不改会话 cwd」（R5）回归测试，
// 以及 §17.5 的三条回执不变式。
//
// 来源：docs/AGENT_CWD_DESIGN_2026-11-19.md §14.1 / §14.5 / §14.11（T1/T2/T3）
// + §17.5（回执不变式）+ §16.3 #1/#2/#3。
//
// 为什么这几条值得单独钉：
//   - R1：原正则 `^cd\s+(.+)$` 在 Go 里 `.` 不匹配 \n、`$` 只在文本末尾 ⇒
//     多行命令（heredoc）**整条匹配不上**，实测真实数据 23.55% 的 cd 是这种形态；
//     只改正则还不够：多行命令的正文会被当成「目标的一部分」整段丢掉 ⇒
//     splitCd 必须同时支持换行分隔（§16.3 #2）。
//   - R5：后台识别必须在 cd 拆分**之前**，否则 `cd X && job &` 会把会话 cwd 带走。

import (
	"strings"
	"testing"
)

// TestSplitCdMatchesMultilineHeredoc T1：多行 heredoc 命令能被解析出 cd 目标。
func TestSplitCdMatchesMultilineHeredoc(t *testing.T) {
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	ctx := bashCtx("run-1", "")
	target := t.TempDir()
	cmd := "cd " + target + " && python3 - <<'EOF'\nprint(1)\nEOF"
	got, rest, ok := tool.splitCd(cmd, tool.currentCwd(ctx))
	if !ok {
		t.Fatalf("splitCd 没匹配上多行命令：%q", cmd)
	}
	if got != target {
		t.Fatalf("目标 = %q, want %q", got, target)
	}
	if !strings.Contains(rest, "print(1)") {
		t.Fatalf("剩余命令丢了 heredoc 正文：%q", rest)
	}
}

// TestSplitCdMatchesTrailingNewline T2：带尾换行的 cd 也能匹配（原正则不行）。
func TestSplitCdMatchesTrailingNewline(t *testing.T) {
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	if _, _, ok := tool.splitCd("cd /tmp\n", "/"); !ok {
		t.Fatal("带尾换行的 cd 应能匹配")
	}
}
